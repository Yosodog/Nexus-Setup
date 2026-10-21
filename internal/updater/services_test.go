package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCoreServiceUnitsFollowHostServices(t *testing.T) {
	units, err := componentServiceUnits(ComponentCore, hostServices{})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"nexus-core.service", "nexus-core-sync.service", "nexus-core-pulse-check.service", "nexus-core-php-fpm.service"}
	if !reflect.DeepEqual(units, expected) {
		t.Fatalf("unexpected base Core units: %#v", units)
	}
	units, err = componentServiceUnits(ComponentCore, hostServices{pulseRedis: true, subsStream: true})
	if err != nil {
		t.Fatal(err)
	}
	expected = []string{"nexus-core.service", "nexus-core-sync.service", "nexus-core-pulse-check.service",
		"nexus-core-pulse-work.service", "nexus-core-subs-stream.service", "nexus-core-php-fpm.service"}
	if !reflect.DeepEqual(units, expected) {
		t.Fatalf("unexpected Redis Core units: %#v", units)
	}
	enabled, err := componentEnableUnits(ComponentCore, hostServices{subsStream: true})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(enabled, "nexus-core-php-fpm.service") || !containsString(enabled, "nexus-core-subs-stream.service") {
		t.Fatalf("unexpected enabled Core units: %#v", enabled)
	}
	if unit, err := componentServiceUnit(ComponentCore); err != nil || unit != "nexus-core.service" {
		t.Fatalf("Core primary unit changed: %q %v", unit, err)
	}
}

func TestCoreUnitFilesRunEachQueueAndDaemon(t *testing.T) {
	units := systemdUnits()
	expected := map[string]string{
		"nexus-core.service":             "artisan queue:work --queue=default ",
		"nexus-core-sync.service":        "artisan queue:work --queue=sync ",
		"nexus-core-pulse-check.service": "artisan pulse:check\n",
		"nexus-core-pulse-work.service":  "artisan pulse:work\n",
		"nexus-core-subs-stream.service": "artisan subs:consume-stream\n",
	}
	for unit, command := range expected {
		content := units["/etc/systemd/system/"+unit]
		if !strings.Contains(content, "ExecStart=/usr/bin/php /opt/nexus/nexus-core/current/"+command) {
			t.Fatalf("%s does not run %q:\n%s", unit, command, content)
		}
		if !strings.Contains(content, "User=nexus-core\n") || !strings.Contains(content, "ProtectSystem=strict\n") || !strings.Contains(content, "WantedBy=multi-user.target\n") {
			t.Fatalf("%s lost the Core sandbox or install section", unit)
		}
	}
	for _, unit := range coreWorkerUnits(hostServices{pulseRedis: true, subsStream: true}) {
		if units["/etc/systemd/system/"+unit] == "" {
			t.Fatalf("%s has no embedded unit file", unit)
		}
	}
}

func TestHostServicesReadPulseDriverAndSubsDelivery(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileAppWebSubsRemoteDB, SubsDelivery: SubsDeliveryRedisStream, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if services := hostServicesFor(paths, state); services.pulseRedis || !services.subsStream {
		t.Fatalf("unexpected services without a Core environment: %#v", services)
	}
	writeTestFile(t, filepath.Join(paths.NexusConfigDir, string(ComponentCore), ".env"), "APP_NAME=Nexus\nPULSE_INGEST_DRIVER=redis\n")
	if services := hostServicesFor(paths, state); !services.pulseRedis || !services.subsStream {
		t.Fatalf("Redis Pulse ingest was not detected: %#v", services)
	}
	writeTestFile(t, filepath.Join(paths.NexusConfigDir, string(ComponentCore), ".env"), "PULSE_INGEST_DRIVER=storage\n")
	state.SubsDelivery = SubsDeliveryHTTP
	if services := hostServicesFor(paths, state); services.pulseRedis || services.subsStream {
		t.Fatalf("storage Pulse and HTTP delivery need no optional units: %#v", services)
	}
	state.SubsDelivery = SubsDeliveryRedisStream
	state.Components[ComponentCore] = InstalledComponent{}
	if services := hostServicesFor(paths, state); services.subsStream {
		t.Fatal("a host without Core cannot consume the Subs stream")
	}
}

func TestRestartComponentsCoversEveryCoreUnit(t *testing.T) {
	executor := &recordingExecutor{}
	engine := DeploymentEngine{Config: DefaultConfig(), Executor: executor}
	if err := engine.restartComponents(context.Background(), []Component{ComponentCore}, hostServices{pulseRedis: true, subsStream: true}); err != nil {
		t.Fatal(err)
	}
	restarted := []string{}
	for _, call := range executor.calls {
		if call[1] != "restart" {
			t.Fatalf("unexpected systemd action: %#v", call)
		}
		restarted = append(restarted, call[2])
	}
	expected, _ := componentServiceUnits(ComponentCore, hostServices{pulseRedis: true, subsStream: true})
	if !reflect.DeepEqual(restarted, expected) {
		t.Fatalf("restarted %#v, expected %#v", restarted, expected)
	}
}

type missingUnitExecutor struct {
	recordingExecutor
	missing string
}

func (executor *missingUnitExecutor) Run(ctx context.Context, binary string, args []string) error {
	_ = executor.recordingExecutor.Run(ctx, binary, args)
	if len(args) == 2 && args[0] == "cat" && args[1] == executor.missing {
		return errors.New("no files found")
	}
	return nil
}

func TestRequireServiceUnitsNamesTheMissingUnit(t *testing.T) {
	executor := &missingUnitExecutor{missing: "nexus-core-sync.service"}
	engine := DeploymentEngine{Config: DefaultConfig(), Executor: executor}
	err := engine.requireServiceUnits(context.Background(), []Component{ComponentCore}, hostServices{})
	if err == nil || !strings.Contains(err.Error(), "nexus-core-sync.service") || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("expected an actionable missing-unit error, got %v", err)
	}
	for _, call := range executor.calls {
		if call[1] != "cat" {
			t.Fatalf("unit check changed systemd state: %#v", call)
		}
	}
}

func TestRestartComponentStopsBeforeChangingWhenAUnitIsMissing(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileSubsOnly, Channel: ChannelStable, Components: map[Component]InstalledComponent{
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	executor := &missingUnitExecutor{missing: "nexus-subs.service"}
	config := DefaultConfig()
	config.Paths = paths
	engine := DeploymentEngine{Config: config, Executor: executor}
	if err := engine.restartComponent(context.Background(), ComponentSubs); err == nil {
		t.Fatal("expected restart to refuse a missing unit")
	}
	if len(executor.calls) != 1 {
		t.Fatalf("restart continued after the unit check failed: %#v", executor.calls)
	}
}

func TestInstallationStateKeepsSubsDelivery(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileAppWebSubsRemoteDB, Channel: ChannelStable, SubsDelivery: SubsDeliveryRedisStream, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadInstallation(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SubsDelivery != SubsDeliveryRedisStream {
		t.Fatalf("Subs delivery was not persisted: %q", loaded.SubsDelivery)
	}
	if cloneInstallation(loaded).SubsDelivery != SubsDeliveryRedisStream {
		t.Fatal("an update's working copy dropped the Subs delivery")
	}

	invalid := cloneInstallation(state)
	invalid.SubsDelivery = "carrier-pigeon"
	if err := SaveInstallation(paths, invalid); err == nil {
		t.Fatal("unknown delivery mode was accepted")
	}
	withoutSubs := InstallationState{Profile: ProfileWebOnly, Channel: ChannelStable, SubsDelivery: SubsDeliveryRedisStream, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, withoutSubs); err == nil {
		t.Fatal("Redis Stream delivery without Subs was accepted")
	}
}

func TestSubsDeliveryDefaultsToHTTP(t *testing.T) {
	environment := map[string]string{}
	if err := (DeploymentEngine{}).applySubsDelivery(environment, nil); err != nil {
		t.Fatal(err)
	}
	if environment["DELIVERY_DRIVER"] != "http" || environment["SUBS_REDIS_HMAC_SECRET_FILE"] != "" {
		t.Fatalf("unexpected HTTP delivery environment: %#v", environment)
	}
	err := (DeploymentEngine{}).applySubsDelivery(environment, &subsRedisStream{URL: "redis://127.0.0.1:6379/3", Stream: defaultSubsRedisStream, HMACSecret: "short"})
	if err == nil {
		t.Fatal("a short HMAC secret was accepted")
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0640); err != nil {
		t.Fatal(err)
	}
}

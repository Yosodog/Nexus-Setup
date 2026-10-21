package updater

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCoreWorkerCountAddsTemplatedQueueWorkers(t *testing.T) {
	for _, count := range []int{0, 1} {
		units, err := componentServiceUnits(ComponentCore, hostServices{coreWorkers: count})
		if err != nil {
			t.Fatal(err)
		}
		for _, unit := range units {
			if strings.HasPrefix(unit, "nexus-core-worker@") {
				t.Fatalf("%d workers started an extra unit: %#v", count, units)
			}
		}
	}
	units, err := componentServiceUnits(ComponentCore, hostServices{coreWorkers: 3})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"nexus-core.service", "nexus-core-worker@2.service", "nexus-core-worker@3.service",
		"nexus-core-sync.service", "nexus-core-pulse-check.service", "nexus-core-php-fpm.service"}
	if !reflect.DeepEqual(units, expected) {
		t.Fatalf("unexpected Core units for three workers: %#v", units)
	}
	enabled, err := componentEnableUnits(ComponentCore, hostServices{coreWorkers: 3})
	if err != nil || !containsString(enabled, "nexus-core-worker@3.service") {
		t.Fatalf("extra workers are not enabled with Core: %#v %v", enabled, err)
	}
	if unit, err := componentServiceUnit(ComponentCore); err != nil || unit != "nexus-core.service" {
		t.Fatalf("Core primary unit changed: %q %v", unit, err)
	}
	optional := optionalCoreWorkerUnits()
	for number := 2; number <= MaxCoreWorkers; number++ {
		if !containsString(optional, coreExtraWorkerUnit(number)) {
			t.Fatalf("bootstrap would not stop surplus worker %d: %#v", number, optional)
		}
	}
}

func TestCoreWorkerTemplateFollowsTheQueueUnit(t *testing.T) {
	units := systemdUnits()
	template := units["/etc/systemd/system/nexus-core-worker@.service"]
	for _, expected := range []string{
		"Description=Nexus Core queue worker %i\n",
		"PartOf=nexus-core.service\n",
		"ExecStart=/usr/bin/php /opt/nexus/nexus-core/current/artisan queue:work --queue=default --sleep=3 --tries=3 --max-time=3600\n",
		"User=nexus-core\n", "ProtectSystem=strict\n", "MemoryMax=512M\n",
		"[Install]\nWantedBy=nexus-core.service\n",
	} {
		if !strings.Contains(template, expected) {
			t.Fatalf("worker template lacks %q:\n%s", expected, template)
		}
	}
	primary := units["/etc/systemd/system/nexus-core.service"]
	if strings.Contains(primary, "PartOf=") || !strings.Contains(primary, "redis-server.service\n\n[Service]\n") || !strings.Contains(primary, "WantedBy=multi-user.target\n") {
		t.Fatalf("nexus-core.service changed shape:\n%s", primary)
	}
}

func TestHostServicesReadCoreWorkerCount(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileWebOnly, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if services := hostServicesFor(paths, state); services.coreWorkers != 1 {
		t.Fatalf("state without a count should run one worker: %#v", services)
	}
	state.CoreWorkers = 4
	if services := hostServicesFor(paths, state); services.coreWorkers != 4 {
		t.Fatalf("worker count was not read: %#v", services)
	}
}

func TestInstallationStateKeepsCoreWorkers(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileWebOnly, Channel: ChannelStable, CoreWorkers: 2, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadInstallation(paths)
	if err != nil || loaded.CoreWorkers != 2 {
		t.Fatalf("Core worker count was not persisted: %d %v", loaded.CoreWorkers, err)
	}
	for _, invalid := range []int{-1, MaxCoreWorkers + 1} {
		state.CoreWorkers = invalid
		if err := SaveInstallation(paths, state); err == nil {
			t.Fatalf("Core worker count %d was accepted", invalid)
		}
	}
}

func TestRestartLetsNexusCoreRestartExtraWorkers(t *testing.T) {
	executor := &recordingExecutor{}
	engine := DeploymentEngine{Config: DefaultConfig(), Executor: executor}
	if err := engine.restartComponents(context.Background(), []Component{ComponentCore}, hostServices{coreWorkers: 2}); err != nil {
		t.Fatal(err)
	}
	actions := []string{}
	for _, call := range executor.calls {
		actions = append(actions, call[1]+" "+call[2])
	}
	expected := []string{"restart nexus-core.service", "start nexus-core-worker@2.service", "restart nexus-core-sync.service",
		"restart nexus-core-pulse-check.service", "restart nexus-core-php-fpm.service"}
	if !reflect.DeepEqual(actions, expected) {
		t.Fatalf("restarted %#v, expected %#v", actions, expected)
	}
}

func TestSetCoreWorkersRequestNeedsACount(t *testing.T) {
	request := testRequest(OperationSetCoreWorkers)
	for _, invalid := range []int{0, -1, MaxCoreWorkers + 1} {
		request.CoreWorkers = invalid
		if err := request.Validate(); err == nil {
			t.Fatalf("Core worker count %d was accepted", invalid)
		}
	}
	request.CoreWorkers = 2
	if err := request.Validate(); err != nil {
		t.Fatalf("two Core workers were rejected: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Request
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.CoreWorkers != 2 || !strings.Contains(string(encoded), `"core_workers":2`) {
		t.Fatalf("Core worker count did not round-trip: %s %v", encoded, err)
	}
	update := testRequest(OperationUpdate)
	update.CoreWorkers = 2
	if err := update.Validate(); err == nil {
		t.Fatal("Core worker count was accepted on an unrelated operation")
	}
	other := request
	other.CoreWorkers = 3
	if other.Fingerprint() == request.Fingerprint() {
		t.Fatal("a retry with a different count would reuse the first operation")
	}
}

func coreWorkersEngine(t *testing.T, count int, executor CommandExecutor) (DeploymentEngine, Paths) {
	t.Helper()
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileWebOnly, Channel: ChannelStable, CoreWorkers: count, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Paths = paths
	return DeploymentEngine{Config: config, Executor: executor}, paths
}

func systemctlActions(calls [][]string, action string) []string {
	units := []string{}
	for _, call := range calls {
		if len(call) == 4 && call[1] == action && call[2] == "--now" {
			units = append(units, call[3])
		}
	}
	return units
}

func TestSetCoreWorkersStartsAddedWorkersOnly(t *testing.T) {
	executor := &recordingExecutor{}
	engine, paths := coreWorkersEngine(t, 0, executor)
	if err := engine.setCoreWorkers(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadInstallation(paths); err != nil || loaded.CoreWorkers != 3 {
		t.Fatalf("Core worker count was not saved: %d %v", loaded.CoreWorkers, err)
	}
	if enabled := systemctlActions(executor.calls, "enable"); !reflect.DeepEqual(enabled, []string{"nexus-core-worker@2.service", "nexus-core-worker@3.service"}) {
		t.Fatalf("unexpected enabled workers: %#v", enabled)
	}
	disabled := systemctlActions(executor.calls, "disable")
	if len(disabled) != MaxCoreWorkers-3 || containsString(disabled, "nexus-core-worker@3.service") {
		t.Fatalf("unexpected disabled workers: %#v", disabled)
	}
	for _, call := range executor.calls {
		if call[1] == "restart" {
			t.Fatalf("changing the count restarted a running unit: %#v", call)
		}
	}
}

func TestSetCoreWorkersStopsSurplusWorkers(t *testing.T) {
	executor := &recordingExecutor{}
	engine, paths := coreWorkersEngine(t, 3, executor)
	if err := engine.setCoreWorkers(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadInstallation(paths); err != nil || loaded.CoreWorkers != 1 {
		t.Fatalf("Core worker count was not saved: %d %v", loaded.CoreWorkers, err)
	}
	if enabled := systemctlActions(executor.calls, "enable"); len(enabled) != 0 {
		t.Fatalf("scaling down enabled workers: %#v", enabled)
	}
	disabled := systemctlActions(executor.calls, "disable")
	if !containsString(disabled, "nexus-core-worker@2.service") || !containsString(disabled, "nexus-core-worker@3.service") {
		t.Fatalf("surplus workers kept running: %#v", disabled)
	}
}

func TestSetCoreWorkersNeedsTheWorkerTemplate(t *testing.T) {
	executor := &missingUnitExecutor{missing: "nexus-core-worker@2.service"}
	engine, paths := coreWorkersEngine(t, 0, executor)
	err := engine.setCoreWorkers(context.Background(), 2)
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("expected an actionable missing-unit error, got %v", err)
	}
	for _, call := range executor.calls {
		if call[1] != "cat" {
			t.Fatalf("the unit check changed systemd state: %#v", call)
		}
	}
	if loaded, _ := LoadInstallation(paths); loaded.CoreWorkers != 0 {
		t.Fatalf("Core worker count changed: %d", loaded.CoreWorkers)
	}
}

type failingUnitExecutor struct {
	recordingExecutor
	action string
	unit   string
}

func (executor *failingUnitExecutor) Run(ctx context.Context, binary string, args []string) error {
	_ = executor.recordingExecutor.Run(ctx, binary, args)
	if len(args) > 0 && args[0] == executor.action && args[len(args)-1] == executor.unit {
		return errors.New("unit failed")
	}
	return nil
}

func TestSetCoreWorkersRestoresThePreviousCountOnFailure(t *testing.T) {
	executor := &failingUnitExecutor{action: "enable", unit: "nexus-core-worker@3.service"}
	engine, paths := coreWorkersEngine(t, 0, executor)
	if err := engine.setCoreWorkers(context.Background(), 3); err == nil {
		t.Fatal("a worker that could not start was accepted")
	}
	if loaded, _ := LoadInstallation(paths); loaded.CoreWorkers != 0 {
		t.Fatalf("Core worker count changed: %d", loaded.CoreWorkers)
	}
	calls := executor.calls
	last := calls[len(calls)-MaxCoreWorkers+1:]
	for _, call := range last {
		if call[1] != "disable" {
			t.Fatalf("the previous count was not restored: %#v", last)
		}
	}
	if !containsString(systemctlActions(last, "disable"), "nexus-core-worker@2.service") {
		t.Fatalf("the started worker kept running: %#v", last)
	}
}

func TestSetCoreWorkersRequiresCore(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileSubsOnly, Channel: ChannelStable, Components: map[Component]InstalledComponent{
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	config := DefaultConfig()
	config.Paths = paths
	engine := DeploymentEngine{Config: config, Executor: executor}
	if err := engine.setCoreWorkers(context.Background(), 2); err == nil || len(executor.calls) != 0 {
		t.Fatalf("a host without Core changed its workers: %v %#v", err, executor.calls)
	}
}

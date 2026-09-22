package updater

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRequiredSubsCanBeDisabledWithoutInvalidatingInstallation(t *testing.T) {
	for _, profile := range []Profile{ProfileFull, ProfileAppWebSubsRemoteDB, ProfileSubsOnly} {
		t.Run(string(profile), func(t *testing.T) {
			paths := NewTestPaths(t.TempDir())
			components := map[Component]InstalledComponent{
				ComponentSubs: {Installed: true, Enabled: false, CurrentRelease: "v1.0.0"},
			}
			if profile != ProfileSubsOnly {
				components[ComponentCore] = InstalledComponent{Installed: true, Enabled: true, CurrentRelease: "v1.0.0"}
			}
			state := InstallationState{Version: 1, Profile: profile, CurrentRelease: "v1.0.0", Components: components}
			if err := SaveInstallation(paths, state); err != nil {
				t.Fatalf("disabled Subs should remain installed: %v", err)
			}
			loaded, err := LoadInstallation(paths)
			if err != nil || loaded.Components[ComponentSubs].Enabled {
				t.Fatalf("disabled Subs state did not round-trip: %v", err)
			}
		})
	}
}

func TestRestartRejectsDisabledComponent(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Version: 1, Profile: ProfileSubsOnly, CurrentRelease: "v1.0.0", Components: map[Component]InstalledComponent{
		ComponentSubs: {Installed: true, Enabled: false, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	config := DefaultConfig()
	config.Paths = paths
	engine := DeploymentEngine{Config: config, Executor: executor}
	if err := engine.restartComponent(context.Background(), ComponentSubs); err == nil {
		t.Fatal("disabled component was restarted")
	}
	if len(executor.calls) != 0 {
		t.Fatalf("disabled component changed systemd state: %#v", executor.calls)
	}
}

func TestDisableRequiredSubsPersistsTheDisabledState(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Version: 1, Profile: ProfileFull, CurrentRelease: "v1.0.0", Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	config := DefaultConfig()
	config.Paths = paths
	engine := DeploymentEngine{Config: config, Executor: executor}
	if err := engine.setComponentEnabled(context.Background(), OperationState{Component: ComponentSubs}, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadInstallation(paths)
	if err != nil || loaded.Components[ComponentSubs].Enabled {
		t.Fatalf("Subs disable did not persist: %v", err)
	}
	if len(executor.calls) != 1 || executor.calls[0][1] != "disable" || executor.calls[0][3] != "nexus-subs.service" {
		t.Fatalf("unexpected systemd action: %#v", executor.calls)
	}
}

func TestDiscordRegistrationUsesFixedUnprivilegedUnit(t *testing.T) {
	executor := &recordingExecutor{}
	engine := DeploymentEngine{Config: DefaultConfig(), Executor: executor}
	if err := engine.registerDiscordCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || executor.calls[0][1] != "start" || executor.calls[0][2] != "nexus-discord-register.service" {
		t.Fatalf("unexpected registration command: %#v", executor.calls)
	}
	unit := systemdUnits()["/etc/systemd/system/nexus-discord-register.service"]
	for _, directive := range []string{"User=nexus-discord", "Group=nexus-discord", "EnvironmentFile=/etc/nexus/nexus-discord/environment", "ExecStart=/usr/bin/node /opt/nexus/nexus-discord/current/src/registerCommands.js"} {
		if !strings.Contains(unit, directive) {
			t.Fatalf("registration unit is missing %q", directive)
		}
	}
	mirror, err := os.ReadFile("../../systemd/nexus-discord-register.service")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(unit) != strings.TrimSpace(string(mirror)) {
		t.Fatal("review copy of the Discord registration unit differs from the embedded unit")
	}
}

package updater

import (
	"context"
	"encoding/json"
	"testing"
)

type recordingLauncher struct{ ids []string }

func (launcher *recordingLauncher) Launch(_ context.Context, id string) error {
	launcher.ids = append(launcher.ids, id)
	return nil
}

func TestServerAcceptsFixedMutationAndMakesRetryIdempotent(t *testing.T) {
	config := DefaultConfig()
	config.Paths = NewTestPaths(t.TempDir())
	state := InstallationState{Version: 1, Profile: ProfileWebOnly, CurrentRelease: "v1.2.3", Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.2.3"}, ComponentSubs: {}, ComponentDiscord: {Installed: true, Enabled: true, CurrentRelease: "v1.2.3"},
	}}
	if err := SaveInstallation(config.Paths, state); err != nil {
		t.Fatal(err)
	}
	launcher := &recordingLauncher{}
	server, err := newServer(config, launcher, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(OperationRestartComponent)
	request.Component = ComponentDiscord
	first := server.handleRequest(request)
	second := server.handleRequest(request)
	if first.Error != nil || !first.Accepted || second.Error != nil || !second.Accepted || len(launcher.ids) != 1 {
		t.Fatalf("idempotent mutation failed: first=%+v second=%+v launches=%v", first, second, launcher.ids)
	}
}

func TestStatusUsesRootInstallationState(t *testing.T) {
	config := DefaultConfig()
	config.Paths = NewTestPaths(t.TempDir())
	state := InstallationState{Version: 1, Profile: ProfileWebOnly, CurrentRelease: "v1.2.3", Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.2.3"}, ComponentSubs: {}, ComponentDiscord: {},
	}}
	if err := SaveInstallation(config.Paths, state); err != nil {
		t.Fatal(err)
	}
	server, err := newServer(config, &recordingLauncher{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := server.handleRequest(testRequest(OperationGetStatus))
	var status StatusData
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.InstalledReleaseID != "v1.2.3" || len(status.Components) != 3 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestServerRejectsArbitraryComponent(t *testing.T) {
	config := DefaultConfig()
	config.Paths = NewTestPaths(t.TempDir())
	server, err := newServer(config, &recordingLauncher{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(OperationRestartComponent)
	request.Component = "arbitrary-service"
	response := server.handleRequest(request)
	if response.Error == nil || response.Error.Code != "invalid_request" {
		t.Fatalf("arbitrary component accepted: %+v", response)
	}
}

func TestServerRejectsMutationsBeforeInstallationCompletes(t *testing.T) {
	config := DefaultConfig()
	config.Paths = NewTestPaths(t.TempDir())
	launcher := &recordingLauncher{}
	server, err := newServer(config, launcher, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(OperationUpdate)
	response := server.handleRequest(request)
	if response.Error == nil || response.Error.Code != "not_installed" || len(launcher.ids) != 0 {
		t.Fatalf("pre-install mutation was not rejected: response=%+v launches=%v", response, launcher.ids)
	}
}

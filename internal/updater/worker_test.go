package updater

import (
	"context"
	"testing"
	"time"
)

type recordingExecutor struct{ calls [][]string }

func (executor *recordingExecutor) Run(_ context.Context, binary string, args []string) error {
	executor.calls = append(executor.calls, append([]string{binary}, args...))
	return nil
}

type emptyReleaseProvider struct{}

func (emptyReleaseProvider) StableReleases(context.Context) ([]ReleaseInfo, error) {
	return []ReleaseInfo{}, nil
}
func (emptyReleaseProvider) Download(context.Context, string, string, string, string, int64, func(int64, int64)) error {
	return nil
}

func TestWorkerUsesOnlyFixedServiceUnit(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Version: 1, Profile: ProfileFull, CurrentRelease: "v1.0.0", Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"}, ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"}, ComponentDiscord: {},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	writeTestHealthDocument(t, paths, ComponentSubs, "v1.0.0", time.Now().UTC())
	store, _ := NewOperationStore(paths)
	locks, _ := NewLockSet(paths)
	request := testRequest(OperationRestartComponent)
	request.Component = ComponentSubs
	operation, _, err := store.CreateOrGet(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{}
	config := DefaultConfig()
	config.Paths = paths
	if err := runWorker(context.Background(), config, store, locks, operation.ID, executor, emptyReleaseProvider{}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("expected restart and a successful health check, got %#v", executor.calls)
	}
	if executor.calls[0][1] != "restart" || executor.calls[0][2] != "nexus-subs.service" {
		t.Fatalf("unexpected service command: %#v", executor.calls[0])
	}
}

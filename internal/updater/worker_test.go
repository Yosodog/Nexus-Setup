package updater

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type recordingExecutor struct{ calls [][]string }

func (executor *recordingExecutor) Run(_ context.Context, binary string, args []string) error {
	executor.calls = append(executor.calls, append([]string{binary}, args...))
	return nil
}

type emptyReleaseProvider struct{}

func (emptyReleaseProvider) Releases(context.Context, string, Channel) ([]ReleaseInfo, error) {
	return []ReleaseInfo{}, nil
}
func (emptyReleaseProvider) Manifest(context.Context, Component, string) (ReleaseManifest, error) {
	return ReleaseManifest{}, nil
}
func (emptyReleaseProvider) Download(context.Context, string, string, string, string, int64, func(int64, int64)) error {
	return nil
}

func TestWorkerUsesOnlyFixedServiceUnit(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileFull, Channel: ChannelStable, Components: map[Component]InstalledComponent{
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
	if len(executor.calls) != 3 {
		t.Fatalf("expected a unit check, restart, and a successful health check, got %#v", executor.calls)
	}
	if executor.calls[0][1] != "cat" || executor.calls[0][2] != "nexus-subs.service" {
		t.Fatalf("unexpected unit check: %#v", executor.calls[0])
	}
	if executor.calls[1][1] != "restart" || executor.calls[1][2] != "nexus-subs.service" {
		t.Fatalf("unexpected service command: %#v", executor.calls[1])
	}
}

func TestFixedCommandErrorIncludesOutputTail(t *testing.T) {
	err := runFixedCommand(context.Background(), "/bin/sh", []string{"-c", "echo starting; echo 'runuser: cannot set user id: Operation not permitted' >&2; exit 1"})
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	if want := "exit status 1: starting runuser: cannot set user id: Operation not permitted"; err.Error() != want {
		t.Fatalf("unexpected error %q, want %q", err, want)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("the exit status is no longer wrapped: %#v", err)
	}
}

func TestFixedCommandErrorKeepsOnlyTheEndOfLongOutput(t *testing.T) {
	err := runFixedCommand(context.Background(), "/bin/sh", []string{"-c", "i=0; while [ $i -lt 2000 ]; do echo line $i; i=$((i+1)); done; echo final reason >&2; exit 3"})
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "exit status 3: ...") || !strings.HasSuffix(message, "line 1999 final reason") {
		t.Fatalf("unexpected error %q", message)
	}
	if len(message) > len("exit status 3: ...")+commandOutputSummaryBytes {
		t.Fatalf("error is longer than the summary limit: %d bytes", len(message))
	}
}

func TestFixedCommandErrorWithoutOutputIsUnchanged(t *testing.T) {
	err := runFixedCommand(context.Background(), "/bin/sh", []string{"-c", "exit 2"})
	if err == nil || err.Error() != "exit status 2" {
		t.Fatalf("unexpected error %v", err)
	}
}

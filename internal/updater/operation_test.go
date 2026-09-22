package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOperationStoreIsAtomicAndIdempotent(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	store, err := NewOperationStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(OperationUpdate)
	created, existing, err := store.CreateOrGet(context.Background(), request)
	if err != nil || existing {
		t.Fatalf("create operation: state=%+v existing=%v err=%v", created, existing, err)
	}
	retried, existing, err := store.CreateOrGet(context.Background(), request)
	if err != nil || !existing || retried.ID != created.ID {
		t.Fatalf("retry was not idempotent: state=%+v existing=%v err=%v", retried, existing, err)
	}
	changed := request
	changed.Source = "gui"
	if _, _, err := store.CreateOrGet(context.Background(), changed); err == nil {
		t.Fatal("request id reuse with different target was accepted")
	}
	updated, err := store.Update(created.ID, func(state *OperationState) error {
		state.Status = OperationRunning
		state.Phase = PhaseValidating
		return nil
	})
	if err != nil || updated.Status != OperationRunning {
		t.Fatalf("update failed: %+v %v", updated, err)
	}
	loaded, err := store.Load(created.ID)
	if err != nil || loaded.Phase != PhaseValidating {
		t.Fatalf("load failed: %+v %v", loaded, err)
	}
	stateFile := filepath.Join(paths.OperationsDir, created.ID, "state.json")
	info, err := os.Stat(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions are %o, want 600", info.Mode().Perm())
	}
}

func TestOperationStoreRejectsCorruptState(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	store, err := NewOperationStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(OperationGetStatus)
	if _, _, err := store.CreateOrGet(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(paths.OperationsDir, request.ID, "state.json")
	if err := os.WriteFile(stateFile, []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(request.ID); err == nil {
		t.Fatal("corrupt operation state was accepted")
	}
}

func TestOperationStoreAcceptsOnlyOneHostMutation(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	store, err := NewOperationStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	first := testRequest(OperationUpdate)
	if _, _, err := store.CreateOrGet(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testRequest(OperationCleanup)
	second.ID = "123e4567-e89b-42d3-a456-426614174000"
	second.OperationID = second.ID
	if _, _, err := store.CreateOrGet(context.Background(), second); err == nil {
		t.Fatal("expected a second queued host mutation to be rejected")
	}
	if _, err := store.Update(first.OperationID, func(state *OperationState) error {
		state.Status = OperationCompleted
		state.Phase = PhaseCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrGet(context.Background(), second); err != nil {
		t.Fatalf("terminal history blocked a new operation: %v", err)
	}
}

func TestFileLockSerializesProcesses(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	locks, err := NewLockSet(paths)
	if err != nil {
		t.Fatal(err)
	}
	first, err := locks.AcquireGlobal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := locks.AcquireGlobal(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want deadline", err)
	}
}

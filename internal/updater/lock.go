package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// FileLock is a process- and host-wide advisory lock. All updater mutations
// acquire the global lock; component operations additionally acquire their
// component lock. The lock file itself is never removed, which avoids a
// pathname replacement race between competing workers.
type FileLock struct {
	file *os.File
}

func acquireFileLock(ctx context.Context, path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, err
	}

	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &FileLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (lock *FileLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockError := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeError := lock.file.Close()
	if unlockError != nil {
		return unlockError
	}
	return closeError
}

// LockSet serializes host mutations and, where applicable, component work.
type LockSet struct {
	paths Paths
}

func NewLockSet(paths Paths) (*LockSet, error) {
	if err := paths.Validate(); err != nil {
		return nil, err
	}
	return &LockSet{paths: paths}, nil
}

func (locks *LockSet) AcquireGlobal(ctx context.Context) (*FileLock, error) {
	return acquireFileLock(ctx, filepath.Join(locks.paths.LocksDir, "update.lock"))
}

func (locks *LockSet) AcquireComponent(ctx context.Context, component Component) (*FileLock, error) {
	if !validComponent(component) {
		return nil, fmt.Errorf("invalid component %q", component)
	}
	return acquireFileLock(ctx, filepath.Join(locks.paths.LocksDir, "components", string(component)+".lock"))
}

func (locks *LockSet) AcquireOperation(ctx context.Context, operationID string) (*FileLock, error) {
	if !uuidPattern.MatchString(operationID) {
		return nil, errors.New("invalid operation id")
	}
	return acquireFileLock(ctx, filepath.Join(locks.paths.LocksDir, "operations", operationID+".lock"))
}

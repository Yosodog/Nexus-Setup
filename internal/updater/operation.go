package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type OperationStatus string

const (
	OperationQueued     OperationStatus = "queued"
	OperationRunning    OperationStatus = "running"
	OperationCompleted  OperationStatus = "completed"
	OperationFailed     OperationStatus = "failed"
	OperationRolledBack OperationStatus = "rolled_back"
	OperationRecovery   OperationStatus = "recovery_required"
)

type OperationPhase string

const (
	PhaseAccepted       OperationPhase = "accepted"
	PhaseLocking        OperationPhase = "locking"
	PhaseValidating     OperationPhase = "validating"
	PhaseDownloading    OperationPhase = "downloading"
	PhaseInspecting     OperationPhase = "inspecting"
	PhaseStaging        OperationPhase = "staging"
	PhaseMaintenance    OperationPhase = "entering_maintenance"
	PhaseMigrating      OperationPhase = "migrating"
	PhaseActivating     OperationPhase = "activating"
	PhaseRestarting     OperationPhase = "restarting"
	PhaseHealthChecking OperationPhase = "health_checking"
	PhaseFinalizing     OperationPhase = "finalizing"
	PhaseCompleted      OperationPhase = "completed"
	PhaseCleaningUp     OperationPhase = "cleaning_up"
	PhaseRollingBack    OperationPhase = "rolling_back_code"
	PhaseRollbackFailed OperationPhase = "rollback_failed"
	PhaseRecovery       OperationPhase = "recovery_required"
)

// OperationState is the durable, sanitized state shared by CLI and GUI. It
// contains no request payload, command output, URLs, environment values, or
// secret material.
type OperationState struct {
	Version          int             `json:"version"`
	ID               string          `json:"id"`
	Operation        Operation       `json:"operation"`
	Component        Component       `json:"component,omitempty"`
	Source           string          `json:"source,omitempty"`
	Fingerprint      string          `json:"fingerprint"`
	HasConfiguration bool            `json:"has_configuration,omitempty"`
	Releases         []string        `json:"releases,omitempty"`
	Status           OperationStatus `json:"status"`
	Phase            OperationPhase  `json:"phase"`
	ProgressBytes    int64           `json:"progress_bytes,omitempty"`
	ProgressTotal    int64           `json:"progress_total,omitempty"`
	ReclaimedBytes   int64           `json:"reclaimed_bytes,omitempty"`
	ErrorCode        string          `json:"error_code,omitempty"`
	ErrorMessage     string          `json:"error_message,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	FinishedAt       *time.Time      `json:"finished_at,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type OperationStore struct {
	paths Paths
	mu    sync.Mutex
}

func NewOperationStore(paths Paths) (*OperationStore, error) {
	if err := paths.Validate(); err != nil {
		return nil, err
	}
	return &OperationStore{paths: paths}, nil
}

func NewOperationID() (string, error) {
	var bytes [16]byte
	if _, err := io.ReadFull(rand.Reader, bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(bytes[0:4]),
		hex.EncodeToString(bytes[4:6]),
		hex.EncodeToString(bytes[6:8]),
		hex.EncodeToString(bytes[8:10]),
		hex.EncodeToString(bytes[10:16])), nil
}

func (store *OperationStore) operationPath(id string) (string, error) {
	if !uuidPattern.MatchString(id) {
		return "", errors.New("invalid operation id")
	}
	return filepath.Join(store.paths.OperationsDir, id, "state.json"), nil
}

// CreateOrGet creates an operation atomically. An identical retry returns the
// existing operation; reusing an id with different safe fields is rejected.
func (store *OperationStore) CreateOrGet(ctx context.Context, request Request) (OperationState, bool, error) {
	if ctx == nil {
		return OperationState{}, false, errors.New("context is required")
	}
	select {
	case <-ctx.Done():
		return OperationState{}, false, ctx.Err()
	default:
	}
	if err := request.Validate(); err != nil {
		return OperationState{}, false, err
	}
	operationID := request.IdempotencyID()
	if !uuidPattern.MatchString(operationID) {
		return OperationState{}, false, errors.New("operation id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := os.MkdirAll(store.paths.OperationsDir, 0700); err != nil {
		return OperationState{}, false, err
	}
	path, err := store.operationPath(operationID)
	if err != nil {
		return OperationState{}, false, err
	}
	if existing, readErr := store.loadLocked(path); readErr == nil {
		if existing.Fingerprint != request.Fingerprint() {
			return OperationState{}, false, errors.New("request id was already used for a different operation")
		}
		return existing, true, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return OperationState{}, false, readErr
	}
	entries, err := os.ReadDir(store.paths.OperationsDir)
	if err != nil {
		return OperationState{}, false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == operationID {
			continue
		}
		other, loadErr := store.loadLocked(filepath.Join(store.paths.OperationsDir, entry.Name(), "state.json"))
		if loadErr != nil {
			return OperationState{}, false, loadErr
		}
		if other.Status == OperationQueued || other.Status == OperationRunning || (other.Status == OperationRecovery && request.Operation != OperationRollback) {
			return OperationState{}, false, fmt.Errorf("operation %s must finish or be recovered first", other.ID)
		}
		if request.Operation == OperationRestartComponent && other.Operation == OperationRestartComponent && other.Component == request.Component && time.Since(other.UpdatedAt) < time.Minute {
			return OperationState{}, false, errors.New("this component was restarted recently; wait one minute before restarting it again")
		}
	}

	now := time.Now().UTC()
	state := OperationState{
		Version:          1,
		ID:               operationID,
		Operation:        request.Operation,
		Component:        request.Component,
		Source:           request.Source,
		Fingerprint:      request.Fingerprint(),
		HasConfiguration: len(request.Configuration) > 0,
		Status:           OperationQueued,
		Phase:            PhaseAccepted,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := atomicWriteJSON(path, state, 0600); err != nil {
		return OperationState{}, false, err
	}
	return state, false, nil
}

func (store *OperationStore) ResolveRecovery(resolutionID string) error {
	if !uuidPattern.MatchString(resolutionID) {
		return errors.New("resolution operation id is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := os.ReadDir(store.paths.OperationsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == resolutionID {
			continue
		}
		path := filepath.Join(store.paths.OperationsDir, entry.Name(), "state.json")
		state, loadErr := store.loadLocked(path)
		if loadErr != nil {
			return loadErr
		}
		if state.Status != OperationRecovery {
			continue
		}
		finished := time.Now().UTC()
		state.Status = OperationRolledBack
		state.Phase = PhaseRollingBack
		state.ErrorCode = "recovered_by_rollback"
		state.ErrorMessage = "a later rollback restored the recorded previous release"
		state.FinishedAt = &finished
		state.UpdatedAt = finished
		if err := atomicWriteJSON(path, state, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (store *OperationStore) Load(id string) (OperationState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	path, err := store.operationPath(id)
	if err != nil {
		return OperationState{}, err
	}
	return store.loadLocked(path)
}

func (store *OperationStore) loadLocked(path string) (OperationState, error) {
	file, err := os.Open(path)
	if err != nil {
		return OperationState{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 256*1024))
	decoder.DisallowUnknownFields()
	var state OperationState
	if err := decoder.Decode(&state); err != nil {
		return OperationState{}, fmt.Errorf("decode operation state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return OperationState{}, errors.New("operation state contains trailing data")
	}
	expectedID := filepath.Base(filepath.Dir(path))
	if state.ID != expectedID {
		return OperationState{}, errors.New("operation state path does not match its identity")
	}
	if err := validateOperationState(state); err != nil {
		return OperationState{}, err
	}
	return state, nil
}

func (store *OperationStore) Update(id string, update func(*OperationState) error) (OperationState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	path, err := store.operationPath(id)
	if err != nil {
		return OperationState{}, err
	}
	state, err := store.loadLocked(path)
	if err != nil {
		return OperationState{}, err
	}
	if err := update(&state); err != nil {
		return OperationState{}, err
	}
	if err := validateOperationState(state); err != nil {
		return OperationState{}, err
	}
	state.UpdatedAt = time.Now().UTC()
	if err := atomicWriteJSON(path, state, 0600); err != nil {
		return OperationState{}, err
	}
	return state, nil
}

func (store *OperationStore) List(limit int) ([]OperationState, error) {
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("limit must be between 1 and 1000")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := os.ReadDir(store.paths.OperationsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []OperationState{}, nil
		}
		return nil, err
	}
	states := make([]OperationState, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(store.paths.OperationsDir, entry.Name(), "state.json")
		state, loadErr := store.loadLocked(path)
		if loadErr != nil {
			return nil, loadErr
		}
		states = append(states, state)
	}
	sort.Slice(states, func(left, right int) bool {
		return states[left].UpdatedAt.After(states[right].UpdatedAt)
	})
	if len(states) > limit {
		states = states[:limit]
	}
	return states, nil
}

func validateOperationState(state OperationState) error {
	if state.Version != 1 || !uuidPattern.MatchString(state.ID) || !validOperation(state.Operation) {
		return errors.New("invalid operation state identity")
	}
	if state.Component != "" && !validComponent(state.Component) {
		return errors.New("invalid operation state component")
	}
	if state.Status != OperationQueued && state.Status != OperationRunning && state.Status != OperationCompleted && state.Status != OperationFailed && state.Status != OperationRolledBack && state.Status != OperationRecovery {
		return errors.New("invalid operation state status")
	}
	if strings.TrimSpace(string(state.Phase)) == "" {
		return errors.New("operation state phase is required")
	}
	for _, release := range state.Releases {
		if _, err := ParseVersion(release); err != nil {
			return errors.New("invalid operation state release")
		}
	}
	return nil
}

func atomicWriteJSON(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

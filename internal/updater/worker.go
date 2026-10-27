package updater

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

type CommandExecutor interface {
	Run(context.Context, string, []string) error
}

type OSCommandExecutor struct{}

func (OSCommandExecutor) Run(ctx context.Context, binary string, arguments []string) error {
	return runFixedCommand(ctx, binary, arguments)
}

func runFixedCommand(ctx context.Context, binary string, arguments []string) error {
	return runFixedCommandInDirectory(ctx, "", binary, arguments)
}

func runFixedCommandInDirectory(ctx context.Context, directory, binary string, arguments []string) error {
	if err := validateFixedCommand(binary, arguments); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_COLORS=0", "DEBIAN_FRONTEND=noninteractive"}
	command.Dir = directory
	command.Stdin = nil
	output := &tailBuffer{limit: commandOutputCaptureBytes}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if summary := output.summary(); summary != "" {
			return fmt.Errorf("%w: %s", err, summary)
		}
		return err
	}
	return nil
}

const (
	commandOutputCaptureBytes = 4096
	commandOutputSummaryBytes = 200
)

// tailBuffer keeps the last bytes a command wrote, so a failure can say why
// without holding the full output of a long apt-get or migration run. Callers
// never pass credentials as arguments, and only the output is reported.
type tailBuffer struct {
	limit int
	data  []byte
}

func (buffer *tailBuffer) Write(p []byte) (int, error) {
	buffer.data = append(buffer.data, p...)
	if excess := len(buffer.data) - buffer.limit; excess > 0 {
		buffer.data = append(buffer.data[:0], buffer.data[excess:]...)
	}
	return len(p), nil
}

// summary flattens the captured output onto one line and keeps its end, which
// is where commands print the reason they failed. Operation errors are cut to
// 256 bytes, so the summary stays short enough to follow "exit status N".
func (buffer *tailBuffer) summary() string {
	text := strings.ToValidUTF8(string(buffer.data), "")
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= commandOutputSummaryBytes {
		return text
	}
	text = text[len(text)-commandOutputSummaryBytes:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return "..." + text
}

func validateFixedCommand(binary string, arguments []string) error {
	if binary == "" || !strings.HasPrefix(binary, "/") || strings.ContainsRune(binary, '\x00') {
		return errors.New("executable path is invalid")
	}
	for _, argument := range arguments {
		if strings.ContainsRune(argument, '\x00') {
			return errors.New("command argument contains a NUL byte")
		}
	}
	return nil
}

var ErrRecoveryRequired = errors.New("operation requires recovery")
var ErrRolledBack = errors.New("update failed health checks and code was rolled back")

func RunWorker(ctx context.Context, config Config, store *OperationStore, locks *LockSet, operationID string, executor CommandExecutor) error {
	return runWorker(ctx, config, store, locks, operationID, executor, NewGitHubReleaseClient())
}

func runWorker(ctx context.Context, config Config, store *OperationStore, locks *LockSet, operationID string, executor CommandExecutor, releases ReleaseProvider) error {
	if !uuidPattern.MatchString(operationID) {
		return errors.New("invalid operation id")
	}
	if executor == nil {
		executor = OSCommandExecutor{}
	}
	operationLock, err := locks.AcquireOperation(ctx, operationID)
	if err != nil {
		return err
	}
	defer operationLock.Close()
	state, err := store.Load(operationID)
	if err != nil {
		return err
	}
	if operationTerminal(state.Status) {
		return nil
	}
	if state.Status == OperationRunning {
		return failOperationWithStatus(store, operationID, OperationRecovery, PhaseRecovery, "interrupted_operation", "the previous worker ended before recording a safe terminal state")
	}
	started := time.Now().UTC()
	if _, err := store.Update(operationID, func(current *OperationState) error {
		current.Status = OperationRunning
		current.Phase = PhaseLocking
		current.StartedAt = &started
		current.ErrorCode = ""
		current.ErrorMessage = ""
		return nil
	}); err != nil {
		return err
	}
	globalLock, err := locks.AcquireGlobal(ctx)
	if err != nil {
		_ = failOperationWithStatus(store, operationID, OperationFailed, PhaseCleaningUp, "lock_timeout", "the updater is busy")
		return err
	}
	defer globalLock.Close()
	var componentLock *FileLock
	if state.Component != "" {
		componentLock, err = locks.AcquireComponent(ctx, state.Component)
		if err != nil {
			_ = failOperationWithStatus(store, operationID, OperationFailed, PhaseCleaningUp, "component_lock_timeout", "the component is busy")
			return err
		}
		defer componentLock.Close()
	}
	if _, err := store.Update(operationID, func(current *OperationState) error {
		current.Phase = PhaseValidating
		return nil
	}); err != nil {
		return err
	}
	configuration := map[string]string{}
	if state.HasConfiguration {
		configuration, err = consumeOperationConfiguration(config.Paths, operationID)
		if err != nil {
			_ = failOperationWithStatus(store, operationID, OperationFailed, PhaseCleaningUp, "configuration_required", "write-only component configuration must be submitted again")
			return err
		}
	}
	engine := DeploymentEngine{Config: config, Store: store, Releases: releases, Executor: executor}
	operationError := engine.Run(ctx, state, configuration)
	if operationError != nil {
		switch {
		case errors.Is(operationError, ErrRolledBack):
			_ = failOperationWithStatus(store, operationID, OperationRolledBack, PhaseRollingBack, "health_check_failed", "the previous code release was restored")
		case errors.Is(operationError, ErrRecoveryRequired):
			_ = failOperationWithStatus(store, operationID, OperationRecovery, PhaseRecovery, "recovery_required", "manual recovery is required")
		default:
			_ = failOperationWithStatus(store, operationID, OperationFailed, PhaseCleaningUp, "operation_failed", safeProtocolMessage(operationError))
		}
		return operationError
	}
	_, err = store.Update(operationID, func(current *OperationState) error {
		current.Status = OperationCompleted
		current.Phase = PhaseCompleted
		finished := time.Now().UTC()
		current.FinishedAt = &finished
		return nil
	})
	return err
}

func operationTerminal(status OperationStatus) bool {
	return status == OperationCompleted || status == OperationFailed || status == OperationRolledBack || status == OperationRecovery
}

// componentServiceUnit is the component's primary unit.
func componentServiceUnit(component Component) (string, error) {
	units, err := componentServiceUnits(component, hostServices{})
	if err != nil {
		return "", err
	}
	return units[0], nil
}

// componentServiceUnits lists every long-running unit a component needs on
// this host; restarts, health checks, and status cover all of them.
func componentServiceUnits(component Component, services hostServices) ([]string, error) {
	switch component {
	case ComponentCore:
		return append(coreWorkerUnits(services), corePHPFPMUnit), nil
	case ComponentSubs:
		return []string{"nexus-subs.service"}, nil
	case ComponentDiscord:
		return []string{"nexus-discord.service"}, nil
	default:
		return nil, errors.New("unsupported component")
	}
}

func failOperationWithStatus(store *OperationStore, id string, status OperationStatus, phase OperationPhase, code, message string) error {
	_, err := store.Update(id, func(current *OperationState) error {
		current.Status = status
		current.Phase = phase
		current.ErrorCode = code
		current.ErrorMessage = normalizeSafeText(message)
		finished := time.Now().UTC()
		current.FinishedAt = &finished
		return nil
	})
	return err
}

func normalizeSafeText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		return value[:256]
	}
	return value
}

func updatePhase(store *OperationStore, id string, phase OperationPhase) error {
	_, err := store.Update(id, func(current *OperationState) error {
		if current.Status != OperationRunning {
			return fmt.Errorf("operation is not running")
		}
		current.Phase = phase
		return nil
	})
	return err
}

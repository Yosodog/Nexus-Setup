package updater

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxComponentArtifact  = int64(2 * 1024 * 1024 * 1024)
	maxUpdaterArtifact    = int64(128 * 1024 * 1024)
	minimumReleaseSpace   = uint64(1024 * 1024 * 1024)
	minimumComponentSpace = uint64(512 * 1024 * 1024)
)

type DeploymentEngine struct {
	Config   Config
	Store    *OperationStore
	Releases ReleaseProvider
	Executor CommandExecutor
}

func (engine DeploymentEngine) Run(ctx context.Context, operation OperationState, configuration map[string]string) error {
	switch operation.Operation {
	case OperationUpdate:
		return engine.update(ctx, operation)
	case OperationRollback:
		return engine.rollback(ctx, operation)
	case OperationCleanup:
		return engine.cleanup(operation)
	case OperationInstallComponent:
		return engine.installComponent(ctx, operation, configuration)
	case OperationEnableComponent:
		return engine.setComponentEnabled(ctx, operation, true)
	case OperationDisableComponent:
		return engine.setComponentEnabled(ctx, operation, false)
	case OperationRestartComponent:
		return engine.restartComponent(ctx, operation.Component)
	default:
		return fmt.Errorf("unsupported worker operation %q", operation.Operation)
	}
}

func (engine DeploymentEngine) update(ctx context.Context, operation OperationState) error {
	installation, err := LoadInstallation(engine.Config.Paths)
	if err != nil {
		return errors.New("Nexus is not installed")
	}
	if installation.Profile == ProfileDatabaseOnly {
		return errors.New("database-only hosts do not have a local Nexus application release to update")
	}
	if err := ensureFreeSpace(engine.Config.Paths.StateDir, minimumReleaseSpace); err != nil {
		return err
	}
	releases, err := engine.Releases.StableReleases(ctx)
	if err != nil {
		return fmt.Errorf("discover GitHub releases: %w", err)
	}
	updates, err := UpdatesAfter(releases, installation.CurrentRelease)
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		if BuildVersion != installation.CurrentRelease {
			if err := engine.updateSelf(ctx, operation.ID, installation.CurrentRelease); err != nil {
				return err
			}
			_ = engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"try-restart", "nexus-updater.service"})
		}
		return nil
	}
	releaseIDs := make([]string, 0, len(updates))
	for _, release := range updates {
		releaseIDs = append(releaseIDs, release.Tag)
	}
	if _, err := engine.Store.Update(operation.ID, func(current *OperationState) error {
		current.Releases = releaseIDs
		return nil
	}); err != nil {
		return err
	}
	for _, release := range updates {
		if err := engine.applyRelease(ctx, operation.ID, &installation, release.Tag); err != nil {
			return err
		}
	}
	if err := engine.updateSelf(ctx, operation.ID, updates[len(updates)-1].Tag); err != nil {
		return err
	}
	_ = engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"try-restart", "nexus-updater.service"})
	return nil
}

func (engine DeploymentEngine) applyRelease(ctx context.Context, operationID string, installation *InstallationState, release string) error {
	if err := engine.phase(operationID, PhaseDownloading); err != nil {
		return err
	}
	installed := installedComponents(*installation)
	if len(installed) == 0 {
		return errors.New("this host has no managed application components to update")
	}
	running := enabledComponents(*installation)
	hasCore := installation.Components[ComponentCore].Installed
	for _, component := range installed {
		if err := engine.stageComponent(ctx, operationID, component, release); err != nil {
			return err
		}
	}
	if hasCore {
		if err := engine.phase(operationID, PhaseMaintenance); err != nil {
			return err
		}
		if err := engine.runArtisan(ctx, installation.CurrentRelease, "down", "--retry=60"); err != nil {
			return fmt.Errorf("enter maintenance mode: %w", err)
		}
		if err := engine.phase(operationID, PhaseMigrating); err != nil {
			if upErr := engine.runArtisan(ctx, installation.CurrentRelease, "up"); upErr != nil {
				return ErrRecoveryRequired
			}
			return err
		}
		if err := engine.prepareCoreRelease(release); err != nil {
			if upErr := engine.runArtisan(ctx, installation.CurrentRelease, "up"); upErr != nil {
				return ErrRecoveryRequired
			}
			return err
		}
		if err := engine.runArtisan(ctx, release, "migrate", "--force"); err != nil {
			if upErr := engine.runArtisan(ctx, installation.CurrentRelease, "up"); upErr != nil {
				return ErrRecoveryRequired
			}
			return fmt.Errorf("database migration failed: %w", err)
		}
	}
	if err := engine.phase(operationID, PhaseActivating); err != nil {
		if hasCore {
			if upErr := engine.runArtisan(ctx, installation.CurrentRelease, "up"); upErr != nil {
				return ErrRecoveryRequired
			}
		}
		return err
	}
	oldRelease := installation.CurrentRelease
	for _, component := range installed {
		if err := atomicSwitchRelease(engine.Config.Paths, component, release); err != nil {
			if rollbackErr := engine.switchComponents(installed, oldRelease); rollbackErr != nil {
				return ErrRecoveryRequired
			}
			if hasCore {
				if upErr := engine.runArtisan(ctx, oldRelease, "up"); upErr != nil {
					return ErrRecoveryRequired
				}
			}
			return err
		}
	}
	previousState := *installation
	installation.PreviousRelease = oldRelease
	installation.CurrentRelease = release
	for component, record := range installation.Components {
		if record.Installed {
			record.PreviousRelease = record.CurrentRelease
			record.CurrentRelease = release
			installation.Components[component] = record
		}
	}
	if err := SaveInstallation(engine.Config.Paths, *installation); err != nil {
		if switchErr := engine.switchComponents(installed, oldRelease); switchErr != nil {
			return ErrRecoveryRequired
		}
		*installation = previousState
		if hasCore {
			if upErr := engine.runArtisan(ctx, oldRelease, "up"); upErr != nil {
				return ErrRecoveryRequired
			}
		}
		return err
	}
	if err := engine.phase(operationID, PhaseRestarting); err != nil {
		return engine.rollbackFailedRelease(ctx, installation, previousState, installed, running, hasCore)
	}
	if err := engine.restartComponents(ctx, running); err != nil {
		return engine.rollbackFailedRelease(ctx, installation, previousState, installed, running, hasCore)
	}
	if err := engine.phase(operationID, PhaseHealthChecking); err != nil {
		return engine.rollbackFailedRelease(ctx, installation, previousState, installed, running, hasCore)
	}
	if err := engine.healthCheck(ctx, running, release); err != nil {
		return engine.rollbackFailedRelease(ctx, installation, previousState, installed, running, hasCore)
	}
	if installation.Components[ComponentDiscord].Installed && installation.Components[ComponentDiscord].Enabled {
		if err := engine.registerDiscordCommands(ctx); err != nil {
			return engine.rollbackFailedRelease(ctx, installation, previousState, installed, running, hasCore)
		}
	}
	if hasCore {
		if err := engine.runArtisan(ctx, release, "up"); err != nil {
			return ErrRecoveryRequired
		}
	}
	return engine.phase(operationID, PhaseFinalizing)
}

func (engine DeploymentEngine) rollbackFailedRelease(ctx context.Context, installation *InstallationState, previous InstallationState, components, running []Component, hasCore bool) error {
	if previous.CurrentRelease == "" {
		return ErrRecoveryRequired
	}
	if err := engine.switchComponents(components, previous.CurrentRelease); err != nil {
		return ErrRecoveryRequired
	}
	if err := SaveInstallation(engine.Config.Paths, previous); err != nil {
		return ErrRecoveryRequired
	}
	*installation = previous
	if err := engine.restartComponents(ctx, running); err != nil {
		return ErrRecoveryRequired
	}
	if err := engine.healthCheck(ctx, running, previous.CurrentRelease); err != nil {
		return ErrRecoveryRequired
	}
	if previous.Components[ComponentDiscord].Installed && previous.Components[ComponentDiscord].Enabled {
		if err := engine.registerDiscordCommands(ctx); err != nil {
			return ErrRecoveryRequired
		}
	}
	if hasCore {
		if err := engine.runArtisan(ctx, previous.CurrentRelease, "up"); err != nil {
			return ErrRecoveryRequired
		}
	}
	return ErrRolledBack
}

func installedComponents(state InstallationState) []Component {
	components := make([]Component, 0, 3)
	for _, component := range []Component{ComponentCore, ComponentSubs, ComponentDiscord} {
		if state.Components[component].Installed {
			components = append(components, component)
		}
	}
	return components
}

func enabledComponents(state InstallationState) []Component {
	components := make([]Component, 0, 3)
	for _, component := range []Component{ComponentCore, ComponentSubs, ComponentDiscord} {
		record := state.Components[component]
		if record.Installed && record.Enabled {
			components = append(components, component)
		}
	}
	return components
}

func (engine DeploymentEngine) stageComponent(ctx context.Context, operationID string, component Component, release string) error {
	target, err := componentReleaseDirectory(engine.Config.Paths, component, release)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && info.IsDir() {
		return ValidateArtifactIdentity(target, component, release)
	}
	repository, asset, err := componentReleaseAsset(component)
	if err != nil {
		return err
	}
	downloadDirectory := filepath.Join(engine.Config.Paths.DownloadsDir, operationID)
	if err := os.MkdirAll(downloadDirectory, 0700); err != nil {
		return err
	}
	artifact := filepath.Join(downloadDirectory, release+"-"+asset+".partial")
	_ = os.Remove(artifact)
	err = engine.Releases.Download(ctx, repository, release, asset, artifact, maxComponentArtifact, func(downloaded, total int64) {
		if engine.Store != nil {
			_, _ = engine.Store.Update(operationID, func(current *OperationState) error {
				current.ProgressBytes = downloaded
				current.ProgressTotal = total
				return nil
			})
		}
	})
	if err != nil {
		return err
	}
	if err := engine.phase(operationID, PhaseInspecting); err != nil {
		return err
	}
	file, err := os.Open(artifact)
	if err != nil {
		return err
	}
	staging := filepath.Join(engine.Config.Paths.StagingDir, operationID, release, string(component))
	_ = os.RemoveAll(staging)
	_, extractErr := ExtractTarGz(file, staging, DefaultArchiveLimits())
	closeErr := file.Close()
	if extractErr != nil {
		return extractErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ValidateArtifactIdentity(staging, component, release); err != nil {
		return err
	}
	if err := engine.phase(operationID, PhaseStaging); err != nil {
		return err
	}
	if err := installStagedTree(staging, target); err != nil {
		return err
	}
	if err := os.Remove(artifact); err != nil {
		return err
	}
	_ = os.RemoveAll(staging)
	_ = os.Remove(downloadDirectory)
	return nil
}

func componentReleaseAsset(component Component) (string, string, error) {
	switch component {
	case ComponentCore:
		return coreRepository, coreAsset, nil
	case ComponentSubs:
		return subsRepository, subsAsset, nil
	case ComponentDiscord:
		return discordRepository, discordAsset, nil
	default:
		return "", "", errors.New("unsupported component")
	}
}

func installStagedTree(source, target string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) {
		return errors.New("release paths must be absolute")
	}
	if _, err := os.Lstat(target); err == nil {
		return errors.New("release target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	partial := target + ".installing"
	_ = os.RemoveAll(partial)
	if err := copyRegularTree(source, partial); err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	if err := os.Rename(partial, target); err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	return nil
}

func copyRegularTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("staged release contains a symlink")
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !entry.Type().IsRegular() {
			return errors.New("staged release contains a special file")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		input, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		openedInfo, err := input.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() {
			_ = input.Close()
			return errors.New("staged source changed while it was being copied")
		}
		mode := os.FileMode(0644)
		if info.Mode().Perm()&0111 != 0 {
			mode = 0755
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeOutputErr != nil {
			return closeOutputErr
		}
		return closeInputErr
	})
}

func (engine DeploymentEngine) prepareCoreRelease(release string) error {
	directory, err := componentReleaseDirectory(engine.Config.Paths, ComponentCore, release)
	if err != nil {
		return err
	}
	links := map[string]string{
		filepath.Join(directory, ".env"):               filepath.Join(engine.Config.Paths.NexusConfigDir, string(ComponentCore), ".env"),
		filepath.Join(directory, "storage"):            filepath.Join(engine.Config.Paths.DataDir, string(ComponentCore), "storage"),
		filepath.Join(directory, "bootstrap", "cache"): filepath.Join(engine.Config.Paths.DataDir, string(ComponentCore), "bootstrap-cache"),
	}
	for link, target := range links {
		if filepath.Base(link) == ".env" {
			if info, statErr := os.Stat(target); statErr != nil || !info.Mode().IsRegular() {
				return errors.New("Core environment file is unavailable")
			}
		} else if err := os.MkdirAll(target, 0750); err != nil {
			return err
		} else if err := chownPathToGroup(target, "nexus-core", 0770); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
			return err
		}
		if info, statErr := os.Lstat(link); statErr == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("managed release path %s blocks persistent state", filepath.Base(link))
			}
			if err := os.Remove(link); err != nil {
				return err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}

func (engine DeploymentEngine) runArtisan(ctx context.Context, release string, arguments ...string) error {
	directory, err := componentReleaseDirectory(engine.Config.Paths, ComponentCore, release)
	if err != nil {
		return err
	}
	artisan := filepath.Join(directory, "artisan")
	if _, err := os.Stat(artisan); err != nil {
		return err
	}
	commandArguments := []string{"-u", "nexus-core", "--", "/usr/bin/php", artisan}
	commandArguments = append(commandArguments, arguments...)
	return runFixedCommandInDirectory(ctx, directory, "/usr/sbin/runuser", commandArguments)
}

func (engine DeploymentEngine) switchComponents(components []Component, release string) error {
	for _, component := range components {
		if err := atomicSwitchRelease(engine.Config.Paths, component, release); err != nil {
			return err
		}
	}
	return nil
}

func (engine DeploymentEngine) restartComponents(ctx context.Context, components []Component) error {
	for _, component := range components {
		units, err := componentServiceUnits(component)
		if err != nil {
			return err
		}
		for _, unit := range units {
			if err := engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"restart", unit}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (engine DeploymentEngine) registerDiscordCommands(ctx context.Context) error {
	if err := engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"start", "nexus-discord-register.service"}); err != nil {
		return errors.New("Discord slash-command registration failed")
	}
	return nil
}

func (engine DeploymentEngine) healthCheck(ctx context.Context, components []Component, release string) error {
	var lastError error
	for attempt := 0; attempt < 20; attempt++ {
		lastError = nil
		for _, component := range components {
			units, err := componentServiceUnits(component)
			if err != nil {
				return err
			}
			for _, unit := range units {
				if err := engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"is-active", "--quiet", unit}); err != nil {
					lastError = fmt.Errorf("%s service is not active", component)
					break
				}
			}
			if lastError != nil {
				break
			}
			if component != ComponentCore {
				if err := validateComponentHealth(engine.Config.Paths, component, release, time.Now().UTC()); err != nil {
					lastError = fmt.Errorf("%s health contract is not ready: %w", component, err)
					break
				}
			} else if err := engine.runArtisan(ctx, release, "migrate:status", "--no-interaction"); err != nil {
				lastError = errors.New("Core could not boot and read its database schema")
				break
			}
		}
		if lastError == nil {
			return nil
		}
		if attempt < 19 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return lastError
}

func (engine DeploymentEngine) updateSelf(ctx context.Context, operationID, release string) error {
	if err := engine.phase(operationID, PhaseDownloading); err != nil {
		return err
	}
	directory := filepath.Join(engine.Config.Paths.UpdaterDir, "releases", release)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	temporary := filepath.Join(directory, "nexus.partial")
	_ = os.Remove(temporary)
	if err := engine.Releases.Download(ctx, setupRepository, release, setupAsset, temporary, maxUpdaterArtifact, nil); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0755); err != nil {
		return err
	}
	if err := validateUpdaterBinary(ctx, temporary, release); err != nil {
		return err
	}
	binary := filepath.Join(directory, "nexus")
	if err := os.Rename(temporary, binary); err != nil {
		return err
	}
	link := filepath.Join(engine.Config.Paths.UpdaterDir, "current")
	next := link + ".next"
	_ = os.Remove(next)
	if err := os.Symlink(directory, next); err != nil {
		return err
	}
	if err := os.Rename(next, link); err != nil {
		return err
	}
	return nil
}

func validateUpdaterBinary(ctx context.Context, binary, expected string) error {
	validationContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(validationContext, binary, "version", "--json")
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	output, err := command.Output()
	if err != nil || len(output) > 4096 {
		return errors.New("downloaded updater failed its version check")
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var identity struct {
		Version string `json:"version"`
	}
	if err := decoder.Decode(&identity); err != nil || identity.Version != expected {
		return errors.New("downloaded updater version does not match the release")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("downloaded updater returned invalid version data")
	}
	return nil
}

func (engine DeploymentEngine) rollback(ctx context.Context, operation OperationState) error {
	state, err := LoadInstallation(engine.Config.Paths)
	if err != nil {
		return err
	}
	if state.PreviousRelease == "" {
		return errors.New("no previous release is available")
	}
	components := installedComponents(state)
	running := enabledComponents(state)
	hasCore := state.Components[ComponentCore].Installed
	if err := engine.phase(operation.ID, PhaseRollingBack); err != nil {
		return err
	}
	for _, component := range components {
		directory, err := componentReleaseDirectory(engine.Config.Paths, component, state.PreviousRelease)
		if err != nil {
			return err
		}
		if err := ValidateArtifactIdentity(directory, component, state.PreviousRelease); err != nil {
			return fmt.Errorf("previous %s release is unavailable: %w", component, err)
		}
	}
	if hasCore {
		if err := engine.runArtisan(ctx, state.CurrentRelease, "down", "--retry=60"); err != nil {
			return fmt.Errorf("enter maintenance mode: %w", err)
		}
	}
	current := state.CurrentRelease
	if err := engine.switchComponents(components, state.PreviousRelease); err != nil {
		if hasCore {
			if upErr := engine.runArtisan(ctx, current, "up"); upErr != nil {
				return ErrRecoveryRequired
			}
		}
		return err
	}
	state.CurrentRelease, state.PreviousRelease = state.PreviousRelease, current
	for component, record := range state.Components {
		if record.Installed {
			record.CurrentRelease, record.PreviousRelease = record.PreviousRelease, record.CurrentRelease
			state.Components[component] = record
		}
	}
	if err := SaveInstallation(engine.Config.Paths, state); err != nil {
		_ = engine.switchComponents(components, current)
		if hasCore {
			_ = engine.runArtisan(ctx, current, "up")
		}
		return ErrRecoveryRequired
	}
	if err := engine.restartComponents(ctx, running); err != nil {
		return ErrRecoveryRequired
	}
	if err := engine.healthCheck(ctx, running, state.CurrentRelease); err != nil {
		return ErrRecoveryRequired
	}
	if state.Components[ComponentDiscord].Installed && state.Components[ComponentDiscord].Enabled {
		if err := engine.registerDiscordCommands(ctx); err != nil {
			return ErrRecoveryRequired
		}
	}
	if hasCore {
		if err := engine.runArtisan(ctx, state.CurrentRelease, "up"); err != nil {
			return ErrRecoveryRequired
		}
	}
	if engine.Store != nil {
		return engine.Store.ResolveRecovery(operation.ID)
	}
	return nil
}

func (engine DeploymentEngine) cleanup(operation OperationState) error {
	if err := engine.phase(operation.ID, PhaseCleaningUp); err != nil {
		return err
	}
	reclaimed, err := RunCleanup(engine.Config.Paths, 24*time.Hour, operation.ID)
	if err != nil {
		return err
	}
	_, err = engine.Store.Update(operation.ID, func(current *OperationState) error {
		current.ReclaimedBytes = reclaimed
		return nil
	})
	return err
}

func (engine DeploymentEngine) installComponent(ctx context.Context, operation OperationState, configuration map[string]string) error {
	if operation.Component == ComponentCore {
		return errors.New("Core is not an optional component")
	}
	state, err := LoadInstallation(engine.Config.Paths)
	if err != nil {
		return err
	}
	record := state.Components[operation.Component]
	if record.Installed {
		return errors.New("component is already installed")
	}
	if state.Profile == ProfileDatabaseOnly || state.Profile == ProfileSubsOnly {
		return errors.New("this profile must install components through the local CLI")
	}
	if err := validateInstallConfiguration(operation.Component, configuration); err != nil {
		return err
	}
	if err := ensureFreeSpace(engine.Config.Paths.StateDir, minimumComponentSpace); err != nil {
		return err
	}
	releaseDirectory, err := componentReleaseDirectory(engine.Config.Paths, operation.Component, state.CurrentRelease)
	if err != nil {
		return err
	}
	_, releaseStatErr := os.Lstat(releaseDirectory)
	releaseExisted := releaseStatErr == nil
	if releaseStatErr != nil && !errors.Is(releaseStatErr, os.ErrNotExist) {
		return releaseStatErr
	}
	unit, err := componentServiceUnit(operation.Component)
	if err != nil {
		return err
	}
	coreEnvironmentPath := filepath.Join(engine.Config.Paths.NexusConfigDir, string(ComponentCore), ".env")
	coreEnvironmentBefore, err := readBoundedRegularFile(coreEnvironmentPath, 1024*1024)
	if err != nil {
		return fmt.Errorf("read Core environment before component installation: %w", err)
	}
	configurationStarted := false
	completed := false
	defer func() {
		if completed {
			return
		}
		_ = engine.Executor.Run(context.Background(), engine.Config.SystemctlBinary, []string{"disable", "--now", unit})
		if link, linkErr := currentLink(engine.Config.Paths, operation.Component); linkErr == nil {
			if target, readErr := os.Readlink(link); readErr == nil && filepath.Clean(target) == filepath.Clean(releaseDirectory) {
				_ = os.Remove(link)
			}
		}
		_ = os.RemoveAll(filepath.Join(engine.Config.Paths.NexusConfigDir, string(operation.Component)))
		_ = os.RemoveAll(filepath.Join(engine.Config.Paths.CredentialDir, string(operation.Component)))
		_ = os.RemoveAll(filepath.Join(engine.Config.Paths.DataDir, string(operation.Component)))
		_ = os.RemoveAll(filepath.Join(engine.Config.Paths.StagingDir, operation.ID))
		_ = os.RemoveAll(filepath.Join(engine.Config.Paths.DownloadsDir, operation.ID))
		if !releaseExisted {
			_ = os.RemoveAll(releaseDirectory)
		}
		if configurationStarted {
			_ = writeCoreEnvironmentBytes(coreEnvironmentPath, coreEnvironmentBefore)
			if operation.Component == ComponentDiscord {
				_ = engine.restartComponents(context.Background(), []Component{ComponentCore})
			}
		}
	}()
	if err := engine.stageComponent(ctx, operation.ID, operation.Component, state.CurrentRelease); err != nil {
		return err
	}
	configurationStarted = true
	if err := engine.writeComponentConfiguration(operation.Component, state.CurrentRelease, configuration); err != nil {
		return err
	}
	if operation.Component == ComponentDiscord {
		if err := engine.restartComponents(ctx, []Component{ComponentCore}); err != nil {
			return fmt.Errorf("reload Core Discord configuration: %w", err)
		}
		if err := engine.healthCheck(ctx, []Component{ComponentCore}, state.CurrentRelease); err != nil {
			return fmt.Errorf("verify Core after Discord configuration: %w", err)
		}
	}
	if err := atomicSwitchRelease(engine.Config.Paths, operation.Component, state.CurrentRelease); err != nil {
		return err
	}
	if err := engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"enable", "--now", unit}); err != nil {
		return err
	}
	if err := engine.healthCheck(ctx, []Component{operation.Component}, state.CurrentRelease); err != nil {
		_ = engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"disable", "--now", unit})
		return err
	}
	if operation.Component == ComponentDiscord {
		if err := engine.registerDiscordCommands(ctx); err != nil {
			return err
		}
	}
	state.Components[operation.Component] = InstalledComponent{Installed: true, Enabled: true, CurrentRelease: state.CurrentRelease}
	if operation.Component == ComponentSubs && state.Profile == ProfileWebOnly {
		state.Profile = ProfileAppWebSubsRemoteDB
	}
	if err := SaveInstallation(engine.Config.Paths, state); err != nil {
		return err
	}
	completed = true
	return nil
}

func validateInstallConfiguration(component Component, configuration map[string]string) error {
	switch component {
	case ComponentSubs:
		if len(configuration) != 0 {
			return errors.New("local Subs installation reuses Core configuration")
		}
		return nil
	case ComponentDiscord:
		for _, field := range []string{"bot_token", "client_id", "guild_id"} {
			if strings.TrimSpace(configuration[field]) == "" {
				return fmt.Errorf("Discord %s is required", field)
			}
		}
		for _, field := range []string{"client_id", "guild_id"} {
			if len(configuration[field]) < 17 || len(configuration[field]) > 20 {
				return fmt.Errorf("Discord %s must be a 17 to 20 digit snowflake", field)
			}
			if _, err := strconv.ParseUint(configuration[field], 10, 64); err != nil {
				return fmt.Errorf("Discord %s must be a 17 to 20 digit snowflake", field)
			}
		}
		return nil
	default:
		return errors.New("unsupported component")
	}
}

func (engine DeploymentEngine) writeComponentConfiguration(component Component, release string, configuration map[string]string) error {
	componentConfig := filepath.Join(engine.Config.Paths.NexusConfigDir, string(component))
	if err := os.MkdirAll(componentConfig, 0750); err != nil {
		return err
	}
	dataDirectory := filepath.Join(engine.Config.Paths.DataDir, string(component))
	if err := os.MkdirAll(dataDirectory, 0750); err != nil {
		return err
	}
	if err := chownPathToGroup(dataDirectory, string(component), 0770); err != nil {
		return err
	}
	coreEnv := filepath.Join(engine.Config.Paths.NexusConfigDir, string(ComponentCore), ".env")
	appURL, err := readEnvValue(coreEnv, "APP_URL")
	if err != nil {
		return errors.New("Core APP_URL is unavailable")
	}
	environment := map[string]string{"NODE_ENV": "production", "NEXUS_RELEASE_ID": release, "BUILD_COMMIT": release}
	switch component {
	case ComponentSubs:
		pwToken, err := readEnvValue(coreEnv, "PW_API_KEY")
		if err != nil || pwToken == "" {
			return errors.New("Core Politics & War credential is unavailable")
		}
		nexusToken, err := ensureCoreSecret(coreEnv, "NEXUS_API_TOKEN")
		if err != nil {
			return err
		}
		pwPath, err := engine.writeCredential(component, "pw-api-token", pwToken)
		if err != nil {
			return err
		}
		nexusPath, err := engine.writeCredential(component, "nexus-api-token", nexusToken)
		if err != nil {
			return err
		}
		environment["PW_API_URL"] = "https://api.politicsandwar.com/subscriptions/v1/subscribe"
		environment["PW_API_TOKEN_FILE"] = pwPath
		environment["PUSHER_SOCKET_HOST"] = "socket.politicsandwar.com"
		environment["PW_AUTH_URL"] = "https://api.politicsandwar.com/subscriptions/v1/auth"
		environment["NEXUS_API_URL"] = strings.TrimRight(appURL, "/") + "/api/v1/subs"
		environment["NEXUS_API_TOKEN_FILE"] = nexusPath
		environment["DELIVERY_DRIVER"] = "http"
		environment["PROCESS_HEALTH_FILE"] = filepath.Join(dataDirectory, "process-health.json")
	case ComponentDiscord:
		nexusToken, err := ensureCoreSecret(coreEnv, "DISCORD_BOT_KEY")
		if err != nil {
			return err
		}
		relayIdentity, err := newDiscordRelayIdentity()
		if err != nil {
			return err
		}
		botPath, err := engine.writeCredential(component, "bot-token", configuration["bot_token"])
		if err != nil {
			return err
		}
		nexusPath, err := engine.writeCredential(component, "nexus-api-token", nexusToken)
		if err != nil {
			return err
		}
		relayPath, err := engine.writeCredential(component, "relay-private-key", relayIdentity.PrivateKey)
		if err != nil {
			return err
		}
		if err := updateCoreEnvironment(coreEnv, map[string]string{
			"DISCORD_APPLICATION_ID":           configuration["client_id"],
			"DISCORD_GUILD_ID":                 configuration["guild_id"],
			"DISCORD_CONNECTION_MODE":          "dedicated",
			"DISCORD_CONNECTION_ID":            relayIdentity.ConnectionID,
			"DISCORD_CONNECTION_GENERATION":    "1",
			"DISCORD_RELAY_PROTOCOL_VERSION":   "2",
			"DISCORD_RELAY_CURRENT_KEY_ID":     relayIdentity.KeyID,
			"DISCORD_RELAY_CURRENT_PUBLIC_KEY": relayIdentity.PublicKey,
		}); err != nil {
			return err
		}
		environment["BOT_DEPLOYMENT_MODE"] = "dedicated"
		environment["DISCORD_BOT_TOKEN_FILE"] = botPath
		environment["DISCORD_CLIENT_ID"] = configuration["client_id"]
		environment["DISCORD_GUILD_ID"] = configuration["guild_id"]
		environment["NEXUS_API_URL"] = appURL
		environment["NEXUS_API_KEY_FILE"] = nexusPath
		environment["NEXUS_DISCORD_CONNECTION_ID"] = relayIdentity.ConnectionID
		environment["NEXUS_DISCORD_CONNECTION_GENERATION"] = "1"
		environment["NEXUS_DISCORD_RELAY_PROTOCOL"] = "2"
		environment["NEXUS_DISCORD_RELAY_KEY_ID"] = relayIdentity.KeyID
		environment["NEXUS_DISCORD_RELAY_CURRENT_KEY_ID"] = relayIdentity.KeyID
		environment["NEXUS_DISCORD_RELAY_PRIVATE_KEY_FILE"] = relayPath
		environment["PROCESS_HEALTH_FILE"] = filepath.Join(dataDirectory, "process-health.json")
	default:
		return errors.New("unsupported component")
	}
	environmentPath := filepath.Join(componentConfig, "environment")
	if err := writeEnvironmentFile(environmentPath, environment); err != nil {
		return err
	}
	return chownPathToGroup(environmentPath, string(component), 0640)
}

func (engine DeploymentEngine) writeCredential(component Component, name, value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("credential value is invalid")
	}
	directory := filepath.Join(engine.Config.Paths.CredentialDir, string(component))
	if err := os.MkdirAll(directory, 0750); err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	if err := atomicWriteBytes(path, []byte(value+"\n"), 0640); err != nil {
		return "", err
	}
	group, err := user.LookupGroup(string(component))
	if err != nil {
		return "", err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return "", err
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return "", err
	}
	return path, nil
}

func writeEnvironmentFile(path string, values map[string]string) error {
	return writeKeyValueFile(path, values, false)
}

func writeDotEnvFile(path string, values map[string]string) error {
	return writeKeyValueFile(path, values, true)
}

func writeKeyValueFile(path string, values map[string]string, escapeDollar bool) error {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if key == "" || strings.ContainsAny(key, "=\r\n\x00") || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("component environment value is invalid")
		}
		keys = append(keys, key)
	}
	sortStrings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteString("=\"")
		value := strings.ReplaceAll(values[key], `\`, `\\`)
		value = strings.ReplaceAll(value, `"`, `\"`)
		if escapeDollar {
			value = strings.ReplaceAll(value, `$`, `\$`)
		}
		builder.WriteString(value)
		builder.WriteString("\"\n")
	}
	return atomicWriteBytes(path, []byte(builder.String()), 0640)
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}

func ensureFreeSpace(path string, required uint64) error {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return fmt.Errorf("check free disk space: %w", err)
	}
	available := stats.Bavail * uint64(stats.Bsize)
	if available < required {
		return fmt.Errorf("insufficient disk space: %d bytes available, %d required", available, required)
	}
	return nil
}

func readEnvValue(path, key string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return "", errors.New("environment file is unsafe or too large")
	}
	prefix := key + "="
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, prefix) {
			return decodeEnvironmentValue(strings.TrimPrefix(line, prefix))
		}
	}
	return "", scanner.Err()
}

func decodeEnvironmentValue(value string) (string, error) {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], nil
	}
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return value, nil
	}
	value = value[1 : len(value)-1]
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			decoded.WriteByte(value[index])
			continue
		}
		index++
		if index >= len(value) || !strings.ContainsRune(`\"$`, rune(value[index])) {
			return "", errors.New("environment value contains an invalid escape")
		}
		decoded.WriteByte(value[index])
	}
	return decoded.String(), nil
}

func ensureCoreSecret(path, key string) (string, error) {
	if value, err := readEnvValue(path, key); err == nil && value != "" {
		return value, nil
	}
	value, err := randomSecret(32)
	if err != nil {
		return "", err
	}
	data, err := readBoundedRegularFile(path, 1024*1024)
	if err != nil {
		return "", err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, []byte(key+"="+value+"\n")...)
	if err := writeCoreEnvironmentBytes(path, data); err != nil {
		return "", err
	}
	return value, nil
}

type discordRelayIdentity struct {
	ConnectionID string
	KeyID        string
	PrivateKey   string
	PublicKey    string
}

func newDiscordRelayIdentity() (discordRelayIdentity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return discordRelayIdentity{}, fmt.Errorf("generate Discord relay key: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return discordRelayIdentity{}, fmt.Errorf("encode Discord relay key: %w", err)
	}
	connectionID, err := randomUUID()
	if err != nil {
		return discordRelayIdentity{}, err
	}
	return discordRelayIdentity{
		ConnectionID: connectionID,
		KeyID:        "relay-current",
		PrivateKey:   base64.StdEncoding.EncodeToString(privateDER),
		PublicKey:    base64.RawURLEncoding.EncodeToString(publicKey),
	}, nil
}

func randomUUID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buffer)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func updateCoreEnvironment(path string, values map[string]string) error {
	data, err := readBoundedRegularFile(path, 1024*1024)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	remaining := make(map[string]string, len(values))
	for key, value := range values {
		if key == "" || strings.ContainsAny(key, "=\r\n\x00") || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("Core environment value is invalid")
		}
		remaining[key] = value
	}
	for index, line := range lines {
		for key, value := range remaining {
			if strings.HasPrefix(line, key+"=") {
				lines[index] = key + "=" + encodeDotEnvValue(value)
				delete(remaining, key)
				break
			}
		}
	}
	keys := make([]string, 0, len(remaining))
	for key := range remaining {
		keys = append(keys, key)
	}
	sortStrings(keys)
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, key := range keys {
		lines = append(lines, key+"="+encodeDotEnvValue(remaining[key]))
	}
	return writeCoreEnvironmentBytes(path, []byte(strings.Join(lines, "\n")+"\n"))
}

func encodeDotEnvValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `$`, `\$`)
	return `"` + value + `"`
}

func writeCoreEnvironmentBytes(path string, data []byte) error {
	if err := atomicWriteBytes(path, data, 0640); err != nil {
		return err
	}
	return chownPathToGroup(path, "nexus-core", 0640)
}

func randomSecret(bytesCount int) (string, error) {
	buffer := make([]byte, bytesCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func atomicWriteBytes(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".nexus-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
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
	return os.Rename(name, path)
}

func (engine DeploymentEngine) setComponentEnabled(ctx context.Context, operation OperationState, enabled bool) error {
	if operation.Component == ComponentCore {
		return errors.New("Core cannot be enabled or disabled")
	}
	state, err := LoadInstallation(engine.Config.Paths)
	if err != nil {
		return err
	}
	record := state.Components[operation.Component]
	if !record.Installed {
		return errors.New("component is not installed")
	}
	wasEnabled := record.Enabled
	unit, _ := componentServiceUnit(operation.Component)
	action := "disable"
	arguments := []string{action, "--now", unit}
	if enabled {
		action = "enable"
		arguments = []string{action, "--now", unit}
	}
	if err := engine.Executor.Run(ctx, engine.Config.SystemctlBinary, arguments); err != nil {
		return err
	}
	if enabled {
		if err := engine.healthCheck(ctx, []Component{operation.Component}, state.CurrentRelease); err != nil {
			_ = engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"disable", "--now", unit})
			return err
		}
		if operation.Component == ComponentDiscord {
			if err := engine.registerDiscordCommands(ctx); err != nil {
				_ = engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"disable", "--now", unit})
				return err
			}
		}
	}
	record.Enabled = enabled
	state.Components[operation.Component] = record
	if err := SaveInstallation(engine.Config.Paths, state); err != nil {
		if enabled {
			_ = engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"disable", "--now", unit})
		} else if wasEnabled {
			_ = engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"enable", "--now", unit})
		}
		return err
	}
	return nil
}

func (engine DeploymentEngine) restartComponent(ctx context.Context, component Component) error {
	state, err := LoadInstallation(engine.Config.Paths)
	if err != nil {
		return err
	}
	if !state.Components[component].Installed {
		return errors.New("component is not installed")
	}
	if !state.Components[component].Enabled {
		return errors.New("component is disabled; enable it before restarting")
	}
	if err := engine.restartComponents(ctx, []Component{component}); err != nil {
		return err
	}
	return engine.healthCheck(ctx, []Component{component}, state.CurrentRelease)
}

func (engine DeploymentEngine) phase(operationID string, phase OperationPhase) error {
	if engine.Store == nil {
		return nil
	}
	return updatePhase(engine.Store, operationID, phase)
}

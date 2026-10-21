package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func readStrictJSONFile(path string, maxBytes int64, destination any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxBytes {
		return errors.New("state file is unsafe or has an invalid size")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("state file contains trailing data")
	}
	return nil
}

type InstalledComponent struct {
	Installed       bool   `json:"installed"`
	Enabled         bool   `json:"enabled"`
	CurrentRelease  string `json:"current_release,omitempty"`
	PreviousRelease string `json:"previous_release,omitempty"`
}

// InstallationState records what is installed on this host. Each component has
// its own release history; LastChanged lists the components the most recent
// update or rollback switched, which is what a plain rollback reverses.
// SubsDelivery records how Subs reaches Core so updates keep the adopted mode;
// an empty value means HTTP. CoreWorkers is how many default-queue workers
// Core runs; zero means one.
type InstallationState struct {
	Version      int                              `json:"version"`
	Profile      Profile                          `json:"profile"`
	Channel      Channel                          `json:"channel"`
	SubsDelivery SubsDelivery                     `json:"subs_delivery,omitempty"`
	CoreWorkers  int                              `json:"core_workers,omitempty"`
	Components   map[Component]InstalledComponent `json:"components"`
	LastChanged  []Component                      `json:"last_changed,omitempty"`
	UpdatedAt    time.Time                        `json:"updated_at"`
}

const installationStateVersion = 2

// installationStateV1 is the original shape, in which every component shared
// one Nexus release.
type installationStateV1 struct {
	Version         int                              `json:"version"`
	Profile         Profile                          `json:"profile"`
	CurrentRelease  string                           `json:"current_release"`
	PreviousRelease string                           `json:"previous_release,omitempty"`
	Components      map[Component]InstalledComponent `json:"components"`
	UpdatedAt       time.Time                        `json:"updated_at"`
}

func LoadInstallation(paths Paths) (InstallationState, error) {
	var state InstallationState
	if err := readStrictJSONFile(paths.InstallationDB, 256*1024, &state); err != nil {
		var legacy installationStateV1
		if legacyErr := readStrictJSONFile(paths.InstallationDB, 256*1024, &legacy); legacyErr != nil || legacy.Version != 1 {
			return InstallationState{}, err
		}
		state = migrateInstallationV1(legacy)
	}
	if err := validateInstallation(state); err != nil {
		return InstallationState{}, err
	}
	return state, nil
}

func migrateInstallationV1(legacy installationStateV1) InstallationState {
	state := InstallationState{Version: installationStateVersion, Profile: legacy.Profile, Channel: ChannelStable, Components: legacy.Components, UpdatedAt: legacy.UpdatedAt}
	if legacy.PreviousRelease != "" {
		for _, component := range planOrder {
			if state.Components[component].Installed && state.Components[component].PreviousRelease != "" {
				state.LastChanged = append(state.LastChanged, component)
			}
		}
	}
	return state
}

func SaveInstallation(paths Paths, state InstallationState) error {
	state.Version = installationStateVersion
	state.UpdatedAt = time.Now().UTC()
	if err := validateInstallation(state); err != nil {
		return err
	}
	return atomicWriteJSON(paths.InstallationDB, state, 0600)
}

func validateInstallation(state InstallationState) error {
	if state.Version != installationStateVersion || !validProfile(state.Profile) {
		return errors.New("installation state version or profile is invalid")
	}
	if !validChannel(state.Channel) {
		return errors.New("installation release channel is invalid")
	}
	core := state.Components[ComponentCore]
	subs := state.Components[ComponentSubs]
	if !validSubsDelivery(state.SubsDelivery) {
		return errors.New("installation Subs delivery mode is invalid")
	}
	if state.SubsDelivery == SubsDeliveryRedisStream && !subs.Installed {
		return errors.New("Redis Stream delivery requires Subs")
	}
	if state.CoreWorkers != 0 && !validCoreWorkers(state.CoreWorkers) {
		return errors.New("installation Core worker count is invalid")
	}
	switch state.Profile {
	case ProfileFull, ProfileAppWebSubsRemoteDB:
		if !core.Installed || !core.Enabled || !subs.Installed {
			return errors.New("installation profile requires Core and Subs")
		}
	case ProfileWebOnly:
		if !core.Installed || !core.Enabled {
			return errors.New("web-only profile requires Core")
		}
	case ProfileDatabaseOnly:
		if core.Installed || subs.Installed || state.Components[ComponentDiscord].Installed {
			return errors.New("database-only profile cannot contain application components")
		}
	case ProfileSubsOnly:
		if core.Installed || !subs.Installed {
			return errors.New("subs-only profile requires only Subs")
		}
	}
	for component, installed := range state.Components {
		if !validComponent(component) {
			return fmt.Errorf("installation contains unsupported component %q", component)
		}
		if installed.Installed {
			if _, err := ParseVersion(installed.CurrentRelease); err != nil {
				return fmt.Errorf("installed component %q has an invalid release", component)
			}
		}
		if installed.PreviousRelease != "" {
			if _, err := ParseVersion(installed.PreviousRelease); err != nil {
				return fmt.Errorf("installed component %q has an invalid previous release", component)
			}
		}
	}
	seen := map[Component]bool{}
	for _, component := range state.LastChanged {
		if seen[component] || !state.Components[component].Installed || state.Components[component].PreviousRelease == "" {
			return errors.New("installation last-changed components are invalid")
		}
		seen[component] = true
	}
	return nil
}

// installedVersions maps each installed component to its current release.
func installedVersions(state InstallationState) map[Component]string {
	versions := map[Component]string{}
	for _, component := range planOrder {
		if record := state.Components[component]; record.Installed {
			versions[component] = record.CurrentRelease
		}
	}
	return versions
}

func componentRoot(paths Paths, component Component) (string, error) {
	if !validComponent(component) {
		return "", errors.New("unsupported component")
	}
	return filepath.Join(paths.ReleaseDir, string(component)), nil
}

func componentReleaseDirectory(paths Paths, component Component, release string) (string, error) {
	if _, err := ParseVersion(release); err != nil {
		return "", err
	}
	root, err := componentRoot(paths, component)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "releases", release), nil
}

func currentLink(paths Paths, component Component) (string, error) {
	root, err := componentRoot(paths, component)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "current"), nil
}

func atomicSwitchRelease(paths Paths, component Component, release string) error {
	target, err := componentReleaseDirectory(paths, component, release)
	if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release directory is unavailable")
	}
	if err := recordActiveRelease(paths, component, release); err != nil {
		return err
	}
	link, err := currentLink(paths, component)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		return err
	}
	temporary := link + ".next"
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, link); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	directory, err := os.Open(filepath.Dir(link))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// recordActiveRelease writes the release a Subs or Discord unit is about to
// run into that component's environment. The process reports these values in
// its health document, and the health check rejects any other release, so
// every switch, including one back to an earlier release, must update them
// before the unit restarts. A component that is not configured yet has no
// environment to update.
func recordActiveRelease(paths Paths, component Component, release string) error {
	if component != ComponentSubs && component != ComponentDiscord {
		return nil
	}
	if _, err := ParseVersion(release); err != nil {
		return err
	}
	path := filepath.Join(paths.NexusConfigDir, string(component), "environment")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := updateEnvironmentFile(path, map[string]string{"NEXUS_RELEASE_ID": release, "BUILD_COMMIT": release}); err != nil {
		return fmt.Errorf("record %s %s in its environment: %w", component, release, err)
	}
	return nil
}

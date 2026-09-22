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

type InstallationState struct {
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
		return InstallationState{}, err
	}
	if err := validateInstallation(state); err != nil {
		return InstallationState{}, err
	}
	return state, nil
}

func SaveInstallation(paths Paths, state InstallationState) error {
	state.Version = 1
	state.UpdatedAt = time.Now().UTC()
	if err := validateInstallation(state); err != nil {
		return err
	}
	return atomicWriteJSON(paths.InstallationDB, state, 0600)
}

func validateInstallation(state InstallationState) error {
	if state.Version != 1 || !validProfile(state.Profile) {
		return errors.New("installation state version or profile is invalid")
	}
	if _, err := ParseVersion(state.CurrentRelease); err != nil {
		return errors.New("installation current release is invalid")
	}
	if state.PreviousRelease != "" {
		if _, err := ParseVersion(state.PreviousRelease); err != nil {
			return errors.New("installation previous release is invalid")
		}
	}
	core := state.Components[ComponentCore]
	subs := state.Components[ComponentSubs]
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
			if installed.CurrentRelease != state.CurrentRelease {
				return fmt.Errorf("installed component %q is not on the Nexus release", component)
			}
		}
	}
	return nil
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

package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Config is process-local updater configuration loaded from a root-owned file.
// This package intentionally does not accept arbitrary config values over the
// privileged protocol.
type Config struct {
	Paths            Paths
	SocketMode       os.FileMode
	SocketGroupGID   int
	AllowedUIDs      []uint32
	AllowedGIDs      []uint32
	WorkerBinary     string
	SystemctlBinary  string
	WorkerUnitPrefix string
	CurrentProfile   Profile
	CurrentOS        string
	CurrentOSVersion string
	CurrentArch      string
}

func DefaultConfig() Config {
	paths := DefaultPaths()
	return Config{
		Paths:            paths,
		SocketMode:       0660,
		SocketGroupGID:   -1,
		AllowedUIDs:      []uint32{0},
		AllowedGIDs:      nil,
		WorkerBinary:     "/usr/local/bin/nexus",
		SystemctlBinary:  "/usr/bin/systemctl",
		WorkerUnitPrefix: "nexus-updater-worker@",
		CurrentProfile:   ProfileFull,
		CurrentOS:        "",
		CurrentOSVersion: "",
		CurrentArch:      "amd64",
	}
}

func (config Config) Validate() error {
	if err := config.Paths.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"worker binary":    config.WorkerBinary,
		"systemctl binary": config.SystemctlBinary,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	if config.SocketMode&0777 != 0660 {
		return errors.New("socket mode must be 0660")
	}
	if config.SocketGroupGID < -1 {
		return errors.New("socket group id is invalid")
	}
	if !validProfile(config.CurrentProfile) {
		return errors.New("current profile is invalid")
	}
	if config.CurrentOS == "" && config.CurrentOSVersion != "" {
		return errors.New("current operating system version requires an operating system")
	}
	if config.CurrentOS != "" && !supportedOperatingSystem(config.CurrentOS, config.CurrentOSVersion) {
		return errors.New("current operating system version is unsupported")
	}
	if config.CurrentArch != "amd64" {
		return errors.New("current architecture is unsupported")
	}
	for _, uid := range config.AllowedUIDs {
		if uid == ^uint32(0) {
			return errors.New("allowed user id is invalid")
		}
	}
	for _, gid := range config.AllowedGIDs {
		if gid == ^uint32(0) {
			return errors.New("allowed group id is invalid")
		}
	}
	return nil
}

type diskConfig struct {
	SocketGroupGID   int      `json:"socket_group_gid"`
	AllowedUIDs      []uint32 `json:"allowed_uids"`
	AllowedGIDs      []uint32 `json:"allowed_gids"`
	CurrentProfile   Profile  `json:"current_profile"`
	CurrentOS        string   `json:"current_os"`
	CurrentOSVersion string   `json:"current_os_version"`
	CurrentArch      string   `json:"current_arch"`
}

// LoadConfig reads only the mutable, root-owned policy values. Executable
// paths, socket paths, release roots, and protocol limits are compiled into
// the updater and cannot be changed through this file.
func LoadConfig(path string) (Config, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Config{}, errors.New("configuration path must be a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Config{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return Config{}, errors.New("updater configuration must not be a symlink or group/world writable")
	}
	if !info.Mode().IsRegular() {
		return Config{}, errors.New("updater configuration must be a regular file")
	}
	if !rootOwned(info) {
		return Config{}, errors.New("updater configuration must be root-owned")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	if len(data) > 64*1024 {
		return Config{}, errors.New("updater configuration is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var disk diskConfig
	if err := decoder.Decode(&disk); err != nil {
		return Config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Config{}, errors.New("updater configuration contains trailing data")
	}
	config := DefaultConfig()
	config.SocketGroupGID = disk.SocketGroupGID
	config.AllowedUIDs = append([]uint32(nil), disk.AllowedUIDs...)
	config.AllowedGIDs = append([]uint32(nil), disk.AllowedGIDs...)
	config.CurrentProfile = disk.CurrentProfile
	config.CurrentOS = disk.CurrentOS
	config.CurrentOSVersion = disk.CurrentOSVersion
	config.CurrentArch = disk.CurrentArch
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate updater configuration: %w", err)
	}
	return config, nil
}

// LoadInstalledConfig permits a first-boot default with root-only access. As
// soon as a config file exists, it must pass all ownership and schema checks.
func LoadInstalledConfig() (Config, error) {
	defaultConfig := DefaultConfig()
	path := filepath.Join(defaultConfig.Paths.ConfigDir, "config.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return defaultConfig, nil
	}
	return LoadConfig(path)
}

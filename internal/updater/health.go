package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type componentHealthDocument struct {
	SchemaVersion      int       `json:"schema_version"`
	ContractVersion    int       `json:"contract_version"`
	ComponentID        Component `json:"component_id"`
	RuntimeState       string    `json:"runtime_state"`
	HealthState        string    `json:"health_state"`
	Reachable          bool      `json:"reachable"`
	Installed          bool      `json:"installed"`
	Enabled            bool      `json:"enabled"`
	ConfigurationReady bool      `json:"configuration_ready"`
	ReleaseID          string    `json:"release_id"`
	HeartbeatAt        string    `json:"heartbeat_at"`
	StaleAfterMS       int64     `json:"stale_after_ms"`
	Build              struct {
		Release string `json:"release"`
	} `json:"build"`
}

func validateComponentHealth(paths Paths, component Component, expectedRelease string, now time.Time) error {
	if component != ComponentSubs && component != ComponentDiscord {
		return errors.New("component does not publish the optional-component health contract")
	}
	if _, err := ParseVersion(expectedRelease); err != nil {
		return errors.New("expected component release is invalid")
	}
	path := filepath.Join(paths.DataDir, string(component), "process-health.json")
	contents, err := readPrivateHealthFile(path, 64*1024)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var document componentHealthDocument
	if err := decoder.Decode(&document); err != nil {
		return errors.New("health document is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("health document contains trailing data")
	}
	if document.SchemaVersion != 1 || document.ContractVersion != 1 || document.ComponentID != component {
		return errors.New("health contract identity is incompatible")
	}
	if document.ReleaseID != expectedRelease || document.Build.Release != expectedRelease {
		return errors.New("health release identity does not match the active release")
	}
	if document.RuntimeState != "ready" || document.HealthState != "healthy" || !document.Reachable || !document.Installed || !document.Enabled || !document.ConfigurationReady {
		return errors.New("component reports that it is not ready")
	}
	if document.StaleAfterMS < 1000 || document.StaleAfterMS > int64(5*time.Minute/time.Millisecond) {
		return errors.New("health heartbeat window is invalid")
	}
	heartbeat, err := time.Parse(time.RFC3339Nano, document.HeartbeatAt)
	if err != nil {
		return errors.New("health heartbeat timestamp is invalid")
	}
	if heartbeat.After(now.Add(30 * time.Second)) {
		return errors.New("health heartbeat is in the future")
	}
	if now.Sub(heartbeat) > time.Duration(document.StaleAfterMS)*time.Millisecond {
		return errors.New("health heartbeat is stale")
	}
	return nil
}

func readPrivateHealthFile(path string, maximum int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("health document is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("health document permissions or size are invalid")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read health document: %w", err)
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("health document exceeds the size limit")
	}
	return contents, nil
}

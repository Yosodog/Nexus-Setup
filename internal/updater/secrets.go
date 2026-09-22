package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func operationSecretPath(paths Paths, operationID string) (string, error) {
	if !uuidPattern.MatchString(operationID) {
		return "", errors.New("invalid operation id")
	}
	return filepath.Join(paths.SecretsDir, operationID), nil
}

func writeOperationConfiguration(paths Paths, operationID string, configuration map[string]string) error {
	if len(configuration) == 0 {
		return nil
	}
	secretPath, err := operationSecretPath(paths, operationID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.SecretsDir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(paths.SecretsDir, 0700); err != nil {
		return err
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return err
	}
	if len(encoded) > MaxFrameSize {
		return errors.New("component configuration is too large")
	}
	file, err := os.OpenFile(secretPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(secretPath)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func consumeOperationConfiguration(paths Paths, operationID string) (map[string]string, error) {
	secretPath, err := operationSecretPath(paths, operationID)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(secretPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("operation configuration file is unsafe")
	}
	file, err := os.OpenFile(secretPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(secretPath); err != nil {
		_ = file.Close()
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxFrameSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxFrameSize {
		return nil, errors.New("operation configuration file has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var configuration map[string]string
	if err := decoder.Decode(&configuration); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("operation configuration contains trailing data")
	}
	return configuration, nil
}

func removeOperationConfiguration(paths Paths, operationID string) {
	secretPath, err := operationSecretPath(paths, operationID)
	if err == nil {
		_ = os.Remove(secretPath)
	}
}

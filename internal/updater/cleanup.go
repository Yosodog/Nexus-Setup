package updater

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cleanupEntry struct {
	Path  string
	Label string
	Bytes int64
}

func PreviewCleanup(paths Paths, staleAfter time.Duration) (CleanupPreview, error) {
	entries, err := cleanupCandidates(paths, staleAfter, "")
	if err != nil {
		return CleanupPreview{}, err
	}
	preview := CleanupPreview{Paths: make([]string, 0, len(entries))}
	for _, entry := range entries {
		preview.ReclaimableBytes += entry.Bytes
		preview.Paths = append(preview.Paths, entry.Label)
	}
	return preview, nil
}

func RunCleanup(paths Paths, staleAfter time.Duration, currentOperationID string) (int64, error) {
	entries, err := cleanupCandidates(paths, staleAfter, currentOperationID)
	if err != nil {
		return 0, err
	}
	var reclaimed int64
	for _, entry := range entries {
		if err := removeManagedEntry(paths, entry.Path); err != nil {
			return reclaimed, err
		}
		reclaimed += entry.Bytes
	}
	return reclaimed, nil
}

func cleanupCandidates(paths Paths, staleAfter time.Duration, currentOperationID string) ([]cleanupEntry, error) {
	if staleAfter <= 0 {
		return nil, errors.New("cleanup age must be positive")
	}
	state, err := LoadInstallation(paths)
	if err != nil {
		return nil, err
	}
	store, err := NewOperationStore(paths)
	if err != nil {
		return nil, err
	}
	operations, err := store.List(1000)
	if err != nil {
		return nil, err
	}
	for _, operation := range operations {
		if operation.ID == currentOperationID {
			continue
		}
		if operation.Status == OperationQueued || operation.Status == OperationRunning || operation.Status == OperationRecovery {
			return nil, errors.New("cleanup is unavailable while an operation is active or needs recovery")
		}
	}

	var entries []cleanupEntry
	for _, component := range []Component{ComponentCore, ComponentSubs, ComponentDiscord} {
		record := state.Components[component]
		root, rootErr := componentRoot(paths, component)
		if rootErr != nil {
			return nil, rootErr
		}
		releasesDirectory := filepath.Join(root, "releases")
		directories, readErr := os.ReadDir(releasesDirectory)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		for _, directory := range directories {
			if !directory.IsDir() || (record.Installed && (directory.Name() == record.CurrentRelease || directory.Name() == record.PreviousRelease)) {
				continue
			}
			if _, parseErr := ParseVersion(directory.Name()); parseErr != nil {
				continue
			}
			path := filepath.Join(releasesDirectory, directory.Name())
			size, sizeErr := managedTreeSize(path)
			if sizeErr != nil {
				return nil, sizeErr
			}
			entries = append(entries, cleanupEntry{Path: path, Label: string(component) + "/" + directory.Name(), Bytes: size})
		}
	}

	cutoff := time.Now().Add(-staleAfter)
	for _, root := range []string{paths.DownloadsDir, paths.StagingDir} {
		directories, readErr := os.ReadDir(root)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range directories {
			info, infoErr := entry.Info()
			if infoErr != nil || info.ModTime().After(cutoff) {
				continue
			}
			path := filepath.Join(root, entry.Name())
			size, sizeErr := managedTreeSize(path)
			if sizeErr != nil {
				return nil, sizeErr
			}
			entries = append(entries, cleanupEntry{Path: path, Label: filepath.Base(root) + "/" + entry.Name(), Bytes: size})
		}
	}
	return entries, nil
}

func managedTreeSize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("managed cleanup tree contains a symlink")
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}

func removeManagedEntry(paths Paths, target string) error {
	target = filepath.Clean(target)
	allowed := false
	for _, root := range []string{paths.ReleaseDir, paths.DownloadsDir, paths.StagingDir} {
		relative, err := filepath.Rel(filepath.Clean(root), target)
		if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("refusing to remove unmanaged path")
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("cleanup target is unsafe")
	}
	return os.RemoveAll(target)
}

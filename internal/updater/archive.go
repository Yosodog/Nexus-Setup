package updater

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// ArchiveLimits bound both compressed input and expanded output regardless of
// what the remote server reports for the asset size.
type ArchiveLimits struct {
	MaxCompressedBytes   int64
	MaxUncompressedBytes int64
	MaxFileBytes         int64
	MaxFiles             int
}

func DefaultArchiveLimits() ArchiveLimits {
	return ArchiveLimits{
		MaxCompressedBytes:   2 * 1024 * 1024 * 1024,
		MaxUncompressedBytes: 8 * 1024 * 1024 * 1024,
		MaxFileBytes:         512 * 1024 * 1024,
		MaxFiles:             100000,
	}
}

type ExtractionResult struct {
	Files        int
	Directories  int
	BytesWritten int64
}

// ExtractTarGz extracts only regular files and directories into a fresh
// staging directory. Links, device nodes, special files, unsafe names,
// unexpected ownership, duplicate entries, and resource-limit violations are
// rejected before the destination can become an active release.
func ExtractTarGz(reader io.Reader, destination string, limits ArchiveLimits) (ExtractionResult, error) {
	if limits.MaxCompressedBytes <= 0 || limits.MaxUncompressedBytes <= 0 || limits.MaxFileBytes <= 0 || limits.MaxFiles <= 0 {
		return ExtractionResult{}, errors.New("archive limits must be positive")
	}
	if !filepath.IsAbs(destination) {
		return ExtractionResult{}, errors.New("extraction destination must be absolute")
	}
	if _, err := os.Lstat(destination); err == nil {
		return ExtractionResult{}, errors.New("extraction destination must not already exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return ExtractionResult{}, err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return ExtractionResult{}, err
	}
	if info, err := os.Lstat(destination); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ExtractionResult{}, errors.New("extraction destination is not a directory")
	}

	limitedReader := &countingReader{reader: io.LimitReader(reader, limits.MaxCompressedBytes+1)}
	gzipReader, err := gzip.NewReader(limitedReader)
	if err != nil {
		return ExtractionResult{}, fmt.Errorf("open gzip artifact: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	seen := make(map[string]struct{})
	var result ExtractionResult
	for {
		header, headerErr := tarReader.Next()
		if errors.Is(headerErr, io.EOF) {
			break
		}
		if headerErr != nil {
			return ExtractionResult{}, fmt.Errorf("read archive entry: %w", headerErr)
		}
		if result.Files+result.Directories >= limits.MaxFiles {
			return ExtractionResult{}, errors.New("archive contains too many entries")
		}
		entryPath, normalizeErr := safeArchivePath(header.Name)
		if normalizeErr != nil {
			return ExtractionResult{}, normalizeErr
		}
		if _, exists := seen[entryPath]; exists {
			return ExtractionResult{}, fmt.Errorf("archive contains duplicate entry %q", header.Name)
		}
		seen[entryPath] = struct{}{}
		if header.Uid != 0 || header.Gid != 0 {
			return ExtractionResult{}, fmt.Errorf("archive entry %q has unexpected ownership", header.Name)
		}
		if header.Mode&^int64(0777) != 0 {
			return ExtractionResult{}, fmt.Errorf("archive entry %q has unsafe mode bits", header.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(entryPath))
		if !withinDirectory(destination, target) {
			return ExtractionResult{}, fmt.Errorf("archive entry %q escapes staging", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return ExtractionResult{}, fmt.Errorf("directory %q has a non-zero size", header.Name)
			}
			if err := ensureParentsSafe(destination, target); err != nil {
				return ExtractionResult{}, err
			}
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0777); err != nil {
				return ExtractionResult{}, fmt.Errorf("create archive directory %q: %w", header.Name, err)
			}
			if err := os.Chmod(target, os.FileMode(header.Mode)&0777); err != nil {
				return ExtractionResult{}, err
			}
			result.Directories++
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > limits.MaxFileBytes {
				return ExtractionResult{}, fmt.Errorf("archive file %q exceeds file-size limit", header.Name)
			}
			if result.BytesWritten > limits.MaxUncompressedBytes-header.Size {
				return ExtractionResult{}, errors.New("archive exceeds uncompressed-size limit")
			}
			if err := ensureParentsSafe(destination, target); err != nil {
				return ExtractionResult{}, err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return ExtractionResult{}, fmt.Errorf("create archive parents for %q: %w", header.Name, err)
			}
			if err := normalizeParentModes(destination, filepath.Dir(target)); err != nil {
				return ExtractionResult{}, err
			}
			file, openErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, os.FileMode(header.Mode)&0777)
			if openErr != nil {
				return ExtractionResult{}, fmt.Errorf("create archive file %q: %w", header.Name, openErr)
			}
			if err := file.Chmod(os.FileMode(header.Mode) & 0777); err != nil {
				_ = file.Close()
				return ExtractionResult{}, err
			}
			written, copyErr := io.CopyN(file, tarReader, header.Size)
			if copyErr == nil {
				copyErr = file.Sync()
			}
			closeErr := file.Close()
			if copyErr != nil {
				return ExtractionResult{}, fmt.Errorf("write archive file %q: %w", header.Name, copyErr)
			}
			if closeErr != nil {
				return ExtractionResult{}, closeErr
			}
			if written != header.Size {
				return ExtractionResult{}, fmt.Errorf("archive file %q was truncated", header.Name)
			}
			result.Files++
			result.BytesWritten += written
		default:
			return ExtractionResult{}, fmt.Errorf("archive entry %q uses unsupported type %d", header.Name, header.Typeflag)
		}
	}
	if limitedReader.count > limits.MaxCompressedBytes {
		return ExtractionResult{}, errors.New("compressed artifact exceeds size limit")
	}
	return result, nil
}

func normalizeParentModes(destination, directory string) error {
	relative, err := filepath.Rel(destination, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("archive parent escapes staging")
	}
	current := destination
	if err := os.Chmod(current, 0700); err != nil {
		return err
	}
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := os.Chmod(current, 0755); err != nil {
			return err
		}
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.ContainsRune(name, '\\') {
		return "", errors.New("archive entry has an invalid name")
	}
	cleaned := path.Clean(name)
	if cleaned == "." || strings.HasPrefix(cleaned, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("archive entry %q is outside staging", name)
	}
	return cleaned, nil
}

func withinDirectory(directory, target string) bool {
	relative, err := filepath.Rel(directory, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func ensureParentsSafe(destination, target string) error {
	relative, err := filepath.Rel(destination, filepath.Dir(target))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("archive parent escapes staging")
	}
	current := destination
	if relative != "." {
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			if part == "" || part == "." {
				continue
			}
			current = filepath.Join(current, part)
			if info, statErr := os.Lstat(current); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive parent %q is a symlink", current)
			}
		}
	}
	return nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.count += int64(count)
	return count, err
}

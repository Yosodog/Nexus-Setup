package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type archiveEntry struct {
	name     string
	typeFlag byte
	mode     int64
	data     string
	link     string
	uid      int
	gid      int
}

func makeTarGz(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data)), Typeflag: entry.typeFlag, Linkname: entry.link, Uid: entry.uid, Gid: entry.gid}
		if entry.typeFlag == tar.TypeDir {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.data != "" {
			if _, err := io.WriteString(tarWriter, entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestExtractTarGzRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name  string
		entry archiveEntry
	}{
		{name: "traversal", entry: archiveEntry{name: "../escape", typeFlag: tar.TypeReg, mode: 0600, data: "x"}},
		{name: "absolute", entry: archiveEntry{name: "/etc/passwd", typeFlag: tar.TypeReg, mode: 0600, data: "x"}},
		{name: "symlink", entry: archiveEntry{name: "link", typeFlag: tar.TypeSymlink, mode: 0777, link: "target"}},
		{name: "device", entry: archiveEntry{name: "device", typeFlag: tar.TypeChar, mode: 0600}},
		{name: "ownership", entry: archiveEntry{name: "owned", typeFlag: tar.TypeReg, mode: 0600, data: "x", uid: 1000}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ExtractTarGz(bytes.NewReader(makeTarGz(t, []archiveEntry{test.entry})), filepath.Join(t.TempDir(), "release"), DefaultArchiveLimits()); err == nil {
				t.Fatal("unsafe archive entry was accepted")
			}
		})
	}
}

func TestExtractTarGzWritesRegularFiles(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "release")
	archive := makeTarGz(t, []archiveEntry{
		{name: "bin", typeFlag: tar.TypeDir, mode: 0755},
		{name: "bin/nexus", typeFlag: tar.TypeReg, mode: 0755, data: "binary"},
	})
	result, err := ExtractTarGz(bytes.NewReader(archive), destination, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.Directories != 1 || result.BytesWritten != 6 {
		t.Fatalf("unexpected extraction result: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(destination, "bin", "nexus"))
	if err != nil || string(data) != "binary" {
		t.Fatalf("extracted file mismatch: %q %v", data, err)
	}
}

func TestExtractTarGzRejectsExistingOrSymlinkDestination(t *testing.T) {
	archive := makeTarGz(t, []archiveEntry{{name: "file", typeFlag: tar.TypeReg, mode: 0644, data: "data"}})
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(existing, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTarGz(bytes.NewReader(archive), existing, DefaultArchiveLimits()); err == nil {
		t.Fatal("existing extraction destination was accepted")
	}

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "release")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTarGz(bytes.NewReader(archive), symlink, DefaultArchiveLimits()); err == nil {
		t.Fatal("symlink extraction destination was accepted")
	}
}

func TestExtractTarGzAppliesSanitizedModesDespiteRestrictiveUmask(t *testing.T) {
	previousUmask := syscall.Umask(0077)
	defer syscall.Umask(previousUmask)
	destination := filepath.Join(t.TempDir(), "release")
	archive := makeTarGz(t, []archiveEntry{
		{name: "bin", typeFlag: tar.TypeDir, mode: 0755},
		{name: "bin/nexus", typeFlag: tar.TypeReg, mode: 0755, data: "binary"},
	})
	if _, err := ExtractTarGz(bytes.NewReader(archive), destination, DefaultArchiveLimits()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "bin", "nexus"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("extracted executable mode = %o, want 755", info.Mode().Perm())
	}
}

func TestExtractTarGzHonorsExpansionLimit(t *testing.T) {
	limits := DefaultArchiveLimits()
	limits.MaxFileBytes = 4
	if _, err := ExtractTarGz(bytes.NewReader(makeTarGz(t, []archiveEntry{{name: "file", typeFlag: tar.TypeReg, mode: 0600, data: "12345"}})), filepath.Join(t.TempDir(), "release"), limits); err == nil {
		t.Fatal("oversized file was accepted")
	}
}

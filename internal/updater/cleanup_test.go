package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupProtectsCurrentPreviousAndPersistentData(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{
		Version: 1, Profile: ProfileWebOnly, CurrentRelease: "v1.0.2", PreviousRelease: "v1.0.1",
		Components: map[Component]InstalledComponent{
			ComponentCore:    {Installed: true, Enabled: true, CurrentRelease: "v1.0.2", PreviousRelease: "v1.0.1"},
			ComponentSubs:    {},
			ComponentDiscord: {},
		},
	}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	for _, release := range []string{"v1.0.0", "v1.0.1", "v1.0.2"} {
		directory, _ := componentReleaseDirectory(paths, ComponentCore, release)
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "artifact"), []byte(release), 0644); err != nil {
			t.Fatal(err)
		}
	}
	persistent := filepath.Join(paths.DataDir, string(ComponentCore), "storage", "keep")
	if err := os.MkdirAll(filepath.Dir(persistent), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistent, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := RunCleanup(paths, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed == 0 {
		t.Fatal("cleanup did not report reclaimed bytes")
	}
	oldDirectory, _ := componentReleaseDirectory(paths, ComponentCore, "v1.0.0")
	if _, err := os.Stat(oldDirectory); !os.IsNotExist(err) {
		t.Fatal("old release was not removed")
	}
	for _, release := range []string{"v1.0.1", "v1.0.2"} {
		directory, _ := componentReleaseDirectory(paths, ComponentCore, release)
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("protected release %s was removed: %v", release, err)
		}
	}
	if _, err := os.Stat(persistent); err != nil {
		t.Fatalf("persistent data was removed: %v", err)
	}
}

func TestCleanupRemovesManagedReleaseLeftByFailedOptionalInstall(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{
		Version: 1, Profile: ProfileWebOnly, CurrentRelease: "v1.0.2",
		Components: map[Component]InstalledComponent{
			ComponentCore:    {Installed: true, Enabled: true, CurrentRelease: "v1.0.2"},
			ComponentSubs:    {},
			ComponentDiscord: {},
		},
	}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	leftover, _ := componentReleaseDirectory(paths, ComponentDiscord, "v1.0.2")
	if err := os.MkdirAll(leftover, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "artifact"), []byte("leftover"), 0644); err != nil {
		t.Fatal(err)
	}

	preview, err := PreviewCleanup(paths, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Paths) != 1 || preview.Paths[0] != "nexus-discord/v1.0.2" {
		t.Fatalf("unexpected cleanup preview: %#v", preview.Paths)
	}
	if _, err := RunCleanup(paths, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("failed component installation release was not removed")
	}
}

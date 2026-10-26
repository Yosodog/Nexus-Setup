package updater

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// withUpdaterUmask runs the rest of a test under the umask the updater's
// systemd units use.
func withUpdaterUmask(t *testing.T) {
	t.Helper()
	previous := syscall.Umask(0077)
	t.Cleanup(func() { syscall.Umask(previous) })
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

// assertReleaseTreeModes checks that every path in a release is readable by
// the service accounts and writable only by its owner.
func assertReleaseTreeModes(t *testing.T, root string, executables map[string]bool) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		want := os.FileMode(0644)
		if entry.IsDir() || executables[filepath.ToSlash(relative)] {
			want = 0755
		}
		assertMode(t, path, want)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// publishTree publishes a component release whose archive holds entries in
// addition to its manifest.
func (provider *fakeReleaseProvider) publishTree(t *testing.T, component Component, release string, entries []archiveEntry) ReleaseManifest {
	t.Helper()
	published := provider.publish(t, component, release, nil)
	manifest := `{"schema_version":2,"component":"` + string(component) + `","release":"` + release + `"}`
	published.artifact = makeTarGz(t, append([]archiveEntry{{name: manifestAsset, typeFlag: tar.TypeReg, mode: 0600, data: manifest}}, entries...))
	digest := sha256.Sum256(published.artifact)
	published.digest = hex.EncodeToString(digest[:])
	result, err := provider.Manifest(context.Background(), component, release)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func restrictiveReleaseEntries() []archiveEntry {
	return []archiveEntry{
		{name: "src/", typeFlag: tar.TypeDir, mode: 0700},
		{name: "src/index.js", typeFlag: tar.TypeReg, mode: 0600, data: "// index"},
		{name: "bin/start", typeFlag: tar.TypeReg, mode: 0700, data: "#!/bin/sh\n"},
		{name: "lib/deep/nested/", typeFlag: tar.TypeDir, mode: 0700},
		{name: "lib/deep/nested/module.js", typeFlag: tar.TypeReg, mode: 0664, data: "// module"},
	}
}

func TestExtractTarGzSetsReleaseModesUnderUpdaterUmask(t *testing.T) {
	withUpdaterUmask(t)
	destination := filepath.Join(t.TempDir(), "stage")
	if _, err := ExtractTarGz(strings.NewReader(string(makeTarGz(t, restrictiveReleaseEntries()))), destination, DefaultArchiveLimits()); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"src", "bin", "lib", "lib/deep", "lib/deep/nested"} {
		assertMode(t, filepath.Join(destination, directory), 0755)
	}
	assertMode(t, filepath.Join(destination, "src", "index.js"), 0644)
	assertMode(t, filepath.Join(destination, "bin", "start"), 0755)
	assertMode(t, filepath.Join(destination, "lib", "deep", "nested", "module.js"), 0644)
}

func TestStageComponentInstallsReadableReleaseUnderUpdaterUmask(t *testing.T) {
	withUpdaterUmask(t)
	provider := newFakeReleaseProvider()
	manifest := provider.publishTree(t, ComponentDiscord, "v1.1.0", restrictiveReleaseEntries())
	paths := NewTestPaths(t.TempDir())
	config := DefaultConfig()
	config.Paths = paths
	engine := DeploymentEngine{Config: config, Releases: provider, Executor: &recordingExecutor{}}
	if err := engine.stageComponent(context.Background(), "stage", manifest); err != nil {
		t.Fatal(err)
	}
	release, err := componentReleaseDirectory(paths, ComponentDiscord, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Dir(release), 0755)
	assertReleaseTreeModes(t, release, map[string]bool{"bin/start": true})
}

func TestStageComponentRepairsModesOfReusedRelease(t *testing.T) {
	provider := newFakeReleaseProvider()
	manifest := provider.publishTree(t, ComponentSubs, "v1.1.0", restrictiveReleaseEntries())
	paths := NewTestPaths(t.TempDir())
	config := DefaultConfig()
	config.Paths = paths
	engine := DeploymentEngine{Config: config, Releases: provider, Executor: &recordingExecutor{}}
	if err := engine.stageComponent(context.Background(), "stage", manifest); err != nil {
		t.Fatal(err)
	}
	release, err := componentReleaseDirectory(paths, ComponentSubs, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a release staged by an updater that relied on its umask.
	err = filepath.WalkDir(release, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()&0700)
	})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(release, "storage")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := engine.stageComponent(context.Background(), "reuse", manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	assertReleaseTreeModes(t, release, map[string]bool{"bin/start": true})
}

type selfUpdateProvider struct {
	emptyReleaseProvider
	binary []byte
}

func (provider selfUpdateProvider) Download(_ context.Context, repository, _, asset, destination string, _ int64, _ func(int64, int64)) error {
	if repository != setupRepository {
		return errors.New("not found")
	}
	switch asset {
	case setupAsset:
		return os.WriteFile(destination, provider.binary, 0600)
	case setupChecksum:
		digest := sha256.Sum256(provider.binary)
		return os.WriteFile(destination, []byte(hex.EncodeToString(digest[:])+"  "+setupAsset+"\n"), 0600)
	default:
		return errors.New("not found")
	}
}

func TestUpdateSelfInstallsReadableReleaseUnderUpdaterUmask(t *testing.T) {
	withUpdaterUmask(t)
	paths := NewTestPaths(t.TempDir())
	config := DefaultConfig()
	config.Paths = paths
	provider := selfUpdateProvider{binary: []byte("#!/bin/sh\necho '{\"version\":\"v1.2.0\"}'\n")}
	engine := DeploymentEngine{Config: config, Releases: provider, Executor: &recordingExecutor{}}
	directory := filepath.Join(paths.UpdaterDir, "releases", "v1.2.0")
	// An earlier updater left this release directory root-only.
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := engine.updateSelf(context.Background(), "self", "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.UpdaterDir, filepath.Dir(directory), directory, filepath.Join(directory, "nexus")} {
		assertMode(t, path, 0755)
	}
	if target, err := os.Readlink(filepath.Join(paths.UpdaterDir, "current")); err != nil || target != directory {
		t.Fatalf("current updater link = %q, %v", target, err)
	}
}

const staleSubsEnvironment = `# written at adoption
NEXUS_RELEASE_ID=v0.3.0
BUILD_COMMIT="v0.3.0"
NEXUS_API_URL="https://nexus.example/api/v1/subs"
NEXUS_RELEASE_ID="v0.3.0"
NODE_ENV="production"
PW_API_TOKEN_FILE="/etc/nexus/credentials/nexus-subs/pw-api-token"
QUOTED="say \"hi\" for $5"
`

func writeTestComponentEnvironment(t *testing.T, paths Paths, component Component, contents string) string {
	t.Helper()
	path := filepath.Join(paths.NexusConfigDir, string(component), "environment")
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	return path
}

// expectedSubsEnvironment is staleSubsEnvironment with every line that sets
// a release key, including a duplicate, pointing at release.
func expectedSubsEnvironment(release string) string {
	return strings.NewReplacer(`=v0.3.0`, `="`+release+`"`, `"v0.3.0"`, `"`+release+`"`).Replace(staleSubsEnvironment)
}

func assertEnvironmentFile(t *testing.T, path, want string, owner *syscall.Stat_t) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("environment file =\n%s\nwant\n%s", data, want)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("environment file mode = %o, want 640", info.Mode().Perm())
	}
	current := info.Sys().(*syscall.Stat_t)
	if current.Uid != owner.Uid || current.Gid != owner.Gid {
		t.Fatalf("environment file owner = %d:%d, want %d:%d", current.Uid, current.Gid, owner.Uid, owner.Gid)
	}
}

func TestUpdateAndRollbackRecordActiveReleaseInEnvironment(t *testing.T) {
	provider := newFakeReleaseProvider()
	provider.publish(t, ComponentSubs, "v0.3.0", nil)
	provider.publish(t, ComponentSubs, "v0.4.0", nil)
	engine, operation := subsOnlyEngine(t, provider, "v0.3.0")
	paths := engine.Config.Paths
	previous, _ := provider.Manifest(context.Background(), ComponentSubs, "v0.3.0")
	seeder := engine
	seeder.Store = nil
	if err := seeder.stageComponent(context.Background(), "seed", previous); err != nil {
		t.Fatal(err)
	}
	if err := atomicSwitchRelease(paths, ComponentSubs, "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	path := writeTestComponentEnvironment(t, paths, ComponentSubs, staleSubsEnvironment)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := info.Sys().(*syscall.Stat_t)

	writeTestHealthDocument(t, paths, ComponentSubs, "v0.4.0", time.Now().UTC())
	if err := engine.update(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	assertEnvironmentFile(t, path, expectedSubsEnvironment("v0.4.0"), owner)

	writeTestHealthDocument(t, paths, ComponentSubs, "v0.3.0", time.Now().UTC())
	finishTestOperation(t, engine.Store, operation.ID)
	rollback := startTestOperation(t, engine.Store, OperationRollback)
	if err := engine.rollback(context.Background(), rollback); err != nil {
		t.Fatal(err)
	}
	assertEnvironmentFile(t, path, expectedSubsEnvironment("v0.3.0"), owner)
}

func TestRestartComponentRepairsStaleReleaseInEnvironment(t *testing.T) {
	provider := newFakeReleaseProvider()
	engine, _ := subsOnlyEngine(t, provider, "v0.4.0")
	paths := engine.Config.Paths
	path := writeTestComponentEnvironment(t, paths, ComponentSubs, staleSubsEnvironment)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestHealthDocument(t, paths, ComponentSubs, "v0.4.0", time.Now().UTC())
	if err := engine.restartComponent(context.Background(), ComponentSubs); err != nil {
		t.Fatal(err)
	}
	assertEnvironmentFile(t, path, expectedSubsEnvironment("v0.4.0"), info.Sys().(*syscall.Stat_t))
}

func TestSwitchingDiscordReleaseKeepsOtherEnvironmentKeys(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	for _, release := range []string{"v1.0.0", "v1.1.0"} {
		directory, err := componentReleaseDirectory(paths, ComponentDiscord, release)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	// No trailing newline, and the release keys are missing, as after a
	// hand edit.
	path := writeTestComponentEnvironment(t, paths, ComponentDiscord, "DISCORD_CLIENT_ID=\"123456789012345678\"\nNODE_ENV=production")
	if err := atomicSwitchRelease(paths, ComponentDiscord, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "DISCORD_CLIENT_ID=\"123456789012345678\"\nNODE_ENV=production\nBUILD_COMMIT=\"v1.1.0\"\nNEXUS_RELEASE_ID=\"v1.1.0\"\n"
	assertEnvironmentFile(t, path, want, info.Sys().(*syscall.Stat_t))
	if value, err := readEnvValue(path, "NEXUS_RELEASE_ID"); err != nil || value != "v1.1.0" {
		t.Fatalf("NEXUS_RELEASE_ID = %q, %v", value, err)
	}
	if err := atomicSwitchRelease(paths, ComponentDiscord, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	assertEnvironmentFile(t, path, strings.ReplaceAll(want, "v1.1.0", "v1.0.0"), info.Sys().(*syscall.Stat_t))
}

func TestSwitchingUnconfiguredComponentLeavesEnvironmentAlone(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	directory, err := componentReleaseDirectory(paths, ComponentDiscord, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := atomicSwitchRelease(paths, ComponentDiscord, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(paths.NexusConfigDir, string(ComponentDiscord), "environment")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("switch created an environment file: %v", err)
	}
}

func TestCopyExecutableIgnoresUpdaterUmask(t *testing.T) {
	withUpdaterUmask(t)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "nexus")
	if err := copyExecutable(source, target); err != nil {
		t.Fatal(err)
	}
	assertMode(t, target, 0755)
}

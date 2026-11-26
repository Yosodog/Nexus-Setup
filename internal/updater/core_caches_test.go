package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sequenceExecutor records systemctl calls into the same log as artisan, so a
// test can check the order of both.
type sequenceExecutor struct {
	log  *[]string
	fail func(arguments []string) error
}

func (executor sequenceExecutor) Run(_ context.Context, _ string, arguments []string) error {
	if arguments[0] == "cat" || arguments[0] == "is-active" {
		return nil
	}
	*executor.log = append(*executor.log, "systemctl "+strings.Join(arguments, " "))
	if executor.fail != nil {
		return executor.fail(arguments)
	}
	return nil
}

type coreCacheHost struct {
	engine    DeploymentEngine
	paths     Paths
	log       *[]string
	shared    string
	failCache string
	// cachedFrom records which release the current link served while each
	// release's config cache was built.
	cachedFrom map[string]string
}

// newCoreCacheHost sets up a web-only host running Core v1.0.0 as an earlier
// updater left it: its bootstrap/cache links to a directory shared by every
// release, holding a config cache built by hand. v1.1.0 is published.
func newCoreCacheHost(t *testing.T) *coreCacheHost {
	t.Helper()
	provider := newFakeReleaseProvider()
	provider.publish(t, ComponentCore, "v1.0.0", nil)
	provider.publish(t, ComponentCore, "v1.1.0", nil)
	paths := NewTestPaths(t.TempDir())
	config := DefaultConfig()
	config.Paths = paths
	store, err := NewOperationStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	log := []string{}
	host := &coreCacheHost{paths: paths, log: &log, cachedFrom: map[string]string{}}
	host.engine = DeploymentEngine{
		Config:        config,
		Store:         store,
		Releases:      provider,
		Executor:      sequenceExecutor{log: host.log},
		ShareWithCore: func(path string, mode os.FileMode) error { return os.Chmod(path, mode) },
		Artisan:       host.artisan,
	}
	environment := filepath.Join(paths.NexusConfigDir, string(ComponentCore), ".env")
	if err := os.MkdirAll(filepath.Dir(environment), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment, []byte("APP_KEY=base64:test\n"), 0640); err != nil {
		t.Fatal(err)
	}
	manifest, _ := provider.Manifest(context.Background(), ComponentCore, "v1.0.0")
	seeder := host.engine
	seeder.Store = nil
	if err := seeder.stageComponent(context.Background(), "seed", manifest); err != nil {
		t.Fatal(err)
	}
	release, _ := componentReleaseDirectory(paths, ComponentCore, "v1.0.0")
	host.shared = filepath.Join(paths.DataDir, string(ComponentCore), "bootstrap-cache")
	if err := os.MkdirAll(host.shared, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host.shared, "config.php"), []byte("v1.0.0"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(release, "bootstrap"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host.shared, filepath.Join(release, "bootstrap", "cache")); err != nil {
		t.Fatal(err)
	}
	if err := atomicSwitchRelease(paths, ComponentCore, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	state := InstallationState{Profile: ProfileWebOnly, Channel: ChannelStable, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	return host
}

func (host *coreCacheHost) artisan(_ context.Context, release string, arguments ...string) error {
	*host.log = append(*host.log, "artisan "+release+" "+strings.Join(arguments, " "))
	if arguments[0] == host.failCache {
		return errors.New("exit status 1")
	}
	if strings.HasSuffix(arguments[0], ":cache") {
		directory, _ := componentReleaseDirectory(host.paths, ComponentCore, release)
		name := strings.TrimSuffix(arguments[0], ":cache") + ".php"
		if err := os.WriteFile(filepath.Join(directory, "bootstrap", "cache", name), []byte(release), 0644); err != nil {
			return err
		}
		if arguments[0] == "config:cache" {
			link, _ := currentLink(host.paths, ComponentCore)
			target, _ := os.Readlink(link)
			host.cachedFrom[release] = filepath.Base(target)
		}
	}
	return nil
}

func (host *coreCacheHost) cachedConfig(t *testing.T, release string) string {
	t.Helper()
	directory, _ := componentReleaseDirectory(host.paths, ComponentCore, release)
	data, err := os.ReadFile(filepath.Join(directory, "bootstrap", "cache", "config.php"))
	if err != nil {
		return ""
	}
	return string(data)
}

func (host *coreCacheHost) currentRelease(t *testing.T) string {
	t.Helper()
	link, _ := currentLink(host.paths, ComponentCore)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(target)
}

func assertOwnCacheDirectory(t *testing.T, paths Paths, release string) {
	t.Helper()
	directory, _ := componentReleaseDirectory(paths, ComponentCore, release)
	info, err := os.Lstat(filepath.Join(directory, "bootstrap", "cache"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s does not have its own cache directory: %v %v", release, info, err)
	}
	if info.Mode().Perm() != 0770 {
		t.Fatalf("%s cache directory mode = %o", release, info.Mode().Perm())
	}
}

var coreUpdateSequence = []string{
	"artisan v1.1.0 config:cache",
	"artisan v1.1.0 route:cache",
	"artisan v1.1.0 event:cache",
	"artisan v1.1.0 view:cache",
	"artisan v1.0.0 down --retry=60",
	"artisan v1.1.0 migrate --force",
	"systemctl restart nexus-core.service",
	"systemctl restart nexus-core-sync.service",
	"systemctl restart nexus-core-pulse-check.service",
	"systemctl restart nexus-core-php-fpm.service",
	"artisan v1.1.0 migrate:status --no-interaction",
	"artisan v1.1.0 up",
}

func TestCoreUpdateBuildsNewReleaseCachesBeforeMaintenance(t *testing.T) {
	host := newCoreCacheHost(t)
	operation := startTestOperation(t, host.engine.Store, OperationUpdate)
	if err := host.engine.update(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*host.log, coreUpdateSequence) {
		t.Fatalf("update ran\n%s\nexpected\n%s", strings.Join(*host.log, "\n"), strings.Join(coreUpdateSequence, "\n"))
	}
	if host.currentRelease(t) != "v1.1.0" || host.cachedFrom["v1.1.0"] != "v1.0.0" {
		t.Fatalf("caches were not built before the switch: current %s, built while %q served", host.currentRelease(t), host.cachedFrom["v1.1.0"])
	}
	assertOwnCacheDirectory(t, host.paths, "v1.1.0")
	if got := host.cachedConfig(t, "v1.1.0"); got != "v1.1.0" {
		t.Fatalf("v1.1.0 config cache = %q", got)
	}
	// The old release keeps the shared cache it was running with untouched,
	// so an automatic switch back still finds matching caches.
	if data, err := os.ReadFile(filepath.Join(host.shared, "config.php")); err != nil || string(data) != "v1.0.0" {
		t.Fatalf("shared cache changed: %q %v", data, err)
	}
	if entries, _ := os.ReadDir(host.shared); len(entries) != 1 {
		t.Fatalf("the new release wrote to the shared cache: %v", entries)
	}
}

func TestCoreUpdateReplacesStaleCachesOfReusedRelease(t *testing.T) {
	host := newCoreCacheHost(t)
	manifest, _ := host.engine.Releases.Manifest(context.Background(), ComponentCore, "v1.1.0")
	seeder := host.engine
	seeder.Store = nil
	if err := seeder.stageComponent(context.Background(), "seed", manifest); err != nil {
		t.Fatal(err)
	}
	if err := host.engine.prepareCoreRelease("v1.1.0"); err != nil {
		t.Fatal(err)
	}
	directory, _ := componentReleaseDirectory(host.paths, ComponentCore, "v1.1.0")
	stale := filepath.Join(directory, "bootstrap", "cache", "packages.php")
	if err := os.WriteFile(stale, []byte("stale"), 0640); err != nil {
		t.Fatal(err)
	}
	operation := startTestOperation(t, host.engine.Store, OperationUpdate)
	if err := host.engine.update(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale package manifest survived the cache rebuild: %v", err)
	}
	assertOwnCacheDirectory(t, host.paths, "v1.1.0")
}

func TestFailedCoreCacheBuildLeavesTheSiteOnTheOldRelease(t *testing.T) {
	host := newCoreCacheHost(t)
	host.failCache = "route:cache"
	operation := startTestOperation(t, host.engine.Store, OperationUpdate)
	err := host.engine.update(context.Background(), operation)
	if err == nil || !strings.Contains(err.Error(), "route:cache") {
		t.Fatalf("update error = %v", err)
	}
	expected := []string{"artisan v1.1.0 config:cache", "artisan v1.1.0 route:cache"}
	if !reflect.DeepEqual(*host.log, expected) {
		t.Fatalf("failed cache build ran %q; it must stop before maintenance mode", *host.log)
	}
	if host.currentRelease(t) != "v1.0.0" {
		t.Fatalf("current release = %s", host.currentRelease(t))
	}
	directory, _ := componentReleaseDirectory(host.paths, ComponentCore, "v1.1.0")
	if entries, err := os.ReadDir(filepath.Join(directory, "bootstrap", "cache")); err != nil || len(entries) != 0 {
		t.Fatalf("partial caches were left behind: %v %v", entries, err)
	}
	state, _ := LoadInstallation(host.paths)
	if state.Components[ComponentCore].CurrentRelease != "v1.0.0" {
		t.Fatalf("failed update changed state: %+v", state.Components[ComponentCore])
	}
}

func TestCoreRollbackRebuildsCachesOfTheRestoredRelease(t *testing.T) {
	host := newCoreCacheHost(t)
	operation := startTestOperation(t, host.engine.Store, OperationUpdate)
	if err := host.engine.update(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	finishTestOperation(t, host.engine.Store, operation.ID)
	*host.log = nil
	rollback := startTestOperation(t, host.engine.Store, OperationRollback)
	if err := host.engine.rollback(context.Background(), rollback); err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"artisan v1.0.0 config:cache",
		"artisan v1.0.0 route:cache",
		"artisan v1.0.0 event:cache",
		"artisan v1.0.0 view:cache",
		"artisan v1.1.0 down --retry=60",
		"systemctl restart nexus-core.service",
		"systemctl restart nexus-core-sync.service",
		"systemctl restart nexus-core-pulse-check.service",
		"systemctl restart nexus-core-php-fpm.service",
		"artisan v1.0.0 migrate:status --no-interaction",
		"artisan v1.0.0 up",
	}
	if !reflect.DeepEqual(*host.log, expected) {
		t.Fatalf("rollback ran\n%s\nexpected\n%s", strings.Join(*host.log, "\n"), strings.Join(expected, "\n"))
	}
	if host.currentRelease(t) != "v1.0.0" || host.cachedFrom["v1.0.0"] != "v1.1.0" {
		t.Fatalf("restored release caches were not built before the switch: current %s, built while %q served", host.currentRelease(t), host.cachedFrom["v1.0.0"])
	}
	// The restored release moves off the shared cache onto its own.
	assertOwnCacheDirectory(t, host.paths, "v1.0.0")
	if got := host.cachedConfig(t, "v1.0.0"); got != "v1.0.0" {
		t.Fatalf("v1.0.0 config cache = %q", got)
	}
	if got := host.cachedConfig(t, "v1.1.0"); got != "v1.1.0" {
		t.Fatalf("rollback touched the caches of the release it left: %q", got)
	}
}

func TestFailedCoreRollbackCacheBuildKeepsTheCurrentRelease(t *testing.T) {
	host := newCoreCacheHost(t)
	operation := startTestOperation(t, host.engine.Store, OperationUpdate)
	if err := host.engine.update(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	finishTestOperation(t, host.engine.Store, operation.ID)
	*host.log = nil
	host.failCache = "view:cache"
	rollback := startTestOperation(t, host.engine.Store, OperationRollback)
	if err := host.engine.rollback(context.Background(), rollback); err == nil {
		t.Fatal("rollback succeeded without caches for the restored release")
	}
	for _, call := range *host.log {
		if strings.Contains(call, " down ") || strings.HasPrefix(call, "systemctl") {
			t.Fatalf("rollback continued after its cache build failed: %q", *host.log)
		}
	}
	if host.currentRelease(t) != "v1.1.0" || host.cachedConfig(t, "v1.1.0") != "v1.1.0" {
		t.Fatalf("failed rollback disturbed the serving release")
	}
}

func TestFailedCoreActivationSwitchesBackToMatchingCaches(t *testing.T) {
	host := newCoreCacheHost(t)
	failed := false
	host.engine.Executor = sequenceExecutor{log: host.log, fail: func(arguments []string) error {
		if !failed && arguments[0] == "restart" && arguments[1] == "nexus-core.service" {
			failed = true
			return errors.New("exit status 1")
		}
		return nil
	}}
	operation := startTestOperation(t, host.engine.Store, OperationUpdate)
	if err := host.engine.update(context.Background(), operation); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("update error = %v", err)
	}
	expected := append(append([]string{}, coreUpdateSequence[:7]...),
		"artisan v1.1.0 down --retry=60",
		"systemctl restart nexus-core.service",
		"systemctl restart nexus-core-sync.service",
		"systemctl restart nexus-core-pulse-check.service",
		"systemctl restart nexus-core-php-fpm.service",
		"artisan v1.0.0 migrate:status --no-interaction",
		"artisan v1.0.0 up",
	)
	if !reflect.DeepEqual(*host.log, expected) {
		t.Fatalf("failed activation ran\n%s\nexpected\n%s", strings.Join(*host.log, "\n"), strings.Join(expected, "\n"))
	}
	if host.currentRelease(t) != "v1.0.0" {
		t.Fatalf("current release = %s", host.currentRelease(t))
	}
	// The old release goes back to exactly the caches it was serving with.
	release, _ := componentReleaseDirectory(host.paths, ComponentCore, "v1.0.0")
	if target, err := os.Readlink(filepath.Join(release, "bootstrap", "cache")); err != nil || target != host.shared {
		t.Fatalf("old release cache link = %q %v", target, err)
	}
	if data, _ := os.ReadFile(filepath.Join(host.shared, "config.php")); string(data) != "v1.0.0" {
		t.Fatalf("old release config cache = %q", data)
	}
}

func TestReusedCoreReleaseKeepsCacheDirectoryModes(t *testing.T) {
	host := newCoreCacheHost(t)
	if err := host.engine.prepareCoreRelease("v1.0.0"); err != nil {
		t.Fatal(err)
	}
	release, _ := componentReleaseDirectory(host.paths, ComponentCore, "v1.0.0")
	cached := filepath.Join(release, "bootstrap", "cache", "config.php")
	if err := os.WriteFile(cached, []byte("secrets"), 0640); err != nil {
		t.Fatal(err)
	}
	manifest, _ := host.engine.Releases.Manifest(context.Background(), ComponentCore, "v1.0.0")
	seeder := host.engine
	seeder.Store = nil
	if err := seeder.stageComponent(context.Background(), "reuse", manifest); err != nil {
		t.Fatal(err)
	}
	assertOwnCacheDirectory(t, host.paths, "v1.0.0")
	if info, err := os.Stat(cached); err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("mode repair changed the cached config: %v %v", info, err)
	}
}

func TestCoreRestartRebuildsCachesBeforeRestartingUnits(t *testing.T) {
	host := newCoreCacheHost(t)
	if err := host.engine.restartComponent(context.Background(), ComponentCore); err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"artisan v1.0.0 config:cache",
		"artisan v1.0.0 route:cache",
		"artisan v1.0.0 event:cache",
		"artisan v1.0.0 view:cache",
		"systemctl restart nexus-core.service",
		"systemctl restart nexus-core-sync.service",
		"systemctl restart nexus-core-pulse-check.service",
		"systemctl restart nexus-core-php-fpm.service",
		"artisan v1.0.0 migrate:status --no-interaction",
	}
	if !reflect.DeepEqual(*host.log, expected) {
		t.Fatalf("restart ran\n%s\nexpected\n%s", strings.Join(*host.log, "\n"), strings.Join(expected, "\n"))
	}
}

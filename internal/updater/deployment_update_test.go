package updater

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeRelease struct {
	requires map[string]string
	artifact []byte
	digest   string
}

type fakeReleaseProvider struct {
	releases map[Component]map[string]*fakeRelease
}

func newFakeReleaseProvider() *fakeReleaseProvider {
	return &fakeReleaseProvider{releases: map[Component]map[string]*fakeRelease{}}
}

func (provider *fakeReleaseProvider) publish(t *testing.T, component Component, release string, requires map[string]string) *fakeRelease {
	t.Helper()
	manifest := `{"schema_version":2,"component":"` + string(component) + `","release":"` + release + `"`
	if len(requires) > 0 {
		manifest += `,"requires":{`
		first := true
		for name, minimum := range requires {
			if !first {
				manifest += ","
			}
			manifest += `"` + name + `":"` + minimum + `"`
			first = false
		}
		manifest += "}"
	}
	manifest += "}"
	artifact := makeTarGz(t, []archiveEntry{
		{name: manifestAsset, typeFlag: tar.TypeReg, mode: 0644, data: manifest},
		{name: "src/index.js", typeFlag: tar.TypeReg, mode: 0644, data: "// " + release},
	})
	digest := sha256.Sum256(artifact)
	entry := &fakeRelease{requires: requires, artifact: artifact, digest: hex.EncodeToString(digest[:])}
	if provider.releases[component] == nil {
		provider.releases[component] = map[string]*fakeRelease{}
	}
	provider.releases[component][release] = entry
	return entry
}

func (provider *fakeReleaseProvider) Releases(_ context.Context, repository string, channel Channel) ([]ReleaseInfo, error) {
	var list []ReleaseInfo
	for _, component := range planOrder {
		componentRepository, _, _ := componentReleaseAsset(component)
		if componentRepository != repository {
			continue
		}
		for tag := range provider.releases[component] {
			if releaseOnChannel(tag, false, channel) {
				list = append(list, ReleaseInfo{Tag: tag})
			}
		}
	}
	sortReleases(list)
	return list, nil
}

func (provider *fakeReleaseProvider) Manifest(_ context.Context, component Component, release string) (ReleaseManifest, error) {
	entry := provider.releases[component][release]
	if entry == nil {
		return ReleaseManifest{}, errors.New("not found")
	}
	return ReleaseManifest{SchemaVersion: 2, Component: component, Release: release, Requires: entry.requires, ArtifactSHA256: entry.digest}, nil
}

func (provider *fakeReleaseProvider) Download(_ context.Context, repository, tag, _ string, destination string, _ int64, _ func(int64, int64)) error {
	for _, component := range planOrder {
		componentRepository, _, _ := componentReleaseAsset(component)
		if componentRepository == repository {
			if entry := provider.releases[component][tag]; entry != nil {
				return os.WriteFile(destination, entry.artifact, 0600)
			}
		}
	}
	return errors.New("not found")
}

func startTestOperation(t *testing.T, store *OperationStore, operation Operation) OperationState {
	t.Helper()
	request := testRequest(operation)
	id, err := NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	request.ID, request.OperationID = id, id
	state, _, err := store.CreateOrGet(context.Background(), request)
	if err == nil {
		state, err = store.Update(state.ID, func(current *OperationState) error {
			current.Status = OperationRunning
			return nil
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func finishTestOperation(t *testing.T, store *OperationStore, id string) {
	t.Helper()
	if _, err := store.Update(id, func(current *OperationState) error {
		current.Status = OperationCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func subsOnlyEngine(t *testing.T, provider ReleaseProvider, installed string) (DeploymentEngine, OperationState) {
	t.Helper()
	paths := NewTestPaths(t.TempDir())
	state := InstallationState{Profile: ProfileSubsOnly, Channel: ChannelStable, Components: map[Component]InstalledComponent{
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: installed},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	store, err := NewOperationStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	operation := startTestOperation(t, store, OperationUpdate)
	config := DefaultConfig()
	config.Paths = paths
	return DeploymentEngine{Config: config, Store: store, Releases: provider, Executor: &recordingExecutor{}}, operation
}

func TestUpdateThenRollbackSwitchesOnlyChangedComponent(t *testing.T) {
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

	writeTestHealthDocument(t, paths, ComponentSubs, "v0.4.0", time.Now().UTC())
	if err := engine.update(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	state, err := LoadInstallation(paths)
	if err != nil {
		t.Fatal(err)
	}
	subs := state.Components[ComponentSubs]
	if subs.CurrentRelease != "v0.4.0" || subs.PreviousRelease != "v0.3.0" || len(state.LastChanged) != 1 {
		t.Fatalf("unexpected state after update: %+v", state)
	}
	link, _ := currentLink(paths, ComponentSubs)
	if target, _ := os.Readlink(link); filepath.Base(target) != "v0.4.0" {
		t.Fatalf("current link points at %s", target)
	}
	recorded, _ := engine.Store.Load(operation.ID)
	if len(recorded.Releases) != 1 || recorded.Releases[0] != (ComponentRelease{Component: ComponentSubs, Release: "v0.4.0"}) {
		t.Fatalf("operation did not record its target releases: %+v", recorded.Releases)
	}

	writeTestHealthDocument(t, paths, ComponentSubs, "v0.3.0", time.Now().UTC())
	finishTestOperation(t, engine.Store, operation.ID)
	rollback := startTestOperation(t, engine.Store, OperationRollback)
	if err := engine.rollback(context.Background(), rollback); err != nil {
		t.Fatal(err)
	}
	state, _ = LoadInstallation(paths)
	if state.Components[ComponentSubs].CurrentRelease != "v0.3.0" || state.Components[ComponentSubs].PreviousRelease != "v0.4.0" {
		t.Fatalf("unexpected state after rollback: %+v", state.Components[ComponentSubs])
	}
	if target, _ := os.Readlink(link); filepath.Base(target) != "v0.3.0" {
		t.Fatalf("current link points at %s after rollback", target)
	}
}

func TestUpdateRejectsArtifactWithWrongDigest(t *testing.T) {
	provider := newFakeReleaseProvider()
	provider.publish(t, ComponentSubs, "v0.4.0", nil).digest = hex.EncodeToString(make([]byte, 32))
	engine, operation := subsOnlyEngine(t, provider, "v0.3.0")
	if err := engine.update(context.Background(), operation); err == nil {
		t.Fatal("artifact with a mismatched digest was installed")
	}
	state, _ := LoadInstallation(engine.Config.Paths)
	if state.Components[ComponentSubs].CurrentRelease != "v0.3.0" {
		t.Fatalf("failed update changed state: %+v", state.Components[ComponentSubs])
	}
}

func TestRollbackRefusesIncompatibleResult(t *testing.T) {
	provider := newFakeReleaseProvider()
	provider.publish(t, ComponentCore, "v1.0.0", nil)
	provider.publish(t, ComponentCore, "v1.1.0", nil)
	provider.publish(t, ComponentSubs, "v0.4.0", map[string]string{"nexus-core": "v1.1.0"})
	paths := NewTestPaths(t.TempDir())
	config := DefaultConfig()
	config.Paths = paths
	engine := DeploymentEngine{Config: config, Releases: provider, Executor: &recordingExecutor{}}
	for _, release := range []ComponentRelease{{ComponentCore, "v1.0.0"}, {ComponentCore, "v1.1.0"}, {ComponentSubs, "v0.4.0"}} {
		manifest, _ := provider.Manifest(context.Background(), release.Component, release.Release)
		if err := engine.stageComponent(context.Background(), "seed", manifest); err != nil {
			t.Fatal(err)
		}
	}
	state := InstallationState{Profile: ProfileFull, Channel: ChannelStable, LastChanged: []Component{ComponentCore}, Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.1.0", PreviousRelease: "v1.0.0"},
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v0.4.0"},
	}}
	if err := SaveInstallation(paths, state); err != nil {
		t.Fatal(err)
	}
	if err := engine.rollback(context.Background(), OperationState{ID: "00000000-0000-4000-8000-000000000000"}); err == nil {
		t.Fatal("rollback left Subs on a release that needs newer Core")
	}
}

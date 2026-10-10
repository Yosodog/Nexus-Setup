package updater

import (
	"errors"
	"strings"
	"testing"
)

type manifestTable map[ComponentRelease]map[string]string

func (table manifestTable) source(component Component, release string) (ReleaseManifest, error) {
	requires, exists := table[ComponentRelease{Component: component, Release: release}]
	if !exists {
		return ReleaseManifest{}, errors.New("manifest asset is missing")
	}
	return ReleaseManifest{SchemaVersion: 2, Component: component, Release: release, Requires: requires, ArtifactSHA256: strings.Repeat("0", 64)}, nil
}

func releasesOf(tags ...string) []ReleaseInfo {
	releases := make([]ReleaseInfo, 0, len(tags))
	for _, tag := range tags {
		releases = append(releases, ReleaseInfo{Tag: tag})
	}
	return releases
}

func planTargets(plan UpdatePlan) map[Component]string {
	targets := map[Component]string{}
	for _, change := range plan.Changes {
		targets[change.Component] = change.To
	}
	return targets
}

func TestPlanUpdatesComponentsIndependently(t *testing.T) {
	current := map[Component]string{ComponentCore: "v1.4.0", ComponentSubs: "v0.3.0"}
	available := map[Component][]ReleaseInfo{
		ComponentCore: releasesOf("v1.3.0", "v1.4.0"),
		ComponentSubs: releasesOf("v0.3.0", "v0.3.1", "v0.4.0"),
	}
	manifests := manifestTable{
		{ComponentSubs, "v0.3.1"}: nil,
		{ComponentSubs, "v0.4.0"}: nil,
	}
	plan, err := PlanReleases(current, available, "v1.0.0", manifests.source)
	if err != nil {
		t.Fatal(err)
	}
	targets := planTargets(plan)
	if len(plan.Changes) != 1 || targets[ComponentSubs] != "v0.4.0" {
		t.Fatalf("expected only Subs to move straight to v0.4.0, got %+v", plan.Changes)
	}
	if len(plan.HeldBack) != 0 {
		t.Fatalf("nothing should be held back: %+v", plan.HeldBack)
	}
}

func TestPlanHoldsBackReleaseWhoseRequirementIsNotAvailable(t *testing.T) {
	current := map[Component]string{ComponentCore: "v1.4.0", ComponentSubs: "v0.3.0"}
	available := map[Component][]ReleaseInfo{
		ComponentCore: releasesOf("v1.4.0"),
		ComponentSubs: releasesOf("v0.3.1", "v0.4.0"),
	}
	manifests := manifestTable{
		{ComponentSubs, "v0.3.1"}: {"nexus-core": "v1.4.0"},
		{ComponentSubs, "v0.4.0"}: {"nexus-core": "v1.5.0"},
	}
	plan, err := PlanReleases(current, available, "v1.0.0", manifests.source)
	if err != nil {
		t.Fatal(err)
	}
	if targets := planTargets(plan); targets[ComponentSubs] != "v0.3.1" {
		t.Fatalf("expected Subs v0.3.1, got %+v", plan.Changes)
	}
	if len(plan.HeldBack) != 1 || plan.HeldBack[0].Release != "v0.4.0" || !strings.Contains(plan.HeldBack[0].Reason, "nexus-core v1.5.0") {
		t.Fatalf("unexpected held-back releases: %+v", plan.HeldBack)
	}
}

func TestPlanUpdatesRequiredComponentInTheSameRun(t *testing.T) {
	current := map[Component]string{ComponentCore: "v1.4.0", ComponentSubs: "v0.3.0"}
	available := map[Component][]ReleaseInfo{
		ComponentCore: releasesOf("v1.5.0"),
		ComponentSubs: releasesOf("v0.4.0"),
	}
	manifests := manifestTable{
		{ComponentCore, "v1.5.0"}: nil,
		{ComponentSubs, "v0.4.0"}: {"nexus-core": "v1.5.0"},
	}
	plan, err := PlanReleases(current, available, "v1.0.0", manifests.source)
	if err != nil {
		t.Fatal(err)
	}
	targets := planTargets(plan)
	if targets[ComponentCore] != "v1.5.0" || targets[ComponentSubs] != "v0.4.0" {
		t.Fatalf("expected Core and Subs to update together, got %+v", plan.Changes)
	}
}

func TestPlanStepsDownWhenDependencyIsHeldBack(t *testing.T) {
	current := map[Component]string{ComponentCore: "v1.4.0", ComponentSubs: "v0.3.0"}
	available := map[Component][]ReleaseInfo{
		ComponentCore: releasesOf("v1.5.0"),
		ComponentSubs: releasesOf("v0.4.0"),
	}
	manifests := manifestTable{
		{ComponentCore, "v1.5.0"}: {RequirementUpdater: "v2.0.0"},
		{ComponentSubs, "v0.4.0"}: {"nexus-core": "v1.5.0"},
	}
	plan, err := PlanReleases(current, available, "v1.0.0", manifests.source)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 || len(plan.HeldBack) != 2 {
		t.Fatalf("expected both releases to be held back, got changes=%+v held=%+v", plan.Changes, plan.HeldBack)
	}
	if !plan.HeldBack[0].NeedsUpdater || plan.HeldBack[1].NeedsUpdater {
		t.Fatalf("only Core should report that it needs a newer updater: %+v", plan.HeldBack)
	}
}

func TestPlanSkipsReleasesWithoutManifest(t *testing.T) {
	current := map[Component]string{ComponentCore: "v1.4.0"}
	available := map[Component][]ReleaseInfo{ComponentCore: releasesOf("v1.4.1", "v1.5.0")}
	manifests := manifestTable{{ComponentCore, "v1.4.1"}: nil}
	plan, err := PlanReleases(current, available, "v1.0.0", manifests.source)
	if err != nil {
		t.Fatal(err)
	}
	if targets := planTargets(plan); targets[ComponentCore] != "v1.4.1" {
		t.Fatalf("expected Core v1.4.1, got %+v", plan.Changes)
	}
}

func TestPlanIgnoresRequirementsOnComponentsNotOnThisHost(t *testing.T) {
	current := map[Component]string{ComponentSubs: ""}
	available := map[Component][]ReleaseInfo{ComponentSubs: releasesOf("v0.4.0")}
	manifests := manifestTable{{ComponentSubs, "v0.4.0"}: {"nexus-core": "v9.0.0"}}
	plan, err := PlanReleases(current, available, "v1.0.0", manifests.source)
	if err != nil {
		t.Fatal(err)
	}
	if targets := planTargets(plan); targets[ComponentSubs] != "v0.4.0" {
		t.Fatalf("subs-only install should not depend on remote Core: %+v", plan.Changes)
	}
}

func TestPlanFreshInstallFailsWithoutCompatibleRelease(t *testing.T) {
	current := map[Component]string{ComponentCore: "", ComponentSubs: ""}
	available := map[Component][]ReleaseInfo{
		ComponentCore: releasesOf("v1.0.0"),
		ComponentSubs: releasesOf("v0.4.0"),
	}
	manifests := manifestTable{
		{ComponentCore, "v1.0.0"}: nil,
		{ComponentSubs, "v0.4.0"}: {"nexus-core": "v2.0.0"},
	}
	if _, err := PlanReleases(current, available, "v1.0.0", manifests.source); err == nil || !strings.Contains(err.Error(), "nexus-subs") {
		t.Fatalf("expected a missing Subs release error, got %v", err)
	}
}

func TestPlanDoesNotDowngradeAfterLeavingBeta(t *testing.T) {
	current := map[Component]string{ComponentCore: "v1.5.0-beta.2"}
	available := map[Component][]ReleaseInfo{ComponentCore: releasesOf("v1.4.0")}
	plan, err := PlanReleases(current, available, "v1.0.0", manifestTable{}.source)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("stable channel downgraded a beta install: %+v", plan.Changes)
	}
}

func TestLoadInstallationMigratesSharedReleaseState(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	legacy := installationStateV1{Version: 1, Profile: ProfileFull, CurrentRelease: "v1.0.1", PreviousRelease: "v1.0.0", Components: map[Component]InstalledComponent{
		ComponentCore: {Installed: true, Enabled: true, CurrentRelease: "v1.0.1", PreviousRelease: "v1.0.0"},
		ComponentSubs: {Installed: true, Enabled: true, CurrentRelease: "v1.0.1", PreviousRelease: "v1.0.0"},
	}}
	if err := atomicWriteJSON(paths.InstallationDB, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadInstallation(paths)
	if err != nil {
		t.Fatal(err)
	}
	if state.Channel != ChannelStable || len(state.LastChanged) != 2 || state.Components[ComponentSubs].PreviousRelease != "v1.0.0" {
		t.Fatalf("unexpected migrated state: %+v", state)
	}
}

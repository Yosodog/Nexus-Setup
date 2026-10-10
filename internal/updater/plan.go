package updater

import (
	"context"
	"fmt"
	"time"
)

// ComponentRelease names one component at one release.
type ComponentRelease struct {
	Component Component `json:"component_id"`
	Release   string    `json:"release_id"`
}

// ComponentChange is one component moving to a newer release.
type ComponentChange struct {
	Component   Component `json:"component_id"`
	From        string    `json:"from_release,omitempty"`
	To          string    `json:"to_release"`
	Notes       string    `json:"release_notes,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
}

// HeldBackRelease is a newer release that cannot be installed yet because one
// of its requirements is not met.
type HeldBackRelease struct {
	Component    Component `json:"component_id"`
	Release      string    `json:"release_id"`
	Reason       string    `json:"reason"`
	NeedsUpdater bool      `json:"needs_updater,omitempty"`
}

// UpdatePlan is the set of component releases an update or installation will
// apply. Each component is versioned independently; manifests declare the
// minimum versions of other components they need.
type UpdatePlan struct {
	Changes   []ComponentChange
	HeldBack  []HeldBackRelease
	Manifests map[Component]ReleaseManifest
}

type manifestSource func(Component, string) (ReleaseManifest, error)

var planOrder = []Component{ComponentCore, ComponentSubs, ComponentDiscord}

// PlanReleases chooses, for every component in current, the newest available
// release whose requirements are satisfied by the other chosen releases and the
// running updater. current maps each component to its installed release, or to
// "" when the component is being installed for the first time. available lists
// each component's channel-visible releases.
func PlanReleases(current map[Component]string, available map[Component][]ReleaseInfo, updaterVersion string, manifests manifestSource) (UpdatePlan, error) {
	cache := map[ComponentRelease]manifestResult{}
	lookup := func(component Component, release string) (ReleaseManifest, error) {
		key := ComponentRelease{Component: component, Release: release}
		if result, cached := cache[key]; cached {
			return result.manifest, result.err
		}
		manifest, err := manifests(component, release)
		cache[key] = manifestResult{manifest: manifest, err: err}
		return manifest, err
	}

	candidates := map[Component][]ReleaseInfo{}
	index := map[Component]int{}
	targets := map[Component]string{}
	for _, component := range planOrder {
		installed, present := current[component]
		if !present {
			continue
		}
		for _, release := range available[component] {
			if _, err := ParseVersion(release.Tag); err != nil {
				continue
			}
			if installed == "" || CompareTags(release.Tag, installed) > 0 {
				candidates[component] = append(candidates[component], release)
			}
		}
		sortReleases(candidates[component])
		index[component] = len(candidates[component]) - 1
		targets[component] = installed
		if index[component] >= 0 {
			targets[component] = candidates[component][index[component]].Tag
		}
	}

	rejection := func(component Component, release string) string {
		manifest, err := lookup(component, release)
		if err != nil {
			return "its release manifest is unavailable"
		}
		return unmetRequirement(manifest, targets, updaterVersion)
	}
	for changed := true; changed; {
		changed = false
		for _, component := range planOrder {
			if _, present := current[component]; !present || index[component] < 0 {
				continue
			}
			if rejection(component, targets[component]) == "" {
				continue
			}
			index[component]--
			targets[component] = current[component]
			if index[component] >= 0 {
				targets[component] = candidates[component][index[component]].Tag
			}
			changed = true
		}
	}

	plan := UpdatePlan{Manifests: map[Component]ReleaseManifest{}}
	for _, component := range planOrder {
		installed, present := current[component]
		if !present {
			continue
		}
		if installed == "" && index[component] < 0 {
			return UpdatePlan{}, fmt.Errorf("no compatible %s release is available", component)
		}
		if index[component] >= 0 {
			chosen := candidates[component][index[component]]
			manifest, err := lookup(component, chosen.Tag)
			if err != nil {
				return UpdatePlan{}, err
			}
			plan.Manifests[component] = manifest
			plan.Changes = append(plan.Changes, ComponentChange{Component: component, From: installed, To: chosen.Tag, Notes: chosen.Notes, PublishedAt: chosen.PublishedAt})
		}
		newest := len(candidates[component]) - 1
		if newest > index[component] {
			release := candidates[component][newest].Tag
			reason := rejection(component, release)
			if reason == "" {
				reason = "a release it depends on is held back"
			}
			needsUpdater := false
			if manifest, err := lookup(component, release); err == nil {
				if minimum, required := manifest.Requires[RequirementUpdater]; required {
					_, parseErr := ParseVersion(updaterVersion)
					needsUpdater = parseErr == nil && CompareTags(updaterVersion, minimum) < 0
				}
			}
			plan.HeldBack = append(plan.HeldBack, HeldBackRelease{Component: component, Release: release, Reason: reason, NeedsUpdater: needsUpdater})
		}
	}
	return plan, nil
}

type manifestResult struct {
	manifest ReleaseManifest
	err      error
}

// planForComponents discovers the channel's releases for each component and
// plans the newest compatible set.
func planForComponents(ctx context.Context, releases ReleaseProvider, channel Channel, current map[Component]string) (UpdatePlan, error) {
	available := map[Component][]ReleaseInfo{}
	for component := range current {
		repository, _, err := componentReleaseAsset(component)
		if err != nil {
			return UpdatePlan{}, err
		}
		list, err := releases.Releases(ctx, repository, channel)
		if err != nil {
			return UpdatePlan{}, fmt.Errorf("discover %s releases: %w", component, err)
		}
		available[component] = list
	}
	return PlanReleases(current, available, BuildVersion, func(component Component, release string) (ReleaseManifest, error) {
		return releases.Manifest(ctx, component, release)
	})
}

// latestUpdaterRelease returns the newest Nexus-Setup release on the channel
// that is newer than the running updater, or "" when none is.
func latestUpdaterRelease(ctx context.Context, releases ReleaseProvider, channel Channel) (ReleaseInfo, error) {
	list, err := releases.Releases(ctx, setupRepository, channel)
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("discover updater releases: %w", err)
	}
	if len(list) == 0 {
		return ReleaseInfo{}, nil
	}
	latest := list[len(list)-1]
	if _, err := ParseVersion(BuildVersion); err != nil {
		return ReleaseInfo{}, nil
	}
	if CompareTags(latest.Tag, BuildVersion) <= 0 {
		return ReleaseInfo{}, nil
	}
	return latest, nil
}

// checkInstalledCompatibility verifies every component's embedded manifest
// against the resulting set of versions.
func checkInstalledCompatibility(paths Paths, versions map[Component]string) error {
	for _, component := range planOrder {
		release := versions[component]
		if release == "" {
			continue
		}
		directory, err := componentReleaseDirectory(paths, component, release)
		if err != nil {
			return err
		}
		manifest, err := ReadArtifactManifest(directory, component, release)
		if err != nil {
			return fmt.Errorf("%s %s: %w", component, release, err)
		}
		if reason := unmetRequirement(manifest, versions, BuildVersion); reason != "" {
			return fmt.Errorf("%s %s %s", component, release, reason)
		}
	}
	return nil
}

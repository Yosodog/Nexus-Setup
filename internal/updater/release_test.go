package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersionRejectsLooseTags(t *testing.T) {
	for _, release := range []string{"1.0.0", "v1.0", "v01.0.0", "v1.0.0-", "v1.0.0-beta..1", "v1.0.0-beta.01", "v1.0.0+build", "v1.0.0-beta_1"} {
		if _, err := ParseVersion(release); err == nil {
			t.Fatalf("invalid release %q was accepted", release)
		}
	}
}

func TestParseVersionAcceptsPrereleaseTags(t *testing.T) {
	for _, release := range []string{"v0.8.0-beta", "v1.0.0-beta.1", "v1.0.0-rc.2", "v1.0.0-alpha.0"} {
		version, err := ParseVersion(release)
		if err != nil {
			t.Fatalf("release %q was rejected: %v", release, err)
		}
		if version.Stable() {
			t.Fatalf("release %q was reported as stable", release)
		}
	}
}

func TestVersionPrecedence(t *testing.T) {
	ordered := []string{"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "v1.0.1-beta.1", "v1.0.1"}
	for index := 0; index+1 < len(ordered); index++ {
		if CompareTags(ordered[index], ordered[index+1]) >= 0 {
			t.Fatalf("%s should sort before %s", ordered[index], ordered[index+1])
		}
	}
}

func TestReleaseOnChannel(t *testing.T) {
	cases := []struct {
		tag        string
		prerelease bool
		channel    Channel
		visible    bool
	}{
		{"v1.0.0", false, ChannelStable, true},
		{"v1.0.0", true, ChannelStable, false},
		{"v1.0.0-beta.1", true, ChannelStable, false},
		{"v1.0.0-beta.1", false, ChannelStable, false},
		{"v1.0.0-beta.1", true, ChannelBeta, true},
		{"v1.0.0", false, ChannelBeta, true},
		{"not-a-tag", false, ChannelBeta, false},
	}
	for _, testCase := range cases {
		if got := releaseOnChannel(testCase.tag, testCase.prerelease, testCase.channel); got != testCase.visible {
			t.Fatalf("releaseOnChannel(%q, %v, %s) = %v", testCase.tag, testCase.prerelease, testCase.channel, got)
		}
	}
}

func TestPublishedManifestValidation(t *testing.T) {
	digest := strings.Repeat("a", 64)
	valid := ReleaseManifest{SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.0", Requires: map[string]string{"nexus-core": "v1.1.0", RequirementUpdater: "v1.0.0-beta.1"}, ArtifactSHA256: digest}
	if err := valid.validate(ComponentSubs, "v1.2.0", true); err != nil {
		t.Fatalf("valid manifest was rejected: %v", err)
	}
	invalid := map[string]ReleaseManifest{
		"schema":         {SchemaVersion: 1, Component: ComponentSubs, Release: "v1.2.0", ArtifactSHA256: digest},
		"component":      {SchemaVersion: 2, Component: ComponentCore, Release: "v1.2.0", ArtifactSHA256: digest},
		"release":        {SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.1", ArtifactSHA256: digest},
		"self":           {SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.0", Requires: map[string]string{"nexus-subs": "v1.0.0"}, ArtifactSHA256: digest},
		"unknown":        {SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.0", Requires: map[string]string{"nexus-web": "v1.0.0"}, ArtifactSHA256: digest},
		"minimum":        {SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.0", Requires: map[string]string{"nexus-core": "1.0"}, ArtifactSHA256: digest},
		"missing digest": {SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.0"},
		"upper digest":   {SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.0", ArtifactSHA256: strings.Repeat("A", 64)},
	}
	for name, manifest := range invalid {
		if err := manifest.validate(ComponentSubs, "v1.2.0", true); err == nil {
			t.Fatalf("%s: invalid manifest was accepted", name)
		}
	}
	embedded := valid
	if err := embedded.validate(ComponentSubs, "v1.2.0", false); err == nil {
		t.Fatal("embedded manifest with a digest was accepted")
	}
}

func TestArtifactManifestMustMatchPublishedRequirements(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, manifestAsset), []byte(`{"schema_version":2,"component":"nexus-subs","release":"v1.2.0","requires":{"nexus-core":"v1.1.0"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	published := ReleaseManifest{SchemaVersion: 2, Component: ComponentSubs, Release: "v1.2.0", Requires: map[string]string{"nexus-core": "v1.1.0"}, ArtifactSHA256: strings.Repeat("0", 64)}
	if err := ValidateArtifactManifest(directory, published); err != nil {
		t.Fatalf("matching artifact was rejected: %v", err)
	}
	published.Requires = map[string]string{"nexus-core": "v1.0.0"}
	if err := ValidateArtifactManifest(directory, published); err == nil {
		t.Fatal("artifact with different requirements was accepted")
	}
}

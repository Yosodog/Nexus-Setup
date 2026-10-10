package updater

import "testing"

func TestUpdatesAfterOrdersSequentialReleases(t *testing.T) {
	releases := []ReleaseInfo{{Tag: "v1.1.0"}, {Tag: "v1.0.2"}, {Tag: "v1.0.1"}}

	updates, err := UpdatesAfter(releases, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 3 || updates[0].Tag != "v1.0.1" || updates[2].Tag != "v1.1.0" {
		t.Fatalf("unexpected update sequence: %+v", updates)
	}
}

func TestUpdatesAfterRejectsMissingIntermediateRelease(t *testing.T) {
	releases := []ReleaseInfo{{Tag: "v1.0.2"}}

	if _, err := UpdatesAfter(releases, "v1.0.0"); err == nil {
		t.Fatal("missing intermediate release was accepted")
	}
}

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

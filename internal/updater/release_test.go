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

func TestParseVersionRejectsPrereleaseAndLooseTags(t *testing.T) {
	for _, release := range []string{"1.0.0", "v1.0", "v1.0.0-beta", "v01.0.0"} {
		if _, err := ParseVersion(release); err == nil {
			t.Fatalf("invalid release %q was accepted", release)
		}
	}
}

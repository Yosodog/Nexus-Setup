package updater

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestPHPPackageVersionKeepsTheInstalledPHP(t *testing.T) {
	cases := []struct {
		name      string
		platform  Platform
		installed string
		want      string
	}{
		{"installed 8.3 on Ubuntu 24.04", Platform{ID: "ubuntu", Version: "24.04"}, "8.3", "8.3"},
		{"installed 8.4 on Ubuntu 22.04", Platform{ID: "ubuntu", Version: "22.04"}, "8.4", "8.4"},
		{"no PHP on Ubuntu 24.04", Platform{ID: "ubuntu", Version: "24.04"}, "", "8.3"},
		{"no PHP on Ubuntu 22.04", Platform{ID: "ubuntu", Version: "22.04"}, "", "8.3"},
		{"no PHP on Debian 12", Platform{ID: "debian", Version: "12"}, "", "8.3"},
		{"no PHP on Debian 13", Platform{ID: "debian", Version: "13"}, "", "8.4"},
		{"too old PHP on Ubuntu 22.04", Platform{ID: "ubuntu", Version: "22.04"}, "8.1", "8.3"},
		{"no PHP and unknown default", Platform{ID: "ubuntu", Version: "26.04"}, "", ""},
		{"installed PHP and unknown default", Platform{ID: "ubuntu", Version: "26.04"}, "8.5", "8.5"},
	}
	for _, test := range cases {
		if got := phpPackageVersion(test.platform, test.installed); got != test.want {
			t.Errorf("%s: got %q, want %q", test.name, got, test.want)
		}
	}
}

func TestPHPVersionSupported(t *testing.T) {
	for version, want := range map[string]bool{"8.3": true, "8.4": true, "8.10": true, "9.0": true, "8.2": false, "7.4": false, "": false, "8": false, "8.3.1": false} {
		if got := phpVersionSupported(version); got != want {
			t.Errorf("phpVersionSupported(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestPHPPackagesAreVersioned(t *testing.T) {
	got := phpPackages("8.3", true)
	want := []string{"php8.3-fpm", "php8.3-cli", "php8.3-mysql", "php8.3-mbstring", "php8.3-xml", "php8.3-curl", "php8.3-zip", "php8.3-bcmath", "php8.3-gd", "php8.3-intl", "php8.3-redis"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected versioned packages: %#v", got)
	}
	unversioned := phpPackages("", false)
	if unversioned[0] != "php-fpm" || unversioned[len(unversioned)-1] != "php-intl" {
		t.Fatalf("unexpected unversioned packages: %#v", unversioned)
	}
}

func TestCoreEnvironmentUsesRedis(t *testing.T) {
	cases := map[string]bool{
		"CACHE_STORE=database\nQUEUE_CONNECTION=database\nSESSION_DRIVER=database\n": false,
		"CACHE_STORE=redis\n":                       true,
		"QUEUE_CONNECTION=\"redis\"\n":              true,
		"SESSION_DRIVER=redis\n":                    true,
		"CACHE_DRIVER=redis\n":                      true,
		"PULSE_INGEST_DRIVER=redis\n":               true,
		"REDIS_CLIENT=phpredis\n":                   true,
		"REDIS_CLIENT=predis\n":                     false,
		"REDIS_HOST=127.0.0.1\n":                    false,
		"# CACHE_STORE=redis\nAPP_ENV=production\n": false,
	}
	for contents, want := range cases {
		path := filepath.Join(t.TempDir(), ".env")
		writeTestFile(t, path, contents)
		got, err := coreEnvironmentUsesRedis(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("coreEnvironmentUsesRedis(%q) = %v, want %v", contents, got, want)
		}
	}
}

func TestLegacyAdoptionStartsDiscordSeparately(t *testing.T) {
	got := legacyAdoptionComponentsWithout([]Component{ComponentCore, ComponentSubs, ComponentDiscord}, ComponentDiscord)
	if !reflect.DeepEqual(got, []Component{ComponentCore, ComponentSubs}) {
		t.Fatalf("unexpected components: %#v", got)
	}
}

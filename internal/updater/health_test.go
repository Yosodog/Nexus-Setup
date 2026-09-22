package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateComponentHealthRequiresCurrentHealthyHeartbeat(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	now := time.Now().UTC()
	path := writeTestHealthDocument(t, paths, ComponentSubs, "v1.2.3", now)

	if err := validateComponentHealth(paths, ComponentSubs, "v1.2.3", now); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.ReplaceAll(string(contents), "v1.2.3", "v1.2.2"))
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateComponentHealth(paths, ComponentSubs, "v1.2.3", now); err == nil {
		t.Fatal("expected mismatched release identity to fail")
	}
}

func TestValidateComponentHealthRejectsStaleOrReadableByOtherUsers(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	now := time.Now().UTC()
	path := writeTestHealthDocument(t, paths, ComponentDiscord, "v2.0.0", now.Add(-time.Minute))

	if err := validateComponentHealth(paths, ComponentDiscord, "v2.0.0", now); err == nil {
		t.Fatal("expected stale heartbeat to fail")
	}
	writeTestHealthDocument(t, paths, ComponentDiscord, "v2.0.0", now)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := validateComponentHealth(paths, ComponentDiscord, "v2.0.0", now); err == nil {
		t.Fatal("expected unsafe health document permissions to fail")
	}
}

func writeTestHealthDocument(t *testing.T, paths Paths, component Component, release string, heartbeat time.Time) string {
	t.Helper()
	directory := filepath.Join(paths.DataDir, string(component))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "process-health.json")
	document := `{"schema_version":1,"contract_version":1,"component_id":"` + string(component) + `","runtime_state":"ready","health_state":"healthy","reachable":true,"installed":true,"enabled":true,"configuration_ready":true,"release_id":"` + release + `","heartbeat_at":"` + heartbeat.Format(time.RFC3339Nano) + `","stale_after_ms":45000,"build":{"release":"` + release + `"}}`
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

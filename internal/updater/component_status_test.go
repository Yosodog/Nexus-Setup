package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComponentConfigurationReadyRequiresEveryPrivateCredential(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	component := ComponentDiscord
	files := []string{
		filepath.Join(paths.NexusConfigDir, string(component), "environment"),
		filepath.Join(paths.CredentialDir, string(component), "bot-token"),
		filepath.Join(paths.CredentialDir, string(component), "nexus-api-token"),
		filepath.Join(paths.CredentialDir, string(component), "relay-private-key"),
	}
	for _, path := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("configured\n"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if !componentConfigurationReady(paths, component) {
		t.Fatal("complete private configuration was not recognized")
	}
	if err := os.Chmod(files[1], 0644); err != nil {
		t.Fatal(err)
	}
	if componentConfigurationReady(paths, component) {
		t.Fatal("world-readable credential was accepted")
	}
}

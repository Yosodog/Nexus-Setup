package updater

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationConfigurationIsWriteOnlyAndConsumed(t *testing.T) {
	paths := NewTestPaths(t.TempDir())
	store, err := NewOperationStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(OperationInstallComponent)
	request.Component = ComponentDiscord
	request.Configuration = map[string]string{
		"bot_token": "canary-secret-token",
		"client_id": "12345678901234567",
		"guild_id":  "98765432109876543",
	}
	state, _, err := store.CreateOrGet(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasConfiguration {
		t.Fatal("operation did not record the presence of write-only configuration")
	}
	stateData, err := os.ReadFile(filepath.Join(paths.OperationsDir, state.ID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateData), "canary-secret-token") {
		t.Fatal("secret was persisted in operation state")
	}
	if err := writeOperationConfiguration(paths, state.ID, request.Configuration); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(paths.SecretsDir, state.ID)
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("secret handoff permissions are %o, want 600", info.Mode().Perm())
	}
	consumed, err := consumeOperationConfiguration(paths, state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if consumed["bot_token"] != request.Configuration["bot_token"] {
		t.Fatal("secret handoff was not consumed intact")
	}
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatal("secret handoff still exists after consumption")
	}
}

func TestComponentConfigurationRejectsUnknownFields(t *testing.T) {
	request := testRequest(OperationInstallComponent)
	request.Component = ComponentDiscord
	request.Configuration = map[string]string{"command": "/bin/sh"}
	if err := request.Validate(); err == nil {
		t.Fatal("unknown component configuration field was accepted")
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "environment_variables") {
		t.Fatal("unexpected generic environment field appeared in the protocol")
	}
}

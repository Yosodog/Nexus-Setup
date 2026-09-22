package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDotEnvFileRoundTripsSpecialCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	wanted := `space # quote " slash \\ dollars ${DATABASE_PASSWORD}`

	if err := writeDotEnvFile(path, map[string]string{"DB_PASSWORD": wanted}); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `\${DATABASE_PASSWORD}`) {
		t.Fatal("dotenv variable reference was not escaped")
	}

	actual, err := readEnvValue(path, "DB_PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	if actual != wanted {
		t.Fatalf("expected %q, got %q", wanted, actual)
	}
}

func TestEnvironmentFileRejectsLineInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environment")
	if err := writeEnvironmentFile(path, map[string]string{"SAFE": "value\nINJECTED=yes"}); err == nil {
		t.Fatal("expected line injection to be rejected")
	}
}

func TestEnabledComponentsExcludesDisabledComponents(t *testing.T) {
	state := InstallationState{Components: map[Component]InstalledComponent{
		ComponentCore:    {Installed: true, Enabled: true},
		ComponentSubs:    {Installed: true, Enabled: false},
		ComponentDiscord: {Installed: false, Enabled: false},
	}}

	components := enabledComponents(state)
	if len(components) != 1 || components[0] != ComponentCore {
		t.Fatalf("unexpected enabled components: %#v", components)
	}
}

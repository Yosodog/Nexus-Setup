package updater

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Paths contains the fixed filesystem locations used by the updater. Production
// callers must use DefaultPaths. Tests may construct an isolated tree with
// NewTestPaths; no production path is read from an environment variable or a
// protocol request.
type Paths struct {
	StateDir       string
	OperationsDir  string
	LocksDir       string
	SocketPath     string
	RuntimeDir     string
	SecretsDir     string
	ReleaseDir     string
	ConfigDir      string
	NexusConfigDir string
	CredentialDir  string
	DataDir        string
	DownloadsDir   string
	StagingDir     string
	UpdaterDir     string
	InstallationDB string
}

// DefaultPaths returns the paths used by a managed Linux installation.
func DefaultPaths() Paths {
	return Paths{
		StateDir:       "/var/lib/nexus-updater",
		OperationsDir:  "/var/lib/nexus-updater/operations",
		LocksDir:       "/run/lock/nexus-updater",
		SocketPath:     "/run/nexus-updater/control.sock",
		RuntimeDir:     "/run/nexus-updater",
		SecretsDir:     "/run/nexus-updater/secrets",
		ReleaseDir:     "/opt/nexus",
		ConfigDir:      "/etc/nexus-updater",
		NexusConfigDir: "/etc/nexus",
		CredentialDir:  "/etc/nexus/credentials",
		DataDir:        "/var/lib/nexus",
		DownloadsDir:   "/var/lib/nexus-updater/downloads",
		StagingDir:     "/var/lib/nexus-updater/staging",
		UpdaterDir:     "/usr/local/libexec/nexus-updater",
		InstallationDB: "/var/lib/nexus-updater/installation.json",
	}
}

// NewTestPaths returns an isolated path set. It intentionally does not try to
// emulate a real host's ownership or systemd layout; integration tests can
// assert the path contract without writing to /var or /etc.
func NewTestPaths(root string) Paths {
	root = filepath.Clean(root)
	return Paths{
		StateDir:       filepath.Join(root, "var", "lib", "nexus-updater"),
		OperationsDir:  filepath.Join(root, "var", "lib", "nexus-updater", "operations"),
		LocksDir:       filepath.Join(root, "run", "lock", "nexus-updater"),
		SocketPath:     filepath.Join(root, "run", "nexus-updater", "control.sock"),
		RuntimeDir:     filepath.Join(root, "run", "nexus-updater"),
		SecretsDir:     filepath.Join(root, "run", "nexus-updater", "secrets"),
		ReleaseDir:     filepath.Join(root, "opt", "nexus"),
		ConfigDir:      filepath.Join(root, "etc", "nexus-updater"),
		NexusConfigDir: filepath.Join(root, "etc", "nexus"),
		CredentialDir:  filepath.Join(root, "etc", "nexus", "credentials"),
		DataDir:        filepath.Join(root, "var", "lib", "nexus"),
		DownloadsDir:   filepath.Join(root, "var", "lib", "nexus-updater", "downloads"),
		StagingDir:     filepath.Join(root, "var", "lib", "nexus-updater", "staging"),
		UpdaterDir:     filepath.Join(root, "usr", "local", "libexec", "nexus-updater"),
		InstallationDB: filepath.Join(root, "var", "lib", "nexus-updater", "installation.json"),
	}
}

// Validate rejects relative or ambiguous paths. The updater never accepts a
// path through the wire protocol; this check protects local configuration and
// makes accidental writes to the working directory impossible.
func (p Paths) Validate() error {
	values := map[string]string{
		"state directory":          p.StateDir,
		"operations directory":     p.OperationsDir,
		"locks directory":          p.LocksDir,
		"socket path":              p.SocketPath,
		"runtime directory":        p.RuntimeDir,
		"secrets directory":        p.SecretsDir,
		"release directory":        p.ReleaseDir,
		"configuration directory":  p.ConfigDir,
		"Nexus configuration":      p.NexusConfigDir,
		"credential directory":     p.CredentialDir,
		"component data directory": p.DataDir,
		"downloads directory":      p.DownloadsDir,
		"staging directory":        p.StagingDir,
		"updater directory":        p.UpdaterDir,
		"installation state":       p.InstallationDB,
	}
	for name, value := range values {
		if value == "" || !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s contains a NUL byte", name)
		}
	}

	return nil
}

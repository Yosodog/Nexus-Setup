package updater

import (
	"bytes"
	"testing"
)

func TestRewriteLegacyNginxPreservesExistingTLSAndChangesManagedPaths(t *testing.T) {
	original := []byte(`server {
    listen 443 ssl;
    root /var/www/nexus/public;
    ssl_certificate /etc/letsencrypt/live/nexus.example/fullchain.pem;
    fastcgi_pass unix:/run/php/php8.3-fpm.sock;
}`)

	updated, err := rewriteLegacyNginx(original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(updated, []byte(legacyCorePath)) {
		t.Fatal("legacy Core path was not replaced")
	}
	if !bytes.Contains(updated, []byte("root /opt/nexus/nexus-core/current/public;")) {
		t.Fatal("managed Core path is missing")
	}
	if !bytes.Contains(updated, []byte("unix:/run/php/nexus-core.sock")) {
		t.Fatal("dedicated PHP-FPM socket is missing")
	}
	if !bytes.Contains(updated, []byte("/etc/letsencrypt/live/nexus.example/fullchain.pem")) {
		t.Fatal("existing TLS configuration was not preserved")
	}
}

func TestRewriteLegacyNginxRejectsUnknownLayouts(t *testing.T) {
	if _, err := rewriteLegacyNginx([]byte("root /srv/custom/public;")); err == nil {
		t.Fatal("expected an unknown Nginx layout to be rejected")
	}
}

func TestOfficialLegacyOriginAcceptsOnlyYosodogRepository(t *testing.T) {
	accepted := []string{
		"https://github.com/Yosodog/Nexus-AMS.git",
		"git@github.com:Yosodog/Nexus-AMS.git",
		"ssh://git@github.com/Yosodog/Nexus-AMS.git",
	}
	for _, origin := range accepted {
		if !officialLegacyOrigin(origin, coreRepository) {
			t.Fatalf("expected origin %q to be accepted", origin)
		}
	}
	for _, origin := range []string{"https://github.com/other/Nexus-AMS.git", "file:///tmp/Nexus-AMS", "https://github.com/Yosodog/Nexus-AMS-fork.git"} {
		if officialLegacyOrigin(origin, coreRepository) {
			t.Fatalf("expected origin %q to be rejected", origin)
		}
	}
}

func TestLocalDatabaseHostRecognitionIsNarrow(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLocalDatabaseHost(host) {
			t.Fatalf("expected %q to be local", host)
		}
	}
	if isLocalDatabaseHost("db.internal") {
		t.Fatal("remote database host was treated as local")
	}
}

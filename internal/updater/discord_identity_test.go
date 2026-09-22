package updater

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"testing"
)

func TestNewDiscordRelayIdentityCreatesMatchingEd25519Material(t *testing.T) {
	identity, err := newDiscordRelayIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !uuidPattern.MatchString(identity.ConnectionID) {
		t.Fatalf("invalid connection UUID: %q", identity.ConnectionID)
	}
	if identity.KeyID != "relay-current" {
		t.Fatalf("unexpected key id: %q", identity.KeyID)
	}
	privateDER, err := base64.StdEncoding.DecodeString(identity.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(privateDER)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("unexpected private key type %T", parsed)
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(identity.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !privateKey.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(publicKey)) {
		t.Fatal("public key does not match private key")
	}
}

func TestValidateInstallConfigurationRequiresDiscordSnowflakes(t *testing.T) {
	valid := map[string]string{
		"bot_token": "write-only-token",
		"client_id": "12345678901234567",
		"guild_id":  "223456789012345678",
	}
	if err := validateInstallConfiguration(ComponentDiscord, valid); err != nil {
		t.Fatal(err)
	}
	invalid := map[string]string{
		"bot_token": "write-only-token",
		"client_id": "123",
		"guild_id":  "223456789012345678",
	}
	if err := validateInstallConfiguration(ComponentDiscord, invalid); err == nil {
		t.Fatal("expected a short Discord application ID to fail")
	}
}

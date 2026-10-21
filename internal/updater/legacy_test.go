package updater

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestLegacySupervisorDiscoveryFindsEveryCheckoutProgram(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, filepath.Join(directory, "nexus-worker.conf"), `[program:nexus-worker]
process_name=%(program_name)s_%(process_num)02d
command=php artisan queue:work --sleep=3
directory=/var/www/nexus
numprocs=2
`)
	writeTestFile(t, filepath.Join(directory, "raid.conf"), `; renamed by hand
[program:raid-intelligence]
command=/usr/bin/php /var/www/nexus/artisan queue:work --queue=raid-intelligence ; old worker
`)
	writeTestFile(t, filepath.Join(directory, "subs.conf"), `[group:subs]
programs=subs-node,subs-stream

[program:subs-node]
command=node index.js
directory=/var/nexus-subs/

[program:subs-stream]
command=php /var/www/nexus/artisan subs:consume-stream
`)
	writeTestFile(t, filepath.Join(directory, "other.conf"), `[program:other]
command=php artisan queue:work
directory=/var/www/nexus-other
`)
	writeTestFile(t, filepath.Join(directory, "pulse.conf.nexus-disabled"), `[program:pulse]
directory=/var/www/nexus
`)
	configs, err := findLegacySupervisorConfigs(directory)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	for _, config := range configs {
		found[filepath.Base(config.Path)] = config.Targets
	}
	expected := map[string][]string{
		"nexus-worker.conf": {"nexus-worker:*"},
		"raid.conf":         {"raid-intelligence:*"},
		"subs.conf":         {"subs:*"},
	}
	if !reflect.DeepEqual(found, expected) {
		t.Fatalf("unexpected Supervisor programs: %#v", found)
	}
}

func TestLegacySupervisorDiscoveryToleratesMissingDirectory(t *testing.T) {
	configs, err := findLegacySupervisorConfigs(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(configs) != 0 {
		t.Fatalf("expected no programs, got %#v %v", configs, err)
	}
}

func TestLegacySupervisorRejectsUnsafeProgramNames(t *testing.T) {
	_, matched, err := parseLegacySupervisorConfig([]byte("[program:bad name]\ndirectory=/var/www/nexus\n"))
	if !matched || err == nil {
		t.Fatalf("expected an unsupported name to block adoption, got matched=%v err=%v", matched, err)
	}
}

func TestLegacySubsRedisStreamIsPreserved(t *testing.T) {
	directory := t.TempDir()
	environment := filepath.Join(directory, ".env")
	writeTestFile(t, environment, "DELIVERY_DRIVER=http\n")
	if stream, err := legacySubsRedisStream(environment, directory); err != nil || stream != nil {
		t.Fatalf("HTTP delivery was not recognized: %#v %v", stream, err)
	}
	if stream, err := legacySubsRedisStream(filepath.Join(directory, "absent.env"), directory); err != nil || stream != nil {
		t.Fatalf("a Subs checkout without .env should deliver over HTTP: %#v %v", stream, err)
	}

	secret := strings.Repeat("s", 40)
	writeTestFile(t, environment, "DELIVERY_DRIVER=redis-stream\nSUBS_REDIS_URL=redis://127.0.0.1:6379/3\nSUBS_REDIS_HMAC_SECRET=\""+secret+"\"\n")
	stream, err := legacySubsRedisStream(environment, directory)
	if err != nil {
		t.Fatal(err)
	}
	if stream == nil || stream.URL != "redis://127.0.0.1:6379/3" || stream.Stream != defaultSubsRedisStream || stream.HMACSecret != secret {
		t.Fatalf("unexpected Redis Stream delivery: %#v", stream)
	}

	writeTestFile(t, filepath.Join(directory, "secrets", "hmac"), secret+"\n")
	writeTestFile(t, environment, "DELIVERY_DRIVER=Redis-Stream\nSUBS_REDIS_URL=redis://redis:6379/3\nSUBS_REDIS_STREAM=custom\nSUBS_REDIS_HMAC_SECRET_FILE=secrets/hmac\n")
	stream, err = legacySubsRedisStream(environment, directory)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Stream != "custom" || stream.HMACSecret != secret {
		t.Fatalf("secret file or stream name was not read: %#v", stream)
	}

	for _, contents := range []string{
		"DELIVERY_DRIVER=redis-stream\nSUBS_REDIS_HMAC_SECRET=" + secret + "\n",
		"DELIVERY_DRIVER=redis-stream\nSUBS_REDIS_URL=redis://127.0.0.1\nSUBS_REDIS_HMAC_SECRET=short\n",
		"DELIVERY_DRIVER=redis-stream\nSUBS_REDIS_URL=http://127.0.0.1\nSUBS_REDIS_HMAC_SECRET=" + secret + "\n",
		"DELIVERY_DRIVER=kafka\n",
	} {
		writeTestFile(t, environment, contents)
		if _, err := legacySubsRedisStream(environment, directory); err == nil {
			t.Fatalf("expected an invalid delivery to be rejected:\n%s", contents)
		}
	}
}

func TestCoreSubsStreamMustMatchSubs(t *testing.T) {
	directory := t.TempDir()
	environment := filepath.Join(directory, ".env")
	secret := strings.Repeat("s", 40)
	stream := subsRedisStream{URL: "redis://127.0.0.1:6379/3", Stream: defaultSubsRedisStream, HMACSecret: secret}
	writeTestFile(t, environment, "SUBS_REDIS_HMAC_SECRET="+secret+"\n")
	if err := checkCoreSubsStream(environment, stream); err != nil {
		t.Fatalf("matching Core settings were rejected: %v", err)
	}
	for _, contents := range []string{
		"APP_NAME=Nexus\n",
		"SUBS_REDIS_HMAC_SECRET=" + strings.Repeat("t", 40) + "\n",
		"SUBS_REDIS_HMAC_SECRET=" + secret + "\nSUBS_REDIS_STREAM=other\n",
	} {
		writeTestFile(t, environment, contents)
		if err := checkCoreSubsStream(environment, stream); err == nil {
			t.Fatalf("expected mismatched Core settings to be rejected:\n%s", contents)
		}
	}
}

func TestLegacyDiscordConfigurationKeepsRelayIdentity(t *testing.T) {
	directory := t.TempDir()
	environment := filepath.Join(directory, ".env")
	writeTestFile(t, filepath.Join(directory, "relay.key"), "relay-private\n")
	writeTestFile(t, environment, `NODE_ENV=production
DISCORD_BOT_TOKEN=bot-token-value
DISCORD_CLIENT_ID=123456789012345678
DISCORD_GUILD_ID=876543210987654321
DISCORD_GUILD_MEMBERS_INTENT=true
DISCORD_MESSAGE_CONTENT_INTENT=false
DISCORD_CONNECTIONS_FILE=data/connections.json
DISCORD_CONNECTIONS_JSON='[]'
DISCORD_CONNECTION_REFRESH_MS=30000
DISCORD_SCHEDULER_QUANTUM=1
NEXUS_API_URL=https://nexus.example
NEXUS_API_KEY="api-key-value"
NEXUS_DISCORD_RELAY_PRIVATE_KEY_FILE=relay.key
NEXUS_DISCORD_CONNECTION_ID=4b0e4f2c-8d0f-4c55-9c3a-7f8d2b1e6a90
NEXUS_DISCORD_CONNECTION_GENERATION=3
NEXUS_DISCORD_RELAY_PROTOCOL=2
NEXUS_DISCORD_RELAY_KEY_ID=relay-2025
NEXUS_DISCORD_RELAY_CURRENT_KEY_ID=relay-2025
NEXUS_DISCORD_CAPABILITIES_JSON={"applications":true}
PROCESS_HEALTH_FILE=data/process-health.json
`)
	configuration, err := readLegacyDiscordConfiguration(environment, directory)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"BOT_DEPLOYMENT_MODE":                 "dedicated",
		"DISCORD_CLIENT_ID":                   "123456789012345678",
		"DISCORD_GUILD_ID":                    "876543210987654321",
		"DISCORD_MESSAGE_CONTENT_INTENT":      "false",
		"DISCORD_CONNECTIONS_FILE":            filepath.Join(directory, "data", "connections.json"),
		"DISCORD_CONNECTIONS_JSON":            "[]",
		"NEXUS_API_URL":                       "https://nexus.example",
		"NEXUS_DISCORD_CONNECTION_ID":         "4b0e4f2c-8d0f-4c55-9c3a-7f8d2b1e6a90",
		"NEXUS_DISCORD_CONNECTION_GENERATION": "3",
		"NEXUS_DISCORD_RELAY_KEY_ID":          "relay-2025",
		"NEXUS_DISCORD_RELAY_CURRENT_KEY_ID":  "relay-2025",
		"NEXUS_DISCORD_CAPABILITIES_JSON":     `{"applications":true}`,
	} {
		if configuration.environment[key] != value {
			t.Fatalf("%s = %q, expected %q", key, configuration.environment[key], value)
		}
	}
	for _, key := range []string{"DISCORD_BOT_TOKEN", "NEXUS_API_KEY", "NEXUS_DISCORD_RELAY_PRIVATE_KEY", "NODE_ENV", "PROCESS_HEALTH_FILE"} {
		if _, found := configuration.environment[key]; found {
			t.Fatalf("%s must not be copied into the managed environment", key)
		}
	}
	credentials := map[string]string{}
	for _, credential := range configuration.credentials {
		credentials[credential.key+"="+credential.name] = credential.value
	}
	expected := map[string]string{
		"DISCORD_BOT_TOKEN=bot-token":                       "bot-token-value",
		"NEXUS_API_KEY=nexus-api-token":                     "api-key-value",
		"NEXUS_DISCORD_RELAY_PRIVATE_KEY=relay-private-key": "relay-private",
	}
	if !reflect.DeepEqual(credentials, expected) {
		t.Fatalf("unexpected credentials: %#v", credentials)
	}
}

func TestLegacyDiscordConfigurationRequiresBotIdentity(t *testing.T) {
	directory := t.TempDir()
	environment := filepath.Join(directory, ".env")
	complete := map[string]string{
		"DISCORD_BOT_TOKEN":               "token",
		"DISCORD_CLIENT_ID":               "123456789012345678",
		"DISCORD_GUILD_ID":                "876543210987654321",
		"NEXUS_API_URL":                   "https://nexus.example",
		"NEXUS_API_KEY":                   "key",
		"NEXUS_DISCORD_RELAY_PRIVATE_KEY": "relay",
	}
	for omitted := range complete {
		var builder strings.Builder
		for key, value := range complete {
			if key != omitted {
				builder.WriteString(key + "=" + value + "\n")
			}
		}
		writeTestFile(t, environment, builder.String())
		if _, err := readLegacyDiscordConfiguration(environment, directory); err == nil {
			t.Fatalf("configuration without %s was accepted", omitted)
		}
	}
	writeTestFile(t, environment, "DISCORD_BOT_TOKEN=token\nDISCORD_CLIENT_ID=12\nDISCORD_GUILD_ID=876543210987654321\nNEXUS_API_URL=https://nexus.example\nNEXUS_API_KEY=key\nNEXUS_DISCORD_RELAY_PRIVATE_KEY=relay\n")
	if _, err := readLegacyDiscordConfiguration(environment, directory); err == nil {
		t.Fatal("an invalid client ID was accepted")
	}
}

func TestLegacyDiscordUnitRecognition(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nexus-discord.service")
	if legacyDiscordUnitInstalled(path) {
		t.Fatal("a missing unit was treated as legacy")
	}
	writeTestFile(t, path, "[Service]\nWorkingDirectory=/opt/nexus-ams-discord\nExecStart=/usr/bin/node src/bot.js\n")
	if !legacyDiscordUnitInstalled(path) {
		t.Fatal("the legacy unit was not recognized")
	}
	writeTestFile(t, path, systemdUnits()[discordUnitPath])
	if legacyDiscordUnitInstalled(path) {
		t.Fatal("the managed unit was treated as legacy")
	}
	link := filepath.Join(directory, "linked.service")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if !legacyDiscordUnitInstalled(link) {
		t.Fatal("a linked unit was not treated as legacy")
	}
}

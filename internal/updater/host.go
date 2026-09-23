package updater

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Platform struct {
	ID       string
	Version  string
	Codename string
	Arch     string
}

type InstallOptions struct {
	Profile               Profile
	Domain                string
	AdminEmail            string
	AdminPassword         string
	AdminNationID         string
	AllianceID            string
	PWAPIKey              string
	PWMutationKey         string
	DatabaseHost          string
	DatabaseName          string
	DatabaseUser          string
	DatabasePassword      string
	DatabaseListenAddress string
	ApplicationHost       string
	CoreURL               string
	CoreToken             string
}

var domainPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
var emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
var digitsPattern = regexp.MustCompile(`^[0-9]+$`)

func supportedOperatingSystem(operatingSystem, version string) bool {
	return (operatingSystem == "ubuntu" && (version == "22.04" || version == "24.04" || version == "26.04")) ||
		(operatingSystem == "debian" && (version == "12" || version == "13"))
}

func DetectPlatform() (Platform, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return Platform{}, err
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if found {
			values[key] = strings.Trim(value, "\"'")
		}
	}
	platform := Platform{ID: values["ID"], Version: values["VERSION_ID"], Codename: values["VERSION_CODENAME"], Arch: runtime.GOARCH}
	if platform.Arch != "amd64" || !supportedOperatingSystem(platform.ID, platform.Version) {
		return Platform{}, fmt.Errorf("unsupported platform %s %s/%s", platform.ID, platform.Version, platform.Arch)
	}
	return platform, nil
}

func ConfigureHost(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("configure-host must run as root")
	}
	platform, err := DetectPlatform()
	if err != nil {
		return err
	}
	for _, group := range []string{"nexus-updater-control", "nexus-core", "nexus-subs", "nexus-discord"} {
		if err := ensureGroup(ctx, group); err != nil {
			return err
		}
	}
	for _, account := range []string{"nexus-core", "nexus-subs", "nexus-discord"} {
		if err := ensureServiceUser(ctx, account); err != nil {
			return err
		}
	}
	if err := runFixedCommand(ctx, "/usr/sbin/usermod", []string{"--append", "--groups", "nexus-updater-control", "nexus-core"}); err != nil {
		return err
	}
	coreUser, err := user.Lookup("nexus-core")
	if err != nil {
		return err
	}
	coreUID, err := strconv.ParseUint(coreUser.Uid, 10, 32)
	if err != nil {
		return err
	}
	controlGroup, err := user.LookupGroup("nexus-updater-control")
	if err != nil {
		return err
	}
	controlGID, err := strconv.Atoi(controlGroup.Gid)
	if err != nil {
		return err
	}
	paths := DefaultPaths()
	currentProfile := ProfileFull
	allowedUIDs := []uint32{0, uint32(coreUID)}
	allowedGIDs := []uint32{}
	existingConfigPath := filepath.Join(paths.ConfigDir, "config.json")
	if _, statErr := os.Lstat(existingConfigPath); statErr == nil {
		existing, loadErr := LoadConfig(existingConfigPath)
		if loadErr != nil {
			return fmt.Errorf("existing updater configuration is invalid: %w", loadErr)
		}
		currentProfile = existing.CurrentProfile
		allowedUIDs = append(allowedUIDs, existing.AllowedUIDs...)
		allowedGIDs = append(allowedGIDs, existing.AllowedGIDs...)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if sudoUID := strings.TrimSpace(os.Getenv("SUDO_UID")); sudoUID != "" && sudoUID != "0" {
		parsedUID, parseErr := strconv.ParseUint(sudoUID, 10, 32)
		if parseErr != nil {
			return errors.New("sudo caller identity is invalid")
		}
		operator, lookupErr := user.LookupId(strconv.FormatUint(parsedUID, 10))
		if lookupErr != nil || operator.Username == "" || strings.ContainsAny(operator.Username, ":\r\n\x00") {
			return errors.New("sudo caller account is unavailable")
		}
		if err := runFixedCommand(ctx, "/usr/sbin/usermod", []string{"--append", "--groups", "nexus-updater-control", operator.Username}); err != nil {
			return err
		}
		allowedUIDs = append(allowedUIDs, uint32(parsedUID))
	}
	allowedUIDs = uniqueUint32(allowedUIDs)
	allowedGIDs = uniqueUint32(allowedGIDs)
	for _, directory := range []string{paths.StateDir, paths.OperationsDir, paths.DownloadsDir, paths.StagingDir, paths.LocksDir, paths.SecretsDir, paths.ConfigDir, paths.CredentialDir} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(paths.RuntimeDir, 0750); err != nil {
		return err
	}
	if err := os.Chown(paths.RuntimeDir, 0, controlGID); err != nil {
		return err
	}
	if err := os.Chmod(paths.RuntimeDir, 0750); err != nil {
		return err
	}
	for _, directory := range []string{paths.NexusConfigDir, paths.ReleaseDir, paths.DataDir, paths.UpdaterDir} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			return err
		}
	}
	for _, account := range []string{"nexus-core", "nexus-subs", "nexus-discord"} {
		dataDirectory := filepath.Join(paths.DataDir, account)
		if err := os.MkdirAll(dataDirectory, 0770); err != nil {
			return err
		}
		if err := chownPathToGroup(dataDirectory, account, 0770); err != nil {
			return err
		}
		configDirectory := filepath.Join(paths.NexusConfigDir, account)
		if err := os.MkdirAll(configDirectory, 0750); err != nil {
			return err
		}
		if err := chownPathToGroup(configDirectory, account, 0750); err != nil {
			return err
		}
	}
	config := diskConfig{SocketGroupGID: controlGID, AllowedUIDs: allowedUIDs, AllowedGIDs: allowedGIDs, CurrentProfile: currentProfile,
		CurrentOS: platform.ID, CurrentOSVersion: platform.Version, CurrentArch: platform.Arch}
	if err := atomicWriteJSON(filepath.Join(paths.ConfigDir, "config.json"), config, 0600); err != nil {
		return err
	}
	if err := installCurrentBinary(paths); err != nil {
		return err
	}
	for path, content := range systemdUnits() {
		if err := atomicWriteBytes(path, []byte(content), 0644); err != nil {
			return err
		}
	}
	if err := runFixedCommand(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}); err != nil {
		return err
	}
	return runFixedCommand(ctx, "/usr/bin/systemctl", []string{"enable", "--now", "nexus-updater.socket"})
}

func uniqueUint32(values []uint32) []uint32 {
	seen := make(map[uint32]struct{}, len(values))
	result := make([]uint32, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func ensureGroup(ctx context.Context, name string) error {
	if _, err := user.LookupGroup(name); err == nil {
		return nil
	}
	return runFixedCommand(ctx, "/usr/sbin/groupadd", []string{"--system", name})
}

func ensureServiceUser(ctx context.Context, name string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	return runFixedCommand(ctx, "/usr/sbin/useradd", []string{"--system", "--gid", name, "--home-dir", "/var/lib/nexus/" + name, "--shell", "/usr/sbin/nologin", name})
}

func installCurrentBinary(paths Paths) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	release := BuildVersion
	if _, err := ParseVersion(release); err != nil {
		release = "bootstrap"
	}
	directory := filepath.Join(paths.UpdaterDir, "releases", release)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	target := filepath.Join(directory, "nexus")
	if err := copyExecutable(executable, target); err != nil {
		return err
	}
	current := filepath.Join(paths.UpdaterDir, "current")
	if err := replaceSymlink(directory, current); err != nil {
		return err
	}
	return replaceSymlink(filepath.Join(current, "nexus"), "/usr/local/bin/nexus")
}

func copyExecutable(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := target + ".new"
	_ = os.Remove(temporary)
	output, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

func replaceSymlink(target, link string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		return err
	}
	temporary := link + ".next"
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, link); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func InstallFresh(ctx context.Context, options InstallOptions) error {
	if err := validateInstallOptions(options); err != nil {
		return err
	}
	config, err := LoadInstalledConfig()
	if err != nil {
		return err
	}
	locks, err := NewLockSet(config.Paths)
	if err != nil {
		return err
	}
	globalLock, err := locks.AcquireGlobal(ctx)
	if err != nil {
		return errors.New("another Nexus host operation is already running")
	}
	defer globalLock.Close()
	if _, err := LoadInstallation(config.Paths); err == nil {
		return errors.New("Nexus is already installed; use nexus update")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("existing installation state is invalid; run nexus doctor")
	}
	requiredSpace := minimumReleaseSpace
	if options.Profile == ProfileDatabaseOnly {
		requiredSpace = minimumComponentSpace
	}
	if err := ensureFreeSpace(config.Paths.StateDir, requiredSpace); err != nil {
		return err
	}
	platform, err := DetectPlatform()
	if err != nil {
		return err
	}
	if err := provisionPackages(ctx, platform, options.Profile); err != nil {
		return err
	}
	releases := NewGitHubReleaseClient()
	available, err := releases.StableReleases(ctx)
	if err != nil || len(available) == 0 {
		return errors.New("no stable Nexus GitHub release is available")
	}
	release := BuildVersion
	if _, err := ParseVersion(release); err != nil {
		release = available[len(available)-1].Tag
	}
	engine := DeploymentEngine{Config: config, Releases: releases, Executor: OSCommandExecutor{}}
	components := componentsForProfile(options.Profile)
	activatedUnits := make([]string, 0, len(components)+1)
	coreSiteConfigured := false
	installCompleted := false
	defer func() {
		if installCompleted {
			return
		}
		for _, unit := range activatedUnits {
			_ = runFixedCommand(context.Background(), config.SystemctlBinary, []string{"disable", "--now", unit})
		}
		for _, component := range components {
			link, linkErr := currentLink(config.Paths, component)
			if linkErr != nil {
				continue
			}
			target, readErr := os.Readlink(link)
			releaseDirectory, releaseErr := componentReleaseDirectory(config.Paths, component, release)
			if readErr == nil && releaseErr == nil && filepath.Clean(target) == filepath.Clean(releaseDirectory) {
				_ = os.Remove(link)
			}
		}
		if coreSiteConfigured {
			_ = os.Remove("/etc/nginx/sites-enabled/nexus.conf")
			if runFixedCommand(context.Background(), "/usr/sbin/nginx", []string{"-t"}) == nil {
				_ = runFixedCommand(context.Background(), config.SystemctlBinary, []string{"reload", "nginx.service"})
			}
		}
		_ = updateInstalledProfile(config.Paths, config.CurrentProfile)
	}()
	for _, component := range components {
		if err := engine.stageComponent(ctx, "install", component, release); err != nil {
			return err
		}
	}
	if options.Profile == ProfileDatabaseOnly {
		if err := configureRemoteDatabase(ctx, platform, options.DatabaseListenAddress); err != nil {
			return err
		}
	}
	_, err = prepareInstallConfiguration(config.Paths, options)
	if err != nil {
		return err
	}
	if containsComponent(components, ComponentCore) {
		if err := engine.prepareCoreRelease(release); err != nil {
			return err
		}
		if err := configurePHPFPM(ctx, config.Paths); err != nil {
			return err
		}
		if err := configureNginx(ctx, options.Domain); err != nil {
			return err
		}
		coreSiteConfigured = true
		if err := engine.runArtisan(ctx, release, "migrate", "--force"); err != nil {
			return err
		}
		if err := provisionAdministrator(ctx, engine, release, options); err != nil {
			return err
		}
	}
	if containsComponent(components, ComponentSubs) {
		if options.Profile == ProfileSubsOnly {
			if err := engine.writeStandaloneSubsConfiguration(release, options); err != nil {
				return err
			}
		} else if err := engine.writeComponentConfiguration(ComponentSubs, release, nil); err != nil {
			return err
		}
	}
	state := InstallationState{Version: 1, Profile: options.Profile, CurrentRelease: release, Components: map[Component]InstalledComponent{}}
	for _, component := range []Component{ComponentCore, ComponentSubs, ComponentDiscord} {
		installed := containsComponent(components, component)
		state.Components[component] = InstalledComponent{Installed: installed, Enabled: installed, CurrentRelease: conditionalRelease(installed, release)}
	}
	for _, component := range components {
		if err := atomicSwitchRelease(config.Paths, component, release); err != nil {
			return err
		}
	}
	for _, component := range components {
		unit, _ := componentServiceUnit(component)
		if err := runFixedCommand(ctx, config.SystemctlBinary, []string{"enable", "--now", unit}); err != nil {
			return err
		}
		activatedUnits = append(activatedUnits, unit)
	}
	if containsComponent(components, ComponentCore) {
		if err := runFixedCommand(ctx, config.SystemctlBinary, []string{"enable", "--now", "nexus-scheduler.timer"}); err != nil {
			return err
		}
		activatedUnits = append(activatedUnits, "nexus-scheduler.timer")
	}
	if err := engine.healthCheck(ctx, components, release); err != nil {
		return err
	}
	if containsComponent(components, ComponentCore) {
		if err := requestTLSCertificate(ctx, options.Domain, options.AdminEmail); err != nil {
			return err
		}
	}
	if err := updateInstalledProfile(config.Paths, options.Profile); err != nil {
		return err
	}
	if err := SaveInstallation(config.Paths, state); err != nil {
		return err
	}
	installCompleted = true
	fmt.Printf("Nexus %s installed successfully.\n", release)
	return nil
}

func validateInstallOptions(options InstallOptions) error {
	if !validProfile(options.Profile) {
		return errors.New("installation profile is invalid")
	}
	if options.Profile == ProfileDatabaseOnly {
		if !privateIPv4(options.DatabaseListenAddress) || !privateIPv4(options.ApplicationHost) || options.DatabaseListenAddress == options.ApplicationHost {
			return errors.New("database-only installation requires distinct private server and application IPv4 addresses")
		}
		return nil
	}
	if options.Profile == ProfileSubsOnly {
		if !strings.HasPrefix(options.CoreURL, "https://") || options.CoreToken == "" || options.PWAPIKey == "" {
			return errors.New("Subs-only installation requires an HTTPS Core URL and both credentials")
		}
		return nil
	}
	if !domainPattern.MatchString(options.Domain) || !strings.Contains(options.Domain, ".") {
		return errors.New("domain is invalid")
	}
	if !emailPattern.MatchString(options.AdminEmail) || len(options.AdminPassword) < 12 || !digitsPattern.MatchString(options.AdminNationID) || !digitsPattern.MatchString(options.AllianceID) || options.PWAPIKey == "" {
		return errors.New("administrator, alliance, or Politics & War configuration is invalid")
	}
	if options.PWMutationKey == "" {
		return errors.New("Politics & War mutation key is required")
	}
	if options.Profile == ProfileAppWebSubsRemoteDB || options.Profile == ProfileWebOnly {
		if options.DatabaseHost == "" || options.DatabaseName == "" || options.DatabaseUser == "" || options.DatabasePassword == "" {
			return errors.New("remote database configuration is incomplete")
		}
	}
	return nil
}

func provisionPackages(ctx context.Context, platform Platform, profile Profile) error {
	if err := runFixedCommand(ctx, "/usr/bin/apt-get", []string{"update"}); err != nil {
		return err
	}
	prerequisites := []string{"install", "-y", "--no-install-recommends", "ca-certificates", "curl", "gnupg"}
	if platform.ID == "ubuntu" && platform.Version == "22.04" {
		prerequisites = append(prerequisites, "software-properties-common")
	}
	if err := runFixedCommand(ctx, "/usr/bin/apt-get", prerequisites); err != nil {
		return err
	}
	if platform.ID == "ubuntu" && platform.Version == "22.04" {
		if err := runFixedCommand(ctx, "/usr/bin/add-apt-repository", []string{"-y", "ppa:ondrej/php"}); err != nil {
			return err
		}
	}
	if platform.ID == "debian" && platform.Version == "12" {
		if err := installSuryKeyring(ctx, platform.Codename); err != nil {
			return err
		}
	}
	if profile != ProfileDatabaseOnly {
		if err := installNodeSourceRepository(ctx); err != nil {
			return err
		}
	}
	if err := runFixedCommand(ctx, "/usr/bin/apt-get", []string{"update"}); err != nil {
		return err
	}
	packages := []string{}
	if profile == ProfileFull || profile == ProfileDatabaseOnly {
		databasePackage := "mysql-server"
		if platform.ID == "debian" {
			databasePackage = "default-mysql-server"
		}
		packages = append(packages, databasePackage)
	}
	if profile != ProfileDatabaseOnly {
		packages = append(packages, "nodejs")
	}
	if profile != ProfileDatabaseOnly && profile != ProfileSubsOnly {
		packages = append(packages, "nginx", "certbot", "python3-certbot-nginx", "php-fpm", "php-cli", "php-mysql", "php-mbstring", "php-xml", "php-curl", "php-zip", "php-bcmath", "php-gd", "php-intl")
	}
	arguments := append([]string{"install", "-y", "--no-install-recommends"}, packages...)
	if err := runFixedCommand(ctx, "/usr/bin/apt-get", arguments); err != nil {
		return err
	}
	return validateInstalledRuntimes(ctx, profile)
}

func installNodeSourceRepository(ctx context.Context) error {
	temporary, err := os.CreateTemp("/var/tmp", "nexus-nodesource-*.key")
	if err != nil {
		return err
	}
	path := temporary.Name()
	_ = temporary.Close()
	_ = os.Remove(path)
	defer os.Remove(path)
	if err := downloadFixedURL(ctx, "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key", path, 1024*1024); err != nil {
		return err
	}
	keyring := "/usr/share/keyrings/nodesource.gpg"
	if err := runFixedCommand(ctx, "/usr/bin/gpg", []string{"--batch", "--yes", "--dearmor", "--output", keyring, path}); err != nil {
		return err
	}
	if err := os.Chmod(keyring, 0644); err != nil {
		return err
	}
	source := "Types: deb\nURIs: https://deb.nodesource.com/node_22.x\nSuites: nodistro\nComponents: main\nArchitectures: amd64\nSigned-By: /usr/share/keyrings/nodesource.gpg\n"
	return atomicWriteBytes("/etc/apt/sources.list.d/nodesource.sources", []byte(source), 0644)
}

func validateInstalledRuntimes(ctx context.Context, profile Profile) error {
	if profile != ProfileDatabaseOnly {
		command := exec.CommandContext(ctx, "/usr/bin/node", "--version")
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		output, err := command.Output()
		if err != nil || !regexp.MustCompile(`^v(2[2-9]|[3-9][0-9])\.`).Match(bytes.TrimSpace(output)) {
			return errors.New("Node.js 22 or newer is required")
		}
	}
	if profile != ProfileDatabaseOnly && profile != ProfileSubsOnly {
		command := exec.CommandContext(ctx, "/usr/bin/php", "-r", "echo PHP_VERSION;")
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		output, err := command.Output()
		if err != nil || !regexp.MustCompile(`^(8\.[3-9]|9\.)`).Match(bytes.TrimSpace(output)) {
			return errors.New("PHP 8.3 or newer is required")
		}
	}
	return nil
}

func installSuryKeyring(ctx context.Context, codename string) error {
	if codename == "" {
		codename = "bookworm"
	}
	temporary, err := os.CreateTemp("/var/tmp", "nexus-sury-*.deb")
	if err != nil {
		return err
	}
	path := temporary.Name()
	_ = temporary.Close()
	_ = os.Remove(path)
	defer os.Remove(path)
	if err := downloadFixedURL(ctx, "https://packages.sury.org/debsuryorg-archive-keyring.deb", path, 10*1024*1024); err != nil {
		return err
	}
	if err := runFixedCommand(ctx, "/usr/bin/dpkg", []string{"-i", path}); err != nil {
		return err
	}
	line := "deb [signed-by=/usr/share/keyrings/debsuryorg-archive-keyring.gpg] https://packages.sury.org/php/ " + codename + " main\n"
	return atomicWriteBytes("/etc/apt/sources.list.d/nexus-php.list", []byte(line), 0644)
}

func downloadFixedURL(ctx context.Context, endpoint, destination string, maxBytes int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > maxBytes {
		return errors.New("fixed package download failed")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxBytes {
		return errors.New("fixed package download exceeded its limit")
	}
	return nil
}

func componentsForProfile(profile Profile) []Component {
	switch profile {
	case ProfileFull, ProfileAppWebSubsRemoteDB:
		return []Component{ComponentCore, ComponentSubs}
	case ProfileWebOnly:
		return []Component{ComponentCore}
	case ProfileSubsOnly:
		return []Component{ComponentSubs}
	default:
		return []Component{}
	}
}

func containsComponent(components []Component, wanted Component) bool {
	for _, component := range components {
		if component == wanted {
			return true
		}
	}
	return false
}

func conditionalRelease(installed bool, release string) string {
	if installed {
		return release
	}
	return ""
}

func prepareInstallConfiguration(paths Paths, options InstallOptions) (map[string]string, error) {
	credentials := map[string]string{}
	if options.Profile == ProfileDatabaseOnly {
		path := filepath.Join(paths.CredentialDir, "database-only.env")
		password, readErr := readEnvValue(path, "DB_PASSWORD")
		if readErr != nil || !regexp.MustCompile(`^[a-f0-9]{48}$`).MatchString(password) {
			var err error
			password, err = randomSecret(24)
			if err != nil {
				return nil, err
			}
		}
		if err := createLocalDatabase("nexus", "nexus_app", password, options.ApplicationHost); err != nil {
			return nil, err
		}
		if err := writeEnvironmentFile(path, map[string]string{"DB_HOST": options.DatabaseListenAddress, "DB_PORT": "3306", "DB_DATABASE": "nexus", "DB_USERNAME": "nexus_app", "DB_PASSWORD": password}); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0600); err != nil {
			return nil, err
		}
		fmt.Printf("Database connection details were saved to %s.\n", path)
		return map[string]string{"database_credentials_file": path}, nil
	}
	if options.Profile == ProfileSubsOnly {
		return credentials, nil
	}
	databaseHost := options.DatabaseHost
	databaseName := options.DatabaseName
	databaseUser := options.DatabaseUser
	databasePassword := options.DatabasePassword
	existingCoreEnvironment := filepath.Join(paths.NexusConfigDir, string(ComponentCore), ".env")
	if options.Profile == ProfileFull {
		databaseHost = "127.0.0.1"
		databaseName = "nexus"
		databaseUser = "nexus_app"
		password, readErr := readEnvValue(existingCoreEnvironment, "DB_PASSWORD")
		if readErr != nil || !regexp.MustCompile(`^[a-f0-9]{48}$`).MatchString(password) {
			password, err := randomSecret(24)
			if err != nil {
				return nil, err
			}
			databasePassword = password
		} else {
			databasePassword = password
		}
		if err := createLocalDatabase(databaseName, databaseUser, databasePassword, "127.0.0.1"); err != nil {
			return nil, err
		}
	}
	appKey, appKeyErr := readEnvValue(existingCoreEnvironment, "APP_KEY")
	if appKeyErr != nil || !strings.HasPrefix(appKey, "base64:") {
		appKeyBytes := make([]byte, 32)
		if _, err := rand.Read(appKeyBytes); err != nil {
			return nil, err
		}
		appKey = "base64:" + base64.StdEncoding.EncodeToString(appKeyBytes)
	}
	nexusToken, nexusTokenErr := readEnvValue(existingCoreEnvironment, "NEXUS_API_TOKEN")
	if nexusTokenErr != nil || nexusToken == "" {
		var err error
		nexusToken, err = randomSecret(32)
		if err != nil {
			return nil, err
		}
	}
	discordToken, discordTokenErr := readEnvValue(existingCoreEnvironment, "DISCORD_BOT_KEY")
	if discordTokenErr != nil || discordToken == "" {
		var err error
		discordToken, err = randomSecret(32)
		if err != nil {
			return nil, err
		}
	}
	values := map[string]string{
		"APP_NAME": "Nexus", "APP_ENV": "production", "APP_KEY": appKey, "APP_DEBUG": "false", "APP_URL": "https://" + options.Domain,
		"DB_CONNECTION": "mysql", "DB_HOST": databaseHost, "DB_PORT": "3306", "DB_DATABASE": databaseName, "DB_USERNAME": databaseUser, "DB_PASSWORD": databasePassword,
		"CACHE_STORE": "database", "SESSION_DRIVER": "database", "QUEUE_CONNECTION": "database",
		"PW_API_KEY": options.PWAPIKey, "PW_API_MUTATION_KEY": options.PWMutationKey, "PW_ALLIANCE_ID": options.AllianceID, "NEXUS_API_TOKEN": nexusToken, "DISCORD_BOT_KEY": discordToken,
	}
	coreConfig := filepath.Join(paths.NexusConfigDir, string(ComponentCore))
	if err := os.MkdirAll(coreConfig, 0750); err != nil {
		return nil, err
	}
	if err := writeDotEnvFile(filepath.Join(coreConfig, ".env"), values); err != nil {
		return nil, err
	}
	if err := chownPathToGroup(filepath.Join(coreConfig, ".env"), "nexus-core", 0640); err != nil {
		return nil, err
	}
	return values, nil
}

func createLocalDatabase(database, username, password, allowedHost string) error {
	if database != "nexus" || username != "nexus_app" || !regexp.MustCompile(`^[a-f0-9]{48}$`).MatchString(password) ||
		(allowedHost != "127.0.0.1" && !privateIPv4(allowedHost)) {
		return errors.New("generated database configuration is invalid")
	}
	sql := "CREATE DATABASE IF NOT EXISTS `nexus` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n" +
		"CREATE USER IF NOT EXISTS 'nexus_app'@'" + allowedHost + "' IDENTIFIED BY '" + password + "';\n" +
		"ALTER USER 'nexus_app'@'" + allowedHost + "' IDENTIFIED BY '" + password + "';\n" +
		"GRANT ALL PRIVILEGES ON `nexus`.* TO 'nexus_app'@'" + allowedHost + "';\nFLUSH PRIVILEGES;\n"
	return runFixedCommandWithInput(context.Background(), "/usr/bin/mysql", nil, []byte(sql))
}

func privateIPv4(value string) bool {
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() {
		return false
	}
	return address.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(address)
}

func configureRemoteDatabase(ctx context.Context, platform Platform, listenAddress string) error {
	if !privateIPv4(listenAddress) {
		return errors.New("database listen address must be private IPv4")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("inspect database host interfaces: %w", err)
	}
	assigned := false
	for _, address := range addresses {
		if networkAddress, ok := address.(*net.IPNet); ok && networkAddress.IP.String() == listenAddress {
			assigned = true
			break
		}
	}
	if !assigned {
		return errors.New("database listen address is not assigned to this host")
	}
	path := "/etc/mysql/mysql.conf.d/99-nexus-remote.cnf"
	unit := "mysql.service"
	if platform.ID == "debian" {
		path = "/etc/mysql/mariadb.conf.d/99-nexus-remote.cnf"
		unit = "mariadb.service"
	}
	if err := atomicWriteBytes(path, []byte("[mysqld]\nbind-address = "+listenAddress+"\n"), 0644); err != nil {
		return err
	}
	return runFixedCommand(ctx, "/usr/bin/systemctl", []string{"restart", unit})
}

func runFixedCommandWithInput(ctx context.Context, binary string, arguments []string, input []byte) error {
	if err := validateFixedCommand(binary, arguments); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}
	command.Stdin = bytes.NewReader(input)
	command.Stdout = nil
	command.Stderr = nil
	return command.Run()
}

func provisionAdministrator(ctx context.Context, engine DeploymentEngine, release string, options InstallOptions) error {
	payload, err := json.Marshal(map[string]string{"email": options.AdminEmail, "password": options.AdminPassword, "nation_id": options.AdminNationID})
	if err != nil {
		return err
	}
	directory, err := componentReleaseDirectory(engine.Config.Paths, ComponentCore, release)
	if err != nil {
		return err
	}
	artisan := filepath.Join(directory, "artisan")
	arguments := []string{"-u", "nexus-core", "--", "/usr/bin/php", artisan, "nexus:provision-admin"}
	if err := validateFixedCommand("/usr/sbin/runuser", arguments); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "/usr/sbin/runuser", arguments...)
	command.Dir = directory
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin = bytes.NewReader(payload)
	return command.Run()
}

func configurePHPFPM(ctx context.Context, paths Paths) error {
	command := exec.CommandContext(ctx, "/usr/bin/php", "-r", "echo PHP_MAJOR_VERSION.'.'.PHP_MINOR_VERSION;")
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	output, err := command.Output()
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+$`).MatchString(version) {
		return errors.New("PHP version could not be determined")
	}
	pool := "[nexus-core]\nuser = nexus-core\ngroup = nexus-core\nlisten = /run/php/nexus-core.sock\nlisten.owner = www-data\nlisten.group = www-data\nlisten.mode = 0660\npm = dynamic\npm.max_children = 20\npm.start_servers = 2\npm.min_spare_servers = 2\npm.max_spare_servers = 6\nclear_env = yes\n"
	path := filepath.Join("/etc/php", version, "fpm/pool.d/nexus-core.conf")
	if err := atomicWriteBytes(path, []byte(pool), 0644); err != nil {
		return err
	}
	phpUnit := "php" + version + "-fpm.service"
	unitPath := filepath.Join("/lib/systemd/system", phpUnit)
	if _, err := os.Lstat(unitPath); err != nil {
		unitPath = filepath.Join("/usr/lib/systemd/system", phpUnit)
		if _, fallbackErr := os.Lstat(unitPath); fallbackErr != nil {
			return errors.New("PHP-FPM systemd unit is unavailable")
		}
	}
	if err := replaceSymlink(unitPath, "/etc/systemd/system/nexus-core-php-fpm.service"); err != nil {
		return err
	}
	if err := runFixedCommand(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}); err != nil {
		return err
	}
	return runFixedCommand(ctx, "/usr/bin/systemctl", []string{"restart", "nexus-core-php-fpm.service"})
}

func configureNginx(ctx context.Context, domain string) error {
	configuration := "server {\n    listen 80;\n    listen [::]:80;\n    server_name " + domain + ";\n    root /opt/nexus/nexus-core/current/public;\n    index index.php;\n    client_max_body_size 32m;\n    location / { try_files $uri $uri/ /index.php?$query_string; }\n    location ~ \\.php$ { include snippets/fastcgi-php.conf; fastcgi_pass unix:/run/php/nexus-core.sock; }\n    location ~ /\\. { deny all; }\n}\n"
	available := "/etc/nginx/sites-available/nexus.conf"
	if err := atomicWriteBytes(available, []byte(configuration), 0644); err != nil {
		return err
	}
	if err := replaceSymlink(available, "/etc/nginx/sites-enabled/nexus.conf"); err != nil {
		return err
	}
	_ = os.Remove("/etc/nginx/sites-enabled/default")
	if err := runFixedCommand(ctx, "/usr/sbin/nginx", []string{"-t"}); err != nil {
		return err
	}
	return runFixedCommand(ctx, "/usr/bin/systemctl", []string{"reload", "nginx.service"})
}

func requestTLSCertificate(ctx context.Context, domain, email string) error {
	return runFixedCommand(ctx, "/usr/bin/certbot", []string{"--nginx", "--non-interactive", "--agree-tos", "--redirect", "--email", email, "-d", domain})
}

func updateInstalledProfile(paths Paths, profile Profile) error {
	path := filepath.Join(paths.ConfigDir, "config.json")
	var config diskConfig
	if err := readStrictJSONFile(path, 64*1024, &config); err != nil {
		return err
	}
	config.CurrentProfile = profile
	return atomicWriteJSON(path, config, 0600)
}

func chownPathToGroup(path, groupName string, mode os.FileMode) error {
	group, err := user.LookupGroup(groupName)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (engine DeploymentEngine) writeStandaloneSubsConfiguration(release string, options InstallOptions) error {
	pwPath, err := engine.writeCredential(ComponentSubs, "pw-api-token", options.PWAPIKey)
	if err != nil {
		return err
	}
	corePath, err := engine.writeCredential(ComponentSubs, "nexus-api-token", options.CoreToken)
	if err != nil {
		return err
	}
	dataDirectory := filepath.Join(engine.Config.Paths.DataDir, string(ComponentSubs))
	if err := os.MkdirAll(dataDirectory, 0750); err != nil {
		return err
	}
	if err := chownPathToGroup(dataDirectory, string(ComponentSubs), 0770); err != nil {
		return err
	}
	values := map[string]string{
		"NODE_ENV": "production", "NEXUS_RELEASE_ID": release, "BUILD_COMMIT": release,
		"PW_API_URL": "https://api.politicsandwar.com/subscriptions/v1/subscribe", "PW_API_TOKEN_FILE": pwPath,
		"PUSHER_SOCKET_HOST": "socket.politicsandwar.com", "PW_AUTH_URL": "https://api.politicsandwar.com/subscriptions/v1/auth",
		"NEXUS_API_URL": options.CoreURL, "NEXUS_API_TOKEN_FILE": corePath, "DELIVERY_DRIVER": "http",
		"PROCESS_HEALTH_FILE": filepath.Join(dataDirectory, "process-health.json"),
	}
	directory := filepath.Join(engine.Config.Paths.NexusConfigDir, string(ComponentSubs))
	environmentPath := filepath.Join(directory, "environment")
	if err := writeEnvironmentFile(environmentPath, values); err != nil {
		return err
	}
	return chownPathToGroup(environmentPath, string(ComponentSubs), 0640)
}

func systemdUnits() map[string]string {
	return map[string]string{
		"/etc/tmpfiles.d/nexus-updater.conf": `d /run/nexus-updater 0750 root nexus-updater-control -
`,
		"/etc/systemd/system/nexus-updater.socket": `[Unit]
Description=Nexus updater control socket
After=systemd-tmpfiles-setup.service

[Socket]
ListenStream=/run/nexus-updater/control.sock
SocketUser=root
SocketGroup=nexus-updater-control
SocketMode=0660
DirectoryMode=0750
RemoveOnStop=yes

[Install]
WantedBy=sockets.target
`,
		"/etc/systemd/system/nexus-updater.service": `[Unit]
Description=Nexus updater coordinator
Requires=nexus-updater.socket

[Service]
ExecStart=/usr/local/bin/nexus internal coordinator
User=root
Group=root
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/run/nexus-updater /var/lib/nexus-updater /run/lock/nexus-updater
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER CAP_SETUID CAP_SETGID CAP_KILL
StateDirectory=nexus-updater
StateDirectoryMode=0700
LimitNOFILE=1024
LimitNPROC=128
MemoryMax=256M
TasksMax=128
UMask=0077
`,
		"/etc/systemd/system/nexus-updater-worker@.service": `[Unit]
Description=Nexus updater operation %i
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/nexus internal worker %i
User=root
Group=root
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/opt/nexus /etc/nexus /etc/nexus-updater /var/lib/nexus /var/lib/nexus-updater /run/nexus-updater /run/lock/nexus-updater /usr/local/libexec/nexus-updater /usr/local/bin
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER CAP_SETUID CAP_SETGID CAP_KILL
UMask=0077
TimeoutStartSec=infinity
RuntimeMaxSec=2h
MemoryMax=512M
TasksMax=128
LimitNOFILE=4096
`,
		"/etc/systemd/system/nexus-core.service": `[Unit]
Description=Nexus Core queue worker
After=network-online.target mysql.service

[Service]
Type=simple
User=nexus-core
Group=nexus-core
WorkingDirectory=/opt/nexus/nexus-core/current
ExecStart=/usr/bin/php /opt/nexus/nexus-core/current/artisan queue:work --sleep=3 --tries=3 --max-time=3600
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/nexus/nexus-core
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
MemoryMax=512M
TasksMax=128
LimitNOFILE=4096
UMask=0027

[Install]
WantedBy=multi-user.target
`,
		"/etc/systemd/system/nexus-scheduler.service": `[Unit]
Description=Nexus scheduler tick

[Service]
Type=oneshot
User=nexus-core
Group=nexus-core
WorkingDirectory=/opt/nexus/nexus-core/current
ExecStart=/usr/bin/php /opt/nexus/nexus-core/current/artisan schedule:run
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/nexus/nexus-core
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
MemoryMax=256M
TasksMax=64
LimitNOFILE=2048
`,
		"/etc/systemd/system/nexus-scheduler.timer": `[Unit]
Description=Run the Nexus scheduler every minute

[Timer]
OnCalendar=*-*-* *:*:00
Persistent=true
Unit=nexus-scheduler.service

[Install]
WantedBy=timers.target
`,
		"/etc/systemd/system/nexus-subs.service": `[Unit]
Description=Nexus Subs
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nexus-subs
Group=nexus-subs
WorkingDirectory=/opt/nexus/nexus-subs/current
EnvironmentFile=/etc/nexus/nexus-subs/environment
Environment=NODE_ENV=production
Environment=NODE_OPTIONS=
Environment=PATH=/usr/bin:/bin
Environment=PROCESS_HEALTH_FILE=/var/lib/nexus/nexus-subs/process-health.json
Environment=DEAD_LETTER_FILE=/var/lib/nexus/nexus-subs/dead-letter-events.jsonl
ExecStart=/usr/bin/node /opt/nexus/nexus-subs/current/src/index.js
Restart=on-failure
RestartSec=5
TimeoutStopSec=70
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/nexus/nexus-subs
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
MemoryMax=512M
TasksMax=128
LimitNOFILE=4096
UMask=0077

[Install]
WantedBy=multi-user.target
`,
		"/etc/systemd/system/nexus-discord.service": `[Unit]
Description=Nexus Discord
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nexus-discord
Group=nexus-discord
WorkingDirectory=/opt/nexus/nexus-discord/current
EnvironmentFile=/etc/nexus/nexus-discord/environment
Environment=NODE_ENV=production
Environment=NODE_OPTIONS=
Environment=PATH=/usr/bin:/bin
Environment=PROCESS_HEALTH_FILE=/var/lib/nexus/nexus-discord/process-health.json
ExecStart=/usr/bin/node /opt/nexus/nexus-discord/current/src/bot.js
Restart=on-failure
RestartSec=5
TimeoutStopSec=320
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/nexus/nexus-discord
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
MemoryMax=768M
TasksMax=256
LimitNOFILE=8192
UMask=0077

[Install]
WantedBy=multi-user.target
`,
		"/etc/systemd/system/nexus-discord-register.service": `[Unit]
Description=Register Nexus Discord slash commands
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=nexus-discord
Group=nexus-discord
WorkingDirectory=/opt/nexus/nexus-discord/current
EnvironmentFile=/etc/nexus/nexus-discord/environment
Environment=NODE_ENV=production
Environment=NODE_OPTIONS=
Environment=PATH=/usr/bin:/bin
ExecStart=/usr/bin/node /opt/nexus/nexus-discord/current/src/registerCommands.js
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
TimeoutStartSec=120
MemoryMax=512M
TasksMax=128
LimitNOFILE=4096
UMask=0077
`,
	}
}

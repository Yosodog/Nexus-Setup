package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const (
	legacyCorePath         = "/var/www/nexus"
	legacySubsPath         = "/var/nexus-subs"
	legacyDiscordPath      = "/opt/nexus-ams-discord"
	legacySupervisorConfig = "/etc/supervisor/conf.d"
	discordUnitPath        = "/etc/systemd/system/nexus-discord.service"
	legacyDisabledSuffix   = ".nexus-disabled"
)

var legacyPHPSocketPattern = regexp.MustCompile(`unix:/run/php/php[0-9]+\.[0-9]+-fpm\.sock`)

type LegacyAssessment struct {
	Detected   bool
	Profile    Profile
	Releases   map[Component]string
	Components []Component
}

func AssessLegacyInstallation(ctx context.Context) (LegacyAssessment, error) {
	coreExists, coreRelease, err := inspectLegacyRepository(ctx, legacyCorePath, coreRepository)
	if err != nil {
		return LegacyAssessment{Detected: true}, err
	}
	subsExists, subsRelease, err := inspectLegacyRepository(ctx, legacySubsPath, subsRepository)
	if err != nil {
		return LegacyAssessment{Detected: coreExists || pathExists(legacySubsPath)}, err
	}
	discordExists, discordRelease, err := inspectLegacyRepository(ctx, legacyDiscordPath, discordRepository)
	if err != nil {
		return LegacyAssessment{Detected: coreExists || subsExists || pathExists(legacyDiscordPath)}, err
	}
	if !coreExists && !subsExists && !discordExists {
		return LegacyAssessment{}, nil
	}
	if discordExists && !coreExists {
		return LegacyAssessment{Detected: true}, errors.New("legacy Discord can only be adopted together with legacy Core on the same host")
	}
	assessment := LegacyAssessment{Detected: true, Releases: map[Component]string{}}
	if coreExists {
		assessment.Releases[ComponentCore] = coreRelease
		assessment.Components = append(assessment.Components, ComponentCore)
	}
	if subsExists {
		assessment.Releases[ComponentSubs] = subsRelease
		assessment.Components = append(assessment.Components, ComponentSubs)
	}
	if discordExists {
		assessment.Releases[ComponentDiscord] = discordRelease
		assessment.Components = append(assessment.Components, ComponentDiscord)
	}

	switch {
	case coreExists && subsExists:
		databaseHost, readErr := readEnvValue(filepath.Join(legacyCorePath, ".env"), "DB_HOST")
		if readErr != nil || databaseHost == "" {
			return assessment, errors.New("legacy Core database configuration could not be read")
		}
		if isLocalDatabaseHost(databaseHost) {
			assessment.Profile = ProfileFull
		} else {
			assessment.Profile = ProfileAppWebSubsRemoteDB
		}
	case coreExists:
		databaseHost, readErr := readEnvValue(filepath.Join(legacyCorePath, ".env"), "DB_HOST")
		if readErr != nil || databaseHost == "" {
			return assessment, errors.New("legacy Core database configuration could not be read")
		}
		if isLocalDatabaseHost(databaseHost) {
			return assessment, errors.New("legacy Core with a local database but without local Subs does not match a supported managed profile")
		} else {
			assessment.Profile = ProfileWebOnly
		}
	case subsExists:
		assessment.Profile = ProfileSubsOnly
	}

	return assessment, nil
}

func inspectLegacyRepository(ctx context.Context, path, repository string) (bool, string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, "", fmt.Errorf("legacy path %s is not a safe directory", path)
	}
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return false, "", fmt.Errorf("legacy path %s is not a recognized Git checkout", path)
	}
	origin, err := runGitOutput(ctx, path, "remote", "get-url", "origin")
	if err != nil || !officialLegacyOrigin(origin, repository) {
		return false, "", fmt.Errorf("legacy path %s does not use the official Yosodog/%s repository", path, repository)
	}
	status, err := runGitOutput(ctx, path, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, "", fmt.Errorf("legacy path %s could not be inspected", path)
	}
	if strings.TrimSpace(status) != "" {
		return false, "", fmt.Errorf("legacy path %s contains local modifications; it was left unchanged", path)
	}
	tag, err := runGitOutput(ctx, path, "describe", "--tags", "--exact-match", "HEAD")
	if err != nil {
		return false, "", fmt.Errorf("legacy path %s is not on an exact published release tag", path)
	}
	tag = strings.TrimSpace(tag)
	if _, err := ParseVersion(tag); err != nil {
		return false, "", fmt.Errorf("legacy path %s has an unsupported release tag", path)
	}
	if repository == coreRepository || repository == discordRepository {
		environment, statErr := os.Lstat(filepath.Join(path, ".env"))
		if statErr != nil || !environment.Mode().IsRegular() || environment.Mode()&os.ModeSymlink != 0 {
			return false, "", fmt.Errorf("legacy environment file %s/.env is unavailable", path)
		}
	}
	return true, tag, nil
}

func officialLegacyOrigin(origin, repository string) bool {
	origin = strings.TrimSpace(strings.TrimSuffix(origin, "/"))
	origin = strings.TrimSuffix(origin, ".git")
	return origin == "https://github.com/Yosodog/"+repository ||
		origin == "git@github.com:Yosodog/"+repository ||
		origin == "ssh://git@github.com/Yosodog/"+repository
}

func runGitOutput(ctx context.Context, directory string, arguments ...string) (string, error) {
	if directory != legacyCorePath && directory != legacySubsPath && directory != legacyDiscordPath {
		return "", errors.New("legacy repository path is not allowlisted")
	}
	commandArguments := append([]string{"-C", directory}, arguments...)
	if err := validateFixedCommand("/usr/bin/git", commandArguments); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "/usr/bin/git", commandArguments...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return "", err
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, 64*1024+1))
	waitErr := command.Wait()
	if readErr != nil {
		return "", readErr
	}
	if len(output) > 64*1024 {
		return "", errors.New("legacy Git output is too large")
	}
	if waitErr != nil {
		return "", waitErr
	}
	return strings.TrimSpace(string(output)), nil
}

func AdoptLegacyInstallation(ctx context.Context) error {
	assessment, err := AssessLegacyInstallation(ctx)
	if err != nil {
		return err
	}
	if !assessment.Detected {
		return errors.New("no standard legacy Nexus installation was detected")
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
		return errors.New("Nexus is already managed; use nexus update")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("existing installation state is invalid; run nexus doctor")
	}
	if err := ensureFreeSpace(config.Paths.StateDir, minimumReleaseSpace); err != nil {
		return err
	}
	adoptsCore := containsComponent(assessment.Components, ComponentCore)
	adoptsSubs := containsComponent(assessment.Components, ComponentSubs)
	adoptsDiscord := containsComponent(assessment.Components, ComponentDiscord)
	var subsStream *subsRedisStream
	if adoptsSubs {
		subsStream, err = legacySubsRedisStream(filepath.Join(legacySubsPath, ".env"), legacySubsPath)
		if err != nil {
			return err
		}
		if subsStream != nil && adoptsCore {
			if err := checkCoreSubsStream(filepath.Join(legacyCorePath, ".env"), *subsStream); err != nil {
				return err
			}
		}
	}
	var discord legacyDiscordConfiguration
	if adoptsDiscord {
		discord, err = readLegacyDiscordConfiguration(filepath.Join(legacyDiscordPath, ".env"), legacyDiscordPath)
		if err != nil {
			return err
		}
	}
	supervisorConfigs, err := findLegacySupervisorConfigs(legacySupervisorConfig)
	if err != nil {
		return err
	}
	for _, program := range supervisorConfigs {
		if pathExists(program.Path + legacyDisabledSuffix) {
			return fmt.Errorf("legacy Supervisor backup already exists for %s", filepath.Base(program.Path))
		}
	}
	platform, err := DetectPlatform()
	if err != nil {
		return err
	}
	if err := provisionPackages(ctx, platform, assessment.Profile); err != nil {
		return err
	}
	releases := NewGitHubReleaseClient()
	versions := assessment.Releases
	channel := ChannelStable
	manifests := map[Component]ReleaseManifest{}
	for _, component := range assessment.Components {
		manifest, manifestErr := releases.Manifest(ctx, component, versions[component])
		if manifestErr != nil {
			return fmt.Errorf("the published %s %s release is unavailable: %w", component, versions[component], manifestErr)
		}
		if reason := unmetRequirement(manifest, versions, BuildVersion); reason != "" {
			return fmt.Errorf("legacy %s %s %s", component, versions[component], reason)
		}
		manifests[component] = manifest
		if version, _ := ParseVersion(versions[component]); !version.Stable() {
			channel = ChannelBeta
		}
	}
	engine := DeploymentEngine{Config: config, Releases: releases, Executor: OSCommandExecutor{}}
	if err := assertLegacyAdoptionTargetsAvailable(config.Paths, assessment.Components); err != nil {
		return err
	}
	for _, component := range assessment.Components {
		if err := engine.stageComponent(ctx, "adoption", manifests[component]); err != nil {
			return err
		}
	}

	state := InstallationState{Profile: assessment.Profile, Channel: channel, Components: map[Component]InstalledComponent{}}
	for _, component := range []Component{ComponentCore, ComponentSubs, ComponentDiscord} {
		installed := containsComponent(assessment.Components, component)
		state.Components[component] = InstalledComponent{Installed: installed, Enabled: installed, CurrentRelease: versions[component]}
	}
	if adoptsSubs {
		state.SubsDelivery = SubsDeliveryHTTP
		if subsStream != nil {
			state.SubsDelivery = SubsDeliveryRedisStream
		}
	}

	activatedUnits := []string{}
	disabledSupervisorConfigs := []string{}
	legacyStopped := false
	legacyDiscord := legacyDiscordService{}
	nginxBackup := []byte(nil)
	nginxMode := os.FileMode(0644)
	nginxChanged := false
	crontabBackup := []byte(nil)
	crontabMode := os.FileMode(0644)
	crontabChanged := false
	legacyMaintenance := false
	completed := false
	defer func() {
		if completed {
			return
		}
		for _, unit := range activatedUnits {
			_ = runFixedCommand(context.Background(), config.SystemctlBinary, []string{"disable", "--now", unit})
		}
		for _, component := range assessment.Components {
			if link, linkErr := currentLink(config.Paths, component); linkErr == nil {
				_ = os.Remove(link)
			}
		}
		_ = os.Remove(config.Paths.InstallationDB)
		if crontabChanged {
			_ = atomicWriteBytes("/etc/crontab", crontabBackup, crontabMode)
		}
		for index := len(disabledSupervisorConfigs) - 1; index >= 0; index-- {
			disabled := disabledSupervisorConfigs[index]
			_ = os.Rename(disabled, strings.TrimSuffix(disabled, legacyDisabledSuffix))
		}
		if len(disabledSupervisorConfigs) > 0 && pathExists("/usr/bin/supervisorctl") {
			_ = runFixedCommand(context.Background(), "/usr/bin/supervisorctl", []string{"reread"})
			_ = runFixedCommand(context.Background(), "/usr/bin/supervisorctl", []string{"update"})
		}
		if nginxChanged {
			_ = atomicWriteBytes("/etc/nginx/sites-available/nexus.conf", nginxBackup, nginxMode)
			_ = runFixedCommand(context.Background(), "/usr/sbin/nginx", []string{"-t"})
			_ = runFixedCommand(context.Background(), config.SystemctlBinary, []string{"reload", "nginx.service"})
		}
		if legacyStopped {
			startLegacySupervisor(context.Background(), supervisorConfigs)
		}
		legacyDiscord.restore(context.Background(), config.SystemctlBinary)
		if legacyMaintenance {
			_ = runLegacyArtisan(context.Background(), "up")
		}
		_ = os.Remove(filepath.Join(config.Paths.NexusConfigDir, string(ComponentCore), ".env"))
		_ = os.Remove(filepath.Join(config.Paths.NexusConfigDir, string(ComponentSubs), "environment"))
		_ = os.RemoveAll(filepath.Join(config.Paths.CredentialDir, string(ComponentSubs)))
		_ = os.Remove(filepath.Join(config.Paths.NexusConfigDir, string(ComponentDiscord), "environment"))
		_ = os.RemoveAll(filepath.Join(config.Paths.CredentialDir, string(ComponentDiscord)))
		_ = os.RemoveAll(filepath.Join(config.Paths.DataDir, string(ComponentCore), "storage"))
	}()

	if adoptsCore {
		if err := runLegacyArtisan(ctx, "down", "--retry=60"); err != nil {
			return fmt.Errorf("put legacy Core in maintenance mode: %w", err)
		}
		legacyMaintenance = true
	}
	stopLegacySupervisor(ctx, supervisorConfigs)
	legacyStopped = true
	if adoptsDiscord {
		if err := legacyDiscord.replace(ctx, config.SystemctlBinary); err != nil {
			return fmt.Errorf("replace the legacy Discord service: %w", err)
		}
	}

	if adoptsCore {
		if err := copyLegacyCoreState(config.Paths); err != nil {
			return err
		}
		if err := engine.prepareCoreRelease(versions[ComponentCore]); err != nil {
			return err
		}
		if err := configurePHPFPM(ctx, config.Paths); err != nil {
			return err
		}
		if err := engine.runArtisan(ctx, versions[ComponentCore], "migrate", "--force"); err != nil {
			return fmt.Errorf("legacy database migration failed: %w", err)
		}
		if err := engine.runArtisan(ctx, versions[ComponentCore], "up"); err != nil {
			return fmt.Errorf("leave managed Core maintenance mode: %w", err)
		}
	}
	if adoptsSubs {
		if assessment.Profile == ProfileSubsOnly {
			options, optionsErr := legacySubsOptions()
			if optionsErr != nil {
				return optionsErr
			}
			if err := engine.writeStandaloneSubsConfiguration(versions[ComponentSubs], options, subsStream); err != nil {
				return err
			}
		} else if err := engine.writeComponentConfiguration(ComponentSubs, versions[ComponentSubs], nil, subsStream); err != nil {
			return err
		}
	}
	if adoptsDiscord {
		if err := engine.writeLegacyDiscordConfiguration(versions[ComponentDiscord], discord); err != nil {
			return err
		}
	}
	for _, component := range assessment.Components {
		if err := atomicSwitchRelease(config.Paths, component, versions[component]); err != nil {
			return err
		}
	}

	services := hostServicesFor(config.Paths, state)
	for _, component := range assessment.Components {
		units, unitErr := componentEnableUnits(component, services)
		if unitErr != nil {
			return unitErr
		}
		for _, unit := range units {
			if err := runFixedCommand(ctx, config.SystemctlBinary, []string{"enable", "--now", unit}); err != nil {
				return err
			}
			activatedUnits = append(activatedUnits, unit)
		}
	}
	if adoptsCore {
		if err := runFixedCommand(ctx, config.SystemctlBinary, []string{"enable", "--now", "nexus-scheduler.timer"}); err != nil {
			return err
		}
		activatedUnits = append(activatedUnits, "nexus-scheduler.timer")
	}
	if err := engine.healthCheck(ctx, assessment.Components, versions, services); err != nil {
		return err
	}
	if adoptsDiscord {
		if err := engine.registerDiscordCommands(ctx); err != nil {
			return err
		}
	}

	if adoptsCore {
		nginxBackup, nginxMode, err = switchLegacyNginx(ctx, config)
		if err != nil {
			return err
		}
		nginxChanged = true
	}
	disabledSupervisorConfigs, err = disableLegacySupervisor(ctx, supervisorConfigs)
	if err != nil {
		return err
	}
	crontabBackup, crontabMode, crontabChanged, err = disableLegacyCron()
	if err != nil {
		return err
	}

	if err := updateInstalledProfile(config.Paths, assessment.Profile); err != nil {
		return err
	}
	if err := SaveInstallation(config.Paths, state); err != nil {
		return err
	}
	if legacyMaintenance {
		if err := runLegacyArtisan(ctx, "up"); err != nil {
			return fmt.Errorf("restore the preserved legacy checkout: %w", err)
		}
		legacyMaintenance = false
	}
	completed = true
	fmt.Printf("Legacy Nexus (%s) was adopted successfully. The original checkouts were preserved.\n", describeVersions(versions))
	return nil
}

func assertLegacyAdoptionTargetsAvailable(paths Paths, components []Component) error {
	targets := []string{}
	if containsComponent(components, ComponentCore) {
		targets = append(targets,
			filepath.Join(paths.NexusConfigDir, string(ComponentCore), ".env"),
			filepath.Join(paths.DataDir, string(ComponentCore), "storage"),
		)
	}
	if containsComponent(components, ComponentSubs) {
		targets = append(targets,
			filepath.Join(paths.NexusConfigDir, string(ComponentSubs), "environment"),
			filepath.Join(paths.CredentialDir, string(ComponentSubs)),
		)
	}
	if containsComponent(components, ComponentDiscord) {
		targets = append(targets,
			filepath.Join(paths.NexusConfigDir, string(ComponentDiscord), "environment"),
			filepath.Join(paths.CredentialDir, string(ComponentDiscord)),
			discordUnitPath+legacyDisabledSuffix,
		)
	}
	for _, component := range components {
		link, err := currentLink(paths, component)
		if err != nil {
			return err
		}
		targets = append(targets, link)
	}
	for _, target := range targets {
		if pathExists(target) {
			return fmt.Errorf("managed adoption target %s already exists; run nexus doctor before retrying", target)
		}
	}
	return nil
}

func runLegacyArtisan(ctx context.Context, arguments ...string) error {
	artisan := filepath.Join(legacyCorePath, "artisan")
	info, err := os.Lstat(artisan)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("legacy Artisan entry point is unavailable")
	}
	commandArguments := []string{"-u", "www-data", "--", "/usr/bin/php", artisan}
	commandArguments = append(commandArguments, arguments...)
	return runFixedCommandInDirectory(ctx, legacyCorePath, "/usr/sbin/runuser", commandArguments)
}

func copyLegacyCoreState(paths Paths) error {
	environment, err := readBoundedRegularFile(filepath.Join(legacyCorePath, ".env"), 1024*1024)
	if err != nil || len(environment) == 0 {
		return errors.New("legacy Core environment file is invalid")
	}
	configDirectory := filepath.Join(paths.NexusConfigDir, string(ComponentCore))
	if err := os.MkdirAll(configDirectory, 0750); err != nil {
		return err
	}
	environmentPath := filepath.Join(configDirectory, ".env")
	if err := atomicWriteBytes(environmentPath, environment, 0640); err != nil {
		return err
	}
	if err := chownPathToGroup(environmentPath, "nexus-core", 0640); err != nil {
		return err
	}
	legacyStorage := filepath.Join(legacyCorePath, "storage")
	managedStorage := filepath.Join(paths.DataDir, string(ComponentCore), "storage")
	if pathExists(managedStorage) {
		return errors.New("managed Core storage already exists; refusing to overwrite it")
	}
	if pathExists(legacyStorage) {
		storageBytes, err := managedTreeSize(legacyStorage)
		if err != nil {
			return fmt.Errorf("inspect legacy Core storage: %w", err)
		}
		if err := ensureFreeSpace(paths.DataDir, uint64(storageBytes)+minimumReleaseSpace); err != nil {
			return err
		}
		if err := copyRegularTree(legacyStorage, managedStorage); err != nil {
			return fmt.Errorf("copy legacy Core storage: %w", err)
		}
	} else if err := os.MkdirAll(managedStorage, 0770); err != nil {
		return err
	}
	return chownTree(managedStorage, "nexus-core")
}

func chownTree(root, account string) error {
	userAccount, err := user.Lookup(account)
	if err != nil {
		return err
	}
	uid, err := parseIdentity(userAccount.Uid)
	if err != nil {
		return err
	}
	gid, err := parseIdentity(userAccount.Gid)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("legacy storage contains a symlink")
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0770)
		}
		return os.Chmod(path, 0660)
	})
}

func parseIdentity(value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, errors.New("service account identity is invalid")
	}
	return parsed, nil
}

func legacySubsOptions() (InstallOptions, error) {
	environment := filepath.Join(legacySubsPath, ".env")
	coreURL, err := readEnvValue(environment, "NEXUS_API_URL")
	if err != nil || coreURL == "" {
		return InstallOptions{}, errors.New("legacy Subs Core URL is unavailable")
	}
	coreToken, err := readEnvValue(environment, "NEXUS_API_TOKEN")
	if err != nil || coreToken == "" {
		return InstallOptions{}, errors.New("legacy Subs Core token is unavailable")
	}
	pwToken, err := readEnvValue(environment, "PW_API_TOKEN")
	if err != nil || pwToken == "" {
		pwToken, err = readEnvValue(environment, "PW_API_KEY")
	}
	if err != nil || pwToken == "" {
		return InstallOptions{}, errors.New("legacy Subs Politics & War credential is unavailable")
	}
	return InstallOptions{Profile: ProfileSubsOnly, CoreURL: coreURL, CoreToken: coreToken, PWAPIKey: pwToken}, nil
}

func switchLegacyNginx(ctx context.Context, config Config) ([]byte, os.FileMode, error) {
	path := "/etc/nginx/sites-available/nexus.conf"
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, errors.New("the standard legacy Nginx site is unavailable")
	}
	contents, err := readBoundedRegularFile(path, 1024*1024)
	if err != nil || len(contents) == 0 {
		return nil, 0, errors.New("the legacy Nginx site is invalid")
	}
	updated, err := rewriteLegacyNginx(contents)
	if err != nil {
		return nil, 0, err
	}
	if err := atomicWriteBytes(path, updated, info.Mode().Perm()); err != nil {
		return nil, 0, err
	}
	if err := runFixedCommand(ctx, "/usr/sbin/nginx", []string{"-t"}); err != nil {
		_ = atomicWriteBytes(path, contents, info.Mode().Perm())
		return nil, 0, err
	}
	if err := runFixedCommand(ctx, config.SystemctlBinary, []string{"reload", "nginx.service"}); err != nil {
		_ = atomicWriteBytes(path, contents, info.Mode().Perm())
		return nil, 0, err
	}
	return contents, info.Mode().Perm(), nil
}

func rewriteLegacyNginx(contents []byte) ([]byte, error) {
	if !bytes.Contains(contents, []byte(legacyCorePath)) {
		return nil, errors.New("the legacy Nginx site does not reference the standard Core path")
	}
	updated := bytes.ReplaceAll(contents, []byte(legacyCorePath), []byte("/opt/nexus/nexus-core/current"))
	updated = legacyPHPSocketPattern.ReplaceAll(updated, []byte("unix:/run/php/nexus-core.sock"))
	return updated, nil
}

// legacySupervisorProgram is a Supervisor configuration file that runs part
// of a legacy checkout, with the supervisorctl names of everything it defines.
type legacySupervisorProgram struct {
	Path    string
	Targets []string
}

var supervisorNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)

// findLegacySupervisorConfigs returns every Supervisor configuration with a
// program whose directory, or command, is inside a legacy checkout. Programs
// are discovered rather than listed so a renamed or extra worker is never left
// running next to the managed services.
func findLegacySupervisorConfigs(directory string) ([]legacySupervisorProgram, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect legacy Supervisor configuration: %w", err)
	}
	configs := []legacySupervisorProgram{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			return nil, fmt.Errorf("legacy Supervisor configuration %s is unsafe or unreadable", entry.Name())
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("legacy Supervisor configuration %s is unreadable", entry.Name())
		}
		targets, matched, err := parseLegacySupervisorConfig(contents)
		if err != nil {
			return nil, fmt.Errorf("legacy Supervisor configuration %s: %w", entry.Name(), err)
		}
		if matched {
			configs = append(configs, legacySupervisorProgram{Path: path, Targets: targets})
		}
	}
	return configs, nil
}

// parseLegacySupervisorConfig reports whether a configuration runs a legacy
// checkout and, if so, the supervisorctl targets for all of its programs.
// Disabling the file removes every program in it, so every one is stopped and
// later restored together.
func parseLegacySupervisorConfig(contents []byte) ([]string, bool, error) {
	type section struct {
		kind, name, directory, command string
		programs                       []string
	}
	sections := []*section{}
	var current *section
	for _, raw := range strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			kind, name, _ := strings.Cut(strings.TrimSpace(line[1:len(line)-1]), ":")
			current = &section{kind: strings.TrimSpace(kind), name: strings.TrimSpace(name)}
			sections = append(sections, current)
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if current == nil || !found {
			continue
		}
		if index := strings.Index(value, " ;"); index >= 0 {
			value = value[:index]
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "directory":
			current.directory = value
		case "command":
			current.command = value
		case "programs":
			for _, program := range strings.Split(value, ",") {
				if program = strings.TrimSpace(program); program != "" {
					current.programs = append(current.programs, program)
				}
			}
		}
	}
	matched := false
	grouped := map[string]bool{}
	for _, section := range sections {
		if section.kind == "program" && legacySupervisorProgramMatches(section.directory, section.command) {
			matched = true
		}
		if section.kind == "group" {
			for _, program := range section.programs {
				grouped[program] = true
			}
		}
	}
	if !matched {
		return nil, false, nil
	}
	targets := []string{}
	for _, section := range sections {
		if (section.kind != "program" && section.kind != "group") || (section.kind == "program" && grouped[section.name]) {
			continue
		}
		if !supervisorNamePattern.MatchString(section.name) {
			return nil, true, fmt.Errorf("Supervisor %s name %q is not supported", section.kind, section.name)
		}
		targets = append(targets, section.name+":*")
	}
	return targets, true, nil
}

func legacySupervisorProgramMatches(directory, command string) bool {
	for _, root := range []string{legacyCorePath, legacySubsPath, legacyDiscordPath} {
		if directory != "" && filepath.Clean(directory) == root {
			return true
		}
		if strings.Contains(command, root+"/") {
			return true
		}
	}
	return false
}

func stopLegacySupervisor(ctx context.Context, configs []legacySupervisorProgram) {
	controlLegacySupervisor(ctx, configs, "stop")
}

func startLegacySupervisor(ctx context.Context, configs []legacySupervisorProgram) {
	controlLegacySupervisor(ctx, configs, "start")
}

func controlLegacySupervisor(ctx context.Context, configs []legacySupervisorProgram, action string) {
	if !pathExists("/usr/bin/supervisorctl") {
		return
	}
	for _, config := range configs {
		for _, target := range config.Targets {
			_ = runFixedCommand(ctx, "/usr/bin/supervisorctl", []string{action, target})
		}
	}
}

func disableLegacySupervisor(ctx context.Context, configs []legacySupervisorProgram) ([]string, error) {
	disabled := []string{}
	for _, config := range configs {
		if !pathExists(config.Path) {
			continue
		}
		target := config.Path + legacyDisabledSuffix
		if pathExists(target) {
			return disabled, fmt.Errorf("legacy Supervisor backup already exists for %s", filepath.Base(config.Path))
		}
		if err := os.Rename(config.Path, target); err != nil {
			return disabled, err
		}
		disabled = append(disabled, target)
	}
	if len(disabled) > 0 {
		if err := runFixedCommand(ctx, "/usr/bin/supervisorctl", []string{"reread"}); err != nil {
			return disabled, err
		}
		if err := runFixedCommand(ctx, "/usr/bin/supervisorctl", []string{"update"}); err != nil {
			return disabled, err
		}
	}
	return disabled, nil
}

func disableLegacyCron() ([]byte, os.FileMode, bool, error) {
	path := "/etc/crontab"
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, false, errors.New("system crontab is unsafe")
	}
	contents, err := os.ReadFile(path)
	if err != nil || len(contents) > 1024*1024 {
		return nil, 0, false, errors.New("system crontab is unavailable")
	}
	lines := strings.Split(string(contents), "\n")
	filtered := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		if strings.Contains(line, legacyCorePath+"/artisan") && strings.Contains(line, "schedule:run") {
			changed = true
			continue
		}
		filtered = append(filtered, line)
	}
	if !changed {
		return contents, info.Mode().Perm(), false, nil
	}
	updated := strings.Join(filtered, "\n")
	if err := atomicWriteBytes(path, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, 0, false, err
	}
	return contents, info.Mode().Perm(), true, nil
}

func isLocalDatabaseHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func readBoundedRegularFile(path string, maximum int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, errors.New("file is unsafe or too large")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("file is too large")
	}
	return contents, nil
}

// readLegacySecret reads a secret from a legacy .env, either inline or from
// the file its *_FILE key names. A relative file is resolved against the
// checkout, which was the legacy process's working directory.
func readLegacySecret(environmentPath, checkout, key string) (string, error) {
	value, err := readEnvValue(environmentPath, key+"_FILE")
	if err != nil {
		return "", err
	}
	if value = strings.TrimSpace(value); value != "" {
		if !filepath.IsAbs(value) {
			value = filepath.Join(checkout, value)
		}
		contents, readErr := readBoundedRegularFile(value, 64*1024)
		if readErr != nil {
			return "", fmt.Errorf("legacy %s file is unreadable", key)
		}
		return strings.TrimSpace(string(contents)), nil
	}
	value, err = readEnvValue(environmentPath, key)
	return strings.TrimSpace(value), err
}

// legacySubsRedisStream returns the legacy Subs Redis Stream delivery, or nil
// when Subs delivers to Core over HTTP.
func legacySubsRedisStream(environmentPath, checkout string) (*subsRedisStream, error) {
	driver, err := readEnvValue(environmentPath, "DELIVERY_DRIVER")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("legacy Subs environment file is unreadable")
	}
	switch SubsDelivery(strings.ToLower(strings.TrimSpace(driver))) {
	case "", SubsDeliveryHTTP:
		return nil, nil
	case SubsDeliveryRedisStream:
	default:
		return nil, errors.New("legacy Subs DELIVERY_DRIVER must be http or redis-stream")
	}
	stream := subsRedisStream{}
	stream.URL, _ = readEnvValue(environmentPath, "SUBS_REDIS_URL")
	stream.Stream, _ = readEnvValue(environmentPath, "SUBS_REDIS_STREAM")
	if stream.Stream = strings.TrimSpace(stream.Stream); stream.Stream == "" {
		stream.Stream = defaultSubsRedisStream
	}
	stream.URL = strings.TrimSpace(stream.URL)
	stream.HMACSecret, err = readLegacySecret(environmentPath, checkout, "SUBS_REDIS_HMAC_SECRET")
	if err != nil {
		return nil, err
	}
	if err := stream.validate(); err != nil {
		return nil, fmt.Errorf("legacy Subs Redis Stream delivery: %w", err)
	}
	return &stream, nil
}

// checkCoreSubsStream confirms the adopted Core can verify and read what Subs
// will publish. Core keeps its own copy of these settings in its .env.
func checkCoreSubsStream(coreEnvironment string, stream subsRedisStream) error {
	secret, err := readEnvValue(coreEnvironment, "SUBS_REDIS_HMAC_SECRET")
	if err != nil || strings.TrimSpace(secret) == "" {
		return errors.New("legacy Subs uses Redis Stream delivery but legacy Core has no SUBS_REDIS_HMAC_SECRET")
	}
	if strings.TrimSpace(secret) != stream.HMACSecret {
		return errors.New("legacy Core and Subs SUBS_REDIS_HMAC_SECRET values differ")
	}
	coreStream, _ := readEnvValue(coreEnvironment, "SUBS_REDIS_STREAM")
	if coreStream = strings.TrimSpace(coreStream); coreStream == "" {
		coreStream = defaultSubsRedisStream
	}
	if coreStream != stream.Stream {
		return errors.New("legacy Core and Subs SUBS_REDIS_STREAM values differ")
	}
	return nil
}

// legacyDiscordConfiguration is the managed Discord configuration built from
// a legacy bot's .env. Its relay identity is kept exactly; a new one would no
// longer match the identity Core already trusts.
type legacyDiscordConfiguration struct {
	environment map[string]string
	credentials []legacyDiscordCredential
}

type legacyDiscordCredential struct {
	key, name, value string
}

// legacyDiscordSettings are copied unchanged when the legacy .env sets them.
var legacyDiscordSettings = []string{
	"BOT_DEPLOYMENT_MODE",
	"DISCORD_CLIENT_ID",
	"DISCORD_GUILD_ID",
	"DISCORD_GUILD_MEMBERS_INTENT",
	"DISCORD_MESSAGE_CONTENT_INTENT",
	"DISCORD_CONNECTIONS_FILE",
	"DISCORD_CONNECTIONS_JSON",
	"DISCORD_CONNECTION_REFRESH_MS",
	"DISCORD_SCHEDULER_QUANTUM",
	"DISCORD_SHARD_COUNT",
	"NEXUS_API_URL",
	"NEXUS_DISCORD_CONNECTION_ID",
	"NEXUS_DISCORD_CONNECTION_GENERATION",
	"NEXUS_DISCORD_RELAY_PROTOCOL",
	"NEXUS_DISCORD_RELAY_KEY_ID",
	"NEXUS_DISCORD_RELAY_CURRENT_KEY_ID",
	"NEXUS_DISCORD_RELAY_NEXT_KEY_ID",
	"NEXUS_DISCORD_CAPABILITIES_JSON",
	"LOG_LEVEL",
	"PROCESS_HEALTH_INTERVAL_MS",
	"PROCESS_HEALTH_STALE_AFTER_MS",
}

func readLegacyDiscordConfiguration(environmentPath, checkout string) (legacyDiscordConfiguration, error) {
	configuration := legacyDiscordConfiguration{environment: map[string]string{}}
	for _, key := range legacyDiscordSettings {
		value, err := readEnvValue(environmentPath, key)
		if err != nil {
			return configuration, errors.New("legacy Discord environment file is unreadable")
		}
		if value = strings.TrimSpace(value); value != "" {
			configuration.environment[key] = value
		}
	}
	if configuration.environment["BOT_DEPLOYMENT_MODE"] == "" {
		mode, _ := readEnvValue(environmentPath, "DISCORD_DEPLOYMENT_MODE")
		if mode = strings.TrimSpace(mode); mode == "" {
			mode = "dedicated"
		}
		configuration.environment["BOT_DEPLOYMENT_MODE"] = mode
	}
	if file := configuration.environment["DISCORD_CONNECTIONS_FILE"]; file != "" && !filepath.IsAbs(file) {
		configuration.environment["DISCORD_CONNECTIONS_FILE"] = filepath.Join(checkout, file)
	}
	for _, credential := range []struct {
		key, name string
		required  bool
	}{
		{"DISCORD_BOT_TOKEN", "bot-token", true},
		{"NEXUS_API_KEY", "nexus-api-token", true},
		{"NEXUS_DISCORD_RELAY_PRIVATE_KEY", "relay-private-key", true},
		{"NEXUS_DISCORD_RELAY_NEXT_PRIVATE_KEY", "relay-next-private-key", false},
	} {
		value, err := readLegacySecret(environmentPath, checkout, credential.key)
		if err != nil {
			return configuration, err
		}
		if value == "" {
			if credential.required {
				return configuration, fmt.Errorf("legacy Discord %s is not configured", credential.key)
			}
			continue
		}
		configuration.credentials = append(configuration.credentials, legacyDiscordCredential{key: credential.key, name: credential.name, value: value})
	}
	if configuration.environment["NEXUS_API_URL"] == "" {
		return configuration, errors.New("legacy Discord NEXUS_API_URL is not configured")
	}
	if err := validateInstallConfiguration(ComponentDiscord, map[string]string{
		"bot_token": configuration.credentials[0].value,
		"client_id": configuration.environment["DISCORD_CLIENT_ID"],
		"guild_id":  configuration.environment["DISCORD_GUILD_ID"],
	}); err != nil {
		return configuration, fmt.Errorf("legacy %w", err)
	}
	return configuration, nil
}

// writeLegacyDiscordConfiguration writes the managed Discord environment and
// credential files. Unlike a new install it leaves Core's DISCORD_* settings
// alone, because the adopted bot keeps the relay identity Core expects.
func (engine DeploymentEngine) writeLegacyDiscordConfiguration(release string, configuration legacyDiscordConfiguration) error {
	componentConfig := filepath.Join(engine.Config.Paths.NexusConfigDir, string(ComponentDiscord))
	if err := os.MkdirAll(componentConfig, 0750); err != nil {
		return err
	}
	dataDirectory := filepath.Join(engine.Config.Paths.DataDir, string(ComponentDiscord))
	if err := os.MkdirAll(dataDirectory, 0750); err != nil {
		return err
	}
	if err := chownPathToGroup(dataDirectory, string(ComponentDiscord), 0770); err != nil {
		return err
	}
	environment := map[string]string{}
	for key, value := range configuration.environment {
		environment[key] = value
	}
	for _, credential := range configuration.credentials {
		path, err := engine.writeCredential(ComponentDiscord, credential.name, credential.value)
		if err != nil {
			return err
		}
		environment[credential.key+"_FILE"] = path
	}
	environment["NODE_ENV"] = "production"
	environment["NEXUS_RELEASE_ID"] = release
	environment["BUILD_COMMIT"] = release
	environment["PROCESS_HEALTH_FILE"] = filepath.Join(dataDirectory, "process-health.json")
	environmentPath := filepath.Join(componentConfig, "environment")
	if err := writeEnvironmentFile(environmentPath, environment); err != nil {
		return err
	}
	return chownPathToGroup(environmentPath, string(ComponentDiscord), 0640)
}

// legacyDiscordUnitInstalled reports whether the Discord unit path holds a
// legacy bot's unit rather than the managed one. The managed unit is always a
// regular file that runs the managed release.
func legacyDiscordUnitInstalled(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return true
	}
	contents, err := readBoundedRegularFile(path, 1024*1024)
	return err != nil || !bytes.Contains(contents, []byte("/opt/nexus/nexus-discord/current"))
}

// legacyDiscordService stops a legacy bot that shares the managed unit name
// and moves its unit aside, remembering enough to put it back.
type legacyDiscordService struct {
	stopped     bool
	wasActive   bool
	wasEnabled  bool
	unitMoved   bool
	unitWritten bool
}

func (service *legacyDiscordService) replace(ctx context.Context, systemctl string) error {
	unit := filepath.Base(discordUnitPath)
	service.wasActive = runFixedCommand(ctx, systemctl, []string{"is-active", "--quiet", unit}) == nil
	service.wasEnabled = runFixedCommand(ctx, systemctl, []string{"is-enabled", "--quiet", unit}) == nil
	service.stopped = true
	if service.wasActive {
		if err := runFixedCommand(ctx, systemctl, []string{"stop", unit}); err != nil {
			return err
		}
	}
	if service.wasEnabled {
		if err := runFixedCommand(ctx, systemctl, []string{"disable", unit}); err != nil {
			return err
		}
	}
	if legacyDiscordUnitInstalled(discordUnitPath) {
		backup := discordUnitPath + legacyDisabledSuffix
		if pathExists(backup) {
			return errors.New("a legacy Discord unit backup already exists")
		}
		if err := os.Rename(discordUnitPath, backup); err != nil {
			return err
		}
		service.unitMoved = true
	}
	if err := atomicWriteBytes(discordUnitPath, []byte(systemdUnits()[discordUnitPath]), 0644); err != nil {
		return err
	}
	service.unitWritten = true
	return runFixedCommand(ctx, systemctl, []string{"daemon-reload"})
}

func (service *legacyDiscordService) restore(ctx context.Context, systemctl string) {
	if !service.stopped {
		return
	}
	unit := filepath.Base(discordUnitPath)
	if service.unitMoved {
		_ = os.Rename(discordUnitPath+legacyDisabledSuffix, discordUnitPath)
	}
	if service.unitWritten || service.unitMoved {
		_ = runFixedCommand(ctx, systemctl, []string{"daemon-reload"})
	}
	if service.wasEnabled {
		_ = runFixedCommand(ctx, systemctl, []string{"enable", unit})
	}
	if service.wasActive {
		_ = runFixedCommand(ctx, systemctl, []string{"start", unit})
	}
}

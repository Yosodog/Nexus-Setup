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
	legacyCorePath = "/var/www/nexus"
	legacySubsPath = "/var/nexus-subs"
)

var legacyPHPSocketPattern = regexp.MustCompile(`unix:/run/php/php[0-9]+\.[0-9]+-fpm\.sock`)

type LegacyAssessment struct {
	Detected   bool
	Profile    Profile
	Release    string
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
	if !coreExists && !subsExists {
		return LegacyAssessment{}, nil
	}
	if coreExists && subsExists && coreRelease != subsRelease {
		return LegacyAssessment{Detected: true}, errors.New("legacy Core and Subs are not on the same exact release tag")
	}

	assessment := LegacyAssessment{Detected: true}
	if coreExists {
		assessment.Release = coreRelease
		assessment.Components = append(assessment.Components, ComponentCore)
	}
	if subsExists {
		assessment.Release = subsRelease
		assessment.Components = append(assessment.Components, ComponentSubs)
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
	if repository == coreRepository {
		environment, statErr := os.Lstat(filepath.Join(path, ".env"))
		if statErr != nil || !environment.Mode().IsRegular() || environment.Mode()&os.ModeSymlink != 0 {
			return false, "", errors.New("legacy Core environment file is unavailable")
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
	if directory != legacyCorePath && directory != legacySubsPath {
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
	platform, err := DetectPlatform()
	if err != nil {
		return err
	}
	if err := provisionPackages(ctx, platform, assessment.Profile); err != nil {
		return err
	}
	releases := NewGitHubReleaseClient()
	available, err := releases.StableReleases(ctx)
	if err != nil || !releaseExists(available, assessment.Release) {
		return errors.New("the matching canonical Nexus-Setup release is unavailable")
	}
	engine := DeploymentEngine{Config: config, Releases: releases, Executor: OSCommandExecutor{}}
	if err := assertLegacyAdoptionTargetsAvailable(config.Paths, assessment.Components); err != nil {
		return err
	}
	for _, component := range assessment.Components {
		if err := engine.stageComponent(ctx, "adoption", component, assessment.Release); err != nil {
			return err
		}
	}

	activatedUnits := []string{}
	disabledSupervisorConfigs := []string{}
	legacyStopped := false
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
			_ = os.Rename(disabled, strings.TrimSuffix(disabled, ".nexus-disabled"))
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
			startLegacySupervisor(context.Background())
		}
		if legacyMaintenance {
			_ = runLegacyArtisan(context.Background(), "up")
		}
		_ = os.Remove(filepath.Join(config.Paths.NexusConfigDir, string(ComponentCore), ".env"))
		_ = os.Remove(filepath.Join(config.Paths.NexusConfigDir, string(ComponentSubs), "environment"))
		_ = os.RemoveAll(filepath.Join(config.Paths.CredentialDir, string(ComponentSubs)))
		_ = os.RemoveAll(filepath.Join(config.Paths.DataDir, string(ComponentCore), "storage"))
	}()

	if containsComponent(assessment.Components, ComponentCore) {
		if err := runLegacyArtisan(ctx, "down", "--retry=60"); err != nil {
			return fmt.Errorf("put legacy Core in maintenance mode: %w", err)
		}
		legacyMaintenance = true
	}
	stopLegacySupervisor(ctx)
	legacyStopped = true

	if containsComponent(assessment.Components, ComponentCore) {
		if err := copyLegacyCoreState(config.Paths); err != nil {
			return err
		}
		if err := engine.prepareCoreRelease(assessment.Release); err != nil {
			return err
		}
		if err := configurePHPFPM(ctx, config.Paths); err != nil {
			return err
		}
		if err := engine.runArtisan(ctx, assessment.Release, "migrate", "--force"); err != nil {
			return fmt.Errorf("legacy database migration failed: %w", err)
		}
		if err := engine.runArtisan(ctx, assessment.Release, "up"); err != nil {
			return fmt.Errorf("leave managed Core maintenance mode: %w", err)
		}
	}
	if containsComponent(assessment.Components, ComponentSubs) {
		if assessment.Profile == ProfileSubsOnly {
			options, optionsErr := legacySubsOptions()
			if optionsErr != nil {
				return optionsErr
			}
			if err := engine.writeStandaloneSubsConfiguration(assessment.Release, options); err != nil {
				return err
			}
		} else if err := engine.writeComponentConfiguration(ComponentSubs, assessment.Release, nil); err != nil {
			return err
		}
	}
	for _, component := range assessment.Components {
		if err := atomicSwitchRelease(config.Paths, component, assessment.Release); err != nil {
			return err
		}
	}

	for _, component := range assessment.Components {
		unit, unitErr := componentServiceUnit(component)
		if unitErr != nil {
			return unitErr
		}
		if err := runFixedCommand(ctx, config.SystemctlBinary, []string{"enable", "--now", unit}); err != nil {
			return err
		}
		activatedUnits = append(activatedUnits, unit)
	}
	if containsComponent(assessment.Components, ComponentCore) {
		if err := runFixedCommand(ctx, config.SystemctlBinary, []string{"enable", "--now", "nexus-scheduler.timer"}); err != nil {
			return err
		}
		activatedUnits = append(activatedUnits, "nexus-scheduler.timer")
	}
	if err := engine.healthCheck(ctx, assessment.Components, assessment.Release); err != nil {
		return err
	}

	if containsComponent(assessment.Components, ComponentCore) {
		nginxBackup, nginxMode, err = switchLegacyNginx(ctx, config)
		if err != nil {
			return err
		}
		nginxChanged = true
	}
	disabledSupervisorConfigs, err = disableLegacySupervisor(ctx)
	if err != nil {
		return err
	}
	crontabBackup, crontabMode, crontabChanged, err = disableLegacyCron()
	if err != nil {
		return err
	}

	state := InstallationState{Version: 1, Profile: assessment.Profile, CurrentRelease: assessment.Release, Components: map[Component]InstalledComponent{}}
	for _, component := range []Component{ComponentCore, ComponentSubs, ComponentDiscord} {
		installed := containsComponent(assessment.Components, component)
		state.Components[component] = InstalledComponent{Installed: installed, Enabled: installed, CurrentRelease: conditionalRelease(installed, assessment.Release)}
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
	fmt.Printf("Legacy Nexus %s was adopted successfully. The original checkouts were preserved.\n", assessment.Release)
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

func releaseExists(releases []ReleaseInfo, release string) bool {
	for _, candidate := range releases {
		if candidate.Tag == release {
			return true
		}
	}
	return false
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

func stopLegacySupervisor(ctx context.Context) {
	if !pathExists("/usr/bin/supervisorctl") {
		return
	}
	for _, program := range []string{"nexus-worker:*", "nexus-worker-sync:*", "nexus-pulse-check:*", "nexus-pulse-work:*", "nexus-subs-stream", "nexus-subs:*"} {
		_ = runFixedCommand(ctx, "/usr/bin/supervisorctl", []string{"stop", program})
	}
}

func startLegacySupervisor(ctx context.Context) {
	if !pathExists("/usr/bin/supervisorctl") {
		return
	}
	for _, program := range []string{"nexus-worker:*", "nexus-worker-sync:*", "nexus-pulse-check:*", "nexus-pulse-work:*", "nexus-subs-stream", "nexus-subs:*"} {
		_ = runFixedCommand(ctx, "/usr/bin/supervisorctl", []string{"start", program})
	}
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

func disableLegacySupervisor(ctx context.Context) ([]string, error) {
	paths := []string{
		"/etc/supervisor/conf.d/nexus-worker.conf",
		"/etc/supervisor/conf.d/nexus-worker-sync.conf",
		"/etc/supervisor/conf.d/nexus-pulse-check.conf",
		"/etc/supervisor/conf.d/nexus-pulse-work.conf",
		"/etc/supervisor/conf.d/nexus-subs-stream.conf",
		"/etc/supervisor/conf.d/nexus-subs.conf",
	}
	disabled := []string{}
	for _, path := range paths {
		if !pathExists(path) {
			continue
		}
		target := path + ".nexus-disabled"
		if pathExists(target) {
			return disabled, fmt.Errorf("legacy Supervisor backup already exists for %s", filepath.Base(path))
		}
		if err := os.Rename(path, target); err != nil {
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

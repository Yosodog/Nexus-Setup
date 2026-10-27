package updater

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
)

const (
	updaterTmpfilesPath = "/etc/tmpfiles.d/nexus-updater.conf"
	updaterSocketUnit   = "nexus-updater.socket"
	updaterServiceUnit  = "nexus-updater.service"
	updaterWorkerUnit   = "nexus-updater-worker@.service"
	systemdUnitDir      = "/etc/systemd/system"
)

// updaterUnits are the units an updater self-update cannot rewrite, because
// its worker cannot write to /etc/systemd/system. Doctor repairs them instead.
var updaterUnits = []string{updaterSocketUnit, updaterServiceUnit, updaterWorkerUnit}

// HostDoctor checks the host setup the coordinator needs before it can answer
// a request. The CLI runs it directly, so it still reports when a missing
// runtime directory keeps the coordinator from starting and the control socket
// refuses connections.
type HostDoctor struct {
	Paths           Paths
	TmpfilesPath    string
	UnitDir         string
	SystemctlBinary string
	TmpfilesBinary  string
	Executor        CommandExecutor
}

// NewHostDoctor returns a HostDoctor for a managed Linux installation.
func NewHostDoctor() HostDoctor {
	return HostDoctor{
		Paths:           DefaultPaths(),
		TmpfilesPath:    updaterTmpfilesPath,
		UnitDir:         systemdUnitDir,
		SystemctlBinary: "/usr/bin/systemctl",
		TmpfilesBinary:  "/usr/bin/systemd-tmpfiles",
		Executor:        OSCommandExecutor{},
	}
}

// Checks reports the boot-time directories and systemd units the coordinator
// depends on. It only reads, so it works without root.
func (doctor HostDoctor) Checks(ctx context.Context) []DoctorCheck {
	checks := []DoctorCheck{}
	content, err := os.ReadFile(doctor.TmpfilesPath)
	tmpfilesCurrent := err == nil && bytes.Equal(content, []byte(updaterTmpfiles()))
	message := "Runtime directories are recreated at boot."
	if !tmpfilesCurrent {
		message = doctor.TmpfilesPath + " does not recreate every runtime directory at boot."
	}
	checks = append(checks, DoctorCheck{Name: "tmpfiles_config", Passed: tmpfilesCurrent, Message: message})
	unitsCurrent := true
	message = "The updater's systemd units are current."
	for _, unit := range updaterUnits {
		content, err := os.ReadFile(filepath.Join(doctor.UnitDir, unit))
		if err != nil || !bytes.Equal(content, []byte(systemdUnits()[filepath.Join(systemdUnitDir, unit)])) {
			unitsCurrent = false
			message = filepath.Join(doctor.UnitDir, unit) + " is not the unit this updater installs."
			break
		}
	}
	checks = append(checks, DoctorCheck{Name: "updater_units", Passed: unitsCurrent, Message: message})
	for _, directory := range []struct{ name, path string }{
		{"runtime_directory", doctor.Paths.RuntimeDir},
		{"locks_directory", doctor.Paths.LocksDir},
	} {
		info, err := os.Stat(directory.path)
		message := directory.path
		if err != nil || !info.IsDir() {
			message = directory.path + " is missing, so the coordinator cannot start."
		}
		checks = append(checks, DoctorCheck{Name: directory.name, Passed: err == nil && info.IsDir(), Message: message})
	}
	socketActive := doctor.Executor.Run(ctx, doctor.SystemctlBinary, []string{"is-active", "--quiet", updaterSocketUnit}) == nil
	message = "The control socket is listening."
	if !socketActive {
		message = updaterSocketUnit + " is not active."
	}
	checks = append(checks, DoctorCheck{Name: "control_socket", Passed: socketActive, Message: message})
	coordinatorFailed := doctor.Executor.Run(ctx, doctor.SystemctlBinary, []string{"is-failed", "--quiet", updaterServiceUnit}) == nil
	message = "The coordinator has not failed."
	if coordinatorFailed {
		message = updaterServiceUnit + " failed to start."
	}
	checks = append(checks, DoctorCheck{Name: "coordinator", Passed: !coordinatorFailed, Message: message})
	return checks
}

// Repair rewrites the updater's units and tmpfiles configuration, recreates
// the runtime directories, and restarts the control socket after systemd gave
// up on it. The caller must run as root.
func (doctor HostDoctor) Repair(ctx context.Context) error {
	for _, unit := range updaterUnits {
		content := systemdUnits()[filepath.Join(systemdUnitDir, unit)]
		if err := atomicWriteBytes(filepath.Join(doctor.UnitDir, unit), []byte(content), 0644); err != nil {
			return err
		}
	}
	if err := doctor.Executor.Run(ctx, doctor.SystemctlBinary, []string{"daemon-reload"}); err != nil {
		return err
	}
	if err := atomicWriteBytes(doctor.TmpfilesPath, []byte(updaterTmpfiles()), 0644); err != nil {
		return err
	}
	if err := doctor.Executor.Run(ctx, doctor.TmpfilesBinary, []string{"--create", doctor.TmpfilesPath}); err != nil {
		return err
	}
	if err := doctor.Executor.Run(ctx, doctor.SystemctlBinary, []string{"reset-failed", updaterServiceUnit, updaterSocketUnit}); err != nil {
		return err
	}
	return doctor.Executor.Run(ctx, doctor.SystemctlBinary, []string{"start", updaterSocketUnit})
}

// updaterTmpfiles recreates the coordinator's runtime and lock directories,
// which live on tmpfs and disappear at every reboot. The coordinator's mount
// namespace cannot be set up while either is missing.
func updaterTmpfiles() string {
	return `d /run/nexus-updater 0750 root nexus-updater-control -
d /run/lock/nexus-updater 0700 root root -
`
}

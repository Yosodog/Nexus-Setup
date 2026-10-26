package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type unitStateExecutor struct {
	recordingExecutor
	socketActive      bool
	coordinatorFailed bool
}

func (executor *unitStateExecutor) Run(ctx context.Context, binary string, args []string) error {
	_ = executor.recordingExecutor.Run(ctx, binary, args)
	switch {
	case reflect.DeepEqual(args, []string{"is-active", "--quiet", updaterSocketUnit}) && !executor.socketActive:
		return errors.New("inactive")
	case reflect.DeepEqual(args, []string{"is-failed", "--quiet", updaterServiceUnit}) && !executor.coordinatorFailed:
		return errors.New("not failed")
	}
	return nil
}

func testHostDoctor(t *testing.T, executor CommandExecutor) HostDoctor {
	t.Helper()
	root := t.TempDir()
	return HostDoctor{
		Paths:           NewTestPaths(root),
		TmpfilesPath:    filepath.Join(root, "etc", "tmpfiles.d", "nexus-updater.conf"),
		SystemctlBinary: "/usr/bin/systemctl",
		TmpfilesBinary:  "/usr/bin/systemd-tmpfiles",
		Executor:        executor,
	}
}

func doctorCheckResults(checks []DoctorCheck) map[string]bool {
	results := map[string]bool{}
	for _, check := range checks {
		results[check.Name] = check.Passed
	}
	return results
}

func TestUpdaterTmpfilesRecreatesLockDirectoryAtBoot(t *testing.T) {
	content := systemdUnits()[updaterTmpfilesPath]
	for _, line := range []string{"d /run/nexus-updater 0750 root nexus-updater-control -\n", "d /run/lock/nexus-updater 0700 root root -\n"} {
		if !strings.Contains(content, line) {
			t.Fatalf("tmpfiles configuration does not contain %q:\n%s", line, content)
		}
	}
	mirror, err := os.ReadFile(filepath.Join("..", "..", "systemd", "nexus-updater.tmpfiles"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mirror) != content {
		t.Fatalf("systemd/nexus-updater.tmpfiles does not match the embedded configuration:\n%s", mirror)
	}
}

func TestHostDoctorReportsLockDirectoryLostAtReboot(t *testing.T) {
	executor := &unitStateExecutor{coordinatorFailed: true}
	doctor := testHostDoctor(t, executor)
	writeTestFile(t, doctor.TmpfilesPath, "d /run/nexus-updater 0750 root nexus-updater-control -\n")
	if err := os.MkdirAll(doctor.Paths.RuntimeDir, 0750); err != nil {
		t.Fatal(err)
	}
	results := doctorCheckResults(doctor.Checks(context.Background()))
	expected := map[string]bool{"tmpfiles_config": false, "runtime_directory": true, "locks_directory": false, "control_socket": false, "coordinator": false}
	if !reflect.DeepEqual(results, expected) {
		t.Fatalf("unexpected checks: %#v", results)
	}
}

func TestHostDoctorPassesOnConfiguredHost(t *testing.T) {
	doctor := testHostDoctor(t, &unitStateExecutor{socketActive: true})
	writeTestFile(t, doctor.TmpfilesPath, updaterTmpfiles())
	for _, directory := range []string{doctor.Paths.RuntimeDir, doctor.Paths.LocksDir} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range doctor.Checks(context.Background()) {
		if !check.Passed {
			t.Fatalf("%s failed on a configured host: %s", check.Name, check.Message)
		}
	}
}

func TestHostDoctorReportsMissingTmpfilesConfiguration(t *testing.T) {
	doctor := testHostDoctor(t, &unitStateExecutor{socketActive: true})
	if results := doctorCheckResults(doctor.Checks(context.Background())); results["tmpfiles_config"] {
		t.Fatal("a missing tmpfiles configuration passed")
	}
}

func TestHostDoctorRepairRestoresDirectoriesAndSocket(t *testing.T) {
	executor := &recordingExecutor{}
	doctor := testHostDoctor(t, executor)
	writeTestFile(t, doctor.TmpfilesPath, "d /run/nexus-updater 0750 root nexus-updater-control -\n")
	if err := doctor.Repair(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(doctor.TmpfilesPath)
	if err != nil || string(content) != updaterTmpfiles() {
		t.Fatalf("tmpfiles configuration was not rewritten: %q %v", content, err)
	}
	expected := [][]string{
		{"/usr/bin/systemd-tmpfiles", "--create", doctor.TmpfilesPath},
		{"/usr/bin/systemctl", "reset-failed", updaterServiceUnit, updaterSocketUnit},
		{"/usr/bin/systemctl", "start", updaterSocketUnit},
	}
	if !reflect.DeepEqual(executor.calls, expected) {
		t.Fatalf("unexpected repair commands: %#v", executor.calls)
	}
}

func TestHostDoctorRepairStopsWhenDirectoriesCannotBeCreated(t *testing.T) {
	executor := &failingUnitExecutor{action: "--create"}
	doctor := testHostDoctor(t, executor)
	executor.unit = doctor.TmpfilesPath
	if err := doctor.Repair(context.Background()); err == nil {
		t.Fatal("repair succeeded although systemd-tmpfiles failed")
	}
	if len(executor.calls) != 1 {
		t.Fatalf("repair restarted the socket after systemd-tmpfiles failed: %#v", executor.calls)
	}
}

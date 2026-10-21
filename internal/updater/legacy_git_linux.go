//go:build linux

package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// prepareLegacyGitCommand makes Git run as the owner of the legacy checkout.
// Git refuses a repository owned by another user, and allowing it with
// safe.directory would let the checkout's own config (clean filters, fsmonitor)
// run commands as root. Running as the owner gives that config no more
// privilege than the owner already has.
func prepareLegacyGitCommand(command *exec.Cmd, directory string) error {
	owner, err := legacyCheckoutOwner(directory)
	if err != nil {
		return err
	}
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if owner != 0 {
		credential, err := legacyAccountCredential(owner)
		if err != nil {
			return fmt.Errorf("its owner uid %d: %w", owner, err)
		}
		attributes.Credential = credential
	}
	command.SysProcAttr = attributes
	// Kill the whole process group so a filter Git started cannot outlive it.
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

// legacyCheckoutOwner returns the uid that owns both the checkout and its
// .git directory. Git checks both, so a checkout whose parts have different
// owners cannot be inspected as either of them.
func legacyCheckoutOwner(directory string) (uint32, error) {
	var owner uint32
	for index, path := range []string{directory, filepath.Join(directory, ".git")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			return 0, errors.New("it or its .git entry is not a directory")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, errors.New("its owner could not be read")
		}
		if index == 0 {
			owner = stat.Uid
		} else if stat.Uid != owner {
			return 0, errors.New("it and its .git directory have different owners")
		}
	}
	return owner, nil
}

// legacyAccountCredential returns the account's primary and supplementary
// groups, matching what runuser would give it, without root's groups.
func legacyAccountCredential(uid uint32) (*syscall.Credential, error) {
	account, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil, errors.New("has no local account")
	}
	gid, err := parseIdentity(account.Gid)
	if err != nil {
		return nil, err
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return nil, errors.New("has groups that could not be read")
	}
	groups := []uint32{}
	for _, groupID := range groupIDs {
		group, err := parseIdentity(groupID)
		if err != nil {
			return nil, err
		}
		groups = append(groups, uint32(group))
	}
	return &syscall.Credential{Uid: uid, Gid: uint32(gid), Groups: groups}, nil
}

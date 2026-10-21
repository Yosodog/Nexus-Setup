//go:build !linux

package updater

import (
	"errors"
	"os/exec"
)

// The production updater is Linux-only. Other platforms fail closed rather
// than running Git in a legacy checkout without dropping to its owner.
func prepareLegacyGitCommand(*exec.Cmd, string) error {
	return errors.New("legacy checkouts can only be inspected on Linux")
}

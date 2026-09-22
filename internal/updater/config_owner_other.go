//go:build !linux

package updater

import "os"

// The production updater is Linux-only. Other platforms intentionally fail
// closed rather than trusting a config file without a reliable owner check.
func rootOwned(os.FileInfo) bool {
	return false
}

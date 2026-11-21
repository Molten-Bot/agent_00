//go:build !windows

package localruntime

import (
	"errors"
	"syscall"
)

// IsWorkspaceDiskFull reports whether workspace allocation ran out of space.
func IsWorkspaceDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

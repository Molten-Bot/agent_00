package localruntime

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// IsWorkspaceDiskFull reports whether workspace allocation ran out of space.
func IsWorkspaceDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, windows.ERROR_DISK_FULL) ||
		errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

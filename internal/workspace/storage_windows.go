package workspace

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// IsDiskFull reports whether an operation ran out of local storage.
func IsDiskFull(err error) bool {
	return errors.Is(err, ErrDiskFull) || errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, windows.ERROR_DISK_FULL) ||
		errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

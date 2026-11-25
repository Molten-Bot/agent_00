//go:build !windows

package workspace

import (
	"errors"
	"syscall"
)

// IsDiskFull reports whether an operation ran out of local storage.
func IsDiskFull(err error) bool {
	return errors.Is(err, ErrDiskFull) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

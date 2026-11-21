//go:build !windows

package localruntime

import (
	"errors"
	"syscall"
)

func isWorkspaceDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

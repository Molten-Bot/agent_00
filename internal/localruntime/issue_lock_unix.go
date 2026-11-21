//go:build !windows

package localruntime

import (
	"errors"
	"os"
	"syscall"
)

func tryIssueLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	// Signals can interrupt flock even when no worker owns the lock. Retry
	// interruptions rather than reporting a busy worker and skipping recovery
	// or retention. LOCK_NB still prevents waiting for a real lock owner.
	for errors.Is(err, syscall.EINTR) {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func processGroupAlive(pid int) (bool, error) {
	if pid <= 0 {
		return true, nil
	}
	err := syscall.Kill(-pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return true, err
}

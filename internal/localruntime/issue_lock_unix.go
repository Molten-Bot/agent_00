//go:build !windows

package localruntime

import (
	"errors"
	"os"
	"syscall"
)

func tryIssueLock(file *os.File) (bool, error) {
	err := flockIssue(file, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockIssueLock(file *os.File) error {
	return flockIssue(file, syscall.LOCK_UN)
}

func flockIssue(file *os.File, operation int) error {
	err := syscall.Flock(int(file.Fd()), operation)
	// Retry signal interruptions. LOCK_NB still prevents acquisition from
	// waiting on a real lock owner.
	for errors.Is(err, syscall.EINTR) {
		err = syscall.Flock(int(file.Fd()), operation)
	}
	return err
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

package agentruntime

import (
	"os"
	"syscall"
	"unsafe"
)

var codexLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func tryCodexFileLock(file *os.File) (bool, error) {
	var overlapped syscall.Overlapped
	// Exclusive, non-blocking lock on the first byte. Closing the handle unlocks.
	result, _, err := codexLockFileEx.Call(file.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result != 0 {
		return true, nil
	}
	if err == syscall.Errno(33) { // ERROR_LOCK_VIOLATION
		return false, nil
	}
	return false, err
}

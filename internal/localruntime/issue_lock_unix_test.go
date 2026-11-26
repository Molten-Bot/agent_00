//go:build !windows

package localruntime

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestIssueControlLockReleasesWithInheritedDescriptor(t *testing.T) {
	t.Parallel()
	store := &IssueClaimStore{Root: t.TempDir()}
	lock, err := store.TryControlLock("worker")
	if err != nil || lock == nil {
		t.Fatalf("acquire control lock: %v", err)
	}
	t.Cleanup(func() { lock.file.Close() })

	// A child between fork and exec still holds a reference to the lock's
	// open file description even when the descriptor is close-on-exec.
	copyFD, err := unix.FcntlInt(lock.file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(copyFD) })
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	worker, err := store.TryLock("worker")
	if err != nil || worker == nil {
		t.Fatalf("released control lock still blocks worker acquisition: %v", err)
	}
	worker.Close()
}

package workspace

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// CheckCacheMounts rejects mounts between root and path or within path.
func CheckCacheMounts(root, path string) error {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return err
	}
	mounts := make([]unix.Statfs_t, count+1)
	n, err := unix.Getfsstat(mounts, unix.MNT_NOWAIT)
	if err != nil {
		return err
	}
	if n >= len(mounts) {
		return fmt.Errorf("mount table grew during cache inspection")
	}
	for _, mount := range mounts[:n] {
		point := unix.ByteSliceToString(mount.Mntonname[:])
		if point != "" && cacheMountIntersects(root, path, point) {
			return fmt.Errorf("cache tree contains mount %s", point)
		}
	}
	return nil
}

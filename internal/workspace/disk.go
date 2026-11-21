package workspace

import (
	"io/fs"
	"os"
	"path/filepath"
)

// MinimumFreeBytes is the initial allocation allowance for an unmeasured task.
const MinimumFreeBytes = 512 * 1024 * 1024

// DiskRoots returns the workspace roots in allocation order.
func (m Manager) DiskRoots() []string {
	return m.rootCandidates()
}

type diskFileID struct{ device, inode uint64 }

// DiskUsage measures the files in a task workspace, including its Git data.
// WalkDir does not follow symlinks to files outside the workspace.
func DiskUsage(root string) (uint64, error) {
	var size uint64
	seenLinks := make(map[diskFileID]struct{})
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		bytes, id := fileDiskBytes(info)
		if id != (diskFileID{}) {
			if _, seen := seenLinks[id]; seen {
				return nil
			}
			seenLinks[id] = struct{}{}
		}
		size += bytes
		return nil
	})
	return size, err
}

// DiskSpace reports the filesystem identity, available bytes, and total capacity.
// A workspace root may not exist yet, so inspect its nearest existing parent.
func DiskSpace(path string) (device string, available, total uint64, err error) {
	path = filepath.Clean(path)
	for {
		device, available, total, err := diskSpace(path)
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return device, available, total, err
		}
		path = filepath.Dir(path)
	}
}

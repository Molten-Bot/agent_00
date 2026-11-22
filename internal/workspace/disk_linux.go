package workspace

import (
	"encoding/hex"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// BTRFS_IOC_FS_INFO is _IOR(0x94, 31, struct btrfs_ioctl_fs_info_args).
// FS_IOC_GETFLAGS supplies the architecture's _IOC_READ bits.
const btrfsIOCFsInfo = (unix.FS_IOC_GETFLAGS & 0xe0000000) | (1024 << 16) | (0x94 << 8) | 31

type btrfsFSInfo struct {
	_    [16]byte
	fsid [16]byte
	_    [992]byte
}

func diskPoolID(path string, fsType uint32) (string, error) {
	if fsType == unix.BTRFS_SUPER_MAGIC {
		// Both st_dev and statfs.f_fsid vary by Btrfs subvolume.
		return btrfsDiskPoolID(path, readBtrfsFSInfo)
	}
	return diskDeviceID(path)
}

func btrfsDiskPoolID(path string, readFSInfo func(int, *btrfsFSInfo) error) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)

	var info btrfsFSInfo
	if err := readFSInfo(fd, &info); err != nil {
		return "", fmt.Errorf("read Btrfs filesystem UUID: %w", err)
	}
	return "btrfs:" + hex.EncodeToString(info.fsid[:]), nil
}

func readBtrfsFSInfo(fd int, info *btrfsFSInfo) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), btrfsIOCFsInfo, uintptr(unsafe.Pointer(info)))
	if errno != 0 {
		return errno
	}
	return nil
}

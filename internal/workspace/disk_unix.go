//go:build linux || darwin

package workspace

import (
	"os"
	"strconv"
	"syscall"
)

func diskSpace(path string) (string, uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", 0, err
	}
	var info syscall.Stat_t
	if err := syscall.Stat(path, &info); err != nil {
		return "", 0, err
	}
	return strconv.FormatUint(uint64(info.Dev), 10), stat.Bavail * uint64(stat.Bsize), nil
}

func fileDiskBytes(info os.FileInfo) (uint64, diskFileID) {
	stat := info.Sys().(*syscall.Stat_t)
	var id diskFileID
	if stat.Nlink > 1 {
		id = diskFileID{uint64(stat.Dev), uint64(stat.Ino)}
	}
	return uint64(stat.Blocks) * 512, id
}

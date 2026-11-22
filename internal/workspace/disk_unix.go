//go:build linux || darwin

package workspace

import (
	"os"
	"strconv"
	"syscall"
)

func diskSpace(path string) (string, uint64, uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", 0, 0, err
	}
	pool, err := diskPoolID(path, uint32(stat.Type))
	if err != nil {
		return "", 0, 0, err
	}
	return pool, stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize), nil
}

func diskDeviceID(path string) (string, error) {
	var info syscall.Stat_t
	if err := syscall.Stat(path, &info); err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(info.Dev), 10), nil
}

func fileDiskBytes(info os.FileInfo) (uint64, diskFileID) {
	stat := info.Sys().(*syscall.Stat_t)
	var id diskFileID
	if stat.Nlink > 1 {
		id = diskFileID{uint64(stat.Dev), uint64(stat.Ino)}
	}
	return uint64(stat.Blocks) * 512, id
}

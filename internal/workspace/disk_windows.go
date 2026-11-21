package workspace

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func diskSpace(path string) (string, uint64, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", 0, err
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &available, nil, nil); err != nil {
		if err == windows.ERROR_PATH_NOT_FOUND || err == windows.ERROR_FILE_NOT_FOUND {
			return "", 0, os.ErrNotExist
		}
		return "", 0, err
	}
	var volumePath, volumeName [windows.MAX_PATH]uint16
	if err := windows.GetVolumePathName(pathPtr, &volumePath[0], uint32(len(volumePath))); err != nil {
		return "", 0, err
	}
	device := windows.UTF16ToString(volumePath[:])
	if windows.GetVolumeNameForVolumeMountPoint(&volumePath[0], &volumeName[0], uint32(len(volumeName))) == nil {
		device = windows.UTF16ToString(volumeName[:])
	}
	return strings.ToLower(device), available, nil
}

func fileDiskBytes(info os.FileInfo) (uint64, diskFileID) {
	return uint64(info.Size()), diskFileID{}
}

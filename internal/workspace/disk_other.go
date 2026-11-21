//go:build !linux && !darwin && !windows

package workspace

import (
	"math"
	"os"
)

func diskSpace(path string) (string, uint64, uint64, error) {
	return path, math.MaxUint64, math.MaxUint64, nil
}

func fileDiskBytes(info os.FileInfo) (uint64, diskFileID) {
	return uint64(info.Size()), diskFileID{}
}

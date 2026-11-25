package workspace

import (
	"errors"
	"strings"
)

// ErrDiskFull identifies storage exhaustion reported by a subprocess.
var ErrDiskFull = errors.New("workspace storage exhausted")

// ErrUnsuitableStorage identifies a workspace mount that cannot support the task.
var ErrUnsuitableStorage = errors.New("unsuitable workspace storage")

// DiskFullDiagnostic recognizes disk exhaustion in subprocess output and legacy
// issue records that predate the typed error.
func DiskFullDiagnostic(detail string) bool {
	detail = strings.ToLower(detail)
	for _, phrase := range []string{
		"no space left on device",
		"out of diskspace",
		"disk quota exceeded",
		"enospc",
		"workspace storage ran out of space",
		ErrDiskFull.Error(),
	} {
		if strings.Contains(detail, phrase) {
			return true
		}
	}
	return false
}

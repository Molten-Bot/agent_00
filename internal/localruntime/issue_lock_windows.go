package localruntime

import (
	"fmt"
	"os"
)

func tryIssueLock(_ *os.File) (bool, error) {
	return false, fmt.Errorf("automatic issue recovery requires inherited worker locks on Unix")
}

func unlockIssueLock(_ *os.File) error { return nil }

func processGroupAlive(_ int) (bool, error) {
	return true, fmt.Errorf("process-group liveness is unavailable")
}

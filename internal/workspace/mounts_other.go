//go:build !linux && !darwin

package workspace

import "fmt"

// CheckCacheMounts requires mount inspection before recursive cache removal.
func CheckCacheMounts(root, path string) error {
	return fmt.Errorf("cache mount inspection is unavailable on this platform")
}

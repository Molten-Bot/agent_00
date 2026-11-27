//go:build !linux

package workspace

// Other platforms retain their existing workspace selection behavior.
func checkWorkspaceSpace(string) error { return nil }

// CheckPersistentStorage detects memory-backed mounts on Linux.
func CheckPersistentStorage(string) error { return nil }

// CheckNoMountsWithin checks for nested mounts on Linux.
func CheckNoMountsWithin(string) error { return nil }

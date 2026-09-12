//go:build !linux

package workspace

// Other platforms retain their existing workspace selection behavior.
func checkWorkspaceSpace(string) error { return nil }

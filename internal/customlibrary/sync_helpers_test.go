package customlibrary

// GeneratedDir is the catalog directory produced for name once Sync has
// succeeded. Test-only accessor over generatedDir.
func (s Syncer) GeneratedDir(name string) string { return s.generatedDir(name) }

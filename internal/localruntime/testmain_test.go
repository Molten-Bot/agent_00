package localruntime

import (
	"os"
	"testing"
)

// Ambient HARNESS_COMMIT_SIGNING_* variables are optional Compose-style
// overrides (see commit_signing_env.go). Tests assert default/unconfigured
// behavior, so a deployment environment that happens to export these must
// not leak into the suite.
func TestMain(m *testing.M) {
	for _, key := range []string{
		"HARNESS_COMMIT_SIGNING_ENABLED",
		"HARNESS_COMMIT_SIGNING_PRIVATE_KEY",
		"HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE",
		"HARNESS_COMMIT_SIGNING_PASSPHRASE",
	} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

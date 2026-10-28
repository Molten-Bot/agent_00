package localruntime

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/gitsigning"
)

const commitSigningEnabledEnv = "HARNESS_COMMIT_SIGNING_ENABLED"

// An explicit environment value overrides saved settings. Empty values leave
// Settings in control, matching optional Compose environment variables.
func commitSigningEnabledFromEnv() (*bool, error) {
	raw := strings.TrimSpace(os.Getenv(commitSigningEnabledEnv))
	if raw == "" {
		return nil, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be true or false", gitsigning.ErrSetup, commitSigningEnabledEnv)
	}
	return &enabled, nil
}

// commitSigningEnvEnforced reports whether the environment pins the signing
// policy. Settings cannot change it, so messages point at the variable instead.
func commitSigningEnvEnforced() bool {
	return strings.TrimSpace(os.Getenv(commitSigningEnabledEnv)) != ""
}

// ApplyEnvironment provisions signing at startup through the same validation
// and private key store as Settings. Secrets never enter the runtime JSON.
// Enabled signing must be ready before the server starts accepting tasks.
func (s SigningSettings) ApplyEnvironment(ctx context.Context) error {
	enabled, err := commitSigningEnabledFromEnv()
	if err != nil {
		return err
	}
	raw := os.Getenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY")
	path := strings.TrimSpace(os.Getenv("HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE"))
	passphrase := os.Getenv("HARNESS_COMMIT_SIGNING_PASSPHRASE")
	if strings.TrimSpace(raw) != "" && path != "" {
		return fmt.Errorf("%w: set only one of HARNESS_COMMIT_SIGNING_PRIVATE_KEY or HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", gitsigning.ErrSetup)
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%w: cannot read HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE", gitsigning.ErrSetup)
		}
		raw = string(data)
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("%w: HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE is empty", gitsigning.ErrSetup)
		}
	}
	if enabled == nil && strings.TrimSpace(raw) == "" && passphrase == "" {
		return nil
	}
	if passphrase != "" && strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: HARNESS_COMMIT_SIGNING_PASSPHRASE requires a private key", gitsigning.ErrSetup)
	}
	cfg, err := s.config()
	if err != nil {
		return err
	}
	_, err = s.Configure(ctx, CommitSigningRequest{Enabled: cfg.Enabled, PrivateKey: raw, Passphrase: passphrase})
	return err
}

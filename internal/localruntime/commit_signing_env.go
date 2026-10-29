package localruntime

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/gitsigning"
)

// ApplyEnvironment validates saved signing settings at startup and optionally
// provisions a private key. The enabled preference is controlled by Settings.
// Secrets never enter the runtime JSON.
// Enabled signing must be ready before the server starts accepting tasks.
func (s SigningSettings) ApplyEnvironment(ctx context.Context) error {
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
	if passphrase != "" && strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: HARNESS_COMMIT_SIGNING_PASSPHRASE requires a private key", gitsigning.ErrSetup)
	}
	cfg, err := s.config()
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		if !cfg.Enabled {
			s.releaseBootHold()
			return nil
		}
		// Saved policy only: whatever is wrong is fixable in Settings → Commit signing,
		// so report a setup hold and let callers that can serve a UI start up.
		// This call revalidates the saved key, so skip a hold an earlier call
		// left behind instead of reporting it in place of today's result. The
		// hold itself stays in place until Verify below clears it, so a
		// submission that lands mid-check still meets the gate.
		if err := s.setupRequirement(false); err != nil {
			return err
		}
		// A key saved by an earlier boot still has to pass identity, signing,
		// and registration checks now. Record the failure as a startup hold so
		// SetupRequirement keeps refusing submissions; a saved key alone would
		// otherwise release the gate and sign with a key this boot rejected.
		if _, err := s.Verify(ctx); err != nil {
			return s.holdBoot(err)
		}
		return nil
	}
	// A supplied key must import, verify, and register before this process
	// accepts work. Continuing would silently sign commits with a stale saved
	// key that this boot never validated.
	_, err = s.Configure(ctx, CommitSigningRequest{Enabled: cfg.Enabled, PrivateKey: raw, Passphrase: passphrase})
	return err
}

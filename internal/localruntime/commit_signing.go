package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
)

var signingSettingsMu sync.Mutex

type CommitSigningRequest struct {
	Enabled    bool   `json:"enabled"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

type CommitSigningState struct {
	Enabled      bool   `json:"enabled"`
	Configured   bool   `json:"configured"`
	PublicKey    string `json:"public_key,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	Account      string `json:"account,omitempty"`
	Registered   bool   `json:"registered"`
	Registration string `json:"registration"`
	// SetupRequired holds tasks until a signing key exists. EnvEnforced marks
	// the policy as pinned by the environment, which Settings cannot turn off.
	SetupRequired bool   `json:"setup_required"`
	EnvEnforced   bool   `json:"env_enforced"`
	Message       string `json:"message,omitempty"`
}

// SigningSettings shares the same store and resolver across UI and CLI tasks.
// GitHub is resolved per operation so a changed authentication token is respected.
type SigningSettings struct {
	Path    string
	Initial InitConfig
	Runner  execx.Runner
	GitHub  func() gitsigning.GitHub
}

func (s SigningSettings) path() string {
	if strings.TrimSpace(s.Path) != "" {
		return s.Path
	}
	return defaultRuntimeConfigPath()
}
func (s SigningSettings) keyDir() string { return filepath.Join(filepath.Dir(s.path()), "signing") }
func (s SigningSettings) runner() execx.Runner {
	if s.Runner != nil {
		return s.Runner
	}
	return execx.OSRunner{}
}
func (s SigningSettings) github() gitsigning.GitHub {
	if s.GitHub != nil {
		return s.GitHub()
	}
	return gitsigning.GitHub{}
}

func (s SigningSettings) config() (gitsigning.Config, error) {
	cfg, err := s.savedConfig()
	if err != nil {
		return cfg, err
	}
	enabled, err := commitSigningEnabledFromEnv()
	if enabled != nil {
		cfg.Enabled = *enabled
	}
	return cfg, err
}

func (s SigningSettings) savedConfig() (gitsigning.Config, error) {
	data, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return s.Initial.CommitSigning, nil
	}
	if err != nil {
		return gitsigning.Config{}, fmt.Errorf("%w: cannot read runtime configuration", gitsigning.ErrSetup)
	}
	var doc struct {
		CommitSigning gitsigning.Config `json:"commit_signing"`
	}
	if err := json.Unmarshal(stripLineComments(data), &doc); err != nil {
		return gitsigning.Config{}, fmt.Errorf("%w: invalid runtime configuration", gitsigning.ErrSetup)
	}
	return doc.CommitSigning, nil
}

func signingState(cfg gitsigning.Config) CommitSigningState {
	registration := "not_configured"
	if cfg.KeyID != "" {
		registration = "unchecked"
	}
	return CommitSigningState{Enabled: cfg.Enabled, Configured: cfg.KeyID != "", PublicKey: cfg.PublicKey, Fingerprint: cfg.Fingerprint, Account: cfg.Account, Registration: registration}
}
func (s SigningSettings) Status(context.Context) (CommitSigningState, error) {
	cfg, err := s.config()
	return signingState(cfg), err
}

// SetupRequirement reports why commit signing cannot sign task commits yet. It
// is a local check with no GitHub calls, so callers can hold tasks and point
// the operator at Settings → Commit signing instead of failing mid-run.
// It returns nil when signing is off or a key is saved.
func (s SigningSettings) SetupRequirement() error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	if !cfg.Enabled || cfg.KeyID != "" {
		return nil
	}
	if commitSigningEnvEnforced() {
		return fmt.Errorf("%w: %s requires a signing key; save one in Settings → Commit signing before running tasks", gitsigning.ErrSetup, commitSigningEnabledEnv)
	}
	return fmt.Errorf("%w: save a signing key first", gitsigning.ErrSetup)
}

// ApplySetupRequirement annotates a state for the UI so Settings can explain
// the hold and keep the signing section in front of the operator.
func (s SigningSettings) ApplySetupRequirement(state CommitSigningState) CommitSigningState {
	state.EnvEnforced = commitSigningEnvEnforced()
	requirement := s.SetupRequirement()
	if requirement == nil {
		return state
	}
	state.SetupRequired = true
	if strings.TrimSpace(state.Message) == "" {
		state.Message = requirement.Error()
	}
	return state
}

func (s SigningSettings) save(cfg gitsigning.Config) error {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	doc, err := loadRuntimeConfigDoc(s.path(), s.Initial)
	if err != nil {
		return fmt.Errorf("%w: cannot load runtime configuration", gitsigning.ErrSetup)
	}
	doc["commit_signing"] = cfg
	if err := writeRuntimeConfigDoc(s.path(), doc); err != nil {
		return fmt.Errorf("%w: cannot save signing settings", gitsigning.ErrSetup)
	}
	return nil
}

func (s SigningSettings) Configure(ctx context.Context, req CommitSigningRequest) (CommitSigningState, error) {
	signingSettingsMu.Lock()
	defer signingSettingsMu.Unlock()
	current, err := s.config()
	if err != nil {
		return signingState(current), err
	}
	if enabled, err := commitSigningEnabledFromEnv(); err != nil {
		return signingState(current), err
	} else if enabled != nil && req.Enabled != *enabled {
		return signingState(current), fmt.Errorf("%w: signing is controlled by %s; change it and restart", gitsigning.ErrSetup, commitSigningEnabledEnv)
	}
	cfg := current
	importing := strings.TrimSpace(req.PrivateKey) != ""
	if importing {
		cfg, err = gitsigning.Import(s.keyDir(), req.PrivateKey, req.Passphrase)
		if err != nil {
			return signingState(current), err
		}
		// Persist the new key's public material now so it stays visible to copy and
		// register on GitHub even if the identity/registration checks below fail.
		if err := s.save(cfg); err != nil {
			return signingState(current), err
		}
	}
	registered := false
	if importing || req.Enabled {
		cfg, err = s.github().Identity(ctx, cfg)
		if err != nil {
			return signingState(cfg), err
		}
		snapshot, loadErr := gitsigning.Load(s.keyDir(), cfg)
		if loadErr != nil {
			return signingState(cfg), loadErr
		}
		if testErr := snapshot.Test(ctx, s.runner()); testErr != nil {
			return signingState(cfg), testErr
		}
		registered, err = s.github().Registered(ctx, cfg)
		if req.Enabled && (err != nil || !registered) {
			if err == nil {
				err = fmt.Errorf("%w: add the public key to this GitHub account as a Signing Key before enabling", gitsigning.ErrSetup)
			}
			return signingState(cfg), err
		}
	}
	cfg.Enabled = req.Enabled
	if err := s.save(cfg); err != nil {
		return signingState(current), err
	}
	state := signingState(cfg)
	if importing || req.Enabled {
		state.Registered = registered
		state.Registration = "not_registered"
		if registered {
			state.Registration = "registered"
		}
		if err != nil {
			state.Registration = "unavailable"
		}
	}
	state.Message = "Commit signing settings saved."
	if importing && !registered {
		state.Message = "Key saved. Add the public key to GitHub as a Signing Key, then check registration and enable signing."
	}
	return state, nil
}

func (s SigningSettings) Verify(ctx context.Context) (CommitSigningState, error) {
	cfg, err := s.config()
	state := signingState(cfg)
	if err != nil {
		return state, err
	}
	if err = s.check(ctx, cfg); err != nil {
		state.Registration = "not_ready"
		return state, err
	}
	state.Registered, state.Registration, state.Message = true, "registered", "Key is registered and local signing works."
	return state, nil
}

func (s SigningSettings) check(ctx context.Context, cfg gitsigning.Config) error {
	identity, err := s.github().Identity(ctx, cfg)
	if err != nil {
		return err
	}
	if identity.AccountID != cfg.AccountID || identity.Account != cfg.Account || identity.Email != cfg.Email {
		return fmt.Errorf("%w: GitHub account changed; save signing settings again for this account", gitsigning.ErrSetup)
	}
	registered, err := s.github().Registered(ctx, cfg)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("%w: public key is not registered as a Signing Key on %s", gitsigning.ErrSetup, cfg.Account)
	}
	snapshot, err := gitsigning.Load(s.keyDir(), cfg)
	if err != nil {
		return err
	}
	return snapshot.Test(ctx, s.runner())
}

func (s SigningSettings) Snapshot(ctx context.Context) (*gitsigning.Snapshot, error) {
	cfg, err := s.config()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	if err := s.check(ctx, cfg); err != nil {
		return nil, err
	}
	return gitsigning.Load(s.keyDir(), cfg)
}

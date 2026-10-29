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
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
)

var signingSettingsMu sync.Mutex

// ErrSigningSetupRequired marks the one signing failure a server can serve
// through: the policy requires signing but Settings cannot sign yet. Callers
// that can hold tasks keep the UI reachable so the operator can save a key;
// every other signing failure stays a fatal configuration error. It always
// accompanies gitsigning.ErrSetup, so existing setup handling is unaffected.
var ErrSigningSetupRequired = errors.New("commit signing setup required")

type setupRequiredError struct{ err error }

func (e setupRequiredError) Error() string { return e.err.Error() }
func (e setupRequiredError) Unwrap() error { return e.err }
func (e setupRequiredError) Is(target error) bool { return target == ErrSigningSetupRequired }

// markSetupRequired tags a signing failure as fixable in Settings → Commit
// signing rather than fatal at startup.
func markSetupRequired(err error) error {
	if err == nil {
		return nil
	}
	return setupRequiredError{err: err}
}

// signingBootHolds records a saved key that failed this process's startup
// checks, keyed by runtime configuration path so independent stores never
// observe each other's holds. A saved key alone satisfies SetupRequirement, so
// without this a server that stays up to collect a working key would admit
// tasks signed by a key the boot already rejected.
var signingBootHolds sync.Map

// holdBoot records err as a startup hold and returns it tagged as fixable in
// Settings → Commit signing.
func (s SigningSettings) holdBoot(err error) error {
	if err == nil {
		return nil
	}
	held := markSetupRequired(err)
	signingBootHolds.Store(s.path(), held)
	return held
}

// holdUnvalidated records a hold when a saved key could not be validated while
// the stored policy still requires signing. err comes back unchanged so the
// caller keeps its own classification; the hold exists because a saved key
// alone satisfies SetupRequirement, and an imported key that never passed the
// identity, signing, and registration checks must not reopen task submissions.
func (s SigningSettings) holdUnvalidated(cfg gitsigning.Config, err error) error {
	if err == nil || !cfg.Enabled || cfg.KeyID == "" {
		return err
	}
	s.holdBoot(err)
	return err
}

// releaseBootHold clears the startup hold once settings validate again.
func (s SigningSettings) releaseBootHold() { signingBootHolds.Delete(s.path()) }

// signingRecheckInterval bounds how often a held submission revalidates the
// saved key. The revalidation costs two GitHub calls plus a test signature, so
// without a floor a queue of held submissions re-runs it once per task.
const signingRecheckInterval = 30 * time.Second

// signingRecheckGate serializes revalidation per runtime configuration path and
// records when the last one ran. Serializing matters as much as the interval:
// two concurrent rechecks that interleave can write their verdicts out of
// order and leave the gate open against the failing one.
type signingRecheckGate struct {
	mu      sync.Mutex
	checked time.Time
}

var signingRecheckGates sync.Map

func (s SigningSettings) recheckGate() *signingRecheckGate {
	gate, _ := signingRecheckGates.LoadOrStore(s.path(), &signingRecheckGate{})
	return gate.(*signingRecheckGate)
}

func (s SigningSettings) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s SigningSettings) bootHold() error {
	held, ok := signingBootHolds.Load(s.path())
	if !ok {
		return nil
	}
	return held.(error)
}

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
	// Now is the clock the submission recheck interval is measured against.
	// It defaults to time.Now; tests inject one to drive the interval.
	Now func() time.Time
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
// It returns nil when signing is off, or when a key is saved and nothing this
// process checked rejected it.
func (s SigningSettings) SetupRequirement() error { return s.setupRequirement(true) }

// SubmissionRequirement reports the same hold for a task submission, with one
// revalidation attempt against GitHub. A hold can come from a failure that has
// since passed, such as GitHub being unreachable at startup, and without this
// it would stick until the operator pressed Check registration. The attempt is
// serialized and rate limited by signingRecheckInterval, so a burst of held
// submissions revalidates once instead of once per task.
func (s SigningSettings) SubmissionRequirement(ctx context.Context) error {
	requirement := s.SetupRequirement()
	if requirement == nil || s.bootHold() == nil {
		// No hold means nothing a recheck can clear: the key itself is missing,
		// or settings cannot be read.
		return requirement
	}
	cfg, err := s.config()
	if err != nil {
		return requirement
	}
	gate := s.recheckGate()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	// Re-read the hold under the lock. A recheck that finished while this
	// submission waited may have cleared or replaced it, and reporting the
	// state this call started with would answer against a stale verdict.
	held := s.bootHold()
	if held == nil {
		return s.SetupRequirement()
	}
	if s.now().Sub(gate.checked) < signingRecheckInterval {
		return held
	}
	gate.checked = s.now()
	if err := s.check(ctx, cfg); err != nil {
		// Replace the stale startup error so the operator sees why signing is
		// still not ready rather than a failure that already passed.
		return s.holdBoot(err)
	}
	s.releaseBootHold()
	return nil
}

// setupRequirement reports the local requirement. Callers that are about to
// revalidate the saved key pass includeHold=false so a hold from an earlier
// call does not stand in for the result they are about to produce.
func (s SigningSettings) setupRequirement(includeHold bool) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	// A key saved by an earlier boot still has to pass this boot's identity,
	// signing, and registration checks. The hold outlives ApplyEnvironment so
	// submissions cannot slip past a failure the operator has not fixed yet.
	if held := s.bootHold(); includeHold && held != nil {
		return held
	}
	if cfg.KeyID != "" {
		return nil
	}
	if commitSigningEnvEnforced() {
		return markSetupRequired(fmt.Errorf("%w: %s requires a signing key; save one in Settings → Commit signing before running tasks", gitsigning.ErrSetup, commitSigningEnabledEnv))
	}
	return markSetupRequired(fmt.Errorf("%w: save a signing key first", gitsigning.ErrSetup))
}

// ApplySetupRequirement annotates a state for the UI so Settings can explain
// the hold and keep the signing section in front of the operator.
func (s SigningSettings) ApplySetupRequirement(state CommitSigningState) CommitSigningState {
	state.EnvEnforced = commitSigningEnvEnforced()
	requirement := s.SetupRequirement()
	if requirement == nil {
		return state
	}
	if strings.TrimSpace(state.Message) == "" {
		state.Message = requirement.Error()
	}
	// SetupRequired sends the operator to the key field and holds submissions
	// behind saving a key. An unreadable or malformed runtime configuration is
	// not something that form can fix, so report it as a message only rather
	// than pointing at a remedy that cannot work.
	if errors.Is(requirement, ErrSigningSetupRequired) {
		state.SetupRequired = true
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
	// Hold the submission gate as well. The checks below end in a hold write,
	// and a recheck that only takes the gate could finish afterwards and publish
	// its older verdict over this one, reopening submissions against settings
	// that just failed.
	gate := s.recheckGate()
	gate.mu.Lock()
	defer gate.mu.Unlock()
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
		// Import returns fresh metadata with signing off. Keep the policy that is
		// already in effect so a failed enable below cannot silently disable
		// signing and let tasks run unsigned.
		cfg.Enabled = current.Enabled
		// Persist the new key's public material now so it stays visible to copy and
		// register on GitHub even if the identity/registration checks below fail.
		if err := s.save(cfg); err != nil {
			return signingState(current), err
		}
	}
	registered := false
	if importing || req.Enabled {
		// Every failure below leaves a key saved under a policy that still
		// requires signing, so each one records a hold: otherwise the saved key
		// alone would satisfy SetupRequirement and reopen task submissions.
		cfg, err = s.github().Identity(ctx, cfg)
		if err != nil {
			return signingState(cfg), s.holdUnvalidated(cfg, err)
		}
		snapshot, loadErr := gitsigning.Load(s.keyDir(), cfg)
		if loadErr != nil {
			return signingState(cfg), s.holdUnvalidated(cfg, loadErr)
		}
		if testErr := snapshot.Test(ctx, s.runner()); testErr != nil {
			return signingState(cfg), s.holdUnvalidated(cfg, testErr)
		}
		registered, err = s.github().Registered(ctx, cfg)
		if req.Enabled && (err != nil || !registered) {
			if err == nil {
				err = fmt.Errorf("%w: add the public key to this GitHub account as a Signing Key before enabling", gitsigning.ErrSetup)
			}
			return signingState(cfg), s.holdUnvalidated(cfg, err)
		}
	}
	cfg.Enabled = req.Enabled
	if err := s.save(cfg); err != nil {
		return signingState(current), err
	}
	// These settings just passed the same checks a boot runs, or turned signing
	// off, so a startup hold no longer applies and queued work can proceed.
	s.releaseBootHold()
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
	// Share the submission gate so this check and its verdict cannot interleave
	// with a held submission's recheck and release a hold the other one just
	// re-recorded, or the other way round. The interval is deliberately not
	// stamped: it throttles bursts of held submissions, and borrowing it for an
	// operator-driven check would make the next submission wait it out instead
	// of revalidating a key the operator may have fixed in between.
	gate := s.recheckGate()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if err = s.check(ctx, cfg); err != nil {
		state.Registration = "not_ready"
		// A key that stops working after a clean boot has no hold yet. Record one
		// so SetupRequirement keeps holding tasks instead of queuing work that
		// can only fail when it tries to sign a commit.
		return state, s.holdUnvalidated(cfg, err)
	}
	// check covers everything a boot checks, so an operator who fixed the key on
	// GitHub releases the startup hold from Check registration, without having
	// to re-save settings or restart the server.
	s.releaseBootHold()
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

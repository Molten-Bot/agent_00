package agentruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var ErrCodexAuthRequired = errors.New("Codex authentication required; sign in again in the harness runtime")

const codexAuthFailureFile = ".moltenhub-auth-required"

// CodexHome resolves the operator's persistent home before task HOME isolation.
func CodexHome(environ []string) (string, error) {
	if len(environ) == 0 {
		environ = os.Environ()
	}
	values := make(map[string]string)
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	home := values["CODEX_HOME"]
	if home == "" {
		if values["HOME"] == "" {
			return "", fmt.Errorf("CODEX_HOME or HOME is required for persistent Codex credentials")
		}
		home = filepath.Join(values["HOME"], ".codex")
	}
	return filepath.Abs(home)
}

// CodexSession owns the credential lock. On Unix, pass ChildFiles to the child
// so it retains the lock if the harness exits while Codex is still running.
type CodexSession struct{ file *os.File }

func (s *CodexSession) Release() { _ = s.file.Close() }

func (s *CodexSession) ChildFiles() []*os.File {
	if runtime.GOOS == "windows" {
		return nil
	}
	return []*os.File{s.file}
}

// AcquireCodexSession serializes Codex processes sharing a login, including
// device login. A kernel lock is released even if the harness crashes. Hold it
// until the child has exited; normal Codex execution rotates and saves tokens.
func AcquireCodexSession(ctx context.Context, home string) (*CodexSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("prepare persistent Codex home: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(home, ".moltenhub-session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Codex session lock: %w", err)
	}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		locked, err := tryCodexFileLock(file)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("lock Codex session: %w", err)
		}
		if locked {
			return &CodexSession{file: file}, nil
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// CodexAuthenticationFailure recognizes provider login failures, not arbitrary
// MCP HTTP 401s. Call only when the Codex invocation has actually failed.
func CodexAuthenticationFailure(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{
		"refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated",
		"refresh token was already used", "refresh token has already been used",
		"refresh token has expired", "refresh token has been revoked",
		"could not parse your authentication token", "unauthorized_unknown",
		"codex authentication required", "not logged in",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// CheckCodexAuthentication remembers a failed credential generation across
// dispatches and restarts. A fresh auth.json from an external login recovers it.
// Only a digest is stored; tokens never enter the marker, task tree, or logs.
func CheckCodexAuthentication(home string) error {
	marker, err := os.ReadFile(filepath.Join(home, codexAuthFailureFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Codex authentication state: %w", err)
	}
	fingerprint, err := codexAuthFingerprint(home)
	if err != nil {
		return err
	}
	if string(marker) == fingerprint {
		return ErrCodexAuthRequired
	}
	return nil
}

// MarkCodexAuthenticationRequired must be called while holding the session lock.
func MarkCodexAuthenticationRequired(home string) error {
	fingerprint, err := codexAuthFingerprint(home)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(home, ".moltenhub-auth-required-*")
	if err != nil {
		return fmt.Errorf("record Codex authentication failure: %w", err)
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(fingerprint)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write Codex authentication state: %w", err)
	}
	return os.Rename(file.Name(), filepath.Join(home, codexAuthFailureFile))
}

// ClearCodexAuthenticationRequired is used after a successful device login.
func ClearCodexAuthenticationRequired(home string) error {
	err := os.Remove(filepath.Join(home, codexAuthFailureFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func codexAuthFingerprint(home string) (string, error) {
	content, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "keyring-or-missing", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Codex credential generation: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(content)), nil
}

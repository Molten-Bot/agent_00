package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var ErrCodexAuthRequired = errors.New("Codex authentication required; sign in again in the harness runtime")

const codexAuthFailureFile = ".agent00-auth-required"
const codexCredentialSourceEnv = "AGENT00_CODEX_CREDENTIAL_SOURCE"
const maxCodexCredentialSize = 16 << 20

var codexReadOnlyConfigEntries = []string{"config.toml", "profiles", "skills", "plugins"}

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

// CodexCredentialSource returns the persistent home used to seed an isolated
// task home. The source marker is removed before the agent process starts.
func CodexCredentialSource(environ []string) (string, bool) {
	return codexCredentialSource(environ)
}

func codexCredentialSource(environ []string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(environ[i], "=")
		if ok && key == codexCredentialSourceEnv && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// SeedCodexCredentials copies persistent credentials into a task-local home.
// Codex may then use its normal sibling-temp-file-and-rename update without
// granting model-issued commands write access to shared credentials.
func SeedCodexCredentials(sourceHome, taskHome string) error {
	if sourceHome == taskHome {
		return nil
	}
	if err := os.MkdirAll(taskHome, 0o700); err != nil {
		return fmt.Errorf("prepare isolated Codex home: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(sourceHome, "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read persistent Codex credentials: %w", err)
	}
	file, err := os.CreateTemp(taskHome, ".auth-*")
	if err != nil {
		return fmt.Errorf("create isolated Codex credentials: %w", err)
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("secure isolated Codex credentials: %w", err)
	}
	_, writeErr := file.Write(content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write isolated Codex credentials: %w", err)
	}
	if err := os.Rename(file.Name(), filepath.Join(taskHome, "auth.json")); err != nil {
		return fmt.Errorf("install isolated Codex credentials: %w", err)
	}
	return nil
}

// SeedCodexHome prepares an isolated home with writable task-local credentials
// and read-only links to operator configuration. Persistent configuration stays
// outside the task's writable sandbox while profiles, skills, and plugins remain
// available to the CLI.
func SeedCodexHome(sourceHome, taskHome string) error {
	if err := SeedCodexCredentials(sourceHome, taskHome); err != nil {
		return err
	}
	if sourceHome == taskHome {
		return nil
	}
	for _, name := range codexReadOnlyConfigEntries {
		source := filepath.Join(sourceHome, name)
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect persistent Codex configuration %s: %w", name, err)
		}
		destination := filepath.Join(taskHome, name)
		if target, err := os.Readlink(destination); err == nil && target == source {
			continue
		} else if err == nil {
			return fmt.Errorf("isolated Codex configuration %s links to unexpected path", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect isolated Codex configuration %s: %w", name, err)
		}
		if err := os.Symlink(source, destination); err != nil {
			return fmt.Errorf("link read-only Codex configuration %s: %w", name, err)
		}
	}
	return nil
}

// PersistCodexCredentials atomically installs a successful CLI rotation in the
// persistent home. Callers must hold the Codex session lock and only invoke this
// after a trusted CLI process exits successfully.
func PersistCodexCredentials(taskHome, persistentHome string) error {
	if taskHome == persistentHome {
		return nil
	}
	taskPath := filepath.Join(taskHome, "auth.json")
	info, err := os.Lstat(taskPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect rotated Codex credentials: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("rotated Codex credentials must be a regular file")
	}
	if info.Size() > maxCodexCredentialSize {
		return fmt.Errorf("rotated Codex credentials exceed %d bytes", maxCodexCredentialSize)
	}
	content, err := os.ReadFile(taskPath)
	if err != nil {
		return fmt.Errorf("read rotated Codex credentials: %w", err)
	}
	if !json.Valid(content) {
		return fmt.Errorf("rotated Codex credentials are not valid JSON")
	}
	persistentPath := filepath.Join(persistentHome, "auth.json")
	if current, err := os.ReadFile(persistentPath); err == nil && string(current) == string(content) {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read persistent Codex credentials: %w", err)
	}
	file, err := os.CreateTemp(persistentHome, ".auth-rotation-*")
	if err != nil {
		return fmt.Errorf("create persistent Codex credentials: %w", err)
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("secure persistent Codex credentials: %w", err)
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write persistent Codex credentials: %w", err)
	}
	if err := os.Rename(file.Name(), persistentPath); err != nil {
		return fmt.Errorf("install persistent Codex credentials: %w", err)
	}
	return nil
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
	file, err := os.OpenFile(filepath.Join(home, ".agent00-session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
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
// tasks and restarts. A fresh auth.json from an external login recovers it.
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
	file, err := os.CreateTemp(home, ".agent00-auth-required-*")
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

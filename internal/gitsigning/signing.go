// Package gitsigning manages optional SSH commit signing without changing global Git configuration.
package gitsigning

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"golang.org/x/crypto/ssh"
)

var ErrSetup = errors.New("commit signing requires attention in Settings → Commit signing")

// Config contains only non-secret metadata. KeyID refers to an immutable managed key.
type Config struct {
	Enabled     bool   `json:"enabled"`
	KeyID       string `json:"key_id,omitempty"`
	PublicKey   string `json:"public_key,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Account     string `json:"account,omitempty"`
	AccountID   int64  `json:"account_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
}

type Snapshot struct {
	Config             Config
	KeyPath            string
	AllowedSignersPath string
}

// Import decrypts in memory and stores an immutable, unlocked copy for unattended use.
// Neither parsing errors nor subprocess arguments contain the submitted key or passphrase.
func Import(dir, raw, passphrase string) (Config, error) {
	key, err := ssh.ParseRawPrivateKey([]byte(strings.TrimSpace(raw)))
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase == "" {
			return Config{}, fmt.Errorf("%w: this key needs its passphrase", ErrSetup)
		}
		key, err = ssh.ParseRawPrivateKeyWithPassphrase([]byte(strings.TrimSpace(raw)), []byte(passphrase))
	}
	if err != nil {
		return Config{}, fmt.Errorf("%w: invalid private key or incorrect passphrase", ErrSetup)
	}
	switch key.(type) {
	case *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey, *ed25519.PrivateKey:
	default:
		return Config{}, fmt.Errorf("%w: use an Ed25519, RSA, or ECDSA private key", ErrSetup)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return Config{}, fmt.Errorf("%w: unsupported private key", ErrSetup)
	}
	block, err := ssh.MarshalPrivateKey(key, "agent_00 commit signing")
	if err != nil {
		return Config{}, fmt.Errorf("%w: cannot encode private key", ErrSetup)
	}
	pub := signer.PublicKey()
	digest := sha256.Sum256(pub.Marshal())
	cfg := Config{KeyID: hex.EncodeToString(digest[:]), PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), Fingerprint: ssh.FingerprintSHA256(pub)}
	if err := secureDir(dir); err != nil {
		return Config{}, err
	}
	if err := writeImmutable(filepath.Join(dir, cfg.KeyID), pem.EncodeToMemory(block)); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("%w: cannot create signing directory", ErrSetup)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: invalid signing directory", ErrSetup)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("%w: cannot restrict signing directory", ErrSetup)
	}
	return nil
}

func writeImmutable(path string, data []byte) error {
	// Create a complete private file before exposing its immutable name.
	f, err := os.CreateTemp(filepath.Dir(path), ".signing-*")
	if err != nil {
		return fmt.Errorf("%w: cannot store signing material", ErrSetup)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("%w: cannot store signing material", ErrSetup)
	}
	if err = os.Link(f.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: cannot store signing material", ErrSetup)
		}
		info, statErr := os.Lstat(path)
		existing, readErr := os.ReadFile(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || readErr != nil {
			return fmt.Errorf("%w: invalid existing signing file", ErrSetup)
		}
		// Private-key encodings contain random check bytes; verify identity when loading instead.
		if strings.HasSuffix(path, ".allowed") && string(existing) != string(data) {
			return fmt.Errorf("%w: signing metadata conflict", ErrSetup)
		}
	}
	return nil
}

func Load(dir string, cfg Config) (*Snapshot, error) {
	id, err := hex.DecodeString(cfg.KeyID)
	if err != nil || len(id) != sha256.Size || cfg.KeyID != strings.ToLower(cfg.KeyID) {
		return nil, fmt.Errorf("%w: save a signing key first", ErrSetup)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid signing directory", ErrSetup)
	}
	path := filepath.Join(dir, cfg.KeyID)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%w: signing key is missing or its permissions are too open", ErrSetup)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read signing key", ErrSetup)
	}
	key, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot load signing key", ErrSetup)
	}
	digest := sha256.Sum256(key.PublicKey().Marshal())
	if hex.EncodeToString(digest[:]) != cfg.KeyID || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key.PublicKey()))) != cfg.PublicKey {
		return nil, fmt.Errorf("%w: signing key does not match saved metadata", ErrSetup)
	}
	if cfg.Email == "" || strings.ContainsAny(cfg.Email, " \t\r\n") || cfg.Account == "" {
		return nil, fmt.Errorf("%w: signing account is missing", ErrSetup)
	}
	allowed := []byte(cfg.Email + " " + cfg.PublicKey + "\n")
	allowedID := sha256.Sum256(allowed)
	allowedPath := filepath.Join(dir, hex.EncodeToString(allowedID[:])+".allowed")
	if err := writeImmutable(allowedPath, allowed); err != nil {
		return nil, err
	}
	return &Snapshot{Config: cfg, KeyPath: path, AllowedSignersPath: allowedPath}, nil
}

// Environment preserves existing Git configuration entries and isolated HOME values.
func (s *Snapshot) Environment(env []string) []string {
	if s == nil {
		return env
	}
	if len(env) == 0 {
		env = os.Environ()
	}
	env = append([]string(nil), env...)
	count := 0
	for _, value := range env {
		if strings.HasPrefix(value, "GIT_CONFIG_COUNT=") {
			count, _ = strconv.Atoi(strings.TrimPrefix(value, "GIT_CONFIG_COUNT="))
		}
	}
	if count < 0 {
		count = 0
	}
	pairs := [][2]string{{"gpg.format", "ssh"}, {"user.signingkey", s.KeyPath}, {"commit.gpgsign", "true"}, {"user.name", s.Config.Name}, {"user.email", s.Config.Email}, {"gpg.ssh.allowedSignersFile", s.AllowedSignersPath}, {"gpg.ssh.program", "ssh-keygen"}}
	overrides := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(count + len(pairs)), "GIT_COMMITTER_NAME": s.Config.Name, "GIT_COMMITTER_EMAIL": s.Config.Email}
	for i, pair := range pairs {
		overrides[fmt.Sprintf("GIT_CONFIG_KEY_%d", count+i)] = pair[0]
		overrides[fmt.Sprintf("GIT_CONFIG_VALUE_%d", count+i)] = pair[1]
	}
	result := make([]string, 0, len(env)+len(overrides))
	for _, value := range env {
		k, _, _ := strings.Cut(value, "=")
		if _, replace := overrides[k]; !replace {
			result = append(result, value)
		}
	}
	for k, v := range overrides {
		result = append(result, k+"="+v)
	}
	return result
}

// Test signs and verifies an isolated commit, without touching any task repository.
func (s *Snapshot) Test(ctx context.Context, runner execx.Runner) error {
	dir, err := os.MkdirTemp("", "agent00-signing-check-*")
	if err != nil {
		return fmt.Errorf("%w: cannot prepare signing check", ErrSetup)
	}
	defer os.RemoveAll(dir)
	for _, args := range [][]string{{"init", "--quiet"}, {"-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "Signing check"}, {"verify-commit", "HEAD"}} {
		if _, err := runner.Run(ctx, execx.Command{Name: "git", Dir: dir, Env: s.Environment(nil), Args: args}); err != nil {
			return fmt.Errorf("%w: local signing check failed; Git 2.34+ and ssh-keygen are required", ErrSetup)
		}
	}
	return nil
}

// VerifyNew checks only commits absent from the trusted history captured during clone/fetch.
func (s *Snapshot) VerifyNew(ctx context.Context, runner execx.Runner, repo string, trusted []string) error {
	args := []string{"rev-list", "HEAD"}
	if len(trusted) > 0 {
		args = append(args, "--not")
		args = append(args, trusted...)
	}
	run := func(args ...string) (execx.Result, error) {
		return runner.Run(ctx, execx.Command{Name: "git", Dir: repo, Env: s.Environment(nil), Args: args})
	}
	res, err := run(args...)
	if err != nil {
		return fmt.Errorf("%w: cannot enumerate commits before push", ErrSetup)
	}
	for _, oid := range strings.Fields(res.Stdout) {
		if _, err := run("verify-commit", oid); err != nil {
			return fmt.Errorf("%w: commit %s is unsigned or has an invalid signature; publication stopped", ErrSetup, oid)
		}
		identity, err := run("show", "-s", "--format=%ce", oid)
		if err != nil || strings.TrimSpace(identity.Stdout) != s.Config.Email {
			return fmt.Errorf("%w: commit %s has a different committer identity; publication stopped", ErrSetup, oid)
		}
	}
	return nil
}

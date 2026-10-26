package gitsigning

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// ecdsaP224PrivateKeyPEM returns a standard SEC1 "EC PRIVATE KEY" PEM block
// for a P-224 curve key. ssh.ParseRawPrivateKey parses this directly (it is
// not the OpenSSH wire format Import normally produces), and the resulting
// *ecdsa.PrivateKey passes Import's type switch (it is a *ecdsa.PrivateKey)
// but P-224 is not one of the curves golang.org/x/crypto/ssh signs with, so
// ssh.NewSignerFromKey rejects it. This is the only practical way found to
// exercise Import's "unsupported private key" branch (as opposed to the
// "use an Ed25519, RSA, or ECDSA private key" branch already covered by the
// DSA key test in signing_test.go) without mocking the ssh package itself.
func ecdsaP224PrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func TestImportRejectsUnsignableCurve(t *testing.T) {
	t.Parallel()
	_, err := Import(t.TempDir(), ecdsaP224PrivateKeyPEM(t), "")
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "unsupported private key") {
		t.Fatalf("Import(P-224 ecdsa key) error = %v, want unsupported private key error", err)
	}
}

// TestWriteImmutableFailsWhenLinkErrorIsNotFileExists exercises writeImmutable's
// "os.Link failed for a reason other than the destination already existing"
// branch. A destination filename longer than the filesystem's NAME_MAX makes
// the hardlink call fail with ENAMETOOLONG, which os.IsExist/errors.Is(err,
// os.ErrExist) correctly reports as false, distinguishing it from the
// already-exists conflict path covered elsewhere in this file.
func TestWriteImmutableFailsWhenLinkErrorIsNotFileExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, strings.Repeat("a", 300))
	if err := writeImmutable(path, []byte("data")); !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot store signing material") {
		t.Fatalf("writeImmutable(name too long) error = %v, want cannot store signing material error", err)
	}
}

// Note on two remaining defensive branches left uncovered:
//
//   - secureDir's "cannot restrict signing directory" branch (os.Chmod
//     failing after a successful os.Lstat) needs a directory the test
//     process can Lstat but cannot Chmod. On Linux that requires either not
//     owning the directory (a second UID, unavailable to this non-root test
//     process, which always owns directories it creates) or an
//     immutable/read-only mount applied to just that directory. A user+mount
//     namespace was attempted (`unshare -rm`) but fails in this sandbox with
//     "cannot open /proc/self/uid_map: read-only file system", since /proc
//     itself is read-only here.
//   - writeImmutable's post-CreateTemp "cannot store signing material"
//     branch for f.Write/f.Sync/f.Close failing needs a write to a just-created
//     regular file to fail (e.g. ENOSPC/EFBIG), which requires either a
//     quota-limited or full filesystem (needs a dedicated mount, blocked for
//     the same /proc reason above) or lowering RLIMIT_FSIZE and ignoring
//     SIGXFSZ process-wide, which would leak process-global state across
//     this shared test binary and risks corrupting unrelated parallel tests.
//
// Both are left uncovered rather than forcing brittle, environment-specific,
// or process-global-state tests.

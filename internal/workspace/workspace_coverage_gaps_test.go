package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMountOptionsForPathFallsBackToProcMountsWhenMountInfoHasNoMatch covers
// the mountOptionsForPath fallback branch that is only reached when
// /proc/self/mountinfo fails to produce a match but /proc/mounts does. The
// real files always exist and (per existing tests) /proc/self/mountinfo
// matching is readily exercised, so the fallback branch can only be
// reliably reached by faking the hard-coded source paths through the
// package-level procSelfMountInfoPath/procMountsPath seam.
func TestMountOptionsForPathFallsBackToProcMountsWhenMountInfoHasNoMatch(t *testing.T) {
	emptyMountInfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(emptyMountInfo, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile(empty mountinfo) error = %v", err)
	}

	mounts := filepath.Join(t.TempDir(), "mounts")
	mountsContent := "tmpfs /fake/exec/target tmpfs rw,nosuid,nodev,noexec 0 0\n"
	if err := os.WriteFile(mounts, []byte(mountsContent), 0o644); err != nil {
		t.Fatalf("WriteFile(mounts) error = %v", err)
	}

	origInfo, origMounts := procSelfMountInfoPath, procMountsPath
	procSelfMountInfoPath = emptyMountInfo
	procMountsPath = mounts
	t.Cleanup(func() { procSelfMountInfoPath, procMountsPath = origInfo, origMounts })

	opts, ok := mountOptionsForPath("/fake/exec/target/nested")
	if !ok {
		t.Fatal("mountOptionsForPath() ok = false, want true via /proc/mounts fallback")
	}
	if _, has := opts["noexec"]; !has {
		t.Fatalf("mountOptionsForPath() opts = %#v, want noexec from fallback mounts file", opts)
	}
}

// TestBaseAllowsExecProbesWhenMountOptionsUnavailable covers the
// baseAllowsExec branch taken when neither /proc/self/mountinfo nor
// /proc/mounts produce a match (mountOptionsForPath returns ok=false),
// forcing a fall-through to the real probeBaseAllowsExec exec check.
func TestBaseAllowsExecProbesWhenMountOptionsUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux mount options only")
	}

	origInfo, origMounts := procSelfMountInfoPath, procMountsPath
	procSelfMountInfoPath = filepath.Join(t.TempDir(), "missing-mountinfo")
	procMountsPath = filepath.Join(t.TempDir(), "missing-mounts")
	t.Cleanup(func() { procSelfMountInfoPath, procMountsPath = origInfo, origMounts })

	dir := t.TempDir()
	baseAllowsExecCache.Delete(dir)
	t.Cleanup(func() { baseAllowsExecCache.Delete(dir) })

	if !baseAllowsExec(dir) {
		t.Fatal("baseAllowsExec() = false, want true (probe should succeed on a writable temp dir)")
	}
}

// TestProbeBaseAllowsExecReturnsFalseWhenBaseDirMissing covers the
// os.MkdirTemp failure branch inside probeBaseAllowsExec.
func TestProbeBaseAllowsExecReturnsFalseWhenBaseDirMissing(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("linux executable probe only")
	}

	missingBase := filepath.Join(t.TempDir(), "does-not-exist")
	if probeBaseAllowsExec(missingBase) {
		t.Fatal("probeBaseAllowsExec(missing base) = true, want false")
	}
}

// TestResolveAgentsSeedPathFallsBackToExecutableDirectory covers the final
// resolveAgentsSeedPath fallback, which walks upward from the running
// executable's directory. That path cannot be reached by manipulating cwd
// or the env override alone, since it only runs after both of those fail;
// the osExecutable seam lets the test substitute a fake executable location
// without depending on the real `go test` binary's on-disk path.
func TestResolveAgentsSeedPathFallsBackToExecutableDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	cwdRoot := t.TempDir()
	if err := os.Chdir(cwdRoot); err != nil {
		t.Fatalf("Chdir(%q) error = %v", cwdRoot, err)
	}
	if _, ok := findPathUpward(cwdRoot, agentsSeedPath); ok {
		t.Fatal("test setup invalid: cwd already resolves the seed path upward")
	}

	exeRoot := t.TempDir()
	seedDir := filepath.Join(exeRoot, "library")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(seedDir) error = %v", err)
	}
	seedPath := filepath.Join(seedDir, "AGENTS.md")
	if err := os.WriteFile(seedPath, []byte("seed"), 0o644); err != nil {
		t.Fatalf("WriteFile(seed) error = %v", err)
	}
	exeDir := filepath.Join(exeRoot, "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(exeDir) error = %v", err)
	}
	fakeExePath := filepath.Join(exeDir, "harness")

	t.Setenv(agentsSeedEnv, "")

	origExecutable := osExecutable
	osExecutable = func() (string, error) { return fakeExePath, nil }
	t.Cleanup(func() { osExecutable = origExecutable })

	if got := resolveAgentsSeedPath(); got != seedPath {
		t.Fatalf("resolveAgentsSeedPath() = %q, want %q", got, seedPath)
	}
}

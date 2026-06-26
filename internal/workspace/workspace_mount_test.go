package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMountOptionsFromMountInfoSelectsMostSpecificMount(t *testing.T) {
	t.Parallel()

	mountInfo := `24 22 0:22 / / rw,relatime - overlay overlay rw
31 24 0:27 / /dev rw,relatime - tmpfs tmpfs rw,nosuid,nodev
32 24 0:28 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k,inode64`
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(mountInfo), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	opts, ok := mountOptionsFromMountInfo(path, "/dev/shm/temp/work")
	if !ok {
		t.Fatal("mountOptionsFromMountInfo() ok = false, want true")
	}
	if _, has := opts["noexec"]; !has {
		t.Fatalf("opts = %#v, expected noexec", opts)
	}
	if _, has := opts["inode64"]; !has {
		t.Fatalf("opts = %#v, expected inode64", opts)
	}
}

func TestMountOptionsFromMountInfoHandlesMissingFileAndNoMatch(t *testing.T) {
	t.Parallel()

	if _, ok := mountOptionsFromMountInfo(filepath.Join(t.TempDir(), "missing"), "/tmp"); ok {
		t.Fatal("mountOptionsFromMountInfo(missing) ok = true, want false")
	}

	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte("24 22 0:22 / /sandbox rw - overlay overlay rw\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, ok := mountOptionsFromMountInfo(path, "/does/not/match"); ok {
		t.Fatal("mountOptionsFromMountInfo(no match) ok = true, want false")
	}
}

func TestMountOptionsFromProcMountsSelectsMostSpecificMount(t *testing.T) {
	t.Parallel()

	procMounts := `overlay / overlay rw,relatime 0 0
tmpfs /dev tmpfs rw,nosuid,nodev 0 0
tmpfs /dev/shm tmpfs rw,nosuid,nodev,noexec,relatime,size=65536k 0 0`
	path := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(path, []byte(procMounts), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	opts, ok := mountOptionsFromProcMounts(path, "/dev/shm/task")
	if !ok {
		t.Fatal("mountOptionsFromProcMounts() ok = false, want true")
	}
	if _, has := opts["noexec"]; !has {
		t.Fatalf("opts = %#v, expected noexec", opts)
	}
	if _, has := opts["size=65536k"]; !has {
		t.Fatalf("opts = %#v, expected size=65536k", opts)
	}
}

func TestMountOptionsForPathAndBaseAllowsExecInputHandling(t *testing.T) {
	t.Parallel()

	if opts, ok := mountOptionsForPath("   "); ok || opts != nil {
		t.Fatalf("mountOptionsForPath(empty) = (%v, %v), want (nil, false)", opts, ok)
	}
	if !baseAllowsExec(" ") {
		t.Fatal("baseAllowsExec(empty) = false, want true")
	}
	if !baseAllowsExec("/path/that/does/not/exist") {
		t.Fatal("baseAllowsExec(nonexistent) = false, want true")
	}

	if runtime.GOOS == "linux" {
		_, _ = mountOptionsForPath(" / ")
	}
}

func TestBaseAllowsExecRejectsNoExecDevShmWhenReported(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux mount options only")
	}
	if _, err := os.Stat(defaultRAMBase); err != nil {
		t.Skipf("%s unavailable: %v", defaultRAMBase, err)
	}
	opts, ok := mountOptionsForPath(defaultRAMBase)
	if !ok {
		t.Skip("mount options unavailable")
	}
	if _, noExec := opts["noexec"]; !noExec {
		t.Skipf("%s is executable in this environment", defaultRAMBase)
	}

	baseAllowsExecCache.Delete(defaultRAMBase)
	t.Cleanup(func() { baseAllowsExecCache.Delete(defaultRAMBase) })
	if baseAllowsExec(defaultRAMBase) {
		t.Fatalf("baseAllowsExec(%q) = true, want false for noexec mount", defaultRAMBase)
	}
}

func TestProbeBaseAllowsExecAcceptsExecutableTempDir(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("linux executable probe only")
	}
	if !probeBaseAllowsExec(t.TempDir()) {
		t.Fatal("probeBaseAllowsExec(temp dir) = false, want true")
	}
}

func TestMountHelpersCoverInvalidInputs(t *testing.T) {
	t.Parallel()

	if _, _, ok := parseMountInfoLine("missing-separator"); ok {
		t.Fatal("parseMountInfoLine(malformed) ok = true, want false")
	}
	if _, _, ok := parseProcMountsLine("too-few-fields"); ok {
		t.Fatal("parseProcMountsLine(malformed) ok = true, want false")
	}
	if pathWithinMount("", "/") {
		t.Fatal("pathWithinMount(empty path) = true, want false")
	}
	if pathWithinMount("/tmp", "") {
		t.Fatal("pathWithinMount(empty mount) = true, want false")
	}
	if !pathWithinMount("/tmp", "/") {
		t.Fatal("pathWithinMount(root mount) = false, want true")
	}
	if !pathWithinMount("/tmp", "/tmp") {
		t.Fatal("pathWithinMount(exact) = false, want true")
	}
}

func TestUnescapeMountFieldAndIsOctal(t *testing.T) {
	t.Parallel()

	if got, want := unescapeMountField(`/tmp/with\040space\011tab`), "/tmp/with space\ttab"; got != want {
		t.Fatalf("unescapeMountField() = %q, want %q", got, want)
	}
	if got, want := unescapeMountField(`/tmp/raw\09`), `/tmp/raw\09`; got != want {
		t.Fatalf("unescapeMountField(invalid octal) = %q, want %q", got, want)
	}

	for _, b := range []byte{'0', '7'} {
		if !isOctal(b) {
			t.Fatalf("isOctal(%q) = false, want true", b)
		}
	}
	for _, b := range []byte{'8', 'x'} {
		if isOctal(b) {
			t.Fatalf("isOctal(%q) = true, want false", b)
		}
	}
}

func TestCreateRunDirRejectsEmptyGUID(t *testing.T) {
	t.Parallel()

	m := Manager{
		NewGUID: func() string { return "" },
	}
	if _, _, err := m.CreateRunDir(); err == nil {
		t.Fatal("CreateRunDir() error = nil, want non-nil")
	}
}

func TestParseMountOptionsSkipsEmptyParts(t *testing.T) {
	t.Parallel()

	opts := parseMountOptions("rw,, noexec, ,nodev")
	if len(opts) != 3 {
		t.Fatalf("len(parseMountOptions()) = %d, want 3 (%#v)", len(opts), opts)
	}
}

func TestResolveAgentsSeedPathFromWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	root := t.TempDir()
	startDir := filepath.Join(root, "internal", "workspace")
	seedPath := filepath.Join(root, "library", "AGENTS.md")
	if err := os.MkdirAll(startDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(startDir) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(seed dir) error = %v", err)
	}
	if err := os.WriteFile(seedPath, []byte("seed"), 0o644); err != nil {
		t.Fatalf("WriteFile(seed) error = %v", err)
	}
	if err := os.Chdir(startDir); err != nil {
		t.Fatalf("Chdir(%q) error = %v", startDir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	t.Setenv(agentsSeedEnv, filepath.Join(root, "missing-seed.md"))
	if got, want := resolveAgentsSeedPath(), seedPath; got != want {
		t.Fatalf("resolveAgentsSeedPath() = %q, want %q", got, want)
	}
}

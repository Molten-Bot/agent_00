package agentruntime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeSandboxScratchDir(t *testing.T) {
	t.Parallel()

	if got, err := claudeSandboxScratchDir("  "); err != nil || got != "" {
		t.Fatalf("claudeSandboxScratchDir(blank) = (%q, %v), want empty", got, err)
	}

	configDir := t.TempDir()
	got, err := claudeSandboxScratchDir(configDir)
	if err != nil {
		t.Fatalf("claudeSandboxScratchDir() error = %v", err)
	}
	if want := filepath.Join(configDir, claudeScratchDirName); got != want {
		t.Fatalf("claudeSandboxScratchDir() = %q, want %q", got, want)
	}
	info, statErr := os.Stat(got)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("scratch directory was not created: %v", statErr)
	}
	// The scratch directory must stay separate from the credential file and
	// the backups the harness restores from.
	for _, protected := range []string{claudeConfigFileName, claudeConfigBackupDirName} {
		if filepath.Base(got) == protected {
			t.Fatalf("scratch directory %q collides with protected entry %q", got, protected)
		}
	}

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeSandboxScratchDir(blocker); err == nil {
		t.Fatal("claudeSandboxScratchDir(file) error = nil, want creation failure")
	}
}

func TestClaudeSandboxScratchDirSkipsUncreatableConfigDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block creation here")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	// The configuration directory is absent and cannot be created, so the CLI
	// cannot write there either: there is no grant to make, and the invocation
	// must still run.
	got, err := claudeSandboxScratchDir(filepath.Join(parent, ".claude"))
	if err != nil || got != "" {
		t.Fatalf("claudeSandboxScratchDir(uncreatable) = (%q, %v), want no grant and no error", got, err)
	}
}

func TestCheckClaudeSessionScratchReportsUnwritableConfigDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block creation here")
	}
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o700) })

	// The configuration directory exists, so the CLI resolves it and tries to
	// create the scratch directory there on its first tool call. The harness
	// cannot create it either, so the whole run would otherwise execute without
	// tools.
	err := CheckClaudeSessionScratch(configDir)
	if !errors.Is(err, ErrClaudeSessionScratchUnwritable) {
		t.Fatalf("CheckClaudeSessionScratch(unwritable) = %v, want ErrClaudeSessionScratchUnwritable", err)
	}
	if !errors.Is(err, ErrClaudeSessionScratch) {
		t.Fatalf("CheckClaudeSessionScratch(unwritable) = %v, want it in the ErrClaudeSessionScratch class", err)
	}
	if want := filepath.Join(configDir, claudeScratchDirName); !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckClaudeSessionScratch(unwritable) = %q, want it to name %q", err, want)
	}
}

func TestCheckClaudeSessionScratchReportsNonPermissionFailuresAsThemselves(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	// The configuration directory is writable, so "make it writable" is advice
	// that cannot fix anything here. A file occupying the scratch path has to be
	// reported as the creation failure it is.
	if err := os.WriteFile(filepath.Join(configDir, claudeScratchDirName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := CheckClaudeSessionScratch(configDir)
	if err == nil {
		t.Fatal("CheckClaudeSessionScratch(blocked scratch path) = nil, want creation failure")
	}
	if errors.Is(err, ErrClaudeSessionScratchUnwritable) {
		t.Fatalf("CheckClaudeSessionScratch(blocked scratch path) = %v, want the creation failure itself", err)
	}
	// A follow-up run boots the same CLI against the same configuration
	// directory, so it stops here too. Callers must be able to recognize that
	// without matching the message.
	if !errors.Is(err, ErrClaudeSessionScratch) {
		t.Fatalf("CheckClaudeSessionScratch(blocked scratch path) = %v, want it in the ErrClaudeSessionScratch class", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(configDir, claudeScratchDirName)) {
		t.Fatalf("CheckClaudeSessionScratch(blocked scratch path) = %q, want it to name the scratch path", err)
	}
}

func TestWriteRefusalDistinguishesDirectoryFromMachineFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "none", err: nil, want: false},
		{
			name: "denied permission",
			err:  &fs.PathError{Op: "open", Path: "/config/probe", Err: fs.ErrPermission},
			want: true,
		},
		{
			name: "absent directory",
			err:  &fs.PathError{Op: "open", Path: "/config/probe", Err: fs.ErrNotExist},
			want: true,
		},
		{
			name: "read-only filesystem",
			err:  errors.New("open /config/probe: read-only file system"),
			want: true,
		},
		{
			// The directory is fine; the machine is not. Making the directory
			// writable fixes neither, so these must keep reaching an operator as
			// the creation failures they are.
			name: "exhausted disk",
			err:  errors.New("open /config/probe: no space left on device"),
			want: false,
		},
		{
			name: "exhausted file descriptors",
			err:  errors.New("open /config/probe: too many open files"),
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isWriteRefusal(tc.err); got != tc.want {
				t.Fatalf("isWriteRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestClaudeSandboxScratchDirMarksCreationFailures(t *testing.T) {
	t.Parallel()

	// A file where the configuration directory belongs fails creation outright,
	// and that failure reaches the harness through BuildCommand instead of
	// CheckClaudeSessionScratch. It belongs to the same class.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := claudeSandboxScratchDir(blocker)
	if !errors.Is(err, ErrClaudeSessionScratch) {
		t.Fatalf("claudeSandboxScratchDir(file) = %v, want it in the ErrClaudeSessionScratch class", err)
	}
}

func TestCheckClaudeSessionScratchAllowsResolvableConfigDirs(t *testing.T) {
	t.Parallel()

	t.Run("blank", func(t *testing.T) {
		t.Parallel()
		// No configuration directory is described, so there is nothing to check.
		if err := CheckClaudeSessionScratch("  "); err != nil {
			t.Fatalf("CheckClaudeSessionScratch(blank) = %v, want nil", err)
		}
	})

	t.Run("writable", func(t *testing.T) {
		t.Parallel()
		// The common case: the scratch directory is created and the run proceeds.
		configDir := t.TempDir()
		if err := CheckClaudeSessionScratch(configDir); err != nil {
			t.Fatalf("CheckClaudeSessionScratch(writable) = %v, want nil", err)
		}
		if info, err := os.Stat(filepath.Join(configDir, claudeScratchDirName)); err != nil || !info.IsDir() {
			t.Fatalf("scratch directory was not created: %v", err)
		}
	})
}

func TestCheckClaudeSessionScratchReportsUncreatableConfigDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block creation here")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	// A read-only HOME leaves the CLI no default location to fall back to: it
	// derives the same configuration directory and cannot create it either, so
	// the run must stop here instead of executing every invocation without the
	// tools that need the scratch directory.
	configDir := filepath.Join(parent, ".claude")
	err := CheckClaudeSessionScratch(configDir)
	if !errors.Is(err, ErrClaudeSessionScratchUnwritable) {
		t.Fatalf("CheckClaudeSessionScratch(uncreatable) = %v, want ErrClaudeSessionScratchUnwritable", err)
	}
	if !errors.Is(err, ErrClaudeSessionScratch) {
		t.Fatalf("CheckClaudeSessionScratch(uncreatable) = %v, want it in the ErrClaudeSessionScratch class", err)
	}
	if want := filepath.Join(configDir, claudeScratchDirName); !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckClaudeSessionScratch(uncreatable) = %q, want it to name %q", err, want)
	}
}

func TestClaudeHomeResolution(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	explicit := t.TempDir()

	for _, tc := range []struct {
		name    string
		environ []string
		want    string
		wantErr bool
	}{
		{name: "config dir wins", environ: []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + explicit}, want: explicit},
		{name: "home fallback", environ: []string{"HOME=" + home}, want: filepath.Join(home, ".claude")},
		{name: "blank config dir falls back", environ: []string{"HOME=" + home, "CLAUDE_CONFIG_DIR= "}, want: filepath.Join(home, ".claude")},
		{name: "no home", environ: []string{"PATH=/usr/bin"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ClaudeHome(tc.environ)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ClaudeHome(%v) = %q, want error", tc.environ, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClaudeHome(%v) error: %v", tc.environ, err)
			}
			if got != tc.want {
				t.Fatalf("ClaudeHome(%v) = %q, want %q", tc.environ, got, tc.want)
			}
		})
	}
}

func TestClaudeConfigurationFailureDetection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "cli startup refusal",
			output: "Claude configuration file not found at: /home/agent/.claude/.claude.json\nA backup file exists at: /home/agent/.claude/backups/.claude.json.backup.1\n",
			want:   true,
		},
		{name: "case insensitive", output: "claude configuration file not found at: /tmp/.claude.json", want: true},
		{name: "quoted in prompt", output: "Investigate: Claude configuration file not found at: /tmp/.claude.json", want: false},
		{name: "unrelated failure", output: "Build failed", want: false},
		{name: "empty", output: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClaudeConfigurationFailure(tc.output); got != tc.want {
				t.Fatalf("ClaudeConfigurationFailure(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

func TestClaudeSessionScratchFailureDetection(t *testing.T) {
	t.Parallel()
	// The scratch directory the invocation under test resolves. Every fixture
	// below is written against this path rather than a literal one, so none of
	// them is a report about a configuration directory that could exist on a
	// machine running these tests: an agent reading this file into a transcript
	// cannot make its own run look like it lost its tools.
	scratchDir := filepath.Join(t.TempDir(), claudeScratchDirName)
	for _, tc := range []struct {
		name       string
		output     string
		scratchDir string
		want       bool
	}{
		{
			name: "agent relays its own tool failure",
			output: "Validation note: no shell execution was available in this runtime " +
				"(the only exec path failed with `EROFS: read-only file system, " +
				"mkdir '" + scratchDir + "'`), so `go build` could not be run.",
			want: true,
		},
		{
			name:   "cli reports a denied write",
			output: "Error: EACCES: permission denied, mkdir '" + scratchDir + "'",
			want:   true,
		},
		{
			name:   "creation wording without mkdir",
			output: "failed to create " + scratchDir + ": operation not permitted",
			want:   true,
		},
		{
			name:   "refusal for an unrelated directory",
			output: "EROFS: read-only file system, mkdir '/srv/app/dist'",
			want:   false,
		},
		{
			// Another installation's scratch directory says nothing about this
			// invocation. Agents quote failing logs, fixtures and source they
			// are changing, and none of that costs the run a tool.
			name:   "refusal for another installation's scratch directory",
			output: "EROFS: read-only file system, mkdir '/some-other-config-dir/" + claudeScratchDirName + "'",
			want:   false,
		},
		{
			name:   "scratch directory named without a refusal",
			output: "The CLI creates " + scratchDir + " on its first tool call.",
			want:   false,
		},
		{
			// A refusal somewhere in a transcript is not evidence about this
			// directory; the sandbox denies plenty of unrelated writes.
			name:   "refusal and scratch path on separate lines",
			output: "EROFS: read-only file system, mkdir '/srv/app/dist'\nScratch lives in " + scratchDir + ".",
			want:   false,
		},
		{
			// Without a resolved configuration directory there is no path to
			// match, so no report can be attributed to this invocation.
			name:       "unresolved scratch directory",
			output:     "EROFS: read-only file system, mkdir '" + scratchDir + "'",
			scratchDir: "  ",
			want:       false,
		},
		{name: "empty", output: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := tc.scratchDir
			if dir == "" {
				dir = scratchDir
			}
			if got := ClaudeSessionScratchFailure(tc.output, dir); got != tc.want {
				t.Fatalf("ClaudeSessionScratchFailure(%q, %q) = %v, want %v", tc.output, dir, got, tc.want)
			}
		})
	}
}

func TestClaudeShellToolRanSeparatesFinishedCommandsFromUnrunnableOnes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{
			// The report a same-host follow-up produces: it quotes the refusal it
			// was asked to diagnose and still ran its own validation.
			name: "quoted refusal alongside a finished command",
			output: "Diagnosed the failing log, which ends with `EROFS: read-only file system, " +
				"mkdir '/some-other-config-dir/" + claudeScratchDirName + "'`. Fixed the handler and `go test ./...` passed.",
			want: true,
		},
		{
			name:   "finished build",
			output: "`npm run build` succeeded and `eslint .` reported no failures.",
			want:   true,
		},
		{
			// The report an invocation that lost its shell tools produces: it
			// names the command precisely because it could not run it.
			name: "command named as unrunnable",
			output: "Validation note: no shell execution was available in this runtime, " +
				"so `go test ./...` could not be run.",
			want: false,
		},
		{name: "command unavailable", output: "go test is unavailable in this runtime.", want: false},
		{name: "command skipped", output: "`npm test` skipped; node_modules missing.", want: false},
		{
			// Split across lines, the command and the success describe two
			// different things, so neither corroborates the other.
			name:   "command and success on separate lines",
			output: "Planned `go test ./...`.\nThe review pass passed its own checks.",
			want:   false,
		},
		{name: "no command named", output: "Everything passed.", want: false},
		{name: "empty", output: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClaudeShellToolRan(tc.output); got != tc.want {
				t.Fatalf("ClaudeShellToolRan(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

func TestClaudeSessionScratchDirResolution(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	if got, want := ClaudeSessionScratchDir([]string{"CLAUDE_CONFIG_DIR=" + configDir}), filepath.Join(configDir, claudeScratchDirName); got != want {
		t.Fatalf("ClaudeSessionScratchDir(config dir) = %q, want %q", got, want)
	}
	home := t.TempDir()
	if got, want := ClaudeSessionScratchDir([]string{"HOME=" + home}), filepath.Join(home, ".claude", claudeScratchDirName); got != want {
		t.Fatalf("ClaudeSessionScratchDir(home) = %q, want %q", got, want)
	}
	// Neither variable is set, so the CLI has no configuration directory to
	// derive a scratch path from and there is nothing to match reports against.
	if got := ClaudeSessionScratchDir([]string{"PATH=/usr/bin"}); got != "" {
		t.Fatalf("ClaudeSessionScratchDir(no config dir) = %q, want empty", got)
	}
}

func TestClaudeAuthenticationFailureRecognizesLoggedOutCLI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{name: "logged out verdict", output: "Not logged in · Please run /login", want: true},
		{name: "login instruction alone", output: "Please run /login", want: true},
		{name: "expired token", output: "OAuth access token has expired", want: true},
		{name: "quoted in prompt", output: "The agent said: Not logged in · Please run /login", want: false},
		{name: "unrelated tool 401", output: "curl: HTTP 401 Unauthorized", want: false},
		{name: "missing config is not an auth failure", output: "Claude configuration file not found at: /tmp/.claude.json", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClaudeAuthenticationFailure(tc.output); got != tc.want {
				t.Fatalf("ClaudeAuthenticationFailure(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

func writeClaudeBackup(t *testing.T, configDir, suffix, content string) string {
	t.Helper()
	dir := filepath.Join(configDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".claude.json.backup."+suffix)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRepairClaudeConfigRestoresNewestReadableBackup(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	writeClaudeBackup(t, configDir, "1793284716264", `{"generation":"older"}`)
	newest := writeClaudeBackup(t, configDir, "1793284716265", `{"generation":"newest"}`)

	restored, err := RepairClaudeConfig(configDir)
	if err != nil {
		t.Fatalf("RepairClaudeConfig: %v", err)
	}
	if restored != newest {
		t.Fatalf("restored from %q, want %q", restored, newest)
	}
	content, err := os.ReadFile(ClaudeConfigPath(configDir))
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if string(content) != `{"generation":"newest"}` {
		t.Fatalf("restored content = %q", content)
	}
}

func TestRepairClaudeConfigSkipsUnparsableBackup(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	usable := writeClaudeBackup(t, configDir, "1793284716264", `{"generation":"usable"}`)
	writeClaudeBackup(t, configDir, "1793284716265", "not json")

	restored, err := RepairClaudeConfig(configDir)
	if err != nil {
		t.Fatalf("RepairClaudeConfig: %v", err)
	}
	if restored != usable {
		t.Fatalf("restored from %q, want %q", restored, usable)
	}
}

func TestRepairClaudeConfigReportsUnusableBackups(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	writeClaudeBackup(t, configDir, "1793284716264", "not json")

	restored, err := RepairClaudeConfig(configDir)
	if restored != "" {
		t.Fatalf("restored = %q, want no restore", restored)
	}
	if !errors.Is(err, ErrClaudeConfigMissing) {
		t.Fatalf("err = %v, want ErrClaudeConfigMissing", err)
	}
}

func TestRepairClaudeConfigLeavesExistingAndFreshInstalls(t *testing.T) {
	t.Parallel()

	t.Run("existing config is never overwritten", func(t *testing.T) {
		t.Parallel()
		configDir := t.TempDir()
		writeClaudeBackup(t, configDir, "1793284716264", `{"generation":"backup"}`)
		if err := os.WriteFile(ClaudeConfigPath(configDir), []byte(`{"generation":"live"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		restored, err := RepairClaudeConfig(configDir)
		if err != nil || restored != "" {
			t.Fatalf("RepairClaudeConfig = %q, %v; want no restore", restored, err)
		}
		content, err := os.ReadFile(ClaudeConfigPath(configDir))
		if err != nil || string(content) != `{"generation":"live"}` {
			t.Fatalf("live config = %q, %v", content, err)
		}
	})

	t.Run("first run without backups is left alone", func(t *testing.T) {
		t.Parallel()
		configDir := t.TempDir()

		restorable, backup, err := ClaudeConfigRestorable(configDir)
		if err != nil || restorable || backup != "" {
			t.Fatalf("ClaudeConfigRestorable = %v, %q, %v; want false", restorable, backup, err)
		}
		restored, err := RepairClaudeConfig(configDir)
		if err != nil || restored != "" {
			t.Fatalf("RepairClaudeConfig = %q, %v; want no restore", restored, err)
		}
		if _, err := os.Stat(ClaudeConfigPath(configDir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("config file created on a first run: %v", err)
		}
	})

	t.Run("blank config dir is a no-op", func(t *testing.T) {
		t.Parallel()
		restored, err := RepairClaudeConfig("  ")
		if err != nil || restored != "" {
			t.Fatalf("RepairClaudeConfig = %q, %v; want no restore", restored, err)
		}
	})
}

func TestClaudeConfigRestorableReportsEmptyConfigFile(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	newest := writeClaudeBackup(t, configDir, "1793284716264", `{"generation":"backup"}`)
	if err := os.WriteFile(ClaudeConfigPath(configDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	restorable, backup, err := ClaudeConfigRestorable(configDir)
	if err != nil {
		t.Fatalf("ClaudeConfigRestorable: %v", err)
	}
	if !restorable || backup != newest {
		t.Fatalf("ClaudeConfigRestorable = %v, %q; want true, %q", restorable, backup, newest)
	}
}

func TestClaudeConfigRestorableLeavesUnexpectedConfigPathAlone(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	writeClaudeBackup(t, configDir, "1793284716264", `{"generation":"backup"}`)
	// A directory at the configuration path is not an absence this repair can
	// reason about; it must never be replaced from a backup.
	if err := os.MkdirAll(ClaudeConfigPath(configDir), 0o700); err != nil {
		t.Fatal(err)
	}

	restorable, backup, err := ClaudeConfigRestorable(configDir)
	if err != nil || restorable || backup != "" {
		t.Fatalf("ClaudeConfigRestorable = %v, %q, %v; want false", restorable, backup, err)
	}
	restored, err := RepairClaudeConfig(configDir)
	if err != nil || restored != "" {
		t.Fatalf("RepairClaudeConfig = %q, %v; want no restore", restored, err)
	}
	if info, err := os.Stat(ClaudeConfigPath(configDir)); err != nil || !info.IsDir() {
		t.Fatalf("configuration path changed: %v, %v", info, err)
	}
}

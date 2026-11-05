package agentruntime

import (
	"errors"
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

	t.Run("uncreatable", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("directory permissions do not block creation here")
		}
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

		// The CLI resolves its own default location when the configuration
		// directory it was pointed at does not exist and cannot be created, so
		// this is not the fault being reported.
		if err := CheckClaudeSessionScratch(filepath.Join(parent, ".claude")); err != nil {
			t.Fatalf("CheckClaudeSessionScratch(uncreatable) = %v, want nil", err)
		}
	})
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

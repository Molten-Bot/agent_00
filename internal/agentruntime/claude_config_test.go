package agentruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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

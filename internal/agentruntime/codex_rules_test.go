package agentruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckCodexRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, filename, contents string
		wantError                bool
	}{
		{"valid", "default.rules", "# Valid policy\nprefix_rule(pattern=[\"git\"], decision=\"prompt\")\n", false},
		{"corrupt", "default.rules", "# Policy\n\n\x00\x00\n# Private policy content\n", true},
		{"backup ignored", "default.rules.backup", "\x00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			dir := filepath.Join(home, "rules")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, tc.filename)
			if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			err := CheckCodexRules(home)
			if (err != nil) != tc.wantError {
				t.Fatalf("CheckCodexRules() = %v", err)
			}
			if err != nil && (!strings.Contains(err.Error(), path+":3") || strings.Contains(err.Error(), "Private policy content")) {
				t.Fatalf("error must identify line without leaking rules: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.contents {
				t.Fatal("policy was modified", err)
			}
		})
	}
}

func TestCheckCodexRulesMissingAndUnreadable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := CheckCodexRules(home); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "rules"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckCodexRules(home); err == nil {
		t.Fatal("invalid policy directory accepted")
	}
}

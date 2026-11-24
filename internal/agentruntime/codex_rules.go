package agentruntime

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckCodexRules detects damaged policy files before task admission reaches
// cloning or publishing. It does not interpret Starlark or modify permissions;
// Codex remains responsible for full syntax validation and policy enforcement.
// A filesystem check also works with older CLIs without execpolicy check.
func CheckCodexRules(home string) error {
	dir := filepath.Join(home, "rules")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Codex policy directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".rules") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read Codex policy file: %w", err)
		}
		if offset := bytes.IndexByte(data, 0); offset >= 0 {
			line := bytes.Count(data[:offset], []byte{'\n'}) + 1
			return fmt.Errorf("Codex policy file %s:%d contains a NUL byte; restore the policy file before running tasks (no rules were changed)", path, line)
		}
	}
	return nil
}

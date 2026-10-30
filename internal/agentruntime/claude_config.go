package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrClaudeConfigMissing reports a Claude CLI configuration file that was
// removed or emptied while Claude's own backup copies are still present. The
// CLI refuses to start in that state, so without this check every Claude task
// fails at the agent stage with an opaque exit status instead of a runtime
// configuration error the operator can act on.
var ErrClaudeConfigMissing = errors.New("Claude configuration file is missing; restore it from a Claude backup, then rerun the failed task")

const (
	claudeConfigFileName      = ".claude.json"
	claudeConfigBackupDirName = "backups"
	claudeConfigBackupPrefix  = ".claude.json.backup."
)

// ClaudeHome resolves the operator's persistent Claude configuration directory
// before task HOME isolation, mirroring CodexHome.
func ClaudeHome(environ []string) (string, error) {
	if len(environ) == 0 {
		environ = os.Environ()
	}
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	home := values["CLAUDE_CONFIG_DIR"]
	if home == "" {
		if values["HOME"] == "" {
			return "", fmt.Errorf("CLAUDE_CONFIG_DIR or HOME is required for persistent Claude configuration")
		}
		home = filepath.Join(values["HOME"], ".claude")
	}
	return filepath.Abs(home)
}

// ClaudeConfigPath returns the Claude CLI configuration file inside configDir.
func ClaudeConfigPath(configDir string) string {
	return filepath.Join(strings.TrimSpace(configDir), claudeConfigFileName)
}

// ClaudeConfigurationFailure recognizes the Claude CLI refusing to start
// because its configuration file is absent. That is a runtime configuration
// fault: neither a task failure nor a credential rejection, so it must not be
// reported as either.
func ClaudeConfigurationFailure(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(line, "claude configuration file not found at:") {
			return true
		}
	}
	return false
}

// ClaudeConfigRestorable reports, without writing anything, whether configDir
// is in the broken state the CLI complains about: no usable configuration
// file, but at least one Claude backup to restore from. A first run has
// neither and is left alone, because the CLI writes its own file then.
func ClaudeConfigRestorable(configDir string) (bool, string, error) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return false, "", nil
	}
	if claudeConfigFileUsable(ClaudeConfigPath(configDir)) {
		return false, "", nil
	}
	backups, err := claudeConfigBackups(configDir)
	if err != nil || len(backups) == 0 {
		return false, "", err
	}
	return true, backups[0], nil
}

// RepairClaudeConfig restores the configuration file from the newest readable
// Claude backup when the live file is missing or empty. It never overwrites an
// existing configuration file and never invents one when Claude kept no
// backup. It returns the backup it restored from, or an empty string when
// nothing had to be restored.
func RepairClaudeConfig(configDir string) (string, error) {
	restorable, _, err := ClaudeConfigRestorable(configDir)
	if err != nil || !restorable {
		return "", err
	}

	configDir = strings.TrimSpace(configDir)
	configPath := ClaudeConfigPath(configDir)
	backups, err := claudeConfigBackups(configDir)
	if err != nil {
		return "", err
	}
	for _, backup := range backups {
		content, readErr := os.ReadFile(backup)
		if readErr != nil || !json.Valid(content) {
			continue
		}
		if writeErr := writeClaudeConfigFile(configPath, content); writeErr != nil {
			return "", fmt.Errorf(
				"%w: restore it manually with: cp %q %q (%v)",
				ErrClaudeConfigMissing,
				backup,
				configPath,
				writeErr,
			)
		}
		return backup, nil
	}
	return "", fmt.Errorf(
		"%w: no readable JSON backup under %s",
		ErrClaudeConfigMissing,
		filepath.Join(configDir, claudeConfigBackupDirName),
	)
}

// claudeConfigFileUsable reports whether the live configuration file must be
// left alone. Only a confirmed absence (or an empty placeholder) is
// repairable: any other stat outcome, including a permission failure, an
// unusual CLAUDE_CONFIG_DIR, or a directory in the way, stays exactly as the
// operator configured it rather than risking a false repair.
func claudeConfigFileUsable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return info.IsDir() || info.Size() > 0
}

// claudeConfigBackups returns Claude's own backup files, newest first. Claude
// names them with a millisecond timestamp suffix; entries without a numeric
// suffix sort last so a malformed name never shadows a real backup.
func claudeConfigBackups(configDir string) ([]string, error) {
	dir := filepath.Join(configDir, claudeConfigBackupDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Claude configuration backups: %w", err)
	}

	type backupFile struct {
		path  string
		name  string
		stamp int64
	}
	found := make([]backupFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, claudeConfigBackupPrefix) {
			continue
		}
		stamp, convErr := strconv.ParseInt(strings.TrimPrefix(name, claudeConfigBackupPrefix), 10, 64)
		if convErr != nil {
			stamp = -1
		}
		found = append(found, backupFile{path: filepath.Join(dir, name), name: name, stamp: stamp})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].stamp != found[j].stamp {
			return found[i].stamp > found[j].stamp
		}
		return found[i].name > found[j].name
	})

	paths := make([]string, 0, len(found))
	for _, entry := range found {
		paths = append(paths, entry.path)
	}
	return paths, nil
}

// writeClaudeConfigFile writes the restored configuration atomically so a
// concurrent Claude process never observes a half-written file.
func writeClaudeConfigFile(path string, content []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), claudeConfigFileName+".restore-*")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)

	_, writeErr := file.Write(content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

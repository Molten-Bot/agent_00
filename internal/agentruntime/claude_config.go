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

// ErrClaudeSessionScratchUnwritable reports a persistent Claude configuration
// directory that exists but cannot be extended with the CLI's per-session
// scratch directory. The CLI creates that directory the first time a task uses
// a tool, so without it every Claude invocation loses the tools that depend on
// it: the agent still reads its prompt, and can still edit files, but its shell
// commands fail, so it cannot run a build or a test.
//
// Nothing downstream can tell that apart from an agent that simply chose not to
// validate anything, so the run spends its whole review budget on changes no
// pass was able to build or test and then fails on review convergence instead
// of on the configuration fault that caused it. This is that fault, reported
// where an operator can act on it.
var ErrClaudeSessionScratchUnwritable = errors.New("Claude session scratch directory cannot be created; make the Claude configuration directory writable, then rerun the failed task")

const (
	claudeConfigFileName      = ".claude.json"
	claudeConfigBackupDirName = "backups"
	claudeConfigBackupPrefix  = ".claude.json.backup."
	claudeScratchDirName      = "session-env"
)

// claudeSandboxScratchDir returns the per-session scratch directory inside
// configDir that the Claude CLI writes to while a task runs, creating it when
// it is missing. The CLI creates it itself the first time a task uses a tool,
// which it can no longer do once only this directory, and not the whole
// configuration directory, is writable from inside the agent sandbox.
//
// The CLI fixes this location, so every task and every review pass that shares
// a configuration directory also shares this directory; per-task isolation is
// not available for it. Only the CLI's own session scratch lives here: the
// stored credentials and Claude's configuration backups sit beside it and stay
// unwritable from inside the sandbox.
//
// A configuration directory the harness cannot extend yields an empty path
// rather than an error. The CLI cannot write there either, so there is no grant
// to make, and a grant is not the place to report it: CheckClaudeSessionScratch
// runs before the first agent stage and fails the run there instead. That
// covers a directory the harness may not write into and a configuration
// directory it may not create in the first place. Anything else, including a
// file where the configuration directory belongs, is still reported.
func claudeSandboxScratchDir(configDir string) (string, error) {
	state, err := claudeSandboxScratchDirState(configDir)
	return state.Dir, err
}

// claudeScratchDirState records what creating the CLI's session scratch
// directory under a configuration directory produced.
type claudeScratchDirState struct {
	// Dir is the scratch directory to grant, empty when there is none.
	Dir string
	// ExtendErr is why the scratch directory could not be created under a
	// configuration directory that exists. The CLI still resolves that
	// directory and still tries to create the scratch directory in it, so the
	// sandbox grant is dropped rather than failed - a policy entry cannot
	// create a directory the harness itself may not create - and
	// CheckClaudeSessionScratch reports it before the first agent stage.
	ExtendErr error
}

// claudeSandboxScratchDirState creates the scratch directory and classifies the
// failure modes its two callers treat differently.
func claudeSandboxScratchDirState(configDir string) (claudeScratchDirState, error) {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return claudeScratchDirState{}, nil
	}
	dir := filepath.Join(configDir, claudeScratchDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		info, statErr := os.Stat(configDir)
		switch {
		case statErr == nil && info.IsDir():
			return claudeScratchDirState{ExtendErr: err}, nil
		case errors.Is(statErr, os.ErrNotExist):
			return claudeScratchDirState{}, nil
		}
		return claudeScratchDirState{}, fmt.Errorf("create Claude session scratch directory: %w", err)
	}
	return claudeScratchDirState{Dir: dir}, nil
}

// claudeConfigDirAcceptsWrites reports whether the harness can still create an
// entry inside configDir, which is what the remediation carried by
// ErrClaudeSessionScratchUnwritable asks an operator to restore. It is only
// consulted once creating the scratch directory has already failed, so it never
// writes into a configuration directory that is working normally.
func claudeConfigDirAcceptsWrites(configDir string) bool {
	probe, err := os.CreateTemp(configDir, ".agent00-scratch-probe-*")
	if err != nil {
		return false
	}
	path := probe.Name()
	_ = probe.Close()
	_ = os.Remove(path)
	return true
}

// CheckClaudeSessionScratch reports a configuration directory the CLI resolves
// but cannot write its session scratch directory into, so the harness can stop
// before an agent stage whose tools would fail.
//
// A configuration directory that is absent, or that cannot be created in the
// first place, is left alone: the CLI resolves its own default location in that
// case, exactly as it does today without this check.
//
// Only a directory that actually rejects writes carries the "make it writable"
// remediation. When the directory still takes writes, that remediation would
// change nothing - something else, such as a file occupying the scratch path,
// is in the way - so the creation failure is reported as itself.
func CheckClaudeSessionScratch(configDir string) error {
	state, err := claudeSandboxScratchDirState(configDir)
	if err != nil {
		return err
	}
	if state.ExtendErr == nil {
		return nil
	}
	configDir = strings.TrimSpace(configDir)
	scratchDir := filepath.Join(configDir, claudeScratchDirName)
	if claudeConfigDirAcceptsWrites(configDir) {
		return fmt.Errorf("create Claude session scratch directory %s: %w", scratchDir, state.ExtendErr)
	}
	return fmt.Errorf("%w: %s", ErrClaudeSessionScratchUnwritable, scratchDir)
}

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

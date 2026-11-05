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
// directory the CLI still resolves but that cannot be extended with the CLI's
// per-session scratch directory, whether it exists and rejects writes or is
// absent under a HOME the harness may not write into. The CLI creates that
// directory the first time a task uses a tool, so without it every Claude
// invocation loses the tools that depend on it: the agent still reads its
// prompt, and can still edit files, but its shell commands fail, so it cannot
// run a build or a test.
//
// Nothing downstream can tell that apart from an agent that simply chose not to
// validate anything, so the run spends its whole review budget on changes no
// pass was able to build or test and then fails on review convergence instead
// of on the configuration fault that caused it. This is that fault, reported
// where an operator can act on it.
var ErrClaudeSessionScratchUnwritable = errors.New("Claude session scratch directory cannot be created; make the Claude configuration directory writable, then rerun the failed task")

// ErrClaudeSessionScratch marks every failure to prepare that scratch
// directory, including the ones an operator cannot fix by changing permissions,
// such as a file occupying the scratch path. Each of them stops the same CLI at
// the same stage against the same configuration directory, so a follow-up run
// re-fails there before it can change anything. Callers recognize the whole
// class through this sentinel instead of matching message text.
var ErrClaudeSessionScratch = errors.New("Claude session scratch directory could not be prepared")

// sessionScratchFailure marks a scratch preparation failure as part of that
// class while leaving the message, and any more specific sentinel it wraps,
// exactly as reported.
type sessionScratchFailure struct{ err error }

func (f sessionScratchFailure) Error() string { return f.err.Error() }

func (f sessionScratchFailure) Unwrap() error { return f.err }

func (sessionScratchFailure) Is(target error) bool { return target == ErrClaudeSessionScratch }

func markSessionScratchFailure(err error) error {
	if err == nil {
		return nil
	}
	return sessionScratchFailure{err: err}
}

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
	// configuration directory the CLI still resolves: one that exists and
	// rejects writes, and one that is absent and cannot be created. The CLI
	// still tries to create the scratch directory there, so the sandbox grant
	// is dropped rather than failed - a policy entry cannot create a directory
	// the harness itself may not create - and CheckClaudeSessionScratch reports
	// it before the first agent stage.
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
		if (statErr == nil && info.IsDir()) || errors.Is(statErr, os.ErrNotExist) {
			return claudeScratchDirState{ExtendErr: err}, nil
		}
		return claudeScratchDirState{}, markSessionScratchFailure(fmt.Errorf("create Claude session scratch directory: %w", err))
	}
	return claudeScratchDirState{Dir: dir}, nil
}

// claudeConfigDirRefusesWrites reports whether configDir is refusing the writes
// the CLI needs, which is what the remediation carried by
// ErrClaudeSessionScratchUnwritable asks an operator to restore. It is only
// consulted once creating the scratch directory has already failed, so it never
// writes into a configuration directory that is working normally.
//
// Only an actual refusal counts. A probe that fails for an unrelated reason -
// no space left on the device, no file descriptors left - would not be fixed by
// changing permissions, so reporting it as an unwritable directory would send
// an operator after the wrong thing; the creation failure is reported as itself
// instead.
func claudeConfigDirRefusesWrites(configDir string) bool {
	probe, err := os.CreateTemp(configDir, ".agent00-scratch-probe-*")
	if err != nil {
		return isWriteRefusal(err)
	}
	path := probe.Name()
	_ = probe.Close()
	_ = os.Remove(path)
	return false
}

// isWriteRefusal reports whether err is the filesystem refusing the write: a
// denied permission, a read-only mount or sandbox policy, or a directory that
// is absent and cannot be created. Those are the failures an operator fixes by
// making the Claude configuration directory writable; every other failure is
// about the machine rather than the directory.
func isWriteRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range claudeSessionScratchRefusalMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// CheckClaudeSessionScratch reports a configuration directory the CLI resolves
// but cannot write its session scratch directory into, so the harness can stop
// before an agent stage whose tools would fail.
//
// Both shapes of that fault are reported: a directory that exists and rejects
// writes, and one that is absent and cannot be created. The CLI derives the
// same path from the same environment, so it fails on the same directory; a
// HOME the harness may not write into leaves the CLI no default location to
// fall back to either.
//
// Only a directory that actually refuses writes carries the "make it writable"
// remediation. When it does not, that remediation would change nothing -
// something else, such as a file occupying the scratch path or a full disk, is
// in the way - so the creation failure is reported as itself.
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
	if !claudeConfigDirRefusesWrites(configDir) {
		return markSessionScratchFailure(fmt.Errorf("create Claude session scratch directory %s: %w", scratchDir, state.ExtendErr))
	}
	return markSessionScratchFailure(fmt.Errorf("%w: %s", ErrClaudeSessionScratchUnwritable, scratchDir))
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

// claudeSessionScratchRefusalMarkers name the operating-system refusals the CLI
// reports when it cannot create its session scratch directory. The sandbox the
// harness wraps every Claude invocation in reports a denied write as a
// read-only filesystem, so that wording covers the sandbox-policy case as well
// as a genuinely read-only mount.
var claudeSessionScratchRefusalMarkers = []string{
	"read-only file system",
	"read only file system",
	"erofs",
	"permission denied",
	"eacces",
	"eperm",
	"operation not permitted",
}

// ClaudeSessionScratchDir returns the session scratch directory the CLI
// resolves from environ, or an empty string when environ, and the harness
// process environment it falls back to, describe no configuration directory to
// derive one from. It only computes the path; CheckClaudeSessionScratch creates
// it.
func ClaudeSessionScratchDir(environ []string) string {
	home, err := ClaudeHome(environ)
	if err != nil {
		return ""
	}
	if home = strings.TrimSpace(home); home == "" {
		return ""
	}
	return filepath.Join(home, claudeScratchDirName)
}

// ClaudeSessionScratchFailure recognizes an invocation reporting that it could
// not create scratchDir, the session scratch directory this invocation's own
// configuration directory resolves to, either from the CLI itself or from an
// agent relaying the tool failure it hit.
//
// CheckClaudeSessionScratch already covers the directory the harness process
// resolves, but the agent runs inside a sandbox with its own filesystem policy.
// When that policy does not reach the scratch directory, the harness check
// passes and the invocation still loses every tool that needs it: the agent
// reads its prompt and edits files, and its shell commands fail, so it cannot
// run a build or a test. Nothing downstream can tell that apart from an agent
// that simply chose not to validate anything, so the run goes on to spend its
// whole review budget on changes no pass was able to build or test and then
// fails on review convergence instead of on this fault.
//
// A match needs the refusal, the creation attempt and this invocation's own
// scratch path on one line. The directory name alone appears in ordinary prose,
// and a refusal alone belongs to whatever the agent was building, so neither is
// evidence on its own. Matching some other installation's scratch path is not
// evidence either: an agent quoting a failing log, a test fixture, or source it
// is changing reproduces that wording without having lost a single tool, so the
// path has to be this invocation's. An empty scratchDir leaves nothing to match
// against, so nothing is reported.
//
// The path test cannot reject one case: a run diagnosing an earlier failure on
// the same host quotes a refusal naming the directory this invocation resolves
// too. ClaudeShellToolRan supplies the counter-evidence for that one, so
// callers consult both.
func ClaudeSessionScratchFailure(output, scratchDir string) bool {
	scratchDir = strings.ToLower(strings.TrimSpace(scratchDir))
	if scratchDir == "" {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.ToLower(line)
		if !strings.Contains(line, scratchDir) {
			continue
		}
		if !strings.Contains(line, "mkdir") && !strings.Contains(line, "create") {
			continue
		}
		for _, marker := range claudeSessionScratchRefusalMarkers {
			if strings.Contains(line, marker) {
				return true
			}
		}
	}
	return false
}

// claudeShellCommandMarkers name commands an agent can only have run through a
// shell tool. The list stays narrow on purpose: a command it does not cover
// leaves the scratch report as the only evidence, which is exactly where
// ClaudeSessionScratchFailure already stands on its own.
var claudeShellCommandMarkers = []string{
	"go test",
	"go build",
	"go vet",
	"gofmt",
	"golangci-lint",
	"npm test",
	"npm run",
	"npx ",
	"pnpm ",
	"yarn ",
	"pytest",
	"cargo test",
	"cargo build",
	"vitest",
	"eslint",
	"playwright",
	"git status",
	"git diff",
}

// claudeShellCommandCompletedMarkers name the ways an agent reports that such a
// command actually finished.
var claudeShellCommandCompletedMarkers = []string{
	"passed",
	"passes",
	"passing",
	"succeeded",
	"succeeds",
	"exit code 0",
	"exit status 0",
	"exited 0",
	"no failures",
}

// claudeShellCommandNotRunMarkers name the opposite report: a command named
// precisely because the invocation could not run it. That is how an invocation
// which lost its shell tools describes the validation it owed.
var claudeShellCommandNotRunMarkers = []string{
	"could not",
	"couldn't",
	"cannot",
	"can't",
	"unable",
	"did not",
	"didn't",
	"was not",
	"wasn't",
	"not run",
	"never ran",
	"no shell",
	"unavailable",
	"skipped",
	"would have",
}

// ClaudeShellToolRan reports whether output states that this invocation ran a
// command to completion through one of the tools that need the session scratch
// directory. An invocation that lost that directory cannot: every shell tool
// call fails for it, so it reports what it was unable to run instead.
//
// This is the counter-evidence ClaudeSessionScratchFailure cannot supply. Its
// path test rejects another installation's scratch directory, but a follow-up
// run diagnosing an earlier failure on the same host quotes a refusal naming
// this invocation's own directory, and the two are identical as text. A command
// that finished tells them apart: the tools the refusal would have taken away
// are the ones that produce one.
//
// Evidence has to sit on a single line, for the same reason a scratch report
// does: a command named in one place and a success claimed in another describe
// two different things.
func ClaudeShellToolRan(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" || !claudeOutputContainsAny(line, claudeShellCommandMarkers) {
			continue
		}
		if claudeOutputContainsAny(line, claudeShellCommandNotRunMarkers) {
			continue
		}
		if claudeOutputContainsAny(line, claudeShellCommandCompletedMarkers) {
			return true
		}
	}
	return false
}

func claudeOutputContainsAny(lowerText string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(lowerText, marker) {
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

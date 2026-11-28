package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/tasktiming"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

const (
	logDirectoryName    = ".log"
	maxLogFileOpenMode  = 0o600
	maxOpenTaskLogFiles = 128
	maxAggregateLogSize = 64 * 1024 * 1024
)

const (
	logFileName           = failurefollowup.LogFileName
	legacyTaskLogFileName = failurefollowup.LegacyTaskLogFileName
	taskTimingLogFileName = "timing.log"
	fallbackLogSubdir     = failurefollowup.FallbackLogSubdir
)

type terminalLogSink interface {
	WriteLine(line string)
	Close() error
}

type taskLogMirror struct {
	mu sync.Mutex

	rootDir       string
	aggregate     *os.File
	taskFiles     map[string]*os.File
	taskFileOrder []string
	writeErr      error
}

func newDefaultTaskLogMirror() (*taskLogMirror, error) {
	rootDir, err := defaultLogRoot()
	if err != nil {
		return nil, err
	}
	return newTaskLogMirror(rootDir)
}

func newTaskLogMirror(rootDir string) (*taskLogMirror, error) {
	absRoot, err := filepath.Abs(strings.TrimSpace(rootDir))
	if err != nil {
		return nil, fmt.Errorf("resolve log root: %w", err)
	}
	if absRoot == "" {
		return nil, fmt.Errorf("log root is empty")
	}
	if err := resetLogRoot(absRoot); err != nil {
		return nil, fmt.Errorf("reset log root: %w", err)
	}

	aggregatePath := filepath.Join(absRoot, logFileName)
	aggregateFile, err := os.OpenFile(aggregatePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, maxLogFileOpenMode)
	if err != nil {
		return nil, fmt.Errorf("open aggregate log file: %w", err)
	}

	return &taskLogMirror{
		rootDir:   absRoot,
		aggregate: aggregateFile,
		taskFiles: make(map[string]*os.File),
	}, nil
}

func resetLogRoot(absRoot string) error {
	absRoot = strings.TrimSpace(absRoot)
	if absRoot == "" {
		return fmt.Errorf("log root is empty")
	}
	if filepath.Base(absRoot) != logDirectoryName {
		return fmt.Errorf("refusing to reset non-%s directory: %s", logDirectoryName, absRoot)
	}
	// Startup must never erase unresolved incidents or follow-up evidence.
	return os.MkdirAll(absRoot, 0o700)
}

func defaultLogRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return defaultLogRootForWorkingDir(wd, resetLogRoot)
}

func defaultLogRootForWorkingDir(wd string, reset func(string) error) (string, error) {
	primary := filepath.Join(workspace.NewManager().SelectBase(), "agent_00", "logs", logRootHash(wd), logDirectoryName)
	if err := reset(primary); err == nil {
		return primary, nil
	} else {
		fallback := filepath.Join(wd, logDirectoryName)
		if fallbackErr := reset(fallback); fallbackErr == nil {
			return fallback, nil
		} else {
			return "", fmt.Errorf("reset log root: %w; fallback %s: %v", err, fallback, fallbackErr)
		}
	}
}

func logRootHash(text string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.TrimSpace(text)))
	return fmt.Sprintf("%016x", h.Sum64())
}

func (m *taskLogMirror) WriteLine(line string) {
	if m == nil {
		return
	}
	trimmed := strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var problems []error

	if m.aggregate != nil {
		if _, err := m.aggregate.WriteString(trimmed + "\n"); err != nil {
			problems = append(problems, err)
		}
	}

	subdir := taskLogSubdirForLine(trimmed)
	names := []string{logFileName, legacyTaskLogFileName}
	if strings.HasPrefix(trimmed, "task_timing ") {
		names = append(names, taskTimingLogFileName)
	}
	for _, fileName := range names {
		taskFile, err := m.taskLogFileLocked(subdir, fileName)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if _, err := taskFile.WriteString(trimmed + "\n"); err != nil {
			problems = append(problems, err)
		}
	}
	if err := errors.Join(problems...); err != nil {
		if m.writeErr == nil {
			fmt.Fprintf(os.Stderr, "task log persistence failed: %v\n", err)
		}
		m.writeErr = err
	} else {
		m.writeErr = nil
	}
}

func (m *taskLogMirror) Error() error { m.mu.Lock(); defer m.mu.Unlock(); return m.writeErr }

func (m *taskLogMirror) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var problems []error
	if m.aggregate != nil {
		if err := m.aggregate.Sync(); err != nil {
			problems = append(problems, err)
		}
	}
	for _, file := range m.taskFiles {
		if err := file.Sync(); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(append(problems, m.writeErr)...)
}

// maintainAggregate bounds the shared diagnostic log. Per-task mirrors keep
// their own retention rules because they contain delivery and recovery evidence.
func (m *taskLogMirror) maintainAggregate(limit int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for path, file := range m.taskFiles {
		current, err := os.Stat(path)
		saved, statErr := file.Stat()
		if !errors.Is(err, os.ErrNotExist) && (err != nil || statErr != nil || os.SameFile(current, saved)) {
			continue
		}
		// Release disk blocks for task logs unlinked by maintenance.
		_ = file.Close()
		delete(m.taskFiles, path)
		m.taskFileOrder = slices.DeleteFunc(m.taskFileOrder, func(name string) bool { return name == path })
	}
	if m.aggregate == nil {
		return nil
	}
	info, err := m.aggregate.Stat()
	if err != nil || info.Size() < limit {
		return err
	}
	current := filepath.Join(m.rootDir, logFileName)
	previous := current + ".previous"
	next, err := os.CreateTemp(m.rootDir, ".aggregate-*")
	if err != nil {
		return err
	}
	defer os.Remove(next.Name())
	if err := os.Rename(current, previous); err != nil {
		next.Close()
		return err
	}
	if err := os.Rename(next.Name(), current); err != nil {
		next.Close()
		return errors.Join(err, os.Rename(previous, current))
	}
	old := m.aggregate
	m.aggregate = next
	return old.Close()
}

func (m *taskLogMirror) Close() error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	if m.aggregate != nil {
		if err := m.aggregate.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		m.aggregate = nil
	}
	for _, taskFile := range m.taskFiles {
		if err := taskFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.taskFiles = nil
	m.taskFileOrder = nil

	return firstErr
}

func (m *taskLogMirror) taskLogFileLocked(subdir, fileName string) (*os.File, error) {
	taskFilePath, err := m.taskLogFilePathLocked(subdir, fileName)
	if err != nil {
		return nil, err
	}
	if existing := m.taskFiles[taskFilePath]; existing != nil {
		return existing, nil
	}
	if m.taskFiles == nil {
		m.taskFiles = make(map[string]*os.File)
	}
	if len(m.taskFiles) >= maxOpenTaskLogFiles {
		m.closeOldestTaskFileLocked()
	}

	taskFile, err := os.OpenFile(taskFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, maxLogFileOpenMode)
	if err != nil {
		return nil, err
	}
	m.taskFiles[taskFilePath] = taskFile
	m.taskFileOrder = append(m.taskFileOrder, taskFilePath)
	return taskFile, nil
}

func (m *taskLogMirror) closeOldestTaskFileLocked() {
	if len(m.taskFileOrder) == 0 {
		return
	}
	oldestPath := m.taskFileOrder[0]
	m.taskFileOrder = m.taskFileOrder[1:]
	taskFile := m.taskFiles[oldestPath]
	delete(m.taskFiles, oldestPath)
	if taskFile != nil {
		_ = taskFile.Close()
	}
}

func (m *taskLogMirror) taskLogFilePathLocked(subdir, fileName string) (string, error) {
	subdir = strings.TrimSpace(subdir)
	if subdir == "" {
		subdir = fallbackLogSubdir
	}
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		fileName = logFileName
	}

	dirPath := filepath.Join(m.rootDir, subdir)
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		return "", err
	}

	return filepath.Join(dirPath, fileName), nil
}

func taskLogSubdirForLine(line string) string {
	fields := parseSimpleKVFields(line)

	if subdir, ok := failurefollowup.IdentifierSubdir(fields["request_id"]); ok {
		return subdir
	}
	if subdir, ok := failurefollowup.IdentifierSubdir(fields["session"]); ok {
		return subdir
	}
	return fallbackLogSubdir
}

// TaskTiming reads the same evidence displayed in the task log, including
// prior attempts whose broker state has already expired or been restarted.
func (m *taskLogMirror) TaskTiming(ctx context.Context, requestID string) ([]tasktiming.Span, error) {
	dir, ok := taskLogDir(m.rootDir, requestID)
	if !ok {
		return nil, nil
	}
	file, err := os.Open(filepath.Join(dir, taskTimingLogFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return tasktiming.Read(ctx, file, requestID)
}

// Cleanup after merge drops the transcript but keeps stage evidence available
// for Studio history. Close cached handles before unlinking either transcript.
func (m *taskLogMirror) removeTaskTranscripts(dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for _, name := range []string{logFileName, legacyTaskLogFileName} {
		path := filepath.Join(dir, name)
		if file := m.taskFiles[path]; file != nil {
			errs = append(errs, file.Close())
			delete(m.taskFiles, path)
			m.taskFileOrder = slices.DeleteFunc(m.taskFileOrder, func(p string) bool { return p == path })
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

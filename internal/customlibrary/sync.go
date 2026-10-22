package customlibrary

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/library"
)

// DefaultRoot mirrors the ./.agent00 runtime-state convention already used
// by internal/localruntime's runtime config file.
const DefaultRoot = "./.agent00/library-sources"

// SyncResult reports what one Sync call did.
type SyncResult struct {
	Source    Source
	CommitSHA string
	TaskCount int
	Changed   bool
	SyncedAt  time.Time
}

// Syncer clones/refreshes configured sources under Root and converts each
// source's ./prompt/*.md files into a library catalog directory at
// Root/<name>/generated, which library.LoadCustomizedCatalog merges in.
type Syncer struct {
	Root   string
	Runner execx.Runner
}

func (s Syncer) runner() execx.Runner {
	if s.Runner != nil {
		return s.Runner
	}
	return execx.OSRunner{}
}

func (s Syncer) root() string {
	if strings.TrimSpace(s.Root) != "" {
		return s.Root
	}
	return DefaultRoot
}

func (s Syncer) sourceDir(name string) string { return filepath.Join(s.root(), name) }
func (s Syncer) repoDir(name string) string   { return filepath.Join(s.sourceDir(name), "repo") }
func (s Syncer) generatedDir(name string) string {
	return filepath.Join(s.sourceDir(name), "generated")
}
func (s Syncer) promptDir(name string) string { return filepath.Join(s.repoDir(name), "prompt") }

// GeneratedDir is the catalog directory produced for name once Sync has
// succeeded at least once.
func (s Syncer) GeneratedDir(name string) string { return s.generatedDir(name) }

// RemoveSource deletes every cached/generated file for a removed source.
func (s Syncer) RemoveSource(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil
	}
	return os.RemoveAll(s.sourceDir(name))
}

// Sync clones (first run) or fetches (subsequent runs) src.Repo and
// regenerates its library catalog. Authentication is whatever the host
// process already has configured (gh auth, ssh-agent, credential helper);
// this package stores no credentials of its own.
//
// On any failure, a previously generated catalog (if one exists) is left in
// place, so a transient or newly broken source does not remove tasks that
// were already working.
func (s Syncer) Sync(ctx context.Context, src Source) (SyncResult, error) {
	src, err := ValidateSource(src)
	if err != nil {
		return SyncResult{}, err
	}

	repoDir := s.repoDir(src.Name)
	beforeSHA, _ := s.gitIn(ctx, repoDir, "rev-parse", "HEAD")

	if _, statErr := os.Stat(filepath.Join(repoDir, ".git")); statErr == nil {
		if _, err := s.gitIn(ctx, repoDir, "fetch", "--depth", "1", "origin", "HEAD"); err != nil {
			return SyncResult{}, fmt.Errorf("fetch %s: %w", src.Name, err)
		}
		if _, err := s.gitIn(ctx, repoDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return SyncResult{}, fmt.Errorf("reset %s: %w", src.Name, err)
		}
	} else {
		if err := os.MkdirAll(s.sourceDir(src.Name), 0o700); err != nil {
			return SyncResult{}, fmt.Errorf("prepare %s: %w", src.Name, err)
		}
		if err := os.RemoveAll(repoDir); err != nil {
			return SyncResult{}, fmt.Errorf("reset workspace for %s: %w", src.Name, err)
		}
		if _, err := s.run(ctx, execx.Command{Name: "git", Args: []string{"clone", "--depth", "1", src.Repo, repoDir}}); err != nil {
			return SyncResult{}, fmt.Errorf("clone %s: %w", src.Name, err)
		}
	}

	afterSHA, err := s.gitIn(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return SyncResult{}, fmt.Errorf("resolve HEAD for %s: %w", src.Name, err)
	}
	afterSHA = strings.TrimSpace(afterSHA)

	tasks, err := loadPromptMarkdown(s.promptDir(src.Name), src.Name)
	if err != nil {
		return SyncResult{}, fmt.Errorf("%s: %w", src.Name, err)
	}

	if err := s.writeGeneratedCatalog(src.Name, tasks); err != nil {
		return SyncResult{}, fmt.Errorf("%s: %w", src.Name, err)
	}

	return SyncResult{
		Source:    src,
		CommitSHA: afterSHA,
		TaskCount: len(tasks),
		Changed:   strings.TrimSpace(beforeSHA) != afterSHA,
		SyncedAt:  time.Now(),
	}, nil
}

func (s Syncer) run(ctx context.Context, cmd execx.Command) (string, error) {
	res, err := s.runner().Run(ctx, cmd)
	return res.Stdout, err
}

func (s Syncer) gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	return s.run(ctx, execx.Command{Name: "git", Dir: dir, Args: args})
}

// writeGeneratedCatalog stages tasks into a temporary directory, validates
// it through the exact loader production code uses, and only then swaps it
// in atomically. A source whose markdown fails validation never overwrites
// its last-known-good generated catalog.
func (s Syncer) writeGeneratedCatalog(name string, tasks []library.TaskDefinition) error {
	genDir := s.generatedDir(name)
	tmpDir := genDir + ".tmp"

	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("clear staging directory: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	for _, task := range tasks {
		data, err := json.MarshalIndent(map[string]library.TaskDefinition{task.Name: task}, "", "  ")
		if err != nil {
			return fmt.Errorf("encode task %q: %w", task.Name, err)
		}
		path := filepath.Join(tmpDir, task.Name+".json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("write task %q: %w", task.Name, err)
		}
	}

	if _, err := library.LoadCatalog(tmpDir); err != nil {
		return fmt.Errorf("validate generated catalog: %w", err)
	}

	if err := os.RemoveAll(genDir); err != nil {
		return fmt.Errorf("clear previous catalog: %w", err)
	}
	if err := os.Rename(tmpDir, genDir); err != nil {
		return fmt.Errorf("activate generated catalog: %w", err)
	}
	return nil
}

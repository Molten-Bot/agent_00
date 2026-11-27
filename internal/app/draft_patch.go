package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

const maxDraftPatchBytes = 8 << 20

const draftPatchCommitsQuery = "{base: .base.sha, head: .head.sha, baseRef: .base.ref, headRef: .head.ref, state, merged, draft}"

var errDraftPatchTooLarge = fmt.Errorf("%w: reviewed patch evidence exceeds the %d-byte file limit; reduce the patch size and retry", ErrReviewPending, maxDraftPatchBytes)

type draftPatchCommits struct {
	Base string `json:"base"`
	Head string `json:"head"`
}

type draftReviewPatch struct {
	commits draftPatchCommits
	path    string
	size    int
	digest  [sha256.Size]byte
}

func (h Harness) prepareDraftReviewPatch(ctx context.Context, repo repoWorkspace, baseRef, head string) (*draftReviewPatch, string, error) {
	if head == "" {
		return nil, "", fmt.Errorf("%w: missing head for the reviewed patch", ErrReviewPending)
	}
	baseResult, err := h.runCommand(ctx, "review", gitCommand(repo.Dir, "rev-parse", "--verify", baseRef+"^{commit}"))
	base := strings.TrimSpace(baseResult.Stdout)
	if err != nil || base == "" {
		return nil, "", fmt.Errorf("%w: could not resolve the base commit for the reviewed patch", ErrReviewPending)
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(repo.Dir))
	if err == nil {
		root, err = filepath.Abs(root)
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: resolve reviewed patch evidence directory: %w", ErrReviewPending, err)
	}
	dir, err := os.MkdirTemp(root, ".draft-review-*")
	if err != nil {
		return nil, "", fmt.Errorf("%w: create reviewed patch evidence directory: %w", ErrReviewPending, err)
	}
	patch := &draftReviewPatch{commits: draftPatchCommits{Base: base, Head: head}, path: filepath.Join(dir, "reviewed.patch")}
	keep := false
	defer func() {
		if !keep {
			patch.remove()
		}
	}()
	f, err := os.OpenFile(patch.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("%w: create reviewed patch evidence: %w", ErrReviewPending, err)
	}
	// Capture exact Git output, including binary changes, without streaming
	// patch content into logs or writing beyond the artifact limit.
	writer := &draftPatchWriter{file: f, remaining: maxDraftPatchBytes}
	cmd := reviewDiffPatchCommand(repo.Dir, base, head)
	cmd.Args = append(cmd.Args, "--no-textconv", "--binary", "--no-color")
	cmd.StdoutWriter = writer
	_, err = h.runCommand(ctx, "review", cmd)
	closeErr := f.Close()
	if writer.exceeded {
		return nil, "", errDraftPatchTooLarge
	}
	if err := errors.Join(err, closeErr); err != nil {
		return nil, "", fmt.Errorf("%w: generate reviewed patch evidence: %w", ErrReviewPending, err)
	}
	if err := os.Chmod(patch.path, 0o400); err != nil {
		return nil, "", fmt.Errorf("%w: protect reviewed patch evidence: %w", ErrReviewPending, err)
	}
	data, err := patch.read()
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("%w: missing patch for the reviewed draft head", ErrReviewPending)
	}
	patch.size, patch.digest = len(data), sha256.Sum256(data)
	keep = true
	return patch, string(data), nil
}

type draftPatchWriter struct {
	file      *os.File
	remaining int
	exceeded  bool
}

func (w *draftPatchWriter) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		w.exceeded = true
		return 0, errDraftPatchTooLarge
	}
	n, err := w.file.Write(data)
	w.remaining -= n
	return n, err
}

func (p *draftReviewPatch) read() ([]byte, error) {
	f, err := os.Open(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: missing reviewed patch evidence file", ErrReviewPending)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: open reviewed patch evidence: %w", ErrReviewPending, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxDraftPatchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read reviewed patch evidence: %w", ErrReviewPending, err)
	}
	if len(data) > maxDraftPatchBytes {
		return nil, errDraftPatchTooLarge
	}
	return data, nil
}

func (p *draftReviewPatch) checkCommits(commits draftPatchCommits) error {
	if p == nil {
		return fmt.Errorf("%w: missing patch for the reviewed draft head", ErrReviewPending)
	}
	if commits.Head == "" || p.commits.Head != commits.Head {
		return fmt.Errorf("reviewed patch evidence does not match the current draft head: %w", ErrReviewHeadChanged)
	}
	if commits.Base == "" || p.commits.Base != commits.Base {
		return fmt.Errorf("%w: reviewed patch evidence has a stale draft base; collect a fresh review and retry", ErrReviewPending)
	}
	return nil
}

func (p *draftReviewPatch) verify(commits draftPatchCommits) (string, error) {
	if err := p.checkCommits(commits); err != nil {
		return "", err
	}
	data, err := p.read()
	if err != nil {
		return "", err
	}
	if len(data) < p.size {
		return "", fmt.Errorf("%w: reviewed patch evidence is truncated (%d of %d bytes)", ErrReviewPending, len(data), p.size)
	}
	if len(data) != p.size || sha256.Sum256(data) != p.digest {
		return "", fmt.Errorf("%w: reviewed patch evidence does not match the captured patch digest", ErrReviewPending)
	}
	return string(data), nil
}

func (p *draftReviewPatch) reference() string {
	return fmt.Sprintf("Read the complete patch in the read-only file %q. Read it in consecutive chunks as needed and cover the full file before reporting a verdict.\nBase commit: %s\nHead commit: %s\nBytes: %d\nSHA-256: %x",
		p.path, p.commits.Base, p.commits.Head, p.size, p.digest)
}

func (p *draftReviewPatch) remove() {
	if p != nil {
		_ = os.RemoveAll(filepath.Dir(p.path))
	}
}

func (h Harness) confirmDraftPatchCommits(ctx context.Context, repo repoWorkspace) error {
	commits, err := h.loadDraftPatchCommits(ctx, repo)
	if err != nil {
		return err
	}
	if err := repo.ReviewedPatch.checkCommits(commits); err != nil {
		return err
	}
	// PR metadata can lag behind the base ref on origin, where the patch base was fetched.
	ref := "refs/heads/" + normalizeBranchRef(repo.BaseBranch)
	remote, err := h.runCommand(ctx, "pr_verification", gitCommand(repo.Dir, "ls-remote", "--heads", publishRemoteOrigin, ref))
	if err != nil {
		return fmt.Errorf("%w: confirm published draft base: %w", ErrReviewPending, err)
	}
	commits.Base = remoteBranchHead(remote.Stdout, repo.BaseBranch)
	if commits.Base == "" {
		return fmt.Errorf("%w: missing published draft base commit for the reviewed patch", ErrReviewPending)
	}
	return repo.ReviewedPatch.checkCommits(commits)
}

func (h Harness) loadDraftPatchCommits(ctx context.Context, repo repoWorkspace) (draftPatchCommits, error) {
	endpoint := strings.TrimSuffix(githubutil.ReviewEndpoint(repo.PRURL), "/reviews")
	cmd := ghCommand(repo.Dir, "api", endpoint, "--jq", draftPatchCommitsQuery)
	if repo.ReviewSanitize {
		cmd = commandWithoutGitHubTokenEnv(cmd)
	}
	sleep := h.Sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	confirmRestoredDraft := true
	poll := newCheckPoll("pr_verification", repo.PRURL)
	for remaining := h.prChecksWatchTimeout(); ; {
		res, err := h.runCheckQuery(ctx, "pr_verification", cmd, checkQueryTimeout)
		if err != nil {
			return draftPatchCommits{}, fmt.Errorf("%w: load current draft base and head commits: %w", ErrReviewPending, err)
		}
		var state struct {
			draftPatchCommits
			BaseRef string `json:"baseRef"`
			HeadRef string `json:"headRef"`
			State   string `json:"state"`
			Merged  bool   `json:"merged"`
			Draft   *bool  `json:"draft"`
		}
		if json.Unmarshal([]byte(res.Stdout), &state) != nil || state.Base == "" || state.Head == "" || state.Draft == nil {
			return draftPatchCommits{}, fmt.Errorf("%w: missing or invalid draft base and head commits for the reviewed patch", ErrReviewPending)
		}
		if state.Merged {
			return draftPatchCommits{}, errMergedBeforeDraftVerification
		}
		if state.State != "open" || state.BaseRef != repo.BaseBranch || state.HeadRef != repo.Branch {
			return draftPatchCommits{}, fmt.Errorf("%w: expected the same open pull request and branches when checking reviewed patch evidence", ErrReviewPending)
		}
		if *state.Draft {
			return state.draftPatchCommits, nil
		}
		if err := repo.ReviewedPatch.checkCommits(state.draftPatchCommits); err != nil {
			return draftPatchCommits{}, err
		}
		refreshedAt := time.Now()
		if err := h.ensurePullRequestDraft(ctx, repo, "pr_verification"); err != nil {
			return draftPatchCommits{}, err
		}
		if confirmRestoredDraft {
			confirmRestoredDraft = false
			continue
		}
		if remaining <= 0 {
			return draftPatchCommits{}, errDraftRestoring
		}
		h.suspendWorkspace()
		delay := min(remaining, checkPollDelay(0, 0))
		poll.report(h, "wait_for_reviewed_head_evidence", repo.ReviewedPatch.commits.Head, nil, refreshedAt, delay)
		if err := sleep(ctx, delay); err != nil {
			return draftPatchCommits{}, err
		}
		remaining -= delay
	}
}

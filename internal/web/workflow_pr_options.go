package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

func (s Server) handleWorkflowPROptions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	repo, ok := validGitHubRepository(r.URL.Query().Get("repo"))
	if !ok {
		workflowError(w, http.StatusBadRequest, fmt.Errorf("repo must be owner/name"))
		return
	}
	allowed, err := s.RepositoryAllowed(r.Context(), repo)
	if err != nil {
		workflowError(w, http.StatusInternalServerError, err)
		return
	}
	if !allowed {
		workflowError(w, http.StatusForbidden, errors.New("repository is disabled in Settings"))
		return
	}
	ctx := githubutil.WithCaller(r.Context(), "workflow_options")
	assignees, err := s.workflowPROptionNames(ctx, repo, "assignees", "login")
	if err != nil {
		workflowError(w, http.StatusBadGateway, err)
		return
	}
	labels, err := s.workflowPROptionNames(ctx, repo, "labels", "name")
	if err != nil {
		workflowError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"assignees": assignees, "labels": labels})
}

func (s Server) workflowPROptionNames(ctx context.Context, repo, resource, field string) ([]string, error) {
	result, err := s.runIssueGH(ctx, execx.Command{
		Name: "gh",
		Args: []string{"api", "--paginate", "repos/" + repo + "/" + resource + "?per_page=100"},
	})
	if err != nil {
		return nil, fmt.Errorf("load repository %s: %w", resource, err)
	}
	names := []string{}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	for {
		var page []map[string]json.RawMessage
		if err := decoder.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode repository %s: %w", resource, err)
		}
		for _, entry := range page {
			var name string
			if err := json.Unmarshal(entry[field], &name); err != nil {
				return nil, fmt.Errorf("decode repository %s: %w", resource, err)
			}
			if name != "" {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

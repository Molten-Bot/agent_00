package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestWorkflowPROptionsListsRepositoryAssigneesAndLabels(t *testing.T) {
	t.Parallel()
	s := NewServer("", NewBroker())
	s.IssueRecovery = &localruntime.IssueRecoveryConfig{}
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		switch strings.Join(cmd.Args, " ") {
		case "api --include --paginate repos/acme/repo/assignees?per_page=100":
			return execx.Result{Stdout: "HTTP/2.0 200 OK\r\n\r\n" + `[{"login":"teammate"}]` + "\nHTTP/2.0 200 OK\r\n\r\n" + `[{"login":"maintainer"},{"login":"teammate"}]`}, nil
		case "api --include --paginate repos/acme/repo/labels?per_page=100":
			return execx.Result{Stdout: `[{"name":"tracking"},{"name":"priority, high"}]`}, nil
		default:
			t.Fatalf("unexpected GitHub request: %v", cmd.Args)
			return execx.Result{}, nil
		}
	})
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workflows/pr-options?repo=acme/repo", nil))
	var options map[string][]string
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &options) != nil {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if !slices.Equal(options["assignees"], []string{"maintainer", "teammate"}) ||
		!slices.Equal(options["labels"], []string{"priority, high", "tracking"}) {
		t.Fatalf("unexpected PR options: %v", options)
	}

	response = httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workflows/pr-options?repo=acme/../other", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid repository response = %d %s", response.Code, response.Body.String())
	}
}

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestStarAgent00RepositoryRejectsEmptyToken(t *testing.T) {
	t.Parallel()

	if err := starAgent00Repository(nil, "   "); err == nil || err.Error() != "github token is required" {
		t.Fatalf("starAgent00Repository(empty token) error = %v, want required", err)
	}
}

func TestStarAgent00RepositoryRejectsEmptyBaseURL(t *testing.T) {
	previousBaseURL := githubStarAPIBaseURL
	githubStarAPIBaseURL = "   "
	t.Cleanup(func() { githubStarAPIBaseURL = previousBaseURL })

	if err := starAgent00Repository(nil, "ghp_token"); err == nil || !strings.Contains(err.Error(), "base URL is required") {
		t.Fatalf("starAgent00Repository(empty base url) error = %v, want base URL required", err)
	}
}

func TestStarAgent00RepositorySucceedsWithNilContextAndDefaultClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	previousBaseURL := githubStarAPIBaseURL
	previousClient := githubStarHTTPClient
	githubStarAPIBaseURL = server.URL
	githubStarHTTPClient = nil // exercise the http.DefaultClient fallback
	t.Cleanup(func() {
		githubStarAPIBaseURL = previousBaseURL
		githubStarHTTPClient = previousClient
	})

	if err := starAgent00Repository(nil, "ghp_token"); err != nil {
		t.Fatalf("starAgent00Repository(nil ctx, nil client) error = %v", err)
	}
}

func TestStarAgent00RepositoryReturnsDoError(t *testing.T) {
	previousBaseURL := githubStarAPIBaseURL
	previousClient := githubStarHTTPClient
	// Port 0 on the loopback address is never listening, so the request
	// should fail fast with a connection error rather than hang.
	githubStarAPIBaseURL = "http://127.0.0.1:0"
	githubStarHTTPClient = &http.Client{}
	t.Cleanup(func() {
		githubStarAPIBaseURL = previousBaseURL
		githubStarHTTPClient = previousClient
	})

	err := starAgent00Repository(nil, "ghp_token")
	if err == nil || !strings.Contains(err.Error(), "star Molten-Bot/agent_00") {
		t.Fatalf("starAgent00Repository(unreachable server) error = %v, want wrapped star error", err)
	}
}

func TestStarAgent00RepositoryUsesStatusTextWhenBodyEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	previousBaseURL := githubStarAPIBaseURL
	previousClient := githubStarHTTPClient
	githubStarAPIBaseURL = server.URL
	githubStarHTTPClient = server.Client()
	t.Cleanup(func() {
		githubStarAPIBaseURL = previousBaseURL
		githubStarHTTPClient = previousClient
	})

	err := starAgent00Repository(nil, "ghp_token")
	if err == nil || !strings.Contains(err.Error(), http.StatusText(http.StatusForbidden)) {
		t.Fatalf("starAgent00Repository(empty body) error = %v, want status text detail", err)
	}
}

func TestConfigureGitHubTokenAndApplyReturnsZeroStateWhenOnSuccessNil(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	previousBaseURL := githubStarAPIBaseURL
	previousClient := githubStarHTTPClient
	githubStarAPIBaseURL = server.URL
	githubStarHTTPClient = server.Client()
	t.Cleanup(func() {
		githubStarAPIBaseURL = previousBaseURL
		githubStarHTTPClient = previousClient
	})

	path := filepath.Join(t.TempDir(), ".agent00", "config.json")
	state, err := configureGitHubTokenAndApply(
		nil,
		"test",
		path,
		localruntime.InitConfig{},
		nil,
		"ghp_onsuccess_nil",
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("configureGitHubTokenAndApply(onSuccess nil) error = %v", err)
	}
	if state.Required || state.Ready || state.State != "" || state.Message != "" {
		t.Fatalf("configureGitHubTokenAndApply(onSuccess nil) state = %+v, want zero value", state)
	}
}

func TestWithTemporaryGitHubTokenEnvironmentNilRunIsNoop(t *testing.T) {
	t.Parallel()

	if err := withTemporaryGitHubTokenEnvironment("token", nil); err != nil {
		t.Fatalf("withTemporaryGitHubTokenEnvironment(nil run) error = %v", err)
	}
}

func TestWithTemporaryGitHubTokenEnvironmentRestoresUnsetVars(t *testing.T) {
	previousGH, hadGH := os.LookupEnv("GH_TOKEN")
	previousGitHub, hadGitHub := os.LookupEnv("GITHUB_TOKEN")
	_ = os.Unsetenv("GH_TOKEN")
	_ = os.Unsetenv("GITHUB_TOKEN")
	t.Cleanup(func() {
		if hadGH {
			_ = os.Setenv("GH_TOKEN", previousGH)
		} else {
			_ = os.Unsetenv("GH_TOKEN")
		}
		if hadGitHub {
			_ = os.Setenv("GITHUB_TOKEN", previousGitHub)
		} else {
			_ = os.Unsetenv("GITHUB_TOKEN")
		}
	})

	var sawDuringRun string
	err := withTemporaryGitHubTokenEnvironment("temp-token", func() error {
		sawDuringRun = os.Getenv("GH_TOKEN")
		return nil
	})
	if err != nil {
		t.Fatalf("withTemporaryGitHubTokenEnvironment() error = %v", err)
	}
	if sawDuringRun != "temp-token" {
		t.Fatalf("GH_TOKEN during run = %q, want temp-token", sawDuringRun)
	}
	if _, stillSet := os.LookupEnv("GH_TOKEN"); stillSet {
		t.Fatal("GH_TOKEN still set after restore, want unset")
	}
	if _, stillSet := os.LookupEnv("GITHUB_TOKEN"); stillSet {
		t.Fatal("GITHUB_TOKEN still set after restore, want unset")
	}
}

func TestWithTemporaryGitHubTokenEnvironmentPropagatesRunError(t *testing.T) {
	t.Setenv("GH_TOKEN", "existing")
	t.Setenv("GITHUB_TOKEN", "existing")

	wantErr := errors.New("boom")
	err := withTemporaryGitHubTokenEnvironment("temp-token", func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("withTemporaryGitHubTokenEnvironment(run error) = %v, want %v", err, wantErr)
	}
	if got := os.Getenv("GH_TOKEN"); got != "existing" {
		t.Fatalf("GH_TOKEN after restore = %q, want existing", got)
	}
}

func TestValidateGitHubTokenRejectsEmptyToken(t *testing.T) {
	t.Parallel()

	if err := validateGitHubToken(nil, nil, "   "); err == nil || err.Error() != "github token is required" {
		t.Fatalf("validateGitHubToken(empty) error = %v, want required", err)
	}
}

func TestValidateGitHubTokenSkipsValidationWithNilRunner(t *testing.T) {
	t.Parallel()

	if err := validateGitHubToken(nil, nil, "ghp_token"); err != nil {
		t.Fatalf("validateGitHubToken(nil runner) error = %v, want nil", err)
	}
}

func TestDecodeJSONStrictRejectsTrailingData(t *testing.T) {
	t.Parallel()

	var payload struct {
		Value string `json:"value"`
	}

	if err := decodeJSONStrict(`{"value":"ok"} garbage`, &payload); err == nil {
		t.Fatal("decodeJSONStrict(trailing garbage) error = nil, want syntax error")
	}

	if err := decodeJSONStrict(`{"value":"ok"} {"value":"again"}`, &payload); err == nil || !strings.Contains(err.Error(), "unexpected trailing JSON data") {
		t.Fatalf("decodeJSONStrict(trailing valid JSON) error = %v, want unexpected trailing JSON data", err)
	}
}

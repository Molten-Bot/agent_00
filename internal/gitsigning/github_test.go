package gitsigning

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitHubIdentityRegistrationAndErrors(t *testing.T) {
	t.Parallel()
	cfg, err := Import(t.TempDir(), testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-test" {
			t.Error("missing auth")
		}
		switch r.URL.Path {
		case "/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 123, "name": "Test Bot"})
		case "/users/test-bot/ssh_signing_keys":
			if r.URL.Query().Get("page") == "1" {
				keys := make([]map[string]string, 100)
				for i := range keys {
					keys[i] = map[string]string{"key": "invalid"}
				}
				json.NewEncoder(w).Encode(keys)
			} else {
				json.NewEncoder(w).Encode([]map[string]string{{"key": cfg.PublicKey + " comment"}})
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("secret response body"))
		}
	}))
	defer server.Close()
	github := GitHub{Client: server.Client(), BaseURL: server.URL, Token: "token-test"}
	cfg, err = github.Identity(context.Background(), cfg)
	if err != nil || cfg.Email != "123+test-bot@users.noreply.github.com" {
		t.Fatalf("identity: %+v %v", cfg, err)
	}
	ok, err := github.Registered(context.Background(), cfg)
	if err != nil || !ok {
		t.Fatalf("registration: %v %v", ok, err)
	}
	cfg.Account = "other"
	_, err = github.Registered(context.Background(), cfg)
	if !errors.Is(err, ErrSetup) {
		t.Fatalf("expected auth error: %v", err)
	}
	if err.Error() != ErrSetup.Error()+": GitHub returned HTTP 401" {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestGitHubThrottledResponse(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		message string
		delay   time.Duration
	}{
		{"primary", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1700003600"}, "private detail", time.Hour},
		{"secondary", 403, nil, "You have exceeded a secondary rate limit: private detail", time.Minute},
		{"retry", 429, map[string]string{"Retry-After": "120"}, "private detail", 2 * time.Minute},
		{"permission", 403, nil, "private detail", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]string{"message": tc.message})
			}))
			defer server.Close()
			client := GitHub{BaseURL: server.URL, Client: server.Client(), Cooldown: &githubutil.Cooldown{Now: func() time.Time { return now }}}
			_, err := client.Identity(t.Context(), Config{})
			if err == nil || strings.Contains(err.Error(), "private detail") {
				t.Fatalf("diagnostic = %v", err)
			}
			var limited *githubutil.RateLimitError
			if tc.delay == 0 {
				if errors.As(err, &limited) || !errors.Is(err, ErrSetup) {
					t.Fatalf("expected setup failure, got %v", err)
				}
				return
			}
			if !errors.As(err, &limited) || !limited.RetryAt.Equal(now.Add(tc.delay)) || !strings.Contains(err.Error(), "identity verification pending") {
				t.Fatalf("unexpected retry: %v", err)
			}
		})
	}
}

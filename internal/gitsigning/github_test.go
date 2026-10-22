package gitsigning

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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

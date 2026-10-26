package gitsigning

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// roundTripFunc lets a test supply canned HTTP responses (or transport
// errors) without ever opening a real network socket, including for
// requests whose URL host is not under test control (e.g. the default
// "https://api.github.com" base).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestGetDefaultsBaseURLWithoutRealNetwork exercises the "BaseURL is empty"
// branch of get, which falls back to the real GitHub API host. A fake
// RoundTripper intercepts the request before any socket is opened so the
// test never touches the network, while still proving the default host and
// path were used.
func TestGetDefaultsBaseURLWithoutRealNetwork(t *testing.T) {
	t.Parallel()
	var capturedURL string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		capturedURL = r.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    r,
		}, nil
	})}
	g := GitHub{Client: client, BaseURL: ""}
	var dst struct{}
	// The canned response above has no body, so decoding will fail; that's
	// fine here since we only care that the request targeted the default
	// host without touching the network. A dedicated test below exercises
	// the malformed-body branch with a real body.
	_ = g.get(context.Background(), "/user", &dst)
	if !strings.HasPrefix(capturedURL, "https://api.github.com/user") {
		t.Fatalf("get() with empty BaseURL used URL %q, want it to target the default GitHub API host", capturedURL)
	}
}

// TestGetWrapsRequestConstructionError exercises the http.NewRequestWithContext
// error branch: a raw control character in the path makes URL parsing fail
// before any request is ever sent.
func TestGetWrapsRequestConstructionError(t *testing.T) {
	t.Parallel()
	g := GitHub{BaseURL: "https://example.invalid"}
	err := g.get(context.Background(), "/bad\npath", &struct{}{})
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "cannot prepare GitHub request") {
		t.Fatalf("get(invalid path) error = %v, want cannot prepare GitHub request error", err)
	}
}

// TestGetUsesDefaultClientAndOmitsAuthHeaderWhenTokenEmpty exercises the
// "Client is nil" branch (a default *http.Client is constructed) together
// with the no-token path, using a local httptest.Server so no external
// network access occurs.
func TestGetUsesDefaultClientAndOmitsAuthHeaderWhenTokenEmpty(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("unexpected Authorization header %q for empty token", auth)
		}
		json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 1})
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, Token: ""}
	var dst struct {
		Login string `json:"login"`
	}
	if err := g.get(context.Background(), "/user", &dst); err != nil {
		t.Fatalf("get() with nil Client error = %v", err)
	}
	if dst.Login != "test-bot" {
		t.Fatalf("get() decoded login = %q, want test-bot", dst.Login)
	}
}

// TestGetWrapsTransportError exercises client.Do returning a transport-level
// error (simulated network failure), which get must translate into a safe
// ErrSetup message without leaking the underlying error text.
func TestGetWrapsTransportError(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("simulated dial failure: should not leak")
	})}
	g := GitHub{Client: client, BaseURL: "https://example.invalid"}
	err := g.get(context.Background(), "/user", &struct{}{})
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "GitHub is unavailable; try again") {
		t.Fatalf("get(transport error) error = %v, want GitHub unavailable error", err)
	}
	if strings.Contains(err.Error(), "simulated dial failure") {
		t.Fatal("transport error details leaked into returned error")
	}
}

// TestGetWrapsMalformedJSONResponse exercises the response-decode error
// branch when GitHub returns a 200 with a body that isn't valid JSON.
func TestGetWrapsMalformedJSONResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{not valid json"))
	}))
	defer server.Close()
	g := GitHub{Client: server.Client(), BaseURL: server.URL}
	err := g.get(context.Background(), "/user", &struct{}{})
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "invalid GitHub response") {
		t.Fatalf("get(malformed JSON) error = %v, want invalid GitHub response error", err)
	}
}

// TestIdentityPropagatesGetError exercises Identity's error-propagation
// branch when the underlying get call fails (here, a non-200 status).
func TestIdentityPropagatesGetError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	github := GitHub{Client: server.Client(), BaseURL: server.URL}
	_, err := github.Identity(context.Background(), Config{})
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "GitHub returned HTTP 500") {
		t.Fatalf("Identity(get failure) error = %v, want propagated HTTP 500 error", err)
	}
}

// TestIdentityRejectsInvalidUser exercises Identity's validation branch for
// a variety of malformed GitHub user responses.
func TestIdentityRejectsInvalidUser(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
	}{
		{"empty login", `{"login":"","id":1}`},
		{"zero id", `{"login":"test-bot","id":0}`},
		{"negative id", `{"login":"test-bot","id":-5}`},
		{"login with space", `{"login":"test bot","id":1}`},
		{"login with at sign", `{"login":"test@bot","id":1}`},
		{"login with slash", `{"login":"test/bot","id":1}`},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(test.body))
			}))
			defer server.Close()
			github := GitHub{Client: server.Client(), BaseURL: server.URL}
			_, err := github.Identity(context.Background(), Config{})
			if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "a GitHub user account is required") {
				t.Fatalf("Identity(%s) error = %v, want invalid user account error", test.name, err)
			}
		})
	}
}

// TestIdentityDefaultsNameToLoginWhenNameBlank exercises the branch where a
// blank or whitespace-only display name falls back to the login.
func TestIdentityDefaultsNameToLoginWhenNameBlank(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "   "} {
		name := name
		t.Run("name="+name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"login": "test-bot", "id": 7, "name": name})
			}))
			defer server.Close()
			github := GitHub{Client: server.Client(), BaseURL: server.URL}
			cfg, err := github.Identity(context.Background(), Config{})
			if err != nil {
				t.Fatalf("Identity() error = %v", err)
			}
			if cfg.Name != "test-bot" {
				t.Fatalf("Identity() cfg.Name = %q, want fallback to login %q", cfg.Name, "test-bot")
			}
		})
	}
}

// TestRegisteredRejectsInvalidSavedPublicKey exercises Registered's
// up-front key-parsing error branch.
func TestRegisteredRejectsInvalidSavedPublicKey(t *testing.T) {
	t.Parallel()
	github := GitHub{}
	_, err := github.Registered(context.Background(), Config{PublicKey: "not a valid authorized key", Account: "test-bot"})
	if !errors.Is(err, ErrSetup) || !strings.Contains(err.Error(), "saved public key is invalid") {
		t.Fatalf("Registered(invalid public key) error = %v, want invalid public key error", err)
	}
}

// TestRegisteredReturnsFalseWhenKeyNotFound exercises the "no match found,
// last page" branch: fewer than 100 keys are returned and none match.
func TestRegisteredReturnsFalseWhenKeyNotFound(t *testing.T) {
	t.Parallel()
	cfg, err := Import(t.TempDir(), testKey(t, "ed25519", false), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Account = "test-bot"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"key": "ssh-ed25519 AAAAunrelated comment"}})
	}))
	defer server.Close()
	github := GitHub{Client: server.Client(), BaseURL: server.URL}
	ok, err := github.Registered(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Registered() error = %v", err)
	}
	if ok {
		t.Fatal("Registered() = true, want false when no key matches and the page is short")
	}
}

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONSettingsGateSubmissionsAndDock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		enabled bool
		err     error
		status  int
	}{
		{"enabled", true, nil, http.StatusOK},
		{"disabled", false, nil, http.StatusForbidden},
		{"unavailable", false, errors.New("settings unavailable"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("", NewBroker())
			s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
				return ReviewSettingsState{ChatEnabled: true, JSONEnabled: tc.enabled}, tc.err
			}
			submitted := 0
			s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) { submitted++; return "req-json", nil }
			h := s.Handler()
			for _, source := range []string{"json", "prompt"} {
				req := httptest.NewRequest(http.MethodPost, "/api/local-prompt", strings.NewReader(`{"prompt":"hello"}`))
				req.Header.Set(taskSourceHeader, source)
				resp := httptest.NewRecorder()
				h.ServeHTTP(resp, req)
				want := http.StatusAccepted
				if source == "json" && tc.status != http.StatusOK {
					want = tc.status
				}
				if resp.Code != want {
					t.Fatalf("%s submit status = %d, want %d: %s", source, resp.Code, want, resp.Body.String())
				}
			}
			wantSubmitted := 1
			if tc.enabled {
				wantSubmitted = 2
			}
			if submitted != wantSubmitted {
				t.Fatalf("submitted = %d, want %d", submitted, wantSubmitted)
			}
			dock := string(s.injectBottomDockComponent(context.Background(), []byte(bottomDockPlaceholder)))
			disabled := strings.Contains(dock, `id="prompt-mode-json" class="prompt-mode-link is-disabled hidden"`)
			if disabled != (tc.status != http.StatusOK) {
				t.Fatalf("json dock link disabled = %v", disabled)
			}
		})
	}
}

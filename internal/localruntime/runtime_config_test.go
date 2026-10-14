package localruntime

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadRuntimeConfigMissingFile(t *testing.T) {
	t.Parallel()

	_, err := LoadRuntimeConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

func TestRuntimeConfigLibraryTaskUsageCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     any
		wantCount int
		wantOK    bool
	}{
		{name: "int", value: int(3), wantCount: 3, wantOK: true},
		{name: "int8", value: int8(4), wantCount: 4, wantOK: true},
		{name: "int16", value: int16(5), wantCount: 5, wantOK: true},
		{name: "int32", value: int32(6), wantCount: 6, wantOK: true},
		{name: "int64", value: int64(7), wantCount: 7, wantOK: true},
		{name: "whole float", value: float64(8), wantCount: 8, wantOK: true},
		{name: "fractional float", value: float64(8.5), wantOK: false},
		{name: "string", value: "9", wantOK: false},
		{name: "nil", value: nil, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotCount, gotOK := runtimeConfigLibraryTaskUsageCount(tt.value)
			if gotOK != tt.wantOK || gotCount != tt.wantCount {
				t.Fatalf("runtimeConfigLibraryTaskUsageCount(%#v) = (%d, %v), want (%d, %v)", tt.value, gotCount, gotOK, tt.wantCount, tt.wantOK)
			}
		})
	}
}

func TestSaveRuntimeConfigClaudeOAuthTokenRejectsEmptyToken(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".moltenhub", "config.json")
	if err := SaveRuntimeConfigClaudeOAuthToken(path, InitConfig{}, " \t "); err == nil {
		t.Fatal("SaveRuntimeConfigClaudeOAuthToken(empty) error = nil, want error")
	}
}

func TestSaveRuntimeConfigClaudeOAuthTokenRejectsMalformedConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".moltenhub", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"base_url":`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := SaveRuntimeConfigClaudeOAuthToken(path, InitConfig{}, "oauth_token_saved")
	if err == nil {
		t.Fatal("SaveRuntimeConfigClaudeOAuthToken(malformed) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "parse runtime config") {
		t.Fatalf("SaveRuntimeConfigClaudeOAuthToken(malformed) error = %v, want parse runtime config", err)
	}
}

func TestDefaultRuntimeConfigPath(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")

	if got := defaultRuntimeConfigPath(); got != runtimeConfigPath {
		t.Fatalf("defaultRuntimeConfigPath() = %q, want %q", got, runtimeConfigPath)
	}
}

func TestRuntimeConfigCandidatePathsDefaultIncludesLegacyLocation(t *testing.T) {
	t.Parallel()

	got := runtimeConfigCandidatePaths("")
	want := []string{runtimeConfigPath, legacyRuntimeConfigPath}
	if len(got) != len(want) {
		t.Fatalf("len(runtimeConfigCandidatePaths()) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runtimeConfigCandidatePaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveRuntimeConfigPathUsesInitSiblingDirectory(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "")

	got := ResolveRuntimeConfigPath("/workspace/init.json")
	want := "/workspace/config.json"
	if got != want {
		t.Fatalf("ResolveRuntimeConfigPath() = %q, want %q", got, want)
	}
}

func TestResolveRuntimeConfigPathPrefersEnvOverride(t *testing.T) {
	t.Setenv(runtimeConfigPathEnv, "/custom/runtime.json")

	got := ResolveRuntimeConfigPath("/workspace/init.json")
	if got != "/custom/runtime.json" {
		t.Fatalf("ResolveRuntimeConfigPath() = %q, want %q", got, "/custom/runtime.json")
	}
}

func TestRuntimeConfigCandidatePathsCustomPathIncludesLegacySibling(t *testing.T) {
	t.Parallel()

	got := runtimeConfigCandidatePaths("/workspace/config.json")
	want := []string{"/workspace/config.json", "/workspace/config/config.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtimeConfigCandidatePaths() = %v, want %v", got, want)
	}
}

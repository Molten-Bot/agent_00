package library

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeDockerfileCopiesFullLibraryCatalog(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	if !strings.Contains(content, "HARNESS_AGENTS_SEED_PATH=/opt/moltenhub/library/AGENTS.md") {
		t.Fatalf("%s does not configure the runtime agents seed path", dockerfilePath)
	}
	if !strings.Contains(content, "COPY library /opt/moltenhub/library") {
		t.Fatalf("%s does not copy the full library directory into the runtime image", dockerfilePath)
	}
	if !containsAny(content,
		"COPY library /workspace/library",
		"ln -s /opt/moltenhub/library /workspace/library",
	) {
		t.Fatalf("%s does not make the full library directory available at /workspace/library for hub runtime loading", dockerfilePath)
	}
	if strings.Contains(content, "COPY library/AGENTS.md /opt/moltenhub/library/AGENTS.md") {
		t.Fatalf("%s still only copies library/AGENTS.md into the runtime image", dockerfilePath)
	}
}

func TestRuntimeDockerfileCopiesSkillsCatalog(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	if !strings.Contains(content, "COPY skills /opt/moltenhub/skills") {
		t.Fatalf("%s does not copy the full skills directory into the runtime image", dockerfilePath)
	}
	if !containsAny(content,
		"COPY skills /workspace/skills",
		"ln -s /opt/moltenhub/skills /workspace/skills",
	) {
		t.Fatalf("%s does not make the full skills directory available at /workspace/skills for hub runtime inspection", dockerfilePath)
	}
}

func TestRuntimeDockerfileInstallsRipgrep(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	if !strings.Contains(string(data), "ripgrep") {
		t.Fatalf("%s does not install ripgrep in the runtime image", dockerfilePath)
	}
}

func TestRuntimeDockerfileInstallsPlaywrightTest(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"ARG PLAYWRIGHT_VERSION=1.62.1",
		"\"playwright@${PLAYWRIGHT_VERSION}\"",
		"\"@playwright/test@${PLAYWRIGHT_VERSION}\"",
		"NODE_PATH=/usr/local/lib/node_modules",
		"PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright",
		"PLAYWRIGHT_SKIP_BROWSER_GC=1",
		"playwright install --with-deps --no-shell chromium",
		"chown -R node:node /workspace /opt/ms-playwright",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s does not install Playwright runtime requirement %q", dockerfilePath, want)
		}
	}
	for _, forbidden := range []string{
		"PLAYWRIGHT_BROWSERS_PATH=/workspace/config",
		"PLAYWRIGHT_BROWSERS_PATH=/workspace",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("%s stores Playwright browsers under a persisted workspace path: %q", dockerfilePath, forbidden)
		}
	}
}

func TestRuntimeDockerfileInstallsRailsmith(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"ARG RAILSMITH_VERSION=0.1.2",
		"\"@moltenbot/railsmith@${RAILSMITH_VERSION}\"",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s does not install pinned railsmith runtime requirement %q", dockerfilePath, want)
		}
	}
}

func TestRuntimeDockerfileInstallsGitChangesByDay(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"ARG GIT_CHANGES_BY_DAY_VERSION=v0.0.0-20260518234615-87ad8a8d0d77",
		"go install github.com/moltenbot000/git-changes-by-day@${GIT_CHANGES_BY_DAY_VERSION}",
		"COPY --from=build --chmod=755 /out/git-changes-by-day /usr/local/bin/git-changes-by-day",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s does not install git-changes-by-day runtime requirement %q", dockerfilePath, want)
		}
	}
}

func TestRuntimeDockerfileInstallsPythonTooling(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"python3",
		"python3-pip",
		"python3-venv",
		"ln -sf /usr/bin/python3 /usr/local/bin/python",
		"ln -sf /usr/bin/pip3 /usr/local/bin/pip",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s does not install Python runtime requirement %q", dockerfilePath, want)
		}
	}
}

func TestRuntimeDockerfileInstallsOpenAIPythonSDK(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"ARG OPENAI_PYTHON_VERSION=2.53.0",
		"python3 -m pip install",
		"openai==${OPENAI_PYTHON_VERSION}",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s does not install latest OpenAI Python SDK requirement %q", dockerfilePath, want)
		}
	}
}

func containsAny(content string, want ...string) bool {
	for _, candidate := range want {
		if strings.Contains(content, candidate) {
			return true
		}
	}
	return false
}

func TestRuntimeDockerfileUsesDebianBaseImages(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"FROM golang:1.26.5-trixie AS build",
		"COPY go.mod go.sum ./",
		"FROM node:26.5.0-trixie-slim AS runtime",
		"apt-get update",
		"apt-get upgrade -y --no-install-recommends",
		"apt-get install -y --no-install-recommends",
		"file",
		"gh",
		"openssh-client",
		"HARNESS_WORKSPACE_RAM_BASE=/workspace",
		"HARNESS_WORKSPACE_DISK_BASE=/workspace",
		"HOME=/workspace/config/home",
		"mkdir -p /workspace/config/home /workspace/agent_00/tasks",
		"chown -R node:node /workspace",
		"ln -sf /usr/local/go/bin/go /usr/local/bin/go",
		"ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt",
		"USER node",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s missing Debian runtime requirement %q", dockerfilePath, want)
		}
	}

	for _, forbidden := range []string{
		"alpine",
		"apk add",
		"github-cli",
		"openssh-client-default",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("%s still contains Alpine-specific token %q", dockerfilePath, forbidden)
		}
	}
}

func TestRuntimeDockerfilePinsAgentCLIs(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"ARG CODEX_VERSION=0.147.0",
		"ARG CLAUDE_CODE_VERSION=2.1.224",
		"\"@openai/codex@${CODEX_VERSION}\"",
		"\"@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}\"",
		"npm install --global --no-audit --no-fund",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s missing pinned agent CLI requirement %q", dockerfilePath, want)
		}
	}
	if strings.Contains(content, "@latest") {
		t.Fatalf("%s contains an unpinned npm or Go dependency", dockerfilePath)
	}
}

func TestRuntimeDockerfilePrunesNonRuntimeGoDistributionTrees(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")

	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", dockerfilePath, err)
	}

	content := string(data)
	for _, want := range []string{
		"/usr/local/go/api",
		"/usr/local/go/doc",
		"/usr/local/go/misc",
		"/usr/local/go/test",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s does not prune non-runtime Go tree %q", dockerfilePath, want)
		}
	}
}

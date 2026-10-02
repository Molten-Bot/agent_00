#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
    echo "usage: $0 IMAGE AUTH_CONTAINER" >&2
    echo "AUTH_CONTAINER must have a persisted Codex login at /workspace/config/home/.codex." >&2
    exit 2
fi

image="$1"
auth_container="$2"
repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
smoke_dir="$(mktemp -d)"
trap 'rm -rf "$smoke_dir"' EXIT HUP INT TERM
# The image runs as node; test binaries must be readable through this mount.
chmod 755 "$smoke_dir"
cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH="$(docker image inspect "$image" --format '{{.Architecture}}')" \
    go test -c -o "$smoke_dir/sandbox.test" ./internal/agentruntime
CGO_ENABLED=0 GOOS=linux GOARCH="$(docker image inspect "$image" --format '{{.Architecture}}')" \
    go test -c -o "$smoke_dir/startup.test" ./internal/app

# Share the persisted login without copying credentials; the task test acquires
# the same process-safe session lock used by the harness in AUTH_CONTAINER.
# Override the entrypoint so this check cannot dispatch Hub work or run onboarding.
docker run --rm --cap-drop=ALL \
    --security-opt seccomp=unconfined \
    --security-opt apparmor=unconfined \
    --volumes-from "$auth_container" \
    --mount "type=bind,src=$smoke_dir,dst=/opt/agent-smoke,readonly" \
    -e HARNESS_TEST_CODEX_HOME=/workspace/config/home/.codex \
    -e HARNESS_TEST_CODEX_SANDBOX=1 \
    -e HARNESS_TEST_CODEX_TASK=1 \
    --entrypoint /bin/sh "$image" -ec '
        /opt/agent-smoke/sandbox.test -test.run "^TestAgentSandboxWriteBoundary$" -test.v
        /opt/agent-smoke/startup.test -test.run "^TestCodexTaskStartup$" -test.v
    '

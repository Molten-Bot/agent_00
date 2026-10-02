#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 IMAGE" >&2
    exit 2
fi

image="$1"
repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
validation_dir="$(mktemp -d)"
trap 'rm -rf "$validation_dir"' EXIT HUP INT TERM
chmod 755 "$validation_dir"
cd "$repo_root"
# Include current edits and new source files, excluding ignored credentials,
# configuration, binaries and caches. No operator login is mounted for this test.
git ls-files -z --cached --others --exclude-standard > "$validation_dir/files"
tar --null -T "$validation_dir/files" -cf "$validation_dir/source.tar"
chmod 644 "$validation_dir/source.tar"

docker run --rm --cap-drop=ALL \
    --security-opt seccomp=unconfined \
    --security-opt apparmor=unconfined \
    --mount "type=bind,src=$validation_dir/source.tar,dst=/opt/source.tar,readonly" \
    --entrypoint /bin/sh "$image" -ec '
        task=/workspace/agent_00/tasks/validation
        mkdir -p "$task/repo" "$task/io/home" "$task/io/tmp" "$task/io/codex"
        tar -xf /opt/source.tar -C "$task/repo"
        cd "$task/repo"
        git init --quiet
        exec env HOME="$task/io/home" TMPDIR="$task/io/tmp" \
            CODEX_HOME="$task/io/codex" CGO_ENABLED=1 \
            GOCACHE="$task/io/cache" GOMODCACHE="$task/io/mod" \
            codex sandbox -c "sandbox_mode=\"workspace-write\"" \
            -c "approval_policy=\"never\"" \
            -c "sandbox_workspace_write.writable_roots=[\"$task\"]" \
            -c sandbox_workspace_write.exclude_slash_tmp=true \
            -c sandbox_workspace_write.exclude_tmpdir_env_var=true \
            -c sandbox_workspace_write.network_access=true \
            -- /bin/sh -ec "go mod download && go build ./... && go build -o bin/harness ./cmd/harness && go test ./... && go test -race ./..."
    '

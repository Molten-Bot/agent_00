#!/bin/sh
set -eu

config_dir="${HARNESS_CONFIG_DIR:-/workspace/config}"
run_config_path="${HARNESS_RUN_CONFIG_PATH:-${config_dir}/config.json}"
init_config_path="${HARNESS_INIT_CONFIG_PATH:-${config_dir}/init.json}"
ui_listen="${HARNESS_UI_LISTEN-:7777}"

# Task configs run once; local runtime preferences start the dashboard.
if [ -f "${run_config_path}" ]; then
    if node -e '
const fs = require("node:fs");
try {
  const raw = fs.readFileSync(process.argv[1], "utf8");
  const cleaned = raw.replace(/"(?:\\.|[^"\\])*"|\/\/[^\n\r]*/g, value => value.startsWith("//") ? "" : value);
  const cfg = JSON.parse(cleaned);
  process.exit(["repo", "repos", "repoUrl", "prompt", "libraryTaskName"].some(key => Object.hasOwn(cfg, key)) ? 0 : 1);
} catch (_) { process.exit(1); }
' "${run_config_path}"; then
        exec harness run --config "${run_config_path}"
    fi
    exec harness serve --config "${run_config_path}" --ui-listen "${ui_listen}"
fi

if [ -f "${init_config_path}" ]; then
    exec harness serve --init "${init_config_path}" --ui-listen "${ui_listen}"
fi

export HARNESS_RUNTIME_CONFIG_PATH="${HARNESS_RUNTIME_CONFIG_PATH:-${run_config_path}}"
echo "no config file found; starting local onboarding with defaults" >&2
exec harness serve --ui-listen "${ui_listen}"

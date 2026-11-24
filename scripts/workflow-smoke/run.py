#!/usr/bin/env python3
"""Exercise the running workflow API and report evidence without inventing success.

Examples:
  python3 scripts/workflow-smoke/run.py --repo example/repository
  python3 scripts/workflow-smoke/run.py --repo example/repository --action install
  python3 scripts/workflow-smoke/run.py --repo example/repository --action observe
  python3 scripts/workflow-smoke/run.py --repo example/repository --action cleanup

All modes use the existing local app. Check never saves definitions. Install saves
DISABLED copies of the checked-in templates and verifies optimistic concurrency.
Run the explicit preparation steps from matrix.json to exercise live execution;
this runner never enables automation, changes global settings, or mutates GitHub.
Observe reports runtime evidence separately from API contract checks. Cleanup
removes only disabled definitions exactly matching this runner's generated copies.
Use a unique --prefix to keep agents' tests independent. Exit 1 means a failed check,
2 means the API could not be reached; blocked execution checks alone do not fail CI.
"""

import argparse
import copy
import datetime
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
MATRIX = json.loads(Path(__file__).with_name("matrix.json").read_text())
TEMPLATES = json.loads((ROOT / "internal/web/workflow/src/templates.json").read_text())


class API:
    def __init__(self, base):
        self.base = base.rstrip("/")

    def request(self, path, data=None, method=None, raw=None):
        body = raw if raw is not None else (json.dumps(data).encode() if data is not None else None)
        request = urllib.request.Request(self.base + path, data=body, method=method,
                                         headers={"Content-Type": "application/json"})
        try:
            response = urllib.request.urlopen(request, timeout=90)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            payload = response.read()
            try:
                decoded = json.loads(payload)
            except ValueError:
                decoded = {"error": "API returned a non-JSON response"}
            return response.code, decoded

    def get(self, path):
        code, value = self.request(path)
        if code != 200:
            raise RuntimeError(f"GET {path}: HTTP {code}: {value.get('error', 'request failed')}")
        return value


def definitions(repo, prefix):
    templates = {entry["id"]: entry["definition"] for entry in TEMPLATES["templates"]}
    result = []
    for case in MATRIX["cases"]:
        definition = copy.deepcopy(templates[case["template"]])
        definition.update(id=f"{prefix}-{case['id']}", name=f"Smoke: {case['name']}",
                          repository=repo, enabled=False, includeBacklog=False)
        if "instructions" in case:
            definition["task"]["instructions"] = case["instructions"]
        else:
            definition["task"]["instructions"] += " Keep the change limited to the smoke-test fixture and report validation results."
        label = f"{prefix}-{case['id']}"
        if definition["trigger"]["event"] == "issue.labeled":
            definition["trigger"]["value"] = label
        elif definition["trigger"]["event"] != "schedule":
            definition["filters"]["rules"] = [{"field": "label", "operator": "is", "value": label}]
        definition.pop("positions", None)
        if not definition["trigger"].get("value"):
            definition["trigger"].pop("value", None)
        result.append(definition)
    return result


def record(report, name, status, **details):
    report["results"].append(dict(check=name, status=status, **details))


def expect(report, name, actual, wanted):
    record(report, name, "pass" if actual == wanted else "fail", actual=actual, expected=wanted)


def check(api, defs, report):
    before = api.get("/api/workflows/runs")
    before_ids = {run["id"] for run in before}
    for definition in defs:
        code, preview = api.request("/api/workflows/preview", definition, "POST")
        valid = code == 200 and isinstance(preview, list)
        record(report, f"preview:{definition['id']}", "pass" if valid else "fail", httpStatus=code,
               matchingItems=sum(bool(item.get("matches")) for item in preview) if valid else 0,
               error=None if valid else preview)
    baseline = defs[0]
    for label, mutation in [
        ("unknown-field", {"unknown": True}),
        ("bad-repository", {"repository": "../invalid"}),
        ("unsupported-trigger", {"trigger": {"event": "unsupported"}}),
        ("invalid-interval", {"trigger": {"event": "schedule", "intervalHours": 0}}),
        ("too-many-reviews", {"reviewCycles": 11}),
        ("draft-merge", {"task": {**baseline["task"], "draft": True},
                         "merge": {**baseline["merge"], "enabled": True}}),
    ]:
        code, _ = api.request("/api/workflows/preview", {**baseline, **mutation}, "POST")
        expect(report, f"reject:{label}", code, 400)
    for suffix in (" {}", " null", " trailing"):
        code, _ = api.request("/api/workflows/preview", method="POST",
                              raw=(json.dumps(baseline) + suffix).encode())
        expect(report, f"reject:trailing{suffix}", code, 400)
    ids = {definition["id"] for definition in defs}
    new_owned = [run["id"] for run in api.get("/api/workflows/runs")
                 if run["id"] not in before_ids and run["workflow"]["id"] in ids]
    expect(report, "preview-does-not-dispatch", len(new_owned), 0)


def install(api, defs, report):
    document = api.get("/api/workflows")
    existing = {item["id"]: item for item in document["workflows"]}
    conflicts = [item["id"] for item in defs if item["id"] in existing and item != existing[item["id"]]]
    if conflicts:
        record(report, "install", "blocked", reason="Namespace contains edited definitions; choose a new --prefix", ids=conflicts)
        return
    updated = copy.deepcopy(document)
    updated["workflows"] += [item for item in defs if item["id"] not in existing]
    code, saved = api.request("/api/workflows", updated, "PUT")
    expect(report, "install-disabled-definitions", code, 200)
    if code != 200:
        return
    code, _ = api.request("/api/workflows", document, "PUT")
    expect(report, "reject-stale-revision", code, 409)
    current = api.get("/api/workflows")
    remaining = {item["id"]: item for item in current["workflows"]}
    expect(report, "preserve-existing-definitions", all(remaining.get(key) == value for key, value in existing.items()), True)
    expect(report, "installed-definitions-disabled", all(remaining.get(item["id"]) == item for item in defs), True)


def observe(api, defs, report):
    runs = api.get("/api/workflows/runs")
    for case, definition in zip(MATRIX["cases"], defs):
        matching = [run for run in runs if run["workflow"]["id"] == definition["id"]]
        latest = matching[-1] if matching else None
        status = ("fail" if latest and latest["status"] == "failed" else
                  "pass" if latest and latest["status"] in case["terminal"] else "blocked")
        record(report, f"execution:{case['id']}", status, runCount=len(matching),
               latestStatus=latest["status"] if latest else None,
               requestId=latest.get("requestId") if latest else None,
               reason=latest.get("reason") if latest else case["fixture"],
               evidenceScope="Runtime terminal state only; inspect PR diff, checks, and review evidence using matrix.json")
    for name in ("dedupe", "pause", "merge-gates"):
        record(report, name, "blocked", reason="Requires controlled transitions and before/after observations; see matrix.json")


def cleanup(api, defs, report):
    document = api.get("/api/workflows")
    owned = {item["id"]: item for item in defs}
    retained = [item for item in document["workflows"] if item["id"] in owned and item != owned[item["id"]]]
    removed = [item["id"] for item in document["workflows"] if owned.get(item["id"]) == item]
    document["workflows"] = [item for item in document["workflows"] if item["id"] not in removed]
    if removed:
        code, _ = api.request("/api/workflows", document, "PUT")
        expect(report, "cleanup-disabled-definitions", code, 200)
    else:
        record(report, "cleanup-disabled-definitions", "pass", removed=[])
    if retained:
        record(report, "cleanup-edited-definitions", "blocked", ids=[item["id"] for item in retained],
               reason="Edited or enabled definitions retained; pause and inspect them manually")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", required=True, help="Explicit owner/repository test target")
    parser.add_argument("--base-url", default="http://127.0.0.1:7777")
    parser.add_argument("--prefix", default="workflow-smoke", help="Unique namespace shared by install/observe/cleanup")
    parser.add_argument("--action", choices=("check", "install", "observe", "cleanup"), default="check")
    parser.add_argument("--output", type=Path, help="Write the JSON report to a private local file")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo) or ".." in args.repo:
        parser.error("--repo must be owner/repository")
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,50}", args.prefix):
        parser.error("--prefix must contain 1–50 letters, digits, underscores, or hyphens")
    report = {"version": 1, "action": args.action, "repository": args.repo, "prefix": args.prefix,
              "checkedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(), "results": []}
    api = API(args.base_url)
    defs = definitions(args.repo, args.prefix)
    exit_code = 0
    try:
        {"check": check, "install": install, "observe": observe, "cleanup": cleanup}[args.action](api, defs, report)
    except (OSError, RuntimeError, ValueError, KeyError, TypeError) as error:
        record(report, "runner", "fail", reason=str(error))
        exit_code = 2
    report["summary"] = {status: sum(result["status"] == status for result in report["results"])
                         for status in ("pass", "fail", "blocked")}
    payload = json.dumps(report, indent=2) + "\n"
    if args.output:
        fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as stream:
            stream.write(payload)
    print(payload, end="")
    return exit_code or int(report["summary"]["fail"] > 0)


if __name__ == "__main__":
    sys.exit(main())

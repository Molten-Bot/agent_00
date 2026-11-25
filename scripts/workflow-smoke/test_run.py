"""Run with: python3 -m unittest discover -s scripts/workflow-smoke -v."""
from collections import Counter
import copy
import json
import subprocess
import unittest
from unittest.mock import Mock, patch

import run


class StoreAPI:
    def __init__(self, definitions):
        self.document = {"version": 2, "revision": 3, "workflows": copy.deepcopy(definitions)}

    def get(self, path):
        assert path == "/api/workflows"
        return copy.deepcopy(self.document)

    def request(self, path, data, method):
        assert path == "/api/workflows" and method == "PUT"
        if data["revision"] != self.document["revision"]:
            return 409, {"error": "stale"}
        self.document = copy.deepcopy(data)
        self.document["revision"] += 1
        return 200, copy.deepcopy(self.document)


class CatalogTests(unittest.TestCase):
    def test_one_case_per_shipped_template(self):
        catalog = Counter(t["id"] for t in run.TEMPLATES["templates"])
        cases = Counter(c["template"] for c in run.MATRIX["cases"])
        missing = sorted(catalog.keys() - cases.keys())
        unknown = sorted(cases.keys() - catalog.keys())
        duplicates = sorted({k for counts in (catalog, cases) for k, n in counts.items() if n > 1})
        self.assertFalse(missing or unknown or duplicates,
                         f"missing={missing}, unknown={unknown}, duplicates={duplicates}")


class RunnerSafetyTests(unittest.TestCase):
    def setUp(self):
        self.definitions = run.definitions("example/test", "isolated")
        self.other = {**self.definitions[0], "id": "user-owned", "enabled": True}
        self.report = {"results": []}

    def test_check_only_posts_previews(self):
        api = Mock()
        api.get.return_value = {"events": []}
        def preview(path, data=None, method=None, raw=None):
            self.assertEqual((path, method), ("/api/workflows/preview", "POST"))
            return (200, []) if data in self.definitions else (400, {})
        api.request.side_effect = preview
        run.check(api, self.definitions, self.report)
        self.assertTrue(all(r["status"] == "pass" for r in self.report["results"]))
        self.assertEqual(sum(r["check"].startswith("preview:") for r in self.report["results"]),
                         len(self.definitions))

    def test_install_preserves_existing_and_leaves_all_test_workflows_disabled(self):
        api = StoreAPI([self.other])
        run.install(api, self.definitions, self.report)
        self.assertEqual(api.document["workflows"][0], self.other)
        self.assertEqual(api.document["workflows"][1:], self.definitions)
        self.assertTrue(all(not w["enabled"] for w in self.definitions))
        self.assertTrue(all(r["status"] == "pass" for r in self.report["results"]))
        run.install(api, self.definitions, {"results": []})
        self.assertEqual(len(api.document["workflows"]), len(self.definitions) + 1)

    def test_cleanup_retains_enabled_or_edited_workflows_and_unrelated_definitions(self):
        enabled = {**self.definitions[0], "enabled": True}
        edited = {**self.definitions[1], "name": "User edited this workflow"}
        api = StoreAPI([self.other, enabled, edited, *self.definitions[2:]])
        run.cleanup(api, self.definitions, self.report)
        self.assertEqual(api.document["workflows"], [self.other, enabled, edited])
        self.assertEqual(self.report["results"][-1]["status"], "blocked")

    def test_install_refuses_namespace_collisions(self):
        edited = {**self.definitions[0], "repository": "example/different"}
        api = StoreAPI([self.other, edited])
        before = copy.deepcopy(api.document)
        run.install(api, self.definitions, self.report)
        self.assertEqual(api.document, before)
        self.assertEqual(self.report["results"][0]["status"], "blocked")

    def test_enable_and_pause_only_selected_definition(self):
        api = StoreAPI([self.other, *self.definitions])
        target = self.definitions[1]
        run.set_enabled(api, target, True, self.report)
        self.assertEqual(api.document["workflows"],
                         [self.other, self.definitions[0], {**target, "enabled": True}, *self.definitions[2:]])
        revision = api.document["revision"]
        run.set_enabled(api, target, True, self.report)
        self.assertEqual(api.document["revision"], revision)
        run.set_enabled(api, target, False, self.report)
        self.assertEqual(api.document["workflows"], [self.other, *self.definitions])
        self.assertTrue(all(r["status"] == "pass" for r in self.report["results"]))

    def test_enable_and_pause_refuse_edited_definitions(self):
        target = self.definitions[0]
        edited = {**target, "repository": "example/different", "enabled": True}
        api = StoreAPI([self.other, edited])
        before = copy.deepcopy(api.document)
        run.set_enabled(api, target, True, self.report)
        run.set_enabled(api, target, False, self.report)
        self.assertEqual(api.document, before)
        self.assertTrue(all(r["status"] == "fail" for r in self.report["results"]))


class AppSubmissionTests(unittest.TestCase):
    def setUp(self):
        self.api = Mock()
        self.defs = run.definitions("example/test", "isolated")
        self.report = {"results": []}
        self.settings = {"workflow_enabled": True, "json_enabled": True, "chat_enabled": True}
        self.task_changes = {}
        self.workflow = None
        self.api.request.return_value = (202, {"request_id": "req"})
        self.api.get.side_effect = self.get

    def get(self, path):
        if path == "/api/review-settings":
            return {"settings": self.settings}
        if path == "/api/workflows":
            return {"workflows": [{**self.definition, "enabled": True}]}
        self.assertEqual(path, "/api/status")
        args, kwargs = self.api.request.call_args
        self.assertEqual((args[0], args[2]), ("/api/local-prompt", "POST"))
        self.assertEqual(kwargs["headers"], {"X-Agent-Task-Source": self.source})
        self.assertEqual(args[1]["repoUrl"], "https://github.com/example/test")
        workflow = self.workflow or self.definition["id"]
        return {"tasks": [{"request_id": "req", "source": self.source, "workflow_triggered": True,
                           "prompt": args[1]["prompt"], "status": "completed",
                           "pr_url": "https://github.com/example/test/pull/7", **self.task_changes}],
                "events": [{"line": f"workflow event=submission workflow_id={workflow} run_id=run request_id=req"}]}

    def submit(self, source):
        self.source = source
        self.definition = next(d for d in self.defs if d["trigger"]["event"] == f"app.{source}")
        self.assertEqual(self.definition["filters"]["rules"], [])
        run.submit(self.api, self.definition, self.report, 1)
        return self.report["results"][-1]

    def test_each_source_requires_its_submitted_artifact(self):
        markers = set()
        for source in ("prompt", "json", "chat"):
            with self.subTest(source=source), patch.object(run.subprocess, "run") as gh:
                def files(command, **kwargs):
                    self.assertEqual(command[-1], "repos/example/test/pulls/7/files")
                    path = self.api.request.call_args.args[1]["prompt"].split()[1]
                    marker = path.removeprefix("docs/").removesuffix(".md")
                    diff = [{"filename": path, "status": "added", "additions": 1, "patch": "+" + marker}]
                    return subprocess.CompletedProcess(command, 0, json.dumps(diff))
                gh.side_effect = files
                result = self.submit(source)
                self.assertEqual(result["status"], "pass")
                self.assertNotIn(result["marker"], markers)
                markers.add(result["marker"])
                gh.return_value = subprocess.CompletedProcess([], 0, "[]")
                gh.side_effect = None
                self.assertEqual(self.submit(source)["status"], "fail")

    def test_wrong_request_source_payload_or_workflow_fails(self):
        for changes in ({"request_id": "other"}, {"source": "json"}, {"prompt": "different"},
                        {"workflow_triggered": False}, {"status": "error"}, {"pr_url": ""}):
            with self.subTest(changes=changes):
                self.task_changes = changes
                self.assertEqual(self.submit("prompt")["status"], "fail")
        self.task_changes = {}
        self.workflow = "some-other-workflow"
        self.assertEqual(self.submit("prompt")["status"], "fail")
        self.api.request.return_value = (202, {})
        self.assertEqual(self.submit("prompt")["status"], "fail")

    def test_disabled_features_block_before_submission(self):
        for source in ("chat", "json"):
            self.settings[source + "_enabled"] = False
            self.assertEqual(self.submit(source)["status"], "blocked")
        self.api.request.assert_not_called()


if __name__ == "__main__":
    unittest.main()

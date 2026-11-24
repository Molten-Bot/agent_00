"""Run with: python3 -m unittest discover -s scripts/workflow-smoke -v."""
import copy
import unittest

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


class RunnerSafetyTests(unittest.TestCase):
    def setUp(self):
        self.definitions = run.definitions("example/test", "isolated")
        self.other = {**self.definitions[0], "id": "user-owned", "enabled": True}
        self.report = {"results": []}

    def test_install_preserves_existing_and_leaves_all_test_workflows_disabled(self):
        api = StoreAPI([self.other])
        run.install(api, self.definitions, self.report)
        self.assertEqual(api.document["workflows"][0], self.other)
        self.assertEqual(api.document["workflows"][1:], self.definitions)
        self.assertTrue(all(not w["enabled"] for w in self.definitions))
        self.assertTrue(all(r["status"] == "pass" for r in self.report["results"]))
        run.install(api, self.definitions, {"results": []})
        self.assertEqual(len(api.document["workflows"]), 9)

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


if __name__ == "__main__":
    unittest.main()

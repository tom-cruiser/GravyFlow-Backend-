import contextlib
import importlib.util
import io
import json
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest import mock

_spec = importlib.util.spec_from_file_location("dm", Path(__file__).with_name("deploy_monorepo.py"))
dm = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(dm)

SECRET = "sup3r-s3cret-value"


class ParseEnvTests(unittest.TestCase):
    def parse(self, text):
        with tempfile.NamedTemporaryFile("w", suffix=".env", delete=False) as f:
            f.write(text)
        return dm.parse_env_file(Path(f.name))

    def test_basic_forms(self):
        env = self.parse(
            "# comment\n\nA=1\nexport B=two\nC = spaced\nD=\"quoted # not a comment\"\n"
            "E='single \\n literal'\nF=value # trailing comment\nG=\n"
        )
        self.assertEqual(env["A"], "1")
        self.assertEqual(env["B"], "two")
        self.assertEqual(env["C"], "spaced")
        self.assertEqual(env["D"], "quoted # not a comment")
        self.assertEqual(env["E"], "single \\n literal")  # single quotes are literal
        self.assertEqual(env["F"], "value")
        self.assertEqual(env["G"], "")

    def test_double_quoted_escapes_and_multiline(self):
        env = self.parse('KEY="-----BEGIN-----\\nabc\\n-----END-----"\nMULTI="line1\nline2"\nAFTER=ok\n')
        self.assertEqual(env["KEY"], "-----BEGIN-----\nabc\n-----END-----")
        self.assertEqual(env["MULTI"], "line1\nline2")
        self.assertEqual(env["AFTER"], "ok")

    def test_urls_with_special_characters_survive(self):
        env = self.parse("DATABASE_URL=postgresql://u:p%40ss@host:5432/db?sslmode=require&x=1\n")
        self.assertEqual(env["DATABASE_URL"], "postgresql://u:p%40ss@host:5432/db?sslmode=require&x=1")


class BuildEnvTests(unittest.TestCase):
    def test_precedence_and_empty_values(self):
        base = {"REDIS_URL": "redis://localhost:6379", "KEEP": "1", "BLANK": "", "SYNC_QUEUE_NAME": "old"}
        manifest = {"env": {"KEEP": "manifest"}}
        svc = {"port": 3003, "env": {"SYNC_QUEUE_NAME": "sales-sync-batch"}}
        env = dm.build_service_env(base, manifest, svc, "redis://:pw@nerva-redis:6379")
        self.assertEqual(env["REDIS_URL"], "redis://:pw@nerva-redis:6379")
        self.assertEqual(env["BULLMQ_REDIS_URL"], "redis://:pw@nerva-redis:6379")
        self.assertEqual(env["KEEP"], "manifest")
        self.assertEqual(env["PORT"], "3003")
        self.assertEqual(env["SYNC_QUEUE_NAME"], "sales-sync-batch")
        self.assertNotIn("BLANK", env)

    def test_no_redis_leaves_env_untouched(self):
        env = dm.build_service_env({"REDIS_URL": "redis://x"}, {}, {"port": 1}, None)
        self.assertEqual(env["REDIS_URL"], "redis://x")


class ManifestTests(unittest.TestCase):
    def load(self, obj):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(obj, f)
        return dm.load_manifest(Path(f.name))

    def test_shipped_nerva_manifest_is_valid(self):
        m = dm.load_manifest(Path(__file__).with_name("nerva.manifest.json"))
        self.assertEqual(len(m["services"]), 8)

    def test_rejects_bad_manifests(self):
        for bad in (
            {"repo": "o/r"},
            {"repo": "o/r", "services": [{"name": "a", "dockerfilePath": "x"}, {"name": "a", "dockerfilePath": "y"}]},
            {"repo": "o/r", "services": [{"name": "Bad_Name", "dockerfilePath": "x"}]},
            {"repo": "o/r", "services": [{"name": "ok"}]},
        ):
            with self.assertRaises(SystemExit):
                self.load(bad)


class FakeGravyFlow(BaseHTTPRequestHandler):
    state: dict = {}

    def log_message(self, *a):  # silence
        pass

    def _send(self, code, obj):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _route(self, method):
        s = self.state
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length) or b"{}") if length else {}
        path = self.path
        s["calls"].append((method, path, body))

        if path == "/auth/login":
            return self._send(200, {"accessToken": "tok-1", "refreshToken": "ref-1", "user": {"id": "u1", "email": body["email"]}})
        if path == "/auth/refresh":
            return self._send(200, {"accessToken": "tok-2", "refreshToken": "ref-2"})
        want = "tok-2" if s.get("expire_first_token") and s["refreshed"] else "tok-1"
        if s.get("expire_first_token") and not s["refreshed"] and path == "/apps" and method == "GET":
            s["refreshed"] = True
            return self._send(401, {"error": "unauthorized"})
        if self.headers.get("Authorization") != f"Bearer {want}" and want == "tok-2":
            return self._send(401, {"error": "stale token"})

        if path == "/integrations/github/repos":
            return self._send(200, {"repos": [{"id": 42, "installationId": 7, "fullName": "tom-cruiser/Nerva-backend"}]})
        if path == "/apps" and method == "GET":
            return self._send(200, {"apps": [{"AppName": "shifts", "DeploymentID": "dep-existing"}]})
        if path == "/users/u1/quota":
            return self._send(200, s["quota"])
        if path == "/billing/plan":
            s["upgraded"] = body["plan"]
            return self._send(200, {"ok": True})
        if path == "/apps" and method == "POST":
            n = len(s["created"]) + 1
            s["created"].append(body)
            return self._send(201, {"deploymentId": f"dep-{body['name']}", "jobId": f"job-{body['name']}"})
        if path.endswith("/env/bulk"):
            s["env"].setdefault(path.split("/")[2], {}).update({v["key"]: v["value"] for v in body["variables"]})
            return self._send(200, {"results": []})
        if path.endswith("/build-settings") and method == "PUT":
            s["build"][path.split("/")[2]] = body
            return self._send(200, {})
        if path.endswith("/deploy"):
            return self._send(202, {"jobId": "job-shifts"})
        if path.startswith("/jobs/"):
            job = path.rsplit("/", 1)[1]
            s["polls"][job] = s["polls"].get(job, 0) + 1
            if s["polls"][job] < 2:
                return self._send(200, {"status": "active", "stage": "building", "progress": 50, "message": "building"})
            if job == "job-realtime":
                return self._send(200, {"status": "failed", "stage": "failed", "error": "boom: build failed"})
            return self._send(200, {"status": "completed", "stage": "completed", "progress": 100, "message": "done"})
        self._send(404, {"error": "not found: " + path})

    def do_GET(self):
        self._route("GET")

    def do_POST(self):
        self._route("POST")

    def do_PUT(self):
        self._route("PUT")


class FlowTests(unittest.TestCase):
    def setUp(self):
        FakeGravyFlow.state = {
            "calls": [], "created": [], "env": {}, "build": {}, "polls": {}, "refreshed": False,
            "quota": {"quota": {"plan": "free"}, "available": {"maxApps": 20, "maxCpu": 20, "maxMemoryMb": 20000}},
        }
        self.server = HTTPServer(("127.0.0.1", 0), FakeGravyFlow)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.tmp = tempfile.TemporaryDirectory()
        tmp = Path(self.tmp.name)
        (tmp / ".env").write_text(f"DATABASE_URL=postgres://u:{SECRET}@db:5432/x\nREDIS_URL=redis://localhost:6379\nEMPTY=\n")
        (tmp / "m.json").write_text(json.dumps({
            "repo": "tom-cruiser/Nerva-backend", "envFile": str(tmp / ".env"),
            "services": [
                {"name": "auth-tenant", "dockerfilePath": "services/auth-tenant/Dockerfile", "port": 3001},
                {"name": "shifts", "dockerfilePath": "services/shifts/Dockerfile", "port": 3006},
                {"name": "realtime", "dockerfilePath": "services/realtime/Dockerfile", "port": 3008},
            ],
        }))
        self.manifest = str(tmp / "m.json")
        self.env = mock.patch.dict("os.environ", {"GRAVYFLOW_EMAIL": "me@example.com", "GRAVYFLOW_PASSWORD": "pw"})
        self.env.start()
        self.sleep = mock.patch.object(dm.time, "sleep")
        self.sleep.start()

    def tearDown(self):
        self.sleep.stop()
        self.env.stop()
        self.server.shutdown()
        self.server.server_close()
        self.tmp.cleanup()

    def run_main(self, *extra):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = dm.main([self.manifest, "--api", self.url, "--no-redis", *extra])
        return code, out.getvalue()

    def test_full_flow(self):
        code, out = self.run_main()
        s = FakeGravyFlow.state
        self.assertEqual(code, 1, "one service fails, so the exit code must be non-zero")

        # new services are created with their Dockerfile + port and the repo ids
        created = {c["name"]: c for c in s["created"]}
        self.assertEqual(set(created), {"auth-tenant", "realtime"})
        self.assertEqual(created["auth-tenant"]["dockerfilePath"], "services/auth-tenant/Dockerfile")
        self.assertEqual(created["auth-tenant"]["containerPort"], 3001)
        self.assertEqual(created["auth-tenant"]["github"], {"installationId": 7, "repositoryId": 42})

        # the existing service is updated and redeployed, not created twice
        self.assertEqual(s["build"]["dep-existing"]["dockerfilePath"], "services/shifts/Dockerfile")
        self.assertIn(("POST", "/apps/dep-existing/deploy", {}), s["calls"])

        # env: PORT is per service, empties are dropped, the secret reached the API
        self.assertEqual(s["env"]["dep-auth-tenant"]["PORT"], "3001")
        self.assertEqual(s["env"]["dep-realtime"]["PORT"], "3008")
        self.assertNotIn("EMPTY", s["env"]["dep-auth-tenant"])
        self.assertIn(SECRET, s["env"]["dep-auth-tenant"]["DATABASE_URL"])

        # results: URLs for successes, the error for the failure
        self.assertIn("OK   auth-tenant", out)
        self.assertIn("http://auth-tenant.localhost", out)
        self.assertIn("FAIL realtime", out)
        self.assertIn("boom: build failed", out)

        # secrets are never printed
        self.assertNotIn(SECRET, out)

    def test_expired_token_is_refreshed_transparently(self):
        FakeGravyFlow.state["expire_first_token"] = True
        code, out = self.run_main("--only", "auth-tenant")
        self.assertEqual(code, 0)
        self.assertTrue(FakeGravyFlow.state["refreshed"])
        self.assertIn(("POST", "/auth/refresh", {"refreshToken": "ref-1"}), FakeGravyFlow.state["calls"])

    def test_quota_shortfall_stops_unless_upgrade_requested(self):
        FakeGravyFlow.state["quota"] = {"quota": {"plan": "free"}, "available": {"maxApps": 1, "maxCpu": 0.5, "maxMemoryMb": 512}}
        with self.assertRaises(SystemExit) as ctx:
            self.run_main()
        self.assertIn("--upgrade-plan", str(ctx.exception))
        self.assertEqual(FakeGravyFlow.state["created"], [], "nothing may be created when the quota is short")

        code, _ = self.run_main("--upgrade-plan", "pro")
        self.assertEqual(FakeGravyFlow.state["upgraded"], "pro")
        self.assertEqual(len(FakeGravyFlow.state["created"]), 2)

    def test_unknown_repo_is_a_clear_error(self):
        manifest = json.loads(Path(self.manifest).read_text())
        manifest["repo"] = "someone/else"
        Path(self.manifest).write_text(json.dumps(manifest))
        with self.assertRaises(SystemExit) as ctx:
            self.run_main()
        self.assertIn("isn't visible", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()

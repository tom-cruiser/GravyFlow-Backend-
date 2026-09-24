#!/usr/bin/env python3
"""Deploy every service of a monorepo to GravyFlow from a manifest.

GravyFlow hosts one container per service, so a monorepo becomes N services
that share one GitHub repository and differ only in the Dockerfile they build
(build_settings.go). This script automates the repetitive part:

  1. (optional) start a password-protected Redis on the apps network so the
     services can reach it by container name
  2. log in to GravyFlow (you type your password; it is never stored)
  3. create each service from the repo with its Dockerfile path and port
  4. import each service's environment from the project's .env, with the
     infrastructure endpoints (Redis) pointed at the hosted containers
  5. watch every build until it finishes and print where each service lives

Secrets are only ever read from the .env you point it at and sent to your own
GravyFlow API; values are never printed.

    python3 scripts/monorepo/deploy_monorepo.py scripts/monorepo/nerva.manifest.json
    python3 scripts/monorepo/deploy_monorepo.py MANIFEST --dry-run     # no login, no changes
    python3 scripts/monorepo/deploy_monorepo.py MANIFEST --only auth-tenant,shifts
"""
from __future__ import annotations

import argparse
import getpass
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

DEFAULT_API = "http://localhost:8080/api/v1"
ENV_BATCH = 50  # the bulk-env endpoint's per-request limit
TERMINAL = {"completed", "failed", "cancelled"}
PER_SERVICE_CPU = 0.5  # resources.go: defaultDeployCPU / defaultDeployMemoryMB
PER_SERVICE_MEM_MB = 512
REPO_ROOT = Path(__file__).resolve().parents[2]


# --------------------------------------------------------------------------- env parsing

_ESCAPES = {"n": "\n", "r": "\r", "t": "\t", '"': '"', "\\": "\\"}


def parse_env_file(path: Path) -> dict[str, str]:
    """Parse a .env file the way docker compose's env_file does: KEY=VALUE,
    optional `export`, # comments, 'single' (literal) and "double" (escapes
    expanded, may span lines) quotes."""
    text = path.read_text()
    env: dict[str, str] = {}
    i, n = 0, len(text)
    while i < n:
        eol = text.find("\n", i)
        eol = n if eol == -1 else eol
        line = text[i:eol].strip()
        i = eol + 1
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[len("export "):].lstrip()
        if "=" not in line:
            continue
        key, _, rest = line.partition("=")
        key, rest = key.strip(), rest.lstrip()
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            continue
        if rest[:1] in ("'", '"'):
            quote = rest[0]
            body = rest[1:]
            # a quoted value may continue over following lines
            while True:
                end = _closing_quote(body, quote)
                if end != -1 or i >= n:
                    break
                eol = text.find("\n", i)
                eol = n if eol == -1 else eol
                body += "\n" + text[i:eol]
                i = eol + 1
            value = body if end == -1 else body[:end]
            if quote == '"':
                value = re.sub(r"\\(.)", lambda m: _ESCAPES.get(m.group(1), "\\" + m.group(1)), value)
        else:
            value = re.split(r"\s+#", rest, maxsplit=1)[0].strip()
        env[key] = value
    return env


def _closing_quote(body: str, quote: str) -> int:
    j = 0
    while j < len(body):
        if quote == '"' and body[j] == "\\":
            j += 2
            continue
        if body[j] == quote:
            return j
        j += 1
    return -1


# --------------------------------------------------------------------------- manifest / env building

def load_manifest(path: Path) -> dict:
    manifest = json.loads(path.read_text())
    for field in ("repo", "services"):
        if field not in manifest:
            raise SystemExit(f"manifest is missing '{field}'")
    names = [s["name"] for s in manifest["services"]]
    if len(names) != len(set(names)):
        raise SystemExit("manifest has duplicate service names")
    for s in manifest["services"]:
        if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", s["name"]):
            raise SystemExit(f"service name {s['name']!r} must be lowercase letters, digits and dashes")
        if not s.get("dockerfilePath"):
            raise SystemExit(f"service {s['name']} has no dockerfilePath")
    return manifest


def redis_url(password: str, container: str) -> str:
    return f"redis://:{password}@{container}:6379"


def build_service_env(base: dict[str, str], manifest: dict, service: dict, redis: str | None) -> dict[str, str]:
    """base .env -> hosted infrastructure endpoints -> manifest-wide overrides
    -> per-service overrides (later wins). Empty values are dropped."""
    env = dict(base)
    if redis:
        env["REDIS_URL"] = redis
        env["BULLMQ_REDIS_URL"] = redis
    env.update({k: str(v) for k, v in manifest.get("env", {}).items()})
    if service.get("port"):
        env["PORT"] = str(service["port"])
    env.update({k: str(v) for k, v in service.get("env", {}).items()})
    return {k: v for k, v in env.items() if v != ""}


# --------------------------------------------------------------------------- infrastructure (Redis)

def sh(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(args, capture_output=True, text=True, check=check)


def apps_network() -> str:
    return os.environ.get("GRAVYFLOW_APPS_NETWORK", "gravyflow-apps")


def ensure_redis(cfg: dict) -> str:
    """Idempotently run a password-protected Redis on the apps network and
    return the password. The password is kept in a gitignored file so a
    re-run keeps talking to the same instance."""
    if not shutil.which("docker"):
        raise SystemExit("docker is required to start Redis (or pass --no-redis and set REDIS_URL yourself)")
    container = cfg["container"]
    network = apps_network()
    pw_file = REPO_ROOT / cfg["passwordFile"]

    if pw_file.exists():
        password = pw_file.read_text().strip()
    else:
        password = secrets.token_hex(24)
        pw_file.parent.mkdir(parents=True, exist_ok=True)
        pw_file.write_text(password + "\n")
        pw_file.chmod(0o600)

    if sh("docker", "network", "inspect", network, check=False).returncode != 0:
        sh("docker", "network", "create", network)

    exists = sh("docker", "inspect", container, check=False).returncode == 0
    if not exists:
        print(f"==> starting Redis container {container!r} on network {network!r}")
        sh("docker", "run", "-d", "--name", container, "--restart", "unless-stopped",
           "--network", network, "-v", f"{cfg['volume']}:/data", "redis:7-alpine",
           "redis-server", "--appendonly", "yes", "--requirepass", password)
    else:
        running = sh("docker", "inspect", "-f", "{{.State.Running}}", container).stdout.strip()
        if running != "true":
            sh("docker", "start", container)
        nets = sh("docker", "inspect", "-f", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}", container).stdout.split()
        if network not in nets:
            sh("docker", "network", "connect", network, container)
        print(f"==> Redis container {container!r} is up (reusing it; password from {cfg['passwordFile']})")
    return password


# --------------------------------------------------------------------------- GravyFlow API client

class ApiError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(f"HTTP {status}: {message}")
        self.status = status
        self.message = message


class Api:
    def __init__(self, base: str):
        self.base = base.rstrip("/")
        self.access = ""
        self.refresh = ""

    def request(self, method: str, path: str, body=None, auth: bool = True, _retry: bool = True):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        if auth and self.access:
            req.add_header("Authorization", f"Bearer {self.access}")
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else {})
        except urllib.error.HTTPError as err:
            raw = err.read()
            try:
                payload = json.loads(raw)
            except ValueError:
                payload = {"error": raw.decode(errors="replace")[:200]}
            if err.code == 401 and auth and _retry and self.refresh and not path.startswith("/auth/"):
                self._refresh()
                return self.request(method, path, body, auth, _retry=False)
            raise ApiError(err.code, payload.get("details") or payload.get("error") or str(payload)) from None
        except urllib.error.URLError as err:
            raise SystemExit(f"cannot reach GravyFlow at {self.base}: {err.reason}") from None

    def _refresh(self):
        _, data = self.request("POST", "/auth/refresh", {"refreshToken": self.refresh}, auth=False)
        self.access, self.refresh = data["accessToken"], data.get("refreshToken", self.refresh)

    def login(self, email: str, password: str, mfa_code=None) -> dict:
        _, data = self.request("POST", "/auth/login", {"email": email, "password": password}, auth=False)
        if data.get("mfaRequired"):
            code = mfa_code() if mfa_code else input("MFA code: ").strip()
            _, data = self.request("POST", "/auth/mfa/verify", {"mfaToken": data["mfaToken"], "code": code}, auth=False)
        self.access, self.refresh = data["accessToken"], data.get("refreshToken", "")
        return data["user"]


# --------------------------------------------------------------------------- deployment steps

def find_repo(api: Api, full_name: str) -> dict:
    _, data = api.request("GET", "/integrations/github/repos")
    for repo in data.get("repos", []):
        if repo["fullName"].lower() == full_name.lower():
            return repo
    known = ", ".join(r["fullName"] for r in data.get("repos", [])[:10]) or "none"
    raise SystemExit(
        f"repository {full_name} isn't visible to your linked GitHub installation (visible: {known}). "
        "Connect GitHub from New Service and grant the app access to it."
    )


def import_env(api: Api, deployment_id: str, env: dict[str, str]) -> None:
    items = [{"key": k, "value": v} for k, v in env.items()]
    for start in range(0, len(items), ENV_BATCH):
        api.request("POST", f"/apps/{deployment_id}/env/bulk", {"variables": items[start:start + ENV_BATCH], "overwrite": True})


def check_quota(api: Api, user: dict, new_services: int, upgrade_plan: str | None) -> None:
    if new_services == 0:
        return
    _, q = api.request("GET", f"/users/{user['id']}/quota")
    avail = q["available"]
    need_cpu, need_mem = new_services * PER_SERVICE_CPU, new_services * PER_SERVICE_MEM_MB
    short = []
    if avail["maxApps"] < new_services:
        short.append(f"apps: need {new_services}, have {avail['maxApps']}")
    if avail["maxCpu"] < need_cpu:
        short.append(f"CPU: need {need_cpu:g}, have {avail['maxCpu']:g}")
    if avail["maxMemoryMb"] < need_mem:
        short.append(f"memory: need {need_mem} MB, have {avail['maxMemoryMb']} MB")
    if not short:
        return
    plan = q["quota"].get("plan", "?")
    print(f"!! your '{plan}' plan can't fit {new_services} more services ({'; '.join(short)})")
    if not upgrade_plan:
        raise SystemExit("re-run with --upgrade-plan pro (or raise your quota in Settings) and try again")
    print(f"==> switching your plan to '{upgrade_plan}'")
    api.request("POST", "/billing/plan", {"plan": upgrade_plan})


def deploy(api: Api, manifest: dict, services: list[dict], envs: dict[str, dict], repo: dict, existing: dict[str, str]) -> dict[str, str]:
    jobs: dict[str, str] = {}
    for svc in services:
        name = svc["name"]
        settings = {"dockerfilePath": svc["dockerfilePath"], "containerPort": int(svc.get("port", 0))}
        if name in existing:
            dep_id = existing[name]
            print(f"==> {name}: already exists; updating build settings + env and redeploying")
            api.request("PUT", f"/apps/{dep_id}/build-settings", settings)
            import_env(api, dep_id, envs[name])
            _, res = api.request("POST", f"/apps/{dep_id}/deploy")
        else:
            print(f"==> {name}: creating from {manifest['repo']} ({svc['dockerfilePath']})")
            _, res = api.request("POST", "/apps", {
                "name": name,
                "github": {"installationId": repo["installationId"], "repositoryId": repo["id"]},
                **settings,
            })
            dep_id = res["deploymentId"]
            # The first build takes minutes; the container is created (and reads
            # its env) only after it, so setting env now is early enough.
            import_env(api, dep_id, envs[name])
        jobs[name] = res["jobId"]
    return jobs


def watch(api: Api, jobs: dict[str, str], timeout_s: int) -> dict[str, dict]:
    last: dict[str, str] = {}
    final: dict[str, dict] = {}
    deadline = time.time() + timeout_s
    while len(final) < len(jobs) and time.time() < deadline:
        for name, job_id in jobs.items():
            if name in final:
                continue
            try:
                _, st = api.request("GET", f"/jobs/{job_id}")
            except ApiError as err:
                line = f"status unavailable ({err.message})"
                st = None
            else:
                line = f"{st['status']}/{st.get('stage', '')} {st.get('progress', 0)}% {st.get('message', '')}".strip()
            if last.get(name) != line:
                print(f"    [{name}] {line}")
                last[name] = line
            if st and st["status"] in TERMINAL:
                final[name] = st
        time.sleep(5)
    for name in jobs:
        final.setdefault(name, {"status": "timeout", "message": f"no result after {timeout_s}s; still running"})
    return final


def summarize(final: dict[str, dict]) -> int:
    print("\n==> summary")
    failed = 0
    for name, st in final.items():
        ok = st["status"] == "completed"
        failed += 0 if ok else 1
        detail = f"http://{name}.localhost" if ok else (st.get("error") or st.get("message") or st["status"])
        print(f"  {'OK  ' if ok else 'FAIL'} {name:<18} {detail}")
    return 1 if failed else 0


# --------------------------------------------------------------------------- main

def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Deploy every service of a monorepo to GravyFlow.")
    ap.add_argument("manifest", type=Path)
    ap.add_argument("--api", default=os.environ.get("GRAVYFLOW_API", DEFAULT_API))
    ap.add_argument("--env-file", type=Path, help="override the manifest's envFile")
    ap.add_argument("--only", help="comma-separated service names to deploy")
    ap.add_argument("--no-redis", action="store_true", help="don't start Redis; REDIS_URL comes from the .env as-is")
    ap.add_argument("--upgrade-plan", metavar="PLAN", help="switch your GravyFlow plan if the quota is too small")
    ap.add_argument("--timeout", type=int, default=45 * 60, help="seconds to wait for the builds")
    ap.add_argument("--dry-run", action="store_true", help="show the plan (key names only) without logging in or changing anything")
    args = ap.parse_args(argv)

    manifest = load_manifest(args.manifest)
    services = manifest["services"]
    if args.only:
        wanted = {s.strip() for s in args.only.split(",")}
        unknown = wanted - {s["name"] for s in services}
        if unknown:
            raise SystemExit(f"unknown service(s): {', '.join(sorted(unknown))}")
        services = [s for s in services if s["name"] in wanted]

    env_path = Path(os.path.expanduser(str(args.env_file or manifest.get("envFile", ".env"))))
    if not env_path.is_file():
        raise SystemExit(f"env file not found: {env_path}")
    base_env = parse_env_file(env_path)
    print(f"==> read {len(base_env)} variables from {env_path}")

    redis = None
    if not args.no_redis and "redis" in manifest:
        redis = redis_url("<generated-password>" if args.dry_run else ensure_redis(manifest["redis"]), manifest["redis"]["container"])
    envs = {s["name"]: build_service_env(base_env, manifest, s, redis) for s in services}

    if args.dry_run:
        print(f"\nplan: {len(services)} services from {manifest['repo']} "
              f"({len(services) * PER_SERVICE_CPU:g} CPU, {len(services) * PER_SERVICE_MEM_MB} MB reserved)")
        for s in services:
            keys = sorted(envs[s["name"]])
            print(f"  - {s['name']:<18} {s['dockerfilePath']}  port {s.get('port', 'auto')}  env keys ({len(keys)}): {', '.join(keys)}")
        return 0

    api = Api(args.api)
    email = os.environ.get("GRAVYFLOW_EMAIL") or input("GravyFlow email: ").strip()
    password = os.environ.get("GRAVYFLOW_PASSWORD") or getpass.getpass("GravyFlow password: ")
    user = api.login(email, password)
    print(f"==> logged in as {user.get('email', email)}")

    repo = find_repo(api, manifest["repo"])
    _, listing = api.request("GET", "/apps")
    existing = {a["AppName"]: a["DeploymentID"] for a in listing.get("apps", [])}
    check_quota(api, user, sum(1 for s in services if s["name"] not in existing), args.upgrade_plan)

    jobs = deploy(api, manifest, services, envs, repo, existing)
    print(f"\n==> {len(jobs)} deployments queued; watching builds (Ctrl-C stops watching, not the builds)")
    return summarize(watch(api, jobs, args.timeout))


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\nstopped watching; the deployments keep running in GravyFlow")
        sys.exit(130)

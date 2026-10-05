#!/usr/bin/env python3
"""Run an isolated live test; builds inside Kubernetes without a local registry.

Creates and deletes only its own namespace and Healthchecks checks. Never prints
credentials, ping URLs, API bodies, or subprocess error output.
"""
import argparse
import base64
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import HTTPRedirectHandler, Request, build_opener

ROOT = Path(__file__).resolve().parent.parent


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


HTTP = build_opener(NoRedirects)


def run(args, data=None):
    result = subprocess.run(args, input=data, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(f"{Path(args[0]).name} command failed (exit {result.returncode})")
    return result.stdout


def wait_for(description, predicate, timeout=180):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            print(f"PASS: {description}", flush=True)
            return
        time.sleep(3)
    raise RuntimeError(f"timed out: {description}")


def decode_resources(text):
    # kubectl emits a stream of JSON objects for a multi-document YAML input.
    decoder = json.JSONDecoder()
    resources = []
    while text.strip():
        text = text.lstrip()
        resource, end = decoder.raw_decode(text)
        resources.extend(resource.get("items", [resource]))
        text = text[end:]
    return resources


def source_archive():
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as archive:
        # Explicit allowlist: .env, .git, and unrelated workspace files cannot enter.
        for name in ("go.mod", "go.sum"):
            if (ROOT / name).is_file():
                archive.add(ROOT / name, arcname=name)
        for directory in ("cmd", "internal"):
            for path in sorted((ROOT / directory).rglob("*.go")):
                archive.add(path, arcname=str(path.relative_to(ROOT)))
    return base64.b64encode(output.getvalue()).decode()


def cronjob(name, fail=False):
    return {
        "apiVersion": "batch/v1", "kind": "CronJob",
        "metadata": {"name": name, "annotations": {
            "healthchecks.io/include": "true", "k8s.cronitor.io/exclude": "true",
            "healthchecks.io/channels": "", "healthchecks.io/grace-seconds": "60",
        }},
        "spec": {
            "schedule": "* * * * *", "timeZone": "UTC", "concurrencyPolicy": "Forbid",
            "successfulJobsHistoryLimit": 10, "failedJobsHistoryLimit": 10,
            "jobTemplate": {"spec": {
                "backoffLimit": 1, "ttlSecondsAfterFinished": 3600,
                "template": {"spec": {"restartPolicy": "Never", "containers": [{
                    "name": "task", "image": "busybox:1.37",
                    "command": ["sh", "-c", "sleep 20; exit " + ("1" if fail else "0")],
                }]}},
            }},
        },
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True, help="explicit Kubernetes context")
    parser.add_argument("--env-file", type=Path, default=ROOT / ".env")
    args = parser.parse_args()
    values = {}
    for line in args.env_file.read_text().splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value.strip().strip("\"'")
    base = values["HEALTHCHECKS_API_URL"].rstrip("/")
    key = values["HEALTHCHECKS_API_KEY"]
    namespace = "healthchecks-test-" + str(int(time.time()))
    cluster = "k3s-smoke-test"
    owner = "hck-owner-" + hashlib.sha256((cluster + "\0" + namespace).encode()).hexdigest()[:24]
    kube = ["kubectl", "--context", args.context, "--namespace", namespace]

    def api(method, path):
        request = Request(base + path, method=method, headers={"X-Api-Key": key})
        try:
            with HTTP.open(request, timeout=15) as response:
                body = response.read()
                return json.loads(body) if body else None
        except (HTTPError, URLError, json.JSONDecodeError):
            raise RuntimeError("Healthchecks test API request failed") from None

    def checks():
        return api("GET", "/checks/?" + urlencode({"tag": owner}))["checks"]

    def by_job(name):
        return next((check for check in checks() if check["name"] == f"{cluster}/{namespace}/{name}"), None)

    def get(kind, name=None):
        command = kube + ["get", kind]
        if name:
            command.append(name)
        return json.loads(run(command + ["-o", "json"]))

    def apply(resources):
        run(kube + ["apply", "-f", "-"], json.dumps({"apiVersion": "v1", "kind": "List", "items": resources}))

    def patch_job(name, patch):
        run(kube + ["patch", "cronjob", name, "--type=merge", "-p", json.dumps(patch)])

    created = False
    try:
        # Preflight is read-only. Do not create resources if either API is inaccessible.
        try:
            run(kube + ["get", "--raw=/version", "--request-timeout=10s"])
        except RuntimeError:
            raise RuntimeError("Kubernetes preflight failed; no test resources were created") from None
        api("GET", "/checks/?" + urlencode({"tag": owner}))
        run(kube + ["create", "namespace", namespace])
        created = True
        print(f"Test namespace: {namespace}", flush=True)
        # Use an ephemeral Secret only; never write plaintext Secret manifests to GitOps.
        apply([{
            "apiVersion": "v1", "kind": "Secret", "metadata": {"name": "healthchecks-api"},
            "stringData": {"HEALTHCHECKS_API_KEY": key},
        }, {
            "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "controller-source"},
            "binaryData": {"source.tgz": source_archive()},
        }])
        rendered = run([
            "helm", "template", "smoke", str(ROOT / "charts/healthchecks-kubernetes"),
            "--namespace", namespace, "--set", "fullnameOverride=healthchecks-smoke",
            "--set", f"config.cluster={cluster}", "--set", f"config.namespace={namespace}",
            "--set", "config.defaultInclude=false", "--set", "credentials.existingSecret=healthchecks-api",
            "--set-string", f"config.apiUrl={base}", "--set-string", "config.channels=",
            "--set", "config.resync=15s",
        ])
        # Parse the Helm output using kubectl's local YAML-to-JSON conversion.
        resources = decode_resources(run(kube + ["create", "--dry-run=client", "--validate=false", "-f", "-", "-o", "json"], rendered))
        for item in resources:
            if item["kind"] != "Deployment":
                continue
            pod = item["spec"]["template"]["spec"]
            pod["securityContext"]["fsGroup"] = 65532
            pod["volumes"] = [
                {"name": "source", "configMap": {"name": "controller-source"}},
                {"name": "output", "emptyDir": {}},
                {"name": "build", "emptyDir": {}},
            ]
            pod["initContainers"] = [{
                "name": "build", "image": "golang:1.25-alpine",
                "securityContext": {"runAsUser": 65532, "runAsGroup": 65532, "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                "env": [{"name": "GOCACHE", "value": "/build/cache"}, {"name": "GOMODCACHE", "value": "/build/modules"}],
                "command": ["sh", "-ec", "tar xzf /source/source.tgz -C /build; cd /build; go mod tidy; CGO_ENABLED=0 go build -o /output/controller ./cmd/healthchecks-kubernetes; cp /etc/ssl/certs/ca-certificates.crt /output/ca-certificates.crt"],
                "resources": {"requests": {"cpu": "250m", "memory": "256Mi"}, "limits": {"cpu": "2", "memory": "2Gi"}},
                "volumeMounts": [{"name": "source", "mountPath": "/source", "readOnly": True}, {"name": "build", "mountPath": "/build"}, {"name": "output", "mountPath": "/output"}],
            }]
            container = pod["containers"][0]
            container["image"] = "alpine:3.23"
            container["command"] = ["/output/controller"]
            container["env"].append({"name": "SSL_CERT_FILE", "value": "/output/ca-certificates.crt"})
            container["volumeMounts"] = [{"name": "output", "mountPath": "/output", "readOnly": True}]
        apply(resources)
        run(kube + ["rollout", "status", "deployment/healthchecks-smoke", "--timeout=10m"])
        print("PASS: controller build and startup", flush=True)
        apply([cronjob("successful"), cronjob("failing", fail=True)])
        wait_for("checks discovered", lambda: len(checks()) == 2)
        initial_ids = {check["uuid"] for check in checks()}
        wait_for("new checks initialized", lambda: all(check["status"] != "new" for check in checks()))

        def delivered(name, signal):
            for job in get("jobs")["items"]:
                if not any(owner["name"] == name and owner["kind"] == "CronJob" for owner in job["metadata"].get("ownerReferences", [])):
                    continue
                marker = job["metadata"].get("annotations", {}).get("healthchecks.io/delivery", "{}")
                if json.loads(marker).get("signal") == signal:
                    return True
            return False

        wait_for("successful Job reported", lambda: delivered("successful", "success"))
        wait_for("failed Job reported after retries", lambda: delivered("failing", "fail"))
        patch_job("successful", {"spec": {"schedule": "*/2 * * * *"}})
        wait_for("schedule update synced", lambda: by_job("successful")["schedule"] == "*/2 * * * *")
        patch_job("successful", {"spec": {"suspend": True}})
        wait_for("suspended CronJob paused", lambda: by_job("successful")["status"] == "paused")
        patch_job("successful", {"spec": {"suspend": False}})
        wait_for("CronJob resumed", lambda: by_job("successful")["status"] != "paused")
        run(kube + ["rollout", "restart", "deployment/healthchecks-smoke"])
        run(kube + ["rollout", "status", "deployment/healthchecks-smoke", "--timeout=10m"])
        wait_for("restart preserved check identities", lambda: {check["uuid"] for check in checks()} == initial_ids)
        run(kube + ["delete", "cronjob", "successful", "failing", "--wait=true"])
        wait_for("deleted CronJobs paused", lambda: len(checks()) == 2 and all(check["status"] == "paused" for check in checks()))
        print("Live smoke test passed.", flush=True)
    finally:
        if created:
            # Stop the controller before deleting remote checks, or reconciliation
            # could recreate them. Namespace deletion also removes build artifacts.
            try:
                run(kube + ["delete", "namespace", namespace, "--wait=true", "--timeout=120s"])
            except RuntimeError:
                raise RuntimeError(f"cleanup incomplete: review test namespace {namespace} before deleting its checks") from None
            try:
                for check in checks():
                    if owner in check.get("tags", "").split() and check["name"].startswith(f"{cluster}/{namespace}/"):
                        api("DELETE", "/checks/" + check["uuid"])
            except RuntimeError:
                raise RuntimeError(f"namespace removed; manually clean up checks tagged {owner}") from None



if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, KeyError, OSError) as error:
        # OSError can contain local paths; do not expose arbitrary exception text.
        message = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        print(f"Smoke test stopped: {message}", file=sys.stderr)
        sys.exit(1)

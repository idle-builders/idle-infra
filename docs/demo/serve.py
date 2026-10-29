#!/usr/bin/env python3
"""Render the actual console UI with fictional data for documentation screenshots.

This is a loopback-only visual preview, not a console integration test. It reads
no credentials, kubeconfig, network services or deployment configuration.
"""
import json
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[2]
TENANTS = ("atlas", "beacon", "cedar")
NOW = datetime.now(timezone.utc)


def ago(minutes):
    return (NOW - timedelta(minutes=minutes)).isoformat()


def section(data):
    return {"status": "ok", "source": "documentation demo", "data": data}


def project(name):
    i = TENANTS.index(name)
    return {
        "name": name, "namespace": name,
        "workloads": section([{"name": name + "-api", "ready": 2, "replicas": 2}]),
        "quota": section({"cpuUsedRatio": .18 + i * .06, "memoryUsedRatio": .26 + i * .04}),
        "database": section({"schema": name, "sizeBytes": (420 + i * 75) * 10**6, "tableCount": 18 + i * 4}),
        "storage": section({"bucket": name + "-files", "objectCount": 12480 + i * 1800, "bytesUsed": (24 + i * 8) * 10**9}),
        "auth": section({"org": name, "applications": ["web", "api"], "userCount": 320 + i * 80}),
        "domains": section([{"host": name + ".example.com", "tlsSource": "Cloudflare"}]),
    }


NODES = [{"name": "worker-0" + str(i + 1), "ready": True,
          "cpuUsedRatio": .18 + i * .04, "memoryUsedRatio": .36 + i * .03,
          "pods": 12 + i, "kubeletVersion": "v1.35.0"} for i in range(3)]
PROJECTS = [{"name": n, "health": "ok", "podsReady": 2,
             "podsDesired": 2, "schema": n} for n in TENANTS]


def response(path):
    if path == "/v1/projects":
        return {"projects": PROJECTS}
    if path == "/v1/platform":
        return {"nodes": section(NODES), "projects": section(PROJECTS),
                "capacity": section({"cpuRequestedRatio": .38, "memoryRequestedRatio": .46,
                                     "source": "resource requests / allocatable"}),
                "backups": section({"lastSuccessAt": ago(42), "schedule": "0 */6 * * *",
                                    "restoreVerifiedAt": ago(180)})}
    if path == "/v1/platform/nodes":
        return {"nodes": NODES}
    for name in TENANTS:
        if path == "/v1/projects/" + name:
            return project(name)
    # Intentionally do not fabricate data for views outside the screenshot set.
    return None


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        path = urlparse(self.path).path
        if path in ("/", "/index.html"):
            body = (ROOT / "docker/infra-console/web/index.html").read_text()
            body = body.replace("<title>infra console</title>", "<title>Idle Infra — documentation demo</title>")
            body = body.replace('    infra\n  </span>', '    infra <small style="font-weight:400;color:var(--dim)">/ demo</small>\n  </span>')
            encoded = body.encode()
            content_type = "text/html; charset=utf-8"
            status = 200
        else:
            data = response(path)
            status = 200 if data is not None else 501
            encoded = json.dumps(data if data is not None else {
                "source": "documentation demo", "detail": "This view is outside the screenshot fixture set."
            }).encode()
            content_type = "application/json"
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(encoded)


if __name__ == "__main__":
    print("Documentation demo: http://127.0.0.1:8765 (Ctrl-C to stop)", flush=True)
    ThreadingHTTPServer(("127.0.0.1", 8765), Handler).serve_forever()

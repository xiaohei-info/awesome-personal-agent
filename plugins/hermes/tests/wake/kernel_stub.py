"""Stand-in kernel HTTP endpoint speaking the delivery-claim and run-credential contract (api/deliveries, api/runs).

Test boundary only: the real kernel routes are covered in api/deliveries and api/runs tests. Mirrors their rules:
the plugin token claims wake deliveries, exchanges run ids, and a run credential is only valid for its own run.
"""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PLUGIN_TOKEN = "plugin-token"


class Kernel:
    def __init__(self):
        self.deliveries = []   # queued deliveries (dicts as domain.Delivery JSON)
        self.reports = []      # (kind, id, body)
        self.cli_posts = []    # (path, bearer, body) of POSTs made under a run credential
        self.cli_calls = []    # (run header, bearer) seen on run-identity requests
        self.exchanges = 0
        self.fail_exchange = False
        self.ttl = "2099-01-01T00:00:00Z"
        k = self

        class H(BaseHTTPRequestHandler):
            def _send(self, code, body=None):
                data = json.dumps(body).encode() if body is not None else b""
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def _auth(self):
                return self.headers.get("Authorization") == "Bearer " + PLUGIN_TOKEN

            def do_GET(self):
                if not self.path.startswith("/v1/deliveries/") and self.headers.get("Authorization", "").startswith("Bearer apa_run_"):
                    k.cli_calls.append((self.path, self.headers["Authorization"]))  # a run-identity CLI call
                    return self._send(200, {"ok": True})
                if not self._auth():
                    return self._send(401, {"error": "bad token"})
                if self.path.startswith("/v1/deliveries/next") and k.deliveries:
                    return self._send(200, k.deliveries.pop(0))
                self._send(204)

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n) or b"{}")
                if self.headers.get("Authorization", "").startswith("Bearer apa_run_"):
                    k.cli_posts.append((self.path, self.headers["Authorization"], body))
                    return self._send(200, {"id": "it1", "phase": "needs_input"})
                if not self._auth():
                    return self._send(401, {"error": "bad token"})
                parts = self.path.split("/")  # "", v1, runs|deliveries, id, credential|ack|fail
                if parts[2] == "runs":
                    if k.fail_exchange:
                        return self._send(409, {"error": "run is not a wake run"})
                    k.exchanges += 1
                    return self._send(200, {"run": parts[3], "token": f"apa_run_{parts[3]}_{k.exchanges}", "expires_at": k.ttl})
                k.reports.append((parts[4], parts[3], body))
                self._send(200, {"id": parts[3]})

            def log_message(self, *a):
                pass

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.srv.server_port}"

    def wake(self, did="wake_r1", run="r1", version=1):
        self.deliveries.append({"id": did, "item": "it1", "run": run, "class": "wake", "route": "wake", "version": version,
                                "content": {"title": f"wake {run}"}})

    def stop(self):
        self.srv.shutdown()
        self.srv.server_close()

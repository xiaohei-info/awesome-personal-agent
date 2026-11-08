import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest


@pytest.fixture
def kernel():
    """Stand-in kernel HTTP endpoint (test boundary only): records each request and answers 200."""
    state = {"log": [], "reply": {"captured": True, "event": "evt_1"}, "status": 200}

    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            state["log"].append({"path": self.path, "session": self.headers.get("X-APA-Session"),
                                 "auth": self.headers.get("Authorization"),
                                 "evidence": (self.headers.get("X-APA-Evidence") or "").encode("latin-1").decode("utf-8") or None, "body": json.loads(self.rfile.read(n) or b"null")})
            data = json.dumps(state["reply"]).encode()
            self.send_response(state["status"])
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *a):
            pass

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    state["url"] = f"http://127.0.0.1:{srv.server_port}"
    yield state
    srv.shutdown()


def posts(kernel, n, wait=15):
    """Requests the detached helper made (asynchronous); waits for n, then a moment for a surplus one."""
    end = time.time() + wait
    while time.time() < end and len(kernel["log"]) < n:
        time.sleep(0.05)
    time.sleep(0.4)
    return kernel["log"]

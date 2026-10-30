"""The two Serverspace endpoints the node's watchdog uses, in memory.

GET /_state shows what was deleted, for the test to assert on.
"""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

KEY = os.environ["API_KEY"]
servers = {"l1s1": {"id": "l1s1", "name": os.environ["NODE_NAME"]}, "l1s2": {"id": "l1s2", "name": "someone-else"}}
deleted = []


class Handler(BaseHTTPRequestHandler):
    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def authorized(self):
        if self.headers.get("X-API-KEY") != KEY:
            self.reply(401, {"errors": [{"message": "bad key"}]})
            return False
        return True

    def do_GET(self):
        if self.path == "/_state":
            return self.reply(200, {"deleted": deleted})
        if self.path == "/api/v1/servers" and self.authorized():
            return self.reply(200, {"servers": list(servers.values())})
        if not self.path.startswith("/api/"):
            self.reply(404, {})

    def do_DELETE(self):
        if not self.authorized():
            return
        sid = self.path.rsplit("/", 1)[-1]
        if servers.pop(sid, None) is None:
            return self.reply(404, {})
        deleted.append(sid)
        self.reply(200, {"task_id": "t-" + sid})


ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

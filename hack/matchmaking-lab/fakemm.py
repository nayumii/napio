"""Lab matching server.

auth.factorio.com is proxied to the real service (that endpoint takes no
credentials), so the dedicated server gets a genuine padlock and proceeds to
the matchmaking step. multiplayer.factorio.com is faked, which is what lets us
capture exactly what a real Factorio dedicated server sends when it registers
and heartbeats a public game.
"""
import http.server, json, ssl, time, urllib.request

LOG = "/log/requests.jsonl"
state = {"next_game_id": 4242}


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _record(self, body, note=""):
        entry = {
            "t": round(time.time(), 3),
            "host": self.headers.get("Host", ""),
            "method": self.command,
            "path": self.path,
            "headers": dict(self.headers),
            "body": body.decode("utf-8", "replace") if body else "",
            "note": note,
        }
        print(json.dumps(entry), flush=True)
        with open(LOG, "a") as fh:
            fh.write(json.dumps(entry) + "\n")

    def _read(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def _reply(self, obj, code=200):
        payload = obj if isinstance(obj, bytes) else json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _passthrough(self, body):
        url = "https://auth.factorio.com" + self.path
        req = urllib.request.Request(url, data=body or None, method=self.command)
        req.add_header("User-Agent", self.headers.get("User-Agent", "napio-lab"))
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                payload = resp.read()
                self._record(payload, note="auth passthrough response")
                self._reply(payload, resp.status)
        except Exception as exc:
            self._record(b"", note="auth passthrough failed: %s" % exc)
            self._reply({"message": str(exc)}, 502)

    def _route(self):
        body = self._read()
        self._record(body)
        host = self.headers.get("Host", "")
        path = self.path.split("?")[0]

        if "auth.factorio.com" in host:
            return self._passthrough(body)

        if path.startswith("/create-game"):
            gid = state["next_game_id"]
            state["next_game_id"] += 1
            return self._reply({"game_id": gid, "game_secret": "lab-secret-%d" % gid})
        if path.startswith("/post-game-heartbeat"):
            return self._reply({"success": True})
        if path.startswith("/remove-game"):
            return self._reply({"success": True})
        return self._reply({"message": "lab: unhandled " + path}, 404)

    do_GET = do_POST = do_PUT = _route

    def log_message(self, *a):
        pass


ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain("/certs/srv.crt", "/certs/srv.key")
srv = http.server.ThreadingHTTPServer(("0.0.0.0", 443), H)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
print("lab matching server listening on 443", flush=True)
srv.serve_forever()

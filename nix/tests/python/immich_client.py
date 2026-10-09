"""Immich REST client for the media-immich VM test; prints JSON.

usage: immich_client.py URL EMAIL PASSWORD COMMAND [ARGS]
  setup                  create the administrator unless one exists
  login                  print a fresh access token
  token-ok TOKEN         print whether a token from an earlier login is still accepted
  upload PREFIX N        upload N distinct PNGs, one acknowledged at a time; print {id: sha1}
  assets                 print {id: sha1} of every asset
  original-sha1 IDS      print {id: sha1} of each comma-separated asset's downloaded original
  queue NAME             print the queue's state
  pause NAME / resume NAME
  wait-thumbs IDS SECS   wait until every comma-separated asset has a thumbnail
"""

import base64
import hashlib
import json
import struct
import sys
import time
import zlib

import requests

TIMEOUT = 30


def png(name):
    """A valid 16x16 PNG whose pixels depend on name, so every upload is distinct."""
    def chunk(kind, data):
        body = kind + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body))
    rows = b"".join(b"\x00" + hashlib.sha256(f"{name}/{y}".encode()).digest()[:16] * 3 for y in range(16))
    header = struct.pack(">IIBBBBB", 16, 16, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", zlib.compress(rows)) + chunk(b"IEND", b"")


def checksum_sha1(b64):
    return base64.b64decode(b64).hex()


class Immich:
    def __init__(self, url, email, password):
        self.api = url.rstrip("/") + "/api"
        self.email, self.password = email, password
        self.headers = {}

    def call(self, method, path, **kw):
        r = requests.request(method, self.api + path, headers=self.headers, timeout=TIMEOUT, **kw)
        if r.status_code >= 400:
            raise SystemExit(f"{method} {path}: {r.status_code} {r.text[:300]}")
        return r

    def login(self):
        token = self.call("POST", "/auth/login", json={"email": self.email, "password": self.password}).json()["accessToken"]
        self.headers = {"Authorization": f"Bearer {token}"}
        return token

    def setup(self):
        if not self.call("GET", "/server/config").json()["isInitialized"]:
            self.call("POST", "/auth/admin-sign-up", json={"email": self.email, "password": self.password, "name": "Admin"})
        return True

    def upload(self, prefix, n):
        done = {}
        for i in range(n):
            data = png(f"{prefix}{i}")
            fields = {"deviceAssetId": f"{prefix}{i}", "deviceId": "expanse-test",
                      "fileCreatedAt": "2026-01-01T00:00:00Z", "fileModifiedAt": "2026-01-01T00:00:00Z"}
            r = self.call("POST", "/assets", data=fields, files={"assetData": (f"{prefix}{i}.png", data, "image/png")})
            if r.json().get("status") != "created":
                raise SystemExit(f"upload {prefix}{i}: {r.text}")
            done[r.json()["id"]] = hashlib.sha1(data).hexdigest()
        return done

    def assets(self):
        found, page = {}, 1
        while page:
            res = self.call("POST", "/search/metadata", json={"page": page, "size": 1000}).json()["assets"]
            found.update({a["id"]: checksum_sha1(a["checksum"]) for a in res["items"]})
            page = res.get("nextPage") and int(res["nextPage"])
        return found

    def original_sha1(self, ids):
        return {i: hashlib.sha1(self.call("GET", f"/assets/{i}/original").content).hexdigest() for i in ids}

    def wait_thumbs(self, ids, seconds):
        deadline, waiting = time.time() + seconds, set(ids)
        while waiting:
            waiting = {i for i in waiting if not self.call("GET", f"/assets/{i}").json().get("thumbhash")}
            if waiting and time.time() > deadline:
                raise SystemExit(f"{len(waiting)} of {len(ids)} assets have no thumbnail after {seconds}s: {sorted(waiting)}")
            time.sleep(1 if waiting else 0)
        return True


def main(url, email, password, command, *args):
    im = Immich(url, email, password)
    if command == "setup":
        return im.setup()
    if command == "token-ok":
        r = requests.get(im.api + "/users/me", headers={"Authorization": f"Bearer {args[0]}"}, timeout=TIMEOUT)
        return r.status_code == 200
    token = im.login()
    if command == "login":
        return token
    if command == "upload":
        return im.upload(args[0], int(args[1]))
    if command == "assets":
        return im.assets()
    if command == "original-sha1":
        return im.original_sha1(args[0].split(","))
    if command == "queue":
        return im.call("GET", f"/queues/{args[0]}").json()
    if command in ("pause", "resume"):
        return im.call("PUT", f"/queues/{args[0]}", json={"isPaused": command == "pause"}).json()
    if command == "wait-thumbs":
        return im.wait_thumbs(args[0].split(","), int(args[1]))
    raise SystemExit(f"unknown command {command}")


if __name__ == "__main__":
    print(json.dumps(main(*sys.argv[1:])))

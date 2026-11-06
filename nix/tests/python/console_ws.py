"""Open a VM's console WebSocket through a node's web UI, send input and print what came back.

Usage: console_ws.py HOST PATH COOKIE_JAR CA_FILE INPUT WANT [TIMEOUT]
       console_ws.py --status HOST PATH COOKIE_JAR CA_FILE ORIGIN
COOKIE_JAR is curl's (`-c`); the script exits 1 unless the output contains WANT in time.
--status prints the handshake's HTTP status when sent from ORIGIN (101 when accepted).
"""

import asyncio
import ssl
import sys

from websockets.asyncio.client import connect
from websockets.exceptions import InvalidStatus


def cookies(jar):
    """The Cookie header for curl's cookie jar, including its HttpOnly lines."""
    pairs = []
    for line in open(jar):
        line = line.strip()
        if line.startswith("#HttpOnly_"):
            line = line[len("#HttpOnly_"):]
        elif not line or line.startswith("#"):
            continue
        fields = line.split("\t")
        if len(fields) == 7:
            pairs.append(f"{fields[5]}={fields[6]}")
    return "; ".join(pairs)


async def run(host, path, jar, cafile, data, want, timeout):
    origin = f"https://{host}:8443"
    ctx = ssl.create_default_context(cafile=cafile)
    got = b""
    async with connect(f"wss://{host}:8443{path}", ssl=ctx, origin=origin,
                       additional_headers={"Cookie": cookies(jar)}) as ws:
        await ws.send(data.encode())
        try:
            async with asyncio.timeout(timeout):
                while want.encode() not in got:
                    msg = await ws.recv()
                    got += msg if isinstance(msg, bytes) else msg.encode()
        except TimeoutError:
            pass
    sys.stdout.write(got.decode(errors="replace"))
    return want.encode() in got


async def status(host, path, jar, cafile, origin):
    ctx = ssl.create_default_context(cafile=cafile)
    try:
        async with connect(f"wss://{host}:8443{path}", ssl=ctx, origin=origin,
                           additional_headers={"Cookie": cookies(jar)}):
            return 101
    except InvalidStatus as e:
        return e.response.status_code


if __name__ == "__main__" and sys.argv[1] == "--status":
    print(asyncio.run(status(*sys.argv[2:7])))
elif __name__ == "__main__":
    host, path, jar, cafile, data, want = sys.argv[1:7]
    timeout = float(sys.argv[7]) if len(sys.argv) > 7 else 20
    sys.exit(0 if asyncio.run(run(host, path, jar, cafile, data.encode().decode("unicode_escape"), want, timeout)) else 1)

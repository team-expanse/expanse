"""Record, writer, ledger and verifier for the vol-durability gate (X1).

Record i is one 4 KiB block at block offset i, fully determined by i, so any node can
regenerate the expected bytes. The writer counts a record as acked only after fsync
returns and an off-node ledger has stored its sequence number.

Subcommands:
  write DEV HOST PORT START PAUSE_MS   write records from START until killed
  ledger PORT FILE                     store acked sequence numbers, replying "ok"
  highest FILE                         print the largest acked sequence (-1 if none)
  verify DEV COUNT                     exit 3 and list records 0..COUNT-1 that are wrong
  digest DEV NBYTES                    sha256 of the first NBYTES, read past the cache
"""

import hashlib
import mmap
import os
import socket
import socketserver
import sys
import time

BLOCK = 4096
CHUNK = 1 << 20
WRITE_LIMIT = 60000


def record(seq):
    header = b"DURABREC seq=%d\n" % seq
    return header + bytes([seq % 251 + 1]) * (BLOCK - len(header))


def read_chunks(path, nbytes, direct=True):
    """Yield the first nbytes of path in CHUNK pieces; O_DIRECT bypasses the page cache."""
    fd = os.open(path, os.O_RDONLY | (os.O_DIRECT if direct else 0))
    buf = mmap.mmap(-1, CHUNK)  # page-aligned, as O_DIRECT requires
    try:
        offset = 0
        while offset < nbytes:
            got = os.preadv(fd, [buf], offset)
            if got <= 0:
                return
            yield bytes(buf[: min(got, nbytes - offset)])
            offset += got
    finally:
        buf.close()
        os.close(fd)


def mismatched(path, count, direct=True, limit=10):
    """The sequences in 0..count-1 whose block is not the record, at most limit of them."""
    bad, seq = [], 0
    for chunk in read_chunks(path, count * BLOCK, direct):
        for start in range(0, len(chunk), BLOCK):
            if seq < count and chunk[start : start + BLOCK] != record(seq):
                bad.append(seq)
            seq += 1
    bad.extend(range(seq, count))
    return bad[:limit]


def digest(path, nbytes, direct=True):
    h = hashlib.sha256()
    for chunk in read_chunks(path, nbytes, direct):
        h.update(chunk)
    return h.hexdigest()


def highest(path):
    """Largest sequence in a ledger file; an unterminated last line was never acked."""
    try:
        with open(path) as f:
            lines = f.read().split("\n")[:-1]
    except FileNotFoundError:
        return -1
    return max((int(line) for line in lines), default=-1)


class _Ledger(socketserver.ThreadingTCPServer):
    daemon_threads = True
    allow_reuse_address = True  # a killed writer leaves connections that would block a rebind


class _LedgerHandler(socketserver.StreamRequestHandler):
    def handle(self):
        with open(self.server.path, "a") as out:
            for line in self.rfile:
                out.write(line.decode())
                out.flush()
                os.fsync(out.fileno())
                self.wfile.write(b"ok\n")


def make_ledger(host, port, path):
    server = _Ledger((host, port), _LedgerHandler)
    server.path = path
    return server


def write_loop(dev, ledger, start, limit=WRITE_LIMIT, pause=0.0):
    fd = os.open(dev, os.O_WRONLY)
    with socket.create_connection(ledger) as sock:
        stream = sock.makefile("rw")
        for seq in range(start, limit):
            os.pwrite(fd, record(seq), seq * BLOCK)
            os.fsync(fd)
            stream.write(f"{seq}\n")
            stream.flush()
            if stream.readline().strip() != "ok":
                raise SystemExit(f"ledger refused record {seq}")
            time.sleep(pause)
    os.close(fd)


def main(argv):
    cmd, args = argv[1], argv[2:]
    if cmd == "write":
        dev, host, port, start, pause_ms = args
        write_loop(dev, (host, int(port)), int(start), pause=int(pause_ms) / 1000)
    elif cmd == "ledger":
        make_ledger("0.0.0.0", int(args[0]), args[1]).serve_forever()
    elif cmd == "highest":
        print(highest(args[0]))
    elif cmd == "verify":
        bad = mismatched(args[0], int(args[1]))
        if bad:
            print("bad records:", *bad)
            return 3
    elif cmd == "digest":
        print(digest(args[0], int(args[1])))
    else:
        raise SystemExit(f"unknown subcommand: {cmd}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

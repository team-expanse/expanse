"""Record helpers for the vol-durability release-blocker test (§4.8).

Record layout: 4 KiB blocks, record i at block offset i. Content is
fully determined by the sequence number, so the checker regenerates
the expected bytes anywhere, any time (no state survives on nodes).

Subcommands:
  write SEQ PATH    write record SEQ to PATH (4 KiB, deterministic)
  check SEQ PATH    exit 0 iff PATH's block SEQ equals record SEQ
  blocks DEV        print per-4KiB-block short hashes of DEV's 16 MiB head
"""

import hashlib
import sys

BLOCK = 4096


def rec(seq: int) -> bytes:
    hdr = b"DURABREC seq=%d\n" % seq
    return hdr + bytes([(seq % 251) + 1]) * (BLOCK - len(hdr))


def main() -> None:
    cmd = sys.argv[1]
    if cmd == "write":
        seq, path = int(sys.argv[2]), sys.argv[3]
        with open(path, "wb") as f:
            f.write(rec(seq))
    elif cmd == "check":
        seq, path = int(sys.argv[2]), sys.argv[3]
        with open(path, "rb") as f:
            f.seek(BLOCK * seq)
            data = f.read(BLOCK)
        sys.exit(0 if data == rec(seq) else 3)
    elif cmd == "blocks":
        # --direct: bypass the host page cache (os.O_DIRECT on a block
        # device). A buffered read can otherwise return pre-resync
        # cached content forever, masking (or faking) divergence.
        direct = "--direct" in sys.argv
        sys.argv = [a for a in sys.argv if a != "--direct"]
        if direct:
            import mmap
            import os
            fd = os.open(sys.argv[2], os.O_RDONLY | os.O_DIRECT)
            # O_DIRECT requires a page-aligned buffer of block-size
            # multiples.
            buf = mmap.mmap(-1, 16 * 1024 * 1024)
            d = b""
            off = 0
            while off < 16 * 1024 * 1024:
                n = os.preadv(fd, [buf], off)
                if n <= 0:
                    break
                off += n
            d = buf[:off]
            buf.close()
            os.close(fd)
        else:
            with open(sys.argv[2], "rb") as f:
                d = f.read(16 * 1024 * 1024)
        print(" ".join(
            hashlib.sha256(d[i:i + BLOCK]).hexdigest()[:8]
            for i in range(0, len(d), BLOCK)))
    else:
        raise SystemExit(f"unknown subcommand: {cmd}")


if __name__ == "__main__":
    main()

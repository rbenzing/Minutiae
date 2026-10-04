#!/usr/bin/env python3
"""Make E01 segment files reproducible: pin the random set identifier.

Usage: ewf_normalize.py <segment files...>

Some writers put a random GUID (the set identifier, volume payload offset 64)
into the `volume` section of every segment set and into its `data` copies.
This rewrites that one field, in place, to a fixed GUID and recomputes the
Adler-32 of each touched section payload (volume payload offset 1048). Nothing
else changes: not the descriptors (their checksum covers the descriptor only),
not the chunks.

A set identifier that is already all zero is left alone: a writer that emits
zeros is deterministic already, and writing a GUID would make the fixture
differ from what the tool produced. The number of rewritten sections is
printed to stderr so the generator log shows whether the normaliser did
anything for the installed tool version.

Standard library only.
"""
import struct
import sys
import zlib

FIXED_ID = bytes.fromhex("5b2c1e3a7d4f4a869c102e8f6a4d3b71")
SIGNATURE = b"EVF\x09\x0d\x0a\xff\x00"
SEG_HEADER = 13
DESC = 76
VOLUME_PAYLOAD = 1052
ID_OFF = 64
SUM_OFF = 1048


def normalize(path):
    buf = bytearray(open(path, "rb").read())
    if buf[:8] != SIGNATURE:
        raise SystemExit("%s: not an EWF segment" % path)
    changed = 0
    off = SEG_HEADER
    seen = set()
    while True:
        if off in seen or off + DESC > len(buf):
            raise SystemExit("%s: bad section offset %d" % (path, off))
        seen.add(off)
        typ = bytes(buf[off:off + 16]).rstrip(b"\0")
        nxt, size = struct.unpack_from("<QQ", buf, off + 16)
        if typ in (b"volume", b"disk", b"data"):
            pay = off + DESC
            if size != DESC + VOLUME_PAYLOAD or pay + VOLUME_PAYLOAD > len(buf):
                raise SystemExit("%s: %s section has size %d" % (path, typ.decode(), size))
            cur = bytes(buf[pay + ID_OFF:pay + ID_OFF + 16])
            if any(cur) and cur != FIXED_ID:
                buf[pay + ID_OFF:pay + ID_OFF + 16] = FIXED_ID
                struct.pack_into("<I", buf, pay + SUM_OFF,
                                 zlib.adler32(bytes(buf[pay:pay + SUM_OFF])) & 0xFFFFFFFF)
                changed += 1
        if typ in (b"next", b"done"):
            break
        if nxt <= off or nxt > len(buf):
            raise SystemExit("%s: bad next pointer %d at %d" % (path, nxt, off))
        off = nxt
    if changed:
        open(path, "wb").write(buf)
    return changed


def main(argv):
    if not argv:
        print(__doc__, file=sys.stderr)
        return 2
    total = sum(normalize(p) for p in argv)
    print("ewf_normalize: %d section(s) rewritten in %d file(s)" % (total, len(argv)),
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

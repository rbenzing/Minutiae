#!/usr/bin/env python3
"""Make a freshly formatted HFS+ image byte-reproducible.

mkfs.hfsplus (hfsprogs 540.1.linux3) draws the 64-bit volume id (finderInfo
words 6 and 7, bytes 104..111 of the volume header) from a random source, and
the faketime clock does not reach it. Everything else it writes was checked to
be identical across runs (cmp of two runs, four formats). This rewrites that
id, in the primary header (offset 1024) and in the alternate header (1024
bytes before the end of the image; for an HFS-wrapped image, of the embedded
volume), to a fixed value, the way
f2fs_normalize.py handles the f2fs UUID.

usage: hfsplus_normalize.py <image> <16 hex digits>
"""
import struct
import sys

ID_OFFSET = 104  # finderInfo[6..7] inside the volume header


def main() -> None:
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    path, hexid = sys.argv[1], sys.argv[2]
    vid = bytes.fromhex(hexid)
    if len(vid) != 8:
        sys.exit("volume id must be 8 bytes (16 hex digits)")
    with open(path, "r+b") as f:
        f.seek(0, 2)
        size = f.tell()
        base, length = 0, size
        f.seek(1024)
        if f.read(2) == b"BD":
            # HFS wrapper: the id lives in the embedded HFS+ volume
            f.seek(1024 + 20)
            alblksiz = struct.unpack(">I", f.read(4))[0]
            f.seek(1024 + 28)
            alblst = struct.unpack(">H", f.read(2))[0]
            f.seek(1024 + 126)
            start, count = struct.unpack(">HH", f.read(4))
            base, length = alblst * 512 + start * alblksiz, count * alblksiz
        for off in (base + 1024, base + length - 1024):
            f.seek(off)
            sig = f.read(2)
            if sig not in (b"H+", b"HX"):
                sys.exit("no HFS+ volume header at offset %d" % off)
            f.seek(off + ID_OFFSET)
            f.write(vid)
    with open(path, "rb") as f:
        f.seek(base + 1024 + ID_OFFSET)
        if f.read(8) != vid:
            sys.exit("volume id write failed")


main()

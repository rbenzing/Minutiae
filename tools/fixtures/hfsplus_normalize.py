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
       hfsplus_normalize.py linkid <image> <digits>

The second form is for the populated fixture. The Linux driver names the
hidden inode of a hard-linked file "iNode<random number>" (a number below 2^30,
from get_random_bytes), and stores that number in the name (UTF-16, twice: the
catalog key and the thread record) and in the hard-link records' special field
(big-endian 32 bit, once per link plus the inode record). With one hard-linked
file the number is the only random byte of the populated image. This rewrites
it to <digits> when both have the same number of digits (exit 3 otherwise: the
caller runs the guest again) and checks the expected number of occurrences.
"""
import struct
import sys

ID_OFFSET = 104  # finderInfo[6..7] inside the volume header


def linkid(path, digits):
    import re

    with open(path, "rb") as f:
        d = bytearray(f.read())
    prefix = "iNode".encode("utf-16-be")
    pat = re.compile(re.escape(prefix) + rb"((?:\x00[0-9])+)")
    found = {m.group(0) for m in pat.finditer(d)}
    if len(found) != 1:
        sys.exit("expected exactly one distinct iNode<number> name, found %d" % len(found))
    old = next(iter(found))
    old_digits = old[len(prefix):].replace(b"\x00", b"")
    if len(old_digits) != len(digits):
        sys.exit(3)
    new = prefix + digits.encode("utf-16-be")
    names = d.count(old)
    nums = d.count(struct.pack(">I", int(old_digits)))
    if names != 2 or nums < 2:
        sys.exit("unexpected number of occurrences: %d names, %d numbers" % (names, nums))
    d = d.replace(old, new).replace(struct.pack(">I", int(old_digits)), struct.pack(">I", int(digits)))
    with open(path, "wb") as f:
        f.write(d)
    print("link id %s -> %s (%d names, %d numbers)" % (old_digits.decode(), digits, names, nums))


def main() -> None:
    if len(sys.argv) == 4 and sys.argv[1] == "linkid":
        linkid(sys.argv[2], sys.argv[3])
        return
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

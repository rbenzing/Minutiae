#!/usr/bin/env python3
"""Make a freshly built F2FS image independent of the machine that built it.

mkfs.f2fs and sload.f2fs write the running kernel's version string (uname) into
the superblock's version[256] and init_version[256] fields; the Docker Desktop
kernel string would otherwise end up in the fixture. This overwrites both
fields in both superblock copies with a fixed text and recomputes each copy's
checksum (when the sb_checksum feature is set; f2fs_crc32: CRC-32 with seed F2FS_SUPER_MAGIC, no inversion, over the
bytes before checksum_offset). It first checks that the stored checksums are
valid, so it cannot paper over a corrupt superblock; the generator then runs
fsck.f2fs, which verifies the new checksums independently.

usage: f2fs_normalize.py <image>
"""
import struct
import sys
import zlib

MAGIC = 0xF2F52010
SB_OFF = 1024
SB_SIZE = 3072
VERSION_OFF, INIT_VERSION_OFF, VERSION_LEN = 1668, 1924, 256
FEATURE_OFF, CHKSUM_FEATURE, CHKSUM_OFF_FIELD = 2180, 0x800, 32
TEXT = b"Linux version minutiae-fixture (version string normalised by f2fs_normalize.py)"


def raw_crc32(seed: int, data: bytes) -> int:
    """The kernel's crc32_le: standard CRC-32 without the initial/final inversion."""
    return (~zlib.crc32(data, ~seed & 0xFFFFFFFF)) & 0xFFFFFFFF


def main() -> None:
    path = sys.argv[1]
    with open(path, "r+b") as f:
        for blk in (0, 1):
            base = blk * 4096 + SB_OFF
            f.seek(base)
            sb = bytearray(f.read(SB_SIZE))
            if struct.unpack_from("<I", sb, 0)[0] != MAGIC:
                sys.exit("f2fs_normalize: superblock copy %d has a bad magic" % blk)
            # Without the sb_checksum feature the superblock has no checksum.
            checksummed = bool(struct.unpack_from("<I", sb, FEATURE_OFF)[0] & CHKSUM_FEATURE)
            off = struct.unpack_from("<I", sb, CHKSUM_OFF_FIELD)[0]
            if checksummed:
                if off > SB_SIZE - 4:
                    sys.exit("f2fs_normalize: bad checksum_offset %d" % off)
                if struct.unpack_from("<I", sb, off)[0] != raw_crc32(MAGIC, bytes(sb[:off])):
                    sys.exit("f2fs_normalize: superblock copy %d checksum is wrong before the change" % blk)
            for o in (VERSION_OFF, INIT_VERSION_OFF):
                sb[o:o + VERSION_LEN] = TEXT.ljust(VERSION_LEN, b"\0")
            if checksummed:
                struct.pack_into("<I", sb, off, raw_crc32(MAGIC, bytes(sb[:off])))
            f.seek(base)
            f.write(sb)


main()

#!/usr/bin/env python3
"""Deterministic source tree for the populated HFS+ fixture.

usage: hfsplus_tree.py <outdir>

Writes, under <outdir>:
  final/        the tree the volume must hold when the guest is done
  phase1.tar    everything the guest extracts first (final/ minus frag/big.bin,
                plus every frag/fNNN filler file)
  rmlist        the filler files the guest then deletes (one path per line,
                relative to the volume root), which leaves 1-block holes
  phase2.tar    frag/big.bin, written into the holes (a fragmented file with
                more than 8 extents, so it needs extents-overflow records)

Every byte of content and every mtime is a pure function of this script.
Names are written in NFC; the Linux hfsplus driver stores them decomposed (the
oracle accounts for that). Modes, mtimes and hard-link groups are set so the
oracle can compare them with what the volume records.
"""
import hashlib
import os
import shutil
import sys
import tarfile
import unicodedata

EPOCH = 1700000000  # 2023-11-14 22:13:20 UTC; the guest clock starts here too


def blob(tag, n):
    """n deterministic bytes: every 4096-byte block starts with a distinct
    SHA-256 digest of (tag, block index) and is otherwise zero, so a block
    mapped to the wrong place changes the content hash, yet the image
    compresses well."""
    out = bytearray()
    for i in range((n + 4095) // 4096):
        out += hashlib.sha256(("%s/%d" % (tag, i)).encode()).digest().ljust(4096, b"\x00")
    return bytes(out[:n])


def main():
    out = os.path.abspath(sys.argv[1])
    final = os.path.join(out, "final")
    if os.path.lexists(out):
        shutil.rmtree(out)
    os.makedirs(final)
    mt = {}  # path -> mtime, applied at the end (deepest first)
    clock = [EPOCH - 86400 * 30]

    def put(rel, data, mode=0o644, mtime=None):
        rel = unicodedata.normalize("NFC", rel)
        p = os.path.join(final, rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "wb") as f:
            f.write(data)
        os.chmod(p, mode)
        clock[0] += 61
        mt[p] = mtime if mtime is not None else clock[0]

    def mkdir(rel, mode=0o755):
        rel = unicodedata.normalize("NFC", rel)
        p = os.path.join(final, rel)
        os.makedirs(p, exist_ok=True)
        os.chmod(p, mode)
        clock[0] += 61
        mt[p] = clock[0]

    def link(target, rel):
        rel = unicodedata.normalize("NFC", rel)
        p = os.path.join(final, rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        os.symlink(target, p)

    put("hello.txt", b"Hello, HFS+\n")
    put("empty.txt", b"")
    put("secret.txt", b"mode 0600\n", 0o600)
    put("tool.sh", b"#!/bin/sh\necho hi\n", 0o755)
    put("group.txt", b"mode 0640\n", 0o640)
    put("with space.txt", b"a name with a space\n")
    put("MixedCase.TXT", b"case is kept, lookup folds it\n")
    mkdir("docs")
    put("docs/readme.txt", b"readme\n" * 40)
    mkdir("docs/private", 0o750)
    put("docs/private/notes.txt", b"private notes\n", 0o640)
    mkdir("docs/empty-dir")

    # Unicode: composed (NFC) in the source, stored decomposed by the driver.
    put("café.txt", b"cafe with an acute accent\n")
    put("über-Ångström.txt", b"umlauts and a ring\n")
    mkdir("日本語")
    put("日本語/ファイル.txt", b"japanese names\n")
    # Not a name outside the BMP: the Linux driver (utf8 nls) turns every byte of a
    # 4-byte UTF-8 sequence into "?", so such a name cannot be made (builder-only).
    put("snow-☃.txt", b"a symbol (BMP) name\n")
    put("документ.txt", b"cyrillic\n")
    # The longest name Linux allows (255 bytes), and the longest CJK one (85
    # characters of 3 UTF-8 bytes = 255 bytes; HFS+ allows 255 UTF-16 units but
    # the VFS limit is 255 bytes).
    mkdir("long")
    put("long/" + ("n" * 251) + ".txt", b"255 ascii characters\n")
    put("long/" + ("名" * 85), b"85 cjk characters\n")

    # Links: symlinks and a group of three hard links to one file.
    link("hello.txt", "link-to-hello")
    link("/docs/readme.txt", "docs/absolute-link")
    link("no-such-target", "dangling-link")
    mkdir("hl")
    put("hl/a.txt", b"one inode, three names\n", 0o644)
    os.link(os.path.join(final, "hl/a.txt"), os.path.join(final, "hl/b.txt"))
    os.link(os.path.join(final, "hl/a.txt"), os.path.join(final, "docs/c-link.txt"))
    put("hl/single.txt", b"not linked\n")

    # A big file (3 MiB) and a directory of 600 entries (a multi-level catalog).
    mkdir("big")
    put("big/three_mib.bin", blob("three_mib", 3 << 20))
    mkdir("many")
    for i in range(600):
        put("many/entry-%04d.txt" % i, b"entry %d\n" % i)

    # Fragmentation: 400 one-block fillers, the odd ones deleted by the guest,
    # then a 150-block file that fills the holes.
    mkdir("frag")
    for i in range(400):
        put("frag/f%03d" % i, blob("frag/f%03d" % i, 4096))
    big = blob("frag/big", 150 * 4096)
    put("frag/big.bin", big)
    rm = ["frag/f%03d" % i for i in range(1, 400, 2)]

    # mtimes: files first, then directories deepest first (a child's creation
    # changed its parent's).
    for p in sorted(mt, key=lambda q: -q.count("/")):
        os.utime(p, (mt[p], mt[p]), follow_symlinks=False)
    final_mtimes = dict(mt)

    # The final tree has no odd fillers.
    for r in rm:
        os.unlink(os.path.join(final, r))
    os.utime(os.path.join(final, "frag"), (final_mtimes[os.path.join(final, "frag")],) * 2)

    def walk():
        for d, dirs, files in os.walk(final):
            dirs.sort()
            for n in sorted(files) + dirs:
                yield os.path.join(d, n)

    def add(tf, p):
        ti = tf.gettarinfo(p, arcname=os.path.relpath(p, final).replace(os.sep, "/"))
        ti.uid = ti.gid = 0
        ti.uname = ti.gname = ""
        if ti.isreg():
            with open(p, "rb") as f:
                tf.addfile(ti, f)
        else:
            tf.addfile(ti)

    # phase 1: the final tree minus frag/big.bin, plus the odd fillers (their
    # content is not in final/: regenerate them in a scratch tree).
    scratch = os.path.join(out, "scratch")
    os.makedirs(scratch)
    for r in rm:
        i = int(r[-3:])
        with open(os.path.join(scratch, os.path.basename(r)), "wb") as f:
            f.write(blob("frag/f%03d" % i, 4096))
        os.utime(os.path.join(scratch, os.path.basename(r)), (EPOCH, EPOCH))
    bigp = os.path.join(final, "frag/big.bin")
    with tarfile.open(os.path.join(out, "phase1.tar"), "w", format=tarfile.GNU_FORMAT) as tf:
        entries = [(p, None) for p in walk() if p != bigp]
        entries += [(os.path.join(scratch, os.path.basename(r)), r) for r in rm]
        entries.sort(key=lambda e: os.path.relpath(e[0], final) if e[1] is None else e[1])
        for p, arc in entries:
            if arc is None:
                add(tf, p)
            else:
                ti = tf.gettarinfo(p, arcname=arc)
                ti.uid = ti.gid = 0
                ti.uname = ti.gname = ""
                with open(p, "rb") as f:
                    tf.addfile(ti, f)
    shutil.rmtree(scratch)
    with tarfile.open(os.path.join(out, "phase2.tar"), "w", format=tarfile.GNU_FORMAT) as tf:
        add(tf, bigp)
    with open(os.path.join(out, "rmlist"), "w") as f:
        f.write("".join(r + "\n" for r in rm))


if __name__ == "__main__":
    main()

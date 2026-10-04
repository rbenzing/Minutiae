#!/usr/bin/env python3
"""Deterministic source tree for the populated APFS fixture.

usage: apfs_tree.py <outdir>

Writes, under <outdir>:
  snap1/       the tree the volume holds when the snapshot "snap1" is taken
  final/       the tree it holds at the end (live tree)
  specials.json  what a directory tree cannot say: extended attributes,
               clone pairs, the interleaved (fragmented) and the sparse files
  phase1.tar   snap1/ minus the files made by ops1
  ops1         tab-separated operations (apfs_guest.c) that complete snap1/
               (the interleaved fragmented files, sparse files, a clone,
               xattrs, modes, owners, times), ending with the snapshot itself
  ops2         operations that make the live tree differ from snap1 (deletes)
  phase3.tar   files added or rewritten after the snapshot
  ops3         final attributes (modes, owners, times) of the changed files
               and of every directory

Every byte of content, mode, owner and time is a pure function of this script.
Names are written exactly as the volume must store them (the driver keeps the
bytes it is given: a composed name stays composed, a decomposed one stays
decomposed). The volume is normalization- and case-insensitive.
"""
import binascii
import hashlib
import json
import os
import shutil
import sys
import tarfile
import unicodedata

EPOCH = 1700000000  # 2023-11-14 22:13:20 UTC; the guest clock starts here too
BS = 4096


def blob(tag, n):
    """n deterministic bytes: every 4096-byte block starts with a distinct
    SHA-256 digest of (tag, block index) and is otherwise zero, so a block
    mapped to the wrong place changes the content hash, yet the image
    compresses well."""
    out = bytearray()
    for i in range((n + BS - 1) // BS):
        out += hashlib.sha256(("%s/%d" % (tag, i)).encode()).digest().ljust(BS, b"\x00")
    return bytes(out[:n])


def mtime_of(rel):
    """A distinct, deterministic (seconds, nanoseconds) per path, older than
    EPOCH by 1 to 30 days."""
    h = hashlib.sha256(rel.encode()).digest()
    sec = EPOCH - 86400 - int.from_bytes(h[:4], "big") % (29 * 86400)
    nsec = int.from_bytes(h[4:8], "big") % 1000000000
    return sec, nsec


class Tree:
    """One state of the volume, materialized under a directory."""

    def __init__(self, root):
        self.root = root
        self.mt = {}  # rel -> (sec, nsec), applied at the end
        self.special = []  # rels the guest writes itself (not in the tarballs)
        self.sparse = {}  # rel -> {"size": n, "extents": [(off, bytes)]}
        self.xattrs = {}  # rel -> {name: bytes}
        self.clones = []  # (src rel, dst rel)
        self.interleaved = []  # (rel a, rel b, blocks, tag a, tag b)
        os.makedirs(root)

    def p(self, rel):
        return os.path.join(self.root, rel)

    def put(self, rel, data, mode=0o644, mtime=None):
        p = self.p(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "wb") as f:
            f.write(data)
        os.chmod(p, mode)
        self.mt[rel] = mtime or mtime_of(rel)

    def mkdir(self, rel, mode=0o755):
        p = self.p(rel)
        os.makedirs(p, exist_ok=True)
        os.chmod(p, mode)
        self.mt[rel] = mtime_of(rel)

    def symlink(self, target, rel):
        p = self.p(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        os.symlink(target, p)
        self.mt[rel] = mtime_of(rel)

    def hardlink(self, src, rel):
        p = self.p(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        os.link(self.p(src), p)
        self.mt[rel] = self.mt[src]

    def put_sparse(self, rel, size, extents, mode=0o644):
        """A file with holes; the guest writes the extents with pwrite and
        sets the size with truncate, so the volume holds real holes."""
        p = self.p(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "wb") as f:
            for off, data in extents:
                f.seek(off)
                f.write(data)
            f.truncate(size)
        os.chmod(p, mode)
        self.mt[rel] = mtime_of(rel)
        self.special.append(rel)
        self.sparse[rel] = {"size": size, "extents": list(extents)}

    def clone(self, src, rel, mode=0o644):
        p = self.p(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        shutil.copyfile(self.p(src), p)
        os.chmod(p, mode)
        self.mt[rel] = mtime_of(rel)
        self.special.append(rel)
        self.clones.append((src, rel))

    def interleave(self, rel_a, rel_b, blocks):
        """Two files written one block at a time, alternately, each block
        flushed before the next (the guest appends and fsyncs), so the driver
        allocates their blocks alternately and neither file is contiguous."""
        for rel in (rel_a, rel_b):
            p = self.p(rel)
            os.makedirs(os.path.dirname(p), exist_ok=True)
            with open(p, "wb") as f:
                f.write(blob(rel, blocks * BS))
            os.chmod(p, 0o644)
            self.mt[rel] = mtime_of(rel)
            self.special.append(rel)
        self.interleaved.append((rel_a, rel_b, blocks))

    def xattr(self, rel, name, value):
        self.xattrs.setdefault(rel, {})[name] = value

    def chown(self, rel, uid, gid):
        os.lchown(self.p(rel), uid, gid)

    def finish(self):
        # deepest first (a child's creation changed its parent's time)
        for rel in sorted(self.mt, key=lambda q: (-q.count("/"), q)):
            sec, nsec = self.mt[rel]
            os.utime(self.p(rel), ns=(sec * 10**9 + nsec,) * 2, follow_symlinks=False)

    def walk(self):
        """Every path below the root, relative, sorted (a directory before its
        content)."""
        out = []
        for d, dirs, files in os.walk(self.root):
            for n in dirs + files:
                out.append(os.path.relpath(os.path.join(d, n), self.root).replace(os.sep, "/"))
        return sorted(out)


def build(root, stage):
    t = Tree(root)
    t.put("hello.txt", b"Hello, APFS\n")
    t.put("empty.txt", b"")
    t.put("secret.txt", b"mode 0600\n", 0o600)
    t.put("tool.sh", b"#!/bin/sh\necho hi\n", 0o755)
    t.put("with space.txt", b"a name with a space\n")
    t.put("MixedCase.TXT", b"case is kept, lookup folds it\n")
    t.mkdir("docs")
    t.put("docs/readme.txt", b"readme\n" * 40)
    t.mkdir("docs/private", 0o750)
    t.put("docs/private/notes.txt", b"private notes\n", 0o640)
    t.mkdir("docs/empty-dir")

    # Unicode names. Composed (NFC) names in "unicode/", the same kinds of
    # name decomposed (NFD) in "nfd/": the volume ignores the difference when
    # it looks a name up (the directory record key hashes the decomposed,
    # case-folded name) but keeps the bytes it was given.
    t.mkdir("unicode")
    for name, body in [
        ("café.txt", b"cafe with an acute accent\n"),
        ("über-Ångström.txt", b"umlauts and a ring\n"),
        ("документ.txt", b"cyrillic\n"),
        ("Ελληνικά.txt", b"greek\n"),
        ("한국어.txt", b"hangul syllables decompose into jamo\n"),
        ("Straße.txt", b"sharp s folds to ss\n"),
        ("İstanbul.txt", b"dotted capital i\n"),
        ("snow-☃.txt", b"a symbol (BMP)\n"),
        ("emoji-\U0001F600.txt", b"outside the BMP\n"),
        ("ǅ-title.txt", b"a titlecase digraph\n"),
    ]:
        t.put("unicode/" + name, body)
    t.mkdir("unicode/日本語")
    t.put("unicode/日本語/ファイル.txt", b"japanese names\n")
    t.mkdir("nfd")
    for name, body in [
        ("café.txt", b"nfd: e + combining acute\n"),
        ("über.txt", b"nfd: u + combining diaeresis\n"),
        ("한국어.txt", b"nfd: conjoining jamo\n"),
        ("Ångström.txt", b"nfd: a + combining ring\n"),
    ]:
        t.put("nfd/" + unicodedata.normalize("NFD", name), body)
    # Case variants live in different directories (the volume folds case).
    t.mkdir("case")
    t.put("case/UPPER.TXT", b"upper\n")
    t.put("case/lower.txt", b"lower\n")
    t.put("case/CamelCase.Txt", b"camel\n")
    # The longest name Linux allows (255 bytes) and the longest CJK one (85
    # characters of 3 UTF-8 bytes).
    t.mkdir("long")
    t.put("long/" + ("n" * 251) + ".txt", b"255 ascii characters\n")
    t.put("long/" + ("名" * 85), b"85 cjk characters\n")

    # Symlinks: relative, absolute, dangling, to a directory, a long target,
    # a non-ASCII name.
    t.symlink("hello.txt", "link-to-hello")
    t.symlink("/docs/readme.txt", "docs/absolute-link")
    t.symlink("no-such-target", "dangling-link")
    t.symlink("docs", "link-to-dir")
    t.symlink("/".join(["segment%02d" % i for i in range(20)]) + "/end", "long-target-link")
    t.symlink("café.txt", "unicode/ünï-link")

    # A group of three hard links to one file, one more file linked once.
    t.mkdir("hl")
    t.put("hl/a.txt", b"one inode, three names\n")
    t.hardlink("hl/a.txt", "hl/b.txt")
    t.hardlink("hl/a.txt", "docs/c-link.txt")
    t.put("hl/single.txt", b"not linked\n")

    # A 3 MiB file and a directory of 600 entries (a multi-level fs tree).
    t.mkdir("big")
    t.put("big/three_mib.bin", blob("three_mib", 3 << 20))
    t.mkdir("many")
    for i in range(600):
        t.put("many/entry-%04d.txt" % i, b"entry %d\n" % i)

    # Fragmentation: two 128-block files written alternately, block by block.
    t.mkdir("frag")
    t.interleave("frag/big.bin", "frag/other.bin", 128)

    # Sparse files: holes between, before and after the data, and no data at all.
    t.mkdir("sparse")
    t.put_sparse("sparse/holes.bin", 256 * BS + 17, [
        (0, blob("sparse/0", BS)),
        (64 * BS, blob("sparse/1", BS)),
        (255 * BS, blob("sparse/2", BS)),
        (256 * BS, b"seventeen bytes!!"),
    ])
    t.put_sparse("sparse/hole-first.bin", 40 * BS, [(30 * BS, blob("sparse/h", 5 * BS + 100))])
    t.put_sparse("sparse/all-hole.bin", 2 << 20, [])
    t.put_sparse("sparse/data-then-hole.bin", 64 * BS, [(0, blob("sparse/d", 3000))])

    # Extended attributes: a small one, an empty one, one too big to be stored
    # in the record (a data stream), on a file and on a directory. The driver
    # prefixes the names with "osx." on Linux; on the volume they are bare.
    t.mkdir("xattr")
    t.put("xattr/tagged.txt", b"has extended attributes\n")
    t.xattr("xattr/tagged.txt", "com.example.small", b"value")
    t.xattr("xattr/tagged.txt", "com.example.empty", b"")
    t.xattr("xattr/tagged.txt", "com.example.big", blob("xattr/big", 6000))
    t.xattr("xattr/tagged.txt", "com.example.binary", bytes(range(256)))
    t.xattr("xattr", "com.example.dir", b"a directory tag")

    # A clone: two files that share every extent.
    t.mkdir("clone")
    t.put("clone/orig.bin", blob("clone/orig", 24 * BS))
    t.clone("clone/orig.bin", "clone/copy.bin")

    # Modes and owners.
    t.mkdir("perm")
    t.put("perm/suid.bin", b"setuid\n", 0o4755)
    t.put("perm/owned.txt", b"owned by 1000:1001\n", 0o640)
    t.mkdir("perm/sticky", 0o1777)
    t.put("perm/readonly.txt", b"read only\n", 0o444)

    t.mkdir("deep")
    t.put("deep/a/b/c/d/e/f/g/leaf.txt", b"deep\n")
    for d in ["a", "a/b", "a/b/c", "a/b/c/d", "a/b/c/d/e", "a/b/c/d/e/f", "a/b/c/d/e/f/g"]:
        t.mt["deep/" + d] = mtime_of("deep/" + d)

    t.chown("perm/owned.txt", 1000, 1001)
    t.chown("docs/private", 1000, 1000)

    if stage == "B":
        # What changes after the snapshot: a deleted file, a rewritten file
        # (new data, so new extents), a new file in a new directory.
        os.unlink(t.p("many/entry-0001.txt"))
        del t.mt["many/entry-0001.txt"]
        os.unlink(t.p("hello.txt"))
        t.put("hello.txt", b"Hello again, APFS: rewritten after the snapshot\n" * 3,
              mtime=(EPOCH - 3600, 123456789))
        t.mkdir("after")
        t.put("after/new.txt", b"made after the snapshot\n")
    t.finish()
    return t


def hx(b):
    return binascii.hexlify(b).decode()


def attrs_ops(t, rels):
    """chmod/chown/utime operations (apfs_guest.c) for the paths rels of t:
    modes and owners first (chown clears setuid), times last, deepest first."""
    lines = []
    for rel in rels:
        st = os.lstat(t.p(rel))
        if (st.st_uid, st.st_gid) != (0, 0):
            lines.append("chown\t%s\t%d\t%d" % (rel, st.st_uid, st.st_gid))
        if not os.path.islink(t.p(rel)):
            lines.append("chmod\t%s\t%o" % (rel, st.st_mode & 0o7777))
    for rel in sorted(rels, key=lambda q: (-q.count("/"), q)):
        sec, nsec = t.mt[rel]
        lines.append("utime\t%s\t%d\t%d" % (rel, sec, nsec))
    return lines


def write_lines(path, lines):
    with open(path, "w") as f:
        f.write("".join(l + "\n" for l in lines))


def main():
    out = os.path.abspath(sys.argv[1])
    if os.path.lexists(out):
        shutil.rmtree(out)
    os.makedirs(out)
    a = build(os.path.join(out, "snap1"), "A")
    b = build(os.path.join(out, "final"), "B")

    special = set(a.special)

    def addto(tf, root, rel):
        ti = tf.gettarinfo(os.path.join(root, rel), arcname=rel)
        ti.uid = ti.gid = 0
        ti.uname = ti.gname = ""
        ti.mtime = 0  # modes, owners and times are set by the ops
        if ti.isreg():
            with open(os.path.join(root, rel), "rb") as f:
                tf.addfile(ti, f)
        else:
            tf.addfile(ti)

    # phase 1: the snap1 tree minus the specials, in path order
    with tarfile.open(os.path.join(out, "phase1.tar"), "w", format=tarfile.GNU_FORMAT) as tf:
        for rel in a.walk():
            if rel not in special:
                addto(tf, a.root, rel)

    # ops1: complete snap1, then take the snapshot
    ops1 = []
    for rel_a, rel_b, blocks in a.interleaved:
        for i in range(blocks):
            for rel in (rel_a, rel_b):
                data = blob(rel, blocks * BS)[i * BS:(i + 1) * BS]
                ops1.append("append\t%s\t%s" % (rel, hx(data)))
    for rel in sorted(a.sparse):
        s = a.sparse[rel]
        if not s["extents"]:
            ops1.append("pwrite\t%s\t0\t" % rel)  # creates the empty file
        for off, data in s["extents"]:
            ops1.append("pwrite\t%s\t%d\t%s" % (rel, off, hx(data)))
        ops1.append("truncate\t%s\t%d" % (rel, s["size"]))
    for src, dst in a.clones:
        ops1.append("clone\t%s\t%s\t%o" % (src, dst, os.lstat(a.p(dst)).st_mode & 0o7777))
    for rel in sorted(a.xattrs):
        for name in sorted(a.xattrs[rel]):
            ops1.append("xattr\t%s\tosx.%s\t%s" % (rel, name, hx(a.xattrs[rel][name])))
    ops1 += attrs_ops(a, a.walk())
    ops1.append("sync")  # the snapshot ioctl does not flush inode changes still in memory
    ops1.append("snap\t.\tsnap1")
    write_lines(os.path.join(out, "ops1"), ops1)

    # after the snapshot: delete, then add and rewrite
    write_lines(os.path.join(out, "ops2"), ["rm\tmany/entry-0001.txt", "rm\thello.txt"])
    changed = ["hello.txt", "after", "after/new.txt"]
    with tarfile.open(os.path.join(out, "phase3.tar"), "w", format=tarfile.GNU_FORMAT) as tf:
        for rel in changed:
            addto(tf, b.root, rel)
    # ops3: the changed files, then every directory again (their times moved)
    dirs = [r for r in b.walk() if os.path.isdir(b.p(r)) and not os.path.islink(b.p(r))]
    write_lines(os.path.join(out, "ops3"), attrs_ops(b, sorted(set(changed) | set(dirs))))

    spec = {
        "snapshot": "snap1",
        "xattrs": {"/" + r: {n: hx(v) for n, v in sorted(d.items())} for r, d in sorted(a.xattrs.items())},
        "clones": [["/" + s, "/" + d] for s, d in a.clones],
        "sparse": {"/" + r: s["size"] for r, s in sorted(a.sparse.items())},
        "interleaved": [["/" + x, "/" + y] for x, y, _ in a.interleaved],
        "mtimes_ns": {"/" + r: s * 10**9 + n for r, (s, n) in sorted(b.mt.items())},
        "snap_mtimes_ns": {"/" + r: s * 10**9 + n for r, (s, n) in sorted(a.mt.items())},
    }
    with open(os.path.join(out, "specials.json"), "w") as f:
        json.dump(spec, f, indent=1, sort_keys=True)


if __name__ == "__main__":
    main()

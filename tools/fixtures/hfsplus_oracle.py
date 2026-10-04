#!/usr/bin/env python3
"""Independent oracle for the HFS+ fixtures: a second, hand-written parse of
the on-disk structures (Apple TN1150) plus external tools. It never uses
Minutiae.

usage: hfsplus_oracle.py <image> --type hfsplus|hfsx --label NAME
           --block-size N --journaled 0|1 --volume-id 16HEX --create-unix SECS
           [--wrapper] [--fsck-issue LINE ...]

The --* arguments are what the generator asked mkfs.hfsplus for; the oracle
FAILS (exit 1, message on stderr) when the image disagrees, or when any of its
cross-checks fails:

  * volume header fields, both copies (primary at 1024, alternate 1024 bytes
    before the end), and the five special-file forks;
  * allocation bitmap: popcount = totalBlocks - freeBlocks, and the set of
    allocated blocks equals EXACTLY the blocks of the two headers plus every
    extent of the special files and of every catalog file fork;
  * catalog B-tree: header node (depth, root, leaf count, node size, key
    compare type, free nodes vs the node map), a top-down walk from the root
    and a walk of the leaf chain visit the same leaf nodes, record counts match
    the header, keys are strictly increasing, every file/folder has a matching
    thread record, parents exist and are folders, valence = number of
    children, fileCount/folderCount match, nextCatalogID is above every CNID;
  * extents overflow and attributes trees: header parse, leaf record counts;
  * journal (journaled images): the journal info block (flags: in-FS, needs-init),
    the .journal_info_block
    and .journal catalog files cover exactly the journal range, and the journal
    region is all zero (mkfs never writes the journal header);
  * fsck.hfsplus -n -f exits 0 and says the volume appears to be OK (HFSX: see
    --fsck-issue);
  * blkid -p -o export agrees on TYPE, LABEL and UUID (UUID also recomputed here
    from the volume id: MD5 of the Apple namespace + id, version-3 bits);
  * the image's sha256 is the same before and after the read-only tools ran.

Stated limitation: this parse is a second implementation by the same author as
the reader; its independence rests on fsck.hfsplus and blkid agreeing, not on
a different author.

Prints the expectation JSON (without the generator section) on stdout.
"""
import argparse
import hashlib
import json
import os
import re
import stat
import struct
import subprocess
import sys
import unicodedata

HFS_EPOCH_DELTA = 2082844800  # seconds from 1904-01-01 to 1970-01-01
APPLE_NS = bytes.fromhex("B3E20F39F29211D697A400306543ECAC")
checks = []
agg = None  # populated images: checks are aggregated (digits normalized), see main()


def die(msg):
    sys.stderr.write("hfsplus_oracle: " + msg + "\n")
    sys.exit(1)


def check(cond, name):
    if not cond:
        die("check failed: " + name)
    if agg is not None:
        key = re.sub(r"[0-9]+", "N", name)
        agg[key] = agg.get(key, 0) + 1
    else:
        checks.append(name)


def u16(b, o):
    return struct.unpack_from(">H", b, o)[0]


def u32(b, o):
    return struct.unpack_from(">I", b, o)[0]


def u64(b, o):
    return struct.unpack_from(">Q", b, o)[0]


def unix(hfs):
    """HFS+ date (seconds since 1904) -> unix seconds; 0 means absent."""
    return None if hfs == 0 else hfs - HFS_EPOCH_DELTA


class Fork:
    def __init__(self, b, o):
        self.logical = u64(b, o)
        self.clump = u32(b, o + 8)
        self.blocks = u32(b, o + 12)
        self.extents = []
        self.used_overflow = False
        for i in range(8):
            start, count = u32(b, o + 16 + 8 * i), u32(b, o + 20 + 8 * i)
            if count:
                self.extents.append((start, count))

    def check_inline(self, what):
        # every fixture fork fits its 8 inline extents (no overflow records)
        check(sum(c for _, c in self.extents) == self.blocks, what + ": inline extents cover all blocks")

    def json(self):
        return {"logical_size": self.logical, "total_blocks": self.blocks,
                "extents": [[s, c] for s, c in self.extents]}


def resolve_fork(fork, file_id, fork_type, overflow, what):
    """Append the overflow extents (from the extents-overflow tree) to the
    fork's 8 inline extents: records are keyed by the first block they map and
    must continue exactly where the previous extent ends."""
    have = sum(c for _, c in fork.extents)
    if len(fork.extents) < 8 and fork.blocks == have:
        check((file_id, fork_type) not in overflow, what + ": no overflow records for a fork that fits inline")
        return
    recs = sorted(overflow.get((file_id, fork_type), []))
    for start, exts in recs:
        check(start == have, what + ": overflow record starts at the block the previous extents end")
        fork.extents.extend(exts)
        have += sum(c for _, c in exts)
    check(have == fork.blocks, what + ": inline plus overflow extents cover every block of the fork")
    fork.used_overflow = bool(recs)


class Image:
    """The image, or the HFS+ volume embedded at byte `base` of a wrapper
    (offsets passed to read() are relative to the volume)."""

    def __init__(self, path, base=0, length=None):
        self.f = open(path, "rb")
        self.f.seek(0, 2)
        self.total = self.f.tell()
        self.base = base
        self.size = self.total - base if length is None else length

    def read(self, off, n):
        if off < 0 or off + n > self.size:
            die("read outside the volume at %d" % off)
        self.f.seek(self.base + off)
        b = self.f.read(n)
        if len(b) != n:
            die("short read at %d" % off)
        return b

    def fork_data(self, fork, bs, limit=None):
        out = bytearray()
        for s, c in fork.extents:
            out += self.read(s * bs, c * bs)
        n = fork.logical if limit is None else min(limit, fork.logical)
        return bytes(out[:n])


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def run_tool(args):
    p = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    return p.returncode, p.stdout


def blkid_uuid(vid):
    h = bytearray(hashlib.md5(APPLE_NS + vid).digest())
    h[6] = 0x30 | (h[6] & 0x0F)
    h[8] = 0x80 | (h[8] & 0x3F)
    x = bytes(h).hex()
    return "%s-%s-%s-%s-%s" % (x[0:8], x[8:12], x[12:16], x[16:20], x[20:32])


def parse_header(b):
    h = {
        "signature": b[0:2],
        "version": u16(b, 2),
        "vol_attributes": u32(b, 4),
        "last_mounted_version": b[8:12],
        "journal_info_block": u32(b, 12),
        "create": u32(b, 16),
        "modify": u32(b, 20),
        "backup": u32(b, 24),
        "checked": u32(b, 28),
        "file_count": u32(b, 32),
        "folder_count": u32(b, 36),
        "block_size": u32(b, 40),
        "total_blocks": u32(b, 44),
        "free_blocks": u32(b, 48),
        "next_allocation": u32(b, 52),
        "rsrc_clump": u32(b, 56),
        "data_clump": u32(b, 60),
        "next_catalog_id": u32(b, 64),
        "write_count": u32(b, 68),
        "encodings_bitmap": u64(b, 72),
        "finder_info": [u32(b, 80 + 4 * i) for i in range(8)],
        "allocation": Fork(b, 112),
        "extents": Fork(b, 192),
        "catalog": Fork(b, 272),
        "attributes": Fork(b, 352),
        "startup": Fork(b, 432),
    }
    return h


class BTree:
    """A B-tree file read from its fork: header node, nodes, records."""

    def __init__(self, data, name):
        self.name, self.data = name, data
        if not data:
            self.empty = True
            return
        self.empty = False
        self.node_size = u16(data, 32)  # header record at 14: nodeSize at +18
        check(self.node_size in (512, 1024, 2048, 4096, 8192, 16384, 32768), name + ": node size is a power of two")
        n = self.node_size
        kind = struct.unpack_from(">b", data, 8)[0]
        check(kind == 1, name + ": node 0 is a header node")
        h = 14
        self.depth = u16(data, h)
        self.root = u32(data, h + 2)
        self.leaf_records = u32(data, h + 6)
        self.first_leaf = u32(data, h + 10)
        self.last_leaf = u32(data, h + 14)
        check(u16(data, h + 18) == n, name + ": nodeSize")
        self.max_key_len = u16(data, h + 20)
        self.total_nodes = u32(data, h + 22)
        self.free_nodes = u32(data, h + 26)
        self.btree_type = data[h + 36]
        self.key_compare = data[h + 37]
        self.attributes = u32(data, h + 38)
        check(len(data) >= self.total_nodes * n, name + ": file holds totalNodes nodes")
        # node usage map: record 2 of the header node
        offs = self.offsets(0)
        mp = data[offs[2]:offs[3]]
        used = sum(bin(x).count("1") for x in mp[: (self.total_nodes + 7) // 8])
        # bits past totalNodes inside the last map byte must not be counted
        extra = (8 - self.total_nodes % 8) % 8
        if extra:
            last = mp[(self.total_nodes + 7) // 8 - 1]
            used -= bin(last & ((1 << extra) - 1)).count("1")
        check(used == self.total_nodes - self.free_nodes, name + ": node map popcount = totalNodes - freeNodes")

    def offsets(self, node):
        n = self.node_size
        base = node * n
        nrec = u16(self.data, base + 10)
        offs = [u16(self.data, base + n - 2 * (i + 1)) for i in range(nrec + 1)]
        for a, b in zip(offs, offs[1:]):
            check(14 <= a <= b <= n - 2 * (nrec + 1), self.name + ": record offsets ascending in node %d" % node)
        return offs

    def desc(self, node):
        b = self.node_size * node
        return (u32(self.data, b), u32(self.data, b + 4), struct.unpack_from(">b", self.data, b + 8)[0],
                self.data[b + 9], u16(self.data, b + 10))

    def records(self, node):
        base = self.node_size * node
        offs = self.offsets(node)
        for a, b in zip(offs, offs[1:]):
            yield self.data[base + a:base + b]

    def leaves_chain(self):
        out, seen, node, prev = [], set(), self.first_leaf, 0
        while node:
            check(node not in seen and node < self.total_nodes, self.name + ": leaf chain sane")
            seen.add(node)
            f, bl, kind, height, nrec = self.desc(node)
            check(kind == -1 and height == 1, self.name + ": chain node %d is a leaf" % node)
            check(bl == prev, self.name + ": leaf bLink")
            out.append(node)
            prev, node = node, f
        check(prev == self.last_leaf, self.name + ": last leaf")
        return out

    def leaves_walk(self):
        out = []

        def visit(node, height, depth):
            check(depth <= self.depth and node < self.total_nodes, self.name + ": walk depth")
            f, bl, kind, h, nrec = self.desc(node)
            check(h == height, self.name + ": node %d height" % node)
            if kind == -1:
                out.append(node)
                return
            check(kind == 0, self.name + ": index node kind")
            for rec in self.records(node):
                klen = u16(rec, 0)
                po = 2 + klen
                po += po & 1
                visit(u32(rec, po), height - 1, depth + 1)

        if self.root:
            visit(self.root, self.depth, 1)
        return out

    def all_leaf_records(self):
        chain = self.leaves_chain() if self.leaf_records or self.first_leaf else []
        walk = self.leaves_walk()
        check(chain == walk, self.name + ": top-down walk and leaf chain visit the same leaves")
        recs = []
        for node in chain:
            recs.extend(self.records(node))
        check(len(recs) == self.leaf_records, self.name + ": leafRecords = records walked")
        return recs


def utf16(rec, o):
    n = u16(rec, o)
    return rec[o + 2:o + 2 + 2 * n].decode("utf-16-be", "surrogatepass"), 2 + 2 * n


def apple_key_lt(a, b, binary):
    """Key order: parentID, then name by case-folded (HFS+) or binary (HFSX)
    UTF-16 code units. Only a coarse check: the fixtures' keys are ASCII."""
    pa, na = a
    pb, nb = b
    if pa != pb:
        return pa < pb
    if binary:
        return na.encode("utf-16-be") < nb.encode("utf-16-be")
    return na.lower().encode("utf-16-be") < nb.lower().encode("utf-16-be")


HLINK = (b"hlnk", b"hfs+")
S_IFMT = 0o170000


def nfd(s):
    return unicodedata.normalize("NFD", s)


def source_tree_entries(root):
    """What the volume must hold, from the source tree alone: path (NFD, as the
    Linux driver stores names) -> expectations."""
    out = {}
    for d, dirs, fs in os.walk(root):
        for name in dirs + fs:
            p = os.path.join(d, name)
            rel = "/" + nfd(os.path.relpath(p, root).replace(os.sep, "/"))
            st = os.lstat(p)
            e = {"mode": st.st_mode & 0o7777, "mtime": int(st.st_mtime), "src_ino": st.st_ino, "nlink": st.st_nlink}
            if stat.S_ISLNK(st.st_mode):
                e["type"], e["target"] = "symlink", os.readlink(p)
                e["size"] = len(e["target"].encode("utf-8"))
                e["sha256"] = hashlib.sha256(e["target"].encode("utf-8")).hexdigest()
            elif stat.S_ISDIR(st.st_mode):
                e["type"] = "dir"
            else:
                e["type"], e["size"] = "file", st.st_size
                e["sha256"] = sha256_file(p)
            out[rel] = e
    return out


def populated_view(src_root, img, bs, h, folders, files, entries, path_of, cat, recs):
    """Populated image: identify the hidden hard-link folder and the link
    records, rewrite the entries of the visible tree (links resolved to their
    inode record), and compare the whole visible tree with the source tree."""
    # --- the private folder: a root folder named 4 NULs (or the Linux driver's
    # U+2400 symbols) + "HFS+ Private Data"
    suffix = "HFS+ Private Data"
    priv = [c for c, f in folders.items() if f["parent"] == 2 and f["name"].endswith(suffix)
            and set(f["name"][: -len(suffix)]) <= {"\x00", "␀"} and len(f["name"]) == len(suffix) + 4]
    links = [f for f in files.values() if (f["ftype"], f["fcreator"]) == HLINK]
    check(len(priv) <= 1, "at most one private data folder")
    if links:
        check(len(priv) == 1, "hard-link records exist, so the private data folder exists")
    priv_id = priv[0] if priv else None
    inodes = {}
    if priv_id is not None:
        for c, f in files.items():
            if f["parent"] == priv_id:
                m = re.fullmatch(r"iNode(\d+)", f["name"])
                check(m is not None, "every file in the private folder is named iNode<number>")
                inodes[int(m.group(1))] = c
        for c, f in folders.items():
            if f["parent"] == priv_id:
                die("unexpected folder inside the private folder")
        check(folders[priv_id]["valence"] == len(inodes), "private folder valence = number of iNode files")
    refs = {}
    for f in links:
        n = f["special"]
        check(n in inodes, "hard-link record points at an existing iNode file")
        refs.setdefault(n, []).append(f["cnid"])
    check(set(inodes) == set(refs), "every iNode file is referenced by at least one link record")
    for n, c in inodes.items():
        ino = files[c]
        # the iNode record's own special field holds its link count on macOS;
        # recorded, compared with the number of link records
        ino["link_count_field"] = ino["special"]

    # --- the visible tree
    def hidden(c):
        r = folders.get(c) or files[c]
        return c == priv_id or (priv_id is not None and r["parent"] == priv_id)

    # Everything the catalog holds stays in the list (the reader shows the
    # private folder too, flagged); the hidden ones are marked and left out of
    # the source-tree comparison.
    out = entries
    for e in out:
        if e["cnid"] != 2 and hidden(e["cnid"]):
            e["private"] = True
    by_cnid = {e["cnid"]: e for e in entries}
    for e in out:
        if e["type"] != "file" or e.get("private"):
            continue
        f = files[e["cnid"]]
        fmode = f["mode"]
        if (f["ftype"], f["fcreator"]) == HLINK:
            ino = files[inodes[f["special"]]]
            ie = by_cnid[ino["cnid"]]
            for k in ("mode", "file_mode", "uid", "gid", "size", "sha256", "extents", "total_blocks", "rsrc_size", "rsrc_extents", "created", "modified", "changed", "accessed", "backup"):
                e[k] = ie[k]
            e["hardlink"] = True
            e["hardlink_inode"] = f["special"]
            e["inode_cnid"] = ino["cnid"]
            e["link_count"] = len(refs[f["special"]])
            fmode = ino["mode"]
        e["type"] = {0o120000: "symlink", 0o100000: "file"}.get(fmode & S_IFMT, "other")
        check(e["type"] != "other", "file record mode is a regular file or a symlink")

    # --- compare with the source tree
    want = source_tree_entries(src_root)
    got = {e["path"]: e for e in out if e["path"] != "/" and not e.get("private")}
    check(set(want) == set(got), "visible tree has exactly the source tree's paths (names NFD-normalized): missing %s, extra %s" % (sorted(set(want) - set(got))[:5], sorted(set(got) - set(want))[:5]))
    groups_src, groups_img = {}, {}
    for p, w in want.items():
        g = got[p]
        check(g["type"] == w["type"], "type of %s" % p)
        check(g["mode"] == w["mode"], "mode of %s (0o%o)" % (p, w["mode"]))
        if w["type"] != "dir":
            check(g["size"] == w["size"], "size of %s" % p)
            check(g["sha256"] == w["sha256"], "content sha256 of %s" % p)
        if w["type"] == "file":
            check(g["modified"] == w["mtime"], "contentModDate of %s = the source mtime" % p)
            if w["nlink"] > 1:
                groups_src.setdefault(w["src_ino"], set()).add(p)
        if g.get("hardlink"):
            groups_img.setdefault(g["inode_cnid"], set()).add(p)
        elif w["type"] != "dir":
            check(w["nlink"] == 1, "%s has no hard links in the source, and none in the image" % p)
    check(sorted(map(sorted, groups_src.values())) == sorted(map(sorted, groups_img.values())), "hard-link groups equal the source tree's")
    for ino_cnid, paths in groups_img.items():
        n = files[ino_cnid]["link_count_field"]
        check(n == len(paths), "iNode link count field = number of links (%d)" % len(paths))
    for p, w in want.items():
        if w["type"] == "symlink":
            got[p]["target"] = w["target"]

    # --- facts the fixture must hold to be worth having
    check(cat.depth >= 2, "catalog tree has an index level (depth >= 2)")
    nover = [p for p, e in got.items() if e["type"] != "dir" and len(e["extents"]) > 8]
    check(len(nover) >= 1, "a file needs more than 8 extents (extents-overflow records)")
    check(max(len(f["name"].encode("utf-16-be")) // 2 for f in files.values()) == 255, "a 255-unit name")
    check(len(groups_img) >= 1 and max(len(v) for v in groups_img.values()) >= 3, "a hard-link group of three")

    # --- where the private folder sorts among the root's children (leaf order)
    root_order = []
    for rec in recs:
        if u32(rec, 2) == 2:
            nm, _ = utf16(rec, 6)
            if nm:
                root_order.append(nm)
    private = None
    if priv_id is not None:
        pname = folders[priv_id]["name"]
        idx = root_order.index(pname)
        private = {
            "cnid": priv_id, "name": pname, "name_utf16_units": [ord(ch) for ch in pname],
            "root_leaf_index": idx, "root_children": len(root_order),
            "previous_sibling": root_order[idx - 1] if idx else None,
            "next_sibling": root_order[idx + 1] if idx + 1 < len(root_order) else None,
            "valence": folders[priv_id]["valence"],
            "inodes": [{"name": "iNode%d" % n, "cnid": c, "link_count_field": files[c]["link_count_field"],
                        "size": files[c]["data"].logical, "links": len(refs[n])} for n, c in sorted(inodes.items())],
        }
    extra = {
        "private_folder": private,
        "root_children_in_leaf_order": root_order,
        "fragmented_files": sorted(nover),
        "source_tree_compared": True,
    }
    out.sort(key=lambda e: e["path"].encode("utf-8"))
    return out, extra


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("image")
    ap.add_argument("--type", required=True, choices=["hfsplus", "hfsx"])
    ap.add_argument("--label", required=True)
    ap.add_argument("--block-size", type=int, required=True)
    ap.add_argument("--journaled", type=int, required=True)
    ap.add_argument("--volume-id", required=True)
    ap.add_argument("--create-unix", type=int, required=True)
    ap.add_argument("--wrapper", action="store_true", help="the HFS+ volume is embedded in an HFS wrapper")
    ap.add_argument("--fsck-issue", action="append", default=[])
    ap.add_argument("--source-tree", help="populated image: the directory the volume must hold (hfsplus_tree.py final/); hard-link groups, modes, sizes, hashes, symlink targets and file mtimes are compared; names are compared after NFD normalization, which the Linux driver applies")
    a = ap.parse_args()
    vid = bytes.fromhex(a.volume_id)

    sha_before = sha256_file(a.image)
    raw = Image(a.image)
    base, size, wrapper = 0, raw.size, None
    if a.wrapper:
        # classic HFS master directory block ("BD") at 1024 that embeds the volume
        mdb = raw.read(1024, 162)
        check(mdb[0:2] == b"BD", "wrapper: HFS master directory block signature")
        alblksiz, alblst = u32(mdb, 20), u16(mdb, 28)
        embed_sig, estart, ecount = u16(mdb, 124), u16(mdb, 126), u16(mdb, 128)
        check(embed_sig == 0x482B, "wrapper: embedded volume signature H+ (drEmbedSigWord)")
        base, size = alblst * 512 + estart * alblksiz, ecount * alblksiz
        check(0 < size and base + size <= raw.size, "wrapper: embedded volume lies inside the image")
        check(raw.read(raw.size - 1024, 162) == mdb, "wrapper: alternate MDB equals the primary")
        check(not any(raw.read(0, 1024)), "wrapper: boot blocks are zero")
        wrapper = {"signature": "BD", "alloc_block_size": alblksiz, "first_alloc_block_sector": alblst,
                   "embed_start_block": estart, "embed_block_count": ecount, "volume_offset": base,
                   "volume_size": size}
    img = Image(a.image, base, size)

    # ---- volume headers
    hb = img.read(1024, 512)
    ab = img.read(size - 1024, 512)
    h = parse_header(hb)
    sig = b"H+" if a.type == "hfsplus" else b"HX"
    check(h["signature"] == sig, "signature " + sig.decode())
    check(h["version"] == (4 if a.type == "hfsplus" else 5), "header version")
    bs = h["block_size"]
    check(bs == a.block_size, "block size as requested")
    check(h["total_blocks"] * bs <= size and size - h["total_blocks"] * bs < bs, "totalBlocks covers the volume")
    check(img.read(0, 1024) == bytes(1024), "boot blocks are zero")
    # the alternate header is a copy; only the journaling/mount counters could differ
    if a.source_tree is None:
        check(ab == hb, "alternate volume header equals the primary")
    else:
        # The Linux driver writes the backup header when it mounts (attributes:
        # inconsistent set, unmounted clear) and does not refresh it on unmount:
        # the attributes word and the modify date (bytes 20..23, which the
        # primary header gets at unmount; the guest clock may have moved a
        # second) may differ, nothing else.
        check(ab[:4] + ab[8:20] + ab[24:] == hb[:4] + hb[8:20] + hb[24:], "alternate volume header equals the primary except for the attributes word and the modify date")
        check(0 <= u32(hb, 20) - u32(ab, 20) <= 120, "primary modify date is at most 120 s after the backup header's")
        check(u32(ab, 20) >= u32(hb, 16) and u32(hb, 20) - u32(hb, 16) <= 600, "modify date within 600 s of the create date (the guest clock is frozen near it)")
    check(struct.pack(">II", h["finder_info"][6], h["finder_info"][7]) == vid, "volume id (finderInfo[6..7]) as written")
    check(unix(h["create"]) == a.create_unix, "createDate is the frozen clock")
    journaled = bool(h["vol_attributes"] & 0x2000)
    check(journaled == bool(a.journaled), "journaled attribute bit (0x2000)")
    check(h["vol_attributes"] & 0x100, "volume unmounted cleanly (0x100)")
    check(not h["vol_attributes"] & 0x800, "volume not inconsistent (0x800)")
    check((h["journal_info_block"] != 0) == journaled, "journalInfoBlock set iff journaled")
    populated = a.source_tree is not None
    if populated:
        global agg
        agg = {}
    overflow = {}  # (fileID, forkType) -> [(startBlock, [extents])] from the extents-overflow tree
    if not populated:
        for k in ("allocation", "extents", "catalog", "attributes", "startup"):
            h[k].check_inline("fork " + k)
            check(h[k].logical <= h[k].blocks * bs, "fork %s logicalSize fits its blocks" % k)
    else:
        # extents-overflow records first: the catalog file itself may need them
        h["extents"].check_inline("fork extents (the extents-overflow file has no overflow of its own)")
        ext0 = BTree(img.fork_data(h["extents"], bs), "extents overflow")
        if not ext0.empty:
            for rec in ext0.all_leaf_records():
                check(u16(rec, 0) == 10, "extents-overflow key is 10 bytes")
                ftype, fid, start = rec[2], u32(rec, 4), u32(rec, 8)
                exts = [(u32(rec, 12 + 8 * i), u32(rec, 16 + 8 * i)) for i in range(8)]
                exts = [(s, c) for s, c in exts if c]
                overflow.setdefault((fid, ftype), []).append((start, exts))
        for k, fid in (("allocation", 6), ("catalog", 4), ("attributes", 8), ("startup", 7)):
            resolve_fork(h[k], fid, 0, overflow, "fork " + k)
        for k in ("allocation", "extents", "catalog", "attributes", "startup"):
            check(h[k].logical <= h[k].blocks * bs, "fork %s logicalSize fits its blocks" % k)
    check(h["allocation"].logical >= (h["total_blocks"] + 7) // 8, "allocation file covers every block")

    # ---- allocation bitmap
    bm = img.fork_data(h["allocation"], bs)
    total = h["total_blocks"]

    def bit(i):
        return bm[i >> 3] >> (7 - (i & 7)) & 1

    used_bits = [i for i in range(total) if bit(i)]
    used_count = len(used_bits)
    check(used_count == total - h["free_blocks"], "bitmap popcount = totalBlocks - freeBlocks")

    # ---- catalog
    cat = BTree(img.fork_data(h["catalog"], bs), "catalog")
    check(not cat.empty and cat.btree_type == 0, "catalog B-tree type")
    check(cat.key_compare == (0xCF if a.type == "hfsplus" else 0xBC), "catalog key compare type")
    check(cat.attributes & 6 == 6, "catalog uses big keys and variable index keys (0x2|0x4)")
    recs = cat.all_leaf_records()

    folders, files, threads = {}, {}, {}
    keys = []
    for rec in recs:
        klen = u16(rec, 0)
        parent = u32(rec, 2)
        name, nlen = utf16(rec, 6)
        check(klen == 4 + nlen, "key length = parentID + name field")
        keys.append((parent, name))
        o = 2 + klen
        o += o & 1
        t = u16(rec, o)
        body = rec[o:]
        if t == 1:
            check(len(body) == 88, "folder record is 88 bytes")
            cnid = u32(body, 8)
            folders[cnid] = {
                "cnid": cnid, "parent": parent, "name": name, "valence": u32(body, 4),
                "create": u32(body, 12), "content_mod": u32(body, 16), "attr_mod": u32(body, 20),
                "access": u32(body, 24), "backup": u32(body, 28),
                "uid": u32(body, 32), "gid": u32(body, 36), "mode": u16(body, 42),
            }
        elif t == 2:
            check(len(body) == 248, "file record is 248 bytes")
            cnid = u32(body, 8)
            files[cnid] = {
                "cnid": cnid, "parent": parent, "name": name,
                "create": u32(body, 12), "content_mod": u32(body, 16), "attr_mod": u32(body, 20),
                "access": u32(body, 24), "backup": u32(body, 28),
                "uid": u32(body, 32), "gid": u32(body, 36), "mode": u16(body, 42),
                "data": Fork(body, 88), "rsrc": Fork(body, 168),
                "special": u32(body, 44), "ftype": body[48:52], "fcreator": body[52:56],
            }
            if not populated:
                files[cnid]["data"].check_inline("file data fork")
                files[cnid]["rsrc"].check_inline("file resource fork")
        elif t in (3, 4):
            check(klen == 6, "thread key has an empty name")
            tp = u32(body, 4)
            tname, _ = utf16(body, 8)
            threads[parent] = (t, tp, tname)
        else:
            die("unknown catalog record type %d" % t)
    binary = a.type == "hfsx"
    if not populated:
        for x, y in zip(keys, keys[1:]):
            if not apple_key_lt(x, y, binary):
                die("catalog keys not strictly increasing near %r %r" % (x, y))
        checks.append("catalog keys strictly increasing")
    else:
        # Apple's full name folding is not reimplemented here: parent ids must be
        # non-decreasing and adjacent all-ASCII names strictly increasing; the
        # complete ordering (non-ASCII names, the private folder) is vouched for
        # by fsck.hfsplus below, which walks the tree with Apple's own comparison.
        for x, y in zip(keys, keys[1:]):
            check(x[0] <= y[0], "catalog parent ids never decrease")
            if x[0] == y[0] and x[1].isascii() and y[1].isascii():
                check(apple_key_lt(x, y, binary), "adjacent ASCII keys strictly increase: %r %r" % (x[1], y[1]))

    check(2 in folders and folders[2]["parent"] == 1, "root folder (CNID 2) with parent 1")
    allc = set(folders) | set(files)
    check(not (set(folders) & set(files)), "CNIDs unique")
    check(set(threads) == allc, "every file and folder has exactly one thread record, and nothing else has")
    for c in allc:
        rec = folders.get(c) or files[c]
        t, tp, tn = threads[c]
        check(t == (3 if c in folders else 4), "thread type matches record type for CNID %d" % c)
        check(tp == rec["parent"] and tn == rec["name"], "thread points back at the record for CNID %d" % c)
        if c != 2:
            check(rec["parent"] in folders, "parent of CNID %d is an existing folder" % c)
    children = {c: 0 for c in folders}
    for c in allc:
        if c != 2:
            children[(folders.get(c) or files[c])["parent"]] += 1
    for c, f in folders.items():
        check(f["valence"] == children[c], "valence of folder %d = number of children" % c)
    check(h["file_count"] == len(files), "fileCount = file records")
    check(h["folder_count"] == len(folders) - 1, "folderCount = folder records, root not counted")
    check(h["next_catalog_id"] > max(allc) and h["next_catalog_id"] >= 16, "nextCatalogID above every CNID")

    # ---- other trees
    ext = BTree(img.fork_data(h["extents"], bs), "extents overflow")
    ext_records = len(ext.all_leaf_records()) if not ext.empty else 0
    if not populated:
        check(ext_records == 0, "extents overflow file has no records (no fork needs overflow extents)")
    else:
        check(ext_records == sum(len(v) for v in overflow.values()), "extents-overflow leaf records = records read")
        for c, f in files.items():
            resolve_fork(f["data"], c, 0x00, overflow, "data fork of CNID %d" % c)
            resolve_fork(f["rsrc"], c, 0xFF, overflow, "resource fork of CNID %d" % c)
        resolved = {(c, t) for c, f in files.items() for t, fk in ((0x00, f["data"]), (0xFF, f["rsrc"])) if getattr(fk, "used_overflow", False)}
        resolved |= {(fid, 0) for fid, fk in ((6, h["allocation"]), (4, h["catalog"]), (8, h["attributes"]), (7, h["startup"])) if getattr(fk, "used_overflow", False)}
        check(resolved == set(overflow), "every extents-overflow record belongs to a fork that needs it (none orphaned)")
    attr = BTree(img.fork_data(h["attributes"], bs), "attributes")
    attr_records = len(attr.all_leaf_records()) if not attr.empty else 0

    # ---- expected allocated set
    exp = set()

    def blocks_of(byte_start, byte_end):
        return range(byte_start // bs, (byte_end - 1) // bs + 1)

    exp.update(blocks_of(0, 1536))  # boot blocks and the volume header
    exp.update(blocks_of(size - 1024, size))  # alternate header and the trailing sector
    for k in ("allocation", "extents", "catalog", "attributes", "startup"):
        for s, c in h[k].extents:
            exp.update(range(s, s + c))
    for f in files.values():
        for k in ("data", "rsrc"):
            for s, c in f[k].extents:
                exp.update(range(s, s + c))
    check(exp == set(used_bits), "allocated blocks = headers + special files + catalog file forks, exactly")
    # no block is claimed twice
    claimed = []
    for k in ("allocation", "extents", "catalog", "attributes", "startup"):
        claimed += [x for s, c in h[k].extents for x in range(s, s + c)]
    for f in files.values():
        claimed += [x for kk in ("data", "rsrc") for s, c in f[kk].extents for x in range(s, s + c)]
    check(len(claimed) == len(set(claimed)), "no block belongs to two forks")

    free = []
    for i in range(total):
        if not bit(i):
            if free and free[-1][1] == i - 1:
                free[-1][1] = i
            else:
                free.append([i, i])
    free_count = sum(e - s + 1 for s, e in free)
    check(free_count == h["free_blocks"], "free ranges sum to freeBlocks")

    # ---- paths and content
    def path_of(c):
        parts = []
        while c != 2:
            r = folders.get(c) or files[c]
            parts.append(r["name"])
            c = r["parent"]
        return "/" + "/".join(reversed(parts))

    entries = []
    for c in sorted(allc):
        isdir = c in folders
        r = folders[c] if isdir else files[c]
        e = {"path": "/" if c == 2 else path_of(c), "type": "dir" if isdir else "file", "cnid": c,
             "parent_cnid": r["parent"], "name": r["name"] if c != 2 else "",
             "mode": r["mode"] & 0o7777, "file_mode": r["mode"], "uid": r["uid"], "gid": r["gid"],
             "created": unix(r["create"]), "modified": unix(r["content_mod"]),
             "changed": unix(r["attr_mod"]), "accessed": unix(r["access"]), "backup": unix(r["backup"])}
        if isdir:
            e["size"], e["sha256"] = 0, ""
            e["valence"] = r["valence"]
            if c == 2:
                e["volume_name"] = r["name"]
        else:
            d = img.fork_data(r["data"], bs)
            e["size"], e["sha256"] = r["data"].logical, hashlib.sha256(d).hexdigest()
            e["extents"] = [[s, n] for s, n in r["data"].extents]
            e["total_blocks"] = r["data"].blocks
            e["rsrc_size"] = r["rsrc"].logical
            e["rsrc_extents"] = [[s, n] for s, n in r["rsrc"].extents]
        entries.append(e)
    check(folders[2]["name"] == a.label, "root folder (volume) name = label")
    entries.sort(key=lambda e: e["path"].encode("utf-8"))

    extra = {}
    if populated:
        entries, extra = populated_view(a.source_tree, img, bs, h, folders, files, entries, path_of, cat, recs)

    # ---- journal
    journal = None
    if journaled:
        jb = img.read(h["journal_info_block"] * bs, 180)
        jflags = u32(jb, 0)
        joff, jsize = u64(jb, 36), u64(jb, 44)
        by_name = {f["name"]: f for f in files.values() if f["parent"] == 2}
        check(".journal_info_block" in by_name and ".journal" in by_name, "journal files are in the root")
        jib, jf = by_name[".journal_info_block"], by_name[".journal"]
        check([e for e in jib["data"].extents] == [(h["journal_info_block"], 1)], ".journal_info_block is the one block at journalInfoBlock")
        check(jflags & 1, "journal info: journal lives in the file system (kJIJournalInFSMask)")
        check(len(jf["data"].extents) == 1, ".journal is one extent")
        js, jc = jf["data"].extents[0]
        check(joff == js * bs and jsize == jf["data"].logical == jc * bs, "journal info offset/size = the .journal file")
        # mkfs.hfsplus never writes a journal header: it sets kJIJournalNeedInitMask
        # (0x4) and leaves the whole journal zero; the first mount creates the header.
        check(jflags == 5, "journal info flags = in-FS | need-init (0x1|0x4)")
        check(not any(img.read(joff, jsize)), "journal region is all zero (initialized on first mount)")
        journal = {"info_flags": jflags, "offset": joff, "size": jsize, "needs_init": True, "region_all_zero": True}

    # ---- external tools (read-only), then the hash must not have moved
    rc_fsck, out = run_tool(["fsck.hfsplus", "-n", "-f", a.image])
    rc = rc_fsck
    # every indented line other than the banner is a problem report
    issues = [ln.strip() for ln in out.splitlines()
              if ln.startswith("   ") and "Executing fsck_hfs" not in ln and "The volume name is" not in ln]
    if a.fsck_issue:
        # a known formatter quirk (see README): fsck must report exactly these
        # lines and nothing else, and every other phase must still complete
        check(rc != 0 and issues == a.fsck_issue, "fsck.hfsplus -n -f reports exactly the known formatter quirk")
        check("** Checking volume information." in out and "was found corrupt and needs to be repaired" in out,
              "fsck.hfsplus completed all phases")
    else:
        check(rc == 0 and not issues and "appears to be OK" in out, "fsck.hfsplus -n -f: volume appears to be OK (exit 0)")
    check(("Journaled" in out) == journaled, "fsck.hfsplus reports journaled iff journaled")
    check(("case-sensitive" in out) == (a.type == "hfsx"), "fsck.hfsplus detects a case-sensitive volume iff HFSX")
    rc, out = run_tool(["blkid", "-p", "-o", "export", a.image])
    check(rc == 0, "blkid -p succeeds")
    kv = dict(line.split("=", 1) for line in out.splitlines() if "=" in line)
    check(kv.get("TYPE") == "hfsplus", "blkid TYPE=hfsplus")
    check(kv.get("LABEL") == a.label, "blkid LABEL = label")
    uuid = blkid_uuid(vid)
    check(kv.get("UUID") == uuid, "blkid UUID = MD5(Apple namespace + volume id), version 3")
    check(sha256_file(a.image) == sha_before, "image unchanged by the read-only tools")

    def trees(t, n):
        d = {"node_size": t.node_size, "depth": t.depth, "root_node": t.root, "leaf_records": t.leaf_records,
             "total_nodes": t.total_nodes, "free_nodes": t.free_nodes, "max_key_length": t.max_key_len,
             "key_compare_type": t.key_compare}
        d["records_walked"] = n
        return d

    result = {
        "type": a.type,
        "label": a.label,
        "uuid": uuid,
        "volume_id": a.volume_id,
        "block_size": bs,
        "size": size,
        "image_size": raw.total,
        "volume_offset": base,
        "wrapper": wrapper,
        "total_blocks": total,
        "case_sensitive": a.type == "hfsx",
        "journaled": journaled,
        "attributes": h["vol_attributes"],
        "version": h["version"],
        "create_date": unix(h["create"]),
        "file_count": h["file_count"],
        "folder_count": h["folder_count"],
        "next_catalog_id": h["next_catalog_id"],
        "journal_info_block": h["journal_info_block"],
        "system_files": {k: h[k].json() for k in ("allocation", "extents", "catalog", "attributes", "startup")},
        "catalog": trees(cat, len(recs)),
        "extents_tree": {"leaf_records": ext_records} if ext.empty else trees(ext, ext_records),
        "attributes_tree": {"leaf_records": attr_records} if attr.empty else trees(attr, attr_records),
        "free_count": free_count,
        "free_blocks": free,
        "populated": populated,
        "files": entries,
        **extra,
        "journal": journal,
        "external_tools": {"fsck": {"exit": rc_fsck, "known_issues": issues}, "blkid": {"TYPE": kv["TYPE"], "LABEL": kv["LABEL"], "UUID": kv["UUID"]}},
        "checks": checks if agg is None else ["%s [x%d]" % kv for kv in agg.items()],
    }
    json.dump(result, sys.stdout, ensure_ascii=False, indent=1)
    sys.stdout.write("\n")


main()

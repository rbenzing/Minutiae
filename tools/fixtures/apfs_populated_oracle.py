#!/usr/bin/env python3
"""Independent oracle for the POPULATED APFS fixture (apfs-populated).

usage: apfs_populated_oracle.py <image> <source tree dir> <label> <cont_uuid> <vol_uuid>

<source tree dir> is the directory apfs_tree.py wrote (snap1/, final/,
specials.json). Prints the oracle JSON on stdout; exits non-zero (message on
stderr) when a cross-check fails, which fails the generator.

It extends apfs_oracle.py (the decoder written for the empty mkapfs images,
from Apple's APFS reference, sharing no code with Minutiae) to a volume with
history: several checkpoints, a snapshot, multi-level B-trees, data. What it
decodes from the image alone: the newest checkpoint, the container and volume
object maps (with their snapshot trees), the volume superblock, the snapshot
metadata tree, every file-system record of the live tree and of the snapshot
tree (inodes, directory records and their name hashes, extended attributes,
file extents, sibling links), the physical extent tree, and the space manager
with its bitmaps. What it compares with the source tree (never with Minutiae):
every path, type, size, SHA-256 of the content (read through the decoded
extents), mode, owner, mtime to the nanosecond, symlink target, hard-link
groups, extended-attribute names and values, clone pairs and sparse files,
for the live tree and for the snapshot. Its own consistency checks: every
object checksum, every directory-record hash, the free space as the zero bits
of the bitmaps against the blocks that all reachable structures and files
account for.
"""
import hashlib
import json
import os
import struct
import sys
import unicodedata

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import apfs_oracle as base  # noqa: E402
from apfs_oracle import (  # noqa: E402
    APSB, CAB, CAB_END, CHUNK_INFO, CHUNK_INFO_SIZE, CIB, CIB_END, CPM, CPM_END, CPMAP,
    CPMAP_SIZE, NXSB, OMAP, SM, OracleError, Image, Tree, decode, fail, header, hdr_summary,
    ranges_from_bits, uuid_str,
)

BS = 4096
S_IFMT, S_IFDIR, S_IFREG, S_IFLNK = 0o170000, 0o040000, 0o100000, 0o120000
SYMLINK_XATTR = "com.apple.fs.symlink"


def u64(b, off=0):
    return struct.unpack_from("<Q", b, off)[0]


# --- object maps ---------------------------------------------------------------
def omap_decode(img, blk, what):
    """The object map at physical block blk: its header, entries and snapshot
    records."""
    b = img.obj(blk, 1, what + " omap")
    h = header(b)
    if h["type"] != 0xB:
        fail("%s: block %d is not an omap" % (what, blk))
    om = decode(b, OMAP)
    if om["om_tree_type"] & 0xFFFF != 2 or om["om_tree_type"] & 0xC0000000 != base.OBJ_PHYSICAL:
        fail("%s: omap tree is not a physical B-tree (%#x)" % (what, om["om_tree_type"]))
    tree = Tree(img, om["om_tree_oid"], 0xB, lambda o: o, what + " omap tree", allow_ghosts=True)
    entries = []
    for key, val in tree.records:
        oid, xid = struct.unpack("<QQ", key)
        flags, size, paddr = struct.unpack("<IIQ", val)
        entries.append({"oid": oid, "xid": xid, "flags": flags, "size": size, "paddr": paddr})
    snaps = []
    if om["om_snapshot_tree_oid"]:
        st = Tree(img, om["om_snapshot_tree_oid"], 0x13, lambda o: o, what + " omap snapshot tree",
                  allow_ghosts=True)
        for key, val in st.records:
            snaps.append({"xid": u64(key), "flags": struct.unpack_from("<I", val, 0)[0]})
    return {"block": blk, "om": om, "entries": entries, "snapshots": snaps,
            "header": hdr_summary(h)}


def omap_find(entries, oid, xid):
    """Newest entry for oid with entry xid <= xid (None when absent or deleted)."""
    best = None
    for e in entries:
        if e["oid"] == oid and e["xid"] <= xid and (best is None or e["xid"] > best["xid"]):
            best = e
    if best is not None and best["flags"] & 1:  # OMAP_VAL_DELETED
        return None
    return best


# --- the container -------------------------------------------------------------
def read_container(img, checks):
    b0 = img.read(0)
    sb0 = decode(b0, NXSB)
    if sb0["nx_magic"] != 0x4253584E or sb0["nx_block_size"] != BS:
        fail("block 0 is not a 4 KiB NXSB")
    desc_base, desc_blocks = sb0["nx_xp_desc_base"], sb0["nx_xp_desc_blocks"]
    data_base, data_blocks = sb0["nx_xp_data_base"], sb0["nx_xp_data_blocks"]
    if desc_blocks & 0x80000000 or data_blocks & 0x80000000:
        fail("non-contiguous checkpoint areas are not supported")
    cands, maps, ring = [], [], []
    for i in range(desc_blocks):
        blk = desc_base + i
        b = img.read(blk)
        if b.count(0) == len(b):
            ring.append({"index": i, "block": blk, "kind": "zero"})
            continue
        b = img.obj(blk, 1, "checkpoint descriptor %d" % i)
        h = header(b)
        ring.append({"index": i, "block": blk, "kind": {1: "superblock", 0xC: "map"}.get(h["type"], "other"),
                     "xid": h["o_xid"]})
        if h["type"] == 1:
            cands.append((h["o_xid"], i, decode(b, NXSB), b))
        elif h["type"] == 0xC:
            m = decode(b, CPM)
            mp = [decode(b, CPMAP, CPM_END + j * CPMAP_SIZE) for j in range(m["cpm_count"])]
            maps.append((h["o_xid"], mp))
    if not cands:
        fail("no checkpoint superblock")
    xid, idx, sb, raw = max(cands, key=lambda c: c[0])
    checks.append("newest checkpoint superblock xid %d (ring index %d) of %d" % (xid, idx, len(cands)))
    if raw != b0:
        fail("block 0 is not a copy of the newest checkpoint superblock")
    checks.append("block 0 equals the newest checkpoint superblock")
    ephemeral = {}
    for mxid, mp in maps:
        if mxid == xid:
            for e in mp:
                ephemeral[e["cpm_oid"]] = e
    return {"ring": ring, "sb": sb, "xid": xid, "ring_index": idx, "ephemeral": ephemeral,
            "desc": (desc_base, desc_blocks), "data": (data_base, data_blocks),
            "checkpoints": len(cands)}


# --- file-system records ---------------------------------------------------------
class Volume:
    """The decoded records of one fs tree (live or a snapshot)."""

    def __init__(self, img, tree_records, ci, hashed, what):
        self.what = what
        self.inodes = {}
        self.drecs = []
        self.xattrs = {}    # id -> {name: record}
        self.extents = {}   # id -> [(logical, length, phys, crypto)]
        self.sibling_links = []
        self.sibling_maps = {}
        self.dstream_ids = {}
        self.dir_stats = {}
        self.counts = {}
        for key, val in tree_records:
            rec = base.decode_fs_record(key, val, ci, hashed)
            ino, typ = rec["id"], rec["type"]
            self.counts[rec["type_name"]] = self.counts.get(rec["type_name"], 0) + 1
            if typ == 3:
                self.inodes[ino] = rec["inode"]
            elif typ == 9:
                self.drecs.append(rec["drec"])
            elif typ == 4:
                x = rec["xattr"]
                x["val"] = val
                self.xattrs.setdefault(ino, {})[x["name"]] = x
            elif typ == 8:
                if len(key) != 16:
                    fail("%s: file extent key of %d bytes" % (what, len(key)))
                logical = u64(key, 8)
                lf, phys, crypto = struct.unpack("<QQQ", val)
                if lf >> 56:
                    fail("%s: file extent flags %#x (unexpected)" % (what, lf >> 56))
                self.extents.setdefault(ino, []).append((logical, lf & 0x00FFFFFFFFFFFFFF, phys, crypto))
            elif typ == 5:
                sib = u64(key, 8)
                parent, nlen = struct.unpack_from("<QH", val, 0)
                name = val[10:10 + nlen]
                if len(name) != nlen or not name.endswith(b"\0"):
                    fail("%s: sibling link name" % what)
                self.sibling_links.append({"id": ino, "sibling_id": sib, "parent_id": parent,
                                           "name": name[:-1].decode("utf-8")})
            elif typ == 12:
                self.sibling_maps[ino] = u64(val)
            elif typ == 6:
                self.dstream_ids[ino] = struct.unpack_from("<I", val, 0)[0]
            elif typ == 10:
                self.dir_stats[ino] = list(struct.unpack_from("<QQQQ", val, 0))
            else:
                fail("%s: record type %d (%s) not expected in this fixture" % (what, typ, rec["type_name"]))
        self.children = {}
        for d in self.drecs:
            self.children.setdefault(d["parent_id"], []).append(d)


def inode_dstream(ino):
    for x in ino["xfields"]:
        if x["type"] == 8:
            return x["dstream"]
    return None


def inode_xfield(ino, t):
    for x in ino["xfields"]:
        if x["type"] == t:
            return x
    return None


def extent_runs(vol, owner_id, size, what):
    """Runs of the stream owner_id of the given size from its file extents:
    [offset, length] byte runs in file order, offset -1 for a hole. Raw: one
    run per extent (adjacent ones are not merged)."""
    ext = sorted(vol.extents.get(owner_id, []))
    runs = []
    pos = 0
    for logical, length, phys, _crypto in ext:
        if logical % BS or length % BS or length == 0:
            fail("%s: stream %d: extent %d+%d is not block aligned" % (what, owner_id, logical, length))
        if logical < pos:
            fail("%s: stream %d: overlapping extents" % (what, owner_id))
        if logical >= size:
            fail("%s: stream %d: extent at %d is past the size %d" % (what, owner_id, logical, size))
        if logical > pos:
            runs.append([-1, logical - pos])
        n = min(length, size - logical)
        runs.append([-1 if phys == 0 else phys * BS, n])
        pos = logical + n
        if n < length:
            pos = size
    if pos < size:
        runs.append([-1, size - pos])
    return runs


def read_runs(img, runs):
    out = bytearray()
    for off, n in runs:
        if off < 0:
            out += bytes(n)
        else:
            if off % BS:
                fail("a run starts inside a block")
            first = off // BS
            blocks = -(-n // BS)
            out += img.read(first, blocks)[:n]
    return bytes(out)


def xattr_value(img, vol, rec, what):
    """The value of an xattr record; for a stream the data is read through the
    stream's own extents (records keyed by the stream's object id)."""
    flags = rec["flags"]
    val = rec["val"]
    xlen = rec["xdata_len"]
    data = val[4:4 + xlen]
    if len(data) != xlen:
        fail("%s: xattr %s value does not fit" % (what, rec["name"]))
    if flags & 1:    # XATTR_DATA_STREAM
        if flags & 2:
            fail("%s: xattr %s is both stream and embedded" % (what, rec["name"]))
        oid = u64(data, 0)
        ds = base.decode(data, base.J_DSTREAM, 8)
        runs = extent_runs(vol, oid, ds["size"], what + " xattr stream")
        return read_runs(img, runs), {"stream_oid": oid, "stream_size": ds["size"], "runs": runs}
    if not flags & 2:   # XATTR_DATA_EMBEDDED
        fail("%s: xattr %s has neither storage flag" % (what, rec["name"]))
    return bytes(data), {"embedded": True}


def build_tree(img, vol, what):
    """The path tree of vol, from the root directory (inode 2) down, from the
    image alone."""
    out = {}
    groups = {}

    def kind(mode):
        t = mode & S_IFMT
        return {S_IFDIR: "dir", S_IFREG: "file", S_IFLNK: "symlink"}.get(t, "other:%o" % t)

    def visit(dir_id, prefix):
        seen = set()
        for d in vol.children.get(dir_id, []):
            name = d["name"]
            if "/" in name or name in seen:
                fail("%s: odd or duplicate name %r under inode %d" % (what, name, dir_id))
            seen.add(name)
            path = prefix + "/" + name
            ino = vol.inodes.get(d["file_id"])
            if ino is None:
                fail("%s: %s: directory record without an inode (%d)" % (what, path, d["file_id"]))
            node = {"path": path, "inode": d["file_id"], "type": kind(ino["mode"]),
                    "mode": ino["mode"] & 0o7777, "uid": ino["owner"], "gid": ino["group"],
                    "mtime_ns": ino["mod_time"], "nlink": ino["nchildren_or_nlink"],
                    "drec_type": d["dirent_type"], "private_id": ino["private_id"],
                    "internal_flags": ino["internal_flags"], "parent_id": ino["parent_id"]}
            want_dt = {"dir": "dir", "file": "reg", "symlink": "lnk"}.get(node["type"])
            if want_dt != d["dirent_type"]:
                fail("%s: %s: directory record type %s, inode mode says %s" % (what, path, d["dirent_type"], node["type"]))
            xs = vol.xattrs.get(d["file_id"], {})
            node["xattrs"] = sorted(xs)
            node["xattr_values"] = {}
            node["xattr_storage"] = {}
            for n, rec in xs.items():
                v, info = xattr_value(img, vol, rec, what + " " + path)
                node["xattr_values"][n] = v
                node["xattr_storage"][n] = info
            if node["type"] == "symlink":
                t = node["xattr_values"].get(SYMLINK_XATTR)
                if t is None or not t.endswith(b"\0"):
                    fail("%s: %s: symlink without a NUL-terminated target xattr" % (what, path))
                node["link"] = t[:-1].decode("utf-8")
                node["size"] = len(t) - 1
                node["sha256"] = None
            elif node["type"] == "file":
                ds = inode_dstream(ino)
                size = ds["size"] if ds else 0
                node["size"] = size
                runs = extent_runs(vol, ino["private_id"], size, what + " " + path) if ds else []
                node["runs"] = runs
                node["alloced_size"] = ds["alloced_size"] if ds else 0
                data = read_runs(img, runs)
                if len(data) != size:
                    fail("%s: %s: runs cover %d bytes of %d" % (what, path, len(data), size))
                node["sha256"] = hashlib.sha256(data).hexdigest()
                sp = inode_xfield(ino, 13)
                node["sparse_bytes"] = struct.unpack("<Q", bytes.fromhex(sp["hex"]))[0] if sp else None
            else:
                node["size"] = 0
            node["name_hash"] = d.get("name_hash")
            node["hash_matches"] = d.get("hash_matches")
            node["date_added"] = d["date_added"]
            if node["nlink"] > 1 and node["type"] == "file":
                groups.setdefault(d["file_id"], []).append(path)
            out[path] = node
            if node["type"] == "dir":
                visit(d["file_id"], path)

    visit(2, "")
    return out, groups


# --- the source tree -------------------------------------------------------------
def sha256_file(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def source_tree(root, spec):
    """path -> expected node, from a directory tree."""
    out, inos = {}, {}
    for d, dirs, files in os.walk(root):
        for n in dirs + files:
            p = os.path.join(d, n)
            rel = "/" + os.path.relpath(p, root).replace(os.sep, "/")
            st = os.lstat(p)
            m = st.st_mode
            node = {"mode": m & 0o7777, "uid": st.st_uid, "gid": st.st_gid,
                    "mtime_ns": st.st_mtime_ns, "nlink": st.st_nlink}
            if os.path.islink(p):
                t = os.readlink(p)
                node.update(type="symlink", link=t, size=len(t.encode("utf-8")), sha256=None)
            elif os.path.isdir(p):
                node.update(type="dir", size=0)
            else:
                node.update(type="file", size=st.st_size, sha256=sha256_file(p))
                if st.st_nlink > 1:
                    inos.setdefault((st.st_dev, st.st_ino), []).append(rel)
            x = spec["xattrs"].get(rel, {})
            node["xattrs"] = {k: bytes.fromhex(v) for k, v in x.items()}
            out[rel] = node
    return out, sorted(sorted(g) for g in inos.values())


def compare_trees(what, got, groups, want, want_groups, spec, checks):
    """Fail unless the decoded tree equals the source tree exactly."""
    if sorted(got) != sorted(want):
        only_got = sorted(set(got) - set(want))[:10]
        only_want = sorted(set(want) - set(got))[:10]
        fail("%s: path sets differ: only in the image %s, only in the source %s" % (what, only_got, only_want))
    bad = []
    for p, w in want.items():
        g = got[p]
        for k in ("type", "mode", "uid", "gid", "mtime_ns", "size"):
            if k == "mode" and w["type"] == "symlink":
                continue  # Linux symlinks are 0777; the driver stores 0755 (checked below)
            if g[k] != w[k]:
                bad.append("%s %s: image %r, source %r" % (p, k, g[k], w[k]))
        if w["type"] == "file" and g["sha256"] != w["sha256"]:
            bad.append("%s sha256: image %s, source %s" % (p, g["sha256"], w["sha256"]))
        if w["type"] == "symlink" and g["link"] != w["link"]:
            bad.append("%s link: image %r, source %r" % (p, g["link"], w["link"]))
        user = {k: v for k, v in g["xattr_values"].items() if k != SYMLINK_XATTR}
        if user != w["xattrs"]:
            bad.append("%s xattrs: image %s, source %s" % (p, sorted(user), sorted(w["xattrs"])))
        if w["type"] == "file" and g["nlink"] != w["nlink"]:
            bad.append("%s nlink: image %d, source %d" % (p, g["nlink"], w["nlink"]))
    if bad:
        fail("%s: %d differences, first: %s" % (what, len(bad), "; ".join(bad[:8])))
    links = {g["mode"] for g in got.values() if g["type"] == "symlink"}
    if links - {0o755}:
        fail("%s: symlink modes %s (the driver is expected to store 0755)" % (what, sorted(oct(m) for m in links)))
    got_groups = sorted(sorted(v) for v in groups.values())
    if got_groups != want_groups:
        fail("%s: hard-link groups differ: image %s, source %s" % (what, got_groups, want_groups))
    checks.append("%s: %d paths equal the source tree (type, size, sha256 through the decoded extents, "
                  "mode, owner, mtime to the ns, symlink targets, xattr names and values, hard-link "
                  "groups %d)" % (what, len(want), len(want_groups)))


# --- the space manager -----------------------------------------------------------
def read_spaceman(img, cont, checks):
    sb = cont["sb"]
    m = cont["ephemeral"].get(sb["nx_spaceman_oid"])
    if m is None or m["cpm_type"] & 0xFFFF != 5:
        fail("no ephemeral mapping for the space manager")
    nblk = m["cpm_size"] // BS
    b = img.obj(m["cpm_paddr"], nblk, "space manager")
    sm = decode(b, SM)
    if sm["sm_block_size"] != BS:
        fail("space manager block size")
    if sm["sm_dev1_block_count"] or sm["sm_dev0_cab_count"]:
        fail("a tier-2 device or a CAB is not supported")
    ncib = sm["sm_dev0_cib_count"]
    off = sm["sm_dev0_addr_offset"]
    cibs = list(struct.unpack_from("<%dQ" % ncib, b, off))
    chunks = []
    for ci_no, a in enumerate(cibs):
        cb = img.obj(a, 1, "chunk-info block %d" % ci_no)
        cib = decode(cb, CIB)
        for k in range(cib["cib_chunk_info_count"]):
            ci = decode(cb, CHUNK_INFO, CIB_END + k * CHUNK_INFO_SIZE)
            chunks.append(ci)
    free_ranges, free_total, bm_blocks = [], 0, []
    for ci in chunks:
        nb = ci["ci_block_count"]
        if ci["ci_bitmap_addr"] == 0:
            if ci["ci_free_count"] != nb:
                fail("a chunk without a bitmap that is not entirely free")
            free_ranges.append([ci["ci_addr"], ci["ci_addr"] + nb - 1])
            free_total += nb
            continue
        bm = img.read(ci["ci_bitmap_addr"])
        bm_blocks.append(ci["ci_bitmap_addr"])
        zeros = base.pop_zero_bits(bm, nb)
        if zeros != ci["ci_free_count"]:
            fail("chunk at %d: %d zero bits, ci_free_count %d" % (ci["ci_addr"], zeros, ci["ci_free_count"]))
        free_ranges += base.ranges_from_bits(bm, ci["ci_addr"], nb)
        free_total += zeros
    if sm["sm_dev0_free_count"] != free_total:
        fail("sm_free_count %d, bitmaps say %d" % (sm["sm_dev0_free_count"], free_total))
    checks.append("space manager: sm_free_count = sum of ci_free_count = zero bits of the bitmaps (%d blocks)" % free_total)
    free_ranges = base.merge_ranges(free_ranges)
    fq = []
    for q in range(3):
        oid = sm["sm_fq%d_tree_oid" % q]
        cnt = sm["sm_fq%d_count" % q]
        entries = []
        if oid:
            def eres(o):
                m = cont["ephemeral"].get(o)
                if m is None:
                    fail("free queue %d: ephemeral oid %d is not mapped" % (q, o))
                return m["cpm_paddr"]

            ft = Tree(img, eres(oid), 0x9, eres, "free queue %d" % q, allow_ghosts=True)
            for key, val in ft.records:
                entries.append({"xid": u64(key), "paddr": u64(key, 8), "len": u64(val) if val else 1})
        if cnt < sum(e["len"] for e in entries):
            fail("free queue %d: sfq_count %d is below the %d blocks its records cover" % (q, cnt, sum(e["len"] for e in entries)))
        fq.append({"queue": q, "tree_oid": oid, "sfq_count": cnt, "entries": entries})
    return {"sm": sm, "cibs": cibs, "chunks": chunks, "free_ranges": free_ranges,
            "free_total": free_total, "bitmap_blocks": bm_blocks, "free_queues": fq,
            "block": m["cpm_paddr"], "blocks": nblk}


# --- main ----------------------------------------------------------------------
def oracle(path, src, label, cont_uuid, vol_uuid):
    img = Image(path)
    if img.bs != BS:
        fail("this oracle assumes 4 KiB blocks")
    checks = []
    spec = json.load(open(os.path.join(src, "specials.json")))
    cont = read_container(img, checks)
    sb = cont["sb"]
    xid = cont["xid"]
    if uuid_str(sb["nx_uuid"]) != cont_uuid:
        fail("container uuid %s, mkapfs was given %s" % (uuid_str(sb["nx_uuid"]), cont_uuid))
    checks.append("container uuid equals the mkapfs argument")

    comap = omap_decode(img, sb["nx_omap_oid"], "container")
    fs_oid = sb["nx_fs_oid"][0]
    if any(sb["nx_fs_oid"][1:]):
        fail("more than one volume")
    ent = omap_find(comap["entries"], fs_oid, xid)
    if ent is None:
        fail("volume oid %d not in the container omap" % fs_oid)
    vb = img.obj(ent["paddr"], 1, "volume superblock")
    vsb = decode(vb, APSB)
    vh = header(vb)
    if vsb["apfs_magic"] != 0x42535041:
        fail("volume superblock magic")
    name = base.cstr(vsb["apfs_volname"])
    if name != label:
        fail("volume name %r, mkapfs was given %r" % (name, label))
    if uuid_str(vsb["apfs_vol_uuid"]) != vol_uuid:
        fail("volume uuid")
    checks.append("volume name and uuid equal the mkapfs arguments")
    incompat = vsb["apfs_incompatible_features"]
    ci = bool(incompat & 1)
    ni = bool(incompat & 8)
    hashed = ci or ni
    if not ci:
        fail("this oracle expects the mkapfs default (case-insensitive: incompatible features 0x1)")
    if vsb["apfs_fs_flags"] & 1 == 0:   # APFS_FS_UNENCRYPTED
        fail("an encrypted volume")

    vomap = omap_decode(img, vsb["apfs_omap_oid"], "volume")
    vx = vh["o_xid"]

    def resolver(xid_):
        def r(oid):
            e = omap_find(vomap["entries"], oid, xid_)
            if e is None:
                fail("virtual oid %d has no omap entry at xid %d" % (oid, xid_))
            return e["paddr"]
        return r

    def load_tree(root_oid, xid_, what):
        r = resolver(xid_)
        t = Tree(img, r(root_oid), 0xE, r, what, allow_ghosts=True)
        return t

    live = load_tree(vsb["apfs_root_tree_oid"], vx, "live fs tree")
    live_vol = Volume(img, live.records, ci, hashed, "live")
    checks.append("live fs tree: %d records, %d nodes" % (len(live.records), len(live.nodes)))

    # the physical extent tree and snapshot metadata
    ext_tree = Tree(img, vsb["apfs_extentref_tree_oid"], 0xF, lambda o: o, "extent tree", allow_ghosts=True)
    phys_ext = []
    for key, val in ext_tree.records:
        k = u64(key)
        if k >> 60 != 2:
            fail("extent tree record of type %d" % (k >> 60))
        lk, owner, refcnt = struct.unpack("<QQi", val)
        phys_ext.append({"pblock": k & 0x0FFFFFFFFFFFFFFF, "len": lk & 0x0FFFFFFFFFFFFFFF,
                         "kind": lk >> 60, "owner": owner, "refcnt": refcnt})
    snap_tree = Tree(img, vsb["apfs_snap_meta_tree_oid"], 0x10, lambda o: o, "snapshot metadata tree", allow_ghosts=True)
    snaps = []
    snap_names = {}
    for key, val in snap_tree.records:
        k = u64(key)
        t, sx = k >> 60, k & 0x0FFFFFFFFFFFFFFF
        if t == 1:
            ref_oid, sblock_oid, create, change, inum, ref_type, sflags, nlen = struct.unpack_from("<QQQQQIIH", val, 0)
            nm = val[50:50 + nlen]
            if len(nm) != nlen or not nm.endswith(b"\0"):
                fail("snapshot name")
            snaps.append({"xid": sx, "extentref_tree_oid": ref_oid, "sblock_oid": sblock_oid,
                          "create_time": create, "change_time": change, "inum": inum,
                          "flags": sflags, "name": nm[:-1].decode("utf-8")})
        elif t == 11:
            nlen = struct.unpack_from("<H", key, 8)[0]
            snap_names[key[10:10 + nlen - 1].decode("utf-8")] = u64(val)
        else:
            fail("snapshot metadata tree record of type %d" % t)
    for s in snaps:
        if snap_names.get(s["name"]) != s["xid"]:
            fail("snapshot %r: name record does not map to xid %d" % (s["name"], s["xid"]))
    if len(snaps) != vsb["apfs_num_snapshots"] or [x["xid"] for x in snaps] != [x["xid"] for x in vomap["snapshots"]]:
        fail("snapshot count: metadata %d, volume says %d, omap %d" % (len(snaps), vsb["apfs_num_snapshots"], len(vomap["snapshots"])))
    checks.append("snapshot metadata, snapshot names, the omap snapshot tree and apfs_num_snapshots agree (%d snapshot)" % len(snaps))

    # the live tree against final/, every snapshot against its source tree
    src_final, final_groups = source_tree(os.path.join(src, "final"), spec)
    live_nodes, live_groups = build_tree(img, live_vol, "live tree")
    compare_trees("live tree", live_nodes, live_groups, src_final, final_groups, spec, checks)

    snap_out = []
    snap_vols = []
    for s in snaps:
        sbk = img.obj(s["sblock_oid"], 1, "snapshot %s superblock" % s["name"])
        ssb = decode(sbk, APSB)
        if ssb["apfs_magic"] != 0x42535041:
            fail("snapshot superblock magic")
        sx = s["xid"]
        st = load_tree(ssb["apfs_root_tree_oid"], sx, "snapshot %s fs tree" % s["name"])
        svol = Volume(img, st.records, ci, hashed, "snapshot " + s["name"])
        nodes, groups = build_tree(img, svol, "snapshot " + s["name"])
        want, want_groups = source_tree(os.path.join(src, s["name"]), spec)
        compare_trees("snapshot " + s["name"], nodes, groups, want, want_groups, spec, checks)
        if s["extentref_tree_oid"]:
            Tree(img, s["extentref_tree_oid"], 0xF, lambda o: o, "snapshot %s extent tree" % s["name"], allow_ghosts=True)
        snap_vols.append((s, svol, nodes, ssb))
        snap_out.append({"name": s["name"], "xid": sx, "create_time": s["create_time"],
                         "change_time": s["change_time"], "sblock_oid": s["sblock_oid"],
                         "extentref_tree_oid": s["extentref_tree_oid"],
                         "records": len(st.records), "tree": node_list(nodes)})

    # directory-record hashes (the key's name hash against the recipe)
    all_nodes = list(live_nodes.values()) + [n for _, _, nodes, _ in snap_vols for n in nodes.values()]
    mism = [n["path"] for n in all_nodes if n["hash_matches"] is not None and not any(n["hash_matches"].values())]
    if mism:
        fail("directory-record hashes that no reading of the recipe reproduces: %s" % mism[:5])
    variants = {}
    for n in live_nodes.values():
        for k, v in (n["hash_matches"] or {}).items():
            variants.setdefault(k, []).append(v)
    variant_ok = {k: all(v) for k, v in variants.items()}
    checks.append("%d live directory records: name hash = CRC-32C of NFD(casefold(name)) as UTF-32; "
                  "variants matching every record: %s" % (len(live_nodes), sorted(k for k, ok in variant_ok.items() if ok)))

    # clones and sparse files
    for a, b in spec["clones"]:
        na, nb = live_nodes[a], live_nodes[b]
        if na["private_id"] != nb["private_id"] and nb["private_id"] != na["inode"]:
            fail("clone %s -> %s do not share a data stream" % (a, b))
        if na["runs"] != nb["runs"]:
            fail("clone runs differ")
    checks.append("clone pairs share their data stream and runs: %s" % spec["clones"])
    for p, size in spec["sparse"].items():
        n = live_nodes[p]
        if not any(r[0] < 0 for r in n["runs"]):
            fail("%s has no hole run" % p)
    checks.append("sparse files hold hole runs: %s" % sorted(spec["sparse"]))
    for a, b in spec["interleaved"]:
        for p in (a, b):
            data_runs = [r for r in merge_runs(live_nodes[p]["runs"]) if r[0] >= 0]
            if len(data_runs) < 20:
                fail("%s is not fragmented (%d data runs)" % (p, len(data_runs)))
    checks.append("interleaved files are fragmented: %s" % {p: len([r for r in merge_runs(live_nodes[p]["runs"]) if r[0] >= 0]) for ab in spec["interleaved"] for p in ab})

    # space accounting
    sp = read_spaceman(img, cont, checks)
    used = account(img, cont, sp, vsb, vomap, comap, live, ext_tree, snap_tree, snap_vols, live_nodes, live_vol, checks)

    free_blocks = sp["free_total"]
    out = {
        "checkpoint": {"ring": cont["ring"], "newest_xid": xid},
        "container": {"block_size": BS, "block_count": img.blocks, "uuid": cont_uuid,
                      "xid": xid, "checkpoints_in_ring": cont["checkpoints"],
                      "ring_index": cont["ring_index"], "next_xid": sb["nx_next_xid"],
                      "next_oid": sb["nx_next_oid"]},
        "volume": {"name": name, "uuid": vol_uuid, "xid": vx, "block": ent["paddr"], "features": vsb["apfs_features"],
                   "incompatible_features": incompat, "num_snapshots": vsb["apfs_num_snapshots"],
                   "num_files": vsb["apfs_num_files"], "num_directories": vsb["apfs_num_directories"],
                   "num_symlinks": vsb["apfs_num_symlinks"], "next_obj_id": vsb["apfs_next_obj_id"],
                   "last_mod_time": vsb["apfs_last_mod_time"],
                   "root_tree_oid": vsb["apfs_root_tree_oid"], "omap_oid": vsb["apfs_omap_oid"]},
        "live": {"records": len(live.records), "nodes": len(live.nodes),
                 "levels": max(n["level"] for n in live.nodes) + 1,
                 "record_counts": live_vol.counts, "tree": node_list(live_nodes)},
        "snapshots": snap_out,
        "extent_tree": phys_ext,
        "free_queues": sp["free_queues"],
        "free_ranges": sp["free_ranges"],
        "free_blocks": free_blocks,
        "accounting": used,
        "specials": {k: spec[k] for k in ("clones", "sparse", "interleaved", "snapshot")},
        "checks": checks,
    }
    return out


def merge_runs(runs):
    out = []
    for off, n in runs:
        if out and ((off < 0 and out[-1][0] < 0) or (off >= 0 and out[-1][0] >= 0 and out[-1][0] + out[-1][1] == off)):
            out[-1] = [out[-1][0], out[-1][1] + n]
        else:
            out.append([off, n])
    return out


def node_list(nodes):
    out = []
    for p in sorted(nodes):
        n = nodes[p]
        e = {"path": p, "type": n["type"], "size": n["size"], "mode": n["mode"], "uid": n["uid"],
             "gid": n["gid"], "mtime_ns": n["mtime_ns"], "nlink": n["nlink"], "inode": n["inode"],
             "private_id": n["private_id"], "xattrs": n["xattrs"],
             "name_hash": n["name_hash"], "date_added": n["date_added"]}
        if n["type"] == "file":
            e["sha256"] = n["sha256"]
            e["runs"] = merge_runs(n["runs"])
        if n["type"] == "symlink":
            e["link"] = n["link"]
        out.append(e)
    return out


def account(img, cont, sp, vsb, vomap, comap, live, ext_tree, snap_tree, snap_vols, live_nodes, live_vol, checks):
    """Every block some reachable structure or file accounts for, against the
    set bits of the bitmaps."""
    sm = sp["sm"]
    used = {}

    def add(blk, n, why):
        for b in range(blk, blk + n):
            used.setdefault(b, why)

    add(0, 1, "block 0")
    add(cont["desc"][0], cont["desc"][1], "checkpoint descriptor area")
    add(cont["data"][0], cont["data"][1], "checkpoint data area")
    add(sm["sm_ip_base"], sm["sm_ip_block_count"], "internal pool")
    add(sm["sm_ip_bm_base"], sm["sm_ip_bm_block_count"], "internal pool bitmap")
    for b in sp["bitmap_blocks"]:
        add(b, 1, "chunk bitmap")
    for om, nm in ((comap, "container"), (vomap, "volume")):
        for e in om["entries"]:
            if not e["flags"] & 1:
                add(e["paddr"], max(1, e["size"] // BS), "%s omap entry (an older version of an object is kept)" % nm)
    for q in sp["free_queues"]:
        for e in q["entries"]:
            add(e["paddr"], e["len"], "free queue %d (freed, not yet reusable)" % q["queue"])
    for blk, why in img.reached.items():
        add(blk, 1, why)
    # data: every extent of every tree (live and snapshots), xattr streams too
    def add_vol(vol, nodes):
        for recs in vol.extents.values():
            for logical, length, phys, _c in recs:
                if phys:
                    add(phys, length // BS, "file data")
    add_vol(live_vol, live_nodes)
    for s, svol, nodes, _ in snap_vols:
        add_vol(svol, nodes)
    # the bitmap's view
    set_bits = set()
    free = set()
    for a, b in sp["free_ranges"]:
        free.update(range(a, b + 1))
    total = img.blocks
    for blk in range(total):
        if blk not in free:
            set_bits.add(blk)
    unexplained = sorted(set_bits - set(used))
    missing = sorted(set(used) & free)
    stale = []
    for blk in unexplained:
        b = img.read(blk)
        if struct.unpack_from("<Q", b, 0)[0] != base.fletcher64(b):
            fail("block %d is allocated, unreachable and not a metadata object" % blk)
        h = header(b)
        stale.append({"block": blk, "type": h["type"], "oid": h["o_oid"], "xid": h["o_xid"]})
    info = {"allocated_blocks": len(set_bits), "accounted_blocks": len(used),
            "allocated_but_unaccounted": len(unexplained), "accounted_but_free": len(missing),
            "unaccounted_objects": stale}
    if missing:
        fail("blocks in use that the bitmap says are free: %s" % missing[:20])
    checks.append("no block that a reachable structure or file uses is free in the bitmap "
                  "(%d allocated, %d accounted, %d allocated blocks unaccounted for)"
                  % (len(set_bits), len(used), len(unexplained)) + "; each is a checksum-valid metadata object of an older transaction")
    return info


def main():
    if len(sys.argv) != 6:
        print(__doc__, file=sys.stderr)
        return 2
    try:
        out = oracle(*sys.argv[1:6])
    except OracleError as e:
        print("apfs_populated_oracle: FAIL: %s" % e, file=sys.stderr)
        return 1
    json.dump(out, sys.stdout, indent=1)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())

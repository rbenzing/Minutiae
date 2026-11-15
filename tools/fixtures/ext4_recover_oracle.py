#!/usr/bin/env python3
"""Independent oracle of the 3C ext4/ext3 recovery fixtures (ext4_recover.sh). Minutiae is never run.

  ext4_recover_oracle.py <final image> <payload dir> <manifest.json>   prints the oracle JSON (without "generator")
  ext4_recover_oracle.py families <expect.json>...                    exits 1 naming a family that did not occur

It reads the finished image with the raw Python reader of ext4_recover_probe.py (own crc32c, superblock, group
descriptors, bitmaps, inodes, extent trees, block maps) and its own directory scanner, and cross-checks with debugfs
(`stat`: the true maps) and dumpe2fs (the free blocks) when they are installed. The manifest lists the files the
generator deleted (path, family, source file in the payload dir, optional alias path for a hard link) and the stage
images in order: the TRUE map of a file is read from the last stage image in which it was still live.

For every deleted directory record of every live directory (the slack-scan rule of the ext4 reader: a record found in
the slack of another record, or the zeroed first record of a block) it states what the recovery of that record must
yield, derived from the facts only: the inode bitmap, the stale inode, its own map, the extent leaves in free blocks
bound to it by the metadata checksum, the free blocks, and the bytes of the source file the generator wrote.
Outcome words: skip reasons are the SkippedBy keys of the recover command (no-map, reused, undecided, unsupported,
no-free-bytes, no-runs, not-a-file, uniform, empty).
"""
import hashlib
import json
import os
import re
import shutil
import struct
import subprocess
import sys

sys.dont_write_bytecode = True  # importing the probe reader must not leave a __pycache__ in the (bind-mounted) tools directory
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ext4_recover_probe as P  # noqa: E402  (its raw reader and crc32c; the module has no side effects on import)

u16, u32, crc = P.u16, P.u32, P.crc

METHOD_INTACT = "ext-inode-intact"
METHOD_LEAF = "ext4-extent-leaf"
BASE = {METHOD_INTACT: 80, METHOD_LEAF: 55}
CAP_OVERLAP, CAP_CUT, CAP_NOT_REPLAYED = 30, 20, 40
EXTENT_MAGIC = 0xF30A
MAX_INIT = 32768
FL_EXTENTS, FL_INLINE, FL_EA_INODE, FL_INDEX = 0x80000, 0x10000000, 0x200000, 0x1000
INCOMPAT_FILETYPE, INCOMPAT_RECOVER = 0x2, 0x4
SECTION = "oracle"


class Stop(Exception):
    pass


def fail(msg):
    print("oracle: " + msg, file=sys.stderr)
    sys.exit(1)


# ------------------------------------------------------------------------------------------------ free blocks

def free_blocks(fs, path):
    """The free blocks of the image from the block bitmaps; cross-checked with dumpe2fs when it is installed."""
    s = set()
    for g, d in enumerate(fs.gd):
        if d["flags"] & 0x2:
            fail("%s: group %d is BLOCK_UNINIT; the oracle reads only fully initialised groups" % (path, g))
        start = fs.first + g * fs.bpg
        for n in range(start, min(start + fs.bpg, fs.blocks)):
            if not fs.block_bit(n):
                s.add(n)
    if shutil.which("dumpe2fs"):
        out = subprocess.run(["dumpe2fs", path], capture_output=True, text=True).stdout
        other = set()
        in_free = False
        for line in out.splitlines():
            m = re.match(r"\s+Free blocks: (.*)$", line)
            if m:
                for part in m.group(1).split(","):
                    part = part.strip()
                    if not part:
                        continue
                    a, _, b = part.partition("-")
                    other.update(range(int(a), int(b or a) + 1))
        if other != s:
            fail("%s: the free blocks of the bitmaps (%d) and of dumpe2fs (%d) differ" % (path, len(s), len(other)))
    return s


# ------------------------------------------------------------------------------------------------ directories

def plausible_name(name):
    return len(name) > 0 and b"\0" not in name and b"/" not in name and name not in (b".", b"..")


def parse_records(fs, buf, limit, first_block, slack_deleted, out_live, out_deleted, where):
    """The record scan of the reader: live records, the zeroed first record of a block, and the slack of every record."""
    n = len(buf)
    off = 0
    while off < limit:
        if off + 12 > n:
            fail("%s: trailing bytes at %d" % (where, off))
        ino = u32(buf, off)
        rl = u16(buf, off + 4)
        if rl < 12 or rl % 4 or off + rl > n:
            fail("%s: invalid rec_len %d at %d" % (where, rl, off))
        nl, ft = buf[off + 6], buf[off + 7]
        if 8 + nl > rl:
            off += rl
            continue
        name = bytes(buf[off + 8:off + 8 + nl])
        if ino != 0 and name not in (b".", b".."):
            out_live.append(dict(ino=ino, name=name, ft=ft))
        elif ino == 0 and off == 0 and first_block is not None and nl > 0 and plausible_name(name) and first_block:
            out_deleted.append(dict(ino=0, name=name, ft=ft, where=where, off=off))
        if slack_deleted:
            q, to = off + ((8 + nl + 3) & ~3), off + rl
            while q + 8 < to:
                dino = u32(buf, q)
                dnl, dft = buf[q + 6], buf[q + 7]
                end = q + 8 + dnl
                if 1 <= dino <= fs.inodes and dnl > 0 and end <= to and plausible_name(bytes(buf[q + 8:end])):
                    out_deleted.append(dict(ino=dino, name=bytes(buf[q + 8:end]), ft=dft, where=where, off=q))
                    q += (8 + dnl + 3) & ~3
                    continue
                q += 4
        off += rl


def inline_regions(fs, ino):
    """The two record regions of an inline directory: i_block after the parent number, and the system.data value."""
    raw = ino["raw"]
    regions = [bytes(ino["ib"][4:60])]
    extra = u16(raw, 0x80)
    ea_start = 0x80 + extra
    if len(raw) > ea_start + 4 and u32(raw, ea_start) == 0xEA020000:
        p = ea_start + 4
        while p + 16 <= len(raw) and raw[p:p + 4] != b"\0\0\0\0":
            nlen, idx, voff, vino, vsize = raw[p], raw[p + 1], u16(raw, p + 2), u32(raw, p + 4), u32(raw, p + 8)
            name = raw[p + 16:p + 16 + nlen]
            if idx == 7 and name == b"data" and vsize:
                regions.append(bytes(raw[ea_start + 4 + voff:ea_start + 4 + voff + vsize]))
            p += (16 + nlen + 3) & ~3
    return regions


def read_dir(fs, dino):
    """(live records, deleted records) of directory inode number dino."""
    ino = fs.inode(dino)
    live, deleted = [], []
    where = "dir %d" % dino
    if ino["flags"] & FL_INLINE:
        for k, region in enumerate(inline_regions(fs, ino)):
            if len(region) >= 12:
                parse_records(fs, region, len(region), None, True, live, deleted, where + " inline %d" % k)
        return live, deleted
    if ino["flags"] & FL_INDEX:
        fail("%s is an indexed directory" % where)
    m = fs.lblk_map(ino)
    nblocks = (ino["size"] + fs.bs - 1) // fs.bs
    for lb in range(nblocks):
        if lb not in m:
            fail("%s: logical block %d is a hole" % (where, lb))
        buf = fs.block(m[lb])
        limit = len(buf)
        if fs.csum:
            tail = buf[-12:]
            if u32(tail, 0) == 0 and u16(tail, 4) == 12 and tail[6] == 0 and tail[7] == 0xDE:
                limit = len(buf) - 12
        parse_records(fs, buf, limit, lb == 0 and False or lb > 0 or None, True, live, deleted, where + " block %d" % m[lb])
    return live, deleted


def walk(fs):
    """All live directories from the root: {path: inode}, and every deleted record with its directory path."""
    dirs = {"/": 2}
    deleted = []
    todo = ["/"]
    while todo:
        path = todo.pop(0)
        live, dels = read_dir(fs, dirs[path])
        for r in dels:
            r["dir"] = path
            deleted.append(r)
        for r in live:
            if r["ft"] == 2:
                p = (path.rstrip("/") + "/" + r["name"].decode("latin-1"))
                if p not in dirs:
                    dirs[p] = r["ino"]
                    todo.append(p)
    return dirs, deleted


def lookup(fs, path):
    """The inode number of a path through the live directories, or None."""
    cur = 2
    for comp in [c for c in path.split("/") if c]:
        live, _ = read_dir(fs, cur)
        hit = [r for r in live if r["name"] == comp.encode("latin-1")]
        if not hit:
            return None
        cur = hit[0]["ino"]
    return cur


# ------------------------------------------------------------------------------------------------ stale inode

def mode_filetype(mode):
    return {0x8000: 1, 0x4000: 2, 0x2000: 3, 0x6000: 4, 0x1000: 5, 0xC000: 6, 0xA000: 7}.get(mode & 0xF000, 0)


def inode_state(fs, n):
    """'free' | 'used' | ('unknown', reason) from the inode bitmap, like the reader."""
    g, idx = divmod(n - 1, fs.ipg)
    d = fs.gd[g]
    honoured = fs.csum or bool(fs.ro & 0x10)
    if honoured and d["flags"] & 0x1:
        return ("unknown", "INODE_UNINIT")
    if fs.csum:
        ib = fs.d[d["ib"] * fs.bs: d["ib"] * fs.bs + fs.ipg // 8]
        c = crc(fs.seed, ib)
        if not d["hi"]:
            c &= 0xFFFF
        if c != d["ibcsum"]:
            return ("unknown", "inode bitmap checksum")
    if honoured and idx >= fs.ipg - d["unused"]:
        return ("unknown", "unused tail")
    return "used" if fs.inode_bit(n) else "free"


# ------------------------------------------------------------------------------------------------ stale map walk

class MapWalker:
    """The stale map of an inode: direct, indirect or extent pointers from logical block 0 to the first stop."""

    def __init__(self, fs, ino, free, limit):
        self.fs, self.ino, self.free = fs, ino, free
        self.bs = fs.bs
        self.limit = min(limit, fs.blocks * fs.bs)
        self.nb = (self.limit + self.bs - 1) // self.bs
        self.next = 0
        self.segs = []  # (lblk, phys, n)
        self.tree = []
        self.seen = set()
        self.stop = ""
        self.cleared = False
        self.seed = fs.inode_seed(ino["ino"], ino["gen"])

    def stop_at(self, reason, lblk):
        if not self.stop:
            self.stop = "%s@%d" % (reason, lblk)
        raise Stop()

    def add_seg(self, phys, n):
        n = min(n, self.nb - self.next)
        if n <= 0:
            return
        self.segs.append((self.next, phys, n))
        self.next += n

    def read_map_block(self, blk, lblk):
        if blk == 0 or blk >= self.fs.blocks:
            self.stop_at("map-damaged", lblk)
        if blk in self.seen:
            self.stop_at("map-overlap", lblk)
        if blk not in self.free:
            self.stop_at("map-block-not-free", lblk)
        self.seen.add(blk)
        self.tree.append(blk)
        return self.fs.block(blk)

    def run(self):
        try:
            if self.limit <= 0:
                self.cleared = True
            elif self.ino["flags"] & FL_EXTENTS:
                self.extent_root()
            else:
                self.block_map()
        except Stop:
            pass
        return self.finish()

    def extent_root(self):
        b = self.ino["ib"]
        if not any(b):
            self.cleared = True
            return
        if u16(b, 0) != EXTENT_MAGIC:
            self.stop_at("map-damaged", 0)
        depth = u16(b, 6)
        if depth > 5:
            self.stop_at("map-damaged", 0)
        if u16(b, 2) == 0 and depth == 0 and u16(b, 4) <= 4:
            self.cleared = True
            return
        self.node(b, depth)

    def node(self, buf, want_depth):
        if len(buf) < 12 or u16(buf, 0) != EXTENT_MAGIC:
            self.stop_at("map-damaged", self.next)
        entries, mx, depth = u16(buf, 2), u16(buf, 4), u16(buf, 6)
        if depth != want_depth or mx > (len(buf) - 12) // 12 or entries > mx:
            self.stop_at("map-damaged", self.next)
        prev = -1
        for i in range(entries):
            if self.next >= self.nb:
                return
            e = buf[12 + 12 * i: 24 + 12 * i]
            if depth > 0:
                lblk = u32(e, 0)
                if lblk <= prev:
                    self.stop_at("map-damaged", self.next)
                prev = lblk
                if lblk > self.next:
                    self.stop_at("hole", self.next)
                if lblk < self.next:
                    self.stop_at("map-damaged", self.next)
                child = u32(e, 4) | (u16(e, 8) << 32)
                cbuf = self.read_map_block(child, lblk)
                if u16(cbuf, 0) != EXTENT_MAGIC:
                    self.stop_at("map-damaged", lblk)
                if self.fs.csum:
                    tail = 12 + 12 * u16(cbuf, 4)
                    if tail + 4 > len(cbuf):
                        self.stop_at("map-damaged", lblk)
                    if u32(cbuf, tail) != crc(self.seed, cbuf[:tail]):
                        self.stop_at("map-block-checksum", lblk)
                self.node(cbuf, depth - 1)
                continue
            lblk, n, hi, lo = struct.unpack("<IHHI", e)
            uninit = n > MAX_INIT
            if uninit:
                n -= MAX_INIT
            phys = (hi << 32) | lo
            if n == 0 or lblk < self.next or lblk + n > 1 << 32:
                self.stop_at("map-damaged", self.next)
            if lblk > self.next:
                self.stop_at("hole", self.next)
            if uninit:
                self.stop_at("uninit", lblk)
            if phys < 1 or phys + n > self.fs.blocks:
                self.stop_at("map-damaged", lblk)
            self.add_seg(phys, n)

    def block_map(self):
        b = self.ino["ib"]
        if not any(b):
            self.cleared = True
            return
        ptr = struct.unpack("<15I", b)
        for i in range(12):
            self.ptr(ptr[i], 0)
        for i in range(3):
            self.ptr(ptr[12 + i], i + 1)

    def ptr(self, p, level):
        if self.next >= self.nb:
            return
        if p == 0:
            self.stop_at("hole", self.next)
        if level == 0:
            if p >= self.fs.blocks:
                self.stop_at("map-damaged", self.next)
            self.add_seg(p, 1)
            return
        buf = self.read_map_block(p, self.next)
        for i in range(self.bs // 4):
            if self.next >= self.nb:
                break
            self.ptr(u32(buf, 4 * i), level - 1)

    def finish(self):
        if not self.cleared and not self.stop and self.next < self.nb:
            self.stop = "hole@%d" % self.next
        k = first_overlap(self.segs, self.tree)
        if k < len(self.segs):
            self.stop = "map-overlap@%d" % self.segs[k][0]
            self.segs = self.segs[:k]
        return trim(self.segs, self.limit, self.bs, self.stop, self.cleared, self.tree)


def first_overlap(segs, extra_blocks):
    """The index of the first extent that overlaps an earlier one or one of extra_blocks (len(segs) when none does)."""
    used = set(extra_blocks)
    for k, (lb, ph, n) in enumerate(segs):
        for b in range(ph, ph + n):
            if b in used:
                return k
        used.update(range(ph, ph + n))
    return len(segs)


def trim(segs, limit, bs, stop, cleared, tree):
    """Segments to the block list of a candidate: the blocks in file order, the last trimmed to `limit` bytes."""
    blocks, covered = [], 0
    for lb, ph, n in segs:
        length = n * bs
        over = covered + length - limit
        if over > 0:
            length -= over
        if length <= 0:
            break
        nb = (length + bs - 1) // bs
        blocks.extend(range(ph, ph + nb))
        covered += length
    return dict(blocks=blocks, covered=covered, stop=stop, cleared=cleared, tree=tree)


def runs_of(blocks):
    out = []
    for b in blocks:
        if out and out[-1][0] + out[-1][1] == b:
            out[-1][1] += 1
        else:
            out.append([b, 1])
    return out


# ------------------------------------------------------------------------------------------------ extent leaves

def plausible_leaf(fs, block):
    if u16(block, 0) != EXTENT_MAGIC:
        return None
    ent, mx, depth = u16(block, 2), u16(block, 4), u16(block, 6)
    if depth != 0 or mx != (fs.bs - 12) // 12 or not 1 <= ent <= mx:
        return None
    exts, prev_end = [], 0
    for i in range(ent):
        lb, ln, hi, lo = struct.unpack_from("<IHHI", block, 12 + 12 * i)
        n = ln - MAX_INIT if ln > MAX_INIT else ln
        if ln == 0 or n == 0:
            return None
        phys = (hi << 32) | lo
        if phys + n > fs.blocks or (i > 0 and lb < prev_end) or lb + n > 1 << 32:
            return None
        exts.append((lb, n, phys, ln > MAX_INIT))
        prev_end = lb + n
    return dict(first=exts[0][0], end=prev_end, exts=exts)


def leaves_of(fs, free, ino):
    """Free blocks that are plausible leaves whose checksum verifies under this inode and generation."""
    seed = fs.inode_seed(ino["ino"], ino["gen"])
    tail = 12 + 12 * ((fs.bs - 12) // 12)
    out = []
    for blk in sorted(free):
        b = fs.block(blk)
        if b[0:2] != b"\x0a\xf3":
            continue
        pl = plausible_leaf(fs, b)
        if pl is None:
            continue
        if crc(seed, b[:tail]) == u32(b, tail):
            pl["blk"] = blk
            out.append(pl)
    return out


def root_cleared(ib):
    return not any(ib) or (u16(ib, 0) == EXTENT_MAGIC and u16(ib, 2) == 0 and u16(ib, 6) == 0 and u16(ib, 4) <= 4)


def chain_leaves(fs, ino, leaves):
    """The chain of bound leaves from logical block 0, like the reader: (candidate dict)."""
    bs = fs.bs
    leaves = sorted(leaves, key=lambda l: (l["first"], l["end"], l["blk"]))
    last_end = max(l["end"] for l in leaves)
    segs, chained, chained_at, cursor, stop = [], [], [], 0, ""

    def stop_at(reason, lblk):
        nonlocal stop
        if not stop:
            stop = "%s@%d" % (reason, lblk)

    broke = False
    for i, l in enumerate(leaves):
        first = l["first"]
        if first > cursor:
            stop_at("leaf-gap", cursor)
            break
        if first < cursor or (i + 1 < len(leaves) and leaves[i + 1]["first"] < l["end"]):
            stop_at("leaf-conflict", first)
            break
        chained.append(l["blk"])
        chained_at.append(len(segs))
        for lb, n, ph, un in l["exts"]:
            if lb != cursor:
                stop_at("hole", cursor)
            elif un:
                stop_at("uninit", lb)
            elif ph < 1:
                stop_at("map-damaged", lb)
            if stop:
                broke = True
                break
            segs.append((lb, ph, n))
            cursor += n
        if broke:
            break
    k = first_overlap(segs, [l["blk"] for l in leaves])
    if k < len(segs):
        cursor = segs[k][0]
        stop = "map-overlap@%d" % cursor
        segs = segs[:k]
        chained = chained[:sum(1 for c in chained_at if c < k)]
    size_in = ino["size"]
    want = (size_in - 1) // bs + 1 if size_in > 0 else 0
    if not stop and size_in > 0 and want > cursor:
        stop_at("leaf-gap", cursor)
    assumptions = ["inode-not-reused-since-entry-deleted", "owner-by-crc32c-seed", "leaf-may-be-superseded"]
    if size_in > 0:
        size = size_in
        assumptions.append("size-from-inode")
        if want != cursor:
            assumptions.append("leaf-end-disagrees-with-inode-size")
    elif stop and last_end > cursor:
        size = last_end * bs
        assumptions.append("file-end-unknown")
    else:
        size = cursor * bs
        assumptions += ["size-from-extent-leaf-blocks", "file-end-unknown"]
    if root_cleared(ino["ib"]):
        assumptions.append("in-inode-map-cleared")
    t = trim(segs, size, bs, stop, False, chained)
    return dict(method=METHOD_LEAF, size=size, blocks=t["blocks"], covered=t["covered"], stop=stop, assumptions=assumptions,
                tree=chained, index_leaves=len(leaves))


# ------------------------------------------------------------------------------------------------ one record

def stale_candidates(fs, free, rec):
    """('err', skip reason, text) or ('ok', stale inode, candidates)."""
    n = rec["ino"]
    if n == 0:
        return ("err", "undecided", "the record lost its inode number")
    st = inode_state(fs, n)
    if st == "used":
        return ("err", "reused", "inode %d is allocated" % n)
    if st != "free":
        return ("err", "undecided", "the allocation state of inode %d is unknown: %s" % (n, st[1]))
    ino = fs.inode(n)
    if fs.csum and not ino["csum_ok"]:
        fail("inode %d is free but its checksum does not match" % n)
    if ino["links"] != 0 or ino["dtime"] == 0:
        fail("inode %d is free with %d links, dtime %d" % (n, ino["links"], ino["dtime"]))
    if fs.incompat & INCOMPAT_FILETYPE and rec["ft"] != 0 and mode_filetype(ino["mode"]) != rec["ft"]:
        return ("err", "reused", "file type %d differs from the inode's" % rec["ft"])
    if ino["flags"] & FL_EA_INODE:
        fail("inode %d is an EA inode" % n)
    if ino["flags"] & FL_INLINE:
        return ("err", "unsupported", "inline data")
    cands = []
    if ino["mode"] & 0xF000 not in (0x8000, 0xA000):
        return ("err", "unsupported", "neither a regular file nor a symlink")
    if ino["size"] > 0:
        m = MapWalker(fs, ino, free, min(ino["size"], fs.blocks * fs.bs)).run()
        if not (m["cleared"] or (not m["blocks"] and not m["stop"])):
            ass = ["inode-not-reused-since-entry-deleted", "size-from-inode"]
            cands.append(dict(method=METHOD_INTACT, size=ino["size"], blocks=m["blocks"], covered=m["covered"], stop=m["stop"],
                              assumptions=ass, tree=m["tree"]))
    if fs.csum and ino["flags"] & FL_EXTENTS:
        leaves = leaves_of(fs, free, ino)
        if leaves:
            cands.append(chain_leaves(fs, ino, leaves))
    for c in cands:
        if c["stop"]:
            c["assumptions"].append("stale-chain-stop=" + c["stop"])
        if fs.incompat & INCOMPAT_RECOVER:
            c["assumptions"].append("journal-not-replayed")
        if ino["dtime"] < ino["ctime"]:
            c["assumptions"].append("dtime-before-ctime")
    return ("ok", ino, cands)


def plan_candidate(fs, free, c):
    """What the planner does with one candidate: ('skip', reason) or ('ok', planned dict)."""
    bs = fs.bs
    runs = runs_of(c["blocks"])
    if c["size"] > 0 and not runs:
        return ("skip", "no-runs")
    captured = []
    for b in c["blocks"]:
        if b not in free:
            break
        captured.append(b)
    if not captured:
        return ("skip", "empty" if c["size"] == 0 else "no-free-bytes")
    cut = len(captured) < len(c["blocks"])
    prefix = c["covered"] < c["size"]
    end_unknown = "file-end-unknown" in c["assumptions"]
    size = min(c["size"], len(captured) * bs) if cut else min(c["size"], c["covered"])
    not_replayed = "journal-not-replayed" in c["assumptions"]
    return ("ok", dict(method=c["method"], captured=captured, size=size, cut=cut, prefix=prefix, end_unknown=end_unknown,
                       not_replayed=not_replayed, cand=c))


def confidence(p, overlap):
    base = BASE[p["method"]]
    if overlap:
        base = min(base, CAP_OVERLAP)
    if p["not_replayed"]:
        base = min(base, CAP_NOT_REPLAYED)
    if p["cut"] or p["prefix"] or p["end_unknown"]:
        base = min(base, CAP_CUT)
    return base


def relation(artifact, source):
    if artifact == source:
        return "full"
    if len(artifact) > len(source) and artifact[:len(source)] == source:
        return "prefix-of-artifact"
    if len(artifact) < len(source) and source[:len(artifact)] == artifact:
        return "artifact-is-prefix"
    return "no"


# ------------------------------------------------------------------------------------------------ true maps

def stat_blocks(img, target):
    """The data blocks and tree blocks debugfs reports for `stat target` (an inode as <n>, or a path)."""
    out = subprocess.run(["debugfs", "-R", "stat " + target, img], capture_output=True, text=True).stdout
    data, tree = {}, []
    for m in re.finditer(r"\((ETB\d+|IND|DIND|TIND|\d+(?:-\d+)?)\):(\d+(?:-\d+)?)", out):
        key, val = m.group(1), m.group(2)
        a, _, b = val.partition("-")
        pa, pb = int(a), int(b or a)
        if key[0].isalpha():
            tree.extend(range(pa, pb + 1))
            continue
        la, _, lb = key.partition("-")
        for k in range(int(lb or la) - int(la) + 1):
            data[int(la) + k] = pa + k
    return data, tree


def true_map(snap_fs, snap_paths, ino_n):
    """(data map lblk->pblk, tree blocks) of inode ino_n in a stage image, from the raw reader; debugfs cross-checks it."""
    ino = snap_fs.inode(ino_n)
    if ino["flags"] & FL_INLINE:
        return {}, []  # the bytes are in the inode
    data = snap_fs.lblk_map(ino)
    tree = []
    if ino["flags"] & FL_EXTENTS:
        tree = [blk for _, blk in snap_fs.extents(ino["ib"])["nodes"]]
    else:
        ptr = struct.unpack("<15I", ino["ib"])
        if ptr[12]:
            tree = [ptr[12]]
    return data, tree


def main(final_path, pay, manifest_path):
    man = json.load(open(manifest_path))
    final = P.Fs(final_path)
    snaps = [(p, P.Fs(p)) for p in man["snapshots"]]
    free = free_blocks(final, final_path)
    dirs, found = walk(final)
    if len(final.d) != final.blocks * final.bs:
        fail("%s: the image size is not the filesystem size" % final_path)
    bs = final.bs
    files = {f["path"]: f for f in man["files"]}
    seen = set()
    recs = []
    for r in sorted(found, key=lambda r: (r["dir"], r["off"], r["name"])):
        path = r["dir"].rstrip("/") + "/" + r["name"].decode("latin-1")
        entry = files.get(path)
        seen.add(path)
        recs.append((path, r, entry))
    # a file the 6.1 kernel removed leaves no record: its name is nowhere in its directory, and its inode is free
    wiped = []
    for path, f in sorted(files.items()):
        if path in seen or not f["family"].startswith("wiped"):
            continue
        parent, _, name = path.rpartition("/")
        dn = lookup(final, parent or "/")
        blocks = [final.block(pb) for _, pb in sorted(final.lblk_map(final.inode(dn)).items())]
        n = next((lookup(sfs, path) for _, sfs in reversed(snaps) if lookup(sfs, path) is not None), None)
        if n is None:
            fail("no stage image shows %s live" % path)
        wiped.append(dict(path=path, name=name, inode=n, inode_free=final.inode_bit(n) == 0,
                          name_in_directory=any(name.encode() in b for b in blocks)))
    missing = [p for p in files if p not in seen and not files[p]["family"].startswith("wiped")]
    if missing:
        fail("deleted entries not found in the image: %s" % ", ".join(missing))

    items = []
    for path, r, entry in recs:
        item = dict(path=path, name=r["name"].decode("latin-1"), family=entry["family"] if entry else "unlisted", inode=r["ino"], r=r, entry=entry)
        src = b""
        if entry and entry["src"] != "-":
            src = open(os.path.join(pay, entry["src"]), "rb").read()
        item["source"] = src
        # the type of the entry: from the inode it names as it is now, or from the record when it names none
        if r["ino"]:
            item["is_file"] = final.inode(r["ino"])["mode"] & 0xF000 in (0x8000, 0xA000)
        else:
            item["is_file"] = r["ft"] in (1, 7)
        items.append(item)

    # true maps: the last stage image in which the file (by path, or by its alias) was live
    for it in items:
        e = it["entry"]
        if not e:
            continue
        if e.get("true") == "content":
            # written and removed in ONE boot: no stage image shows it live. Its true blocks are the blocks of the image that hold
            # the blocks of its source (the source is pseudo-random, so a block is told from every other by its bytes).
            src = it["source"]
            index = {}
            for pb in range(final.first, final.blocks):
                b = final.block(pb)
                if any(b):
                    index.setdefault(bytes(b), []).append(pb)
            data = {}
            for k in range((len(src) + bs - 1) // bs):
                hits = index.get(src[k * bs:(k + 1) * bs].ljust(bs, b"\0"), [])
                if len(hits) != 1:
                    fail("%s: logical block %d of the source is held by %d blocks of the image, want exactly one" % (e["path"], k, len(hits)))
                data[k] = hits[0]
            it["true"] = dict(data=data, tree=[], snap=None, snap_path=None, ino=None, basis="content-scan")
            continue
        for p in (e["path"], e.get("alias")):
            if not p:
                continue
            for img, sfs in reversed(snaps):
                n = lookup(sfs, p)
                if n is not None and sfs.inode(n)["mode"] & 0xF000 == 0x8000:
                    data, tree = true_map(sfs, img, n)
                    it["true"] = dict(data=data, tree=tree, snap=sfs, snap_path=img, ino=n, basis="stage-image")
                    break
            if "true" in it:
                break
        if "true" not in it and e["src"] != "-" and e["family"] != "pad-aux":
            fail("no stage image shows %s live" % e["path"])
        if "true" in it and it["true"]["snap"] is not None and shutil.which("debugfs"):
            d, t = stat_blocks(it["true"]["snap_path"], "<%d>" % it["true"]["ino"])
            if it["true"]["snap"].inode(it["true"]["ino"])["size"] > 0 and (d != it["true"]["data"] or sorted(t) != sorted(it["true"]["tree"])):
                fail("%s: debugfs and the raw reader disagree on the true map" % e["path"])

    # per item: the reader's answer, the planner's cut and confidence
    for it in items:
        r = it["r"]
        it["err"] = None
        it["planned"] = []
        if not it["is_file"]:
            it["err"] = ("not-a-file", "a deleted directory or special file")
            continue
        st = stale_candidates(final, free, r)
        if st[0] == "err":
            it["err"] = (st[1], st[2])
            continue
        _, ino, cands = st
        it["ino_rec"] = ino
        it["cands"] = cands
        if not cands:
            it["err"] = ("no-map", "no candidate")
            continue
        rejects = []
        for c in cands:
            kind, p = plan_candidate(final, free, c)
            if kind == "skip":
                rejects.append(p)
                continue
            data = b"".join(final.block(b) for b in p["captured"])[:p["size"]]
            if len(data) > 0 and data == bytes([data[0]]) * len(data):
                rejects.append("uniform")
                continue
            p["artifact"] = data
            it["planned"].append(p)
        it["rejects"] = rejects
    # overlap among the selected candidates of different entries
    sel = []
    for it in items:
        if it["planned"]:
            it["planned"].sort(key=lambda p: -confidence(p, False))
            sel.append((it, it["planned"][0]))
    for it, p in sel:
        p["overlaps"] = sorted(o["path"] for o, q in sel if o is not it and set(p["captured"]) & set(q["captured"]))

    deleted, totals = [], dict(recovered=0, skipped_by={}, undecided=0)

    def skipped(reason, n=1):
        totals["skipped_by"][reason] = totals["skipped_by"].get(reason, 0) + n
        if reason == "undecided":
            totals["undecided"] += n

    for it in items:
        e = it["entry"]
        ino = it.get("ino_rec") or (final.inode(it["inode"]) if it["inode"] else None)
        rec = dict(path=it["path"], name=it["name"], family=it["family"], inode=it["inode"],
                   generation=ino["gen"] if ino else 0, source_sha256=hashlib.sha256(it["source"]).hexdigest(), source_size=len(it["source"]))
        t = it.get("true")
        rec["true_basis"] = t["basis"] if t else ""
        if t:
            lbs = sorted(t["data"])
            rec["true_blocks"] = runs_of([t["data"][k] for k in lbs])
            rec["tree_blocks"] = list(t["tree"])
        else:
            rec["true_blocks"], rec["tree_blocks"] = [], []
        rec["reader_blocks"], rec["reader_stop"] = [], ""
        rec["candidates"] = []
        if it.get("cands"):
            for c in it["cands"]:
                rec["candidates"].append(dict(method=c["method"], size=c["size"], blocks=runs_of(c["blocks"]), stop=c["stop"],
                                              assumptions=c["assumptions"]))
            rec["reader_blocks"], rec["reader_stop"] = rec["candidates"][0]["blocks"], rec["candidates"][0]["stop"]
        # kernel state: what the kernel left in the stale inode and in the leaves
        leaf_ok = False
        if t and it["inode"] and ino is not None and t["tree"] and t["snap"].inode(t["ino"])["flags"] & FL_EXTENTS:
            leaf_ok = True
            for blk in t["tree"]:
                tl = final.leaf_tail(it["inode"], ino["gen"], blk)
                if blk not in free or final.block(blk) != t["snap"].block(blk) or not tl or tl[3] != tl[4]:
                    leaf_ok = False
        rec["kernel_state"] = dict(i_size=ino["size"] if ino else 0,
                                   root_entries=(0 if not ino or ino["flags"] & FL_INLINE else u16(ino["ib"], 2) if ino["flags"] & FL_EXTENTS else
                                                 sum(1 for k in range(15) if u32(ino["ib"], 4 * k))),
                                   leaf_intact=leaf_ok, inode_free=bool(it["inode"]) and final.inode_bit(it["inode"]) == 0)
        ex = dict(outcome="skip", skip="", method="", size=0, captured_blocks=[], cut=False, incomplete=False, confidence=0,
                  assumptions=[], equal_source="no", candidate_methods=[c["method"] for c in it.get("cands", [])], overlaps=[])
        if it["planned"]:
            p = it["planned"][0]
            ov = p.get("overlaps", [])
            conf = confidence(p, bool(ov))
            ex.update(outcome="recover", method=p["method"], size=p["size"], captured_blocks=runs_of(p["captured"]), cut=p["cut"],
                      incomplete=p["cut"] or p["prefix"] or p["end_unknown"], confidence=conf, overlaps=ov,
                      assumptions=list(p["cand"]["assumptions"]) + (["overlap=" + o for o in ov]),
                      equal_source=relation(p["artifact"], it["source"]))
            totals["recovered"] += 1
            for q in it["planned"][1:]:
                skipped("not-selected")
            for reason in it["rejects"]:
                skipped(reason)
        else:
            reason = it["err"][0] if it["err"] else (it["rejects"][0] if it.get("rejects") else "no-map")
            ex["skip"] = reason
            skipped(reason)
            for extra in it.get("rejects", [])[1:]:
                skipped(extra)
        rec["expect"] = ex
        deleted.append(rec)
    out = dict(fs_type=man["fs_type"], block_size=bs, deleted_entry_count=len(items),
               unlisted_count=sum(1 for it in items if not it["entry"]),
               totals=totals, deleted=deleted, wiped=wiped)
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    print()


# ------------------------------------------------------------------------------------------------ families

REQUIRED = {
    "ext4-recover": {
        "small-kernel": lambda r: skip(r, "no-map"),
        "deep-kernel": lambda r: recover(r, METHOD_LEAF) and r["kernel_state"]["leaf_intact"] and len(r["true_blocks"]) >= 90
        and len(r["tree_blocks"]) >= 2 and r["expect"]["equal_source"] == "prefix-of-artifact",
        "deep-reuse-tail": lambda r: recover(r, METHOD_LEAF) and r["expect"]["cut"] and r["expect"]["equal_source"] == "artifact-is-prefix",
        "deep-reuse-head": lambda r: recover(r, METHOD_LEAF) and r["expect"]["equal_source"] == "no",
        "leaf-reused": lambda r: skip(r, "no-map") and r["tree_blocks"] and not r["kernel_state"]["leaf_intact"],
        "inode-reused": lambda r: skip(r, "reused"),
        "hardlink": lambda r: skip(r, "reused"),
        "inline": lambda r: skip(r, "unsupported"),
        "zero-size": lambda r: skip(r, "no-map"),
        "sparse-kernel": lambda r: recover(r, METHOD_LEAF) and r["reader_stop"].startswith("hole@") and r["expect"]["equal_source"] == "artifact-is-prefix",
        "intact-debugfs-small": lambda r: recover(r, METHOD_INTACT) and r["expect"]["confidence"] == 80 and r["expect"]["equal_source"] == "full",
        "intact-debugfs-deep": lambda r: recover(r, METHOD_INTACT) and r["expect"]["confidence"] == 80 and r["expect"]["equal_source"] == "full"
        and len(r["tree_blocks"]) >= 1,
    },
    # a depth-1 file written and removed in ONE boot (ruling 3C-C2 amended): the leaves at their home location are older versions,
    # so the leaf method either finds nothing usable or builds a prefix; it must never present a mis-assembled map
    "ext4-recover-sameboot": {
        "deep-same-boot": lambda r: len(r["true_blocks"]) >= 20 and r["true_basis"] == "content-scan"
        and (r["expect"]["outcome"] == "skip" and r["expect"]["skip"] in ("no-map", "undecided", "no-runs")
             or recover(r, METHOD_LEAF) and r["expect"]["confidence"] <= 20 and r["expect"]["equal_source"] != "no"),
    },
    "ext3-recover": {
        "blockmap-kernel-direct": lambda r: skip(r, "no-map"),
        "blockmap-kernel-indirect": lambda r: skip(r, "no-map"),
        "intact-debugfs-indirect": lambda r: recover(r, METHOD_INTACT) and r["expect"]["confidence"] == 80 and r["expect"]["equal_source"] == "full"
        and len(r["tree_blocks"]) == 1,
        # the reuser took part of the removed file: the map is complete (or stops at its indirect block) and the free prefix is cut
        "intact-debugfs-reused": lambda r: recover(r, METHOD_INTACT) and r["expect"]["cut"] and r["expect"]["confidence"] == 20
        and r["expect"]["equal_source"] == "artifact-is-prefix",
        "first-record": lambda r: skip(r, "undecided"),
    },
}


def skip(r, reason):
    return r["expect"]["outcome"] == "skip" and r["expect"]["skip"] == reason


def recover(r, method):
    return r["expect"]["outcome"] == "recover" and r["expect"]["method"] == method


def families(paths):
    bad, kinds = [], set()
    for p in paths:
        d = json.load(open(p))
        name = os.path.basename(p).replace(".expect.json", "")
        if name == "ext4-recover-wiped":
            if d["deleted_entry_count"] != 0 or d["deleted"]:
                bad.append("%s: a wiped record must list nothing, found %d" % (name, d["deleted_entry_count"]))
            continue
        for fam, ok in REQUIRED[name].items():
            hits = [r for r in d["deleted"] if r["family"] == fam]
            if not hits:
                bad.append("%s: family %s is missing" % (name, fam))
            elif not all(ok(r) for r in hits):
                bad.append("%s: family %s did not realise its scenario: %s" % (name, fam, json.dumps(hits[0]["expect"], sort_keys=True)))
        kinds.update(r["expect"]["equal_source"] for r in d["deleted"] if r["expect"]["outcome"] == "recover")
        if d["unlisted_count"]:
            print("%s: %d deleted entries the generator did not delete (logged, counted in deleted_entry_count)" % (name, d["unlisted_count"]))
    for k in ("full", "prefix-of-artifact", "artifact-is-prefix", "no"):
        if k not in kinds:
            bad.append("equal_source kind %s occurs in no image" % k)
    if bad:
        for b in bad:
            print("family check: " + b + "; re-rule, do not edit the ops", file=sys.stderr)
        sys.exit(1)
    print("families: every required family and equal_source kind occurs")


if __name__ == "__main__":
    if len(sys.argv) >= 3 and sys.argv[1] == "families":
        families(sys.argv[2:])
    elif len(sys.argv) == 4:
        main(*sys.argv[1:])
    else:
        raise SystemExit(__doc__)

#!/usr/bin/env python3
"""Independent jbd2 decoder and probe analysis of plan 3G (Task 1; Task 13 adds `oracle`).

It only READS finished disk images (never inside the guest, never with a kernel mount) with its own raw ext4
reader (superblock, group descriptors, inodes, extent trees, block maps), its own table-driven crc32c and its own
jbd2 decoder: journal superblock, live scan by the kernel's PASS_SCAN rule, stale scan, tags, revokes, commit
blocks and the checksums. It is built from the kernel header layout (plan 3G-R2 to R6, "from memory"), so it is
independent code, NOT an independent specification. debugfs, dumpe2fs and e2fsck are used only as cross-checks:
wherever both report a field the two are compared, and any difference exits 1.

  ext4_journal_oracle.py probe --payload <dir> <kernel>=<image dir> [<kernel>=<image dir> ...]
      prints FACT / HEX / CROSSCHECK / GATE-J<n> lines (grammar below). Exit 1 on a cross-check difference, 2 when a
      STOP gate (J1, J2, J4) does not hold, 0 otherwise.

Output grammar: "FACT J<n>.<tag> kernel=<k> key=value ...", "HEX <label> <hex>", "CROSSCHECK <ok|DIFF> <what> ...",
"GATE-J<n>: <value> <basis>".
"""
import binascii
import os
import re
import struct
import subprocess
import sys


def u16(b, o):
    return struct.unpack_from("<H", b, o)[0]


def u32(b, o):
    return struct.unpack_from("<I", b, o)[0]


def b16(b, o):
    return struct.unpack_from(">H", b, o)[0]


def b32(b, o):
    return struct.unpack_from(">I", b, o)[0]


def b64(b, o):
    return struct.unpack_from(">Q", b, o)[0]


# ---- crc32c (Castagnoli), raw register: no initial or final inversion; the callers pass the seed ----
TBL = []
for _i in range(256):
    _c = _i
    for _ in range(8):
        _c = (_c >> 1) ^ 0x82F63B78 if _c & 1 else _c >> 1
    TBL.append(_c)


def crc(seed, data):
    c = seed
    for x in data:
        c = TBL[(c ^ x) & 255] ^ (c >> 8)
    return c


# the standard check value: crc32c("123456789") = 0xE3069283 with init and final inversion
assert crc(0xFFFFFFFF, b"123456789") ^ 0xFFFFFFFF == 0xE3069283

MAGIC = 0xC03B3998
T_DESC, T_COMMIT, T_SB1, T_SB2, T_REVOKE = 1, 2, 3, 4, 5
BT_NAME = {1: "descriptor block", 2: "commit block", 3: "V1 superblock", 4: "V2 superblock", 5: "revoke table"}
F_ESCAPE, F_SAME_UUID, F_DELETED, F_LAST = 1, 2, 4, 8
C_CHECKSUM = 0x1
I_REVOKE, I_64BIT, I_ASYNC, I_CSUM2, I_CSUM3, I_FC = 0x1, 0x2, 0x4, 0x8, 0x10, 0x20
COMPAT_NAMES = {0x1: "journal_checksum"}
INCOMPAT_NAMES = {0x1: "journal_incompat_revoke", 0x2: "journal_64bit", 0x4: "journal_async_commit",
                  0x8: "journal_checksum_v2", 0x10: "journal_checksum_v3", 0x20: "journal_fast_commit"}

DIFFS = []
FACTS_J = {}


def fact(fid, **kv):
    print("FACT %s %s" % (fid, " ".join("%s=%s" % (k, v) for k, v in kv.items())))


def hx(label, b):
    print("HEX %-60s %s" % (label, binascii.hexlify(bytes(b)).decode()))


def check(what, a, b, **kv):
    """A cross-check between two independent readings of one field: logs ok or DIFF (which makes the exit status 1)."""
    if a == b:
        print("CROSSCHECK ok %s value=%s" % (what, str(a)[:200]))
        return True
    DIFFS.append(what)
    print("CROSSCHECK DIFF %s decoder=%s other=%s %s" % (what, str(a)[:400], str(b)[:400], " ".join("%s=%s" % i for i in kv.items())))
    return False


def run(*cmd):
    r = subprocess.run(list(cmd), capture_output=True, text=True, errors="replace")
    return r.returncode, r.stdout, r.stderr


def dbg(img, cmd):
    return run("debugfs", "-R", cmd, img)[1]


def seq_gt(a, b):
    return ((a - b) & 0xFFFFFFFF) != 0 and ((a - b) & 0xFFFFFFFF) < 0x80000000


def seq_ge(a, b):
    return a == b or seq_gt(a, b)


# ================================================================================================
# a raw ext4 reader (only what the journal analysis needs)
# ================================================================================================
class Img:
    def __init__(self, path):
        self.path = path
        self.d = open(path, "rb").read()
        sb = self.d[1024:2048]
        assert u16(sb, 0x38) == 0xEF53, "not ext: " + path
        self.sb = sb
        self.inodes = u32(sb, 0)
        self.blocks = u32(sb, 4)
        self.first = u32(sb, 0x14)
        self.bs = 1024 << u32(sb, 0x18)
        self.bpg = u32(sb, 0x20)
        self.ipg = u32(sb, 0x28)
        self.isz = u16(sb, 0x58)
        self.compat, self.incompat, self.ro = u32(sb, 0x5C), u32(sb, 0x60), u32(sb, 0x64)
        self.uuid = sb[0x68:0x78]
        self.has64 = bool(self.incompat & 0x80)
        if self.has64:
            self.blocks |= u32(sb, 0x150) << 32
        self.dsz = u16(sb, 0xFE) if self.has64 else 32
        self.csum = bool(self.ro & 0x400)
        self.seed = u32(sb, 0x270) if self.incompat & 0x2000 else crc(0xFFFFFFFF, self.uuid)
        self.ngroups = (self.blocks - self.first + self.bpg - 1) // self.bpg
        self.jinum = u32(sb, 0xE0)
        self.jdev = u32(sb, 0xE4)
        self.needs_recovery = bool(self.incompat & 0x4)
        self.has_journal = bool(self.compat & 0x4)
        self.fast_commit = bool(self.compat & 0x400)
        self.gd = [self._desc(g) for g in range(self.ngroups)]

    def _desc(self, g):
        o = (self.first + 1) * self.bs + g * self.dsz
        b = self.d[o:o + self.dsz]
        hi = self.dsz >= 64
        return dict(it=u32(b, 8) | ((u32(b, 0x28) << 32) if hi else 0))

    def block(self, n):
        return self.d[n * self.bs:(n + 1) * self.bs]

    def loc(self, ino):
        """(fs block, byte offset in the block) of an inode slot."""
        g, i = divmod(ino - 1, self.ipg)
        off = i * self.isz
        return self.gd[g]["it"] + off // self.bs, off % self.bs

    def itable_range(self):
        per = (self.ipg * self.isz + self.bs - 1) // self.bs
        return [(g["it"], g["it"] + per) for g in self.gd]

    def ino_of_slot(self, blk, slot):
        per = (self.ipg * self.isz + self.bs - 1) // self.bs
        for g, d in enumerate(self.gd):
            if d["it"] <= blk < d["it"] + per:
                return g * self.ipg + ((blk - d["it"]) * self.bs + slot) // self.isz + 1
        return None

    def inode(self, ino):
        blk, slot = self.loc(ino)
        return self.decode_inode(self.block(blk)[slot:slot + self.isz], ino)

    def inode_seed(self, ino, gen):
        return crc(crc(self.seed, struct.pack("<I", ino)), struct.pack("<I", gen))

    def decode_inode(self, raw, ino):
        raw = bytes(raw)
        r = dict(ino=ino, raw=raw, mode=u16(raw, 0), size=u32(raw, 4) | (u32(raw, 0x6C) << 32), ctime=u32(raw, 0xC),
                 mtime=u32(raw, 0x10), dtime=u32(raw, 0x14), links=u16(raw, 0x1A), blocks=u32(raw, 0x1C), flags=u32(raw, 0x20),
                 ib=raw[0x28:0x64], gen=u32(raw, 0x64))
        r["csum_ok"] = self.inode_csum_ok(ino, raw) if self.csum else None
        return r

    def inode_csum_ok(self, ino, raw):
        has_hi = self.isz > 128 and u16(raw, 0x80) >= 4
        z = bytearray(raw)
        z[0x7C:0x7E] = b"\0\0"
        if has_hi:
            z[0x82:0x84] = b"\0\0"
        c = crc(self.inode_seed(ino, u32(raw, 0x64)), bytes(z))
        if has_hi:
            return c == (u16(raw, 0x7C) | (u16(raw, 0x82) << 16))
        return (c & 0xFFFF) == u16(raw, 0x7C)

    # ---- maps: extents (any depth) or the 12+1+1+1 block map ----
    def mapping(self, ib, flags, home=None):
        """(dict lblk->pblk, [meta blocks]) of an i_block; tree blocks are read from `home` (an Img, default self)."""
        home = home or self
        m, meta = {}, []
        if flags & 0x80000:
            def rec(data, level):
                magic, ent, _, depth = struct.unpack_from("<HHHH", data, 0)
                if magic != 0xF30A or level > 5:
                    return
                for i in range(min(ent, (len(data) - 12) // 12)):
                    e = data[12 + 12 * i:24 + 12 * i]
                    if depth == 0:
                        lb, ln, hi, lo = struct.unpack("<IHHI", e)
                        ln = ln - 32768 if ln > 32768 else ln
                        for k in range(ln):
                            m[lb + k] = (hi << 32) | lo + k
                    else:
                        _, lo, hi, _ = struct.unpack("<IIHH", e)
                        blk = (hi << 32) | lo
                        meta.append(blk)
                        if 0 < blk < home.blocks:
                            rec(home.block(blk), level + 1)
            rec(ib, 0)
            return m, meta
        ptr = struct.unpack("<15I", ib)
        per = home.bs // 4
        for k in range(12):
            if ptr[k]:
                m[k] = ptr[k]

        def ind(blk, level, base):
            if not (0 < blk < home.blocks):
                return
            meta.append(blk)
            ents = struct.unpack("<%dI" % per, home.block(blk))
            span = per ** (level - 1)
            for i, p in enumerate(ents):
                if not p:
                    continue
                if level == 1:
                    m[base + i] = p
                else:
                    ind(p, level - 1, base + i * span)
        ind(ptr[12], 1, 12)
        ind(ptr[13], 2, 12 + per)
        ind(ptr[14], 3, 12 + per + per * per)
        return m, meta


def parse_debugfs_stat(text):
    """Fields of `debugfs stat`, plus the data map and the tree/indirect blocks it prints."""
    r = {}
    m = re.search(r"Inode: (\d+)\s+Type: (\w+)\s+Mode:\s+(\d+)\s+Flags: (0x[0-9a-f]+)", text)
    if m:
        r["flags"] = int(m.group(4), 16)
        r["mode"] = int(m.group(3), 8)
    m = re.search(r"Generation: (\d+)", text)
    if m:
        r["gen"] = int(m.group(1))
    m = re.search(r"Size: (\d+)", text)
    if m:
        r["size"] = int(m.group(1))
    m = re.search(r"Links: (\d+)\s+Blockcount: (\d+)", text)
    if m:
        r["links"], r["blockcount"] = int(m.group(1)), int(m.group(2))
    m = re.search(r"dtime:\s+0x([0-9a-f]+)", text)
    if m:
        r["dtime"] = int(m.group(1), 16)
    data, meta = {}, []
    sect = re.search(r"(?:EXTENTS|BLOCKS):\n(.*?)(?:\nTOTAL:|\Z)", text, re.S)
    if sect:
        for m in re.finditer(r"\((ETB\d+|IND|DIND|TIND|\d+(?:-\d+)?)\):(\d+)(?:-(\d+))?", sect.group(1)):
            tag = m.group(1)
            a = int(m.group(2))
            if tag[0].isalpha():
                meta.append(a)
                continue
            lo, _, hi = tag.partition("-")
            lo = int(lo)
            hi = int(hi) if hi else lo
            for k in range(hi - lo + 1):
                data[lo + k] = a + k
    r["map"], r["meta"] = data, meta
    return r


# ================================================================================================
# the jbd2 decoder
# ================================================================================================
class Txn:
    def __init__(self, seq, start, region):
        self.seq, self.start, self.region = seq, start, region
        self.tags, self.revokes, self.items, self.desc, self.rev_blocks = [], [], [], [], []
        self.commit, self.committed, self.csum = -1, False, "not-recorded"
        self.commit_sec = self.commit_nsec = 0
        self.warn = []
        self.csum_fields = None


class Jrn:
    def __init__(self, img):
        self.img = img
        self.bs = img.bs
        ino = img.inode(img.jinum)
        self.inode = ino
        m, self.meta = img.mapping(ino["ib"], ino["flags"])
        n = (ino["size"] + self.bs - 1) // self.bs
        self.map = [m.get(i) for i in range(n)]
        self.holes = [i for i, x in enumerate(self.map) if x is None]
        sb = self.read(0)
        self.sbraw = sb
        self.magic, self.btype, self.hseq = b32(sb, 0), b32(sb, 4), b32(sb, 8)
        self.blocksize, self.maxlen, self.first = b32(sb, 0xC), b32(sb, 0x10), b32(sb, 0x14)
        self.sequence, self.start, self.errno = b32(sb, 0x18), b32(sb, 0x1C), b32(sb, 0x20)
        self.fcompat, self.fincompat, self.fro = b32(sb, 0x24), b32(sb, 0x28), b32(sb, 0x2C)
        self.uuid = sb[0x30:0x40]
        self.nr_users = b32(sb, 0x40)
        self.max_trans, self.max_trans_data = b32(sb, 0x48), b32(sb, 0x4C)
        self.csum_type = sb[0x50]
        self.num_fc = b32(sb, 0x54)
        self.checksum = b32(sb, 0xFC)
        self.v1 = self.btype == T_SB1
        self.v2 = bool(self.fincompat & I_CSUM2) and not self.v1
        self.v3 = bool(self.fincompat & I_CSUM3) and not self.v1
        self.csum23 = self.v2 or self.v3
        self.f64 = bool(self.fincompat & I_64BIT) and not self.v1
        self.seed = crc(0xFFFFFFFF, self.uuid)
        z = bytearray(sb[:1024])
        z[0xFC:0x100] = b"\0\0\0\0"
        self.sb_csum_ok = (crc(0xFFFFFFFF, bytes(z)) == self.checksum) if self.csum23 else None
        if self.v3:
            self.tag_size = 16
        else:
            sz = 12 + (2 if self.v2 else 0)
            self.tag_size = sz if self.f64 else sz - 4
        self.fc_start = self.maxlen - (self.num_fc or 256) if (self.fincompat & I_FC) else self.maxlen
        self.live_end = self.fc_start

    def read(self, jb):
        fs = self.map[jb]
        return self.img.block(fs)

    def feature_names(self):
        out = []
        for bit, n in sorted(COMPAT_NAMES.items()):
            if self.fcompat & bit:
                out.append(n)
        for bit, n in sorted(INCOMPAT_NAMES.items()):
            if self.fincompat & bit:
                out.append(n)
        return out

    def wrap(self, x):
        return x - (self.live_end - self.first) if x >= self.live_end else x

    def hdr(self, blk):
        if b32(blk, 0) != MAGIC:
            return None
        return b32(blk, 4), b32(blk, 8)

    def desc_csum_ok(self, blk):
        if not self.csum23:
            return None
        z = bytearray(blk)
        z[-4:] = b"\0\0\0\0"
        return crc(self.seed, bytes(z)) == b32(blk, self.bs - 4)

    def commit_csum_ok(self, blk):
        if not self.csum23:
            return None
        z = bytearray(blk)
        z[0x10:0x14] = b"\0\0\0\0"
        return crc(self.seed, bytes(z)) == b32(blk, 0x10)

    def tag_csum_ok(self, seq, slot):
        if not self.csum23:
            return None
        c = crc(crc(self.seed, struct.pack(">I", seq)), slot)
        return c if self.v3 else c & 0xFFFF

    def parse_tags(self, blk):
        tsz = self.tag_size
        limit = self.bs - (4 if self.csum23 else 0)
        off, tags = 12, []
        while off + tsz <= limit and len(tags) < 4096:
            t = blk[off:off + tsz]
            fs = b32(t, 0)
            flags = b16(t, 6)
            if self.v3:
                csum, hi = b32(t, 12), b32(t, 8)
            else:
                csum, hi = b16(t, 4), (b32(t, 8) if self.f64 else 0)
            tags.append(dict(fs=(hi << 32) | fs, flags=flags, csum=csum, copy="?", slot=None))
            off += tsz
            if not flags & F_SAME_UUID:
                off += 16
            if flags & F_LAST:
                break
        return tags

    def walk(self, start, expected, claimed, region):
        """The kernel's PASS_SCAN rule from `start` with `expected` (3G-R3). Returns (txns, stop, consumed blocks, events)."""
        txns, cur, consumed, ev = [], None, set(), []
        b, exp, stop = start, expected, None
        stale = region == "stale"
        while True:
            if b in consumed or b in claimed:
                stop = ("cycle" if b in consumed else "claimed") + "@%d" % b
                break
            if not (self.first <= b < self.live_end):
                stop = "outside@%d" % b
                break
            blk = self.read(b)
            h = self.hdr(blk)
            if h is None or h[1] != exp:
                stop = "end-of-log@%d" % b
                ev.append(("end", b))
                break
            bt = h[0]
            if cur is None:
                cur = Txn(exp, b, region)
            consumed.add(b)
            ev.append(("hdr", bt, exp, b))
            if bt == T_DESC:
                cur.desc.append(b)
                ok = self.desc_csum_ok(blk)
                tags = self.parse_tags(blk)
                nxt = self.wrap(b + 1)
                for t in tags:
                    t["slot"] = nxt
                    ev.append(("tag", t["fs"], nxt, t["flags"]))
                    missing = False
                    if stale and (nxt in consumed or nxt in claimed or nxt < self.first or nxt >= self.live_end
                                  or (self.hdr(self.read(nxt)) or (0, 0))[0] in (T_DESC, T_COMMIT, T_REVOKE)):
                        missing = True
                    if missing:
                        t["copy"] = "missing"
                    else:
                        consumed.add(nxt)
                        data = self.read(nxt)
                        want = self.tag_csum_ok(exp, data)
                        if want is None:
                            t["copy"] = "unverified"
                        else:
                            t["copy"] = "verified" if want == t["csum"] else "mismatch"
                    t["escaped"] = bool(t["flags"] & F_ESCAPE)
                    t["seq"] = exp
                    nxt = self.wrap(nxt + 1)
                cur.tags += tags
                if ok is False:
                    cur.csum = "bad"
                    stop = "descriptor-checksum@%d" % b
                    break
                b = nxt
            elif bt == T_COMMIT:
                ok = self.commit_csum_ok(blk)
                cur.commit = b
                cur.commit_sec, cur.commit_nsec = b64(blk, 0x30), b32(blk, 0x38)
                cur.csum_fields = (blk[12], blk[13], b32(blk, 0x10))
                if ok is False:
                    cur.csum = "bad"
                    stop = "commit-checksum@%d" % b
                    txns.append(cur)
                    cur = None
                    break
                cur.committed = True
                cur.csum = "ok" if ok else "not-recorded"
                txns.append(cur)
                cur = None
                exp = (exp + 1) & 0xFFFFFFFF
                b = self.wrap(b + 1)
            elif bt == T_REVOKE:
                ok = self.desc_csum_ok(blk)
                rc = b32(blk, 12)
                limit = self.bs - (4 if self.csum23 else 0)
                cur.rev_blocks.append(b)
                if ok is False:
                    cur.csum = "bad"
                    stop = "revoke-checksum@%d" % b
                    break
                if rc < 16 or rc > limit:
                    stop = "revoke-damaged@%d" % b
                    break
                rec = 8 if self.f64 else 4
                off = 16
                while off + rec <= rc:
                    r = b64(blk, off) if rec == 8 else b32(blk, off)
                    cur.revokes.append(r)
                    ev.append(("rev", r))
                    off += rec
                b = self.wrap(b + 1)
            else:
                stop = "unexpected-block@%d" % b
                break
        if cur is not None:
            txns.append(cur)
        return txns, stop, consumed, ev

    def live_scan(self):
        if self.start == 0:
            return [], "empty", set(), []
        return self.walk(self.start, self.sequence, set(), "live")

    def stale_scan(self, claimed):
        claimed, frags = set(claimed), []
        for b in range(self.first, self.live_end):
            if b in claimed:
                continue
            h = self.hdr(self.read(b))
            if h and h[0] in (T_DESC, T_COMMIT, T_REVOKE):
                txns, stop, cons, _ = self.walk(b, h[1], claimed, "stale")
                for t in txns:
                    t.stop = stop
                claimed |= cons
                frags += txns
        return frags

    def walk_old(self, start):
        """What `logdump -O` does from journal block `start`: sequential, ignoring the expected sequence (1.47.0, from memory)."""
        ev, b, expected, stop = [], start, self.sequence, None
        seen = set()
        while b not in seen:
            seen.add(b)
            blk = self.read(b)
            h = self.hdr(blk)
            if h is None:
                ev.append(("nomagic", b))
                break
            bt, seq = h
            if seq != expected:
                ev.append(("seqmismatch", seq, expected, b))
            ev.append(("hdr", bt, seq, b))
            if bt == T_DESC:
                nxt = self.wrap(b + 1)
                for t in self.parse_tags(blk):
                    ev.append(("tag", t["fs"], nxt, t["flags"]))
                    nxt = self.wrap(nxt + 1)
                b = nxt
            elif bt == T_COMMIT:
                expected = (expected + 1) & 0xFFFFFFFF
                b = self.wrap(b + 1)
            elif bt == T_REVOKE:
                rc = b32(blk, 12)
                rec = 8 if self.f64 else 4
                off = 16
                while off + rec <= min(rc, self.bs):
                    ev.append(("rev", b64(blk, off) if rec == 8 else b32(blk, off)))
                    off += rec
                b = self.wrap(b + 1)
            else:
                ev.append(("unexpected", bt, b))
                break
        return ev


def jsuper(img, jinode_runs=None):
    """The journal superblock of an image as a dict (3G-R2)."""
    j = Jrn(img)
    return dict(magic=j.magic, blocktype=j.btype, blocksize=j.blocksize, maxlen=j.maxlen, first=j.first, sequence=j.sequence,
                start=j.start, compat=j.fcompat, incompat=j.fincompat, ro=j.fro, uuid=j.uuid.hex(), checksum_type=j.csum_type,
                num_fc=j.num_fc, checksum=j.checksum, checksum_ok=j.sb_csum_ok, tag_size=j.tag_size, features=j.feature_names())


def live_scan(j):
    return j.live_scan()


def stale_scan(j, claimed):
    return j.stale_scan(claimed)


def revoked_in(txns, x_seq, blk):
    """3G-R6: revoked iff a COMMITTED transaction R with seqGE(R, X) holds a revoke record for blk."""
    for r in txns:
        if r.committed and seq_ge(r.seq, x_seq) and blk in r.revokes:
            return True
    return False


def revokes(txns):
    return [(t.seq, t.region, t.start, list(t.revokes)) for t in txns if t.revokes]


def inode_copy(block, slot, isz=256, ino=0, img=None):
    """Decode the inode slot at byte offset `slot` of an inode-table block (with the inode checksum when `img` is given)."""
    raw = bytes(block[slot:slot + isz])
    if img is None:
        return dict(mode=u16(raw, 0), size=u32(raw, 4) | (u32(raw, 0x6C) << 32), dtime=u32(raw, 0x14), links=u16(raw, 0x1A),
                    flags=u32(raw, 0x20), ib=raw[0x28:0x64], gen=u32(raw, 0x64), raw=raw)
    return img.decode_inode(raw, ino)


def parse_logdump(text):
    """The events of a `debugfs logdump` output, in order: ('hdr', type, seq, block), ('tag', fs, jblk, flags), ('rev', blk),
    ('end', block) and ('seqmismatch', seq, expected, block); plus the 'Journal starts' line and the unrecognised lines."""
    ev, info, other = [], {}, []
    for line in text.splitlines():
        m = re.match(r"Journal starts at block (\d+), transaction (\d+)", line)
        if m:
            info["start"], info["seq"] = int(m.group(1)), int(m.group(2))
            continue
        m = re.match(r"Found expected sequence (\d+), type (\d+) \(.*?\) at block (\d+)", line)
        if m:
            ev.append(("hdr", int(m.group(2)), int(m.group(1)), int(m.group(3))))
            continue
        m = re.match(r"\s*FS block (\d+) logged at (?:sequence (\d+), )?journal block (\d+) \(flags 0x([0-9a-f]+)\)", line)
        if m:
            ev.append(("tag", int(m.group(1)), int(m.group(3)), int(m.group(4), 16)))
            continue
        m = re.match(r"\s*Revoke FS block (\d+)", line)
        if m:
            ev.append(("rev", int(m.group(1))))
            continue
        m = re.match(r"No magic number at block (\d+): end of journal", line)
        if m:
            ev.append(("nomagic", int(m.group(1))))
            continue
        m = re.match(r"Found sequence (\d+) \(not (\d+)\) at block (\d+): end of journal", line)
        if m:
            ev.append(("seqmismatch", int(m.group(1)), int(m.group(2)), int(m.group(3))))
            continue
        if line.startswith("Journal ") or line.startswith("Ext2 superblock") or line.startswith("Dumping ") or line.strip() == "" \
                or line.startswith("Contents of") or re.match(r"^[0-9a-f ]{3,}", line.strip()) or line.startswith("Journal"):
            continue
        other.append(line)
    return ev, info, other


def dumpe2fs_journal(path):
    out = run("dumpe2fs", "-h", path)[1]
    r = {}
    for line in out.splitlines():
        k, _, v = line.partition(":")
        v = v.strip()
        if k.startswith("Journal features"):
            r["features"] = v
        elif k == "Total journal blocks":
            r["blocks"] = int(v)
        elif k == "Fast commit length":
            r["fc"] = int(v)
        elif k == "Journal sequence":
            r["sequence"] = int(v, 16)
        elif k == "Journal start":
            r["start"] = int(v)
        elif k == "Journal checksum type":
            r["csum_type"] = v
        elif k == "Journal checksum":
            r["checksum"] = int(v, 16)
        elif k == "Filesystem features":
            r["fs_features"] = v
        elif k == "Filesystem state":
            r["state"] = v
    return r, out


def feature_bits(text):
    """dumpe2fs journal feature names -> (compat, incompat) bits; an unnamed one is returned in `unknown`."""
    comp = inc = 0
    unknown = []
    for n in text.split():
        if n == "(none)":
            continue
        for bit, nm in COMPAT_NAMES.items():
            if nm == n:
                comp |= bit
                break
        else:
            for bit, nm in INCOMPAT_NAMES.items():
                if nm == n:
                    inc |= bit
                    break
            else:
                unknown.append(n)
    return comp, inc, unknown


# ================================================================================================
# the probe
# ================================================================================================
class Stage:
    """One finished image with its decoded journal."""

    def __init__(self, path):
        self.path = path
        self.name = os.path.basename(path)[:-4]
        self.img = Img(path)
        self.j = Jrn(self.img)
        self.live, self.live_stop, self.live_blocks, self.live_ev = self.j.live_scan()
        self.stale = self.j.stale_scan(self.live_blocks)
        self.txns = self.live + self.stale

    def copies(self, ino):
        """Every inode-table copy of inode `ino` in the journal (both regions), newest first."""
        blk, slot = self.img.loc(ino)
        out = []
        for t in self.txns:
            for tag in t.tags:
                if tag["fs"] != blk or tag["copy"] not in ("verified", "unverified"):
                    continue
                data = self.j.read(tag["slot"])
                if tag["escaped"]:
                    data = b"\xc0\x3b\x39\x98" + data[4:]
                c = self.img.decode_inode(data[slot:slot + self.img.isz], ino)
                c.update(txn=t, tag=tag, revoked=revoked_in(self.txns, t.seq, blk))
                out.append(c)
        out.sort(key=lambda c: (c["txn"].seq, c["txn"].region == "live", c["txn"].start), reverse=True)
        # the sort above is by plain sequence value; the logs here never wrap the 32-bit counter
        return out


def tag_count(txns):
    return sum(len(t.tags) for t in txns)


def hexdump_tail(b, n=32):
    return binascii.hexlify(bytes(b[-n:])).decode()


def read_payload(pay, name):
    return open(os.path.join(pay, name), "rb").read()


def guest_lines(d, prefix):
    out = []
    for fn in sorted(os.listdir(d)):
        if fn.startswith("guest-") and fn.endswith(".log"):
            for line in open(os.path.join(d, fn), "rb").read().decode("latin-1").replace("\r", "").splitlines():
                if line.startswith(prefix):
                    out.append((fn, line))
    return out


def compare_events(what, dec, other):
    """Compare two event streams (decoder vs logdump); on a difference print the first one."""
    if dec == other:
        print("CROSSCHECK ok %s events=%d" % (what, len(dec)))
        return True
    for i in range(max(len(dec), len(other))):
        a = dec[i] if i < len(dec) else None
        b = other[i] if i < len(other) else None
        if a != b:
            DIFFS.append(what)
            print("CROSSCHECK DIFF %s first-difference-at-event=%d decoder=%s logdump=%s (decoder %d events, logdump %d)"
                  % (what, i, a, b, len(dec), len(other)))
            return False
    return True


def norm_live_events(ev):
    """Decoder live-walk events in logdump's vocabulary (the end marker is compared separately)."""
    return [e for e in ev if e[0] != "end"]


def crosscheck_journal_header(k, st, tag):
    """dumpe2fs vs the decoder's journal superblock; debugfs stat <8> vs the decoder's journal inode map."""
    j = st.j
    de, raw = dumpe2fs_journal(st.path)
    comp, inc, unk = feature_bits(de.get("features", ""))
    check("%s %s dumpe2fs journal features" % (k, tag), (j.fcompat, j.fincompat), (comp, inc), raw=de.get("features"), unknown=unk)
    check("%s %s dumpe2fs journal blocks" % (k, tag), j.maxlen, de.get("blocks"))
    check("%s %s dumpe2fs journal sequence" % (k, tag), j.sequence, de.get("sequence"))
    check("%s %s dumpe2fs journal start" % (k, tag), j.start, de.get("start"))
    check("%s %s dumpe2fs fast commit length" % (k, tag), j.num_fc if j.fincompat & I_FC else 0, de.get("fc", 0))
    if j.csum23:
        check("%s %s dumpe2fs journal checksum" % (k, tag), j.checksum, de.get("checksum"))
        check("%s %s dumpe2fs journal checksum type" % (k, tag), "crc32c" if j.csum_type == 4 else j.csum_type, de.get("csum_type"))
    return de


def crosscheck_inode(k, tag, img, ino, ref=None):
    """debugfs stat <ino> vs the decoder's inode and map (works on a deleted inode as well)."""
    ds = parse_debugfs_stat(dbg(img.path, "stat <%d>" % ino))
    i = img.inode(ino)
    for f in ("size", "links", "gen", "flags"):
        check("%s %s inode %d %s" % (k, tag, ino, f), i[f], ds.get(f))
    if "dtime" in ds:
        check("%s %s inode %d dtime" % (k, tag, ino), i["dtime"], ds["dtime"])
    m, meta = img.mapping(i["ib"], i["flags"])
    if i["flags"] & 0x10000000:
        return i, m, meta, ds
    if i["links"] or i["dtime"] == 0:
        check("%s %s inode %d map" % (k, tag, ino), sorted(m.items()), sorted(ds["map"].items()))
        check("%s %s inode %d tree/indirect blocks" % (k, tag, ino), sorted(meta), sorted(ds["meta"]))
    return i, m, meta, ds


def analyse_kernel(k, d, pay):
    res = {}
    print("== kernel %s: images in %s" % (k, d))
    st = {}
    for fn in sorted(os.listdir(d)):
        if fn.endswith(".img"):
            try:
                st[fn[:-4]] = Stage(os.path.join(d, fn))
            except Exception as e:  # a decode failure is a fact, not a crash
                print("FACT DECODE-FAIL kernel=%s image=%s error=%r" % (k, fn, e))
                DIFFS.append("decode %s %s" % (k, fn))
    res["stages"] = st
    inos = {}
    for fn, line in guest_lines(d, "PROBE-INO "):
        p = line.split()
        inos[(fn.replace("guest-", "").replace(".log", ""), p[1])] = int(p[2])
    res["inos"] = inos

    # ---- J11 / J8 / J1 basics on every image: journal superblock, features, tag size, checksums ----
    sigs = {}
    for name, s in st.items():
        j = s.j
        fact("J1.jsuper", kernel=k, image=name, version=2 if j.btype == T_SB2 else 1 if j.btype == T_SB1 else j.btype, blocksize=j.blocksize,
             maxlen=j.maxlen, first=j.first, sequence=j.sequence, start=j.start, compat=hex(j.fcompat), incompat=hex(j.fincompat),
             features="+".join(j.feature_names()) or "(none)", tag_size=j.tag_size, checksum_type=j.csum_type, num_fc_blks=j.num_fc,
             sb_checksum_ok=j.sb_csum_ok, needs_recovery=s.img.needs_recovery, fs_fast_commit=s.img.fast_commit, holes=len(j.holes),
             inode_size=j.inode["size"], inode_blocks_mapped=len(j.map) - len(j.holes))
        if name.endswith("-p0") or name.endswith("-p1"):
            hx("J1 jsb %s %s block 0 first 128 bytes" % (k, name), j.sbraw[:128])
        check("%s %s journal inode fits s_maxlen" % (k, name), (j.maxlen <= len(j.map) and not j.holes and j.magic == MAGIC), True)
        crosscheck_journal_header(k, s, name)
        ji = parse_debugfs_stat(dbg(s.path, "stat <8>"))
        check("%s %s debugfs stat <8> size" % (k, name), j.inode["size"], ji.get("size"))
        check("%s %s debugfs stat <8> journal block map" % (k, name), {i: x for i, x in enumerate(j.map) if x is not None}, ji.get("map"))
        check("%s %s s_journal_inum" % (k, name), s.img.jinum, 8)
        if s.img.jdev:
            print("FACT J1.jdev kernel=%s image=%s s_journal_dev=%#x" % (k, name, s.img.jdev))
    # per-image scan summaries and checksum counters
    csum_total = dict(desc=0, desc_ok=0, commit=0, commit_ok=0, revoke=0, revoke_ok=0, tag=0, tag_ok=0)
    for name, s in st.items():
        j = s.j
        nl, ns = len(s.live), len(s.stale)
        fact("J9.scan", kernel=k, image=name, live_txns=nl, live_committed=sum(1 for t in s.live if t.committed), live_stop=s.live_stop,
             stale_txns=ns, stale_committed=sum(1 for t in s.stale if t.committed), tags_live=tag_count(s.live), tags_stale=tag_count(s.stale),
             revoke_records=sum(len(t.revokes) for t in s.txns), seq_min=min([t.seq for t in s.txns], default=None),
             seq_max=max([t.seq for t in s.txns], default=None),
             log_blocks_used=len(s.live_blocks) + sum(len(t.desc) + len(t.rev_blocks) + (1 if t.commit >= 0 else 0) + len(t.tags) for t in s.stale),
             journal_sequence=j.sequence, start=j.start)
        for t in s.txns:
            for tg in t.tags:
                if tg["copy"] in ("verified", "mismatch"):
                    csum_total["tag"] += 1
                    csum_total["tag_ok"] += tg["copy"] == "verified"
            if t.commit >= 0:
                csum_total["commit"] += 1
                csum_total["commit_ok"] += t.csum != "bad"
            if t.desc:
                csum_total["desc"] += len(t.desc)
                csum_total["desc_ok"] += len(t.desc) if t.csum != "bad" else len(t.desc) - 1
    fact("J1.checksums", kernel=k, **csum_total)
    res["csum_total"] = csum_total

    # ---- J1: the live scan of the crash image equals logdump -a, event by event ----
    crash = st.get("ext4-p4")
    res["j1"] = {}
    if crash is not None:
        j = crash.j
        out = run("debugfs", "-R", "logdump -a", crash.path)[1]
        lev, info, other = parse_logdump(out)
        check("%s ext4-p4 logdump Journal starts at block" % k, j.start, info.get("start"))
        check("%s ext4-p4 logdump transaction" % k, j.sequence, info.get("seq"))
        dec_all = norm_live_events(crash.live_ev)
        endev = [e for e in lev if e[0] in ("nomagic", "seqmismatch")]
        lbody = [e for e in lev if e[0] not in ("nomagic", "seqmismatch")]
        ok_body = compare_events("%s ext4-p4 live scan vs logdump -a (headers, tags, revokes)" % k, dec_all, lbody)
        end_b = [e[1] for e in crash.live_ev if e[0] == "end"]
        lend = [e[-1] for e in endev]
        ok_end = check("%s ext4-p4 live scan end block vs logdump" % k, end_b, lend)
        if other:
            print("FACT J1.logdump-other kernel=%s lines=%r" % (k, other[:6]))
        fact("J1.live", kernel=k, image="ext4-p4", txns=len(crash.live), committed=sum(1 for t in crash.live if t.committed),
             tags=tag_count(crash.live), events=len(dec_all), stop=crash.live_stop, logdump_events=len(lbody), logdump_end=lend)
        csum_bad = [("tag", t.seq, tg["slot"]) for t in crash.live for tg in t.tags if tg["copy"] == "mismatch"] + \
                   [("txn", t.seq, t.commit) for t in crash.live if t.csum == "bad"]
        sbok = j.sb_csum_ok in (True, None)
        res["j1"] = dict(equal=ok_body and ok_end, csum_bad=csum_bad, sb_ok=sbok, tag_size=j.tag_size, live_txns=len(crash.live),
                         features=j.feature_names())
        # raw evidence: the first descriptor, the first commit, the first revoke block of the live region
        for t in crash.live[:1]:
            if t.desc:
                hx("J1 live descriptor %s ext4-p4 jblk %d head 64 + tail 16" % (k, t.desc[0]), j.read(t.desc[0])[:64] + b"..." + j.read(t.desc[0])[-16:])
            if t.commit >= 0:
                hx("J1 live commit %s ext4-p4 jblk %d head 64" % (k, t.commit), j.read(t.commit)[:64])
        for t in crash.live:
            if t.rev_blocks:
                hx("J1 live revoke %s ext4-p4 jblk %d head 48" % (k, t.rev_blocks[0]), j.read(t.rev_blocks[0])[:48])
                break
        # the kernel ran a commit and the descriptor tail checksums verify
        fact("J1.live-checksums", kernel=k, desc_tail_ok=all(j.desc_csum_ok(j.read(b)) in (True, None) for t in crash.live for b in t.desc),
             commit_ok=all(t.csum != "bad" for t in crash.live), tags_verified=sum(1 for t in crash.live for tg in t.tags if tg["copy"] == "verified"),
             tags_unverified=sum(1 for t in crash.live for tg in t.tags if tg["copy"] == "unverified"))
        # an uncommitted trailing txn is data too
        fact("J1.live-txns", kernel=k, seqs=",".join("%d%s" % (t.seq, "C" if t.committed else "u") for t in crash.live))
    else:
        print("FACT J1.missing kernel=%s image=ext4-p4" % k)

    # ---- J3: logdump -O on the clean images ----
    res["j3"] = {}
    for name in ("ext4-p1", "ext4-p2", "ext4-p3", "ext3-p1", "ext3-p2"):
        s = st.get(name)
        if s is None:
            continue
        rc, out, err = run("debugfs", "-R", "logdump -O -a", s.path)
        lev, info, other = parse_logdump(out)
        res["j3"][name] = dict(rc=rc, stderr=err.strip()[:200], events=len(lev), other=other[:4], info=info)
        if lev:
            start = 1
            dec = s.j.walk_old(start)
            nend = [e for e in dec if e[0] in ("nomagic",)]
            dec_body = [e for e in dec if e[0] not in ("nomagic",)]
            lbody = [e for e in lev if e[0] not in ("nomagic",)]
            compare_events("%s %s stale view vs logdump -O -a" % (k, name), dec_body, lbody)
            check("%s %s logdump -O end block" % (k, name), [e[1] for e in nend], [e[1] for e in lev if e[0] == "nomagic"])
            fact("J3.logdump-O", kernel=k, image=name, rc=rc, events=len(lev), headers=sum(1 for e in lev if e[0] == "hdr"),
                 tags=sum(1 for e in lev if e[0] == "tag"), revokes=sum(1 for e in lev if e[0] == "rev"),
                 seq_mismatch=sum(1 for e in lev if e[0] == "seqmismatch"), info=info, other=other[:3])
        else:
            fact("J3.logdump-O", kernel=k, image=name, rc=rc, events=0, stderr=repr(err.strip()[:200]), stdout_head=repr(out[:200]))
    return res


def inode_range_copy_report(k, s, ino, label):
    cs = s.copies(ino)
    rows = []
    for c in cs:
        rows.append("seq%d/%s/jblk%d/links%d/dtime%d/size%d/gen%d/%s%s" % (c["txn"].seq, c["txn"].region, c["tag"]["slot"], c["links"], c["dtime"],
                                                                         c["size"], c["gen"], c["tag"]["copy"], "/revoked" if c["revoked"] else ""))
    fact("J2.copies", kernel=k, image=s.name, file=label, ino=ino, n=len(cs), copies=";".join(rows[:40]))
    return cs


def analyse_j2(k, d, r, pay):
    """J2: after the write boot (p1) and the delete boot (p2), a stale fragment of p2 holds a live-state inode-table copy of the
    deleted inode with the map, size and generation of the on-disk deleted inode."""
    st, inos = r["stages"], r["inos"]
    out = {}
    for fsname, files in (("ext4", ("small", "deep")), ("ext3", ("bm",))):
        p1, p2 = st.get(fsname + "-p1"), st.get(fsname + "-p2")
        if p1 is None or p2 is None:
            out[fsname] = None
            continue
        for fname in files:
            ino = inos.get((fsname + "-p1", fname))
            if ino is None:
                fact("J2.skip", kernel=k, fs=fsname, file=fname, why="no PROBE-INO line")
                out[(fsname, fname)] = dict(found=False, why="no inode number")
                continue
            truth, tmap, tmeta, tds = crosscheck_inode(k, fsname + "-p1 (before the delete)", p1.img, ino)
            dead, dmap, dmeta, dds = crosscheck_inode(k, fsname + "-p2 (after the delete)", p2.img, ino)
            fact("J2.on-disk", kernel=k, fs=fsname, file=fname, ino=ino, pre_links=truth["links"], pre_size=truth["size"], pre_gen=truth["gen"],
                 pre_extents_flag=bool(truth["flags"] & 0x80000), pre_depth=(u16(truth["ib"], 6) if truth["flags"] & 0x80000 else "blockmap"),
                 pre_blocks=len(tmap), pre_meta=sorted(tmeta), post_links=dead["links"], post_dtime=dead["dtime"], post_size=dead["size"],
                 post_gen=dead["gen"], post_ib_zero=dead["ib"] == bytes(60), post_flags=hex(dead["flags"]), post_csum_ok=dead["csum_ok"])
            hx("J2 %s %s %s ino %d i_block before delete (p1)" % (k, fsname, fname, ino), truth["ib"])
            hx("J2 %s %s %s ino %d i_block after delete (p2)" % (k, fsname, fname, ino), dead["ib"])
            cs = inode_range_copy_report(k, p2, ino, fname)
            live_state = [c for c in cs if c["links"] > 0 and c["dtime"] == 0 and not c["revoked"]]
            stale_live = [c for c in live_state if c["txn"].region == "stale" and c["txn"].committed]
            good = None
            for c in stale_live:
                if c["gen"] != dead["gen"]:
                    continue
                cmap, cmeta = p1.img.mapping(c["ib"], c["flags"], home=p1.img)
                if c["size"] == truth["size"] and cmap == tmap and c["ib"] == truth["ib"]:
                    good = c
                    break
            partial = [c for c in stale_live if c["gen"] == dead["gen"]]
            if good is not None:
                hx("J2 %s %s %s stale copy slot (seq %d jblk %d)" % (k, fsname, fname, good["txn"].seq, good["tag"]["slot"]), good["raw"][:128])
            out[(fsname, fname)] = dict(found=good is not None, ino=ino, copy=good, copies=len(cs), live_state=len(live_state),
                                        stale_live=len(stale_live), same_gen=len(partial),
                                        csum_ok=(good["csum_ok"] if good is not None else None),
                                        depth=(u16(truth["ib"], 6) if truth["flags"] & 0x80000 else None))
            fact("J2.result", kernel=k, fs=fsname, file=fname, found=good is not None, live_state_copies=len(live_state),
                 stale_committed_live_copies=len(stale_live), same_generation=len(partial),
                 where=("seq%d/%s/jblk%d/%s" % (good["txn"].seq, good["txn"].region, good["tag"]["slot"], good["tag"]["copy"])) if good else "-",
                 map_equal=good is not None, size_equal=good is not None, gen_equal=good is not None)
    return out


def analyse_j4_j5_j6_j7(k, d, r, pay):
    st, inos = r["stages"], r["inos"]
    out = {}
    # ---- J4 ----
    c = st.get("ext4-p4")
    j4 = dict(ok=False)
    if c is not None:
        j = c.j
        fact("J4.state", kernel=k, needs_recovery=c.img.needs_recovery, s_start=j.start, s_sequence=j.sequence, live_txns=len(c.live),
             committed=sum(1 for t in c.live if t.committed), stop=c.live_stop)
        yino, lino, xino = inos.get(("ext4-p4", "y")), inos.get(("ext4-p1", "live")), inos.get(("ext4-p4", "x"))
        found = {}
        for label, ino in (("y", yino), ("live", lino), ("x", xino)):
            if ino is None:
                found[label] = None
                continue
            cs = [cc for cc in c.copies(ino) if cc["txn"].region == "live" and cc["txn"].committed]
            found[label] = [(cc["txn"].seq, cc["links"], cc["dtime"], cc["size"]) for cc in cs]
            fact("J4.copies", kernel=k, file=label, ino=ino, committed_live_copies=";".join("seq%d/links%d/dtime%d/size%d" % x for x in found[label]))
        ys = read_payload(pay, "y")
        live0 = read_payload(pay, "live")
        app = read_payload(pay, "app")
        y_in = found["y"] is not None and any(links > 0 and dt == 0 and size == len(ys) for (_, links, dt, size) in found["y"])
        live_in = found["live"] is not None and any(links > 0 and dt == 0 and size == len(live0) + len(app) for (_, links, dt, size) in found["live"])
        x_del = found["x"] is not None and any(dt != 0 or links == 0 for (_, links, dt, size) in found["x"])
        # as-found view versus the kernel's replay of a COPY (p5)
        asfound = run("debugfs", "-R", "ls -l /", c.path)[1]
        ydeb = parse_debugfs_stat(dbg(c.path, "stat <%d>" % yino)) if yino else {}
        fact("J4.as-found", kernel=k, y_in_home_inode=ydeb.get("links"), y_home_size=ydeb.get("size"), root_listing=repr(asfound.replace("\n", " | ")[:300]))
        rf = {}
        for fn, line in guest_lines(d, "RF "):
            p = line.split()
            rf[p[1]] = (int(p[2]), p[3])
        fact("J4.replay-listing", kernel=k, files=";".join("%s:%d:%s" % (n, s, h[:12]) for n, (s, h) in sorted(rf.items())))
        want_y = ("/mnt/y" in rf) and rf["/mnt/y"][0] == len(ys) and rf["/mnt/y"][1] == sha256(ys)
        want_live = ("/mnt/live" in rf) and rf["/mnt/live"][0] == len(live0) + len(app) and rf["/mnt/live"][1] == sha256(live0 + app)
        no_x = "/mnt/x" not in rf
        no_z = "/mnt/z" not in rf
        sha = {}
        for fn, line in guest_lines(d, "PROBE-SHA "):
            p = line.split()
            sha[p[1]] = p[2]
        fact("J4.replay", kernel=k, y_listed=want_y, live_appended_listed=want_live, x_absent=no_x, z_absent=no_z,
             p4_sha_before_replay=sha.get("p4-before"), p4_sha_after_replay=sha.get("p4-after"), p4_unchanged=(sha.get("p4-before") == sha.get("p4-after") and sha.get("p4-before") is not None))
        dm = [line for fn, line in guest_lines(d, "PROBE-DMESG ")]
        fact("J4.dmesg", kernel=k, lines=repr(dm[:6]))
        j4 = dict(ok=bool(c.img.needs_recovery and j.start != 0 and y_in and live_in and want_y and want_live),
                  needs_recovery=c.img.needs_recovery, start=j.start, y_in=y_in, live_in=live_in, x_del=x_del, want_y=want_y,
                  want_live=want_live, no_x=no_x, no_z=no_z, unchanged=(sha.get("p4-before") == sha.get("p4-after") and sha.get("p4-before") is not None))
        # J5: s_start and the home inode-table slot of x around the syncs
        j5 = {}
        for fn, line in guest_lines(d, "PROBE-J5 "):
            p = line.split()
            kv = dict(x.split("=", 1) for x in p[2:])
            j5[p[1]] = kv
        rows = []
        for lab in ("j0", "j1", "j2", "j3", "j4"):
            kv = j5.get(lab)
            if not kv:
                continue
            jsb = kv.get("jsb", "")
            seqv, startv = (int(jsb[0:8], 16), int(jsb[8:16], 16)) if len(jsb) >= 16 else (None, None)
            slot = kv.get("xslot", "-")
            xi = None
            if slot not in ("-", ""):
                raw = bytes.fromhex(slot)
                xi = (u16(raw, 0x1A), u32(raw, 0x14), u32(raw, 4))
            rows.append((lab, seqv, startv, xi))
            fact("J5.row", kernel=k, label=lab, disk_s_sequence=seqv, disk_s_start=startv, x_home_slot_links_dtime_size=xi)
        hx("J5 %s home slot of x after the second sync (O_DIRECT read of the disk)" % k, bytes.fromhex(j5["j2"]["xslot"])[:128] if "j2" in j5 and j5["j2"].get("xslot", "-") != "-" else b"")
        by = {a: (b, c_, dd) for a, b, c_, dd in rows}
        j5v = None
        if "j1" in by and "j2" in by and by["j2"][2] is not None:
            moved = by["j1"][1] != by["j2"][1]
            home_deleted = by["j2"][2][0] == 0 or by["j2"][2][1] != 0
            home_created = by["j1"][2] is not None and by["j1"][2][0] > 0
            j5v = dict(s_start_moved_by_second_sync=moved, home_deleted_after_sync=home_deleted, home_created_after_first_sync=home_created)
            fact("J5.result", kernel=k, **j5v)
        out["j5"] = j5v
        # ---- J6 / J7 are on other stages ----
    out["j4"] = j4
    # ---- J6: the revokes of the delete transaction (p2) against the leaves of deep ----
    p1, p2 = st.get("ext4-p1"), st.get("ext4-p2")
    j6 = None
    if p1 is not None and p2 is not None:
        dino = inos.get(("ext4-p1", "deep"))
        if dino is not None:
            ti = p1.img.inode(dino)
            tmap, tmeta = p1.img.mapping(ti["ib"], ti["flags"])
            maxseq1 = max([t.seq for t in p1.txns], default=0)
            new = [t for t in p2.txns if seq_gt(t.seq, maxseq1)]
            revs = set()
            for t in new:
                if t.committed:
                    revs |= set(t.revokes)
            leaves = set(tmeta)
            fact("J6.revokes", kernel=k, deep_ino=dino, deep_depth=u16(ti["ib"], 6), deep_leaf_blocks=sorted(leaves), new_txns_in_p2=len(new),
                 revoke_records_in_new_txns=sorted(revs)[:40], leaves_revoked=sorted(leaves & revs), leaves_not_revoked=sorted(leaves - revs))
            for t in new:
                if t.rev_blocks:
                    hx("J6 revoke block %s ext4-p2 seq %d jblk %d head 48" % (k, t.seq, t.rev_blocks[0]), p2.j.read(t.rev_blocks[0])[:48])
                    break
            j6 = bool(leaves) and leaves <= revs
            out["j6_detail"] = dict(leaves=len(leaves), revoked=len(leaves & revs))
    out["j6"] = j6
    # ext3: the single indirect block of bm
    e1, e2 = st.get("ext3-p1"), st.get("ext3-p2")
    if e1 is not None and e2 is not None:
        bino = inos.get(("ext3-p1", "bm"))
        if bino is not None:
            ti = e1.img.inode(bino)
            tmap, tmeta = e1.img.mapping(ti["ib"], ti["flags"])
            maxseq1 = max([t.seq for t in e1.txns], default=0)
            new = [t for t in e2.txns if seq_gt(t.seq, maxseq1)]
            revs = set()
            for t in new:
                if t.committed:
                    revs |= set(t.revokes)
            fact("J6.ext3", kernel=k, bm_ino=bino, indirect_blocks=sorted(tmeta), revoked=sorted(set(tmeta) & revs), revoke_records=sorted(revs)[:20])
    # ---- J7: same-boot depth-1 file ----
    p3 = st.get("ext4-p3")
    j7 = None
    if p3 is not None:
        sino = inos.get(("ext4-p3", "same"))
        if sino is not None:
            pl = read_payload(pay, "same")
            bs = p3.img.bs
            want = {}
            for kk in range((len(pl) + bs - 1) // bs):
                want[pl[kk * bs:(kk + 1) * bs].ljust(bs, b"\0")] = kk
            truth = {}
            for n in range(p3.img.first, p3.img.blocks):
                kk = want.get(p3.img.block(n))
                if kk is not None:
                    truth[kk] = n
            cs = [cc for cc in p3.copies(sino) if cc["txn"].committed and cc["links"] > 0 and cc["dtime"] == 0 and cc["size"] > 0]
            fact("J7.copies", kernel=k, same_ino=sino, live_state_committed_copies=";".join("seq%d/%s/depth%d/size%d%s" % (cc["txn"].seq, cc["txn"].region, u16(cc["ib"], 6) if cc["flags"] & 0x80000 else -1, cc["size"], "/revoked" if cc["revoked"] else "") for cc in cs[:12]),
                 content_truth_blocks=len(truth), expected_blocks=len(want))
            best = cs[0] if cs else None
            res_map = {}
            problems = []
            leaves_from = []
            if best is not None and best["flags"] & 0x80000:
                depth = u16(best["ib"], 6)
                ent = u16(best["ib"], 2)
                if depth == 1:
                    for i in range(min(ent, 4)):
                        e = best["ib"][12 + 12 * i:24 + 12 * i]
                        _, lo, hi, _ = struct.unpack("<IIHH", e)
                        leaf = (hi << 32) | lo
                        # newest copy of the leaf not newer than the inode copy (3G-R12)
                        lc = []
                        for t in p3.txns:
                            if not t.committed or not seq_ge(best["txn"].seq, t.seq):
                                continue
                            for tg in t.tags:
                                if tg["fs"] == leaf and tg["copy"] in ("verified", "unverified"):
                                    lc.append((t.seq, t.region == "live", t.start, tg, t))
                        lc.sort(key=lambda x: (x[0], x[1], x[2]), reverse=True)
                        if not lc:
                            problems.append("no journal copy of leaf %d" % leaf)
                            continue
                        data = p3.j.read(lc[0][3]["slot"])
                        leaves_from.append((leaf, lc[0][0], lc[0][3]["slot"], revoked_in(p3.txns, lc[0][0], leaf)))
                        magic, ent2, mx, dep2 = struct.unpack_from("<HHHH", data, 0)
                        if magic != 0xF30A or dep2 != 0:
                            problems.append("leaf %d copy is not a leaf (magic %#x depth %d)" % (leaf, magic, dep2))
                            continue
                        for q in range(min(ent2, (len(data) - 12) // 12)):
                            lb, ln, hi2, lo2 = struct.unpack("<IHHI", data[12 + 12 * q:24 + 12 * q])
                            for z in range(ln):
                                res_map[lb + z] = ((hi2 << 32) | lo2) + z
                    hx("J7 %s ext4-p3 inode copy root of same (seq %d)" % (k, best["txn"].seq), best["raw"][:128])
                else:
                    problems.append("newest live-state copy has depth %d" % depth)
            else:
                problems.append("no usable live-state copy")
            j7 = bool(res_map) and res_map == truth and not problems and len(truth) == len(want)
            fact("J7.result", kernel=k, journal_map_blocks=len(res_map), truth_blocks=len(truth), equal=res_map == truth, problems=problems,
                 leaves_from=";".join("leaf%d@seq%d/jblk%d/revoked%s" % x for x in leaves_from), depth=(u16(best["ib"], 6) if best is not None and best["flags"] & 0x80000 else None))
    out["j7"] = j7
    return out


def sha256(b):
    import hashlib
    return hashlib.sha256(b).hexdigest()


def analyse_j9(k, d, r):
    """How many transactions and blocks each stage logs, and whether a p1 copy survives p2 and p3."""
    st, inos = r["stages"], r["inos"]
    out = {}
    prev_max = 0
    for name in ("ext4-p1", "ext4-p2", "ext4-p3", "ext4-p4", "ext3-p1", "ext3-p2"):
        s = st.get(name)
        if s is None:
            continue
        fsn = name.split("-")[0]
        before = {"ext4-p1": 0, "ext3-p1": 0}.get(name)
        if name.endswith("p1"):
            prev_max = 0
        else:
            prev = st.get(fsn + "-p" + str(int(name[-1]) - 1))
            prev_max = max([t.seq for t in prev.txns], default=0) if prev is not None else 0
        new = [t for t in s.txns if seq_gt(t.seq, prev_max)]
        blocks_new = sum(len(t.desc) + len(t.rev_blocks) + (1 if t.commit >= 0 else 0) + len(t.tags) for t in new)
        lo = min([t.start for t in new], default=None)
        hi = max([max([t.start] + [tg["slot"] or 0 for tg in t.tags] + [t.commit]) for t in new], default=None)
        fact("J9.stage", kernel=k, image=name, new_txns=len(new), new_tags=tag_count(new), new_revoke_records=sum(len(t.revokes) for t in new),
             new_log_blocks=blocks_new, new_first_jblk=lo, new_last_jblk=hi, journal_blocks=s.j.maxlen,
             surviving_older_txns=len([t for t in s.txns if not seq_gt(t.seq, prev_max)]), max_seq=max([t.seq for t in s.txns], default=None))
        out[name] = dict(new=len(new), blocks=blocks_new)
    # a p1 copy survives p2 and p3: small (ext4) and bm (ext3) in the p1 live state
    surv = {}
    for fsn, fname in (("ext4", "small"), ("ext3", "bm")):
        ino = inos.get((fsn + "-p1", fname))
        if ino is None:
            continue
        p1 = st.get(fsn + "-p1")
        if p1 is None:
            continue
        truth = p1.img.inode(ino)
        p1_seqs = {t.seq for t in p1.txns}
        for later in ("p2", "p3"):
            s = st.get(fsn + "-" + later)
            if s is None:
                continue
            cs = [c for c in s.copies(ino) if c["links"] > 0 and c["dtime"] == 0 and c["txn"].seq in p1_seqs and c["size"] == truth["size"] and c["gen"] == truth["gen"] and c["txn"].committed and not c["revoked"]]
            surv[(fsn, later)] = len(cs)
            fact("J9.survival", kernel=k, fs=fsn, file=fname, image=fsn + "-" + later, live_state_p1_copies=len(cs),
                 jblks=",".join(str(c["tag"]["slot"]) for c in cs[:8]), seqs=",".join(str(c["txn"].seq) for c in cs[:8]))
    out["surv"] = surv
    return out


def signature(r):
    """The jbd2 format signature of a kernel: everything that is format, nothing that is data."""
    st = r["stages"]
    sig = {}
    for name, s in sorted(st.items()):
        j = s.j
        sig[name] = dict(version=j.btype, blocksize=j.blocksize, maxlen=j.maxlen, first=j.first, compat=j.fcompat, incompat=j.fincompat, ro=j.fro,
                         csum_type=j.csum_type, tag_size=j.tag_size, num_fc=j.num_fc, sb_ok=j.sb_csum_ok,
                         all_tag_ok=all(tg["copy"] != "mismatch" for t in s.live for tg in t.tags),
                         all_commit_ok=all(t.csum != "bad" for t in s.live))
    # what a stage happens to contain (block types, tag flag values) is data: compared as the union over all stages of a kernel
    alls = [t for s in st.values() for t in s.txns]
    sig["(all stages)"] = dict(block_types=sorted({1} if any(t.desc for t in alls) else set()) , tag_flag_values=sorted({tg["flags"] & ~F_LAST & ~F_SAME_UUID for t in alls for tg in t.tags}),
                              commit_csum_fields=sorted({(t.csum_fields[0], t.csum_fields[1]) for t in alls if t.csum_fields}))
    sig["(all stages)"]["block_types"] = sorted(set(sig["(all stages)"]["block_types"]) | ({2} if any(t.commit >= 0 for t in alls) else set()) | ({5} if any(t.rev_blocks for t in alls) else set()))
    return sig


def sha_text(path):
    import hashlib
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def main(argv):
    if len(argv) < 3 or argv[1] != "probe":
        print(__doc__)
        return 2
    args = argv[2:]
    pay = None
    kernels = []
    i = 0
    while i < len(args):
        if args[i] == "--payload":
            pay = args[i + 1]
            i += 2
        else:
            n, _, d = args[i].partition("=")
            kernels.append((n, d))
            i += 1
    results = {}
    for k, d in kernels:
        try:
            r = analyse_kernel(k, d, pay)
            r["j2"] = analyse_j2(k, d, r, pay)
            r["j45"] = analyse_j4_j5_j6_j7(k, d, r, pay)
            r["j9"] = analyse_j9(k, d, r)
            results[k] = r
        except Exception as e:
            import traceback
            traceback.print_exc()
            print("FACT PROBE-ERROR kernel=%s error=%r" % (k, e))
            DIFFS.append("analysis crashed for kernel %s" % k)
    # ---- gates ----
    stop_fail = False
    print("== gates")

    def per_kernel(f):
        return {k: f(r) for k, r in results.items()}

    # J1
    j1 = per_kernel(lambda r: bool(r["j1"]) and r["j1"]["equal"] and not r["j1"]["csum_bad"] and r["j1"]["sb_ok"] and r["j1"]["live_txns"] > 0)
    g = "holds" if j1 and all(j1.values()) and len(j1) == len(kernels) else ("fails" if not any(j1.values()) else "mixed")
    print("GATE-J1: %s per-kernel=%s features=%s tag_size=%s" % (g, j1, {k: r["j1"].get("features") for k, r in results.items()}, {k: r["j1"].get("tag_size") for k, r in results.items()}))
    stop_fail |= g != "holds"
    # J2
    def j2f(r):
        a, b = r["j2"].get(("ext4", "small")), r["j2"].get(("ext3", "bm"))
        return bool(a and a["found"] and b and b["found"])
    j2 = per_kernel(j2f)
    g = "holds" if j2 and all(j2.values()) and len(j2) == len(kernels) else ("fails" if not any(j2.values()) else "mixed")
    detail = {k: {f: (v["found"] if v else None) for f, v in ((("%s/%s" % key), val) for key, val in r["j2"].items() if isinstance(key, tuple))} for k, r in results.items()}
    print("GATE-J2: %s per-kernel=%s files=%s" % (g, j2, detail))
    stop_fail |= g != "holds"
    # J3
    j3 = {k: {n: (v["events"] > 0) for n, v in r["j3"].items()} for k, r in results.items()}
    anyyes = any(any(v.values()) for v in j3.values())
    print("GATE-J3: %s per-image=%s" % ("yes" if anyyes else "no", j3))
    # J4
    j4 = per_kernel(lambda r: r["j45"]["j4"].get("ok", False))
    g = "holds" if j4 and all(j4.values()) and len(j4) == len(kernels) else ("fails" if not any(j4.values()) else "mixed")
    print("GATE-J4: %s per-kernel=%s detail=%s" % (g, j4, {k: r["j45"]["j4"] for k, r in results.items()}))
    stop_fail |= g != "holds"
    # J5
    print("GATE-J5: %s detail=%s" % (
        "yes" if all((r["j45"].get("j5") or {}).get("home_deleted_after_sync") and not (r["j45"].get("j5") or {}).get("s_start_moved_by_second_sync") for r in results.values()) and results else "no",
        {k: r["j45"].get("j5") for k, r in results.items()}))
    # J6
    print("GATE-J6: %s detail=%s" % ("yes" if results and all(r["j45"].get("j6") for r in results.values()) else "no",
                                     {k: (r["j45"].get("j6"), r["j45"].get("j6_detail")) for k, r in results.items()}))
    # J7
    print("GATE-J7: %s detail=%s" % ("yes" if results and all(r["j45"].get("j7") for r in results.values()) else "no", {k: r["j45"].get("j7") for k, r in results.items()}))
    # J8
    j8 = {}
    for k, r in results.items():
        e = [s for n, s in r["stages"].items() if n.startswith("ext3-")]
        ok = bool(e) and all(s.j.tag_size == 8 and not s.j.csum23 and not s.j.f64 for s in e)
        j8[k] = dict(ok=ok, tag_size=sorted({s.j.tag_size for s in e}), features=sorted({"+".join(s.j.feature_names()) or "(none)" for s in e}))
    print("GATE-J8: %s detail=%s" % ("yes" if results and all(v["ok"] for v in j8.values()) else "no", j8))
    # J9
    j9 = {}
    for k, r in results.items():
        j9[k] = {"%s/%s" % kk: v for kk, v in r["j9"].get("surv", {}).items()}
    jsz = {k: sorted({r["stages"][n].j.maxlen for n in r["stages"]}) for k, r in results.items()}
    survived = results and all(v > 0 for r in results.values() for v in r["j9"].get("surv", {}).values()) and all(r["j9"].get("surv") for r in results.values())
    print("GATE-J9: %s journal_blocks=%s survival=%s" % ("keeps" if survived else "loses", jsz, j9))
    # J10
    sigs = {k: signature(r) for k, r in results.items()}
    ks = list(sigs)
    same = True
    diff = []
    if len(ks) == 2:
        a, b = sigs[ks[0]], sigs[ks[1]]
        for name in sorted(set(a) | set(b)):
            if name not in a or name not in b:
                continue
            for f in a[name]:
                if a[name][f] != b[name].get(f):
                    same = False
                    diff.append((name, f, a[name][f], b[name].get(f)))
    print("GATE-J10: %s kernels=%s differences=%s" % ("identical" if same and len(ks) == 2 else ("differs" if len(ks) == 2 else "unmeasured"), ks, diff))
    # J11
    j11 = {}
    for k, r in results.items():
        fc = any(s.j.fincompat & I_FC or s.img.fast_commit for s in r["stages"].values())
        asy = any(s.j.fincompat & I_ASYNC for s in r["stages"].values())
        j11[k] = dict(fast_commit=fc, async_commit=asy, features=sorted({"+".join(s.j.feature_names()) or "(none)" for s in r["stages"].values()}))
    print("GATE-J11: %s detail=%s" % ("clean" if results and not any(v["fast_commit"] or v["async_commit"] for v in j11.values()) else "present", j11))
    print("CROSSCHECK-SUMMARY differences=%d %s" % (len(DIFFS), DIFFS[:20]))
    if DIFFS:
        return 1
    return 2 if stop_fail else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

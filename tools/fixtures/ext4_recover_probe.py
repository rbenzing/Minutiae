#!/usr/bin/env python3
"""Host-side reader of the 3C ext4/ext3 recovery probe (recover_ext4_probe.sh).

It only READS finished disk images (never inside the guest, never with a kernel
mount) with its own raw ext4 reader: superblock, group descriptors, bitmaps,
inodes, extent trees, block maps, directory blocks and a table-driven crc32c of
its own. debugfs is used only to turn a path into an inode number.

  ext4_recover_probe.py pick <image> <dir path>       prints "<first> <middle>": two live names
                                                      of directory block 1
  ext4_recover_probe.py report <image dir> <payload dir>   prints FACT / HEX / GATE lines

Output grammar: "FACT <id> key=value ...", "HEX <label> <hex>", "GATE-X: <verdict> ...".
"""
import binascii
import os
import struct
import subprocess
import sys


def u16(b, o):
    return struct.unpack_from("<H", b, o)[0]


def u32(b, o):
    return struct.unpack_from("<I", b, o)[0]


# ---- crc32c (Castagnoli), raw register: no initial or final inversion; the callers pass the seed ----
TBL = []
for _i in range(256):
    _c = _i
    for _ in range(8):
        _c = (_c >> 1) ^ 0x82F63B78 if _c & 1 else _c >> 1
    TBL.append(_c)


def crc(seed, data):
    c = seed
    for b in data:
        c = TBL[(c ^ b) & 255] ^ (c >> 8)
    return c


# the standard check value: crc32c("123456789") = 0xE3069283 with init and final inversion
assert crc(0xFFFFFFFF, b"123456789") ^ 0xFFFFFFFF == 0xE3069283


def fact(fid, **kv):
    print("FACT %s %s" % (fid, " ".join("%s=%s" % (k, v) for k, v in kv.items())))


def hx(label, b):
    print("HEX %-52s %s" % (label, binascii.hexlify(bytes(b)).decode()))


def dbg(img, cmd):
    r = subprocess.run(["debugfs", "-R", cmd, img], capture_output=True, text=True)
    return r.stdout


def dirents(b):
    out, off = [], 0
    while off + 8 <= len(b):
        ino, rl, nl, ft = struct.unpack_from("<IHBB", b, off)
        if rl < 8 or rl % 4 or off + rl > len(b) or 8 + nl > rl:
            out.append(dict(off=off, bad=True, ino=ino, rl=rl, nl=nl, ft=ft, name=b""))
            break
        out.append(dict(off=off, bad=False, ino=ino, rl=rl, nl=nl, ft=ft, name=bytes(b[off + 8: off + 8 + nl])))
        off += rl
    return out


class Fs:
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
        self.free_blocks_sb = u32(sb, 0x0C)
        self.free_inodes_sb = u32(sb, 0x10)
        self.state = u16(sb, 0x3A)
        self.gd = [self._desc(g) for g in range(self.ngroups)]

    def _desc(self, g):
        o = (self.first + 1) * self.bs + g * self.dsz
        b = self.d[o:o + self.dsz]
        hi = self.dsz >= 64
        g32 = lambda lo, h: u32(b, lo) | ((u32(b, h) << 32) if hi else 0)
        return dict(
            bb=g32(0, 0x20), ib=g32(4, 0x24), it=g32(8, 0x28), flags=u16(b, 0x12),
            bbcsum=u16(b, 0x18) | ((u16(b, 0x38) << 16) if hi else 0),
            ibcsum=u16(b, 0x1A) | ((u16(b, 0x3A) << 16) if hi else 0),
            unused=u16(b, 0x1C) | ((u16(b, 0x32) << 16) if hi else 0),
            free_blocks=u16(b, 0xC) | ((u16(b, 0x2C) << 16) if hi else 0),
            free_inodes=u16(b, 0xE) | ((u16(b, 0x2E) << 16) if hi else 0),
            csum=u16(b, 0x1E), hi=hi)

    def block(self, n):
        return self.d[n * self.bs:(n + 1) * self.bs]

    # ---- bitmaps ----
    def block_bit(self, n):
        g, i = divmod(n - self.first, self.bpg)
        return (self.d[self.gd[g]["bb"] * self.bs + i // 8] >> (i % 8)) & 1

    def inode_bit(self, ino):
        g, i = divmod(ino - 1, self.ipg)
        return (self.d[self.gd[g]["ib"] * self.bs + i // 8] >> (i % 8)) & 1

    def free_set(self):
        s = set()
        for n in range(self.first, self.blocks):
            if not self.block_bit(n):
                s.add(n)
        return s

    def bitmap_csums(self, g):
        d = self.gd[g]
        ib = self.d[d["ib"] * self.bs: d["ib"] * self.bs + self.ipg // 8]
        bb = self.d[d["bb"] * self.bs: d["bb"] * self.bs + self.bpg // 8]
        return crc(self.seed, ib), crc(self.seed, bb)

    # ---- inodes ----
    def inode(self, ino):
        g, i = divmod(ino - 1, self.ipg)
        o = self.gd[g]["it"] * self.bs + i * self.isz
        raw = self.d[o:o + self.isz]
        return dict(
            ino=ino, raw=raw, mode=u16(raw, 0), size=u32(raw, 4) | (u32(raw, 0x6C) << 32), ctime=u32(raw, 0xC),
            mtime=u32(raw, 0x10), dtime=u32(raw, 0x14), links=u16(raw, 0x1A),
            blocks=u32(raw, 0x1C) | (u16(raw, 0x74) << 32), flags=u32(raw, 0x20), ib=raw[0x28:0x64],
            gen=u32(raw, 0x64), extra=u16(raw, 0x80) if self.isz > 128 else 0, csum_ok=self.inode_csum_ok(ino, raw))

    def inode_seed(self, ino, gen):
        return crc(crc(self.seed, struct.pack("<I", ino)), struct.pack("<I", gen))

    def inode_csum_ok(self, ino, raw):
        if not self.csum:
            return None
        has_hi = self.isz > 128 and u16(raw, 0x80) >= 4
        z = bytearray(raw)
        z[0x7C:0x7E] = b"\0\0"
        if has_hi:
            z[0x82:0x84] = b"\0\0"
        c = crc(self.inode_seed(ino, u32(raw, 0x64)), bytes(z))
        if has_hi:
            return c == (u16(raw, 0x7C) | (u16(raw, 0x82) << 16))
        return (c & 0xFFFF) == u16(raw, 0x7C)

    # ---- maps ----
    def extents(self, ib, depth_limit=4):
        """Extent tree from a 60-byte i_block: dict(root, extents[(lblk,len,pblk,uninit)], nodes[(level,pblk)], problems)."""
        res = dict(root=struct.unpack_from("<HHHH", ib, 0), extents=[], nodes=[], problems=[])

        def rec(data, level):
            magic, ent, _, depth = struct.unpack_from("<HHHH", data, 0)
            if magic != 0xF30A:
                res["problems"].append("bad-magic@L%d" % level)
                return
            ent = min(ent, (len(data) - 12) // 12)
            for i in range(ent):
                e = data[12 + 12 * i: 24 + 12 * i]
                if depth == 0:
                    lb, ln, hi, lo = struct.unpack("<IHHI", e)
                    un = ln > 32768
                    res["extents"].append((lb, ln - 32768 if un else ln, (hi << 32) | lo, un))
                else:
                    _, lo, hi, _ = struct.unpack("<IIHH", e)
                    blk = (hi << 32) | lo
                    res["nodes"].append((level + 1, blk))
                    if level < depth_limit and 0 < blk < self.blocks:
                        rec(self.block(blk), level + 1)

        rec(ib, 0)
        return res

    def blockmap(self, ib):
        """(data blocks in logical order, single-indirect block number) of a 12+1 block map (no double indirect)."""
        ptr = struct.unpack("<15I", ib)
        data = [p for p in ptr[:12]]
        ind = ptr[12]
        if ind:
            data += list(struct.unpack("<%dI" % (self.bs // 4), self.block(ind)))
        return data, ind

    def lblk_map(self, inode):
        m = {}
        if inode["flags"] & 0x80000:
            for lb, ln, pb, un in self.extents(inode["ib"])["extents"]:
                for k in range(ln):
                    m[lb + k] = pb + k
        else:
            data, _ = self.blockmap(inode["ib"])
            for k, p in enumerate(data):
                if p:
                    m[k] = p
        return m

    def leaf_tail(self, ino, gen, blk):
        """(entries, max, depth, stored tail, computed tail) of a block read as an extent leaf, or None."""
        b = self.block(blk)
        magic, ent, mx, depth = struct.unpack_from("<HHHH", b, 0)
        if magic != 0xF30A or 12 + 12 * mx + 4 > len(b):
            return None
        t = 12 + 12 * mx
        return ent, mx, depth, u32(b, t), crc(self.inode_seed(ino, gen), b[:t])


def ino_of(img, path):
    for line in dbg(img, "stat " + path).splitlines():
        if line.startswith("Inode:"):
            return int(line.split()[1])
    raise SystemExit("no inode for %s in %s" % (path, img))


def dir_block(fs, dino, k):
    ino = fs.inode(dino)
    if ino["flags"] & 0x10000000:
        raise SystemExit("directory %d is inline" % dino)
    return fs.lblk_map(ino)[k]


def pick(img, dpath):
    fs = Fs(img)
    dino = ino_of(img, dpath)
    recs = [r for r in dirents(fs.block(dir_block(fs, dino, 1))) if not r["bad"] and r["ino"]]
    if len(recs) < 4:
        raise SystemExit("directory block 1 holds fewer than 4 live records")
    print(recs[0]["name"].decode(), recs[len(recs) // 2]["name"].decode())


# ------------------------------------------------------------------------------------------------


def rng(blocks):
    s = sorted(blocks)
    if not s:
        return "[]"
    runs, a, p = [], s[0], s[0]
    for c in s[1:]:
        if c != p + 1:
            runs.append((a, p))
            a = c
        p = c
    runs.append((a, p))
    return ",".join("%d-%d" % r if r[0] != r[1] else str(r[0]) for r in runs)


def hdr(ib):
    return "magic=%#06x entries=%d max=%d depth=%d" % struct.unpack_from("<HHHH", ib, 0)


def e12(name, fs):
    """E12 and the superblock/descriptor facts of one image."""
    fact("E12.sb", image=name, state=fs.state, needs_recovery=bool(fs.incompat & 4), csum=fs.csum, desc_size=fs.dsz,
         blocks=fs.blocks, inodes=fs.inodes, bpg=fs.bpg, ipg=fs.ipg, inode_size=fs.isz, free_blocks_sb=fs.free_blocks_sb,
         free_inodes_sb=fs.free_inodes_sb)
    for g, d in enumerate(fs.gd):
        row = dict(image=name, group=g, flags=hex(d["flags"]), itable_unused=d["unused"], free_inodes_gd=d["free_inodes"],
                   free_blocks_gd=d["free_blocks"])
        if fs.csum:
            ic, bc = fs.bitmap_csums(g)
            hi = d["hi"]
            row.update(ibcsum_stored=hex(d["ibcsum"]), ibcsum_computed=hex(ic if hi else ic & 0xFFFF),
                       ibcsum_ok=(d["ibcsum"] == (ic if hi else ic & 0xFFFF)), hi_branch=hi,
                       bbcsum_stored=hex(d["bbcsum"]), bbcsum_computed=hex(bc if hi else bc & 0xFFFF),
                       bbcsum_ok=(d["bbcsum"] == (bc if hi else bc & 0xFFFF)))
        fact("E12.group", **row)


def generic(tag, fs_pre, fs_post, ino, nblocks_hint=None):
    """Prints the common before/after facts of one inode and returns (pre, post)."""
    a, b = fs_pre.inode(ino), fs_post.inode(ino)
    fact(tag + ".inode", ino=ino, mode_before=oct(a["mode"]), mode_after=oct(b["mode"]), i_size_before=a["size"], i_size_after=b["size"],
         i_blocks_before=a["blocks"], i_blocks_after=b["blocks"], links_before=a["links"], links_after=b["links"],
         dtime_before=a["dtime"], dtime_after=b["dtime"], gen_before=a["gen"], gen_after=b["gen"],
         flags_before=hex(a["flags"]), flags_after=hex(b["flags"]), inode_csum_ok_before=a["csum_ok"], inode_csum_ok_after=b["csum_ok"],
         inode_bit_before=fs_pre.inode_bit(ino), inode_bit_after=fs_post.inode_bit(ino))
    hx(tag + " i_block before", a["ib"])
    hx(tag + " i_block after", b["ib"])
    return a, b


def extent_file(tag, fs_pre, fs_post, ino, pay=None):
    """E1/E2/E8 style facts of an extent-mapped file; returns (pre, post, intact map?, pre tree)."""
    a, b = generic(tag, fs_pre, fs_post, ino)
    ta, tb = fs_pre.extents(a["ib"]), fs_post.extents(b["ib"])
    fact(tag + ".tree", root_before=hdr(a["ib"]), root_after=hdr(b["ib"]), extents_before=ta["extents"], extents_after=tb["extents"],
         nodes_before=[n[1] for n in ta["nodes"]], nodes_after=[n[1] for n in tb["nodes"]], problems_before=ta["problems"],
         problems_after=tb["problems"])
    ent_a = struct.unpack_from("<H", a["ib"], 2)[0]
    intact = (struct.unpack_from("<H", b["ib"], 2)[0] == ent_a and ent_a > 0
              and b["ib"][12:12 + 12 * ent_a] == a["ib"][12:12 + 12 * ent_a])
    data_blocks = [pb + k for lb, ln, pb, un in ta["extents"] for k in range(ln)]
    same = all(fs_pre.block(n) == fs_post.block(n) for n in data_blocks)
    freed = [fs_post.block_bit(n) == 0 for n in data_blocks]
    nodes_freed = [fs_post.block_bit(n[1]) == 0 for n in ta["nodes"]]
    fact(tag + ".data", blocks=rng(data_blocks), bits_before=[fs_pre.block_bit(n) for n in data_blocks][:16],
         all_bits_before_set=all(fs_pre.block_bit(n) for n in data_blocks), all_freed_after=all(freed),
         bytes_unchanged=same, tree_blocks_freed_after=nodes_freed)
    if pay is not None:
        want = pay
        got = b"".join(fs_pre.block(pb + k) for lb, ln, pb, un in sorted(ta["extents"]) for k in range(ln))
        fact(tag + ".payload", pre_delete_data_equals_payload=(got[:len(want)] == want))
    return a, b, intact, ta


def leaf_state(fs_pre, fs_post, ino, a, ta):
    """E2: per leaf block, before/after identity, tail checksum under (inode, generation), bitmap bits."""
    ok = []
    for level, blk in ta["nodes"]:
        pre_b, post_b = fs_pre.block(blk), fs_post.block(blk)
        t_pre = fs_pre.leaf_tail(ino, a["gen"], blk)
        t_post = fs_post.leaf_tail(ino, fs_post.inode(ino)["gen"], blk)
        eq = pre_b == post_b
        csum_pre = bool(t_pre) and t_pre[3] == t_pre[4]
        csum_post = bool(t_post) and t_post[3] == t_post[4]
        fact("E2.leaf", block=blk, level=level, bit_before=fs_pre.block_bit(blk), bit_after=fs_post.block_bit(blk),
             byte_equal_to_before=eq, entries_before=t_pre[0] if t_pre else None, entries_after=t_post[0] if t_post else None,
             tail_ok_before=csum_pre, tail_ok_after=csum_post, tail_stored_after=hex(t_post[3]) if t_post else None,
             tail_computed_after=hex(t_post[4]) if t_post else None)
        hx("E2 leaf %d before" % blk, pre_b)
        hx("E2 leaf %d after" % blk, post_b)
        ok.append(eq and csum_post and fs_post.block_bit(blk) == 0 and fs_post.inode(ino)["gen"] == a["gen"])
    return ok


def scan_bound_leaves(fs, ino, gen):
    """Every block whose extent-leaf tail checksum verifies under (ino, gen): the leaf method's own index, by brute force."""
    found = []
    cap = (fs.bs - 12) // 12
    for n in range(fs.first, fs.blocks):
        b = fs.block(n)
        if b[0:2] != b"\x0a\xf3":
            continue
        _, ent, mx, depth = struct.unpack_from("<HHHH", b, 0)
        if depth != 0 or mx != cap or not 1 <= ent <= mx:
            continue
        t = 12 + 12 * mx
        if crc(fs.inode_seed(ino, gen), b[:t]) == u32(b, t):
            found.append(n)
    return found


def e4(tag, pre, post, dino, first, mid):
    for role, name in (("first-of-block-1", first), ("non-first", mid)):
        nm = name.encode()
        lm = pre.lblk_map(pre.inode(dino))
        where = None
        for lb in sorted(lm):
            recs = dirents(pre.block(lm[lb]))
            for i, r in enumerate(recs):
                if r["name"] == nm and r["ino"]:
                    where = (lb, lm[lb], i, recs)
        if where is None:
            fact(tag, role=role, name=name, found_before=False)
            continue
        lb, pb, i, recs = where
        r = recs[i]
        post_recs = dirents(post.block(pb))
        live_after = [x for x in post_recs if x["name"] == nm and x["ino"]]
        b = post.block(pb)
        info = dict(role=role, name=name, lblk=lb, pblk=pb, index_in_block=i, rec_off=r["off"], inode_before=r["ino"],
                    rec_len_before=r["rl"], live_after=bool(live_after), inode_field_after=u32(b, r["off"]),
                    name_len_field_after=b[r["off"] + 6], name_bytes_after_equal=(b[r["off"] + 8:r["off"] + 8 + r["nl"]] == nm))
        if i > 0:
            p = recs[i - 1]
            pa = [x for x in post_recs if x["off"] == p["off"]]
            info.update(pred_off=p["off"], pred_rec_len_before=p["rl"], pred_rec_len_after=pa[0]["rl"] if pa else None,
                        record_merged_into_predecessor=bool(pa) and pa[0]["rl"] == p["rl"] + r["rl"],
                        record_rec_len_field_after=u16(b, r["off"] + 4))
        else:
            info.update(rec_len_after=post_recs[0]["rl"] if post_recs else None)
        fact(tag, **info)
        hx("%s %s record before" % (tag, role), pre.block(pb)[r["off"]:r["off"] + 8 + r["nl"]])
        hx("%s %s record after" % (tag, role), b[r["off"]:r["off"] + 8 + r["nl"]])
    return None


def report(d, pdir):
    pay = {n: open(os.path.join(pdir, n), "rb").read() for n in os.listdir(pdir)}
    img = lambda n: os.path.join(d, n + ".img")
    picks = {}
    for k in ("ext4", "ext3"):
        picks[k] = open(os.path.join(d, k + ".picks")).read().split()
    gates = {}
    print("######## E12 and E9 inputs: superblock and descriptors of every image")
    for n in ("ext4-s0", "ext4-s1w", "ext4-s2f", "ext4-s3d", "ext4-rs", "ext4-rp", "ext4-sb", "ext3-s0", "ext3-s1w", "ext3-s2d"):
        e12(n, Fs(img(n)))

    # ---------------- ext4 ----------------
    pre, post = Fs(img("ext4-s2f")), Fs(img("ext4-s3d"))
    print("######## ext4: pre-delete image ext4-s2f, post-delete image ext4-s3d")
    names = {n: ino_of(img("ext4-s2f"), "/" + n) for n in ("e1", "e5", "e5b", "e8", "deep", "d")}
    fact("names", **names)
    K, S, L = [], [], []

    print("######## E1 small extent file (3 blocks)")
    a, b, intact, ta = extent_file("E1", pre, post, names["e1"], pay["e1"])
    K.append(("ext4:e1", intact))
    S.append(("ext4:e1", b["size"] == 0))
    fact("E1.cross", debugfs_ex_pre=" ".join(dbg(img("ext4-s2f"), "ex <%d>" % names["e1"]).split()))

    print("######## E8 sparse file")
    a, b, intact, ta = extent_file("E8", pre, post, names["e8"])
    K.append(("ext4:e8", intact))
    S.append(("ext4:e8", b["size"] == 0))

    print("######## E2 depth-1 file")
    a, b, intact, ta = extent_file("E2", pre, post, names["deep"], pay["deep"])
    K.append(("ext4:deep", intact))
    S.append(("ext4:deep", b["size"] == 0))
    depth = struct.unpack_from("<H", a["ib"], 6)[0]
    leaves = [n[1] for n in ta["nodes"] if n[0] == 1]
    fact("E2.workload", root_depth=depth, extents=len(ta["extents"]), leaf_blocks=leaves, ok=depth == 1 and len(leaves) >= 1, leaves_ge_2=len(leaves) >= 2)
    L = leaf_state(pre, post, names["deep"], a, ta)
    bound_all = scan_bound_leaves(pre, names["deep"], a["gen"])
    bound_pre = [n for n in bound_all if pre.block_bit(n) == 0]
    fact("E2.leaf-scan", all_bound_blocks_pre=len(bound_all), allocated_bound_blocks_pre=len(bound_all) - len(bound_pre),
         free_bound_leaves_in_post_image=[n for n in scan_bound_leaves(post, names["deep"], a["gen"]) if post.block_bit(n) == 0], free_bound_leaves_in_pre_image=bound_pre,
         bound_but_not_in_tree=[n for n in bound_pre if n not in leaves])
    for n in bound_pre:
        if n not in leaves:
            t = pre.leaf_tail(names["deep"], a["gen"], n)
            fact("E2.superseded-leaf", block=n, bit_pre=pre.block_bit(n), bit_post=post.block_bit(n), entries=t[0],
                 byte_equal_pre_post=pre.block(n) == post.block(n))
            hx("E2 bound leaf outside the final tree %d" % n, pre.block(n))
    gates["L-clean"] = (depth == 1 and len(leaves) >= 1, L)

    print("######## E5 inline data")
    for nm in ("e5", "e5b"):
        a, b = generic("E5." + nm, pre, post, names[nm])
        k = 0x80 + a["extra"]
        fact("E5." + nm + ".inline", inline_flag_before=bool(a["flags"] & 0x10000000), inline_flag_after=bool(b["flags"] & 0x10000000),
             ibody_ea_before=a["raw"][k:k + 4].hex(), ibody_ea_after=b["raw"][k:k + 4].hex())
        hx("E5 %s ibody after extra_isize, before" % nm, a["raw"][k:])
        hx("E5 %s ibody after extra_isize, after" % nm, b["raw"][k:])
        # inline data lives in the inode, not in a map: reported, kept out of the K and S gates
        fact("E5." + nm + ".gates", i_block_kept=a["ib"] == b["ib"], i_size_zeroed=b["size"] == 0)

    print("######## E4 directory records (ext4, htree directory)")
    e4("E4.ext4", pre, post, names["d"], *picks["ext4"])

    print("######## E6/E11 inode reuse, E7 full volume")
    freed_ino = sorted(i for i in range(1, pre.inodes + 1) if pre.inode_bit(i) and not post.inode_bit(i))
    fact("E6.freed", count=len(freed_ino), inodes=freed_ino)
    fb = {n for n in range(pre.first, pre.blocks) if pre.block_bit(n) and not post.block_bit(n)}
    fs_free = [pre.free_set(), post.free_set()]
    fact("E7.free", blocks_free_s1w=len(Fs(img("ext4-s1w")).free_set()), blocks_free_s2f=len(fs_free[0]), blocks_free_s3d=len(fs_free[1]),
         sb_free_s2f=pre.free_blocks_sb, sb_free_s3d=post.free_blocks_sb,
         freed_by_delete=len(fb), s3d_free_equals_freed_by_delete=(fs_free[1] == fb))
    U = []
    for br, clock in (("ext4-rs", "same clock as the delete boot"), ("ext4-rp", "delete boot + 1 h")):
        f = Fs(img(br))
        got = {}
        for nm in ("r1", "r2", "r3", "nx"):
            ino = ino_of(img(br), "/" + nm)
            n = f.inode(ino)
            old = pre.inode(ino)
            got[nm] = (ino, n["gen"], old["gen"])
            fact("E6." + br + "." + nm, ino=ino, took_freed_inode=ino in freed_ino, gen_new=n["gen"], gen_in_pre_image=old["gen"],
                 gen_differs=n["gen"] != old["gen"], dtime_new=n["dtime"], links_new=n["links"], csum_ok=n["csum_ok"])
            U.append(ino in freed_ino)
        nx = f.inode(got["nx"][0])
        nxb = sorted(pb for lb, ln, pb, un in f.extents(nx["ib"])["extents"] for pb in range(pb, pb + ln))
        fact("E7." + br, next_file_blocks=rng(nxb), next_file_blocks_all_in_target_set=all(x in fb for x in nxb),
             deep_blocks_reused=len(set(nxb) & {pb + k for lb, ln, pb, un in ta["extents"] for k in range(ln)}))
        fact("E6." + br + ".lowest", freed_lowest4=freed_ino[:4], new_inodes=[got[n][0] for n in ("r1", "r2", "r3", "nx")], clock=clock)

    print("######## same-boot delete (cp, sync, rm, sync in ONE boot): the leaf scan over the free blocks")
    sbf = Fs(img("ext4-sb"))
    sb_ino = int(open(os.path.join(d, "ext4-sb.ino")).read().split()[0])
    di = sbf.inode(sb_ino)
    fact("SB.inode", ino=sb_ino, gen=di["gen"], links=di["links"], dtime=di["dtime"], i_size=di["size"], inode_bit=sbf.inode_bit(sb_ino),
         csum_ok=di["csum_ok"], root=hdr(di["ib"]))
    hx("SB i_block after", di["ib"])
    bound_all = scan_bound_leaves(sbf, sb_ino, di["gen"])
    bound = [n for n in bound_all if sbf.block_bit(n) == 0]
    ents, okd = [], []
    for n in bound:
        bl = sbf.block(n)
        _, ent, _, _ = struct.unpack_from("<HHHH", bl, 0)
        for i in range(ent):
            lb, ln, hi, lo = struct.unpack_from("<IHHI", bl, 12 + 12 * i)
            ents.append((lb, ln, (hi << 32) | lo))
    ents.sort()
    cover_l = sorted(lb + k for lb, ln, pb in ents for k in range(ln))
    cover = sorted(set(cover_l))
    want = pay["deep2"]
    okd = all(sbf.block(pb + k) == want[(lb + k) * sbf.bs:(lb + k + 1) * sbf.bs].ljust(sbf.bs, b"\0") for lb, ln, pb in ents for k in range(ln))
    nb = (len(want) + sbf.bs - 1) // sbf.bs
    fact("SB.leaf-scan", all_bound_blocks=len(bound_all), free_bound_leaves=bound, entries=len(ents), no_lblk_twice=len(cover_l) == len(cover),
         covers_lblk_0_to_n=cover == list(range(nb)), file_blocks=nb, data_at_entries_equals_payload=okd)
    for n in bound:
        hx("SB leaf %d" % n, sbf.block(n))
    gates["L-sb"] = (len(bound) >= 1 and cover == list(range(nb)) and len(cover_l) == len(cover) and okd, bound)

    # ---------------- ext3 ----------------
    p3, q3 = Fs(img("ext3-s1w")), Fs(img("ext3-s2d"))
    print("######## ext3: pre-delete image ext3-s1w, post-delete image ext3-s2d")
    n3 = {n: ino_of(img("ext3-s1w"), "/" + n) for n in ("m20", "m3", "d")}
    fact("names.ext3", **n3)
    for nm in ("m20", "m3"):
        a, b = generic("E3." + nm, p3, q3, n3[nm])
        pa, ia = p3.blockmap(a["ib"])
        pb_, ib_ = q3.blockmap(b["ib"]) if b["ib"] else ([], 0)
        fact("E3." + nm + ".map", pointers_before=list(struct.unpack("<15I", a["ib"])), pointers_after=list(struct.unpack("<15I", b["ib"])),
             indirect_block=ia, indirect_bytes_equal_before_after=(ia != 0 and p3.block(ia) == q3.block(ia)),
             data_bytes_unchanged=all(p3.block(x) == q3.block(x) for x in pa if x), data_blocks_freed=all(q3.block_bit(x) == 0 for x in pa if x))
        if ia:
            hx("E3 %s indirect block before (first 96 bytes)" % nm, p3.block(ia)[:96])
            hx("E3 %s indirect block after (first 96 bytes)" % nm, q3.block(ia)[:96])
        K.append(("ext3:" + nm, list(struct.unpack("<15I", b["ib"])) == list(struct.unpack("<15I", a["ib"]))))
        S.append(("ext3:" + nm, b["size"] == 0))
    print("######## E4 directory records (ext3, linear directory)")
    e4("E4.ext3", p3, q3, n3["d"], *picks["ext3"])

    # ---------------- gates ----------------
    print("######## GATE INPUTS")
    okc, Lc = gates["L-clean"]
    oks, Ls = gates["L-sb"]
    fact("GATE-L.inputs", workload_depth1=okc, clean_unmount_leaves_intact=Lc, same_boot_leaf_found_and_valid=oks)
    if not okc:
        lv = "undecidable (the E2 workload did not produce a depth-1 file)"
    elif all(Lc) and oks:
        lv = "open"
    elif not any(Lc) and not oks:
        lv = "closed"
    else:
        lv = "mixed"
    print("GATE-L: %s (clean boundary: %d of %d leaf blocks byte-equal to before, checksum valid under (inode, generation), free, generation unchanged; same-boot delete: %s)" % (
        lv, sum(Lc), len(Lc), "bound valid leaf found" if oks else "no valid bound leaf"))
    ki = [v for _, v in K]
    kv = "open" if all(ki) else "closed" if not any(ki) else "mixed"
    print("GATE-K: %s (map left intact after a kernel rm: %s)" % (kv, ", ".join("%s=%s" % (n, "intact" if v else "cleared") for n, v in K)))
    si = [v for _, v in S]
    sv = "zeroed" if all(si) else "kept" if not any(si) else "mixed"
    print("GATE-S: %s (i_size after a kernel rm: %s)" % (sv, ", ".join("%s=%s" % (n, "zeroed" if v else "kept") for n, v in S)))
    uv = "reuse" if all(U) else "no-reuse" if not any(U) else "mixed"
    print("GATE-U: %s (new files took a freed inode number: %s)" % (uv, U))


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "pick":
        pick(sys.argv[2], sys.argv[3])
    elif len(sys.argv) == 4 and sys.argv[1] == "report":
        report(sys.argv[2], sys.argv[3])
    else:
        raise SystemExit(__doc__)

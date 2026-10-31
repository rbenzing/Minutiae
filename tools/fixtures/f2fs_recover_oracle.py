#!/usr/bin/env python3
"""Independent F2FS decoder for the 3D recovery fixtures (never Minutiae).

Reads raw F2FS images and the captures of the f2fs tools; written from the kernel
header layout and cross-checked against dump.f2fs where both report the same field
(any difference exits 1). Layout facts taken from memory of the kernel headers are
marked "from memory" below; the probe confirms or refutes each of them (README.md,
"F2FS recovery probe (3D)").

usage: f2fs_recover_oracle.py probe --images <dir> --tools <dir>
  <dir> of images holds p0..p5.img (small workload, nodiscard), d1, d3.img (small, discard),
  l1, l2, l3, l5.img (large workload, nodiscard) and m1, m3.img (large, discard);
  <dir> of tools holds one folder per image with the captures of
  fsck.f2fs -f --dry-run / -t, dump.f2fs -d 1 -s0~-1 / -n0~-1 / -a0~-1.
  Prints one "FACT Q<n> ..." line per fact (raw bytes as "HEX" lines) and "GATE G<n>"
  verdicts (Task 1). Its decoder functions (superblock, checkpoint, nat_entry, sit_entry, ssa_entry, node) are reused below.

usage: f2fs_recover_oracle.py snap --image <img> --tools <dir> --stage <name> --expected <dir> --out <json>
  After a write stage: for every path the inode number (decoder == fsck.f2fs -t), the NAT address
  (== dump.f2fs -n), the data extents in file order (decoder walk of i_addr/i_nid through the NAT ==
  fsck.f2fs -d 1 -M), the holes, the node blocks and the bytes of the inode block. <dir> of --tools holds
  one folder per stage (fsck-f.txt/.rc, tree.txt, map.txt, dump-i.txt, dump_nat, dump_sit); --expected is
  the tree of the bytes the generator told the guest to write (the content of every file is checked
  against it, never against Minutiae).

usage: f2fs_recover_oracle.py final --image <img> --tools <dir> --stage <name> --expected <dir>
         --variant nodiscard|discard --snaps <json>... [--report-unlisted <family>...] --out <json>
  After the delete stage: the oracle JSON (README.md, "F2FS recovery fixtures (3D)"): free blocks (SIT
  popcount checked against valid_block_count), live files, every deleted dentry decoded from the dentry
  areas (the live ones checked against fsck.f2fs -t), and for each deleted dentry the expectation of the spec
  rules (stale inode by structure, map to the first hole, captured = the leading free part, confidence)
  computed from the finished image. Exits 1 with "family <name> missing: ...; re-rule, do not edit the ops"
  when a required family is absent or has the wrong shape.
"""
import argparse
import binascii
import builtins
import hashlib
import json
import mmap
import os
import re
import struct
import sys
import zlib

BLK = 4096
BPS = 512  # blocks per segment (log_blocks_per_seg 9)
MAGIC = 0xF2F52010
NULL_ADDR = 0
NEW_ADDR = 0xFFFFFFFF
NAT_PER_BLK = 455  # 4096 // 9 (from memory: struct f2fs_nat_entry is 9 bytes packed)
SIT_PER_BLK = 55  # 4096 // 74 (from memory: struct f2fs_sit_entry is 74 bytes)
ADDRS_PER_NODE = 1018  # from memory: (4096 - 24 footer) / 4
INLINE_XATTR_ADDRS = 50  # from memory: DEFAULT_INLINE_XATTR_ADDRS
DEF_ADDRS_PER_INODE = 923  # from memory
CP_CRC_RECOVERY_FLAG = 0x40  # from memory
CP_COMPACT_SUM_FLAG = 0x4  # from memory
CP_LARGE_NAT_BITMAP_FLAG = 0x400  # from memory
SUM_TYPE_NODE = 1  # from memory (SUM_TYPE_DATA is 0)
NODE_SEG_TYPES = (3, 4, 5)  # from memory: HOT_NODE, WARM_NODE, COLD_NODE


def fail(msg):
    sys.stderr.write("f2fs_recover_oracle: %s\n" % msg)
    sys.exit(1)


def raw_crc32(seed, data):
    """The kernel's crc32_le: standard CRC-32 without the initial/final inversion."""
    return (~zlib.crc32(data, ~seed & 0xFFFFFFFF)) & 0xFFFFFFFF


def u16(b, o):
    return struct.unpack_from("<H", b, o)[0]


def u32(b, o):
    return struct.unpack_from("<I", b, o)[0]


def u64(b, o):
    return struct.unpack_from("<Q", b, o)[0]


def bit_msb(bm, nr):
    """SIT valid map and NAT/SIT version bitmaps: most significant bit first (from memory)."""
    return (bm[nr >> 3] >> (7 - (nr & 7))) & 1


def bit_le(bm, nr):
    """Inline dentry bitmap: little-endian bit order (from memory: test_bit_le)."""
    return (bm[nr >> 3] >> (nr & 7)) & 1


def hx(b):
    return binascii.hexlify(bytes(b)).decode()


class Image:
    def __init__(self, path):
        self.path = path
        self.f = open(path, "rb")
        self.m = mmap.mmap(self.f.fileno(), 0, access=mmap.ACCESS_READ)
        self.nblocks = len(self.m) // BLK
        self.sb = superblock(self)
        self.cp = checkpoint(self)
        self.journals = None

    def blk(self, n):
        if n < 0 or n >= self.nblocks:
            fail("%s: block %d is outside the image (%d blocks)" % (self.path, n, self.nblocks))
        return bytes(self.m[n * BLK:(n + 1) * BLK])

    def close(self):
        self.m.close()
        self.f.close()


# ---------------------------------------------------------------- superblock, checkpoint


def superblock(img):
    raw = img.blk(0)[1024:1024 + 3072]
    if u32(raw, 0) != MAGIC:
        fail("%s: bad superblock magic" % img.path)
    sb = {
        "major_ver": u16(raw, 4), "minor_ver": u16(raw, 6), "log_sectorsize": u32(raw, 8),
        "log_sectors_per_block": u32(raw, 12), "log_blocksize": u32(raw, 16),
        "log_blocks_per_seg": u32(raw, 20), "segs_per_sec": u32(raw, 24), "secs_per_zone": u32(raw, 28),
        "checksum_offset": u32(raw, 32), "block_count": u64(raw, 36), "section_count": u32(raw, 44),
        "segment_count": u32(raw, 48), "segment_count_ckpt": u32(raw, 52), "segment_count_sit": u32(raw, 56),
        "segment_count_nat": u32(raw, 60), "segment_count_ssa": u32(raw, 64), "segment_count_main": u32(raw, 68),
        "segment0_blkaddr": u32(raw, 72), "cp_blkaddr": u32(raw, 76), "sit_blkaddr": u32(raw, 80),
        "nat_blkaddr": u32(raw, 84), "ssa_blkaddr": u32(raw, 88), "main_blkaddr": u32(raw, 92),
        "root_ino": u32(raw, 96), "node_ino": u32(raw, 100), "meta_ino": u32(raw, 104),
        "cp_payload": u32(raw, 1664), "features": u32(raw, 2180),
    }
    if sb["log_blocksize"] != 12 or sb["log_blocks_per_seg"] != 9 or sb["segs_per_sec"] != 1:
        fail("%s: unsupported geometry %r" % (img.path, sb))
    if sb["cp_payload"] != 0:
        fail("%s: cp_payload %d is not supported (the bitmaps would sit in later checkpoint blocks)" % (img.path, sb["cp_payload"]))
    if sb["features"] & 0x800:
        off = sb["checksum_offset"]
        if off > len(raw) - 4 or u32(raw, off) != raw_crc32(MAGIC, raw[:off]):
            fail("%s: superblock checksum is wrong" % img.path)
    return sb


CP_FIELD_OFFSETS = [  # name, offset, width
    ("checkpoint_ver", 0, 8), ("user_block_count", 8, 8), ("valid_block_count", 16, 8),
    ("rsvd_segment_count", 24, 4), ("overprov_segment_count", 28, 4), ("free_segment_count", 32, 4),
    ("ckpt_flags", 132, 4), ("cp_pack_total_block_count", 136, 4), ("cp_pack_start_sum", 140, 4),
    ("valid_node_count", 144, 4), ("valid_inode_count", 148, 4), ("next_free_nid", 152, 4),
    ("sit_ver_bitmap_bytesize", 156, 4), ("nat_ver_bitmap_bytesize", 160, 4), ("checksum_offset", 164, 4),
]


def checkpoint(img):
    """The newer valid pack: header and last block carry the same version, header crc is right."""
    sb = img.sb
    best = None
    for pack in (sb["cp_blkaddr"], sb["cp_blkaddr"] + BPS):
        h = img.blk(pack)
        c = {name: struct.unpack_from("<" + {4: "I", 8: "Q"}[w], h, o)[0] for name, o, w in CP_FIELD_OFFSETS}
        off = c["checksum_offset"]
        if off > 4092 or off < 192:
            continue
        crc = raw_crc32(MAGIC, h[:off])
        if off + 4 < 4092:
            crc = raw_crc32(crc, h[off + 4:4092])
        if u32(h, off) != crc:
            continue
        total = c["cp_pack_total_block_count"]
        if total < 5 or total > BPS or u64(img.blk(pack + total - 1), 0) != c["checkpoint_ver"]:
            continue
        c["blk"] = pack
        c["curseg"] = {
            "node_segno": [u32(h, 36 + 4 * i) for i in range(8)], "node_blkoff": [u16(h, 68 + 2 * i) for i in range(8)],
            "data_segno": [u32(h, 84 + 4 * i) for i in range(8)], "data_blkoff": [u16(h, 116 + 2 * i) for i in range(8)],
        }
        sl, nl = c["sit_ver_bitmap_bytesize"], c["nat_ver_bitmap_bytesize"]
        if c["ckpt_flags"] & CP_LARGE_NAT_BITMAP_FLAG:
            c["nat_bitmap"] = h[196:196 + nl]
            c["sit_bitmap"] = h[196 + nl:196 + nl + sl]
        else:
            c["sit_bitmap"] = h[192:192 + sl]
            c["nat_bitmap"] = h[192 + sl:192 + sl + nl]
        if best is None or c["checkpoint_ver"] > best["checkpoint_ver"]:
            best = c
    if best is None:
        fail("%s: no valid checkpoint pack" % img.path)
    return best


# ---------------------------------------------------------------- NAT, SIT, journals


def journals(img):
    """(nat, sit): the NAT journal {nid: (ver, ino, addr)} and the SIT journal {segno: raw entry}.
    From memory: the hot-data summary holds the NAT journal and the cold-data summary the SIT
    journal; in the compact form (CP_COMPACT_SUM_FLAG) both journals share the first block."""
    if img.journals is not None:
        return img.journals
    cp = img.cp
    base = cp["blk"] + cp["cp_pack_start_sum"]
    if cp["ckpt_flags"] & CP_COMPACT_SUM_FLAG:
        b = img.blk(base)
        nj, sj = b[0:507], b[507:1014]
    else:
        nj, sj = img.blk(base)[3584:4091], img.blk(base + 2)[3584:4091]
    nat, sit = {}, {}
    n = u16(nj, 0)
    if n > (507 - 2) // 13:
        fail("%s: NAT journal count %d is impossible" % (img.path, n))
    for i in range(n):
        o = 2 + 13 * i
        nat[u32(nj, o)] = struct.unpack_from("<BII", nj, o + 4)
    n = u16(sj, 0)
    if n > (507 - 2) // 78:
        fail("%s: SIT journal count %d is impossible" % (img.path, n))
    for i in range(n):
        o = 2 + 78 * i
        sit[u32(sj, o)] = sj[o + 4:o + 4 + 74]
    img.journals = (nat, sit)
    return img.journals


def meta_pair_addr(base, blk_off, bitmap):
    """From memory: the two copies of a NAT/SIT block are one segment apart in a segment pair;
    the checkpoint bitmap bit says which one is current."""
    a = base + ((blk_off >> 9) << 10) + (blk_off & (BPS - 1))
    cur = bit_msb(bitmap, blk_off)
    return a, a + BPS, cur


def nat_entry(img, nid):
    """Both NAT copies, the journal entry and the resolved one: dict with 'copies' [(ver, ino, addr)] x2,
    'cur' (index of the current copy), 'journal' (tuple or None), 'resolved' (journal wins)."""
    boff = nid // NAT_PER_BLK
    a0, a1, cur = meta_pair_addr(img.sb["nat_blkaddr"], boff, img.cp["nat_bitmap"])
    o = (nid % NAT_PER_BLK) * 9
    copies = [struct.unpack_from("<BII", img.blk(a), o) for a in (a0, a1)]
    j = journals(img)[0].get(nid)
    return {"copies": copies, "cur": cur, "journal": j, "resolved": j if j is not None else copies[cur]}


def nat_addr(img, nid):
    return nat_entry(img, nid)["resolved"][2]


def sit_entry(img, seg):
    """dict: vblocks (count), type, map (64 bytes), mtime, src ('journal' or 'block'), plus the block's own entry."""
    boff = seg // SIT_PER_BLK
    a0, a1, cur = meta_pair_addr(img.sb["sit_blkaddr"], boff, img.cp["sit_bitmap"])
    o = (seg % SIT_PER_BLK) * 74
    raw = img.blk(a1 if cur else a0)[o:o + 74]
    jraw = journals(img)[1].get(seg)
    use = jraw if jraw is not None else raw
    v = u16(use, 0)
    return {"vblocks": v & 0x3FF, "type": v >> 10, "map": use[2:66], "mtime": u64(use, 66),
            "src": "journal" if jraw is not None else "block", "block_raw": raw}


def seg_of(img, blk):
    return (blk - img.sb["main_blkaddr"]) // BPS


def valid_bit(img, blk):
    """SIT valid bit of a main-area block (MSB-first within each map byte; from memory, 3D-R27)."""
    seg = seg_of(img, blk)
    return bit_msb(sit_entry(img, seg)["map"], (blk - img.sb["main_blkaddr"]) % BPS)


def ssa_entry(img, blk):
    """The SSA entry of a main-area block: nid, version, ofs_in_node (7 bytes each; from memory) and
    the footer type of its SSA block (byte 4091; SUM_TYPE_NODE is 1)."""
    seg = seg_of(img, blk)
    b = img.blk(img.sb["ssa_blkaddr"] + seg)
    o = ((blk - img.sb["main_blkaddr"]) % BPS) * 7
    return {"nid": u32(b, o), "version": b[o + 4], "ofs": u16(b, o + 5), "type": b[4091]}


# ---------------------------------------------------------------- nodes


def footer(b):
    return {"nid": u32(b, 4072), "ino": u32(b, 4076), "flag": u32(b, 4080), "ofs": u32(b, 4080) >> 3,
            "cp_ver": u64(b, 4084), "next": u32(b, 4092)}  # ofs = flag >> OFFSET_BIT_SHIFT (3), from memory


def node_offset_expected(slot):
    """3D-R26: the footer ofs a node must carry. slot: ('i', j) | ('d', j, k) | ('dd', i, k) ..."""
    kind = slot[0]
    if kind == "inid":
        return {0: 1, 1: 2, 2: 3, 3: 1022, 4: 2041}[slot[1]]
    if kind == "child2":
        return 4 + slot[1]
    if kind == "child3":
        return 1023 + slot[1]
    if kind == "child4":
        return 2042 + slot[1] * 1019
    if kind == "grand4":
        return 2043 + slot[1] * 1019 + slot[2]
    fail("bad node slot %r" % (slot,))


def inode_csum_ok(img, b):
    """Inode checksum (from memory): crc32_le over (seed from the uuid, ino, generation, the inode with
    i_inode_checksum zeroed). True when the inode has none."""
    inline = b[3]
    feats = img.sb["features"]
    if not (feats & 0x20) or not (inline & 0x20):
        return None
    raw = img.blk(0)[1024:1024 + 3072]
    seed = raw_crc32(0xFFFFFFFF, raw[108:124])
    ino = u32(b, 4076)
    gen = u32(b, 68)
    c = raw_crc32(seed, struct.pack("<I", ino))
    c = raw_crc32(c, struct.pack("<I", gen))
    c = raw_crc32(c, b[:368])
    c = raw_crc32(c, b"\0\0\0\0")
    c = raw_crc32(c, b[372:])
    return c == u32(b, 368)


def node(img, blk):
    """Decode a node block: footer, and the inode or the address/nid list."""
    b = img.blk(blk)
    n = footer(b)
    n["blk"] = blk
    n["raw"] = b
    n["is_inode"] = n["ofs"] == 0 and n["nid"] == n["ino"]
    if n["is_inode"]:
        inline = b[3]
        n["mode"], n["advise"], n["inline"] = u16(b, 0), b[2], inline
        n["links"], n["size"], n["blocks"] = u32(b, 12), u64(b, 16), u64(b, 24)
        n["gen"], n["xattr_nid"], n["flags"], n["pino"] = u32(b, 68), u32(b, 76), u32(b, 80), u32(b, 84)
        nl = u32(b, 88)
        n["namelen"] = nl
        n["name"] = b[92:92 + min(nl, 255)].decode("utf-8", "replace")
        n["extra_isize"] = u16(b, 360) if inline & 0x20 else 0
        n["csum_ok"] = inode_csum_ok(img, b)
        start = 360 + n["extra_isize"]
        cnt = (4052 - start) // 4
        if inline & 0x01:
            cnt -= INLINE_XATTR_ADDRS
        n["addr_start"] = start
        n["addrs"] = [u32(b, start + 4 * i) for i in range(cnt)]
        n["nids"] = [u32(b, 4052 + 4 * i) for i in range(5)]
    else:
        n["items"] = [u32(b, 4 * i) for i in range(ADDRS_PER_NODE)]
    return n


def node_kind(ofs):
    """From the footer ofs (3D-R26 numbering): 'inode', 'direct' or 'indirect' (double-indirect is 2041)."""
    if ofs == 0:
        return "inode"
    if ofs in (1, 2):
        return "direct"
    if ofs in (3, 1022):
        return "indirect"
    if 4 <= ofs <= 1021 or 1023 <= ofs <= 2040:
        return "direct"
    if ofs == 2041:
        return "dindirect"
    return "indirect" if (ofs - 2042) % 1019 == 0 else "direct"


def valid_main(img, addr):
    return img.sb["main_blkaddr"] <= addr < min(img.nblocks, img.sb["main_blkaddr"] + img.sb["segment_count_main"] * BPS)


def file_tree(img, ino):
    """Everything a file owns at the NAT's current state: the inode, its node blocks (with the expected
    ofs of their slot) and its data block addresses. Returns dict."""
    ia = nat_addr(img, ino)
    if not valid_main(img, ia):
        fail("%s: inode %d has no valid NAT address (%d)" % (img.path, ino, ia))
    ind = node(img, ia)
    if not ind["is_inode"] or ind["nid"] != ino:
        fail("%s: block %d is not inode %d" % (img.path, ia, ino))
    nodes = [{"nid": ino, "addr": ia, "exp_ofs": 0, "node": ind, "kind": "inode"}]
    data = [a for a in ind["addrs"] if a not in (NULL_ADDR, NEW_ADDR) and valid_main(img, a)]
    if ind["inline"] & 0x02 or ind["inline"] & 0x04:
        data = []  # inline data or an inline dentry: the addresses are not block addresses

    def add(nid, slot, parent):
        a = nat_addr(img, nid)
        if not valid_main(img, a):
            nodes.append({"nid": nid, "addr": a, "exp_ofs": node_offset_expected(slot), "node": None, "kind": "missing"})
            return None
        nd = node(img, a)
        nodes.append({"nid": nid, "addr": a, "exp_ofs": node_offset_expected(slot), "node": nd,
                      "kind": node_kind(node_offset_expected(slot))})
        return nd

    for j, nid in enumerate(ind["nids"]):
        if nid == 0:
            continue
        nd = add(nid, ("inid", j), ind)
        if nd is None:
            continue
        if j in (0, 1):
            data += [a for a in nd["items"] if a not in (NULL_ADDR, NEW_ADDR) and valid_main(img, a)]
        elif j in (2, 3):
            for k, cn in enumerate(nd["items"]):
                if cn == 0:
                    continue
                c = add(cn, ("child2" if j == 2 else "child3", k), nd)
                if c is not None:
                    data += [a for a in c["items"] if a not in (NULL_ADDR, NEW_ADDR) and valid_main(img, a)]
        else:
            for i, cn in enumerate(nd["items"]):
                if cn == 0:
                    continue
                c = add(cn, ("child4", i), nd)
                if c is None:
                    continue
                for k, gn in enumerate(c["items"]):
                    if gn == 0:
                        continue
                    g = add(gn, ("grand4", i, k), c)
                    if g is not None:
                        data += [a for a in g["items"] if a not in (NULL_ADDR, NEW_ADDR) and valid_main(img, a)]
    return {"ino": ino, "inode": ind, "nodes": nodes, "data": data}


def inode_table(img):
    """{ino: node} for every inode the NAT reaches (nids up to next_free_nid + 128)."""
    out = {}
    top = img.cp["next_free_nid"] + 128
    for nid in range(1, top):
        a = nat_addr(img, nid)
        if not valid_main(img, a):
            continue
        n = node(img, a)
        if n["is_inode"] and n["nid"] == nid:
            out[nid] = n
    return out


def paths(table, root):
    """{path: ino} from the i_pino/i_name of the inode table (the cross-check against fsck -t is separate)."""
    out = {}
    for ino, n in table.items():
        if ino == root:
            out["/"] = ino
            continue
        parts, cur, guard = [n["name"]], n["pino"], 0
        while cur != root and guard < 64:
            p = table.get(cur)
            if p is None:
                parts = None
                break
            parts.append(p["name"])
            cur = p["pino"]
            guard += 1
        if parts is not None:
            out["/" + "/".join(reversed(parts))] = ino
    return out


def inline_dentries(img, n):
    """Inline dentry area of a directory inode (from memory): bitmap, reserved, dentry[NR] (11 bytes: hash,
    ino, name_len, type), filename[NR][8]. Returns [(slot, live, hash, ino, name_len, name)] for every slot
    whose record looks like one (ino != 0, 0 < name_len <= 255), live or not."""
    cnt = len(n["addrs"])
    max_inline = 4 * (cnt - 1)
    nr = (max_inline * 8) // ((11 + 8) * 8 + 1)
    bm_size = (nr + 7) // 8
    reserved = max_inline - (11 + 8) * nr - bm_size
    base = n["addr_start"] + 4
    b = n["raw"]
    bm = b[base:base + bm_size]
    dent = base + bm_size + reserved
    names = dent + 11 * nr
    out = []
    for i in range(nr):
        h, ino, nl, ft = struct.unpack_from("<IIHB", b, dent + 11 * i)
        if ino == 0 or nl == 0 or nl > 255 or names + 8 * i + nl > 4052:
            continue
        nm = b[names + 8 * i:names + 8 * i + nl]
        out.append((i, bool(bit_le(bm, i)), h, ino, nl, nm.decode("utf-8", "replace")))
    return out


# ---------------------------------------------------------------- scanning


def scan_copies(img, wanted):
    """Every main-area block whose footer names a wanted nid with its owner: {nid: [node dicts]}."""
    main = img.sb["main_blkaddr"]
    end = min(img.nblocks, main + img.sb["segment_count_main"] * BPS)
    out = {}
    m = img.m
    for blk in range(main, end):
        o = blk * BLK + 4072
        nid = struct.unpack_from("<I", m, o)[0]
        if nid in wanted and struct.unpack_from("<I", m, o + 4)[0] == wanted[nid]:
            out.setdefault(nid, []).append(node(img, blk))
    return out


# ---------------------------------------------------------------- tool captures (cross-check)


def parse_dump(text):
    out = {}
    for line in text.splitlines():
        m = re.match(r"^(\S+)\s+\[0x\s*([0-9a-f]+) : (-?\d+)\]\s*$", line)
        if m:
            out.setdefault(m.group(1), int(m.group(3)))
    return out


def parse_sit(text):
    segs = []
    cur = None
    for line in text.splitlines():
        m = re.match(r"^segno:\s*(\d+)\s+vblocks:\s*(\d+)\s+seg_type:\s*(\d+)\s+sit_pack:\s*(\d+)", line)
        if m:
            cur = {"segno": int(m.group(1)), "vblocks": int(m.group(2)), "type": int(m.group(3)), "bytes": []}
            segs.append(cur)
        elif cur is not None and re.match(r"^\s+([0-9a-f]{2}\s*)+$", line):
            cur["bytes"] += [int(x, 16) for x in line.split()]
    return segs


def read_text(path):
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read()


def crosscheck(name, img, tdir):
    """Compare this decoder with dump.f2fs on the fields both report. Any difference exits 1."""
    d = os.path.join(tdir, name)
    dump = parse_dump(read_text(os.path.join(d, "dump-i.txt")))
    bad = []
    cpf = {k: v for k, v in img.cp.items() if k != "checksum_offset"}  # printed for both superblock and checkpoint
    for k, v in list(img.sb.items()) + list(cpf.items()):
        if k in dump and isinstance(v, int) and dump[k] != v:
            bad.append("%s: decoder %d, dump.f2fs %d" % (k, v, dump[k]))
    checked = [k for k in list(img.sb) + list(cpf) if k in dump and isinstance(img.sb.get(k, cpf.get(k)), int)]
    nat = {}
    for line in read_text(os.path.join(d, "dump_nat")).splitlines():
        m = re.match(r"^nid:\s*(\d+)\s+ino:\s*(\d+)\s+offset:\s*(\d+)\s+blkaddr:\s*(\d+)\s+pack:\s*(\d+)", line)
        if m:
            nat[int(m.group(1))] = (int(m.group(2)), int(m.group(4)))
    if not nat:
        fail("%s: no NAT line parsed from dump_nat" % name)
    nbad = 0
    for nid, (ino, addr) in nat.items():
        r = nat_entry(img, nid)["resolved"]
        if (r[1], r[2]) != (ino, addr):
            nbad += 1
            if nbad <= 5:
                bad.append("NAT %d: decoder ino %d addr %d, dump.f2fs ino %d addr %d" % (nid, r[1], r[2], ino, addr))
    sit = parse_sit(read_text(os.path.join(d, "dump_sit")))
    if not sit:
        fail("%s: no SIT segment parsed from dump_sit" % name)
    sbad = 0
    for s in sit:
        e = sit_entry(img, s["segno"])
        ok = e["vblocks"] == s["vblocks"] and e["type"] == s["type"]
        if s["bytes"] and bytes(s["bytes"]) != e["map"]:
            ok = False
        if not ok:
            sbad += 1
            if sbad <= 5:
                bad.append("SIT %d: decoder vblocks %d type %d, dump.f2fs vblocks %d type %d" % (
                    s["segno"], e["vblocks"], e["type"], s["vblocks"], s["type"]))
    if bad:
        fail("%s: decoder and dump.f2fs differ (%d NAT, %d SIT): %s" % (name, nbad, sbad, "; ".join(bad)))
    print("CROSSCHECK %s ok: %d superblock/checkpoint fields (%s), %d NAT entries, %d SIT segments" % (
        name, len(checked), ",".join(sorted(checked)[:6]) + "...", len(nat), len(sit)))


# ---------------------------------------------------------------- the probe


def fact(q, text):
    print("FACT[%s] %s %s" % (LABEL, q, text))


def hexline(label, b):
    print("HEX[%s] %-44s %s" % (LABEL, label, hx(b)))


def lo32(v):
    return v & 0xFFFFFFFF


def sha(b):
    return hashlib.sha256(b).hexdigest()[:16]


SCENARIOS = [
    # label, roles: w1 = after the writes, w = stage W (after the rename), d = stage D (after the unlinks),
    # c = after creating new files (None: not run), f = after filling the volume, dw/dd = discard images
    ("small", {"w1": "p1", "w": "p2", "d": "p3", "c": "p4", "f": "p5", "dw": "d1", "dd": "d3"}),
    ("large", {"w1": "l1", "w": "l2", "d": "l3", "c": None, "f": "l5", "dw": "m1", "dd": "m3"}),
]
LABEL = ""


def probe(imgdir, tdir):
    names = ["p0", "p1", "p2", "p3", "p4", "p5", "d1", "d3", "l1", "l2", "l3", "l5", "m1", "m3"]
    im = {n: Image(os.path.join(imgdir, n + ".img")) for n in names}
    for n in names:
        crosscheck(n, im[n], tdir)
    for n in names:
        rc = read_text(os.path.join(tdir, n, "fsck-f.rc")).strip()
        print("FACT TOOL %s fsck.f2fs -f --dry-run exit status %s; checkpoint_ver %d flags 0x%x" % (
            n, rc, im[n].cp["checkpoint_ver"], im[n].cp["ckpt_flags"]))
    res = {}
    for label, roles in SCENARIOS:
        res[label] = {}
        scenario(label, im, roles, tdir, res[label])
    # the verdicts that decide the later tasks
    for g in ("G1", "G4"):
        v = {res[l].get(g) for l, _ in SCENARIOS}
        print("GATE-FINAL %s %s (small=%s large=%s)" % (g, v.pop() if len(v) == 1 else "MIXED", res["small"].get(g), res["large"].get(g)))
    for g in ("G2", "G3", "G5"):
        print("GATE-FINAL %s %s (decided by the large scenario; small=%s)" % (g, res["large"].get(g), res["small"].get(g)))
    for g in ("G6.renamed", "G6.nidreuse", "G6.reuse"):
        print("GATE-FINAL %s %s (small=%s large=%s)" % (g, res["small"].get(g), res["small"].get(g), res["large"].get(g)))
    for n in names:
        im[n].close()


def scenario(label, im, r, tdir, verdicts):
    global LABEL
    LABEL = label

    def print(*a):  # a GATE line is recorded and tagged with the scenario
        text = " ".join(str(x) for x in a)
        m = re.match(r"GATE (\S+) (\S+)", text)
        if m:
            verdicts[m.group(1)] = m.group(2)
            text = "GATE[%s] %s" % (label, text[5:])
        builtins.print(text)

    w, d = im[r["w"]], im[r["d"]]  # stage W (before the unlinks) and stage D (after)
    root = w.sb["root_ino"]
    tw = inode_table(w)
    pw = paths(tw, root)
    # tree cross-check: fsck -t lists the same paths with the same inode numbers
    tree = {}
    stack = []
    for line in read_text(os.path.join(tdir, r["w"], "tree.txt")).splitlines():
        m = re.match(r"^((?:\|   |    )*)[|`]-- (.*) <ino = 0x([0-9a-f]+)>", line)
        if m:
            depth = len(m.group(1)) // 4
            del stack[depth:]
            stack.append(m.group(2))
            tree["/" + "/".join(stack)] = int(m.group(3), 16)
    for p, ino in tree.items():
        if pw.get(p) != ino:
            fail("fsck.f2fs -t says %s is inode %d, the decoder says %r" % (p, ino, pw.get(p)))
    fact("TREE", "p2 paths (decoder == fsck -t): " + " ".join("%s=%d" % (p, pw[p]) for p in sorted(pw) if p in tree))

    files = {"A": "/p/A", "B": "/p/B", "C": "/p/C", "H": "/p/H", "I": "/p/I", "R2": "/p/R2", "f1": "/s/x/f1"}
    trees = {k: file_tree(w, pw[p]) for k, p in files.items()}
    inos = {k: pw[p] for k, p in files.items()}
    wanted = {}
    for k, t in trees.items():
        for nd in t["nodes"]:
            wanted[nd["nid"]] = t["ino"]
    print("DELETED nids: " + " ".join("%s=%s" % (k, sorted(nd["nid"] for nd in t["nodes"])) for k, t in trees.items()))

    # ---- Q1, Q7: stale blocks identical to stage W; ofs against the table
    g1 = {}
    for k, t in trees.items():
        ident = []
        for nd in t["nodes"]:
            same = d.blk(nd["addr"]) == w.blk(nd["addr"])
            ident.append(same)
            f = footer(w.blk(nd["addr"]))
            fact("Q1", "file=%s nid=%d kind=%s addr=%d identical_to_W=%s sha=%s footer(nid=%d ino=%d ofs=%d exp_ofs=%d)" % (
                k, nd["nid"], nd["kind"], nd["addr"], same, sha(d.blk(nd["addr"])), f["nid"], f["ino"], f["ofs"], nd["exp_ofs"]))
        g1[k] = all(ident)
        ofs_ok = all(nd["node"] is not None and nd["node"]["ofs"] == nd["exp_ofs"] and nd["node"]["ino"] == t["ino"]
                     for nd in t["nodes"])
        fact("Q7", "file=%s inode_nid=%d i_nid=%s nodes=%d ofs_equals_table=%s" % (
            k, t["ino"], t["inode"]["nids"], len(t["nodes"]), ofs_ok))
        g1[k] = g1[k] and ofs_ok
    for k in ("A", "B"):
        i = trees[k]["inode"]
        hexline("Q1 inode %s head(0..96)" % k, i["raw"][:96])
        hexline("Q1 inode %s footer" % k, i["raw"][4072:])
    # stage W == stage D for the sake of the stage-W names: p1 blocks at the same addresses
    w1 = im[r["w1"]]
    same_p1 = {k: all(w1.blk(nd["addr"]) == w.blk(nd["addr"]) for nd in t["nodes"]) for k, t in trees.items()}
    fact("Q1", "stage W (p2) blocks equal the p1 blocks at the same addresses, per file: %r" % same_p1)
    abc = [g1[k] for k in ("A", "B", "C")]
    verdict = "HOLDS" if all(abc) else ("FAILS" if not any(abc) else "MIXED")
    print("GATE G1 %s (A=%s B=%s C=%s; informational H=%s I=%s R2=%s f1=%s)" % (
        verdict, g1["A"], g1["B"], g1["C"], g1["H"], g1["I"], g1["R2"], g1["f1"]))

    # ---- Q2: NAT in stage D
    for k, t in trees.items():
        for nd in t["nodes"]:
            e = nat_entry(d, nd["nid"])
            fact("Q2", "file=%s nid=%d resolved=%r cur_copy=%d copy0=%r copy1=%r journal=%r" % (
                k, nd["nid"], e["resolved"], e["cur"], e["copies"][0], e["copies"][1], e["journal"]))
    zero = all(nat_entry(d, nd["nid"])["resolved"][2] == 0 for t in trees.values() for nd in t["nodes"])
    fact("Q2", "every deleted nid resolves to block address 0 in stage D: %s; NAT journal holds %d entries, SIT journal %d" % (
        zero, len(journals(d)[0]), len(journals(d)[1])))
    ea = nat_entry(d, trees["A"]["ino"])
    hexline("Q2 NAT entry nid=%d copy0 (ver,ino,addr)" % trees["A"]["ino"], struct.pack("<BII", *ea["copies"][0]))
    hexline("Q2 NAT entry nid=%d copy1 (current=%d)" % (trees["A"]["ino"], ea["cur"]), struct.pack("<BII", *ea["copies"][1]))

    # ---- stale copies in stage D
    copies = scan_copies(d, wanted)
    cs = d.cp["curseg"]
    cur_segs = set(cs["node_segno"][:3]) | set(cs["data_segno"][:3])
    vd = lo32(d.cp["checkpoint_ver"])
    vw = lo32(w.cp["checkpoint_ver"])
    g4_bad = []
    ssa_checked, ssa_bad = [], []  # stale copies in non-current segments
    freed = []  # stale copies in non-current segments that the deletes left at vblocks 0
    total = 0
    for nid in sorted(copies):
        for c in copies[nid]:
            total += 1
            seg = seg_of(d, c["blk"])
            se = sit_entry(d, seg)
            ssa = ssa_entry(d, c["blk"])
            vb = valid_bit(d, c["blk"])
            cur = seg in cur_segs
            ver = lo32(c["cp_ver"])
            c["ver"] = ver
            fact("Q3", "nid=%d blk=%d seg=%d sit_type=%d vblocks=%d valid_bit=%d src=%s current_segment=%s ssa_footer_type=%d" % (
                nid, c["blk"], seg, se["type"], se["vblocks"], vb, se["src"], cur, ssa["type"]))
            fact("Q4", "nid=%d blk=%d ssa(nid=%d ver=%d ofs=%d) footer(nid=%d ofs=%d) in_current_segment=%s" % (
                nid, c["blk"], ssa["nid"], ssa["version"], ssa["ofs"], c["nid"], c["ofs"], cur))
            fact("Q5", "nid=%d blk=%d cp_ver=0x%016x low32=%d high32=0x%08x W_ver=%d D_ver=%d" % (
                nid, c["blk"], c["cp_ver"], ver, c["cp_ver"] >> 32, vw, vd))
            if not cur:
                ssa_checked.append((nid, c["blk"]))
                if ssa["nid"] != c["nid"]:
                    ssa_bad.append((nid, c["blk"], ssa["nid"]))
                if se["vblocks"] == 0:
                    freed.append((nid, c["blk"], seg, se["type"], ssa["type"]))
            if not ver < vd:
                g4_bad.append((nid, c["blk"], ver))
    fact("Q3", "%d stale copies; %d in non-current segments, %d of those in segments left at vblocks 0" % (
        total, len(ssa_checked), len(freed)))
    fact("Q3", "freed segments (seg: sit_type, ssa_footer_type): %r" % sorted({(f[2], f[3], f[4]) for f in freed}))
    if not freed:
        print("GATE G3 STOP no stale node copy lies in a freed, non-current segment (the probe cannot decide)")
    elif all(f[3] in NODE_SEG_TYPES for f in freed):
        print("GATE G3 SIT (all %d copies in freed segments lie in segments whose SIT type is node: %r)" % (
            len(freed), sorted({f[3] for f in freed})))
    elif all(f[4] == SUM_TYPE_NODE for f in freed if f[3] not in NODE_SEG_TYPES):
        print("GATE G3 SIT+SSA (freed segments lose the node type in the SIT: %r; the SSA footer still says node)" % (
            sorted({(f[2], f[3]) for f in freed if f[3] not in NODE_SEG_TYPES})))
    else:
        print("GATE G3 STOP neither the SIT type nor the SSA footer identifies the freed node segments: %r" % (freed[:6],))
    if not ssa_checked:
        print("GATE G2 STOP no stale node block lies in a non-current segment (the probe cannot decide)")
    else:
        print("GATE G2 %s (%d stale node blocks in non-current segments, %d with ssa.nid != footer.nid%s)" % (
            "KEEP" if not ssa_bad else "DROP", len(ssa_checked), len(ssa_bad), ": " + repr(ssa_bad[:6]) if ssa_bad else ""))
    flags = d.cp["ckpt_flags"]
    fact("Q5", "checkpoint flags W=0x%x D=0x%x CP_CRC_RECOVERY_FLAG(0x40) set: W=%s D=%s; checkpoint_ver W=%d D=%d" % (
        w.cp["ckpt_flags"], flags, bool(w.cp["ckpt_flags"] & CP_CRC_RECOVERY_FLAG), bool(flags & CP_CRC_RECOVERY_FLAG), vw, vd))
    print("GATE G4 %s (%d copies with low32(cp_ver) >= low32(stage-D checkpoint_ver)=%d%s)" % (
        "HOLDS" if total and not g4_bad else "FAILS", len(g4_bad), vd, "; " + repr(g4_bad[:5]) if g4_bad else ""))
    if copies:
        c0 = copies[sorted(copies)[0]][0]
        hexline("Q5 footer nid=%d blk=%d" % (c0["nid"], c0["blk"]), c0["raw"][4072:])
    nc = next((c for nid in sorted(copies) for c in copies[nid] if seg_of(d, c["blk"]) not in cur_segs), None)
    if nc is not None:
        seg = seg_of(d, nc["blk"])
        se = sit_entry(d, seg)
        hexline("Q3 SIT entry seg=%d (%s), 74 bytes" % (seg, se["src"]), se["block_raw"] if se["src"] == "block" else journals(d)[1][seg])
        ssb = d.blk(d.sb["ssa_blkaddr"] + seg)
        o = ((nc["blk"] - d.sb["main_blkaddr"]) % BPS) * 7
        hexline("Q4 SSA entry of blk=%d (7 bytes) + footer type byte" % nc["blk"], ssb[o:o + 7] + ssb[4091:4092])

    # ---- cursors (3D-R27), on stage W where the last written blocks are still live
    names3 = ["hot", "warm", "cold"]
    wcs = w.cp["curseg"]
    for i in range(3):
        seg, off = wcs["node_segno"][i], wcs["node_blkoff"][i]
        last = w.sb["main_blkaddr"] + seg * BPS + off - 1
        if off > 0 and valid_main(w, last):
            f = footer(w.blk(last))
            nxt = valid_bit(w, last + 1) if off < BPS else "n/a"
            ok = f["nid"] != 0 and f["nid"] < 1 << 24 and valid_bit(w, last) == 1 and (off == BPS or nxt == 0)
            fact("Q4", "stage W cursor node/%s: cur_node_segno=%d blkoff=%d last block %d footer(nid=%d ino=%d ofs=%d) sit_valid=%d next_valid=%s -> main_relative_and_last_written=%s" % (
                names3[i], seg, off, last, f["nid"], f["ino"], f["ofs"], valid_bit(w, last), nxt, ok))
        else:
            fact("Q4", "stage W cursor node/%s: cur_node_segno=%d blkoff=%d: nothing written yet" % (names3[i], seg, off))

    # ---- Q8: copies and ties
    ties = 0
    for nid in sorted(copies):
        cl = copies[nid]
        top = max(c["ver"] for c in cl)
        tied = [c for c in cl if c["ver"] == top]
        eq = "n/a"
        if len(tied) > 1:
            ties += 1
            if tied[0]["is_inode"]:
                eq = all((c["addrs"], c["nids"], c["size"]) == (tied[0]["addrs"], tied[0]["nids"], tied[0]["size"]) for c in tied)
            else:
                eq = all(c["items"] == tied[0]["items"] for c in tied)
        fact("Q8", "nid=%d copies=%d versions=%r highest_shared_by=%d tied_equal=%s" % (
            nid, len(cl), sorted(c["ver"] for c in cl), len(tied), eq))
    fact("Q8", "nids with a same-version tie: %d of %d" % (ties, len(copies)))

    # ---- Q9: the renamed file
    rn = pw["/p/R2"]
    p1r = paths(inode_table(w1), root)["/p/R"]
    rc = scan_copies(d, {rn: rn})
    for c in sorted(rc.get(rn, []), key=lambda c: lo32(c["cp_ver"])):
        fact("Q9", "inode %d copy blk=%d ver=%d name=%r pino=%d" % (rn, c["blk"], lo32(c["cp_ver"]), c["name"], c["pino"]))
    newest = sorted(rc.get(rn, []), key=lambda c: lo32(c["cp_ver"]))
    top = [c for c in newest if lo32(c["cp_ver"]) == lo32(newest[-1]["cp_ver"])] if newest else []
    names_top = sorted({c["name"] for c in top})
    fact("Q9", "p1 inode of R is %d, p2 inode of R2 is %d; names in the newest copies: %r" % (p1r, rn, names_top))
    for c in newest:
        hexline("Q9 inode %d copy blk=%d i_namelen+i_name" % (rn, c["blk"]), c["raw"][88:100])
    print("GATE G6.renamed %s (newest copy names %r)" % ("REAL" if names_top == ["R2"] else ("MIXED" if "R2" in names_top else "BUILDER-ONLY"), names_top))

    # ---- Q10: nid reuse after the deletes (p3 -> p4)
    if r["c"]:
        p4 = im[r["c"]]
    freed = sorted(wanted)
    taken = [n for n in freed if nat_addr(p4, n) != 0] if r["c"] else []
    if r["c"]:
        p4p = paths(inode_table(p4), root)
        nn = sorted(p4p[p] for p in p4p if p.startswith("/p/N"))
        fact("Q10", "freed nids %r; taken again in p4 (NAT address != 0): %r; inodes of /p/N1../p/N8: %r; next_free_nid p3=%d p4=%d" % (
            freed, taken, nn, d.cp["next_free_nid"], p4.cp["next_free_nid"]))
        print("GATE G6.nidreuse %s" % ("REAL" if taken else "BUILDER-ONLY"))
        hexline("Q10 NAT entry nid=%d in p4 (ver,ino,addr)" % freed[0], struct.pack("<BII", *nat_entry(p4, freed[0])["resolved"]))

    # ---- Q11: reuse after filling the volume (p3 -> p5)
    p5 = im[r["f"]]
    reused_any = False
    for k, t in trees.items():
        node_blocks = [nd["addr"] for nd in t["nodes"]]
        inode_intact = p5.blk(t["nodes"][0]["addr"]) == d.blk(t["nodes"][0]["addr"])
        dr = [a for a in t["data"] if p5.blk(a) != d.blk(a)]
        nr = [a for a in node_blocks if p5.blk(a) != d.blk(a)]
        vr = [a for a in t["data"] if valid_bit(p5, a)]
        fact("Q11", "file=%s data_blocks=%d changed=%d sit_valid_now=%d node_blocks=%d changed=%d stale_inode_intact=%s" % (
            k, len(t["data"]), len(dr), len(vr), len(node_blocks), len(nr), inode_intact))
        if dr and inode_intact:
            reused_any = True
    print("GATE G6.reuse %s" % ("REAL" if reused_any else "BUILDER-ONLY"))
    b0 = trees["B"]["data"][0]
    hexline("Q11 B data block %d first 16 bytes, stage D" % b0, d.blk(b0)[:16])
    hexline("Q11 B data block %d first 16 bytes, after fill" % b0, p5.blk(b0)[:16])
    fact("Q11", "checkpoint p5: free_segment_count=%d valid_block_count=%d user_block_count=%d (the SYSFS lines above carry the GC counters)" % (
        p5.cp["free_segment_count"], p5.cp["valid_block_count"], p5.cp["user_block_count"]))

    # ---- Q12: inline directory /s/x
    xdir = pw["/s/x"]
    xa = nat_addr(d, xdir)
    xn = node(d, xa)
    fact("Q12", "dir /s/x inode %d at %d in stage D inline=0x%x; f1 inode was %d" % (xdir, xa, xn["inline"], inos["f1"]))
    if xn["inline"] & 0x04:
        for ent in inline_dentries(d, xn):
            fact("Q12", "slot=%d live=%s hash=0x%08x ino=%d name_len=%d name=%r" % ent)
        hexline("Q12 dir inode inline area head", xn["raw"][xn["addr_start"]:xn["addr_start"] + 96])
    else:
        fact("Q12", "the directory is not inline")

    # ---- Q13: the sparse file
    h = trees["H"]["inode"]
    fact("Q13", "H size=%d inline=0x%x i_addr[0..5]=%s" % (h["size"], h["inline"], ["0x%x" % a for a in h["addrs"][:6]]))
    hexline("Q13 H i_addr[0..5]", h["raw"][h["addr_start"]:h["addr_start"] + 24])

    # ---- Q6: discard images
    d1, d3 = im[r["dw"]], im[r["dd"]]
    pd = paths(inode_table(d1), root)
    dtrees = {k: file_tree(d1, pd[p]) for k, p in {"A": "/p/A", "B": "/p/B", "C": "/p/C", "H": "/p/H", "I": "/p/I",
                                                    "R": "/p/R", "f1": "/s/x/f1"}.items()}
    zeros = bytes(BLK)
    dcur = set(d3.cp["curseg"]["node_segno"][:3]) | set(d3.cp["curseg"]["data_segno"][:3])
    dz = dt = nz = nt = 0  # blocks outside the current segments (the ones a discard can reach)
    cur_dz = cur_dt = cur_nz = cur_nt = 0  # blocks inside a current segment
    for k, t in dtrees.items():
        row = [0, 0, 0, 0, 0, 0, 0, 0]
        for a in t["data"]:
            incur = seg_of(d3, a) in dcur
            z = d3.blk(a) == zeros
            if incur:
                cur_dt += 1
                cur_dz += z
            else:
                dt += 1
                dz += z
            row[0 if not incur else 1] += 1
            row[2 if not incur else 3] += z
        for nd in t["nodes"]:
            incur = seg_of(d3, nd["addr"]) in dcur
            z = d3.blk(nd["addr"]) == zeros
            if incur:
                cur_nt += 1
                cur_nz += z
            else:
                nt += 1
                nz += z
            row[4 if not incur else 5] += 1
            row[6 if not incur else 7] += z
        fact("Q6", "file=%s data(outside current segs %d zero %d; in current segs %d zero %d) nodes(outside %d zero %d; in current %d zero %d)" % (
            k, row[0], row[2], row[1], row[3], row[4], row[6], row[5], row[7]))
    fact("Q6", "outside the current segments: data %d zero %d, nodes %d zero %d; inside: data %d zero %d, nodes %d zero %d" % (
        dt, dz, nt, nz, cur_dt, cur_dz, cur_nt, cur_nz))
    ab = dtrees["B"]["data"][0]
    an = dtrees["A"]["nodes"][0]["addr"]
    hexline("Q6 B data block %d first 16 bytes, before" % ab, d1.blk(ab)[:16])
    hexline("Q6 B data block %d first 16 bytes, after" % ab, d3.blk(ab)[:16])
    hexline("Q6 A inode block %d last 24 bytes, before" % an, d1.blk(an)[4072:])
    hexline("Q6 A inode block %d last 24 bytes, after" % an, d3.blk(an)[4072:])
    if not nt or not dt:
        v = "UNDECIDED (no node or data block outside the current segments)"
    elif nz == nt:
        v = "gone"
    elif dt and dz == dt and nz == 0:
        v = "uniform"
    elif dz == 0 and nz == 0:
        v = "none"
    else:
        v = "MIXED"
    print("GATE G5 %s (outside the current segments: data zero %d/%d, node zero %d/%d)" % (v, dz, dt, nz, nt))

    # ---- where the stale node copies are after the fill (3D-R25: segments now typed data)
    ty = {}
    for nid in sorted(copies):
        for c in copies[nid]:
            s5 = sit_entry(p5, seg_of(p5, c["blk"]))
            intact = p5.blk(c["blk"]) == d.blk(c["blk"])
            ty.setdefault((s5["type"], intact), 0)
            ty[(s5["type"], intact)] += 1
    fact("Q11", "stale node copies after the fill, by (SIT type of their segment, block unchanged): %r" % sorted(ty.items()))


# ---------------------------------------------------------------- the recovery fixtures (Task 9): snap and final
#
# Everything below decodes the finished images and the captures of the f2fs tools. The expectations of "final" are
# the spec's rules (6.5, 3D-R2 to R7 and R13) written a second time, from the image bytes and from what the generator
# wrote (the "expected" tree), never from the Go reader.

DENT_BM, DENT_RES, DENT_NR, DENT_SIZE, SLOT_LEN = 27, 3, 214, 11, 8
INLINE_DATA, INLINE_DENTRY = 0x02, 0x04
FT_REG, FT_DIR, FT_SYMLINK, FT_MAX = 1, 2, 7, 7
MODE_FMT = {0x8000: "file", 0x4000: "dir", 0xA000: "symlink"}
COMMON_FAMILIES = ["contig", "twofrag", "direct", "indirect", "holes", "inline", "empty", "gone"]
G6_FAMILIES = ["renamed", "nidreuse", "reuse"]



def nat_capacity(img):
    """Node ids the NAT area can describe: its segment pairs, each of 512 blocks of 455 entries."""
    return (img.sb["segment_count_nat"] // 2) * BPS * NAT_PER_BLK


def rangeify(blocks):
    """Block numbers in order -> inclusive [a, b] ranges of consecutive numbers (the order is kept)."""
    out = []
    for b in blocks:
        if out and out[-1][1] == b - 1:
            out[-1][1] = b
        else:
            out.append([b, b])
    return out


def expand(ranges):
    return [b for r in ranges for b in range(r[0], r[1] + 1)]


def is_hole(a):
    return a in (NULL_ADDR, NEW_ADDR)

def extents(addrs):
    """Inclusive absolute ranges of the data blocks in file order; a hole always ends a range."""
    out, prev = [], None
    for a in addrs:
        if is_hole(a):
            prev = None
            continue
        if prev is not None and a == prev + 1:
            out[-1][1] = a
        else:
            out.append([a, a])
        prev = a
    return out



def slot_stream(ind, nodefor):
    """The data slots of an inode in logical order: ('a', addr) or ('stop', token). nodefor(nid, owner, ofs) returns
    the node dict or None (missing). A zero node id stands for a range of holes. Numbering of 3D-R26."""
    owner = ind["nid"]

    def dnode(nid, ofs):
        if nid == 0:
            for _ in range(ADDRS_PER_NODE):
                yield ("a", NULL_ADDR)
            return
        n = nodefor(nid, owner, ofs)
        if n is None:
            yield ("stop", "node-missing@%d" % nid)
            return
        for a in n["items"]:
            yield ("a", a)

    def indirect(nid, ofs, child_ofs):
        if nid == 0:
            for _ in range(ADDRS_PER_NODE * ADDRS_PER_NODE):
                yield ("a", NULL_ADDR)
            return
        n = nodefor(nid, owner, ofs)
        if n is None:
            yield ("stop", "node-missing@%d" % nid)
            return
        for k, cn in enumerate(n["items"]):
            yield from dnode(cn, child_ofs(k))

    for a in ind["addrs"]:
        yield ("a", a)
    nids = ind["nids"]
    yield from dnode(nids[0], 1)
    yield from dnode(nids[1], 2)
    yield from indirect(nids[2], 3, lambda k: 4 + k)
    yield from indirect(nids[3], 1022, lambda k: 1023 + k)
    if nids[4] != 0:
        fail("inode %d uses a double-indirect node, which no fixture builds (a file over 8 GiB)" % owner)


def logical_blocks(ind, nodefor, need):
    """(addrs, stop): the first `need` logical block addresses (NULL_ADDR/NEW_ADDR for holes) and the stop token
    when the stream stopped early."""
    out, stop = [], None
    if need == 0:
        return out, stop
    for kind, v in slot_stream(ind, nodefor):
        if kind == "stop":
            stop = v
            break
        out.append(v)
        if len(out) == need:
            break
    return out, stop


def live_nodefor(img):
    """Node lookup through the CURRENT NAT: the live tree of a file."""
    def nodefor(nid, owner, ofs):
        a = nat_addr(img, nid)
        if not valid_main(img, a):
            return None
        n = node(img, a)
        if n["nid"] != nid or n["ino"] != owner or n["ofs"] != ofs:
            fail("%s: node %d at %d: footer nid %d ino %d ofs %d, expected owner %d ofs %d" % (
                img.path, nid, a, n["nid"], n["ino"], n["ofs"], owner, ofs))
        return n
    return nodefor


def inline_bytes(ind, size):
    start = ind["addr_start"] + 4  # DEF_INLINE_RESERVED_SIZE is one address slot
    if size > 4052 - start:
        fail("inode %d: inline size %d does not fit" % (ind["nid"], size))
    return ind["raw"][start:start + size]


def decode_file(img, ino):
    """A live regular file as the oracle states it: data blocks in file order (inclusive absolute ranges), holes
    (inclusive LOGICAL block ranges), the nodes it owns, the content bytes."""
    a = nat_addr(img, ino)
    if not valid_main(img, a):
        fail("%s: inode %d has no NAT address" % (img.path, ino))
    ind = node(img, a)
    if not ind["is_inode"] or ind["nid"] != ino:
        fail("%s: block %d is not inode %d" % (img.path, a, ino))
    size = ind["size"]
    inline = bool(ind["inline"] & INLINE_DATA)
    addrs = []
    if not inline:
        need = (size + BLK - 1) // BLK
        addrs, stop = logical_blocks(ind, live_nodefor(img), need)
        if stop or len(addrs) != need:
            fail("%s: inode %d: cannot decode its block map (%s)" % (img.path, ino, stop))
    data = [x for x in addrs if not is_hole(x)]
    for x in data:
        if not valid_main(img, x) or not valid_bit(img, x):
            fail("%s: inode %d: data block %d is outside the main area or not valid in the SIT" % (img.path, ino, x))
    holes, i = [], 0
    while i < len(addrs):
        if is_hole(addrs[i]):
            j = i
            while j + 1 < len(addrs) and is_hole(addrs[j + 1]):
                j += 1
            holes.append([i, j])
            i = j + 1
        else:
            i += 1
    if inline:
        content = inline_bytes(ind, size)
    else:
        zero = bytes(BLK)
        content = b"".join(zero if is_hole(x) else img.blk(x) for x in addrs)[:size]
    return {"ind": ind, "size": size, "inline": inline, "data": data, "blocks": extents(addrs), "holes": holes,
            "content": content, "tree": file_tree(img, ino), "indirect": any(n != 0 for n in ind["nids"][2:])}


def parse_tree(text):
    """fsck.f2fs -t: '|   |-- name <ino = 0x16>, <encrypted (0)>' -> {path: ino}."""
    paths_, stack = {}, []
    for line in text.splitlines():
        m = re.match(r"^((?:\|   |    )*)[|`]-- (.*) <ino = 0x([0-9a-f]+)>, <encrypted \((\d)\)>$", line)
        if not m:
            continue
        depth = len(m.group(1)) // 4
        del stack[depth:]
        if len(stack) != depth:
            fail("tree output skips a level: " + line)
        stack.append(m.group(2))
        if m.group(4) != "0":
            fail("an entry is encrypted: " + line)
        p = "/" + "/".join(stack)
        if p in paths_:
            fail("duplicate path in the tree output: " + p)
        paths_[p] = int(m.group(3), 16)
    return paths_


def parse_map(text):
    """fsck.f2fs -d 1 -M: '/path a-b c-d [0]' (progress is separated by CR or ESC[2K): {path: [[a, b], ...]}."""
    out = {}
    for chunk in re.split(r"[\r\n]|\x1b\[2K", text):
        if not chunk.startswith("/"):
            continue
        toks = chunk.split(" ")
        i = len(toks)
        while i > 1 and re.fullmatch(r"\d+(-\d+)?", toks[i - 1]):
            i -= 1
        p, ext = " ".join(toks[:i]), toks[i:]
        runs = []
        for t in ext:
            if t == "0":
                continue
            a, _, b = t.partition("-")
            a, b = int(a), int(b or a)
            if b < a:
                fail("descending extent in the file map: " + chunk)
            runs.append([a, b])
        if runs:
            out[p] = runs
    return out


def stage_checks(img, tdir, stage):
    """Cross-check the decoder against the tools on one stage image. Returns (tree, fmap, table, pw_files)."""
    crosscheck(stage, img, tdir)
    fsck = read_text(os.path.join(tdir, stage, "fsck-f.txt"))
    rc = read_text(os.path.join(tdir, stage, "fsck-f.rc")).strip()
    if rc != "0" or "[Fail" in fsck:
        fail("%s: fsck.f2fs -f --dry-run exit status %s" % (stage, rc))
    valid = 0
    for seg in range(img.sb["segment_count_main"]):
        e = sit_entry(img, seg)
        bits = sum(bin(b).count("1") for b in e["map"])
        if bits != e["vblocks"]:
            fail("%s: SIT segment %d has %d bits set and vblocks %d" % (stage, seg, bits, e["vblocks"]))
        valid += bits
    if valid != img.cp["valid_block_count"]:
        fail("%s: SIT popcount %d != checkpoint valid_block_count %d" % (stage, valid, img.cp["valid_block_count"]))
    root = img.sb["root_ino"]
    table = inode_table(img)
    pw_files = {p: i for p, i in paths(table, root).items() if p != "/"}
    tree = parse_tree(read_text(os.path.join(tdir, stage, "tree.txt")))
    if tree != pw_files:
        fail("%s: decoder paths and fsck.f2fs -t differ: only decoder %r, only fsck %r, other %r" % (
            stage, sorted(set(pw_files) - set(tree))[:5], sorted(set(tree) - set(pw_files))[:5],
            [p for p in tree if p in pw_files and tree[p] != pw_files[p]][:5]))
    fmap = parse_map(read_text(os.path.join(tdir, stage, "map.txt")))
    return tree, fmap, table, pw_files


def walk_files(img, table, pw_files, fmap, exp, stage):
    """Decode every regular file of a stage image, cross-check with fsck -M and with what the generator wrote."""
    out = {}
    for p, ino in sorted(pw_files.items()):
        kind = MODE_FMT.get(table[ino]["mode"] & 0xF000, "other")
        if kind == "dir":
            continue
        if kind != "file":
            fail("%s: %s is a %s, which no fixture creates" % (stage, p, kind))
        d = decode_file(img, ino)
        if d["blocks"] != fmap.get(p, []):
            fail("%s: %s: decoder data extents %r, fsck.f2fs -M %r" % (stage, p, d["blocks"][:4], fmap.get(p, [])[:4]))
        sha = hashlib.sha256(d["content"]).hexdigest()
        if p == "/fill":
            if d["content"].count(0) != len(d["content"]):
                fail("%s: /fill holds a byte other than zero" % stage)
        elif p in exp:
            if sha != exp[p]:
                fail("%s: %s: the image holds sha256 %s, the generator wrote %s" % (stage, p, sha, exp[p]))
        elif d["size"] != 0:
            fail("%s: %s (%d bytes) is not in the expected tree" % (stage, p, d["size"]))
        out[p] = {"path": p, "ino": ino, "dec": d, "sha256": sha}
    for p in fmap:
        if p not in out:
            fail("%s: fsck.f2fs -M lists %s, which is not a regular file of the decoder" % (stage, p))
    return out


def expected_tree(root):
    """{'/rel/path': sha256} of the generator's expected-content tree (the bytes the shell was told to write)."""
    out = {}
    for dp, _, names in os.walk(root):
        for nm in names:
            full = os.path.join(dp, nm)
            with open(full, "rb") as f:
                out["/" + os.path.relpath(full, root).replace(os.sep, "/")] = hashlib.sha256(f.read()).hexdigest()
    return out


def cmd_snap(a):
    img = Image(a.image)
    _tree, fmap, table, pw_files = stage_checks(img, a.tools, a.stage)
    files = walk_files(img, table, pw_files, fmap, expected_tree(a.expected), a.stage)
    recs = {}
    for p, ino in pw_files.items():
        n = table[ino]
        r = {"path": p, "ino": ino, "name": n["name"], "pino": n["pino"], "mode": n["mode"], "nat_addr": n["blk"],
             "cp_ver": n["cp_ver"], "inode_hex": n["raw"].hex(), "i_nid": n["nids"], "inline_flags": n["inline"],
             "size": n["size"], "kind": MODE_FMT.get(n["mode"] & 0xF000, "other"),
             "blocks": [], "holes": [], "sha256": "", "indirect": False, "inline": False}
        nodes = [{"nid": ino, "addr": n["blk"], "ofs": 0}]
        if p in files:
            d = files[p]["dec"]
            r.update({"blocks": d["blocks"], "holes": d["holes"], "sha256": files[p]["sha256"],
                      "indirect": d["indirect"], "inline": d["inline"]})
            nodes = [{"nid": nd["nid"], "addr": nd["addr"], "ofs": nd["exp_ofs"]} for nd in d["tree"]["nodes"]]
        for nd in nodes:
            nd["sha256"] = hashlib.sha256(img.blk(nd["addr"])).hexdigest()
        r["nodes"] = nodes
        recs[str(ino)] = r
    with open(a.out, "w", encoding="utf-8") as f:
        json.dump({"stage": a.stage, "checkpoint_ver": img.cp["checkpoint_ver"], "files": recs}, f)
    print("SNAP %s: %d paths (%d regular files decoded, equal to fsck.f2fs -t and -M), checkpoint %d" % (
        a.stage, len(pw_files), len(files), img.cp["checkpoint_ver"]))
    img.close()


# ---- dentries

def dir_areas(img, ind):
    """The dentry areas of a live directory: the block bytes and the offsets of bitmap, dentries and names."""
    if ind["inline"] & INLINE_DENTRY:
        max_inline = 4 * (len(ind["addrs"]) - 1)
        nr = (max_inline * 8) // ((11 + 8) * 8 + 1)
        bm_size = (nr + 7) // 8
        reserved = max_inline - (11 + 8) * nr - bm_size
        base = ind["addr_start"] + 4
        ents = base + bm_size + reserved
        return [{"b": ind["raw"], "bm": base, "ents": ents, "names": ents + 11 * nr, "nr": nr, "blk": 0, "first": True}]
    need = (ind["size"] + BLK - 1) // BLK
    addrs, stop = logical_blocks(ind, live_nodefor(img), need)
    if stop:
        fail("%s: directory %d: block map stops (%s)" % (img.path, ind["nid"], stop))
    out = []
    for lb, ad in enumerate(addrs):
        if not is_hole(ad):
            out.append({"b": img.blk(ad), "bm": 0, "ents": DENT_BM + DENT_RES,
                        "names": DENT_BM + DENT_RES + DENT_NR * DENT_SIZE, "nr": DENT_NR, "blk": lb, "first": lb == 0})
    return out


def scan_area(area, natcap):
    """The dentry rules (spec 6.2) written from the on-disk format: the records of one area, live and deleted, in
    slot order. A free slot is a deleted record when it names a plausible inode, name and type and no live slot
    overlaps its name."""
    b, nr = area["b"], area["nr"]
    bitmap = b[area["bm"]:area["bm"] + (nr + 7) // 8]

    def live(i):
        return bit_le(bitmap, i)

    recs, i = [], 0
    while i < nr:
        _h, ino, nl, ft = struct.unpack_from("<IIHB", b, area["ents"] + i * DENT_SIZE)
        slots = (nl + SLOT_LEN - 1) // SLOT_LEN
        if live(i):
            if nl < 1 or nl > 255:
                i += 1
                continue
            if i + slots > nr:
                break
            if ino == 0 or ft > FT_MAX:
                i += slots
                continue
            name = bytes(b[area["names"] + i * SLOT_LEN:area["names"] + i * SLOT_LEN + nl])
            if not (area["first"] and i < 2 and name in (b".", b"..")):
                recs.append({"live": True, "ino": ino, "name": name, "ft": ft, "blk": area["blk"], "slot": i})
            i += slots
            continue
        if ino == 0 or nl < 1 or nl > 255 or ft < 1 or ft > FT_MAX or ino >= natcap or i + slots > nr:
            i += 1
            continue
        name = bytes(b[area["names"] + i * SLOT_LEN:area["names"] + i * SLOT_LEN + nl])
        if name in (b".", b"..") or b"\x00" in name or b"/" in name or any(live(s) for s in range(i + 1, i + slots)):
            i += 1
            continue
        recs.append({"live": False, "ino": ino, "name": name, "ft": ft, "blk": area["blk"], "slot": i})
        i += slots
    return recs


def all_records(img, table):
    """{dir ino: [records]} for every directory of the image."""
    natcap = nat_capacity(img)
    out = {}
    for ino, n in table.items():
        if n["mode"] & 0xF000 == 0x4000:
            out[ino] = [r for ar in dir_areas(img, n) for r in scan_area(ar, natcap)]
    return out


# ---- the stale index (spec 6.5, 3D-R5) and the rules of 3D-R2 to R4, R6, R7

def stale_index(img):
    """({nid: [node dicts, newest first]}, evicted nids): footers of node-segment blocks whose SIT valid bit is
    clear, below the cursor in a current node segment, with a version below the checkpoint's; at most 8 copies
    per nid (the lowest versions are evicted)."""
    cs = img.cp["curseg"]
    cur = {cs["node_segno"][i]: cs["node_blkoff"][i] for i in range(3)}
    below = lo32(img.cp["checkpoint_ver"])
    idx, evicted = {}, set()
    for seg in range(img.sb["segment_count_main"]):
        e = sit_entry(img, seg)
        if e["type"] not in NODE_SEG_TYPES:
            continue
        base = img.sb["main_blkaddr"] + seg * BPS
        for off in range(BPS):
            if seg in cur and off >= cur[seg]:
                break
            if bit_msb(e["map"], off):
                continue
            b = img.blk(base + off)
            nid = u32(b, 4072)
            if nid == 0 or lo32(u64(b, 4084)) >= below:
                continue
            idx.setdefault(nid, []).append(node(img, base + off))
    for nid, lst in idx.items():
        lst.sort(key=lambda n: (-lo32(n["cp_ver"]), n["blk"]))
        if len(lst) > 8:
            evicted.add(nid)
            del lst[8:]
    return idx, evicted


def stale_nodefor(idx):
    def nodefor(nid, owner, ofs):
        cands = [n for n in idx.get(nid, []) if n["ino"] == owner and n["ofs"] == ofs]
        if not cands:
            return None
        tied = [n for n in cands if lo32(n["cp_ver"]) == lo32(cands[0]["cp_ver"])]
        if len({n["raw"] for n in tied}) > 1:
            fail("oracle: node %d has tied copies with different bytes; the expectation is not modelled" % nid)
        return tied[0]
    return nodefor


def classify_deleted(D, rec, idx, evicted, free):
    """The oracle's expectation for one deleted dentry: (expect dict, map blocks)."""
    ex = {"outcome": "skip", "skip": "", "method": "", "captured": [], "cut": False, "prefix": False, "stop": "",
          "equal_source": False, "uniform": False, "confidence": 0}
    ft, ino = rec["ft"], rec["ino"]
    if ft == FT_DIR:
        ex["skip"] = "not-a-file"
        return ex, []
    if ft not in (FT_REG, FT_SYMLINK):
        ex["skip"] = "unsupported"
        return ex, []
    if nat_addr(D, ino) != NULL_ADDR:
        ex["skip"] = "reused"  # the CURRENT NAT maps the inode number (3D-R8, R23, R34)
        return ex, []
    below = lo32(D.cp["checkpoint_ver"])
    qual = []
    for c in idx.get(ino, []):
        if not c["is_inode"] or c["nid"] != ino or c["ino"] != ino or lo32(c["cp_ver"]) >= below or c["csum_ok"] is False:
            continue
        typ = c["mode"] & 0xF000
        if (ft == FT_REG and typ != 0x8000) or (ft == FT_SYMLINK and typ != 0xA000):
            continue
        if c["pino"] != rec["dir"] or bytes(c["raw"][92:92 + min(c["namelen"], 255)]) != rec["name"]:
            continue
        qual.append(c)
    if not qual:
        ex["skip"] = "undecided" if ino in evicted else "not-found"
        return ex, []
    top = lo32(qual[0]["cp_ver"])
    maps = []
    for c in [c for c in qual if lo32(c["cp_ver"]) == top]:
        if c["inline"] & INLINE_DATA:
            ex["skip"] = "unsupported"
            return ex, []
        if c["size"] > D.nblocks * BLK:
            ex["skip"] = "corrupt"
            return ex, []
        need = (c["size"] + BLK - 1) // BLK
        addrs, stop = logical_blocks(c, stale_nodefor(idx), need)
        blocks, st = [], ""
        for lb, ad in enumerate(addrs):
            if is_hole(ad):
                st = "hole@%d" % lb
                break
            if not valid_main(D, ad) or ad in blocks:
                st = ("address-outside-main@%d" if not valid_main(D, ad) else "address-repeated@%d") % lb
                break
            blocks.append(ad)
        if not st and stop:
            st = stop
        maps.append((c["size"], blocks, st))
    if any(m != maps[0] for m in maps[1:]):
        ex["skip"] = "undecided"
        return ex, []
    size, blocks, stop = maps[0]
    if size == 0:
        ex["skip"] = "empty"
        return ex, []
    if stop.startswith("node-"):
        fail("oracle: the stale tree of inode %d stops at %s; the expectation is not modelled" % (ino, stop))
    cap = []
    for b in blocks:
        if not free(b):
            break
        cap.append(b)
    if not cap:
        ex["skip"] = "no-free-bytes"
        return ex, blocks
    ex.update({"outcome": "recover", "method": "f2fs-node-scan", "stop": stop, "prefix": bool(stop),
               "captured": rangeify(cap), "cut": len(cap) < len(blocks)})
    return ex, blocks


def cmd_final(a):
    D = Image(a.image)
    tree, fmap, table, pw_files = stage_checks(D, a.tools, a.stage)
    files = walk_files(D, table, pw_files, fmap, expected_tree(a.expected), a.stage)
    root = D.sb["root_ino"]
    snaps = []
    for sp in a.snaps:
        with open(sp, encoding="utf-8") as f:
            snaps.append(json.load(f))
    nodiscard = a.variant == "nodiscard"

    # free blocks: every main-area block whose SIT valid bit is clear
    free_list = []
    for seg in range(D.sb["segment_count_main"]):
        e = sit_entry(D, seg)
        base = D.sb["main_blkaddr"] + seg * BPS
        free_list += [base + off for off in range(BPS) if not bit_msb(e["map"], off)]
    free_set = set(free_list)

    live = []
    for p, r in sorted(files.items(), key=lambda kv: kv[0].encode()):
        d = r["dec"]
        live.append({"path": p, "size": d["size"], "sha256": r["sha256"], "blocks": d["blocks"], "holes": d["holes"],
                     "indirect": d["indirect"], "inline": d["inline"]})

    # dentries: the live ones are cross-checked with fsck -t, the deleted ones are the entries
    inv = {i: p for p, i in pw_files.items()}
    inv[root] = ""
    live_seen, deleted = {}, []
    for dino, rl in all_records(D, table).items():
        for r in rl:
            r["dir"] = dino
            full = inv[dino] + "/" + r["name"].decode("utf-8", "surrogateescape")
            if r["live"]:
                live_seen[full] = r["ino"]
            else:
                r["path"] = full
                deleted.append(r)
    if live_seen != tree:
        fail("%s: live dentries and fsck.f2fs -t differ: only dentries %r, only fsck %r" % (
            a.stage, sorted(set(live_seen) - set(tree))[:5], sorted(set(tree) - set(live_seen))[:5]))

    idx, evicted = stale_index(D)

    def snap_for(r):
        """The stage record of the inode this dentry names: the same inode number, parent and name (the latest)."""
        best = None
        for s in snaps:
            x = s["files"].get(str(r["ino"]))
            if x and x["pino"] == r["dir"] and x["name"].encode() == r["name"]:
                best = x
        return best

    entries = []
    for r in sorted(deleted, key=lambda r: (r["path"].encode(), r["slot"])):
        s = snap_for(r)
        if s is None:
            fail("deleted entry %s (inode %d) was never written by the generator's stages" % (r["path"], r["ino"]))
        ex, _blocks = classify_deleted(D, r, idx, evicted, lambda b: b in free_set)
        sha = s["sha256"]
        if ex["outcome"] == "recover":
            body = b"".join(D.blk(b) for b in expand(ex["captured"]))[:s["size"]]
            ex["uniform"] = len(body) > 0 and body.count(body[:1]) == len(body)
            ex["equal_source"] = not ex["cut"] and not ex["prefix"] and hashlib.sha256(body).hexdigest() == sha
        entries.append({"path": r["path"], "family": r["path"].split("/")[1], "listed": True, "dir_nid": r["dir"],
                        "ino": r["ino"], "size": s["size"], "sha256": sha, "true_blocks": s["blocks"],
                        "stale": {"addr": s["nat_addr"], "cp_ver_low": lo32(s["cp_ver"]),
                                  "intact": D.blk(s["nat_addr"]).hex() == s["inode_hex"]},
                        "expect": ex, "_nodes_intact": all(hashlib.sha256(D.blk(nd["addr"])).hexdigest() == nd["sha256"]
                                                           for nd in s["nodes"])})

    # confidence: 75, 20 for a cut or prefix, and at most 30 where the captured bytes are another listed entry's
    for i, e in enumerate(entries):
        x = e["expect"]
        if x["outcome"] != "recover":
            continue
        mine = set(expand(x["captured"]))
        shared = any(j != i and o["listed"] and o["expect"]["outcome"] == "recover" and mine & set(expand(o["expect"]["captured"]))
                     for j, o in enumerate(entries))
        base = 20 if (x["cut"] or x["prefix"]) else 75
        x["confidence"] = min(base, 30) if shared else base

    # files of a deleted directory are not listed (the walk does not descend): reported, never expected to be listed
    for fam in a.report_unlisted:
        for s in snaps[0]["files"].values():
            if s["path"].startswith("/%s/" % fam) and s["kind"] == "file" and all(e["ino"] != s["ino"] for e in entries):
                entries.append({"path": s["path"], "family": fam, "listed": False, "dir_nid": s["pino"], "ino": s["ino"],
                                "size": s["size"], "sha256": s["sha256"], "true_blocks": s["blocks"],
                                "stale": {"addr": s["nat_addr"], "cp_ver_low": lo32(s["cp_ver"]),
                                          "intact": D.blk(s["nat_addr"]).hex() == s["inode_hex"]},
                                "expect": {"outcome": "skip", "skip": "not-listed", "method": "", "captured": [], "cut": False,
                                           "prefix": False, "stop": "", "equal_source": False, "uniform": False,
                                           "confidence": 0},
                                "_nodes_intact": True})

    check_shapes(D, entries, snaps, free_set, nodiscard)
    fams = {}
    for e in entries:
        del e["_nodes_intact"]
        fams[e["family"]] = fams.get(e["family"], 0) + 1
    out = {"checkpoint_version": D.cp["checkpoint_ver"], "block_size": BLK, "free_blocks": rangeify(free_list),
           "live": live, "deleted_entry_count": sum(1 for e in entries if e["listed"]), "deleted": entries,
           "families": fams}
    with open(a.out, "w", encoding="utf-8") as f:
        json.dump(out, f)
    print("FINAL %s: %d live files, %d deleted entries (%d listed), families %r" % (
        a.variant, len(live), len(entries), out["deleted_entry_count"], fams))
    for e in entries:
        x = e["expect"]
        print("ENTRY %-34s ino %-4d %-8s %-14s stop=%-8s cut=%d prefix=%d uniform=%d equal=%d conf=%d intact=%d" % (
            e["path"], e["ino"], x["outcome"], x["skip"], x["stop"], x["cut"], x["prefix"], x["uniform"],
            x["equal_source"], x["confidence"], e["stale"]["intact"]))
    D.close()


def check_shapes(D, entries, snaps, free_set, nodiscard):
    """The required families exist and have the shape their name promises; otherwise exit 1 (3D-R10)."""
    by = {}
    for e in entries:
        by.setdefault(e["family"], []).append(e)
    snap_by_ino = {}
    for s in snaps:
        for k, v in s["files"].items():
            snap_by_ino[int(k)] = v  # the latest stage wins

    def need(fam, why):
        fail("family %s missing: %s; re-rule, do not edit the ops" % (fam, why))

    for fam in COMMON_FAMILIES + (G6_FAMILIES if nodiscard else []):
        if not by.get(fam):
            need(fam, "no deleted entry")
    for e in by["contig"]:
        if nodiscard and not (e["stale"]["intact"] and e["_nodes_intact"]):
            need("contig", "the stale inode is not intact")
        if len(e["true_blocks"]) != 1:
            need("contig", "%d extents" % len(e["true_blocks"]))
    for e in by["twofrag"]:
        if len(e["true_blocks"]) < 2:
            need("twofrag", "one extent")
    for e in by["direct"]:
        nids = snap_by_ino[e["ino"]]["i_nid"]
        if nids[0] == 0 or nids[2] != 0:
            need("direct", "i_nid %r has no i_nid[0] or has i_nid[2]" % (nids,))
    for e in by["indirect"]:
        nids = snap_by_ino[e["ino"]]["i_nid"]
        if nids[2] == 0:
            need("indirect", "i_nid %r has no i_nid[2]" % (nids,))
    if nodiscard:
        for fam in ("direct", "indirect"):
            for e in by[fam]:
                if not (e["stale"]["intact"] and e["_nodes_intact"]):
                    need(fam, "the stale tree changed since stage W")
    for e in by["holes"]:
        s = snap_by_ino[e["ino"]]
        if s["holes"] != [[1, 2]] or len(expand(s["blocks"])) != 2:
            need("holes", "blocks %r holes %r" % (s["blocks"], s["holes"]))
    for e in by["inline"]:
        if not snap_by_ino[e["ino"]]["inline"]:
            need("inline", "the file is not stored inline")
    for e in by["empty"]:
        if e["size"] != 0:
            need("empty", "size %d" % e["size"])
    if not any(e["listed"] and e["expect"]["skip"] == "not-a-file" for e in by["gone"]) or not any(not e["listed"] for e in by["gone"]):
        need("gone", "the directory is not skipped as not-a-file or its inner file is listed")
    if nodiscard:
        if sorted(e["path"] for e in by["renamed"]) != ["/renamed/a", "/renamed/b"]:
            need("renamed", "entries %r" % [e["path"] for e in by["renamed"]])
        for e in by["nidreuse"]:
            if e["expect"]["skip"] != "reused":
                need("nidreuse", "the inode number %d is not taken by a later file (%r)" % (e["ino"], e["expect"]))
        for e in by["reuse"]:
            taken = sum(1 for b in expand(e["true_blocks"]) if b not in free_set)
            if taken == 0 or not e["stale"]["intact"] or nat_addr(D, e["ino"]) != NULL_ADDR:
                need("reuse", "%d blocks taken, stale inode intact=%s, inode number live=%s" % (
                    taken, e["stale"]["intact"], nat_addr(D, e["ino"]) != NULL_ADDR))


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("probe")
    p.add_argument("--images", required=True)
    p.add_argument("--tools", required=True)
    for name in ("snap", "final"):
        p = sub.add_parser(name)
        p.add_argument("--image", required=True)
        p.add_argument("--tools", required=True, help="directory of per-stage tool captures")
        p.add_argument("--stage", required=True, help="stage name: the folder of --tools that belongs to --image")
        p.add_argument("--expected", required=True, help="the expected-content tree the generator wrote")
        p.add_argument("--out", required=True)
        if name == "final":
            p.add_argument("--variant", required=True, choices=("nodiscard", "discard"))
            p.add_argument("--snaps", nargs="+", required=True, help="snap files, in stage order")
            p.add_argument("--report-unlisted", nargs="*", default=[], help="families whose files are reported as not listed")
    a = ap.parse_args()
    if a.cmd == "probe":
        probe(a.images, a.tools)
    elif a.cmd == "snap":
        cmd_snap(a)
    else:
        cmd_final(a)


if __name__ == "__main__":
    main()

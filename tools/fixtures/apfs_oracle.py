#!/usr/bin/env python3
"""Independent decoder and cross-checker for the APFS fixtures.

usage: apfs_oracle.py <image> <label> <case_sensitive> <norm_sensitive> <cont_uuid> <vol_uuid>

Prints the oracle JSON on stdout and exits non-zero (message on stderr) when a
cross-check fails, which fails the generator. <case_sensitive> and
<norm_sensitive> are 0/1 and are the mkapfs flags (-s, -z) the image was made
with; <label>, <cont_uuid> and <vol_uuid> are the mkapfs arguments.

This file is written from Apple's "Apple File System Reference" (2020-06-22)
with the stdlib `struct` module. It shares no code with Minutiae and never
imports it. Field layouts are listed in the order of the reference's structure
definitions and the offsets are computed from the formats, so they are checked
against the real bytes rather than copied from anywhere. The only facts the
reference leaves out are the Fletcher-64 constants (implemented below) and the
shape of one mkapfs checkpoint; both are measured, not assumed.

Scope: containers made by mkapfs: one checkpoint, one volume, no files.
Anything the decoder does not understand is an error, never silently skipped.
"""
import json
import struct
import sys
import unicodedata

M32 = 0xFFFFFFFF

# --- object types (reference: Object Types) -------------------------------
TYPE_NAMES = {
    0x1: "nx_superblock", 0x2: "btree", 0x3: "btree_node", 0x5: "spaceman",
    0x6: "spaceman_cab", 0x7: "spaceman_cib", 0x8: "spaceman_bitmap",
    0x9: "spaceman_free_queue", 0xB: "omap", 0xC: "checkpoint_map", 0xD: "fs",
    0xE: "fstree", 0xF: "blockreftree", 0x10: "snapmetatree",
    0x11: "nx_reaper", 0x12: "nx_reap_list", 0x13: "omap_snapshot",
}
OBJ_EPHEMERAL = 0x80000000
OBJ_PHYSICAL = 0x40000000
OBJ_NOHEADER = 0x20000000
OBJ_ENCRYPTED = 0x10000000
OBJ_NONPERSISTENT = 0x08000000

# j_obj_types
FS_TYPE_NAMES = {
    0: "any", 1: "snap_metadata", 2: "extent", 3: "inode", 4: "xattr",
    5: "sibling_link", 6: "dstream_id", 7: "crypto_state", 8: "file_extent",
    9: "dir_rec", 10: "dir_stats", 11: "snap_name", 12: "sibling_map",
    13: "file_info",
}
XF_INODE_NAMES = {
    1: "snap_xid", 2: "delta_tree_oid", 3: "document_id", 4: "name",
    5: "prev_fsize", 7: "finder_info", 8: "dstream", 10: "dir_stats_key",
    11: "fs_uuid", 13: "sparse_bytes", 14: "rdev",
}
DREC_TYPES = {1: "fifo", 2: "chr", 4: "dir", 6: "blk", 8: "reg", 10: "lnk",
              12: "sock", 14: "wht"}


class OracleError(Exception):
    pass


def fail(msg):
    raise OracleError(msg)


# --- Fletcher-64 (NOT given by the reference; measured against real blocks) -
def fletcher64(buf):
    """Checksum of an object: Fletcher-64 over the bytes after the first 8,
    as little-endian u32 words, modulo 2^32-1, then the two check words that
    make the full-object sums vanish."""
    if len(buf) % 4 or len(buf) < 12:
        fail("bad object length for a checksum")
    words = struct.unpack("<%dI" % ((len(buf) - 8) // 4), buf[8:])
    s1 = s2 = 0
    for w in words:
        s1 = (s1 + w) % M32
        s2 = (s2 + s1) % M32
    c1 = M32 - ((s1 + s2) % M32)
    c2 = M32 - ((s1 + c1) % M32)
    return (c2 << 32) | c1


# --- CRC-32C for the directory-record name hash ------------------------------
def _crc32c_table():
    poly = 0x82F63B78
    t = []
    for i in range(256):
        c = i
        for _ in range(8):
            c = (c >> 1) ^ poly if c & 1 else c >> 1
        t.append(c)
    return t


_CRC = _crc32c_table()


def crc32c_raw(data):
    """CRC-32C with initial value all ones and NO final complement (the
    reference lets an implementation drop both complements)."""
    c = M32
    for b in data:
        c = _CRC[(c ^ b) & 0xFF] ^ (c >> 8)
    return c


def drec_hash(name, case_fold):
    """Hash recipe from the reference: NFD, UTF-32, CRC-32C, low 22 bits.
    Returns {variant: hash22} for the two readings of "null-terminated"."""
    n = unicodedata.normalize("NFD", name)
    if case_fold:
        n = unicodedata.normalize("NFD", n.casefold())
    out = {}
    for variant, s in (("with_nul", n + "\0"), ("without_nul", n)):
        raw = b"".join(struct.pack("<I", ord(ch)) for ch in s)
        out[variant] = crc32c_raw(raw) & 0x3FFFFF
    return out


# --- structure layouts: (name, struct format), in the reference's order ------
def layout(fields, start=0):
    """Return ({name: (offset, fmt)}, end) for a list of (name, fmt)."""
    out = {}
    off = start
    for name, fmt in fields:
        out[name] = (off, fmt)
        off += struct.calcsize("<" + fmt)
    return out, off


def decode(buf, lay, base=0):
    d = {}
    for name, (off, fmt) in lay.items():
        if off + struct.calcsize("<" + fmt) > len(buf):
            fail("structure runs past its buffer at %s" % name)
        v = struct.unpack_from("<" + fmt, buf, base + off)
        d[name] = v[0] if len(v) == 1 else list(v)
    return d


OBJ_PHYS = [("o_cksum", "Q"), ("o_oid", "Q"), ("o_xid", "Q"), ("o_type", "I"),
            ("o_subtype", "I")]
NXSB, NXSB_END = layout(OBJ_PHYS + [
    ("nx_magic", "I"), ("nx_block_size", "I"), ("nx_block_count", "Q"),
    ("nx_features", "Q"), ("nx_readonly_compatible_features", "Q"),
    ("nx_incompatible_features", "Q"), ("nx_uuid", "16s"),
    ("nx_next_oid", "Q"), ("nx_next_xid", "Q"),
    ("nx_xp_desc_blocks", "I"), ("nx_xp_data_blocks", "I"),
    ("nx_xp_desc_base", "Q"), ("nx_xp_data_base", "Q"),
    ("nx_xp_desc_next", "I"), ("nx_xp_data_next", "I"),
    ("nx_xp_desc_index", "I"), ("nx_xp_desc_len", "I"),
    ("nx_xp_data_index", "I"), ("nx_xp_data_len", "I"),
    ("nx_spaceman_oid", "Q"), ("nx_omap_oid", "Q"), ("nx_reaper_oid", "Q"),
    ("nx_test_type", "I"), ("nx_max_file_systems", "I"),
    ("nx_fs_oid", "100Q"), ("nx_counters", "32Q"),
    ("nx_blocked_out_prange", "2Q"), ("nx_evict_mapping_tree_oid", "Q"),
    ("nx_flags", "Q"), ("nx_efi_jumpstart", "Q"), ("nx_fusion_uuid", "16s"),
    ("nx_keylocker", "2Q"), ("nx_ephemeral_info", "4Q"), ("nx_test_oid", "Q"),
    ("nx_fusion_mt_oid", "Q"), ("nx_fusion_wbc_oid", "Q"),
    ("nx_fusion_wbc", "2Q"), ("nx_newest_mounted_version", "Q"),
    ("nx_mkb_locker", "2Q"),
])
CPM, CPM_END = layout(OBJ_PHYS + [("cpm_flags", "I"), ("cpm_count", "I")])
CPMAP, CPMAP_SIZE = layout([
    ("cpm_type", "I"), ("cpm_subtype", "I"), ("cpm_size", "I"),
    ("cpm_pad", "I"), ("cpm_fs_oid", "Q"), ("cpm_oid", "Q"),
    ("cpm_paddr", "Q")])
OMAP, _ = layout(OBJ_PHYS + [
    ("om_flags", "I"), ("om_snap_count", "I"), ("om_tree_type", "I"),
    ("om_snapshot_tree_type", "I"), ("om_tree_oid", "Q"),
    ("om_snapshot_tree_oid", "Q"), ("om_most_recent_snap", "Q"),
    ("om_pending_revert_min", "Q"), ("om_pending_revert_max", "Q")])
# The modified_by array is 8 x (32s, Q, Q); struct has no nested repeat, so the
# volume superblock is laid out with the history expanded.
_APSB_FIELDS = OBJ_PHYS + [
    ("apfs_magic", "I"), ("apfs_fs_index", "I"), ("apfs_features", "Q"),
    ("apfs_readonly_compatible_features", "Q"),
    ("apfs_incompatible_features", "Q"), ("apfs_unmount_time", "Q"),
    ("apfs_fs_reserve_block_count", "Q"), ("apfs_fs_quota_block_count", "Q"),
    ("apfs_fs_alloc_count", "Q"),
    ("wmcs_major_version", "H"), ("wmcs_minor_version", "H"),
    ("wmcs_cpflags", "I"), ("wmcs_persistent_class", "I"),
    ("wmcs_key_os_version", "I"), ("wmcs_key_revision", "H"),
    ("wmcs_unused", "H"),
    ("apfs_root_tree_type", "I"), ("apfs_extentref_tree_type", "I"),
    ("apfs_snap_meta_tree_type", "I"), ("apfs_omap_oid", "Q"),
    ("apfs_root_tree_oid", "Q"), ("apfs_extentref_tree_oid", "Q"),
    ("apfs_snap_meta_tree_oid", "Q"), ("apfs_revert_to_xid", "Q"),
    ("apfs_revert_to_sblock_oid", "Q"), ("apfs_next_obj_id", "Q"),
    ("apfs_num_files", "Q"), ("apfs_num_directories", "Q"),
    ("apfs_num_symlinks", "Q"), ("apfs_num_other_fsobjects", "Q"),
    ("apfs_num_snapshots", "Q"), ("apfs_total_blocks_alloced", "Q"),
    ("apfs_total_blocks_freed", "Q"), ("apfs_vol_uuid", "16s"),
    ("apfs_last_mod_time", "Q"), ("apfs_fs_flags", "Q"),
    ("formatted_by_id", "32s"), ("formatted_by_timestamp", "Q"),
    ("formatted_by_last_xid", "Q"),
]
for _i in range(8):  # APFS_MAX_HIST
    _APSB_FIELDS += [("modified_by_%d_id" % _i, "32s"),
                     ("modified_by_%d_timestamp" % _i, "Q"),
                     ("modified_by_%d_last_xid" % _i, "Q")]
_APSB_FIELDS += [
    ("apfs_volname", "256s"), ("apfs_next_doc_id", "I"), ("apfs_role", "H"),
    ("reserved", "H"), ("apfs_root_to_xid", "Q"), ("apfs_er_state_oid", "Q"),
    ("apfs_cloneinfo_id_epoch", "Q"), ("apfs_cloneinfo_xid", "Q"),
    ("apfs_snap_meta_ext_oid", "Q"), ("apfs_volume_group_id", "16s"),
    ("apfs_integrity_meta_oid", "Q"), ("apfs_fext_tree_oid", "Q"),
    ("apfs_fext_tree_type", "I"),
]
APSB, _ = layout(_APSB_FIELDS)

SPACEMAN_DEV = [("sm_block_count", "Q"), ("sm_chunk_count", "Q"),
                ("sm_cib_count", "I"), ("sm_cab_count", "I"),
                ("sm_free_count", "Q"), ("sm_addr_offset", "I"),
                ("sm_reserved", "I"), ("sm_reserved2", "Q")]
SPACEMAN_FQ = [("sfq_count", "Q"), ("sfq_tree_oid", "Q"),
               ("sfq_oldest_xid", "Q"), ("sfq_tree_node_limit", "H"),
               ("sfq_pad16", "H"), ("sfq_pad32", "I"), ("sfq_reserved", "Q")]
_SM = OBJ_PHYS + [("sm_block_size", "I"), ("sm_blocks_per_chunk", "I"),
                  ("sm_chunks_per_cib", "I"), ("sm_cibs_per_cab", "I")]
for _d in range(2):  # SD_COUNT
    _SM += [("sm_dev%d_%s" % (_d, n[3:]), f) for n, f in SPACEMAN_DEV]
_SM += [("sm_flags", "I"), ("sm_ip_bm_tx_multiplier", "I"),
        ("sm_ip_block_count", "Q"), ("sm_ip_bm_size_in_blocks", "I"),
        ("sm_ip_bm_block_count", "I"), ("sm_ip_bm_base", "Q"),
        ("sm_ip_base", "Q"), ("sm_fs_reserve_block_count", "Q"),
        ("sm_fs_reserve_alloc_count", "Q")]
for _q in range(3):  # SFQ_COUNT
    _SM += [("sm_fq%d_%s" % (_q, n[4:]), f) for n, f in SPACEMAN_FQ]
_SM += [("sm_ip_bm_free_head", "H"), ("sm_ip_bm_free_tail", "H"),
        ("sm_ip_bm_xid_offset", "I"), ("sm_ip_bitmap_offset", "I"),
        ("sm_ip_bm_free_next_offset", "I"), ("sm_version", "I"),
        ("sm_struct_size", "I")]
SM, SM_END = layout(_SM)

CIB, CIB_END = layout(OBJ_PHYS + [("cib_index", "I"),
                                  ("cib_chunk_info_count", "I")])
CHUNK_INFO, CHUNK_INFO_SIZE = layout([
    ("ci_xid", "Q"), ("ci_addr", "Q"), ("ci_block_count", "I"),
    ("ci_free_count", "I"), ("ci_bitmap_addr", "Q")])
CAB, CAB_END = layout(OBJ_PHYS + [("cab_index", "I"), ("cab_cib_count", "I")])
REAPER, _ = layout(OBJ_PHYS + [
    ("nr_next_reap_id", "Q"), ("nr_completed_id", "Q"), ("nr_head", "Q"),
    ("nr_tail", "Q"), ("nr_flags", "I"), ("nr_rlcount", "I"),
    ("nr_type", "I"), ("nr_size", "I"), ("nr_fs_oid", "Q"), ("nr_oid", "Q"),
    ("nr_xid", "Q"), ("nr_nrle_flags", "I"), ("nr_state_buffer_size", "I")])
BTN, BTN_DATA = layout(OBJ_PHYS + [
    ("btn_flags", "H"), ("btn_level", "H"), ("btn_nkeys", "I"),
    ("ts_off", "H"), ("ts_len", "H"), ("free_off", "H"), ("free_len", "H"),
    ("kfl_off", "H"), ("kfl_len", "H"), ("vfl_off", "H"), ("vfl_len", "H")])
BT_INFO, BT_INFO_SIZE = layout([
    ("bt_flags", "I"), ("bt_node_size", "I"), ("bt_key_size", "I"),
    ("bt_val_size", "I"), ("bt_longest_key", "I"), ("bt_longest_val", "I"),
    ("bt_key_count", "Q"), ("bt_node_count", "Q")])
J_INODE_VAL, J_INODE_VAL_SIZE = layout([
    ("parent_id", "Q"), ("private_id", "Q"), ("create_time", "Q"),
    ("mod_time", "Q"), ("change_time", "Q"), ("access_time", "Q"),
    ("internal_flags", "Q"), ("nchildren_or_nlink", "i"),
    ("default_protection_class", "I"), ("write_generation_counter", "I"),
    ("bsd_flags", "I"), ("owner", "I"), ("group", "I"), ("mode", "H"),
    ("pad1", "H"), ("uncompressed_size", "Q")])
J_DSTREAM, J_DSTREAM_SIZE = layout([
    ("size", "Q"), ("alloced_size", "Q"), ("default_crypto_id", "Q"),
    ("total_bytes_written", "Q"), ("total_bytes_read", "Q")])
J_DREC_VAL, J_DREC_VAL_SIZE = layout([("file_id", "Q"), ("date_added", "Q"),
                                      ("flags", "H")])

assert NXSB_END <= 1344 + 8 * 8 + 64, NXSB_END  # sanity: layout is the long form


def uuid_str(b):
    h = b.hex()
    return "%s-%s-%s-%s-%s" % (h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])


def cstr(b):
    i = b.find(b"\0")
    if i < 0:
        fail("unterminated string")
    return b[:i].decode("utf-8")


def iso(ns):
    import datetime
    if ns == 0:
        return None
    s, n = divmod(ns, 10**9)
    return datetime.datetime.fromtimestamp(s, datetime.timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%S") + (".%09d" % n) + "Z"


class Image:
    def __init__(self, path):
        self.f = open(path, "rb")
        self.f.seek(0, 2)
        self.size = self.f.tell()
        self.f.seek(0)
        head = self.f.read(4096)
        if len(head) < 4096 or head[32:36] != b"NXSB":
            fail("block 0 is not an NXSB superblock")
        self.bs = struct.unpack_from("<I", head, 36)[0]
        if self.bs < 4096 or self.bs > 65536 or self.bs & (self.bs - 1):
            fail("implausible block size %d" % self.bs)
        self.blocks = self.size // self.bs
        self.reached = {}  # block -> description, objects whose checksum was verified
        self.raw = {}      # block -> description, raw (header-less) blocks

    def read(self, blk, n=1):
        if blk < 0 or blk + n > self.blocks:
            fail("block %d+%d outside the image" % (blk, n))
        self.f.seek(blk * self.bs)
        b = self.f.read(n * self.bs)
        if len(b) != n * self.bs:
            fail("short read at block %d" % blk)
        return b

    def nonzero_blocks(self):
        zero = bytes(self.bs)
        chunk = 256
        blk = 0
        while blk < self.blocks:
            n = min(chunk, self.blocks - blk)
            data = self.read(blk, n)
            if data.count(0) != len(data):
                for i in range(n):
                    if data[i * self.bs:(i + 1) * self.bs] != zero:
                        yield blk + i
            blk += n

    def obj(self, blk, nblocks=1, what=""):
        """Read an object, verify its Fletcher-64, record it as reached."""
        b = self.read(blk, nblocks)
        stored = struct.unpack_from("<Q", b, 0)[0]
        want = fletcher64(b)
        if stored != want:
            fail("Fletcher-64 of block %d (%s): stored %016x, computed %016x"
                 % (blk, what, stored, want))
        for i in range(nblocks):
            self.reached[blk + i] = what or "object"
        return b


def header(b):
    d = decode(b, layout(OBJ_PHYS)[0])
    d["type"] = d["o_type"] & 0xFFFF
    d["flags"] = d["o_type"] & 0xFFFF0000
    d["type_name"] = TYPE_NAMES.get(d["type"], "type_%#x" % d["type"])
    return d


def hdr_summary(h):
    return {"oid": h["o_oid"], "xid": h["o_xid"], "type": h["type"],
            "type_name": h["type_name"], "storage": storage(h["flags"]),
            "o_type": h["o_type"], "o_subtype": h["o_subtype"]}


def storage(flags):
    s = flags & 0xC0000000
    return {0: "virtual", OBJ_PHYSICAL: "physical",
            OBJ_EPHEMERAL: "ephemeral"}.get(s, "invalid")


def pop_zero_bits(bitmap, nbits):
    """Number of zero bits among the first nbits (bit i = byte i//8, mask
    1 << (i % 8)); polarity and order are asserted by the caller's checks."""
    nfull, rem = divmod(nbits, 8)
    zeros = sum(8 - bin(b).count("1") for b in bitmap[:nfull])
    if rem:
        zeros += sum(1 for i in range(rem) if not bitmap[nfull] & (1 << i))
    return zeros


def ranges_from_bits(bitmap, base, nbits):
    """Inclusive [first,last] block ranges of the zero (free) bits."""
    out = []
    start = None
    for i in range(nbits):
        free = not bitmap[i // 8] & (1 << (i % 8))
        if free and start is None:
            start = i
        elif not free and start is not None:
            out.append([base + start, base + i - 1])
            start = None
    if start is not None:
        out.append([base + start, base + nbits - 1])
    return out


def merge_ranges(rs):
    out = []
    for a, b in sorted(rs):
        if out and a <= out[-1][1] + 1:
            out[-1][1] = max(out[-1][1], b)
        else:
            out.append([a, b])
    return out


# --- B-trees ----------------------------------------------------------------
class Tree:
    """A decoded B-tree: nodes (for the oracle) and leaf records (raw)."""

    def __init__(self, img, root_blk, expect_subtype, resolve, what, root_buf=None,
                 root_what_blk=None,
                 allow_ghosts=False):
        self.img = img
        self.what = what
        self.expect_subtype = expect_subtype
        self.resolve = resolve  # oid -> block, for the tree's child pointers
        self.nodes = []
        self.records = []
        self.info = None
        self.root_blk = root_blk
        self.allow_ghosts = allow_ghosts
        self._walk(root_blk, 0, root_buf)

    def _walk(self, blk, depth, buf=None):
        if depth > 16:
            fail("%s: B-tree deeper than 16" % self.what)
        img = self.img
        if buf is None:
            buf = img.obj(blk, 1, "%s node" % self.what)
        else:
            img.reached[blk] = "%s node" % self.what
        h = header(buf)
        n = decode(buf, BTN)
        is_root = bool(n["btn_flags"] & 1)
        is_leaf = bool(n["btn_flags"] & 2)
        fixed = bool(n["btn_flags"] & 4)
        if depth == 0 and not is_root:
            fail("%s: first node is not a root" % self.what)
        if depth > 0 and is_root:
            fail("%s: root flag on a child" % self.what)
        want_type = 2 if is_root else 3
        if h["type"] != want_type:
            fail("%s: node type %d, want %d" % (self.what, h["type"], want_type))
        if h["o_subtype"] != self.expect_subtype:
            fail("%s: node subtype %#x, want %#x"
                 % (self.what, h["o_subtype"], self.expect_subtype))
        if (n["btn_level"] == 0) != is_leaf:
            fail("%s: level/leaf flag disagree" % self.what)
        bs = img.bs
        valend = bs - (BT_INFO_SIZE if is_root else 0)
        info = None
        if is_root:
            info = decode(buf, BT_INFO, valend)
            if depth == 0:
                self.info = info
            if info["bt_node_size"] != bs:
                fail("%s: bt_node_size %d != block size" % (self.what, info["bt_node_size"]))
        data = BTN_DATA
        ts_off, ts_len = n["ts_off"], n["ts_len"]
        toc = data + ts_off
        keyarea = toc + ts_len
        esz = 4 if fixed else 8
        if n["btn_nkeys"] * esz > ts_len:
            fail("%s: ToC too small" % self.what)
        node = {"block": blk, "oid": h["o_oid"], "xid": h["o_xid"],
                "type_name": h["type_name"], "storage": storage(h["flags"]),
                "flags": n["btn_flags"], "level": n["btn_level"],
                "nkeys": n["btn_nkeys"], "table_space": [ts_off, ts_len],
                "free_space": [n["free_off"], n["free_len"]],
                "key_free_list": [n["kfl_off"], n["kfl_len"]],
                "val_free_list": [n["vfl_off"], n["vfl_len"]]}
        if info is not None:
            node["btree_info"] = info
        self.nodes.append(node)
        for i in range(n["btn_nkeys"]):
            if fixed:
                k_off, v_off = struct.unpack_from("<HH", buf, toc + i * 4)
                k_len, v_len = self.info["bt_key_size"], self.info["bt_val_size"]
            else:
                k_off, k_len, v_off, v_len = struct.unpack_from("<HHHH", buf, toc + i * 8)
            if v_off == 0xFFFF:
                if self.allow_ghosts:
                    continue  # a deleted record whose space is kept (driver output)
                fail("%s: ghost entry (unexpected in mkapfs output)" % self.what)
            ks = keyarea + k_off
            vs = valend - v_off
            if ks < keyarea or ks + k_len > bs or vs < 0 or vs + v_len > valend:
                fail("%s: entry %d outside its node" % (self.what, i))
            key = buf[ks:ks + k_len]
            val = buf[vs:vs + v_len]
            if is_leaf:
                self.records.append((key, val))
            else:
                child = struct.unpack_from("<Q", val, 0)[0]
                self._walk(self.resolve(child), depth + 1)


def omap_lookup(entries, oid, xid):
    best = None
    for e in entries:
        if e["oid"] == oid and e["xid"] <= xid and (best is None or e["xid"] > best["xid"]):
            best = e
    return best


def decode_omap(img, blk, what, xid_limit):
    b = img.obj(blk, 1, what + " omap")
    h = header(b)
    if h["type"] != 0xB:
        fail("%s: not an omap" % what)
    om = decode(b, OMAP)
    out = {"block": blk, "header": hdr_summary(h)}
    for k in ("om_flags", "om_snap_count", "om_tree_type", "om_snapshot_tree_type",
              "om_tree_oid", "om_snapshot_tree_oid", "om_most_recent_snap",
              "om_pending_revert_min", "om_pending_revert_max"):
        out[k] = om[k]
    ttype = om["om_tree_type"]
    if ttype & 0xFFFF != 2:
        fail("%s: omap tree type %#x is not a B-tree" % (what, ttype))
    if (ttype & 0xC0000000) != OBJ_PHYSICAL:
        fail("%s: virtual omap trees do not occur in mkapfs output" % what)
    tree = Tree(img, om["om_tree_oid"], 0xB, lambda o: o, what + " omap tree")
    entries = []
    for key, val in tree.records:
        oid, xid = struct.unpack("<QQ", key)
        flags, size, paddr = struct.unpack("<IIQ", val)
        entries.append({"oid": oid, "xid": xid, "flags": flags, "size": size,
                        "paddr": paddr})
    out["tree"] = {"root_block": om["om_tree_oid"], "nodes": tree.nodes}
    out["entries"] = entries
    if om["om_snapshot_tree_oid"] != 0 or om["om_snap_count"] != 0:
        fail("%s: omap snapshots present (unexpected)" % what)
    return out


# --- file-system records ------------------------------------------------------
def decode_xfields(blob, vol_ci):
    """Extended fields after a fixed record part. Returns a list of dicts."""
    if not blob:
        return []
    if len(blob) < 4:
        fail("truncated xfield blob")
    num, used = struct.unpack_from("<HH", blob, 0)
    table = 4
    data = table + num * 4
    if data > len(blob) or data + used > len(blob):
        fail("xfield blob does not fit")
    out = []
    pos = data
    for i in range(num):
        t, fl, sz = struct.unpack_from("<BBH", blob, table + i * 4)
        val = blob[pos:pos + sz]
        if len(val) != sz:
            fail("xfield value does not fit")
        e = {"type": t, "name": XF_INODE_NAMES.get(t, "type_%d" % t),
             "flags": fl, "size": sz}
        if t == 8:
            e["dstream"] = decode(val, J_DSTREAM)
        elif t == 4:
            e["value"] = cstr(val)
        elif t == 10 and sz == 8:
            e["hex"] = val.hex()
        else:
            e["hex"] = val.hex()
        out.append(e)
        pos += (sz + 7) & ~7
    if pos - data != used:
        fail("xfield used_data %d != consumed %d" % (used, pos - data))
    return out


def decode_fs_record(key, val, case_insensitive, hashed):
    if len(key) < 8:
        fail("fs key shorter than j_key_t")
    raw = struct.unpack_from("<Q", key, 0)[0]
    ino, typ = raw & 0x0FFFFFFFFFFFFFFF, raw >> 60
    rec = {"id": ino, "type": typ, "type_name": FS_TYPE_NAMES.get(typ, "type_%d" % typ),
           "key_length": len(key), "value_length": len(val)}
    if typ == 3:  # inode
        if len(val) < J_INODE_VAL_SIZE:
            fail("inode value shorter than j_inode_val_t")
        d = decode(val, J_INODE_VAL)
        if len(key) != 8:
            fail("inode key is not 8 bytes")
        rec["inode"] = {
            "parent_id": d["parent_id"], "private_id": d["private_id"],
            "create_time": d["create_time"], "mod_time": d["mod_time"],
            "change_time": d["change_time"], "access_time": d["access_time"],
            "create_time_iso": iso(d["create_time"]),
            "internal_flags": d["internal_flags"],
            "nchildren_or_nlink": d["nchildren_or_nlink"],
            "default_protection_class": d["default_protection_class"],
            "write_generation_counter": d["write_generation_counter"],
            "bsd_flags": d["bsd_flags"], "owner": d["owner"], "group": d["group"],
            "mode": d["mode"], "mode_octal": "%o" % d["mode"],
            "uncompressed_size": d["uncompressed_size"],
            "xfields": decode_xfields(val[J_INODE_VAL_SIZE:], case_insensitive)}
        rec["xfield_types"] = [x["type"] for x in rec["inode"]["xfields"]]
    elif typ == 9:  # directory record
        # Which key form a volume uses is not stated by the reference; measured
        # on mkapfs output: hashed (j_drec_hashed_key_t) on normalization-
        # insensitive volumes, plain (j_drec_key_t) on normalization-sensitive
        # ones. The decoder demands exactly that form and a consistent key.
        if hashed:
            if len(key) < 13:
                fail("drec key too short")
            nlh = struct.unpack_from("<I", key, 8)[0]
            nlen = nlh & 0x3FF
            # The reference prints J_DREC_HASH_MASK as 0xfffff400, which drops
            # bit 11 of the 22-bit hash; the stored hashes only reproduce with
            # the full field (nlh >> 10, i.e. mask 0xfffffc00). Measured.
            hashv = nlh >> 10
            name_b = key[12:]
        else:
            if len(key) < 11:
                fail("drec key too short")
            nlen = struct.unpack_from("<H", key, 8)[0]
            hashv = None
            name_b = key[10:]
        if len(name_b) != nlen or not name_b.endswith(b"\0"):
            fail("drec key is not a self-consistent %s key" % ("hashed" if hashed else "plain"))
        name = name_b[:-1].decode("utf-8")
        if len(val) < J_DREC_VAL_SIZE:
            fail("drec value too short")
        d = decode(val, J_DREC_VAL)
        rec["drec"] = {
            "parent_id": ino, "name": name,
            "key_form": "hashed" if hashed else "plain",
            "name_len_with_nul": nlen,
            "file_id": d["file_id"], "date_added": d["date_added"],
            "flags": d["flags"], "dirent_type": DREC_TYPES.get(d["flags"] & 0xF, "?"),
            "xfields": decode_xfields(val[J_DREC_VAL_SIZE:], case_insensitive)}
        if hashed:
            want = drec_hash(name, case_insensitive)
            rec["drec"]["name_hash"] = hashv
            rec["drec"]["hash_matches"] = {k: v == hashv for k, v in want.items()}
    elif typ == 4:  # xattr
        nlen = struct.unpack_from("<H", key, 8)[0]
        name_b = key[10:]
        if len(name_b) != nlen or not name_b.endswith(b"\0"):
            fail("xattr key is not self-consistent")
        flags, xlen = struct.unpack_from("<HH", val, 0)
        rec["xattr"] = {"name": name_b[:-1].decode("utf-8"), "flags": flags,
                        "xdata_len": xlen}
    return rec


# --- the oracle ---------------------------------------------------------------
def main():
    if len(sys.argv) != 7:
        print(__doc__, file=sys.stderr)
        return 2
    path, label, case_s, norm_s, cont_uuid, vol_uuid = sys.argv[1:7]
    case_sensitive = case_s in ("1", "true", "True")
    norm_sensitive = norm_s in ("1", "true", "True")
    try:
        out = oracle(path, label, case_sensitive, norm_sensitive, cont_uuid, vol_uuid)
    except OracleError as e:
        print("apfs_oracle: FAIL: %s" % e, file=sys.stderr)
        return 1
    json.dump(out, sys.stdout, indent=1)
    sys.stdout.write("\n")
    return 0


def oracle(path, label, case_sensitive, norm_sensitive, cont_uuid, vol_uuid):
    img = Image(path)
    bs = img.bs
    checks = []  # human-readable list of the cross-checks that passed

    def check(cond, text):
        if not cond:
            fail("check failed: " + text)
        checks.append(text)

    # ---- block 0 and the checkpoint descriptor area ----
    b0 = img.read(0)
    sb0 = decode(b0, NXSB)
    check(sb0["nx_magic"] == 0x4253584E, "nx_magic is 'BSXN' (bytes NXSB at offset 32)")
    check(bs == sb0["nx_block_size"], "block size read from block 0")
    block_count = sb0["nx_block_count"]
    check(block_count * bs <= img.size, "container fits the image (%d blocks)" % block_count)
    check(block_count * bs == img.size, "image size is exactly block_count * block_size")
    desc_blocks, data_blocks = sb0["nx_xp_desc_blocks"], sb0["nx_xp_data_blocks"]
    check(not (desc_blocks & 0x80000000 or data_blocks & 0x80000000),
          "descriptor and data areas are contiguous (high bit clear)")
    desc_base, data_base = sb0["nx_xp_desc_base"], sb0["nx_xp_data_base"]
    check(desc_base + desc_blocks <= data_base or data_base + data_blocks <= desc_base,
          "descriptor and data areas do not overlap")

    ring = []
    sb_candidates = []
    for i in range(desc_blocks):
        blk = desc_base + i
        b = img.read(blk)
        if b.count(0) == len(b):
            ring.append({"index": i, "block": blk, "kind": "zero"})
            continue
        b = img.obj(blk, 1, "checkpoint descriptor %d" % i)
        h = header(b)
        if h["type"] == 1:
            sb = decode(b, NXSB)
            ring.append({"index": i, "block": blk, "kind": "superblock",
                         "oid": h["o_oid"], "xid": h["o_xid"], "o_type": h["o_type"],
                         "identical_to_block0": b == b0})
            sb_candidates.append((h["o_xid"], i, sb, b))
        elif h["type"] == 0xC:
            m = decode(b, CPM)
            maps = []
            for j in range(m["cpm_count"]):
                mp = decode(b, CPMAP, CPM_END + j * CPMAP_SIZE)
                maps.append(mp)
            ring.append({"index": i, "block": blk, "kind": "map", "oid": h["o_oid"],
                         "xid": h["o_xid"], "o_type": h["o_type"],
                         "flags": m["cpm_flags"], "count": m["cpm_count"],
                         "mappings": maps})
        else:
            fail("unexpected object type %d in the descriptor area" % h["type"])
    check(sb_candidates, "the descriptor area holds at least one superblock")
    newest_xid, sb_idx, sb, sbbuf = max(sb_candidates, key=lambda t: t[0])
    check(sbbuf == b0, "block 0 is a byte-identical copy of the newest checkpoint superblock")
    hdr0 = header(b0)
    check(img.obj(0, 1, "block 0 superblock") == b0, "block 0 checksum verifies")
    check(hdr0["o_oid"] == 1 and hdr0["flags"] == OBJ_EPHEMERAL and hdr0["type"] == 1,
          "superblock header: oid 1, ephemeral, type nx_superblock")

    # which blocks belong to the checkpoint: [desc_index, desc_index+desc_len)
    di, dl = sb["nx_xp_desc_index"], sb["nx_xp_desc_len"]
    members = [(di + k) % desc_blocks for k in range(dl)]
    check(members[-1] == sb_idx, "the checkpoint's last descriptor block is its superblock")
    maps_of_cp = [r for r in ring if r["index"] in members[:-1]]
    check(len(maps_of_cp) == dl - 1 and all(r["kind"] == "map" for r in maps_of_cp),
          "the dl-1 blocks before the superblock are checkpoint-map blocks")
    check(all(r["xid"] == newest_xid for r in maps_of_cp), "map blocks carry the superblock's xid")
    check(maps_of_cp[-1]["flags"] & 1 == 1, "the last map block has CHECKPOINT_MAP_LAST")
    check(sb["nx_xp_desc_next"] == (di + dl) % desc_blocks,
          "nx_xp_desc_next follows the checkpoint")
    check(all(r["kind"] == "zero" or r["index"] in members for r in ring),
          "no other descriptor block is in use (a single checkpoint)")

    mappings = [mp for r in maps_of_cp for mp in r["mappings"]]
    data_len, data_idx = sb["nx_xp_data_len"], sb["nx_xp_data_index"]
    data_objs = []
    mapped_blocks = 0
    for mp in mappings:
        nb = mp["cpm_size"] // bs
        check(mp["cpm_size"] % bs == 0, "mapping size %d is a block multiple" % mp["cpm_size"])
        check(data_base <= mp["cpm_paddr"] and mp["cpm_paddr"] + nb <= data_base + data_blocks,
              "mapping paddr %d lies in the data area" % mp["cpm_paddr"])
        buf = img.obj(mp["cpm_paddr"], nb, "ephemeral %#x" % mp["cpm_oid"])
        h = header(buf)
        check(h["o_oid"] == mp["cpm_oid"] and h["o_type"] == mp["cpm_type"]
              and h["o_subtype"] == mp["cpm_subtype"] and h["o_xid"] == newest_xid,
              "ephemeral object at %d matches its mapping (oid %#x)"
              % (mp["cpm_paddr"], mp["cpm_oid"]))
        data_objs.append({"block": mp["cpm_paddr"], "blocks": nb, "oid": h["o_oid"],
                          "xid": h["o_xid"], "type": h["type"],
                          "type_name": h["type_name"], "subtype": h["o_subtype"],
                          "o_type": h["o_type"], "mapping": mp})
        mapped_blocks += nb
    check(mapped_blocks == data_len, "the mapped objects exactly fill nx_xp_data_len")
    check(sb["nx_xp_data_next"] == (data_idx + data_len) % data_blocks,
          "nx_xp_data_next follows the data run")

    # ---- container fields ----
    check(uuid_str(sb["nx_uuid"]) == cont_uuid.lower(), "container uuid equals the -U argument")
    check(sb["nx_max_file_systems"] >= 1, "nx_max_file_systems >= 1")
    fs_oids = [o for o in sb["nx_fs_oid"] if o]
    check(len(fs_oids) == 1 and sb["nx_fs_oid"][0] == fs_oids[0], "exactly one volume, in slot 0")
    check(sb["nx_incompatible_features"] & 2 != 0 and sb["nx_incompatible_features"] & 0x101 == 0,
          "NX_INCOMPAT_VERSION2 set; v1 and Fusion clear")
    container = {
        "block_size": bs, "block_count": block_count, "uuid": uuid_str(sb["nx_uuid"]),
        "features": sb["nx_features"],
        "readonly_compatible_features": sb["nx_readonly_compatible_features"],
        "incompatible_features": sb["nx_incompatible_features"],
        "flags": sb["nx_flags"], "next_oid": sb["nx_next_oid"], "next_xid": sb["nx_next_xid"],
        "max_file_systems": sb["nx_max_file_systems"], "fs_oids": fs_oids,
        "spaceman_oid": sb["nx_spaceman_oid"], "omap_oid": sb["nx_omap_oid"],
        "reaper_oid": sb["nx_reaper_oid"], "test_type": sb["nx_test_type"],
        "efi_jumpstart": sb["nx_efi_jumpstart"], "keylocker": sb["nx_keylocker"],
        "blocked_out_prange": sb["nx_blocked_out_prange"],
        "evict_mapping_tree_oid": sb["nx_evict_mapping_tree_oid"],
        "ephemeral_info": sb["nx_ephemeral_info"],
        "ephemeral_info_decoded": {
            "version": sb["nx_ephemeral_info"][0] & 0xFFFF,
            "max_fs_eph_structs": (sb["nx_ephemeral_info"][0] >> 16) & 0xFFFF,
            "min_block_count": sb["nx_ephemeral_info"][0] >> 32},
        "counters_nonzero": {i: v for i, v in enumerate(sb["nx_counters"]) if v},
        "newest_mounted_version": sb["nx_newest_mounted_version"],
        "mkb_locker": sb["nx_mkb_locker"],
    }
    checkpoint = {
        "newest_xid": newest_xid,
        "superblock_ring_index": sb_idx,
        "desc": {"base": desc_base, "blocks": desc_blocks, "next": sb["nx_xp_desc_next"],
                 "index": di, "len": dl},
        "data": {"base": data_base, "blocks": data_blocks, "next": sb["nx_xp_data_next"],
                 "index": data_idx, "len": data_len},
        "ring": ring,
        "ephemeral_objects": data_objs,
        "block0_identical_to_ring_superblock": True,
    }

    # ---- reaper ----
    reaper_obj = next((o for o in data_objs if o["oid"] == sb["nx_reaper_oid"]), None)
    check(reaper_obj is not None and reaper_obj["type"] == 0x11, "the reaper is mapped, type nx_reaper")
    reaper = decode(img.read(reaper_obj["block"]), REAPER)
    reaper = {k: v for k, v in reaper.items() if k.startswith("nr_")}

    # ---- container object map ----
    cont_omap = decode_omap(img, sb["nx_omap_oid"], "container", newest_xid)
    check(cont_omap["om_flags"] & 1 == 1, "container omap is MANUALLY_MANAGED")
    centries = cont_omap["entries"]
    check(len(centries) == len(fs_oids) and all(
        omap_lookup(centries, o, newest_xid) is not None for o in fs_oids),
        "the container omap maps every volume superblock oid")

    # ---- space manager ----
    sm_obj = next((o for o in data_objs if o["oid"] == sb["nx_spaceman_oid"]), None)
    check(sm_obj is not None and sm_obj["type"] == 5, "the space manager is mapped, type spaceman")
    smbuf = img.read(sm_obj["block"], sm_obj["blocks"])
    sm = decode(smbuf, SM)
    check(sm["sm_block_size"] == bs, "sm_block_size equals nx_block_size")
    bpc = sm["sm_blocks_per_chunk"]
    check(bpc == bs * 8, "blocks per chunk is block_size * 8")
    check(sm["sm_dev1_block_count"] == 0 and sm["sm_dev1_chunk_count"] == 0
          and sm["sm_dev1_cib_count"] == 0 and sm["sm_dev1_cab_count"] == 0,
          "no second (Fusion) device")
    d0_blocks = sm["sm_dev0_block_count"]
    check(d0_blocks == block_count, "sm_dev[0].sm_block_count equals nx_block_count")
    nchunks = -(-d0_blocks // bpc)
    check(sm["sm_dev0_chunk_count"] == nchunks, "chunk count %d follows from block_count" % nchunks)
    ncib = -(-nchunks // sm["sm_chunks_per_cib"])
    check(sm["sm_dev0_cib_count"] == ncib, "CIB count %d follows from the chunk count" % ncib)
    ncab = sm["sm_dev0_cab_count"]
    if ncab:
        check(ncab == -(-ncib // sm["sm_cibs_per_cab"]), "CAB count follows from the CIB count")

    # CIB addresses (array at sm_addr_offset from the start of the spaceman object)
    off = sm["sm_dev0_addr_offset"]
    n_addr = ncab if ncab else ncib
    check(off + 8 * n_addr <= len(smbuf), "CIB/CAB address array lies inside the spaceman object")
    addrs = list(struct.unpack_from("<%dQ" % n_addr, smbuf, off))
    cab_blocks = []
    if ncab:
        cib_addrs = []
        for a in addrs:
            cb = img.obj(a, 1, "spaceman CAB")
            ch = header(cb)
            check(ch["type"] == 6, "CAB at %d has type spaceman_cab" % a)
            c = decode(cb, CAB)
            cab_blocks.append({"block": a, "index": c["cab_index"], "count": c["cab_cib_count"]})
            cib_addrs += list(struct.unpack_from("<%dQ" % c["cab_cib_count"], cb, CAB_END))
    else:
        cib_addrs = addrs
    check(len(cib_addrs) == ncib, "the address array lists every CIB")

    chunks = []
    cibs = []
    ci_free_total = 0
    zero_bits_total = 0
    free_ranges_all = []
    chunk_bitmaps = {}
    zero_bitmap_chunks = []
    for cidx, a in enumerate(cib_addrs):
        cb = img.obj(a, 1, "spaceman CIB")
        ch = header(cb)
        check(ch["type"] == 7, "CIB at %d has type spaceman_cib" % a)
        c = decode(cb, CIB)
        check(c["cib_index"] == cidx, "CIB %d has cib_index %d" % (a, cidx))
        want = min(sm["sm_chunks_per_cib"], nchunks - cidx * sm["sm_chunks_per_cib"])
        check(c["cib_chunk_info_count"] == want, "CIB %d holds %d chunk-info records" % (a, want))
        cibs.append({"block": a, "oid": ch["o_oid"], "xid": ch["o_xid"], "index": c["cib_index"],
                     "chunk_info_count": c["cib_chunk_info_count"], "o_type": ch["o_type"]})
        for k in range(c["cib_chunk_info_count"]):
            ci = decode(cb, CHUNK_INFO, CIB_END + k * CHUNK_INFO_SIZE)
            chunk_no = cidx * sm["sm_chunks_per_cib"] + k
            first = chunk_no * bpc
            nb = min(bpc, d0_blocks - first)
            check(ci["ci_addr"] == first, "chunk %d: ci_addr is the first block of the chunk" % chunk_no)
            check(ci["ci_block_count"] == nb, "chunk %d: ci_block_count %d" % (chunk_no, nb))
            check(ci["ci_free_count"] <= nb and ci["ci_free_count"] & 0xFFF00000 == 0,
                  "chunk %d: ci_free_count within range" % chunk_no)
            if ci["ci_bitmap_addr"] == 0:
                # mkapfs gives a chunk with no allocated block no bitmap block at
                # all: address 0 means "every block free".
                check(ci["ci_free_count"] == nb,
                      "chunk %d: no bitmap block (address 0) only when every block is free" % chunk_no)
                bm = bytes(bs)
                zero_bitmap_chunks.append(chunk_no)
            else:
                bm = img.read(ci["ci_bitmap_addr"])
                chunk_bitmaps[ci["ci_bitmap_addr"]] = "chunk %d bitmap" % chunk_no
                img.raw[ci["ci_bitmap_addr"]] = "chunk %d bitmap" % chunk_no
            zeros = pop_zero_bits(bm, nb)
            check(zeros == ci["ci_free_count"],
                  "chunk %d: zero bits in the bitmap (%d) equal ci_free_count" % (chunk_no, zeros))
            check(all(v == 0 for v in bm[(nb + 7) // 8:]),
                  "chunk %d: bitmap bytes past the chunk are zero" % chunk_no)
            ci_free_total += ci["ci_free_count"]
            zero_bits_total += zeros
            free_ranges_all += ranges_from_bits(bm, first, nb)
            chunks.append({"chunk": chunk_no, "cib_block": a, "ci_xid": ci["ci_xid"],
                           "ci_addr": ci["ci_addr"], "ci_block_count": ci["ci_block_count"],
                           "ci_free_count": ci["ci_free_count"],
                           "ci_bitmap_addr": ci["ci_bitmap_addr"]})
    check(sm["sm_dev0_free_count"] == ci_free_total,
          "sm_dev[0].sm_free_count (%d) equals the sum of ci_free_count" % ci_free_total)
    check(ci_free_total == zero_bits_total,
          "sum of ci_free_count equals the zero-bit popcount of the bitmaps")
    free_ranges = merge_ranges(free_ranges_all)
    free_blocks = sum(b - a + 1 for a, b in free_ranges)
    check(free_blocks == ci_free_total, "free ranges cover exactly sm_free_count blocks")

    # internal pool: ip bitmap + ip blocks are raw (header-less) block runs
    ip_bm = [sm["sm_ip_bm_base"], sm["sm_ip_bm_base"] + sm["sm_ip_bm_block_count"] - 1]
    ip = [sm["sm_ip_base"], sm["sm_ip_base"] + sm["sm_ip_block_count"] - 1]
    check(ip_bm[1] < d0_blocks and ip[1] < d0_blocks, "internal pool lies inside the container")
    check(ip_bm[1] < ip[0] or ip[1] < ip_bm[0], "internal pool bitmap and pool do not overlap")
    for blk in range(ip_bm[0], ip_bm[1] + 1):
        img.raw[blk] = "internal-pool bitmap"
    ip_bits = []
    ipbm = img.read(sm["sm_ip_bm_base"])
    for i in range(sm["sm_ip_block_count"]):
        if ipbm[i // 8] & (1 << (i % 8)):
            ip_bits.append(sm["sm_ip_base"] + i)
    for blk in range(ip[0], ip[1] + 1):
        img.raw[blk] = "internal pool"
    ip_resident = sorted([a for a in cib_addrs if ip[0] <= a <= ip[1]] +
                         [a for a in chunk_bitmaps if ip[0] <= a <= ip[1]] +
                         [c["block"] for c in cab_blocks if ip[0] <= c["block"] <= ip[1]])
    check(ip_bits == ip_resident,
          "internal-pool bitmap marks exactly the CIB/CAB/chunk-bitmap blocks inside the pool")
    for a in cib_addrs:
        img.reached[a] = "spaceman CIB"

    # free queues: ephemeral B-tree roots mapped in the checkpoint
    free_queues = []
    for o in data_objs:
        if o["type"] == 2 and o["subtype"] == 9:
            t = Tree(img, o["block"], 9, lambda x: x, "free queue %#x" % o["oid"],
                     root_buf=img.read(o["block"]))
            free_queues.append({"oid": o["oid"], "block": o["block"], "records": len(t.records),
                                "node": t.nodes[0], "btree_info": t.info})
            check(len(t.records) == 0, "free queue %#x is empty" % o["oid"])
    fq_in_sm = [sm["sm_fq%d_tree_oid" % q] for q in range(3)]
    check(sorted(f["oid"] for f in free_queues) == sorted(x for x in fq_in_sm if x),
          "the mapped free-queue roots are the ones the space manager names")

    spaceman = {k: v for k, v in sm.items() if k not in ("o_cksum", "o_oid", "o_xid", "o_type", "o_subtype")}
    spaceman["header"] = {"oid": sm_obj["oid"], "xid": sm_obj["xid"], "o_type": sm_obj["o_type"],
                          "block": sm_obj["block"], "blocks": sm_obj["blocks"]}
    spaceman["cib_blocks"] = cibs
    spaceman["cab_blocks"] = cab_blocks
    spaceman["internal_pool_bitmap_blocks"] = ip_bm
    spaceman["internal_pool_blocks"] = ip
    spaceman["internal_pool_in_use"] = ip_bits

    # ---- the volume ----
    vol_oid = fs_oids[0]
    ve = omap_lookup(centries, vol_oid, newest_xid)
    check(ve["flags"] == 0 and ve["size"] == bs, "volume superblock omap entry: flags 0, one block")
    vbuf = img.obj(ve["paddr"], 1, "volume superblock")
    vh = header(vbuf)
    check(vh["type"] == 0xD and vh["flags"] == 0 and vh["o_oid"] == vol_oid,
          "volume superblock: type fs, virtual, oid from nx_fs_oid")
    v = decode(vbuf, APSB)
    check(v["apfs_magic"] == 0x42535041, "apfs_magic is 'BSPA' (bytes APSB at offset 32)")
    check(v["apfs_fs_index"] == 0, "apfs_fs_index is the slot (0)")
    volname = cstr(v["apfs_volname"])
    check(volname == label, "volume name equals the -L argument")
    check(uuid_str(v["apfs_vol_uuid"]) == vol_uuid.lower(), "volume uuid equals the -u argument")
    inc = v["apfs_incompatible_features"]
    CASE_INS, NORM_INS = 1, 8
    # mkapfs: -z => 0 (case- and normalization-sensitive); else -s => NORMALIZATION_INSENSITIVE
    # only; else CASE_INSENSITIVE only (case-insensitive implies normalization-insensitive).
    expect_inc = 0 if norm_sensitive else (NORM_INS if case_sensitive else CASE_INS)
    check(inc == expect_inc, "volume incompatible features %#x match mkapfs -s=%s -z=%s"
          % (inc, int(case_sensitive), int(norm_sensitive)))
    case_insensitive = bool(inc & CASE_INS)
    norm_insensitive = bool(inc & (NORM_INS | CASE_INS))
    check(case_insensitive == (not (case_sensitive or norm_sensitive)),
          "case sensitivity of the volume follows the flags")
    check(norm_insensitive == (not norm_sensitive), "normalization sensitivity follows -z")
    check(v["apfs_fs_flags"] & 1 == 1, "APFS_FS_UNENCRYPTED set (volume is not encrypted)")
    check(v["apfs_num_snapshots"] == 0 and v["apfs_snap_meta_tree_oid"] != 0, "no snapshots recorded")
    check(v["apfs_revert_to_xid"] == 0 and v["apfs_root_to_xid"] == 0, "no revert pending")

    vol_omap = decode_omap(img, v["apfs_omap_oid"], "volume", newest_xid)
    check(vol_omap["om_flags"] & 1 == 0, "volume omap is not MANUALLY_MANAGED")
    ventries = vol_omap["entries"]

    # fs tree: virtual root -> volume omap
    rtype = v["apfs_root_tree_type"]
    check(rtype & 0xFFFF == 2 and rtype & 0xC0000000 == 0, "fs-tree root is a virtual B-tree")
    re = omap_lookup(ventries, v["apfs_root_tree_oid"], newest_xid)
    check(re is not None and re["flags"] == 0, "volume omap maps the fs-tree root oid")

    def vresolve(oid):
        e = omap_lookup(ventries, oid, newest_xid)
        if e is None:
            fail("virtual oid %#x not in the volume omap" % oid)
        return e["paddr"]

    fst = Tree(img, re["paddr"], 0xE, vresolve, "fs tree")
    records = [decode_fs_record(k, vv, case_insensitive, norm_insensitive) for k, vv in fst.records]
    fs_nodes = fst.nodes
    check(fst.info["bt_key_count"] == len(records), "fs-tree key_count equals the records found")
    check(fst.info["bt_node_count"] == len(fs_nodes), "fs-tree node_count equals the nodes found")
    check(len(fs_nodes) == 1 and fs_nodes[0]["level"] == 0, "the fs tree is a single leaf")
    order = [(r["id"], r["type"]) for r in records]
    check(order == sorted(order), "fs-tree records are sorted by (object id, type)")
    inodes = {r["id"]: r["inode"] for r in records if r["type"] == 3}
    dreclist = [r["drec"] for r in records if r["type"] == 9]
    check(set(inodes) == {2, 3}, "inodes are the root directory (2) and the private dir (3)")
    check({d["name"] for d in dreclist} == {"root", "private-dir"} and
          all(d["parent_id"] == 1 for d in dreclist),
          "two directory records under the root-parent id 1: root and private-dir")
    check(all(d["file_id"] in (2, 3) for d in dreclist), "directory records point at inodes 2 and 3")
    check(inodes[2]["parent_id"] == 1 and inodes[3]["parent_id"] == 1,
          "both inodes have parent_id 1")
    check(all(i["mode"] & 0o170000 == 0o040000 for i in inodes.values()), "both inodes are directories")
    check(all(x["name"] in ("dstream", "name", "dir_stats_key", "document_id", "finder_info")
              for i in inodes.values() for x in i["xfields"]), "known xfield types only")
    # the reference's name-hash recipe, on these real records
    hash_ok = []
    if norm_insensitive:
        hash_variants = {}
        for d in dreclist:
            for k, ok in d["hash_matches"].items():
                hash_variants.setdefault(k, []).append(ok)
        hash_ok = [k for k, oks in hash_variants.items() if all(oks)]
        check(bool(hash_ok), "the reference's name-hash recipe reproduces every stored drec hash")
    check(all(d["key_form"] == ("hashed" if norm_insensitive else "plain") for d in dreclist),
          "drec key form follows the volume's normalization sensitivity")

    # snapshot metadata and extent-reference trees (physical roots)
    def phys_tree(root_oid, ttype, subtype, what):
        check(ttype & 0xFFFF == 2 and ttype & 0xC0000000 == OBJ_PHYSICAL, "%s root is a physical B-tree" % what)
        t = Tree(img, root_oid, subtype, lambda o: o, what)
        return t

    snap_t = phys_tree(v["apfs_snap_meta_tree_oid"], v["apfs_snap_meta_tree_type"], 0x10,
                       "snapshot metadata tree")
    ext_t = phys_tree(v["apfs_extentref_tree_oid"], v["apfs_extentref_tree_type"], 0xF,
                      "extent reference tree")
    check(len(snap_t.records) == 0, "the snapshot-metadata tree is empty")
    check(len(ext_t.records) == 0, "the extent-reference tree is empty")
    check(snap_t.info["bt_key_count"] == 0 and ext_t.info["bt_key_count"] == 0,
          "both empty trees say key_count 0")

    # ---- block accounting ----
    check(vh["o_xid"] == newest_xid, "volume superblock xid equals the checkpoint xid")
    # allocated bitmap vs reached objects and areas
    for blk in img.reached:
        check_free = any(a <= blk <= b for a, b in free_ranges)
        if check_free:
            fail("object block %d (%s) is marked free in the bitmap" % (blk, img.reached[blk]))
    checks.append("every verified object block is marked allocated in the bitmap")
    for blk in img.raw:
        if any(a <= blk <= b for a, b in free_ranges):
            fail("raw block %d (%s) is marked free" % (blk, img.raw[blk]))
    checks.append("every bitmap / internal-pool block is marked allocated in the bitmap")
    areas = [(0, 0), (desc_base, desc_base + desc_blocks - 1),
             (data_base, data_base + data_blocks - 1), tuple(ip_bm), tuple(ip)]
    for a, b in free_ranges:
        for x, y in areas:
            if not (b < x or a > y):
                fail("free range [%d,%d] overlaps the metadata area [%d,%d]" % (a, b, x, y))
    checks.append("no free range overlaps block 0, the checkpoint areas or the internal pool")
    # every non-zero block of the image is accounted for
    stray = [b for b in img.nonzero_blocks() if b not in img.reached and b not in img.raw]
    check(not stray, "every non-zero block is a verified object or a known raw block (stray: %s)" % stray[:5])

    volume = {
        "name": volname, "oid": vol_oid, "block": ve["paddr"], "header": hdr_summary(vh),
        "uuid": uuid_str(v["apfs_vol_uuid"]), "fs_index": v["apfs_fs_index"],
        "features": v["apfs_features"],
        "readonly_compatible_features": v["apfs_readonly_compatible_features"],
        "incompatible_features": inc, "fs_flags": v["apfs_fs_flags"],
        "role": v["apfs_role"], "case_insensitive": case_insensitive,
        "normalization_insensitive": norm_insensitive, "encrypted": not (v["apfs_fs_flags"] & 1),
        "unmount_time": v["apfs_unmount_time"], "last_mod_time": v["apfs_last_mod_time"],
        "fs_reserve_block_count": v["apfs_fs_reserve_block_count"],
        "fs_quota_block_count": v["apfs_fs_quota_block_count"],
        "fs_alloc_count": v["apfs_fs_alloc_count"],
        "meta_crypto": {k[5:]: v[k] for k in v if k.startswith("wmcs_")},
        "root_tree_type": rtype, "extentref_tree_type": v["apfs_extentref_tree_type"],
        "snap_meta_tree_type": v["apfs_snap_meta_tree_type"], "omap_oid": v["apfs_omap_oid"],
        "root_tree_oid": v["apfs_root_tree_oid"], "extentref_tree_oid": v["apfs_extentref_tree_oid"],
        "snap_meta_tree_oid": v["apfs_snap_meta_tree_oid"],
        "revert_to_xid": v["apfs_revert_to_xid"], "revert_to_sblock_oid": v["apfs_revert_to_sblock_oid"],
        "next_obj_id": v["apfs_next_obj_id"], "num_files": v["apfs_num_files"],
        "num_directories": v["apfs_num_directories"], "num_symlinks": v["apfs_num_symlinks"],
        "num_other_fsobjects": v["apfs_num_other_fsobjects"], "num_snapshots": v["apfs_num_snapshots"],
        "total_blocks_alloced": v["apfs_total_blocks_alloced"],
        "total_blocks_freed": v["apfs_total_blocks_freed"],
        "formatted_by": {"id": cstr(v["formatted_by_id"]), "timestamp": v["formatted_by_timestamp"],
                         "timestamp_iso": iso(v["formatted_by_timestamp"]),
                         "last_xid": v["formatted_by_last_xid"]},
        "modified_by_nonzero": [
            {"index": i, "id": cstr(v["modified_by_%d_id" % i]),
             "timestamp": v["modified_by_%d_timestamp" % i]}
            for i in range(8) if v["modified_by_%d_timestamp" % i] or v["modified_by_%d_id" % i][0]],
        "next_doc_id": v["apfs_next_doc_id"], "root_to_xid": v["apfs_root_to_xid"],
        "er_state_oid": v["apfs_er_state_oid"], "cloneinfo_id_epoch": v["apfs_cloneinfo_id_epoch"],
        "cloneinfo_xid": v["apfs_cloneinfo_xid"], "snap_meta_ext_oid": v["apfs_snap_meta_ext_oid"],
        "volume_group_id": uuid_str(v["apfs_volume_group_id"]),
        "integrity_meta_oid": v["apfs_integrity_meta_oid"], "fext_tree_oid": v["apfs_fext_tree_oid"],
        "fext_tree_type": v["apfs_fext_tree_type"],
    }

    # ---- measured answers to the plan's [M] questions ----
    measured = {
        "checkpoints_in_ring": len(sb_candidates),
        "descriptor_ring": "index %d len %d: map block(s) at ring index %s then the superblock at %d; "
                           "remaining %d ring blocks are zero" % (
                               di, dl, members[:-1], sb_idx,
                               sum(1 for r in ring if r["kind"] == "zero")),
        "map_blocks_immediately_before_superblock": True,
        "block0_is_copy_of_newest_checkpoint_superblock": True,
        "fs_tree_is_one_leaf": True,
        "drec_key_form": "%s (j_drec_%skey_t) on this volume; fs-tree bt_flags=%#x" % (
            "hashed" if norm_insensitive else "plain", "hashed_" if norm_insensitive else "",
            fst.info["bt_flags"]),
        "drec_hash_recipe_reproduces_stored_hashes": hash_ok,
        "fs_tree_bt_flags": fst.info["bt_flags"],
        "fs_tree_toc_format": "kvloc (variable)" if not (fs_nodes[0]["flags"] & 4) else "kvoff (fixed)",
        "omap_tree_toc_format": "kvoff (fixed)" if cont_omap["tree"]["nodes"][0]["flags"] & 4 else "kvloc",
        "ci_addr_is": "first block of the chunk (chunk_no * sm_blocks_per_chunk)",
        "ci_bitmap_addr_is": "physical block of a raw header-less bitmap block, inside the internal pool; "
                             "0 (no bitmap block) for a chunk whose every block is free: chunks %s" % zero_bitmap_chunks,
        "chunks_without_bitmap_block": zero_bitmap_chunks,
        "bitmap_polarity": "bit set = allocated; bit i = byte i//8, mask 1<<(i%8); bit 0 = first block of the chunk",
        "sm_addr_offset_origin": "bytes from the start of the spaceman object",
        "sm_addr_offset_array": "u64 CIB block addresses (%d) when sm_cab_count is 0" % ncib,
        "bt_node_size": fst.info["bt_node_size"],
        "tree_types": {
            "container_omap_tree": storage_of_type(cont_omap["om_tree_type"]),
            "volume_omap_tree": storage_of_type(vol_omap["om_tree_type"]),
            "fs_tree": storage_of_type(rtype),
            "extentref_tree": storage_of_type(v["apfs_extentref_tree_type"]),
            "snap_meta_tree": storage_of_type(v["apfs_snap_meta_tree_type"]),
            "free_queues": "ephemeral"},
        "spaceman_blocks": sm_obj["blocks"],
        "hash_trees_flag_0x80": bool(fst.info["bt_flags"] & 0x80),
    }

    return {
        "oracle": {"decoder": "tools/fixtures/apfs_oracle.py", "source": "Apple File System Reference 2020-06-22; "
                   "values come from the image and the mkapfs arguments, never from Minutiae"},
        "args": {"label": label, "case_sensitive": case_sensitive, "norm_sensitive": norm_sensitive,
                 "container_uuid": cont_uuid.lower(), "volume_uuid": vol_uuid.lower()},
        "container": container,
        "checkpoint": checkpoint,
        "reaper": reaper,
        "spaceman": spaceman,
        "free_queues": free_queues,
        "chunks": chunks,
        "free_ranges": free_ranges,
        "free_blocks": free_blocks,
        "allocated_blocks": block_count - free_blocks,
        "container_omap": cont_omap,
        "volume": volume,
        "volume_omap": vol_omap,
        "fs_tree": {"root_oid": v["apfs_root_tree_oid"], "root_block": re["paddr"],
                    "btree_info": fst.info, "nodes": fs_nodes, "records": records},
        "snap_meta_tree": {"root_block": v["apfs_snap_meta_tree_oid"], "nodes": snap_t.nodes,
                           "btree_info": snap_t.info, "records": []},
        "extentref_tree": {"root_block": v["apfs_extentref_tree_oid"], "nodes": ext_t.nodes,
                           "btree_info": ext_t.info, "records": []},
        "verified_object_blocks": sorted(img.reached),
        "raw_blocks": {str(k): v for k, v in sorted(img.raw.items())},
        "measured": measured,
        "checks": checks,
    }


def storage_of_type(t):
    return {"type": t & 0xFFFF, "storage": storage(t & 0xFFFF0000), "raw": t}


if __name__ == "__main__":
    sys.exit(main())

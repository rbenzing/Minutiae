#!/usr/bin/env python3
"""Independent oracle for the F2FS fixtures. It never reads an image itself and
never runs Minutiae: it walks the SOURCE TREE handed to sload.f2fs, and it
parses the text that the external tools (fsck.f2fs, dump.f2fs, blkid) printed
about the finished image.

usage:
  f2fs_oracle.py tree <srcdir> [--exclude /path ...]
      the expected entries (type, size, sha256, mode&0o7777, mtime, symlink
      target) as {"files": [...]}, sorted by path in byte order. Paths named by
      --exclude (files the generator later removes) are left out.

  f2fs_oracle.py layout <capture dir> <files.json>
      what the tools say about the image, as JSON: superblock and checkpoint
      fields, the NAT block address of every inode, every file's data blocks,
      and the free main-area blocks. The capture directory holds the raw tool
      output under fixed names (see CAPTURES). Every value that two tools
      report is cross-checked and every fsck verdict must be clean; any
      mismatch exits non-zero, so the generator fails.
"""
import hashlib
import json
import os
import re
import stat
import sys

BLOCK = 4096
BLOCKS_PER_SEG = 512

# Names of the files the generator saves the tool output in.
CAPTURES = {
    "fsck_l": "fsck-l.txt",  # fsck.f2fs -l: superblock and checkpoint
    "dump_i": "dump-i.txt",  # dump.f2fs -d 1 -s0~-1: superblock, checkpoint (stdout; the SIT goes to dump_sit)
    "fsck_f": "fsck-f.txt",  # fsck.f2fs -f --dry-run: consistency verdicts
    "tree": "tree.txt",  # fsck.f2fs -t --dry-run: directory tree with inode numbers
    "map": "map.txt",  # fsck.f2fs -d 1 -M --dry-run: data blocks of every file
    "sit": "dump_sit",  # file written by dump.f2fs -d 1 -s0~-1: per-segment valid-block bitmaps
    "nat": "dump_nat",  # file written by dump.f2fs -n0~-1: NID -> block address
    "blkid": "blkid.txt",  # blkid -p -o export: label, uuid, type
}


def fail(msg: str) -> None:
    sys.exit("f2fs_oracle: " + msg)


# ---------------------------------------------------------------- tree


def cmd_tree(args: list) -> None:
    src = args.pop(0)
    exclude = set()
    while args:
        if args.pop(0) != "--exclude":
            fail("unknown argument")
        exclude.add(args.pop(0))

    files = []
    for root, dirs, names in os.walk(src):
        dirs.sort()
        for name in sorted(dirs + names):
            full = os.path.join(root, name)
            rel = "/" + os.path.relpath(full, src).replace(os.sep, "/")
            if rel in exclude:
                continue
            st = os.lstat(full)
            e = {"path": rel, "mode": stat.S_IMODE(st.st_mode), "mtime": int(st.st_mtime)}
            if stat.S_ISDIR(st.st_mode):
                e["type"], e["size"], e["sha256"] = "dir", 0, ""
            elif stat.S_ISLNK(st.st_mode):
                target = os.readlink(full)
                e["type"], e["size"] = "symlink", len(os.fsencode(target))
                e["sha256"] = hashlib.sha256(os.fsencode(target)).hexdigest()
                e["link_target"] = target
            elif stat.S_ISREG(st.st_mode):
                h = hashlib.sha256()
                with open(full, "rb") as f:
                    for chunk in iter(lambda: f.read(1 << 20), b""):
                        h.update(chunk)
                e["type"], e["size"], e["sha256"] = "file", st.st_size, h.hexdigest()
            else:
                fail("unsupported file type in source tree: " + rel)
            files.append(e)
    files.sort(key=lambda e: os.fsencode(e["path"]))
    json.dump({"files": files}, sys.stdout, ensure_ascii=False)


# ---------------------------------------------------------------- layout


def read(cap: str, key: str) -> str:
    with open(os.path.join(cap, CAPTURES[key]), encoding="utf-8", errors="strict") as f:
        return f.read()


def parse_colon(text: str) -> dict:
    """fsck.f2fs -l: 'name:   value' lines (integers where they parse)."""
    out = {}
    for line in text.splitlines():
        m = re.match(r"^([A-Za-z_][\w\[\]]*|Filesystem volume name):\s+(.*?)\s*$", line)
        if not m:
            continue
        key, val = m.group(1), m.group(2)
        # First wins: the superblock precedes the checkpoint, and both print checksum_offset.
        out.setdefault(key, int(val) if re.fullmatch(r"-?\d+", val) else val)
    return out


def parse_dump(text: str) -> dict:
    """dump.f2fs: 'name   [0xhex : dec]' lines."""
    out = {}
    for line in text.splitlines():
        m = re.match(r"^(\S+)\s+\[0x\s*([0-9a-f]+) : (-?\d+)\]\s*$", line)
        if m:
            out.setdefault(m.group(1), int(m.group(3)))
    return out


SB_FIELDS = [
    "major_ver", "minor_ver", "log_sectorsize", "log_sectors_per_block", "log_blocksize",
    "log_blocks_per_seg", "segs_per_sec", "secs_per_zone", "checksum_offset", "block_count",
    "section_count", "segment_count", "segment_count_ckpt", "segment_count_sit",
    "segment_count_nat", "segment_count_ssa", "segment_count_main", "segment0_blkaddr",
    "cp_blkaddr", "sit_blkaddr", "nat_blkaddr", "ssa_blkaddr", "main_blkaddr", "root_ino",
    "node_ino", "meta_ino", "cp_payload",
]
CP_FIELDS = [
    "checkpoint_ver", "user_block_count", "valid_block_count", "rsvd_segment_count",
    "overprov_segment_count", "free_segment_count", "ckpt_flags", "cp_pack_total_block_count",
    "cp_pack_start_sum", "valid_node_count", "valid_inode_count", "next_free_nid",
    "sit_ver_bitmap_bytesize", "nat_ver_bitmap_bytesize",
]


def parse_features(*texts: str) -> tuple:
    found = set()
    for t in texts:
        for m in re.finditer(r"^Info: superblock features = ([0-9a-f]+) :(.*)$", t, re.M):  # the tools print the bits in hex, unprefixed
            found.add((int(m.group(1), 16), tuple(m.group(2).split())))
    if len(found) != 1:
        fail("the tools disagree on the superblock features (or print none): %r" % sorted(found))
    bits, names = found.pop()
    return bits, list(names)


def parse_tree(text: str) -> dict:
    """fsck.f2fs -t: '|   |-- name <ino = 0x16>, <encrypted (0)>' -> {path: ino}."""
    paths, stack = {}, []
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
        path = "/" + "/".join(stack)
        if path in paths:
            fail("duplicate path in the tree output: " + path)
        paths[path] = int(m.group(3), 16)
    return paths


def parse_map(text: str) -> dict:
    """fsck.f2fs -d 1 -M: '/path a-b c-d [0]' (progress is separated by CR or
    ESC[2K). Extents are inclusive absolute block ranges in file order; a lone
    '0' is not an extent (block 0 is never data)."""
    out = {}
    for chunk in re.split(r"[\r\n]|\x1b\[2K", text):
        if not chunk.startswith("/"):
            continue
        # The path may contain spaces (none do in the fixtures): extents are
        # the trailing tokens made only of digits and dashes.
        toks = chunk.split(" ")
        i = len(toks)
        while i > 1 and re.fullmatch(r"\d+(-\d+)?", toks[i - 1]):
            i -= 1
        path, ext = " ".join(toks[:i]), toks[i:]
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
            out[path] = runs
    return out


def parse_sit(text: str) -> list:
    """dump.f2fs -s: per segment 'segno: N vblocks: V seg_type: T sit_pack: P'
    followed (when V > 0) by 64 bitmap bytes, most significant bit first."""
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


def cmd_layout(args: list) -> None:
    cap, files_json = args
    with open(files_json, encoding="utf-8") as f:
        source_files = json.load(f)["files"]

    fsck_l, dump_i = read(cap, "fsck_l"), read(cap, "dump_i")
    c = parse_colon(fsck_l)
    d = parse_dump(dump_i)

    # Superblock and checkpoint: fsck.f2fs -l and dump.f2fs must agree.
    sb, cp = {}, {}
    for k in SB_FIELDS:
        if k not in c:
            fail("fsck.f2fs -l did not print " + k)
        if k in d and d[k] != c[k]:
            fail("fsck.f2fs and dump.f2fs disagree on %s: %r vs %r" % (k, c[k], d[k]))
        sb[k] = c[k]
    for k in CP_FIELDS:
        if k not in c:
            fail("fsck.f2fs -l did not print " + k)
        if k in d and d[k] != c[k]:
            fail("fsck.f2fs and dump.f2fs disagree on %s: %r vs %r" % (k, c[k], d[k]))
        cp[k] = c[k]
    bits, features = parse_features(fsck_l, dump_i, read(cap, "fsck_f"))

    label = c.get("Filesystem volume name")
    if label is None:
        fail("fsck.f2fs -l did not print the volume name")
    blkid = dict(l.split("=", 1) for l in read(cap, "blkid").splitlines() if "=" in l)
    if blkid.get("TYPE") != "f2fs" or blkid.get("LABEL") != label:
        fail("blkid disagrees with fsck.f2fs on type/label: %r" % blkid)
    uuid = blkid.get("UUID")
    if not uuid:
        fail("blkid printed no UUID")

    if sb["log_blocksize"] != 12 or sb["log_blocks_per_seg"] != 9:
        fail("unexpected geometry")
    main_first = sb["main_blkaddr"]
    main_segs = sb["segment_count_main"]

    # fsck's consistency verdicts: every line must be Ok.
    verdicts = [l for l in read(cap, "fsck_f").splitlines() if l.startswith("[FSCK]")]
    bad = [l for l in verdicts if "[Fail" in l or "[Ok..]" not in l and "Max image size" not in l and "fixing SIT" not in l]
    if bad or not any("SIT valid block bitmap checking" in l and "[Ok..]" in l for l in verdicts):
        fail("fsck.f2fs is not clean: %r" % bad)
    fsck_valid = None
    for l in verdicts:
        m = re.search(r"valid_block_count matching with CP\s+\[Ok\.\.\] \[0x([0-9a-f]+)\]", l)
        if m:
            fsck_valid = int(m.group(1), 16)
    if fsck_valid is None or fsck_valid != cp["valid_block_count"]:
        fail("fsck valid_block_count %r != checkpoint %r" % (fsck_valid, cp["valid_block_count"]))

    # Free space: the SIT bitmaps of the main area.
    segs = parse_sit(read(cap, "sit"))
    if [s["segno"] for s in segs] != list(range(main_segs)):
        fail("dump.f2fs -s did not list exactly the %d main-area segments" % main_segs)
    valid_total, free = 0, []
    for s in segs:
        bm = s["bytes"]
        if s["vblocks"] == 0:
            bm = [0] * 64
        if len(bm) != BLOCKS_PER_SEG // 8:
            fail("segment %d bitmap has %d bytes" % (s["segno"], len(bm)))
        ones = sum(bin(b).count("1") for b in bm)
        if ones != s["vblocks"]:
            fail("segment %d: %d bits set but vblocks %d" % (s["segno"], ones, s["vblocks"]))
        valid_total += ones
        base = main_first + s["segno"] * BLOCKS_PER_SEG
        for i in range(BLOCKS_PER_SEG):
            if not (bm[i >> 3] & (0x80 >> (i & 7))):
                free.append(base + i)
    if valid_total != cp["valid_block_count"]:
        fail("SIT valid blocks %d != checkpoint valid_block_count %d" % (valid_total, cp["valid_block_count"]))
    if valid_total + len(free) != main_segs * BLOCKS_PER_SEG:
        fail("valid + free != main area size")
    free_ranges = []
    for b in free:
        if free_ranges and free_ranges[-1][1] == b - 1:
            free_ranges[-1][1] = b
        else:
            free_ranges.append([b, b])

    # Inodes: the tree (path -> ino) and the NAT (nid -> block address).
    tree = parse_tree(read(cap, "tree"))
    want = {e["path"] for e in source_files}
    if set(tree) != want:
        fail("directory tree differs from the source tree: only in image %r, only in source %r"
             % (sorted(set(tree) - want)[:5], sorted(want - set(tree))[:5]))
    nat = {}
    for line in read(cap, "nat").splitlines():
        m = re.match(r"^nid:\s*(\d+)\s+ino:\s*(\d+)\s+offset:\s*(\d+)\s+blkaddr:\s*(\d+)\s+pack:\s*(\d+)", line)
        if m:
            nat[int(m.group(1))] = (int(m.group(2)), int(m.group(4)))
    inodes = []
    for path in sorted(tree, key=os.fsencode):
        ino = tree[path]
        if ino not in nat or nat[ino][0] != ino:
            fail("inode %d of %s has no NAT entry with ino == nid" % (ino, path))
        addr = nat[ino][1]
        if not main_first <= addr < main_first + main_segs * BLOCKS_PER_SEG:
            fail("NAT address %d of %s is outside the main area" % (addr, path))
        inodes.append({"path": path, "ino": ino, "nat_blkaddr": addr})
    root = sb["root_ino"]
    if root not in nat or nat[root][0] != root:
        fail("root inode %d has no NAT entry" % root)

    # Data blocks of every file that has any (inline files and directories
    # without data blocks are absent from the map).
    fmap = parse_map(read(cap, "map"))
    by_path = {e["path"]: e for e in source_files}
    blocks = {}
    for path, runs in fmap.items():
        e = by_path.get(path)
        if e is None or e["type"] != "file":
            fail("file map names %r, which is not a regular file of the source tree" % path)
        n = sum(b - a + 1 for a, b in runs)
        if n != (e["size"] + BLOCK - 1) // BLOCK:
            fail("file map of %s covers %d blocks, size %d needs %d" % (path, n, e["size"], (e["size"] + BLOCK - 1) // BLOCK))
        for a, b in runs:
            if a < main_first or b >= main_first + main_segs * BLOCKS_PER_SEG:
                fail("file map of %s leaves the main area" % path)
        blocks[path] = runs
    fragmented = sorted(p for p, r in blocks.items() if len(r) > 1)

    json.dump(
        {
            "type": "f2fs",
            "label": label,
            "uuid": uuid,
            "block_size": BLOCK,
            "size": sb["block_count"] * BLOCK,
            "feature_bits": bits,
            "features": features,
            "superblock": sb,
            "checkpoint": cp,
            "root": {"ino": root, "nat_blkaddr": nat[root][1]},
            "inodes": inodes,
            "file_blocks": dict(sorted(blocks.items(), key=lambda kv: os.fsencode(kv[0]))),
            "fragmented": fragmented,
            "main_blkaddr": main_first,
            "main_blocks": main_segs * BLOCKS_PER_SEG,
            "free_blocks": free_ranges,
            "free_block_count": len(free),
        },
        sys.stdout,
        ensure_ascii=False,
    )


def main() -> None:
    args = sys.argv[1:]
    if not args:
        fail("usage: tree|layout ...")
    cmd = args.pop(0)
    if cmd == "tree":
        cmd_tree(args)
    elif cmd == "layout" and len(args) == 2:
        cmd_layout(args)
    else:
        fail("usage: tree|layout ...")


main()

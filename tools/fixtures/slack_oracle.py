#!/usr/bin/env python3
"""Independent oracle for the slack fixtures (plan 3F, Task 12). It never runs
Minutiae and imports none of its code. What it reads:

  * the filesystems' own tools (mshowfat for FAT chains, debugfs stat for ext4
    extents, dump.f2fs -i and fsck.f2fs -t for F2FS inodes, fsck.fat -v,
    dump.exfat, dumpe2fs, fsck.f2fs -l for geometry), and
  * the raw image bytes: the exFAT boot sector, FAT and directory entry sets are
    parsed here (exfatprogs 1.2.0's dump.exfat has no chain listing), and every
    byte range the oracle states is read from the image and checked or hashed.

usage:
  slack_oracle.py ops <fs> <mount dir>       the guest commands that write the scenario
  slack_oracle.py ops-delete <fs> <mount dir> the guest command that deletes /gone.txt
  slack_oracle.py units <fs> <img> <pat> <out.json> [all|live]
      the allocation units of every scenario file (and of /gone.txt) as the
      tools report them, with the file contents checked against the pattern
      (mode live leaves /gone.txt out: it is deleted)
  slack_oracle.py same <before.json> <after.json>
      every live file has the same units in both; /gone.txt is absent after
  slack_oracle.py tail <img> <fs_size>       append the volume tail (3F-R15)
  slack_oracle.py expect <fs> <img> <units.json> <generator.json> <out.json>
      the expectation file; exits 1 unless the scenario produced what the
      fixture needs (3F-R16, 3F-R26)

Scenario (per filesystem, unit u): files of 1, 600, u-1, u, u+1, 3u+5 and 5000
bytes, an empty file, a 100-byte file (inline on F2FS), a file in a sub-directory
with a long name, a fragmented file (written, a second file, then appended),
on ext4 and F2FS a sparse tail (700 bytes then truncate to 5000) and a hole in
the middle, and /gone.txt (1500 bytes; 5000 on F2FS, where 1500 would be inline).
"""
import hashlib
import json
import re
import subprocess
import sys

SENT = b"SLACK-SENTINEL-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLM"
assert len(SENT) == 64
UNIT = {"fat": 2048, "exfat": 32768, "ext4": 1024, "f2fs": 4096}
TYPE = {"fat": "fat16", "exfat": "exfat", "ext4": "ext4", "f2fs": "f2fs"}
CHUNK = 4096
TAIL_A, TAIL_B, TAIL_C = 524288, 524288, 4196  # sentinel, zeros, sentinel (3F-R15)
LONG = "a-rather-long-file-name-for-lfn-entries.dat"


def fail(msg):
    sys.exit("slack_oracle: " + msg)


def tool(*args, stdin=None):
    res = subprocess.run(list(args), capture_output=True, text=True, stdin=stdin if stdin else subprocess.DEVNULL)
    return res.stdout + res.stderr if res.returncode else res.stdout


def num(text, pattern):
    m = re.search(pattern, text, re.M)
    if not m:
        fail("pattern %r not found in:\n%s" % (pattern, text[:2000]))
    return int(m.group(1))


# ------------------------------------------------------------------ scenario


def scenario(fs):
    """[(path, parts, ops)]; parts are ('pat', n) or ('zero', n) and give the expected content;
    ops are guest commands with {m} for the mount directory. gone.txt comes last."""
    u = UNIT[fs]
    sc = []

    def add(path, parts, ops):
        sc.append((path, parts, ops))

    def write(path, n):
        add(path, [("pat", n)], ["head -c %d /payload/pat >{m}%s" % (n, path)])

    for name, n in (("s1", 1), ("s600", 600), ("sum1", u - 1), ("su", u), ("sup1", u + 1), ("s3u5", 3 * u + 5), ("s5000", 5000), ("inline100", 100)):
        write("/" + name, n)
    add("/empty", [], [": >{m}/empty"])
    add("/dir1/" + LONG, [("pat", 777)], ["mkdir {m}/dir1", "head -c 777 /payload/pat >{m}/dir1/" + LONG])
    add("/frag", [("pat", u), ("pat", u + 300)], [
        "head -c %d /payload/pat >{m}/frag" % u, "sync",
        "head -c %d /payload/pat >{m}/fragsep" % u, "sync",
        "head -c %d /payload/pat >>{m}/frag" % (u + 300), "sync"])
    sc.append(("/fragsep", [("pat", u)], []))
    if fs in ("ext4", "f2fs"):
        add("/sparse", [("pat", 700), ("zero", 4300)], ["head -c 700 /payload/pat >{m}/sparse", "truncate -s 5000 {m}/sparse"])
        add("/sparsemid", [("pat", 300), ("zero", 3 * u - 300), ("pat", 500)], [
            "head -c 300 /payload/pat >{m}/sparsemid",
            "dd if=/payload/pat of={m}/sparsemid bs=1 count=500 seek=%d conv=notrunc 2>/dev/null" % (3 * u)])
    gone = 5000 if fs == "f2fs" else 1500
    write("/gone.txt", gone)
    return sc


def expected_bytes(parts, pat):
    out = bytearray()
    for kind, n in parts:
        out += pat[:n] if kind == "pat" else b"\0" * n
    return bytes(out)


def cmd_ops(fs, m):
    for _, _, ops in scenario(fs):
        for op in ops:
            print(op.replace("{m}", m))
    print("sync")


def cmd_ops_delete(fs, m):
    print("rm %s/gone.txt" % m)
    print("sync")


# ------------------------------------------------------------------ geometry


def geom(fs, img):
    """(data_start, fs_size) as the filesystem's own tool states them."""
    if fs == "fat":
        t = tool("fsck.fat", "-v", "-n", img)
        return num(t, r"^Data area starts at byte (\d+)"), num(t, r"^\s*(\d+) sectors total") * num(t, r"^\s*(\d+) bytes per logical sector")
    if fs == "exfat":
        t = tool("dump.exfat", img)
        ss = 1 << num(t, r"^Sector Size Bits:\s+(\d+)")
        return num(t, r"^Cluster Heap Offset \(sector offset\):\s+(\d+)") * ss, num(t, r"^Volume Length\(sectors\):\s+(\d+)") * ss
    if fs == "ext4":
        t = tool("dumpe2fs", img)
        bs = num(t, r"^Block size:\s+(\d+)")
        return (num(t, r"Inode table at \d+-(\d+)") + 1) * bs, num(t, r"^Block count:\s+(\d+)") * bs
    if fs == "f2fs":
        t = tool("fsck.f2fs", "-l", img)
        return num(t, r"^main_blkaddr:\s+(\d+)") * 4096, num(t, r"^block_count:\s+(\d+)") * 4096
    fail("unknown fs " + fs)


# ------------------------------------------------------------------ allocation units per filesystem
# Every function returns {path: {"size": int|None, "inline": bool, "extents": [(logical unit, first unit number, count)]}}
# where a unit number is the filesystem's own: a cluster number (2-based) on FAT and exFAT, a block number on ext4
# and F2FS.


def fat_units(img, paths):
    out = {}
    for path in paths:
        lines = tool("mshowfat", "-i", img, "::" + path).strip().splitlines()
        m = re.fullmatch(r"::(.*?)((?: <[0-9,\-]*>)+)", lines[-1]) if lines else None
        ext, idx = [], 0
        if m:
            for part in filter(None, re.split(r"[ <>,]+", m.group(2))):
                lo, _, hi = part.partition("-")
                lo, hi = int(lo), int(hi or lo)
                ext.append((idx, lo, hi - lo + 1))
                idx += hi - lo + 1
        out[path] = {"size": None, "inline": False, "extents": ext}
    return out


def exfat_units(img, paths):
    with open(img, "rb") as f:
        d = f.read()
    bps = 1 << d[108]
    cs = bps << d[109]
    fat_off = int.from_bytes(d[80:84], "little") * bps
    heap = int.from_bytes(d[88:92], "little") * bps
    ncl = int.from_bytes(d[92:96], "little")

    def fatnext(c):
        return int.from_bytes(d[fat_off + 4 * c:fat_off + 4 * c + 4], "little")

    def chain(first, nofat, size):
        if nofat:
            return list(range(first, first + -(-size // cs)))
        res, c = [], first
        while 2 <= c <= ncl + 1 and len(res) <= ncl:
            res.append(c)
            c = fatnext(c)
        if c < 0xFFFFFFF8:
            fail("exFAT chain from %d ends at %#x" % (first, c))
        return res

    found = {}

    def walk(cl, prefix):
        raw = b"".join(d[heap + (c - 2) * cs:heap + (c - 1) * cs] for c in cl)
        i = 0
        while i + 32 <= len(raw) and raw[i] != 0:
            if raw[i] != 0x85:
                i += 32
                continue
            cnt = raw[i + 1]
            attr = int.from_bytes(raw[i + 4:i + 6], "little")
            st = raw[i + 32:i + 64]
            if st[0] != 0xC0:
                fail("exFAT entry set without a stream entry at %d" % i)
            nlen = st[3]
            name = b"".join(raw[i + 64 + 32 * k + 2:i + 64 + 32 * k + 32] for k in range(cnt - 1)).decode("utf-16-le")[:nlen]
            first = int.from_bytes(st[20:24], "little")
            dlen = int.from_bytes(st[24:32], "little")
            vdl = int.from_bytes(st[8:16], "little")
            nofat = bool(st[1] & 2)
            path = prefix + "/" + name
            if attr & 0x10:
                walk(chain(first, nofat, dlen), path)
            else:
                cl2 = chain(first, nofat, dlen) if dlen else []
                found[path] = (dlen, vdl, cl2)
            i += 32 * (1 + cnt)

    walk(chain(int.from_bytes(d[96:100], "little"), False, 0), "")
    out = {}
    for path in paths:
        if path not in found:
            fail("exFAT: %s is not in the directory tree" % path)
        dlen, vdl, cl = found[path]
        if vdl != dlen:
            fail("exFAT: %s has ValidDataLength %d != DataLength %d (the kernel driver is not expected to write that)" % (path, vdl, dlen))
        ext = [(i, c, 1) for i, c in enumerate(cl)]
        out[path] = {"size": dlen, "inline": False, "extents": ext}
    return out


def ext4_units(img, paths):
    out = {}
    for path in paths:
        t = tool("debugfs", "-R", "stat " + path, img)
        if "ETB" in t:
            fail("ext4: %s has an extent tree block (the oracle reads only inode extents)" % path)
        size = num(t, r"Size: (\d+)")
        ext = []
        for m in re.finditer(r"\((\d+)(?:-(\d+))?( \[uninit\])?\):(\d+)(?:-(\d+))?", t):
            if m.group(3):
                fail("ext4: %s has an uninitialised extent" % path)
            lo = int(m.group(1))
            hi = int(m.group(2) or lo)
            ext.append((lo, int(m.group(4)), hi - lo + 1))
        out[path] = {"size": size, "inline": False, "extents": ext}
    return out


def f2fs_units(img, paths):
    tree = tool("fsck.f2fs", "-t", "--dry-run", img)
    inos, stack = {}, []
    for line in tree.splitlines():
        m = re.match(r"^((?:\|   |    )*)[|`]-- (.*) <ino = 0x([0-9a-f]+)>, <encrypted \((\d)\)>$", line)
        if not m:
            continue
        depth = len(m.group(1)) // 4
        del stack[depth:]
        stack.append(m.group(2))
        inos["/" + "/".join(stack)] = int(m.group(3), 16)
    out = {}
    for path in paths:
        if path not in inos:
            fail("F2FS: %s is not in the fsck.f2fs tree" % path)
        # dump.f2fs -i asks whether to dump the file: stdin is closed so it stops after printing the inode
        t = tool("dump.f2fs", "-i", hex(inos[path]), img)
        size = num(t, r"^i_size\s+\[0x\s*[0-9a-f]+ : (\d+)\]")
        flags = num(t, r"^i_inline\s+\[0x\s*[0-9a-f]+ : (\d+)\]")
        if size > 900 * 4096:
            fail("F2FS: %s needs node blocks (the oracle reads the inode's own addresses)" % path)
        ext = []
        # with inline data the i_addr area holds the file's bytes, not block addresses
        for m in ([] if flags & 2 else re.finditer(r"^i_addr\[0x([0-9a-f]+)\]\s+\[0x\s*[0-9a-f]+ : (\d+)\]", t, re.M)):
            idx, blk = int(m.group(1), 16), int(m.group(2))
            if blk == 0:
                continue
            if blk >= 0xFFFFFFFF:
                fail("F2FS: %s has a reserved (NEW_ADDR) block at %d" % (path, idx))
            if ext and ext[-1][0] + ext[-1][2] == idx and ext[-1][1] + ext[-1][2] == blk:
                ext[-1] = (ext[-1][0], ext[-1][1], ext[-1][2] + 1)
            else:
                ext.append((idx, blk, 1))
        out[path] = {"size": size, "inline": bool(flags & 2), "extents": ext}
    return out


UNITS = {"fat": fat_units, "exfat": exfat_units, "ext4": ext4_units, "f2fs": f2fs_units}


def unit_offset(fs, data_start, n):
    u = UNIT[fs]
    return data_start + (n - 2) * u if fs in ("fat", "exfat") else n * u


def read_at(f, off, n):
    f.seek(off)
    buf = f.read(n)
    if len(buf) != n:
        fail("short read of %d bytes at %d" % (n, off))
    return buf


def cmd_units(fs, img, patfile, outfile, mode="all"):
    u = UNIT[fs]
    pat = open(patfile, "rb").read()
    sc = scenario(fs)
    if mode == "live":  # after the delete: /gone.txt is no longer in the tree
        sc = [e for e in sc if e[0] != "/gone.txt"]
    elif mode != "all":
        fail("units mode %r" % mode)
    data_start, fs_size = geom(fs, img)
    got = UNITS[fs](img, [p for p, _, _ in sc])
    res = {}
    owner = {}
    with open(img, "rb") as f:
        for path, parts, _ in sc:
            g = got[path]
            want = expected_bytes(parts, pat)
            size = len(want)
            if g["size"] is not None and g["size"] != size:
                fail("%s: the tool states size %d, the scenario wrote %d" % (path, g["size"], size))
            if g["inline"]:
                if g["extents"]:
                    fail("%s: inline file with blocks" % path)
            elif size > 0 and not g["extents"]:
                fail("%s: %d bytes and no allocation units listed" % (path, size))
            if fs in ("fat", "exfat") and size > 0 and sum(c for _, _, c in g["extents"]) != -(-size // u):
                fail("%s: the chain has %d clusters for %d bytes" % (path, sum(c for _, _, c in g["extents"]), size))
            mapped = {}
            for lo, n, cnt in g["extents"]:
                for k in range(cnt):
                    if lo + k in mapped:
                        fail("%s: logical unit %d listed twice" % (path, lo + k))
                    mapped[lo + k] = n + k
                    off = unit_offset(fs, data_start, n + k)
                    if off in owner:
                        fail("%s and %s share the unit at %d" % (path, owner[off], off))
                    owner[off] = path
            # the unit map is checked against the bytes: every mapped logical range holds the pattern's
            # content, every unmapped one (a hole) is zero in the expectation
            if not g["inline"]:
                for idx in range((size + u - 1) // u):
                    lo, hi = idx * u, min(size, (idx + 1) * u)
                    if idx in mapped:
                        if read_at(f, unit_offset(fs, data_start, mapped[idx]), hi - lo) != want[lo:hi]:
                            fail("%s: logical unit %d does not hold the written bytes" % (path, idx))
                    elif any(want[lo:hi]):
                        fail("%s: logical unit %d holds data but is not allocated" % (path, idx))
            res[path] = {"size": size, "inline": g["inline"], "extents": [list(e) for e in g["extents"]]}
    json.dump({"fs": fs, "unit": u, "data_start": data_start, "fs_size": fs_size, "files": res}, open(outfile, "w"), indent=1, sort_keys=True)
    print("units %s: %d files, data_start=%d fs_size=%d" % (fs, len(res), data_start, fs_size))


def cmd_same(a, b):
    ua, ub = json.load(open(a)), json.load(open(b))
    for k in ("fs", "unit", "data_start", "fs_size"):
        if ua[k] != ub[k]:
            fail("%s changed between the two reads: %r vs %r" % (k, ua[k], ub[k]))
    for path, v in ua["files"].items():
        if path == "/gone.txt":
            continue
        if ub["files"].get(path) != v:
            fail("%s: units changed between the two reads: %r vs %r" % (path, v, ub["files"].get(path)))
    print("same: %d live files have identical units" % len(ub["files"]))


# ------------------------------------------------------------------ volume tail and expectation


def cmd_tail(img, fs_size):
    fs_size = int(fs_size)
    with open(img, "r+b") as f:
        f.seek(0, 2)
        if f.tell() != fs_size:
            fail("image is %d bytes, the filesystem %d: nothing may follow it yet" % (f.tell(), fs_size))
        if fs_size % 64:
            fail("filesystem size is not a multiple of 64")
        f.write(SENT * (TAIL_A // 64))
        f.write(b"\0" * TAIL_B)
        f.write((SENT * (TAIL_C // 64 + 1))[:TAIL_C])  # fs_size + 1 MiB is a multiple of 64: the phase is 0
    print("tail: appended %d bytes after %d" % (TAIL_A + TAIL_B + TAIL_C, fs_size))


def fingerprint(buf):
    return {"sha256": hashlib.sha256(buf).hexdigest(), "uniform": buf.count(buf[:1]) == len(buf)}


def volume_oracle(f, fs_size, image_size):
    length = image_size - fs_size
    if length != TAIL_A + TAIL_B + TAIL_C:
        fail("volume tail is %d bytes, want %d" % (length, TAIL_A + TAIL_B + TAIL_C))
    segs, cur, uc, ub = [], None, 0, 0
    for off in range(0, length, CHUNK):
        buf = read_at(f, fs_size + off, min(CHUNK, length - off))
        uni = buf.count(buf[:1]) == len(buf)
        fill = buf[0] if uni else None
        if uni:
            uc += 1
            ub += len(buf)
        if cur is not None and cur["uniform"] == uni and cur["fill"] == fill:
            cur["data"] += buf  # 1 MiB at most per segment: fine in memory
        else:
            cur = {"image_offset": fs_size + off, "uniform": uni, "fill": fill, "data": bytearray(buf)}
            segs.append(cur)
    out = []
    for s in segs:
        e = {"image_offset": s["image_offset"], "length": len(s["data"]), "sha256": hashlib.sha256(bytes(s["data"])).hexdigest(), "uniform": s["uniform"]}
        if s["uniform"]:
            e["fill"] = s["fill"]
        out.append(e)
    return {"image_offset": fs_size, "length": length, "chunk": CHUNK, "segments": out, "uniform_chunks": uc, "uniform_bytes": ub}


def cmd_expect(fs, img, unitsfile, genfile, outfile):
    u = UNIT[fs]
    uj = json.load(open(unitsfile))
    gen = json.load(open(genfile))
    ds, fs_size = uj["data_start"], uj["fs_size"]
    if uj["fs"] != fs or uj["unit"] != u:
        fail("units file is for another filesystem")
    files, deleted = [], []
    counts = {"slack": 0, "uniform_slack": 0, "nonuniform_slack": 0, "exact-fit": 0, "no-runs": 0, "trailing-hole": 0}
    with open(img, "rb") as f:
        f.seek(0, 2)
        image_size = f.tell()
        for path in sorted(uj["files"]):
            v = uj["files"][path]
            size = v["size"]
            mapped = {}
            for lo, n, cnt in v["extents"]:
                for k in range(cnt):
                    mapped[lo + k] = n + k
            # units: physical unit ranges in logical order, merged where one follows the other
            units = []
            for idx in sorted(mapped):
                n = mapped[idx]
                if units and units[-1][1] + 1 == n:
                    units[-1][1] = n
                else:
                    units.append([n, n])
            last = (size - 1) // u if size > 0 else -1
            if path == "/gone.txt":
                if last not in mapped:
                    fail("/gone.txt has no last unit to record")
                deleted.append({"path": path, "size": size, "last_unit_image_offset": unit_offset(fs, ds, mapped[last]), "unit": u})
                continue
            entry = {"path": path, "size": size, "units": units, "extents": v["extents"], "slack": None}
            if not mapped:
                entry["outcome"] = "no-runs"
            elif last not in mapped:
                entry["outcome"] = "trailing-hole"
            elif size % u == 0:
                entry["outcome"] = "exact-fit"
            else:
                entry["outcome"] = "slack"
                used = size - last * u
                off = unit_offset(fs, ds, mapped[last]) + used
                buf = read_at(f, off, u - used)
                entry["slack"] = dict(fs_offset=off, image_offset=off, length=u - used, **fingerprint(buf))
                counts["slack"] += 1
                counts["uniform_slack" if entry["slack"]["uniform"] else "nonuniform_slack"] += 1
            if entry["outcome"] != "slack":
                counts[entry["outcome"]] += 1
            files.append(entry)
        vol = volume_oracle(f, fs_size, image_size)
    if vol["uniform_chunks"] != 128 or vol["uniform_bytes"] != TAIL_B:
        fail("volume tail has %d uniform chunks / %d bytes, want 128 / %d" % (vol["uniform_chunks"], vol["uniform_bytes"], TAIL_B))
    # what the fixture needs (3F-R16, 3F-R26)
    need = []
    if fs in ("fat", "exfat") and counts["nonuniform_slack"] < 3:
        need.append("%d non-uniform slack tails, need 3" % counts["nonuniform_slack"])
    if counts["exact-fit"] < 1:
        need.append("no exact-fit file")
    if counts["uniform_slack"] < 1:
        need.append("no uniform slack tail")
    if fs in ("ext4", "f2fs") and counts["trailing-hole"] < 1:
        need.append("no trailing-hole file")
    if fs == "f2fs" and counts["no-runs"] < 1:
        need.append("no no-runs file")
    if fs in ("ext4", "f2fs") and counts["nonuniform_slack"]:
        need.append("the kernel left non-uniform residue on %s: the expectation 'the tail is zero' does not hold" % fs)
    if not deleted:
        need.append("no deleted file")
    if need:
        fail("%s fixture is not usable: %s" % (fs, "; ".join(need)))
    out = {
        "generator": gen,
        "type": TYPE[fs], "unit": u, "fs_size": fs_size, "image_size": image_size, "data_start": ds,
        "sentinel": SENT.decode(),
        "files": files, "deleted": deleted, "volume": vol, "counts": counts,
    }
    json.dump(out, open(outfile, "w"), indent=1)
    print("expect %s: %s" % (fs, json.dumps(counts, sort_keys=True)))


def main():
    a = sys.argv[1:]
    if not a:
        fail(__doc__)
    cmd, args = a[0], a[1:]
    table = {"ops": cmd_ops, "ops-delete": cmd_ops_delete, "units": cmd_units, "same": cmd_same, "tail": cmd_tail, "expect": cmd_expect}
    if cmd not in table:
        fail("unknown command " + cmd)
    table[cmd](*args)


main()

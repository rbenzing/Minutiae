#!/usr/bin/env python3
"""Independent oracle of the FAT recovery fixtures (3B). Never Minutiae.

The images are written and deleted by the KERNEL vfat driver (fat_recover.sh).
This script reads a finished image with its own FAT/directory parser, checks it
against `mshowfat` (mtools) and `fsck.fat -v`, and against the SOURCE TREE the
guest was fed. It decides, from the finished image alone, what a reader that
recovers deleted files by the contiguous-allocation rule must find.

usage:
  fat_recover_oracle.py snap  <img> <src tree> <out.json> <expect-free|any>
      a snapshot of the live tree: the cluster chains of every live file and
      directory (own parser == mshowfat), the free-cluster count, and the bytes
      of every live file against the source tree. Taken while files are still
      live, so the true chains of files deleted later are known.
  fat_recover_oracle.py final <img> <src tree> <fsck -v output> <snap.json>... > oracle.json
  fat_recover_oracle.py families <expect.json>...
      the REQUIRED outcome families must occur across the images together.
"""
import hashlib
import json
import os
import re
import struct
import subprocess
import sys

CLUSTER = 512


def fail(msg):
    sys.exit("fat_recover_oracle: " + msg)


def ranges(clusters):
    out = []
    for c in sorted(clusters):
        if out and c == out[-1][1] + 1:
            out[-1][1] = c
        else:
            out.append([c, c])
    return out


def runs(clusters):
    """Adjacent ascending clusters merged, in FILE order (a chain may wrap: [70, 2848] then [2, 2])."""
    out = []
    for c in clusters:
        if out and c == out[-1][1] + 1:
            out[-1][1] = c
        else:
            out.append([c, c])
    return out


def lfn_checksum(name11):
    s = 0
    for b in name11:
        s = (((s & 1) << 7) + (s >> 1) + b) & 0xFF
    return s


class Vol:
    def __init__(self, path):
        d = self.d = open(path, "rb").read()
        self.bps, self.spc, self.res, self.nfats, self.rootents = struct.unpack_from("<HBHBH", d, 11)
        total = struct.unpack_from("<H", d, 19)[0] or struct.unpack_from("<I", d, 32)[0]
        self.fatsz = struct.unpack_from("<H", d, 22)[0] or struct.unpack_from("<I", d, 36)[0]
        self.cs = self.bps * self.spc
        root_sectors = (self.rootents * 32 + self.bps - 1) // self.bps
        self.fat0 = self.res * self.bps
        self.root_off = (self.res + self.nfats * self.fatsz) * self.bps
        self.data_start = self.root_off + root_sectors * self.bps
        self.nclusters = (total - self.res - self.nfats * self.fatsz - root_sectors) // self.spc
        self.type = 12 if self.nclusters < 4085 else 16 if self.nclusters < 65525 else 32
        self.root_cluster = struct.unpack_from("<I", d, 44)[0] if self.type == 32 else 0
        if self.cs != CLUSTER:
            fail("cluster size %d, want %d" % (self.cs, CLUSTER))

    def fat(self, c):
        d = self.d
        if self.type == 12:
            v = struct.unpack_from("<H", d, self.fat0 + c * 3 // 2)[0]
            return v >> 4 if c & 1 else v & 0xFFF
        if self.type == 16:
            return struct.unpack_from("<H", d, self.fat0 + c * 2)[0]
        return struct.unpack_from("<I", d, self.fat0 + c * 4)[0] & 0x0FFFFFFF

    def is_eoc(self, v):
        return v >= {12: 0xFF8, 16: 0xFFF8, 32: 0x0FFFFFF8}[self.type]

    def chain(self, first):
        out, seen, c = [], set(), first
        while True:
            if c < 2 or c > self.nclusters + 1 or c in seen:
                fail("bad cluster %d in the chain from %d" % (c, first))
            seen.add(c)
            out.append(c)
            n = self.fat(c)
            if self.is_eoc(n):
                return out
            c = n

    def cluster(self, c):
        o = self.data_start + (c - 2) * self.cs
        return self.d[o:o + self.cs]

    def data(self, clusters, size):
        return b"".join(self.cluster(c) for c in clusters)[:size]

    def fat_free_count(self):
        return sum(1 for c in range(2, self.nclusters + 2) if self.fat(c) == 0)

    def dir_bytes(self, first):
        if first == 0 and self.type != 32:
            return self.d[self.root_off:self.data_start]
        return b"".join(self.cluster(c) for c in self.chain(first or self.root_cluster))


def sfn_text(name11, ntres):
    base, ext = name11[:8].rstrip(b" "), name11[8:].rstrip(b" ")
    if base[:1] == b"\x05":
        base = b"\xe5" + base[1:]
    b, e = base.decode("cp437"), ext.decode("cp437")
    if ntres & 0x08:
        b = b.lower()
    if ntres & 0x10:
        e = e.lower()
    return b + ("." + e if e else "")


def lfn_part(e):
    raw = e[1:11] + e[14:26] + e[28:32]
    out = []
    for i in range(0, 26, 2):
        u = raw[i] | raw[i + 1] << 8
        if u in (0, 0xFFFF):
            break
        out.append(chr(u))
    return "".join(out)


def entries(raw):
    out = []
    for i in range(0, len(raw), 32):
        e = raw[i:i + 32]
        if e[0] == 0:
            break
        out.append(e)
    return out


def is_lfn(e):
    return e[11] & 0x3F == 0x0F


def short_fields(v, e):
    first = e[26] | e[27] << 8
    if v.type == 32:
        first |= (e[20] | e[21] << 8) << 16
    return first, struct.unpack_from("<I", e, 28)[0]


class Src:
    def __init__(self, root):
        self.root = root

    def path(self, p):
        return os.path.join(self.root, p.lstrip("/"))

    def children(self, dirpath):
        d = self.path(dirpath)
        if not os.path.isdir(d):
            fail("no source directory for %s" % dirpath)
        return [(n, os.path.isdir(os.path.join(d, n)), os.path.getsize(os.path.join(d, n))) for n in sorted(os.listdir(d))]

    def sha(self, p):
        return hashlib.sha256(open(self.path(p), "rb").read()).hexdigest()


def short83(name):
    """The upper-case 8.3 form of a source name when it has one (else None)."""
    base, dot, ext = name.upper().partition(".")
    if len(base) > 8 or len(ext) > 3 or not base or not re.fullmatch(r"[A-Z0-9_$~!#%&'()@^`{}-]*", base + ext):
        return None
    return (base.ljust(8) + ext.ljust(3)).encode("cp437")


def scan(v, src, ents, dirpath, live, deleted, listed):
    """Live entries go to `live` ({'path', 'first', 'size', 'dir'}), deleted ones to `deleted`.
    A deleted directory's first cluster is scanned too (its chain is gone), with listed False."""
    for i, e in enumerate(ents):
        if is_lfn(e) or e[11] & 0x08 or e[0] in (0x2E,):
            continue
        first, size = short_fields(v, e)
        isdir = bool(e[11] & 0x10)
        if e[0] != 0xE5:
            lfn = []
            j = i - 1
            while j >= 0 and is_lfn(ents[j]) and ents[j][0] != 0xE5:
                lfn.append(ents[j])
                j -= 1
            name = sfn_text(bytes(e[:11]), e[12])
            if lfn and all(x[13] == lfn_checksum(bytes(e[:11])) for x in lfn) and lfn[-1][0] & 0x40:
                name = "".join(lfn_part(x) for x in lfn)
            live.append({"path": dirpath.rstrip("/") + "/" + name, "first": first, "size": size, "dir": isdir})
            continue
        # a deleted short entry: its deleted long-name entries, checksum against the name with the first byte recovered
        lfn, j, orig = [], i - 1, None
        if j >= 0 and is_lfn(ents[j]) and ents[j][0] == 0xE5:
            want = ents[j][13]
            cands = [b for b in range(256) if lfn_checksum(bytes([b]) + bytes(e[1:11])) == want]
            if len(cands) != 1:
                fail("no unique first byte for the checksum of the deleted entry at %s #%d" % (dirpath, i))
            orig = cands[0]
            while j >= 0 and is_lfn(ents[j]) and ents[j][0] == 0xE5 and ents[j][13] == want:
                lfn.append(ents[j])
                j -= 1
        text = "".join(lfn_part(x) for x in lfn)
        rec = {"dirpath": dirpath, "first": first, "size": size, "dir": isdir, "listed": listed, "lfn_text": text,
               "lfn_count": len(lfn), "short": bytes(e[:11]), "ntres": e[12]}
        rec["srcname"] = match(src, rec)
        rec["path"] = dirpath.rstrip("/") + "/" + rec["srcname"]
        if lfn:
            rec["name_state"] = "lfn-intact" if text == rec["srcname"] else "lfn-tail-lost" if rec["srcname"].startswith(text) and text else None
            if rec["name_state"] is None:
                fail("%s: the long-name entries spell %r, not a prefix of %r" % (rec["path"], text, rec["srcname"]))
        else:
            rec["name_state"] = "short-only"
        deleted.append(rec)
        if isdir and first >= 2:
            sub = entries(v.cluster(first))  # only the first cluster is left to read: the chain was zeroed
            inner = []
            scan(v, src, sub, rec["path"], inner, deleted, False)
            if inner:
                fail("%s: a live entry inside a deleted directory" % rec["path"])


def match(src, rec):
    cands = [c for c in src.children(rec["dirpath"]) if c[1] == rec["dir"] and (rec["dir"] or c[2] == rec["size"])]
    if rec["lfn_text"]:
        sub = [c for c in cands if c[0].startswith(rec["lfn_text"])]
        cands = sub
    else:
        sub = []
        for c in cands:
            s = short83(c[0])
            if s is None or s[1:] == rec["short"][1:]:
                sub.append(c)
        cands = sub
    if len(cands) != 1:
        fail("cannot tie the deleted entry %r (size %d, dir %s, long name %r) to one source name: %s" % (rec["short"], rec["size"], rec["dirpath"], rec["lfn_text"], cands))
    return cands[0][0]


def walk_live(v, src, dirpath, first, live, deleted):
    ents = entries(v.dir_bytes(first))
    mine = []
    scan(v, src, ents, dirpath, mine, deleted, True)
    for m in mine:
        live.append(m)
        if m["dir"]:
            walk_live(v, src, m["path"], m["first"], live, deleted)


def mshowfat(img, path):
    res = subprocess.run(["mshowfat", "-i", img, "::" + path], capture_output=True, text=True, check=True)
    line = res.stdout.strip().splitlines()[-1]
    m = re.fullmatch(r"::(.*?)((?: <[0-9,\-]*>)+)", line)
    if not m:
        if line.endswith("Root directory or empty file"):
            return []
        fail("cannot parse mshowfat output %r" % line)
    out = []
    for part in filter(None, re.split(r"[ <>,]+", m.group(2))):
        lo, _, hi = part.partition("-")
        out.append([int(lo), int(hi or lo)])
    return out


def analyse_live(img, v, src):
    """The live tree with chains (own parser cross-checked against mshowfat) and the free clusters."""
    live, deleted = [], []
    walk_live(v, src, "/", 0, live, deleted)
    used, chains, files = {}, {}, []
    if v.type == 32:
        for c in v.chain(v.root_cluster):
            used[c] = "/"
    for m in live:
        own = v.chain(m["first"]) if m["first"] >= 2 else []
        theirs = mshowfat(img, m["path"])
        if runs(own) != theirs:
            fail("%s: own chain %s differs from mshowfat %s" % (m["path"], runs(own), theirs))
        if m["dir"]:
            own = v.chain(m["first"])
        else:
            need = -(-m["size"] // v.cs)
            if len(own) != need:
                fail("%s: chain holds %d clusters, size %d needs %d" % (m["path"], len(own), m["size"], need))
        for c in own:
            if c in used:
                fail("cluster %d is in two chains (%s and %s)" % (c, used[c], m["path"]))
            used[c] = m["path"]
        chains[m["path"]] = runs(own)
        if not m["dir"]:
            files.append(dict(m, clusters=own))
    free = [c for c in range(2, v.nclusters + 2) if c not in used]
    if len(free) != v.fat_free_count():
        fail("%d clusters held by no live chain, but the FAT has %d zero entries" % (len(free), v.fat_free_count()))
    return live, deleted, files, chains, used, free


def check_files(v, src, files):
    out = []
    for f in files:
        data = v.data(f["clusters"], f["size"])
        sha = hashlib.sha256(data).hexdigest()
        p = src.path(f["path"])
        if os.path.exists(p):
            want = open(p, "rb").read()
            if want != data:
                fail("%s: live bytes differ from the source tree" % f["path"])
        elif f["path"] == "/fill.bin":
            if data.strip(b"\x00"):
                fail("/fill.bin is not all zero")
        else:
            fail("live file %s has no source" % f["path"])
        out.append({"path": f["path"], "size": f["size"], "sha256": sha, "chain": runs(f["clusters"])})
    return sorted(out, key=lambda x: x["path"])


def cmd_snap(img, srcroot, out, expect_free):
    v, src = Vol(img), Src(srcroot)
    _, _, files, chains, _, free = analyse_live(img, v, src)
    check_files(v, src, files)
    if expect_free != "any" and len(free) != int(expect_free):
        fail("%d free clusters, want %s (the volume must be full where the plan fills it)" % (len(free), expect_free))
    json.dump({"chains": chains, "free": len(free)}, open(out, "w"))


def fsck_numbers(report):
    def num(pattern):
        m = re.search(pattern, report, re.M)
        if not m:
            fail("fsck.fat -v report lacks " + pattern)
        return int(m.group(1))

    return {
        "cluster_size": num(r"^\s*(\d+) bytes per cluster"),
        "data_start": num(r"^Data area starts at byte (\d+)"),
        "cluster_count": num(r"^\s*(\d+) data clusters"),
        "used": num(r"^\S+: \d+ files, (\d+)/\d+ clusters"),
    }


PRIMARY = {
    "/contiguous/target.bin": "contiguous",
    "/zero-size/target.dat": "zero-size",
    "/reuse-all/target.bin": "reuse-all",
    "/reuse-head/target.bin": "reuse-head",
    "/cut-l/target.bin": "cut-l",
    "/cut-h/target.bin": "cut-h",
    "/frag-live/f.bin": "frag-live",
    "/frag-dead/f.bin": "frag-dead",
    "/lfn/a-long-deleted-name-for-slot-reuse.bin": "lfn",
    "/gone": "gone",
}


def family_of(path):
    if path in PRIMARY:
        return PRIMARY[path]
    top = path.split("/")[1]
    for suffix in ("-w", "-hid"):
        if top.endswith(suffix):
            top = top[: -len(suffix)]
    return top + "-aux"  # a helper of a scenario: a hole, a writer or a hidden overwriter


def cmd_final(img, srcroot, fsck, snaps):
    v, src = Vol(img), Src(srcroot)
    g = fsck_numbers(open(fsck, encoding="utf-8").read())
    if (g["cluster_size"], g["data_start"], g["cluster_count"]) != (v.cs, v.data_start, v.nclusters):
        fail("own geometry %s differs from fsck.fat -v %s" % ((v.cs, v.data_start, v.nclusters), g))
    live, deleted, files, chains, used, free = analyse_live(img, v, src)
    if len(used) != g["used"]:
        fail("live chains hold %d clusters, fsck.fat -v reports %d in use" % (len(used), g["used"]))
    live_files = check_files(v, src, files)
    true = {}
    for s in snaps:  # later snapshots win
        true.update(json.load(open(s))["chains"])
    freeset = set(free)
    out = []
    for d in sorted(deleted, key=lambda r: (not r["listed"], r["path"])):
        rec = {"path": d["path"], "family": family_of(d["path"]), "listed": d["listed"], "size": d["size"],
               "sha256": "" if d["dir"] else src.sha(d["path"]), "first_cluster": d["first"],
               "true_chain": [] if d["size"] == 0 and not d["dir"] else true.get(d["path"]), "name_state": d["name_state"]}
        if rec["true_chain"] is None:
            fail("no snapshot saw %s live: its true chain is unknown" % d["path"])
        if rec["true_chain"] and rec["true_chain"][0][0] != d["first"]:
            fail("%s: the directory entry's first cluster %d is not the snapshot's %s" % (d["path"], d["first"], rec["true_chain"]))
        e = {"outcome": "skip", "skip": "", "method": "", "range": [], "captured": [], "cut": False,
             "equal_source": False, "overlaps": [], "confidence": 0, "assumptions": []}
        if d["dir"]:
            e["skip"] = "not-a-file"
        elif d["size"] == 0:
            e["skip"] = "empty"
        else:
            n = -(-d["size"] // v.cs)
            lo, hi = d["first"], d["first"] + n - 1
            if lo < 2 or hi > v.nclusters + 1:
                fail("%s: range %d-%d leaves the volume" % (d["path"], lo, hi))
            k = 0
            while k < n and lo + k in freeset:
                k += 1
            e["range"] = [[lo, hi]]
            e["captured"] = [[lo, lo + k - 1]] if k else []
            e["cut"] = 0 < k < n
            if k == 0:
                e["skip"] = "no-free-bytes"
            else:
                e["outcome"], e["method"], e["assumptions"] = "recover", "fat-contiguous", ["contiguous-allocation"]
                got = hashlib.sha256(v.data(range(lo, lo + k), d["size"])).hexdigest()
                e["equal_source"] = (not e["cut"]) and got == rec["sha256"]
        rec["expect"] = e
        out.append(rec)
    for a in out:
        if not a["listed"] or a["expect"]["outcome"] != "recover":
            continue
        (alo, ahi), = a["expect"]["captured"]
        for b in out:
            if b is a or not b["listed"] or b["expect"]["outcome"] != "recover":
                continue
            (blo, bhi), = b["expect"]["captured"]
            if alo <= bhi and blo <= ahi:
                a["expect"]["overlaps"].append(b["path"])
        a["expect"]["overlaps"].sort()
    for a in out:
        e = a["expect"]
        if e["outcome"] == "recover":
            e["confidence"] = min(55, 20 if e["cut"] else 55, 30 if e["overlaps"] else 55)
    fam = {}

    def bump(k):
        fam[k] = fam.get(k, 0) + 1

    for a in out:
        e = a["expect"]
        if e["outcome"] == "skip":
            bump("empty" if e["skip"] == "empty" else "skip-" + e["skip"])
        elif e["cut"]:
            bump("cut-prefix")
        elif e["equal_source"]:
            bump("contiguous-equal")
        else:
            bump("not-equal-complete")
        if e["overlaps"]:
            bump("overlap")
        if a["name_state"] == "lfn-tail-lost":
            bump("lfn-tail-lost")
        if a["name_state"] == "short-only":
            bump("short-only")
    seen = {a["family"] for a in out}
    for name in set(PRIMARY.values()):
        if name not in seen:
            fail("scenario family %s is missing from %s" % (name, img))
    by = {a["path"]: a for a in out}
    for p in ("/contiguous/target.bin",):
        if len(by[p]["true_chain"]) != 1:
            fail("%s is not contiguous on the image: %s" % (p, by[p]["true_chain"]))
    for p in ("/frag-live/f.bin", "/frag-dead/f.bin"):
        if len(by[p]["true_chain"]) != 2:
            fail("%s is not fragmented in two pieces: %s" % (p, by[p]["true_chain"]))
    geo = {"type": "fat%d" % v.type, "cluster_size": v.cs, "data_start": v.data_start, "cluster_count": v.nclusters}
    json.dump({**geo, "live": live_files, "free_clusters": ranges(free),
               "deleted_entry_count": sum(1 for d in deleted if d["listed"]), "deleted": out, "families": fam},
              sys.stdout, indent=1)
    print()


REQUIRED = ["contiguous-equal", "empty", "cut-prefix", "skip-no-free-bytes", "not-equal-complete", "overlap", "lfn-tail-lost", "short-only"]


def cmd_families(paths):
    total = {}
    for p in paths:
        for k, n in json.load(open(p))["families"].items():
            total[k] = total.get(k, 0) + n
    for name in REQUIRED:
        if total.get(name, 0) < 1:
            fail("family %s missing: the allocation-order row used (ascending) did not produce it; re-rule, do not edit the ops" % name)
    print("families across %d images: %s" % (len(paths), json.dumps(total, sort_keys=True)), file=sys.stderr)


def main():
    a = sys.argv[1:]
    if len(a) == 5 and a[0] == "snap":
        cmd_snap(*a[1:])
    elif len(a) >= 5 and a[0] == "final":
        cmd_final(a[1], a[2], a[3], a[4:])
    elif len(a) >= 2 and a[0] == "families":
        cmd_families(a[1:])
    else:
        sys.exit(__doc__)


main()

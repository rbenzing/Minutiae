#!/usr/bin/env python3
"""Independent oracle of the exFAT recovery fixture (3B). Never Minutiae.

The image is written and deleted by the KERNEL exfat driver (exfat_recover.sh).
This script reads a finished image with its own boot-sector, FAT, allocation
bitmap and entry-set parser, checks it against `dump.exfat` (geometry and its
"Free Clusters" statistic), `fsck.exfat -n` (run by the caller) and the SOURCE
TREE the guest was fed, and decides from the finished image alone what a reader
must find for every deleted file.

usage:
  exfat_recover_oracle.py snap  <img> <src tree> <out.json> <expect-free|any>
      a snapshot of the live tree: chains, NoFatChain flags and the free count,
      taken while the files are still live (the true chains of files deleted
      later), with the bytes of every live file checked against the source.
  exfat_recover_oracle.py final <img> <src tree> <snap.json>... > oracle.json
  exfat_recover_oracle.py families <expect.json>...
"""
import hashlib
import json
import os
import re
import struct
import subprocess
import sys


def fail(msg):
    sys.exit("exfat_recover_oracle: " + msg)


def ranges(clusters):
    out = []
    for c in sorted(clusters):
        if out and c == out[-1][1] + 1:
            out[-1][1] = c
        else:
            out.append([c, c])
    return out


def runs(clusters):
    """Adjacent ascending clusters merged, in chain order."""
    out = []
    for c in clusters:
        if out and c == out[-1][1] + 1:
            out[-1][1] = c
        else:
            out.append([c, c])
    return out


class Vol:
    def __init__(self, path):
        d = self.d = open(path, "rb").read()
        if d[3:11] != b"EXFAT   ":
            fail("not an exFAT boot sector")
        self.bps = 1 << d[108]
        self.cs = self.bps << d[109]
        self.fat_off = struct.unpack_from("<I", d, 80)[0] * self.bps
        self.heap_off = struct.unpack_from("<I", d, 88)[0] * self.bps
        self.nclusters = struct.unpack_from("<I", d, 92)[0]
        self.root = struct.unpack_from("<I", d, 96)[0]
        self.nfats = d[110]
        if self.cs != 4096:
            fail("cluster size %d, want 4096" % self.cs)

    def fat(self, c):
        return struct.unpack_from("<I", self.d, self.fat_off + 4 * c)[0]

    def valid(self, c):
        return 2 <= c <= self.nclusters + 1

    def need(self, length):
        return -(-length // self.cs)

    def chain(self, first, nofat, length):
        if first == 0:
            return []
        n = self.need(length)
        if nofat:
            out = list(range(first, first + n))
            if not all(self.valid(c) for c in out):
                fail("contiguous range %d+%d leaves the volume" % (first, n))
            return out
        out, seen, c = [], set(), first
        while len(out) < n:
            if not self.valid(c) or c in seen:
                fail("bad cluster %d in the chain from %d" % (c, first))
            seen.add(c)
            out.append(c)
            c = self.fat(c)
            if len(out) < n and c >= 0xFFFFFFF8:
                fail("chain from %d ends after %d of %d clusters" % (first, len(out), n))
        return out

    def cluster(self, c):
        o = self.heap_off + (c - 2) * self.cs
        return self.d[o:o + self.cs]

    def data(self, clusters, size):
        return b"".join(self.cluster(c) for c in clusters)[:size]

    def bitmap(self, first, length):
        raw = self.data(range(first, first + self.need(length)), length)
        return raw


def utf16(raw):
    return raw.decode("utf-16-le")


class Src:
    def __init__(self, root):
        self.root = root

    def path(self, p):
        return os.path.join(self.root, p.lstrip("/"))

    def sha(self, p):
        return hashlib.sha256(open(self.path(p), "rb").read()).hexdigest()


def parse_dir(v, data, dirpath, live, deleted, listed, specials):
    n = len(data) // 32
    i = 0
    while i < n:
        e = data[i * 32:(i + 1) * 32]
        t = e[0]
        if t == 0:
            break
        if t in (0x85, 0x05):
            cnt = e[1]
            if cnt < 2 or i + 1 + cnt > n:
                fail("%s: entry set at %d claims %d secondaries" % (dirpath, i, cnt))
            sec = [data[(i + 1 + k) * 32:(i + 2 + k) * 32] for k in range(cnt)]
            used = t == 0x85
            st, names = sec[0], sec[1:]
            if st[0] != (0xC0 if used else 0x40) or any(x[0] != (0xC1 if used else 0x41) for x in names):
                fail("%s: entry set at %d is partly overwritten (types %s)" % (dirpath, i, [hex(x[0]) for x in sec]))
            nlen = st[3]
            name = utf16(b"".join(x[2:32] for x in names))[:nlen]
            if len(name) != nlen:
                fail("%s: entry set at %d spells %d of %d name characters" % (dirpath, i, len(name), nlen))
            rec = {
                "path": dirpath.rstrip("/") + "/" + name,
                "first": struct.unpack_from("<I", st, 20)[0],
                "size": struct.unpack_from("<Q", st, 24)[0],
                "vdl": struct.unpack_from("<Q", st, 8)[0],
                "nofat": bool(st[1] & 2),
                "dir": bool(struct.unpack_from("<H", e, 4)[0] & 0x10),
            }
            if used:
                live.append(rec)
            else:
                rec["listed"] = listed
                deleted.append(rec)
                if rec["dir"] and rec["first"] >= 2:
                    # a deleted directory: only its first cluster is read, whatever its stream says
                    inner = []
                    parse_dir(v, v.cluster(rec["first"]), rec["path"], inner, deleted, False, specials)
                    if inner:
                        fail("%s: a live entry inside a deleted directory" % rec["path"])
            i += 1 + cnt
            continue
        if t in (0x81, 0x82):
            specials[t] = (struct.unpack_from("<I", e, 20)[0], struct.unpack_from("<Q", e, 24)[0])
        elif t in (0x40, 0x41, 0xC0, 0xC1):
            fail("%s: a stray secondary entry (type %#x) at %d" % (dirpath, t, i))
        i += 1


def walk_live(v, dirpath, first, nofat, size, live, deleted, specials):
    mine = []
    parse_dir(v, b"".join(v.cluster(c) for c in v.chain(first, nofat, size)), dirpath, mine, deleted, True, specials)
    for m in mine:
        live.append(m)
        if m["dir"]:
            walk_live(v, m["path"], m["first"], m["nofat"], m["size"], live, deleted, specials)


def dump_report(img):
    res = subprocess.run(["dump.exfat", img], capture_output=True, text=True, check=True)
    return res.stdout


def dump_num(report, pattern):
    m = re.search(pattern, report, re.M)
    if not m:
        fail("dump.exfat report lacks " + pattern)
    return int(m.group(1))


def analyse_live(img, v, src):
    # the root directory is a FAT chain; its length is found by following the FAT to the end
    root_chain, c = [], v.root
    while True:
        if not v.valid(c) or c in root_chain:
            fail("bad cluster %d in the root chain" % c)
        root_chain.append(c)
        c = v.fat(c)
        if c >= 0xFFFFFFF8:
            break
    live, deleted, specials = [], [], {}
    walk_live(v, "/", v.root, False, len(root_chain) * v.cs, live, deleted, specials)
    if 0x81 not in specials or 0x82 not in specials:
        fail("the root has no allocation bitmap or up-case entry")
    used = {}
    for c in root_chain:
        used[c] = "/"
    for key, what in ((0x81, "bitmap"), (0x82, "upcase")):
        first, length = specials[key]
        for c in range(first, first + v.need(length)):
            used[c] = what
    files, chains = [], {}
    for m in live:
        own = v.chain(m["first"], m["nofat"], m["size"])
        if m["vdl"] != m["size"]:
            fail("%s: valid data length %d differs from data length %d (not reachable with the guest tools, P5)" % (m["path"], m["vdl"], m["size"]))
        for c in own:
            if c in used:
                fail("cluster %d is in two chains (%s and %s)" % (c, used[c], m["path"]))
            used[c] = m["path"]
        chains[m["path"]] = {"runs": runs(own), "nofat": m["nofat"]}
        if not m["dir"]:
            files.append(dict(m, clusters=own))
    free = [c for c in range(2, v.nclusters + 2) if c not in used]
    bfirst, blen = specials[0x81]
    bm = v.bitmap(bfirst, blen)
    zero = [c for c in range(2, v.nclusters + 2) if not (bm[(c - 2) // 8] >> ((c - 2) % 8)) & 1]
    if zero != free:
        fail("the allocation bitmap frees %d clusters, the live chains leave %d (%s vs %s)" % (len(zero), len(free), ranges(zero)[:6], ranges(free)[:6]))
    rep = dump_report(img)
    geo = (dump_num(rep, r"^Cluster size:\s+(\d+)"), dump_num(rep, r"^Total Clusters:\s+(\d+)"))
    if geo != (v.cs, v.nclusters):
        fail("own geometry %s differs from dump.exfat %s" % ((v.cs, v.nclusters), geo))
    dfree = dump_num(rep, r"^Free Clusters:\s+(\d+)")
    if dfree != len(free):
        fail("dump.exfat reports %d free clusters, the bitmap and the live chains give %d" % (dfree, len(free)))
    # NoFatChain cross-check: a contiguous (NoFatChain) file's FAT entries were never written
    for m in files:
        links = [v.fat(c) for c in m["clusters"]]
        if m["nofat"] and any(links):
            fail("%s: NoFatChain but its FAT entries are written" % m["path"])
        if not m["nofat"] and m["clusters"] and not all(links):
            fail("%s: not NoFatChain but a FAT entry is zero" % m["path"])
    return live, deleted, files, chains, used, free, specials


def check_files(v, src, files):
    out = []
    for f in files:
        data = v.data(f["clusters"], f["size"])
        p = src.path(f["path"])
        if os.path.exists(p):
            if open(p, "rb").read() != data:
                fail("%s: live bytes differ from the source tree" % f["path"])
        elif f["path"] == "/fill.bin":
            if data.strip(b"\x00"):
                fail("/fill.bin is not all zero")
        else:
            fail("live file %s has no source" % f["path"])
        out.append({"path": f["path"], "size": f["size"], "sha256": hashlib.sha256(data).hexdigest(), "chain": runs(f["clusters"])})
    return sorted(out, key=lambda x: x["path"])


def cmd_snap(img, srcroot, out, expect_free):
    v, src = Vol(img), Src(srcroot)
    _, _, files, chains, _, free, _ = analyse_live(img, v, src)
    check_files(v, src, files)
    if expect_free != "any" and len(free) != int(expect_free):
        fail("%d free clusters, want %s (the volume must be full where the plan fills it)" % (len(free), expect_free))
    json.dump({"chains": chains, "free": len(free)}, open(out, "w"))


PRIMARY = {
    "/contiguous/target.bin": "contiguous",
    "/zero-size/target.dat": "zero-size",
    "/reuse-head/target.bin": "reuse-head",
    "/cut-h/target.bin": "cut-h",
    "/chained/f.bin": "chained",
    "/gone": "gone",
}


def family_of(path):
    if path in PRIMARY:
        return PRIMARY[path]
    top = path.split("/")[1]
    for suffix in ("-w", "-hid"):
        if top.endswith(suffix):
            top = top[: -len(suffix)]
    return top + "-aux"


def cmd_final(img, srcroot, snaps):
    v, src = Vol(img), Src(srcroot)
    live, deleted, files, chains, used, free, specials = analyse_live(img, v, src)
    live_files = check_files(v, src, files)
    true, flags = {}, {}
    for s in snaps:  # later snapshots win
        for p, c in json.load(open(s))["chains"].items():
            true[p], flags[p] = c["runs"], c["nofat"]
    freeset = set(free)
    out = []
    for d in sorted(deleted, key=lambda r: (not r["listed"], r["path"])):
        size = d["size"]
        if d["vdl"] != size:
            fail("%s: valid data length differs from data length" % d["path"])
        tc = [] if size == 0 and not d["dir"] else true.get(d["path"])
        if tc is None:
            fail("no snapshot saw %s live: its true chain is unknown" % d["path"])
        if tc and tc[0][0] != d["first"]:
            fail("%s: the entry's first cluster %d is not the snapshot's %s" % (d["path"], d["first"], tc))
        if d["path"] in flags and flags[d["path"]] != d["nofat"]:
            fail("%s: NoFatChain changed between the snapshot and the deleted entry" % d["path"])
        tcl = [c for lo, hi in tc for c in range(lo, hi + 1)]
        # fat_residue: what the FINAL FAT still holds of the file's true chain
        if d["nofat"] or not tcl:
            residue = "not-chained"
        else:
            kept = all(v.fat(tcl[i]) == tcl[i + 1] for i in range(len(tcl) - 1)) and v.fat(tcl[-1]) >= 0xFFFFFFF8
            residue = "chain-kept" if kept else "chain-cleared"
        rec = {"path": d["path"], "family": family_of(d["path"]), "listed": d["listed"], "size": size,
               "sha256": "" if d["dir"] else src.sha(d["path"]), "true_chain": tc, "first_cluster": d["first"],
               "no_fat_chain": d["nofat"], "data_length": size, "valid_data_length": d["vdl"], "fat_residue": residue}
        e = {"outcome": "skip", "skip": "", "method": "", "range": [], "captured": [], "cut": False,
             "equal_source": False, "overlaps": [], "confidence": 0, "assumptions": []}
        cap_clusters = []
        if d["dir"]:
            e["skip"] = "not-a-file"
        elif size == 0:
            e["skip"] = "empty"
        else:
            n = v.need(size)
            if d["nofat"]:
                method, base, assumptions = "exfat-nofatchain", 75, ["no-fat-chain"]
                cl = v.chain(d["first"], True, size)
            elif residue == "chain-kept":
                method, base, assumptions = "exfat-fatchain", 65, ["fat-chain-kept"]
                cl = tcl[:n]
            else:
                method, base, assumptions = "exfat-contiguous", 55, ["contiguous-allocation"]
                cl = v.chain(d["first"], True, size)
            k = 0
            while k < len(cl) and cl[k] in freeset:
                k += 1
            e["range"] = runs(cl)
            e["captured"] = runs(cl[:k])
            e["cut"] = 0 < k < len(cl)
            cap_clusters = cl[:k]
            if k == 0:
                e["skip"] = "no-free-bytes"
            else:
                e["outcome"], e["method"], e["assumptions"] = "recover", method, assumptions
                got = hashlib.sha256(v.data(cap_clusters, size)).hexdigest()
                e["equal_source"] = (not e["cut"]) and got == rec["sha256"]
                e["base"] = base
        rec["expect"] = e
        rec["_cap"] = set(cap_clusters)
        out.append(rec)
    for a in out:
        if not a["listed"] or a["expect"]["outcome"] != "recover":
            continue
        for b in out:
            if b is not a and b["listed"] and b["expect"]["outcome"] == "recover" and a["_cap"] & b["_cap"]:
                a["expect"]["overlaps"].append(b["path"])
        a["expect"]["overlaps"].sort()
    fam = {}

    def bump(k):
        fam[k] = fam.get(k, 0) + 1

    for a in out:
        e = a["expect"]
        if e["outcome"] == "recover":
            e["confidence"] = min(e.pop("base"), 20 if e["cut"] else 99, 30 if e["overlaps"] else 99)
        del a["_cap"]
        if e["outcome"] == "skip":
            bump("empty" if e["skip"] == "empty" else "skip-" + e["skip"])
        else:
            if e["cut"]:
                bump("cut-prefix")
            elif e["equal_source"]:
                bump({"exfat-nofatchain": "nofatchain-equal", "exfat-fatchain": "fatchain-equal", "exfat-contiguous": "contiguous-equal"}[e["method"]])
            else:
                bump("not-equal-complete")
                if e["method"] == "exfat-contiguous":
                    bump("contiguous-not-equal")
            if a["family"] == "chained":
                bump("chained")
        if e["overlaps"]:
            bump("overlap")
    seen = {a["family"] for a in out}
    for name in set(PRIMARY.values()):
        if name not in seen:
            fail("scenario family %s is missing from %s" % (name, img))
    by = {a["path"]: a for a in out}
    t = by["/contiguous/target.bin"]
    if not t["no_fat_chain"] or len(t["true_chain"]) != 1:
        fail("/contiguous/target.bin is not a single NoFatChain run: %s" % t["true_chain"])
    f = by["/chained/f.bin"]
    if len(f["true_chain"]) != 2 or f["no_fat_chain"]:
        fail("/chained/f.bin is not a fragmented FAT-chained file: %s, no_fat_chain %s" % (f["true_chain"], f["no_fat_chain"]))
    if f["fat_residue"] != "chain-kept":
        fail("/chained/f.bin: the FAT chain of a deleted chained file is %s, but probe fact P3 says the kernel keeps it: the F1 gate fails, re-rule F1" % f["fat_residue"])
    geo = {"type": "exfat", "cluster_size": v.cs, "data_start": v.heap_off, "cluster_count": v.nclusters}
    json.dump({**geo, "live": live_files, "free_clusters": ranges(free),
               "deleted_entry_count": sum(1 for d in deleted if d["listed"]), "deleted": out, "families": fam},
              sys.stdout, indent=1)
    print()


REQUIRED = ["nofatchain-equal", "empty", "chained", "skip-no-free-bytes", "cut-prefix", "not-equal-complete"]


def cmd_families(paths):
    total = {}
    for p in paths:
        for k, n in json.load(open(p))["families"].items():
            total[k] = total.get(k, 0) + n
    for name in REQUIRED:
        if total.get(name, 0) < 1:
            fail("family %s missing: the allocation-order row used (ascending) did not produce it; re-rule, do not edit the ops" % name)
    print("families: %s" % json.dumps(total, sort_keys=True), file=sys.stderr)


def main():
    a = sys.argv[1:]
    if len(a) == 5 and a[0] == "snap":
        cmd_snap(*a[1:])
    elif len(a) >= 4 and a[0] == "final":
        cmd_final(a[1], a[2], a[3:])
    elif len(a) >= 2 and a[0] == "families":
        cmd_families(a[1:])
    else:
        sys.exit(__doc__)


main()

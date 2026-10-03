#!/usr/bin/env python3
"""Cluster-chain and free-space oracle for the exFAT fixture.

usage: exfat_chains.py <image> <files.json>

Every live path of files.json (the source-tree oracle) and the root directory
are asked to `dump.exfat -c -d <path>` (exfatprogs: it walks the directory
entry set and the cluster chain itself, never Minutiae). The allocation bitmap
and the up-case table are system files whose clusters the boot report names.
The clusters none of them holds are the free clusters, and their number must
equal the "Free Clusters" statistic dump.exfat prints, else this script fails.
Geometry and identity come from the dump.exfat boot report.

Prints JSON: geometry, label, serial, "chains" {path: [[first, last], ...]}
(inclusive cluster numbers) and "free_clusters" (sorted ranges).
"""
import json
import re
import subprocess
import sys


def ranges(clusters):
    out = []
    for c in sorted(clusters):
        if out and c == out[-1][1] + 1:
            out[-1][1] = c
        else:
            out.append([c, c])
    return out


def dump(image, *args):
    res = subprocess.run(["dump.exfat", *args, image], capture_output=True, text=True, check=True)
    return res.stdout


def chain(image, path):
    out = dump(image, "-c", "-d", path)
    lines = out.splitlines()
    result = []
    i = 0
    while i < len(lines) and "Cluster chain:" not in lines[i]:
        i += 1
    while i < len(lines):
        m = re.search(r"(?:Cluster chain:)?\s+(\d+):(\d+)\s*$", lines[i])
        if not m:
            break
        first, count = int(m.group(1)), int(m.group(2))
        result.append([first, first + count - 1])
        i += 1
    return result


def main() -> None:
    image, files_json = sys.argv[1:3]
    report = dump(image)

    def num(pattern, base=10):
        m = re.search(pattern, report, re.M)
        if not m:
            sys.exit("dump.exfat report lacks " + pattern)
        return int(m.group(1), base)

    sector_size = num(r"^Bytes per Sector:\s+(\d+)")
    cluster_size = num(r"^Cluster size:\s+(\d+)")
    count = num(r"^Total Clusters:\s+(\d+)")
    free_stat = num(r"^Free Clusters:\s+(\d+)")
    geo = {
        "sector_size": sector_size,
        "cluster_size": cluster_size,
        "data_start": num(r"^Cluster Heap Offset \(sector offset\):\s+(\d+)") * sector_size,
        "cluster_count": count,
        "size": num(r"^Volume Length\(sectors\):\s+(\d+)") * sector_size,
        "serial": "%08X" % num(r"^Volume Serial:\s+0x([0-9a-fA-F]+)", 16),
    }
    label = re.search(r"^Volume label:\s*(.*)$", report, re.M)
    geo["label"] = label.group(1).strip() if label else ""

    def system_chain(start_pat, size_pat):
        first = num(start_pat)
        n = -(-num(size_pat) // cluster_size)  # contiguous (NoFatChain) by construction of mkfs.exfat
        return [first, first + n - 1]

    used = set()

    def add(lo, hi, what):
        for c in range(lo, hi + 1):
            if c in used:
                sys.exit("cluster %d is in two chains (at %s)" % (c, what))
            used.add(c)

    add(*system_chain(r"^Bitmap start cluster:\s+(\d+)", r"^Bitmap size:\s+(\d+)"), "bitmap")
    add(*system_chain(r"^Upcase table start cluster:\s+(\d+)", r"^Upcase table size:\s+(\d+)"), "upcase")
    for lo, hi in chain(image, "/"):
        add(lo, hi, "/")
    files = json.load(open(files_json, encoding="utf-8"))["files"]
    chains = {}
    for f in files:
        chains[f["path"]] = chain(image, f["path"])
        for lo, hi in chains[f["path"]]:
            add(lo, hi, f["path"])
        if f["type"] == "file" and f["size"] > 0 and not chains[f["path"]]:
            sys.exit("dump.exfat reports no cluster chain for " + f["path"])
    free = [c for c in range(2, count + 2) if c not in used]
    if len(free) != free_stat:
        sys.exit("chains leave %d clusters free, dump.exfat reports %d" % (len(free), free_stat))
    json.dump({**geo, "free_count": len(free), "free_clusters": ranges(free), "chains": chains}, sys.stdout)


main()

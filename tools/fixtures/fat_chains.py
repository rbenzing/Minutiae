#!/usr/bin/env python3
"""Cluster-chain and free-space oracle for the FAT fixtures.

usage: fat_chains.py <image> <files.json> <fsck.fat -v output>

Every live path of files.json (the source-tree oracle) is asked to
`mshowfat`, which follows the FAT chain itself (mtools, never Minutiae); the
FAT32 root directory is asked too. The clusters no live chain holds are the
free clusters. The geometry (cluster size, data-area offset, cluster count,
clusters in use) is read from the `fsck.fat -v` report, and the free count it
implies must equal the one derived from the chains, else this script fails.

Prints JSON: geometry, "chains" {path: [[first, last], ...]} (inclusive
cluster numbers, empty for a file without clusters) and "free_clusters" (the
sorted ranges of free clusters).
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


def chain(image, path):
    res = subprocess.run(["mshowfat", "-i", image, "::" + path], capture_output=True, text=True, check=True)
    line = res.stdout.strip().splitlines()[-1]
    # A chain in several pieces prints as "::/path <5-9> <20-30>".
    m = re.fullmatch(r"::(.*?)((?: <[0-9,\-]*>)+)", line)
    if not m:
        if line.endswith("Root directory or empty file"):
            return []
        sys.exit("cannot parse mshowfat output %r" % line)
    out = []
    for part in filter(None, re.split(r"[ <>,]+", m.group(2))):
        lo, _, hi = part.partition("-")
        out.append([int(lo), int(hi or lo)])
    return out


def main() -> None:
    image, files_json, fsck = sys.argv[1:4]
    report = open(fsck, encoding="utf-8").read()

    def num(pattern):
        m = re.search(pattern, report, re.M)
        if not m:
            sys.exit("fsck.fat -v report lacks " + pattern)
        return int(m.group(1))

    geo = {
        "sector_size": num(r"^\s*(\d+) bytes per logical sector"),
        "cluster_size": num(r"^\s*(\d+) bytes per cluster"),
        "data_start": num(r"^Data area starts at byte (\d+)"),
        "cluster_count": num(r"^\s*(\d+) data clusters"),
        "size": num(r"^\s*(\d+) sectors total") * num(r"^\s*(\d+) bytes per logical sector"),
    }
    used_by_fsck = num(r"^\S+: \d+ files, (\d+)/\d+ clusters")

    files = json.load(open(files_json, encoding="utf-8"))["files"]
    chains = {f["path"]: chain(image, f["path"]) for f in files}
    used = set()
    for path in ["/"] + list(chains):
        for lo, hi in (chains[path] if path in chains else chain(image, path)):
            for c in range(lo, hi + 1):
                if c in used:
                    sys.exit("cluster %d is in two chains (at %s)" % (c, path))
                used.add(c)
    free = [c for c in range(2, geo["cluster_count"] + 2) if c not in used]
    if len(used) != used_by_fsck:
        sys.exit("mshowfat chains hold %d clusters, fsck.fat -v reports %d in use" % (len(used), used_by_fsck))
    json.dump({**geo, "free_count": len(free), "free_clusters": ranges(free), "chains": chains}, sys.stdout)


main()

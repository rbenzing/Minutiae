#!/usr/bin/env python3
"""Free-space oracle for the ext4 fixtures: reads the full `dumpe2fs <image>`
report on stdin and prints the free block ranges as JSON, [[first, last], ...]
(inclusive block numbers, sorted, adjacent ranges across groups merged).

dumpe2fs derives them from the filesystem itself (the block bitmaps, and for
BLOCK_UNINIT groups the layout), through libext2fs, never through Minutiae.
"""
import json
import re
import sys


def main() -> None:
    ranges = []
    seen = False
    for line in sys.stdin:
        m = re.match(r"\s+Free blocks: *(.*?)\s*$", line)
        if not m:
            continue
        seen = True
        for part in filter(None, (p.strip() for p in m.group(1).split(","))):
            lo, _, hi = part.partition("-")
            ranges.append([int(lo), int(hi or lo)])
    if not seen:
        sys.exit("no 'Free blocks:' line in the dumpe2fs report")
    ranges.sort()
    merged = []
    for lo, hi in ranges:
        if merged and lo <= merged[-1][1] + 1:
            merged[-1][1] = max(merged[-1][1], hi)
        else:
            merged.append([lo, hi])
    json.dump(merged, sys.stdout)


main()

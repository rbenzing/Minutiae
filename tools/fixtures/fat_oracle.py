#!/usr/bin/env python3
"""Independent oracle for the FAT and exFAT fixtures: walks the SOURCE TREE the
image was populated from (never the image, never Minutiae) and prints the
expected entries as JSON.

usage: fat_oracle.py <srcdir> [--exclude /path ...] [--exclude-tree /dir ...]

--exclude names a file the generator later deletes; --exclude-tree a directory
it deletes with its whole content. Both are listed by the generator itself
under "deleted". Entries are sorted by path (byte order). The root directory
is not listed.
"""
import hashlib
import json
import os
import stat
import sys


def main() -> None:
    args = sys.argv[1:]
    src = args.pop(0)
    exclude, exclude_tree = set(), []
    while args:
        flag = args.pop(0)
        if flag == "--exclude":
            exclude.add(args.pop(0))
        elif flag == "--exclude-tree":
            exclude_tree.append(args.pop(0))
        else:
            sys.exit("unknown argument " + flag)

    files = []
    for root, dirs, names in os.walk(src):
        dirs.sort()
        for name in sorted(dirs + names):
            full = os.path.join(root, name)
            rel = "/" + os.path.relpath(full, src).replace(os.sep, "/")
            if rel in exclude or any(rel == t or rel.startswith(t + "/") for t in exclude_tree):
                continue
            st = os.lstat(full)
            e = {"path": rel, "mtime": int(st.st_mtime)}
            if stat.S_ISDIR(st.st_mode):
                e["type"], e["size"], e["sha256"] = "dir", 0, ""
            elif stat.S_ISREG(st.st_mode):
                h = hashlib.sha256()
                with open(full, "rb") as f:
                    for chunk in iter(lambda: f.read(1 << 20), b""):
                        h.update(chunk)
                e["type"], e["size"], e["sha256"] = "file", st.st_size, h.hexdigest()
            else:
                sys.exit("unsupported file type in source tree: " + rel)
            files.append(e)
    files.sort(key=lambda e: os.fsencode(e["path"]))
    json.dump({"files": files}, sys.stdout, ensure_ascii=False)


main()

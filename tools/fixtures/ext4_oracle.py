#!/usr/bin/env python3
"""Independent oracle for the ext4 fixtures: walks the SOURCE TREE that was
handed to mke2fs -d (never the image, never Minutiae) and prints the expected
entries as JSON.

usage: ext4_oracle.py <srcdir> [--exclude /path ...]

Paths named by --exclude (the files the generator later removes with debugfs)
are left out of "files"; they are listed by the generator itself under
"deleted". Entries are sorted by path (byte order).
"""
import hashlib
import json
import os
import stat
import sys


def main() -> None:
    args = sys.argv[1:]
    src = args.pop(0)
    exclude = set()
    while args:
        if args.pop(0) != "--exclude":
            sys.exit("unknown argument")
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
                sys.exit("unsupported file type in source tree: " + rel)
            files.append(e)
    files.sort(key=lambda e: os.fsencode(e["path"]))
    json.dump({"files": files}, sys.stdout, ensure_ascii=False)


main()

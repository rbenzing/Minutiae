#!/usr/bin/env python3
"""Probe list for the kernel casefold lookup oracle (standard library only).

The probes are derived from the committed Unicode 12.1.0 files
(internal/filesys/utf8fold/testdata/ucd-12.1.0) parsed HERE; nothing is shared
with the Go generator of the utf8fold package, so a bug in either shows as a
disagreement with the kernel's own verdicts.

  casefold_probes.py --list                 JSON array of the probes (no verdicts)
  casefold_probes.py --payload DIR          write the guest payload tree
  casefold_probes.py --vectors OUT --verdicts FILE --meta JSON
                                            merge the guest verdicts into kernel-vectors.json
  casefold_probes.py --expect ext4|f2fs --image IMG --out FILE --meta JSON
                                            list the finished image into <fs>-casefold.expect.json

Payload layout (every name is a FILE NAME, so nothing is escaped or decoded in
the guest): DIR/p/<bucket>/<id>/q/<query bytes> and DIR/p/<bucket>/<id>/s/<stored
bytes>, and DIR/s/<bucket>/<stored bytes> for the stored names of a bucket.

A name that contains a newline, a slash or a NUL cannot be a file name here and
is never generated (see README, "Casefold lookup oracle").
"""
import argparse
import gzip
import json
import os
import re
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
UCD = os.path.join(HERE, "..", "..", "internal", "filesys", "utf8fold", "testdata", "ucd-12.1.0")

# Hangul syllable arithmetic (Unicode ch. 3.12)
SBASE, LBASE, VBASE, TBASE = 0xAC00, 0x1100, 0x1161, 0x11A7
LCOUNT, VCOUNT, TCOUNT = 19, 21, 28
NCOUNT = VCOUNT * TCOUNT
SCOUNT = LCOUNT * NCOUNT

# Code points assigned AFTER Unicode 12.1. The script asserts none is assigned
# in the 12.1 UnicodeData file, so a mistake here cannot silently mislabel a row.
POST_PAIRS = [
    ("13.0", 0xA7C7, 0xA7C8),  # LATIN CAPITAL/SMALL LETTER D WITH SHORT STROKE OVERLAY
    ("13.0", 0xA7C9, 0xA7CA),  # LATIN CAPITAL/SMALL LETTER S WITH SHORT STROKE OVERLAY
    ("13.0", 0xA7F5, 0xA7F6),  # LATIN CAPITAL/SMALL LETTER REVERSED HALF H
    ("14.0", 0xA7C0, 0xA7C1),  # LATIN CAPITAL/SMALL LETTER OLD POLISH O
]
POST_SINGLES = [
    ("14.0", 0x061D),  # ARABIC END OF TEXT MARK
    ("15.0", 0x0CF3),  # KANNADA SIGN COMBINING ANUSVARA ABOVE RIGHT
]


def read_lines(name):
    with gzip.open(os.path.join(UCD, name + ".gz"), "rt", encoding="utf-8") as f:
        return f.read().split("\n")


class Ucd:
    def __init__(self):
        self.assigned = set()
        self.ccc = {}
        self.decomp = {}  # canonical, one level
        self.surrogate = set()
        first = None
        for line in read_lines("UnicodeData.txt"):
            if not line:
                continue
            f = line.split(";")
            cp = int(f[0], 16)
            if f[1].endswith(", First>"):
                first = cp
                continue
            lo = cp
            if f[1].endswith(", Last>"):
                lo = first
            for c in range(lo, cp + 1):
                self.assigned.add(c)
            if f[2] == "Cs":
                for c in range(lo, cp + 1):
                    self.surrogate.add(c)
            if f[3] != "0":
                self.ccc[cp] = int(f[3])
            if f[5] and not f[5].startswith("<"):
                self.decomp[cp] = [int(x, 16) for x in f[5].split()]
        self.fold = {}  # cp -> list of cps, status C and F
        for line in read_lines("CaseFolding.txt"):
            line = line.split("#")[0].strip()
            if not line:
                continue
            f = [x.strip() for x in line.split(";")]
            if f[1] in ("C", "F"):
                self.fold[int(f[0], 16)] = [int(x, 16) for x in f[2].split()]
        self.ignorable = set()
        for line in read_lines("DerivedCoreProperties.txt"):
            line = line.split("#")[0].strip()
            if not line:
                continue
            rng, prop = [x.strip() for x in line.split(";")]
            if prop != "Default_Ignorable_Code_Point":
                continue
            if ".." in rng:
                a, b = rng.split("..")
                self.ignorable.update(range(int(a, 16), int(b, 16) + 1))
            else:
                self.ignorable.add(int(rng, 16))

    def nfd_cp(self, cp, out):
        if SBASE <= cp < SBASE + SCOUNT:
            s = cp - SBASE
            out.append(LBASE + s // NCOUNT)
            out.append(VBASE + (s % NCOUNT) // TCOUNT)
            if s % TCOUNT:
                out.append(TBASE + s % TCOUNT)
            return
        d = self.decomp.get(cp)
        if d is None:
            out.append(cp)
            return
        for c in d:
            self.nfd_cp(c, out)

    def nfd(self, cps):
        out = []
        for c in cps:
            self.nfd_cp(c, out)
        # canonical ordering: stable sort of every run of non-starters by ccc
        i = 0
        while i < len(out):
            if self.ccc.get(out[i], 0) == 0:
                i += 1
                continue
            j = i
            while j < len(out) and self.ccc.get(out[j], 0) != 0:
                j += 1
            out[i:j] = sorted(out[i:j], key=lambda c: self.ccc[c])
            i = j
        return out

    def key(self, name):
        """The script's own fold, used ONLY to keep a bucket's stored names pairwise
        distinct. Not a claim about the kernel: the kernel's verdicts are the data."""
        try:
            s = name.decode("utf-8")
        except UnicodeDecodeError:
            return ("raw", name)
        cps = [ord(c) for c in s]
        cps = self.nfd(cps)
        f = []
        for c in cps:
            f.extend(self.fold.get(c, [c]))
        f = self.nfd(f)
        return tuple(c for c in f if c not in self.ignorable)


def enc(cps):
    return "".join(chr(c) for c in cps).encode("utf-8", "surrogatepass")


def usable(name):
    return 0 < len(name) <= 255 and b"/" not in name and b"\x00" not in name and b"\n" not in name


class Probes:
    def __init__(self, u):
        self.u = u
        self.rows = []  # (category, stored bytes, query bytes)

    def add(self, cat, stored, query):
        if usable(stored) and usable(query):
            self.rows.append((cat, stored, query))

    def add_neg(self, cat, stored, query):
        # a stored name with an extra trailing letter must not resolve
        self.add(cat, stored + b"z", query)


def build(u):
    p = Probes(u)
    # (1) every code point with a C or F case folding
    cf = sorted(u.fold)
    for i, cp in enumerate(cf):
        if cp in u.surrogate:
            continue
        p.add("casefold", enc(u.fold[cp]), enc([cp]))
        if i % 10 == 0:
            p.add_neg("casefold", enc(u.fold[cp]), enc([cp]))
    # (2) every code point with a canonical decomposition
    dc = sorted(u.decomp)
    for i, cp in enumerate(dc):
        full = []
        u.nfd_cp(cp, full)
        p.add("decomposition", enc(full), enc([cp]))
        if i % 10 == 0:
            p.add_neg("decomposition", enc(full), enc([cp]))
    # (3) every Default_Ignorable_Code_Point
    ig = sorted(u.ignorable)
    for i, cp in enumerate(ig):
        if cp in u.surrogate:
            continue
        p.add("ignorable", b"ab", b"a" + enc([cp]) + b"b")
        if i % 10 == 0:
            p.add_neg("ignorable", b"ab", b"a" + enc([cp]) + b"b")
    # (4) 2000 assigned code points with no mapping, across all planes
    plain = [
        c for c in sorted(u.assigned)
        if c not in u.fold and c not in u.decomp and c not in u.ignorable
        and c not in u.surrogate and not (SBASE <= c < SBASE + SCOUNT)
        and u.ccc.get(c, 0) == 0 and c not in (0, 0x0A, 0x2F)
    ]
    step = len(plain) / 2000.0
    for k in range(2000):
        c = plain[int(k * step)]
        x = enc([c])
        p.add("unmapped", b"x" + x, b"x" + x)  # identity, expected 1
        p.add("unmapped", b"x" + x, b"X" + x)  # case control, expected 1
        p.add("unmapped", b"x" + x, b"y" + x)  # expected 0
    # (5) combining-mark reordering, Hangul, and the special letters
    a = ord("a")
    pairs = [
        ([a, 0x323, 0x301], [a, 0x301, 0x323]),
        ([a, 0x323, 0x301], [a, 0x323, 0x301]),
        ([a, 0x301, 0x323], [a, 0x323, 0x301]),
    ]
    for n in (2, 15, 29, 30, 31, 32, 33, 34, 40):
        s_run = [a] + [0x301, 0x323] * (n // 2) + ([0x301] if n % 2 else [])
        q_run = [a] + sorted(s_run[1:], key=lambda c: u.ccc[c])
        pairs.append((s_run, q_run))
        pairs.append((q_run, s_run))
    pairs += [
        ([0xAC00], [0x1100, 0x1161]),  # LV syllable versus jamo
        ([0x1100, 0x1161], [0xAC00]),
        ([0xAC01], [0x1100, 0x1161, 0x11A8]),  # LVT
        ([0x1100, 0x1161, 0x11A8], [0xAC01]),
        ([0xAC00, 0x11A8], [0xAC01]),  # LV followed by T composes
        ([0xAC01], [0xAC00, 0x11A8]),
        ([0x1100, 0x1161, 0x11A8], [0xAC00, 0x11A8]),
    ]
    for s, q in pairs:
        p.add("reorder", enc(s), enc(q))
        p.add_neg("reorder", enc(s), enc(q))
    specials = [
        (0x130,), (0x131,), (0x1E9E,), (0xDF,), (0x3C2,), (0x3C3,), (0x3A3,),
        (0x212A,), (0x2126,), (0xFB03,), (0x49, 0x307), (0x69, 0x307), (0x4B,), (0x6B,),
        (0x53, 0x53), (0x73, 0x73), (0x1E9E,), (0xC9,), (0xE9,), (0x45, 0x301), (0x65, 0x301),
        (0x3A9,), (0x3C9,), (0x1C5,), (0x1C4,), (0x1C6,), (0x17F,), (0x73,), (0x53,),
    ]
    seen = set()
    for x in specials:
        for y in specials:
            if (x, y) in seen:
                continue
            seen.add((x, y))
            p.add("special", enc([ord("p")] + list(x)), enc([ord("p")] + list(y)))
    for x in specials:
        p.add_neg("special", enc([ord("p")] + list(x)), enc([ord("p")] + list(x)))
    # (6) code points assigned after Unicode 12.1
    for ver, up, lo in POST_PAIRS:
        for c in (up, lo):
            assert c not in u.assigned, "U+%04X is assigned in 12.1.0" % c
        pu, pl = enc([up]), enc([lo])
        p.add("post-12.1", b"x" + pl, b"x" + pu)  # upper/lower pair
        p.add("post-12.1", b"x" + pu, b"x" + pl)
        p.add("post-12.1", b"x" + pl, b"x" + pl)  # identity
        p.add("post-12.1", b"x" + pl, b"X" + pl)  # ASCII case beside it
        p.add_neg("post-12.1", b"x" + pl, b"x" + pu)
        # NFD-style: marks after an unassigned base are reordered?
        p.add("post-12.1", enc([ord("x"), up, 0x323, 0x301]), enc([ord("x"), up, 0x301, 0x323]))
        p.add("post-12.1", enc([ord("x"), lo, 0x323, 0x301]), enc([ord("x"), lo, 0x301, 0x323]))
        p.add("post-12.1", enc([up, 0xE9]), enc([up, 0x65, 0x301]))
        p.add("post-12.1", enc([up, 0x65, 0x301]), enc([up, 0xE9]))
    for ver, c in POST_SINGLES:
        assert c not in u.assigned, "U+%04X is assigned in 12.1.0" % c
        x = enc([c])
        p.add("post-12.1", b"x" + x, b"x" + x)
        p.add("post-12.1", b"x" + x, b"X" + x)
        p.add("post-12.1", b"a" + x + b"b", b"A" + x + b"B")
        p.add_neg("post-12.1", b"x" + x, b"x" + x)
    # (7) invalid input
    bad = [
        b"\x80",  # lone continuation
        b"\xc1\x81",  # overlong two-byte 'A'
        b"\xc0\xaf",  # overlong '/'
        b"\xe2\x82",  # truncated
        b"\xed\xa0\x80",  # UTF-16 surrogate in UTF-8
        b"\xef\xbf\xbe",  # U+FFFE
        b"\xef\xbf\xbf",  # U+FFFF
        b"\xf4\x8f\xbf\xbf",  # U+10FFFF
        b"\xf4\x90\x80\x80",  # beyond U+10FFFF
        b"\xf8\x88\x80\x80\x80",  # five-byte form
        b"\xff",
        b"\xfe\xfe",
    ]
    for b in bad:
        p.add("invalid", b"x" + b, b"x" + b)  # identity
        p.add("invalid", b"x" + b, b"X" + b)  # ASCII case beside an invalid sequence
        p.add("invalid", b"\xc3\x89" + b, b"e\xcc\x81" + b)  # fold-equal valid part beside it
        p.add("invalid", b"e\xcc\x81" + b, b"\xc3\xa9" + b)
        p.add_neg("invalid", b"x" + b, b"x" + b)
    p.add("invalid", b"xA", b"x\xc1\x81")  # does an overlong form resolve to its character?
    # an invalid byte AFTER valid text: does the kernel stop at it (and so equate the name with the text before it)?
    for pre in (b"x", b"e\xcc\x81", b"\xc3\xa9", b"\xc3\x89"):
        for b in (b"\x80", b"\xff", b"\xc1\x81"):
            p.add("invalid", pre + b, pre)
            p.add("invalid", pre, pre + b)
    p.add("invalid", b"x\xef\xbf\xbd", b"x\xff")  # U+FFFD versus a bad byte
    p.add("invalid", b"x\xc3\x89", b"x\xc3")  # a name that is a prefix of a valid one
    seen_rows = set()
    out = []
    for r in p.rows:
        if r not in seen_rows:
            seen_rows.add(r)
            out.append(r)
    return out


def assign_buckets(u, rows):
    """Greedy first fit: a bucket never holds two stored names with the same script
    fold unless the stored bytes are identical (one directory entry).

    A stored name that is not valid UTF-8 gets a bucket of its own. The first run
    measured why: the kernel resolved e + U+0301 + 0x80 to an entry that was
    already there, so the second spelling was never created (the cp found the
    first one). How the kernel compares a name with an invalid byte is what the
    vectors measure, so no bucket may rely on a guess about it."""
    buckets = []  # list of dict key -> stored bytes
    isolated = {}  # invalid stored name -> its own bucket
    out = []
    for cat, stored, query in rows:
        try:
            stored.decode("utf-8")
        except UnicodeDecodeError:
            if stored not in isolated:
                buckets.append({("raw", stored): stored})
                isolated[stored] = len(buckets) - 1
            out.append((cat, "%02d" % isolated[stored], stored, query))
            continue
        k = u.key(stored)
        for bi, b in enumerate(buckets):
            if bi in isolated.values():
                continue
            have = b.get(k)
            if have is None:
                b[k] = stored
                break
            if have == stored:
                break
        else:
            buckets.append({k: stored})
            bi = len(buckets) - 1
        out.append((cat, "%02d" % bi, stored, query))
    return out


def probes():
    u = Ucd()
    rows = assign_buckets(u, build(u))
    return [
        {"id": i + 1, "category": c, "bucket": b, "stored_hex": s.hex(), "query_hex": q.hex()}
        for i, (c, b, s, q) in enumerate(rows)
    ]


def write_payload(root):
    plist = probes()
    for r in plist:
        b, i = r["bucket"], r["id"]
        s, q = bytes.fromhex(r["stored_hex"]), bytes.fromhex(r["query_hex"])
        base = os.path.join(os.fsencode(root), b"p", b.encode(), str(i).encode())
        for kind, name in ((b"q", q), (b"s", s)):
            d = os.path.join(base, kind)
            os.makedirs(d, exist_ok=True)
            open(os.path.join(d, name), "wb").close()
        sd = os.path.join(os.fsencode(root), b"s", b.encode())
        os.makedirs(sd, exist_ok=True)
        open(os.path.join(sd, s), "wb").close()
    return plist


def read_verdicts(path):
    """Lines 'V <id> abcd' from the guest: a/b = ext4 stored present / query resolves,
    c/d = the same for F2FS."""
    v = {}
    with open(path, "rb") as f:
        for line in f.read().split(b"\n"):
            parts = line.strip().split(b" ")
            if len(parts) == 3 and parts[0] == b"V" and len(parts[2]) == 4:
                v[int(parts[1])] = parts[2].decode()
    return v


def merge(out, verdicts, meta):
    plist = probes()
    v = read_verdicts(verdicts)
    if len(v) != len(plist):
        sys.exit("guest answered %d probes of %d" % (len(v), len(plist)))
    bad = [i for i in v if v[i][0] != "1" or v[i][2] != "1"]
    if bad:
        sys.exit("stored name refused (S 0) for probes %s: aborting, no verdict is recorded" % bad[:20])
    with open(meta) as f:
        gen = json.load(f)
    vectors = []
    exceptions = []
    for r in plist:
        i = r["id"]
        row = dict(r)
        row["stored_present"] = True
        row["ext4"] = v[i][1] == "1"
        row["f2fs"] = v[i][3] == "1"
        if row["ext4"] != row["f2fs"]:
            exceptions.append(i)
        vectors.append(row)
    doc = {"generator": gen, "vectors": vectors}
    if exceptions:
        doc["exceptions"] = exceptions
    with open(out, "w") as f:
        json.dump(doc, f, separators=(",", ":"))
        f.write("\n")
    print("vectors: %d, ext4/f2fs disagreements: %d" % (len(vectors), len(exceptions)))


def run_tool(args):
    r = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if r.returncode != 0:
        sys.exit("%s failed: %s" % (args, r.stderr.decode("utf-8", "replace")))
    return r.stdout


def list_ext4(image, path):
    """Names in an ext4 directory, from debugfs 'ls -p' on the finished image."""
    names = []
    for line in run_tool(["debugfs", "-R", "ls -p " + path, image]).split(b"\n"):
        f = line.split(b"/")
        if len(f) < 8:
            continue
        name = f[5]
        if name in (b".", b".."):
            continue
        names.append(name)
    return names


def ext4_dir_casefold(image, path):
    out = run_tool(["debugfs", "-R", "stat " + path, image]).decode("utf-8", "replace")
    for line in out.split("\n"):
        if "Flags:" in line:
            flags = int(line.split("Flags:")[1].split()[0], 16)
            return bool(flags & 0x40000000)
    sys.exit("no flags for " + path)


F2FS_SOURCE = "fsck.f2fs -t (path tree) | dump.f2fs -i <ino> (i_flags)"
F2FS_CASEFOLD_FL = 0x40000000
_TREE_LINE = re.compile(rb"^((?:\|   |    )*)[|`]-- (.*) <ino = 0x([0-9a-f]+)>, <encrypted \((\d)\)>$")
_f2fs_tree = {}


def f2fs_tree(image):
    """{dir path: [(name, ino)]} and {dir path: ino} from 'fsck.f2fs -t' run on a COPY."""
    if image in _f2fs_tree:
        return _f2fs_tree[image]
    copy = image + ".ro-copy"
    shutil.copyfile(image, copy)
    try:
        out = run_tool(["fsck.f2fs", "-t", copy])
    finally:
        os.remove(copy)
    children, inos = {}, {"/": 3}
    stack = []
    for line in out.split(b"\n"):
        m = _TREE_LINE.match(line)
        if not m:
            continue
        depth, name, ino = len(m.group(1)) // 4, m.group(2), int(m.group(3), 16)
        del stack[depth:]
        parent = "/" + "/".join(stack)
        children.setdefault(parent, []).append((name, ino))
        stack.append(name.decode("latin-1"))
        inos["/" + "/".join(stack)] = ino
    _f2fs_tree[image] = (children, inos)
    return _f2fs_tree[image]


def list_f2fs(image, path):
    children, _ = f2fs_tree(image)
    key = "/" + "/".join(x for x in path.split("/") if x)
    return [n for n, _ in children.get(key, [])]


def f2fs_dir_casefold(image, path):
    _, inos = f2fs_tree(image)
    key = "/" + "/".join(x for x in path.split("/") if x)
    out = run_tool(["dump.f2fs", "-d", "1", "-i", "0x%x" % inos[key], image]).decode("utf-8", "replace")
    for line in out.split("\n"):
        if line.startswith("i_flags"):
            return bool(int(line.split("[0x")[1].split(":")[0].strip(), 16) & F2FS_CASEFOLD_FL)
    sys.exit("no i_flags for " + path)


def expect(fs, image, out, meta):
    plist = probes()
    stored = {}
    for r in plist:
        stored.setdefault(r["bucket"], set()).add(bytes.fromhex(r["stored_hex"]))
    with open(meta) as f:
        gen = json.load(f)
    gen = {"image_sha256": gen["image_sha256"][fs], "commands": gen["commands"]}
    dirs = []
    if fs == "ext4":
        source = "debugfs ls -p | stat flags"
        lister, caser = list_ext4, ext4_dir_casefold
    else:
        source = F2FS_SOURCE
        lister, caser = list_f2fs, f2fs_dir_casefold
    paths = ["/cf/" + b for b in sorted(stored)] + ["/plain"]
    for p in paths:
        names = lister(image, p)
        cf = caser(image, p)
        want = stored[p.split("/")[-1]] if p != "/plain" else stored["00"]
        have = set(names)
        missing = [n for n in want if n not in have]
        if missing:
            near = [n.hex() for n in names if n[:2] == missing[0][:2] and len(n) < 12][:12]
            sys.exit("%s: %s lacks %d stored spellings: %s; entries starting alike: %s" % (fs, p, len(missing), [m.hex() for m in missing[:10]], near))
        if cf != (p != "/plain"):
            sys.exit("%s: %s casefold flag is %s" % (fs, p, cf))
        dirs.append({"path": p, "casefold": cf, "names_hex": sorted(n.hex() for n in names), "source": source})
    pair = None
    if fs == "ext4":
        # the optional fold-equal pair, made on the host with debugfs after the guest unmounted
        names = lister(image, "/ambig")
        if names:
            if len(names) != 2 or not caser(image, "/ambig"):
                sys.exit("ext4: /ambig holds %d names, casefold=%s" % (len(names), caser(image, "/ambig")))
            dirs.append({"path": "/ambig", "casefold": True, "names_hex": sorted(n.hex() for n in names), "source": source})
            pair = {"path": "/ambig", "names_hex": sorted(n.hex() for n in names)}
    doc = {"generator": gen, "directories": dirs, "ambiguous_pair": pair}
    with open(out, "w") as f:
        json.dump(doc, f, separators=(",", ":"))
        f.write("\n")
    print("%s: %d directories listed" % (fs, len(dirs)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true")
    ap.add_argument("--payload")
    ap.add_argument("--vectors")
    ap.add_argument("--verdicts")
    ap.add_argument("--meta")
    ap.add_argument("--expect", choices=["ext4", "f2fs"])
    ap.add_argument("--image")
    ap.add_argument("--out")
    a = ap.parse_args()
    if a.list:
        json.dump(probes(), sys.stdout, separators=(",", ":"))
        sys.stdout.write("\n")
    elif a.payload:
        n = len(write_payload(a.payload))
        print("payload: %d probes" % n)
    elif a.vectors:
        merge(a.vectors, a.verdicts, a.meta)
    elif a.expect:
        expect(a.expect, a.image, a.out, a.meta)
    else:
        ap.error("one of --list, --payload, --vectors, --expect")


if __name__ == "__main__":
    main()

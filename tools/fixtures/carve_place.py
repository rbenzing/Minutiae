#!/usr/bin/env python3
"""Placement and oracle for the carving ground-truth images (3E).

usage: carve_place.py <objects-dir> <out-dir> <work-dir>

Builds carve-fat12.img.gz, carve-ext4.img.gz and carve-raw.img.gz plus
carve.expect.json in <out-dir> from the objects of carve_objects.py. Every image
is pre-filled with the ASCII pattern MINUTIAE-CARVE-FILL- (no object header
appears in it; asserted). Objects are written by the real tools (mtools,
debugfs), their placement is READ BACK from mshowfat / debugfs and the
filesystem's own free-space report (fsck.fat, dumpe2fs), and deleted with the
tools (mdel, debugfs rm), which free the clusters/blocks without erasing them.
Carving never consults deleted metadata, so how the deletion happened does not
matter. The generator exits 1 on any failure, missing family or wrong placement.
Minutiae computes none of this.
"""
import gzip
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys

PATTERN = b"MINUTIAE-CARVE-FILL-"
CLOCK = "2023-11-14 22:13:20"  # an absolute date is a frozen clock
MIB = 1 << 20
CMDS = []
WORK = OBJS = "<unset>"
ENV = dict(os.environ, MTOOLS_SKIP_CHECK="1", NO_FAKE_STAT="1", FAKETIME_DONT_FAKE_MONOTONIC="1",
           E2FSPROGS_FAKE_TIME="1700000000", LC_ALL="C.UTF-8")

# the twelve families' header patterns (offset, bytes), ALL of a family's must match
HEADERS = {
    "jpeg": [(0, b"\xff\xd8\xff")],
    "png": [(0, b"\x89PNG\r\n\x1a\n")],
    "gif": [(0, b"GIF8")],
    "webp": [(0, b"RIFF"), (8, b"WEBP")],
    "isobmff": [(4, b"ftyp")],
    "sqlite": [(0, b"SQLite format 3\x00")],
    "bplist": [(0, b"bplist00")],
    "zip": [(0, b"PK\x03\x04")],
    "pdf": [(0, b"%PDF-")],
    "gzip": [(0, b"\x1f\x8b\x08")],
    "wav": [(0, b"RIFF"), (8, b"WAVE")],
    "ogg": [(0, b"OggS\x00")],
}


def fail(msg):
    sys.exit("carve_place: " + msg)


def sha(b):
    return hashlib.sha256(b).hexdigest()


def sh(cmd, faketime=False, **kw):
    if faketime:
        cmd = ["faketime", "-f", CLOCK] + cmd
    CMDS.append(" ".join(str(c) for c in cmd).replace(WORK, "<work>").replace(OBJS, "<objects>"))
    res = subprocess.run([str(c) for c in cmd], env=ENV, capture_output=True, text=kw.pop("text", True), **kw)
    return res


def must(cmd, **kw):
    res = sh(cmd, **kw)
    if res.returncode != 0:
        fail("%s failed (%d): %s" % (cmd, res.returncode, res.stderr))
    return res


def family_at(buf, pos):
    """The built-in family whose header matches buf at pos, or None."""
    for fam, pats in HEADERS.items():
        if all(buf[pos + o:pos + o + len(b)] == b for o, b in pats):
            return fam
    return None


def headers_in(buf, unit):
    return [(i, family_at(buf, i)) for i in range(0, len(buf), unit) if family_at(buf, i)]


def fill(n, start=0):
    reps = n // len(PATTERN) + 2
    k = start % len(PATTERN)
    return (PATTERN * reps)[k:k + n]


def seeded(n, seed):
    out, i = bytearray(), 0
    while len(out) < n:
        out += hashlib.sha256(("%s:%d" % (seed, i)).encode()).digest()
        i += 1
    return bytes(out[:n])


def check_fill_clean():
    big = PATTERN * 4
    bad = [i for i in range(len(PATTERN) * 2) if family_at(big, i)]
    if bad:
        fail("the fill pattern carries a header at %r" % bad)
    # a header may also straddle two pattern repetitions only if it fits in the tripled string
    for fam, pats in HEADERS.items():
        for _, b in pats:
            if b in big:
                fail("the fill pattern contains the %s header bytes %r" % (fam, b))


def random_control(n, unit):
    """Seeded random bytes with no built-in header at any unit-aligned position."""
    seed = 0
    while True:
        b = seeded(n, "carve-random-%d" % seed)
        if not headers_in(b, unit):
            return b
        seed += 1


def text_control(cluster):
    """Two clusters of text that quote the signatures of the families at cluster starts and
    at 512-aligned positions; every one is followed by prose, so no validator accepts it."""
    quotes = [
        (0, b"GIF89a"),
        (512, b"\x89PNG\r\n\x1a\n"),
        (1024, b"RIFF\x24\x00\x00\x00WEBP"),
        (1536, b"\x00\x00\x00\x18ftypisom"),
        (cluster, b"\xff\xd8\xff"),
    ]
    prose = b"This control is plain text that quotes the signature of a file type at the start of a block.\n"
    buf = bytearray(prose * (2 * cluster // len(prose) + 2))[:2 * cluster]
    for off, q in quotes:
        buf[off:off + len(q)] = q
    return bytes(buf)


def ranges_of(nums):
    out = []
    for n in sorted(nums):
        if out and n == out[-1][1] + 1:
            out[-1][1] = n
        else:
            out.append([n, n])
    return out


def meta(name, fam, typ, data, off, state, expect, nested_in=None):
    return {"name": name, "family": fam, "type": typ, "image_offset": off, "length": len(data), "sha256": sha(data),
            "state": state, "expect": expect, "nested_in": nested_in}


# ------------------------------------------------------------------ FAT12

def mshowfat(fs, name):
    res = must(["mshowfat", "-i", fs, "::/" + name])
    line = res.stdout.strip().splitlines()[-1]
    m = re.fullmatch(r"::(.*?)((?: <[0-9,\-]*>)+)", line)
    if not m:
        fail("cannot parse mshowfat output %r" % line)
    out = []
    for part in filter(None, re.split(r"[ <>,]+", m.group(2))):
        lo, _, hi = part.partition("-")
        out.append([int(lo), int(hi or lo)])
    return out


def clusters_of(chain):
    return [c for lo, hi in chain for c in range(lo, hi + 1)]


def fsck_geometry(fs):
    res = sh(["fsck.fat", "-n", "-v", fs])
    if res.returncode != 0:
        fail("fsck.fat reports a problem: %s%s" % (res.stdout, res.stderr))
    rep = res.stdout

    def num(pat):
        m = re.search(pat, rep, re.M)
        if not m:
            fail("fsck.fat -v report lacks " + pat)
        return int(m.group(1))

    return {
        "cluster_size": num(r"^\s*(\d+) bytes per cluster"),
        "data_start": num(r"^Data area starts at byte (\d+)"),
        "cluster_count": num(r"^\s*(\d+) data clusters"),
        "used": num(r"^\S+: \d+ files, (\d+)/\d+ clusters"),
    }


def build_fat12(objdir, objs, workdir, name):
    cs = 4096
    psize = 3 * MIB
    fs = os.path.join(workdir, name + ".fs")
    open(fs, "wb").write(fill(psize))
    must(["mkfs.fat", "-F", "12", "-s", "8", "-f", "2", "-n", "CARVEFAT", "--invariant", fs], faketime=True)
    geo = fsck_geometry(fs)
    if geo["cluster_size"] != cs:
        fail("cluster size %d, want %d" % (geo["cluster_size"], cs))

    src = os.path.join(workdir, "fatsrc")
    os.makedirs(src, exist_ok=True)

    def stage(fname, data):
        p = os.path.join(src, fname)
        open(p, "wb").write(data)
        os.utime(p, (1700000000, 1700000000))
        return p

    files = []  # (name, data, family, type, expect, state)
    byfile = {o["file"]: o for o in objs}
    for o in objs:
        files.append((o["file"], open(os.path.join(objdir, o["file"]), "rb").read(), o["family"], o["type"], o["expect"], "deleted"))
    mp4 = open(os.path.join(objdir, "mp4-h264.mp4"), "rb").read()
    gif = open(os.path.join(objdir, "gif89a.gif"), "rb").read()
    inner_at = 8 + 13  # offset of the nested GIF inside the trailing free box
    payload = b"NESTED-PAD---" + gif + b"-END-"
    freebox = (len(payload) + 8).to_bytes(4, "big") + b"free" + payload
    outer = mp4 + freebox
    files.append(("nested-outer.mp4", outer, "isobmff", "mp4", "validated", "deleted"))
    jpg = open(os.path.join(objdir, "jpeg-baseline.jpg"), "rb").read()
    files.append(("live.jpg", jpg, "jpeg", "jpeg", "validated", "live"))
    tar_dir = os.path.join(workdir, "tarsrc")
    os.makedirs(tar_dir, exist_ok=True)
    open(os.path.join(tar_dir, "notes.txt"), "wb").write(b"plain notes, not a carvable type\n" * 20)
    open(os.path.join(tar_dir, "more.txt"), "wb").write(b"more plain notes\n" * 30)
    tarf = os.path.join(workdir, "archive.tar")
    must(["tar", "--sort=name", "--owner=0", "--group=0", "--numeric-owner", "--mtime=@1700000000", "-cf", tarf,
          "-C", tar_dir, "more.txt", "notes.txt"])
    tar_data = open(tarf, "rb").read()
    elf_data = open("/usr/bin/true", "rb").read()
    if elf_data[:4] != b"\x7fELF":
        fail("/usr/bin/true is not an ELF file")
    files.append(("archive.tar", tar_data, "tar", "tar", "not-in-set", "deleted"))
    files.append(("true.elf", elf_data, "elf", "elf", "not-in-set", "deleted"))
    frag = open(os.path.join(objdir, "jpeg-progressive.jpg"), "rb").read()

    def cp(fname, data):
        must(["mcopy", "-i", fs, "-m", stage(fname, data), "::/" + fname], faketime=True)

    # fragmentation: a (1 cluster) and b are written, a is deleted, the fragmented file then takes a's cluster
    # and continues after b (every mcopy is its own process and allocates first-fit)
    cp("hole-a.bin", fill(cs))
    cp("hole-b.bin", fill(cs))
    must(["mdel", "-i", fs, "::/hole-a.bin"], faketime=True)
    cp("fragmented.jpg", frag)
    for fname, data, *_ in files:
        cp(fname, data)

    chains = {n: mshowfat(fs, n) for n in ["hole-b.bin", "fragmented.jpg"] + [f[0] for f in files]}
    fchain = chains["fragmented.jpg"]
    if len(fchain) < 2:
        fail("fragmented.jpg is contiguous: %r" % fchain)
    for fname, data, *_ in files:
        ch = chains[fname]
        if len(ch) != 1 or (ch[0][1] - ch[0][0] + 1) * cs < len(data) or (ch[0][1] - ch[0][0]) * cs >= len(data):
            fail("%s is not one run of ceil(size/cluster) clusters: %r" % (fname, ch))
    used_ever = set()
    for ch in chains.values():
        used_ever.update(clusters_of(ch))
    for n in ("hole-b.bin", "fragmented.jpg") + tuple(f[0] for f in files if f[5] == "deleted"):
        must(["mdel", "-i", fs, "::/" + n], faketime=True)
    state_of = {f[0]: f[5] for f in files}
    live_chain = clusters_of(chains["live.jpg"])
    geo = fsck_geometry(fs)
    if geo["used"] != len(live_chain):
        fail("fsck.fat reports %d clusters in use, live.jpg holds %d" % (geo["used"], len(live_chain)))
    allc = set(range(2, geo["cluster_count"] + 2))
    free = allc - set(live_chain)
    for fname, data, *_ in files:
        if state_of[fname] == "deleted" and not set(clusters_of(chains[fname])) <= free:
            fail("%s: clusters not all free after deletion" % fname)
    if not set(clusters_of(fchain)) <= free:
        fail("fragmented.jpg: clusters not all free after deletion")

    def coff(c):
        return geo["data_start"] + (c - 2) * cs

    fsb = bytearray(open(fs, "rb").read())
    ctrl_pool = sorted(free - used_ever, reverse=True)
    top = ctrl_pool[0]

    ctrl_clusters = set()

    def take(n):
        nonlocal top
        first = top - n + 1
        want = set(range(first, top + 1))
        if not want <= (free - used_ever):
            fail("control clusters %d..%d are not free and unused" % (first, top))
        ctrl_clusters.update(want)
        top = first - 2  # one unused cluster between controls
        return first

    controls, placed = [], []
    rnd = random_control(4 * cs, 512)
    c0 = take(4)
    fsb[coff(c0):coff(c0) + len(rnd)] = rnd
    controls.append(("random", "random-16k.bin", coff(c0), rnd))
    txt = text_control(cs)
    c0 = take(2)
    fsb[coff(c0):coff(c0) + len(txt)] = txt
    controls.append(("text", "quoted-signatures.txt", coff(c0), txt))
    png = open(os.path.join(objdir, "png-rgb.png"), "rb").read()
    trunc = png[:len(png) * 60 // 100]
    c0 = take(1)
    fsb[coff(c0):coff(c0) + len(trunc)] = trunc
    controls.append(("truncated", "png-rgb-truncated.png", coff(c0), trunc))
    pal = open(os.path.join(objdir, "png-palette.png"), "rb").read()
    c0 = take(1)
    fsb[coff(c0) + 7:coff(c0) + 7 + len(pal)] = pal
    unaligned_off = coff(c0) + 7
    # the contiguous part of the free-or-unused-cluster test: no control may leave its clusters
    if len(pal) + 7 > cs or len(trunc) > cs:
        fail("a one-cluster control spans a cluster boundary")

    # everything free that nothing wrote still holds the fill (the oracle is complete)
    for c in ctrl_pool:
        if c in ctrl_clusters:
            continue
        s = coff(c)
        if bytes(fsb[s:s + cs]) != fill(cs, s):
            fail("free cluster %d does not hold the fill pattern" % c)
    open(fs, "wb").write(fsb)

    # the volume's directory metadata after the deletions must still be clean
    res = sh(["fsck.fat", "-n", fs])
    if res.returncode != 0:
        fail("fsck.fat after the deletions: %s" % res.stdout)

    # assemble: pattern, the MBR sector sfdisk wrote, the volume at 1 MiB
    img = bytearray(fill(4 * MIB))
    tab = os.path.join(workdir, "mbr.img")
    open(tab, "wb").write(b"\0" * (4 * MIB))
    script = "label: dos\nlabel-id: 0x4d494e55\nunit: sectors\n\nstart=2048, size=6144, type=01\n"
    must(["sfdisk", "--no-reread", "--no-tell-kernel", tab], input=script)
    mbr = open(tab, "rb").read(512)
    ent = mbr[446:462]
    if mbr[510:512] != b"\x55\xaa" or ent[4] != 1 or int.from_bytes(ent[8:12], "little") != 2048 or int.from_bytes(ent[12:16], "little") != 6144:
        fail("unexpected partition entry %r" % ent)
    img[:512] = mbr
    base = 1 * MIB
    img[base:base + psize] = fsb

    objects = []
    for fname, data, fam, typ, expect, state in files:
        ch = chains[fname]
        off = base + coff(ch[0][0])
        if bytes(img[off:off + len(data)]) != data:
            fail("%s: bytes at %d differ from the object" % (fname, off))
        objects.append(meta(fname, fam, typ, data, off, state, expect))
    outer_off = next(o["image_offset"] for o in objects if o["name"] == "nested-outer.mp4")
    objects.append(meta("nested-inner.gif", "gif", "gif", gif, outer_off + len(mp4) + inner_at, "deleted", "nested", outer_off))
    if bytes(img[outer_off + len(mp4) + inner_at:][:len(gif)]) != gif:
        fail("the nested GIF is not at the recorded offset")
    objects.append(meta("unaligned-png-palette.png", "png", "png", pal, base + unaligned_off, "placed", "unaligned"))
    ctrl_out = []
    for kind, cname, off, data in controls:
        ctrl_out.append({"kind": kind, "name": cname, "image_offset": base + off, "length": len(data), "first_fragment_length": len(data), "sha256": sha(data)})
    f0 = fchain[0]
    ctrl_out.append({"kind": "fragmented", "name": "fragmented.jpg", "image_offset": base + coff(f0[0]), "length": len(frag),
                     "first_fragment_length": min(len(frag), (f0[1] - f0[0] + 1) * cs), "sha256": sha(frag)})
    for cname, data in (("random-16k.bin", rnd), ("quoted-signatures.txt", txt), ("png-rgb-truncated.png", trunc)):
        o = next(c for c in ctrl_out if c["name"] == cname)
        if bytes(img[o["image_offset"]:o["image_offset"] + len(data)]) != data:
            fail("control %s is not where recorded" % cname)
    return bytes(img), {
        "name": name, "table": "mbr", "partition_start": base, "fs": "fat", "block_size": cs, "objects": objects, "controls": ctrl_out,
        "fragmented_chain": fchain,
    }


# ------------------------------------------------------------------ ext4

def build_ext4(objdir, objs, workdir, name):
    bs = 4096
    size = 8 * MIB
    img = os.path.join(workdir, name + ".img")
    open(img, "wb").write(fill(size))
    must(["mke2fs", "-q", "-F", "-t", "ext4", "-b", str(bs), "-O", "^has_journal", "-m", "0", "-L", "carveext4",
          "-U", "3c4a5e6d-1b2a-4c3d-8e9f-0a1b2c3d4e5f",
          "-E", "nodiscard,lazy_itable_init=0,root_owner=0:0,hash_seed=21222324-2526-4728-a92a-2b2c2d2e2f30", img], faketime=True)
    names = ["jpeg-baseline.jpg", "png-rgb.png", "heic.heic", "mp4-h264.mp4", "webp-lossy.webp", "sqlite.sqlite",
             "apk-like.zip", "pdf-single.pdf", "ogg-vorbis.ogg"]
    byfile = {o["file"]: o for o in objs}
    blocks = {}
    for n in names:
        if n not in byfile:
            fail("ext4 object %s missing" % n)
        must(["debugfs", "-w", "-R", "write %s /%s" % (os.path.join(objdir, n), n), img], faketime=True)
    for n in names:
        res = must(["debugfs", "-R", "blocks /" + n, img], faketime=True)
        b = [int(x) for x in res.stdout.split()]
        data = open(os.path.join(objdir, n), "rb").read()
        if not b or b != list(range(b[0], b[0] + len(b))) or len(b) != -(-len(data) // bs):
            fail("%s: blocks %r are not one contiguous run of ceil(size/block) blocks" % (n, b))
        blocks[n] = b
    for n in names:
        must(["debugfs", "-w", "-R", "rm /" + n, img], faketime=True)
    dump = must(["dumpe2fs", img], faketime=True).stdout
    fb = subprocess.run(["python3", os.path.join(os.path.dirname(os.path.abspath(__file__)), "ext4_freeblocks.py")],
                        input=dump, capture_output=True, text=True, check=True).stdout
    free = set()
    for lo, hi in json.loads(fb):
        free.update(range(lo, hi + 1))
    used_ever = set()
    for n in names:
        used_ever.update(blocks[n])
        if not set(blocks[n]) <= free:
            fail("%s: blocks not all free after rm" % n)
    res = sh(["e2fsck", "-fn", img])
    if res.returncode != 0:
        fail("e2fsck after the deletions: %s" % res.stdout)

    data = bytearray(open(img, "rb").read())
    top = max(free - used_ever)
    ctrl = []
    ctrl_blocks = set()

    def take(n):
        nonlocal top
        first = top - n + 1
        want = set(range(first, top + 1))
        if not want <= (free - used_ever):
            fail("control blocks %d..%d are not free and unused" % (first, top))
        ctrl_blocks.update(want)
        top = first - 2
        return first

    rnd = random_control(4 * bs, 512)
    b0 = take(4)
    data[b0 * bs:b0 * bs + len(rnd)] = rnd
    ctrl.append(("random", "random-16k.bin", b0 * bs, rnd))
    png = open(os.path.join(objdir, "png-rgb.png"), "rb").read()
    trunc = png[:len(png) * 60 // 100]
    b0 = take(1)
    data[b0 * bs:b0 * bs + len(trunc)] = trunc
    ctrl.append(("truncated", "png-rgb-truncated.png", b0 * bs, trunc))
    for b in sorted(free - used_ever - ctrl_blocks):
        s = b * bs
        blk = bytes(data[s:s + bs])
        # mke2fs zeroes the last blocks of the device (old RAID metadata); everything else is the fill
        if blk != fill(bs, s) and blk != bytes(bs):
            fail("free block %d holds neither the fill pattern nor zeros" % b)
    objects = []
    for n in names:
        o = byfile[n]
        d = open(os.path.join(objdir, n), "rb").read()
        off = blocks[n][0] * bs
        if bytes(data[off:off + len(d)]) != d:
            fail("%s: bytes at %d differ from the object" % (n, off))
        objects.append(meta(n, o["family"], o["type"], d, off, "deleted", o["expect"]))
    ctrl_out = [{"kind": k, "name": cn, "image_offset": off, "length": len(d), "first_fragment_length": len(d), "sha256": sha(d)} for k, cn, off, d in ctrl]
    return bytes(data), {"name": name, "table": "none", "partition_start": 0, "fs": "ext4", "block_size": bs, "objects": objects, "controls": ctrl_out}


# ------------------------------------------------------------------ raw

def build_raw(objdir, objs, name):
    size = 2 * MIB
    img = bytearray(fill(size))
    pos = 4096
    objects, ctrl = [], []

    def put(data, at):
        if at + len(data) > size:
            fail("raw image too small")
        img[at:at + len(data)] = data

    def adv(end, i):
        return -(-end // 512) * 512 + 512 * (1 + i % 4)

    for i, o in enumerate(objs):
        d = open(os.path.join(objdir, o["file"]), "rb").read()
        put(d, pos)
        objects.append(meta(o["file"], o["family"], o["type"], d, pos, "placed", o["expect"]))
        pos = adv(pos + len(d), i)
    pal = open(os.path.join(objdir, "png-palette.png"), "rb").read()
    put(pal, pos + 7)
    objects.append(meta("unaligned-png-palette.png", "png", "png", pal, pos + 7, "placed", "unaligned"))
    pos = adv(pos + 7 + len(pal), 1)
    png = open(os.path.join(objdir, "png-rgb.png"), "rb").read()
    for kind, cname, d in (("random", "random-16k.bin", random_control(4 * 4096, 512)),
                           ("text", "quoted-signatures.txt", text_control(4096)),
                           ("truncated", "png-rgb-truncated.png", png[:len(png) * 60 // 100])):
        put(d, pos)
        ctrl.append({"kind": kind, "name": cname, "image_offset": pos, "length": len(d), "first_fragment_length": len(d), "sha256": sha(d)})
        pos = adv(pos + len(d), 2)
    return bytes(img), {"name": name, "table": "none", "partition_start": 0, "fs": "none", "block_size": 0, "objects": objects, "controls": ctrl}


def main():
    global WORK, OBJS
    objdir, out, workdir = sys.argv[1:4]
    OBJS = objdir.rstrip("/")
    WORK = workdir.rstrip("/")
    os.makedirs(out, exist_ok=True)
    os.makedirs(workdir, exist_ok=True)
    check_fill_clean()
    oe = json.load(open(os.path.join(objdir, "objects.expect.json"), encoding="utf-8"))
    objs = oe["objects"]
    fams = {o["family"] for o in objs}
    missing = set(HEADERS) - fams
    if missing:
        fail("missing families: %s" % sorted(missing))

    images = []
    for build in (
        lambda: build_fat12(objdir, objs, workdir, "carve-fat12"),
        lambda: build_ext4(objdir, objs, workdir, "carve-ext4"),
        lambda: build_raw(objdir, objs, "carve-raw"),
    ):
        data, doc = build()
        # every recorded object range lies inside the image and the oracle rows are complete
        for o in doc["objects"]:
            if o["image_offset"] + o["length"] > len(data):
                fail("%s lies outside the image" % o["name"])
        doc["size"] = len(data)
        doc["image_sha256"] = sha(data)
        path = os.path.join(out, doc["name"] + ".img.gz")
        with open(path, "wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=9, mtime=0) as z:
                z.write(data)
        print("%s: %d bytes -> %d gz" % (doc["name"], len(data), os.path.getsize(path)))
        images.append(doc)

    pk = ["python3", "mtools", "dosfstools", "e2fsprogs", "fdisk", "faketime", "tar", "gzip"]
    pkgs = subprocess.run(["dpkg-query", "-W", "-f", "${Package} ${Version}\n"] + pk, check=True, capture_output=True, text=True).stdout
    order = ["name", "image_sha256", "size", "table", "partition_start", "fs", "block_size", "objects", "controls"]
    doc = {
        "generator": {
            "packages": [dict(zip(("package", "version"), l.split(" "))) for l in pkgs.strip().splitlines()],
            "fill_pattern": PATTERN.decode(),
            "objects_expect_sha256": sha(open(os.path.join(objdir, "objects.expect.json"), "rb").read()),
            "commands": CMDS,
        },
        "images": [{k: im[k] for k in order} | ({"fragmented_chain": im["fragmented_chain"]} if "fragmented_chain" in im else {}) for im in images],
    }
    with open(os.path.join(out, "carve.expect.json"), "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1)
        f.write("\n")


main()

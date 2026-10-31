#!/usr/bin/env python3
"""Placement and oracle for the image-format carving ground truth (3E2).

usage: carve_images_place.py <objects-dir> <out-dir>

Builds carve-images.img.gz and carve-images.expect.json in <out-dir> from the
objects of carve_images_objects.py (images.expect.json). The image is raw (no table, no filesystem) and
pre-filled with the ASCII pattern MINUTIAE-CARVE-FILL-. Every object sits at a
512-aligned offset. Controls: text quoting the new magics, the first 60% of a
BMP, TIFF, ICO and JPEG XL container, and seeded random data that starts with
BM and with 00 00 01 00. The generator exits 1 on any failure: a header of the
extended set anywhere it was not put on purpose, bytes at an image_offset that
do not hash to the object, a missing family. Minutiae computes none of this.
"""
import gzip
import hashlib
import json
import os
import subprocess
import sys

PATTERN = b"MINUTIAE-CARVE-FILL-"
SIZE = 1 << 20
UNIT = 512

# the 3E and 3E2 magics (offset, bytes); ALL of a family's patterns must match, a family may have several rows
HEADERS = {
    "jpeg": [[(0, b"\xff\xd8\xff")]],
    "png": [[(0, b"\x89PNG\r\n\x1a\n")]],
    "gif": [[(0, b"GIF8")]],
    "webp": [[(0, b"RIFF"), (8, b"WEBP")]],
    "isobmff": [[(4, b"ftyp")], [(0, bytes.fromhex("0000000c4a584c200d0a870a"))]],
    "sqlite": [[(0, b"SQLite format 3\x00")]],
    "bplist": [[(0, b"bplist00")]],
    "zip": [[(0, b"PK\x03\x04")]],
    "pdf": [[(0, b"%PDF-")]],
    "gzip": [[(0, b"\x1f\x8b\x08")]],
    "wav": [[(0, b"RIFF"), (8, b"WAVE")]],
    "ogg": [[(0, b"OggS\x00")]],
    "bmp": [[(0, b"BM")]],
    "tiff": [[(0, bytes.fromhex("49492a00"))], [(0, bytes.fromhex("4d4d002a"))], [(0, bytes.fromhex("49492b00"))],
             [(0, bytes.fromhex("4d4d002b"))], [(0, b"IIRO")], [(0, b"IIRS")], [(0, b"MMOR")], [(0, bytes.fromhex("49495500"))]],
    "ico": [[(0, bytes.fromhex("00000100"))]],
    "cur": [[(0, bytes.fromhex("00000200"))]],
}


def fail(msg):
    sys.exit("carve_images_place: " + msg)


def sha(b):
    return hashlib.sha256(b).hexdigest()


def family_at(buf, pos):
    for fam, rows in HEADERS.items():
        for pats in rows:
            if all(buf[pos + o:pos + o + len(b)] == b for o, b in pats):
                return fam
    return None


def headers_in(buf, unit=UNIT, skip0=False):
    return [(i, family_at(buf, i)) for i in range(unit if skip0 else 0, len(buf), unit) if family_at(buf, i)]


def fill(n):
    return (PATTERN * (n // len(PATTERN) + 2))[:n]


def seeded(n, seed):
    out, i = bytearray(), 0
    while len(out) < n:
        out += hashlib.sha256(("%s:%d" % (seed, i)).encode()).digest()
        i += 1
    return bytes(out[:n])


def check_fill_clean():
    big = PATTERN * 4
    if [i for i in range(len(PATTERN) * 2) if family_at(big, i)]:
        fail("the fill pattern carries a header")
    for rows in HEADERS.values():
        for pats in rows:
            for _, b in pats:
                if b in big:
                    fail("the fill pattern contains the header bytes %r" % b)


def random_control(magic, n):
    """Seeded random bytes that start with magic and hold no other header at a 512-aligned position."""
    seed = 0
    while True:
        b = magic + seeded(n, "carve-images-random-%s-%d" % (magic.hex(), seed))[len(magic):]
        if not headers_in(b, skip0=True):
            return b
        seed += 1


def text_control():
    """Prose that quotes each new magic at a 512-aligned start, every quote followed by prose."""
    quotes = [b"BM"] + [rows[0][1] for rows in HEADERS["tiff"]] + [HEADERS["ico"][0][0][1], HEADERS["cur"][0][0][1],
                                                                  HEADERS["isobmff"][1][0][1]]
    prose = b"This control is plain text that quotes the signature of a file type at the start of a block.\n"
    buf = bytearray(prose * (len(quotes) * UNIT // len(prose) + 2))[:len(quotes) * UNIT]
    for k, q in enumerate(quotes):
        buf[k * UNIT:k * UNIT + len(q)] = q
    return bytes(buf), quotes


def main():
    objdir, out = sys.argv[1:3]
    os.makedirs(out, exist_ok=True)
    check_fill_clean()
    oe = json.load(open(os.path.join(objdir, "images.expect.json"), encoding="utf-8"))
    objs = list(oe["objects"])
    fams = {o["family"] for o in objs}
    need = {"bmp", "tiff", "isobmff", "ico", "cur"}
    if need - fams:
        fail("missing families: %s" % sorted(need - fams))

    img = bytearray(fill(SIZE))
    placed = []  # (offset, length) of everything written, for the final header audit
    pos = UNIT * 8

    def put(data):
        nonlocal pos
        if pos + len(data) > SIZE:
            fail("image too small")
        img[pos:pos + len(data)] = data
        placed.append((pos, len(data)))
        at = pos
        pos = -(-(pos + len(data)) // UNIT) * UNIT + UNIT * (1 + len(placed) % 3)
        return at

    objects, controls = [], []
    for o in objs:
        data = open(os.path.join(objdir, o["file"]), "rb").read()
        if sha(data) != o["sha256"]:
            fail("%s does not match images.expect.json" % o["file"])
        at = put(data)
        objects.append({"name": o["file"], "family": o["family"], "type": o["type"], "image_offset": at, "length": len(data),
                        "sha256": sha(data), "state": "placed", "expect": o["expect"], "nested_in": None})

    def control(kind, name, data):
        at = put(data)
        controls.append({"kind": kind, "name": name, "image_offset": at, "length": len(data), "first_fragment_length": len(data),
                         "sha256": sha(data)})

    text, quotes = text_control()
    control("text", "quoted-signatures.txt", text)
    byname = {o["file"]: o for o in objs}
    for name in ("bmp-v3-24.bmp", "tiff-le.tif", "ico-dib.ico", "jxl-container.jxl"):
        d = open(os.path.join(objdir, name), "rb").read()
        control("truncated", name + ".truncated", d[:len(d) * 60 // 100])
        if byname[name]["size"] != len(d):
            fail("size mismatch for " + name)
    control("random", "random-bm-4k.bin", random_control(b"BM", 4096))
    control("random", "random-ico-4k.bin", random_control(bytes.fromhex("00000100"), 4096))

    img = bytes(img)
    # every recorded range lies inside the image and the bytes there hash to the object
    for o in objects + controls:
        if o["image_offset"] % UNIT:
            fail("%s is not 512-aligned" % o["name"])
        if sha(img[o["image_offset"]:o["image_offset"] + o["length"]]) != o["sha256"]:
            fail("the bytes at %d do not hash to %s" % (o["image_offset"], o["name"]))
    # a header of the extended set sits only where one was put on purpose: an object start, a control start or a quote
    expected = {o["image_offset"] for o in objects}
    expected |= {c["image_offset"] for c in controls if c["kind"] != "text"} | {c["image_offset"] for c in controls if c["kind"] == "truncated"}
    tc = next(c for c in controls if c["kind"] == "text")
    expected |= {tc["image_offset"] + k * UNIT for k in range(len(quotes))}
    stray = [(i, f) for i, f in headers_in(img) if i not in expected]
    if stray:
        fail("headers at unexpected 512-aligned offsets: %r" % stray)
    missing = [o["name"] for o in objects if family_at(img, o["image_offset"]) is None]
    if missing:
        fail("objects without a header at their offset: %s" % missing)

    path = os.path.join(out, "carve-images.img.gz")
    with open(path, "wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=9, mtime=0) as z:
            z.write(img)
    gz = os.path.getsize(path)
    print("carve-images: %d bytes -> %d gz" % (len(img), gz))
    if gz > 250 << 10:
        fail("carve-images.img.gz is %d bytes, budget 256000" % gz)

    pk = ["python3", "faketime", "gzip"]
    pkgs = subprocess.run(["dpkg-query", "-W", "-f", "${Package} ${Version}\n"] + pk, check=True, capture_output=True, text=True).stdout
    doc = {
        "generator": {
            "packages": [dict(zip(("package", "version"), l.split(" "))) for l in pkgs.strip().splitlines()],
            "fill_pattern": PATTERN.decode(),
            "objects_expect_sha256": sha(open(os.path.join(objdir, "images.expect.json"), "rb").read()),
        },
        "images": [{"name": "carve-images", "image_sha256": sha(img), "size": len(img), "table": "none", "partition_start": 0,
                    "fs": "none", "block_size": 0, "objects": objects, "controls": controls}],
    }
    with open(os.path.join(out, "carve-images.expect.json"), "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1)
        f.write("\n")


main()

#!/usr/bin/env python3
"""Real-encoder image-format objects for the carving fixtures (3E2).

usage: carve_images_objects.py <objects-dir> <work-dir>

Writes eighteen objects into <objects-dir> and images.expect.json beside them
(the schema of objects.expect.json). Encoders (Debian packages of the pinned
Dockerfile.carve): ImageMagick (BMP, big-endian TIFF), Pillow (TIFF, pages,
Exif and GPS directories, ICO), libtiff-tools (BigTIFF, tiled), icoutils (ICO
with DIB entries, CUR), libavif-bin (AVIF still and sequence), libjxl-tools
(JPEG XL container from a JPEG transcode). DNG, CR2, ORF, RW2 and CR3 are not
attempted (ruling Q3: builder-only).

Every fact in the oracle is what a separate tool reports about the finished
bytes (identify, tiffinfo, tiffdump, icotool, avifdec, jxlinfo, Pillow) or a
pure-Python ISO-BMFF box lister written here; Minutiae computes none of it.
The objects of the 3E set are not touched.
"""
import hashlib
import json
import os
import re
import struct
import subprocess
import sys

from PIL import Image, TiffImagePlugin

CMDS = []
OBJDIR = WORK = "<unset>"
W, H = 64, 48


def run(cmd, **kw):
    CMDS.append(" ".join(str(c) for c in cmd).replace(OBJDIR, "<objects>").replace(WORK, "<work>"))
    return subprocess.run([str(c) for c in cmd], check=True, capture_output=True, text=True, **kw)


def sha(b):
    return hashlib.sha256(b).hexdigest()


def gradient(alpha=False):
    im = Image.new("RGBA" if alpha else "RGB", (W, H))
    px = im.load()
    for y in range(H):
        for x in range(W):
            r, g, b = (x * 4) % 256, (y * 5) % 256, ((x + y) * 2) % 256
            px[x, y] = (r, g, b, (x * 3 + y * 2) % 256) if alpha else (r, g, b)
    return im


def boxes(data):
    """Top-level ISO-BMFF boxes as (type, size); asserts they tile the file exactly."""
    out, pos = [], 0
    while pos < len(data):
        if pos + 8 > len(data):
            sys.exit("box walk: short header at %d" % pos)
        size, typ = struct.unpack_from(">I4s", data, pos)
        hdr = 8
        if size == 1:
            size = struct.unpack_from(">Q", data, pos + 8)[0]
            hdr = 16
        elif size == 0:
            size = len(data) - pos
        if size < hdr or pos + size > len(data):
            sys.exit("box walk: bad size %d at %d" % (size, pos))
        out.append((typ.decode("latin-1"), size))
        pos += size
    return out


def bmp_header(data):
    dib = struct.unpack_from("<I", data, 14)[0]
    bpp = struct.unpack_from("<H", data, 24 if dib == 12 else 28)[0]
    comp = struct.unpack_from("<I", data, 30)[0] if dib != 12 else 0
    return {"bf_size": struct.unpack_from("<I", data, 2)[0], "dib_header_size": dib, "bits_per_pixel": bpp,
            "compression": comp}


def identify(path):
    out = run(["identify", "-format", "%m %w %h %z", path]).stdout.split()
    return out[0], int(out[1]), int(out[2])


def tiff_facts(path):
    dump = run(["tiffdump", path]).stdout
    m = re.search(r"Magic: 0x([0-9a-f]+) <(\S+)> Version: 0x([0-9a-f]+) <(\S+)>", dump)
    info = run(["tiffinfo", path]).stdout
    dirs = len(re.findall(r"^=== TIFF directory \d+ ===", info, re.M))
    sub = re.findall(r"^--- (EXIF|GPS) directory", run(["tiffinfo", "-D", path]).stdout, re.M)
    w = int(re.search(r"Image Width: (\d+)", info).group(1))
    h = int(re.search(r"Image Length: (\d+)", info).group(1))
    return (w, h), {
        "byte_order": "big" if m.group(2) == "big-endian" else "little", "magic": "0x" + m.group(1),
        "version": int(m.group(3), 16), "bigtiff": m.group(4) == "BigTIFF", "directories": dirs,
        "tiled": "Tile Width:" in info, "sub_directories": sorted(set(sub)),
    }


def icotool_entries(path):
    ents = []
    for line in run(["icotool", "-l", path]).stdout.splitlines():
        kv = dict(re.findall(r"--([a-z-]+)=(\d+)", line))
        ents.append({"kind": line.split()[0].lstrip("-"), "width": int(kv["width"]), "height": int(kv["height"]),
                     "bit_depth": int(kv["bit-depth"]), "palette_size": int(kv["palette-size"])})
    return ents


def main():
    global OBJDIR, WORK
    objdir, work = sys.argv[1:3]
    OBJDIR, WORK = objdir.rstrip("/"), work.rstrip("/")
    os.makedirs(objdir, exist_ok=True)
    os.makedirs(work, exist_ok=True)
    objects = []

    def p(name):
        return os.path.join(objdir, name)

    def wk(name):
        return os.path.join(work, name)

    def add(file, family, typ, ext, dims, brand, facts):
        data = open(p(file), "rb").read()
        objects.append({"file": file, "family": family, "type": typ, "ext": ext, "size": len(data), "sha256": sha(data),
                        "expect": "validated", "width": dims[0], "height": dims[1], "brand": brand, "facts": facts})

    gradient().save(wk("grad.png"), "PNG")
    gradient(alpha=True).save(wk("grad-alpha.png"), "PNG")
    Image.open(wk("grad.png")).transpose(Image.Transpose.FLIP_LEFT_RIGHT).save(wk("grad-flip.png"), "PNG")

    # BMP: ImageMagick writes every header generation
    def magick_bmp(name, args, spec, src="grad.png"):
        run(["convert", wk(src)] + args + ["%s:%s" % (spec, p(name))])

    magick_bmp("bmp-core24.bmp", [], "BMP2")
    magick_bmp("bmp-v3-24.bmp", ["-compress", "None"], "BMP3")
    magick_bmp("bmp-v3-8pal.bmp", ["-type", "palette", "-colors", "256", "-compress", "None"], "BMP3")
    magick_bmp("bmp-v3-1bit.bmp", ["-monochrome", "-compress", "None"], "BMP3")
    magick_bmp("bmp-v5-32.bmp", [], "BMP", "grad-alpha.png")
    magick_bmp("bmp-rle8.bmp", ["-type", "palette", "-colors", "256", "-compress", "RLE"], "BMP3")
    want = {"bmp-core24.bmp": (12, 24, 0), "bmp-v3-24.bmp": (40, 24, 0), "bmp-v3-8pal.bmp": (40, 8, 0),
            "bmp-v3-1bit.bmp": (40, 1, 0), "bmp-v5-32.bmp": (124, 32, 3), "bmp-rle8.bmp": (40, 8, 1)}
    for f, (dib, bpp, comp) in want.items():
        data = open(p(f), "rb").read()
        h = bmp_header(data)
        if (h["dib_header_size"], h["bits_per_pixel"], h["compression"]) != (dib, bpp, comp) or h["bf_size"] != len(data):
            # the v5 32-bit file may carry BI_RGB (0) instead of BI_BITFIELDS (3); the width and depth are what matter
            if not (f == "bmp-v5-32.bmp" and (h["dib_header_size"], h["bits_per_pixel"]) == (dib, bpp) and h["bf_size"] == len(data)):
                sys.exit("%s: header %r, want dib %d bpp %d comp %d" % (f, h, dib, bpp, comp))
        fmt, w, hh = identify(p(f))
        if (w, hh) != (W, H):
            sys.exit("%s: identify reports %dx%d" % (f, w, hh))
        with Image.open(p(f)) as im:
            im.load()
            pil = {"pillow_format": im.format, "pillow_mode": im.mode}
        add(f, "bmp", "bmp", "bmp", (w, hh), "", dict(h, identify_format=fmt, **pil))

    # TIFF
    base = Image.open(wk("grad.png"))
    base.save(p("tiff-le.tif"), "TIFF")
    a = base.transpose(Image.Transpose.FLIP_LEFT_RIGHT)
    b = base.transpose(Image.Transpose.FLIP_TOP_BOTTOM)
    base.save(p("tiff-le-pages.tif"), "TIFF", save_all=True, append_images=[a, b])
    # ImageMagick 6 ignores -endian MSB for TIFF (probed: it writes II), so libtiff-tools makes the big-endian file
    run(["tiffcp", "-B", p("tiff-le.tif"), p("tiff-be.tif")])
    run(["tiffcp", "-8", p("tiff-le.tif"), p("bigtiff.tif")])
    run(["tiffcp", "-t", "-w", "32", "-l", "32", p("tiff-le.tif"), p("tiff-tiled.tif")])
    ifd = TiffImagePlugin.ImageFileDirectory_v2()
    ifd[0x010F] = "carve fixture"
    ifd[0x8769] = {0x9003: "2023:11:14 22:13:20"}
    ifd[0x8825] = {1: "N", 3: "E"}
    base.save(p("tiff-exif.tif"), "TIFF", tiffinfo=ifd)
    expect_t = {"tiff-le.tif": ("little", False, 1, False, []), "tiff-le-pages.tif": ("little", False, 3, False, []),
                "tiff-be.tif": ("big", False, 1, False, []), "bigtiff.tif": ("little", True, 1, False, []),
                "tiff-tiled.tif": ("little", False, 1, True, []), "tiff-exif.tif": ("little", False, 1, False, ["EXIF", "GPS"])}
    for f, (order, big, ndir, tiled, subs) in expect_t.items():
        dims, facts = tiff_facts(p(f))
        got = (facts["byte_order"], facts["bigtiff"], facts["directories"], facts["tiled"], facts["sub_directories"])
        if got != (order, big, ndir, tiled, subs) or dims != (W, H):
            sys.exit("%s: tools report %r %r, want %r" % (f, dims, got, (order, big, ndir, tiled, subs)))
        add(f, "tiff", "tiff", "tif", dims, "", facts)

    # ICO / CUR
    base.save(p("ico-png.ico"), "ICO", sizes=[(16, 16), (32, 32)])
    run(["convert", wk("grad.png"), "-resize", "16x16", wk("i16.png")])
    run(["convert", wk("grad.png"), "-resize", "32x32", wk("i32.png")])
    run(["icotool", "-c", "-o", p("ico-dib.ico"), wk("i16.png"), wk("i32.png")])
    run(["icotool", "-c", "--cursor", "-o", p("cur.cur"), wk("i16.png"), wk("i32.png")])
    for f, fam, kind, dib_entries in (("ico-png.ico", "ico", "icon", False), ("ico-dib.ico", "ico", "icon", True),
                                      ("cur.cur", "cur", "cursor", True)):
        data = open(p(f), "rb").read()
        typ, count = struct.unpack_from("<2xHH", data, 0)
        ents = icotool_entries(p(f))
        if typ != (2 if fam == "cur" else 1) or count != len(ents) or any(e["kind"] != kind for e in ents):
            sys.exit("%s: icotool lists %r" % (f, ents))
        # the payload of every entry: PNG signature (Pillow) or a BITMAPINFOHEADER (icotool)
        pays = []
        for i in range(count):
            off = struct.unpack_from("<I", data, 6 + 16 * i + 12)[0]
            pays.append("png" if data[off:off + 8] == b"\x89PNG\r\n\x1a\n" else "dib")
        if set(pays) != ({"dib"} if dib_entries else {"png"}):
            sys.exit("%s: entry payloads %r" % (f, pays))
        big = max(ents, key=lambda e: e["width"] * e["height"])
        add(f, fam, fam, fam, (big["width"], big["height"]), "", {"entries": ents, "entry_count": count, "payloads": pays})

    # AVIF still and sequence (avifenc), JPEG XL container (cjxl)
    run(["avifenc", "-s", "10", "-j", "1", wk("grad.png"), p("avif.avif")])
    run(["avifenc", "-s", "10", "-j", "1", "--fps", "2", wk("grad.png"), wk("grad-flip.png"), p("avis.avif")])
    run(["cjxl", "--num_threads=0", p("jpeg-baseline.jpg"),  # a 3E object, read only
         p("jxl-container.jxl")])
    for f, typ, brand in (("avif.avif", "avif", "avif"), ("avis.avif", "avif", "avis")):
        data = open(p(f), "rb").read()
        bx = boxes(data)
        if bx[0][0] != "ftyp" or data[8:12].decode() != brand:
            sys.exit("%s: major brand %r, boxes %r" % (f, data[8:12], bx))
        info = run(["avifdec", "--info", p(f)]).stdout
        res = re.search(r"Resolution\s*:\s*(\d+)x(\d+)", info)
        if (int(res.group(1)), int(res.group(2))) != (W, H):
            sys.exit("%s: avifdec resolution %s" % (f, res.group(0)))
        compat = [data[16 + 4 * i:20 + 4 * i].decode("latin-1") for i in range((bx[0][1] - 16) // 4)]
        add(f, "isobmff", typ, "avif", (W, H), brand, {"boxes": [t for t, _ in bx], "compatible_brands": compat,
                                                      "avifdec_resolution": [W, H]})
    f = "jxl-container.jxl"
    data = open(p(f), "rb").read()
    bx = boxes(data)
    names = [t for t, _ in bx]
    if names[0] != "JXL " or "jbrd" not in names or data[16:20] != b"ftyp":
        sys.exit("%s: boxes %r" % (f, names))
    info = run(["jxlinfo", p(f)]).stdout
    m = re.search(r"JPEG XL image, (\d+)x(\d+)", info)
    if "container" not in info or "reconstruction data available" not in info or (int(m.group(1)), int(m.group(2))) != (W, H):
        sys.exit("%s: jxlinfo says %r" % (f, info))
    add(f, "isobmff", "jxl", "jxl", (W, H), "jxl ", {"boxes": names, "jxlinfo_container": True, "jpeg_reconstruction": True})

    pk = ["python3", "python3-pil", "imagemagick", "libtiff-tools", "icoutils", "libavif-bin", "libjxl-tools"]
    pkgs = subprocess.run(["dpkg-query", "-W", "-f", "${Package} ${Version}\n"] + pk, check=True, capture_output=True, text=True).stdout
    doc = {
        "generator": {
            "packages": [dict(zip(("package", "version"), l.split(" "))) for l in pkgs.strip().splitlines()],
            "pillow": Image.__version__,
            "commands": CMDS,
        },
        "objects": objects,
    }
    with open(p("images.expect.json"), "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=1)
        fh.write("\n")
    total = sum(o["size"] for o in objects)
    print("image objects: %d files, %d bytes" % (len(objects), total))
    if total > 250 << 10:
        sys.exit("the new objects total %d bytes, budget 256000" % total)


main()

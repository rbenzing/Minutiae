#!/usr/bin/env python3
"""Real-encoder objects for the carving fixtures (3E).

usage: carve_objects.py <objects-dir> <work-dir>

Writes one object per family and variant into <objects-dir> and
objects.expect.json beside them. Every object comes from a real encoder (Pillow,
ffmpeg, libheif's heif-enc, the sqlite3 shell, zip, gzip, plistlib) except the
incremental PDF, which is the Pillow PDF plus an appended update section written
here. The facts in objects.expect.json are what a separate tool reports about
the finished bytes (Pillow, ffprobe, heif-info, sqlite3 PRAGMAs, zipfile,
wave, plistlib, gzip); Minutiae computes none of it.

The pixel content is deterministic (a gradient; the progressive JPEG carries a
seeded noise layer so that it spans several 4 KiB clusters and can be
fragmented). Encoder flags are chosen for byte-identical output.
"""
import gzip
import hashlib
import json
import os
import plistlib
import re
import subprocess
import sys
import wave
import zipfile
from datetime import datetime, timezone

from PIL import Image

CMDS = []
OBJDIR = WORK = "<unset>"  # set by main; the recorded commands name neither directory
W, H = 64, 48


def run(cmd, **kw):
    CMDS.append(" ".join(str(c) for c in cmd).replace(OBJDIR, "<objects>").replace(WORK, "<work>"))
    return subprocess.run(cmd, check=True, capture_output=True, text=kw.pop("text", True), **kw)


def noise(n, seed):
    out = bytearray()
    i = 0
    while len(out) < n:
        out += hashlib.sha256(("%s:%d" % (seed, i)).encode()).digest()
        i += 1
    return bytes(out[:n])


def gradient(noisy=False):
    im = Image.new("RGB", (W, H))
    px = im.load()
    nz = noise(W * H * 3, "carve-jpeg-noise") if noisy else None
    for y in range(H):
        for x in range(W):
            r, g, b = (x * 4) % 256, (y * 5) % 256, ((x + y) * 2) % 256
            if noisy:
                k = (y * W + x) * 3
                r, g, b = (r + nz[k]) % 256, (g + nz[k + 1]) % 256, (b + nz[k + 2]) % 256
            px[x, y] = (r, g, b)
    return im


def sha(b):
    return hashlib.sha256(b).hexdigest()


def pil_facts(path, **extra):
    with Image.open(path) as im:
        im.load()
        facts = {"format": im.format, "mode": im.mode, "size": list(im.size)}
        facts.update(extra)
        return im.size, facts


def ffprobe(path):
    out = run(["ffprobe", "-v", "error", "-show_format", "-show_streams", "-of", "json", path]).stdout
    j = json.loads(out)
    st = j["streams"][0]
    tags = j["format"].get("tags", {})
    facts = {
        "format_name": j["format"]["format_name"],
        "major_brand": tags.get("major_brand", ""),
        "compatible_brands": tags.get("compatible_brands", ""),
        "codec_name": st["codec_name"],
        "duration": j["format"]["duration"],
        "nb_streams": j["format"]["nb_streams"],
    }
    if st["codec_type"] == "video":
        facts["nb_frames"] = st.get("nb_frames", "")
    return (st.get("width", 0), st.get("height", 0)), facts


def ffmpeg_video(out, fmt):
    run([
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
        "testsrc=size=%dx%d:rate=10:duration=1" % (W, H), "-c:v", "libx264", "-pix_fmt", "yuv420p",
        "-threads", "1", "-x264-params", "threads=1:sliced-threads=0", "-fflags", "+bitexact",
        "-flags:v", "+bitexact", "-map_metadata", "-1", "-an", "-f", fmt, out,
    ])


def ffmpeg_audio(out, codec, fmt):
    run([
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
        "sine=frequency=440:duration=1:sample_rate=8000", "-c:a", codec, "-ac", "1", "-fflags", "+bitexact",
        "-flags:a", "+bitexact", "-map_metadata", "-1", "-f", fmt, out,
    ])


def pdf_update(base):
    """Append an incremental update (a new Info object, a new xref section and a
    trailer with /Prev) to a finished PDF. Returns the bytes and the facts."""
    old_sx = int(re.search(rb"startxref\s+(\d+)\s+%%EOF\s*$", base).group(1))
    size = int(re.search(rb"/Size (\d+)", base[old_sx:]).group(1))
    root = re.search(rb"/Root (\d+ 0 R)", base[old_sx:]).group(1)
    body = base if base.endswith(b"\n") else base + b"\n"
    obj_off = len(body)
    obj = b"%d 0 obj\n<< /Title (carve fixture update) >>\nendobj\n" % size
    xref_off = obj_off + len(obj)
    xref = b"xref\n%d 1\n%010d 00000 n \n" % (size, obj_off)
    trailer = b"trailer\n<< /Size %d /Root %s /Info %d 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n" % (
        size + 1, root, size, old_sx, xref_off)
    return body + obj + xref + trailer


def pdf_facts(data):
    sx = [(m.start(), int(m.group(1))) for m in re.finditer(rb"startxref\s+(\d+)\s+%%EOF", data)]
    eof = [m.start() for m in re.finditer(rb"%%EOF", data)]
    for _, off in sx:
        if data[off:off + 4] != b"xref":
            sys.exit("startxref %d does not point at xref" % off)
    return {"startxref": [v for _, v in sx], "eof_offsets": eof, "xref_checked": True}


def main():
    global OBJDIR, WORK
    objdir, work = sys.argv[1:3]
    OBJDIR, WORK = objdir.rstrip("/"), work.rstrip("/")
    os.makedirs(objdir, exist_ok=True)
    os.makedirs(work, exist_ok=True)
    objects = []

    def add(file, family, typ, ext, expect, dims=(0, 0), brand="", facts=None):
        data = open(os.path.join(objdir, file), "rb").read()
        objects.append({
            "file": file, "family": family, "type": typ, "ext": ext, "size": len(data), "sha256": sha(data),
            "expect": expect, "width": dims[0], "height": dims[1], "brand": brand, "facts": facts or {},
        })

    def p(name):
        return os.path.join(objdir, name)

    # JPEG
    gradient().save(p("jpeg-baseline.jpg"), "JPEG", quality=85, optimize=False, progressive=False)
    gradient(noisy=True).save(p("jpeg-progressive.jpg"), "JPEG", quality=100, subsampling=0, optimize=False, progressive=True)
    for f, prog in (("jpeg-baseline.jpg", False), ("jpeg-progressive.jpg", True)):
        with Image.open(p(f)) as im:
            got = bool(im.info.get("progressive"))
        if got != prog:
            sys.exit("%s: Pillow reports progressive=%s" % (f, got))
        dims, facts = pil_facts(p(f), progressive=got)
        add(f, "jpeg", "jpeg", "jpg", "validated", dims, "", facts)
    if os.path.getsize(p("jpeg-progressive.jpg")) <= 2 * 4096:
        sys.exit("jpeg-progressive.jpg must span at least three 4 KiB clusters to be fragmented")

    # PNG
    gradient().save(p("png-rgb.png"), "PNG", compress_level=9, optimize=False)
    pal = gradient().quantize(colors=16, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE)
    pal.save(p("png-palette.png"), "PNG", compress_level=9, optimize=False)
    for f in ("png-rgb.png", "png-palette.png"):
        dims, facts = pil_facts(p(f))
        add(f, "png", "png", "png", "validated", dims, "", facts)

    # GIF: Pillow writes GIF87a without extensions and GIF89a once a loop or duration is set
    g1 = gradient()
    g2 = gradient().transpose(Image.Transpose.FLIP_LEFT_RIGHT)
    g1.save(p("gif89a.gif"), "GIF", save_all=True, append_images=[g2], duration=100, loop=0)
    gradient().convert("P", palette=Image.Palette.ADAPTIVE, colors=64).save(p("gif87a.gif"), "GIF")
    for f, ver in (("gif89a.gif", b"GIF89a"), ("gif87a.gif", b"GIF87a")):
        raw = open(p(f), "rb").read()
        if raw[:6] != ver:
            sys.exit("%s: header %r, want %r" % (f, raw[:6], ver))
        with Image.open(p(f)) as im:
            frames = getattr(im, "n_frames", 1)
            info_ver = im.info.get("version", b"").decode()
            size = im.size
        add(f, "gif", "gif", "gif", "validated", size, "", {"version": info_ver, "n_frames": frames})

    # WebP
    gradient().save(p("webp-lossy.webp"), "WEBP", lossless=False, quality=80, method=4)
    gradient().save(p("webp-lossless.webp"), "WEBP", lossless=True, quality=100, method=4)
    for f, lossless in (("webp-lossy.webp", False), ("webp-lossless.webp", True)):
        dims, facts = pil_facts(p(f), lossless=lossless)
        add(f, "webp", "webp", "webp", "validated", dims, "", facts)

    # HEIC (heif-enc encodes the PNG; heif-info must report brand heic, ruling 3E-R27)
    run(["heif-enc", "-q", "50", "-o", p("heic.heic"), p("png-rgb.png")])
    info = run(["heif-info", p("heic.heic")]).stdout
    brand = re.search(r"^main brand: (\S+)", info, re.M).group(1)
    if brand != "heic":
        sys.exit("heif-info reports brand %r, want heic" % brand)
    m = re.search(r"^image: (\d+)x(\d+)", info, re.M)
    add("heic.heic", "isobmff", "heic", "heic", "validated", (int(m.group(1)), int(m.group(2))), brand, {
        "mime": re.search(r"^MIME type: (\S+)", info, re.M).group(1),
        "compatible_brands": re.search(r"^compatible brands: (.*)$", info, re.M).group(1),
    })

    # ISO-BMFF video (libx264)
    for f, fmt, typ in (("mp4-h264.mp4", "mp4", "mp4"), ("mov-h264.mov", "mov", "mov"), ("3gp.3gp", "3gp", "3gp")):
        ffmpeg_video(p(f), fmt)
        dims, facts = ffprobe(p(f))
        add(f, "isobmff", typ, typ, "validated", dims, facts["major_brand"], facts)

    # SQLite 3: two tables, several pages
    sq = p("sqlite.sqlite")
    if os.path.exists(sq):
        os.remove(sq)
    sql = ["PRAGMA page_size=4096;", "CREATE TABLE notes(id INTEGER PRIMARY KEY, title TEXT, body TEXT);",
           "CREATE TABLE tags(id INTEGER PRIMARY KEY, note INTEGER, tag TEXT);"]
    for i in range(1, 41):
        sql.append("INSERT INTO notes VALUES(%d,'note %d','%s');" % (i, i, noise(60, "note%d" % i).hex()))
        sql.append("INSERT INTO tags VALUES(%d,%d,'tag-%d');" % (i, i, i % 7))
    run(["sqlite3", sq], input="\n".join(sql) + "\n")
    out = run(["sqlite3", sq, "PRAGMA page_size; PRAGMA page_count; PRAGMA integrity_check;"]).stdout.split()
    psz, pcount, integ = int(out[0]), int(out[1]), out[2]
    if integ != "ok" or pcount < 3 or os.path.getsize(sq) != psz * pcount:
        sys.exit("sqlite facts: %r size %d" % (out, os.path.getsize(sq)))
    add("sqlite.sqlite", "sqlite", "sqlite", "sqlite", "validated", (0, 0), "",
        {"page_size": psz, "page_count": pcount, "integrity_check": integ})

    # binary plist
    pl = plistlib.dumps({"name": "carve fixture", "count": 3, "ratio": 1.5, "flag": True,
                         "items": ["alpha", "beta", "gamma"], "when": datetime(2023, 11, 14, 22, 13, 20, tzinfo=timezone.utc).replace(tzinfo=None),
                         "blob": b"\x00\x01\x02\x03" * 8}, fmt=plistlib.FMT_BINARY, sort_keys=True)
    open(p("bplist.plist"), "wb").write(pl)
    back = plistlib.loads(pl)
    add("bplist.plist", "bplist", "bplist", "plist", "validated", (0, 0), "",
        {"roundtrip": back["count"] == 3 and back["items"][2] == "gamma", "keys": sorted(back)})

    # ZIP: the zip tool, fixed mtimes, no extra attributes
    zsrc = os.path.join(work, "zsrc")
    os.makedirs(zsrc, exist_ok=True)

    def mkzip(out, entries, store=()):
        if os.path.exists(out):
            os.remove(out)
        for name, data in entries:
            fp = os.path.join(zsrc, name)
            os.makedirs(os.path.dirname(fp), exist_ok=True)
            open(fp, "wb").write(data)
            os.utime(fp, (1700000000, 1700000000))
        for name, _ in entries:
            level = "-0" if name in store else "-6"
            run(["zip", "-q", "-X", "-nw", level, out, name], cwd=zsrc)

    def zip_facts(path, expect_names):
        with zipfile.ZipFile(path) as z:
            names = z.namelist()
            if names != expect_names or z.testzip() is not None:
                sys.exit("%s: zip facts %r" % (path, names))
            return {"namelist": names, "testzip": None, "compress_type": [i.compress_type for i in z.infolist()]}

    text = (b"carve fixture entry, deflates well. " * 40)
    mkzip(p("zip-mixed.zip"), [("stored.txt", noise(300, "stored")), ("deflated.txt", text)], store=("stored.txt",))
    add("zip-mixed.zip", "zip", "zip", "zip", "validated", (0, 0), "", zip_facts(p("zip-mixed.zip"), ["stored.txt", "deflated.txt"]))
    apk = [("AndroidManifest.xml", b"<manifest package=\"carve.fixture\"/>\n" * 8),
           ("classes.dex", b"dex\n035\x00" + noise(400, "dex")), ("META-INF/MANIFEST.MF", b"Manifest-Version: 1.0\nCreated-By: fixture\n")]
    mkzip(p("apk-like.zip"), apk)
    add("apk-like.zip", "zip", "apk", "apk", "validated", (0, 0), "", zip_facts(p("apk-like.zip"), [n for n, _ in apk]))
    docx = [("[Content_Types].xml", b"<Types xmlns=\"http://schemas.openxmlformats.org/package/2006/content-types\"/>\n" * 4),
            ("word/document.xml", b"<w:document xmlns:w=\"urn:carve\"><w:body>fixture</w:body></w:document>\n" * 6)]
    mkzip(p("docx-like.zip"), docx)
    add("docx-like.zip", "zip", "docx", "docx", "validated", (0, 0), "", zip_facts(p("docx-like.zip"), [n for n, _ in docx]))

    # PDF: Pillow's writer, then an appended incremental update
    gradient().save(p("pdf-single.pdf"), "PDF", resolution=72.0)
    single = open(p("pdf-single.pdf"), "rb").read()
    open(p("pdf-incremental.pdf"), "wb").write(pdf_update(single))
    for f in ("pdf-single.pdf", "pdf-incremental.pdf"):
        facts = pdf_facts(open(p(f), "rb").read())
        add(f, "pdf", "pdf", "pdf", "footer", (0, 0), "", facts)

    # gzip: gzip -n (no name, no mtime)
    src = os.path.join(work, "gzsrc.txt")
    open(src, "wb").write(b"".join(b"line %d of the carve fixture gzip stream\n" % i for i in range(200)))
    with open(p("gzip.gz"), "wb") as f:
        CMDS.append("gzip -n -9 -c <work>/gzsrc.txt")
        subprocess.run(["gzip", "-n", "-9", "-c", src], check=True, stdout=f)
    raw = open(p("gzip.gz"), "rb").read()
    add("gzip.gz", "gzip", "gzip", "gz", "bounded", (0, 0), "",
        {"inflates": gzip.decompress(raw) == open(src, "rb").read(), "inflated_size": len(gzip.decompress(raw))})

    # WAV (pcm_s16le) and Ogg Vorbis
    ffmpeg_audio(p("wav-pcm.wav"), "pcm_s16le", "wav")
    with wave.open(p("wav-pcm.wav")) as w:
        add("wav-pcm.wav", "wav", "wav", "wav", "validated", (0, 0), "",
            {"nchannels": w.getnchannels(), "framerate": w.getframerate(), "nframes": w.getnframes(), "sampwidth": w.getsampwidth()})
    ffmpeg_audio(p("ogg-vorbis.ogg"), "libvorbis", "ogg")
    _, ofacts = ffprobe(p("ogg-vorbis.ogg"))
    if ofacts["format_name"] != "ogg":
        sys.exit("ogg: format_name %r" % ofacts["format_name"])
    add("ogg-vorbis.ogg", "ogg", "ogg", "ogg", "validated", (0, 0), "", {"format_name": ofacts["format_name"], "codec_name": ofacts["codec_name"]})

    pk = ["python3", "python3-pil", "ffmpeg", "libheif-examples", "sqlite3", "zip", "gzip"]
    pkgs = subprocess.run(["dpkg-query", "-W", "-f", "${Package} ${Version}\n"] + pk, check=True, capture_output=True, text=True).stdout
    doc = {
        "generator": {
            "packages": [dict(zip(("package", "version"), l.split(" "))) for l in pkgs.strip().splitlines()],
            "pillow": Image.__version__,
            "commands": CMDS,
        },
        "objects": objects,
    }
    with open(p("objects.expect.json"), "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1)
        f.write("\n")
    total = sum(o["size"] for o in objects)
    print("objects: %d files, %d bytes" % (len(objects), total))


main()

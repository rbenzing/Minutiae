#!/usr/bin/env python3
"""Independent EWF (E01) decoder and oracle for the ewf fixtures.

Usage: ewf_inspect.py <segment files in order...> --raw disk.img [--raw-bytes N]

Written from the format notes in the 2E plan, not from the Go reader and not
from libewf. It walks every section of every segment, checks every descriptor
and payload Adler-32, decodes EVERY chunk (zlib stream or raw bytes + Adler-32),
rebuilds the media and exits non-zero unless the rebuilt media equals the raw
image (its first --raw-bytes bytes when given, else the whole file).

On success it prints one JSON document: segments and sections, the volume,
the tables, the stored hashes, the chunk statistics and a few structural
observations (which conventions the writer used). On any structural problem or
a media mismatch it prints a message to stderr and exits 1.

Standard library only; every number read from the file is bounds-checked
before it drives a loop or a slice, although the inputs are trusted fixtures.
"""
import hashlib
import json
import struct
import sys
import zlib

SIGNATURE = b"EVF\x09\x0d\x0a\xff\x00"
SEG_HEADER = 13
DESC = 76
VOLUME_PAYLOAD = 1052
TERMINAL = (b"next", b"done")


class Bad(Exception):
    pass


def fail(msg):
    raise Bad(msg)


def u16(b, o):
    return struct.unpack_from("<H", b, o)[0]


def u32(b, o):
    return struct.unpack_from("<I", b, o)[0]


def u64(b, o):
    return struct.unpack_from("<Q", b, o)[0]


def adler(b):
    return zlib.adler32(b) & 0xFFFFFFFF


def check_adler(b, upto, at, what):
    """Adler-32 of b[:upto] must equal the u32 at b[at:]."""
    if at + 4 > len(b):
        fail("%s: checksum field out of range" % what)
    if adler(b[:upto]) != u32(b, at):
        fail("%s: adler32 mismatch" % what)


def parse_volume(p, what):
    if len(p) != VOLUME_PAYLOAD:
        fail("%s: payload is %d bytes, want %d" % (what, len(p), VOLUME_PAYLOAD))
    check_adler(p, 1048, 1048, what)
    return {
        "media_type": p[0],
        "chunks": u32(p, 4),
        "spc": u32(p, 8),
        "bps": u32(p, 12),
        "sectors": u64(p, 16),
        "chs": [u32(p, 24), u32(p, 28), u32(p, 32)],
        "media_flags": p[36],
        "palm_start_sector": u32(p, 40),
        "legacy_log_start_sector": u32(p, 48),
        "compression_level": p[52],
        "error_granularity": u32(p, 56),
        "set_identifier": p[64:80].hex(),
        "signature": p[1043:1048].hex(),
        "nonzero_unknown_regions": nonzero_unknown(p),
    }


def nonzero_unknown(p):
    """Offsets of non-zero bytes outside the documented fields (observation)."""
    known = set()
    for lo, hi in ((0, 1), (4, 8), (8, 12), (12, 16), (16, 24), (24, 36),
                   (36, 37), (40, 44), (48, 52), (52, 53), (56, 60),
                   (64, 80), (1043, 1052)):
        known.update(range(lo, hi))
    return [i for i in range(len(p)) if p[i] and i not in known]


def parse_header_text(raw, wide):
    if wide:
        if raw[:2] == b"\xff\xfe":
            raw = raw[2:]
        text = raw.decode("utf-16-le")
    else:
        text = raw.decode("ascii")
    lines = text.replace("\r\n", "\n").split("\n")
    # Line 0: number of categories. Then per category: its name, the field
    # names, the values and a blank line (header2 has more categories, each
    # with its own layout; only "main" is decoded).
    if len(lines) < 4 or not lines[0].strip().isdigit() or lines[1] != "main":
        fail("unexpected header text layout")
    names = lines[2].split("\t")
    vals = lines[3].split("\t")
    if len(names) != len(vals):
        fail("header main: %d names, %d values" % (len(names), len(vals)))
    cats = ["main"]
    for k in range(4, len(lines) - 1):
        if lines[k - 1] == "" and lines[k] != "" and lines[k].isalpha():
            cats.append(lines[k])
    return {"category_count": int(lines[0]), "categories": cats,
            "main": dict(zip(names, vals)), "main_field_order": names}


def parse_table(seg_idx, off, size, payload, sectors):
    if len(payload) < 24:
        fail("table at %d: payload %d bytes" % (off, len(payload)))
    check_adler(payload, 20, 20, "table at %d header" % off)
    n = u32(payload, 0)
    base = u64(payload, 8)
    rest = len(payload) - 24
    if rest == 4 * n:
        footer = False
    elif rest == 4 * n + 4:
        footer = True
        check_adler(payload[24:], 4 * n, 4 * n, "table at %d entries" % off)
    else:
        fail("table at %d: %d entry bytes for %d entries" % (off, rest, n))
    ents = [u32(payload, 24 + 4 * i) for i in range(n)]
    return {"segment": seg_idx, "offset": off, "entries": n, "base_offset": base,
            "footer": footer, "sectors": sectors, "ents": ents,
            "payload": bytes(payload)}


def walk_segment(idx, name, data):
    if len(data) < SEG_HEADER + DESC:
        fail("%s: too short" % name)
    if data[:8] != SIGNATURE:
        fail("%s: bad signature" % name)
    if data[8] != 1 or u16(data, 11) != 0:
        fail("%s: bad fields start/end" % name)
    number = u16(data, 9)
    if number != idx + 1:
        fail("%s: segment number %d at position %d" % (name, number, idx + 1))
    secs = []
    off = SEG_HEADER
    seen = set()
    while True:
        if off in seen or off + DESC > len(data):
            fail("%s: bad section offset %d" % (name, off))
        seen.add(off)
        d = data[off:off + DESC]
        if adler(d[:72]) != u32(d, 72):
            fail("%s: descriptor at %d adler32 mismatch" % (name, off))
        typ = d[:16].rstrip(b"\0")
        if d[len(typ):16].strip(b"\0"):
            fail("%s: descriptor type not NUL padded" % name)
        nxt, size = u64(d, 16), u64(d, 24)
        if d[32:72] != bytes(40):
            fail("%s: descriptor padding not zero at %d" % (name, off))
        secs.append({"type": typ.decode("ascii"), "offset": off, "next": nxt,
                     "size": size})
        if typ in TERMINAL:
            if nxt != off:
                fail("%s: terminal section %s next=%d != own offset %d"
                     % (name, typ, nxt, off))
            break
        if nxt <= off or nxt > len(data):
            fail("%s: section at %d has next %d" % (name, off, nxt))
        if size != nxt - off:
            fail("%s: section %s size %d != next-offset %d"
                 % (name, typ, size, nxt - off))
        off = nxt
    return number, secs


def main(argv):
    files, raw_path, raw_bytes = [], None, None
    it = iter(argv)
    for a in it:
        if a == "--raw":
            raw_path = next(it)
        elif a == "--raw-bytes":
            raw_bytes = int(next(it))
        else:
            files.append(a)
    if not files or raw_path is None:
        print(__doc__, file=sys.stderr)
        return 2
    raw = open(raw_path, "rb").read()
    if raw_bytes is not None:
        raw = raw[:raw_bytes]

    segs = []
    volume = None
    data_copies = []
    tables = []
    table_ctx = {}
    hash_sec = None
    digest_sec = None
    error2 = []
    headers = {}
    header_counts = {}
    table2_equal = []
    obs = {"terminal_sizes": set(), "size_equals_next_minus_offset": True,
           "section_order_by_segment": [], "table2_equals_table": True,
           "base_offset_relation": set(), "table_footer": set(),
           "data_equals_volume": True, "hash_md5_equals_digest_md5": None}

    for idx, path in enumerate(files):
        data = open(path, "rb").read()
        name = path.replace("\\", "/").rsplit("/", 1)[-1]
        number, secs = walk_segment(idx, name, data)
        last = idx == len(files) - 1
        term = secs[-1]["type"]
        if last and term != "done":
            fail("%s: last segment ends with %s" % (name, term))
        if not last and term != "next":
            fail("%s: non-last segment ends with %s" % (name, term))
        obs["terminal_sizes"].add(secs[-1]["size"])
        obs["section_order_by_segment"].append([s["type"] for s in secs])
        segs.append({"file": name, "size": len(data), "number": number,
                     "sections": secs})
        cur_sectors = None
        prev_table = None
        for s in secs:
            t, off, size = s["type"], s["offset"], s["size"]
            payload = data[off + DESC:off + size] if size else b""
            if t in ("header", "header2"):
                try:
                    txt = zlib.decompress(payload)
                except zlib.error as e:
                    fail("%s: %s zlib: %s" % (name, t, e))
                headers.setdefault(t, []).append(parse_header_text(txt, t == "header2"))
                headers[t][-1]["has_bom"] = txt[:2] == b"\xff\xfe"
                header_counts[t] = header_counts.get(t, 0) + 1
            elif t in ("volume", "disk"):
                v = parse_volume(payload, "%s volume" % name)
                if volume is not None:
                    fail("second volume section")
                if idx != 0:
                    fail("volume outside segment 1")
                volume = v
                volume_type = t
            elif t == "data":
                v = parse_volume(payload, "%s data" % name)
                data_copies.append(v)
            elif t == "sectors":
                cur_sectors = {"offset": off, "size": size, "end": off + size}
            elif t == "table":
                if volume is None:
                    fail("table before volume")
                if cur_sectors is None:
                    fail("table without preceding sectors section")
                tb = parse_table(idx + 1, off, size, payload, cur_sectors)
                tables.append(tb)
                prev_table = tb
                obs["table_footer"].add(tb["footer"])
                b = tb["base_offset"]
                if b == cur_sectors["offset"]:
                    rel = "sectors_descriptor_offset"
                elif b == cur_sectors["offset"] + DESC:
                    rel = "sectors_payload_offset"
                elif b == 0:
                    rel = "zero"
                else:
                    rel = "other"
                obs["base_offset_relation"].add(rel)
            elif t == "table2":
                if prev_table is None:
                    fail("table2 without table")
                same = payload == prev_table["payload"]
                table2_equal.append(same)
                if not same:
                    obs["table2_equals_table"] = False
            elif t == "hash":
                if len(payload) != 36:
                    fail("hash payload %d bytes" % len(payload))
                check_adler(payload, 32, 32, "hash")
                hash_sec = {"md5": payload[:16].hex(),
                            "unknown_nonzero": any(payload[16:32])}
            elif t == "digest":
                if len(payload) != 80:
                    fail("digest payload %d bytes" % len(payload))
                check_adler(payload, 76, 76, "digest")
                digest_sec = {"md5": payload[:16].hex(), "sha1": payload[16:36].hex(),
                              "padding_nonzero": any(payload[36:76])}
            elif t == "error2":
                if len(payload) < 520:
                    fail("error2 payload %d bytes" % len(payload))
                check_adler(payload, 516, 516, "error2 header")
                n = u32(payload, 0)
                if len(payload) != 520 + 8 * n + 4:
                    fail("error2: %d entries in %d bytes" % (n, len(payload)))
                check_adler(payload[520:], 8 * n, 8 * n, "error2 entries")
                for i in range(n):
                    error2.append({"first_sector": u32(payload, 520 + 8 * i),
                                   "sectors": u32(payload, 524 + 8 * i)})
            elif t in ("next", "done"):
                pass
            else:
                fail("%s: unknown section type %r" % (name, t))

    if volume is None:
        fail("no volume section")
    for dc in data_copies:
        if dc != volume:
            obs["data_equals_volume"] = False
    if hash_sec and digest_sec:
        obs["hash_md5_equals_digest_md5"] = hash_sec["md5"] == digest_sec["md5"]

    bps, spc, sectors = volume["bps"], volume["spc"], volume["sectors"]
    chunk_size = bps * spc
    media_size = sectors * bps
    if chunk_size <= 0 or media_size < 0:
        fail("bad geometry")
    nchunks = -(-media_size // chunk_size)
    if volume["chunks"] != nchunks:
        fail("volume says %d chunks, geometry says %d" % (volume["chunks"], nchunks))

    # Decode every chunk.
    seg_data = [open(p, "rb").read() for p in files]
    out = bytearray()
    idx = 0
    n_comp = n_unc = 0
    first = {"compressed": None, "uncompressed": None}
    chunk_kinds = []
    comp_trailing = 0
    last_comp_ends_at_section = None
    for tb in tables:
        tb["first_chunk"] = idx
        data = seg_data[tb["segment"] - 1]
        sec = tb["sectors"]
        base = tb["base_offset"]
        locs = [(base + (e & 0x7FFFFFFF), bool(e & 0x80000000)) for e in tb["ents"]]
        tb["compressed"] = tb["uncompressed"] = 0
        for j, (pos, comp) in enumerate(locs):
            if idx >= nchunks:
                fail("more table entries than chunks")
            mlen = min(chunk_size, media_size - idx * chunk_size)
            if pos < sec["offset"] + DESC or pos >= sec["end"]:
                fail("chunk %d at %d outside its sectors section" % (idx, pos))
            if comp:
                end = locs[j + 1][0] if j + 1 < len(locs) else sec["end"]
                if end <= pos or end > sec["end"]:
                    fail("chunk %d: bad end %d" % (idx, end))
                zd = zlib.decompressobj()
                try:
                    chunk = zd.decompress(data[pos:end])
                except zlib.error as e:
                    fail("chunk %d zlib: %s" % (idx, e))
                if not zd.eof:
                    fail("chunk %d zlib stream incomplete" % idx)
                if zd.unused_data:
                    comp_trailing += len(zd.unused_data)
                if len(chunk) != mlen:
                    fail("chunk %d inflates to %d, want %d" % (idx, len(chunk), mlen))
                tb["compressed"] += 1
                n_comp += 1
                if j + 1 == len(locs):
                    last_comp_ends_at_section = (end - len(zd.unused_data) == sec["end"])
                length = end - pos - len(zd.unused_data)
            else:
                chunk = data[pos:pos + mlen]
                if len(chunk) != mlen or pos + mlen + 4 > sec["end"]:
                    fail("chunk %d raw data out of range" % idx)
                if adler(chunk) != u32(data, pos + mlen):
                    fail("chunk %d adler32 mismatch" % idx)
                tb["uncompressed"] += 1
                n_unc += 1
                length = mlen + 4
            kind = "compressed" if comp else "uncompressed"
            if first[kind] is None:
                first[kind] = {"chunk": idx, "segment": tb["segment"], "offset": pos,
                               "length": length}
            chunk_kinds.append(1 if comp else 0)
            out += chunk
            idx += 1
    if idx != nchunks:
        fail("tables cover %d of %d chunks" % (idx, nchunks))
    if len(out) != media_size:
        fail("rebuilt %d bytes, media is %d" % (len(out), media_size))
    if media_size != len(raw):
        fail("media size %d != raw size %d" % (media_size, len(raw)))
    if bytes(out) != raw:
        fail("rebuilt media differs from the raw image")

    md5 = hashlib.md5(out).hexdigest()
    sha1 = hashlib.sha1(out).hexdigest()
    sha256 = hashlib.sha256(out).hexdigest()
    if hash_sec and hash_sec["md5"] != md5:
        fail("hash section md5 != media md5")
    if digest_sec and (digest_sec["md5"] != md5 or digest_sec["sha1"] != sha1):
        fail("digest section != media hashes")

    def dump_table(t):
        return {"segment": t["segment"], "offset": t["offset"], "entries": t["entries"],
                "base_offset": t["base_offset"], "footer": t["footer"],
                "compressed": t["compressed"], "uncompressed": t["uncompressed"],
                "first_chunk": t["first_chunk"]}

    vol = dict(volume)
    vol["section_type"] = volume_type
    obs["terminal_sizes"] = sorted(obs["terminal_sizes"])
    obs["base_offset_relation"] = sorted(obs["base_offset_relation"])
    obs["table_footer"] = sorted(obs["table_footer"])
    obs["table2_count"] = len(table2_equal)
    obs["data_section_count"] = len(data_copies)
    obs["header_section_counts"] = header_counts
    obs["compressed_trailing_bytes"] = comp_trailing
    obs["last_compressed_chunk_of_table_ends_at_sectors_end"] = last_comp_ends_at_section
    doc = {
        "segments": segs,
        "volume": vol,
        "tables": [dump_table(t) for t in tables],
        "hash": {"md5": hash_sec["md5"]} if hash_sec else None,
        "digest": {"md5": digest_sec["md5"], "sha1": digest_sec["sha1"]} if digest_sec else None,
        "error2": error2,
        "header": {k: v for k, v in headers.items()},
        "chunks": {"total": nchunks, "compressed": n_comp, "uncompressed": n_unc,
                   "first_compressed": first["compressed"],
                   "first_uncompressed": first["uncompressed"],
                   "kinds": "".join("c" if k else "u" for k in chunk_kinds)},
        "media": {"size": media_size, "md5": md5, "sha1": sha1, "sha256": sha256},
        "observations": obs,
    }
    json.dump(doc, sys.stdout, indent=1)
    print()
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except Bad as e:
        print("ewf_inspect: %s" % e, file=sys.stderr)
        sys.exit(1)

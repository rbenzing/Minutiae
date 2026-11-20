"""Independent TL oracle for the Telegram fixtures of plan 4I4 (Task 5; spec 6.5, 7, 13 "Independent oracle").

    py -3 -I tools/fixtures/telegram_tl_oracle.py --self-test                 prints "ok"
    py -3 -I tools/fixtures/telegram_tl_oracle.py --decode <hex> <Type>       prints the decode as JSON

A second reader of the app's stored TL blobs, written from recon 2-3 (the wire rules: little-endian int32/int64,
IEEE-754 doubles, boxed booleans 0x997275b5/0xbc799737, the 1-byte and 0xFE+3-byte string framing with zero padding to
a multiple of 4, boxed vectors 0x1cb5c415, `flags:#` words gating optional fields bit by bit) and the constructor rows
of schemas/telegram-android/constructors.tsv, read here as data with csv. It imports nothing of the encoder, the
generator, the shared telephony helpers or the Go decoder (TestTLOracleIsIndependent). Like the Go walker it never skips: the first
object it cannot read ends the decode, and everything read before it is returned with the stop.

decode(blob, type) -> {"top", "fields", "stop", "flags", "end"}:
  top     the constructor id of the blob (None when fewer than 4 bytes or the blob is over MAX_BLOB)
  fields  the fields read, in wire order, each {"path", "kind", "start", ...}: "value" for a number, boolean or byte
          string (strings stay bytes), "len" for a secret (its bytes are never kept) and for a vector (the count),
          "id" and "cls" for a nested object, "cls" for an inline id (id32@<class>). The top object has no entry.
  stop    None, or {"kind", "path", "offset", "type", "id"}: kind is one of
            unknown-constructor  an id the table lacks for the type expected there ("path" "" at the top)
            opaque               an object whose row lists no body (its id is known, its fields are not)
            prefix               an object whose row lists its fields only up to a point (the decode ends after them)
            truncated            fewer bytes than a value, a length prefix, its padding or a declared length or count
                                 needs (spec ruling T7: a length past the end is truncated, never a cap)
            malformed            a boolean or a vector constructor that is not one of its ids
            cap                  a vector count below 0 or above MAX_VECTOR, nesting above MAX_DEPTH, more than
                                 MAX_STEPS values, a blob above MAX_BLOB
          "offset" is where the item that stopped the decode starts (the constructor id, the length prefix, the
          value); for prefix it is the end of the last listed field. "id" is the constructor id when the stop names one.
  flags   in this order when they occur: length-prefix-255 (a string whose first byte is 255, read as the long form),
          non-canonical-length (the long form for a length below 254), padding-nonzero, unknown-flag-bits (a `full`
          object whose flags words set a bit that gates no field and names no flag-only true).
  end     the offset after the last value read whole.
"""
import argparse
import csv
import json
import math
import os
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
TABLE_FILE = os.path.join(HERE, "schemas", "telegram-android", "constructors.tsv")

BOOL_TRUE, BOOL_FALSE, VECTOR = 0x997275B5, 0xBC799737, 0x1CB5C415
MAX_BLOB = 16 << 20
MAX_DEPTH = 32
MAX_VECTOR = 10000
MAX_STEPS = 1000000
FLAG_ORDER = ("length-prefix-255", "non-canonical-length", "padding-nonzero", "unknown-flag-bits")
PRIMITIVES = ("#", "i32", "i64", "dbl", "bool", "bytes", "str", "secret")


# ---------------------------------------------------------------- the table


class Item:
    """One item of a row: a value field (kind set) or a flag-only true (kind None, bit set). word/bit: the flags
    word and bit that gate it ("flags"/"flags2"), or for a flag-only true the bit it names."""

    def __init__(self, name, kind, word, bit):
        self.name, self.kind, self.word, self.bit = name, kind, word, bit


def _word(ref):
    w, n = ref.split(".")
    return {"F": "flags", "F2": "flags2"}[w], int(n)


def parse_items(text):
    """The items of a row's field column (grammar of plan 4I4-P4), read token by token."""
    items, gate = [], None
    for tok in text.split():
        if tok[0] == "[":
            gate = _word(tok[1:].rstrip(":"))
            continue
        closing = tok[-1] == "]"
        tok = tok.rstrip("]")
        name, colon, kind = tok.partition(":")
        if not colon:  # name=F.n
            name, _, ref = tok.partition("=")
            w, b = _word(ref)
            items.append(Item(name, None, w, b))
        else:
            w, b = gate if gate else (None, None)
            items.append(Item(name, kind, w, b))
        if closing:
            gate = None
    return items


class Row:
    def __init__(self, typ, cid, cls, body, items):
        self.typ, self.cid, self.cls, self.body, self.items = typ, cid, cls, body, items


_TABLE = {}


def table():
    """{id: Row} of constructors.tsv, read once."""
    if not _TABLE:
        with open(TABLE_FILE, encoding="utf-8", newline="") as fh:
            for rec in csv.DictReader(fh, delimiter="\t", quoting=csv.QUOTE_NONE):
                cid = int(rec["id"], 16)
                _TABLE[cid] = Row(rec["type"], cid, rec["class"], rec["body"], parse_items(rec["fields"] or ""))
    return _TABLE


def row_of(cid, expected):
    """The row of cid when it can stand where expected (a type name, or a class name a field names) is read."""
    r = table().get(cid)
    if r is not None and (r.typ == expected or r.cls == expected):
        return r
    return None


# ---------------------------------------------------------------- the reader


class Stopped(Exception):
    def __init__(self, kind, path, offset, typ="", cid=None):
        Exception.__init__(self, kind)
        self.stop = {"kind": kind, "path": path, "offset": offset, "type": typ, "id": cid}


class Reader:
    def __init__(self, blob):
        self.b, self.pos, self.end, self.steps = blob, 0, 0, 0
        self.fields, self.flags = [], set()

    def need(self, n, path, at):
        if len(self.b) - self.pos < n:
            raise Stopped("truncated", path, at)

    def step(self, path):
        self.steps += 1
        if self.steps > MAX_STEPS:
            raise Stopped("cap", path, self.pos)

    def u32(self, path):
        at = self.pos
        self.need(4, path, at)
        v = struct.unpack_from("<I", self.b, at)[0]
        self.pos += 4
        return v, at

    def emit(self, path, kind, at, **kw):
        self.step(path)
        entry = {"path": path, "kind": kind, "start": at}
        entry.update(kw)
        self.fields.append(entry)
        self.end = self.pos

    def string(self, path):
        """(bytes, start) of a TL string or bytes value."""
        at = self.pos
        self.need(1, path, at)
        first = self.b[at]
        if first < 254:
            n, head = first, 1
        else:
            self.need(4, path, at)
            n, head = self.b[at + 1] | self.b[at + 2] << 8 | self.b[at + 3] << 16, 4
            if first == 255:
                self.flags.add("length-prefix-255")
            if n < 254:
                self.flags.add("non-canonical-length")
        pad = (4 - (head + n) % 4) % 4
        self.need(head + n + pad, path, at)
        data = bytes(self.b[at + head:at + head + n])
        if any(self.b[at + head + n:at + head + n + pad]):
            self.flags.add("padding-nonzero")
        self.pos = at + head + n + pad
        return data, at

    def value(self, path, kind):
        """Reads one primitive of kind (not an object) and records it."""
        if kind in ("i32", "#") or kind.startswith("id32@"):
            at = self.pos
            self.need(4, path, at)
            raw = struct.unpack_from("<i" if kind != "#" else "<I", self.b, at)[0]
            self.pos += 4
            extra = {"cls": kind[5:]} if kind.startswith("id32@") else {}
            self.emit(path, "id32" if extra else kind, at, value=raw, **extra)
            return raw
        if kind == "i64" or kind.startswith("id64@"):
            at = self.pos
            self.need(8, path, at)
            raw = struct.unpack_from("<q", self.b, at)[0]
            self.pos += 8
            extra = {"cls": kind[5:]} if kind.startswith("id64@") else {}
            self.emit(path, "id64" if extra else kind, at, value=raw, **extra)
            return raw
        if kind == "dbl":
            at = self.pos
            self.need(8, path, at)
            raw = struct.unpack_from("<d", self.b, at)[0]
            self.pos += 8
            self.emit(path, kind, at, value=raw)
            return raw
        if kind == "bool":
            v, at = self.u32(path)
            if v not in (BOOL_TRUE, BOOL_FALSE):
                raise Stopped("malformed", path, at)
            self.emit(path, kind, at, value=(v == BOOL_TRUE))
            return v == BOOL_TRUE
        if kind in ("str", "bytes"):
            data, at = self.string(path)
            self.emit(path, kind, at, value=data)
            return data
        if kind == "secret":
            data, at = self.string(path)
            self.emit(path, kind, at, len=len(data))  # the bytes never leave the reader
            return None
        raise AssertionError("not a primitive: " + kind)

    def vector_head(self, path, min_elem):
        at = self.pos
        v, _ = self.u32(path)
        if v != VECTOR:
            raise Stopped("malformed", path, at)
        count_at = self.pos
        self.need(4, path, count_at)
        count = struct.unpack_from("<i", self.b, count_at)[0]
        self.pos += 4
        if count < 0 or count > MAX_VECTOR:
            raise Stopped("cap", path, count_at)
        if count * min_elem > len(self.b) - self.pos:
            raise Stopped("truncated", path, count_at)
        self.emit(path, "vector", at, len=count)
        return count

    def obj(self, path, expected, depth):
        """Reads a boxed object of the expected type at path; depth 1 is the blob's top object."""
        if depth > MAX_DEPTH:
            raise Stopped("cap", path, self.pos, expected)
        at = self.pos
        cid, _ = self.u32(path)
        if depth == 1:
            self.top = cid
        r = row_of(cid, expected)
        if r is None:
            raise Stopped("unknown-constructor", path, at, expected, cid)
        if depth > 1:
            self.emit(path, "object", at, id=cid, cls=r.cls)
        else:
            self.end = self.pos
        if r.body == "opaque":
            raise Stopped("opaque", path, at, expected, cid)
        words = {}
        for it in r.items:
            p = path + "." + it.name if path else it.name
            if it.kind is None:
                if words.get(it.word, 0) >> it.bit & 1:
                    self.emit(p, "true", self.pos, value=True)
                continue
            if it.word is not None and not (words.get(it.word, 0) >> it.bit & 1):
                continue
            if it.kind == "#":
                words[it.name] = self.value(p, "#")
                if r.body == "full":
                    self.check_bits(r, it.name, words[it.name])
                continue
            self.field(p, it.kind, depth)
        if r.body == "prefix":
            raise Stopped("prefix", path, self.pos, expected, cid)

    def check_bits(self, r, word, value):
        used = {it.bit for it in r.items if it.word == word}
        for bit in range(32):
            if value >> bit & 1 and bit not in used:
                self.flags.add("unknown-flag-bits")
                return

    def field(self, path, kind, depth):
        if kind in PRIMITIVES or kind.startswith("id32@") or kind.startswith("id64@"):
            self.value(path, kind)
        elif kind in ("vec<i32>", "vec<i64>"):
            size = 4 if kind == "vec<i32>" else 8
            n = self.vector_head(path, size)
            for i in range(n):
                self.value("%s[%d]" % (path, i), "i32" if size == 4 else "i64")
        elif kind.startswith("vec<"):
            n = self.vector_head(path, 4)
            for i in range(n):
                self.obj("%s[%d]" % (path, i), kind[4:-1], depth + 1)
        else:
            self.obj(path, kind, depth + 1)


def decode(blob, typ):
    """The decode of blob as TL type typ (module doc)."""
    blob = bytes(blob)
    rd = Reader(blob)
    rd.top = None
    stop = None
    if len(blob) > MAX_BLOB:
        stop = {"kind": "cap", "path": "", "offset": 0, "type": typ, "id": None}
    else:
        try:
            rd.obj("", typ, 1)
        except Stopped as e:
            stop = e.stop
    return {"top": rd.top, "fields": rd.fields, "stop": stop,
            "flags": [f for f in FLAG_ORDER if f in rd.flags], "end": rd.end}


def read_string(blob, at):
    """(bytes, offset after it) of one canonical-or-long TL string at blob[at:], or None when the bytes there do not
    hold one whole (the trailer of spec 6.5 is read with it)."""
    rd = Reader(bytes(blob))
    rd.pos = at
    try:
        data, _ = rd.string("trailer")
    except Stopped:
        return None
    return data, rd.pos


# ---------------------------------------------------------------- self-test and CLI


def self_test():
    def check(what, ok):
        if not ok:
            raise SystemExit("self-test: " + what)

    le = lambda v: struct.pack("<I", v & 0xFFFFFFFF)
    peer_user = le(0x59511722) + struct.pack("<q", 1000002)
    r = decode(peer_user, "Peer")
    check("peerUser", r["stop"] is None and r["top"] == 0x59511722 and r["fields"][0]["value"] == 1000002 and r["end"] == 12)
    check("wrong type", decode(peer_user, "User")["stop"]["kind"] == "unknown-constructor")
    check("short blob", decode(b"\x01\x02", "Peer")["stop"]["kind"] == "truncated" and decode(b"", "Peer")["top"] is None)
    # TL_message: flags, flags2, id, peer_id, date, message "hello"
    msg = le(0x7600B9D3) + le(0) + le(0) + le(7) + peer_user + le(1700000000) + b"\x05hello\x00\x00"
    r = decode(msg, "Message")
    check("message", r["stop"] is None and [f["path"] for f in r["fields"]] ==
          ["flags", "flags2", "id", "peer_id", "peer_id.user_id", "date", "message"] and r["end"] == len(msg))
    check("message text", r["fields"][-1]["value"] == b"hello")
    for cut in range(len(msg)):
        rc = decode(msg[:cut], "Message")
        check("cut %d" % cut, rc["stop"]["kind"] == "truncated" and rc["end"] <= cut)
    check("flag bit 12 unknown", "unknown-flag-bits" in decode(le(0x7600B9D3) + le(1 << 12) + msg[8:], "Message")["flags"])
    long255 = le(0x7600B9D3) + le(0) + le(0) + le(7) + peer_user + le(1) + b"\xff\x03\x00\x00abc\x00"
    check("prefix 255", decode(long255, "Message")["flags"] == ["length-prefix-255", "non-canonical-length"])
    check("padding", decode(msg[:-1] + b"\x07", "Message")["flags"] == ["padding-nonzero"])
    check("string past end (T7)", decode(msg[:-8] + b"\xfe\xff\xff\xff", "Message")["stop"]["kind"] == "truncated")
    ent = le(0x7600B9D3) + le(1 << 7) + le(0) + le(7) + peer_user + le(1) + b"\x00\x00\x00\x00"
    check("bad vector", decode(ent + le(0x1CB5C416) + le(0), "Message")["stop"]["kind"] == "malformed")
    check("vector cap", decode(ent + le(VECTOR) + le(0x7FFFFFFF), "Message")["stop"]["kind"] == "cap")
    check("vector past end", decode(ent + le(VECTOR) + le(3), "Message")["stop"]["kind"] == "truncated")
    old = le(0x22EB6ABA) + le(1) + le(2) + le(0x9DB1BC6D) + le(3) + le(0x12345678)
    check("bad bool", decode(old, "Message")["stop"]["kind"] == "malformed")
    # TL_messageMediaDice is opaque in the table: the decode stops at it with its id
    head = le(0x7600B9D3) + le(1 << 9) + le(0) + le(7) + peer_user + le(1) + b"\x00\x00\x00\x00"
    s = decode(head + le(0x08CBEC07), "Message")["stop"]
    check("opaque", s["kind"] == "opaque" and s["path"] == "media" and s["id"] == 0x08CBEC07)
    # TL_messageMediaEmpty reads no field: it is full with an empty field list and read whole (ruling M3)
    media = head + le(0x3DED6320)
    r = decode(media, "Message")
    check("empty media read whole", r["stop"] is None and r["end"] == len(media) and r["fields"][-1]["path"] == "media"
          and r["fields"][-1]["id"] == 0x3DED6320)
    # the depth and steps caps (Task 5 review M1), with the bounds lowered for the test: the nested peer_id object is
    # depth 2 and the fourth value read (flags, flags2, id, then peer_id)
    global MAX_DEPTH, MAX_STEPS
    saved = MAX_DEPTH, MAX_STEPS
    try:
        MAX_DEPTH = 1
        s = decode(msg, "Message")["stop"]
        check("depth cap", s["kind"] == "cap" and s["path"] == "peer_id" and s["offset"] == 16)
        MAX_DEPTH, MAX_STEPS = saved[0], 3
        r = decode(msg, "Message")
        check("steps cap", r["stop"]["kind"] == "cap" and r["stop"]["path"] == "peer_id" and len(r["fields"]) == 3)
    finally:
        MAX_DEPTH, MAX_STEPS = saved
    check("caps restored", decode(msg, "Message")["stop"] is None)
    print("ok")
    return 0


def jsonable(v):
    if isinstance(v, bytes):
        return v.hex()
    if isinstance(v, float) and not math.isfinite(v):
        return repr(v)
    if isinstance(v, dict):
        return {k: jsonable(x) for k, x in v.items()}
    if isinstance(v, list):
        return [jsonable(x) for x in v]
    return v


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--decode", nargs=2, metavar=("HEX", "TYPE"))
    a = ap.parse_args(argv)
    if a.self_test:
        return self_test()
    if a.decode:
        print(json.dumps(jsonable(decode(bytes.fromhex(a.decode[0]), a.decode[1])), indent=1))
        return 0
    ap.error("give --self-test or --decode")


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

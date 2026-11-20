"""Telegram TL encoder for the fixtures of plan 4I4 (Task 4; spec 6.5, 13 third bullet).

    py -3 -I tools/fixtures/telegram_tl_encode.py --self-test        prints "ok"

    import telegram_tl_encode as tl
    blob = tl.encode("Message", {"_": "TL_message", "id": 7, "peer_id": {"_": "TL_peerUser", "user_id": 1000002},
                                  "date": 1700000000, "message": "hello"})
    tl.field_offsets("Message", value)       # [(field, start offset)..., ("<end>", len(blob))]
    tl.trailer(path, params, fwd_msg_id)     # the app-local suffix after a stored Message (spec 6.5)

The writer of the fixtures, never a reader of the product: it encodes the wire form a stored blob has (little-endian
int32/int64, boxed bool, the canonical string framing, boxed vectors, `flags:#` words) from the constructor rows of
schemas/telegram-android/constructors.tsv, and it never reads facts.tsv. A value is {"_": <class>, <field>: ...}
(a field's dotted name is its key as written in the row, "fwd_from.from_id"):

  - A field the value does not give (or gives as None) is absent. An ungated field the value lacks is an error (no
    default is invented). A key the row does not list is an error (a typo must not pass).
  - The flags words are written from the gated fields that are present and the flag-only trues that are true, unless
    the value gives `flags` / `flags2` explicitly (an app-mutated flags word, recon 2 item 6): then exactly the
    given word is written and exactly the present fields follow, however the two disagree (a hostile blob). Gated
    fields that share one bit (views and forwards share bit 10) must be given together when the word is computed.
  - A string or bytes value is framed canonically unless it is {"_raw": <bytes>}; `_raw` replaces the whole encoding
    of any field value (an oversized length byte, a short body, a bad boolean: hostile blobs).
  - A row with body `prefix` or `opaque` encodes only the fields it lists (an opaque row: its id, then `_raw` bytes
    when the value gives them): the fixture states the rest as `_raw`.
  - `secret` fields hold key material in the app. The encoder refuses any value that does not start with
    SYNTHETIC_PREFIX, so a fixture can only ever carry obviously synthetic bytes; use synthetic_secret(n).
"""
import os
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
CONSTRUCTORS = os.path.join(HERE, "schemas", "telegram-android", "constructors.tsv")

VECTOR_ID = 0x1CB5C415
BOOL_TRUE = 0x997275B5
BOOL_FALSE = 0xBC799737
SYNTHETIC_PREFIX = b"SYNTHETIC-NOT-A-KEY-"
FLAG_WORDS = {"F": "flags", "F2": "flags2"}


def synthetic_secret(n):
    """n obviously synthetic bytes for a `secret` field."""
    return (SYNTHETIC_PREFIX * (n // len(SYNTHETIC_PREFIX) + 1))[:n]


class Field:
    """One field of a constructor row: name, kind (None for a flag-only true), the gate it sits in and the flag
    bit it sets (word name, bit)."""

    def __init__(self, name, kind, gate, flag):
        self.name, self.kind, self.gate, self.flag = name, kind, gate, flag


def parse_fields(spec):
    out, gate = [], None
    for tok in spec.split():
        if tok.startswith("[") and tok.endswith(":"):
            ref = tok[1:-1]
            word, _, bit = ref.partition(".")
            gate = (FLAG_WORDS[word], int(bit))
            continue
        closes = tok.endswith("]")
        if closes:
            tok = tok[:-1]
        if "=" in tok and ":" not in tok:
            name, _, ref = tok.partition("=")
            word, _, bit = ref.partition(".")
            out.append(Field(name, None, None, (FLAG_WORDS[word], int(bit))))
        else:
            name, _, kind = tok.partition(":")
            out.append(Field(name, kind, gate, None))
        if closes:
            gate = None
    return out


_ROWS = None


def rows():
    """[(type, id, class, body, fields)] of constructors.tsv, read once."""
    global _ROWS
    if _ROWS is None:
        with open(CONSTRUCTORS, encoding="utf-8") as fh:
            lines = fh.read().rstrip("\n").split("\n")[1:]
        _ROWS = []
        for line in lines:
            typ, cid, cls, body, _pin, _src, fields = line.split("\t")
            _ROWS.append((typ, int(cid, 16), cls, body, parse_fields(fields)))
    return _ROWS


def find_row(typ, cls):
    """The row of class cls, preferring the one of type typ (a field may name a class: TL_factCheck)."""
    hits = [r for r in rows() if r[2] == cls]
    for r in hits:
        if r[0] == typ:
            return r
    if len(hits) == 1:
        return hits[0]
    raise ValueError("no constructor row of class %r (type %r)" % (cls, typ))


def i32(v):
    if not -(1 << 31) <= v < (1 << 32):
        raise ValueError("int32 out of range: %r" % (v,))
    return struct.pack("<I", v & 0xFFFFFFFF)


def i64(v):
    if not -(1 << 63) <= v < (1 << 64):
        raise ValueError("int64 out of range: %r" % (v,))
    return struct.pack("<Q", v & 0xFFFFFFFFFFFFFFFF)


def tl_bytes(data):
    """The canonical string/bytes framing: one length byte up to 253, else 0xFE and 3 LE bytes; zero padding to a
    multiple of 4 counting the prefix."""
    n = len(data)
    head = bytes([n]) if n <= 253 else b"\xfe" + struct.pack("<I", n)[:3]
    raw = head + data
    return raw + b"\x00" * (-len(raw) % 4)


def _raw(v):
    return v["_raw"] if isinstance(v, dict) and "_raw" in v else None


def encode_kind(kind, v):
    raw = _raw(v)
    if raw is not None:
        return bytes(raw)
    if kind == "i32" or kind == "id32" or kind.startswith("id32@"):
        return i32(v)
    if kind == "i64" or kind.startswith("id64@"):
        return i64(v)
    if kind == "dbl":
        return struct.pack("<d", v)
    if kind == "bool":
        return i32(BOOL_TRUE if v else BOOL_FALSE)
    if kind == "str" or kind == "bytes":
        return tl_bytes(v.encode("utf-8") if isinstance(v, str) else bytes(v))
    if kind == "secret":
        data = bytes(v)
        if not data.startswith(SYNTHETIC_PREFIX):
            raise ValueError("a secret field takes only synthetic bytes (synthetic_secret)")
        return tl_bytes(data)
    if kind == "#":
        return i32(v)
    if kind in ("vec<i32>", "vec<i64>"):
        pack = i32 if kind == "vec<i32>" else i64
        return i32(VECTOR_ID) + i32(len(v)) + b"".join(pack(x) for x in v)
    if kind.startswith("vec<"):
        elem = kind[4:-1]
        return i32(VECTOR_ID) + i32(len(v)) + b"".join(encode(elem, x) for x in v)
    return encode(kind, v)  # an object: boxed, its constructor first


def chunks(row, value):
    """[(field name, bytes)] of the body of one constructor row, in wire order (flag-only trues write nothing)."""
    typ, _cid, cls, body, fields = row
    names = {f.name for f in fields} | {"_", "flags", "flags2"}
    for k in value:
        if k not in names:
            raise ValueError("%s has no field %r" % (cls, k))
    present = lambda f: value.get(f.name) is not None
    bits = {"flags": 0, "flags2": 0}
    shared = {}
    for f in fields:
        if f.flag and value.get(f.name):
            bits[f.flag[0]] |= 1 << f.flag[1]
        if f.gate:
            shared.setdefault(f.gate, []).append(f)
            if present(f):
                bits[f.gate[0]] |= 1 << f.gate[1]
    for (word, bit), group in shared.items():
        given = [present(f) for f in group]
        if value.get(word) is None and any(given) and not all(given):
            raise ValueError("%s: gated fields %s share %s bit %d and must be given together" % (
                cls, [f.name for f in group], word, bit))
    out = []
    for f in fields:
        if f.kind is None:
            continue
        if f.kind == "#" and f.name in bits:
            out.append((f.name, i32(bits[f.name] if value.get(f.name) is None else value[f.name])))
        elif present(f):
            out.append((f.name, encode_kind(f.kind, value[f.name])))
        elif f.gate is None:
            raise ValueError("%s: ungated field %r is not given" % (cls, f.name))
    return out


def _start(typ, value):
    """(row, bytes before the body) of a value, or (None, its raw bytes) for a {"_raw"} value."""
    raw = _raw(value)
    if raw is not None:
        return None, bytes(raw)
    row = find_row(typ, value["_"])
    return row, i32(row[1])


def encode(typ, value):
    """The boxed encoding of value as TL type typ: the constructor id (LE int32), then the body."""
    row, head = _start(typ, value)
    if row is None:
        return head
    if row[3] == "opaque":
        return head + bytes(value.get("_body", b""))
    return head + b"".join(b for _, b in chunks(row, value))


def field_offsets(typ, value):
    """[(field name, start offset)] of every field that writes bytes, the constructor id first as "<constructor>",
    then ("<end>", length of the blob): Task 5's truncation rows cut a blob at these offsets."""
    row, head = _start(typ, value)
    if row is None:
        return [("<constructor>", 0), ("<end>", len(head))]
    out, pos = [("<constructor>", 0)], len(head)
    for name, b in chunks(row, value) if row[3] != "opaque" else []:
        out.append((name, pos))
        pos += len(b)
    if row[3] == "opaque":
        pos += len(value.get("_body", b""))
    out.append(("<end>", pos))
    return out


def trailer(path, params, fwd_msg_id):
    """The app-local suffix of a stored Message (spec 6.5): one TL string (the attach path, " " when empty; with
    params "||" + repeated key|=|value|| pairs + the path), then one int32 fwd_msg_id when it is not None."""
    text = path or " "
    if params:
        pairs = params.items() if isinstance(params, dict) else params
        text = "||" + "".join("%s|=|%s||" % (k, v) for k, v in pairs) + (path or "")
    out = tl_bytes(text.encode("utf-8"))
    return out + (i32(fwd_msg_id) if fwd_msg_id is not None else b"")


def self_test():
    def eq(got, want, what):
        if got != want:
            raise SystemExit("self-test %s: got %s want %s" % (what, got.hex(), want.hex()))

    eq(encode_kind("bool", True), bytes.fromhex("b5757299"), "boolTrue")
    eq(encode_kind("str", "abc"), bytes.fromhex("03616263"), "string abc")
    eq(encode_kind("bytes", b"\x07" * 254), b"\xfe\xfe\x00\x00" + b"\x07" * 254 + b"\x00\x00", "254 bytes")
    eq(encode_kind("bytes", b"\x07" * 253), b"\xfd" + b"\x07" * 253 + b"\x00\x00", "253 bytes")
    eq(encode_kind("vec<i32>", []), bytes.fromhex("15c4b51c00000000"), "empty vector")
    eq(encode("Peer", {"_": "TL_peerUser", "user_id": 1000002}),
       bytes.fromhex("22175159") + struct.pack("<q", 1000002), "peerUser")
    eq(encode_kind("str", {"_raw": b"\xff\x01"}), b"\xff\x01", "_raw override")
    eq(trailer("", None, None), tl_bytes(b" "), "empty trailer")
    eq(trailer("p", {"k": "v"}, -5), tl_bytes(b"||k|=|v||p") + i32(-5), "params trailer")
    value = {"_": "TL_message", "flags": 0, "flags2": 0, "id": 7, "peer_id": {"_": "TL_peerUser", "user_id": 1000002},
             "date": 1700000000, "message": "hello"}
    blob = encode("Message", value)
    offs = field_offsets("Message", value)
    if offs[-1] != ("<end>", len(blob)) or offs[0] != ("<constructor>", 0):
        raise SystemExit("self-test field_offsets: %r vs blob length %d" % (offs, len(blob)))
    if [n for n, _ in offs] != ["<constructor>", "flags", "flags2", "id", "peer_id", "date", "message", "<end>"]:
        raise SystemExit("self-test field_offsets names: %r" % (offs,))
    if dict(offs)["message"] != len(blob) - 8:  # "hello": 1 length byte + 5 bytes + 2 padding
        raise SystemExit("self-test message offset: %r" % (offs,))
    # computed flags: a present gated field and a true flag-only set their bits; an explicit word is written as given
    computed = encode("Message", dict(value, flags=None, flags2=None, out=True, edit_date=5, post_author=None))
    eq(computed[4:8], i32(2 | (1 << 15)), "computed flags")
    mutated = encode("Message", dict(value, flags=0x40000000, out=True))
    eq(mutated[4:8], i32(0x40000000), "explicit flags")
    for bad in (dict(value, flags=None, views=1), dict(value, nope=1), {k: v for k, v in value.items() if k != "date"}):
        try:
            encode("Message", bad)
        except ValueError:
            continue
        raise SystemExit("self-test: an invalid value was accepted: %r" % (sorted(bad),))
    try:
        encode("PhoneCallDiscardReason", {"_": "TL_phoneCallDiscardReasonAllowGroupCall", "encrypted_key": b"real"})
    except ValueError:
        pass
    else:
        raise SystemExit("self-test: a non-synthetic secret was accepted")
    entity = encode("MessageEntity", {"_": "TL_messageEntityFormattedDate", "offset": 0, "length": 3, "date": 9,
                                       "relative": True, "day_of_week": True})
    eq(entity[4:8], i32(1 | 32), "flag-only trues")
    print("ok")
    return 0


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        sys.exit(self_test())
    raise SystemExit(__doc__)

"""Shared pieces of the iOS fixture generators (plan 4E, ruling 4E-R32).

Everything here is written from the specification text and the format notes, not from the
Go code under test: the Cocoa time conversions restate spec 7.5, the typedstream encoder
follows the layout in the doc comment of internal/decode/typedstream/typedstream.go (not the
Go test builder), and the backup path rules restate the iOS backup layout. Self-checks:
    py -3 -I tools/fixtures/ios_common.py
"""
import datetime
import hashlib
import math
import plistlib
import struct

UTC = datetime.timezone.utc
I64_MAX = 2**63 - 1
I64_MIN = -(2**63)
INT_SENTINELS = {0, -1, I64_MAX, I64_MIN}
MAX_UNIX_SECONDS = 4102444800  # 2100-01-01T00:00:00Z, exclusive
COCOA_TO_UNIX = 978307200  # seconds from 1970-01-01 to 2001-01-01
NS = 1_000_000_000
AUTO_BOUNDARY = 10**11  # |v| >= 1e11 is nanoseconds (CocoaAuto, 4E-R15)


# ---- Cocoa times -----------------------------------------------------------------------

def _unix_parts(sec, nsec):
    """(sec, nsec) since 1970 -> RFC 3339 UTC text, or None outside 1970..2100."""
    if sec < 0 or sec >= MAX_UNIX_SECONDS:
        return None
    t = datetime.datetime.fromtimestamp(sec, UTC)
    s = t.strftime("%Y-%m-%dT%H:%M:%S")
    if nsec:
        s += (".%09d" % nsec).rstrip("0")
    return s + "Z"


def cocoa_iso(v, unit):
    """RFC 3339 UTC (trailing zeros of the fraction trimmed) of a Cocoa value (unit 's' or
    'ns'), or None for a sentinel, a non-finite value or a time outside 1970..2100. A float
    is seconds only."""
    if isinstance(v, float):
        if unit != "s" or not -1e11 <= v <= 1e11 or v == 0 or v == -1:  # NaN fails the range
            return None
        whole = math.floor(v)
        ns = int(math.floor((v - whole) * NS + 0.5))
        sec = int(whole) + COCOA_TO_UNIX
        if ns >= NS:
            sec, ns = sec + 1, 0
        return _unix_parts(sec, ns)
    if v in INT_SENTINELS:
        return None
    if unit == "s":
        return _unix_parts(v + COCOA_TO_UNIX, 0)
    q, r = divmod(v, NS)
    return _unix_parts(q + COCOA_TO_UNIX, r)


def cocoa_status(v, unit):
    """'valid', 'sentinel' or 'invalid' for a Cocoa value (a non-finite float is invalid)."""
    if isinstance(v, float):
        if not math.isfinite(v) or unit != "s" or not -1e11 <= v <= 1e11:
            return "invalid"
        if v == 0 or v == -1:
            return "sentinel"
    elif v in INT_SENTINELS:
        return "sentinel"
    return "valid" if cocoa_iso(v, unit) is not None else "invalid"


def cocoa_auto(v):
    """CocoaAuto restated: (iso or None, status, unit) with unit 's', 'ns' or None (no unit
    for a sentinel or a non-finite value)."""
    if isinstance(v, float) and not math.isfinite(v):
        return None, "invalid", None
    unit = "ns" if abs(v) >= AUTO_BOUNDARY else "s"
    if isinstance(v, float) and unit == "ns":
        if v != math.trunc(v) or not I64_MIN <= v < 2**63:
            return None, "invalid", unit
        v = int(v)
    st = cocoa_status(v, unit)
    return cocoa_iso(v, unit), st, (None if st == "sentinel" else unit)


def real_cells(col, v):
    """How an oracle states a REAL value (program ruling P-4-1): a finite value as itself, a
    non-finite one only as <col>_ieee (16 lowercase hex digits of the float64 bits) with
    <col>_status 'invalid'."""
    if math.isfinite(v):
        return {col: v}
    return {col + "_ieee": "%016x" % struct.unpack(">Q", struct.pack(">d", v))[0],
            col + "_status": "invalid"}


# ---- backup paths ----------------------------------------------------------------------

def backup_file_id(domain, rel):
    """The backup file ID: hex(sha1(domain + "-" + relativePath))."""
    return hashlib.sha1((domain + "-" + rel).encode("utf-8")).hexdigest()


def backup_remote_path(udid, domain, rel):
    fid = backup_file_id(domain, rel)
    return "%s/%s/%s" % (udid, fid[:2], fid)


def canonical(p):
    """4E-R42: a Files.relativePath is usable only as stored: non-empty, no '//', no leading
    or trailing '/', no '.' or '..' element, no NUL, no backslash."""
    if not p or "\x00" in p or "\\" in p or "//" in p or p.startswith("/") or p.endswith("/"):
        return False
    return not any(e in (".", "..") for e in p.split("/"))


# ---- typedstream encoder ---------------------------------------------------------------

TS_HEADER = bytes([0x04, 0x0B]) + b"streamtyped" + bytes([0x81, 0xE8, 0x03])


def ts_int(n):
    """Typedstream integer: below 0x80 the byte itself; 0x81 + 2 bytes LE; 0x82 + 4 bytes LE."""
    if 0 <= n < 0x80:
        return bytes([n])
    if 0 <= n < 0x10000:
        return b"\x81" + struct.pack("<H", n)
    return b"\x82" + struct.pack("<I", n)


def ts_class(name):
    b = name.encode("ascii")
    return ts_int(len(b)) + b


def typedstream(text, cls="NSString", wrap=True):
    """A typedstream whose first string object holds `text` (bytes or str): the header, then
    (wrap) an NSAttributedString record, then the length-prefixed class name, the "+" type
    encoding (0x01 0x2B), the integer length and the bytes, then a terminator."""
    if isinstance(text, str):
        text = text.encode("utf-8")
    out = bytearray(TS_HEADER)
    if wrap:
        out += b"\x84\x01\x40\x84\x84\x84" + ts_class("NSAttributedString") + b"\x00"
        out += b"\x84\x84" + ts_class("NSObject") + b"\x00\x85\x92\x84\x84\x84"
    out += ts_class(cls) + b"\x01\x94\x84\x01\x2b" + ts_int(len(text)) + text + b"\x86"
    return bytes(out)


# ---- plists ----------------------------------------------------------------------------

def plist_binary(obj):
    return plistlib.dumps(obj, fmt=plistlib.FMT_BINARY, sort_keys=True)


def keyed_archive(d):
    """A minimal NSKeyedArchiver archive of a flat dict of str -> str|int|float|bool|bytes:
    $objects = [$null, root dict, class record, then key and value objects]."""
    objects = ["$null", None, {"$classname": "NSDictionary", "$classes": ["NSDictionary", "NSObject"]}]
    keys, vals = [], []
    for k in sorted(d):
        objects.append(k)
        keys.append(plistlib.UID(len(objects) - 1))
        objects.append(d[k])
        vals.append(plistlib.UID(len(objects) - 1))
    objects[1] = {"NS.keys": keys, "NS.objects": vals, "$class": plistlib.UID(2)}
    return plist_binary({"$archiver": "NSKeyedArchiver", "$version": 100000,
                         "$top": {"root": plistlib.UID(1)}, "$objects": objects})


def _selfcheck():
    assert backup_file_id("HomeDomain", "Library/SMS/sms.db") == "3d0d7e5fb2ce288813306e4d4636395e047a3d28"
    assert backup_remote_path("UDID1", "HomeDomain", "Library/SMS/sms.db") == \
        "UDID1/3d/3d0d7e5fb2ce288813306e4d4636395e047a3d28"
    cocoa = datetime.datetime(2001, 1, 1, tzinfo=UTC)
    want = (cocoa + datetime.timedelta(seconds=700000000)).strftime("%Y-%m-%dT%H:%M:%SZ")
    assert cocoa_iso(700000000, "s") == want == "2023-03-08T20:26:40Z", cocoa_iso(700000000, "s")
    assert cocoa_iso(700000000123456789, "ns") == "2023-03-08T20:26:40.123456789Z"
    assert cocoa_iso(700000000.5, "s") == "2023-03-08T20:26:40.5Z"
    for v in (0, -1, I64_MAX, I64_MIN):
        assert cocoa_iso(v, "s") is None and cocoa_status(v, "ns") == "sentinel"
    assert cocoa_iso(0.0, "s") is None and cocoa_status(-1.0, "s") == "sentinel"
    assert cocoa_iso(4_000_000_000, "s") is None and cocoa_status(4_000_000_000, "s") == "invalid"
    assert cocoa_iso(float("nan"), "s") is None and cocoa_status(float("inf"), "s") == "invalid"
    assert cocoa_iso(-COCOA_TO_UNIX - 1, "s") is None  # before 1970
    assert cocoa_iso(-COCOA_TO_UNIX, "s") == "1970-01-01T00:00:00Z"
    assert cocoa_auto(99_999_999_999) == (None, "invalid", "s")
    assert cocoa_auto(100_000_000_000) == ("2001-01-01T00:01:40Z", "valid", "ns")
    assert cocoa_auto(0) == (None, "sentinel", None)
    assert cocoa_auto(7e17) == ("2023-03-08T20:26:40Z", "valid", "ns")
    assert cocoa_auto(float("inf")) == (None, "invalid", None)
    assert real_cells("c", 1.5) == {"c": 1.5}
    assert real_cells("c", float("inf")) == {"c_ieee": "7ff0000000000000", "c_status": "invalid"}
    assert real_cells("c", float("-inf"))["c_ieee"] == "fff0000000000000"
    assert real_cells("c", float("nan"))["c_status"] == "invalid"
    assert canonical("Library/SMS/sms.db")
    for bad in ("", "a//b", "/a", "a/", "a/../b", "a/./b", "..", "a\\b", "a\x00b"):
        assert canonical(bad) is False, bad
    assert TS_HEADER == bytes.fromhex("040b73747265616d747970656481e803")
    assert typedstream(b"hi").startswith(TS_HEADER)
    assert b"\x08NSString\x01\x94\x84\x01\x2b\x02hi" in typedstream("hi")
    assert typedstream("x" * 200, cls="NSMutableString", wrap=False) == \
        TS_HEADER + b"\x0fNSMutableString\x01\x94\x84\x01\x2b\x81\xc8\x00" + b"x" * 200 + b"\x86"
    a = plistlib.loads(keyed_archive({"k": "v", "n": 3}))
    assert a["$archiver"] == "NSKeyedArchiver" and a["$top"]["root"] == plistlib.UID(1)
    root = a["$objects"][1]
    got = {a["$objects"][int(k)]: a["$objects"][int(v)] for k, v in zip(root["NS.keys"], root["NS.objects"])}
    assert got == {"k": "v", "n": 3}, got
    assert a["$objects"][int(root["$class"])]["$classname"] == "NSDictionary"
    assert plist_binary({"a": 1}).startswith(b"bplist00")
    print("ios_common self-checks OK")


if __name__ == "__main__":
    _selfcheck()

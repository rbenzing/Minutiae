"""iOS call history fixtures (plan 4E, Task 11): coredata-current, coredata-wal and legacy-call-history.

The layouts are written from memory (ruling 4E-R13): no public source defines CallHistory.storedata or
call_history.db, so these fixtures prove that the parser reads the remembered layout and nothing about a
real database. The oracle restates the "Mapping contract ios.calls" of the plan and rulings 4E-R21, R22
and R35 in Python from what this generator wrote (the engine's read-back of its own files); it shares no
code with the Go parser and takes no value from Go output.

Rules stated here (the contract, not the code):
  ZCALLRECORD: ZORIGINATED 1 -> out, 0 -> in, else unknown. Outcome: in+ZANSWERED 1 answered, in+0 missed,
  out+1 answered, out+0 unknown (raw ZANSWERED, ZDURATION and ZDISCONNECTED_CAUSE kept: it also covers
  busy, failed and not answered), anything else unknown. ZCALLTYPE 1 voice, 8 facetime_video, 16
  facetime_audio, else unknown. ZDATE is Cocoa seconds, 0 and -1 sentinels, [1970, 2100). Duration
  finite >= 0 -> duration_s, negative -> raw ZDURATION_status "negative", non-finite -> <col>_ieee plus
  <col>_status "invalid" (program ruling P-4-1). TimeEnd = Time + duration when 0 < duration <= 1e9.
  legacy call: the flags bits are not interpreted (direction and outcome unknown, raw flags kept); date
  is Unix seconds, a fractional REAL is invalid.
Unmapped columns of ZCALLRECORD: Z_ENT, Z_OPT and ZDEVICE_ID (the contract table maps no other column).
"""
import datetime
import math
import os

import ios_common as I
import telephony_common as C

FAMILY = "ios_calls"
SCHEMAS = os.path.join(C.HERE, "schemas", "ios")
REMOTE_CORE = "/private/var/mobile/Library/CallHistoryDB/CallHistory.storedata"
REMOTE_LEGACY = "/private/var/mobile/Library/CallHistory/call_history.db"
MAX_TEXT = 256 << 10
MAX_END = 10**9
T0C = 704628000  # Cocoa seconds of 2023-05-01T10:00:00Z (checked in the self-test below)
T0U = 1682935200  # the same instant as Unix seconds
# The mapped columns, in the order of the DDL snapshot; (name, required).
CORE_MAPPED = [("Z_PK", True), ("ZANSWERED", False), ("ZCALLTYPE", False), ("ZDISCONNECTED_CAUSE", False),
               ("ZFACE_TIME_DATA", False), ("ZNUMBER_AVAILABILITY", False), ("ZORIGINATED", False), ("ZREAD", False),
               ("ZJUNKCONFIDENCE", False), ("ZDATE", True), ("ZDURATION", False), ("ZADDRESS", False),
               ("ZISO_COUNTRY_CODE", False), ("ZLOCATION", False), ("ZNAME", False), ("ZSERVICE_PROVIDER", False),
               ("ZUNIQUE_ID", False)]
LEGACY_MAPPED = [("ROWID", True), ("address", False), ("date", True), ("duration", False), ("flags", False),
                 ("id", False), ("name", False), ("country_code", False), ("network_code", False), ("read", False),
                 ("assisted", False), ("face_time_data", False), ("originalAddress", False)]
CORE_RAW_INT = ["ZORIGINATED", "ZANSWERED", "ZCALLTYPE", "ZDISCONNECTED_CAUSE", "ZNUMBER_AVAILABILITY",
                "ZFACE_TIME_DATA", "ZJUNKCONFIDENCE"]
LEGACY_RAW_INT = ["flags", "id", "assisted"]
KIND = {1: "voice", 8: "facetime_video", 16: "facetime_audio"}


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def is_real(v):
    return isinstance(v, float)


def is_text(v):
    return isinstance(v, bytes) and not isinstance(v, C.Blob)


def jf(x):
    """A float as Go's encoding/json prints it: an integral value without a fraction."""
    if isinstance(x, float) and x.is_integer() and abs(x) < 1e15:
        assert not (x == 0 and math.copysign(1, x) < 0), "negative zero is not part of the scenario"
        return int(x)
    return x


def plain(s):
    ok = s and s.isascii() and s.isprintable() and len(s) <= 64 and s == s.strip() and "  " not in s
    return s if ok else None


def text(v, key, p, raw, limit=MAX_TEXT):
    """A TEXT cell through CleanText: sets p[key] when non-empty; an altered text is listed in raw.text_flags."""
    c = C.clean(v, limit)
    assert not c.truncated, "no text of the scenario reaches its cap"
    if c.flags:
        C.mark(raw, key)
    if c.text:
        p[key] = c.text
    return c.text


def address(v, p, raw):
    """ZADDRESS / address: TEXT or BLOB through CleanText; NULL absent; empty absent with raw.address_state."""
    if v is None:
        return None
    if isinstance(v, C.Blob):
        raw["address_storage"] = "blob"
    if len(v) == 0:
        raw["address_state"] = "empty"
        return None
    c = C.clean(bytes(v), MAX_TEXT)
    assert not c.truncated
    p["address"] = c.text
    if c.flags:
        raw["address_flags"] = c.flags
        raw["address_raw_b64"] = c.b64
    return c.text


def put_instant(raw, col, v, status, iso):
    if is_real(v) and not math.isfinite(v):
        raw.update(I.real_cells(col, v))
        return
    raw[col] = jf(v)
    if status != "valid":
        raw[col + "_status"] = status


def put_duration(p, raw, col, d):
    """The duration rules of the contract; returns the finite non-negative duration in seconds or None."""
    if d is None:
        return None
    if not (is_int(d) or is_real(d)):
        raw.setdefault("unknown_columns", {})[col] = "type-mismatch"
        return None
    f = float(d)
    if not math.isfinite(f):
        raw.update(I.real_cells(col, f))
        return None
    if f < 0:
        raw[col] = jf(d)
        raw[col + "_status"] = "negative"
        return None
    p["duration_s"] = jf(f)
    return f


def finish(p, raw, rowid, table, remote, file, t, t_end, direction, outcome, addr):
    if raw:
        p["raw"] = raw
    summ = None
    if addr is None:
        summ = "call %s %s" % (direction, outcome)
    elif plain(addr):
        summ = "call %s %s %s" % (direction, outcome, addr)
    return {"table": table, "rowid": rowid, "file": file, "type": "call", "source_path": remote, "time": t,
            "time_end": t_end, "times": {}, "deleted": False, "summary": summ, "body": None, "payload": p}


def end_of(t_iso, date, dur, convert):
    if t_iso is None or dur is None or not 0 < dur <= MAX_END:
        return None
    return convert(date, dur)


def expect_core(r, remote, file):
    p, raw = {}, {}
    mism = {}
    for col in CORE_RAW_INT:
        v = r.get(col)
        if is_int(v):
            raw[col] = v
        elif v is not None:
            mism[col] = "type-mismatch"
    orig, ans = r.get("ZORIGINATED"), r.get("ZANSWERED")
    direction = {1: "out", 0: "in"}.get(orig, "unknown") if is_int(orig) else "unknown"
    outcome = "unknown"
    if is_int(ans):
        outcome = {("in", 1): "answered", ("in", 0): "missed", ("out", 1): "answered"}.get((direction, ans), "unknown")
    keep_duration = direction == "out" and ans == 0  # 4E-R35: raw ZANSWERED, ZDURATION and ZDISCONNECTED_CAUSE kept
    p["direction"], p["outcome"] = direction, outcome
    addr = address(r.get("ZADDRESS"), p, raw)
    for col, key in (("ZNAME", "name_cached"), ("ZISO_COUNTRY_CODE", "country_iso"), ("ZLOCATION", "geo_description")):
        if r.get(col) is not None:
            text(r[col], key, p, raw)
    ct = r.get("ZCALLTYPE")
    if is_int(ct):
        p["kind"] = KIND.get(ct, "unknown")
    sp = r.get("ZSERVICE_PROVIDER")
    if sp is not None:
        s = text(sp, "service", p, raw)
        raw["ZSERVICE_PROVIDER"] = s
    if r.get("ZUNIQUE_ID") is not None:
        raw["ZUNIQUE_ID"] = C.clean(r["ZUNIQUE_ID"], MAX_TEXT).text
    rd = r.get("ZREAD")
    if is_int(rd) and rd in (0, 1):
        p["read"] = rd == 1
    dur = put_duration(p, raw, "ZDURATION", r.get("ZDURATION"))
    if keep_duration and "ZDURATION" not in raw and r.get("ZDURATION") is not None and dur is not None:
        raw["ZDURATION"] = jf(r["ZDURATION"])
    date, t, t_end = r.get("ZDATE"), None, None
    if is_int(date) or is_real(date):
        st = I.cocoa_status(date, "s")
        put_instant(raw, "ZDATE", date, st, None)
        if st == "valid":
            t = I.cocoa_iso(date, "s")
            if dur is not None and 0 < dur <= MAX_END:
                t_end = I.cocoa_iso(date + dur, "s")
    elif date is not None:
        mism["ZDATE"] = "type-mismatch"
    if mism:
        raw["unknown_columns"] = dict(raw.get("unknown_columns", {}), **mism)
    return finish(p, raw, r["rowid"], "ZCALLRECORD", remote, file, t, t_end, direction, outcome, addr)


def unix_status(v):
    """Unix seconds restated: 'valid', 'sentinel' or 'invalid' (a fractional REAL is invalid)."""
    if is_real(v):
        if not math.isfinite(v) or v != math.floor(v):
            return "invalid"
        v = int(v)
    if v in I.INT_SENTINELS:
        return "sentinel"
    return "valid" if 0 <= v < I.MAX_UNIX_SECONDS else "invalid"


def unix_iso(v):
    return datetime.datetime.fromtimestamp(int(v), I.UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def expect_legacy(r, remote, file):
    p, raw = {"direction": "unknown", "outcome": "unknown"}, {}  # 4E-R21: the flags bits are not interpreted
    mism = {}
    for col in LEGACY_RAW_INT:
        v = r.get(col)
        if is_int(v):
            raw[col] = v
        elif v is not None:
            mism[col] = "type-mismatch"
    addr = address(r.get("address"), p, raw)
    for col, key in (("name", "name_cached"), ("country_code", "country_iso")):
        if r.get(col) is not None:
            text(r[col], key, p, raw)
    for col in ("network_code", "originalAddress"):
        if r.get(col) is not None:
            raw[col] = C.clean(r[col], MAX_TEXT).text
    ft = r.get("face_time_data")
    if isinstance(ft, C.Blob):
        raw["face_time_data_bytes"] = len(ft)
    rd = r.get("read")
    if is_int(rd) and rd in (0, 1):
        p["read"] = rd == 1
    dur = put_duration(p, raw, "duration", r.get("duration"))
    date, t, t_end = r.get("date"), None, None
    if is_int(date) or is_real(date):
        st = unix_status(date)
        put_instant(raw, "date", date, st, None)
        if st == "valid":
            t = unix_iso(date)
            if dur is not None and 0 < dur <= MAX_END:
                t_end = unix_iso(int(date) + int(dur))
    elif date is not None:
        mism["date"] = "type-mismatch"
    if mism:
        raw["unknown_columns"] = dict(raw.get("unknown_columns", {}), **mism)
    return finish(p, raw, r["rowid"], "call", remote, file, t, t_end, "unknown", "unknown", addr)


# ---- scenario rows -----------------------------------------------------------------------

def core_row(i, **kw):
    r = {"Z_PK": i, "Z_ENT": 2, "Z_OPT": 1, "ZORIGINATED": 0, "ZANSWERED": 1, "ZCALLTYPE": 1, "ZREAD": 1,
         "ZDATE": float(T0C + 3600 * i) + 0.5, "ZDURATION": 30.0, "ZADDRESS": "+1555123%04d" % i,
         "ZSERVICE_PROVIDER": "com.apple.Telephony", "ZUNIQUE_ID": "CALL-%04d" % i}
    r.update(kw)
    return r


def cs(y, mo, d):
    return int((datetime.datetime(y, mo, d, tzinfo=I.UTC) - datetime.datetime(2001, 1, 1, tzinfo=I.UTC)).total_seconds())


def core_all():
    return [
        core_row(1, ZDURATION=125.0, ZNAME="Alice", ZISO_COUNTRY_CODE="us", ZLOCATION="California", ZNUMBER_AVAILABILITY=0),
        core_row(2, ZANSWERED=0, ZDURATION=0.0, ZREAD=0),
        core_row(3, ZORIGINATED=1, ZDURATION=60.0, ZADDRESS=b"+15551230003", ZREAD=None),
        core_row(4, ZORIGINATED=1, ZANSWERED=0, ZDURATION=0.0, ZDISCONNECTED_CAUSE=7, ZREAD=None),
        core_row(5, ZORIGINATED=1, ZANSWERED=0, ZDURATION=12.5, ZREAD=None),
        core_row(6, ZORIGINATED=2, ZANSWERED=1, ZREAD=None),
        core_row(7, ZCALLTYPE=8, ZDURATION=300.0, ZSERVICE_PROVIDER="com.apple.FaceTime", ZFACE_TIME_DATA=1),
        core_row(8, ZORIGINATED=1, ZCALLTYPE=16, ZADDRESS="user@example.com", ZSERVICE_PROVIDER="com.apple.FaceTime", ZREAD=None),
        core_row(9, ZANSWERED=0, ZCALLTYPE=99, ZDURATION=0.0, ZSERVICE_PROVIDER="com.example.voip", ZJUNKCONFIDENCE=2),
        core_row(10, ZDATE=T0C + 3600 * 10, ZADDRESS=b"+15551230010\xff\xfe"),
        core_row(11, ZDATE=T0C + 3600 * 11, ZANSWERED=0, ZADDRESS=None, ZDURATION=0.0),
        core_row(12, ZDATE=T0C + 3600 * 12, ZADDRESS=""),
        core_row(13, ZDATE=float(T0C + 3600 * 13)),  # an integral REAL: NUMERIC affinity stores an INTEGER
        core_row(14, ZDATE=0),
        core_row(15, ZDATE=cs(2100, 6, 1)),
        core_row(16, ZDATE=T0C + 3600 * 16, ZDURATION=-3.0),
        core_row(17, ZDATE=T0C + 3600 * 17, ZDURATION=float("inf")),
        core_row(18, ZDATE=T0C + 3600 * 18, ZDURATION=2e9),
        core_row(19, ZDATE=-1),
        core_row(20, ZDATE=T0C + 3600 * 20, ZDURATION=None, ZREAD=1, ZNAME="Bob é"),
    ]


def legacy_row(i, **kw):
    r = {"ROWID": i, "address": "+1555777%04d" % i, "date": T0U + 3600 * i, "duration": 30, "flags": 4, "id": 1000 + i,
         "read": 1, "assisted": 0, "country_code": "us", "network_code": "310", "originalAddress": "555777%04d" % i}
    r.update(kw)
    return r


def legacy_all():
    return [
        legacy_row(1, duration=125, flags=4, name="Alice"),
        legacy_row(2, duration=60, flags=5, read=0),
        legacy_row(3, duration=0, flags=8),
        legacy_row(4, duration=30, flags=1769472, face_time_data=b"\x01\x02\x03"),
        legacy_row(5, date=T0U + 3600 * 5 + 0.5),  # a fractional REAL date: invalid
        legacy_row(6, duration=-4),
        legacy_row(7, address=None, date=0),
        legacy_row(8, address="", duration=None, read=None),
    ]


# ---- building ----------------------------------------------------------------------------

def lit(v):
    if isinstance(v, float):
        assert math.isfinite(v), "a non-finite REAL has no SQL literal"
        return repr(v)
    if isinstance(v, (bytes, bytearray)) and not isinstance(v, C.Raw):
        return "x'%s'" % bytes(v).hex()
    return C.lit(v)


def create(work, ddl_name, table, rows, extra=None):
    path = work + "/c.db"
    ph = C.load_ddl(ddl_name, SCHEMAS)
    con = C.connect(path)
    con.executescript(ph[0])
    with C.batch(con):
        for r in rows:
            C.insert(con, table, r)
        for t, r in (extra or []):
            C.insert(con, t, r)
    return path, con


def core_extra(n):
    return [("Z_PRIMARYKEY", {"Z_ENT": 2, "Z_NAME": "CallRecord", "Z_SUPER": 0, "Z_MAX": n}),
            ("Z_METADATA", {"Z_VERSION": 1, "Z_UUID": "00000000-0000-4000-8000-000000000001",
                            "Z_PLIST": I.plist_binary({"NSStoreType": "SQLite"})})]


def columns_notes(con, table, mapped, notes):
    cols = C.columns(con, table)
    names = [n.lower() for n, _ in mapped]
    lowered = [c.lower() for c in cols]
    opt = [n for n, req in mapped if not req and n.lower() in lowered]
    v = "columns:%d" % len(cols)
    if opt:
        v += ";optional:" + ",".join(opt)
    notes["schema." + table] = v
    un = [c for c in cols if c.lower() not in names]
    if un:
        notes["unmapped_columns." + table] = ",".join(un)


def oracle(files, con, ddl_name, variant, table, wal_rows=()):
    other = "call" if table == "ZCALLRECORD" else "ZCALLRECORD"
    view = C.engine_view(files, [table])
    has_wal = "wal" in files
    remote = REMOTE_CORE if table == "ZCALLRECORD" else REMOTE_LEGACY
    fn = expect_core if table == "ZCALLRECORD" else expect_legacy
    exp = C.expect_header(FAMILY, variant, ddl_name)
    for r in view[table]:
        if r["rowid"] in wal_rows:
            exp["records"].append(fn(r, remote + "-wal", "wal"))
        else:
            exp["records"].append(fn(r, "" if has_wal else remote, "any"))
    columns_notes(con, table, CORE_MAPPED if table == "ZCALLRECORD" else LEGACY_MAPPED, exp["notes"])
    exp["notes"]["table." + other] = "absent"
    return exp


def variant_current(work):
    rows = core_all()
    path, con = create(work, "callhistory-coredata.sql", "ZCALLRECORD", rows, core_extra(len(rows)))
    files = {"db": C.read(path)}
    exp = oracle(files, con, "callhistory-coredata.sql", "coredata-current", "ZCALLRECORD")
    con.close()
    return files, exp


def variant_wal(work):
    base = [r for r in core_all() if r["Z_PK"] <= 8]
    path, con = create(work, "callhistory-coredata.sql", "ZCALLRECORD", base, core_extra(8))
    con.execute("pragma journal_mode = wal")
    con.close()
    new = [core_row(9, ZANSWERED=0, ZDURATION=0.0, ZNAME="only in the wal, a"),
           core_row(10, ZORIGINATED=1, ZDURATION=45.0, ZNAME="only in the wal, b"),
           core_row(11, ZCALLTYPE=8, ZDURATION=5.5, ZSERVICE_PROVIDER="com.apple.FaceTime", ZNAME="only in the wal, c")]
    stmts = ["pragma wal_autocheckpoint = 0"]
    for r in new:
        cols = list(r)
        stmts.append("insert into ZCALLRECORD(%s) values (%s)" % (",".join(cols), ",".join(lit(r[c]) for c in cols)))
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    sc = C.connect(work + "/schema.db")
    sc.executescript(C.load_ddl("callhistory-coredata.sql", SCHEMAS)[0])
    exp = oracle(files, sc, "callhistory-coredata.sql", "coredata-wal", "ZCALLRECORD", {r["Z_PK"] for r in new})
    sc.close()
    return files, exp


def variant_legacy(work):
    path, con = create(work, "callhistory-legacy.sql", "call", legacy_all())
    files = {"db": C.read(path)}
    exp = oracle(files, con, "callhistory-legacy.sql", "legacy-call-history", "call")
    con.close()
    return files, exp


VARIANTS = {"coredata-current": variant_current, "coredata-wal": variant_wal, "legacy-call-history": variant_legacy}


def _selfcheck():
    assert cs(2023, 5, 1) + 36000 == T0C
    assert unix_iso(T0U) == "2023-05-01T10:00:00Z" and I.cocoa_iso(T0C, "s") == "2023-05-01T10:00:00Z"
    assert unix_status(0) == "sentinel" and unix_status(1.5) == "invalid" and unix_status(-5) == "invalid"
    assert unix_status(I.MAX_UNIX_SECONDS) == "invalid" and unix_status(T0U) == "valid"
    print("ios_calls self-checks OK")


if __name__ == "__main__":
    _selfcheck()

"""Android call log fixtures: calllog.db variants and the calls inside contacts2.db."""
import re

import telephony_common as C

FAMILY = "calllog"
CLAIMED = ("calls",)  # Meta.Claims of android.calllog
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle)
REMOTE_CALLLOG = "/data/user_de/0/com.android.providers.contacts/databases/calllog.db"
REMOTE_CONTACTS = "/data/user_de/0/com.android.providers.contacts/databases/contacts2.db"
T0, MS = 1682935200, 1000
MAX_TEXT = 256 << 10
CALL_MAPPED = [("number", True), ("date", True), ("duration", True), ("type", True), ("presentation", False), ("name", False),
               ("countryiso", False), ("geocoded_location", False), ("subscription_id", False), ("features", False),
               ("is_read", False), ("new", False)]
RAW_INT = ["type", "duration", "presentation", "features", "is_read", "new"]
EXACT = 2**53  # beyond this magnitude an integer is not exactly a float64 (ruling T62)
# The runes Go's strings.TrimSpace removes (unicode.IsSpace); Python's str.strip() differs.
GO_SPACE = "".join(chr(c) for c in [9, 10, 11, 12, 13, 32, 0x85, 0xA0, 0x1680, *range(0x2000, 0x200B), 0x2028, 0x2029, 0x202F, 0x205F, 0x3000])
DECIMAL = re.compile(r"[+-]?[0-9]+", re.ASCII)  # strconv.ParseInt base 10 (ruling T63)
PRESENTATION = {1: "allowed", 2: "restricted", 3: "unknown", 4: "payphone"}


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def parse_small(txt):
    """The subscription parse of the mapper: text of at most 32 bytes, trimmed, then ParseInt; None when refused."""
    if len(txt.encode("utf-8")) > 32:
        return None
    t = txt.strip(GO_SPACE)
    return int(t) if DECIMAL.fullmatch(t) else None


def call_row(i, **kw):
    r = {"_id": i, "number": "+1555123%04d" % i, "date": (T0 + 3600 * i) * MS, "duration": 10, "type": 1, "new": 0}
    r.update(kw)
    return r


def calls_all():
    return [
        call_row(1, duration=125, name="Alice", countryiso="US", geocoded_location="California", is_read=1),
        call_row(2, type=2, duration=60),
        call_row(3, type=2, duration=0),
        call_row(4, type=3, duration=0, new=1),
        call_row(5, type=4, duration=30),
        call_row(6, type=5, duration=0),
        call_row(7, type=6, duration=0),
        call_row(8, type=7, duration=15),
        call_row(9, number="", type=3, duration=0, presentation=2),
        call_row(10, number=None, type=3, duration=0, presentation=3),
        call_row(11, type=1, presentation=4),
        call_row(12, date=4102444800000),
        call_row(13, date=0),
        call_row(14, date=-1),
        call_row(15, date=C.I64_MAX),
        call_row(16, duration=-5),
        call_row(17, type=2, duration=None),
        call_row(18, type=2, duration=30, features=1),
        call_row(19, type=99),
        call_row(20, subscription_id="89014103211118510720"),
        call_row(21, subscription_id="2"),
        call_row(22, number=C.Raw(b"+1555\xff0001")),
        call_row(23, duration=4_000_000_000),
        call_row(24, date=4102444700000, duration=200),
        call_row(25, name=C.Raw(b"Al\xffice")),
        call_row(26, subscription_id=b"\x01\x02"),
        call_row(27, duration=EXACT + 1),
        call_row(28, subscription_id=chr(0x663)),  # an Arabic-Indic digit: Python's int() accepts it, ParseInt refuses
        call_row(29, subscription_id="5_0"),  # Python's int() accepts the underscore, ParseInt refuses
        call_row(30, subscription_id=chr(0x663) * 17),  # 34 bytes: refused before any conversion
        call_row(31, subscription_id="-3"),  # parses, but a negative subscription is refused
    ]


def expect_call(r, remote, file="any"):
    """The expectation of one calls row as the engine reads it back. Every variant here is a lone
    database (no WAL, no journal), so the oracle states the db path for every row (rulings T48, T51)."""
    raw, unknown = {}, {}
    typ, dur = r.get("type"), r.get("duration")
    dur_known = is_int(dur)
    if not is_int(typ):
        direction, outcome = "unknown", "unknown"
    elif typ == 2:
        direction = "out"
        outcome = ("answered" if dur > 0 else "cancelled" if dur == 0 else "unknown") if dur_known else "unknown"
    else:
        direction, outcome = {1: ("in", "answered"), 3: ("in", "missed"), 4: ("in", "voicemail"), 5: ("in", "rejected"),
                              6: ("in", "blocked"), 7: ("in", "answered")}.get(typ, ("unknown", "unknown"))
    p = {"direction": direction, "outcome": outcome}
    num = r.get("number")
    address = None
    if num not in (None, b""):
        c = C.clean(num, MAX_TEXT)
        address = p["address"] = c.text
        if c.flags:
            raw["number_flags"] = c.flags
            if c.b64:
                raw["number_raw_b64"] = c.b64
    pres = r.get("presentation")
    if is_int(pres):
        p["address_presentation"] = PRESENTATION.get(pres, "unknown")
    for col, key in (("name", "name_cached"), ("countryiso", "country_iso"), ("geocoded_location", "geo_description")):
        v = r.get(col)
        if v is not None:
            c = C.clean(v, MAX_TEXT)
            if c.flags:
                C.mark(raw, key)
            if c.text:
                p[key] = c.text
    if dur_known and abs(dur) > EXACT:
        unknown["duration"] = "precision"  # not exactly a float64: duration_s is not stated
    elif dur_known and dur >= 0:
        p["duration_s"] = dur
    ft = r.get("features")
    if is_int(ft):
        p["kind"] = "video" if ft & 1 else "voice"
    sid = r.get("subscription_id")
    if is_int(sid):
        raw["subscription_id"] = sid
        if sid >= 0:
            p["subscription"] = sid
    elif isinstance(sid, C.Blob) or (sid is not None and not isinstance(sid, bytes)):
        unknown["subscription_id"] = "type-mismatch"  # REAL or BLOB: neither a subscription nor absent
    elif sid is not None:
        c = C.clean(sid, MAX_TEXT)
        if c.flags:
            C.mark(raw, "subscription_id")
        txt = c.text
        raw["subscription_id"] = txt
        v = parse_small(txt)
        if v is not None and 0 <= v <= C.I64_MAX:
            p["subscription"] = v
        else:
            unknown["subscription_id"] = "unparseable"  # T66: a refused text is flagged, its raw text kept
    for col, key in (("is_read", "read"), ("new", "new")):
        if is_int(r.get(col)) and r[col] in (0, 1):
            p[key] = r[col] == 1
    for n in RAW_INT:
        v = r.get(n)
        if is_int(v):
            raw[n] = v
        elif v is not None:
            unknown[n] = "type-mismatch"
    date, t, t_end = r.get("date"), None, None
    if is_int(date):
        st = C.classify(date, "ms")
        raw["date"] = date
        if st != "valid":
            raw["date_status"] = st
        else:
            t = C.iso(date, "ms")
            if dur_known and 0 < dur <= 10**9 and date + dur * 1000 < C.MAX_SECONDS * 1000:
                t_end = C.iso(date + dur * 1000, "ms")
    elif date is not None:
        unknown["date"] = "type-mismatch"
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    plain = address is None or (address.isascii() and len(address) <= 64)
    summ = ("call %s %s" % (direction, outcome) + (" " + address if address else "")) if plain else None
    return {"table": "calls", "rowid": r["rowid"], "file": file, "type": "call", "source_path": remote, "time": t, "time_end": t_end,
            "times": {}, "deleted": False, "summary": summ, "body": None, "payload": p}


def build(work, ddl_name, variant, rows, remote):
    path = work + "/c.db"
    ph = C.load_ddl(ddl_name)
    con = C.connect(path)
    con.executescript(ph[0])
    with C.batch(con):
        for r in rows:
            C.insert(con, "calls", r)
    cols = C.columns(con, "calls")
    con.close()
    files = {"db": C.read(path)}
    view = C.engine_view(files, ["calls"])
    exp = C.expect_header(FAMILY, variant, ddl_name)
    exp["records"] = [expect_call(r, remote) for r in view["calls"]]
    C.note_columns(cols, CALL_MAPPED, "calls", exp["notes"])
    return files, exp


def variant_pre(work):
    keep = {1, 2, 3, 4, 5, 8, 12, 13, 14, 15, 16, 17, 19, 22, 23, 24}
    return build(work, "calllog-pre-presentation.sql", "pre-presentation", [r for r in calls_all() if r["_id"] in keep], REMOTE_CALLLOG)


def variant_features(work):
    return build(work, "calllog-features-subid.sql", "features-subid", calls_all(), REMOTE_CALLLOG)


def variant_c2(work):
    keep = {1, 2, 3, 4, 5, 6, 7, 12, 13}
    return build(work, "calllog-in-contacts2.sql", "in-contacts2", [r for r in calls_all() if r["_id"] in keep], REMOTE_CONTACTS)


def numbered(i):
    return call_row(i, type=(1, 2, 3)[i % 3], duration=10 + i, name="Caller number %d" % i, countryiso="US",
                    geocoded_location="Somewhere %d, State" % i)


def variant_deleted(work):
    """100 calls on 1024-byte pages, secure_delete OFF: calls 21 to 80 are deleted and whole leaf pages go to the freelist.
    A lone database: a freelist is read as history only when no WAL or journal could have changed the free pages."""
    path = work + "/c.db"
    ph = C.load_ddl("calllog-features-subid.sql")
    con = C.connect(path, page_size=1024)
    con.executescript(ph[0])
    with C.batch(con):
        for i in range(1, 101):
            C.insert(con, "calls", numbered(i))
    scen = SCENARIOS["calllog-deleted"] = C.Scenario(CLAIMED)
    scen.write("calls", scen.snap(con, "calls"))
    # freed leaf pages are never owned by a table: the oracle expects basis fit, attributed to calls
    scen.want("calls", scen.snap(con, "calls", "_id between 21 and 80"), label="calls")
    con.execute("delete from calls where _id between 21 and 80")
    cols = C.columns(con, "calls")
    con.close()
    files = {"db": C.read(path)}
    view = C.engine_view(files, ["calls"])
    exp = C.expect_header(FAMILY, "calllog-deleted", "calllog-features-subid.sql")
    exp["records"] = [expect_call(r, REMOTE_CALLLOG) for r in view["calls"]]
    C.note_columns(cols, CALL_MAPPED, "calls", exp["notes"])
    return files, exp


def variant_wal_edit(work):
    """12 calls, then, in a WAL that is never checkpointed, call 3 is edited, call 7 deleted and call 13 added: the
    database file still holds the versions of calls 3 and 7 (history), the WAL holds the live ones."""
    path = work + "/c.db"
    ph = C.load_ddl("calllog-features-subid.sql")
    con = C.connect(path, page_size=1024)
    con.executescript(ph[0])
    with C.batch(con):
        for i in range(1, 13):
            C.insert(con, "calls", numbered(i))
    scen = SCENARIOS["calllog-wal-edit"] = C.Scenario(CLAIMED)
    scen.write("calls", scen.snap(con, "calls"))
    scen.want("calls", scen.snap(con, "calls", "_id in (3, 7)"))  # as the database file still holds them
    cols = C.columns(con, "calls")
    con.execute("pragma journal_mode = wal")
    con.close()
    new = numbered(13)
    stmts = ["pragma wal_autocheckpoint = 0", "update calls set duration = 999, type = 2 where _id = 3", "delete from calls where _id = 7",
             "insert into calls(%s) values (%s)" % (",".join(new), ",".join(C.lit(v) for v in new.values()))]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    view = C.engine_view(files, ["calls"])
    exp = C.expect_header(FAMILY, "calllog-wal-edit", "calllog-features-subid.sql")
    exp["records"] = [expect_call(r, "") for r in view["calls"]]  # a WAL companion: the source path follows the role of the cell
    C.note_columns(cols, CALL_MAPPED, "calls", exp["notes"])
    return files, exp


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (ruling 4G-R14)."""
    return C.recover_oracle(variant, files, SCENARIOS[variant], lambda f: None, lambda table, d, env, file: expect_call(d, "", file))


VARIANTS = {"pre-presentation": variant_pre, "features-subid": variant_features, "in-contacts2": variant_c2,
            "calllog-deleted": variant_deleted, "calllog-wal-edit": variant_wal_edit}

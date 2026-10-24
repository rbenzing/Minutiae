"""Safari History.db fixtures (plan 4F, Task 6): current, current-wal and no-tombstones.

The layout is written from memory (ruling 4F-R12): no public source defines it, so these fixtures
prove that the parser reads the remembered layout and nothing about a real database. The oracle
restates rulings 4F-R9, R13, R14, R15, R23 and R24 of the plan in Python and shares no code with the
Go parser. Cocoa seconds are converted as decode/ts documents it: the instant is
floor(v) + 978307200 seconds plus (v - floor(v)) * 1e9 nanoseconds, rounded half away from zero by
comparing the fraction with 0.5 (never Python's round), in [1970, 2100), and 0, -0 and -1 are sentinels.
"""
import datetime
import math
import os

import telephony_common as C

FAMILY = "safari"
SCHEMAS = os.path.join(C.HERE, "schemas", "safari")
REMOTE = "/private/var/mobile/Library/Safari/History.db"
MAX_URL = 2 << 20
MAX_TEXT = 256 << 10
MAX_BODY_PART = 64 << 10
COCOA_TO_UNIX = 978307200
ITEM_MAPPED = [("url", True), ("visit_count", False), ("status_code", False)]
VISIT_MAPPED = [("history_item", True), ("visit_time", True), ("title", False), ("load_successful", False),
                ("redirect_source", False), ("redirect_destination", False), ("http_non_get", False),
                ("synthesized", False), ("origin", False), ("generation", False), ("attributes", False), ("score", False)]
TOMB_MAPPED = [("start_time", True), ("end_time", True), ("url", False), ("generation", False)]
RAW_INT_VISIT = ["http_non_get", "synthesized", "origin", "generation", "attributes", "score"]
DAILY = b"\x01\x02\x03\x04"


def cs(y, mo, d, h=0, mi=0, s=0):
    """Cocoa seconds of an instant: the arithmetic of the stated UTC time, not a remembered number."""
    t = datetime.datetime(y, mo, d, h, mi, s, tzinfo=C.UTC)
    return float((t - datetime.datetime(2001, 1, 1, tzinfo=C.UTC)).total_seconds())


T_VISIT = cs(2023, 5, 1, 10, 0, 0)  # 2023-05-01T10:00:00Z
NS_PROBE = 704714400.123456789  # stored as the nearest float64; the Go test converts it with ts.CocoaSeconds


def is_num(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def jf(x):
    """A float as Go's encoding/json prints it: an integral value without a fraction."""
    if isinstance(x, float):
        assert not (x == 0 and math.copysign(1, x) < 0), "negative zero is not part of the scenario"
        if x.is_integer() and abs(x) < 1e15:
            return int(x)
    return x


def iso_ns(sec, ns):
    t = C.EPOCH + datetime.timedelta(seconds=sec)
    s = t.strftime("%Y-%m-%dT%H:%M:%S")
    if ns:
        s += (".%09d" % ns).rstrip("0")
    return s + "Z"


def cocoa(v):
    """(status, iso): ts.ClassifyFloat/CocoaSeconds restated."""
    if not (-1e11 <= v <= 1e11):
        return "invalid", None
    if v == 0 or v == -1:
        return "sentinel", None
    whole = math.floor(v)
    frac = (v - whole) * 1e9
    fl = math.floor(frac)
    ns = int(fl) + 1 if frac - fl >= 0.5 else int(fl)  # half away from zero (frac is never negative)
    sec = int(whole) + COCOA_TO_UNIX
    if ns >= 10**9:
        sec, ns = sec + 1, 0
    if not (0 <= sec < C.MAX_SECONDS):
        return "invalid", None
    return "valid", iso_ns(sec, ns)


def text(b, field, raw, limit):
    """common.CleanText restated: the cleaned text; the field is listed in raw.text_flags and the original kept when altered."""
    c = C.clean(b, limit)
    assert not c.truncated, "no text of the scenario reaches its cap"
    if c.flags:
        C.mark(raw, field)
        if c.b64:
            raw[field + "_raw_b64"] = c.b64
        else:
            raw[field + "_raw_sha256"], raw[field + "_raw_len"] = c.sha, c.rawlen
    return c.text


def plain(s):
    """A summary the oracle states: printable ASCII, one line, no doubled spaces, within 80 characters."""
    ok = s and s.isascii() and s.isprintable() and len(s) <= 80 and s == s.strip() and "  " not in s
    return s if ok else None


def cut(s, limit):
    b = s.encode("utf-8")
    if len(b) <= limit:
        return s
    n = limit
    while n > 0 and (b[n] & 0xC0) == 0x80:
        n -= 1
    return b[:n].decode("utf-8")


def body_of(url, title, raw):
    parts = [url] + ([title] if title else [])
    kept = [cut(p, MAX_BODY_PART) for p in parts]
    if kept != parts:
        raw["body_truncated"] = True
        raw["body_total_bytes"] = len("\n".join(parts).encode("utf-8"))
    return "\n".join(kept)


def item_facts(item, p, raw, put_unknown):
    """visit_count and status_code of a history_items row (4F-R8, R24)."""
    vc = item["visit_count"]
    if is_int(vc) and vc >= 0:
        p["url_visit_count"] = vc
    elif is_int(vc):
        put_unknown("visit_count", "negative")
    elif vc is not None:
        put_unknown("visit_count", "type-mismatch")
    sc = item["status_code"]
    if is_int(sc) and sc != 0:
        p["status_code"] = sc
    elif is_int(sc):
        raw["status_code"] = 0
    elif sc is not None:
        put_unknown("status_code", "type-mismatch")


def finish(table, row, file, remote, p, raw, unk, joined, t, times, deleted):
    if unk:
        raw["unknown_columns"] = unk
    if joined:
        raw["joined_unknown_columns"] = joined
    if raw:
        p["raw"] = raw
    summ = plain(p.get("title") or p["url"])  # web.VisitSummary: the title, else the URL; stated only when plain
    return {"table": table, "rowid": row["rowid"], "file": file, "type": "web_visit", "source_path": remote, "time": t,
            "time_end": None, "times": times, "deleted": deleted, "summary": summ, "body": p.pop("_body"), "payload": p}


def expect_item(item, remote, file):
    """A history_items row without any visit (4F-R9): no time, no visit id."""
    raw, unk = {"visitless": True}, {}
    p = {"browser": "safari"}
    p["url"] = text(item["url"], "url", raw, MAX_URL)
    item_facts(item, p, raw, lambda k, v: unk.__setitem__(k, v))
    p["_body"] = body_of(p["url"], "", raw)
    return finish("history_items", item, file, remote, p, raw, unk, {}, None, {}, False)


def expect_visit(v, items, remote, file):
    """(record, warning reason): a history_visits row joined to its history_items row (4F-R4, R24)."""
    if not is_int(v["history_item"]):
        return None, "history_item is not an integer"
    item = items.get(v["history_item"])
    if item is None:
        return None, "history_item %d has no history_items row" % v["history_item"]
    raw, unk, joined = {}, {}, {}
    p = {"browser": "safari"}
    p["url"] = text(item["url"], "url", raw, MAX_URL)
    title = text(v["title"], "title", raw, MAX_TEXT) if isinstance(v["title"], bytes) and not isinstance(v["title"], C.Blob) else ""
    if title:
        p["title"] = title
    p["visit_id"] = str(v["rowid"])
    item_facts(item, p, raw, lambda k, val: joined.__setitem__("history_items." + k, val))
    t = None
    vt = v["visit_time"]
    if is_num(vt):
        vt = float(vt)
        st, iso = cocoa(vt)
        raw["visit_time"] = jf(vt)
        if st == "valid":
            t = iso
        else:
            raw["visit_time_status"] = st
    elif vt is not None:
        unk["visit_time"] = "type-mismatch"
    ls = v["load_successful"]
    if is_int(ls) and ls in (0, 1):
        p["load_successful"] = ls == 1
    elif is_int(ls):
        unk["load_successful"] = "not-boolean"
    elif ls is not None:
        unk["load_successful"] = "type-mismatch"
    for col, key in (("redirect_source", "source_visit_id"), ("redirect_destination", "destination_visit_id")):
        val = v[col]
        if is_int(val):
            p.setdefault("redirect", {})[key] = str(val)
            raw[col] = val
        elif val is not None:
            unk[col] = "type-mismatch"
    for col in RAW_INT_VISIT:
        val = v[col]
        if is_int(val):
            raw[col] = val
        elif val is not None:
            unk[col] = "type-mismatch"
    p["_body"] = body_of(p["url"], title, raw)
    return finish("history_visits", v, file, remote, p, raw, unk, joined, t, {}, False), None


def expect_tombstone(tb, remote, file):
    """A history_tombstones row (4F-R13): None when it has no usable url (counted in a note)."""
    url = tb["url"]
    if not isinstance(url, bytes) or isinstance(url, C.Blob) or not url:
        return None
    raw, unk = {}, {}
    p = {"browser": "safari", "deleted": {"source": "history_tombstones"}}
    p["url"] = text(url, "url", raw, MAX_URL)
    times = {}
    for col, kind in (("start_time", "deleted_range_start"), ("end_time", "deleted_range_end")):
        val = tb[col]
        if is_num(val):
            val = float(val)
            st, iso = cocoa(val)
            raw[col] = jf(val)
            if st == "valid":
                times[kind] = iso
            else:
                raw[col + "_status"] = st
        elif val is not None:
            unk[col] = "type-mismatch"
    g = tb["generation"]
    if is_int(g):
        raw["generation"] = g
    elif g is not None:
        unk["generation"] = "type-mismatch"
    p["_body"] = body_of(p["url"], "", raw)
    return finish("history_tombstones", tb, file, remote, p, raw, unk, {}, None, times, True)


def columns_notes(con, table, mapped, notes):
    """The notes rowrun writes (schema.<t>, unmapped_columns.<t>); the rowid alias id is never unmapped."""
    cols = C.columns(con, table)
    names = [n.lower() for n, _ in mapped]
    opt = [n for n, req in mapped if not req and n in cols]
    v = "columns:%d" % len(cols)
    if opt:
        v += ";optional:" + ",".join(opt)
    notes["schema." + table] = v
    un = [c for c in cols if c.lower() not in names and c != "id"]
    if un:
        notes["unmapped_columns." + table] = ",".join(un)


def item_row(i, url, vc, status=0, **kw):
    r = {"id": i, "url": url, "visit_count": vc, "daily_visit_counts": DAILY, "should_recompute_derived_visit_counts": 0,
         "visit_count_score": 0, "status_code": status}
    r.update(kw)
    return r


def visit_row(i, item, t, title=None, **kw):
    r = {"id": i, "history_item": item, "visit_time": t, "title": title}
    r.update(kw)
    return r


def items_all():
    return [
        item_row(1, "https://www.example.org/", 3, 200),
        item_row(2, "https://visitless.example/", 1, 404),
        item_row(3, C.Raw(b"https://invalid.example/\xff"), 1),
        item_row(4, "https://zero-status.example/", 2, 0),
        item_row(5, "data:text/plain;base64," + "A" * (100 * 1024 - 23), 1),
        item_row(6, "https://real-count.example/", 2.5),
    ]


def visits_all():
    return [
        visit_row(1, 1, T_VISIT + 0.5, "Example Domain", load_successful=1),
        visit_row(2, 1, T_VISIT + 60, "Redirect source", redirect_destination=3),
        visit_row(3, 1, T_VISIT + 61, "Redirect target", redirect_source=2),
        visit_row(4, 1, T_VISIT + 100, "Failed load", load_successful=0),
        visit_row(5, 1, T_VISIT + 200, "Posted form", http_non_get=1),
        visit_row(6, 1, 0.0, "Sentinel zero"),
        visit_row(7, 1, -1.0, "Sentinel minus one"),
        visit_row(8, 1, 4e9, "After 2100"),
        visit_row(9, 1, NS_PROBE, "Nanosecond rounding"),
        visit_row(10, 999, T_VISIT + 300, "Dangling item"),
        visit_row(11, 1, T_VISIT + 400, "Extra columns", synthesized=1, origin=1, generation=7, attributes=2, score=5),
        visit_row(12, 1, "yesterday", "Text visit time"),
        visit_row(13, 1, T_VISIT + 500, None),
        visit_row(14, 5, T_VISIT + 600, None),
        visit_row(15, 6, T_VISIT + 700, "Real visit count"),
        visit_row(16, 1, T_VISIT + 800, "Not a boolean", load_successful=2),
        visit_row(17, 1, T_VISIT + 900, C.Raw(b"Caf\xe9 \x00title")),
    ]


def tombstones_all():
    return [
        {"id": 1, "start_time": T_VISIT - 20000, "end_time": T_VISIT - 100, "url": "https://deleted.example/", "generation": 1},
        {"id": 2, "start_time": T_VISIT - 20000, "end_time": 0.0, "url": "https://half.example/"},
        {"id": 3, "start_time": T_VISIT - 20000, "end_time": T_VISIT - 100, "url": None},
        {"id": 4, "start_time": T_VISIT - 20000, "end_time": T_VISIT - 100, "url": ""},
        {"id": 5, "start_time": 4e9, "end_time": T_VISIT - 100, "url": "https://late-start.example/", "generation": 2},
    ]


def lit(v):
    """A SQL literal for the statements a killed child runs."""
    if isinstance(v, float):
        assert math.isfinite(v), "a non-finite REAL has no SQL literal"
        return repr(v)
    if isinstance(v, (bytes, bytearray)) and not isinstance(v, C.Raw):
        return "x'%s'" % bytes(v).hex()
    return C.lit(v)


def create(work, ddl_name, items, visits, tombs):
    path = work + "/h.db"
    con = C.connect(path)
    con.executescript(C.load_ddl(ddl_name, SCHEMAS)[0])
    with C.batch(con):
        for t, rows in (("history_items", items), ("history_visits", visits), ("history_tombstones", tombs)):
            for r in rows:
                C.insert(con, t, r)
    return path, con


def oracle(files, con, ddl_name, variant, wal_rows=()):
    """The expectation of the engine's view of files; wal_rows is {(table, rowid)} that exist only in the -wal."""
    view = C.engine_view(files, ["history_items", "history_visits", "history_tombstones"])
    has_wal = "wal" in files

    def where(table, rowid):
        if (table, rowid) in wal_rows:
            return "wal", REMOTE + "-wal"
        return "any", ("" if has_wal else REMOTE)

    items = {r["rowid"]: r for r in view["history_items"]}
    seen = {v["history_item"] for v in view["history_visits"] if is_int(v["history_item"])}
    exp = C.expect_header(FAMILY, variant, ddl_name)
    for it in view["history_items"]:
        if it["rowid"] not in seen:
            f, sp = where("history_items", it["rowid"])
            exp["records"].append(expect_item(it, sp, f))
    unmapped = 0
    for v in view["history_visits"]:
        f, sp = where("history_visits", v["rowid"])
        rec, why = expect_visit(v, items, sp, f)
        if rec is None:
            unmapped += 1
            exp["warnings"].append({"table": "history_visits", "rowid": v["rowid"], "contains": "history_item"})
        else:
            exp["records"].append(rec)
    notes = exp["notes"]
    columns_notes(con, "history_items", ITEM_MAPPED, notes)
    columns_notes(con, "history_visits", VISIT_MAPPED, notes)
    if unmapped:
        notes["unmapped_rows.history_visits"] = str(unmapped)
    if "history_tombstones" in view:
        without = 0
        for tb in view["history_tombstones"]:
            f, sp = where("history_tombstones", tb["rowid"])
            rec = expect_tombstone(tb, sp, f)
            if rec is None:
                without += 1
            else:
                exp["records"].append(rec)
        columns_notes(con, "history_tombstones", TOMB_MAPPED, notes)
        if without:
            notes["tombstones_without_url"] = str(without)
    else:
        notes["table.history_tombstones"] = "absent"
    return exp


def variant_current(work):
    path, con = create(work, "safari-current.sql", items_all(), visits_all(), tombstones_all())
    files = {"db": C.read(path)}
    exp = oracle(files, con, "safari-current.sql", "current")
    con.close()
    return files, exp


def variant_wal(work):
    path, con = create(work, "safari-current.sql", items_all(), visits_all(), tombstones_all())
    con.execute("pragma journal_mode = wal")
    base_visits = len(visits_all())
    new_visits = [visit_row(base_visits + 1 + i, 1, T_VISIT + 5000 + i, "only in the wal %s" % "abc"[i]) for i in range(3)]
    new_tomb = {"id": len(tombstones_all()) + 1, "start_time": T_VISIT + 1000, "end_time": T_VISIT + 2000,
                "url": "https://wal-deleted.example/", "generation": 3}
    stmts = ["pragma wal_autocheckpoint = 0"]
    for table, r in [("history_visits", v) for v in new_visits] + [("history_tombstones", new_tomb)]:
        cols = list(r)
        stmts.append("insert into %s(%s) values (%s)" % (table, ",".join(cols), ",".join(lit(r[c]) for c in cols)))
    con.close()
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    con = C.connect(work + "/schema.db")
    con.executescript(C.load_ddl("safari-current.sql", SCHEMAS)[0])
    wal_rows = {("history_visits", v["id"]) for v in new_visits} | {("history_tombstones", new_tomb["id"])}
    exp = oracle(files, con, "safari-current.sql", "current-wal", wal_rows)
    con.close()
    return files, exp


def variant_no_tombstones(work):
    items = [r for r in items_all() if r["id"] in (1, 2, 4)]
    visits = [r for r in visits_all() if r["id"] in (1, 2, 3, 4)]
    path, con = create(work, "safari-no-tombstones.sql", items, visits, [])
    files = {"db": C.read(path)}
    exp = oracle(files, con, "safari-no-tombstones.sql", "no-tombstones")
    con.close()
    return files, exp


VARIANTS = {"current": variant_current, "current-wal": variant_wal, "no-tombstones": variant_no_tombstones}

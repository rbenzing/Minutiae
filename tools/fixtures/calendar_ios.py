"""iOS calendar store (Calendar.sqlitedb) fixtures: the from-memory snapshot, the CORE-iOS scenario, the literal oracle
(plan 4I3, Task 7; spec 8, 11, ruling 3).

    py -3 -B -I tools/fixtures/telephony_gen.py --family calendar-ios --out internal/parsers/ioscalendar/testdata \\
        --pins-go internal/parsers/ioscalendar/pins_test.go --pins-package ioscalendar_test
    py -3 -B -I tools/fixtures/telephony_gen.py --family calendar-ios --recover-oracle --out internal/parsers/ioscalendar/testdata

The databases are built from tools/fixtures/schemas/calendar-ios/calendar.sql, which is written FROM MEMORY (verdict
no-public-source, CROSSCHECK.md there): every table, column, type, join column, the entity_type values, the _float
marker, the Cocoa-seconds storage and the all-day midnight convention are memory, so these fixtures prove that the
parser reads the remembered layout and nothing about a real database. The oracle reads the stored values back from the
engine on a COPY of the finished files and states the expected parser output literally, from spec 5, 6.2, 6.4 and 8.2
of plan 4I3, the builder-check rulings B1, B2, B5, B6 and B8, and the plan's state vocabularies. It is a second reading
of the spec, not independent of the design: it shares no code with the Go parser, but both follow the same text. A
branch the scenario never reaches is not restated: the oracle stops with an AssertionError there instead of guessing.

Time encoding of the oracle (the expect.json "time", "time_end" and "times" strings), as calendar_android.py: a utc
instant ends in "Z"; a local-unknown reading (an all-day date at 00:00 "as if UTC", or a floating wall-clock reading)
has no zone suffix.

Text is read by go_text, ident, free_text and summarize, imported from calendar_android.py (go_text: Task 6 ruling): each byte that starts no valid UTF-8
sequence becomes its own U+FFFD, exactly as the parser's validText; never Python's errors="replace".

Placement (the Go tests bundle each variant; the oracle holds file "any" rows, so the source path follows the role):
calendar-ios, calendar-ios-noentitytype, calendar-ios-oldlayout and calendar-ios-deleted as an iOS backup file of
HomeDomain at Library/Calendar/Calendar.sqlitedb (parsertest BundleIOSBackup), calendar-ios-extract as an extraction at
/private/var/mobile/Library/Calendar/Calendar.sqlitedb with logical path ios:HomeDomain/Library/Calendar/Calendar.sqlitedb
(parsertest BundleIOS).

Every value here is fictional (example.test addresses, invented titles).
"""
import hashlib
import json
import math
import os

import ios_common as I
import telephony_common as C
import calendar_android as A
from calendar_android import go_text, summarize

FAMILY = "calendar-ios"
CLAIMED = ("CalendarItem",)  # Meta.Claims of ios.calendar (spec 10)
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle)
SCHEMAS = os.path.join(C.HERE, "schemas", "calendar-ios")
DDL = "calendar.sql"
DOMAIN = "HomeDomain"
REL = "Library/Calendar/Calendar.sqlitedb"
EXTRACT_REMOTE = "/private/var/mobile/Library/Calendar/Calendar.sqlitedb"

D = 721692800  # Cocoa seconds of 2023-11-14T22:13:20Z
M = 721699200  # Cocoa seconds of 2023-11-15T00:00:00Z
H = 3600
DAY = 86400
FLOAT = b"_float"  # the floating-time marker (from memory)
NOT_EVENT = "CalendarItem row is not an event (entity_type 3)"  # spec 8.2, Q11 (4I3-P10)

MAX_TITLE = 4 << 10  # title, location text and address (spec 9)
MAX_BODY = 256 << 10  # the description (spec 4.2)
MAX_ZONE = 256  # zone names (spec 4.1)
MAX_ID = 1 << 10  # uid, sync id and participant addresses (spec 9); url too, from the plan's Task 10 url_state too-long
MAX_LIST = 1000

# The tables the mapping reads (spec 8.1 and 8.2); every other base table is named in the note unmapped_tables.
MAPPED_TABLES = ("CalendarItem", "Location", "Participant", "Identity", "Calendar", "Store", "Recurrence",
                 "ExceptionDate", "Alarm")
JOIN_TABLES = MAPPED_TABLES[1:]
# Columns kept in raw exactly as stored when the cell is not NULL (the plan's "As stored" list for ios.calendar).
AS_STORED = ("hidden", "has_recurrences", "has_attendees", "invitation_status", "sequence_num", "birthday_id")
# The coded columns kept as "unknown" with the stored code (Q1): column -> payload field.
CODES = (("status", "status"), ("availability", "availability"), ("privacy_level", "privacy"))


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def is_real(v):
    return isinstance(v, float)


def is_text(v):
    return isinstance(v, bytes) and not isinstance(v, C.Blob)


def jf(x):
    """A float as Go's encoding/json prints it: an integral value without a fraction (the REAL columns hold integral
    Cocoa seconds; the oracle compares numbers as their JSON text)."""
    if isinstance(x, float):
        assert math.isfinite(x), "a non-finite REAL is not part of the scenario (not restated)"
        if x.is_integer() and abs(x) < 1e15:
            assert not (x == 0 and math.copysign(1, x) < 0), "negative zero is not part of the scenario"
            return int(x)
    return x


def jv(v):
    """A stored value as raw JSON: TEXT as its text, a BLOB as {"blob_b64": ...}, a REAL as Go prints it."""
    if isinstance(v, C.Blob):
        return C.jv(bytes(v))
    if isinstance(v, bytes):
        return go_text(v)
    return jf(v)


def put_entry(entry, key, v):
    """A stored value in a child entry (level 4: scalars only, B8): a BLOB is the sibling scalar <key>_blob_b64."""
    j = jv(v)
    if isinstance(j, dict):
        entry[key + "_blob_b64"] = j["blob_b64"]
    else:
        entry[key] = j
    return entry


def entry_of(row, cols):
    e = {}
    for k in cols:
        if row.get(k) is not None:
            put_entry(e, k, row[k])
    return e


def capped(items, key, holder):
    if len(items) > MAX_LIST:
        holder[key + "_total"], holder[key + "_truncated"] = len(items), True
        return items[:MAX_LIST]
    return items


def local(iso):
    """A local-unknown reading: the Cocoa reading "as if UTC" without a zone suffix (module doc)."""
    return iso[:-1]


def date_of(v):
    return I.cocoa_iso(v, "s")[:10]


def midnight(v):
    """An integral number of days from 2001-01-01, a UTC midnight (spec 5.2, from memory for iOS)."""
    return float(v).is_integer() and int(v) % DAY == 0


# ---------------------------------------------------------------- text helpers (shared with calendar_android.py)


def _retable(src, unknown, table):
    """The unknown_columns keys the Android helpers write as "Events.<col>", under this table's name."""
    for k, v in src.items():
        assert k.startswith("Events."), k
        unknown[table + k[len("Events"):]] = v


def ident(raw, unknown, col, v, limit, table="CalendarItem"):
    """calendar_android.ident (an identifier as stored, never cut, spec 9) with the iOS table name."""
    u = {}
    out = A.ident(raw, u, col, v, limit)
    _retable(u, unknown, table)
    return out


def free_text(raw, tf, field, v, limit, truncated, unknown, col, table="CalendarItem"):
    """calendar_android.free_text (a cleaned free text, C4) with the iOS table name."""
    u = {}
    out = A.free_text(raw, tf, field, v, limit, truncated, u, col)
    _retable(u, unknown, table)
    return out


def plain_text(v, limit, what):
    """A text the scenario stores clean (no flag, no cut): its CleanText result; anything else is not restated."""
    c = C.clean(v, limit)
    assert not c.flags, what + ": an altered or cut text is not restated"
    return c.text


# ---------------------------------------------------------------- the event oracle


class Env:
    """The live view of the join tables; None in the typed recovery pass (B2: a recovered row joins nothing)."""

    def __init__(self, view):
        for t in JOIN_TABLES:
            assert t in view, "a join table absent from the store is not part of the scenario (not restated)"
        self.rows = {t: {r["rowid"]: r for r in view[t]} for t in JOIN_TABLES}
        self.by = {}
        for t, col in (("Participant", "item_id"), ("Recurrence", "owner_id"), ("ExceptionDate", "owner_id"),
                       ("Alarm", "calendaritem_owner_id")):
            m = {}
            for r in view[t]:
                m.setdefault(r.get(col), []).append(r)
            self.by[t] = {k: sorted(v, key=lambda x: x["rowid"]) for k, v in m.items()}

    def match(self, table, key):
        return self.by[table].get(key, [])


def instant(raw, col, v, unknown):
    """cellx.NumberCell + CocoaSeconds + Instant.Record restated (spec 5.4): (value, iso) or (value, None). An
    INTEGER or REAL cell is recorded in raw with its status when not valid; NULL -> raw null; another type ->
    unknown_columns, nothing in raw."""
    if v is None:
        raw[col] = None
        return None, None
    if not (is_int(v) or is_real(v)):
        unknown["CalendarItem." + col] = "type-mismatch"
        return None, None
    raw[col] = jf(v)
    st = I.cocoa_status(v, "s")
    if st != "valid":
        raw[col + "_status"] = st
        return v, None
    return v, I.cocoa_iso(v, "s")


def zone_class(raw, unknown, col, z, state_prefix):
    """The timing class a zone column names when all_day is 0 (spec 8.2): floating for _float, timed for another
    non-empty text of at most 256 bytes, else None with the reason (start-tz-absent, -too-long, -unreadable)."""
    if z is None or (is_text(z) and z == b""):
        return None, state_prefix + "-absent"
    if not is_text(z):
        unknown["CalendarItem." + col] = "type-mismatch"
        return None, state_prefix + "-unreadable"
    if z == FLOAT:
        return "floating", ""
    if len(go_text(z).encode("utf-8")) > MAX_ZONE:
        return None, state_prefix + "-too-long"
    return "timed", ""


def times_of(r, raw, unknown):
    """Spec 5, 8.2, B1, B5, B6: (timing, start, end, start_date, end_date, times)."""
    ad = r.get("all_day")
    stz, etz = r.get("start_tz"), r.get("end_tz")
    if is_int(ad) and ad == 1:
        timing = "all_day"
    elif is_int(ad) and ad == 0:
        timing, why = zone_class(raw, unknown, "start_tz", stz, "start-tz")
        if timing is None:
            timing = "unknown"
            raw["timing_state"] = why
    else:
        timing = "unknown"
        raw["timing_state"] = "all-day-undecided"
        raw["all_day"] = jv(ad)
        if ad is not None and not is_int(ad):
            unknown["CalendarItem.all_day"] = "type-mismatch"
    for col, z in (("start_tz", stz), ("end_tz", etz)):
        if z == FLOAT:
            raw[col] = "_float"  # the marker is raw only, never a zone name
    sv, siso = instant(raw, "start_date", r.get("start_date"), unknown)
    ev, eiso = instant(raw, "end_date", r.get("end_date"), unknown)
    start = end = sdate = edate = None
    if timing != "unknown" and siso is not None:
        if timing == "timed":
            start = siso
        elif timing == "floating":
            start = local(siso)
        elif midnight(sv):
            start, sdate = local(siso), date_of(sv)
        else:
            raw["start_state"] = "all-day-not-midnight"
    if timing != "unknown":
        if start is None:
            raw["end_state"] = "no-start"  # B5, before any other end rule
        else:
            end_class = "all_day" if timing == "all_day" else zone_class(raw, {}, "end_tz", etz, "end-tz")[0]
            if end_class != timing:
                raw["end_state"] = "timing-mismatch"  # B6: also an end_tz whose class is undecidable
            elif eiso is not None:
                if ev < sv:
                    raw["end_state"] = "end-before-start"
                elif timing == "timed":
                    end = eiso
                elif timing == "floating":
                    end = local(eiso)
                elif midnight(ev):
                    end, edate = local(eiso), date_of(ev)
                else:
                    raw["end_state"] = "all-day-not-midnight"  # B1: no TimeEnd, no end_date
            elif r.get("end_date") is None:
                raw["end_state"] = "none-stored"
    times = {}
    for col, key in (("creation_date", "created"), ("last_modified", "modified")):
        if r.get(col) is not None:
            _, iso = instant(raw, col, r[col], unknown)
            if iso is not None:
                times[key] = iso
    return timing, start, end, sdate, edate, times


def get_join(raw, unknown, env, state_key, table, key_col, key):
    """A Get join by a key column (spec 8.2): the row, or None with the state stated (not-joined-recovered in the typed
    pass, no-<Table>-row for a key naming no row). A NULL key joins nothing and states nothing."""
    if key is None:
        return None
    if not is_int(key):
        raise AssertionError("an undecidable join key is not part of the scenario (not restated): " + key_col)
    if env is None:
        raw[state_key] = "not-joined-recovered"
        return None
    row = env.rows[table].get(key)
    if row is None:
        raw[state_key] = "no-%s-row" % table
    return row


def person_of(p, env, tf):
    """(address, name, identity_state) of a Participant row: address = email else Identity.address, name =
    Identity.display_name (spec 8.2)."""
    addr = name = None
    email = p.get("email")
    if email is not None:
        assert is_text(email) and len(email) <= MAX_ID, "an unreadable or over-long address is not restated"
        addr = go_text(email) or None
    state = None
    iid = p.get("identity_id")
    if iid is not None:
        assert is_int(iid), "an undecidable identity key is not restated"
        ident_row = env.rows["Identity"].get(iid)
        if ident_row is None:
            state = "no-Identity-row"
        else:
            a = ident_row.get("address")
            if addr is None and a is not None:
                assert is_text(a) and len(a) <= MAX_ID, "an unreadable or over-long address is not restated"
                addr = go_text(a) or None
            n = ident_row.get("display_name")
            if n is not None:
                assert is_text(n), "an unreadable name is not restated"
                c = C.clean(n, MAX_TITLE)
                if c.flags:
                    tf.add("attendees")
                name = c.text or None
    return addr, name, state


def expect_item(r, env, file="any"):
    """The expected record of one CalendarItem row (spec 8.2), or None for a non-event row (entity_type 3, Q11).
    env None: the typed recovery pass (B2)."""
    rid = r["rowid"]
    raw, unknown, tf, truncated = {}, {}, set(), []
    if "entity_type" in r:
        et = r["entity_type"]
        if is_int(et) and et == 3:
            return None
        if not (is_int(et) and et == 2):
            raw["item_kind_state"] = "undecided"
            if et is None or is_int(et):
                raw["entity_type"] = et
            else:
                unknown["CalendarItem.entity_type"] = "type-mismatch"
    raw["deletion_state"] = "not-evaluated"  # spec 6.4
    p = {"source": "ios_calendar", "source_id": str(rid)}
    timing, start, end, sdate, edate, times = times_of(r, raw, unknown)
    p["timing"] = timing
    if sdate:
        p["start_date"] = sdate
    if edate:
        p["end_date"] = edate
    for col, key in (("start_tz", "start_timezone"), ("end_tz", "end_timezone")):
        z = r.get(col)
        if z != FLOAT and z is not None and is_text(z):
            v = ident(raw, unknown, col, z, MAX_ZONE)
            if v:
                p[key] = v
    title = free_text(raw, tf, "title", r.get("summary"), MAX_TITLE, truncated, unknown, "summary")
    if title:
        p["title"] = title
    body = None
    desc = r.get("description")
    if desc is not None and not is_text(desc):
        unknown["CalendarItem.description"] = "type-mismatch"
    if is_text(desc):
        c = C.clean(desc, MAX_BODY)
        body = c.text or None
        if c.flags:
            p["body_flags"] = list(c.flags)
        if c.truncated:
            p["body_truncated"], p["body_total_bytes"] = True, c.total
        if c.b64:
            p["body_raw_b64"] = c.b64
        elif c.sha:
            p["body_raw_sha256"], p["body_raw_len"] = c.sha, c.rawlen
    for col, key in CODES:  # Q1: never labelled
        v = r.get(col)
        if v is not None:
            if not is_int(v):
                unknown["CalendarItem." + col] = "type-mismatch"
            p[key] = "unknown"
            raw[col] = jv(v)
    for col, key in (("url", "url"), ("unique_identifier", "uid"), ("external_id", "sync_id")):
        v = ident(raw, unknown, col, r.get(col), MAX_ID)
        if v:
            p[key] = v
    xr = r.get("external_rep")
    if xr is not None:
        b = bytes(xr) if isinstance(xr, (bytes, bytearray)) else str(xr).encode()
        raw["external_rep_len"], raw["external_rep_sha256"] = len(b), hashlib.sha256(b).hexdigest()
    oid = r.get("orig_item_id")
    if oid is not None and not is_int(oid):
        unknown["CalendarItem.orig_item_id"] = "type-mismatch"
    if is_int(oid) and oid != 0:  # 0 or NULL = none (spec 6.2)
        p["exception_of"] = {"event_id": str(oid)}
    if r.get("orig_date") is not None:
        raw["orig_date"] = jv(r["orig_date"])
        raw["orig_date_basis"] = "unestablished"
    for col in AS_STORED:
        if r.get(col) is not None:
            raw[col] = jv(r[col])
    # location (Get)
    loc = get_join(raw, unknown, env, "location_state", "Location", "location_id", r.get("location_id"))
    if loc is not None:
        lp = {}
        t = free_text(raw, tf, "location", loc.get("title"), MAX_TITLE, truncated, unknown, "title", "Location")
        if t:
            lp["text"] = t
        a = loc.get("address")
        if a is not None:  # its own flags and raw bytes have no key in the plan's vocabulary: only a clean text is restated
            assert is_text(a), "a wrongly typed address is not restated"
            if plain_text(a, MAX_TITLE, "Location.address"):
                lp["address"] = plain_text(a, MAX_TITLE, "Location.address")
        for col, lim in (("latitude", 90), ("longitude", 180)):
            v = loc.get(col)
            if v is None:
                continue
            assert is_int(v) or is_real(v), "a wrongly typed coordinate is not restated"
            if math.isfinite(v) and -lim <= v <= lim:
                lp[col] = jf(float(v))
            else:
                raw[col] = jv(v)
                raw[col + "_state"] = "out-of-range"
        if lp:
            p["location"] = lp
    # calendar (Get) and its store (Get)
    cal = get_join(raw, unknown, env, "calendar_state", "Calendar", "calendar_id", r.get("calendar_id"))
    if cal is not None:
        cp = {"id": str(r["calendar_id"])}
        t = cal.get("title")
        if is_text(t) and t:
            cp["title"] = plain_text(t, MAX_TITLE, "Calendar.title")
        st = get_join(raw, unknown, env, "store_state", "Store", "store_id", cal.get("store_id"))
        if st is not None:
            if is_text(st.get("name")) and st.get("name"):
                cp["account"] = {"name": plain_text(st["name"], MAX_TITLE, "Store.name")}
            if st.get("type") is not None:
                raw["store_type"] = jv(st["type"])
        p["calendar"] = cp
    # organizer (Get Participant, Get Identity)
    org = get_join(raw, unknown, env, "organizer_state", "Participant", "organizer_id", r.get("organizer_id"))
    if org is not None:
        addr, name, state = person_of(org, env, tf)
        o = {}
        if addr:
            o["address"] = addr
        if name:
            o["name"] = name
        assert not (o and state), "an organizer with an address and a dangling identity is not restated"
        if o:
            p["organizer"] = o
        elif state:
            raw["organizer_state"] = state
    # the four Match joins by owner: an empty match is no children and states nothing (F15)
    if env is None:
        for k in ("participants_state", "recurrence_state", "exception_dates_state", "alarms_state"):
            raw[k] = "not-joined-recovered"
    else:
        children(p, raw, tf, env, rid)
    if tf:
        raw["text_flags"] = ",".join(sorted(tf))
    if truncated:
        p["truncated_fields"] = sorted(set(truncated))
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    return {"table": "CalendarItem", "rowid": rid, "file": file, "type": "calendar_event", "source_path": "",
            "time": start, "time_end": end, "times": times, "deleted": False, "summary": summary(p, start),
            "body": body, "payload": p}


def children(p, raw, tf, env, rid):
    att, att_raw, unnamed, unnamed_raw = [], [], 0, []
    for x in env.match("Participant", rid):
        addr, name, state = person_of(x, env, tf)
        codes = entry_of(x, ("role", "type", "status"))
        for col in ("role", "type", "status"):
            assert x.get(col) is None or is_int(x[col]), "a wrongly typed participant code is not restated"
        if state:
            codes["identity_state"] = state
        if not addr and not name:
            unnamed += 1
            if state:  # its stored codes and state are kept beside the count (as android.calendar's F27 entries)
                unnamed_raw.append(codes)
            continue
        e = {}
        if addr:
            e["address"] = addr
        if name:
            e["name"] = name
        for col in ("role", "type", "status"):
            if x.get(col) is not None:
                e[col] = "unknown"  # Q1: the stored code is in raw.attendees
        att.append(e)
        att_raw.append(codes)
    att = capped(att, "attendees", p)
    if att:
        p["attendees"] = att
        raw["attendees"] = att_raw[:MAX_LIST]
    if unnamed:
        raw["attendees_unnamed"] = unnamed
    unnamed_raw = capped(unnamed_raw, "attendees_unnamed_entries", raw)
    if unnamed_raw:
        raw["attendees_unnamed_entries"] = unnamed_raw
    rules = env.match("Recurrence", rid)
    if rules:
        p["recurrence"] = {"stored_rules": len(rules)}
        raw["recurrence"] = capped([entry_of(x, ("frequency", "interval", "count", "end_date", "specifier")) for x in rules],
                                   "recurrence", raw)
    dates = capped([jv(x.get("date")) for x in env.match("ExceptionDate", rid)], "exception_dates", raw)
    if dates:
        raw["exception_dates"] = dates
    alarms = capped([entry_of(x, ("trigger_interval", "trigger_date", "type")) for x in env.match("Alarm", rid)], "alarms", raw)
    if alarms:
        raw["alarms"] = alarms


def summary(p, start):
    """calendar.Summary restated for the texts it can state (None: not compared)."""
    parts = ["calendar"]
    if "recurrence" in p:
        parts.append("series")
    elif "exception_of" in p:
        parts.append("exception")
    if start is not None:
        if p["timing"] == "all_day":
            parts.append(start[:10])
        elif p["timing"] == "timed":
            parts.append(start[:19] + "Z")
        elif p["timing"] == "floating":
            parts.append(start[:19] + " (floating)")
    t = summarize(p.get("title"), 120)
    if t is None:
        return None
    if t:
        parts.append(t)
    if "location" in p and p["location"].get("text"):
        lt = summarize(p["location"]["text"], 64)
        if lt is None:
            return None
        parts += ["@", lt]
    return " ".join(parts)


# ---------------------------------------------------------------- the CORE-iOS scenario


def item(i, **kw):
    row = {"ROWID": i, "summary": "Event %d" % i, "start_date": D, "end_date": D + H, "start_tz": "Europe/Berlin",
           "end_tz": "Europe/Berlin", "all_day": 0, "calendar_id": 1, "entity_type": 2}
    row.update(kw)
    return row


def core_items():
    W = 7 * DAY
    return [
        item(1, summary="Dentist", description="bring card", location_id=1, unique_identifier="uid-1@example.test",
             external_id="ext-1", url="https://example.test/e1", creation_date=D - DAY, last_modified=D - H, status=1,
             availability=0, privacy_level=0, organizer_id=4),
        item(2, end_date=D + 1800, start_tz="_float", end_tz="_float"),
        item(3, all_day=1, start_date=M, end_date=M + DAY, start_tz="_float", end_tz="_float"),
        item(4, all_day=1, start_date=M - H, end_date=M + DAY - H),
        item(5, all_day=1, start_date=M, end_date=M + DAY - 1),
        item(6, start_tz=None),
        item(7, orig_item_id=8, orig_date=D + W, start_date=D + W + 1800, end_date=D + W + 5400),
        item(8, has_recurrences=1),
        item(9, has_attendees=1),
        item(10),
        item(11, entity_type=3),
        item(12, entity_type=None),
        item(13, entity_type=7),
        item(14, start_tz="America/New_York", end_tz="Europe/London", end_date=D + 28800),
        item(15, end_tz="_float"),
        item(16, location_id=2),
        item(17, calendar_id=77, location_id=99),
        item(18, start_date=0),
        item(19, start_date=721692800.5),
        item(20, description=("A long description line. " * 12300)[:300 << 10]),
        item(21, summary=C.Raw(b"Bad\x00Title")),
        item(22, start_tz="Zone/" + "x" * 295),
        item(23, all_day=2),
        item(24, end_date=D - 60),
        item(25, hidden=1),
        item(26, start_date=None, end_date=D),
    ]


STORES = [{"ROWID": 1, "name": "iCloud", "type": 2}, {"ROWID": 2, "name": "Local", "type": 0}]
CALENDARS = [{"ROWID": 1, "title": "Home", "store_id": 1}, {"ROWID": 2, "title": "Birthdays", "store_id": 2}]
LOCATIONS = [{"ROWID": 1, "title": "Office", "address": "1 Main St", "latitude": 52.52, "longitude": 13.405},
             {"ROWID": 2, "title": "Nowhere", "address": None, "latitude": 200.0, "longitude": 13.4}]
IDENTITIES = [{"ROWID": 1, "display_name": "Ann", "address": "mailto:ann@example.test"},
              {"ROWID": 2, "display_name": "Bob", "address": "mailto:bob@example.test"}]
# record names the changed item (rows 1 and 3 exist in every variant that has items)
CHANGES = [{"ROWID": 1, "record": 1, "type": 1, "calendar_id": 1}, {"ROWID": 2, "record": 3, "type": 2, "calendar_id": 1}]


def core_children(ids):
    """The join and side rows of the CORE items in ids, by table."""
    out = {"Participant": [], "Recurrence": [], "ExceptionDate": [], "Alarm": [], "OccurrenceCache": []}
    if 9 in ids:
        out["Participant"] += [
            {"ROWID": 1, "item_id": 9, "identity_id": None, "email": "c@example.test", "role": 1, "type": 1, "status": 2},
            {"ROWID": 2, "item_id": 9, "identity_id": 2, "email": None, "role": 0, "type": 2, "status": 7},
            {"ROWID": 3, "item_id": 9, "identity_id": 99, "email": None}]
    if 1 in ids:  # row 1's organizer, and through Match(Participant.item_id) also its attendee (F29)
        out["Participant"].append({"ROWID": 4, "item_id": 1, "identity_id": 1, "email": None, "role": 2, "type": 1, "status": 1})
    if 8 in ids:
        out["Recurrence"] += [{"ROWID": 1, "owner_id": 8, "frequency": 1, "interval": 1},
                              {"ROWID": 2, "owner_id": 8, "frequency": 2, "interval": 1}]
        out["ExceptionDate"] += [{"ROWID": 1, "owner_id": 8, "date": D + DAY}, {"ROWID": 2, "owner_id": 8, "date": D + 2 * DAY}]
        out["OccurrenceCache"] += [{"day": M + 7 * k * DAY, "event_id": 8, "occurrence_date": D + 7 * k * DAY} for k in range(3)]
    if 10 in ids:
        out["Alarm"] += [{"ROWID": 1, "calendaritem_owner_id": 10, "trigger_interval": -900, "type": 0},
                         {"ROWID": 2, "calendaritem_owner_id": 10, "trigger_date": D - H, "type": 1}]
    return out


def fill(con, items, ids=None, extra=()):
    ids = set(r["ROWID"] for r in items) if ids is None else ids
    kids = core_children(ids)
    with C.batch(con):
        for t, rows in (("Store", STORES), ("Calendar", CALENDARS), ("Location", LOCATIONS), ("Identity", IDENTITIES),
                        ("CalendarItemChanges", CHANGES)):
            for r in rows:
                C.insert(con, t, r)
        for r in items:
            C.insert(con, "CalendarItem", r)
        for t, rows in kids.items():
            for r in rows:
                C.insert(con, t, r)
        for t, r in extra:
            C.insert(con, t, r)


def core_subset(ids):
    return [r for r in core_items() if r["ROWID"] in ids]


# ---------------------------------------------------------------- the expect.json


def byte_order(names):
    return sorted(names, key=lambda n: n.encode("utf-8"))


def oracle(files, variant, ddl=DDL, refused=False):
    """The <variant>.expect.json content. refused: the parser makes no record of this database (no CalendarItem table
    at the system path), so none is expected."""
    exp = C.expect_header(FAMILY, variant, ddl)
    if refused:
        return exp
    view = C.engine_view(files, ["CalendarItem", "OccurrenceCache", "sqlite_master"] + list(JOIN_TABLES))
    env = Env(view)
    notes = exp["notes"]
    sql = [jv(m["sql"]) for m in view["sqlite_master"] if m["type"] == b"table" and jv(m["name"]) == "CalendarItem"]
    has_kind = any(c == "entity_type" for c, _ in A.declared_columns(sql[0]))  # the declared columns, not a row's keys
    kinds = {}
    for r in view["CalendarItem"]:
        e = expect_item(r, env)
        if e is None:
            exp["warnings"].append({"table": "CalendarItem", "rowid": r["rowid"], "contains": NOT_EVENT})
        else:
            exp["records"].append(e)
        if has_kind and is_int(r.get("entity_type")):
            kinds[r["entity_type"]] = kinds.get(r["entity_type"], 0) + 1
    # 4I3-P10: the item kinds of the live rows (NULL is no value), or the column's absence
    if not has_kind:
        notes["entity_type"] = "absent"
    else:
        assert len(kinds) <= 16, "more than 16 kinds is not part of the scenario"
        notes["entity_type_values"] = ",".join("%d:%d" % (k, kinds[k]) for k in sorted(kinds))
        if kinds.get(3):  # a zero count is not stated
            notes["non_event_rows"] = str(kinds[3])
    tables = byte_order({jv(m["name"]) for m in view["sqlite_master"] if m["type"] == b"table"})
    notes["tables"] = ",".join(tables)
    unmapped = [t for t in tables if t not in MAPPED_TABLES]
    if unmapped:
        notes["unmapped_tables"] = ",".join(unmapped)
    if "OccurrenceCache" in view:  # a present table's row count, 0 included
        notes["occurrence_cache_rows"] = str(len(view["OccurrenceCache"]))
    changes = [t for t in tables if t.endswith("Changes")]
    if changes:
        counts = C.engine_view(files, changes)
        notes["change_tables"] = ",".join("%s:%d" % (t, len(counts[t])) for t in changes)
    return exp


# ---------------------------------------------------------------- variants


def create_db(path, ddl_text, page_size=4096):
    con = C.connect(path, page_size=page_size)
    con.executescript(ddl_text)
    return con


def snapshot():
    return C.load_ddl(DDL, SCHEMAS)[0]


def insert_sql(row):
    cols = list(row)
    return "insert into CalendarItem(%s) values (%s)" % (",".join(cols), ",".join(C.lit(row[c]) for c in cols))


def variant_core(work):
    """CORE-iOS rows 1-26 in the database file; row 27 in a committed WAL frame, row 28 in a WAL frame that is never
    committed (the writer dies inside its transaction after the frames spilled; the transaction also creates and fills
    a scratch table, never committed, so the leaf of row 28 spills)."""
    path = os.path.join(work, "Calendar.sqlitedb")
    con = create_db(path, snapshot())
    fill(con, core_items())
    con.execute("pragma journal_mode = wal")
    con.close()
    r27 = item(27, summary="Only in the WAL, committed")
    r28 = item(28, summary="Only in the WAL, uncommitted")
    stmts = ["pragma wal_autocheckpoint = 0", insert_sql(r27), "pragma cache_size = 1", "pragma cache_spill = 1", "BEGIN",
             insert_sql(r28), "update CalendarItem set description = 'spill ' || printf('%036000d', 0) where ROWID = 28",
             # a scratch table filled in the same transaction evicts row 28's leaf page from the one-page cache, so it
             # spills into the WAL too (the long description alone spills only its overflow pages)
             "create table spill(v integer)",
             "insert into spill with recursive c(i) as (select 1 union all select i + 1 from c where i < 20000) select i from c"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    if b"Only in the WAL, uncommitted" not in files["wal"]:
        raise SystemExit("calendar-ios: row 28 did not reach the WAL (no spill)")
    exp = oracle(files, "calendar-ios")
    # the reader's warning about the uncommitted frames comes before the row warnings (4I2 PF13a)
    exp["warnings"].insert(0, {"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
    return files, exp


def variant_lone(work, variant, ddl_text):
    path = os.path.join(work, "Calendar.sqlitedb")
    con = create_db(path, ddl_text)
    fill(con, core_subset(range(1, 4)))
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, variant)


def variant_extract(work):
    """CORE-iOS rows 1-3, a lone database, placed as an extraction (module doc)."""
    return variant_lone(work, "calendar-ios-extract", snapshot())


def variant_noentitytype(work):
    """CORE-iOS rows 1-3 in a CalendarItem without the entity_type column (Q11: every row a record, note absent)."""
    ddl = snapshot()
    without = ddl.replace(", entity_type INTEGER);", ");")
    if without == ddl:
        raise SystemExit("calendar-ios-noentitytype: the snapshot has no entity_type column to drop")
    return variant_lone(work, "calendar-ios-noentitytype", without)


def variant_oldlayout(work):
    """The older layout (spec 8.1): one table Event(ROWID, summary, start_date) and no CalendarItem; no record."""
    path = os.path.join(work, "Calendar.sqlitedb")
    con = create_db(path, "CREATE TABLE Event (ROWID INTEGER PRIMARY KEY, summary TEXT, start_date REAL);")
    with C.batch(con):
        for i in range(1, 4):
            C.insert(con, "Event", {"ROWID": i, "summary": "Event %d" % i, "start_date": D + i * H})
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, "calendar-ios-oldlayout", ddl="none: Event(ROWID, summary, start_date) written by the generator",
                         refused=True)


def disk_form(rows):
    """The scenario rows as their cells hold them: the engine stores an integral value of a REAL column as an INTEGER
    and reads it back as REAL, and the page walker compares cell values by type, so an integral float is an int here."""
    return [(rowid, {k: int(v) if isinstance(v, float) and v.is_integer() else v for k, v in d.items()}) for rowid, d in rows]


def variant_deleted(work):
    """CORE-iOS rows 1-3 live; CalendarItem rows 40-44 deleted on 512-byte pages, so their leaf pages go to the
    freelist (secure_delete OFF; the typed recovery oracle is a lower bound, 4I2 PF13). Row 40 names Location 1 and
    Calendar 1, and the Participant row 10 names it: all three survive in the live view (F17), so a recovered row 40
    that joined anything would show them (B2). Row 44 is a task (entity_type 3): recovered, it is the counted warning.
    A lone database."""
    path = os.path.join(work, "Calendar.sqlitedb")
    con = create_db(path, snapshot(), page_size=512)
    gone = [item(i, summary="Deleted %d" % i, start_date=D + i * H, end_date=D + i * H + H,
                 description="removed event %d " % i + "r" * 300) for i in range(40, 45)]
    gone[0]["location_id"] = 1
    gone[4]["entity_type"] = 3
    late = ("Participant", {"ROWID": 10, "item_id": 40, "identity_id": None, "email": "late@example.test", "role": 1,
                            "type": 1, "status": 1})
    fill(con, core_subset(range(1, 4)) + gone, ids=set(range(1, 4)), extra=[late])
    scen = SCENARIOS["calendar-ios-deleted"] = C.Scenario(CLAIMED, alias="ROWID")
    scen.write("CalendarItem", disk_form(scen.snap(con, "CalendarItem")))
    scen.want("CalendarItem", disk_form(scen.snap(con, "CalendarItem", "ROWID between 40 and 44")), label="CalendarItem")
    # a one-page scratch table dropped first becomes the freelist trunk, so every page the delete frees is a freelist
    # LEAF (a trunk page's list overwrites the cells it held); each deleted row fills a 512-byte page of its own
    con.execute("create table scratch(k integer primary key, v text)")
    con.execute("insert into scratch values (1, 'scratch')")
    con.execute("drop table scratch")
    with C.batch(con):
        con.execute("delete from CalendarItem where ROWID between 40 and 44")
    if con.execute("pragma freelist_count").fetchone()[0] < 3:
        raise SystemExit("calendar-ios-deleted: no freelist pages")
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, "calendar-ios-deleted")


def zform(rec):
    """The record with every local-unknown time written with Z: the Go recover kit's digest renders every time as
    RFC 3339 UTC whatever its basis, so the digest must be computed from this form."""
    def z(s):
        return s if s is None or s.endswith("Z") else s + "Z"
    out = dict(rec)
    out["time"], out["time_end"] = z(rec["time"]), z(rec["time_end"])
    out["times"] = {k: z(v) for k, v in (rec["times"] or {}).items()}
    return out


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (F24): every recovered CalendarItem row is
    mapped with env None, so it joins nothing (B2); a recovered task row is listed under unmapped with the non-event
    warning text. The digests use the Z form of the times (zform); the full records keep the oracle's time encoding."""
    def expect_fn(table, d, env, file):
        rec = expect_item(d, None, file)
        if rec is None:
            return None
        z = zform(rec)
        z["_oracle_form"] = rec  # both forms travel on the returned object; record_digest reads only the typed keys
        return z

    out = C.recover_oracle(variant, files, SCENARIOS[variant], lambda f: None, expect_fn, nobody=NOT_EVENT)
    for r in out["records"]:
        r["record"] = r["record"].pop("_oracle_form")
    return out


VARIANTS = {"calendar-ios": variant_core, "calendar-ios-extract": variant_extract,
            "calendar-ios-noentitytype": variant_noentitytype, "calendar-ios-oldlayout": variant_oldlayout,
            "calendar-ios-deleted": variant_deleted}


def _selfcheck():
    assert I.cocoa_iso(D, "s") == "2023-11-14T22:13:20Z" and I.cocoa_iso(M, "s") == "2023-11-15T00:00:00Z"
    assert midnight(M) and midnight(float(M)) and not midnight(M - H) and not midnight(721692800.5)
    assert jf(721692800.0) == 721692800 and json.dumps(jf(52.52)) == "52.52"
    print("calendar_ios self-checks OK")


if __name__ == "__main__":
    _selfcheck()

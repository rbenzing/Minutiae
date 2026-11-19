"""Android calendar provider (calendar.db) fixtures: DDL variants, the CORE scenario, the literal oracle (plan 4I3).

    py -3 -I tools/fixtures/telephony_gen.py --family calendar-android --out internal/parsers/androidcalendar/testdata \\
        --pins-go internal/parsers/androidcalendar/pins_test.go --pins-package androidcalendar_test
    py -3 -I tools/fixtures/telephony_gen.py --family calendar-android --recover-oracle --out internal/parsers/androidcalendar/testdata

The databases are built from the DDL snapshots of tools/fixtures/schemas/calendar-android (cross-checked against the
public AOSP calendar provider, CROSSCHECK.md there), with the PRAGMA user_version each snapshot's header states. The
oracle reads the stored values back from the engine on a COPY of the finished files and states the expected parser
output literally, from spec 5, 6 and 7.3 of plan 4I3, the plan's state vocabularies and the AOSP constants
(CalendarContract) and end rule (CalendarInstancesHelper). It is a second reading of the spec, not independent of the
design: it shares no code with the Go parser, but both follow the same text.

Time encoding of the oracle (the expect.json "time", "time_end" and "times" strings): a utc instant ends in "Z"
("2023-11-14T22:13:20Z"); a local-unknown reading (an all-day date at 00:00 "as if UTC") has no zone suffix
("2023-11-15T00:00:00"). The basis of a record time is therefore read from its string.

Every value here is fictional (example.test addresses, invented titles).
"""
import base64
import datetime
import hashlib
import json
import os
import re

import telephony_common as C

FAMILY = "calendar-android"
CLAIMED = ("Events",)  # Meta.Claims of android.calendar (spec 10)
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle)
# variant -> C.Scenario of the rows on freelist pages of the DATABASE FILE when the variant also has a WAL: the page
# walker (sqlite_oracle.collect_images) reads freelist leaves only for a lone database, so recover_expect walks the
# database file alone for them and merges the two answers (the WAL leaves those pages untouched).
FREELIST_SCENARIOS = {}
REMOTE = "/data/data/com.android.providers.calendar/databases/calendar.db"
SCHEMAS = os.path.join(C.HERE, "schemas", "calendar-android")

D = 1700000000000  # 2023-11-14T22:13:20Z
M = 1700006400000  # 2023-11-15T00:00:00Z
H = 3600000
DAY = 86400000
MS_1955 = -457747200000  # 1955-07-01T00:00:00Z (Q5: before 1970, so invalid)

MAX_TITLE = 4 << 10  # title and location text (spec 9)
MAX_BODY = 256 << 10  # the description (spec 4.2)
MAX_ZONE = 256  # zone names (spec 4.1)
MAX_ID = 1 << 10  # uid, sync id, organizer and attendee addresses
MAX_RULE = 16 << 10  # each recurrence text
MAX_DURATION = 64
MAX_PROP = 4 << 10  # an extended property's value (spec 7.3)
MAX_SYNC_DATA = 1 << 10  # sync_data1..10, raw only
MAX_LIST = 1000

# The AOSP platform contract (CalendarContract.java at android-14.0.0_r1; lines in CROSSCHECK.md).
EVENT_STATUS = {0: "tentative", 1: "confirmed", 2: "cancelled"}
AVAILABILITY = {0: "busy", 1: "free", 2: "tentative"}
ACCESS_LEVEL = {0: "default", 1: "confidential", 2: "private", 3: "public"}
RELATIONSHIP = {0: "none", 1: "attendee", 2: "organizer", 3: "performer", 4: "speaker"}
ATTENDEE_TYPE = {0: "none", 1: "required", 2: "optional", 3: "resource"}
ATTENDEE_STATUS = {0: "none", 1: "accepted", 2: "declined", 3: "invited", 4: "tentative"}
REMINDER_METHOD = {0: "default", 1: "alert", 2: "email", 3: "sms", 4: "alarm"}
ALERT_STATE = {0: "scheduled", 1: "fired", 2: "dismissed"}

# Columns kept in raw exactly as stored when the cell is not NULL (the plan's "As stored" list).
AS_STORED_INT = ("eventColor", "hasAlarm", "hasExtendedProperties", "hasAttendeeData",
                 "guestsCanModify", "guestsCanInviteOthers", "guestsCanSeeGuests", "isOrganizer")
AS_STORED_TEXT = ("eventColor_index", "customAppPackage", "customAppUri")
CAL_RAW = ("name", "calendar_timezone", "visible", "sync_events", "calendar_access_level", "isPrimary", "deleted", "dirty")
GO_SPACE = "".join(chr(c) for c in [9, 10, 11, 12, 13, 32, 0x85, 0xA0, 0x1680, *range(0x2000, 0x200B), 0x2028, 0x2029, 0x202F, 0x205F, 0x3000])


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def is_text(v):
    return isinstance(v, bytes) and not isinstance(v, C.Blob)


def jv(v):
    """A stored value as raw JSON: a TEXT cell as its string, a BLOB as {"blob_b64": ...}."""
    if isinstance(v, C.Blob):
        return {"blob_b64": base64.b64encode(bytes(v)).decode()}
    if isinstance(v, bytes):
        return v.decode("utf-8", errors="replace")
    return v


def utc(ms):
    """A utc instant: RFC 3339 with Z."""
    return C.iso(ms, "ms")


def local(ms):
    """A local-unknown reading: the same wall-clock text without a zone suffix (module doc)."""
    return C.iso(ms, "ms")[:-1]


def date_of(ms):
    return (C.EPOCH + datetime.timedelta(milliseconds=ms)).strftime("%Y-%m-%d")


# ---------------------------------------------------------------- the duration grammar (spec 5.3, plan Task 3)

DURATION_RE = re.compile(rb"[+-]?P(T?)((?:[0-9]+[WDHMST]?)+)")
UNIT = {b"W": 604800, b"D": 86400, b"H": 3600, b"M": 60, b"S": 1}


def parse_duration(b):
    """(seconds, state): state "" (parsed), "unparsed" or "out-of-range" (spec 5.3 grammar). A text over 64 bytes is
    "unparsed": the grammar is bounded at 64 bytes and the vocabulary has no other value (fix round 1, M3)."""
    if len(b) > MAX_DURATION:
        return 0, "unparsed"
    i, n, sign = 0, len(b), 1
    if i < n and b[i:i + 1] in (b"+", b"-"):
        sign = -1 if b[i:i + 1] == b"-" else 1
        i += 1
    if b[i:i + 1] != b"P":
        return 0, "unparsed"
    i += 1
    seen_t, seen, runs, total, over = False, set(), 0, 0, False
    while i < n:
        c = b[i:i + 1]
        if c == b"T":
            if seen_t:
                return 0, "unparsed"
            seen_t = True
            i += 1
            continue
        j = i
        while j < n and b[j:j + 1].isdigit():
            j += 1
        if j == i or j >= n:
            return 0, "unparsed"  # no digits, or a trailing digit run without a unit
        unit = b[j:j + 1]
        if unit not in UNIT or unit in seen:
            return 0, "unparsed"
        seen.add(unit)
        if j - i > 9:
            over = True
        else:
            total += int(b[i:j]) * UNIT[unit]
        runs += 1
        i = j + 1
    if runs == 0:
        return 0, "unparsed"
    if over or total > 2147483647:
        return 0, "out-of-range"
    return sign * total, ""


# ---------------------------------------------------------------- text helpers


def summarize(s, limit):
    """common.Summarize for the text this oracle restates: None when the text is outside what is restated (any control,
    format or separator character other than a single inner space, or longer than limit bytes)."""
    if s is None:
        return ""
    if s != s.strip(GO_SPACE) or "  " in s or any((not ch.isprintable() and ch != " ") or ch in GO_SPACE.replace(" ", "") for ch in s):
        return None
    return s if len(s.encode("utf-8")) <= limit else None


def ident(raw, unknown, col, v, limit):
    """An identifier as stored, never cut (spec 9): over limit -> absent with <col>_state too-long, _len and _sha256."""
    if v is None:
        return None
    if not is_text(v):
        unknown["Events." + col] = "type-mismatch"
        return None
    if len(v) > limit:
        raw[col + "_state"] = "too-long"
        raw[col + "_len"] = len(v)
        raw[col + "_sha256"] = hashlib.sha256(v).hexdigest()
        return None
    s = v.decode("utf-8", errors="replace")
    return s or None


def free_text(raw, tf, field, v, limit, truncated):
    """A cleaned free text (C4): altered or cut -> listed in raw.text_flags; cut -> truncated_fields; invalid UTF-8 or NUL
    -> raw.<field>_raw_b64 (or _raw_sha256 when large)."""
    if not is_text(v):
        return None
    c = C.clean(v, limit)
    if c.flags:
        tf.add(field)
    if c.truncated:
        truncated.append(field)
    if c.b64:
        raw[field + "_raw_b64"] = c.b64
    elif c.sha:
        raw[field + "_raw_sha256"] = c.sha
    return c.text or None


# ---------------------------------------------------------------- the event oracle


class Env:
    """The live view of the joined tables; None in the typed recovery pass (4I3-P4: a recovered row joins nothing)."""

    def __init__(self, view):
        self.calendars = {r["rowid"]: r for r in view.get("Calendars", [])}
        self.events = {r["rowid"] for r in view.get("Events", [])}
        self.children = {}
        for t in ("Attendees", "Reminders", "CalendarAlerts", "ExtendedProperties", "EventsRawTimes"):
            by = {}
            for r in view.get(t, []):
                by.setdefault(r.get("event_id"), []).append(r)
            self.children[t] = by


def instant(raw, col, v, unknown):
    """rowrun.Millis + Instant.Record restated: (ms or None). An INTEGER cell is recorded in raw with its status when not
    valid; NULL -> raw null; another type -> unknown_columns, nothing in raw."""
    if v is None:
        raw[col] = None
        return None
    if not is_int(v):
        unknown["Events." + col] = "type-mismatch"
        return None
    raw[col] = v
    st = C.classify(v, "ms")
    if st != "valid":
        raw[col + "_status"] = st
        return None
    return v


def times_of(r, raw, unknown):
    """Spec 5 and plan Task 3: (timing, start, end, start_date, end_date, times)."""
    ad = r.get("allDay")
    timing = {0: "timed", 1: "all_day"}.get(ad) if is_int(ad) else None
    if timing is None:
        timing = "unknown"
        raw["timing_state"] = "undecided"
        raw["allDay"] = jv(ad)
        if ad is not None and not is_int(ad):
            unknown["Events.allDay"] = "type-mismatch"
    start_ms = instant(raw, "dtstart", r.get("dtstart"), unknown)
    dtend_ms = None
    if "dtend" in r:
        dtend_ms = instant(raw, "dtend", r.get("dtend"), unknown)
    start = end = None
    sdate = edate = None
    if timing == "all_day":
        tz = r.get("eventTimezone")
        if is_text(tz) and tz != b"UTC":
            raw["all_day_zone_state"] = "not-utc"  # 4I3-P8: the date is still read from the UTC midnight
    if timing != "unknown" and start_ms is not None:
        if timing == "timed":
            start = utc(start_ms)
        elif start_ms % DAY == 0:
            start, sdate = local(start_ms), date_of(start_ms)
        else:
            raw["start_state"] = "all-day-not-midnight"
    if timing != "unknown":
        if start is None:
            raw["end_state"] = "no-start"  # B5, before any other end rule
        else:
            end_ms, basis = None, None
            dur = r.get("duration")
            if dur is not None and is_text(dur):
                secs, state = parse_duration(dur)
                if state:
                    raw["duration_state"] = state  # no fallback to dtend (spec 5.3)
                else:
                    end_ms, basis = start_ms + secs * 1000, "dtstart+duration"
                    if dtend_ms is not None and dtend_ms != end_ms:
                        raw["end_conflict"] = True
            elif dur is not None:
                unknown["Events.duration"] = "type-mismatch"
            elif dtend_ms is not None:
                end_ms, basis = dtend_ms, "dtend"
            elif r.get("dtend") is None:
                raw["end_state"] = "none-stored"
            if end_ms is not None:
                if end_ms < start_ms:
                    raw["end_state"] = "end-before-start"
                elif timing == "timed":
                    end = utc(end_ms)
                    raw["time_end_basis"] = basis
                elif end_ms % DAY == 0:
                    end, edate = local(end_ms), date_of(end_ms)
                    raw["time_end_basis"] = basis
                else:
                    raw["end_state"] = "all-day-not-midnight"
    times = {}
    oit = r.get("originalInstanceTime")
    if oit is not None:
        o_ms = instant(raw, "originalInstanceTime", oit, unknown)
        oad = r.get("originalAllDay")
        if oad is not None:
            raw["originalAllDay"] = jv(oad)
        if o_ms is not None:
            if is_int(oad) and oad == 0:
                times["original_start"] = utc(o_ms)
            elif is_int(oad) and oad == 1:
                if o_ms % DAY == 0:
                    times["original_start"] = local(o_ms)
                else:
                    raw["original_start_state"] = "all-day-not-midnight"
            else:
                raw["original_start_state"] = "undecided"
    return timing, start, end, sdate, edate, times


def flag(raw, unknown, name, v, state_key):
    """A deletion or sync flag (spec 6.3) of the column name ("<Table>.<col>"): 0 or 1, else undecided with
    raw.<state_key> = "undecided" and an unknown_columns entry: "type-mismatch" for a cell of another type,
    "out-of-set" for an INTEGER other than 0 and 1 (fix round 1, M1, M2). A stored NULL is undecided too but never
    listed in unknown_columns (the vocabulary's rule)."""
    if is_int(v) and v in (0, 1):
        return v
    if v is not None:
        unknown[name] = "out-of-set" if is_int(v) else "type-mismatch"
    raw[state_key] = "undecided"
    return None


def labelled(raw, col, v, table, keep_raw=True):
    """(label or None): a CalendarContract code; another value -> "unknown" with the code in raw."""
    if v is None:
        return None
    if is_int(v) and v in table:
        if keep_raw:
            raw[col] = v
        return table[v]
    raw[col] = jv(v)
    return "unknown"


def expect_event(r, env, remote, file="any"):
    """The expected record of one Events row (spec 7.3). env None: the typed recovery pass (4I3-P4)."""
    rid = r["rowid"]
    raw, unknown, tf, truncated = {}, {}, set(), []
    p = {"source": "android_calendar", "source_id": str(rid)}
    timing, start, end, sdate, edate, times = times_of(r, raw, unknown)
    p["timing"] = timing
    if sdate:
        p["start_date"] = sdate
    if edate:
        p["end_date"] = edate
    for col, key in (("eventTimezone", "start_timezone"), ("eventEndTimezone", "end_timezone")):
        z = ident(raw, unknown, col, r.get(col), MAX_ZONE)
        if z:
            p[key] = z
    # duration: the payload text when it fits 64 bytes; a longer one is never cut and never kept in the payload: its
    # length and SHA-256 go to raw beside raw.duration_state = "unparsed" (set by times_of). raw.duration_state carries
    # only the parse vocabulary (unparsed, out-of-range); ident's "too-long" is not used for this column (M3).
    dv = r.get("duration")
    if is_text(dv) and dv:
        if len(dv) > MAX_DURATION:
            raw["duration_len"], raw["duration_sha256"] = len(dv), hashlib.sha256(dv).hexdigest()
        else:
            p["duration"] = dv.decode("utf-8", errors="replace")
    title = free_text(raw, tf, "title", r.get("title"), MAX_TITLE, truncated)
    if title:
        p["title"] = title
    loc = free_text(raw, tf, "location", r.get("eventLocation"), MAX_TITLE, truncated)
    if loc:
        p["location"] = {"text": loc}
    body = None
    desc = r.get("description")
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
    st = labelled(raw, "eventStatus", r.get("eventStatus"), EVENT_STATUS)
    if st:
        p["status"] = st
    for col, key, table in (("availability", "availability", AVAILABILITY), ("accessLevel", "privacy", ACCESS_LEVEL)):
        v = labelled(raw, col, r.get(col), table)
        if v:
            p[key] = v
    ss = labelled(raw, "selfAttendeeStatus", r.get("selfAttendeeStatus"), ATTENDEE_STATUS, keep_raw=False)
    if ss:
        p["self_status"] = ss
    org = r.get("organizer")
    if is_text(org):
        a = ident(raw, unknown, "organizer", org, MAX_ID)
        if a:
            p["organizer"] = {"address": a}
    elif org is not None:
        raw["organizer"] = jv(org)
        raw["organizer_state"] = "not-text"  # NUMERIC affinity: the written text is not recoverable
    rec = {}
    for col in ("rrule", "rdate", "exrule", "exdate"):
        v = ident(raw, unknown, col, r.get(col), MAX_RULE)
        if v:
            rec[col] = v
    if rec:
        p["recurrence"] = rec
    exc = {}
    oid = r.get("original_id")
    if is_int(oid):
        exc["event_id"] = str(oid)
        if env is None:
            raw["original_state"] = "not-joined-recovered"
        elif oid not in env.events:
            raw["original_state"] = "no-events-row"
    osid = ident(raw, unknown, "original_sync_id", r.get("original_sync_id"), MAX_ID)
    if osid:
        exc["sync_id"] = osid
    if exc:
        p["exception_of"] = exc
    uid = ident(raw, unknown, "uid2445", r.get("uid2445"), MAX_ID)
    if uid:
        p["uid"] = uid
    sid = ident(raw, unknown, "_sync_id", r.get("_sync_id"), MAX_ID)
    if sid:
        p["sync_id"] = sid
    # the calendar (spec 7.3) and the deletion states (spec 6.3, B7)
    cal_deleted = None
    if env is None:
        raw["calendar_state"] = "not-joined-recovered"
        raw["deleted_via_state"] = "not-joined-recovered"
    else:
        cid = r.get("calendar_id")
        cal = env.calendars.get(cid) if is_int(cid) else None
        if not is_int(cid):  # an undecidable key (spec 7.3): the join is undecided, never "no row" (fix round 1, M5)
            raw["join_undecided"] = ["Calendars"]
            if cid is not None:
                unknown["Events.calendar_id"] = "type-mismatch"
        elif cal is None:
            raw["calendar_state"] = "no-calendars-row"
        else:
            cp = {"id": str(cid)}
            t = cal.get("calendar_displayName")
            if is_text(t) and t:
                cp["title"] = C.clean(t, MAX_TITLE).text
            acct = {}
            for col, key in (("account_type", "type"), ("account_name", "name")):
                if is_text(cal.get(col)) and cal.get(col):
                    acct[key] = cal[col].decode("utf-8", errors="replace")
            if acct:
                cp["account"] = acct
            if is_text(cal.get("ownerAccount")) and cal.get("ownerAccount"):
                cp["owner"] = cal["ownerAccount"].decode("utf-8", errors="replace")
            p["calendar"] = cp
            rc = {k: jv(cal[k]) for k in CAL_RAW if cal.get(k) is not None}
            if rc:
                raw["calendar"] = rc
            cal_deleted = flag(raw, unknown, "Calendars.deleted", cal.get("deleted"), "deleted_via_state")
    ev_deleted = flag(raw, unknown, "Events.deleted", r.get("deleted"), "deleted_state")
    last_synced = flag(raw, unknown, "Events.lastSynced", r.get("lastSynced"), "lastSynced_state") if "lastSynced" in r else None
    dirty = r.get("dirty")
    if dirty is not None:
        if is_int(dirty) and dirty in (0, 1):
            raw["dirty"] = dirty
        else:
            flag(raw, unknown, "Events.dirty", dirty, "dirty_state")
            if is_int(dirty):
                raw["dirty"] = dirty
    if r.get("mutators") is not None:
        raw["mutators"] = jv(r["mutators"])
    if last_synced == 1:
        raw["last_synced"] = 1
    matches = []
    if ev_deleted == 1:
        matches.append(("Events.deleted", {"source": "Events.deleted"}))
    if last_synced == 1:
        matches.append(("Events.lastSynced", {"source": "Events.lastSynced", "relation": "last-synced-copy"}))
    if cal_deleted == 1:
        matches.append(("Calendars.deleted", {"source": "Calendars.deleted"}))
    deleted = bool(matches)
    if matches:
        p["deleted"] = matches[0][1]
        if matches[0][0] == "Calendars.deleted":
            raw["deleted_via"] = "calendar"
        if len(matches) > 1:
            raw["deletion_also"] = ",".join(m[0] for m in matches[1:])
    # raw only, as stored
    for col in AS_STORED_INT + AS_STORED_TEXT:
        if r.get(col) is not None:
            raw[col] = jv(r[col])
    if r.get("lastDate") is not None:
        instant(raw, "lastDate", r["lastDate"], unknown)
    for i in range(1, 11):
        col = "sync_data%d" % i
        v = r.get(col)
        if v is None:
            continue
        if is_text(v) and len(v) > MAX_SYNC_DATA:
            raw[col] = C.clean(v, MAX_SYNC_DATA).text
            raw[col + "_len"], raw[col + "_sha256"] = len(v), hashlib.sha256(v).hexdigest()
        else:
            raw[col] = jv(v)
    # children (spec 7.3; plan Task 5)
    if env is None:
        for k in ("attendees_state", "reminders_state", "alerts_state", "extended_properties_state", "raw_times_state"):
            raw[k] = "not-joined-recovered"
    else:
        children(p, raw, tf, unknown, env, rid)
    if tf:
        raw["text_flags"] = ",".join(sorted(tf))
    if truncated:
        p["truncated_fields"] = sorted(truncated)
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    return {"table": "Events", "rowid": rid, "file": file, "type": "calendar_event", "source_path": remote,
            "time": start, "time_end": end, "times": times, "deleted": deleted, "summary": summary(p, start),
            "body": body, "payload": p}


def capped(items, key, holder):
    if len(items) > MAX_LIST:
        holder[key + "_total"], holder[key + "_truncated"] = len(items), True
        return items[:MAX_LIST]
    return items


def children(p, raw, tf, unknown, env, rid):
    att, att_raw, unnamed = [], [], 0
    rows = sorted(env.children["Attendees"].get(rid, []), key=lambda x: x["rowid"])
    for a in rows:
        e, extra = {}, {}
        addr = a.get("attendeeEmail")
        if is_text(addr) and addr and len(addr) <= MAX_ID:
            e["address"] = addr.decode("utf-8", errors="replace")
        elif is_text(addr) and addr:  # C5: never cut, never dropped silently (F27, fix round 1, M4)
            extra = {"address_state": "too-long", "address_len": len(addr), "address_sha256": hashlib.sha256(addr).hexdigest()}
        elif addr is not None and not is_text(addr):
            unknown["Attendees.attendeeEmail"] = "type-mismatch"
        name = a.get("attendeeName")
        if is_text(name) and name:
            c = C.clean(name, MAX_TITLE)
            if c.flags:
                tf.add("attendees")
            if c.text:
                e["name"] = c.text
        codes = {k: jv(a[k]) for k in ("attendeeRelationship", "attendeeType", "attendeeStatus", "attendeeIdentity", "attendeeIdNamespace")
                 if a.get(k) is not None}
        if not e:
            unnamed += 1
            if extra:  # F27: its raw.attendees entry keeps the address length and SHA-256 as sibling scalars
                att_raw.append(dict(codes, **extra))
            continue
        for col, key, table in (("attendeeRelationship", "role", RELATIONSHIP), ("attendeeType", "type", ATTENDEE_TYPE),
                                ("attendeeStatus", "status", ATTENDEE_STATUS)):
            v = a.get(col)
            if v is not None:
                e[key] = table[v] if is_int(v) and v in table else "unknown"
        att.append(e)
        att_raw.append(dict(codes, **extra))
    att = capped(att, "attendees", p)
    if att:
        p["attendees"] = att
    if att_raw:
        raw["attendees"] = att_raw[:MAX_LIST]
    if unnamed:
        raw["attendees_unnamed"] = unnamed
    rem, rem_raw = [], []
    for x in sorted(env.children["Reminders"].get(rid, []), key=lambda x: x["rowid"]):
        e = {}
        m = x.get("minutes")
        if is_int(m) and m >= 0:
            e["before_s"] = m * 60
        elif is_int(m) and m != -1:
            raw["reminders_state"] = "negative-minutes"
        meth = x.get("method")
        if meth is not None:
            e["method"] = REMINDER_METHOD[meth] if is_int(meth) and meth in REMINDER_METHOD else "unknown"
        if not e:
            continue
        rem.append(e)
        rem_raw.append({k: jv(x[k]) for k in ("minutes", "method") if x.get(k) is not None})
    rem = capped(rem, "reminders", p)
    if rem:
        p["reminders"] = rem
        raw["reminders"] = rem_raw[:MAX_LIST]
    alerts = []
    for x in sorted(env.children["CalendarAlerts"].get(rid, []), key=lambda x: x["rowid"]):
        e = {k: jv(x[k]) for k in ("begin", "end", "alarmTime", "creationTime", "receivedTime", "notifyTime", "state") if x.get(k) is not None}
        if is_int(x.get("state")) and x["state"] in ALERT_STATE:
            e["state_label"] = ALERT_STATE[x["state"]]
        alerts.append(e)
    alerts = capped(alerts, "alerts", raw)
    if alerts:
        raw["alerts"] = alerts
    props = []
    for x in sorted(env.children["ExtendedProperties"].get(rid, []), key=lambda x: x["rowid"]):
        e = {}
        if x.get("name") is not None:
            e["name"] = jv(x["name"])
        v = x.get("value")
        if is_text(v):
            c = C.clean(v, MAX_PROP)
            e["value"] = c.text
            if c.flags:
                e["value_flags"] = ",".join(c.flags)
        elif v is not None:
            e["value"] = jv(v)
        props.append(e)
    props = capped(props, "extended_properties", raw)
    if props:
        raw["extended_properties"] = props
    rt = env.children["EventsRawTimes"].get(rid, [])
    if rt:
        x = sorted(rt, key=lambda x: x["rowid"])[0]
        e = {k: jv(x[k]) for k in ("dtstart2445", "dtend2445", "originalInstanceTime2445", "lastDate2445") if x.get(k) is not None}
        if e:
            raw["raw_times"] = e


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
            parts.append(start)
    t = summarize(p.get("title"), 120)
    if t is None:
        return None
    if t:
        parts.append(t)
    if "location" in p:
        lt = summarize(p["location"].get("text"), 64)
        if lt is None:
            return None
        if lt:
            parts += ["@", lt]
    if p.get("status") == "cancelled":
        parts.append("[cancelled]")
    return " ".join(parts)


# ---------------------------------------------------------------- the CORE scenario


def ev(i, **kw):
    row = {"_id": i, "calendar_id": 1, "allDay": 0, "dtstart": D, "dtend": D + H, "eventTimezone": "Europe/Berlin",
           "deleted": 0, "lastSynced": 0, "dirty": 0, "title": "Event %d" % i}
    row.update(kw)
    return row


def core_events():
    rtl = "פגישה موعد"
    return [
        ev(1, title="Dentist", eventLocation="Main St 1", description="bring card", eventStatus=1, availability=0, accessLevel=0,
           selfAttendeeStatus=1, organizer="ann@example.test", uid2445="uid-1@example.test", _sync_id="s1"),
        ev(2, dtend=None, duration="PT1H", rrule="FREQ=WEEKLY;BYDAY=TU", exdate="20231121T221320Z", _sync_id="s2"),
        ev(3, dtend=None, duration="P3600S"),
        ev(4, allDay=1, dtstart=M, dtend=None, duration="P1D", eventTimezone="UTC"),
        ev(5, dtend=None, duration="P2W"),
        ev(6, allDay=1, dtstart=M, dtend=M + DAY, eventTimezone="UTC", title="Holiday"),
        ev(7, allDay=1, dtstart=M, dtend=M + 3 * DAY, eventTimezone="UTC"),
        ev(8, allDay=1, dtstart=M + H, dtend=M + DAY + H, eventTimezone="UTC"),
        ev(9, original_id=2, originalInstanceTime=D + 7 * DAY, originalAllDay=0, dtstart=D + 7 * DAY + H // 2, dtend=D + 7 * DAY + 3 * H // 2),
        ev(10, original_id=2, originalInstanceTime=D + 14 * DAY, originalAllDay=0, eventStatus=2),
        ev(11, original_id=99, original_sync_id="s99", originalInstanceTime=D, originalAllDay=0),
        ev(12, deleted=1, dirty=1, mutators="com.example.app", _sync_id="s12"),
        ev(13, calendar_id=3),
        ev(14, _sync_id="s14", dirty=1, title="Meeting v2"),
        ev(15, _sync_id="s14", lastSynced=1, title="Meeting v1"),
        ev(16, organizer=15550000001),
        ev(17), ev(18), ev(19),
        ev(20, allDay=1, dtstart=M, dtend=M + DAY, eventTimezone="America/New_York"),
        ev(21, dtend=D + 2 * H, rrule="FREQ=DAILY;COUNT=5"),
        ev(22, calendar_id=4, uid2445="uid-1@example.test", _sync_id="s1"),
        ev(23, lastSynced=1, _sync_id="s23"),
        ev(24, dtend=D + 2 * H, duration="PT1H"),
        ev(25, duration="P1Y"),
        ev(26, dtend=None, duration="P9999999999S"),
        ev(27, dtend=D - H),
        ev(28, dtstart=None, dtend=D),
        ev(29, dtstart=0),
        ev(30, dtstart=5000000000000),
        ev(31, allDay=2),
        ev(32, deleted="x"),
        ev(33, calendar_id=5),
        ev(34, lastSynced="y"),
        ev(35, description=("A long description line. " * 12300)[:300 << 10]),
        ev(36, title=C.Raw(b"Bad\x00Title\xff")),
        ev(37, title=rtl),
        ev(38, eventTimezone="Zone/" + "x" * 295),
        ev(39, uid2445="uid-" + "x" * 1996),
        ev(40, calendar_id=77),
        ev(41, allDay=1, dtstart=MS_1955, dtend=MS_1955 + DAY, eventTimezone="UTC"),
    ]


def calendars():
    def cal(i, disp, atype, aname, owner, deleted, **kw):
        row = {"_id": i, "calendar_displayName": disp, "account_type": atype, "account_name": aname, "ownerAccount": owner,
               "deleted": deleted, "name": disp.lower(), "calendar_timezone": "Europe/Berlin", "visible": 1, "sync_events": 1,
               "calendar_access_level": 700, "isPrimary": 0, "dirty": 0}
        row.update(kw)
        return row
    return [cal(1, "Personal", "com.example.sync", "ann@example.test", "ann@example.test", 0, isPrimary=1),
            cal(2, "Local", "LOCAL", "local", None, 0),
            cal(3, "Old", "com.example.sync", "ann@example.test", "ann@example.test", 1),
            cal(4, "Team", "com.example.sync", "bob@example.test", "bob@example.test", 0),
            cal(5, "Odd", "com.example.sync", "ann@example.test", "ann@example.test", "x")]


def core_children(ids):
    """The child rows of the CORE events in ids."""
    out = {"Attendees": [], "Reminders": [], "CalendarAlerts": [], "ExtendedProperties": [], "EventsRawTimes": [], "Instances": []}
    if 17 in ids:
        for n, (email, name, rel, typ, status) in enumerate([
                ("a@example.test", "A", 2, 1, 1), ("b@example.test", None, 1, 2, 2), (None, "C", 3, 3, 3),
                ("d@example.test", "D", 4, 0, 4), ("e@example.test", None, 0, 0, 0), ("f@example.test", None, 9, 7, 8),
                (None, None, 1, 1, 1)], 1):
            out["Attendees"].append({"_id": n, "event_id": 17, "attendeeEmail": email, "attendeeName": name,
                                     "attendeeRelationship": rel, "attendeeType": typ, "attendeeStatus": status})
    if 1 in ids:
        out["Reminders"].append({"_id": 1, "event_id": 1, "minutes": 10, "method": 9})
        out["Instances"].append({"_id": 1, "event_id": 1, "begin": D, "end": D + H, "startDay": 2460264, "endDay": 2460264,
                                 "startMinute": 1393, "endMinute": 1453})
    if 18 in ids:
        for n, (m, meth) in enumerate([(-1, 0), (0, 1), (30, 2), (-5, 4)], 2):
            out["Reminders"].append({"_id": n, "event_id": 18, "minutes": m, "method": meth})
    if 19 in ids:
        out["CalendarAlerts"] += [
            {"_id": 1, "event_id": 19, "begin": D, "end": D + H, "alarmTime": D - 600000, "creationTime": D - 700000,
             "receivedTime": D - 650000, "notifyTime": D - 600000, "state": 1, "minutes": 10},
            {"_id": 2, "event_id": 19, "begin": D, "end": D + H, "alarmTime": D - 300000, "creationTime": D - 700000,
             "receivedTime": D - 350000, "notifyTime": D - 300000, "state": 7, "minutes": 5}]
        out["ExtendedProperties"] += [{"_id": 1, "event_id": 19, "name": "vnd.example/x", "value": "1"},
                                      {"_id": 2, "event_id": 19, "name": "vnd.example/big", "value": ("p" * 64 + "\n") * 80}]
        out["EventsRawTimes"].append({"_id": 1, "event_id": 19, "dtstart2445": "20231114T221320Z", "dtend2445": "20231114T231320Z"})
    return out


META = {"_id": 1, "localTimezone": "Europe/Berlin", "minInstance": 1698796800000, "maxInstance": 1701388800000}
CACHE = [{"_id": 1, "key": "timezoneType", "value": "auto"}, {"_id": 2, "key": "timezoneInstances", "value": "Europe/Berlin"},
         {"_id": 3, "key": "timezoneInstancesPrevious", "value": "Europe/Berlin"}]
CHILD_TABLES = ("Attendees", "Reminders", "CalendarAlerts", "ExtendedProperties", "EventsRawTimes", "Instances")


# ---------------------------------------------------------------- the expect.json


def user_version_of(ddl_name):
    with open(os.path.join(SCHEMAS, ddl_name), encoding="utf-8") as f:
        m = re.search(r"^-- user_version (\d+)$", f.read(), re.M)
    return int(m.group(1))


# The count notes of spec 7.3 (soft_deleted, lastsynced_copies); spec 7.3 lists no note for the events of a deleted
# calendar, so none is stated (fix round 1, M6).
COUNT_NOTE = {"Events.deleted": "soft_deleted", "Events.lastSynced": "lastsynced_copies"}


def oracle(files, ddl_name, variant, user_version, refused=False):
    """The <variant>.expect.json content. refused: the parser makes no record of this database (a user version outside
    the band 400-699, or no Events table), so none is expected."""
    exp = C.expect_header(FAMILY, variant, ddl_name)
    if refused:
        return exp
    view = C.engine_view(files, ["Events", "Calendars"] + list(CHILD_TABLES) + ["CalendarMetaData", "CalendarCache"])
    env = Env(view)
    lone = set(files) == {"db"}
    remote, file = (REMOTE, "db") if lone else ("", "any")
    counts = {"soft_deleted": 0, "lastsynced_copies": 0}
    undecided = {}
    for r in view.get("Events", []):
        e = expect_event(r, env, remote, file)
        exp["records"].append(e)
        key = COUNT_NOTE.get(e["payload"].get("deleted", {}).get("source"))
        if key:
            counts[key] += 1
        raw = e["payload"]["raw"]
        for key, col in (("deleted_state", "Events.deleted"), ("lastSynced_state", "Events.lastSynced"),
                         ("dirty_state", "Events.dirty"), ("deleted_via_state", "Calendars.deleted")):
            if raw.get(key) == "undecided":
                undecided[col] = undecided.get(col, 0) + 1
    notes = exp["notes"]
    notes["database_version"] = str(user_version)
    for k in ("soft_deleted", "lastsynced_copies"):
        if counts[k]:
            notes[k] = str(counts[k])
    if view.get("Instances"):
        notes["rows.Instances"] = str(len(view["Instances"]))
    for m in view.get("CalendarMetaData", [])[:1]:
        for k in ("localTimezone", "minInstance", "maxInstance"):
            if m.get(k) is not None:
                notes["meta." + k] = str(jv(m[k]))
    for c in view.get("CalendarCache", []):
        k = jv(c.get("key"))
        if k in ("timezoneType", "timezoneInstances", "timezoneInstancesPrevious") and c.get("value") is not None:
            notes["cache." + k] = str(jv(c["value"]))
    # 4I3-P9 (spec B9): one counted warning per column after Run, in this order
    for col in ("Events.deleted", "Events.lastSynced", "Events.dirty", "Calendars.deleted"):
        if undecided.get(col):
            table = col.split(".")[0]
            exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:table=" + table,
                                    "contains": "calendar-flag-undecided: %s: %d records" % (col, undecided[col])})
    return exp


# ---------------------------------------------------------------- variants


def create_db(path, phase0, user_version, page_size=4096):
    con = C.connect(path, page_size=page_size)
    con.executescript(phase0)
    con.execute("pragma user_version = %d" % user_version)
    return con


def fill(con, events, ids=None, meta=False):
    ids = set(r["_id"] for r in events) if ids is None else ids
    kids = core_children(ids)
    with C.batch(con):
        for r in calendars():
            C.insert(con, "Calendars", r)
        for r in events:
            C.insert(con, "Events", r)
        for t in CHILD_TABLES:
            for r in kids[t]:
                C.insert(con, t, r)
        if meta:
            C.insert(con, "CalendarMetaData", META)
            for r in CACHE:
                C.insert(con, "CalendarCache", r)


def core_subset(ids):
    return [r for r in core_events() if r["_id"] in ids]


def variant_601(work):
    """CORE rows 1-41 in the database file; row 42 in a committed WAL frame, row 43 in a WAL frame that is never committed
    (the writer dies inside its transaction after the frames spilled)."""
    ddl = "601.sql"
    path = os.path.join(work, "c.db")
    ph = C.load_ddl(ddl, SCHEMAS)
    con = create_db(path, ph[0], user_version_of(ddl))
    fill(con, core_events(), meta=True)
    con.execute("pragma journal_mode = wal")
    con.close()
    r42 = ev(42, title="Only in the WAL, committed")
    r43 = ev(43, title="Only in the WAL, uncommitted")
    # the long text is built by the engine (printf), so the child's command line stays short
    stmts = ["pragma wal_autocheckpoint = 0", insert_sql(r42), "pragma cache_size = 1", "pragma cache_spill = 1", "BEGIN", insert_sql(r43),
             "update Events set description = 'spill ' || printf('%036000d', 0) where _id = 43"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    if b"Only in the WAL, uncommitted" not in files["wal"]:
        raise SystemExit("calendar-601: row 43 did not reach the WAL (no spill)")
    exp = oracle(files, ddl, "calendar-601", user_version_of(ddl))
    # the reader's warning about the uncommitted frames comes before the warnings Parse raises after Run (4I2 PF13a)
    exp["warnings"].insert(0, {"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
    return files, exp


def insert_sql(row):
    cols = list(row)
    return "insert into Events(%s) values (%s)" % (",".join(cols), ",".join(C.lit(row[c]) for c in cols))


def variant_600(work):
    """CORE rows 1-6, then a writer that dies inside a transaction that rewrote row 6's title (with enough overflow pages
    to spill into the database file): the hot journal rolls it back."""
    ddl = "600.sql"
    path = os.path.join(work, "c.db")
    ph = C.load_ddl(ddl, SCHEMAS)
    con = create_db(path, ph[0], user_version_of(ddl))
    fill(con, core_subset(range(1, 7)))
    con.execute("pragma journal_mode = delete")
    con.close()
    stmts = ["pragma cache_size = 2", "pragma cache_spill = 1", "BEGIN",
             "update Events set title = 'Interrupted ' || printf('%040000d', 0) where _id = 6"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "journal": C.read(path + "-journal")}
    exp = oracle(files, ddl, "calendar-600", user_version_of(ddl))
    exp["warnings"].insert(0, {"table": "", "rowid": 0, "contains": "journal-hot"})  # the reader's warning, not a row's
    return files, exp


def variant_502(work):
    """CORE rows 1-6 and freelist pages: a scratch table filled and dropped."""
    ddl = "502.sql"
    path = os.path.join(work, "c.db")
    ph = C.load_ddl(ddl, SCHEMAS)
    con = create_db(path, ph[0], user_version_of(ddl))
    fill(con, core_subset(range(1, 7)))
    con.execute("create table scratch(k integer primary key, v text)")
    with C.batch(con):
        for k in range(1, 61):
            con.execute("insert into scratch values (?, ?)", (k, "scratch row %d " % k + "s" * 400))
    con.execute("drop table scratch")
    if con.execute("pragma freelist_count").fetchone()[0] < 3:
        raise SystemExit("calendar-502: no freelist pages")
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, ddl, "calendar-502", user_version_of(ddl))


def variant_403(work):
    ddl = "403.sql"
    path = os.path.join(work, "c.db")
    con = create_db(path, C.load_ddl(ddl, SCHEMAS)[0], user_version_of(ddl))
    fill(con, core_subset(range(1, 7)))
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, ddl, "calendar-403", user_version_of(ddl))


def variant_upgraded(work):
    """A 403 database upgraded to 601: rows 1-3 (and the calendars) written before the ALTERs are short records; rows 4-6
    are written after them."""
    ddl = "403-upgraded-601.sql"
    path = os.path.join(work, "c.db")
    ph = C.load_ddl(ddl, SCHEMAS)
    con = create_db(path, ph[0], 403)
    fill(con, core_subset(range(1, 4)), ids={1, 2, 3})
    con.executescript(ph[1])
    con.execute("pragma user_version = %d" % user_version_of(ddl))
    with C.batch(con):
        for r in core_subset(range(4, 7)):
            C.insert(con, "Events", r)
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, ddl, "calendar-403-upgraded-601", user_version_of(ddl))


def variant_otherversion(work):
    """601.sql with PRAGMA user_version = 307 (outside the band 400-699, B4): the parser refuses it, no record."""
    ddl = "601.sql"
    path = os.path.join(work, "c.db")
    con = create_db(path, C.load_ddl(ddl, SCHEMAS)[0], 307)
    fill(con, core_subset(range(1, 4)))
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, ddl, "calendar-otherversion", 307, refused=True)


def variant_noevents(work):
    """Only the Calendars table of 601.sql, user version 601 (F05), one calendar: no Events table, no record."""
    ddl = "601.sql"
    path = os.path.join(work, "c.db")
    create = [s for s in C.load_ddl(ddl, SCHEMAS)[0].splitlines() if s.startswith("CREATE TABLE Calendars ")]
    con = create_db(path, "\n".join(create), 601)
    with C.batch(con):
        C.insert(con, "Calendars", calendars()[0])
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, ddl, "calendar-noevents", 601, refused=True)


def variant_deleted(work):
    """CORE rows 1-6 live; Events rows 50-59 (calendar 1) deleted on 512-byte pages, so their leaf pages go to the
    freelist (secure_delete OFF; the typed recovery oracle is a lower bound, 4I2 PF13); an Attendees and a Reminders row
    of event 50 written AFTER the delete, so they survive in the live view (the provider's cleanup trigger would have
    removed them, F17); then, in a WAL that is never checkpointed, row 6 is edited twice: the database file holds its
    original image and the first WAL frame an older image than the live one."""
    ddl = "601.sql"
    path = os.path.join(work, "c.db")
    con = create_db(path, C.load_ddl(ddl, SCHEMAS)[0], user_version_of(ddl), page_size=512)
    gone = [ev(i, title="Deleted %d" % i, dtstart=D + i * H, dtend=D + i * H + H, description="removed event %d " % i + "r" * 300)
            for i in range(50, 60)]
    fill(con, core_subset(range(1, 7)) + gone, ids=set(range(1, 7)))
    free = FREELIST_SCENARIOS["calendar-deleted"] = C.Scenario(CLAIMED)
    free.write("Events", free.snap(con, "Events"))
    free.want("Events", free.snap(con, "Events", "_id between 50 and 59"), label="Events")
    scen = SCENARIOS["calendar-deleted"] = C.Scenario(CLAIMED)
    scen.write("Events", scen.snap(con, "Events"))
    six = scen.snap(con, "Events", "_id = 6")  # its original image, later superseded in the WAL
    # a one-page scratch table dropped first becomes the freelist trunk, so every page the delete frees is a freelist
    # LEAF (a trunk page's list overwrites the cells it held); each deleted row fills a 512-byte page of its own
    con.execute("create table scratch(k integer primary key, v text)")
    con.execute("insert into scratch values (1, 'scratch')")
    con.execute("drop table scratch")
    with C.batch(con):
        con.execute("delete from Events where _id between 50 and 59")
    if con.execute("pragma freelist_count").fetchone()[0] < 3:
        raise SystemExit("calendar-deleted: no freelist pages")
    with C.batch(con):
        C.insert(con, "Attendees", {"_id": 10, "event_id": 50, "attendeeEmail": "late@example.test", "attendeeName": "Late",
                                    "attendeeRelationship": 1, "attendeeType": 1, "attendeeStatus": 1})
        C.insert(con, "Reminders", {"_id": 10, "event_id": 50, "minutes": 15, "method": 1})
    con.execute("pragma journal_mode = wal")
    con.close()
    stmts = ["pragma wal_autocheckpoint = 0", "update Events set title = 'Holiday v2' where _id = 6",
             "update Events set title = 'Holiday v3' where _id = 6"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    v2 = [(6, dict(six[0][1], title="Holiday v2"))]  # the first WAL frame's image, superseded by the second
    scen.write("Events", v2)
    scen.want("Events", six + v2)
    return files, oracle(files, ddl, "calendar-deleted", user_version_of(ddl))


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
    """The <variant>.recover.expect.json of a variant that has a scenario (F24): every recovered Events row is mapped with
    env None, so it joins nothing (B2, 4I3-P4). The digests use the Z form of the times (zform); the full records keep
    the oracle's time encoding (module doc)."""
    def expect_fn(table, d, env, file):
        rec = expect_event(d, None, "", file)
        z = zform(rec)
        z["_oracle_form"] = rec  # both forms travel on the returned object; record_digest reads only the typed keys
        return z

    out = C.recover_oracle(variant, files, SCENARIOS[variant], lambda f: None, expect_fn)
    if variant in FREELIST_SCENARIOS:
        alone = C.recover_oracle(variant, {"db": files["db"]}, FREELIST_SCENARIOS[variant], lambda f: None, expect_fn)
        for k in ("records", "digests", "unmapped"):
            out[k] = alone[k] + out[k]
        seen = set()
        written = []
        for w in alone["written"] + out["written"]:
            key = json.dumps(w, sort_keys=True)
            if key not in seen:
                seen.add(key)
                written.append(w)
        out["written"] = sorted(written, key=lambda w: (w["table"], w["rowid"], json.dumps(w["values"], sort_keys=True)))
    for r in out["records"]:
        r["record"] = r["record"].pop("_oracle_form")
    return out


VARIANTS = {"calendar-601": variant_601, "calendar-600": variant_600, "calendar-502": variant_502, "calendar-403": variant_403,
            "calendar-403-upgraded-601": variant_upgraded, "calendar-otherversion": variant_otherversion,
            "calendar-noevents": variant_noevents, "calendar-deleted": variant_deleted}

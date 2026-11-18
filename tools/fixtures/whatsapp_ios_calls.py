"""iOS WhatsApp CallHistory.sqlite fixtures (plan 4I2, task 7): ios.whatsapp.calls.

    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-calls --out internal/parsers/ioswacalls/testdata \
        --pins-go internal/parsers/ioswacalls/pins_test.go --pins-package ioswacalls_test
    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-calls --check
    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-calls --recover-oracle --out internal/parsers/ioswacalls/testdata

The databases are written by the engine (Python's sqlite3), the WAL variants by a child process that dies without closing
(C.kill_child). The DDL is constructed from memory (schemas/whatsapp-ios/CROSSCHECK.md), so a fixture proves what the parser
does with the column set the sources report, not what an installed app writes.

The oracle is a SECOND READING OF THE SPEC (spec 6.11, 7.4, 7.6, the vocabulary of plan 4I2 4I2-R7 and the rulings 4I2-R4,
PF4, PF6 and PF9) over the stored values read back from the engine on a COPY of the finished files. It shares no code with the
Go parser, but it is written by the same author as the Go design and is not independent of it: a wrong reading shared by both
would pass. The literal facts of oracle_facts_test.go are written beside it for that reason (4I2-R13).

What the oracle states (4I2-R4, PF4, PF6):
  - direction comes only from an aggregate (ZWAAGGREGATECALLEVENT) that links exactly one call event, this one: its ZINCOMING
    0 is out and 1 is in. An aggregate that links several calls states multi-call-aggregate (no direction); a recovered row
    states recovered-row (no direction, never borrowed from a live aggregate).
  - the counterparty (address) is ABSENT: the participant table is not joined, so no participant JID is in any record; only the
    JIDs on the call row itself (ZGROUPJIDSTRING, ZGROUPCALLCREATORUSERJIDSTRING) are in raw.
  - ZVIDEO is NOT mapped (no per-value source): kind is "unknown" and the stored value is in raw.

Variants (the placement of each is in internal/parsers/ioswacalls/fixtures_test.go placeOf):
  ios-callhistory            consumer domain, with -wal: the scenario of this module, call 15 and aggregate 12 in a committed
                             WAL frame, call 16 in an uncommitted one
  ios-callhistory-extract    an app group of a full-file-system extraction, calls 1 and 2
  ios-callhistory-otherapp   a same-named database of another app: one table ZCALL(Z_PK)
  ios-callhistory-deleted    deleted calls (every other one of a run, so no page is freed) through the WAL, among them a call on
                             aggregate A1 whose one live call is another row (plan 4I2 PF2, Review Focus 4)

Python 3 with its bundled sqlite3 only; no Docker. Not part of gen.sh all.
"""
import math
import os
import shutil
import sqlite3
import tempfile

import ios_common as I
import telephony_common as C
import whatsapp_gen as W
import whatsapp_ios_common as X

FAMILY = "whatsapp-ios-calls"
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle, PF2)
CLAIMED = ("ZWACDCALLEVENT",)
ALIAS = "Z_PK"

UDID = "UDID1"
DOMAIN_WA = "AppDomainGroup-" + X.GROUP_WA
REL = "CallHistory.sqlite"
APP_GROUP = "5A0B4C3D-0000-4000-8000-00000000A001"
EXT_DIR = "/private/var/mobile/Containers/Shared/AppGroup/" + APP_GROUP
EXT_REMOTE = EXT_DIR + "/" + REL
EXT_CONTAINER = "ios:HomeDomain/Containers/Shared/AppGroup/" + APP_GROUP

D = 600000100  # ZDATE of call n is D + 100 * n (Cocoa seconds; call 1 is 2020-01-06T10:43:20Z)
MAX_END = 10**9  # the largest duration that derives an end time (restated from the other call parsers)
GRP = X.jid("120363000000000001", "g.us")
CREATOR = X.jid("15550000002", "s.whatsapp.net")
PART1 = X.jid("15550000001", "s.whatsapp.net")
PART2 = X.jid("15550000009", "s.whatsapp.net")

# the Core Data entities of CallHistory as (name, Z_ENT, Z_SUPER): the numbers a parser must not hard-code (4I2-R3)
MODEL = [("WACDCallEvent", 1, 0), ("WAAggregateCallEvent", 2, 0), ("WACDCallEventParticipant", 3, 0)]
E_CALL, E_AGG, E_PART = 1, 2, 3
OUTCOME = {0: "answered", 1: "missed"}
INCOMING = {0: "out", 1: "in"}
# the tables the plan lists as unmapped for the calls parser, in its order ("Notes and warnings (exact texts)")
UNMAPPED = ["ZWACDCALLEVENTPARTICIPANT", "ZWAJOINABLECALLEVENT", "ZWAUPCOMINGCALLEVENT"]
TABLES = ["ZWACDCALLEVENT", "ZWAAGGREGATECALLEVENT", "ZWACDCALLEVENTPARTICIPANT", "Z_PRIMARYKEY"]
MAX_AGG_CALLS = 1000


def jf(x):
    """A float as Go's encoding/json prints it: an integral value without a fraction."""
    if isinstance(x, float) and x.is_integer() and abs(x) < 1e15:
        return int(x)
    return x


# ----------------------------------------------------------------------------------------- the scenario

def date(pk):
    return D + 100 * pk


def call(pk, agg, outcome=0, video=0, **kw):
    """A ZWACDCALLEVENT row: Z1CALLEVENTS (the aggregate), ZOUTCOME, ZVIDEO, then columns. ZDURATION is 0 unless stated."""
    r = dict(Z_PK=pk, Z_ENT=E_CALL, Z_OPT=1, Z1CALLEVENTS=agg, ZOUTCOME=outcome, ZVIDEO=video, ZDATE=date(pk), ZDURATION=0)
    r.update(kw)
    return r


def agg(pk, incoming, **kw):
    r = dict(Z_PK=pk, Z_ENT=E_AGG, Z_OPT=1, ZINCOMING=incoming)
    r.update(kw)
    return r


def aggregates(last=True):
    """A1 (1, 0, 0), A2 (0, 0, 0), A3 (1, 1, 1), A4 (7, 0, 0): ZINCOMING, ZVIDEO, ZMISSED; then A6 to A11 and, with last,
    A12 (the aggregate a committed WAL frame carries); only ZINCOMING."""
    rows = [agg(1, 1, ZVIDEO=0, ZMISSED=0), agg(2, 0, ZVIDEO=0, ZMISSED=0), agg(3, 1, ZVIDEO=1, ZMISSED=1),
            agg(4, 7, ZVIDEO=0, ZMISSED=0), agg(6, 1), agg(7, 1), agg(8, 1), agg(9, 0), agg(10, 1), agg(11, 1)]
    return rows + [agg(12, 1)] if last else rows


def calls(last=True):
    """The calls 1 to 14 of the scenario and, with last, call 15 (in the WAL)."""
    rows = [
        call(1, 1, 0, 0, ZDURATION=61.5),
        call(2, 2, 1, 0),
        call(3, 3, 1, 1),
        call(4, 3, 1, 1),
        call(5, 3, 1, 1),
        call(6, 4, 0, 0),
        call(7, None),
        call(8, 99),
        call(9, 6, 2),
        call(10, 7, 4),
        call(11, 8, 1),
        call(12, 9, 0, 1, ZGROUPJIDSTRING=GRP, ZGROUPCALLCREATORUSERJIDSTRING=CREATOR, ZCALLIDSTRING="C12",
             ZBYTESSENT=1024, ZBYTESRECEIVED=2048),
        call(13, 10, ZDATE=None, ZDURATION=-5),
        call(14, 11, ZVIDEO=2),
        call(17, "A3x"),  # a key that is TEXT: undecided, not "no aggregate"; it is no key the aggregates note counts
    ]
    return rows + [call(15, 12, ZOUTCOME=99)] if last else rows


def participants():
    """Two participant rows of A1: in no record (4I2-R4, the table is not joined)."""
    return [dict(Z_PK=1, Z_ENT=E_PART, Z_OPT=1, Z1PARTICIPANTS=1, ZJIDSTRING=PART1),
            dict(Z_PK=2, Z_ENT=E_PART, Z_OPT=1, Z1PARTICIPANTS=1, ZJIDSTRING=PART2)]


# ----------------------------------------------------------------------------------------- the oracle

def app_label(place, domain):
    """(app, basis, state) of spec 6.8 and 4I2-R5. A backup names the app by its domain; an extraction needs the container
    metadata plist, which these variants do not carry (BundleIOS has no extras): unknown, no-container-metadata."""
    if place == "backup":
        return "whatsapp", "backup_domain", None
    return "unknown", None, "no-container-metadata"


def duration_of(c, raw, p):
    """ZDURATION in seconds: duration_s in the payload when it is finite and not negative; a negative or non-finite value is
    stated in raw with its status (restated from the other call parsers); a non-number is a type-mismatch."""
    v = c.row.get("ZDURATION")
    if v is None:
        return None
    if not X.is_number(v):
        c.unknown[c.prefix + "ZDURATION"] = "type-mismatch"
        return None
    f = float(v)
    if not math.isfinite(f):
        raw.update(I.real_cells("ZDURATION", f))
        return None
    if f < 0:
        raw["ZDURATION"] = jf(v)
        raw["ZDURATION_status"] = "negative"
        return None
    p["duration_s"] = jf(f)
    return f


def direction_of(c, raw, unknown, ctx):
    """The direction of a call and its raw.direction_state (4I2-R7), under the single-call aggregate rule (4I2-R4, PF4)."""
    stored = c.row.get("Z1CALLEVENTS")
    key = c.int("Z1CALLEVENTS")  # a cell that is not an integer is stated in unknown_columns
    if stored is not None:
        raw["Z1CALLEVENTS"] = key if key is not None else X.stored(raw, "Z1CALLEVENTS", stored)  # as stored, whatever the direction
    if ctx["aggs"] is None:  # a recovered row: the live aggregate says nothing about it (Review Focus 4, P8)
        raw["direction_state"] = "recovered-row"
        return "unknown"
    if stored is not None and key is None:  # not NULL, not an integer: no key was read, so no "no-aggregate" (unknown is never absent, 4I2-A4)
        raw["direction_state"] = "undecided"
        return "unknown"
    if key is None or key == 0:
        raw["direction_state"] = "no-aggregate"
        return "unknown"
    n = ctx["aggs"][key]  # the calls that name this aggregate, this one included
    row = ctx["agg_rows"].get(key)
    if row is not None:  # the stored columns are stated whatever the number of calls (4I2-A4); only the direction is limited
        a = X.Cells(row, {}, unknown, "ZWAAGGREGATECALLEVENT.")
        stated = {"Z_PK": key}
        for name in ("ZINCOMING", "ZVIDEO", "ZMISSED", "ZMISSEDREASON"):
            v = a.int(name)
            if v is not None:
                stated[name] = v
        raw["aggregate"] = stated
    if n >= 2:
        raw["direction_state"] = "multi-call-aggregate"
        raw["aggregate_calls"] = min(n, MAX_AGG_CALLS)
        if n > MAX_AGG_CALLS:
            raw["aggregate_calls_truncated"] = True
        return "unknown"
    if row is None:
        raw["direction_state"] = "no-ZWAAGGREGATECALLEVENT-row"
        return "unknown"
    if stated.get("ZINCOMING") in INCOMING:
        return INCOMING[stated["ZINCOMING"]]
    raw["direction_state"] = "incoming-unknown"
    return "unknown"


def expect_call(r, ctx):
    """The expectation of one ZWACDCALLEVENT row as the engine reads it back (spec 6.11, 7.4, 7.6; the 4I2-R7 vocabulary)."""
    raw = {"app": ctx["app"]}
    if ctx["app_basis"]:
        raw["app_basis"] = ctx["app_basis"]
    if ctx["app_state"]:
        raw["app_state"] = ctx["app_state"]
    unknown = {}
    c = X.Cells(r, raw, unknown, "ZWACDCALLEVENT.")
    p = {}
    code = X.read_code(c, "ZOUTCOME")
    X.read_code(c, "ZVIDEO")  # not mapped (PF6): the stored value is in raw, the kind is unknown
    for n in ("Z_ENT", "ZBYTESSENT", "ZBYTESRECEIVED"):
        v = c.int(n)
        if v is not None:
            raw[n] = v
    for n in ("ZGROUPJIDSTRING", "ZGROUPCALLCREATORUSERJIDSTRING", "ZCALLIDSTRING"):  # the call row's own JIDs: raw only (4I2-R4)
        b = c.text(n)
        if b is not None:
            raw[n] = X.cleaned(raw, n, b)

    direction = direction_of(c, raw, unknown, ctx)
    outcome = "unknown"
    if code == 0:
        outcome = "answered"
    elif code == 1:
        if direction == "in":
            outcome = "missed"
        else:
            raw["label_ZOUTCOME"] = "missed"  # a missed code on a call that is not known incoming is kept, not an outcome

    t = X.read_instant(c, "ZDATE")
    dur = duration_of(c, raw, p)
    t_end = None
    if t is not None and dur is not None and 0 < dur <= MAX_END:
        t_end = I.cocoa_iso(c.row["ZDATE"] + dur, "s")
        if t_end is not None:
            raw["time_end_basis"] = "start+duration"
    if unknown:
        raw["unknown_columns"] = unknown

    payload = {"direction": direction, "outcome": outcome, "kind": "unknown", "service": "whatsapp", "raw": raw}
    payload.update(p)
    return {"table": "ZWACDCALLEVENT", "rowid": r["rowid"], "file": "any", "type": "call", "source_path": "", "time": t,
            "time_end": t_end, "times": {}, "deleted": False, "summary": "call %s %s" % (direction, outcome), "body": None,
            "payload": payload}


def context(view, place, domain, recovered=False):
    """The oracle's view of a finished store. aggs maps an aggregate key to the number of calls that name it (None for a
    recovered row, which borrows nothing)."""
    ctx = {"agg_rows": {r["rowid"]: r for r in view.get("ZWAAGGREGATECALLEVENT", [])}, "place": place, "domain": domain}
    counts = {}
    for r in view.get("ZWACDCALLEVENT", []):
        k = r.get("Z1CALLEVENTS")
        if X.is_int(k) and k != 0:
            counts[k] = counts.get(k, 0) + 1
    ctx["aggs"] = None if recovered else counts
    ctx["keys"] = len(counts)
    ctx["app"], ctx["app_basis"], ctx["app_state"] = app_label(place, domain)
    return ctx


def oracle(files, variant, place, domain):
    """The <variant>.expect.json of a variant whose parse succeeds: every ZWACDCALLEVENT row is one record."""
    view = C.engine_view(files, TABLES)
    ctx = context(view, place, domain)
    exp = C.expect_header(FAMILY, variant, "callhistory.sql")
    for r in view.get("ZWACDCALLEVENT", []):
        exp["records"].append(expect_call(r, ctx))
    notes = exp["notes"]
    notes["container"] = "backup_domain" if place == "backup" else "app_group"
    notes["app"] = ctx["app"]
    if ctx["app_basis"]:
        notes["app_basis"] = ctx["app_basis"]
    if ctx["app_state"]:
        notes["app_state"] = ctx["app_state"]
    present = {n for t, n, _ in W.master_rows(files) if t == "table"}
    un = [t for t in UNMAPPED if t in present]
    if un:
        notes["unmapped_tables"] = ",".join(un)
    notes["aggregates"] = str(ctx["keys"])
    return exp


def unparsed(variant, ddl=""):
    """A variant the parser refuses (another app's database): no records, no notes."""
    return C.expect_header(FAMILY, variant, ddl)


# ----------------------------------------------------------------------------------------- building

def build_db(work, tables, page_size=4096, wal=False):
    """A CallHistory database: tables = {table: [row, ...]} inserted in order; the Z_PRIMARYKEY rows of MODEL, the Z_METADATA
    row and Z_MAX last (X.set_z_max)."""
    path = work + "/m.db"
    con = C.connect(path, page_size=page_size)
    con.executescript(X.load_ios_ddl("callhistory.sql")[0])
    with C.batch(con):
        for table, rows in tables.items():
            for row in rows:
                C.insert(con, table, row)
        for table, row in X.primary_key_rows(MODEL):
            C.insert(con, table, row)
        X.set_z_max(con)
        C.insert(con, *X.metadata_row())
    if wal:
        con.execute("pragma journal_mode = wal")
    con.close()
    return path


def finish(path):
    files = {"db": C.read(path)}
    if os.path.exists(path + "-wal"):
        files["wal"] = C.read(path + "-wal")
    return files


def v_callhistory(work):
    """The scenario in the main file (calls 1-14, aggregates 1-11), call 15 and aggregate 12 in a committed WAL frame (with
    Z_MAX of both entities, so that Z_MAX stays the real maximum) and call 16 in frames written without a commit (a large row
    that spilled from a tiny page cache before the kill): the engine sees calls 1-15."""
    path = build_db(work, {"ZWAAGGREGATECALLEVENT": aggregates(False), "ZWACDCALLEVENT": calls(False),
                           "ZWACDCALLEVENTPARTICIPANT": participants()}, wal=True)
    big = call(16, None, ZCALLIDSTRING="@@BIG@@")
    big_stmt = X.insert_stmt("ZWACDCALLEVENT", big).replace("'@@BIG@@'", "replace(hex(zeroblob(100000)), '0', 'x')")
    X.leave_old_frame(path, ["pragma wal_autocheckpoint = 0", "begin", X.insert_stmt("ZWAAGGREGATECALLEVENT", aggregates()[-1]),
                             X.insert_stmt("ZWACDCALLEVENT", calls()[-1]),
                             "update Z_PRIMARYKEY set Z_MAX = 15 where Z_ENT = %d" % E_CALL,
                             "update Z_PRIMARYKEY set Z_MAX = 12 where Z_ENT = %d" % E_AGG, "commit",
                             "pragma cache_size = 5", "pragma cache_spill = 1", "begin", big_stmt])
    files = finish(path)
    assert X.uncommitted_frames(files["wal"], 4096) > 0, "the WAL holds no frame after its last commit"
    exp = oracle(files, "ios-callhistory", "backup", DOMAIN_WA)
    # the reader's warning: rowrun forwards it when Run ends (no per-table join warning follows: every join is decided)
    exp["warnings"].append({"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
    return files, exp


def v_extract(work):
    path = build_db(work, {"ZWAAGGREGATECALLEVENT": aggregates(False)[:2], "ZWACDCALLEVENT": calls(False)[:2]})
    files = finish(path)
    return files, oracle(files, "ios-callhistory-extract", "extract", "")


def v_otherapp(work):
    path = work + "/o.db"
    con = C.connect(path)
    con.execute("create table ZCALL(Z_PK integer primary key)")
    with C.batch(con):
        con.execute("insert into ZCALL values (1)")
    con.close()
    return {"db": C.read(path)}, unparsed("ios-callhistory-otherapp")


# ----------------------------------------------------------------------------------------- typed recovery (plan 4I2 PF2)

def bulk(first, n):
    """n plain calls with ids first.. (the rows the deleted variant writes and deletes): no aggregate, so that no live
    aggregate count changes. The 140-byte call id sets the row size: with it the deleting transaction frees no page (v_deleted
    asserts that); another size may make SQLite merge two half-empty leaves."""
    return [call(i, None, i % 2, 0, ZDATE=D + 5000 + i, ZDURATION=i % 7 + 0.5,
                 ZCALLIDSTRING=("bulk call %d " % i).ljust(140, ".")) for i in range(first, first + n)]


def new_scenario(variant, con):
    scen = SCENARIOS[variant] = C.Scenario(CLAIMED, alias=ALIAS)
    scen.snap(con, "ZWACDCALLEVENT", "0")
    return scen


def freelist_count(files):
    """The freelist page count of the finished files as the engine sees them (a WAL is applied to a COPY)."""
    work = tempfile.mkdtemp(prefix="telephony-free-")
    try:
        path = work + "/f.db"
        for role, suffix in (("db", ""), ("wal", "-wal")):
            if role in files:
                with open(path + suffix, "wb") as f:
                    f.write(files[role])
        con = sqlite3.connect(path)
        n = con.execute("pragma freelist_count").fetchone()[0]
        con.close()
        return n
    finally:
        shutil.rmtree(work, ignore_errors=True)


def v_deleted(work):
    """Calls 1 and 2 live (on the aggregates A1 and A2); 60 bulk calls (ids 100-159) and calls 90-91 in the main file; one
    committed WAL transaction inserts calls 92-93 and the next deletes 90-93 and the even ids of 100-159. The odd ids stay live,
    so every leaf keeps half its cells and no page is freed: a page that later belongs to the freelist makes the engine state
    relation unknown with owner-changed (internal/sqlitefile/rowsident.go, ruling C44), which the oracle walker does not model.
    The main file still holds the deleted rows (versions that are history under the WAL) and the first WAL frame holds 92-93
    (the old frame). The deleted calls: 90 is on aggregate A1, whose one live call is call 1 (Review Focus 4: a recovered row
    never borrows that direction); 91 names no aggregate; 92 names A1 as well and has no date; 93 names an aggregate with no
    row. Deleted rows are found only through the WAL here: the history oracle (sqlite_oracle.build_history) reads freelist
    leaves only of a store with no WAL or journal. Every deleted row has a fractional ZDURATION: a whole-number REAL is stored
    as an integer in the record, which the page walker reads back as an integer, not as the float the engine returns."""
    deleted = [call(90, 1, 0, 0, ZDURATION=30.5), call(91, None, 1, 0, ZDURATION=12.25)]
    old = [call(92, 1, 1, 0, ZDATE=None, ZDURATION=7.75), call(93, 99, 0, 1, ZDURATION=2.5)]
    rows = calls(False)[:2] + deleted + bulk(100, 60)
    path = build_db(work, {"ZWAAGGREGATECALLEVENT": aggregates(False)[:2], "ZWACDCALLEVENT": rows}, wal=True)
    con = C.connect(path)
    scen = new_scenario("ios-callhistory-deleted", con)
    scen.write("ZWACDCALLEVENT", scen.snap(con, "ZWACDCALLEVENT"))
    scen.want("ZWACDCALLEVENT", scen.snap(con, "ZWACDCALLEVENT", "(Z_PK between 100 and 159 and Z_PK % 2 = 0) or Z_PK in (90, 91)"))
    con.close()
    # the rows the first WAL transaction inserts, as the engine would store them
    shadow = work + "/shadow.db"
    sc = C.connect(shadow)
    sc.executescript(X.load_ios_ddl("callhistory.sql")[0])
    for m in old:
        C.insert(sc, "ZWACDCALLEVENT", m)
    old_rows = scen.snap(sc, "ZWACDCALLEVENT")
    sc.close()
    scen.write("ZWACDCALLEVENT", old_rows)
    scen.want("ZWACDCALLEVENT", old_rows)
    X.leave_old_frame(path, ["pragma wal_autocheckpoint = 0"] + [X.insert_stmt("ZWACDCALLEVENT", m) for m in old] +
                      ["delete from ZWACDCALLEVENT where Z_PK in (90, 91, 92, 93) or (Z_PK between 100 and 159 and Z_PK % 2 = 0)"])
    files = finish(path)
    assert "wal" in files and freelist_count(files) == 0, "a page was freed: the oracle walker does not model that"
    return files, oracle(files, "ios-callhistory-deleted", "backup", DOMAIN_WA)


def recover_env(files):
    """The live view of the finished files with no aggregate counts (a recovered row borrows nothing); the place is the
    consumer backup domain (PF5: a recovered row at a domain place states its domain label)."""
    return context(C.engine_view(files, TABLES), "backup", DOMAIN_WA, recovered=True)


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (ruling 4G-R14)."""
    def one(table, d, env, file):
        rec = expect_call(d, env)
        rec["source_path"], rec["file"] = "", file  # a recovered record: the source path follows the role of the file
        return rec

    return C.recover_oracle(variant, files, SCENARIOS[variant], recover_env, one)


VARIANTS = {
    "ios-callhistory": v_callhistory,
    "ios-callhistory-extract": v_extract,
    "ios-callhistory-otherapp": v_otherapp,
    "ios-callhistory-deleted": v_deleted,
}

"""iOS WhatsApp ContactsV2.sqlite fixtures (plan 4I2, task 9): ios.whatsapp.contacts.

    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-contacts --out internal/parsers/ioswacontacts/testdata \
        --pins-go internal/parsers/ioswacontacts/pins_test.go --pins-package ioswacontacts_test
    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-contacts --check
    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-contacts --recover-oracle --out internal/parsers/ioswacontacts/testdata

The databases are written by the engine (Python's sqlite3), the WAL variants by a child process that dies without closing
(C.kill_child). The DDL is constructed from memory (schemas/whatsapp-ios/CROSSCHECK.md), so a fixture proves what the parser
does with the column set the sources report, not what an installed app writes.

The oracle is a SECOND READING OF THE SPEC (spec 6.11 ios.whatsapp.contacts, D6, the vocabulary of plan 4I2 4I2-R7 and the
rulings 4I2-R6, 4I2-R10 and PF13) over the stored values read back from the engine on a COPY of the finished files. It shares
no code with the Go parser, but it is written by the same author as the Go design and is not independent of it: a wrong
reading shared by both would pass. The literal facts of oracle_facts_test.go are written beside it for that reason (4I2-R13).

What the oracle states:
  - every ZWAADDRESSBOOKCONTACT row is a record or one counted warning (4I2-R10). A row names no person when its JID parses to a
    class other than user or lid (group, broadcast, status, newsletter, other, unparseable), or when it has neither a full name
    nor a number (the Android wa.db rule restated: a JID-only row, a row with a given or highlighted name only, a row whose name
    cell is not text). The JID class is decided first, on the stored bytes; a JID that cleaning changes is `altered`, an over-long
    one `too-long` (checked first, no raw text), a cell that is not TEXT `not-text`: none of them is parsed, so none makes a
    row a non-person.
  - the phone label `_$!<X>!$_` is decoded as ios.addressbook does (the rule restated in ios_addressbook.py: the wrapper with a
    non-empty X gives X, the stored label in raw.phone_label_stored); any other label text is kept as it is.
  - the LID statement is the row's own ZLID (D6): raw.lid_map only for a non-empty unaltered ZLID on a row whose ZWHATSAPPID is a
    user-class JID; with a non-empty ZLID otherwise raw.lid_state says why (amendment 4I2-A5: jid-undecided for a JID that is
    altered, too-long or not text, jid-not-a-phone for a decided JID of another class or an absent one) and raw.ZLID keeps the
    stored LID unless it is too long. A column the variant lacks adds nothing. A JID cell that is not text keeps its stored value
    in raw.ZWHATSAPPID; a row with no name or number and an undecided JID is the warning "(JID undecided)", never "(JID only)".
  - ZABOUTTEXT is in the Body only (cut at the text cap with its name in raw.text_flags); ZABOUTTIMESTAMP is raw, never a time.

Variants (the placement of each is in internal/parsers/ioswacontacts/fixtures_test.go placeOf):
  ios-contactsv2           consumer domain, with -wal: rows 1-13, 15 and 17 in the main file, row 14 in a committed WAL frame, row 16 in an uncommitted one
  ios-contactsv2-nofullname  consumer domain, no ZFULLNAME column, rows 1, 3, 4 (PF16(b))
  ios-contactsv2-nolid     consumer domain, no ZLID column (S7 saw it absent on some images), rows 1-3
  ios-contactsv2-extract   an app group of a full-file-system extraction, rows 1-3
  ios-contactsv2-otherapp  a same-named database of another app: one table ZCONTACT(Z_PK)
  ios-contactsv2-deleted   deleted contacts (every other one of a run, so no page is freed) through the WAL, among them a
                           non-person row (plan 4I2 PF2)

Python 3 with its bundled sqlite3 only; no Docker. Not part of gen.sh all.
"""
import os
import shutil
import sqlite3
import tempfile

import telephony_common as C
import whatsapp_gen as W
import whatsapp_ios_common as X

FAMILY = "whatsapp-ios-contacts"
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle, PF2)
CLAIMED = ("ZWAADDRESSBOOKCONTACT",)
ALIAS = "Z_PK"
TABLE = "ZWAADDRESSBOOKCONTACT"
PREFIX = TABLE + "."

DOMAIN_WA = "AppDomainGroup-" + X.GROUP_WA
REL = "ContactsV2.sqlite"
APP_GROUP = "5A0B4C3D-0000-4000-8000-00000000A001"
EXT_DIR = "/private/var/mobile/Containers/Shared/AppGroup/" + APP_GROUP
EXT_REMOTE = EXT_DIR + "/" + REL

MODEL = [("WAAddressBookContact", 1, 0)]
E_CONTACT = 1
MAX_ADDR = W.MAX_ADDR  # 1024 bytes: the message contract's maxAddr (whatsapp_gen.MAX_ADDR) and Android wa.db's maxJID; the contact contract has no JID limit of its own, so the parser restates this one
PERSON = {"user", "lid"}
TABLES = [TABLE, "Z_PRIMARYKEY"]

# the warning texts of the plan's vocabulary ("State vocabularies", ios.whatsapp.contacts), byte-exact
W_NO_ID = "ZWAADDRESSBOOKCONTACT row has no name, number or JID"  # PF16(c)
W_JID_ONLY = "ZWAADDRESSBOOKCONTACT row has no name or number (JID only)"
W_JID_UNDECIDED = "ZWAADDRESSBOOKCONTACT row has no name or number (JID undecided)"  # final review M3
W_NO_FULL = "ZWAADDRESSBOOKCONTACT row has no full name or number"
W_DAMAGED = "ZWAADDRESSBOOKCONTACT row has no readable name or number (a name column is damaged or not text)"


def w_not_person(cls):
    return "ZWAADDRESSBOOKCONTACT row is not a person (%s)" % cls


U = lambda n: X.jid("1555000%04d" % n, "s.whatsapp.net")  # noqa: E731  (fictional 555 numbers)
LONG_ABOUT = ("The quick brown fox jumps over the lazy dog. " * 7000)[:300 << 10]
BIG_JID = "7" * 2000 + "@s.whatsapp.net"  # over the 1024-byte JID limit (MAX_ADDR) and not cut by cleaning: too-long, no text kept


# ----------------------------------------------------------------------------------------- the scenario

def row(pk, **kw):
    """A ZWAADDRESSBOOKCONTACT row: Z_PK, Z_ENT, Z_OPT, then the columns given."""
    r = dict(Z_PK=pk, Z_ENT=E_CONTACT, Z_OPT=1)
    r.update(kw)
    return r


def contacts():
    """The rows 1 to 13 of the plan (Task 9) and the two rows of the writer: 14 (an over-long JID) and 15 (a JID that is a
    BLOB). A column a variant lacks is dropped on insert (C.insert)."""
    return [
        row(1, ZFULLNAME="Ann Lee", ZGIVENNAME="Ann", ZPHONENUMBER="+15550000001", ZPHONENUMBERLABEL="_$!<Mobile>!$_",
            ZWHATSAPPID=U(1), ZABOUTTEXT="busy", ZABOUTTIMESTAMP=600000000, ZIDENTIFIER="A1B2C3", ZLID=X.jid("100000000000001", "lid")),
        row(2, ZPHONENUMBER="+15550000002", ZPHONENUMBERLABEL="Work line"),
        row(3, ZFULLNAME="Bob", ZWHATSAPPID=U(3)),
        row(4, ZWHATSAPPID=U(4)),
        row(5, ZFULLNAME="Group", ZWHATSAPPID=X.jid("120363000000000001", "g.us")),
        row(6, ZGIVENNAME="Cy", ZWHATSAPPID=U(6)),
        row(7, ZFULLNAME="Dee", ZPHONENUMBER="+15550000007", ZWHATSAPPID=None),
        row(8, ZFULLNAME="Eve", ZWHATSAPPID=C.Raw(b"15550000008\x00@s.whatsapp.net")),
        row(9, ZFULLNAME="Fay", ZWHATSAPPID=X.jid("100000000000009", "lid"), ZLID=X.jid("100000000000009", "lid")),
        row(10, ZFULLNAME="Gus", ZABOUTTEXT=LONG_ABOUT),
        row(11, ZFULLNAME="Ann L.", ZWHATSAPPID=U(1)),  # the JID of row 1 again: two records, never merged (Review Focus 5)
        row(12, ZFULLNAME="Hal", ZWHATSAPPID="-1"),
        row(13, ZFULLNAME=C.Blob(b"\xff\x01Ida"), ZWHATSAPPID=U(13)),
        row(14, ZFULLNAME="Ivy", ZHIGHLIGHTEDNAME="Ivy H", ZWHATSAPPID=BIG_JID),
        row(15, ZFULLNAME="Jon", ZHIGHLIGHTEDNAME="Jon H", ZWHATSAPPID=C.Blob(b"15550000015@s.whatsapp.net")),
        row(16, ZFULLNAME="Kay", ZABOUTTEXT="@@BIG@@"),  # left in an uncommitted WAL frame: no record (the reader's warning)
        row(17, ZIDENTIFIER="X17"),  # a NULL JID and no naming text (PF16(c))
    ]


def pick(rows, ids):
    return [r for r in rows if r["Z_PK"] in ids]


# ----------------------------------------------------------------------------------------- the oracle

def jid_class(s):
    """decode/wajid classification restated: user, group, broadcast, status, lid, newsletter, other or unparseable."""
    p = W.parse_jid(s)
    if p is None:
        return "unparseable"
    user, server = p
    if server == "s.whatsapp.net":
        return "user"
    if server == "g.us":
        return "group"
    if server == "broadcast":
        return "status" if user == "status" else "broadcast"
    if server == "lid":
        return "lid"
    if server == "newsletter":
        return "newsletter"
    return "other"


def app_label(place, domain):
    """(app, basis, state) of spec 6.8 and 4I2-R5. A backup names the app by its domain; an extraction needs the container
    metadata plist, which these variants do not carry (BundleIOS has no extras): unknown, no-container-metadata."""
    if place == "backup":
        return "whatsapp", "backup_domain", None
    return "unknown", None, "no-container-metadata"


def wrapped(label):
    """The label rule of ios.addressbook (ios_addressbook.wrapped): `_$!<X>!$_` with a non-empty X is the system label X."""
    if label.startswith("_$!<") and label.endswith(">!$_") and len(label) > 8:
        return label[4:-4]
    return None


def jid_cell(c, name):
    """A JID cell decided on its stored bytes: (state, cleaned text). absent: NULL or empty; not-text: another class (stated in
    unknown_columns by the cell reader); too-long (checked first, no text kept): over the contract's limit or cut by cleaning;
    altered: cleaning changed it, never parsed; else state None."""
    if c.row.get(name) is None:
        return "absent", None
    b = c.text(name)
    if b is None:
        return "not-text", None
    if len(b) == 0:
        return "absent", None
    cl = W.text_of(b)
    if len(cl.text.encode("utf-8")) > MAX_ADDR or cl.truncated:
        return "too-long", None
    if cl.flags:
        return "altered", cl.text
    return None, cl.text


def has_text(v):
    return W.is_text(v) and len(v) > 0


def classify(r):
    """('record', None) or ('warn', text): what the spec says of one ZWAADDRESSBOOKCONTACT row (4I2-R10, the totality rule)."""
    c = X.Cells(r, {}, {}, PREFIX)
    state, text = jid_cell(c, "ZWHATSAPPID")
    if state is None:
        cls = jid_class(text)
        if cls not in PERSON:
            return "warn", w_not_person(cls)
    if has_text(r.get("ZFULLNAME")) or has_text(r.get("ZPHONENUMBER")):
        return "record", None
    names = ("ZFULLNAME", "ZPHONENUMBER", "ZGIVENNAME", "ZHIGHLIGHTEDNAME")
    # unknown is not absent: a name cell that is not text, or a name or number column the table lacks (PF16(b)), says nothing
    # of the row's names
    if any(n not in r or (r[n] is not None and not W.is_text(r[n])) for n in names):
        return "warn", W_DAMAGED
    if has_text(r.get("ZGIVENNAME")) or has_text(r.get("ZHIGHLIGHTEDNAME")):
        return "warn", W_NO_FULL
    if state == "absent":  # PF16(c): NULL (or empty) JID and no name, number or other naming text
        assert not any(has_text(r.get(n)) for n in names), "W_NO_ID is for a row with no naming text"
        return "warn", W_NO_ID
    if state is not None:  # altered, too-long or not-text: no class is decided, so never "JID only"
        return "warn", W_JID_UNDECIDED
    return "warn", W_JID_ONLY


def expect_contact(r, ctx, file="any"):
    """The expectation of one ZWAADDRESSBOOKCONTACT row as the engine reads it back (None when the row is a counted warning)."""
    if classify(r)[0] != "record":
        return None
    raw = {"app": ctx["app"]}
    if ctx["app_basis"]:
        raw["app_basis"] = ctx["app_basis"]
    if ctx["app_state"]:
        raw["app_state"] = ctx["app_state"]
    unknown = {}
    c = X.Cells(r, raw, unknown, PREFIX)
    p = {}

    def text(name):
        """The cleaned text of a non-empty TEXT cell, its name in raw.text_flags when cleaning changed it; else None."""
        b = c.text(name)
        if b is None or len(b) == 0:
            return None
        return X.cleaned(raw, name, b)

    display, given, phone, ident, about = (text(n) for n in ("ZFULLNAME", "ZGIVENNAME", "ZPHONENUMBER", "ZIDENTIFIER", "ZABOUTTEXT"))
    label = text("ZPHONENUMBERLABEL")
    if display:
        p["display_name"] = display
    if given:
        p["names"] = {"given": given}
    if phone:
        ph = {"value": phone}
        if label:
            x = wrapped(label)
            if x:
                ph["label"] = x
                raw["phone_label_stored"] = label
            else:
                ph["label"] = label
        p["phones"] = [ph]
    else:
        assert not label, "the scenario holds no phone label without a phone"
    if ident:
        p["source_id"] = ident

    state, jid = jid_cell(c, "ZWHATSAPPID")
    if state == "altered":
        raw["whatsapp_id_text"] = jid
        C.mark(raw, "whatsapp_id_text")
    if state == "not-text":  # final review M4: the stored value of a JID cell that is not text, never absent
        raw["ZWHATSAPPID"] = X.stored(raw, "ZWHATSAPPID", r.get("ZWHATSAPPID"))
    if state:
        raw["whatsapp_id_state"] = state
    else:
        p["ims"] = [{"value": jid, "kind": "whatsapp"}]

    # D6: the LID statement of the row itself, never inferred. Amendment 4I2-A5: a JID that is altered, too-long or not text is
    # "jid-undecided", a decided JID of another class or an absent one "jid-not-a-phone"; with no lid_map, raw.ZLID keeps the
    # stored LID (cleaned, text_flags on alteration) unless it is too long.
    lid_cell = r.get("ZLID")
    if lid_cell is not None:
        lb = c.text("ZLID")
        if lb is None:
            raw["lid_state"] = "not-text"
            raw["ZLID"] = X.stored(raw, "ZLID", lid_cell)
        elif len(lb) > 0:
            cl = W.text_of(lb)
            assert len(cl.text.encode("utf-8")) <= MAX_ADDR and not cl.truncated, "the scenario holds no over-long ZLID"
            if cl.flags:
                raw["lid_state"] = "altered"
            elif state is None and jid_class(jid) == "user":
                raw["lid_map"] = [{"lid": cl.text, "jid": jid, "source": "ZWAADDRESSBOOKCONTACT.ZLID"}]
            elif state in ("altered", "too-long", "not-text"):
                raw["lid_state"] = "jid-undecided"
            else:
                raw["lid_state"] = "jid-not-a-phone"
            if "lid_map" not in raw:
                raw["ZLID"] = X.cleaned(raw, "ZLID", lb)

    # raw beside interpreted: stored as they are
    v = c.int("Z_ENT")
    if v is not None:
        raw["Z_ENT"] = v
    hl = r.get("ZHIGHLIGHTEDNAME")
    if hl is not None:
        s = X.stored(raw, "ZHIGHLIGHTEDNAME", hl)
        if s is not None:
            raw["ZHIGHLIGHTEDNAME"] = s
    ts = r.get("ZABOUTTIMESTAMP")
    if ts is not None:
        raw["ZABOUTTIMESTAMP"] = X.stored(raw, "ZABOUTTIMESTAMP", ts)
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw

    body = "\n".join(x for x in (display, phone, about) if x)
    plain = all(x.isascii() and x.isprintable() for x in (display or "", phone or "")) and len(display or "") <= 80 \
        and len((phone or "").encode("utf-8")) <= 32
    summ = " ".join(x for x in (display, phone) if x) if plain else None
    return {"table": TABLE, "rowid": r["rowid"], "file": file, "type": "contact", "source_path": "", "time": None,
            "time_end": None, "times": {}, "deleted": False, "summary": summ, "body": body, "payload": p}


def oracle(files, variant, place, domain):
    """The <variant>.expect.json of a variant whose parse succeeds: every row is one record or one counted warning, in rowid
    order, then the reader's warning when the WAL holds frames after its last commit (PF13a: the warnings of the mappings,
    then those rowrun.Run forwards from the reader)."""
    view = C.engine_view(files, TABLES)
    app, basis, state = app_label(place, domain)
    ctx = {"app": app, "app_basis": basis, "app_state": state}
    exp = C.expect_header(FAMILY, variant, "contactsv2.sql")
    for r in view.get(TABLE, []):
        kind, why = classify(r)
        if kind == "record":
            exp["records"].append(expect_contact(r, ctx))
        else:
            exp["warnings"].append({"table": TABLE, "rowid": r["rowid"], "contains": why})
    if "wal" in files and X.uncommitted_frames(files["wal"], 4096) > 0:
        exp["warnings"].append({"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
    notes = exp["notes"]
    notes["container"] = "backup_domain" if place == "backup" else "app_group"
    notes["app"] = app
    if basis:
        notes["app_basis"] = basis
    if state:
        notes["app_state"] = state
    rows = view.get(TABLE, [])
    if rows and "ZLID" not in rows[0]:
        notes["ZLID"] = "absent"  # PF16(a): the store has no ZLID column
    return exp


def unparsed(variant):
    """A variant the parser refuses (another app's database): no records, no notes."""
    return C.expect_header(FAMILY, variant, "")


# ----------------------------------------------------------------------------------------- building

def build_db(work, rows, page_size=4096, wal=False, lid=True, drop=()):
    """A ContactsV2 database: rows inserted in order; the Z_PRIMARYKEY row of MODEL, the Z_METADATA row and Z_MAX last
    (X.set_z_max). lid=False builds the table without ZLID (S7: absent on some images)."""
    path = work + "/m.db"
    con = C.connect(path, page_size=page_size)
    ddl = X.load_ios_ddl("contactsv2.sql")[0]
    if not lid:
        stripped = ddl.replace(" ZLID VARCHAR,", "")
        assert stripped != ddl, "the DDL snapshot has no ZLID column to drop"
        ddl = stripped
    for col in drop:
        stripped = ddl.replace(" %s VARCHAR," % col, "")
        assert stripped != ddl, "the DDL snapshot has no %s column to drop" % col
        ddl = stripped
    con.executescript(ddl)
    with C.batch(con):
        for r in rows:
            C.insert(con, TABLE, r)
        for table, prow in X.primary_key_rows(MODEL):
            C.insert(con, table, prow)
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


def v_contactsv2(work):
    """The scenario: rows 1-13, 15 and 17 in the main file, row 14 (the over-long JID) in a committed WAL frame and row 16 (a large
    row that spilled from a tiny page cache before the kill) in uncommitted ones: the engine sees rows 1-15 and 17, and the reader
    warns that frames were not applied (stated after the row warnings, PF13a)."""
    rows = contacts()
    path = build_db(work, [r for r in rows if r["Z_PK"] not in (14, 16)], wal=True)
    row14, row16 = pick(rows, {14})[0], pick(rows, {16})[0]
    big = X.insert_stmt(TABLE, row16).replace("'@@BIG@@'", "replace(hex(zeroblob(100000)), '0', 'x')")
    X.leave_old_frame(path, ["pragma wal_autocheckpoint = 0", "begin", X.insert_stmt(TABLE, row14), "commit",
                             "pragma cache_size = 5", "pragma cache_spill = 1", "begin", big])
    files = finish(path)
    assert "wal" in files and X.uncommitted_frames(files["wal"], 4096) > 0, "the WAL holds no frame after its last commit"
    return files, oracle(files, "ios-contactsv2", "backup", DOMAIN_WA)


def v_nolid(work):
    path = build_db(work, pick(contacts(), {1, 2, 3}), lid=False)
    files = finish(path)
    return files, oracle(files, "ios-contactsv2-nolid", "backup", DOMAIN_WA)


def v_nofullname(work):
    """A store whose table lacks ZFULLNAME (PF16(b)): a name column the table lacks is unreadable, never absent. Row 1 has a
    phone and stays a record; rows 3 and 4 (a name or a JID that the missing column hid) are the damaged warning, never
    "JID only"."""
    path = build_db(work, pick(contacts(), {1, 3, 4}), drop=("ZFULLNAME",))
    files = finish(path)
    return files, oracle(files, "ios-contactsv2-nofullname", "backup", DOMAIN_WA)


def v_extract(work):
    path = build_db(work, pick(contacts(), {1, 2, 3}))
    files = finish(path)
    return files, oracle(files, "ios-contactsv2-extract", "extract", "")


def v_otherapp(work):
    path = work + "/o.db"
    con = C.connect(path)
    con.execute("create table ZCONTACT(Z_PK integer primary key)")
    with C.batch(con):
        con.execute("insert into ZCONTACT values (1)")
    con.close()
    return {"db": C.read(path)}, unparsed("ios-contactsv2-otherapp")


# ----------------------------------------------------------------------------------------- typed recovery (plan 4I2 PF2)

def bulk(first, n):
    """n plain contacts with ids first.. (the rows the deleted variant writes and deletes). The 120-byte ZABOUTTEXT sets the row
    size: with it the deleting transaction frees no page (v_deleted asserts that); another size may make SQLite merge two
    half-empty leaves (140 bytes did, 100 and 120 do not: the assertion guards it)."""
    return [row(i, ZFULLNAME="Bulk %d" % i, ZPHONENUMBER="+1555100%04d" % i, ZWHATSAPPID=U(1000 + i),
                ZABOUTTEXT=("bulk contact %d " % i).ljust(120, "."), ZIDENTIFIER="B%04d" % i) for i in range(first, first + n)]


def new_scenario(variant, con):
    scen = SCENARIOS[variant] = C.Scenario(CLAIMED, alias=ALIAS)
    scen.snap(con, TABLE, "0")
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
    """Contacts 1 and 2 live; 60 bulk contacts (ids 100-159) and contacts 90-91 in the main file; one committed WAL transaction
    inserts contacts 92-93 and the next deletes 90-93 and the even ids of 100-159. The odd ids stay live, so every leaf keeps
    half its cells and no page is freed: a page that later belongs to the freelist makes the engine state relation unknown
    with owner-changed (internal/sqlitefile/rowsident.go, ruling C44), which the oracle walker does not model. The main file
    still holds the deleted rows (versions that are history under the WAL) and the first WAL frame holds 92-93 (the old frame).
    The deleted contacts: 90 a full person (name, wrapped label, JID, LID, about text, identifier), 91 a name with no JID,
    92 a highlighted name with a phone and no label, 93 a group row (not a person: reported unmapped, never typed). Deleted rows
    are found only through the WAL here: the history oracle (sqlite_oracle.build_history) reads freelist leaves only of a store
    with no WAL or journal."""
    c = contacts()
    deleted = [row(90, ZFULLNAME="Gil Gone", ZGIVENNAME="Gil", ZPHONENUMBER="+15550000090", ZPHONENUMBERLABEL="_$!<Home>!$_",
                   ZWHATSAPPID=U(90), ZABOUTTEXT="gone", ZABOUTTIMESTAMP=600000090, ZIDENTIFIER="G90",
                   ZLID=X.jid("100000000000090", "lid")),
               row(91, ZFULLNAME="Hana Gone")]
    old = [row(92, ZPHONENUMBER="+15550000092", ZHIGHLIGHTEDNAME="Ivo H", ZWHATSAPPID=U(92)),
           row(93, ZFULLNAME="Old Group", ZWHATSAPPID=X.jid("120363000000000093", "g.us"))]
    rows = pick(c, {1, 2}) + deleted + bulk(100, 60)
    path = build_db(work, rows, wal=True)
    con = C.connect(path)
    scen = new_scenario("ios-contactsv2-deleted", con)
    scen.write(TABLE, scen.snap(con, TABLE))
    scen.want(TABLE, scen.snap(con, TABLE, "(Z_PK between 100 and 159 and Z_PK % 2 = 0) or Z_PK in (90, 91)"))
    con.close()
    # the rows the first WAL transaction inserts, as the engine would store them
    shadow = work + "/shadow.db"
    sc = C.connect(shadow)
    sc.executescript(X.load_ios_ddl("contactsv2.sql")[0])
    for m in old:
        C.insert(sc, TABLE, m)
    old_rows = scen.snap(sc, TABLE)
    sc.close()
    scen.write(TABLE, old_rows)
    scen.want(TABLE, old_rows)
    X.leave_old_frame(path, ["pragma wal_autocheckpoint = 0"] + [X.insert_stmt(TABLE, m) for m in old] +
                      ["delete from %s where Z_PK in (90, 91, 92, 93) or (Z_PK between 100 and 159 and Z_PK %% 2 = 0)" % TABLE])
    files = finish(path)
    assert "wal" in files and freelist_count(files) == 0, "a page was freed: the oracle walker does not model that"
    return files, oracle(files, "ios-contactsv2-deleted", "backup", DOMAIN_WA)


def recover_env(files):
    """The consumer backup domain (PF5: a recovered row at a domain place states its domain label)."""
    app, basis, state = app_label("backup", DOMAIN_WA)
    return {"app": app, "app_basis": basis, "app_state": state}


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (ruling 4G-R14)."""
    def one(table, d, env, file):
        rec = expect_contact(d, env, file)
        if rec is not None:
            rec["source_path"] = ""  # a recovered record: the source path follows the role of the file
        else:
            assert classify(d) == ("warn", w_not_person("group")), "the deleted scenario's only unmapped rows are group rows"
        return rec

    return C.recover_oracle(variant, files, SCENARIOS[variant], recover_env, one, nobody=w_not_person("group"))


VARIANTS = {
    "ios-contactsv2": v_contactsv2,
    "ios-contactsv2-nolid": v_nolid,
    "ios-contactsv2-nofullname": v_nofullname,
    "ios-contactsv2-extract": v_extract,
    "ios-contactsv2-otherapp": v_otherapp,
    "ios-contactsv2-deleted": v_deleted,
}

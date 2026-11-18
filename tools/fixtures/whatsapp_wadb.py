"""Android WhatsApp wa.db fixtures (plan 4I task 12): the contact table wa_contacts.

    py -3 tools/fixtures/telephony_gen.py --family whatsapp-wadb --out internal/parsers/androidwacontacts/testdata \
        --pins-go internal/parsers/androidwacontacts/pins_test.go --pins-package androidwacontacts_test
    py -3 tools/fixtures/telephony_gen.py --family whatsapp-wadb --out internal/parsers/androidwacontacts/testdata --recover-oracle
    py -3 tools/fixtures/telephony_gen.py --family whatsapp-wadb --check

The databases are written by the engine (Python's sqlite3), the WAL variants by a child process that dies without
closing (C.kill_child). The oracle states the spec's contact rules (spec 6.6, R2) over the stored values read back from
the engine on a COPY of the finished files; it shares no code with the Go parser. The DDL snapshots are from memory
(no source holds wa.db's DDL), so the oracle states what the parser does with the column set the sources report.

Python 3 with its bundled sqlite3 only; no Docker. Not part of gen.sh all. The helpers jid() and vcard() of
whatsapp_gen are shared.
"""
import re

import telephony_common as C
import whatsapp_gen as W

FAMILY = "whatsapp-wadb"
CLAIMED = ("wa_contacts",)  # Meta.Claims of android.whatsapp.contacts
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle)
SCHEMAS = W.SCHEMAS
MAX_TEXT = 256 << 10

REMOTE_211 = "/data/data/com.whatsapp/databases/wa.db"
REMOTE_CURRENT = "/data/user/0/com.whatsapp/databases/wa.db"
REMOTE_BUSINESS = "/data/data/com.whatsapp.w4b/databases/wa.db"
REMOTE_BACKUP_PLAIN = "/sdcard/WhatsApp/Databases/wa.db"
REMOTE_CRYPT15 = "/sdcard/WhatsApp/Databases/wa.db.crypt15"
REMOTE_OTHER = "/storage/emulated/0/Android/data/com.whatsapp/databases/wa.db"

# the columns the Go mapping reads, in its order, with whether each is required (rowrun.note_columns)
MAPPED = [("jid", True), ("is_whatsapp_user", False), ("status", False), ("status_timestamp", False), ("number", False),
          ("raw_contact_id", False), ("display_name", False), ("phone_type", False), ("phone_label", False),
          ("unseen_msg_count", False), ("thumb_ts", False), ("photo_id_timestamp", False), ("given_name", False),
          ("family_name", False), ("wa_name", False), ("sort_name", False), ("nickname", False), ("company", False),
          ("title", False)]
# stored as they are in raw (raw beside interpreted), a NULL cell being absent
RAW_AS_STORED = ["is_whatsapp_user", "status", "status_timestamp", "raw_contact_id", "phone_type", "phone_label",
                 "unseen_msg_count", "thumb_ts", "photo_id_timestamp", "wa_name", "sort_name", "nickname", "company", "title"]
# the TEXT columns the mapping interprets; a cell of another class is stated and kept in raw as stored
TEXT_INTERPRETED = ["display_name", "number", "given_name", "family_name"]
UNMAPPED_TABLES = ("wa_biz_profiles", "wa_block_list", "wa_group_admin_settings", "wa_vnames")
PERSON = {"user", "lid"}

NAMELESS_JID_ONLY = "wa_contacts row has no name or number (JID only)"
NAMELESS_OTHER = "wa_contacts row has no display name, push name or number"
NO_JID = "wa_contacts row has no readable JID"
TOO_LONG = "wa_contacts row has a JID over 1024 bytes"
NAME_DAMAGED = "wa_contacts row has no readable name or number (a name column is damaged or not text)"


def jid_class(b):
    """decode/wajid.Parse restated: the class of the address stored as bytes b, or None when it does not parse."""
    try:
        s = b.decode("utf-8")
    except UnicodeDecodeError:
        return None
    if "\x00" in s or "@" not in s:
        return None
    raw, server = s.rsplit("@", 1)
    if not raw or not server:
        return None
    user = raw
    if ":" in raw:
        user, dev = raw.rsplit(":", 1)
        if not re.fullmatch(r"[0-9]+", dev, re.ASCII) or int(dev) > 65535:
            return None
        if "." in user:
            user, agent = user.rsplit(".", 1)
            if not re.fullmatch(r"[0-9]+", agent, re.ASCII) or int(agent) > 255:
                return None
        if not user:
            return None
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


# ----------------------------------------------------------------------------------------- the scenario rows

def row(i, jid_, **kw):
    r = {"_id": i, "jid": jid_}
    r.update(kw)
    return r


U = lambda n: W.jid("1555000%04d" % n, "s.whatsapp.net")  # noqa: E731  (fictional 555 numbers)
LONG_STATUS = ("The quick brown fox jumps over the lazy dog. " * 7000)[:300 << 10]


def contacts_current():
    """The ids 1 to 14 of the plan (Task 12). A column a variant lacks is dropped on insert (C.insert)."""
    return [
        row(1, U(1), is_whatsapp_user=1, status="busy", status_timestamp=1600000000000, number="+15550000001", raw_contact_id=17,
            display_name="Ann", phone_type=2, phone_label="Mobile", unseen_msg_count=3, thumb_ts=1600000000, photo_id_timestamp=1600000001000,
            given_name="Ann", family_name="Smith", wa_name="Annie", sort_name="Ann Smith", nickname="A"),
        row(2, U(2), is_whatsapp_user=1, display_name=None, number="", wa_name="Bob the Builder"),
        row(3, U(3), is_whatsapp_user=1, display_name="", number="+15550000003", wa_name=None),
        row(4, U(4), is_whatsapp_user=1, display_name="", number="", wa_name=None),
        row(5, W.jid("100000000000005", "lid"), is_whatsapp_user=1, display_name="Lidia"),
        row(6, "120363000000000001@g.us", is_whatsapp_user=0, display_name="Family Group"),
        row(7, "15550000007-1500000000@broadcast", is_whatsapp_user=0, display_name="Friends list"),
        row(8, "status@broadcast", is_whatsapp_user=0, display_name="Status"),
        row(9, "120363000000000099@newsletter", is_whatsapp_user=0, display_name="News"),
        row(10, "123456@bot", is_whatsapp_user=0, display_name="Helper"),
        row(11, "-1", is_whatsapp_user=0, display_name="Broken"),
        row(12, U(12), is_whatsapp_user=1, display_name="", number="", wa_name=None, given_name="Gina", family_name="Fox",
            company="Acme", title="CEO"),
        row(13, U(13), is_whatsapp_user=0, display_name="Dan", number="+15550000013", status="away", unseen_msg_count=0),
        row(14, U(14), is_whatsapp_user=1, display_name="Eve", status=LONG_STATUS, status_timestamp=1600000002000),
    ]


def pick(rows, ids):
    return [r for r in rows if r["_id"] in ids]


# ----------------------------------------------------------------------------------------- the oracle

def classify(r):
    """('record', None) or ('warn', text): what the spec says of one wa_contacts row."""
    jv = r.get("jid")
    if not W.is_text(jv):
        return "warn", NO_JID
    cls = jid_class(jv)
    if cls is None:
        return "warn", "wa_contacts row is not a person (unparseable)"
    if cls not in PERSON:
        return "warn", "wa_contacts row is not a person (%s)" % cls
    if len(jv) > 1024:
        return "warn", TOO_LONG
    texts = {n: r.get(n) for n in ("display_name", "wa_name", "number")}
    if not any(W.is_text(v) and len(v) > 0 for v in texts.values()):
        if any(v is not None and not W.is_text(v) for v in texts.values()):
            return "warn", NAME_DAMAGED  # unknown is not absent: a name cell that is not text says nothing of the row's names
        other = any(W.is_text(r.get(n)) and len(r.get(n)) > 0
                    for n in ("given_name", "family_name", "nickname", "company", "title", "sort_name"))
        return "warn", NAMELESS_OTHER if other else NAMELESS_JID_ONLY
    return "record", None


def expect_contact(r, remote, file="any"):
    """The expectation of one wa_contacts row (None when the row is a counted warning, not a record)."""
    kind, _ = classify(r)
    if kind != "record":
        return None
    raw, unknown = {}, {}
    c = W.Cells(r, raw, unknown)
    p = {}
    jid_text = W.cleaned(raw, "jid", r["jid"])
    dn, wn, num = c.text("display_name"), c.text("wa_name"), c.text("number")
    # wa_name is read as text for the display name; a cell of another class is stated by the as-stored pass below
    display = ""
    if dn:
        display = W.text_of(dn).text
        if W.text_of(dn).flags:
            C.mark(raw, "display_name")
        raw["display_name_source"] = "address_book"
    elif wn:
        display = W.text_of(wn).text
        if W.text_of(wn).flags:
            C.mark(raw, "display_name")
        raw["display_name_source"] = "wa_name_push_name"
    if display:
        p["display_name"] = display
    names = {}
    for col, key in (("given_name", "given"), ("family_name", "family")):
        b = c.text(col)
        if b:
            cl = W.text_of(b)
            if cl.flags:
                C.mark(raw, col)
            if cl.text:
                names[key] = cl.text
    if names:
        p["names"] = names
    phone = ""
    if num:
        cl = W.text_of(num)
        if cl.flags:
            C.mark(raw, "number")
        phone = cl.text
        p["phones"] = [{"value": phone}]
    p["ims"] = [{"value": jid_text, "kind": "whatsapp"}]
    # raw beside interpreted: the columns kept as stored; a TEXT-interpreted cell of another class is kept as stored too
    for n in RAW_AS_STORED:
        v = r.get(n)
        if v is not None:
            if n == "is_whatsapp_user" and not W.is_int(v):
                unknown[n] = "type-mismatch"
            s = W.stored(raw, n, v)
            if s is not None:
                raw[n] = s
    for n in TEXT_INTERPRETED + ["wa_name"]:
        v = r.get(n)
        if v is not None and not W.is_text(v):
            unknown[n] = "type-mismatch"
            raw[n] = W.stored(raw, n, v)
    if W.is_int(r.get("is_whatsapp_user")) and r["is_whatsapp_user"] == 1:
        raw["label_is_whatsapp_user"] = "whatsapp_user"
    raw["app"], raw["app_basis"] = W.app_of(remote), "path"
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    lines = [display, phone, jid_text]
    if wn and W.is_text(wn) and W.text_of(wn).text != display:
        lines.append(W.text_of(wn).text)
    st = r.get("status")
    if W.is_text(st) and len(st) > 0:
        lines.append(W.text_of(st).text)
    body = "\n".join(x for x in lines if x)
    plain = all(x.isascii() and x.isprintable() for x in (display, phone)) and len(display) <= 80 and len(phone.encode("utf-8")) <= 32
    summ = " ".join(x for x in (display, phone) if x) if plain else None
    return {"table": "wa_contacts", "rowid": r["rowid"], "file": file, "type": "contact", "source_path": remote, "time": None,
            "time_end": None, "times": {}, "deleted": False, "summary": summ, "body": body, "payload": p}


def oracle(files, remote, variant, ddl):
    view = C.engine_view(files, ["wa_contacts"] + list(UNMAPPED_TABLES))
    exp = C.expect_header(FAMILY, variant, ddl)
    for r in view["wa_contacts"]:
        kind, why = classify(r)
        if kind == "record":
            exp["records"].append(expect_contact(r, remote))
        else:
            exp["warnings"].append({"table": "wa_contacts", "rowid": r["rowid"], "contains": why})
    for rec in exp["records"]:
        if "wal" in files:
            rec["source_path"] = ""  # a WAL companion: the source path follows the role of the file that holds the cell
    notes = exp["notes"]
    notes["app"] = W.app_of(remote)
    notes["database_user"] = "0"
    unmapped = [t for t in UNMAPPED_TABLES if t in view]
    if unmapped:
        notes["unmapped_tables"] = ",".join(unmapped)
    return exp


def build(work, ddl_name, rows, page_size=4096, wal=False):
    path = work + "/w.db"
    con = C.connect(path, page_size=page_size)
    con.executescript(C.load_ddl(ddl_name, SCHEMAS)[0])
    with C.batch(con):
        for r in rows:
            C.insert(con, "wa_contacts", r)
    cols = C.columns(con, "wa_contacts")
    if wal:
        con.execute("pragma journal_mode = wal")
    return con, path, cols


def finish(path, cols, remote, variant, ddl):
    files = {"db": C.read(path)}
    if C.os.path.exists(path + "-wal"):
        files["wal"] = C.read(path + "-wal")
    exp = oracle(files, remote, variant, ddl)
    C.note_columns(cols, MAPPED, "wa_contacts", exp["notes"])
    return files, exp


def plain(work, ddl, rows, remote, variant):
    con, path, cols = build(work, ddl, rows)
    con.close()
    return finish(path, cols, remote, variant, ddl)


def v_211(work):
    return plain(work, "wa-2.11.sql", pick(contacts_current(), {1, 2, 3, 4, 6, 11, 13}), REMOTE_211, "wa-2.11")


def v_current(work):
    """Rows 1 to 14 live. Beyond them, in the database file: row 15 and the first version of row 13 (its status); in a WAL
    that is never checkpointed: row 13 is edited and row 15 deleted, so the file still holds two versions that are history."""
    rows = contacts_current() + [row(15, U(15), is_whatsapp_user=1, display_name="Zed", number="+15550000015", status="gone")]
    rows[12]["status"] = "old status"
    con, path, cols = build(work, "wa-current.sql", rows, wal=True)
    scen = SCENARIOS["wa-current"] = C.Scenario(CLAIMED)
    scen.write("wa_contacts", scen.snap(con, "wa_contacts"))
    scen.want("wa_contacts", scen.snap(con, "wa_contacts", "_id in (13, 15)"))  # as the database file still holds them
    con.close()
    stmts = ["pragma wal_autocheckpoint = 0", "update wa_contacts set status = 'away' where _id = 13", "delete from wa_contacts where _id = 15"]
    C.kill_child(path, stmts)
    return finish(path, cols, REMOTE_CURRENT, "wa-current", "wa-current.sql")


def v_business(work):
    return plain(work, "wa-current.sql", pick(contacts_current(), {1, 2, 3, 5, 13}), REMOTE_BUSINESS, "wa-business")


def v_no_table(work):
    path = work + "/n.db"
    con = C.connect(path)
    con.execute("create table foo(x)")
    con.close()
    return {"db": C.read(path)}, C.expect_header(FAMILY, "wa-no-table", "")


def unparsed(work, variant):
    """A plain database that is never parsed (a backup copy, R7; or outside the live directory): no record, no note."""
    con, path, cols = build(work, "wa-current.sql", pick(contacts_current(), {1, 2}))
    con.close()
    return {"db": C.read(path)}, C.expect_header(FAMILY, variant, "wa-current.sql")


def v_backup_plain(work):
    return unparsed(work, "wa-backup-plain")


def v_crypt15(work):
    return {"db": W.stream(64 << 10)}, C.expect_header(FAMILY, "wa-crypt15", "")


def v_other_location(work):
    return unparsed(work, "wa-other-location")


def numbered(i):
    return row(i, U(100 + i), is_whatsapp_user=1, display_name="Contact number %d" % i, number="+1555123%04d" % i,
               status="Hey there I am using WhatsApp number %d" % i, wa_name="Pushname %d" % i)


def v_deleted(work):
    """100 contacts on 1024-byte pages, secure_delete OFF: contacts 21 to 80 are deleted and whole leaf pages go to the
    freelist. A lone database: a freelist is read as history only when no WAL or journal could have changed the free pages."""
    con, path, cols = build(work, "wa-current.sql", [numbered(i) for i in range(1, 101)], page_size=1024)
    scen = SCENARIOS["wa-deleted"] = C.Scenario(CLAIMED)
    scen.write("wa_contacts", scen.snap(con, "wa_contacts"))
    scen.want("wa_contacts", scen.snap(con, "wa_contacts", "_id between 21 and 80"), label="wa_contacts")
    con.execute("delete from wa_contacts where _id between 21 and 80")
    con.close()
    return finish(path, cols, REMOTE_CURRENT, "wa-deleted", "wa-current.sql")


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (ruling 4G-R14)."""
    return C.recover_oracle(variant, files, SCENARIOS[variant], lambda f: None,
                            lambda table, d, env, file: expect_contact(d, "", file), nobody="wa_contacts row")


VARIANTS = {
    "wa-2.11": v_211,
    "wa-current": v_current,
    "wa-business": v_business,
    "wa-no-table": v_no_table,
    "wa-backup-plain": v_backup_plain,
    "wa-crypt15": v_crypt15,
    "wa-other-location": v_other_location,
    "wa-deleted": v_deleted,
}

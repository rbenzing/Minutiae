"""Android contacts provider (contacts2.db) fixtures: DDL variants, scenario rows, oracle."""
import re

import telephony_common as C

FAMILY = "contacts"
REMOTE = "/data/user_de/0/com.android.providers.contacts/databases/contacts2.db"
T0, MS = 1682935200, 1000
# Contact ids from this value up name an aggregate contact that does not exist (a dangling contact_id): Builder.contact adds no
# contacts row for them, and the scenario uses DANGLING_CONTACT_ID for its dangling example (T74).
FIRST_DANGLING_CONTACT = 90
DANGLING_CONTACT_ID = 98
assert DANGLING_CONTACT_ID >= FIRST_DANGLING_CONTACT
MAX_TEXT = 256 << 10
MAX_LIST = 1000
MIMES = ["vnd.android.cursor.item/name", "vnd.android.cursor.item/phone_v2", "vnd.android.cursor.item/email_v2",
         "vnd.android.cursor.item/organization", "vnd.android.cursor.item/postal-address_v2", "vnd.android.cursor.item/im",
         "vnd.android.cursor.item/website", "vnd.android.cursor.item/note", "vnd.android.cursor.item/photo",
         "vnd.android.cursor.item/nickname", "vnd.android.cursor.item/group_membership"]
NAME, PHONE, EMAIL, ORG, POSTAL, IM, WEB, NOTE, PHOTO, NICK, GROUP = range(1, 12)
# account_name and account_type are the inline columns of old schemas (a fallback, read only when physically present);
# the account of a current schema comes through accounts (account_id) and the time through contacts (contact_id) (T68, T69).
RC_MAPPED = [("display_name", True), ("account_id", False), ("account_name", False), ("account_type", False), ("contact_id", False),
             ("starred", False), ("deleted", False), ("x_times_contacted", False), ("sourceid", False)]
AGG_SOURCE = "contacts.contact_last_updated_timestamp"
DC_MAPPED = [("contact_id", True), ("contact_deleted_timestamp", False)]
PHONE_T = {1: "home", 2: "mobile", 3: "work", 4: "fax_work", 5: "fax_home", 6: "pager", 7: "other"}
EMAIL_T = {1: "home", 2: "work", 3: "other", 4: "mobile"}
URL_T = {1: "homepage", 2: "blog", 3: "profile", 4: "home", 5: "work", 6: "ftp", 7: "other"}
IM_T = {0: "aim", 1: "msn", 2: "yahoo", 3: "skype", 4: "qq", 5: "google_talk", 6: "icq", 7: "jabber", 8: "netmeeting"}
POSTAL_T = {1: "home", 2: "work", 3: "other"}
# The runes Go's strings.TrimSpace removes (unicode.IsSpace); Python's str.strip() differs.
GO_SPACE = "".join(chr(c) for c in [9, 10, 11, 12, 13, 32, 0x85, 0xA0, 0x1680, *range(0x2000, 0x200B), 0x2028, 0x2029, 0x202F, 0x205F, 0x3000])
DECIMAL = re.compile(r"[+-]?[0-9]+", re.ASCII)  # strconv.ParseInt base 10: ASCII digits only (ruling T63)


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def num(v):
    """The number a stored cell reads as: an INTEGER, or TEXT of at most 32 bytes that is trimmed decimal ASCII; else None."""
    if is_int(v):
        return v
    if isinstance(v, bytes) and not isinstance(v, C.Blob):
        if len(v) > 32:
            return None
        try:
            t = v.decode("utf-8").strip(GO_SPACE)
        except UnicodeDecodeError:
            return None
        if DECIMAL.fullmatch(t):
            n = int(t)
            return n if -(2**63) <= n < 2**63 else None
    return None


def im_lab(proto):
    """(label, unknown_type) of an instant-messaging protocol: -1 is the custom marker (handled by the caller), 0 is AIM."""
    if proto is None:
        return "", False
    if proto in IM_T:
        return IM_T[proto], False
    return "", True


def lab(table, t, custom):
    """(label, unknown_type)."""
    if t is None:
        return "", False
    if t == 0:
        return custom, False
    if t in table:
        return table[t], False
    return "", True


class Builder:
    def __init__(self):
        self.rc, self.data, self._n = [], [], 0
        self.accounts = [{"_id": 1, "account_name": "alice@example.test", "account_type": "com.google"},
                         {"_id": 2, "account_name": "alice@samsung.test", "account_type": "com.samsung.account"},
                         {"_id": 3, "account_name": "+15551230001", "account_type": "com.whatsapp", "data_set": "wa"}]
        self.contacts = {}

    def contact(self, rid, ts=None, **kw):
        """A raw contact of account 1 and of the aggregate contact rid, whose last update is ts (default a valid time).
        account_id=None is no account; an id with no accounts row is dangling. contact_id=None is not aggregated; a
        contact_id with no contacts row (the scenario names an id the table lacks) is dangling."""
        row = {"_id": rid, "account_id": 1, "contact_id": rid, "deleted": 0, "starred": 0, "x_times_contacted": 0}
        row.update(kw)
        self.rc.append(row)
        cid = row["contact_id"]
        if cid is not None and cid < FIRST_DANGLING_CONTACT and cid not in self.contacts:
            self.contacts[cid] = {"_id": cid, "contact_last_updated_timestamp": (T0 + cid) * MS if ts is None else ts}

    def d(self, rid, mime, primary=0, **kw):
        self._n += 1
        self.data.append(dict(_id=self._n, raw_contact_id=rid, mimetype_id=mime, is_primary=primary, **kw))


def baseline():
    b = Builder()
    b.contact(1, display_name="Alice Example", starred=1, x_times_contacted=5, sourceid="g-1")
    b.d(1, NAME, data1="Alice Example", data2="Alice", data3="Example", data4="Dr.", data5="Q.", data6="Jr.", data7="al", data9="ex")
    b.d(1, PHONE, 1, data1="+1 555-123-0001", data2=2)
    b.d(1, PHONE, 0, data1="+15551230002", data2=3)
    b.d(1, PHONE, 0, data1="5551230003", data2=0, data3="Fax Home")
    b.d(1, PHONE, 0, data1="", data2=1)
    b.d(1, PHONE, 0, data1="911", data2=77)
    b.d(1, EMAIL, 0, data1="alice@example.test", data2=1)
    b.d(1, ORG, 0, data1="Acme", data4="Engineer", data5="R&D")
    b.d(1, ORG, 0, data1="Second Corp")
    b.d(1, POSTAL, 0, data1="1 Main St\nTown", data2=1, data4="1 Main St", data7="Town", data8="CA", data9="90001", data10="USA")
    b.d(1, IM, 0, data1="alice_skype", data5=3)
    b.d(1, IM, 0, data1="alice_aim", data5=0)
    b.d(1, IM, 0, data1="alice@jabber.test", data5=-1, data6="XMPP")
    b.d(1, WEB, 0, data1="https://example.test/alice", data2=1)
    b.d(1, NOTE, 0, data1="first note")
    b.d(1, PHOTO, 0, data15=b"\x89PNG-synthetic-bytes")
    b.d(1, NICK, 0, data1="Al")
    b.d(1, NAME, data1="Second Name", data2="Second")
    b.d(1, GROUP, 0, data1="7")
    b.d(1, 99, 0, data1="dangling mimetype")
    b.contact(2, display_name="+15559998888", account_id=None)
    b.d(2, PHONE, 0, data1="+15559998888", data2=2)
    b.contact(3, display_name="Old Contact", deleted=1, sourceid="g-3")
    b.d(3, NAME, data1="Old Contact", data2="Old", data3="Contact")
    b.d(3, PHONE, 0, data1="+15550003333", data2=1)
    b.contact(4, display_name=None, account_id=None)
    b.contact(5, display_name="\u0645\u062d\u0645\u062f \U0001F600")
    b.d(5, NAME, data1="\u0645\u062d\u0645\u062f \U0001F600", data2="\u0645\u062d\u0645\u062f")
    b.d(5, PHONE, 0, data1="+15555550005", data2=0, data3="\u0627\u0644\u062c\u0648\u0627\u0644")
    b.contact(6, display_name=C.Raw(b"Bad\x00Name\xff"))
    b.d(6, PHONE, 0, data1=C.Raw(b"+1555\xff0006"), data2=2)
    b.contact(7, display_name="Alice Example", account_id=2)
    b.d(7, PHONE, 0, data1="+1 555-123-0001", data2=2)
    b.contact(8, display_name="Long Number")
    b.d(8, PHONE, 0, data1="1" * 400, data2=2)
    b.contact(9, display_name="Zero Timestamp", ts=0)
    b.d(9, EMAIL, 0, data1="zero@example.test", data2=4)
    b.contact(10, display_name="Year 2100", ts=4102444800000)
    b.d(10, EMAIL, 0, data1="y2100@example.test", data2=99)
    b.contact(11, display_name=None, deleted=1, sourceid="g-11", contact_id=None)
    b.contact(12, display_name="Dangling Account", account_id=99)
    b.d(12, EMAIL, 0, data1="dangling@example.test", data2=1)
    b.contact(13, display_name="Dangling Aggregate", contact_id=DANGLING_CONTACT_ID)
    b.d(13, EMAIL, 0, data1="aggregate@example.test", data2=2)
    return b


def mimetype_rows():
    return [{"_id": i + 1, "mimetype": m} for i, m in enumerate(MIMES)]


# ---------------------------------------------------------------- oracle
# Every variant is a lone database (no WAL, no journal), so the oracle states the db path as source_path of every row (rulings T48, T51).


def capped(items, name, raw):
    if len(items) > MAX_LIST:
        raw[name + "_total"], raw[name + "_truncated"] = len(items), True
        return items[:MAX_LIST]
    return items


def contact_body(p):
    lines = []
    if p.get("display_name"):
        lines.append(p["display_name"])
    for key in ("phones", "emails"):
        lines += [v["value"] for v in p.get(key, [])]
    for a in p.get("addresses", []):
        if a.get("formatted"):
            lines.append(a["formatted"])
        else:
            j = ", ".join(a[k] for k in ("street", "city", "region", "postcode", "country") if a.get(k))
            if j:
                lines.append(j)
    o = p.get("organization")
    if o:
        j = ", ".join(o[k] for k in ("name", "title", "department") if o.get(k))
        if j:
            lines.append(j)
    return "\n".join(lines)


def expect_contact(rc, data_rows, mimes, remote, accounts=None, aggregates=None, file="any"):
    raw, unknown, p = {}, {}, {}
    tf = set()
    rid = rc["rowid"]

    def field(v, f):
        if not isinstance(v, bytes) or isinstance(v, C.Blob):
            return ""
        c = C.clean(v, MAX_TEXT)
        if c.flags:
            tf.add(f)
        return c.text

    def txt(d, n):
        return field(d.get("data%d" % n), "data")

    name = field(rc.get("display_name"), "display_name")
    if name:
        p["display_name"] = name
    undecided = []
    joined_unknown = {}  # cells of a JOINED row that hold no usable value, by "<table>.<column>"; unknown_columns is the mapped row's own
    if "account_name" in rc or "account_type" in rc:  # an old schema: the inline columns, only when physically present
        at = field(rc.get("account_type"), "account_type")
        an = field(rc.get("account_name"), "account_name")
        for col in ("account_type", "account_name"):
            if isinstance(rc.get(col), C.Blob):
                unknown[col] = "type-mismatch"
    else:  # the account comes only from the join accounts._id = raw_contacts.account_id
        at = an = ""
        aid = rc.get("account_id")
        if "account_id" not in rc:
            raw["account_state"] = "no-account-id-column"  # neither account_id nor the inline columns: this variant cannot say
        elif is_int(aid):
            raw["account_id"] = aid
            acct = accounts.get(aid) if accounts is not None else None
            if accounts is None:
                undecided.append("accounts")  # no accounts table: the join cannot be decided, so the account is unknown
            elif acct is None:
                raw["account_state"] = "no-accounts-row"  # a dangling id: decided, stated; not "no account" (T72)
            else:
                at = field(acct.get("account_type"), "account_type")
                an = field(acct.get("account_name"), "account_name")
                for col in ("account_type", "account_name"):
                    if isinstance(acct.get(col), C.Blob):
                        joined_unknown["accounts." + col] = "type-mismatch"
        elif aid is not None:
            unknown["account_id"] = "type-mismatch"
        # a NULL account_id is a contact without an account (device-local): no account, nothing flagged
    if at or an:
        a = {}
        if at:
            a["type"] = at
        if an:
            a["name"] = an
        p["accounts"] = [a]
    if is_int(rc.get("starred")):
        raw["starred"] = rc["starred"]
        if rc["starred"] in (0, 1):
            p["starred"] = rc["starred"] == 1
    deleted = False
    if is_int(rc.get("deleted")):
        raw["deleted"] = rc["deleted"]
        if rc["deleted"] != 0:
            deleted = True
            p["deleted"] = {"source": "raw_contacts.deleted"}
    if is_int(rc.get("x_times_contacted")):
        raw["x_times_contacted"] = rc["x_times_contacted"]
    # The time is the AGGREGATE contact's last update (contacts.contact_last_updated_timestamp through contact_id), never the
    # raw contact's own modification time: it is stated under a times key that names it, with its source, and Record.Time stays unset.
    t, times = None, {}
    cid = rc.get("contact_id")
    ts = None
    if "contact_id" not in rc:
        raw["aggregate_state"] = "no-contact-id-column"
    elif is_int(cid):
        raw["contact_id"] = cid
        agg = aggregates.get(cid) if aggregates is not None else None
        if aggregates is None:
            undecided.append("contacts")  # no contacts table: the join cannot be decided
        elif agg is None:
            raw["aggregate_state"] = "no-contacts-row"  # a dangling contact_id (T72)
        else:
            ts = agg.get("contact_last_updated_timestamp")
    elif cid is None:
        raw["aggregate_state"] = "no-contact-id"  # not aggregated: the time is unknown, never zero (T72)
    else:
        unknown["contact_id"] = "type-mismatch"
    if is_int(ts):
        st = C.classify(ts, "ms")
        raw["contact_last_updated_timestamp"] = ts
        raw["contact_last_updated_source"] = AGG_SOURCE
        if st != "valid":
            raw["contact_last_updated_timestamp_status"] = st
        else:
            times["aggregate_last_updated"] = C.iso(ts, "ms")
    elif ts is not None:
        joined_unknown["contacts.contact_last_updated_timestamp"] = "type-mismatch"
    p["source_id"] = str(rid)
    sid = field(rc.get("sourceid"), "sourceid")
    if sid:
        p["source_ids"] = [sid]
    raw["data_rows"] = len(data_rows)
    names, nick, got_name, org = {}, "", False, None
    phones, emails, urls, ims, addrs, notes = [], [], [], [], [], []
    notes_total = 0
    unmapped = other = skipped = unk = extra_names = extra_org = 0
    photo = False
    for d in data_rows:
        mid = num(d.get("mimetype_id"))
        mt = mimes.get(mid) if mid is not None else None
        if not mt:
            unmapped += 1
            continue
        prim = d.get("is_primary")
        primary = (prim == 1) if is_int(prim) and prim in (0, 1) else None

        def value(label_table, kind=None, custom_col=3, type_col=2):
            v = txt(d, 1)
            if not v:
                return None
            lb, u = lab(label_table, num(d.get("data%d" % type_col)), txt(d, custom_col))
            e = {"value": v}
            if lb:
                e["label"] = lb
            if kind:
                e["kind"] = C.addr_kind(v)
            if primary is not None:
                e["primary"] = primary
            return e, u

        if mt.endswith("/name"):
            if got_name:
                extra_names += 1
                continue
            got_name = True
            ph = " ".join(x for x in (txt(d, 7), txt(d, 8), txt(d, 9)) if x)
            for k, v in (("given", txt(d, 2)), ("family", txt(d, 3)), ("prefix", txt(d, 4)), ("middle", txt(d, 5)), ("suffix", txt(d, 6)), ("phonetic", ph)):
                if v:
                    names[k] = v
        elif mt.endswith("/nickname"):
            nick = nick or txt(d, 1)
        elif mt.endswith("/phone_v2") or mt.endswith("/email_v2"):
            tbl = PHONE_T if mt.endswith("/phone_v2") else EMAIL_T
            r = value(tbl, kind=True)
            if r is None:
                skipped += 1
                continue
            (phones if mt.endswith("/phone_v2") else emails).append(r[0])
            unk += r[1]
        elif mt.endswith("/website"):
            r = value(URL_T)
            if r is None:
                skipped += 1
                continue
            urls.append(r[0])
            unk += r[1]
        elif mt.endswith("/im"):
            v = txt(d, 1)
            if not v:
                skipped += 1
                continue
            proto = num(d.get("data5"))
            lb, u = (txt(d, 6), False) if proto == -1 else im_lab(proto)
            e = {"value": v}
            if lb:
                e["label"] = lb
            if primary is not None:
                e["primary"] = primary
            ims.append(e)
            unk += u
        elif mt.endswith("/organization"):
            if org is not None:
                extra_org += 1
                continue
            org = {k: v for k, v in (("name", txt(d, 1)), ("title", txt(d, 4)), ("department", txt(d, 5))) if v}
        elif mt.endswith("/postal-address_v2"):
            lb, u = lab(POSTAL_T, num(d.get("data2")), txt(d, 3))
            a = {k: v for k, v in (("formatted", txt(d, 1)), ("street", txt(d, 4)), ("city", txt(d, 7)), ("region", txt(d, 8)),
                                   ("postcode", txt(d, 9)), ("country", txt(d, 10)), ("label", lb)) if v}
            unk += u
            if any(k != "label" for k in a):
                addrs.append(a)
        elif mt.endswith("/note"):
            s = txt(d, 1)
            if s:
                notes_total += 1
                if len(notes) < 100:
                    notes.append(s)
        elif mt.endswith("/photo"):
            v = d.get("data15")
            photo = photo or (isinstance(v, bytes) and len(v) > 0)
        else:
            other += 1
    if nick:
        names["nickname"] = nick
    if names:
        p["names"] = names
    if org:
        p["organization"] = org
    for key, items in (("phones", phones), ("emails", emails), ("urls", urls), ("ims", ims), ("addresses", addrs)):
        items = capped(items, key, raw)
        if items:
            p[key] = items
    if photo:
        p["photo"] = {"present": True}
    for key, v in (("data_unmapped", unmapped), ("data_other", other), ("values_skipped", skipped), ("labels_unknown", unk),
                   ("name_rows_extra", extra_names), ("organization_rows_extra", extra_org)):
        if v:
            raw[key] = v
    if notes:
        raw["notes"] = notes
    if notes_total > len(notes):
        raw["notes_total"], raw["notes_truncated"] = notes_total, True
    if tf:
        raw["text_flags"] = sorted(tf)
    if undecided:
        raw["join_undecided"] = undecided
    if joined_unknown:
        raw["joined_unknown_columns"] = joined_unknown
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    identifies = bool(name or phones or emails or (org and any(org.values())) or (deleted and p.get("source_ids")))
    if not identifies:
        return None
    first_phone = phones[0]["value"] if phones else ""
    summ = None
    if (name + first_phone).isascii() and len(name) <= 80 and len(first_phone.encode()) <= 32 and (name or first_phone):
        summ = " ".join(x for x in (name, first_phone) if x)
    elif not name and not first_phone and not emails and not org and deleted:
        sid0 = p["source_ids"][0]  # a tombstone-shaped row: the summary names the deleted source row
        if sid0.isascii() and len(sid0) <= 64:
            summ = "deleted contact " + sid0
    return {"table": "raw_contacts", "rowid": rid, "file": file, "type": "contact", "source_path": remote, "time": t, "time_end": None, "times": times, "deleted": deleted,
            "summary": summ, "body": contact_body(p) or None, "payload": p}


def expect_tombstone(dc, remote, file="any"):
    raw = {}
    cid = dc["rowid"]
    p = {"source_ids": [str(cid)], "deleted": {"source": "deleted_contacts"}}
    t, times = None, {}
    ts = dc.get("contact_deleted_timestamp")
    if is_int(ts):
        st = C.classify(ts, "ms")
        raw["contact_deleted_timestamp"] = ts
        if st != "valid":
            raw["contact_deleted_timestamp_status"] = st
        else:
            t = C.iso(ts, "ms")
            times["deleted"] = t
    p["raw"] = raw
    return {"table": "deleted_contacts", "rowid": cid, "file": file, "type": "contact", "source_path": remote, "time": t, "time_end": None, "times": times, "deleted": True,
            "summary": "deleted contact %d" % cid, "body": None, "payload": p}


def oracle(files, ddl_name, variant, rc_cols, dc_cols):
    view = C.engine_view(files, ["raw_contacts", "data", "mimetypes", "deleted_contacts", "accounts", "contacts"])
    accounts = {r["rowid"]: r for r in view["accounts"]} if "accounts" in view else None
    aggregates = {r["rowid"]: r for r in view["contacts"]} if "contacts" in view else None
    mimes = {}
    for r in view["mimetypes"]:
        if isinstance(r.get("mimetype"), bytes):
            mimes[r["rowid"]] = C.clean(r["mimetype"], MAX_TEXT).text
    by_rc = {}
    for d in view["data"]:
        by_rc.setdefault(d.get("raw_contact_id"), []).append(d)
    exp = C.expect_header(FAMILY, variant, ddl_name)
    for rc in view["raw_contacts"]:
        e = expect_contact(rc, by_rc.get(rc["rowid"], []), mimes, REMOTE, accounts, aggregates)
        if e is None:
            exp["warnings"].append({"table": "raw_contacts", "rowid": rc["rowid"], "contains": "identifies nobody"})
        else:
            exp["records"].append(e)
    for dc in view.get("deleted_contacts", []):
        exp["records"].append(expect_tombstone(dc, REMOTE))
    C.note_columns(rc_cols, RC_MAPPED, "raw_contacts", exp["notes"])
    if dc_cols:
        C.note_columns(dc_cols, DC_MAPPED, "deleted_contacts", exp["notes"])
    return exp


# ---------------------------------------------------------------- variants


def build(work, ddl_name, variant, b, extra=None, **conn):
    path = work + "/k.db"
    ph = C.load_ddl(ddl_name)
    con = C.connect(path, **conn)
    # the DDL declares COLLATE PHONEBOOK, a collation the platform registers; no index uses it here, so a stand-in order does
    con.create_collation("PHONEBOOK", lambda x, y: (x > y) - (x < y))
    con.executescript(ph[0])
    with C.batch(con):
        for r in mimetype_rows():
            C.insert(con, "mimetypes", r)
        for r in b.accounts:
            C.insert(con, "accounts", r)
        for r in b.contacts.values():
            C.insert(con, "contacts", r)
        for r in b.rc:
            C.insert(con, "raw_contacts", r)
        for r in b.data:
            C.insert(con, "data", r)
        for r in (extra or []):
            C.insert(con, "deleted_contacts", r)
    rc_cols = C.columns(con, "raw_contacts")
    try:
        dc_cols = C.columns(con, "deleted_contacts")
    except Exception:
        dc_cols = []
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, ddl_name, variant, rc_cols, dc_cols)


def variant_baseline(work):
    return build(work, "contacts-aosp-baseline.sql", "aosp-baseline", baseline())


def variant_deleted(work):
    b = baseline()
    b.rc = [r for r in b.rc if r["_id"] in (1, 3)]
    b.data = [d for d in b.data if d["raw_contact_id"] in (1, 3)]
    tomb = [{"contact_id": 77, "contact_deleted_timestamp": T0 * MS}, {"contact_id": 78, "contact_deleted_timestamp": 0}]
    return build(work, "contacts-deleted-contacts.sql", "deleted-contacts", b, tomb)


def variant_multi(work):
    b = Builder()
    for i, (atype, aname) in enumerate([("com.google", "a@example.test"), ("com.samsung.account", "a@samsung.test"), ("com.whatsapp", "+15551230001")], 1):
        b.contact(i, display_name="Same Person", account_id=i, contact_id=1)
        b.d(i, PHONE, 1, data1="+15551230001", data2=2)
    return build(work, "contacts-aosp-baseline.sql", "multi-account", b)


def variant_rtl(work):
    b = Builder()
    for i, nm in enumerate(["\u05d3\u05e0\u05d9", "\u0645\u062d\u0645\u062f \u202eevil\u202c", "\u7530\u4e2d\u592a\u90ce", "caf\u00e9 \U0001F600"], 1):
        b.contact(i, display_name=nm)
        b.d(i, NAME, data1=nm, data2=nm)
        b.d(i, PHONE, 0, data1="+1555000%04d" % i, data2=0, data3=nm)
    return build(work, "contacts-aosp-baseline.sql", "unicode-rtl", b)


def variant_huge(work):
    b = Builder()
    b.contact(1, display_name="Huge Contact")
    for i in range(1200):
        b.d(1, PHONE, 0, data1="+1555%07d" % i, data2=2)
    for i in range(1100):
        b.d(1, EMAIL, 0, data1="user%d@example.test" % i, data2=1)
    for i in range(130):
        b.d(1, NOTE, 0, data1="note %d" % i)
    return build(work, "contacts-aosp-baseline.sql", "huge-contact", b)


VARIANTS = {"aosp-baseline": variant_baseline, "deleted-contacts": variant_deleted, "multi-account": variant_multi,
            "unicode-rtl": variant_rtl, "huge-contact": variant_huge}

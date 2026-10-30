"""iOS address book fixtures (plan 4E, Task 13): current, with-images, older-schema and huge-contact.

The layouts are written from memory (ruling 4E-R13): no public source defines AddressBook.sqlitedb or
AddressBookImages.sqlitedb, so these fixtures prove that the parser reads the remembered layout and nothing
about a real database. The oracle restates the "Mapping contract ios.addressbook" of the plan and rulings 4E-R23
(birthday), R24 (display name), R25 and R41 (photo presence) in Python from what this generator wrote (the
engine's read-back of its own files, the images database included); it shares no code with the Go parser and
takes no value from Go output.

Rules stated here (the contract, not the code), one record per ABPerson row, in ROWID order:
  names: First, Last, Middle, Prefix, Suffix, Nickname; phonetic = the non-empty FirstPhonetic MiddlePhonetic
  LastPhonetic joined by single spaces. display_name: the stored DisplayName when present and non-empty
  (raw.display_name_source "stored"), else Prefix First Middle Last Suffix over the non-empty parts ("composed"),
  else absent. organization: name Organization, title JobTitle, department Department.
  phones, emails, urls: ABMultiValue rows of property 3, 4, 22 with record_id = ROWID, in UID order; the label is
  ABMultiValueLabel.value at rowid `label`; `_$!<X>!$_` gives X and raw.label_raw gets {list, index, label}
  (the stored text; row order; index within the kept list); other label text is kept as is; a label id with no
  row gives no label and counts in raw.labels_unresolved; cap 1000 per list (raw.<list>_total / _truncated).
  addresses: property 5; the ABMultiValueEntry rows with parent_id = UID name their parts by ABMultiValueEntryKey:
  Street, City, State (region), ZIP (postcode), Country; the multi-value value is `formatted`; the label follows
  the same rule as above; every other key (CountryCode included) is kept in raw.address_keys_unmapped as
  {index, key, value}. Other properties: raw.other_values {property, label (as stored), value}, cap 1000.
  accounts [{type "ab-store", name}] through StoreID; a StoreID with no ABStore row is raw.store_state
  "no-store-row"; no StoreID column or a NULL StoreID: nothing. raw.groups: the sorted ABGroup names of the
  ABGroupMembers rows with member_id = ROWID and member_type 0; other member types: raw.group_members_other.
  Times: CreationDate -> times.created, ModificationDate -> times.modified (Cocoa seconds, 1970..2100, 0 and -1
  sentinels); Time = modified when valid, else created when valid; raw.<col> keeps the stored value, with
  <col>_status "sentinel"/"invalid" when it is no time, <col>_ieee for a non-finite REAL (program ruling P-4-1).
  Birthday is NOT derived (4E-R23): raw.birthday keeps the stored text. source_id is the decimal ROWID,
  source_ids the non-empty guid, ExternalIdentifier, ExternalUUID in that order. raw.Kind is the stored
  INTEGER, raw.Note the cleaned Note (64 KiB cap), a wrong storage class is raw.unknown_columns type-mismatch.
  A person with no display name, phone, e-mail and organization identifies nobody: a warning, no record.
  photo {present}: only when an images database exists: true when ABThumbnailImage or ABFullSizeImage holds a
  row for the ROWID, false when both were read and neither does.
Unmapped columns of ABPerson: FirstSort, LastSort, CompositeNameFallback, ExternalModificationTag and
ExternalRepresentation (the contract table maps no other column).
"""
import math
import os
import unicodedata

import ios_common as I
import telephony_common as C

FAMILY = "ios_addressbook"
SCHEMAS = os.path.join(C.HERE, "schemas", "ios")
MAX_TEXT = 256 << 10
MAX_NOTE = 64 << 10
MAX_LIST = 1000
# The mapped ABPerson columns in the order of the DDL snapshot; (name, required).
PERSON_MAPPED = [("ROWID", True), ("First", False), ("Last", False), ("Middle", False), ("FirstPhonetic", False),
                 ("MiddlePhonetic", False), ("LastPhonetic", False), ("Organization", False), ("Department", False),
                 ("Note", False), ("Kind", False), ("Birthday", False), ("JobTitle", False), ("Nickname", False),
                 ("Prefix", False), ("Suffix", False), ("CreationDate", False), ("ModificationDate", False),
                 ("ExternalIdentifier", False), ("ExternalUUID", False), ("StoreID", False), ("DisplayName", False),
                 ("guid", False)]
LISTS = {3: "phones", 4: "emails", 22: "urls"}
ADDRESS = 5
ADDRESS_KEYS = {"Street": "street", "City": "city", "State": "region", "ZIP": "postcode", "Country": "country"}
TABLES = ["ABPerson", "ABMultiValue", "ABMultiValueLabel", "ABMultiValueEntry", "ABMultiValueEntryKey", "ABStore",
          "ABGroup", "ABGroupMembers"]
GO_SPACE = "".join(chr(c) for c in [9, 10, 11, 12, 13, 32, 0x85, 0xA0, 0x1680, *range(0x2000, 0x200B), 0x2028, 0x2029,
                                     0x202F, 0x205F, 0x3000])


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def is_real(v):
    return isinstance(v, float)


def is_text(v):
    return isinstance(v, bytes) and not isinstance(v, C.Blob)


def jf(x):
    """A float as Go's encoding/json prints it: an integral value without a fraction."""
    if isinstance(x, float) and x.is_integer() and abs(x) < 1e15:
        return int(x)
    return x


def wrapped(label):
    """4E-R? label rule: `_$!<X>!$_` with a non-empty X is the system label X."""
    if label.startswith("_$!<") and label.endswith(">!$_") and len(label) > 8:
        return label[4:-4]
    return None


def summarize(s, limit):
    """common.Summarize restated: whitespace and control runs collapse to one space, a format or bidi character is
    SHOWN as <U+XXXX>, and the result must fit limit characters. None when the text is outside what is restated (an
    unassigned, private-use or surrogate character, or more than limit characters: the cut is not restated)."""
    out, pending = [], False
    for ch in s:
        cat = unicodedata.category(ch)
        if ch in GO_SPACE or cat == "Cc":
            pending = bool(out)
            continue
        if cat[0] == "C" and cat != "Cf":
            return None
        if pending:
            out.append(" ")
            pending = False
        out.append("<U+%04X>" % ord(ch) if cat == "Cf" else ch)
    r = "".join(out)
    return r if len(r) <= limit else None


def contact_body(p):
    """contact.Body restated: the display name, every phone, every e-mail, every address, the organization."""
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


def contact_summary(p):
    """contact.Summary restated: display name (80 characters) and the first phone (32 bytes); else the first e-mail,
    else the organization. None when a part is outside what summarize restates (the comparer then skips it)."""
    name = summarize(p.get("display_name", ""), 80)
    phones = [v["value"] for v in p.get("phones", [])]
    phone = ""
    for v in phones:
        if not v.isascii():
            return None  # the byte limit of a non-ASCII phone is not restated
        s = summarize(v, 32)
        if s is None:
            return None
        if s:
            phone = s
            break
    if name is None:
        return None
    if name or phone:
        return " ".join(x for x in (name, phone) if x)
    for v in p.get("emails", []):
        s = summarize(v["value"], 80)
        if s is None:
            return None
        if s:
            return s
    o = p.get("organization") or {}
    for k in ("name", "title", "department"):
        s = summarize(o.get(k, ""), 80)
        if s is None:
            return None
        if s:
            return s
    return ""


class Joins:
    """The tables a person row joins, as the engine reads them back."""

    def __init__(self, view):
        self.labels = {r["rowid"]: r.get("value") for r in view.get("ABMultiValueLabel", [])}
        self.keys = {r["rowid"]: r.get("value") for r in view.get("ABMultiValueEntryKey", [])}
        self.mv = {}
        for r in view.get("ABMultiValue", []):
            self.mv.setdefault(r.get("record_id"), []).append(r)
        self.entries = {}
        for r in view.get("ABMultiValueEntry", []):
            self.entries.setdefault(r.get("parent_id"), []).append(r)
        self.has_store = "ABStore" in view
        self.stores = {r["rowid"]: r for r in view.get("ABStore", [])}
        self.groups = {r["rowid"]: r for r in view.get("ABGroup", [])}
        self.members = {}
        for r in view.get("ABGroupMembers", []):
            self.members.setdefault(r.get("member_id"), []).append(r)


def put_instant(raw, col, v, status):
    if is_real(v) and not math.isfinite(v):
        raw.update(I.real_cells(col, v))
        return
    raw[col] = jf(v)
    if status != "valid":
        raw[col + "_status"] = status


def expect_person(r, J, photos):
    p, raw, unknown, tf = {}, {}, {}, set()
    rid = r["rowid"]

    def text(col, limit=MAX_TEXT):
        v = r.get(col)
        if v is None:
            return ""
        assert is_text(v), "the scenario holds no BLOB in text column %s" % col
        c = C.clean(v, limit)
        assert not c.truncated, "no text of the scenario reaches its cap"
        if c.flags:
            tf.add(col)
        return c.text

    def joined(v, what):
        """A text cell of a joined table (never hostile in the scenario)."""
        assert v is None or is_text(v), what
        c = C.clean(v or b"", MAX_TEXT)
        assert not c.flags, "no joined text of the scenario needs cleaning: " + what
        return c.text

    first, last, middle, prefix, suffix, nick = (text(c) for c in ("First", "Last", "Middle", "Prefix", "Suffix", "Nickname"))
    phonetic = " ".join(x for x in (text("FirstPhonetic"), text("MiddlePhonetic"), text("LastPhonetic")) if x)
    names = {k: v for k, v in (("given", first), ("family", last), ("middle", middle), ("prefix", prefix),
                               ("suffix", suffix), ("nickname", nick), ("phonetic", phonetic)) if v}
    if names:
        p["names"] = names
    stored = text("DisplayName")
    if stored:
        p["display_name"], raw["display_name_source"] = stored, "stored"
    else:
        composed = " ".join(x for x in (prefix, first, middle, last, suffix) if x)
        if composed:
            p["display_name"], raw["display_name_source"] = composed, "composed"
    org = {k: v for k, v in (("name", text("Organization")), ("title", text("JobTitle")),
                             ("department", text("Department"))) if v}
    if org:
        p["organization"] = org
    # --- stored scalars
    kind = r.get("Kind")
    if is_int(kind):
        raw["Kind"] = kind
    elif kind is not None:
        unknown["Kind"] = "type-mismatch"
    note = text("Note", MAX_NOTE)
    if note:
        raw["Note"] = note
    birthday = text("Birthday")
    if birthday:
        raw["birthday"] = birthday  # 4E-R23: never derived
    times, t = {}, None
    for col, key in (("CreationDate", "created"), ("ModificationDate", "modified")):
        v = r.get(col)
        if is_int(v) or is_real(v):
            st = I.cocoa_status(v, "s")
            put_instant(raw, col, v, st)
            if st == "valid":
                times[key] = I.cocoa_iso(v, "s")
        elif v is not None:
            unknown[col] = "type-mismatch"
    t = times.get("modified") or times.get("created")
    p["source_id"] = str(rid)
    ids = [x for x in (text("guid"), text("ExternalIdentifier"), text("ExternalUUID")) if x]
    if ids:
        p["source_ids"] = ids
    # --- the store
    if "StoreID" in r:
        sid = r["StoreID"]
        if is_int(sid):
            assert J.has_store, "the scenario never has StoreID without ABStore"
            st = J.stores.get(sid)
            if st is None:
                raw["store_state"] = "no-store-row"
            else:
                a = {"type": "ab-store"}
                if joined(st.get("Name"), "ABStore.Name"):
                    a["name"] = joined(st.get("Name"), "ABStore.Name")
                p["accounts"] = [a]
        elif sid is not None:
            unknown["StoreID"] = "type-mismatch"
    # --- multi-values, in UID order
    lists = {"phones": [], "emails": [], "urls": []}
    totals = {"phones": 0, "emails": 0, "urls": 0}
    label_raw, unresolved, others, addrs, unmapped = [], 0, [], [], []
    for mv in J.mv.get(rid, []):
        prop = mv.get("property")
        assert is_int(prop)
        lab_id, lab, lab_stored = mv.get("label"), "", None
        if is_int(lab_id):
            if lab_id not in J.labels:
                unresolved += 1
                stored_label = None
            else:
                stored_label = joined(J.labels[lab_id], "ABMultiValueLabel.value")
                assert stored_label, "the scenario holds no empty label"
        else:
            assert lab_id is None
            stored_label = None
        if stored_label is not None:
            x = wrapped(stored_label)
            lab, lab_stored = (x, stored_label) if x else (stored_label, None)
        value = joined(mv.get("value"), "ABMultiValue.value")
        if prop in LISTS:
            assert value, "the scenario holds no empty list value"
            name = LISTS[prop]
            totals[name] += 1
            e = {"value": value}
            if lab:
                e["label"] = lab
            if len(lists[name]) < MAX_LIST:
                if lab_stored:
                    label_raw.append({"list": name, "index": len(lists[name]), "label": lab_stored})
                lists[name].append(e)
        elif prop == ADDRESS:
            a, un = {}, []
            if value:
                a["formatted"] = value
            for en in J.entries.get(mv["rowid"], []):
                key = joined(J.keys.get(en.get("key")), "ABMultiValueEntryKey.value")
                assert key, "the scenario names every entry key"
                v = joined(en.get("value"), "ABMultiValueEntry.value")
                if key in ADDRESS_KEYS:
                    if v:
                        a[ADDRESS_KEYS[key]] = v
                else:
                    un.append({"key": key, "value": v})
            assert a, "the scenario's addresses have a mapped part"
            idx = len(addrs)
            if lab_stored:
                label_raw.append({"list": "addresses", "index": idx, "label": lab_stored})
            if lab:
                a["label"] = lab
            for u in un:
                unmapped.append({"index": idx, "key": u["key"], "value": u["value"]})
            addrs.append(a)
        else:
            e = {"property": prop}
            if stored_label is not None:
                e["label"] = stored_label
            if value:
                e["value"] = value
            others.append(e)
    for name in ("phones", "emails", "urls"):
        if lists[name]:
            p[name] = lists[name]
        if totals[name] > MAX_LIST:
            raw[name + "_total"], raw[name + "_truncated"] = totals[name], True
    if addrs:
        p["addresses"] = addrs
    if unmapped:
        raw["address_keys_unmapped"] = unmapped
    if label_raw:
        raw["label_raw"] = label_raw
    if unresolved:
        raw["labels_unresolved"] = unresolved
    if others:
        assert len(others) <= MAX_LIST
        raw["other_values"] = others
    # --- groups
    gnames, gother = [], 0
    for m in J.members.get(rid, []):
        assert is_int(m.get("member_type"))
        if m["member_type"] == 0:
            g = J.groups.get(m.get("group_id"))
            assert g is not None, "the scenario's group members name a group"
            gnames.append(joined(g.get("Name"), "ABGroup.Name"))
        else:
            gother += 1
    if gnames:
        raw["groups"] = sorted(gnames)
    if gother:
        raw["group_members_other"] = gother
    if tf:
        raw["text_flags"] = sorted(tf)
    if unknown:
        raw["unknown_columns"] = unknown
    assert raw, "every person of the scenario has a non-empty raw"
    p["raw"] = raw
    if photos is not None:
        p["photo"] = {"present": rid in photos}
    identifies = bool(p.get("display_name") or p.get("phones") or p.get("emails") or org)
    if not identifies:
        return None
    return {"table": "ABPerson", "rowid": rid, "file": "any", "type": "contact", "source_path": "", "time": t,
            "time_end": None, "times": times, "deleted": False, "summary": contact_summary(p),
            "body": contact_body(p) or None, "payload": p}


def column_notes(cols, notes):
    names = [n.lower() for n, _ in PERSON_MAPPED]
    lowered = [c.lower() for c in cols]
    opt = [n for n, req in PERSON_MAPPED if not req and n.lower() in lowered]
    v = "columns:%d" % len(cols)
    if opt:
        v += ";optional:" + ",".join(opt)
    notes["schema.ABPerson"] = v
    un = [c for c in cols if c.lower() not in names]
    if un:
        notes["unmapped_columns.ABPerson"] = ",".join(un)


def photo_ids(images):
    """The ROWIDs the images database holds a thumbnail or a full-size image for, read back from the file the
    generator wrote (both tables exist in it, so both are decided)."""
    view = C.engine_view({"db": images}, ["ABThumbnailImage", "ABFullSizeImage"])
    assert set(view) == {"ABThumbnailImage", "ABFullSizeImage"}
    return {r["record_id"] for t in view.values() for r in t}


def oracle(files, ddl_name, variant, cols):
    view = C.engine_view(files, TABLES)
    J = Joins(view)
    photos = photo_ids(files["images"]) if "images" in files else None
    exp = C.expect_header(FAMILY, variant, ddl_name)
    for r in view["ABPerson"]:
        e = expect_person(r, J, photos)
        if e is None:
            exp["warnings"].append({"table": "ABPerson", "rowid": r["rowid"], "contains": "identifies nobody"})
        else:
            exp["records"].append(e)
    column_notes(cols, exp["notes"])
    return exp


# ---- scenario ----------------------------------------------------------------------------

class Book:
    """Rows of every table; multi-value rows get their UID in insertion order."""

    def __init__(self):
        self.t = {n: [] for n in TABLES}
        self._uid = 0

    def person(self, rid, **kw):
        row = {"ROWID": rid, "Kind": 0}
        row.update(kw)
        self.t["ABPerson"].append(row)

    def mv(self, rid, prop, label=None, value=None, entries=None, **kw):
        self._uid += 1
        row = {"UID": self._uid, "record_id": rid, "property": prop, "identifier": self._uid, "label": label, "value": value}
        row.update(kw)
        self.t["ABMultiValue"].append(row)
        for k, v in (entries or []):
            self.t["ABMultiValueEntry"].append({"parent_id": self._uid, "key": k, "value": v})

    def labels(self, *names):
        self.t["ABMultiValueLabel"] += [{"value": n} for n in names]

    def keys(self, *names):
        self.t["ABMultiValueEntryKey"] += [{"value": n} for n in names]

    def group(self, rid, name, members):
        self.t["ABGroup"].append({"ROWID": rid, "Name": name})
        for mt, mid in members:
            self.t["ABGroupMembers"].append({"group_id": rid, "member_type": mt, "member_id": mid})


def cs(y, mo, d):
    import datetime
    return int((datetime.datetime(y, mo, d, tzinfo=I.UTC) - datetime.datetime(2001, 1, 1, tzinfo=I.UTC)).total_seconds())


def current_book():
    b = Book()
    b.t["ABStore"] += [{"ROWID": 1, "Name": "iCloud", "Type": 2, "Enabled": 1},
                       {"ROWID": 2, "Name": "On My iPhone", "Type": 0, "Enabled": 1}]
    # label rowids 1..6; 99 is dangling
    b.labels("_$!<Mobile>!$_", "_$!<Home>!$_", "_$!<Work>!$_", "Lab line", "_$!<HomePage>!$_", "Skype")
    b.keys("Street", "City", "State", "ZIP", "Country", "CountryCode", "Weird")
    b.person(1, Prefix="Dr.", First="Alice", Middle="B.", Last="Smith", Suffix="Jr.", FirstPhonetic="Ah-liss",
             MiddlePhonetic="Bee", LastPhonetic="Smyth", Nickname="Ali", DisplayName="Alice Smith",
             Organization="Acme Corp", Department="R&D", JobTitle="CEO", Note="Likes tea\nand coffee",
             Birthday="1985-03-04", CreationDate=700000000, ModificationDate=700000100.5, StoreID=1,
             guid="G-0001", ExternalIdentifier="ext-0001", ExternalUUID="UUID-0001",
             FirstSort="alice", LastSort="smith", CompositeNameFallback="Alice Smith", ExternalModificationTag="tag-1",
             ExternalRepresentation=b"\x00vcard\x01")
    b.mv(1, 3, 1, "+1 555 0100")
    b.mv(1, 4, 3, "alice@example.test")
    b.mv(1, 3, 2, "+1 555 0101")
    b.mv(1, 3, 4, "+1 555 0102")
    b.mv(1, 22, 5, "https://example.test/alice")
    b.mv(1, 3, 99, "+1 555 0103")
    b.mv(1, 4, None, "alice.home@example.test")
    b.mv(1, 13, 6, "alice_im")
    b.mv(1, 5, 2, "1 Main St\nSpringfield", entries=[(1, "1 Main St"), (2, "Springfield"), (3, "IL"), (4, "62701"),
                                                     (5, "United States"), (6, "us"), (7, "x")])
    b.mv(1, 5, None, None, entries=[(1, "2 Side Rd"), (5, "Canada"), (6, "ca")])
    b.person(2, First="Bob", Last="Jones", CreationDate=700000200, StoreID=2, ExternalIdentifier="ext-0002")
    b.mv(2, 3, 2, "+1 555 0200")
    b.mv(2, 4, 3, "bob@example.test")
    b.person(3, Organization="Initech", DisplayName="", Kind=1, ModificationDate=700000300)
    b.person(4, First="דני", Last="כהן", DisplayName="محمد ‮evil‬",
             ModificationDate=700000400)
    b.mv(4, 3, 1, "+972 50 000 0004")
    b.person(5, First="太郎", Last="田中")
    b.mv(5, 4, None, "taro@example.test")
    b.person(6, First="Zoë \U0001F600", Last="O'Brien")
    b.mv(6, 3, 1, "+44 20 7946 0006")
    b.person(7, First="Nora", Note=C.Raw(b"before\x00after"))
    b.mv(7, 3, None, "+1 555 0700")
    b.person(8, First="Eve", Last=C.Raw(b"Bad\xffName"))
    b.mv(8, 3, None, "+1 555 0800")
    b.person(9, Nickname="ghost", Note="just a note")
    b.mv(9, 22, None, "https://example.test/ghost")
    b.person(10, First="Gina", Last="Dates", CreationDate=0, ModificationDate=cs(2100, 6, 1))
    b.mv(10, 3, None, "+1 555 1000")
    b.person(11, First="Hal", Last="Inf", CreationDate=700000500, ModificationDate=float("inf"), Birthday="489024000")
    b.mv(11, 3, None, "+1 555 1100")
    b.person(12, First="Ivy", Last="Store", StoreID=9, ModificationDate=700000600.25)
    b.mv(12, 3, None, "+1 555 1200")
    b.person(13, First="Jon", Last="Types", Kind="company", CreationDate="yesterday")
    b.mv(13, 3, None, "+1 555 1300")
    b.group(1, "Family", [(0, 1), (0, 2), (1, 12)])
    b.group(2, "Friends", [(0, 1), (2, 12)])
    b.group(3, "Work", [(0, 12), (0, 77)])
    return b


def older_book():
    b = Book()
    b.labels("_$!<Mobile>!$_", "_$!<Home>!$_", "Boat phone")
    b.keys("Street", "City", "ZIP")
    b.person(1, Prefix="Ms.", First="Carol", Last="White", Birthday="1970-01-01", CreationDate=700000000.75,
             ModificationDate=700000900, ExternalIdentifier="old-1")
    b.mv(1, 3, 1, "+1 555 1001")
    b.mv(1, 4, 2, "carol@example.test")
    b.mv(1, 5, 2, None, entries=[(1, "9 Old Way"), (2, "Oldtown"), (3, "00001")])
    b.person(2, Organization="Globex", JobTitle="Buyer", Kind=1)
    b.person(3, First="Dan", Middle="Q.", Last="Public", Suffix="III")
    b.mv(3, 3, 3, "+1 555 1003")
    b.person(4, Nickname="Zed")
    b.mv(4, 4, None, "zed@example.test")
    b.group(1, "Family", [(0, 1)])
    return b


def images_book():
    b = Book()
    b.person(1, First="Thumb", Last="Only")
    b.mv(1, 3, None, "+1 555 2001")
    b.person(2, First="Full", Last="Size")
    b.mv(2, 3, None, "+1 555 2002")
    b.person(3, First="No", Last="Photo")
    b.mv(3, 3, None, "+1 555 2003")
    return b


def huge_book():
    b = Book()
    b.person(1, First="Huge", Last="Contact")
    for i in range(1200):
        b.mv(1, 3, None, "+1555%07d" % i)
    for i in range(1100):
        b.mv(1, 4, None, "user%d@example.test" % i)
    return b


# ---- building ----------------------------------------------------------------------------

def build(work, name, ddl_name, book):
    path = work + "/" + name
    con = C.connect(path)
    con.executescript(C.load_ddl(ddl_name, SCHEMAS)[0])
    with C.batch(con):
        for table in TABLES:
            if not C.columns(con, table):
                continue
            for r in book.t[table]:
                C.insert(con, table, r)
    cols = C.columns(con, "ABPerson")
    con.close()
    return path, cols


def images_db(work):
    path = work + "/images.db"
    con = C.connect(path)
    con.executescript(C.load_ddl("addressbookimages.sql", SCHEMAS)[0])
    with C.batch(con):
        C.insert(con, "ABThumbnailImage", {"ROWID": 1, "record_id": 1, "format": 3, "derived_from_format": None,
                                          "data": b"THUMBNAIL-1"})
        C.insert(con, "ABFullSizeImage", {"ROWID": 1, "record_id": 2, "crop_x": 0, "crop_y": 0, "crop_width": 64,
                                         "crop_height": 64, "data": b"FULLSIZE-2"})
    con.close()
    return C.read(path)


def variant(work, name, ddl_name, book, images=None):
    path, cols = build(work, "a.db", ddl_name, book)
    files = {"db": C.read(path)}
    if images is not None:
        files["images"] = images
    return files, oracle(files, ddl_name, name, cols)


def variant_current(work):
    return variant(work, "current", "addressbook-current.sql", current_book())


def variant_images(work):
    return variant(work, "with-images", "addressbook-current.sql", images_book(), images_db(work))


def variant_older(work):
    return variant(work, "older-schema", "addressbook-older.sql", older_book())


def variant_huge(work):
    return variant(work, "huge-contact", "addressbook-current.sql", huge_book())


VARIANTS = {"current": variant_current, "with-images": variant_images, "older-schema": variant_older,
            "huge-contact": variant_huge}


def _selfcheck():
    assert wrapped("_$!<Mobile>!$_") == "Mobile" and wrapped("Lab line") is None and wrapped("_$!<>!$_") is None
    assert I.cocoa_iso(700000000, "s") == "2023-03-08T20:26:40Z" and I.cocoa_iso(700000100.5, "s") == "2023-03-08T20:28:20.5Z"
    assert I.cocoa_status(cs(2100, 6, 1), "s") == "invalid" and I.cocoa_status(0, "s") == "sentinel"
    assert summarize("a‮b", 80) == "a<U+202E>b" and summarize("x" * 81, 80) is None
    assert summarize("a \n\t b", 80) == "a b"
    print("ios_addressbook self-checks OK")


if __name__ == "__main__":
    _selfcheck()

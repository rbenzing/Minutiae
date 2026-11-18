"""iOS WhatsApp ChatStorage.sqlite fixtures (plan 4I2, task 2): ios.whatsapp.chats.

    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-chats --out internal/parsers/ioswhatsapp/testdata \
        --pins-go internal/parsers/ioswhatsapp/pins_test.go --pins-package ioswhatsapp_test
    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-chats --check
    py -3 -I tools/fixtures/telephony_gen.py --family whatsapp-ios-chats --recover-oracle --out internal/parsers/ioswhatsapp/testdata

The databases are written by the engine (Python's sqlite3), the WAL variants by a child process that dies without closing
(C.kill_child). The DDL is constructed from memory (schemas/whatsapp-ios/CROSSCHECK.md), so a fixture proves what the parser
does with the column set the sources report, not what an installed app writes.

The oracle is a SECOND READING OF THE SPEC (spec 6.7, 6.8, 6.11, 7, the vocabularies of plan 4I2 4I2-R7 and the rulings
4I2-R6, PF1 to PF7) over the stored values read back from the engine on a COPY of the finished files. It shares no code with
the Go parser, but it is written by the same author as the Go design and is not independent of it: a wrong reading shared by
both would pass. The literal facts of oracle_facts_test.go are written beside it for that reason (4I2-R13).

Variants (the placement of each is in internal/parsers/ioswhatsapp/fixtures_test.go placeOf):
  ios-chatstorage              consumer domain, with -wal: CORE-iOS, row 45 in a committed WAL frame, 46 in an uncommitted one
  ios-chatstorage-business     Business domain, sessions 1, messages 1-3
  ios-chatstorage-extract      an app group of a full-file-system extraction, sessions 1, messages 1-3
  ios-chatstorage-renumbered   Z_PRIMARYKEY numbers the entities otherwise (the parser never hard-codes them, 4I2-R3)
  ios-chatstorage-nomodel      no Z_PRIMARYKEY table
  ios-chatstorage-othermodel   Z_PRIMARYKEY names only CallRecord
  ios-chatstorage-otherapp     a same-named database of another app: one table ZMESSAGE(Z_PK, ZTEXT)
  ios-chatstorage-nooptional   no ZWAGROUPMEMBER, ZWAMESSAGEINFO or ZWAMEDIAITEM table (plan 4I2 PF3: the absence is stated)
  ios-chatstorage-deleted      deleted ZWAMESSAGE rows and an old WAL frame (plan 4I2 PF2): the typed recovery scenario
  ios-chatstorage-deleted-freelist  no WAL: deleted ZWAMESSAGE rows on freelist pages, with a deleted image and a deleted revoke

Python 3 with its bundled sqlite3 only; no Docker. Not part of gen.sh all.
"""
import base64
import hashlib
import math

import ios_common as I
import telephony_common as C
import whatsapp_gen as W
import whatsapp_ios_common as X

FAMILY = "whatsapp-ios-chats"
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle, PF2)
CLAIMED = ("ZWAMESSAGE",)
ALIAS = "Z_PK"

UDID = "UDID1"
DOMAIN_WA = "AppDomainGroup-" + X.GROUP_WA
DOMAIN_SMB = "AppDomainGroup-" + X.GROUP_SMB
REL = "ChatStorage.sqlite"
APP_GROUP = "5A0B4C3D-0000-4000-8000-00000000A001"
EXT_DIR = "/private/var/mobile/Containers/Shared/AppGroup/" + APP_GROUP
EXT_REMOTE = EXT_DIR + "/" + REL
EXT_CONTAINER = "ios:HomeDomain/Containers/Shared/AppGroup/" + APP_GROUP

D = 600000000  # Cocoa seconds of 2020-01-06T10:40:00Z
P1 = X.jid("15550000001", "s.whatsapp.net")
P2 = X.jid("15550000002", "s.whatsapp.net")
P3 = X.jid("15550000003", "s.whatsapp.net")
P4 = X.jid("15550000004", "s.whatsapp.net")
P99 = X.jid("15550000099", "s.whatsapp.net")
GRP = X.jid("120363000000000001", "g.us")
GRP2 = X.jid("120363000000000002", "g.us")
GRP3 = X.jid("120363000000000003", "g.us")
STATUS = "status@broadcast"
BCAST = "1234@broadcast"
NUL_JID = C.Raw(b"1555\x00" + b"0000007@s.whatsapp.net")
VCARD = "BEGIN:VCARD\r\nFN:Ann\r\nEND:VCARD\r\n"
MAX_TEXT = X.MAX_TEXT
MAX_ADDR = W.MAX_ADDR

# the Core Data entities of ChatStorage as (name, Z_ENT, Z_SUPER): the numbers a parser must read, never hard-code (4I2-R3)
MODEL = [("WAChatSession", 4, 0), ("WAGroupInfo", 5, 0), ("WAGroupMember", 6, 0), ("WAMediaItem", 8, 0),
         ("WAMessage", 9, 0), ("WAMessageInfo", 11, 0)]
MODEL_RENUMBERED = [("WAMessage", 2, 0), ("WAChatSession", 3, 0), ("WAGroupInfo", 4, 0), ("WAGroupMember", 5, 0),
                    ("WAMediaItem", 6, 0), ("WAMessageInfo", 7, 0)]
MODEL_OTHER = [("CallRecord", 2, 0)]

MESSAGE_TYPE = {0: "text", 1: "image", 2: "video", 3: "audio", 4: "contact_card", 5: "location", 6: "group_event",
                7: "link", 8: "document", 10: "system_notice", 11: "gif", 14: "revoked", 15: "sticker"}
FILE_TYPES = {1, 2, 3, 8, 11, 15}  # the types whose ZVCARDSTRING is a MIME type and whose item is an attachment (spec 6.7)
MEDIA_TYPES = FILE_TYPES | {4, 5, 7}  # the kinds "media": their ZWAMEDIAITEM row is joined
GROUP_EVENT = {2: "participant_added", 3: "participant_left", 4: "group_icon_changed"}
MESSAGE_STATUS = {6: "delivered", 8: "read_by_recipient"}
SESSION_TYPE = {0: "private", 1: "group", 2: "broadcast", 3: "status"}
OPTIONAL = ("ZWAGROUPMEMBER", "ZWAMESSAGEINFO", "ZWAMEDIAITEM")
# the tables the plan lists as unmapped for the chats parser, in its order ("Notes and warnings (exact texts)")
UNMAPPED = ["ZWAPROFILEPUSHNAME", "ZWAGROUPMEMBERSCHANGE", "ZWAGROUPINFO", "ZWAMESSAGEDATAITEM", "ZWAVCARDMENTION",
            "ZWACHATPROPERTIES", "ZWACHATPUSHCONFIG", "ZWABLACKLISTITEM", "ZWAZ1PAYMENTTRANSACTION"]
TABLES = ["ZWAMESSAGE", "ZWACHATSESSION", "ZWAGROUPMEMBER", "ZWAMEDIAITEM", "ZWAMESSAGEINFO", "Z_PRIMARYKEY"]


def jf(x):
    """A float as Go's encoding/json prints it: an integral value without a fraction."""
    if isinstance(x, float) and x.is_integer() and abs(x) < 1e15:
        return int(x)
    return x


# ----------------------------------------------------------------------------------------- the CORE-iOS scenario

def sessions(ent):
    rows = [
        (1, dict(ZCONTACTJID=P1, ZSESSIONTYPE=0, ZPARTNERNAME="Ann")),
        (2, dict(ZCONTACTJID=GRP, ZSESSIONTYPE=1, ZPARTNERNAME="Group A", ZGROUPINFO=1)),
        (3, dict(ZCONTACTJID=STATUS, ZSESSIONTYPE=3)),
        (4, dict(ZCONTACTJID=BCAST, ZSESSIONTYPE=2, ZPARTNERNAME="List")),
        (5, dict(ZCONTACTJID=None, ZSESSIONTYPE=0)),
        (6, dict(ZCONTACTJID=GRP2, ZSESSIONTYPE=None)),
        (7, dict(ZCONTACTJID=NUL_JID, ZSESSIONTYPE=0)),
        (8, dict(ZCONTACTJID=GRP3, ZSESSIONTYPE=0)),  # a group JID on a type-0 session: a group for the sender, never an address_conflict
    ]
    return [dict(Z_PK=pk, Z_ENT=ent, Z_OPT=1, **kw) for pk, kw in rows]


def group_members(ent):
    rows = [(1, 2, P2), (2, 2, None), (3, 6, P4), (4, 2, GRP)]
    return [dict(Z_PK=pk, Z_ENT=ent, Z_OPT=1, ZCHATSESSION=s, ZMEMBERJID=j) for pk, s, j in rows]


def group_info(ent):
    return [dict(Z_PK=1, Z_ENT=ent, Z_OPT=1, ZCHATSESSION=2, ZCREATORJID=P2, ZSTATE=1)]


def msg(pk, session=1, me=0, typ=0, **kw):
    """A ZWAMESSAGE row: session, ZISFROMME, ZMESSAGETYPE, then columns. ZMESSAGEDATE is D + pk; a text row has text."""
    r = dict(Z_PK=pk, Z_OPT=1, ZCHATSESSION=session, ZISFROMME=me, ZMESSAGETYPE=typ, ZMESSAGEDATE=D + pk)
    if typ == 0 and "ZTEXT" not in kw:
        r["ZTEXT"] = "text %d" % pk
    r.update(kw)
    return r


def big_text():
    return "x" * (300 << 10)  # 300 KiB: over the 256 KiB text cap


def core_messages(wal_row=True):
    """The CORE-iOS messages 1 to 44 (and 45, the row the WAL carries, with wal_row)."""
    m = [
        msg(1, 1, 0, 0, ZTEXT="hello", ZFROMJID=P1, ZSTANZAID="3EB0A001", ZMESSAGESTATUS=0),
        msg(2, 1, 1, 0, ZTEXT="hi", ZTOJID=P1, ZMESSAGESTATUS=6),
        msg(3, 1, 1, 0, ZTEXT="seen", ZMESSAGESTATUS=8),
        msg(4, 1, 0, 1, ZMEDIAITEM=1),
        msg(5, 1, 0, 2),
        msg(6, 1, 0, 3),
        msg(7, 1, 0, 4),
        msg(8, 1, 0, 5),
        msg(9, 1, 0, 1),
        msg(10, 2, 0, 0, ZTEXT="g", ZGROUPMEMBER=1, ZFROMJID=GRP),
        msg(11, 2, 0, 0, ZFROMJID=GRP),
        msg(12, 2, 0, 0, ZGROUPMEMBER=2, ZFROMJID=GRP),
        msg(13, 2, 0, 0, ZGROUPMEMBER=99, ZFROMJID=GRP),
        msg(14, 2, 0, 0, ZGROUPMEMBER=4, ZFROMJID=GRP),
        msg(15, 2, 1, 0, ZTEXT="out", ZTOJID=GRP),
        msg(16, 3, 0, 0, ZFROMJID=P3, ZGROUPMEMBER=1, ZPUSHNAME="Ann"),
        msg(17, 3, 1, 0, ZTOJID=STATUS),
        msg(18, 2, 0, 6, ZGROUPEVENTTYPE=2),
        msg(19, 2, 0, 6, ZGROUPEVENTTYPE=12, ZTEXT="New name"),
        msg(20, 1, 0, 10, ZGROUPEVENTTYPE=1),
        msg(21, 1, 0, 14, ZTEXT="leftover"),
        msg(22, 1, 0, 999),
        msg(23, 1, 0, 12),
        msg(24, ZMESSAGEDATE=None),
        msg(25, ZMESSAGEDATE=600000000000),
        msg(26, ZMESSAGEDATE=600000000.25),
        msg(27, ZMESSAGEDATE=600000000),
        msg(28, 1, 0, 1, ZMEDIAITEM=5),
        msg(29, 1, 0, 1, ZMEDIAITEM=1),
        msg(30, 1, 0, 1),
        msg(31, 1, 0, 1),
        msg(32, 1, 0, 1),
        msg(33, 1, 0, 1),
        msg(34, 1, 0, 0, ZFROMJID=P99),
        msg(35, 5),
        msg(36, 77),
        msg(37, 1, 2),
        msg(38, 1, 0, 0, ZTEXT=big_text()),
        msg(39, 4, 1, 0),
        msg(40, 6, 0, 0, ZGROUPMEMBER=3, ZFROMJID=GRP2),
        msg(41, 6, 0, 0, ZFROMJID=GRP2),
        msg(42, 7),
        msg(43, 1, 0, 15),
        msg(44, 1, 0, 0, ZMESSAGEINFO=1),
        msg(47, 8, 0, 0, ZGROUPMEMBER=1, ZFROMJID=P99),
    ]
    if wal_row:
        m.append(msg(45, 1, 0, 0))
    return m


IMG4 = "Media/15550000001@s.whatsapp.net/a/b/IMG-0004.jpg"
IMG9 = "Media/15550000001@s.whatsapp.net/a/b/IMG-0009.jpg"
IMG28 = "Media/15550000001@s.whatsapp.net/a/b/IMG-0028.jpg"
STK43 = "Media/15550000001@s.whatsapp.net/a/b/STK-0043.webp"
META20 = bytes(range(1, 21))
RECEIPT10 = bytes(range(0xB0, 0xBA))
MEDIA_KEY = b"\x01\x02\x03\x04"


def media_items():
    """(Z_PK, ZMESSAGE, columns): the ZWAMEDIAITEM rows of the CORE-iOS scenario."""
    jpg = dict(ZVCARDSTRING="image/jpeg", ZFILESIZE=1000)
    return [
        (1, 4, dict(ZTITLE="pic", ZMEDIALOCALPATH=IMG4, ZVCARDSTRING="image/jpeg", ZFILESIZE=12345, ZMETADATA=META20)),
        (2, 7, dict(ZVCARDSTRING=VCARD)),
        (3, 8, dict(ZLATITUDE=52.52, ZLONGITUDE=13.405)),
        (4, 9, dict(ZLATITUDE=640.0, ZLONGITUDE=480.0, ZMEDIALOCALPATH=IMG9, **jpg)),
        (5, 28, dict(ZMEDIALOCALPATH=IMG28, **jpg)),
        (6, 21, dict(ZVCARDNAME="x1")),
        (7, 30, dict(ZMEDIALOCALPATH="Media/a*b.jpg", **jpg)),
        (8, 31, dict(ZMEDIALOCALPATH="../escape.jpg", **jpg)),
        (9, 32, dict(ZMEDIALOCALPATH=None, **jpg)),
        (10, 43, dict(ZMEDIALOCALPATH=STK43, ZVCARDSTRING="image/webp", ZFILESIZE=777)),
        # the media rows of the messages the freelist variant deletes: the rows stay, so a recovered message meets its join
        (11, 150, dict(ZMEDIALOCALPATH="Media/15550000001@s.whatsapp.net/a/b/IMG-0150.jpg", ZVCARDSTRING="image/jpeg", ZFILESIZE=555)),
        (12, 151, dict(ZVCARDNAME="gone1")),
    ]


# the files the scenario holds beside the consumer-domain database: (relative path in the domain, bytes, device)
EXTRAS = [
    ("Message/" + IMG4, b"JPG4", UDID),     # the same device and backup: the one link (message 4)
    ("Message/" + STK43, b"WEBP", "IOS2"),  # another device: found, but of another source (message 43)
]


def small_messages():
    """The three text messages of the small variants (sessions 1, messages 1-3)."""
    return core_messages(False)[:3]


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
        return ("whatsapp_business" if domain == DOMAIN_SMB else "whatsapp"), "backup_domain", None
    return "unknown", None, "no-container-metadata"


def address(b, raw, field):
    """A JID cell (bytes): (state, text). altered: cleaning changed it (never parsed, the cleaned text in raw under field);
    too-long: over the contract's limit (never cut)."""
    cl = W.text_of(b)
    if cl.flags:
        C.mark(raw, field)
        return "altered", cl.text
    if len(cl.text.encode("utf-8")) > MAX_ADDR:
        return "too-long", None
    return None, cl.text


def chat_of(ctx, raw, unknown, key):
    """The chat session of a message: {str, state, cls, type, title, group}; the stored facts of the session are in raw."""
    out = {"str": None, "state": None, "cls": None, "type": None, "title": None}
    if key is None or key == 0:
        out["state"] = "absent"
        return out
    sess = ctx["sessions"].get(key)
    if sess is None:
        out["state"] = "no-ZWACHATSESSION-row"
        return out
    c = X.Cells(sess, raw, unknown, "ZWACHATSESSION.")
    stype = c.int("ZSESSIONTYPE")
    s = {}
    for k in ("ZSESSIONTYPE", "ZREMOVED", "ZHIDDEN", "ZFLAGS"):
        v = c.int(k)
        if v is not None:
            s[k] = v
    if s:
        raw["session"] = s
    out["type"] = stype
    if stype in SESSION_TYPE:
        raw["label_ZSESSIONTYPE"] = SESSION_TYPE[stype]
    b = sess.get("ZCONTACTJID")
    if b is not None and not X.is_text(b):
        unknown["ZWACHATSESSION.ZCONTACTJID"] = "type-mismatch"
        out["state"] = "undecided"
    elif b is None or len(b) == 0:
        out["state"] = "no-contact-jid"
    else:
        state, text = address(b, raw, "chat_jid")
        if state == "altered":
            raw["chat_jid"] = text
            out["state"] = "altered"
        elif state == "too-long":
            out["state"] = "too-long"
        else:
            out["str"] = text
            raw["chat_jid"] = text
            out["cls"] = jid_class(text)
            raw["chat_class"] = out["cls"]
    if stype == 1:
        t = c.text("ZPARTNERNAME")
        if t:
            out["title"] = X.cleaned(raw, "thread.title", t)
    return out


def sender_of(ctx, raw, unknown, key):
    """The sender of an incoming group message through ZWAGROUPMEMBER: {str, state}."""
    if key is None or key == 0:
        return {"str": None, "state": "absent"}
    if not ctx["has_members"]:
        undecided(raw, "ZWAGROUPMEMBER")
        return {"str": None, "state": "undecided"}
    row = ctx["members"].get(key)
    if row is None:
        return {"str": None, "state": "no-ZWAGROUPMEMBER-row"}
    b = row.get("ZMEMBERJID")
    if b is not None and not X.is_text(b):
        unknown["ZWAGROUPMEMBER.ZMEMBERJID"] = "type-mismatch"
        return {"str": None, "state": "undecided"}
    if b is None or len(b) == 0:
        return {"str": None, "state": "no-member-jid"}
    state, text = address(b, raw, "member_jid_text")
    if state == "altered":
        raw["member_jid_text"] = text
        return {"str": None, "state": "altered"}
    if state == "too-long":
        return {"str": None, "state": "too-long"}
    if jid_class(text) in ("group", "broadcast", "status"):
        return {"str": None, "state": "not-a-person"}
    return {"str": text, "state": None}


def undecided(raw, table):
    lst = raw.setdefault("join_undecided", [])
    if table not in lst:
        lst.append(table)
        lst.sort()


def media_join(ctx, r, c, raw):
    """The ZWAMEDIAITEM row of a message: ('row', item), ('none', None) or ('undecided', None) (4I2-P4, spec 6.7). F = the
    items whose ZMESSAGE is this message; B = the item the message's ZMEDIAITEM names."""
    if not ctx["has_items"]:
        undecided(raw, "ZWAMEDIAITEM")
        return "undecided", None
    pk = r["rowid"]
    found = [it for it in ctx["items"] if it.get("ZMESSAGE") == pk]
    b = c.int("ZMEDIAITEM")
    b_set = b is not None and b != 0
    if len(found) >= 2:
        undecided(raw, "ZWAMEDIAITEM")
        return "undecided", None
    if len(found) == 1:
        if b_set and b != found[0]["rowid"]:
            undecided(raw, "ZWAMEDIAITEM")
            return "undecided", None
        return "row", found[0]
    if b_set and b in ctx["items_by_pk"]:
        undecided(raw, "ZWAMEDIAITEM")  # the single-source direction never links alone
        return "undecided", None
    return "none", None


def path_refused(path, ctx):
    """4I2-R6: a path that is not a relative path of plain elements, or that is too deep under the container."""
    if path.startswith("/") or "\\" in path:
        return True
    elems = path.split("/")
    if any(e in ("", ".", "..") for e in elems):
        return True
    return len(elems) + 1 + ctx["container_depth"] > 64


def resolve_media(path, ctx):
    """(state, logical): found, missing, other_source or ambiguous at <container>/Message/<path>, the exactly-one same-source
    rule (4I2-R6)."""
    want = "%s/Message/%s" % (ctx["container"], path)
    cands = [e for e in ctx["extras"] if e["logical"] == want]
    if not cands:
        return "missing", None
    same = [e for e in cands if e["device"] == UDID]
    if not same:
        return "other_source", None
    if len(same) > 1:
        return "ambiguous", None
    return "found", same[0]["logical"]


def media_fields(c, raw, typ, body_is_title):
    """The flat media raw of an item row, as stored (plan vocabulary 'Media')."""
    for n in ("ZMEDIAURL", "ZTHUMBNAILLOCALPATH", "ZAUTHORNAME", "ZVCARDNAME"):
        b = c.text(n)
        if b is not None:
            raw[n] = X.cleaned(raw, n, b)
    for n in ("ZFILESIZE", "ZMOVIEDURATION", "ZCLOUDSTATUS"):
        v = c.int(n)
        if v is not None:
            raw[n] = v
    v = c.row.get("ZMEDIAURLDATE")
    if v is not None:
        if X.is_number(v):
            raw["ZMEDIAURLDATE"] = jf(v)
        else:
            c.unknown[c.prefix + "ZMEDIAURLDATE"] = "type-mismatch"
    if not body_is_title:
        b = c.text("ZTITLE")
        if b:
            raw["ZTITLE"] = X.cleaned(raw, "ZTITLE", b)
    if c.row.get("ZMEDIAKEY") is not None:
        raw["media_key_present"] = isinstance(c.row["ZMEDIAKEY"], C.Blob)
    lat, lon = W.num(c, "ZLATITUDE"), W.num(c, "ZLONGITUDE")
    if typ == 5:
        loc = {}
        if lat is not None:
            loc["latitude"] = jf(lat)
        if lon is not None:
            loc["longitude"] = jf(lon)
        if loc:
            raw["location"] = loc
    else:
        if lat is not None:
            raw["ZLATITUDE"] = jf(lat)
        if lon is not None:
            raw["ZLONGITUDE"] = jf(lon)
    md = c.row.get("ZMETADATA")
    if md is not None:
        raw["ZMETADATA"] = {"bytes": len(md), "sha256": hashlib.sha256(bytes(md)).hexdigest()}


def attach(ctx, item, raw, unknown):
    """The attachment of a file-type message with an item row and the counters of how its file was looked up (4I2-R6, PF7)."""
    c = X.Cells(item, raw, unknown, "ZWAMEDIAITEM.")
    mime, size, ref, altered = None, None, None, False
    b = c.text("ZVCARDSTRING")
    if b:
        mime = X.cleaned(raw, "attachments.mime", b)
    v = c.int("ZFILESIZE")
    if v is not None and v > 0:
        size = v
    b = c.text("ZMEDIALOCALPATH")
    if b:
        ref = X.cleaned(raw, "attachments.source_ref", b)
        altered = bool(W.text_of(b).flags)
    if not (mime or size or ref):
        return []
    a = {}
    if mime:
        a["mime"] = mime
    if size:
        a["size"] = size
    if ref:
        a["source_ref"] = ref
    state, logical = None, None
    if not ref:
        raw["media_state"] = "no-local-path"
        state = "none"
    elif altered or "*" in ref:
        raw["media_state"] = "lookup-path-not-expressible"
        state = "not_expressible"
    elif path_refused(ref, ctx):
        raw["media_state"] = "lookup-path-refused"
        state = "none"
    else:
        state, logical = resolve_media(ref, ctx)
    if state == "found":
        a["artifact_id"], a["artifact_sha256"] = "@id:" + logical, "@sha256:" + logical
    raw["attachments_resolved"] = 1 if state == "found" else 0
    raw["attachments_unresolved"] = 1 if state in ("none", "missing") else 0
    for st, key in (("ambiguous", "attachments_ambiguous"), ("other_source", "attachments_other_source"),
                    ("not_expressible", "attachments_lookup_undecided")):
        if state == st:
            raw[key] = 1
    return [a]


def expect_message(r, ctx):
    """The expectation of one ZWAMESSAGE row as the engine reads it back (spec 6.7, 6.11, 7; the 4I2-R7 vocabulary)."""
    raw = {"app": ctx["app"]}
    if ctx["app_basis"]:
        raw["app_basis"] = ctx["app_basis"]
    if ctx["app_state"]:
        raw["app_state"] = ctx["app_state"]
    unknown = {}
    c = X.Cells(r, raw, unknown, "ZWAMESSAGE.")
    me = X.read_code(c, "ZISFROMME")
    direction = {0: "in", 1: "out"}.get(me, "unknown") if me is not None else "unknown"
    typ = X.read_code(c, "ZMESSAGETYPE")
    gev = X.read_code(c, "ZGROUPEVENTTYPE")
    status_code = X.read_code(c, "ZMESSAGESTATUS")
    for n in ("ZMESSAGEERRORSTATUS", "ZSORT", "ZFLAGS", "ZSTARRED", "Z_ENT"):
        v = c.int(n)
        if v is not None:
            raw[n] = v
    v = c.row.get("ZSENTDATE")
    if v is not None:
        if X.is_number(v):
            raw["ZSENTDATE"] = jf(v)  # meaning [X]: raw only, never a Time
        else:
            unknown["ZWAMESSAGE.ZSENTDATE"] = "type-mismatch"
    for n in ("ZFROMJID", "ZTOJID", "ZPUSHNAME"):
        b = c.text(n)
        if b is not None:
            raw[n] = X.cleaned(raw, n, b)
    gm = c.int("ZGROUPMEMBER")
    if gm is not None:
        raw["ZGROUPMEMBER"] = gm
    guid = c.text("ZSTANZAID")
    guid = X.cleaned(raw, "guid", guid) if guid else None

    # kind and labels (spec 7.1; 14 is the revoke rule)
    kind = "unknown"
    if typ in MESSAGE_TYPE:
        raw["label_ZMESSAGETYPE"] = MESSAGE_TYPE[typ]
        kind = "text" if typ == 0 else "system" if typ in (6, 10) else "unknown" if typ == 14 else "media"
    if typ == 6 and gev in GROUP_EVENT:
        raw["label_ZGROUPEVENTTYPE"] = GROUP_EVENT[gev]
    revoke = typ == 14
    status = "unknown"
    if direction == "out" and status_code in MESSAGE_STATUS:
        status = "delivered"  # read is never set from it (spec 7.3)
        raw["label_ZMESSAGESTATUS"] = MESSAGE_STATUS[status_code]

    time_t = X.read_instant(c, "ZMESSAGEDATE")

    # chat and participants (spec 6.11 and 6.4.3)
    chat = chat_of(ctx, raw, unknown, c.int("ZCHATSESSION"))
    if chat["state"]:
        raw["chat_state"] = chat["state"]
    is_group = chat["type"] == 1 or chat["cls"] == "group"
    is_status = chat["type"] == 3 or chat["cls"] == "status"
    participants, unknown_p = [], False
    if direction == "out" and chat["str"]:
        participants = [{"role": "to", "address": chat["str"]}]
    elif direction == "in" and is_status:
        unknown_p = True
    elif direction == "in" and is_group:
        sender = sender_of(ctx, raw, unknown, gm)
        if sender["str"]:
            participants = [{"role": "from", "address": sender["str"]}]
        else:
            unknown_p = True
            raw["sender_state"] = sender["state"]
    elif direction == "in" and chat["str"]:
        participants = [{"role": "from", "address": chat["str"]}]
    else:
        unknown_p = True
    if chat["type"] == 0 and not is_group and chat["str"] and direction in ("in", "out"):
        key = "ZFROMJID" if direction == "in" else "ZTOJID"
        b = c.row.get(key)
        if X.is_text(b) and not W.text_of(b).flags and W.text_of(b).text != chat["str"]:
            raw["address_conflict"] = [key]

    # body and media (spec 6.7, 6.11; PF1: every ZWAMEDIAITEM field is mapped by the parser)
    txt = c.text("ZTEXT")
    text_source = "text" if txt else None
    item, attachments = None, []
    if typ in MEDIA_TYPES or revoke:
        j, item = media_join(ctx, r, c, raw)
        if j == "none" and typ in FILE_TYPES:
            raw["media_state"] = "no-ZWAMEDIAITEM-row"
    body_title = False
    if item is not None and not revoke:
        ic = X.Cells(item, raw, unknown, "ZWAMEDIAITEM.")
        if typ == 4:
            vb = ic.text("ZVCARDSTRING")
            if vb:
                txt, text_source = vb, "part"
                raw["body_column"] = "ZVCARDSTRING"
        elif typ in FILE_TYPES and not txt:
            tb = ic.text("ZTITLE")
            if tb:
                txt, text_source = tb, "part"
                raw["body_column"] = "ZTITLE"
                body_title = True
        media_fields(ic, raw, typ, body_title)
        if typ in FILE_TYPES:
            attachments = attach(ctx, item, raw, unknown)
    if revoke and item is not None:
        b = X.Cells(item, raw, unknown, "ZWAMEDIAITEM.").text("ZVCARDNAME")
        if b is not None:
            raw["ZVCARDNAME"] = X.cleaned(raw, "ZVCARDNAME", b)
    body = None
    if txt:
        cl = W.text_of(txt)
        if cl.text:
            body = cl.text
            if cl.flags:
                C.mark(raw, "body")
    if revoke:
        if txt:
            raw["revoked_row_text"] = X.cleaned(raw, "revoked_row_text", txt)
        body, text_source = None, None
    # the receipt blob (never decoded, spec 8)
    info = c.int("ZMESSAGEINFO")
    if info is not None and info != 0:
        if not ctx["has_infos"]:
            undecided(raw, "ZWAMESSAGEINFO")
        elif info not in ctx["infos"]:
            raw["receipt_info_state"] = "no-ZWAMESSAGEINFO-row"
        else:
            rc = ctx["infos"][info].get("ZRECEIPTINFO")
            if rc is not None:
                raw["receipt_info"] = {"bytes": len(rc), "sha256": hashlib.sha256(bytes(rc)).hexdigest()}
    if unknown:
        raw["unknown_columns"] = unknown

    payload = {"channel": "whatsapp", "direction": direction, "kind": kind, "participants": participants,
               "status": status, "raw": raw}
    if attachments:
        payload["attachments"] = attachments
    if unknown_p:
        payload["participants_unknown"] = True
    thread = {}
    if chat["str"]:
        thread["id"] = chat["str"]
    if chat["title"]:
        thread["title"] = chat["title"]
    if chat["type"] in SESSION_TYPE:
        thread["group"] = chat["type"] == 1
    if thread:
        payload["thread"] = thread
    if guid:
        payload["guid"] = guid
    if body is not None:
        payload["text_source"] = text_source
    if revoke:
        payload["deleted"] = {"source": "ZMESSAGETYPE"}
    counter = participants[0]["address"] if participants else (chat["str"] or "unknown")
    return {"table": "ZWAMESSAGE", "rowid": r["rowid"], "file": "any", "type": "message", "source_path": "", "time": time_t,
            "time_end": None, "times": {}, "deleted": revoke,
            "summary": W.summary_of(direction, counter, "[revoked, no content]" if revoke else body), "body": body,
            "payload": payload}


def context(view, place, domain, extras):
    ctx = {"sessions": {r["rowid"]: r for r in view.get("ZWACHATSESSION", [])},
           "has_members": "ZWAGROUPMEMBER" in view, "members": {r["rowid"]: r for r in view.get("ZWAGROUPMEMBER", [])},
           "has_items": "ZWAMEDIAITEM" in view, "items": view.get("ZWAMEDIAITEM", []),
           "has_infos": "ZWAMESSAGEINFO" in view, "infos": {r["rowid"]: r for r in view.get("ZWAMESSAGEINFO", [])},
           "extras": extras, "place": place, "domain": domain}
    ctx["items_by_pk"] = {r["rowid"]: r for r in ctx["items"]}
    ctx["container"] = "ios:" + domain if place == "backup" else EXT_CONTAINER
    ctx["container_depth"] = 1 if place == "backup" else 5
    ctx["app"], ctx["app_basis"], ctx["app_state"] = app_label(place, domain)
    return ctx


def entities(view):
    return {bytes(r["Z_NAME"]).decode("utf-8", "replace"): r["Z_ENT"] for r in view.get("Z_PRIMARYKEY", [])
            if X.is_text(r.get("Z_NAME"))}


def oracle(files, variant, place, domain, extras=()):
    """The <variant>.expect.json of a variant whose parse succeeds: every ZWAMESSAGE row is one record."""
    view = C.engine_view(files, TABLES)
    ext = [{"logical": "ios:%s/%s" % (domain, rel), "device": dev, "sha": hashlib.sha256(b).hexdigest()}
           for rel, b, dev in extras]
    ctx = context(view, place, domain, ext)
    exp = C.expect_header(FAMILY, variant, "chatstorage.sql")
    for r in view.get("ZWAMESSAGE", []):
        exp["records"].append(expect_message(r, ctx))
    notes = exp["notes"]
    notes["container"] = "backup_domain" if place == "backup" else "app_group"
    notes["app"] = ctx["app"]
    if ctx["app_basis"]:
        notes["app_basis"] = ctx["app_basis"]
    if ctx["app_state"]:
        notes["app_state"] = ctx["app_state"]
    ents = entities(view)
    for name in ("WAMessage", "WAChatSession"):
        notes["entity." + name] = str(ents[name])
    present = {n for t, n, _ in W.master_rows(files) if t == "table"}
    un = [t for t in UNMAPPED if t in present]
    if un:
        notes["unmapped_tables"] = ",".join(un)
    counts = {}
    for rec in exp["records"]:
        for t in rec["payload"]["raw"].get("join_undecided", []):
            counts[t] = counts.get(t, 0) + 1
    for t in OPTIONAL:
        if t not in present:
            notes["join_undecided." + t] = "table absent"  # the reason text is the writer's: the plan names none for an absent table
    for t in sorted(counts):  # plan P7: one counted warning per table, locator sqlite:table=<t>
        exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:table=" + t,
                                "contains": "whatsapp-ios-join-undecided: %s: %d records" % (t, counts[t])})
    exp["extra_files"] = [{"remote_path": I.backup_remote_path(dev, domain, rel),
                           "content_b64": base64.b64encode(b).decode(), "device": "" if dev == UDID else dev,
                           "domain": domain, "relative_path": rel} for rel, b, dev in extras]
    return exp


def unparsed(variant, ddl=""):
    """A variant the parser refuses (no model, another app's database): no records, no notes."""
    return C.expect_header(FAMILY, variant, ddl)


# ----------------------------------------------------------------------------------------- building

def build_db(work, model, tables, drop=(), wal=False, page_size=4096):
    """A ChatStorage database: tables = {table: [row, ...]} inserted in order; the Z_PRIMARYKEY rows of model, the
    Z_METADATA row and Z_MAX last (X.set_z_max). Rows are dicts; a Z_ENT the row does not carry is the model's entity."""
    path = work + "/m.db"
    con = C.connect(path, page_size=page_size)
    con.executescript(X.load_ios_ddl("chatstorage.sql")[0])
    for stmt in drop:
        con.execute(stmt)
    with C.batch(con):
        for table, rows in tables.items():
            if not C.columns(con, table):  # a variant dropped it
                continue
            for row in rows:
                C.insert(con, table, row)
        if C.columns(con, "Z_PRIMARYKEY"):  # the nomodel variant has dropped it
            for table, row in X.primary_key_rows(model):
                C.insert(con, table, row)
            X.set_z_max(con)
        C.insert(con, *X.metadata_row())
    if wal:
        con.execute("pragma journal_mode = wal")
    con.close()
    return path


def tables_of(model, messages, sess=None, with_items=True):
    ent = {name: n for name, n, _ in model}
    e_msg, e_ses = ent.get("WAMessage", 9), ent.get("WAChatSession", 4)
    rows = {"ZWACHATSESSION": sess if sess is not None else sessions(e_ses),
            "ZWAGROUPMEMBER": group_members(ent.get("WAGroupMember", 6)),
            "ZWAGROUPINFO": group_info(ent.get("WAGroupInfo", 5)),
            "ZWAMESSAGE": [dict(m, Z_ENT=e_msg) for m in messages]}
    pks = {m["Z_PK"] for m in messages}
    if with_items:
        rows["ZWAMEDIAITEM"] = [dict(Z_PK=pk, Z_ENT=ent.get("WAMediaItem", 8), Z_OPT=1, ZMESSAGE=mpk, **kw)
                                for pk, mpk, kw in media_items() if mpk in pks]
        rows["ZWAMESSAGEINFO"] = [dict(Z_PK=1, Z_ENT=ent.get("WAMessageInfo", 11), Z_OPT=1, ZMESSAGE=44,
                                       ZRECEIPTINFO=RECEIPT10)] if 44 in pks else []
    return rows


def finish(path):
    files = {"db": C.read(path)}
    if C.os.path.exists(path + "-wal"):
        files["wal"] = C.read(path + "-wal")
    return files


def insert_stmt(table, row):
    return "insert into %s(%s) values (%s)" % (table, ",".join(row), ",".join(C.lit(v) for v in row.values()))


def v_chatstorage(work):
    """CORE-iOS in the main file (messages 1-44), row 45 in a committed WAL frame and row 46 in frames written without a
    commit (a large row that spilled from a tiny page cache before the kill): the engine sees 1-45."""
    path = build_db(work, MODEL, tables_of(MODEL, core_messages(False)), wal=True)
    row45 = dict(msg(45, 1, 0, 0), Z_ENT=9)
    big = dict(msg(46, 1, 0, 0, ZTEXT="@@BIG@@"), Z_ENT=9)
    # 200,000 characters built inside the engine (a command line cannot carry them)
    big_stmt = insert_stmt("ZWAMESSAGE", big).replace("'@@BIG@@'", "replace(hex(zeroblob(100000)), '0', 'x')")
    X.leave_old_frame(path, ["pragma wal_autocheckpoint = 0", "begin", insert_stmt("ZWAMESSAGE", row45),
                             "commit",
                             "pragma cache_size = 5", "pragma cache_spill = 1", "begin", big_stmt])
    files = finish(path)
    assert uncommitted_frames(files["wal"], 4096) > 0, "the WAL holds no frame after its last commit"
    exp = oracle(files, "ios-chatstorage", "backup", DOMAIN_WA, EXTRAS)
    for rec in exp["records"]:
        rec["source_path"] = ""  # a WAL companion: the source path follows the role of the cell
    # the reader's warnings come first (rowrun forwards them when Run ends), then the parser's per-table warnings
    exp["warnings"].insert(0, {"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
    return files, exp


def uncommitted_frames(wal, page_size):
    """Frames after the last commit frame of a WAL (frames whose commit field is zero at the tail)."""
    n = (len(wal) - 32) // (24 + page_size)
    last = 0
    for i in range(n):
        off = 32 + i * (24 + page_size)
        if int.from_bytes(wal[off + 4:off + 8], "big"):
            last = i + 1
    return n - last


def small(work, model=MODEL, drop=()):
    """The files of a small variant: session 1 and messages 1-3, the Z_PRIMARYKEY of model."""
    ent = {name: n for name, n, _ in model}.get("WAChatSession", 4)
    path = build_db(work, model, tables_of(model, small_messages(), sessions(ent)[:1], with_items=False), drop=drop)
    return finish(path)


def v_business(work):
    files = small(work)
    return files, oracle(files, "ios-chatstorage-business", "backup", DOMAIN_SMB)


def v_extract(work):
    files = small(work)
    return files, oracle(files, "ios-chatstorage-extract", "extract", "")


def v_renumbered(work):
    files = small(work, MODEL_RENUMBERED)
    return files, oracle(files, "ios-chatstorage-renumbered", "backup", DOMAIN_WA)


def v_nomodel(work):
    files = small(work, drop=["drop table Z_PRIMARYKEY"])
    return files, unparsed("ios-chatstorage-nomodel", "chatstorage.sql")


def v_othermodel(work):
    files = small(work, MODEL_OTHER)
    return files, unparsed("ios-chatstorage-othermodel", "chatstorage.sql")


def v_otherapp(work):
    path = work + "/o.db"
    con = C.connect(path)
    con.execute("create table ZMESSAGE(Z_PK integer primary key, ZTEXT varchar)")
    with C.batch(con):
        con.execute("insert into ZMESSAGE values (1, 'hello')")
    con.close()
    return {"db": C.read(path)}, unparsed("ios-chatstorage-otherapp")


def v_nooptional(work):
    """The tables ZWAGROUPMEMBER, ZWAMESSAGEINFO and ZWAMEDIAITEM are absent (plan 4I2 PF3: optional, so their absence is
    stated, never 'no row'). Messages 1-3 (text), 4 (an image with a media row in the full store), 10 (an incoming group
    message that names a member), 44 (a message that names a receipt row)."""
    msgs = [m for m in core_messages(False) if m["Z_PK"] in (1, 2, 3, 4, 10, 44)]
    path = build_db(work, MODEL, tables_of(MODEL, msgs, sessions(4)[:2], with_items=False),
                       drop=["drop table ZWAGROUPMEMBER", "drop table ZWAMESSAGEINFO", "drop table ZWAMEDIAITEM"])
    files = finish(path)
    return files, oracle(files, "ios-chatstorage-nooptional", "backup", DOMAIN_WA)


# ----------------------------------------------------------------------------------------- typed recovery (plan 4I2 PF2)

def bulk(first, n, session=1):
    """n plain text messages with ids first.. (the rows the deleted variant writes and deletes)."""
    return [msg(i, session, i % 2, 0, ZTEXT="bulk message number %d with some text to fill the page" % i,
                ZMESSAGEDATE=D + 5000 + i, **({"ZGROUPMEMBER": 1, "ZFROMJID": GRP} if session == 2 and i % 2 == 0 else {}))
            for i in range(first, first + n)]


def new_scenario(variant, con):
    scen = SCENARIOS[variant] = C.Scenario(CLAIMED, alias=ALIAS)
    scen.snap(con, "ZWAMESSAGE", "0")
    return scen


def v_deleted(work):
    """Messages 1-3 live; 60 bulk messages (ids 100-159, half of them in the group) and messages 90-91 in the main file;
    one committed WAL transaction inserts messages 92-93 and the next deletes 90-93 and 100-159. The main file still holds
    the deleted rows (versions that are history under the WAL) and the first WAL frame holds 92-93 (the old frame). Typed
    recovery claims ZWAMESSAGE rows; the joins read the live side tables. Deleted rows are found only through the WAL here:
    the history oracle (sqlite_oracle.build_history) reads freelist leaves only of a store with no WAL or journal."""
    msgs = [dict(m, Z_ENT=9) for m in small_messages() + bulk(90, 2) + bulk(100, 30, 1) + bulk(130, 30, 2)]
    path = build_db(work, MODEL, tables_of(MODEL, msgs, sessions(4)[:2], with_items=False), wal=True)
    con = C.connect(path)
    scen = new_scenario("ios-chatstorage-deleted", con)
    scen.write("ZWAMESSAGE", scen.snap(con, "ZWAMESSAGE"))
    scen.want("ZWAMESSAGE", scen.snap(con, "ZWAMESSAGE", "Z_PK between 100 and 159 or Z_PK in (90, 91)"))
    con.close()
    # the rows the first WAL transaction inserts, as the engine would store them
    old = [dict(m, Z_ENT=9) for m in bulk(92, 2)]
    shadow = work + "/shadow.db"
    sc = C.connect(shadow)
    sc.executescript(X.load_ios_ddl("chatstorage.sql")[0])
    for m in old:
        C.insert(sc, "ZWAMESSAGE", m)
    old_rows = scen.snap(sc, "ZWAMESSAGE")
    sc.close()
    scen.write("ZWAMESSAGE", old_rows)
    scen.want("ZWAMESSAGE", old_rows)
    X.leave_old_frame(path, ["pragma wal_autocheckpoint = 0"] + [insert_stmt("ZWAMESSAGE", m) for m in old] +
                      ["delete from ZWAMESSAGE where Z_PK in (90, 91, 92, 93) or Z_PK between 100 and 159"])
    files = finish(path)
    exp = oracle(files, "ios-chatstorage-deleted", "backup", DOMAIN_WA)
    for rec in exp["records"]:
        rec["source_path"] = ""
    return files, exp


def v_deleted_freelist(work):
    """A store with no WAL (a checkpointed backup copy): 1024-byte pages; messages 1-3 live and messages 100-159 (an image 150
    whose media row stays, a revoke 151 whose media row stays, and text rows) deleted with secure_delete off, so whole leaf
    pages land on the freelist and the history oracle reads them (basis fit, attributed to ZWAMESSAGE, 4G)."""
    msgs = small_messages() + bulk(100, 50) + [msg(150, 1, 0, 1, ZMESSAGEDATE=D + 150), msg(151, 1, 0, 14, ZTEXT="gone", ZMESSAGEDATE=D + 151)] + bulk(152, 8)
    path = build_db(work, MODEL, tables_of(MODEL, msgs, sessions(4)[:1], with_items=True), page_size=1024)
    con = C.connect(path)
    scen = new_scenario("ios-chatstorage-deleted-freelist", con)
    scen.write("ZWAMESSAGE", scen.snap(con, "ZWAMESSAGE"))
    scen.want("ZWAMESSAGE", scen.snap(con, "ZWAMESSAGE", "Z_PK between 100 and 159"), label="ZWAMESSAGE")
    X.delete_rows(con, "ZWAMESSAGE", range(100, 160))
    con.close()
    files = finish(path)
    assert "wal" not in files
    exp = oracle(files, "ios-chatstorage-deleted-freelist", "backup", DOMAIN_WA)
    return files, exp


def recover_env(files):
    """The live view of the finished files; the place is the consumer backup domain (PF5: a recovered row at a domain
    place states its domain label)."""
    return context(C.engine_view(files, TABLES), "backup", DOMAIN_WA, [])


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (ruling 4G-R14)."""
    def one(table, d, env, file):
        rec = expect_message(d, env)
        rec["source_path"], rec["file"] = "", file  # a recovered record: the source path follows the role of the file
        return rec

    return C.recover_oracle(variant, files, SCENARIOS[variant], recover_env, one)


VARIANTS = {
    "ios-chatstorage": v_chatstorage,
    "ios-chatstorage-business": v_business,
    "ios-chatstorage-extract": v_extract,
    "ios-chatstorage-renumbered": v_renumbered,
    "ios-chatstorage-nomodel": v_nomodel,
    "ios-chatstorage-othermodel": v_othermodel,
    "ios-chatstorage-otherapp": v_otherapp,
    "ios-chatstorage-nooptional": v_nooptional,
    "ios-chatstorage-deleted": v_deleted,
    "ios-chatstorage-deleted-freelist": v_deleted_freelist,
}

"""iOS Messages (sms.db) fixtures (plan 4E, Task 7): ios10-seconds, ios14-attributed, ios17-nanos and ios17-wal.

The layouts are written from memory (ruling 4E-R13): no public source defines sms.db, so these fixtures prove that
the parser reads the remembered layout and nothing about a real database. The oracle restates the "Mapping contract
ios.sms" of the plan and rulings 4E-R14 to R20, R31 and R41 in Python from what this generator wrote (the engine's
read-back of its own files, and, for the typedstream and plist blobs, the generator's own record of what each blob
holds); it shares no code with the Go parser and takes no value from Go output. The attributedBody bytes come from
the typedstream ENCODER of ios_common.py, written from the layout in the doc comment of the Go decoder, not from
the Go test builder.

Rules stated here (the contract, not the code):
  direction: is_from_me 1 out, 0 in, else unknown. channel: service exactly iMessage / SMS / RCS, else unknown.
  status: in received; out: error != 0 failed, else is_delivered = 1 delivered, else is_sent = 1 sent, else
  unknown. read: only for incoming messages (is_read 0/1). kind: associated_message_type in [2000, 3999] reaction;
  else an item_type or associated_message_type cell that is NULL or not an INTEGER makes it unknown (4E-R41);
  else item_type != 0 system; else a body was read text; else an attachment media; else unknown.
  body: text (CleanText, 256 KiB) else the text of a decodable attributedBody; text and a decodable attributedBody
  that differ keep text and set raw.attributed_body_differs; an undecodable attributedBody sets
  raw.attributed_body_status. time: date through CocoaAuto (INTEGER at or above 1e11 is nanoseconds); every time
  column is kept in raw with its unit and, unless valid, its status; only valid values become Times.
  participants / thread (4E-R20), attachments (4E-R19), recently deleted (chat_recoverable_message_join), the
  summary and payload plists (4E-R18) as in the plan.
"""
import base64
import datetime
import math
import os
import plistlib
import re

import ios_common as I
import telephony_common as C

FAMILY = "ios_sms"
SCHEMAS = os.path.join(C.HERE, "schemas", "ios")
DOMAIN, REL = "HomeDomain", "Library/SMS/sms.db"
UDID, UDID2 = "UDID1", "UDID2"
REMOTE_DB = I.backup_remote_path(UDID, DOMAIN, REL)
REMOTE_WAL = I.backup_remote_path(UDID, DOMAIN, REL + "-wal")
MAX_TEXT = 256 << 10
MAX_LIST = 1000
T0C = 704628000  # Cocoa seconds of 2023-05-01T10:00:00Z (checked in the self-test below)
NS = 10**9
GUID_P = "E8F3C1A2-4B5D-4E6F-8A9B-0C1D2E3F4A5B"
GUID_BP = "0A1B2C3D-4E5F-4A6B-9C7D-8E9F0A1B2C3D"
TABLES = ["message", "handle", "chat", "chat_message_join", "chat_handle_join", "attachment",
          "message_attachment_join", "chat_recoverable_message_join"]
# The mapped columns of message, in the order of the DDL snapshot: (name, required). Everything else stays unmapped.
MAPPED = [("ROWID", True), ("guid", True), ("text", False), ("handle_id", True), ("subject", False),
          ("attributedBody", False), ("service", False), ("error", False), ("date", True), ("date_read", False),
          ("date_delivered", False), ("is_delivered", False), ("is_from_me", True), ("is_read", False),
          ("is_sent", False), ("cache_roomnames", False), ("date_played", False), ("item_type", False),
          ("other_handle", False), ("group_title", False), ("group_action_type", False),
          ("associated_message_guid", False), ("associated_message_type", False), ("balloon_bundle_id", False),
          ("payload_data", False), ("expressive_send_style_id", False), ("message_summary_info", False),
          ("date_retracted", False), ("date_edited", False)]
RAW_INT = ["handle_id", "is_from_me", "is_read", "is_sent", "is_delivered", "error", "item_type",
           "group_action_type", "associated_message_type", "other_handle"]
RAW_TEXT = ["group_title", "associated_message_guid", "balloon_bundle_id", "cache_roomnames",
            "expressive_send_style_id"]
TIME_KINDS = [("read", "date_read"), ("delivered", "date_delivered"), ("played", "date_played"),
              ("edited", "date_edited"), ("retracted", "date_retracted")]
GUID_RE = re.compile(r"[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}")
PREFIXES = ("~/", "/var/mobile/", "/private/var/mobile/")

# What the generator wrote into each blob it makes, keyed by the blob's bytes: the oracle states the facts from
# this record, never from decoding the blob again.
AB_TRUTH = {}      # attributedBody bytes -> the text bytes it holds, or None when the stream is damaged
SUMMARY_TRUTH = {}  # message_summary_info bytes -> (sorted keys, edit count or None)
PAYLOAD_TRUTH = {}  # payload_data bytes -> root class, or None when the bytes are damaged


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def is_real(v):
    return isinstance(v, float)


def is_text(v):
    return isinstance(v, bytes) and not isinstance(v, C.Blob)


def is_blob(v):
    return isinstance(v, C.Blob)


def jf(x):
    """A float as Go's encoding/json prints it: an integral value without a fraction."""
    if isinstance(x, float) and x.is_integer() and abs(x) < 1e15:
        return int(x)
    return x


def cs(y, mo, d):
    return int((datetime.datetime(y, mo, d, tzinfo=I.UTC) - datetime.datetime(2001, 1, 1, tzinfo=I.UTC)).total_seconds())


def ns(i, extra=0):
    """The Cocoa nanosecond date of the scenario's row i: 2023-05-01T10:00:00Z + i hours (+ extra ns)."""
    return (T0C + 3600 * i) * NS + extra


def sec(i):
    return T0C + 3600 * i


# ---- blobs, with the generator's record of what they hold ------------------------------------------------

def ab_ok(text):
    t = text.encode("utf-8") if isinstance(text, str) else text
    b = I.typedstream(t)
    AB_TRUTH[b] = t
    return b


def ab_cut(text, drop=6):
    """A typedstream cut inside its text: the declared length is longer than what is left (truncated)."""
    b = I.typedstream(text)[:-drop]
    assert len(text) > drop
    AB_TRUTH[b] = None
    return b


def summary_info(obj):
    b = I.plist_binary(obj)
    ec = obj.get("ec")
    count = None
    if isinstance(ec, dict) and all(isinstance(v, list) for v in ec.values()):
        count = sum(len(v) for v in ec.values())
    SUMMARY_TRUTH[b] = (sorted(obj), count)
    return b


def link_archive(cls="LPLinkMetadata"):
    """An NSKeyedArchiver archive whose root object is an instance of cls."""
    objects = ["$null", {"$class": plistlib.UID(2), "URL": plistlib.UID(3), "title": plistlib.UID(4)},
               {"$classname": cls, "$classes": [cls, "NSObject"]}, "https://example.test/x", "Example"]
    b = I.plist_binary({"$archiver": "NSKeyedArchiver", "$version": 100000, "$top": {"root": plistlib.UID(1)},
                        "$objects": objects})
    PAYLOAD_TRUTH[b] = cls
    return b


def corrupt_payload():
    b = b"bplist00\xd1\x01\x02\xff"  # a header and a cut object table
    PAYLOAD_TRUTH[b] = None
    return b


# ---- the oracle -------------------------------------------------------------------------------------------

def field(raw, name, v, unknown=None):
    """A TEXT cell through CleanText: the cleaned text when non-empty (None otherwise); an altered text is listed
    in raw.text_flags. A cell of another class is recorded in unknown_columns."""
    if v is None:
        return None
    if not is_text(v):
        if unknown is not None:
            unknown[name] = "type-mismatch"
        return None
    c = C.clean(v, MAX_TEXT)
    assert not c.truncated, "no field of the scenario reaches its cap"
    if c.flags:
        C.mark(raw, name)
    return c.text or None


def body_fields(p, c):
    if c.flags:
        p["body_flags"] = c.flags
    if c.truncated:
        p["body_truncated"], p["body_total_bytes"] = True, c.total
    if c.b64:
        p["body_raw_b64"], p["body_raw_len"] = c.b64, c.rawlen
    if c.sha:
        p["body_raw_sha256"], p["body_raw_len"] = c.sha, c.rawlen


def summary(channel, direction, counterparty, text):
    """One line; only ASCII text of at most 80 characters with a counterparty is stated (common.Summarize)."""
    if not (channel + direction + (counterparty or "") + (text or "")).isascii() or not counterparty:
        return None
    one_line = re.sub("[" + chr(0) + "-" + chr(32) + chr(127) + "]+", " ", text or "").strip(" ")
    if len(one_line) > 80:
        return None
    return ("%s %s %s: %s" % (channel, direction, counterparty, one_line)).rstrip(" ")


def target_guid(v):
    """4E-R17: the GUID of p:<decimal>/<GUID>, bp:<GUID> or a bare <GUID>; None for anything else."""
    if GUID_RE.fullmatch(v):
        return v
    if v.startswith("bp:") and GUID_RE.fullmatch(v[3:]):
        return v[3:]
    m = re.fullmatch(r"p:[0-9]+/(.*)", v, re.DOTALL)
    if m and GUID_RE.fullmatch(m.group(1)):
        return m.group(1)
    return None


def attachment_rel(fn):
    """4E-R19: the relative path of ~/, /var/mobile/ or /private/var/mobile/ names, or None (unresolvable)."""
    for pre in PREFIXES:
        if fn.startswith(pre):
            rel = fn[len(pre):]
            break
    else:
        return None
    if "*" in rel or not I.canonical(rel) or len(rel.split("/")) > 64:
        return None
    return rel


class Env:
    pass


def make_env(view, extras):
    env = Env()
    env.handles = {r["rowid"]: r for r in view.get("handle", [])}
    env.chats = {r["rowid"]: r for r in view.get("chat", [])}
    env.chat_of = {}
    for r in view.get("chat_message_join", []):
        env.chat_of.setdefault(r["message_id"], []).append(r["chat_id"])
    env.members = {}
    for r in view.get("chat_handle_join", []):
        env.members.setdefault(r["chat_id"], []).append(r["handle_id"])
    env.att = {r["rowid"]: r for r in view.get("attachment", [])}
    env.att_of = {}
    for r in view.get("message_attachment_join", []):
        env.att_of.setdefault(r["message_id"], []).append(r["attachment_id"])
    env.recov = {}
    for r in view.get("chat_recoverable_message_join", []):
        env.recov.setdefault(r["message_id"], r)
    env.extras = extras  # [(device, logical path)]
    env.differs = 0
    env.warnings = []
    return env


def instant(row, col, raw, unknown):
    """A time column through CocoaAuto: kept in raw with its unit and status; returns the RFC 3339 text of a valid
    value, else None."""
    v = row.get(col)
    if v is None:
        return None
    if not (is_int(v) or is_real(v)):
        unknown[col] = "type-mismatch"
        return None
    if is_real(v) and not math.isfinite(v):
        raw.update(I.real_cells(col, v))
        return None
    iso, st, unit = I.cocoa_auto(v)
    raw[col] = jf(v)
    if unit:
        raw[col + "_unit"] = {"s": "seconds", "ns": "nanoseconds"}[unit]
    if st != "valid":
        raw[col + "_status"] = st
    return iso


def addr_of(env, hid):
    h = env.handles.get(hid)
    if h is None:
        return None
    c = C.clean(h["id"], MAX_TEXT)
    assert not c.flags and 0 < len(c.text.encode()) <= 1024, "no handle of the scenario needs the address flags"
    return c.text


def part(addr, role):
    return {"role": role, "address": addr, "kind": C.addr_kind(addr)}


def expect_message(r, env, file, source_path):
    p, raw, unknown = {}, {}, {}
    rid = r["rowid"]
    # direction, channel, service
    ifm = r.get("is_from_me")
    direction = {1: "out", 0: "in"}.get(ifm, "unknown") if is_int(ifm) else "unknown"
    svc = r.get("service")
    channel = {b"iMessage": "imessage", b"SMS": "sms", b"RCS": "rcs"}.get(svc, "unknown") if is_text(svc) else "unknown"
    service = field(raw, "service", svc, unknown)
    if service:
        p["service"] = raw["service"] = service
    for col in RAW_INT:
        v = r.get(col)
        if is_int(v):
            raw[col] = v
        elif v is None and col in ("item_type", "associated_message_type") and col in r:
            unknown[col] = "null"
        elif v is not None:
            unknown[col] = "type-mismatch"
    for col in RAW_TEXT:
        t = field(raw, col, r.get(col), unknown)
        if t:
            raw[col] = t
    # status and read
    if direction == "in":
        status = "received"
    elif direction == "out":
        err, dl, snt = r.get("error"), r.get("is_delivered"), r.get("is_sent")
        status = "failed" if is_int(err) and err != 0 else "delivered" if dl == 1 and is_int(dl) else \
            "sent" if snt == 1 and is_int(snt) else "unknown"
    else:
        status = "unknown"
    rd = r.get("is_read")
    if direction == "in" and is_int(rd) and rd in (0, 1):
        p["read"] = rd == 1
    # the body (4E-R14)
    text, ab = r.get("text"), r.get("attributedBody")
    body_c, text_source = None, None
    truth = AB_TRUTH[bytes(ab)] if is_blob(ab) else None
    if is_text(text):
        body_c, text_source = C.clean(text, MAX_TEXT), "text"
        if is_blob(ab):
            if truth is None:
                raw["attributed_body_status"] = "undecodable"
            elif truth != text:
                raw["attributed_body_differs"] = True
                env.differs += 1
    elif is_blob(ab):
        if truth is None:
            raw["attributed_body_status"] = "undecodable"
        else:
            body_c, text_source = C.clean(truth, MAX_TEXT), "attributedBody"
    body = None
    if body_c is not None:
        body = body_c.text
        p["text_source"] = text_source
        body_fields(p, body_c)
    sub = field(raw, "subject", r.get("subject"), unknown)
    if sub:
        p["subject"] = sub
    guid = field(raw, "guid", r.get("guid"), unknown)
    if guid:
        p["guid"] = guid
    ag = raw.get("associated_message_guid")
    if ag is not None and target_guid(ag):
        p["target_guid"] = target_guid(ag)
    # attachments (4E-R19)
    atts, resolved, unresolved, ambiguous, other = [], 0, 0, 0, 0
    for aid in env.att_of.get(rid, [])[:MAX_LIST]:
        arow = env.att[aid]
        a = {}
        for col, key in (("transfer_name", "name"), ("mime_type", "mime")):
            t = field(raw, "attachments." + key, arow.get(col), unknown)
            if t:
                a[key] = t
        tb = arow.get("total_bytes")
        if is_int(tb) and tb >= 0:
            a["size"] = tb
        fn = field(raw, "attachments.source_ref", arow.get("filename"), unknown)
        if fn:
            a["source_ref"] = fn
            rel = attachment_rel(fn)
            logical = "ios:MediaDomain/" + rel if rel else None
            own = [x for x in env.extras if x[1] == logical and x[0] == UDID] if logical else []
            elsewhere = [x for x in env.extras if x[1] == logical and x[0] != UDID] if logical else []
            if len(own) == 1:
                a["artifact_id"], a["artifact_sha256"] = "@id:" + logical, "@sha256:" + logical
                resolved += 1
            elif len(own) > 1:
                ambiguous += 1
            elif elsewhere:
                other += 1
            else:
                unresolved += 1
        else:
            unresolved += 1
        atts.append(a)
    if atts:
        p["attachments"] = atts
        raw["attachments_resolved"], raw["attachments_unresolved"] = resolved, unresolved
        if ambiguous:
            raw["attachments_ambiguous"] = ambiguous
        if other:
            raw["attachments_other_source"] = other
    # kind (4E-R17, R41)
    def cell(col):
        if col not in r:
            return "missing", None
        v = r[col]
        return ("int", v) if is_int(v) else ("undecided", None)
    (ak, av), (ik, iv) = cell("associated_message_type"), cell("item_type")
    if ak == "int" and 2000 <= av <= 3999:
        kind = "reaction"
    elif "undecided" in (ak, ik):
        kind = "unknown"
    elif ik == "int" and iv != 0:
        kind = "system"
    elif body is not None:
        kind = "text"
    elif atts:
        kind = "media"
    else:
        kind = "unknown"
    # participants and thread (4E-R20)
    hid = r.get("handle_id")
    haddr = None
    if is_int(hid) and hid > 0:
        haddr = addr_of(env, hid)
        if haddr is None:
            raw["handle_state"] = "no-handle-row"
    chats = env.chat_of.get(rid, [])
    parts = []
    if direction == "in":
        parts = [part(haddr, "from")] if haddr else []
    elif direction == "out":
        if len(chats) == 1:
            parts = [part(addr_of(env, h), "to") for h in sorted(env.members.get(chats[0], [])[:MAX_LIST])]
        if not parts and haddr:
            parts = [part(haddr, "to")]
    elif haddr:
        parts = [part(haddr, "member")]
    thread = None
    if not chats:
        raw["thread_state"] = "no-chat-join"
    elif len(chats) == 1:
        cid = chats[0]
        thread = {"id": str(cid)}
        chat = env.chats.get(cid)
        if chat is None:
            raw["thread_state"] = "no-chat-row"
        else:
            title = field(raw, "thread.title", chat.get("display_name"), unknown)
            if title:
                thread["title"] = title
            style = chat.get("style")
            if is_int(style) and style in (43, 45):
                thread["group"] = style == 43
            members = [addr_of(env, h) for h in sorted(env.members.get(cid, []))[:MAX_LIST]]
            if members:
                thread["members"] = members
    else:
        raw["thread_state"] = "several-chats"
        raw["chat_ids"] = sorted(chats)
    # times (cocoa instants)
    time_iso = instant(r, "date", raw, unknown)
    times = {}
    for kname, col in TIME_KINDS:
        iso = instant(r, col, raw, unknown)
        if iso:
            times[kname] = iso
    # recently deleted
    deleted = False
    rv = env.recov.get(rid)
    if rv is not None:
        deleted = True
        p["deleted"] = {"source": "chat_recoverable_message_join"}
        if is_int(rv.get("chat_id")):
            raw["recoverable_chat_id"] = rv["chat_id"]
        iso = instant(rv, "delete_date", raw, unknown)
        if iso:
            times["deleted"] = iso
    # message_summary_info and payload_data (4E-R18)
    msi = r.get("message_summary_info")
    if is_blob(msi):
        info = SUMMARY_TRUTH.get(bytes(msi))
        assert info is not None, "the scenario writes no damaged summary plist"
        raw["summary_info"] = {"keys": info[0][:64]}
        if info[1] is not None:
            raw["edit_count"] = info[1]
    pd = r.get("payload_data")
    if is_blob(pd):
        cls = PAYLOAD_TRUTH[bytes(pd)]
        if cls is None:
            raw["payload_data_status"] = "undecodable"
            env.warnings.append({"table": "message", "rowid": rid, "contains": "payload_data"})
        else:
            raw["payload_data"] = {"root_class": cls}
    if unknown:
        raw["unknown_columns"] = unknown
    p.update({"channel": channel, "direction": direction, "kind": kind, "status": status, "participants": parts})
    if not parts:
        p["participants_unknown"] = True
    if thread:
        p["thread"] = thread
    if raw:
        p["raw"] = raw
    cp = parts[0]["address"] if parts else None
    return {"table": "message", "rowid": rid, "file": file, "type": "message", "source_path": source_path,
            "time": time_iso, "time_end": None, "times": times, "deleted": deleted,
            "summary": summary(channel, direction, cp, body), "body": body, "payload": p}


def columns_notes(cols, notes):
    names = [n.lower() for n, _ in MAPPED]
    lowered = [c.lower() for c in cols]
    opt = [n for n, req in MAPPED if not req and n.lower() in lowered]
    v = "columns:%d" % len(cols)
    if opt:
        v += ";optional:" + ",".join(opt)
    notes["schema.message"] = v
    un = [c for c in cols if c.lower() not in names]
    if un:
        notes["unmapped_columns.message"] = ",".join(un)


def oracle(files, ddl_name, variant, extras=(), wal_rows=()):
    view = C.engine_view(files, TABLES)
    env = make_env(view, [(d, lg) for d, lg, _ in extras])
    exp = C.expect_header(FAMILY, variant, ddl_name)
    has_wal = "wal" in files
    for r in view["message"]:
        in_wal = r["rowid"] in wal_rows
        file = "wal" if in_wal else "any"
        path = REMOTE_WAL if in_wal else ("" if has_wal else REMOTE_DB)  # T48; T51 for a database-only variant
        exp["records"].append(expect_message(r, env, file, path))
    exp["warnings"] = env.warnings
    cols = [k for k in view["message"][0] if k != "rowid"]
    columns_notes(cols, exp["notes"])
    if env.differs:
        exp["notes"]["attributed_body_differs"] = str(env.differs)
    exp["extra_files"] = [{"remote_path": I.backup_remote_path(d, "MediaDomain", lg[len("ios:MediaDomain/"):]),
                           "content_b64": base64.b64encode(data).decode(), "device": "" if d == UDID else d,
                           "domain": "MediaDomain", "relative_path": lg[len("ios:MediaDomain/"):]}
                          for d, lg, data in sorted(extras, key=lambda x: (x[0], x[1]))]
    return exp


# ---- scenario rows ----------------------------------------------------------------------------------------

def guid_of(i):
    return {1: GUID_P, 3: GUID_BP}.get(i, "5E9C1A44-7B2D-4C3E-9F10-%012d" % i)


def msg(i, **kw):
    """An incoming iMessage from handle 1 at t0 + i hours, read; kw override or add columns."""
    r = {"ROWID": i, "guid": guid_of(i), "text": "message %d" % i, "handle_id": 1, "service": "iMessage",
         "date": ns(i), "is_from_me": 0, "is_read": 1}
    r.update(kw)
    return r


def out(i, **kw):
    """An outgoing sent iMessage to handle 1."""
    r = msg(i, is_from_me=1, is_sent=1)
    r.update(kw)
    return r


def content(tag, n=300):
    return (tag.encode() + b" synthetic attachment bytes ") * (n // 30)


def attachments_17():
    c1, c2, c3, c4 = content("jpeg1"), content("heic2", 500), content("other3", 220), content("png4", 160)
    rows = [
        {"ROWID": 1, "guid": "ATT-0001", "filename": "~/Library/SMS/Attachments/ab/12/IMG_0001.jpeg", "uti": "public.jpeg",
         "mime_type": "image/jpeg", "transfer_name": "IMG_0001.jpeg", "total_bytes": len(c1)},
        {"ROWID": 2, "guid": "ATT-0002", "filename": "~/Library/SMS/Attachments/bb/22/missing.jpeg", "uti": "public.jpeg",
         "mime_type": "image/jpeg", "transfer_name": "missing.jpeg", "total_bytes": 51200},
        {"ROWID": 3, "guid": "ATT-0003", "filename": "/var/mobile/Library/SMS/Attachments/cd/34/IMG_0002.heic",
         "uti": "public.heic", "mime_type": "image/heic", "transfer_name": "IMG_0002.heic", "total_bytes": len(c2)},
        {"ROWID": 4, "guid": "ATT-0004", "filename": "~/Library/SMS/Attachments/ef/56/other.jpeg", "uti": "public.jpeg",
         "mime_type": "image/jpeg", "transfer_name": "other.jpeg", "total_bytes": len(c3)},
        {"ROWID": 5, "guid": "ATT-0005", "filename": "~/Library/SMS/Attachments/aa/11/a*b.png", "uti": "public.png",
         "mime_type": "image/png", "transfer_name": "a*b.png", "total_bytes": len(c4)},
    ]
    pre = "Library/SMS/Attachments/"
    extras = [(UDID, "ios:MediaDomain/" + pre + "ab/12/IMG_0001.jpeg", c1),
              (UDID, "ios:MediaDomain/" + pre + "cd/34/IMG_0002.heic", c2),
              (UDID2, "ios:MediaDomain/" + pre + "ef/56/other.jpeg", c3),
              (UDID, "ios:MediaDomain/" + pre + "aa/11/a*b.png", c4)]
    return rows, extras


HANDLES = [(1, "+15551230001", "iMessage"), (2, "+15551230001", "SMS"), (3, "alice@example.test", "iMessage"),
           (4, "+15551230004", "iMessage"), (5, "+15551230005", "iMessage"), (6, "55555", "SMS"),
           (7, "BANK", "SMS"), (8, "+15551230008", "RCS")]
CHATS = [(1, "iMessage;-;+15551230001", 45, None, [1]), (2, "SMS;-;+15551230001", 45, None, [2]),
         (3, "iMessage;+;chat100000000000000001", 43, "Weekend plans", [1, 3, 4]),
         (4, "iMessage;-;alice@example.test", 45, None, [3]), (5, "SMS;-;55555", 45, None, [6]),
         (6, "RCS;-;+15551230008", 45, None, [8]), (7, "iMessage;-;+15551230005", None, None, [5])]


def rows_17():
    emoji = "Great job \U0001F44D\U0001F3FD family \U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466 flag \U0001F1E8\U0001F1E6 done"
    msi = summary_info({"amc": 1, "ec": {"0": [{"d": 704000000.5, "t": b"first edit"}, {"d": 704000100.5, "t": b"second edit"}]},
                        "ep": [0], "otr": {"0": {"lo": 0, "le": 11}}})
    return [
        msg(1, handle_id=2, service="SMS", text="hello from sms", date_read=ns(1, 5 * NS)),
        out(2, handle_id=0, text="see you at 5", is_delivered=1, date=ns(2, 123456789), date_delivered=ns(2, 2 * NS),
            date_read=ns(2, 30 * NS), cache_roomnames="chat100000000000000001"),
        msg(3, text=None, attributedBody=ab_ok(emoji)),
        msg(4, text="same text", attributedBody=ab_ok("same text")),
        msg(5, text="text column wins", attributedBody=ab_ok("attributed differs")),
        msg(6, text=None, attributedBody=ab_ok(b"bad \xff\xfe bytes")),
        msg(7, text=None, attributedBody=ab_cut("this text is cut")),
        msg(8, text="plain with broken blob", attributedBody=ab_cut("another cut text here")),
        msg(9, text=None),
        msg(10, text=None),
        msg(11, text="Loved \u201chello from sms\u201d", associated_message_type=2001, associated_message_guid="p:0/" + GUID_P),
        out(12, text="Removed a like from \u201cphoto\u201d", associated_message_type=3001, associated_message_guid="bp:" + GUID_BP),
        msg(13, text="Laughed at \u201cx\u201d", associated_message_type=2002, associated_message_guid="x:abc"),
        msg(14, handle_id=3, text=None, item_type=2, group_title="Weekend plans", group_action_type=1),
        msg(15, text="no timestamp", date=0),
        msg(16, text="seconds date", date=700000000),
        msg(17, text="nanosecond precision", date=ns(17, 987654321)),
        msg(18, text="year 2100", date=cs(2100, 6, 1)),
        out(19, text="all the times", is_delivered=1, date_delivered=ns(19, 2 * NS), date_read=ns(19, 60 * NS),
            date_played=ns(19, 90 * NS), date_edited=ns(19, 600 * NS), date_retracted=ns(19, 3600 * NS)),
        msg(20, text="not read yet", is_read=0, date_read=0),
        out(21, text="edited text", is_delivered=1, date_edited=ns(21, 300 * NS), message_summary_info=msi),
        msg(22, text="to be deleted"),
        out(23, handle_id=3, text="in two chats"),
        out(24, text="in no chat"),
        msg(25, handle_id=0, text="unknown sender"),
        msg(26, handle_id=99, text="dangling handle"),
        msg(27, handle_id=4, text="photo via var path"),
        msg(28, is_from_me=2, text="odd direction"),
        out(29, error=22, text="failed send"),
        msg(30, handle_id=8, service="RCS", text="hello over rcs"),
        msg(31, service=None, text="no service"),
        msg(32, text=C.Raw(b"nul\x00in text")),
        msg(33, text="a" * (300 * 1024)),
        msg(34, text="\u05e9\u05dc\u05d5\u05dd \u05e2\u05d5\u05dc\u05dd \u0645\u0631\u062d\u0628\u0627"),
        msg(35, text=None),
        msg(36, text=None),
        msg(37, text="null item type", item_type=None),
        msg(38, text="text associated type", associated_message_type="abc"),
        msg(39, text="https://example.test/x", balloon_bundle_id="com.apple.messages.URLBalloonProvider", payload_data=link_archive()),
        msg(40, text="https://example.test/y", balloon_bundle_id="com.apple.messages.URLBalloonProvider", payload_data=corrupt_payload()),
        msg(41, text="see you soon", date=ns(41), guid="5E9C1A44-7B2D-4C3E-9F10-00000000A041"),
        msg(42, text="see you soon", date=ns(41), guid="5E9C1A44-7B2D-4C3E-9F10-00000000A042"),
        out(43, handle_id=5, text="sent only", subject="Re: plans", expressive_send_style_id="com.apple.messages.effect.CKHappyBirthdayEffect"),
        out(44, handle_id=6, service="SMS", text="queued", is_sent=0),
        msg(45, text="with a bad subject", subject=C.Raw(b"bad \xff subject")),
    ]


CHAT_OF_17 = {1: [2], 2: [3], 3: [1], 4: [1], 5: [1], 6: [1], 7: [1], 8: [1], 9: [1], 10: [1], 11: [1], 12: [1], 13: [1],
              14: [3], 15: [1], 16: [1], 17: [1], 18: [1], 19: [1], 20: [1], 21: [1], 23: [1, 4], 25: [1], 26: [1],
              27: [3], 28: [1], 29: [1], 30: [6], 31: [1], 32: [1], 33: [1], 34: [1], 35: [1], 36: [1], 37: [1],
              38: [1], 39: [1], 40: [1], 41: [1], 42: [1], 43: [7], 44: [5], 45: [1]}
ATT_OF_17 = {10: [1, 2], 27: [3], 35: [4], 36: [5]}


# ---- building ----------------------------------------------------------------------------------------------

def lit(v):
    """A SQL literal for the statements a killed child process runs."""
    if isinstance(v, (bytes, bytearray)) and not isinstance(v, C.Raw):
        return "x'%s'" % bytes(v).hex()
    if isinstance(v, float):
        assert math.isfinite(v)
        return repr(v)
    return C.lit(v)


def create(work, ddl_name, edit=None):
    path = work + "/s.db"
    script = C.load_ddl(ddl_name, SCHEMAS)[0]
    if edit:
        script = edit(script)
    con = C.connect(path)
    con.executescript(script)
    return path, con


def populate_dims(con, handles, chats):
    for hid, addr, svc in handles:
        C.insert(con, "handle", {"ROWID": hid, "id": addr, "country": "us", "service": svc, "uncanonicalized_id": addr})
    for cid, guid, style, name, members in chats:
        C.insert(con, "chat", {"ROWID": cid, "guid": guid, "style": style, "display_name": name, "chat_identifier": guid.split(";")[-1],
                               "service_name": guid.split(";")[0]})
        for h in members:
            C.insert(con, "chat_handle_join", {"chat_id": cid, "handle_id": h})


def populate_messages(con, rows, chat_of, att_of=None, recov=()):
    byid = {r["ROWID"]: r for r in rows}
    for r in rows:
        C.insert(con, "message", r)
    for mid, chats in sorted(chat_of.items()):
        for cid in chats:
            C.insert(con, "chat_message_join", {"chat_id": cid, "message_id": mid, "message_date": byid[mid]["date"]})
    for mid, aids in sorted((att_of or {}).items()):
        for aid in aids:
            C.insert(con, "message_attachment_join", {"message_id": mid, "attachment_id": aid})
    for cid, mid, when in recov:
        C.insert(con, "chat_recoverable_message_join", {"chat_id": cid, "message_id": mid, "delete_date": when})


def variant_nanos(work):
    path, con = create(work, "sms-ios17.sql")
    att_rows, extras = attachments_17()
    with C.batch(con):
        populate_dims(con, HANDLES, CHATS)
        for a in att_rows:
            C.insert(con, "attachment", a)
        populate_messages(con, rows_17(), CHAT_OF_17, ATT_OF_17, [(1, 22, ns(22, 86400 * NS))])
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, "sms-ios17.sql", "ios17-nanos", extras)


def variant_wal(work):
    path, con = create(work, "sms-ios17.sql")
    handles, chats = HANDLES[:2], [CHATS[0], CHATS[1]]
    base = [msg(1, text="first"), out(2, text="second"), msg(3, handle_id=2, service="SMS", text="third"),
            msg(4, text="fourth"), msg(5, text="to be edited"), out(6, text="sixth"), msg(7, text=None, attributedBody=ab_ok("seventh, attributed")),
            msg(8, handle_id=2, service="SMS", text="eighth")]
    chat_of = {1: [1], 2: [1], 3: [2], 4: [1], 5: [1], 6: [1], 7: [1], 8: [2]}
    with C.batch(con):
        populate_dims(con, handles, chats)
        populate_messages(con, base, chat_of)
    con.execute("pragma journal_mode = wal")
    con.close()
    new = [msg(9, text="only in the wal, a"), out(10, text="only in the wal, b"), msg(11, handle_id=2, service="SMS", text="only in the wal, c")]
    msi = summary_info({"ec": {"0": [{"d": 704000000.5, "t": b"the original"}]}, "ep": [0]})
    stmts = ["pragma wal_autocheckpoint = 0"]
    for r in new:
        cols = list(r)
        stmts.append("insert into message(%s) values (%s)" % (",".join(cols), ",".join(lit(r[c]) for c in cols)))
        stmts.append("insert into chat_message_join(chat_id, message_id, message_date) values (%d, %d, %d)" % (2 if r["handle_id"] == 2 else 1, r["ROWID"], r["date"]))
    stmts.append("update message set text = 'edited in the wal', date_edited = %d, message_summary_info = %s where ROWID = 5" % (ns(5, 300 * NS), lit(msi)))
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    return files, oracle(files, "sms-ios17.sql", "ios17-wal", wal_rows={5, 9, 10, 11})


def variant_ios14(work):
    path, con = create(work, "sms-ios14.sql")
    handles, chats = HANDLES[:3], [CHATS[0], CHATS[1], (3, "iMessage;+;chat100000000000000002", 43, "Book club", [1, 2, 3])]
    rows = [
        msg(1, text="plain text"),
        out(2, text="plain reply", is_delivered=1, date_delivered=ns(2, NS)),
        msg(3, text=None, attributedBody=ab_ok("attributed one")),
        msg(4, text=None, attributedBody=ab_ok("attributed \U0001F600 emoji")),
        msg(5, text=None, attributedBody=ab_ok("line one\nline two")),
        msg(6, text=None, attributedBody=ab_ok("\u05e9\u05dc\u05d5\u05dd \u0645\u0631\u062d\u0628\u0627")),
        msg(7, text=None, attributedBody=ab_ok("\u201csmart\u201d quotes")),
        msg(8, text=None, attributedBody=ab_ok("caf\u00e9 cr\u00e8me")),
        msg(9, text="both columns", attributedBody=ab_ok("both columns")),
        msg(10, text="Loved \u201cplain text\u201d", associated_message_type=2000, associated_message_guid="p:1/" + GUID_P),
        msg(11, text="https://example.test/z", balloon_bundle_id="com.apple.messages.URLBalloonProvider", payload_data=link_archive()),
        out(12, handle_id=0, text="to the book club", is_delivered=1),
    ]
    chat_of = {i: [1] for i in range(1, 12)}
    chat_of[12] = [3]
    with C.batch(con):
        populate_dims(con, handles, chats)
        populate_messages(con, rows, chat_of)
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, "sms-ios14.sql", "ios14-attributed", ())


def variant_ios10(work):
    def without_last_column(script):
        out_lines = []
        for line in script.splitlines():
            if line.startswith("CREATE TABLE message ("):
                changed = line.replace(", ck_sync_state INTEGER DEFAULT 0)", ")")
                assert changed != line
                line = changed
            out_lines.append(line)
        return "\n".join(out_lines)

    path, con = create(work, "sms-ios10.sql", without_last_column)
    handles, chats = HANDLES[:3], [CHATS[0], CHATS[1], (3, "iMessage;+;chat100000000000000003", 43, "Old group", [1, 2, 3])]

    def s(i, **kw):
        r = msg(i, date=sec(i))
        r.update(kw)
        return r

    early = [s(1, text="first, written before the upgrade", service="SMS", handle_id=2),
             s(2, is_from_me=1, is_sent=1, text="second, short record", service="SMS", handle_id=2),
             s(3, text="third, short record")]
    late = [s(4, text="fourth, after the new column", date_read=sec(4) + 60),
            s(5, is_from_me=1, is_sent=1, is_delivered=1, text="fifth", date_delivered=sec(5) + 2),
            s(6, text="sixth, a fractional date", date=float(sec(6)) + 0.5),
            s(7, text="seventh, utf-8 \u00e9\u00e8"),
            s(8, is_from_me=1, is_sent=1, text="eighth", handle_id=2, service="SMS"),
            s(9, is_from_me=1, handle_id=0, text="ninth, to the group", is_sent=1, is_delivered=1),
            s(10, is_from_me=1, error=3, text="tenth, failed", handle_id=2, service="SMS")]
    chat_of = {1: [2], 2: [2], 3: [1], 4: [1], 5: [1], 6: [1], 7: [1], 8: [2], 9: [3], 10: [2]}
    with C.batch(con):
        populate_dims(con, handles, chats)
        populate_messages(con, early, {k: v for k, v in chat_of.items() if k <= 3})
    con.execute("alter table message add column ck_sync_state INTEGER DEFAULT 0")  # the records above are now short
    with C.batch(con):
        populate_messages(con, late, {k: v for k, v in chat_of.items() if k > 3})
    con.close()
    files = {"db": C.read(path)}
    return files, oracle(files, "sms-ios10.sql", "ios10-seconds", ())


VARIANTS = {"ios10-seconds": variant_ios10, "ios14-attributed": variant_ios14, "ios17-nanos": variant_nanos, "ios17-wal": variant_wal}


def _selfcheck():
    assert cs(2023, 5, 1) + 36000 == T0C
    assert I.cocoa_iso(T0C, "s") == "2023-05-01T10:00:00Z"
    assert I.cocoa_iso(ns(17, 987654321), "ns") == "2023-05-02T03:00:00.987654321Z"
    assert REMOTE_DB == "UDID1/3d/3d0d7e5fb2ce288813306e4d4636395e047a3d28"
    assert target_guid("p:0/" + GUID_P) == GUID_P and target_guid("bp:" + GUID_BP) == GUID_BP and target_guid(GUID_P) == GUID_P
    for bad in ("x:abc", GUID_P[:-1], "p:x/" + GUID_P, "p:/" + GUID_P, "bp:" + GUID_P[1:], "q:0/" + GUID_P):
        assert target_guid(bad) is None, bad
    assert attachment_rel("~/Library/SMS/Attachments/a/b.jpg") == "Library/SMS/Attachments/a/b.jpg"
    assert attachment_rel("/private/var/mobile/Library/x") == "Library/x"
    for bad in ("Library/x", "~/a*b", "~/a//b", "~/", "~/a/../b", "~/a\\b", "/tmp/x", "~/" + "/".join(["a"] * 65)):
        assert attachment_rel(bad) is None, bad
    assert len(ab_cut("this text is cut")) < len(ab_ok("this text is cut"))
    print("ios_sms self-checks OK")


if __name__ == "__main__":
    _selfcheck()

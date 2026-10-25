"""Android telephony provider (mmssms.db) fixtures: variants, scenario rows, oracle."""
import base64
import re
import sqlite3

import telephony_common as C

FAMILY = "mmssms"
CLAIMED = ("sms", "pdu")  # Meta.Claims of android.mmssms
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle)
REMOTE = "/data/user_de/0/com.android.providers.telephony/databases/mmssms.db"
T0 = 1682935200  # 2023-05-01T10:00:00Z
MS = 1000
PART_DIR = "/data/user/0/com.android.providers.telephony/app_parts/"
PRESENT = {PART_DIR + "PART_1.jpg": b"\xff\xd8\xff\xe0 synthetic jpeg bytes"}
MAX_LIST = 1000
MAX_BODY = 256 << 10

# the Go mapping's ColumnSpec order: (name, required)
SMS_MAPPED = [("address", True), ("date", True), ("type", True), ("body", True), ("thread_id", False), ("date_sent", False),
              ("read", False), ("status", False), ("subject", False), ("protocol", False), ("service_center", False),
              ("locked", False), ("sub_id", False), ("error_code", False), ("creator", False), ("seen", False)]
PDU_MAPPED = [("date", True), ("msg_box", True), ("thread_id", False), ("date_sent", False), ("read", False), ("m_id", False),
              ("sub", False), ("m_type", False), ("m_size", False), ("ct_t", False), ("locked", False), ("seen", False),
              ("sub_id", False), ("text_only", False)]
SMS_RAW_INT = ["type", "status", "protocol", "read", "seen", "locked", "error_code", "sub_id"]
PDU_RAW_INT = ["msg_box", "m_type", "read", "seen", "locked", "text_only", "m_size", "sub_id"]


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def sms_row(i, **kw):
    r = {"_id": i, "thread_id": 1, "address": "+15551234567", "date": (T0 + 60 * i) * MS, "type": 1, "read": 1, "body": "hello %d" % i}
    r.update(kw)
    return r


def sms_baseline():
    return [
        sms_row(1, date=T0 * MS, date_sent=(T0 - 2) * MS, protocol=0, status=-1, body="hello", service_center="+15550000001"),
        sms_row(2, type=2, status=0, body="hi there", date_sent=0),
        sms_row(3, type=3, body="draft text"),
        sms_row(4, read=0, body=None),
        sms_row(5, date=0, body="no time"),
        sms_row(6, date=1700000000, body="seconds in a milliseconds column"),
        sms_row(7, body=C.Raw(b"nul\x00and bad \xff\xfe bytes")),
        sms_row(8, thread_id=2, address="+15557654321", body="\u05e9\u05dc\u05d5\u05dd \U0001F600 \u0645\u0631\u062d\u0628\u0627"),
        sms_row(9, thread_id=4, address="55555", body="code 1234"),
        sms_row(10, thread_id=4, address="BANK", body="alphanumeric sender"),
        sms_row(11, type=2, body="long " * 1200),
        sms_row(12, type=0, body="type zero"),
        sms_row(13, thread_id=3, address="alice@example.test", type=5, body="failed send"),
        sms_row(14, thread_id=99, address="+15559990000", body="orphan thread"),
        sms_row(15, address=None, body="no address"),
        sms_row(16, body="a" * (300 * 1024)),
        sms_row(17, thread_id=5, body="thread whose recipient row is missing"),
        sms_row(18, address="1" * 1500, body="address too long"),
        sms_row(19, type=2, address="", body="empty address"),
        sms_row(20, date=4102444800000, body="year 2100"),
        sms_row(21, thread_id=2, subject="a subject", address="+15557654321", body="with subject", type=2, read=0),
        sms_row(22, subject=C.Raw(b"bad \xff subject"), body="subject flagged"),
    ]


def dims_baseline():
    return {
        "threads": [{"_id": 1, "recipient_ids": "1"}, {"_id": 2, "recipient_ids": "2 3"}, {"_id": 3, "recipient_ids": "3"},
                    {"_id": 4, "recipient_ids": "4"}, {"_id": 5, "recipient_ids": "7"}],
        "canonical_addresses": [{"_id": 1, "address": "+15551234567"}, {"_id": 2, "address": "+15557654321"},
                                {"_id": 3, "address": "alice@example.test"}, {"_id": 4, "address": "55555"}],
    }


def mms_baseline():
    pdu, addr, part = [], [], []
    pdu.append({"_id": 1, "thread_id": 1, "date": T0 + 300, "date_sent": T0 + 298, "msg_box": 1, "read": 1, "m_id": "mid-1", "sub": "pics", "m_type": 132, "m_size": 4096, "ct_t": "application/vnd.wap.multipart.related"})
    addr += [{"_id": 1, "msg_id": 1, "address": "+15551234567", "type": 137}, {"_id": 2, "msg_id": 1, "address": "insert-address-token", "type": 151}]
    part += [{"_id": 1, "mid": 1, "seq": -1, "ct": "application/smil", "name": "smil.xml", "text": "<smil/>"},
             {"_id": 2, "mid": 1, "seq": 0, "ct": "text/plain", "name": "text_0.txt", "text": "see photo"},
             {"_id": 3, "mid": 1, "seq": 1, "ct": "image/jpeg", "name": "img.jpg", "_data": PART_DIR + "PART_1.jpg"},
             {"_id": 4, "mid": 1, "seq": 2, "ct": "text/plain", "text": "second text"}]
    pdu.append({"_id": 2, "thread_id": 2, "date": T0 + 400, "date_sent": 0, "msg_box": 2, "read": 1, "m_type": 128})
    addr += [{"_id": 3, "msg_id": 2, "address": "insert-address-token", "type": 137}, {"_id": 4, "msg_id": 2, "address": "+15557654321", "type": 151},
             {"_id": 5, "msg_id": 2, "address": "alice@example.test", "type": 151}, {"_id": 6, "msg_id": 2, "address": "+15551234567", "type": 130},
             {"_id": 7, "msg_id": 2, "address": "+15550001111", "type": 129}, {"_id": 8, "msg_id": 2, "address": "odd@example.test", "type": 99}]
    part += [{"_id": 5, "mid": 2, "seq": 0, "ct": "text/plain", "text": "group message"}]
    pdu.append({"_id": 3, "thread_id": 1, "date": T0 + 500, "msg_box": 1, "m_type": 130})  # notification-ind
    addr.append({"_id": 9, "msg_id": 3, "address": "+15551234567", "type": 137})
    pdu.append({"_id": 4, "thread_id": 1, "date": T0 + 600, "msg_box": 1, "m_type": 132})  # no addr rows, no parts
    pdu.append({"_id": 5, "thread_id": 1, "date": T0 + 700, "msg_box": 3, "m_type": 128})
    addr.append({"_id": 10, "msg_id": 5, "address": "+15551234567", "type": 151})
    part += [{"_id": 6, "mid": 5, "seq": 0, "ct": "image/png", "name": "a*b.png", "_data": PART_DIR + "a*b.png"},
             {"_id": 7, "mid": 5, "seq": 1, "ct": None, "cl": "unknown-type.bin", "_data": PART_DIR + "missing.bin"},
             {"_id": 8, "mid": 5, "seq": 2, "ct": "text/plain", "text": None}]
    pdu.append({"_id": 6, "thread_id": 1, "date": 0, "msg_box": 1, "m_type": 132})
    addr.append({"_id": 11, "msg_id": 6, "address": "+15551234567", "type": 137})
    part.append({"_id": 9, "mid": 6, "seq": 0, "ct": "text/plain", "text": "zero date"})
    return pdu, addr, part


# ---------------------------------------------------------------- oracle


def text_of(raw, field, v):
    """common.CleanText for a free-text field without a flag slot of its own: an altered text is listed in raw.text_flags."""
    if v is None:
        return None
    c = C.clean(v, MAX_BODY)
    if c.flags:
        C.mark(raw, field)
    return c


def raw_ints(r, names, raw, unknown):
    for n in names:
        v = r.get(n)
        if is_int(v):
            raw[n] = v
        elif v is not None:
            unknown[n] = "type-mismatch"


def thread_for(env, r, raw):
    tid = r.get("thread_id")
    if not is_int(tid):
        return None
    raw["thread_id"] = tid
    th = {"id": str(tid)}
    if not env["has_threads"]:
        return th
    trow = env["threads"].get(tid)
    if trow is None:
        raw["thread_state"] = "no-threads-row"
        return th
    rids = trow.get("recipient_ids")
    if rids is None:
        raw["thread_state"] = "recipients-unknown"
        return th
    toks = rids.split()
    if toks:
        th["group"] = len(toks) > 1
    if len(toks) > MAX_LIST:
        raw["thread_recipients_total"], raw["thread_recipients_truncated"] = len(toks), True
        toks = toks[:MAX_LIST]
    members, missing, bad, skipped = [], [], 0, 0
    for tk in toks:
        try:
            cid = int(tk)
        except ValueError:
            bad += 1
            continue
        c = env["canon"].get(cid)
        if c is None or c.get("address") is None:
            missing.append(cid)
            continue
        cl = C.clean(c["address"], MAX_BODY)
        if cl.flags:
            C.mark(raw, "thread.members")
        if len(cl.text.encode()) > 1024:
            skipped += 1
            continue
        members.append(cl.text)
    if members:
        th["members"] = members
    if missing:
        raw["thread_recipients_missing"] = missing
    if bad:
        raw["thread_recipients_unparsed"] = bad
    if skipped:
        raw["thread_members_skipped"] = skipped
    return th


def participant(addr, role, raw, prefix="address"):
    """Returns the participant dict or None (and the reason in raw)."""
    if addr is None or addr == b"":
        return None
    c = C.clean(addr, MAX_BODY)
    if len(c.text.encode()) > 1024:
        raw[prefix + "_state"] = "too-long"
        return None
    if c.flags:
        raw[prefix + "_flags"] = c.flags
        if c.b64:
            raw[prefix + "_raw_b64"] = c.b64
    return {"role": role, "address": c.text, "kind": C.addr_kind(c.text)}


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
    """The record summary is one line: a run of whitespace or control characters in the text becomes one space,
    and none stays at either end (spec C-summary; common.Summarize). Only ASCII text of at most 80 characters is stated."""
    if not (channel + direction + (counterparty or "") + (text or "")).isascii() or counterparty is None:
        return None
    one_line = re.sub("[" + chr(0) + "-" + chr(32) + chr(127) + "]+", " ", text or "").strip(" ")
    if len(one_line) > 80:
        return None
    return ("%s %s %s: %s" % (channel, direction, counterparty, one_line)).rstrip(" ")


def source_path(file, db_only=False):
    """Ruling T48: the remote path of the file that holds the cell. A row that may lie in any file states
    nothing when the variant has a WAL or journal (the comparer derives the path from the role the parser's
    locator reports); a db-only variant has one possible file, so ruling T51 states the db path for it."""
    if file == "any":
        return REMOTE if db_only else ""
    return REMOTE + {"db": "", "wal": "-wal", "journal": "-journal"}[file]


def expect_sms(r, env, file="any", db_only=False):
    raw, unknown = {}, {}
    typ = r.get("type")
    direction, status = {1: ("in", "received"), 2: ("out", "sent"), 3: ("out", "draft"), 4: ("out", "queued"),
                         5: ("out", "failed"), 6: ("out", "queued")}.get(typ if is_int(typ) else None, ("unknown", "unknown"))
    p = {"channel": "sms", "kind": "text", "direction": direction, "status": status}
    role = {"in": "from", "out": "to"}.get(direction, "member")
    addr = r.get("address")
    part = participant(addr, role, raw)
    p["participants"] = [part] if part else []
    if not part:
        p["participants_unknown"] = True
    th = thread_for(env, r, raw)
    if th:
        p["thread"] = th
    # times
    times, t = {}, None
    date, dsent = r.get("date"), r.get("date_sent")
    for col, v in (("date", date), ("date_sent", dsent)):
        if is_int(v):
            st = C.classify_sms_date(v)
            raw[col] = v
            if st != "valid":
                raw[col + "_status"] = st
        elif v is not None:
            unknown[col] = "type-mismatch"
    if is_int(date) and C.classify_sms_date(date) == "valid":
        t = C.iso(date, "ms")
        if direction == "in":
            times["received"] = t
        elif direction == "out":
            times["sent"] = t
    if direction == "in" and is_int(dsent) and C.classify_sms_date(dsent) == "valid":
        times["sent"] = C.iso(dsent, "ms")
    # body, subject
    body, bt = r.get("body"), None
    if body is not None:
        c = C.clean(body, MAX_BODY)
        bt = c.text
        p["text_source"] = "text"
        body_fields(p, c)
    sub = text_of(raw, "subject", r.get("subject"))
    if sub and sub.text:
        p["subject"] = sub.text
    rd = r.get("read")
    if rd in (0, 1) and is_int(rd):
        p["read"] = rd == 1
    sid = r.get("sub_id")
    if is_int(sid) and sid >= 0:
        p["subscription"] = sid
    raw_ints(r, SMS_RAW_INT, raw, unknown)
    for col in ("service_center", "creator"):
        if r.get(col) is not None:
            raw[col] = text_of(raw, col, r[col]).text
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    cp = part["address"] if part else None
    return {"table": "sms", "rowid": r["rowid"], "file": file, "type": "message", "source_path": source_path(file, db_only), "time": t, "time_end": None, "times": times, "deleted": False,
            "summary": summary("sms", direction, cp, bt), "body": bt, "payload": p}


def ct_lower(v):
    return None if v is None else v.decode("utf-8", "replace").lower()


def expect_pdu(r, env, file="any", db_only=False):
    raw, unknown = {}, {}
    box = r.get("msg_box")
    direction, status = {1: ("in", "received"), 2: ("out", "sent"), 3: ("out", "draft"), 4: ("out", "queued")}.get(box if is_int(box) else None, ("unknown", "unknown"))
    p = {"channel": "mms", "direction": direction, "status": status}
    mid = r["rowid"]
    mtype = r.get("m_type") if is_int(r.get("m_type")) else None
    # participants
    parts = []
    arows = env["addr"].get(mid, [])
    raw["addr_rows"] = len(arows)
    for a in arows:
        role = {137: "from", 151: "to", 129: "bcc", 130: "cc"}.get(a.get("type") if is_int(a.get("type")) else None, "member")
        pt = participant(a.get("address"), role, raw, "addr")
        if pt:
            parts.append(pt)
    p["participants"] = parts[:MAX_LIST]
    if len(parts) > MAX_LIST:
        raw["participants_total"], raw["participants_truncated"] = len(parts), True
    if not parts:
        p["participants_unknown"] = True
    # parts
    prow = sorted(env["part"].get(mid, []), key=lambda x: (x.get("seq") if is_int(x.get("seq")) else 0))
    raw["parts"] = len(prow)
    texts, atts, smil, notext = [], [], 0, 0
    for q in prow:
        ct = ct_lower(q.get("ct"))
        if ct == "text/plain":
            if q.get("text") is None:
                notext += 1
            else:
                texts.append(q["text"])
        elif ct == "application/smil":
            smil += 1
        else:
            atts.append(q)
    if smil:
        raw["smil_parts"] = smil
    if notext:
        raw["text_parts_without_text"] = notext
    bt = None
    if texts:
        joined = b"\n".join(texts)
        cap = MAX_BODY * 2
        buf = joined[:cap]
        if len(joined) > len(buf):
            while buf and (buf[-1] & 0xC0) == 0x80:
                buf = buf[:-1]
            if buf and buf[-1] >= 0xC0:
                buf = buf[:-1]
        body_c = C.clean(buf, MAX_BODY)
        if len(joined) > len(buf):
            body_c.truncated, body_c.total = True, len(joined)
            if "truncated" not in body_c.flags:
                body_c.flags.append("truncated")
        bt = body_c.text
        p["text_source"] = "part"
        body_fields(p, body_c)
    out_atts, resolved, unresolved = [], 0, 0
    for q in atts[:MAX_LIST]:
        a = {}
        nm = text_of(raw, "attachments.name", q.get("name"))
        if nm and nm.text:
            a["name"] = nm.text
        else:
            cl = text_of(raw, "attachments.name", q.get("cl"))
            if cl and cl.text:
                a["name"] = cl.text
        mime = text_of(raw, "attachments.mime", q.get("ct"))
        if mime and mime.text:
            a["mime"] = mime.text
        d = text_of(raw, "attachments.source_ref", q.get("_data"))
        if d and d.text:
            a["source_ref"] = d.text
            path = d.text
            if path in PRESENT and path.startswith("/") and "*" not in path:
                a["artifact_id"], a["artifact_sha256"], a["size"] = "@id:" + path, "@sha256:" + path, "@size:" + path
                resolved += 1
            else:
                unresolved += 1
        out_atts.append(a)
    if out_atts:
        p["attachments"] = out_atts
        raw["attachments_resolved"], raw["attachments_unresolved"] = resolved, unresolved
    if len(atts) > MAX_LIST:
        raw["attachments_total"], raw["attachments_truncated"] = len(atts), True
    # kind
    if mtype is not None and mtype not in (128, 132):
        kind = "system"
    elif atts:
        kind = "media"
    elif texts:
        kind = "text"
    else:
        kind = "unknown"
    p["kind"] = kind
    th = thread_for(env, r, raw)
    if th:
        p["thread"] = th
    # times (seconds)
    times, t = {}, None
    date, dsent = r.get("date"), r.get("date_sent")
    for col, v in (("date", date), ("date_sent", dsent)):
        if is_int(v):
            st = C.classify(v, "s")
            raw[col] = v
            if st != "valid":
                raw[col + "_status"] = st
        elif v is not None:
            unknown[col] = "type-mismatch"
    if is_int(date) and C.classify(date, "s") == "valid":
        t = C.iso(date, "s")
        if direction == "in":
            times["received"] = t
        elif direction == "out":
            times["sent"] = t
    if direction == "in" and is_int(dsent) and C.classify(dsent, "s") == "valid":
        times["sent"] = C.iso(dsent, "s")
    sub = text_of(raw, "subject", r.get("sub"))
    if sub and sub.text:
        p["subject"] = sub.text
    mi = text_of(raw, "guid", r.get("m_id"))
    if mi and mi.text:
        p["guid"] = mi.text
    rd = r.get("read")
    if is_int(rd) and rd in (0, 1):
        p["read"] = rd == 1
    sid = r.get("sub_id")
    if is_int(sid) and sid >= 0:
        p["subscription"] = sid
    raw_ints(r, PDU_RAW_INT, raw, unknown)
    if r.get("ct_t") is not None:
        raw["ct_t"] = text_of(raw, "ct_t", r["ct_t"]).text
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    cp = None
    for pt in parts:
        if pt["role"] == ("from" if direction == "in" else "to"):
            cp = pt["address"]
            break
    return {"table": "pdu", "rowid": mid, "file": file, "type": "message", "source_path": source_path(file, db_only), "time": t, "time_end": None, "times": times, "deleted": False,
            "summary": summary("mms", direction, cp, bt), "body": bt, "payload": p}


def group(rows, key):
    out = {}
    for r in rows:
        out.setdefault(r.get(key), []).append(r)
    return out


def make_env(files):
    """The LIVE view of the joined tables (a recovered row's joins read it too, spec 12.1)."""
    view = C.engine_view(files, ["sms", "threads", "canonical_addresses", "pdu", "addr", "part"])
    return view, {
        "has_threads": "threads" in view, "threads": {r["rowid"]: r for r in view.get("threads", [])},
        "canon": {r["rowid"]: r for r in view.get("canonical_addresses", [])},
        "addr": group(view.get("addr", []), "msg_id"), "part": group(view.get("part", []), "mid"),
    }


def oracle(files, ddl_name, variant, wal_touched=()):
    view, env = make_env(files)
    exp = C.expect_header(FAMILY, variant, ddl_name)
    db_only = set(files) == {"db"}
    for r in view.get("sms", []):
        exp["records"].append(expect_sms(r, env, "wal" if ("sms", r["rowid"]) in wal_touched else "any", db_only))
    for r in view.get("pdu", []):
        exp["records"].append(expect_pdu(r, env, "any", db_only))
    # only variants that carry the baseline MMS rows ship the attachment file
    if any(p.get("payload", {}).get("attachments") for p in exp["records"]):
        exp["extra_files"] = [{"remote_path": k, "content_b64": base64.b64encode(v).decode()} for k, v in sorted(PRESENT.items())]
    return exp


# ---------------------------------------------------------------- variants


def populate(con, ddl, rows_sms, dims, mms):
    with C.batch(con):
        for t, rows in dims.items():
            for r in rows:
                C.insert(con, t, r)
        for r in rows_sms:
            C.insert(con, "sms", r)
        if mms:
            pdu, addr, part = mms
            for t, rows in (("pdu", pdu), ("addr", addr), ("part", part)):
                for r in rows:
                    C.insert(con, t, r)


def build_plain(work, ddl_name, **conn):
    path = work + "/m.db"
    ph = C.load_ddl(ddl_name)
    con = C.connect(path, **conn)
    con.executescript(ph[0])
    return path, con, ph


def variant_baseline(work):
    path, con, ph = build_plain(work, "mmssms-aosp-baseline.sql")
    populate(con, ph, sms_baseline(), dims_baseline(), mms_baseline())
    return finish_notes(con, path, "mmssms-aosp-baseline.sql", "aosp-baseline")


def finish_notes(con, path, ddl_name, variant, files=None, wal_touched=()):
    sms_cols, pdu_cols = C.columns(con, "sms"), C.columns(con, "pdu")
    con.close()
    files = files or {"db": C.read(path)}
    exp = oracle(files, ddl_name, variant, wal_touched)
    C.note_columns(sms_cols, SMS_MAPPED, "sms", exp["notes"])
    C.note_columns(pdu_cols, PDU_MAPPED, "pdu", exp["notes"])
    return files, exp


def variant_subid_alter(work):
    path, con, ph = build_plain(work, "mmssms-aosp-subid-alter.sql")
    old = [r for r in sms_baseline() if r["_id"] <= 6]
    populate(con, ph, old, dims_baseline(), mms_baseline())
    con.executescript(ph[1])
    new = [sms_row(30, body="after the upgrade", sub_id=2, creator="com.example.sender"),
           sms_row(31, body="after, no subscription", sub_id=-1)]
    with C.batch(con):
        for r in new:
            C.insert(con, "sms", r)
        C.insert(con, "pdu", {"_id": 7, "thread_id": 1, "date": T0 + 800, "msg_box": 1, "m_type": 132, "sub_id": 1, "text_only": 1})
        C.insert(con, "addr", {"_id": 12, "msg_id": 7, "address": "+15551234567", "type": 137})
        C.insert(con, "part", {"_id": 10, "mid": 7, "seq": 0, "ct": "text/plain", "text": "after the upgrade mms"})
    return finish_notes(con, path, "mmssms-aosp-subid-alter.sql", "aosp-subid-alter")


def variant_oem(work):
    path, con, ph = build_plain(work, "mmssms-oem-extended.sql")
    rows = [dict(r, sim_slot=1, is_favorite=0, msg_hash="h%d" % r["_id"]) for r in sms_baseline() if r["_id"] <= 4]
    populate(con, ph, rows, dims_baseline(), None)
    return finish_notes(con, path, "mmssms-oem-extended.sql", "oem-extended")


def variant_utf16(work):
    path, con, ph = build_plain(work, "mmssms-aosp-baseline.sql", page_size=1024, encoding="UTF-16le")
    rows = [r for r in sms_baseline() if r["_id"] in (1, 2, 4, 8, 13)]
    populate(con, ph, rows, dims_baseline(), mms_baseline())
    return finish_notes(con, path, "mmssms-aosp-baseline.sql", "utf16")


def variant_freelist(work):
    path, con, ph = build_plain(work, "mmssms-aosp-baseline.sql", page_size=512, auto_vacuum=2)
    rows = [sms_row(i, body=("message %d " % i) * 8) for i in range(1, 301)]
    populate(con, ph, rows, dims_baseline(), None)
    scen = SCENARIOS["av-freelist-512"] = C.Scenario(CLAIMED)
    scen.write("sms", scen.snap(con, "sms"))
    # freed leaf pages are never owned by a table: the oracle expects basis fit, attributed to sms
    scen.want("sms", scen.snap(con, "sms", "_id between 100 and 250"), label="sms")
    con.execute("delete from sms where _id between 100 and 250")
    if con.execute("pragma freelist_count").fetchone()[0] < 10:
        raise SystemExit("the freelist fixture has no freelist pages")
    return finish_notes(con, path, "mmssms-aosp-baseline.sql", "av-freelist-512")


def schema_conn(ph):
    """An in-memory database with the variant's phase-0 schema, only to list its columns."""
    con = sqlite3.connect(":memory:", isolation_level=None)
    con.executescript(ph[0])
    return con


def variant_wal(work):
    path, con, ph = build_plain(work, "mmssms-aosp-baseline.sql")
    populate(con, ph, [r for r in sms_baseline() if r["_id"] <= 6], dims_baseline(), None)
    scen = SCENARIOS["wal-uncheckpointed"] = C.Scenario(CLAIMED)
    before = scen.snap(con, "sms")
    scen.write("sms", before)  # the versions the WAL then supersedes; the WAL's own rows are live
    scen.want("sms", [r for r in before if r[0] == 2])  # row 2 as the database file still holds it
    con.execute("pragma journal_mode = wal")
    con.close()
    new = [sms_row(7, body="only in the wal, a"), sms_row(8, type=2, body="only in the wal, b"), sms_row(9, body="only in the wal, c")]
    stmts = ["pragma wal_autocheckpoint = 0"]
    for r in new:
        cols = list(r)
        stmts.append("insert into sms(%s) values (%s)" % (",".join(cols), ",".join(C.lit(r[c]) for c in cols)))
    stmts.append("update sms set body = 'edited in the wal' where _id = 2")
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    return finish_notes(schema_conn(ph), path, "mmssms-aosp-baseline.sql", "wal-uncheckpointed", files, {("sms", 2), ("sms", 7), ("sms", 8), ("sms", 9)})


def variant_journal(work):
    path, con, ph = build_plain(work, "mmssms-aosp-baseline.sql")
    rows = [sms_row(i, body=("original %d " % i) + "x" * 100) for i in range(1, 1501)]
    populate(con, ph, rows, dims_baseline(), None)
    scen = SCENARIOS["hot-journal"] = C.Scenario(CLAIMED)
    scen.write("sms", scen.snap(con, "sms"))
    interrupted = []
    for rowid, d in scen.snap(con, "sms", "_id % 7 = 0"):
        interrupted.append((rowid, dict(d, body="interrupted-%d" % rowid + d["body"][19:])))  # substr(body, 20)
    scen.write("sms", interrupted)  # the originals are live again after the rollback
    scen.want("sms", [r for r in interrupted if r[0] % 14 == 0])  # half of them: the oracle file stays small; every version is in written
    con.close()
    stmts = ["pragma cache_size = 10", "pragma cache_spill = 10", "BEGIN",
             "update sms set body = 'interrupted-' || _id || substr(body, 20) where _id % 7 = 0"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "journal": C.read(path + "-journal")}
    files, exp = finish_notes(schema_conn(ph), path, "mmssms-aosp-baseline.sql", "hot-journal", files)
    exp["warnings"] = [{"table": "", "rowid": 0, "contains": "journal-hot"}]  # the library's warning, not a row's
    return files, exp


def variant_pdu_deleted(work):
    """Deleted MMS and SMS whose pages went to the freelist (secure_delete OFF), plus a dropped decoy table of
    exactly the sms shape (4G-R23: a strict fit cannot tell it from sms, so its rows are attributed to sms)."""
    path, con, ph = build_plain(work, "mmssms-aosp-baseline.sql", page_size=512)
    con.execute("pragma secure_delete = OFF")
    scen = SCENARIOS["mmssms-pdu-deleted"] = C.Scenario(CLAIMED)
    sms = [sms_row(i, body=("sms %d " % i) * 6) for i in range(1, 41)]
    pdu, addr, part = [], [], []
    for i in range(1, 41):
        pdu.append({"_id": i, "thread_id": 1, "date": T0 + 10 * i, "msg_box": 1 + i % 2, "m_type": 132, "m_id": "mid-%d" % i, "sub": "mms %d" % i})
        addr.append({"_id": i, "msg_id": i, "address": "+1555123%04d" % i, "type": 137 if i % 2 else 151})
        part.append({"_id": i, "mid": i, "seq": 0, "ct": "text/plain", "text": "mms text %d " % i + "x" * 20})
    populate(con, ph, sms, dims_baseline(), (pdu, addr, part))
    sms_sql = con.execute("select sql from sqlite_master where name = 'sms'").fetchone()[0]
    con.execute(sms_sql.replace("CREATE TABLE sms", "CREATE TABLE sms_decoy", 1))
    with C.batch(con):
        for i in range(1, 31):
            C.insert(con, "sms_decoy", sms_row(i, body=("decoy %d " % i) * 6, address="+15550009999"))
    scen.write("pdu", scen.snap(con, "pdu"))
    scen.write("sms", scen.snap(con, "sms"))
    decoy = scen.snap(con, "sms_decoy")
    scen.write("sms", decoy)
    scen.want("sms_decoy", decoy, label="sms")
    con.execute("drop table sms_decoy")
    scen.want("pdu", scen.snap(con, "pdu", "_id between 11 and 30"), label="pdu")
    scen.want("sms", scen.snap(con, "sms", "_id between 10 and 30"), label="sms")
    with C.batch(con):
        con.execute("delete from pdu where _id between 11 and 30")
        con.execute("delete from sms where _id between 10 and 30")
        con.execute("delete from addr where msg_id between 11 and 30")
        con.execute("delete from part where mid between 11 and 30")
    return finish_notes(con, path, "mmssms-aosp-baseline.sql", "mmssms-pdu-deleted")


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (ruling 4G-R14)."""
    def expect_fn(table, d, env, file):
        return (expect_pdu if table == "pdu" else expect_sms)(d, env, file, False)
    return C.recover_oracle(variant, files, SCENARIOS[variant], lambda f: make_env(f)[1], expect_fn)


VARIANTS = {
    "aosp-baseline": variant_baseline, "aosp-subid-alter": variant_subid_alter, "oem-extended": variant_oem,
    "utf16": variant_utf16, "av-freelist-512": variant_freelist, "wal-uncheckpointed": variant_wal, "hot-journal": variant_journal,
    "mmssms-pdu-deleted": variant_pdu_deleted,
}

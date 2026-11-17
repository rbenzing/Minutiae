"""Android WhatsApp msgstore.db fixtures (plan 4I): the structural variants.

    py -3 tools/fixtures/telephony_gen.py --family whatsapp-android --out internal/parsers/androidwhatsapp/testdata \
        --pins-go internal/parsers/androidwhatsapp/pins_test.go --pins-package androidwhatsapp_test
    py -3 tools/fixtures/telephony_gen.py --family whatsapp-android --check

The databases are written by the engine (Python's sqlite3), the WAL variants by a child process that dies without
closing (C.kill_child). The oracle states the spec's mapping rules (spec 6.4, 7) over the stored values read back
from the engine on a COPY of the finished files; it shares no code with the Go parser. The module also holds the
helpers the wa.db generator (whatsapp_wadb.py) shares: jid(), ms() and vcard().

Python 3 with its bundled sqlite3 only; no Docker. Not part of gen.sh all.
"""
import hashlib
import re
import struct

import telephony_common as C

FAMILY = "whatsapp-android"
SCENARIOS = {}  # no typed recovery oracle yet (plan 4I task 14)
SCHEMAS = C.os.path.join(C.HERE, "schemas", "whatsapp-android")
MAX_TEXT = 256 << 10

PKG_WA, PKG_W4B = "com.whatsapp", "com.whatsapp.w4b"
REMOTE = "/data/data/com.whatsapp/databases/msgstore.db"
REMOTE_BUSINESS = "/data/data/com.whatsapp.w4b/databases/msgstore.db"
REMOTE_USER10 = "/data/user/10/com.whatsapp/databases/msgstore.db"
REMOTE_CRYPT14 = "/sdcard/WhatsApp/Databases/msgstore.db.crypt14"
REMOTE_CRYPT15 = "/storage/emulated/0/Android/media/com.whatsapp/WhatsApp/Databases/msgstore-2026-11-01.1.db.crypt15"
REMOTE_BACKUP_BUSINESS = "/sdcard/WhatsApp Business/Databases/msgstore.db.crypt14"
REMOTE_BACKUP_PLAIN = "/sdcard/WhatsApp/Databases/msgstore.db"
REMOTE_OTHER = "/storage/emulated/0/Android/data/com.whatsapp/databases/msgstore.db"

T0 = 1600000000000  # ms
FLOOR_MS = 100_000_000_000  # message-table millisecond floor (1973): below it a value is invalid, never rescaled

C1 = "15550000001@s.whatsapp.net"
GRP = "120363000000000001@g.us"
STATUS = "status@broadcast"
CHAN = "120363000000000099@newsletter"
P2 = "15550000002@s.whatsapp.net"
P3 = "15550000003@s.whatsapp.net"
P9 = ("15550000009", "s.whatsapp.net")  # the chat whose jid row has a NULL raw_string

MODERN_TYPE = {0: "text", 1: "image", 2: "audio", 3: "video", 4: "contact_card", 5: "location", 9: "document",
               16: "live_location", 7: "system", 13: "gif", 20: "sticker", 24: "group_invite"}
REVOKED_TYPES = (15, 64)
LEGACY_TYPE = {0: "text", 1: "image", 2: "audio", 3: "video", 4: "contact_card", 5: "location"}
VCARD = "BEGIN:VCARD\r\nFN:Ann\r\nEND:VCARD\r\n"
DECIMAL = re.compile(r"[+-]?[0-9]+", re.ASCII)  # strconv.ParseInt base 10


def jid(user, server):
    """A JID string as WhatsApp stores it (shared with the wa.db generator)."""
    return "%s@%s" % (user, server)


def ms(seconds):
    """Unix seconds to the millisecond value the stores keep (shared with the wa.db generator)."""
    return int(seconds) * 1000


def vcard(name):
    return "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:%s\r\nEND:VCARD\r\n" % name


def split(j):
    user, server = j.rsplit("@", 1)
    return user, server


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


# ----------------------------------------------------------------------------------------- the CORE scenario

def core(modern_only):
    """The CORE messages as generation-neutral facts (plan 4I task 4). modern_only adds rows 18 and 19."""
    m = [
        dict(id=2, chat=C1, me=0, text="hello", ts=T0, recv=T0 + 1000, status=0, type=0),
        dict(id=3, chat=C1, me=1, text="hi", ts=T0 + 2000, status=5, type=0, delivered=T0 + 3000),
        dict(id=4, chat=C1, me=0, text="pic", ts=T0 + 4000, recv=T0 + 4100, status=0, type=1),
        dict(id=5, chat=C1, me=0, ts=T0 + 5000, recv=T0 + 5100, status=0, type=2),
        dict(id=6, chat=C1, me=1, ts=T0 + 6000, status=5, type=3),
        dict(id=7, chat=C1, me=0, ts=T0 + 7000, recv=T0 + 7100, status=0, type=4, vcard=VCARD),
        dict(id=8, chat=C1, me=0, ts=T0 + 8000, recv=T0 + 8100, status=0, type=5, lat=52.52, lon=13.405),
        dict(id=9, chat=GRP, me=0, text="g", ts=T0 + 9000, recv=T0 + 9100, status=0, type=0, sender=P2),
        dict(id=10, chat=GRP, me=0, text="g10", ts=T0 + 10000, recv=T0 + 10100, status=0, type=0),
        dict(id=11, chat=GRP, me=1, text="g11", ts=T0 + 11000, status=5, type=0),
        dict(id=12, chat=STATUS, me=0, text="s12", ts=T0 + 12000, recv=T0 + 12100, status=0, type=0, sender=P3),
        dict(id=13, chat=STATUS, me=1, text="s13", ts=T0 + 13000, status=5, type=0),
        dict(id=14, chat=C1, me=0, ts=T0 + 14000, recv=T0 + 14100, status=0, type=999),
        dict(id=15, chat=GRP, me=0, text="Group A", ts=T0 + 15000, recv=T0 + 15100, status=6, type=7, system=True),
        dict(id=16, chat=C1, me=0, text="s16", ts=1600000, recv=T0 + 16100, status=0, type=0),
        dict(id=17, chat=C1, me=0, text="s17", ts=0, status=0, type=0),
    ]
    if modern_only:
        m += [
            dict(id=18, chat=CHAN, me=0, text="news", ts=T0 + 18000, recv=T0 + 18100, status=0, type=0),
            dict(id=19, chat=P9, me=0, text="p19", ts=T0 + 19000, recv=T0 + 19100, status=0, type=0),
        ]
    for x in m:
        x["key"] = "K%02d" % x["id"]
    return m


def legacy_hostile():
    """Rows 21 to 25 of android-legacy-2.11: hostile media_wa_type values."""
    vals = ["1", " 1", "1.0", "x", b"\x01"]
    return [dict(id=21 + i, chat=C1, me=0, text="h%d" % (21 + i), ts=T0 + 21000 + i, recv=T0 + 21100 + i, status=0, type=-1,
                 wa_type=v, key="K%02d" % (21 + i)) for i, v in enumerate(vals)]


def legacy_row(m):
    """A messages row (legacy columns) for the neutral fact m."""
    wa_type = m.get("wa_type", str(m["type"]))
    sys_row = m.get("system")
    r = {"_id": m["id"], "key_remote_jid": m["chat"] if isinstance(m["chat"], str) else jid(*m["chat"]),
         "key_from_me": m["me"], "key_id": m["key"], "status": m["status"], "data": m.get("text"),
         "timestamp": m["ts"], "media_wa_type": "0" if sys_row else wa_type,
         "received_timestamp": m.get("recv", -1), "send_timestamp": -1,
         "receipt_server_timestamp": -1, "receipt_device_timestamp": m.get("delivered", -1)}
    if sys_row:
        r["media_size"] = 1
    if m["type"] == 1:
        r["media_caption"] = m.get("text")
        r["data"] = None
    if m["type"] == 4:
        r["data"] = m["vcard"]
    if m["type"] == 5:
        r["latitude"], r["longitude"] = m["lat"], m["lon"]
    if m.get("sender"):
        r["remote_resource"] = m["sender"]
    if is_int(m["type"]) and m["type"] == 999:
        r["media_wa_type"] = "999"
    return r


def sentinel_row():
    return {"_id": 1, "key_remote_jid": "-1", "key_from_me": None, "key_id": "K01", "status": None}


class Jids:
    """The jid and chat tables of a modern database: ids are allocated in first-use order."""

    def __init__(self):
        self.jids, self.ids, self.chats = [], {}, []

    def jid_id(self, j, null_raw=False):
        key = (j, null_raw)
        if key not in self.ids:
            user, server = split(j) if isinstance(j, str) else j
            raw = None if null_raw else (j if isinstance(j, str) else jid(*j))
            self.ids[key] = len(self.jids) + 1
            self.jids.append({"_id": len(self.jids) + 1, "user": user, "server": server, "raw_string": raw,
                              "type": 0 if server == "s.whatsapp.net" else 1})
        return self.ids[key]

    def chat_id(self, j):
        null_raw = not isinstance(j, str)
        jid_id = self.jid_id(j, null_raw)
        for c in self.chats:
            if c["jid_row_id"] == jid_id:
                return c["_id"]
        subject = {GRP: "Group A", CHAN: "Channel One"}.get(j)
        self.chats.append({"_id": len(self.chats) + 1, "jid_row_id": jid_id, "subject": subject, "hidden": 0})
        return len(self.chats)


def modern_row(m, js):
    """A message row (modern columns) for the neutral fact m."""
    chat_id = js.chat_id(m["chat"])
    sender = m.get("sender")
    r = {"_id": m["id"], "chat_row_id": chat_id, "from_me": m["me"], "key_id": m["key"],
         "sender_jid_row_id": js.jid_id(sender) if sender else 0, "status": m["status"], "timestamp": m["ts"],
         "received_timestamp": m.get("recv", 0), "message_type": m["type"], "text_data": m.get("text"), "sort_id": m["id"],
         "starred": 0}
    if m.get("system"):
        r["message_type"] = 7
    return r


# ----------------------------------------------------------------------------------------- the oracle

def text_of(v):
    """The cleaned text of a stored TEXT value (bytes), as common.CleanText reads it."""
    return C.clean(v, MAX_TEXT)


def stored(v):
    """A stored value as the raw payload shows it."""
    if isinstance(v, C.Blob):
        return C.jv(bytes(v))
    if isinstance(v, bytes):
        return v.decode("utf-8")
    return v


def time_of(v):
    """(time or None, status): ms in a message table; a sentinel or invalid value is no time (floor 1e11, spec 7.6)."""
    if not is_int(v):
        return None, None
    st = C.classify(v, "ms")
    if st == "valid" and v < FLOOR_MS:
        st = "invalid"
    return (C.iso(v, "ms") if st == "valid" else None), st


def app_of(remote):
    """The app the path names (spec 6.8): the package directory, or the external folder of a backup copy."""
    return "whatsapp_business" if ("/" + PKG_W4B + "/") in remote or "/WhatsApp Business/" in remote else "whatsapp"


def summary_of(direction, counterparty, text):
    plain = text is None or (text.isascii() and text.isprintable() and len(text) <= 80)
    if not plain or not counterparty.isascii() or len(counterparty.encode()) > 64:
        return None
    return ("whatsapp %s %s: %s" % (direction, counterparty, text or "")).rstrip(" ")


def expect_message(table, r, ctx, remote):
    """The expectation of one messages or message row as the engine reads it back (spec 6.4, 7.1, 7.3, 7.6)."""
    modern = table == "message"
    me = r.get("from_me" if modern else "key_from_me")
    direction = {0: "in", 1: "out"}.get(me, "unknown") if is_int(me) else "unknown"
    raw = {"source_table": table, "app": app_of(remote), "app_basis": "path"}
    # chat
    if modern:
        chat = ctx["chat"].get(r.get("chat_row_id"))
        chat_raw = ctx["jid"].get(chat["jid_row_id"]) if chat else None
        chat_jid = chat_raw["raw_string"].decode() if chat_raw and chat_raw.get("raw_string") else None
        raw["chat_row_id"] = r.get("chat_row_id")
        subject = chat["subject"] if chat else None
        jparts = {"user": chat_raw["user"].decode(), "server": chat_raw["server"].decode()} if chat_raw else None
    else:
        chat_jid = r["key_remote_jid"].decode() if r.get("key_remote_jid") else None
        subject = ctx.get("subject", {}).get(chat_jid)
        jparts = None
    if chat_jid:
        raw["chat_jid"] = chat_jid
    elif jparts:
        raw["chat_jid_parts"] = jparts
    server = split(chat_jid)[1] if chat_jid else (jparts["server"] if jparts else "")
    group = server == "g.us"
    status_chat = chat_jid == STATUS
    if status_chat:
        raw["chat_class"] = "status"
    # kind
    if modern:
        t = r.get("message_type")
        raw["message_type"] = t
        if is_int(t) and t in MODERN_TYPE:
            kind = "text" if t == 0 else "system" if t == 7 else "media"
            raw["label_message_type"] = MODERN_TYPE[t]
        elif is_int(t) and t in REVOKED_TYPES:
            kind, raw["label_message_type"] = "unknown", "revoked"
        else:
            kind = "unknown"
    else:
        wt = r.get("media_wa_type")
        raw["media_wa_type"] = stored(wt)
        kind = "unknown"
        if isinstance(wt, bytes) and not isinstance(wt, C.Blob) and DECIMAL.fullmatch(wt.decode("ascii", "replace")):
            v = int(wt.decode())
            if v in LEGACY_TYPE and not (v == 0 and r.get("status") == 6):
                kind = "text" if v == 0 else "media"
                raw["label_media_wa_type"] = LEGACY_TYPE[v]
    # status: never mapped (R4), the stored value in raw
    if r.get("status") is not None:
        raw["status"] = r["status"]
    if me is not None:
        raw["from_me" if modern else "key_from_me"] = me
    # participants
    sender_raw = None
    participants, unknown = [], False
    if modern:
        sid = r.get("sender_jid_row_id")
        if is_int(sid) and sid > 0:
            sj = ctx["jid"].get(sid)
            sender_raw = sj["raw_string"].decode() if sj and sj.get("raw_string") else None
        if sid is not None:
            raw["sender_jid_row_id"] = sid
    else:
        if r.get("remote_resource"):
            sender_raw = r["remote_resource"].decode()
            raw["remote_resource"] = sender_raw
    if sender_raw:
        raw["sender_jid"] = sender_raw
    if direction == "out" and chat_jid:
        participants = [{"role": "to", "address": chat_jid}]
    elif direction == "in" and status_chat:
        unknown = True
    elif direction == "in" and group:
        if sender_raw:
            participants = [{"role": "from", "address": sender_raw}]
        else:
            unknown, raw["sender_state"] = True, "absent"
    elif direction == "in" and chat_jid:
        participants = [{"role": "from", "address": chat_jid}]
    else:
        unknown = True
        if not chat_jid and jparts:
            raw["from_jid" if direction == "in" else "to_jid"] = dict(jparts)
    # body
    body, text_source = None, None
    if modern:
        txt = r.get("text_data")
        if r.get("message_type") == 4:
            vc = ctx["vcard"].get(r["rowid"])
            txt, text_source = vc, "part"
    else:
        txt = r.get("data")
        if kind == "media" and raw.get("label_media_wa_type") == "contact_card":
            text_source = "part"
        if r.get("media_caption") is not None:
            raw["media_caption"] = r["media_caption"].decode()
        if r.get("media_size") is not None and r.get("status") == 6:
            raw["media_size"] = r["media_size"]
    if txt:
        c = text_of(txt)
        if c.text:
            body, text_source = c.text, text_source or "text"
            if c.flags:
                C.mark(raw, "body")
    # times
    t, st = time_of(r.get("timestamp"))
    if r.get("timestamp") is not None:
        raw["timestamp"] = r["timestamp"]
    if st and st != "valid":
        raw["timestamp_status"] = st
    times = {}
    rt, rst = time_of(r.get("received_timestamp"))
    if r.get("received_timestamp") is not None:
        raw["received_timestamp"] = r["received_timestamp"]
        if rst and rst != "valid":
            raw["received_timestamp_status"] = rst
    if rt:
        times["received"] = rt
    if not modern:
        dt, dst = time_of(r.get("receipt_device_timestamp"))
        if r.get("receipt_device_timestamp") is not None:
            raw["receipt_device_timestamp"] = r["receipt_device_timestamp"]
            if dst and dst != "valid":
                raw["receipt_device_timestamp_status"] = dst
        if dt:
            times["delivered"] = dt
    payload = {"channel": "whatsapp", "direction": direction, "kind": kind, "participants": participants,
               "status": "unknown", "raw": raw}
    if unknown:
        payload["participants_unknown"] = True
    thread = {"group": group}
    if chat_jid:
        thread["id"] = chat_jid
    if subject:
        thread["title"] = text_of(subject).text
    payload["thread"] = thread
    if r.get("key_id"):
        payload["guid"] = r["key_id"].decode()
    if text_source and body is not None:
        payload["text_source"] = text_source
    counter = participants[0]["address"] if participants else (chat_jid or "unknown")
    return {"table": table, "rowid": r["rowid"], "file": "any", "type": "message", "source_path": remote, "time": t,
            "time_end": None, "times": times, "deleted": False,
            "summary": summary_of(direction, counter, body), "body": body, "payload": payload}


def context(view):
    """Lookup tables of the stored rows, by key."""
    ctx = {"chat": {}, "jid": {}, "vcard": {}, "subject": {}}
    for r in view.get("chat", []):
        ctx["chat"][r["rowid"]] = r
    for r in view.get("jid", []):
        ctx["jid"][r["rowid"]] = r
    for r in view.get("message_vcard", []):
        ctx["vcard"].setdefault(r["message_row_id"], r["vcard"])
    for r in view.get("chat_list", []):
        if r.get("subject"):
            ctx["subject"][r["key_remote_jid"].decode()] = r["subject"]
    return ctx


TABLES = ["messages", "message", "jid", "chat", "chat_list", "message_vcard", "messages_vcards"]


def oracle(files, remote, variant, ddl, generation, app_user=None):
    view = C.engine_view(files, TABLES)
    ctx = context(view)
    exp = C.expect_header(FAMILY, variant, ddl)
    n_messages = n_message = sentinel = 0
    for r in view.get("messages", []):
        if r.get("key_remote_jid") == b"-1":
            sentinel += 1
            exp["warnings"].append({"table": "messages", "rowid": r["rowid"], "contains": "sentinel"})
            continue
        n_messages += 1
        exp["records"].append(expect_message("messages", r, ctx, remote))
    for r in view.get("message", []):
        n_message += 1
        exp["records"].append(expect_message("message", r, ctx, remote))
    notes = exp["notes"]
    notes["generation"] = generation
    if "messages" in view:
        notes["rows.messages"] = str(n_messages)
        notes["sentinel_rows"] = str(sentinel)
    if "message" in view:
        notes["rows.message"] = str(n_message)
    notes["app"] = app_of(remote)
    notes["database_user"] = "10" if "/user/10/" in remote else "0"
    if n_messages and n_message:
        exp["warnings"].append({"table": "", "rowid": 0, "contains": "whatsapp-both-generations"})
    return exp


# ----------------------------------------------------------------------------------------- building

def build_db(work, ddl_name, fill, drop=(), journal_wal=False, page_size=4096):
    path = work + "/m.db"
    con = C.connect(path, page_size=page_size)
    con.executescript(C.load_ddl(ddl_name, SCHEMAS)[0])
    for stmt in drop:
        con.execute(stmt)
    with C.batch(con):
        fill(con)
    if journal_wal:
        con.execute("pragma journal_mode = wal")
    con.close()
    return path


def fill_legacy(rows):
    def f(con):
        seen = []
        for r in rows:
            C.insert(con, "messages", r)
            j = r["key_remote_jid"]
            if j != "-1" and j not in seen:
                seen.append(j)
        for j in seen:
            C.insert(con, "chat_list", {"key_remote_jid": j, "subject": "Group A" if j == GRP else None})
        for r in rows:
            if r.get("data") and r["data"].startswith("BEGIN:VCARD"):
                C.insert(con, "messages_vcards", {"message_row_id": r["_id"], "vcard": r["data"]})
    return f


def populate_modern(con, facts, js, vcards=True):
    for m in facts:
        row = modern_row(m, js)
        C.insert(con, "message", row)
    for j in js.jids:
        C.insert(con, "jid", j)
    for c in js.chats:
        C.insert(con, "chat", c)
    if vcards:
        for m in facts:
            if m["type"] == 4:
                C.insert(con, "message_vcard", {"message_row_id": m["id"], "vcard": m["vcard"]})
            if m["type"] == 5:
                C.insert(con, "message_location", {"message_row_id": m["id"], "latitude": m["lat"], "longitude": m["lon"]})


def modern_fill(facts, with_vcards=True):
    def f(con):
        # the jid and chat rows first need their ids: allocate by walking the facts in id order
        js = Jids()
        for m in facts:
            js.chat_id(m["chat"])
            if m.get("sender"):
                js.jid_id(m["sender"])
        populate_modern(con, facts, js, with_vcards)
    return f


def finish(path, remote, variant, ddl, generation, extra=None):
    files = {"db": C.read(path)}
    if C.os.path.exists(path + "-wal"):
        files["wal"] = C.read(path + "-wal")
    exp = oracle(files, remote, variant, ddl, generation)
    return files, exp


def v_legacy(work):
    rows = [sentinel_row()] + [legacy_row(m) for m in core(False)] + [legacy_row(m) for m in legacy_hostile()]
    path = build_db(work, "legacy-2.11.sql", fill_legacy(rows))
    return finish(path, REMOTE, "android-legacy-2.11", "legacy-2.11.sql", "legacy")


def transition_fill(legacy_facts, modern_facts, sentinel=True):
    def f(con):
        if sentinel:
            C.insert(con, "messages", sentinel_row())
        for m in legacy_facts:
            C.insert(con, "messages", legacy_row(m))
        js = Jids()
        for m in legacy_facts + modern_facts:
            js.chat_id(m["chat"])
            if m.get("sender"):
                js.jid_id(m["sender"])
        for m in modern_facts:
            C.insert(con, "message", modern_row(m, js))
        for j in js.jids:
            C.insert(con, "jid", j)
        for c in js.chats:
            C.insert(con, "chat", c)
        for m in legacy_facts:
            if m["type"] == 4:
                C.insert(con, "messages_vcards", {"message_row_id": m["id"], "vcard": m["vcard"]})
        for m in modern_facts:
            if m["type"] == 4:
                C.insert(con, "message_vcard", {"message_row_id": m["id"], "vcard": m["vcard"]})
    return f


def v_trans_legacy(work):
    path = build_db(work, "transition-2021.sql", transition_fill(core(False), []))
    return finish(path, REMOTE, "android-transition-legacydata", "transition-2021.sql", "transition")


def v_trans_modern(work):
    path = build_db(work, "transition-2021.sql", transition_fill([], core(False)))
    return finish(path, REMOTE, "android-transition-moderndata", "transition-2021.sql", "transition")


def v_trans_both(work):
    path = build_db(work, "transition-2021.sql", transition_fill(core(False), core(False)))
    return finish(path, REMOTE, "android-transition-both", "transition-2021.sql", "transition")


def v_trans_empty(work):
    path = build_db(work, "transition-2021.sql", transition_fill([], []))
    return finish(path, REMOTE, "android-transition-empty", "transition-2021.sql", "transition")


def insert_stmt(table, row):
    return "insert into %s(%s) values (%s)" % (table, ",".join(row), ",".join(C.lit(v) for v in row.values()))


def v_trans_walonly(work):
    """CORE in messages (the main file); the first message row only in a committed WAL frame; one more row in
    frames that were written without a commit (a large row that spilled from a tiny page cache before the kill)."""
    facts = core(False)
    path = build_db(work, "transition-2021.sql", transition_fill(facts, []), journal_wal=True)
    js = Jids()
    for m in facts:
        js.chat_id(m["chat"])
        if m.get("sender"):
            js.jid_id(m["sender"])
    first = modern_row(facts[0], js)
    big = dict(modern_row(dict(facts[0], id=40, key="K40"), js), text_data="@@BIG@@")
    # 200,000 characters built inside the engine (a command line cannot carry them)
    big_stmt = insert_stmt("message", big).replace("'@@BIG@@'", "replace(hex(zeroblob(100000)), '0', 'x')")
    stmts = ["pragma wal_autocheckpoint = 0", insert_stmt("message", first), "pragma cache_size = 5",
             "pragma cache_spill = 1", "begin", big_stmt]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    assert uncommitted_frames(files["wal"], 4096) > 0, "the WAL holds no frame after its last commit"
    exp = oracle(files, REMOTE, "android-transition-walonly", "transition-2021.sql", "transition")
    for rec in exp["records"]:
        rec["source_path"] = ""  # a WAL companion: the source path follows the role of the cell
    return files, exp


def uncommitted_frames(wal, page_size):
    """Frames after the last commit frame of a WAL (frames whose commit field is zero at the tail)."""
    n = (len(wal) - 32) // (24 + page_size)
    last = 0
    for i in range(n):
        off = 32 + i * (24 + page_size)
        if struct.unpack(">I", wal[off + 4:off + 8])[0]:
            last = i + 1
    return n - last


MODERN_2021_DROP = ["drop table messages", "drop table messages_quotes", "drop table messages_vcards", "drop table receipts",
                    "drop view legacy_available_messages_view", "drop view message_view", "drop view available_message_view"]


def v_modern_2021(work):
    path = build_db(work, "transition-2021.sql", modern_fill(core(True)), drop=MODERN_2021_DROP)
    return finish(path, REMOTE, "android-modern-2021", "transition-2021.sql", "modern")


def modern_variant(work, remote, variant, wal=False):
    facts = core(True)
    early, late = (facts[:-4], facts[-4:]) if wal else (facts, [])
    path = build_db(work, "modern-2026.sql", modern_fill(early), journal_wal=wal)
    if wal:
        js = Jids()
        for m in early:
            js.chat_id(m["chat"])
            if m.get("sender"):
                js.jid_id(m["sender"])
        nj, nc = len(js.jids), len(js.chats)
        stmts = ["pragma wal_autocheckpoint = 0"]
        rows = [modern_row(m, js) for m in late]
        for j in js.jids[nj:]:
            stmts.append(insert_stmt("jid", j))
        for c in js.chats[nc:]:
            stmts.append(insert_stmt("chat", c))
        stmts += [insert_stmt("message", r) for r in rows]
        C.kill_child(path, stmts)
    files, exp = finish(path, remote, variant, "modern-2026.sql", "modern")
    if wal:
        for rec in exp["records"]:
            rec["source_path"] = ""
    return files, exp


def v_modern_2026(work):
    return modern_variant(work, REMOTE_USER10, "android-modern-2026", wal=True)


def v_modern_business(work):
    return modern_variant(work, REMOTE_BUSINESS, "android-modern-business")


def v_neither(work):
    path = work + "/n.db"
    con = C.connect(path)
    con.execute("create table foo(x)")
    con.close()
    return {"db": C.read(path)}, C.expect_header(FAMILY, "android-neither", "")


def stream(n):
    """n bytes of a SHA-256 counter stream: high entropy, no header of any real format (synthetic)."""
    out = b""
    i = 0
    while len(out) < n:
        out += hashlib.sha256(struct.pack(">Q", i)).digest()
        i += 1
    return out[:n]


def v_crypt14(work):
    return {"db": stream(64 << 10)}, C.expect_header(FAMILY, "android-crypt14", "")


def v_crypt15(work):
    return {"db": stream(64 << 10)}, C.expect_header(FAMILY, "android-crypt15", "")


def v_backup_notsqlite(work):
    return {"db": (b"This is a plain text file, not a database. " * 100)[:4096]}, C.expect_header(FAMILY, "android-backup-notsqlite", "")


def unparsed(work, variant):
    path = build_db(work, "modern-2026.sql", modern_fill(core(True)))
    return {"db": C.read(path)}, C.expect_header(FAMILY, variant, "modern-2026.sql")


def v_backup_plain(work):
    return unparsed(work, "android-backup-plain")


def v_other_location(work):
    return unparsed(work, "android-other-location")


VARIANTS = {
    "android-legacy-2.11": v_legacy,
    "android-transition-legacydata": v_trans_legacy,
    "android-transition-moderndata": v_trans_modern,
    "android-transition-both": v_trans_both,
    "android-transition-walonly": v_trans_walonly,
    "android-transition-empty": v_trans_empty,
    "android-modern-2021": v_modern_2021,
    "android-modern-2026": v_modern_2026,
    "android-modern-business": v_modern_business,
    "android-neither": v_neither,
    "android-crypt14": v_crypt14,
    "android-crypt15": v_crypt15,
    "android-backup-notsqlite": v_backup_notsqlite,
    "android-backup-plain": v_backup_plain,
    "android-other-location": v_other_location,
}

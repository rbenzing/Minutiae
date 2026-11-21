"""Telegram Android cache4.db fixtures: DDL variants, the CORE scenario with TL blobs, the literal oracle (plan 4I4).

    py -3 -I tools/fixtures/telephony_gen.py --family telegram-android --out internal/parsers/androidtelegram/testdata \\
        --pins-go internal/parsers/androidtelegram/pins_test.go --pins-package androidtelegram_test
    py -3 -I tools/fixtures/telephony_gen.py --family telegram-android --recover-oracle --out internal/parsers/androidtelegram/testdata
    py -3 -I tools/fixtures/telephony_gen.py --family telegram-android --check

The databases are built from the DDL snapshots of tools/fixtures/schemas/telegram-android (cross-checked against the
pinned app source, CROSSCHECK.md there) with the user version each variant states, written as the app writes them
(PRAGMA secure_delete = ON, journal_mode = WAL; spec 6.1). Every TL blob is built by telegram_tl_encode.py from
constructors.tsv. The expected records (expect.json) come from the decode of every blob by the independent oracle
telegram_tl_oracle.py, then the mapping rules of spec 8-10 and the plan's state vocabularies, restated here in Python
(shared with no Go code), over the stored values read back from the engine on a COPY of the finished files. The
oracle, the encoder and the Go table derive from the same facts: their independent check is the cross-check against
the app source, not each other.

Choices this oracle states where the spec leaves the detail to the plan (the Go parser follows them):
  - a NULL mid, uid, date or out is undecided (unknown_columns "<table>.<col>" = "null"), never a stored NULL in raw
    (spec 12; plan 4I4 Task 8 amended the oracle); a NULL in another plain column is kept as stored.
  - a blob's stop: raw.<col>_stop_kind, _stop_offset (where the item that stopped the decode starts: a vector's bad
    count included, Task 8), _stop_path (when
    not the top), _stop_id (hex, when the stop names an id); none for a `prefix` stop (decoded-prefix).
  - raw.<col>_undecided lists the record fields (MESSAGE_MAPPED, USER_MAPPED) of the top row that start after the
    top-level field holding the stop and are present by their flags (or whose flags word was not read, or ungated).
  - kind: a truncated, malformed or capped message blob is `unknown`; a partial one takes the media rule.
  - replydata gets the blob keys (replydata_state, _len, _sha256, _constructor, ...) and, when its top is a Message
    row, raw.reply_copy {copy, constructor, id, date, text}.
  - the dialog address of an outgoing message is the dialog's own (4I4-P11); a negative dialog left unresolved, a
    folder and an unknown dialog have no participant and no thread group.
  - a CORE message row has no trailer unless the row states one (R3, R41, R42).
  - secret fields hold SYNTHETIC-NOT-A-KEY- plus a SHA-256 chain of the scenario seed (the encoder refuses anything
    else); enc_chats key columns hold the SHA-256 chains of their seeds (spec 9, the key-material test's targets).
Every value here is fictional (example.test, 1555 numbers, invented names).
"""
import base64
import hashlib
import json
import os
import re
import sqlite3
import struct
import unicodedata

import telegram_tl_encode as E
import telegram_tl_oracle as O
import telephony_common as C

FAMILY = "telegram-android"
CLAIMED = ("messages", "messages_v2", "users")  # Meta.Claims of android.telegram (spec 11)
SCENARIOS = {}  # variant -> C.Scenario (the typed recovery oracle)
REMOTE = "/data/data/org.telegram.messenger/files/cache4.db"
SCHEMAS = os.path.join(C.HERE, "schemas", "telegram-android")

D = 1700000000  # 2023-11-14T22:13:20Z
SELF, ANN, BOT = 1000001, 1000002, 1000003
SECRET_BIT = 1 << 62
S77 = SECRET_BIT | 77
S5 = SECRET_BIT | 0xFFFFFFFB
FOLDER1 = (1 << 61) | 1
I64_MIN = -(1 << 63)

MAX_BODY = 256 << 10
MAX_SHORT = 4 << 10  # reply_copy.text, attach_path, media texts
MAX_NAME = 1 << 10  # names and titles
MAX_VCARD = 64 << 10
MAX_ID = 1 << 10  # identifiers are never cut: over it they are absent with their state
MAX_PARAMS = 64
MAX_NOTE = 512
MAX_SEEN_NOTE = 1000

MESSAGE_MAPPED = ("out", "id", "from_id", "peer_id", "fwd_from", "via_bot_id", "reply_to", "date", "message", "media",
                  "action", "views", "edit_date", "grouped_id", "ttl_period")
USER_MAPPED = ("id", "access_hash", "first_name", "last_name", "username", "phone")
BLOB_STATES = ("decoded", "decoded-prefix", "partial", "unknown-constructor", "truncated", "malformed", "cap", "budget", "null")
COUNTED_STATES = BLOB_STATES[2:8]
MESSAGE_BOOLS = ("silent", "post", "from_scheduled", "legacy", "edit_hide", "pinned", "noforwards", "offline",
                 "invert_media", "mentioned", "media_unread", "unread")
USER_BOOLS = (("self", "self"), ("contact", "contact"), ("mutual_contact", "mutual_contact"), ("deleted", "account_deleted"),
              ("bot", "bot"), ("min", "min"), ("verified", "verified"), ("scam", "scam"), ("fake", "fake"), ("premium", "premium"))
STATUS_LABELS = {-100: "recently", -101: "last_week", -102: "last_month",
                 -1000: "recently_by_me", -1001: "last_week_by_me", -1002: "last_month_by_me"}
REASONS = {0x85E42301: "missed", 0xFAF7E8C9: "busy", 0x57ADC690: "hangup", 0xE095C1A0: "disconnect",
           0x9FBBF1F7: "migrate_conference_call"}
PHONE_CALL = 0x80E11A7F
READ_TABLES = ("messages", "messages_v2", "users", "users_data", "chats", "contacts", "enc_chats")
ROW_NOTE_TABLES = ("messages", "messages_v2", "users", "chats", "enc_chats", "contacts", "users_data", "media_v4",
                   "media_v3", "media_v2", "dialogs", "messages_topics", "scheduled_messages_v2", "user_contacts_v7")
GO_SPACE = "".join(chr(c) for c in [9, 10, 11, 12, 13, 32, 0x85, 0xA0, 0x1680, *range(0x2000, 0x200B), 0x2028, 0x2029, 0x202F, 0x205F, 0x3000])


# ---------------------------------------------------------------- small helpers


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def is_blob(v):
    return isinstance(v, C.Blob)


def hx(v):
    return "0x%08x" % v


def sha(b):
    return hashlib.sha256(bytes(b)).hexdigest()


def gofloat(x):
    """A float as Go's encoding/json writes it (an integral value without a fraction)."""
    return int(x) if float(x).is_integer() and abs(x) < 1e21 else x


def chain(seed, n):
    """n distinctive bytes: SHA-256 of the seed, then of each previous digest, concatenated."""
    out, h = b"", seed.encode("utf-8")
    while len(out) < n:
        h = hashlib.sha256(h).digest()
        out += h
    return out[:n]


def secret(seed, n):
    """A secret field value: the encoder's synthetic marker, then the seed's chain."""
    return (E.SYNTHETIC_PREFIX + chain(seed, n))[:max(n, len(E.SYNTHETIC_PREFIX) + 4)]


def u32le(v):
    return struct.pack("<I", v)


def s32(v):
    """The low 32 bits of v as a signed int (a cast to Java int)."""
    v &= 0xFFFFFFFF
    return v - (1 << 32) if v >= 1 << 31 else v


def summarize(s, limit, chars=False):
    """common.Summarize / SummarizeChars restated: whitespace and control runs become one inner space, a format
    character is shown as <U+XXXX>, the result is cut at limit (bytes, or characters) with "..."."""
    out, cost, cut_len, pending, over = [], 0, 0, False, False

    def add(unit):
        nonlocal cost, cut_len
        out.append(unit)
        cost += len(unit) if chars else len(unit.encode("utf-8"))
        if cost <= limit - 3:
            cut_len = len("".join(out))
        return cost > limit

    for ch in s:
        if ch in GO_SPACE or unicodedata.category(ch) == "Cc":
            pending = bool(out)
            continue
        if pending:
            pending = False
            if add(" "):
                over = True
                break
        unit = ch
        if unicodedata.category(ch) == "Cf":
            unit = "<U+%04X>" % ord(ch)
        if add(unit):
            over = True
            break
    text = "".join(out)
    if not over:
        return text
    return text[:cut_len].rstrip(" ") + "..."


def message_summary(channel, direction, counterparty, text):
    head = " ".join([summarize(channel, 32), summarize(direction, 16), summarize(counterparty, 64)])
    return (head + ": " + summarize(text, 80, True)).rstrip(" ")


def call_summary(direction, outcome, address):
    parts = ["call"] + [p for p in (summarize(direction, 16), summarize(outcome, 16), summarize(address, 64)) if p]
    return " ".join(parts)


def contact_summary(display, phones):
    name = summarize(display or "", 80, True)
    phone = ""
    for p in phones:
        phone = summarize(p, 32)
        if phone:
            break
    return " ".join(x for x in (name, phone) if x)


def note_list(names, limit=MAX_NOTE, sep=","):
    """A list joined by sep, cut at an entry boundary within limit bytes, with a trailing ",…"."""
    out = ""
    for n in names:
        add = len(n.encode("utf-8")) + (len(sep) if out else 0)
        if len(out.encode("utf-8")) + add > limit - len(",…".encode("utf-8")):
            return out + ",…"
        out += (sep if out else "") + n
    return out


def cleaned(raw, key, value, limit):
    """A cleaned text (C4) or None when empty; an altered text is listed in raw.text_flags."""
    c = C.clean(bytes(value), limit)
    if c.flags:
        C.mark(raw, key)
    return c.text or None


# ---------------------------------------------------------------- the decode, read through paths


class Blob:
    """The oracle's decode of one blob with the derived state (4I4-P10)."""

    def __init__(self, data, typ):
        self.data = bytes(data)
        self.res = O.decode(self.data, typ)
        self.f = {}
        for e in self.res["fields"]:
            self.f.setdefault(e["path"], e)
        stop = self.res["stop"]
        if stop is None:
            self.state = "decoded"
        elif stop["kind"] == "unknown-constructor" and stop["path"] == "":
            self.state = "unknown-constructor"
        elif stop["kind"] in ("unknown-constructor", "opaque"):
            self.state = "partial"
        elif stop["kind"] == "prefix":
            self.state = "decoded-prefix"
        else:
            self.state = stop["kind"]
        top = self.res["top"]
        self.row = O.table().get(top) if top is not None and self.state != "unknown-constructor" else None
        self.cls = self.row.cls if self.row else None

    def val(self, path):
        e = self.f.get(path)
        return None if e is None else e.get("value")

    def has(self, path):
        return path in self.f

    def obj_id(self, path):
        e = self.f.get(path)
        return e["id"] if e and e["kind"] == "object" else None

    def obj_cls(self, path):
        e = self.f.get(path)
        return e["cls"] if e and e["kind"] == "object" else None

    def stop_at(self, path):
        """The stop when it lies at path (an object whose id stopped the decode), else None."""
        s = self.res["stop"]
        return s if s and s["path"] == path else None

    def flag(self, name):
        return self.val(name) is True

    def undecided(self, mapped):
        """The mapped fields of the top row that start after the top-level field holding the stop and are present
        by their flags (or whose flags word was not read, or ungated)."""
        s = self.res["stop"]
        if s is None or s["kind"] == "prefix" or self.row is None:
            return []
        head = re.split(r"[.\[]", s["path"])[0]
        names = [it.name.split(".")[0] for it in self.row.items if it.kind is not None]
        if head not in names:  # the stop is at the top object itself
            return []
        after = names.index(head)
        out = []
        for i, it in enumerate([it for it in self.row.items if it.kind is not None]):
            name = it.name.split(".")[0]
            if i <= after or name not in mapped or name in out:
                continue
            if it.word is not None:
                word = self.val(it.word)
                if word is not None and not word >> it.bit & 1:
                    continue
            out.append(name)
        return out


def blob_raw(raw, prefix, b, mapped=None):
    """The keys beside a decoded non-NULL blob (plan "State vocabularies")."""
    raw[prefix + "_state"] = b.state
    raw[prefix + "_len"] = len(b.data)
    raw[prefix + "_sha256"] = sha(b.data)
    if len(b.data) >= 4:
        raw[prefix + "_constructor"] = hx(struct.unpack_from("<I", b.data)[0])
    if b.res["flags"]:
        raw[prefix + "_flags"] = ",".join(b.res["flags"])
    s = b.res["stop"]
    if s is not None and s["kind"] != "prefix":
        raw[prefix + "_stop_kind"] = s["kind"]
        raw[prefix + "_stop_offset"] = s["offset"]
        if s["path"]:
            raw[prefix + "_stop_path"] = s["path"]
        if s["id"] is not None:
            raw[prefix + "_stop_id"] = hx(s["id"])
    if mapped:
        und = b.undecided(mapped)
        if und:
            raw[prefix + "_undecided"] = ",".join(und)


PEER_KIND = (("TL_peerUser", "user", "user_id"), ("TL_peerChat", "chat", "chat_id"), ("TL_peerChannel", "channel", "channel_id"))


def address(b, path):
    """The participant address of a Peer object or an inline id at path (4I4-P11, T4), or None."""
    e = b.f.get(path)
    if e is None:
        return None
    if e["kind"] == "object":
        for prefix, kind, field in PEER_KIND:
            if e["cls"].startswith(prefix):
                v = b.val(path + "." + field)
                return None if v is None else "%s:%d" % (kind, v)
        return None
    if e["kind"] in ("id32", "id64") and e["cls"] in ("user", "chat", "channel"):
        return "%s:%d" % (e["cls"], e["value"])
    return None


def peer_class(b, path):
    cls = b.obj_cls(path)
    for prefix, kind, _ in PEER_KIND:
        if cls and cls.startswith(prefix):
            return kind
    return None


def chat_class(cls):
    if cls is None:
        return None
    for p, k in (("TL_chatForbidden", "chat"), ("TL_chatEmpty", "chat"), ("TL_chat", "chat"),
                 ("TL_channelForbidden", "channel"), ("TL_channel", "channel"), ("TL_community", "community")):
        if cls.startswith(p):
            return k
    return None


def media_type(cls):
    name = re.sub(r"^(TL_messageMedia|MessageMedia)", "", cls).split("_")[0]
    return re.sub(r"(?<!^)([A-Z])", r"_\1", name).lower()


# ---------------------------------------------------------------- the environment (the joins of a live parse)


class Env:
    """The live view of the joined tables; None in the typed recovery pass (4I4-P6: a recovered row joins nothing)."""

    def __init__(self, view):
        self.users = {r["rowid"]: r for r in view.get("users", [])}
        self.chats = {r["rowid"]: r for r in view.get("chats", [])}
        self.contacts = {r["rowid"]: r for r in view.get("contacts", [])}
        self.users_data = {r["rowid"]: r for r in view.get("users_data", [])}
        self.enc = {r["rowid"]: r for r in view.get("enc_chats", [])}


# ---------------------------------------------------------------- the message mapping (spec 8.3-8.8, 9, 10)

PLAIN = (("mid", "mid"), ("uid", "uid"), ("out", "out"), ("read_state", "read_state"), ("send_state", "send_state"),
         ("ttl", "ttl"), ("media", "views_or_media_flag"), ("imp", "imp"), ("mention", "mention"), ("forwards", "forwards"),
         ("thread_reply_id", "thread_reply_id"), ("is_channel", "is_channel"), ("reply_to_message_id", "reply_to_message_id"),
         ("group_id", "group_id"), ("reply_to_story_id", "reply_to_story_id"))


KEY_COLUMNS = ("mid", "uid", "date", "out")  # a NULL there is undecided, never a stored NULL (spec 12; plan 4I4 Task 8)


def int_cell(r, table, col, unknown, nan):
    """(value, readable): an INTEGER cell, None for NULL or absent; a cell of another class (REAL, NaN, TEXT, BLOB)
    is listed in unknown_columns and unreadable, and so is a NULL key column (as "null")."""
    if col not in r:
        return None, True
    v = r[col]
    if (r.get("rowid"), col) in nan:
        unknown[table + "." + col] = "type-mismatch"
        return None, False
    if v is None and col in KEY_COLUMNS:
        unknown[table + "." + col] = "null"
        return None, False
    if v is None or is_int(v):
        return v, True
    unknown[table + "." + col] = "type-mismatch"
    return None, False


def dialog_of(table, uid):
    """(class, basis, enc_chat_id) of a dialog id (4I4-P11, P12, facts.tsv predicates)."""
    if uid is None:
        return "unknown", None, None
    if table == "messages" and uid & 0xFFFFFFFF == 0:  # predicate.pre84_secret_uid (no guard on the high part)
        return "secret", "pre-84-high-id", s32(uid >> 32)
    if uid > 0:
        if uid >> 62 & 1:
            return "secret", None, s32(uid)  # cast.encrypted_chat_id int32
        if uid >> 61 & 1:
            return "folder", None, None
        return "user", None, None
    if uid < 0 and uid != I64_MIN:
        return "negative", None, None
    return "unknown", None, None


def map_message(table, r, env, remote, file, nan=frozenset()):
    """The expected record of one message row (a call for a phone-call action, spec 8.7)."""
    raw, unknown = {"source_table": table}, {}
    cells = {}
    for col, key in PLAIN:
        if col not in r:
            continue
        v, ok = int_cell(r, table, col, unknown, nan)
        cells[col] = (v, ok)
        if ok:
            raw[key] = v
    mid, mid_ok = cells.get("mid", (None, False))
    uid, uid_ok = cells.get("uid", (None, False))
    if env is not None:
        raw["account"] = "0"

    # the date column (spec 10)
    time = None
    date = r.get("date")
    if (r.get("rowid"), "date") in nan or not (date is None or is_int(date)):
        unknown[table + ".date"] = "type-mismatch"
    elif date is None:
        unknown[table + ".date"] = "null"
    else:
        st = C.classify(date, "s")
        if st == "valid":
            time = C.iso(date, "s")
        else:
            raw["date_status"] = st

    # pre-84 conventions (spec 8.3, F10, F32)
    message_id = mid
    if table == "messages" and is_int(mid) and is_int(uid):
        if uid < 0:
            if mid >> 32 == -uid:
                message_id = s32(mid)
                raw["message_id"], raw["message_id_basis"] = message_id, "pre-84-channel-packing"
            else:
                raw["mid_state"] = "undecided"
        ttl = cells.get("ttl", (None, False))[0]
        if is_int(ttl) and ttl < 0:
            raw["ttl_state"] = "pre-84-forward-peer"

    # the message blob (spec 8.4)
    data = r.get("data")
    b = None
    if data is None:
        raw["data_state"] = "null"
    elif is_blob(data):
        b = Blob(data, "Message")
        blob_raw(raw, "data", b, MESSAGE_MAPPED)
    else:
        unknown[table + ".data"] = "type-mismatch"
    readable = b is not None and b.state in ("decoded", "partial")
    cls = b.cls if b is not None else None

    # blob raw (spec 8.5 raw list)
    if b is not None and cls is not None:
        if b.has("flags"):
            raw["blob_flags"] = b.val("flags")
        if b.has("flags2"):
            raw["blob_flags2"] = b.val("flags2")
        for name in MESSAGE_BOOLS:
            if b.flag(name):
                raw[name] = True
        bid = b.val("id")
        if bid is not None:
            raw["id"] = bid
            if is_int(message_id) and bid != message_id:
                raw["id_conflict"] = True
        for key in ("via_bot_id", "grouped_id", "ttl_period", "views"):
            if b.val(key) is not None:
                raw[key] = b.val(key)
        rt = b.val("reply_to.reply_to_msg_id")
        if rt is not None:
            raw["reply_to_msg_id"] = rt
        pa = address(b, "peer_id")
        if pa:
            raw["peer_id"] = pa
        if b.has("fwd_from") or b.has("fwd_from.from_id") or b.has("fwd_from.date"):
            fwd = {}
            fa = address(b, "fwd_from.from_id")
            if fa:
                fwd["from"] = fa
            if b.val("fwd_from.from_name") is not None:
                t = cleaned(raw, "fwd.from_name", b.val("fwd_from.from_name"), MAX_NAME)
                if t:
                    fwd["from_name"] = t
            for k in ("date", "channel_post", "saved_from_msg_id"):
                if b.val("fwd_from." + k) is not None:
                    fwd[k] = b.val("fwd_from." + k)
            if b.val("fwd_from.post_author") is not None:
                t = cleaned(raw, "fwd.post_author", b.val("fwd_from.post_author"), MAX_NAME)
                if t:
                    fwd["post_author"] = t
            sa = address(b, "fwd_from.saved_from_peer")
            if sa:
                fwd["saved_from_peer"] = sa
            raw["fwd"] = fwd
        if b.val("edit_date") is not None:
            raw["edit_date"], raw["edit_date_unit"] = b.val("edit_date"), "unestablished"
        bdate = b.val("date")
        if bdate is not None:
            raw["date"] = bdate
            if is_int(date) and bdate != date:
                raw["date_conflict"] = True
        if cls.startswith("TL_messageEmpty"):
            raw["label_constructor"] = "messageEmpty"

    # direction (spec 8.5)
    out, _ = cells.get("out", (None, False))
    direction = {0: "in", 1: "out"}.get(out, "unknown") if is_int(out) else "unknown"
    if b is not None and cls is not None and direction != "unknown":
        has_out = any(it.name == "out" for it in b.row.items)
        known = b.has("flags") or b.has("out")
        if has_out and known:
            blob_out = b.val("out") is True
            sched = b.val("from_scheduled") is True
            if (direction == "in" and blob_out) or (direction == "out" and not blob_out and not sched):
                direction, raw["direction_conflict"] = "unknown", True

    # status and read (spec 8.5)
    send, _ = cells.get("send_state", (None, False))
    status = None
    if is_int(send):
        if direction == "out":
            status = {0: "sent", 1: "queued", 2: "failed", 3: "unknown"}.get(send, "unknown")
            if send == 1:
                raw["label_send_state"] = "sending"
            if send == 3:
                raw["label_send_state"] = "editing"
        elif direction == "in":
            status = "received" if send == 0 else "unknown"
        else:
            status = "unknown"
    rs, _ = cells.get("read_state", (None, False))
    read = None
    if is_int(rs):
        if 0 <= rs <= 3:
            if direction == "in":
                read = bool(rs & 1)
        else:
            raw["read_state_state"] = "out-of-range"

    # the dialog (spec 8.6, 4I4-P11, P12)
    dclass, basis, enc_id = dialog_of(table, uid if uid_ok and is_int(uid) else None)
    if not uid_ok or uid is None:
        raw["thread_state"] = "undecided"
    if basis:
        raw["dialog_basis"] = basis
    chat_row_cls, chat_title, chat_b = None, None, None
    if dclass == "negative":
        sources = []
        pc = peer_class(b, "peer_id") if b is not None else None
        if pc:
            sources.append(pc)
        isch, _ = cells.get("is_channel", (None, False))
        if is_int(isch) and isch != 0:
            sources.append("channel")
        if env is not None:
            crow = env.chats.get(-uid)
            if crow is not None and is_blob(crow.get("data")):
                chat_b = Blob(crow["data"], "Chat")
                chat_row_cls = chat_class(chat_b.cls)
                if chat_row_cls:
                    sources.append(chat_row_cls)
        if sources:
            if all(s == sources[0] for s in sources):
                dclass = sources[0]
            else:
                raw["dialog_class_state"] = "conflict"
    raw["dialog_class"] = dclass
    if dclass == "secret":
        raw["secret_chat"] = True
        raw["enc_chat_id"] = enc_id

    # joins
    counterparty_user = None
    thread = None
    if uid_ok and is_int(uid):
        thread = {"id": str(uid)}
        if dclass in ("user", "secret"):
            thread["group"] = False
        elif dclass in ("chat", "channel", "community"):
            thread["group"] = True
    if dclass == "user":
        if env is None:
            raw["thread_user_state"] = "not-joined-recovered"
        elif uid not in env.users:
            raw["thread_user_state"] = "no-users-row"
    elif dclass == "secret":
        if env is None:
            raw["counterparty_state"] = "not-joined-recovered"
        else:
            erow = env.enc.get(enc_id)
            if erow is None:
                raw["counterparty_state"] = "no-enc_chats-row"
            elif is_int(erow.get("user")):
                counterparty_user = erow["user"]
    if is_int(uid) and uid < 0 and uid != I64_MIN:
        if env is None:
            raw["thread_title_state"] = "not-joined-recovered"
        else:
            crow = env.chats.get(-uid)
            if crow is None:
                raw["thread_title_state"] = "no-chats-row"
            elif not is_blob(crow.get("data")):
                raw["thread_title_state"] = "blob-undecided"
            else:
                blob_raw(raw, "chat_data", chat_b)
                if chat_row_cls is None or chat_b.val("title") is None:
                    raw["thread_title_state"] = "blob-undecided"
                elif chat_row_cls != dclass:
                    raw["thread_title_state"] = "class-mismatch"
                else:
                    chat_title = cleaned(raw, "thread.title", chat_b.val("title"), MAX_NAME)
                    if chat_title:
                        thread["title"] = chat_title

    # participants (spec 8.6)
    parts, unknown_parts = [], False
    own = None
    if dclass == "user":
        own = "user:%d" % uid
    elif dclass in ("chat", "channel", "community"):
        own = "%s:%d" % (dclass, -uid)
    elif dclass == "secret" and counterparty_user is not None:
        own = "user:%d" % counterparty_user
    if direction == "out":
        if own:
            parts = [{"role": "to", "address": own}]
        else:
            unknown_parts = True
    elif direction == "in":
        if dclass == "user":
            parts = [{"role": "from", "address": own}]
        elif dclass in ("secret", "chat", "channel", "community"):
            fa = address(b, "from_id") if b is not None and cls else None
            if fa:
                parts = [{"role": "from", "address": fa}]
            elif dclass == "channel" and b is not None and b.flag("post") and b.has("flags"):
                parts = [{"role": "from", "address": own}]
                raw["sender_basis"] = "channel-post"
            else:
                unknown_parts = True
                from_read = b is not None and cls is not None and b.has("flags") and (
                    b.state == "decoded" or "from_id" not in b.undecided(MESSAGE_MAPPED))
                raw["sender_state"] = "absent" if from_read else "blob-undecided"
        else:
            unknown_parts = True
    else:
        unknown_parts = True

    # custom_params, replies_data (spec 8.5)
    for col in ("custom_params", "replies_data"):
        v = r.get(col)
        if v is None or col not in r:
            continue
        if not is_blob(v):
            unknown[table + "." + col] = "type-mismatch"
            continue
        raw[col + "_len"], raw[col + "_sha256"] = len(v), sha(v)
        if col == "replies_data" and len(v) >= 4:
            raw[col + "_constructor"] = hx(struct.unpack_from("<I", v)[0])

    # reply copy (spec 8.5)
    rd = r.get("replydata")
    if rd is not None and "replydata" in r:
        if is_blob(rd):
            rb = Blob(rd, "Message")
            blob_raw(raw, "replydata", rb)
            if rb.cls is not None:
                copy = {"copy": True, "constructor": hx(rb.res["top"])}
                if rb.val("id") is not None:
                    copy["id"] = rb.val("id")
                if rb.val("date") is not None:
                    copy["date"] = rb.val("date")
                if rb.val("message") is not None:
                    t = cleaned(raw, "reply_copy.text", rb.val("message"), MAX_SHORT)
                    if t:
                        copy["text"] = t
                raw["reply_copy"] = copy
        else:
            unknown[table + ".replydata"] = "type-mismatch"

    # media summary (spec 8.5)
    attachments = []
    media_known = None  # "empty", "media" or None (undecided)
    if b is not None and cls is not None and not cls.startswith(("TL_messageService", "TL_messageEmpty")):
        if b.has("media"):
            media_known = "empty" if b.obj_id("media") == 0x3DED6320 else "media"
            media = {"type": media_type(b.obj_cls("media")), "constructor": hx(b.obj_id("media"))}
            map_media(b, media, attachments, raw)
            raw["media"] = media
        elif b.stop_at("media"):
            raw["media"] = {"constructor": hx(b.stop_at("media")["id"])}
        else:
            gate = [it for it in b.row.items if it.name == "media"]
            if gate and gate[0].word is not None and b.val(gate[0].word) is not None and not b.val(gate[0].word) >> gate[0].bit & 1:
                media_known = "empty"

    # kind (spec 8.5)
    kind = "unknown"
    call_action = False
    if readable and cls is not None:
        if cls.startswith("TL_messageEmpty"):
            kind = "unknown"
        elif cls.startswith("TL_messageService"):
            if b.obj_id("action") == PHONE_CALL:
                call_action = True
            elif b.has("action") or b.stop_at("action"):
                kind = "system"
        elif media_known == "empty":
            kind = "text"
        elif media_known == "media":
            kind = "media"

    # trailer (spec 8.8)
    if b is not None and cls is not None:
        trailer(raw, b, cls)

    # body (spec 8.5)
    body, text = None, ""
    p = {"channel": "telegram", "direction": direction}
    msg = b.val("message") if b is not None and cls is not None else None
    if msg is not None:
        c = C.clean(msg, MAX_BODY)
        text = body = c.text
        if c.text:
            p["text_source"] = "text"
        if c.flags:
            p["body_flags"] = c.flags
        if c.truncated:
            p["body_truncated"], p["body_total_bytes"] = True, c.total
        if c.b64:
            p["body_raw_b64"], p["body_raw_len"] = c.b64, c.rawlen
        elif c.sha:
            p["body_raw_sha256"], p["body_raw_len"] = c.sha, c.rawlen
    if unknown:
        raw["unknown_columns"] = unknown

    if call_action:
        return map_call(table, r, raw, b, direction, dclass, uid, time, remote, file)

    p["kind"] = kind
    p["participants"] = parts
    if unknown_parts:
        p["participants_unknown"] = True
    if thread is not None:
        p["thread"] = thread
    if status:
        p["status"] = status
    if read is not None:
        p["read"] = read
    if attachments:
        p["attachments"] = attachments
    if mid_ok and is_int(mid):
        p["guid"] = str(mid)
    else:
        raw["guid_state"] = "undecided"
    p["raw"] = raw
    cp = parts[0]["address"] if parts else ""
    channel = "telegram secret" if dclass == "secret" else "telegram"
    return record(table, r, "message", time, None, body, message_summary(channel, direction, cp, text), p, remote, file)


def map_media(b, media, attachments, raw):
    cls = b.obj_cls("media")
    if cls.startswith("TL_messageMediaPhoto"):
        if b.has("media.photo"):
            for k in ("id", "date"):
                if b.val("media.photo." + k) is not None:
                    media[k] = b.val("media.photo." + k)
            if b.has("media.photo.access_hash"):
                media["access_hash_present"] = True
            if b.has("media.photo.file_reference"):
                media["file_reference_present"] = True
    elif cls.startswith("TL_messageMediaDocument"):
        if b.has("media.document"):
            d = "media.document."
            for k in ("id", "date", "size", "dc_id"):
                if b.val(d + k) is not None:
                    media[k] = b.val(d + k)
            mime = b.val(d + "mime_type")
            if mime is not None:
                media["mime_type"] = cleaned(raw, "media.mime_type", mime, MAX_SHORT) or ""
                if not media["mime_type"]:
                    del media["mime_type"]
            for k, key in (("access_hash", "access_hash_present"), ("file_reference", "file_reference_present"), ("key", "key_present")):
                if b.has(d + k):
                    media[key] = True
            att = {}
            if media.get("mime_type"):
                att["mime"] = media["mime_type"]
            if is_int(media.get("size")) and media["size"] >= 0:
                att["size"] = media["size"]
            if att:
                attachments.append(att)
    elif cls.startswith(("TL_messageMediaGeo", "TL_messageMediaVenue")):
        for k, wire, lim in (("lat", "lat", 90), ("long", "_long", 180)):
            v = b.val("media.geo." + wire)
            if v is None:
                continue
            if v == v and abs(v) <= lim and abs(v) != float("inf"):
                media[k] = gofloat(v)
            else:
                media[k + "_state"] = "out-of-range"
        if cls.startswith("TL_messageMediaVenue"):
            for k in ("title", "address"):
                if b.val("media." + k) is not None:
                    t = cleaned(raw, "media." + k, b.val("media." + k), MAX_SHORT)
                    if t:
                        media[k] = t
    elif cls.startswith("TL_messageMediaContact"):
        for k, lim in (("phone_number", MAX_ID), ("first_name", MAX_NAME), ("last_name", MAX_NAME), ("vcard", MAX_VCARD)):
            if b.val("media." + k) is not None:
                t = cleaned(raw, "media." + k, b.val("media." + k), lim)
                if t:
                    media[k] = t
        if b.val("media.user_id") is not None:
            media["user_id"] = b.val("media.user_id")
    elif cls.startswith("TL_messageMediaWebPage"):
        for k in ("url", "display_url"):
            if b.val("media.webpage." + k) is not None:
                t = cleaned(raw, "media." + k, b.val("media.webpage." + k), MAX_SHORT)
                if t:
                    media[k] = t


def trailer(raw, b, cls):
    """raw.trailer_state and its keys (spec 8.8)."""
    if b.state != "decoded":
        raw["trailer_state"] = "unreachable"
        return
    rest = b.data[b.res["end"]:]
    if not rest:
        raw["trailer_state"] = "none"
        return
    got = O.read_string(b.data, b.res["end"])
    fwd = None
    ok = got is not None
    if ok:
        s, after = got
        secret_cls = cls.startswith("TL_message_secret")
        flags, mid = b.val("flags"), b.val("id")
        if not secret_cls and flags is not None and flags & 4 and mid is not None and mid < 0:
            if len(b.data) - after == 4:
                fwd = struct.unpack_from("<i", b.data, after)[0]
                after += 4
            else:
                ok = False
        if after != len(b.data):
            ok = False
    if not ok:
        raw["trailer_state"], raw["trailer_len"], raw["trailer_sha256"] = "unparsed", len(rest), sha(rest)
        return
    raw["trailer_state"] = "read"
    if fwd is not None:
        raw["fwd_msg_id"] = fwd
    if s == b" ":
        raw["attach_path_state"] = "empty-marker"
        return
    path = s
    if s.startswith(b"||"):
        parts = s[2:].split(b"||")
        path, pairs = parts[-1], parts[:-1]
        params = []
        for pr in pairs:
            k, sep, v = pr.partition(b"|=|")
            params.append({"key": C.clean(k, MAX_ID).text, "value": C.clean(v, MAX_ID).text})
        if params:
            raw["attach_params"] = params[:MAX_PARAMS]
            raw["attach_params_total"] = len(params)
            if len(params) > MAX_PARAMS:
                raw["attach_params_truncated"] = True
    if path:
        c = C.clean(path, MAX_SHORT)
        raw["attach_path"] = c.text
        if c.flags:
            raw["attach_path_flags"] = ",".join(c.flags)


def map_call(table, r, raw, b, direction, dclass, uid, time, remote, file):
    """The call record of a phone-call action (spec 8.7)."""
    a = "action."
    flags = b.val(a + "flags")
    if flags is not None:
        raw["flags"] = flags
    if b.val(a + "call_id") is not None:
        raw["call_id"] = b.val(a + "call_id")
    video = b.val(a + "video") is True
    if video:
        raw["video"] = True
    reason = b.obj_id(a + "reason")
    label = REASONS.get(reason)
    if reason is not None:
        raw["reason"] = hx(reason)
    if b.val(a + "reason.slug") is not None:
        raw["slug"] = C.clean(b.val(a + "reason.slug"), MAX_ID).text
    if reason is None:
        outcome = "unknown"
        raw["reason_state"] = "not-recorded"
    elif label == "missed" and direction == "in":
        outcome = "missed"
    elif label == "busy" and direction == "in":
        outcome = "rejected"
    else:
        outcome = "unknown"
        if label:
            raw["label_reason"] = label
    p = {"direction": direction, "outcome": outcome, "kind": "video" if video else "voice", "service": "telegram"}
    duration = b.val(a + "duration")
    time_end = None
    if duration is not None:
        raw["duration"] = duration
        if duration >= 0:
            p["duration_s"] = duration  # facts.tsv unit.call_duration: seconds
        if duration > 0 and time is not None:
            time_end = C.iso(r["date"] + duration, "s")
            raw["time_end_basis"] = "start+duration"
    if dclass == "user":
        p["address"] = "user:%d" % uid
    else:
        raw["address_state"] = "not-user-dialog"
    p["raw"] = raw
    return record(table, r, "call", time, time_end, None, call_summary(direction, outcome, p.get("address", "")), p, remote, file)


def record(table, r, typ, time, time_end, body, summary, payload, remote, file):
    return {"table": table, "rowid": r["rowid"], "file": file, "type": typ, "source_path": remote, "time": time,
            "time_end": time_end, "times": {}, "deleted": False, "summary": summary, "body": body or None, "payload": payload}


# ---------------------------------------------------------------- the contact mapping (spec 8.9)


def map_user(r, env, remote, file):
    raw, unknown = {}, {}
    uid = r.get("rowid")
    if env is not None:
        raw["account"] = "0"
    p = {}
    status = r.get("status")
    if status is not None:
        if is_int(status):
            raw["status"] = status
            if status in STATUS_LABELS:
                raw["status_label"] = STATUS_LABELS[status]
            elif status > 0:
                raw["status_expires"] = status
        else:
            unknown["users.status"] = "type-mismatch"
    data = r.get("data")
    b = None
    if data is None:
        raw["data_state"] = "null"
    elif is_blob(data):
        b = Blob(data, "User")
        blob_raw(raw, "data", b, USER_MAPPED)
    else:
        unknown["users.data"] = "type-mismatch"
    names, phones, ims = {}, [], [{"value": "user:%d" % uid, "kind": "telegram"}]
    username = None
    if b is not None and b.cls is not None:
        for flag, key in USER_BOOLS:
            if b.flag(flag):
                raw[key] = True
        if b.has("access_hash"):
            raw["access_hash_present"] = True
        for k, key in (("first_name", "given"), ("last_name", "family")):
            if b.val(k) is not None:
                t = cleaned(raw, "names." + key, b.val(k), MAX_NAME)
                if t:
                    names[key] = t
        if b.val("phone") is not None:
            ph = ident(raw, "phone", b.val("phone"))
            if ph:
                phones.append(ph)
        if b.val("username") is not None:
            username = ident(raw, "username", b.val("username"))
            if username:
                ims.append({"value": username, "kind": "telegram_username"})
    display = " ".join(x for x in (names.get("given"), names.get("family")) if x) or None
    if display:
        p["display_name"] = display
        raw["display_name_source"] = "composed"
    if names:
        p["names"] = names
    if phones:
        p["phones"] = [{"value": v} for v in phones]
    p["ims"] = ims
    p["source_id"] = str(uid)
    about = None
    if env is None:
        raw["contact_row_state"] = "not-joined-recovered"
        raw["about_state"] = "not-joined-recovered"
    else:
        crow = env.contacts.get(uid)
        if crow is None:
            raw["contact_row"] = False
        else:
            raw["contact_row"] = True
            if is_int(crow.get("mutual")):
                raw["mutual"] = crow["mutual"]
        drow = env.users_data.get(uid)
        if drow is None:
            raw["about_state"] = "no-users_data-row"
        elif drow.get("about") is not None:
            about = cleaned(raw, "about", drow["about"], MAX_BODY)
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    body = "\n".join(x for x in (display, phones[0] if phones else None, username, about) if x)
    rec = record("users", r, "contact", None, None, body, contact_summary(display, phones), p, remote, file)
    rec["_warn"] = not (display or phones or username)
    return rec


def ident(raw, key, v):
    """An identifier as stored, never cut: over MAX_ID it is absent with its state, length and SHA-256."""
    c = C.clean(bytes(v), 1 << 30)
    if len(c.text.encode("utf-8")) > MAX_ID:
        raw[key + "_state"], raw[key + "_len"], raw[key + "_sha256"] = "too-long", len(v), sha(v)
        return None
    if c.flags:
        C.mark(raw, key)
    return c.text or None


# ---------------------------------------------------------------- the expect.json of a variant


def user_version_of(ddl_name):
    with open(os.path.join(SCHEMAS, ddl_name), encoding="utf-8") as f:
        return int(re.search(r"^-- user_version (\d+)$", f.read(), re.M).group(1))


def oracle(files, ddl_name, variant, user_version, refused=False, nan=frozenset()):
    """The <variant>.expect.json content. refused: the parser makes no record of this database (a user version outside
    64-181, or no message table at the app path)."""
    exp = C.expect_header(FAMILY, variant, ddl_name)
    if refused:
        return exp
    view = C.engine_view(files, list(READ_TABLES) + ["sqlite_master"] + [t for t in ROW_NOTE_TABLES if t not in READ_TABLES])
    env = Env(view)
    lone = set(files) == {"db"}
    remote, file = (REMOTE, "db") if lone else ("", "any")
    counts = {}  # (table, col) -> {state: n}
    seen = {}
    secret_dialogs = set()
    for table in ("messages", "messages_v2"):
        for r in view.get(table, []):
            rec = map_message(table, r, env, remote, file, nan)
            exp["records"].append(rec)
            raw = rec["payload"]["raw"]
            for col in ("data", "replydata"):
                st = raw.get(col + "_state")
                if st:
                    counts.setdefault((table, col), {}).setdefault(st, 0)
                    counts[(table, col)][st] += 1
            if is_blob(r.get("data")) and len(r["data"]) >= 4:
                top = struct.unpack_from("<I", r["data"])[0]
                seen[top] = seen.get(top, 0) + 1
            if raw.get("secret_chat"):
                secret_dialogs.add(raw.get("uid"))
    selfs = 0
    for r in view.get("users", []):
        rec = map_user(r, env, remote, file)
        warn = rec.pop("_warn")
        exp["records"].append(rec)
        raw = rec["payload"]["raw"]
        if raw.get("self"):
            selfs += 1
        st = raw.get("data_state")
        if st:
            counts.setdefault(("users", "data"), {}).setdefault(st, 0)
            counts[("users", "data")][st] += 1
        if warn:
            exp["warnings"].append({"table": "users", "rowid": r["rowid"], "contains": "users row has no name, phone or username"})
    notes = exp["notes"]
    notes["database_version"] = str(user_version)
    notes["account"] = "0"
    notes["database_user"] = "0"
    held = [t for t in ("messages", "messages_v2") if view.get(t)]
    notes["generation"] = "both" if len(held) == 2 else (held[0] if held else "none")
    present = sorted({m["name"].decode("utf-8") for m in view["sqlite_master"] if m["type"] == b"table"}, key=lambda n: n.encode("utf-8"))
    for t in ROW_NOTE_TABLES:
        if t in present:
            notes["rows." + t] = str(len(view.get(t, [])))
    states = []
    for (table, col) in [(t, c) for t in ("messages", "messages_v2", "users") for c in ("data", "replydata")]:
        if (table, col) in counts:
            states.append("%s.%s=%s" % (table, col, ",".join("%s:%d" % (s, counts[(table, col)][s]) for s in BLOB_STATES if s in counts[(table, col)])))
    if states:
        notes["blob_states"] = note_list(states, MAX_NOTE, ";")
    if seen:
        order = sorted(seen, key=lambda k: (-seen[k], k))
        entries = ["%s:%d" % (hx(k), seen[k]) for k in order[:64]]
        if len(order) > 64:
            entries.append("other:%d" % sum(seen[k] for k in order[64:]))
        notes["constructors_seen"] = note_list(entries, MAX_SEEN_NOTE)
    notes["secret_chats"] = str(len(secret_dialogs))
    notes["self_users"] = str(selfs)
    unmapped = [t for t in present if t not in READ_TABLES]
    if unmapped:
        notes["unmapped_tables"] = note_list(unmapped)
    # the warnings Parse raises after Run (plan "Notes and warnings")
    if len(held) == 2:
        exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:schema", "contains": "telegram-both-generations: messages=%d messages_v2=%d" % (
            len(view["messages"]), len(view["messages_v2"]))})
    for (table, col) in [(t, c) for t in ("messages", "messages_v2", "users") for c in ("data", "replydata")]:
        cnt = counts.get((table, col), {})
        bad = [(s, cnt[s]) for s in COUNTED_STATES if cnt.get(s)]
        if bad:
            exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:table=" + table,
                                    "contains": "telegram-blob-undecoded: %s.%s: %d records (%s)" % (
                                        table, col, sum(n for _, n in bad), ",".join("%s:%d" % x for x in bad))})
    return exp


# ---------------------------------------------------------------- the scenario


def pu(i):
    return {"_": "TL_peerUser", "user_id": i}


def pc(i):
    return {"_": "TL_peerChat", "chat_id": i}


def pch(i):
    return {"_": "TL_peerChannel", "channel_id": i}


def tlmsg(mid, rid, text, peer=None, out=False, frm=None, **kw):
    v = {"_": "TL_message", "id": mid, "peer_id": peer or pu(SELF), "date": D + rid, "message": text}
    if out:
        v["out"] = True
    if frm is not None:
        v["from_id"] = frm
    v.update(kw)
    return v


def enc(value, typ="Message"):
    return E.encode(typ, value)


def call_action(call_id, reason=None, duration=None, video=False, slug=None):
    a = {"_": "TL_messageActionPhoneCall", "call_id": call_id}
    if video:
        a["video"] = True
    if reason:
        a["reason"] = {"_": reason}
        if slug is not None:
            a["reason"]["slug"] = slug
    if duration is not None:
        a["duration"] = duration
    return a


def service(mid, rid, action, peer=None, out=False, frm=None):
    v = {"_": "TL_messageService", "id": mid, "peer_id": peer or pu(SELF), "date": D + rid, "action": action}
    if out:
        v["out"] = True
    if frm is not None:
        v["from_id"] = frm
    return v


def photo(sizes=()):
    return {"_": "TL_messageMediaPhoto", "photo": {"_": "TL_photo", "id": 9001, "access_hash": 5, "file_reference": secret("4i4-fileref", 24),
                                                   "date": D, "sizes": list(sizes), "dc_id": 2}}


def enc_document(attributes):
    return {"_": "TL_messageMediaDocument", "document": {
        "_": "TL_documentEncrypted", "id": 7001, "access_hash": 7, "date": D, "mime_type": "image/jpeg", "size": 2048,
        # a boxed PhotoSize the table does not hold (Task 5 ruling on F22): the decode stops here, before key and iv
        "thumbs": {"_raw": u32le(0x0BAD0005) + b"\x01\x00\x00\x00"},
        "dc_id": 1, "attributes": attributes, "key": secret("4i4-dockey", 32), "iv": secret("4i4-dociv", 32)}}


EMPTY_MEDIA = {"_": "TL_messageMediaEmpty"}


def row(rid, mid, uid, data, out=0, **cols):
    r = {"rowid": rid, "mid": mid, "uid": uid, "read_state": 0 if out else 3, "send_state": 0, "date": D + rid, "data": data,
         "out": out, "ttl": 0, "media": -1, "replydata": None, "imp": 0, "mention": 0, "forwards": 0, "replies_data": None,
         "thread_reply_id": 0, "is_channel": 0, "reply_to_message_id": 0, "custom_params": None, "group_id": None,
         "reply_to_story_id": 0}
    r.update(cols)
    return r


def core_messages():
    """R1-R59 of the plan's CORE table (R51, R52 are written to the WAL by variant_v181)."""
    R = {}
    R[1] = row(1, 101, ANN, enc(tlmsg(101, 1, "Hello Ann here", frm=pu(ANN))))
    R[2] = row(2, 102, ANN, enc(tlmsg(102, 2, "Hi Ann", peer=pu(ANN), out=True)), out=1)
    R[3] = row(3, -1, ANN, enc(tlmsg(-1, 3, "Forwarded", peer=pu(ANN), out=True, fwd_from={"_": "TL_messageFwdHeader", "from_id": pu(BOT), "date": D - 60}))
               + E.trailer("/sdcard/x.jpg", [("a", "1"), ("b", "2")], 55), out=1, send_state=1)
    R[4] = row(4, 103, -2000001, enc(tlmsg(103, 4, "Group hello", peer=pc(2000001), frm=pu(BOT))))
    R[5] = row(5, 104, -3000001, enc(tlmsg(104, 5, "News post", peer=pch(3000001), post=True, views=42, forwards=3)), is_channel=3000001, media=42)
    R[6] = row(6, 105, -3000002, enc(tlmsg(105, 6, "Club post", peer=pch(3000002), frm=pu(ANN))), is_channel=3000002)
    R[7] = row(7, 106, -4000001, u32le(0x0BADF00D) + b"\x00" * 12)
    R[8] = row(8, -7, S77, enc({"_": "TL_message_secret", "id": -7, "ttl": 30, "from_id": ANN, "peer_id": pu(SELF), "date": D + 8,
                                "message": "secret hi", "media": EMPTY_MEDIA, "entities": []}), ttl=30)
    R[9] = row(9, -8, S77, enc({"_": "TL_message_secret_layer72", "out": True, "id": -8, "ttl": 0, "from_id": SELF, "peer_id": pu(ANN),
                                "date": D + 9, "message": "", "media": enc_document([]), "entities": []}), out=1)
    R[10] = row(10, -9, S5, enc({"_": "TL_message_secret", "out": True, "id": -9, "ttl": 0, "from_id": SELF, "peer_id": pu(1000099),
                                 "date": D + 10, "message": "sent", "media": EMPTY_MEDIA, "entities": []}), out=1)
    R[11] = row(11, 107, ANN, enc(service(107, 11, call_action(9101, "TL_phoneCallDiscardReasonMissed"), frm=pu(ANN))))
    R[12] = row(12, 108, ANN, enc(service(108, 12, call_action(9102, "TL_phoneCallDiscardReasonHangup", 125, video=True), peer=pu(ANN), out=True)), out=1)
    R[13] = row(13, 109, ANN, enc(service(109, 13, call_action(9103, "TL_phoneCallDiscardReasonBusy"), frm=pu(ANN))))
    R[14] = row(14, 110, ANN, enc(service(110, 14, call_action(9104, None, 60), frm=pu(ANN))))
    R[15] = row(15, 111, ANN, enc(service(111, 15, call_action(9105, "TL_phoneCallDiscardReasonDisconnect"), frm=pu(ANN))))
    R[16] = row(16, 112, ANN, enc(service(112, 16, call_action(9106, "TL_phoneCallDiscardReasonMigrateConferenceCall", slug="conf-1"), frm=pu(ANN))))
    R[17] = row(17, 113, -2000001, enc(service(113, 17, call_action(9107, "TL_phoneCallDiscardReasonMissed"), peer=pc(2000001), frm=pu(ANN))))
    R[18] = row(18, 114, ANN, enc(service(114, 18, {"_raw": u32le(0x2FFE2F7A) + b"\x00" * 8}, frm=pu(ANN))))
    R[19] = row(19, 115, -2000001, enc(service(115, 19, {"_raw": u32le(0x0BAD0001) + b"\x00" * 4}, peer=pc(2000001), frm=pu(BOT))))
    R[20] = row(20, 116, ANN, enc({"_": "TL_messageEmpty", "id": 116}))
    R[21] = row(21, 117, ANN, enc(tlmsg(117, 21, "", frm=pu(ANN), media=photo())))
    R[22] = row(22, 118, ANN, enc(tlmsg(118, 22, "", frm=pu(ANN), media=photo([{"_raw": u32le(0x0BAD0002) + b"\x00" * 8}]))))
    R[23] = row(23, 119, ANN, enc(tlmsg(119, 23, "", frm=pu(ANN), media={"_": "TL_messageMediaDocument", "document": {
        "_": "TL_document", "id": 9002, "access_hash": 6, "file_reference": secret("4i4-docref", 24), "date": D, "mime_type": "application/pdf",
        "size": 12345, "dc_id": 2, "attributes": [{"_raw": u32le(0x0BAD0003) + b"\x00" * 4}]}})))
    geo = lambda lat: {"_": "TL_geoPoint", "_long": 13.405, "lat": lat, "access_hash": 0}
    R[24] = row(24, 120, ANN, enc(tlmsg(120, 24, "", frm=pu(ANN), media={"_": "TL_messageMediaGeo", "geo": geo(52.52)})))
    R[25] = row(25, 121, ANN, enc(tlmsg(121, 25, "", frm=pu(ANN), media={"_": "TL_messageMediaGeo", "geo": geo(91.0)})))
    R[26] = row(26, 122, ANN, enc(tlmsg(122, 26, "", frm=pu(ANN), media={
        "_": "TL_messageMediaContact", "phone_number": "+15550000009", "first_name": "Cee", "last_name": "Contact",
        "vcard": "BEGIN:VCARD\nVERSION:3.0\nFN:Cee Contact\nTEL:+15550000009\nEND:VCARD", "user_id": ANN})))
    R[27] = row(27, 123, ANN, enc(tlmsg(123, 27, "", frm=pu(ANN), media={
        "_": "TL_messageMediaVenue", "geo": geo(52.52), "title": "Office", "address": "1 Main St", "provider": "", "venue_id": "", "venue_type": ""})))
    R[28] = row(28, 124, ANN, enc(tlmsg(124, 28, "", frm=pu(ANN), media={"_": "TL_messageMediaWebPage", "webpage": {
        "_": "TL_webPage", "id": 9003, "url": "https://example.test/a", "display_url": "example.test/a", "hash": 0}})))
    R[29] = row(29, 125, ANN, enc(tlmsg(125, 29, "", frm=pu(ANN), media={"_": "TL_messageMediaDice", "_body": b"\x03\x00\x00\x00\x02\xf0\x9f\x8e\xb2\x00"})))
    R[30] = row(30, 126, ANN, enc(tlmsg(126, 30, "Reply", frm=pu(ANN), reply_to={"_": "TL_messageReplyHeader", "reply_to_msg_id": 101})),
                replydata=R[1]["data"], reply_to_message_id=101)
    bold_url = [{"_": "TL_messageEntityBold", "offset": 0, "length": 4}, {"_": "TL_messageEntityUrl", "offset": 5, "length": 4}]
    R[31] = row(31, 127, ANN, enc(tlmsg(127, 31, "Bold text", frm=pu(ANN), entities=bold_url)))
    R[32] = row(32, 128, ANN, enc(tlmsg(128, 32, "Quoting", frm=pu(ANN), reply_to={
        "_": "TL_messageReplyHeader", "quote": True, "reply_to_msg_id": 101, "quote_text": "Hello",
        "quote_entities": [{"_": "TL_messageEntityBold", "offset": 0, "length": 5}], "quote_offset": 0})), reply_to_message_id=101)
    R[33] = row(33, 129, ANN, enc(tlmsg(129, 33, "Edited", frm=pu(ANN), edit_date=D + 500)))
    R[34] = row(34, 130, ANN, enc(tlmsg(130, 34, "Out in the blob", out=True, frm=pu(ANN))))
    R[35] = row(35, 131, ANN, enc(dict(tlmsg(999, 35, "Other date", frm=pu(ANN)), date=D + 9999)))
    R[36] = row(36, 132, ANN, enc(tlmsg(132, 36, "Failed", peer=pu(ANN), out=True)), out=1, send_state=2)
    R[37] = row(37, 133, ANN, enc(tlmsg(133, 37, "Editing", peer=pu(ANN), out=True)), out=1, send_state=3)
    R[38] = row(38, 134, ANN, enc(tlmsg(134, 38, "Media read only", frm=pu(ANN))), read_state=2)
    R[39] = row(39, 135, ANN, enc(tlmsg(135, 39, "Odd read state", frm=pu(ANN))), read_state=7)
    R[40] = row(40, 136, ANN, enc(tlmsg(136, 40, "Odd send state", frm=pu(ANN))), send_state=1)
    R[41] = row(41, 137, ANN, enc(tlmsg(137, 41, "Empty path", peer=pu(ANN), out=True)) + E.trailer("", None, None), out=1)
    R[42] = row(42, 138, ANN, enc(tlmsg(138, 42, "Odd trailer", frm=pu(ANN))) + b"ab")
    R[43] = row(43, 139, ANN, enc(tlmsg(139, 43, "No trailer", frm=pu(ANN))))
    R[44] = row(44, 101, BOT, enc(tlmsg(101, 44, "same mid", frm=pu(BOT))))
    R[45] = row(45, 141, SELF, enc(tlmsg(141, 45, "note to self", peer=pu(SELF), out=True)), out=1)
    R[46] = row(46, 142, SELF, enc(tlmsg(142, 46, "forwarded to self", frm=pu(SELF), fwd_from={"_": "TL_messageFwdHeader", "from_id": pu(ANN), "date": D - 100})))
    R[47] = row(47, 143, -3000003, enc(tlmsg(143, 47, "mismatch", peer=pc(3000003))))
    R[48] = row(48, 144, FOLDER1, enc(tlmsg(144, 48, "folder")))
    R[49] = row(49, 145, 1000099, enc(tlmsg(145, 49, "orphan", frm=pu(1000099))))
    R[50] = row(50, 146, ANN, None)
    R[51] = row(51, 147, ANN, enc(tlmsg(147, 51, "in the WAL, committed", frm=pu(ANN))))
    R[52] = row(52, 148, ANN, enc(tlmsg(148, 52, "in the WAL, uncommitted R52", frm=pu(ANN))))
    R[53] = row(53, 149, ANN, enc(tlmsg(149, 53, "long " + "x" * (300 << 10), frm=pu(ANN))))
    R[54] = row(54, 150, ANN, enc(tlmsg(150, 54, b"nul\x00and\xffbyte", frm=pu(ANN))))
    R[55] = row(55, 151, ANN, enc(tlmsg(151, 55, "params", frm=pu(ANN))), custom_params=struct.pack("<ii", 1, 0) + b"\x01" * 8,
                replies_data=u32le(0x0BAD0004) + b"\x02" * 8)
    R[56] = row(56, 152, ANN, enc(tlmsg(152, 56, "", frm=pu(ANN), media=photo(), grouped_id=777)), group_id=777)
    R[57] = row(57, -10, S77, enc({"_": "TL_message_secret_old", "id": -10, "ttl": 0, "from_id": ANN, "peer_id": pu(SELF), "date": D + 57,
                                   "message": "old secret hi", "media": EMPTY_MEDIA}))
    # an app-mutated flags word: bit 12 gates nothing in TL_message (recon 2 item 6; F33)
    R[58] = row(58, 153, ANN, enc(tlmsg(153, 58, "Flagged", flags=1 << 12, flags2=0)))
    R[59] = row(59, -11, S77, enc({"_": "TL_message_secret_layer72", "out": True, "id": -11, "ttl": 0, "from_id": SELF, "peer_id": pu(ANN),
                                   "date": D + 59, "message": "", "media": enc_document([{"_raw": u32le(0x0BAD0006)}]), "entities": []}), out=1)
    return R


def user_rows():
    def u(uid, value, name="", status=0):
        return {"uid": uid, "name": name, "status": status, "data": value if isinstance(value, bytes) else E.encode("User", value)}
    tu = lambda **kw: dict({"_": "TL_user", "access_hash": 1}, **kw)
    return {
        1: u(SELF, tu(self=True, id=SELF, first_name="Owner", last_name="Phone", username="owner_x", phone="15550000001"), "owner phone;;;owner_x"),
        2: u(ANN, tu(contact=True, mutual_contact=True, id=ANN, first_name="Ann", last_name="Example", username="ann_example", phone="15550000002"),
             "ann example;;;ann_example", D + 3600),
        3: u(BOT, tu(bot=True, id=BOT, first_name="Helper Bot", username="helper_bot"), "helper bot;;;helper_bot", -100),
        4: u(1000004, tu(min=True, id=1000004, first_name="Min"), "min"),
        5: u(1000005, tu(deleted=True, id=1000005)),
        6: u(1000006, {"_": "TL_userEmpty", "id": 1000006}),
        7: u(1000007, tu(id=1000007, first_name="", last_name="Solo", username=""), "solo"),
        8: u(1000008, {"_": "TL_user_layer65", "id": 1000008, "access_hash": 1, "first_name": "Old", "last_name": "Timer", "phone": "15550000008"}, "old timer"),
        9: u(1000009, u32le(0x0BADF00D) + b"\x00" * 8),
        10: u(1000010, tu(id=1000010, first_name="Dee"), "dee", -1001),
    }


def chat_rows():
    ch = lambda cid, title, **kw: dict({"_": "TL_channel", "id": cid, "access_hash": 1, "title": title}, **kw)
    return {
        1: {"uid": 2000001, "name": "family", "data": E.encode("Chat", {"_": "TL_chat", "flags": 131072, "id": 2000001, "title": "Family"})},
        2: {"uid": 3000001, "name": "news", "data": E.encode("Chat", ch(3000001, "News", broadcast=True, username="news_x"))},
        3: {"uid": 3000002, "name": "club", "data": E.encode("Chat", ch(3000002, "Club", megagroup=True))},
        4: {"uid": 4000001, "name": "hood", "data": E.encode("Chat", {"_": "TL_community", "id": 4000001, "title": "Hood"})},
        5: {"uid": 3000003, "name": "mismatch", "data": E.encode("Chat", ch(3000003, "Mismatch"))},
    }


def enc_chat_rows():
    def e(uid, user):
        return {"uid": uid, "user": user, "name": "", "data": chain("4i4-encdata", 64), "g": chain("4i4-g", 256),
                "authkey": chain("4i4-authkey", 256), "ttl": 0, "layer": 73, "seq_in": 0, "seq_out": 0, "use_count": 0,
                "exchange_id": 0, "key_date": D, "fprint": 0x0123456789ABCDEF, "fauthkey": chain("4i4-fauthkey", 256),
                "khash": chain("4i4-khash", 32), "in_seq_no": 0, "admin_id": SELF, "mtproto_seq": 0}
    return [e(77, ANN), e(-5, 1000099)]


def pre84_rows():
    """P1-P6 of the plan (messages, v64): rowid = mid."""
    P = {}

    def m(mid, uid, data, out=0, **cols):
        r = {"mid": mid, "uid": uid, "read_state": 0 if out else 3, "send_state": 0, "date": D + 1, "data": data, "out": out,
             "ttl": 0, "media": -1, "replydata": None, "imp": 0, "mention": 0}
        r.update(cols)
        return r
    l104 = lambda mid, text, peer, **kw: dict({"_": "TL_message_layer104", "id": mid, "peer_id": peer, "date": D + 1, "message": text}, **kw)
    P[1] = m(44 | 3000001 << 32, -3000001, enc(l104(44, "Old channel post", {"_": "TL_peerChannel_layer131", "channel_id": 3000001}, post=True)))
    P[2] = m(-7, 77 << 32, enc({"_": "TL_message_secret", "id": -7, "ttl": 0, "from_id": ANN, "peer_id": {"_": "TL_peerUser_layer131", "user_id": SELF},
                                "date": D + 1, "message": "old secret", "media": EMPTY_MEDIA, "entities": []}))
    P[3] = m(47, ANN, enc(l104(47, "Forward peer", {"_": "TL_peerUser_layer131", "user_id": SELF}, from_id=ANN)), ttl=-1000002)
    P[4] = m(45 | 3000002 << 32, -3000001, enc(l104(45, "Undecided mid", {"_": "TL_peerChannel_layer131", "channel_id": 3000001}, post=True)))
    # F5: R1's values in the old generation
    P[5] = m(101, ANN, enc(l104(101, "Hello Ann here", {"_": "TL_peerUser_layer131", "user_id": SELF}, from_id=ANN)))
    P[6] = m(107, ANN, enc({"_": "TL_messageService_layer118", "id": 107, "from_id": ANN, "peer_id": {"_": "TL_peerUser_layer131", "user_id": SELF},
                            "date": D + 1, "action": call_action(9201, "TL_phoneCallDiscardReasonMissed")}))
    return P


def v84_rows():
    """R1, R2, R4 as the 8.1.1 app wrote them: TL_message_layer123 blobs with layer-131 peers."""
    l123 = lambda mid, rid, text, peer, **kw: dict({"_": "TL_message_layer123", "id": mid, "peer_id": peer, "date": D + rid, "message": text}, **kw)
    lu = lambda i: {"_": "TL_peerUser_layer131", "user_id": i}
    return {1: row(1, 101, ANN, enc(l123(101, 1, "Hello Ann here", lu(SELF), from_id=lu(ANN)))),
            2: row(2, 102, ANN, enc(l123(102, 2, "Hi Ann", lu(ANN), out=True)), out=1),
            4: row(4, 103, -2000001, enc(l123(103, 4, "Group hello", {"_": "TL_peerChat_layer131", "chat_id": 2000001}, from_id=lu(BOT))))}


# ---------------------------------------------------------------- layers and hostile rows


def mandatory(r, typ, i, text):
    """A value of row r with only its ungated fields (a flags word computed), the text given where it has message."""
    v = {"_": r.cls}
    for it in r.items:
        if it.kind is None or it.word is not None or it.kind == "#":
            continue
        name = it.name
        k = it.kind
        if name in v:
            continue
        if name == "message":
            v[name] = text
        elif name == "media":
            v[name] = EMPTY_MEDIA
        elif name == "action":
            v[name] = call_action(9300 + i)
        elif k in ("str", "bytes"):
            v[name] = r.cls if name in ("first_name", "title") else ""
        elif k == "secret":
            v[name] = secret("4i4-layers", 8)
        elif k == "bool":
            v[name] = False
        elif k in ("i32", "i64") or k.startswith(("id32@", "id64@")):
            v[name] = {"id": i, "date": D, "user_id": ANN, "chat_id": 2000001, "channel_id": 3000001}.get(name.split(".")[-1], 0)
            if name.split(".")[-1] == "from_id":
                v[name] = ANN
        elif k == "dbl":
            v[name] = 0.0
        elif k.startswith("vec<"):
            v[name] = []
        elif k == "Peer":
            v[name] = pu(SELF) if name == "peer_id" else pu(ANN)
        elif k == "GeoPoint":
            v[name] = {"_": "TL_geoPointEmpty"}
        elif k == "WebPage":
            v[name] = {"_": "TL_webPageEmpty"}
        else:
            v[name] = {"_raw": u32le(0x0BAD0100)}
    return v


def layers_rows():
    rows, users, chats = [], [], []
    tab = O.table()
    ordered = [tab[k] for k in sorted(tab, key=lambda k: list(tab).index(k))]
    msgs = [r for r in ordered if r.typ == "Message"]
    for i, r in enumerate(msgs):
        rid = 1001 + i
        v = mandatory(r, "Message", rid, "layer " + r.cls)
        rows.append(row(rid, rid, ANN, E.encode("Message", v)))
    for i, r in enumerate([r for r in ordered if r.typ == "MessageFwdHeader"]):
        rid = 1101 + i
        rows.append(row(rid, rid, ANN, enc(tlmsg(rid, rid, "fwd " + r.cls, frm=pu(ANN), fwd_from={"_": r.cls, "date": D}))))
    for i, r in enumerate([r for r in ordered if r.typ == "Peer"]):
        rid = 1201 + i
        fid = {"user_id": ANN, "chat_id": 2000001, "channel_id": 3000001}[r.items[0].name]
        rows.append(row(rid, rid, -2000001, enc(tlmsg(rid, rid, "peer " + r.cls, peer=pc(2000001), frm={"_": r.cls, r.items[0].name: fid}))))
    for i, r in enumerate([r for r in ordered if r.typ == "MessageReplyHeader"]):
        rid = 1301 + i
        rows.append(row(rid, rid, ANN, enc(tlmsg(rid, rid, "reply " + r.cls, frm=pu(ANN), reply_to=mandatory(r, "MessageReplyHeader", 101, "")))))
    for i, r in enumerate([r for r in ordered if r.typ == "MessageEntity"]):
        rid = 1401 + i
        ent = mandatory(r, "MessageEntity", 0, "")
        ent.update({k: 1 for k in ("length",) if k in ent})
        rows.append(row(rid, rid, ANN, enc(tlmsg(rid, rid, "entity " + r.cls, frm=pu(ANN), entities=[ent]))))
    # every full media wrapper (TL_messageMediaEmpty and TL_messageMediaUnsupported among them since ruling M3: a
    # constructor that reads no field is full) and one opaque one, TL_messageMediaUnsupported_old
    media = [r for r in ordered if r.typ == "MessageMedia" and r.body == "full"] + [tab[0x29632A36]]
    for i, r in enumerate(media):
        rid = 1501 + i
        rows.append(row(rid, rid, ANN, enc(tlmsg(rid, rid, "media " + r.cls, frm=pu(ANN), media=mandatory(r, "MessageMedia", rid, "")))))
    for i, r in enumerate([r for r in ordered if r.typ == "User"]):
        uid = 5000001 + i
        v = mandatory(r, "User", uid, "")
        v["id"] = uid if "id" in v else v.get("id")
        users.append({"uid": uid, "name": "", "status": 0, "data": E.encode("User", {k: x for k, x in v.items() if x is not None})})
    for i, r in enumerate([r for r in ordered if r.typ == "Chat"]):
        cid = 6000001 + i
        v = mandatory(r, "Chat", cid, "")
        if "id" in v:
            v["id"] = cid
        chats.append({"uid": cid, "name": "", "data": E.encode("Chat", v)})
    return rows, users, chats


NAN_MARK = 1.2345678901234567e300
NAN_BITS = struct.pack(">Q", 0x7FF8000000000001)


def hostile_rows():
    """H1-H12 of the plan; rowids from 2001. Returns (rows, [(rowid, column) that hold NaN])."""
    out = []
    base = enc(tlmsg(2000, 0, "Hello Ann here", frm=pu(ANN)))
    rid = [2000]

    def h(data, **cols):
        rid[0] += 1
        r = row(rid[0], rid[0], ANN, data)
        r.update(cols)
        out.append(r)
        return r
    h(u32le(0x0BADF00D) + b"\x00" * 8)
    v31 = tlmsg(127, 31, "Bold text", frm=pu(ANN), entities=[{"_": "TL_messageEntityBold", "offset": 0, "length": 4},
                                                           {"_": "TL_messageEntityUrl", "offset": 5, "length": 4}])
    full = enc(v31)
    for _, off in E.field_offsets("Message", v31)[:-1]:
        h(full[:off])
    h(enc({"_": "TL_message_old", "id": 3, "from_id": ANN, "peer_id": pu(SELF), "out": {"_raw": u32le(0x12345678)}, "unread": False,
           "date": D, "message": "bad bool", "media": EMPTY_MEDIA}))
    h(enc(tlmsg(4, 0, "bad vector", frm=pu(ANN), entities={"_raw": u32le(0x1CB5C416) + u32le(0)})))
    h(enc(tlmsg(5, 0, "vector count", frm=pu(ANN), entities={"_raw": u32le(E.VECTOR_ID) + u32le(0x7FFFFFFF)})))
    h(enc(tlmsg(6, 0, {"_raw": b"\xfe\xff\xff\xff"}, frm=pu(ANN))))
    h(enc(tlmsg(7, 0, {"_raw": b"\xff\x03\x00\x00abc\x00"}, frm=pu(ANN))))
    h(enc(tlmsg(8, 0, {"_raw": b"\x02ab\x07"}, frm=pu(ANN))))
    h(base, uid=None)
    h(base, uid=1.5)
    nan_row = h(base, uid=NAN_MARK)
    h(base, uid=C.Raw(b"1000002"))
    h(base, mid=None)
    h(base, date=float(D) + 0.5)
    h(C.Raw(b"text in a blob column"))
    h(base, out=7)
    return out, frozenset({(nan_row["rowid"], "uid")})


# ---------------------------------------------------------------- writing


def connect(path):
    con = sqlite3.connect(path, isolation_level=None)
    con.execute("pragma page_size = 4096")
    return con


def create_db(path, ddl_name, user_version, only=None):
    con = connect(path)
    sql = C.load_ddl(ddl_name, SCHEMAS)[0]
    stmts = [l for l in sql.splitlines() if l.startswith("CREATE TABLE ")]
    if only is not None:
        stmts = [s for s in stmts if s.split()[2] in only]
    for s in stmts:
        con.execute(s)
    con.execute("pragma user_version = %d" % user_version)
    con.execute("pragma secure_delete = ON")  # as the app (MS:327)
    con.execute("pragma journal_mode = wal")  # as the app (MS:329)
    return con


def lit(v):
    if isinstance(v, (bytes, bytearray)) and not isinstance(v, C.Raw):
        return "x'%s'" % bytes(v).hex()
    if isinstance(v, float):
        return repr(v)
    return C.lit(v)


def insert(con, table, r, rowid=True):
    cols = [c[1] for c in con.execute("pragma table_info(%s)" % table)]
    keys = [k for k in r if k in cols]
    if rowid and "rowid" in r and table in ("messages_v2",):
        keys = ["rowid"] + keys
    vals = [r[k].b if isinstance(r[k], C.Raw) else r[k] for k in keys]
    marks = ["cast(? as text)" if isinstance(r[k], C.Raw) else "?" for k in keys]
    con.execute("insert into %s(%s) values (%s)" % (table, ",".join(keys), ",".join(marks)), vals)


def insert_sql(table, r, cols, verb="insert"):
    keys = [k for k in r if k in cols and k != "rowid"]
    if "rowid" in r and verb == "insert" and table == "messages_v2":
        keys = ["rowid"] + keys
    return "%s into %s(%s) values (%s)" % (verb, table, ",".join(keys), ",".join(lit(r[k]) for k in keys))


def fill(con, msgs=(), users=(), chats=(), enc=(), contacts=(), users_data=(), table="messages_v2"):
    with C.batch(con):
        for r in msgs:
            insert(con, table, r)
        for r in users:
            insert(con, "users", r)
        for r in chats:
            insert(con, "chats", r)
        for r in enc:
            insert(con, "enc_chats", r)
        for uid, mutual in contacts:
            con.execute("insert into contacts(uid, mutual) values (?, ?)", (uid, mutual))
        for uid, about in users_data:
            con.execute("insert into users_data(uid, about) values (?, ?)", (uid, about))


CONTACTS = [(ANN, 1), (BOT, 0), (1000008, 1)]
USERS_DATA = [(ANN, "hi there")]


def finish(con, path):
    """Closes the last connection: SQLite checkpoints the WAL into the file and removes it (a variant without -wal)."""
    con.execute("pragma wal_checkpoint(truncate)")
    con.close()
    if os.path.exists(path + "-wal"):
        raise SystemExit("%s: the WAL was not checkpointed" % path)
    return {"db": C.read(path)}


def variant_v181(work):
    """CORE R1-R59 but R51/R52 in the file; R51 in a committed WAL frame; R52 in a frame the writer never commits."""
    path = os.path.join(work, "cache4.db")
    con = create_db(path, "v181.sql", 181)
    R = core_messages()
    fill(con, [R[k] for k in sorted(R) if k not in (51, 52)], user_rows().values(), chat_rows().values(), enc_chat_rows(), CONTACTS, USERS_DATA)
    con.execute("pragma wal_checkpoint(truncate)")
    con.close()
    cols = list(R[51])
    stmts = ["pragma secure_delete = ON", "pragma wal_autocheckpoint = 0", insert_sql("messages_v2", R[51], cols),
             "pragma cache_size = 1", "pragma cache_spill = 1", "BEGIN", insert_sql("messages_v2", R[52], cols),
             "update messages_v2 set custom_params = zeroblob(40000) where rowid = 52"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    if b"uncommitted R52" not in files["wal"]:
        raise SystemExit("telegram-v181: R52 did not reach the WAL (no spill)")
    exp = oracle(files, "v181.sql", "telegram-v181", 181)
    exp["warnings"].insert(0, {"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
    return files, exp


def simple(work, variant, ddl, version, msgs=(), users=(), chats=(), enc=(), contacts=(), users_data=(), table="messages_v2",
           refused=False, only=None, oracle_ddl=None):
    path = os.path.join(work, "cache4.db")
    con = create_db(path, ddl, version, only)
    fill(con, msgs, users, chats, enc, contacts, users_data, table)
    files = finish(con, path)
    return files, oracle(files, oracle_ddl or ddl, variant, version, refused)


def pick(d, keys):
    return [d[k] for k in keys]


def variant_v159(work):
    R, U, Ch = core_messages(), user_rows(), chat_rows()
    return simple(work, "telegram-v159", "v159.sql", 159, pick(R, (1, 2, 4, 5, 11)), pick(U, (1, 2)), pick(Ch, (1, 2)))


def variant_v84(work):
    U = user_rows()
    return simple(work, "telegram-v84", "v84.sql", 84, v84_rows().values(), pick(U, (1, 2, 8)))


def variant_upgraded(work):
    """R1, R2 written at version 84 (short records), then the ALTERs of DatabaseMigrationHelper, then R4 at 181."""
    path = os.path.join(work, "cache4.db")
    sql = C.load_ddl("v84-upgraded-v181.sql", SCHEMAS)[0]
    con = connect(path)
    for s in sql.splitlines():
        if s.startswith("CREATE TABLE "):
            con.execute(s)
    con.execute("pragma user_version = 84")
    con.execute("pragma secure_delete = ON")
    con.execute("pragma journal_mode = wal")
    R = core_messages()
    fill(con, pick(R, (1, 2)))
    for s in sql.splitlines():
        if s.startswith("ALTER TABLE "):
            con.execute(s)
    con.execute("pragma user_version = 181")
    fill(con, [R[4]])
    files = finish(con, path)
    return files, oracle(files, "v84-upgraded-v181.sql", "telegram-v84-upgraded-v181", 181)


def variant_v64(work):
    U, Ch = user_rows(), chat_rows()
    return simple(work, "telegram-v64", "v64.sql", 64, pre84_rows().values(), pick(U, (1, 2, 8)), pick(Ch, (2,)), table="messages")


def variant_both(work):
    """v181.sql plus v64's messages table, user version 181: messages P5, P3; messages_v2 R1, R2 (F5)."""
    path = os.path.join(work, "cache4.db")
    con = create_db(path, "v181.sql", 181)
    old = [l for l in C.load_ddl("v64.sql", SCHEMAS)[0].splitlines() if l.startswith("CREATE TABLE messages ")]
    con.execute(old[0])
    P, R = pre84_rows(), core_messages()
    fill(con, pick(P, (5, 3)), table="messages")
    fill(con, pick(R, (1, 2)))
    files = finish(con, path)
    return files, oracle(files, "v181.sql", "telegram-both", 181)


def variant_otherversion(work):
    return simple(work, "telegram-otherversion", "v181.sql", 182, pick(core_messages(), (1,)), pick(user_rows(), (1,)), refused=True)


def variant_oldversion(work):
    return simple(work, "telegram-oldversion", "v64.sql", 63, pick(pre84_rows(), (5,)), pick(user_rows(), (1,)), table="messages", refused=True)


def variant_nomessages(work):
    return simple(work, "telegram-nomessages", "v181.sql", 181, (), pick(user_rows(), (1,)), refused=True, only=("users",))


def variant_layers(work):
    rows, users, chats = layers_rows()
    return simple(work, "telegram-layers", "v181.sql", 181, rows, users, chats)


def variant_hostile(work):
    """H1-H12; the NaN cell is written as a REAL marker and its 8 bytes (in the row and in the primary-key index)
    replaced with a NaN afterwards (SQLite itself never stores a NaN)."""
    path = os.path.join(work, "cache4.db")
    rows, nan = hostile_rows()
    con = create_db(path, "v181.sql", 181)
    # TEXT in INTEGER columns: written while the column has no type (writable_schema), then the declaration restored
    texty = [r for r in rows if any(isinstance(v, C.Raw) for k, v in r.items() if k != "data")]
    plain = [r for r in rows if r not in texty]
    fill(con, plain)
    orig = con.execute("select sql from sqlite_master where name = 'messages_v2'").fetchone()[0]
    con.execute("pragma writable_schema = ON")
    con.execute("update sqlite_master set sql = ? where name = 'messages_v2'", (orig.replace("uid INTEGER,", "uid,"),))
    con.execute("pragma writable_schema = OFF")
    con.close()
    con = connect(path)
    fill(con, texty)
    con.execute("pragma writable_schema = ON")
    con.execute("update sqlite_master set sql = ? where name = 'messages_v2'", (orig,))
    con.execute("pragma writable_schema = OFF")
    con.close()
    con = connect(path)
    # the engine's check names the TEXT cell it was built to hold, and nothing else
    got = [x[0] for x in con.execute("pragma integrity_check").fetchall()]
    if got not in (["ok"], ["TEXT value in messages_v2.uid"]):
        raise SystemExit("telegram-hostile: integrity_check after the schema round trip: %r" % (got,))
    files = finish(con, path)
    mark = struct.pack(">d", NAN_MARK)
    if files["db"].count(mark) != 2:
        raise SystemExit("telegram-hostile: the NaN marker is in %d places, want 2 (row and index)" % files["db"].count(mark))
    files["db"] = files["db"].replace(mark, NAN_BITS)
    return files, oracle(files, "v181.sql", "telegram-hostile", 181, nan=nan)


def variant_deleted(work):
    """R1, R2, R4 live; in a WAL that is never checkpointed: rows 201-205 inserted (committed), then deleted (a later
    frame); R1 edited and R2 rewritten with REPLACE INTO (the pinned write_verb.messages_v2.edit and .put, T2: each
    gets a new rowid; the old image stays in the database file and in earlier frames). secure_delete is ON."""
    path = os.path.join(work, "cache4.db")
    con = create_db(path, "v181.sql", 181)
    R = core_messages()
    live = pick(R, (1, 2, 4))
    fill(con, live)
    scen = SCENARIOS["telegram-deleted"] = C.Scenario(CLAIMED, alias={"users": "uid", "messages": "mid"})
    scen.write("messages_v2", scen.snap(con, "messages_v2"))
    old = scen.snap(con, "messages_v2", "rowid in (1, 2)")
    con.execute("pragma wal_checkpoint(truncate)")
    con.close()
    gone = [row(200 + i, 200 + i, ANN, enc(tlmsg(200 + i, 200 + i, "deleted message %d" % i, frm=pu(ANN)))) for i in range(1, 6)]
    cols = list(gone[0])
    edited = dict(R[1], data=enc(tlmsg(101, 1, "Hello Ann here (edited)", frm=pu(ANN), edit_date=D + 600)))
    edited.pop("rowid")
    put = dict(R[2])
    put.pop("rowid")
    stmts = ["pragma secure_delete = ON", "pragma wal_autocheckpoint = 0", "BEGIN"] + [insert_sql("messages_v2", g, cols) for g in gone] + [
        "COMMIT", "delete from messages_v2 where rowid between 201 and 205",
        insert_sql("messages_v2", edited, cols, "replace"), insert_sql("messages_v2", put, cols, "replace")]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    gone_snap = [(g["rowid"], {k: v for k, v in g.items() if k != "rowid"}) for g in gone]
    scen.write("messages_v2", gone_snap)
    scen.want("messages_v2", gone_snap + old)
    return files, oracle(files, "v181.sql", "telegram-deleted", 181)


def recover_expect(variant, files):
    """The <variant>.recover.expect.json: every recovered row of a claimed table mapped with env None (4I4-P6: it joins
    nothing) and its raw.live_key_state from its own (mid, uid) against the live rows of its table (T2)."""
    view = C.engine_view(files, ["messages", "messages_v2"])
    keys = {(t, r.get("mid"), r.get("uid")) for t in ("messages", "messages_v2") for r in view.get(t, [])}

    def expect_fn(table, d, env, file):
        if table == "users":
            rec = map_user(d, None, "", file)
            rec.pop("_warn")
            return rec
        rec = map_message(table, d, None, "", file)
        mid, uid = d.get("mid"), d.get("uid")
        raw = rec["payload"]["raw"]
        if is_int(mid) and is_int(uid):
            raw["live_key_state"] = "present" if (table, mid, uid) in keys else "absent"
        else:
            raw["live_key_state"] = "undecided"
        return rec

    return C.recover_oracle(variant, files, SCENARIOS[variant], lambda f: None, expect_fn)


VARIANTS = {"telegram-v181": variant_v181, "telegram-v159": variant_v159, "telegram-v84": variant_v84,
            "telegram-v84-upgraded-v181": variant_upgraded, "telegram-v64": variant_v64, "telegram-both": variant_both,
            "telegram-otherversion": variant_otherversion, "telegram-oldversion": variant_oldversion,
            "telegram-nomessages": variant_nomessages, "telegram-layers": variant_layers, "telegram-hostile": variant_hostile,
            "telegram-deleted": variant_deleted}

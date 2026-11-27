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
  - raw.<col>_undecided lists the record fields (MESSAGE_MAPPED, USER_MAPPED) of the top row not read because of the
    stop: the stopped field itself when the stop is its own read, then those that start after the last item of the
    stop's top-level name and are present by their flags (or whose flags word was not read, or ungated) (Task 8
    review M2, M3).
  - kind: a partial message blob takes the media rule, and so does a truncated, malformed or capped one whose stop
    lies after media (or the action); one stopped at or before it is `unknown` (spec 8.4; Task 8 review I1).
  - replydata gets the blob keys (replydata_state, _len, _sha256, _constructor, ...; no _undecided) and, when its top
    is a Message row, raw.reply_copy {copy, constructor, id, date, text}, else raw.reply_copy_state (plan 4I4 Task 9).
  - raw.text_flags is one comma-joined sorted string (the plan's vocabulary, B8); a contact's phone_number is an
    identifier, never cut; a coordinate out of range states <k>_state and its IEEE-754 bits <k>_bits; a forward header
    with two from_id fields states both (from_ids, from_state "conflict") and no from (plan 4I4 Task 9).
  - the dialog address of an outgoing message is the dialog's own (4I4-P11); a negative dialog left unresolved, a
    folder and an unknown dialog have no participant and no thread group.
  - a CORE message row has no trailer unless the row states one (R3, R41, R42).
  - secret fields hold SYNTHETIC-NOT-A-KEY- plus a SHA-256 chain of the scenario seed (the encoder refuses anything
    else); enc_chats key columns hold the SHA-256 chains of their seeds (spec 9, the key-material test's targets).
Choices this oracle states where plan 4I4b's "Raw shapes" contract leaves the detail (Tasks 5 and 6 follow them, or the
coordinator rules):
  - a class label (attributes, sticker sets, web page attributes, entities, reactions) treats a run of capitals as one
    word: TL_inputStickerSetID is "id", as the contract's examples say.
  - a list read whole states <key>_total (0 included) and <key>_truncated when cut; an empty list and an empty entry
    are absent. A stop at or inside a list or a tail object (its own read included) is <key>_state "partial".
  - a document's attribute scalars (P5) and its labels are stated only when the attributes vector was read whole;
    file_names lists the distinct names in wire order (file_names_total counts them).
  - raw.media.caption is captionLegacy when the wrapper read it, else a legacy Photo or Video caption.
  - raw.replies_data (the replies shape) is stated when the cell decoded whole.
  - the text-flag names: media.<key>, <list>.type of a size entry, post_author, summary_from_language,
    restriction_reason.<key>, factcheck.<key>, entities.<key>, reactions.emoticon.
Every value here is fictional (example.test, 1555 numbers, invented names).
"""
import base64
import hashlib
import json
import math
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
GONE_USER = 1000012  # the users row telegram-deleted inserts and deletes in its WAL
I64_MIN = -(1 << 63)

MAX_BODY = 256 << 10
MAX_SHORT = 4 << 10  # reply_copy.text, attach_path, media texts
MAX_NAME = 1 << 10  # names and titles
MAX_VCARD = 64 << 10
MAX_ID = 1 << 10  # identifiers are never cut: over it they are absent with their state
MAX_PARAMS = 64
MAX_NOTE = 512
MAX_SEEN_NOTE = 1000

MAX_TINY = 64  # plan 4I4b "Raw shapes": emoticons, a size type, video_codec, language, country, summary_from_language
MAX_SIZES, MAX_LABELS, MAX_FILE_NAMES = 16, 32, 8
MAX_REACTIONS, MAX_TOP_REACTORS, MAX_REPLIERS, MAX_RESTRICTIONS, MAX_ENTITIES = 64, 16, 16, 16, 64

MESSAGE_MAPPED = ("out", "id", "from_id", "peer_id", "fwd_from", "via_bot_id", "reply_to", "date", "message", "media",
                  "action", "views", "edit_date", "grouped_id", "ttl_period",
                  # plan 4I4b "Raw shapes": the tail, in this order after ttl_period
                  "forwards", "post_author", "replies", "reactions", "restriction_reason", "quick_reply_shortcut_id", "effect",
                  "factcheck", "report_delivery_until_date", "paid_message_stars", "suggested_post", "schedule_repeat_period",
                  "summary_from_language", "entities")
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


def contact_summary(display, phones, ims):
    name = summarize(display or "", 80, True)
    phone = ""
    for p in phones:
        phone = summarize(p, 32)
        if phone:
            break
    if name or phone:
        return " ".join(x for x in (name, phone) if x)
    for im in ims:  # an app account identifies a contact that has nothing else (plan 4I4 Task 12)
        if summarize(im["value"], 80, True):
            return summarize(im["value"], 80, True)
    return ""


def note_list(names, limit=MAX_NOTE, sep=","):
    """A list joined by sep, cut at an entry boundary within limit bytes, with a trailing ",…"."""
    out = ""
    for n in names:
        add = len(n.encode("utf-8")) + (len(sep) if out else 0)
        if len(out.encode("utf-8")) + add > limit - len(",…".encode("utf-8")):
            return out + ",…"
        out += (sep if out else "") + n
    return out


def mark(raw, key):
    """Names key in raw.text_flags: one comma-joined, sorted string of the texts cleaning altered or cut (the plan's
    shared vocabulary, B8; plan 4I4 Task 9 amended the list form)."""
    names = set(raw["text_flags"].split(",")) if raw.get("text_flags") else set()
    raw["text_flags"] = ",".join(sorted(names | {key}))


def cleaned(raw, key, value, limit):
    """A cleaned text (C4) or None when empty; an altered text is listed in raw.text_flags."""
    c = C.clean(bytes(value), limit)
    if c.flags:
        mark(raw, key)
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

    def heads(self):
        """The top-level names of the top row's items that carry bytes, in order."""
        return [it.name.split(".")[0] for it in self.row.items if it.kind is not None]

    def stop_head_at(self):
        """The index of the last item sharing the stop's top-level name (a name may span several items, such as an
        old layer's fwd_from.from_id and fwd_from.date), or -1."""
        s = self.res["stop"]
        if s is None or self.row is None:
            return -1
        head = re.split(r"[.\[]", s["path"])[0]
        names = self.heads()
        return max((i for i, n in enumerate(names) if n == head), default=-1)

    def stop_after(self, name):
        """The stop lies at a top-level item after the last item named name."""
        if self.row is None:
            return False
        names = self.heads()
        at = self.stop_head_at()
        target = max((i for i, n in enumerate(names) if n == name), default=-1)
        return at >= 0 and target >= 0 and at > target

    def undecided(self, mapped):
        """The mapped fields of the top row not read because of the stop: the stopped field itself when the stop is
        its own read (a truncated, malformed, capped or refused value at the top level), then those that start after
        the last item of the stop's top-level name and are present by their flags (or whose flags word was not read,
        or ungated)."""
        s = self.res["stop"]
        if s is None or s["kind"] == "prefix" or self.row is None:
            return []
        head = re.split(r"[.\[]", s["path"])[0]
        after = self.stop_head_at()
        if after < 0:  # the stop is at the top object itself
            return []
        out = []
        if s["kind"] in ("truncated", "malformed", "cap", "budget") and s["path"] == head and head in mapped:
            out.append(head)
        for i, it in enumerate([it for it in self.row.items if it.kind is not None]):
            name = it.name.split(".")[0]
            if i <= after or name == head or name not in mapped or name in out:
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
    return address_of(b, e, path)


def address_of(b, e, path):
    """The participant address of the decoded entry e at path, or None."""
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
    times = {}  # Times of the record (plan 4I4b Task 1: edited)
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
    # a stop after media (or the action) keeps the fields before it, as partial (spec 8.4; plan 4I4 Task 8 review I1)
    readable = b is not None and (b.state in ("decoded", "partial") or (b.cls is not None and b.stop_after(
        "action" if b.cls.startswith("TL_messageService") else "media")))
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
            # every from_id read (an old header names the user at bit 0 and the channel at bit 1 from_id): two are a
            # conflict, none chosen; an inline id of an unresolved class is contradictory (T5; plan 4I4 Task 9)
            origins = [e for e in b.res["fields"] if e["path"] == "fwd_from.from_id"]
            addrs = [address_of(b, e, "fwd_from.from_id") for e in origins]
            known = [a for a in addrs if a]
            unresolved = [str(e["value"]) for e, a in zip(origins, addrs) if not a and e["kind"] in ("id32", "id64")]
            if len(known) + len(unresolved) > 1:
                fwd["from_ids"], fwd["from_state"] = ",".join(known + unresolved), "conflict"
            elif known:
                fwd["from"] = known[0]
            elif unresolved:
                fwd["from_id"], fwd["from_class_state"] = unresolved[0], "contradictory"
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
            # plan 4I4b Task 1 (rulings R5, B3, design D9): edit_date is Unix seconds at all four pins (facts.tsv
            # unit.edit_date); the date-status rule of the date column: 0 or -1 is a sentinel, outside 1970-2100 invalid
            ed = b.val("edit_date")
            raw["edit_date"], raw["edit_date_unit"] = ed, "seconds"
            st = C.classify(ed, "s")
            if st == "valid":
                times["edited"] = C.iso(ed, "s")
            else:
                raw["edit_date_status"] = st
        map_tail(b, raw)
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
            # every source must name the same group class: a user peer in a negative dialog names none (spec 8.6: a
            # negative dialog is a basic group, channel or community; plan 4I4 Task 10)
            if all(s == sources[0] for s in sources) and sources[0] in ("chat", "channel", "community"):
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
            else:  # a user cell that holds no id is undecided, never "no counterparty" (spec 12; plan 4I4 Task 10)
                unknown["enc_chats.user"] = "null" if erow.get("user") is None else "type-mismatch"
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
            fe = b.f.get("from_id") if b is not None and cls else None
            stop = b.res["stop"] if b is not None else None
            if fa:
                parts = [{"role": "from", "address": fa}]
            elif fe is not None and fe["kind"] in ("id32", "id64"):
                # an inline id of a class the table leaves unresolved: no address, never a guess (T5, 4I4-P11)
                unknown_parts = True
                raw["from_id_id"], raw["from_id_class_state"] = str(fe["value"]), "contradictory"
            elif dclass == "channel" and b is not None and b.flag("post") and b.has("flags"):
                parts = [{"role": "from", "address": own}]
                raw["sender_basis"] = "channel-post"
            else:
                unknown_parts = True
                # a stop inside from_id leaves it unread, never absent (plan 4I4 Task 10)
                inside = stop is not None and re.split(r"[.\[]", stop["path"])[0] == "from_id"
                from_read = b is not None and cls is not None and b.has("flags") and not inside and (
                    b.state == "decoded" or "from_id" not in b.undecided(MESSAGE_MAPPED))
                raw["sender_state"] = "absent" if from_read else "blob-undecided"
        else:
            unknown_parts = True
            # a negative dialog left unresolved keeps the decoded sender in raw, never as a participant (plan 4I4 Task
            # 10 fix 1, M5)
            if dclass == "negative" and b is not None and cls:
                fa = address(b, "from_id")
                if fa:
                    raw["from_id"] = fa
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
        if col == "custom_params":
            raw[col + "_len"], raw[col + "_sha256"] = len(v), sha(v)
            continue
        # design D13, P6: the replies_data cell is the serialized message.replies, decoded as MessageReplies with the
        # blob keys (no _undecided); the replies shape when it decoded whole, and replies_conflict when the blob's own
        # replies was read whole and the two differ (B8: by decoded values)
        rb = Blob(v, "MessageReplies")
        blob_raw(raw, "replies_data", rb)
        if rb.state == "decoded" and rb.cls is not None:
            raw["replies_data"] = replies_shape(rb, "")
            if isinstance(raw.get("replies"), dict) and raw["replies"] != raw["replies_data"]:
                raw["replies_conflict"] = True

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
                raw["reply_copy_state"] = rb.state  # spec 8.5: not a Message (a StoryItem, an unknown ID)
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
            # an ID that was read is its constructor; a stop before or at the ID for another reason is its state,
            # never an ID that is not on the wire (plan 4I4 Task 9 review I1)
            st = b.stop_at("media")
            if st["kind"] in ("unknown-constructor", "opaque"):
                raw["media"] = {"constructor": hx(st["id"])}
            else:
                raw["media"] = {"state": st["kind"]}
                if st["id"] and O.table().get(st["id"]) is not None:
                    raw["media"].update({"constructor": hx(st["id"]), "type": media_type(O.table()[st["id"]].cls)})
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
    return record(table, r, "message", time, None, body, message_summary(channel, direction, cp, text), p, remote, file,
                  times)


def inside(b, path):
    """The decode's stop lies at path or inside what starts there (a nested field or a vector element)."""
    s = b.res["stop"]
    return s is not None and s["kind"] != "prefix" and (
        s["path"] == path or s["path"].startswith(path + ".") or s["path"].startswith(path + "["))


def whole(b, path):
    """The value at path was read and the stop lies neither at nor inside it (P3: read whole)."""
    return b.has(path) and not inside(b, path)


def vec_len(b, path):
    e = b.f.get(path)
    return e["len"] if e is not None and e["kind"] == "vector" else None


def class_label(cls, prefix):
    """classLabel (plan "Raw shapes" Labels): the class name after prefix up to the first "_", in snake case, a run of
    capitals one word (TL_inputStickerSetID "id", TL_documentAttributeImageSize "image_size"); "" for another prefix."""
    if cls is None or not cls.startswith(prefix):
        return ""
    name = cls[len(prefix):].split("_")[0]
    return re.sub(r"(?<=[a-z0-9])([A-Z])", r"_\1", name).lower()


def entity_label(cls):
    if cls is not None and cls.startswith("TL_inputMessageEntity"):
        return "input_" + class_label(cls, "TL_inputMessageEntity")
    return class_label(cls, "TL_messageEntity")


def bits(x):
    return "0x%016x" % struct.unpack("<Q", struct.pack("<d", x))[0]


def finite_into(m, key, v):
    """A number at m[key]; a double that is not finite is <key>_state out-of-range with its bits (the coordinate rule
    without a range)."""
    if isinstance(v, float) and not math.isfinite(v):
        m[key + "_state"], m[key + "_bits"] = "out-of-range", bits(v)
    else:
        m[key] = gofloat(v) if isinstance(v, float) else v


def clean_into(m, raw, key, flag, value, limit):
    """A cleaned text at m[key] (absent when empty); flag names it in raw.text_flags when cleaning altered or cut it."""
    t = cleaned(raw, flag, value, limit)
    if t:
        m[key] = t
    return t


def ident_into(m, raw, key, flag, v, limit):
    """An identifier at m[key], never cut: over limit bytes it is absent with <key>_state too-long, _len, _sha256."""
    c = C.clean(bytes(v), 1 << 30)
    if len(c.text.encode("utf-8")) > limit:
        m[key + "_state"], m[key + "_len"], m[key + "_sha256"] = "too-long", len(v), sha(v)
        return None
    if c.text:
        m[key] = c.text
        if c.flags:
            mark(raw, flag)
    return c.text or None


def put_list(b, m, key, path, cap, entry):
    """A list of entry(element path) at m[key] when the vector at path was read whole (P3): at most cap entries, empty
    ones and an empty list absent, <key>_total the count read, <key>_truncated when cut; a stop at or inside it is
    m[<key>_state] partial and no entry."""
    if whole(b, path):
        n = vec_len(b, path)
        out = [x for x in (entry("%s[%d]" % (path, i)) for i in range(min(n, cap))) if x]
        if out:
            m[key] = out
        m[key + "_total"] = n
        if n > cap:
            m[key + "_truncated"] = True
    elif inside(b, path):
        m[key + "_state"] = "partial"


def field_kind(b, path):
    """The kind the row of the object holding path gives the field at path (None when that object was not read)."""
    parent, _, name = path.rpartition(".")
    row = O.table().get(b.obj_id(parent)) if parent else b.row
    for it in row.items if row else []:
        if it.name == name and it.kind is not None:
            return it.kind
    return None


SIZE_KINDS = {"TL_photoSize": "size", "TL_photoCachedSize": "cached", "TL_photoStrippedSize": "stripped",
              "TL_photoPathSize": "path", "TL_photoSizeProgressive": "progressive", "TL_photoSizeEmpty": "empty",
              "TL_videoSize": "video", "TL_videoSizeEmojiMarkup": "emoji_markup",
              "TL_videoSizeStickerMarkup": "sticker_markup"}


def size_entry(b, raw, p, flag):
    """One scalar-only size entry (plan "Raw shapes" Size entry) of the PhotoSize or VideoSize object at p."""
    cls = b.obj_cls(p)
    if cls is None:
        return None
    e = {"kind": SIZE_KINDS.get(cls.split("_layer")[0], "unknown")}
    if b.val(p + ".type") is not None:
        clean_into(e, raw, "type", flag + ".type", b.val(p + ".type"), MAX_TINY)
    for k in ("w", "h", "size", "emoji_id", "sticker_id"):
        if b.val(p + "." + k) is not None:
            e[k] = b.val(p + "." + k)
    if b.val(p + ".bytes") is not None:
        e["bytes_len"], e["bytes_sha256"] = len(b.val(p + ".bytes")), sha(b.val(p + ".bytes"))
    for k, key in (("sizes", "progressive_total"), ("background_colors", "background_colors_total")):
        if vec_len(b, p + "." + k) is not None:
            e[key] = vec_len(b, p + "." + k)
    if b.obj_cls(p + ".location") is not None:
        e["location_class"] = b.obj_cls(p + ".location")  # the class only: never a FileLocation value (D12)
    if b.val(p + ".video_start_ts") is not None:
        finite_into(e, "video_start_ts", b.val(p + ".video_start_ts"))
    if b.obj_cls(p + ".stickerset") is not None:
        e["stickerset_kind"] = class_label(b.obj_cls(p + ".stickerset"), "TL_inputStickerSet")
    return e


def size_list(b, raw, key, path):
    """raw.<key> (B1: at the top of raw) of the sizes at path: a vector, or the single PhotoSize of old classes as one
    entry (plan "Raw shapes")."""
    kind = field_kind(b, path)
    if kind is None:
        return
    if kind.startswith("vec<"):
        put_list(b, raw, key, path, MAX_SIZES, lambda p: size_entry(b, raw, p, key))
    elif whole(b, path):
        raw[key] = [size_entry(b, raw, path, key)]
        raw[key + "_total"] = 1
    elif inside(b, path):
        raw[key + "_state"] = "partial"


def canon(v):
    if isinstance(v, float):
        return ("n", v) if math.isfinite(v) else ("f", bits(v))
    if isinstance(v, tuple):
        return tuple(canon(x) for x in v)
    return ("n", v) if is_int(v) else ("o", v)


def file_name(b, m, raw, doc, key):
    """D10, R6, B6: the name of the document at doc from exactly its filename attributes (or the inline file_name of an
    old class): m[key] when every one holds the same bytes and they clean to a text; else m[<key>_state] absent, empty,
    conflict (with <key>s, at most 8 distinct names in wire order, and <key>s_total) or undecided. Returns the name."""
    row = O.table().get(b.obj_id(doc))
    names = [it.name for it in row.items] if row else []
    if "file_name" in names:
        if not b.has(doc + ".file_name"):
            m[key + "_state"] = "undecided"
            return None
        values = [b.val(doc + ".file_name")]
    elif "attributes" in names:
        p = doc + ".attributes"
        if not whole(b, p):
            m[key + "_state"] = "undecided"
            return None
        values = [b.val("%s[%d].file_name" % (p, i)) for i in range(vec_len(b, p))
                  if b.obj_cls("%s[%d]" % (p, i)) == "TL_documentAttributeFilename"]
    else:
        return None
    distinct = []
    for v in values:
        if v not in distinct:
            distinct.append(v)
    if not distinct:
        m[key + "_state"] = "absent"
        return None
    if len(distinct) > 1:
        m[key + "_state"] = "conflict"
        m[key + "s"] = [cleaned(raw, "media." + key, v, MAX_NAME) or "" for v in distinct[:MAX_FILE_NAMES]]
        m[key + "s_total"] = len(distinct)
        return None
    t = cleaned(raw, "media." + key, distinct[0], MAX_NAME)
    if not t:
        m[key + "_state"] = "empty"
        return None
    m[key] = t
    return t


def attribute_scalars(b, m, raw, path):
    """P5: the scalars of the attributes at path (read whole): a key every carrier holds the same decoded value of is
    stated, carriers that disagree give <key>_state conflict; a boolean is true when any carrier sets it."""
    vals, bools = {}, set()

    def add(k, v):
        vals.setdefault(k, []).append(v)
    for i in range(vec_len(b, path)):
        a = "%s[%d]" % (path, i)
        lab = class_label(b.obj_cls(a), "TL_documentAttribute")
        g = lambda f: b.val(a + "." + f)
        flags = {"video": ("round_message", "supports_streaming", "nosound"), "audio": ("voice",), "sticker": ("mask",)}
        for f in flags.get(lab, ()):
            if b.flag(a + "." + f):
                bools.add(f)
        fields = {"image_size": ("w", "h"), "video": ("duration", "w", "h", "preload_prefix_size", "video_start_ts", "video_codec"),
                  "audio": ("duration", "title", "performer", "waveform")}
        for f in fields.get(lab, ()):
            if g(f) is not None:
                add(f, g(f))
        if lab in ("sticker", "custom_emoji"):
            if g("alt") is not None:
                add("sticker_alt", g("alt"))
            s = a + ".stickerset"
            if b.obj_cls(s) is not None:
                add("stickerset_kind", class_label(b.obj_cls(s), "TL_inputStickerSet"))
                for f, key in (("id", "stickerset_id"), ("short_name", "stickerset_short_name"), ("emoticon", "stickerset_emoticon")):
                    if b.val(s + "." + f) is not None:
                        add(key, b.val(s + "." + f))
                if b.has(s + ".access_hash"):
                    add("stickerset_access_hash_present", True)
            if b.has(a + ".mask_coords"):
                add("mask_coords", tuple(b.val(a + ".mask_coords." + f) for f in ("n", "x", "y", "zoom")))
    for k in sorted(vals):
        lst = vals[k]
        if any(canon(v) != canon(lst[0]) for v in lst):
            m[k + "_state"] = "conflict"
            continue
        v = lst[0]
        if k == "duration":
            finite_into(m, k, v)
            m["duration_unit"] = "unestablished"
        elif k == "video_start_ts":
            finite_into(m, k, v)
        elif k in ("title", "performer", "sticker_alt"):
            clean_into(m, raw, k, "media." + k, v, MAX_NAME)
        elif k in ("video_codec", "stickerset_emoticon"):
            clean_into(m, raw, k, "media." + k, v, MAX_TINY)
        elif k == "stickerset_short_name":
            ident_into(m, raw, k, "media." + k, v, MAX_ID)
        elif k == "waveform":
            m["waveform_len"], m["waveform_sha256"] = len(v), sha(v)  # content never shown (D11)
        elif k == "mask_coords":
            mc = {}
            for f, x in zip(("n", "x", "y", "zoom"), v):
                if x is not None:
                    finite_into(mc, f, x)
            m[k] = mc
        else:
            m[k] = v
    for f in sorted(bools):
        m[f] = True


def label_list(b, m, key, path, prefix):
    if whole(b, path):
        n = vec_len(b, path)
        labels = [class_label(b.obj_cls("%s[%d]" % (path, i)), prefix) for i in range(min(n, MAX_LABELS))]
        if labels:
            m[key] = labels
        m[key + "_total"] = n
        if n > MAX_LABELS:
            m[key + "_truncated"] = True
    elif inside(b, path):
        m[key + "_state"] = "partial"


def presence(b, m, prefix):
    for k, key in (("access_hash", "access_hash_present"), ("file_reference", "file_reference_present"), ("key", "key_present"), ("iv", "iv_present")):
        if b.has(prefix + k):
            m[key] = True


def map_media(b, media, attachments, raw):
    """raw.media (4I4 spec 8.5, plan 4I4b "Raw shapes"): what was read of the media object, with the lists at the top
    of raw (B1); a document's (and a legacy video's or audio's) attachment {name, mime, size}, each only when read and
    valid, the name by D10 from the wrapper's own document only."""
    cls = b.obj_cls("media")
    W = "media."
    for f in ("spoiler", "video", "round", "voice", "nopremium", "live_photo"):
        if b.flag(W + f):
            media[f] = True
    if b.val(W + "ttl_seconds") is not None:
        media["ttl_seconds"] = b.val(W + "ttl_seconds")
    if b.has(W + "captionLegacy"):
        clean_into(media, raw, "caption", "media.caption", b.val(W + "captionLegacy"), MAX_SHORT)
    else:
        for p in (W + "photo.caption", W + "video_unused.caption"):
            if b.has(p):
                clean_into(media, raw, "caption", "media.caption", b.val(p), MAX_SHORT)
                break
    if cls.startswith("TL_messageMediaPhoto"):
        if b.has("media.photo"):
            P = "media.photo."
            for k in ("id", "date", "dc_id", "user_id"):
                if b.val(P + k) is not None:
                    media[k] = b.val(P + k)
            presence(b, media, P)
            if b.flag(P + "has_stickers"):
                media["has_stickers"] = True
            coordinate(b, media, "geo_lat", P + "geo.lat", 90)
            coordinate(b, media, "geo_long", P + "geo._long", 180)
            size_list(b, raw, "media_sizes", P + "sizes")
            size_list(b, raw, "media_video_sizes", P + "video_sizes")
        if b.has("media.document"):  # P9: a live photo's document names no attachment
            if b.val("media.document.id") is not None:
                media["live_photo_document_id"] = b.val("media.document.id")
            if b.val("media.document.mime_type") is not None:
                clean_into(media, raw, "live_photo_mime_type", "media.live_photo_mime_type", b.val("media.document.mime_type"), MAX_SHORT)
    elif cls.startswith("TL_messageMediaDocument"):
        if b.has("media.document"):
            d = "media.document."
            for k in ("id", "date", "size", "dc_id"):
                if b.val(d + k) is not None:
                    media[k] = b.val(d + k)
            mime = b.val(d + "mime_type")
            if mime is not None:
                clean_into(media, raw, "mime_type", "media.mime_type", mime, MAX_SHORT)
            presence(b, media, d)
            name = file_name(b, media, raw, "media.document", "file_name")
            label_list(b, media, "attributes", d + "attributes", "TL_documentAttribute")
            if whole(b, d + "attributes"):
                attribute_scalars(b, media, raw, d + "attributes")
            size_list(b, raw, "media_thumbs", d + "thumbs")
            size_list(b, raw, "media_video_thumbs", d + "video_thumbs")
            att = {}
            if name:
                att["name"] = name
            if media.get("mime_type"):
                att["mime"] = media["mime_type"]
            if is_int(media.get("size")) and media["size"] >= 0:
                att["size"] = media["size"]
            if att:
                attachments.append(att)
        if vec_len(b, W + "alt_documents") is not None and whole(b, W + "alt_documents"):
            media["alt_documents_total"] = vec_len(b, W + "alt_documents")
        elif b.obj_cls(W + "alt_documents") is not None and whole(b, W + "alt_documents"):
            media["alt_documents_total"] = 1
        if b.val(W + "video_cover.id") is not None:
            media["video_cover_photo_id"] = b.val(W + "video_cover.id")
        if b.val(W + "video_timestamp") is not None:
            media["video_timestamp"] = b.val(W + "video_timestamp")
    elif cls.startswith(("TL_messageMediaVideo", "TL_messageMediaAudio")):
        o = "media.video_unused." if cls.startswith("TL_messageMediaVideo") else "media.audio_unused."
        if b.has(o[:-1]):
            for k in ("id", "date", "size", "dc_id", "w", "h", "user_id"):
                if b.val(o + k) is not None:
                    media[k] = b.val(o + k)
            if b.val(o + "duration") is not None:
                media["duration"], media["duration_unit"] = b.val(o + "duration"), "unestablished"
            if b.val(o + "mime_type") is not None:
                clean_into(media, raw, "mime_type", "media.mime_type", b.val(o + "mime_type"), MAX_SHORT)
            presence(b, media, o)
            size_list(b, raw, "media_thumbs", o + "thumb")
            att = {}
            if media.get("mime_type"):
                att["mime"] = media["mime_type"]
            if is_int(media.get("size")) and media["size"] >= 0:
                att["size"] = media["size"]
            if att:
                attachments.append(att)
    elif cls.startswith(("TL_messageMediaGeo", "TL_messageMediaVenue")):
        coordinate(b, media, "lat", "media.geo.lat", 90)
        coordinate(b, media, "long", "media.geo._long", 180)
        if cls.startswith("TL_messageMediaVenue"):
            for k in ("title", "address"):
                if b.val("media." + k) is not None:
                    clean_into(media, raw, k, "media." + k, b.val("media." + k), MAX_SHORT)
    elif cls.startswith("TL_messageMediaContact"):
        if b.val("media.phone_number") is not None:  # an identifier: never cut (spec 12; plan 4I4 Task 9)
            ident_into(media, raw, "phone_number", "media.phone_number", b.val("media.phone_number"), MAX_ID)
        for k, lim in (("first_name", MAX_NAME), ("last_name", MAX_NAME), ("vcard", MAX_VCARD)):
            if b.val("media." + k) is not None:
                clean_into(media, raw, k, "media." + k, b.val("media." + k), lim)
        if b.val("media.user_id") is not None:
            media["user_id"] = b.val("media.user_id")
    elif cls.startswith("TL_messageMediaWebPage"):
        w = "media.webpage."
        for k, key in (("url", "url"), ("display_url", "display_url"), ("type", "webpage_type"), ("site_name", "site_name"),
                       ("title", "title"), ("description", "description"), ("author", "author"), ("embed_url", "embed_url"),
                       ("embed_type", "embed_type")):
            if b.val(w + k) is not None:
                clean_into(media, raw, key, "media." + key, b.val(w + k), MAX_SHORT)
        for k in ("embed_width", "embed_height", "duration"):
            if b.val(w + k) is not None:
                media[k] = b.val(w + k)
        if b.val(w + "photo.id") is not None:
            media["photo_id"] = b.val(w + "photo.id")
        if b.has(w + "document"):  # a link preview's document: never an attachment (spec 8.1)
            if b.val(w + "document.id") is not None:
                media["document_id"] = b.val(w + "document.id")
            if b.val(w + "document.mime_type") is not None:
                clean_into(media, raw, "document_mime_type", "media.document_mime_type", b.val(w + "document.mime_type"), MAX_SHORT)
            file_name(b, media, raw, w + "document", "document_file_name")
        label_list(b, media, "webpage_attributes", w + "attributes", "TL_webPageAttribute")
        s = b.stop_at(w + "cached_page")
        if s and s["id"] is not None and O.table().get(s["id"]) is not None:
            media["cached_page_class"] = O.table()[s["id"]].cls
    elif cls.startswith("TL_messageMediaDice"):
        if b.val(W + "emoticon") is not None:
            clean_into(media, raw, "emoticon", "media.emoticon", b.val(W + "emoticon"), MAX_TINY)
        if b.val(W + "value") is not None:
            media["value"] = b.val(W + "value")
        if b.has(W + "game_outcome"):
            g = {}
            for k in ("stake_ton_amount", "ton_amount"):
                if b.val(W + "game_outcome." + k) is not None:
                    g[k] = b.val(W + "game_outcome." + k)
            if b.val(W + "game_outcome.seed") is not None:
                g["seed_len"] = len(b.val(W + "game_outcome.seed"))  # the seed itself never (D11)
            media["game_outcome"] = g


def coordinate(b, m, key, path, lim):
    """A coordinate read at path: within +-lim and finite as read, else <key>_state out-of-range and its IEEE-754 bits
    (plan 4I4 Task 9: never shown as a coordinate)."""
    v = b.val(path)
    if v is None:
        return
    if v == v and abs(v) <= lim and abs(v) != float("inf"):
        m[key] = gofloat(v)
    else:
        m[key + "_state"], m[key + "_bits"] = "out-of-range", bits(v)


def reaction_of(b, raw, path):
    """A reaction (plan "Raw shapes"): emoji:<emoticon> of a boxed TL_reactionEmoji or of an inline reaction.emoticon,
    custom:<document_id>, paid, empty; None when nothing was read."""
    cls = b.obj_cls(path)
    if cls is None:
        if b.val(path + ".emoticon") is None:
            return None
        return "emoji:" + (cleaned(raw, "reactions.emoticon", b.val(path + ".emoticon"), MAX_TINY) or "")
    lab = class_label(cls, "TL_reaction")
    if lab == "emoji":
        return "emoji:" + (cleaned(raw, "reactions.emoticon", b.val(path + ".emoticon") or b"", MAX_TINY) or "")
    if lab == "custom_emoji":
        return "custom:%d" % b.val(path + ".document_id")
    return lab


def join(p, k):
    return p + "." + k if p else k


def replies_shape(b, p):
    """The replies object (plan "Raw shapes") of the MessageReplies read whole at p ("" for a replies_data cell)."""
    r = {}
    for k in ("replies", "replies_pts", "max_id", "read_max_id"):
        if b.val(join(p, k)) is not None:
            r[k] = b.val(join(p, k))
    if b.flag(join(p, "comments")):
        r["comments"] = True
    if b.val(join(p, "channel_id")) is not None:
        r["channel"] = "channel:%d" % b.val(join(p, "channel_id"))
    rp = join(p, "recent_repliers")
    n = vec_len(b, rp)
    if n is not None:
        addrs = [a for a in (address(b, "%s[%d]" % (rp, i)) for i in range(min(n, MAX_REPLIERS))) if a]
        if addrs:
            r["recent_repliers"] = addrs
        r["recent_repliers_total"] = n
        if n > MAX_REPLIERS:
            r["recent_repliers_truncated"] = True
    return r


def map_tail(b, raw):
    """The tail of a message blob (plan "Raw shapes" Tail): raw only, never a record field or a participant. An object
    or list is stated when read whole; a stop at or inside it is <key>_state partial (P3); one never reached is
    nothing here (data_undecided names it)."""
    fw = b.val("forwards")
    if fw is not None:  # B10: the column-mapped forwards stays raw.forwards
        raw["blob_forwards"] = fw
        if is_int(raw.get("forwards")) and raw["forwards"] != fw:
            raw["forwards_conflict"] = True
    if b.val("post_author") is not None:
        clean_into(raw, raw, "post_author", "post_author", b.val("post_author"), MAX_NAME)
    for k in ("quick_reply_shortcut_id", "effect", "paid_message_stars"):
        if b.val(k) is not None:
            raw[k] = b.val(k)
    for k in ("report_delivery_until_date", "schedule_repeat_period"):  # P8: no unit is pinned for these
        if b.val(k) is not None:
            raw[k], raw[k + "_unit"] = b.val(k), "unestablished"
    if b.val("summary_from_language") is not None:
        ident_into(raw, raw, "summary_from_language", "summary_from_language", b.val("summary_from_language"), MAX_TINY)
    if whole(b, "replies"):
        raw["replies"] = replies_shape(b, "replies")
    elif inside(b, "replies"):
        raw["replies_state"] = "partial"
    if whole(b, "reactions"):
        p, o = "reactions", {}
        for f in ("min", "can_see_list", "reactions_as_tags"):
            if b.flag(p + "." + f):
                o[f] = True

        def result(e):
            x = {}
            r = reaction_of(b, raw, e + ".reaction")
            if r is not None:
                x["reaction"] = r
            for k in ("count", "chosen_order"):
                if b.val(e + "." + k) is not None:
                    x[k] = b.val(e + "." + k)
            return x

        def recent(e):
            x = {}
            a = address(b, e + ".peer_id")
            if a:
                x["peer"] = a
            r = reaction_of(b, raw, e + ".reaction")
            if r is not None:
                x["reaction"] = r
            for f in ("big", "unread"):
                if b.flag(e + "." + f):
                    x[f] = True
            if b.val(e + ".date") is not None:  # P4: no unit is invented
                x["date"], x["date_unit"] = b.val(e + ".date"), "unestablished"
            return x

        def reactor(e):
            x = {}
            a = address(b, e + ".peer_id")
            if a:
                x["peer"] = a
            if b.val(e + ".count") is not None:
                x["count"] = b.val(e + ".count")
            for f in ("top", "my", "anonymous"):
                if b.flag(e + "." + f):
                    x[f] = True
            return x
        for sub, key, cap, fn in (("results", "results", MAX_REACTIONS, result), ("recent_reactions", "recent", MAX_REACTIONS, recent),
                                  ("top_reactors", "top_reactors", MAX_TOP_REACTORS, reactor)):
            q = p + "." + sub
            n = vec_len(b, q)
            if n is None:
                continue
            entries = [x for x in (fn("%s[%d]" % (q, i)) for i in range(min(n, cap))) if x]
            if entries:
                raw["reactions_" + key] = entries
            o[key + "_total"] = n
            if n > cap:
                o[key + "_truncated"] = True
        raw["reactions"] = o
    elif inside(b, "reactions"):
        raw["reactions_state"] = "partial"

    def restriction(e):
        x = {}
        for k in ("platform", "reason", "text"):
            if b.val(e + "." + k) is not None:
                clean_into(x, raw, k, "restriction_reason." + k, b.val(e + "." + k), MAX_NAME)
        return x
    put_list(b, raw, "restriction_reason", "restriction_reason", MAX_RESTRICTIONS, restriction)
    if whole(b, "factcheck"):
        p, o = "factcheck.", {}
        if b.flag(p + "need_check"):
            o["need_check"] = True
        if b.val(p + "country") is not None:
            clean_into(o, raw, "country", "factcheck.country", b.val(p + "country"), MAX_TINY)
        if b.val(p + "text.text") is not None:
            clean_into(o, raw, "text", "factcheck.text", b.val(p + "text.text"), MAX_SHORT)
        if vec_len(b, p + "text.entities") is not None:
            o["entities_total"] = vec_len(b, p + "text.entities")
        if b.val(p + "hash") is not None:
            o["hash"] = b.val(p + "hash")
        raw["factcheck"] = o
    elif inside(b, "factcheck"):
        raw["factcheck_state"] = "partial"
    if whole(b, "suggested_post"):
        p, o = "suggested_post.", {}
        for f in ("accepted", "rejected"):
            if b.flag(p + f):
                o[f] = True
        if b.obj_cls(p + "price") is not None:
            price = {"kind": {"TL_starsAmount": "stars", "TL_starsTonAmount": "ton"}.get(b.obj_cls(p + "price"), "unknown")}
            for k in ("amount", "nanos"):
                if b.val(p + "price." + k) is not None:
                    price[k] = b.val(p + "price." + k)
            o["price"] = price
        if b.val(p + "schedule_date") is not None:
            o["schedule_date"], o["schedule_date_unit"] = b.val(p + "schedule_date"), "unestablished"
        raw["suggested_post"] = o
    elif inside(b, "suggested_post"):
        raw["suggested_post_state"] = "partial"

    def entity(e):
        x = {"type": entity_label(b.obj_cls(e))}
        for k in ("offset", "length", "document_id"):
            if b.val(e + "." + k) is not None:
                x[k] = b.val(e + "." + k)
        if b.val(e + ".url") is not None:
            clean_into(x, raw, "url", "entities.url", b.val(e + ".url"), MAX_SHORT)
        if b.val(e + ".language") is not None:
            clean_into(x, raw, "language", "entities.language", b.val(e + ".language"), MAX_TINY)
        uid = b.f.get(e + ".user_id")
        if uid is not None and uid["kind"] in ("i32", "i64"):
            x["user"] = "user:%d" % uid["value"]
        return x
    put_list(b, raw, "entities", "entities", MAX_ENTITIES, entity)


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
        for i, pr in enumerate(pairs):
            k, sep, v = pr.partition(b"|=|")
            ck, cv = C.clean(k, MAX_ID), C.clean(v, MAX_ID)
            if i < MAX_PARAMS and (ck.flags or cv.flags):
                mark(raw, "attach_params")  # an altered or cut param is never a silent cut (Task 9 review M2)
            params.append({"key": ck.text, "value": cv.text})
        if params:
            raw["attach_params"] = params[:MAX_PARAMS]
            raw["attach_params_total"] = len(params)
            if len(params) > MAX_PARAMS:
                raw["attach_params_truncated"] = True
    if path:
        c = C.clean(path, MAX_SHORT)
        if c.text:
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
    if b.val(a + "reason.slug") is not None:  # an identifier: never cut (spec 12); over MAX_ID absent with its state
        slug = ident(raw, "slug", b.val(a + "reason.slug"))
        if slug:
            raw["slug"] = slug
    # presence is the action's flags, never whether the field was reached (plan 4I4 Task 11 review I1)
    if reason is None:
        outcome = "unknown"
        raw["reason_state"] = "undecided" if flags is None or flags & 1 else "not-recorded"
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
    if duration is None:
        if flags is None or flags & 2:
            raw["duration_state"] = "undecided"
    else:
        raw["duration"] = duration
        if duration < 0:
            raw["duration_state"] = "negative"
        elif duration > 0 and time is None:
            raw["duration_state"] = "no-start-time"
        else:
            p["duration_s"] = duration  # facts.tsv unit.call_duration: seconds
            if duration > 0:
                time_end = C.iso(r["date"] + duration, "s")
                raw["time_end_basis"] = "start+duration"
    if dclass == "user":
        p["address"] = "user:%d" % uid
    else:
        raw["address_state"] = "not-user-dialog"
    p["raw"] = raw
    return record(table, r, "call", time, time_end, None, call_summary(direction, outcome, p.get("address", "")), p, remote, file,
                  {})


def record(table, r, typ, time, time_end, body, summary, payload, remote, file, times):
    return {"table": table, "rowid": r["rowid"], "file": file, "type": typ, "source_path": remote, "time": time,
            "time_end": time_end, "times": times, "deleted": False, "summary": summary, "body": body or None,
            "payload": payload}


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
    rec = record("users", r, "contact", None, None, body, contact_summary(display, phones, ims), p, remote, file, {})
    rec["_warn"] = not (display or phones or username)
    return rec


def ident(raw, key, v):
    """An identifier as stored, never cut: over MAX_ID it is absent with its state, length and SHA-256."""
    c = C.clean(bytes(v), 1 << 30)
    if len(c.text.encode("utf-8")) > MAX_ID:
        raw[key + "_state"], raw[key + "_len"], raw[key + "_sha256"] = "too-long", len(v), sha(v)
        return None
    if c.flags:
        mark(raw, key)
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
    secret_dialogs = set()  # by enc_chats key: one chat in both generations is one (plan 4I4 Task 10 fix 1, M4)
    undecided_dialogs = 0
    for table in ("messages", "messages_v2"):
        for r in view.get(table, []):
            rec = map_message(table, r, env, remote, file, nan)
            exp["records"].append(rec)
            raw = rec["payload"]["raw"]
            for col in ("data", "replydata", "replies_data"):
                st = raw.get(col + "_state")
                if st:
                    counts.setdefault((table, col), {}).setdefault(st, 0)
                    counts[(table, col)][st] += 1
            if is_blob(r.get("data")) and len(r["data"]) >= 4:
                top = struct.unpack_from("<I", r["data"])[0]
                seen[top] = seen.get(top, 0) + 1
            if raw.get("secret_chat"):
                secret_dialogs.add(raw.get("enc_chat_id"))
            if raw.get("thread_state") == "undecided":
                undecided_dialogs += 1
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
    for (table, col) in [(t, c) for t in ("messages", "messages_v2", "users") for c in ("data", "replydata", "replies_data")]:
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
    notes["secret_chats"] = str(len(secret_dialogs)) if not undecided_dialogs else ">=%d (%d rows with an undecided dialog)" % (
        len(secret_dialogs), undecided_dialogs)
    notes["self_users"] = str(selfs)
    unmapped = [t for t in present if t not in READ_TABLES]
    if unmapped:
        notes["unmapped_tables"] = note_list(unmapped)
    # the warnings Parse raises after Run (plan "Notes and warnings")
    if len(held) == 2:
        exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:schema", "contains": "telegram-both-generations: messages=%d messages_v2=%d" % (
            len(view["messages"]), len(view["messages_v2"]))})
    for (table, col) in [(t, c) for t in ("messages", "messages_v2", "users") for c in ("data", "replydata", "replies_data")]:
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


def secret_i64(n):
    """A distinctive int64 for every new access_hash and FileLocation.secret (plan 4I4b "Fixture scenario"; R7)."""
    return 0x7E575EC2E7A10000 + n


def thumb(n, k):
    """k bytes of thumbnail content: SYNTHETIC-THUMB-<n>- repeated (D11: only its length and SHA-256 are ever shown)."""
    unit = ("SYNTHETIC-THUMB-%02d-" % n).encode("ascii")
    return (unit * (k // len(unit) + 1))[:k]


def wave(n, k):
    unit = ("SYNTHETIC-WAVE-%02d-" % n).encode("ascii")
    return (unit * (k // len(unit) + 1))[:k]


SEED = b"SYNTHETIC-SEED-0001"


def fname(name):
    return {"_": "TL_documentAttributeFilename", "file_name": name}


def document(n, mime, attributes, ref, size=1000, **kw):
    """A TL_document of id n (its access_hash secret_i64(n), its file_reference ref: a literal secret("4i4b-<site>",
    24) at the call, so the key-material test derives it)."""
    d = {"_": "TL_document", "id": n, "access_hash": secret_i64(n), "file_reference": ref, "date": D, "mime_type": mime,
         "size": size, "dc_id": 2, "attributes": attributes}
    d.update(kw)
    return d


def doc_media(doc, **kw):
    m = {"_": "TL_messageMediaDocument", "document": doc}
    m.update(kw)
    return m


def stage_a_messages(R):
    """R60-R80 of the plan 4I4b "Fixture scenario" (telegram-v181; mid = 100 + rowid)."""
    def m(rid, text, uid=ANN, cols=None, **kw):
        R[rid] = row(rid, 100 + rid, uid, enc(tlmsg(100 + rid, rid, text, frm=pu(ANN), **kw)), **(cols or {}))
    R[60] = row(60, 160, ANN, enc(r60_value()), forwards=4)
    m(61, "Forward count", cols={"forwards": 4}, views=1, forwards=5)
    emoji = lambda e: {"_": "TL_reactionEmoji", "emoticon": e}
    custom = {"_": "TL_reactionCustomEmoji", "document_id": 5000000001}
    m(62, "Reactions", reactions={"_": "TL_messageReactions", "results": [
        {"_": "TL_reactionCount", "chosen": True, "chosen_order": 1, "reaction": emoji("\U0001F44D"), "count": 2},
        {"_": "TL_reactionCount", "reaction": custom, "count": 1},
        {"_": "TL_reactionCount", "reaction": {"_": "TL_reactionPaid"}, "count": 3},
        {"_": "TL_reactionCount", "reaction": {"_": "TL_reactionEmpty"}, "count": 0}],
        "recent_reactions": [
            {"_": "TL_messagePeerReaction", "big": True, "peer_id": pu(ANN), "date": D + 800, "reaction": emoji("❤")},
            {"_": "TL_messagePeerReaction_layer154", "unread": True, "peer_id": pu(SELF), "reaction": custom},
            {"_": "TL_messagePeerReaction_layer144", "peer_id": pu(ANN), "reaction.emoticon": "\U0001F525"},
            {"_": "TL_messagePeerReaction_layer137", "peer_id": 1000009, "reaction.emoticon": "\U0001F602"}],
        "top_reactors": [
            {"_": "TL_messageReactor", "top": True, "peer_id": pu(ANN), "count": 2},
            {"_": "TL_messageReactor", "anonymous": True, "count": 1},
            {"_": "TL_messageReactor_old", "my": True, "peer_id": pu(SELF), "count": 1}]})
    replies = lambda n: {"_": "TL_messageReplies", "comments": True, "replies": n, "replies_pts": 77, "recent_repliers": [pu(ANN)],
                         "channel_id": 3000001, "max_id": 300, "read_max_id": 299}
    m(63, "Thread root", cols={"replies_data": E.encode("MessageReplies", replies(3))}, replies=replies(3))
    m(64, "Thread root", cols={"replies_data": E.encode("MessageReplies", replies(4))}, replies=replies(3))
    m(65, "Checked",
      restriction_reason=[{"_": "TL_reastrictionReason", "platform": "android", "reason": "sensitive", "text": "Hidden on Android"}],
      factcheck={"_": "TL_factCheck", "need_check": True, "country": "US", "hash": 99, "text": {
          "_": "TL_textWithEntities", "text": "Context added", "entities": [{"_": "TL_messageEntityBold", "offset": 0, "length": 7}]}},
      suggested_post={"_": "SuggestedPost", "accepted": True, "price": {"_": "TL_starsAmount", "amount": 50, "nanos": 0},
                      "schedule_date": D + 900})
    m(66, "Suggested", suggested_post={"_": "SuggestedPost", "rejected": True, "price": {"_": "TL_starsTonAmount", "amount": 1000000000}})
    m(67, "Nice photo", cols={"reply_to_message_id": 117}, reply_to={
        "_": "TL_messageReplyHeader", "reply_to_msg_id": 117, "reply_media": {"_": "TL_messageMediaPhoto", "photo": {
            "_": "TL_photo", "id": 9001, "access_hash": secret_i64(9001), "file_reference": secret("4i4b-r67-fileref", 24), "date": D,
            "sizes": [{"_": "TL_photoSize", "type": "m", "w": 320, "h": 240, "size": 12345}], "dc_id": 2}}})
    m(68, "", media=doc_media(document(9102, "image/webp", [
        {"_": "TL_documentAttributeSticker", "mask": True, "alt": "\U0001F600",
         "stickerset": {"_": "TL_inputStickerSetID", "id": 7000001, "access_hash": secret_i64(7000001)},
         "mask_coords": {"_": "TL_maskCoords", "n": 1, "x": 0.5, "y": -0.25, "zoom": 1.5}},
        {"_": "TL_documentAttributeImageSize", "w": 512, "h": 512}], secret("4i4b-r68-fileref", 24))))
    m(69, "", media=doc_media(document(9103, "application/x-tgsticker", [
        {"_": "TL_documentAttributeCustomEmoji", "free": True, "alt": "⭐",
         "stickerset": {"_": "TL_inputStickerSetShortName", "short_name": "FixtureSet"}}], secret("4i4b-r69-fileref", 24))))
    m(70, "", media=doc_media(document(9104, "audio/ogg", [
        {"_": "TL_documentAttributeAudio", "voice": True, "duration": 12, "title": "Memo", "performer": "Ann", "waveform": wave(1, 63)}],
        secret("4i4b-r70-fileref", 24)), voice=True))
    m(71, "", media=doc_media(document(9105, "video/mp4", [
        {"_": "TL_documentAttributeVideo", "round_message": True, "supports_streaming": True, "video_codec": "h264", "duration": 7.5,
         "w": 240, "h": 240, "video_start_ts": 0.5}], secret("4i4b-r71-fileref", 24),
        video_thumbs=[{"_": "TL_videoSize", "type": "u", "w": 240, "h": 240, "size": 3000, "video_start_ts": 0.5}]), round=True))
    m(72, "", media={"_": "TL_messageMediaDice", "value": 5, "emoticon": "\U0001F3B2", "game_outcome": {
        "_": "TL_messages_emojiGameOutcome", "seed": SEED, "stake_ton_amount": 100, "ton_amount": 200}})
    m(73, "", media=doc_media(document(9106, "text/plain", [fname("a.txt"), fname("b.txt")], secret("4i4b-r73-fileref", 24))))
    m(74, "", media=doc_media(document(9107, "text/plain", [fname("same.txt"), fname("same.txt")], secret("4i4b-r74-fileref", 24))))
    m(75, "", media=doc_media(document(9108, "text/plain", [fname("")], secret("4i4b-r75-fileref", 24))))
    R[76] = row(76, 176, ANN, enc(r76_value()))
    m(77, "", media={"_": "TL_messageMediaWebPage", "webpage": {
        "_": "TL_webPage", "id": 9300, "url": "fixture.invalid/p", "display_url": "fixture.invalid/p", "hash": 0, "type": "article",
        "site_name": "Fixture", "title": "A page", "description": "Words",
        "document": document(9301, "application/zip", [fname("bundle.zip")], secret("4i4b-r77-fileref", 24)),
        "attributes": [{"_": "TL_webPageAttributeStickerSet", "stickers": []}]}})
    m(78, "", media=doc_media(document(9401, "video/mp4", [fname("main.mp4")], secret("4i4b-r78-fileref", 24)),
                              alt_documents=[document(9402, "video/mp4", [fname("alt.mp4")], secret("4i4b-r78-altref", 24))]))
    m(79, "", edit_date=D + 50, media={"_": "TL_messageMediaWebPage", "webpage": {
        "_": "TL_webPage", "id": 9500, "url": "fixture.invalid/c", "display_url": "fixture.invalid/c", "hash": 0,
        "cached_page": {"_": "TL_page_layer110"}}})
    R[80] = row(80, -13, S77, enc({"_": "TL_message_secret", "id": -13, "ttl": 0, "from_id": ANN, "peer_id": pu(SELF), "date": D + 80,
                                   "message": "", "entities": [], "media": doc_media({
                                       "_": "TL_documentEncrypted", "id": 7002, "access_hash": secret_i64(7002), "date": D,
                                       "mime_type": "application/pdf", "size": 4096, "dc_id": 1, "attributes": [fname("secret.pdf")],
                                       "thumbs": {"_": "TL_photoCachedSize_layer127", "type": "s", "w": 90, "h": 90, "bytes": thumb(5, 32),
                                                  "location": {"_": "TL_fileEncryptedLocation", "dc_id": 1, "volume_id": 5, "local_id": 6,
                                                               "secret": secret_i64(7003), "key": secret("4i4b-r80-lockey", 24),
                                                               "iv": secret("4i4b-r80-lociv", 24)}},
                                       "key": secret("4i4b-r80-key", 24), "iv": secret("4i4b-r80-iv", 24)})}))


def r60_value():
    """R60: a document with a file name, two thumbs, edit_date, every plain tail field and a text_url entity."""
    doc = document(9101, "application/pdf", [fname("report.pdf"), {"_": "TL_documentAttributeImageSize", "w": 800, "h": 600}],
                   secret("4i4b-r60-fileref", 24), size=48213,
                   thumbs=[{"_": "TL_photoStrippedSize", "type": "i", "bytes": thumb(1, 40)},
                           {"_": "TL_photoSize", "type": "m", "w": 320, "h": 240, "size": 12345}])
    return tlmsg(160, 60, "Report attached", frm=pu(ANN), media=doc_media(doc), edit_date=D + 700, views=9, forwards=4,
                 post_author="Desk", quick_reply_shortcut_id=7, effect=5000000002, report_delivery_until_date=D + 1000,
                 paid_message_stars=10, schedule_repeat_period=86400, summary_from_language="en",
                 entities=[{"_": "TL_messageEntityTextUrl", "offset": 0, "length": 6, "url": "fixture.invalid/r"}])


def r76_value():
    return tlmsg(176, 76, "", frm=pu(ANN), media=r76_media())


def r76_media():
    """R76: a photo with every PhotoSize kind of the plan and three video sizes, and its live-photo document (P9)."""
    return {"_": "TL_messageMediaPhoto", "live_photo": True, "photo": {
        "_": "TL_photo", "id": 9200, "access_hash": secret_i64(9200), "file_reference": secret("4i4b-r76-fileref", 24), "date": D, "dc_id": 2,
        "sizes": [{"_": "TL_photoStrippedSize", "type": "i", "bytes": thumb(2, 24)},
                  {"_": "TL_photoCachedSize", "type": "s", "w": 90, "h": 90, "bytes": thumb(3, 48)},
                  {"_": "TL_photoSizeProgressive", "type": "y", "w": 1280, "h": 960, "sizes": [1000, 5000, 20000]},
                  {"_": "TL_photoPathSize", "type": "j", "bytes": thumb(4, 16)}],
        "video_sizes": [{"_": "TL_videoSize", "type": "u", "w": 800, "h": 800, "size": 4000},
                        {"_": "TL_videoSizeEmojiMarkup", "emoji_id": 5000000003, "background_colors": [1, 2]},
                        {"_": "TL_videoSizeStickerMarkup", "stickerset": {"_": "TL_inputStickerSetEmpty"}, "sticker_id": 5000000004,
                         "background_colors": [3]}]},
        "document": document(9201, "video/mp4", [fname("live.mov")], secret("4i4b-r76-liveref", 24))}


def core_messages():
    """R1-R59 of the plan's CORE table (R51, R52 are written to the WAL by variant_v181), and R60-R80 of plan 4I4b."""
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
    R[29] = row(29, 125, ANN, enc(tlmsg(125, 29, "", frm=pu(ANN), media={"_": "TL_messageMediaDice", "value": 3, "emoticon": "\U0001F3B2"})))  # Dice is full since 4I4b: a well-formed body
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
    stage_a_messages(R)
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
    # plan 4I4b P7, P8: the 5.14.0 media path (recon 2), a TL_message_layer117 photo and document
    l117 = lambda mid, media: {"_": "TL_message_layer117", "id": mid, "from_id": ANN, "peer_id": {"_": "TL_peerUser_layer131", "user_id": SELF},
                               "date": D + 1, "message": "", "media": media}
    P[7] = m(108, ANN, enc(l117(108, {"_": "TL_messageMediaPhoto_layer223", "photo": {
        "_": "TL_photo_layer115", "id": 9601, "access_hash": secret_i64(9601), "file_reference": secret("4i4b-p7-fileref", 24), "date": D,
        "sizes": [{"_": "TL_photoSize_layer127", "type": "m", "w": 320, "h": 240, "size": 12345, "location": {
                      "_": "TL_fileLocation_layer97", "dc_id": 2, "volume_id": 1, "local_id": 2, "secret": secret_i64(9602),
                      "file_reference": secret("4i4b-p7-locref", 24)}},
                  {"_": "TL_photoStrippedSize", "type": "i", "bytes": thumb(6, 24)},
                  {"_": "TL_photoCachedSize_layer127", "type": "s", "w": 90, "h": 90, "bytes": thumb(7, 32), "location": {
                      "_": "TL_fileLocation_layer82", "dc_id": 2, "volume_id": 1, "local_id": 3, "secret": secret_i64(9603)}}],
        "dc_id": 2}})))
    P[8] = m(109, ANN, enc(l117(109, {"_": "TL_messageMediaDocument_layer159", "document": {
        "_": "TL_document_layer113", "id": 9604, "access_hash": secret_i64(9604), "file_reference": secret("4i4b-p8-fileref", 24),
        "date": D, "mime_type": "video/mp4", "size": 1000000, "dc_id": 2, "attributes": [
            fname("IMG_0001.mp4"), {"_": "TL_documentAttributeVideo_layer159", "duration": 30, "w": 1280, "h": 720},
            {"_": "TL_documentAttributeImageSize", "w": 1280, "h": 720}]}})))
    return P


def v84_rows():
    """R1, R2, R4 as the 8.1.1 app wrote them: TL_message_layer123 blobs with layer-131 peers; V5-V8 of plan 4I4b."""
    l123 = lambda mid, rid, text, peer, **kw: dict({"_": "TL_message_layer123", "id": mid, "peer_id": peer, "date": D + rid, "message": text}, **kw)
    lu = lambda i: {"_": "TL_peerUser_layer131", "user_id": i}
    R = {1: row(1, 101, ANN, enc(l123(101, 1, "Hello Ann here", lu(SELF), from_id=lu(ANN)))),
         2: row(2, 102, ANN, enc(l123(102, 2, "Hi Ann", lu(ANN), out=True)), out=1),
         4: row(4, 103, -2000001, enc(l123(103, 4, "Group hello", {"_": "TL_peerChat_layer131", "chat_id": 2000001}, from_id=lu(BOT))))}
    # plan 4I4b V5-V8: legacy media with captionLegacy and an empty message, legacy video, audio and an old document
    empty_thumb = {"_": "TL_photoSizeEmpty", "type": "s"}
    legacy = {
        5: {"_": "TL_messageMediaPhoto_layer74", "captionLegacy": "Old caption", "photo": {
            "_": "TL_photo_layer55", "id": 9701, "access_hash": secret_i64(9701), "date": D, "sizes": []}},
        6: {"_": "TL_messageMediaVideo_layer45", "captionLegacy": "", "video_unused": {
            "_": "TL_video_layer45", "id": 9702, "access_hash": secret_i64(9702), "date": D, "duration": 5, "mime_type": "video/mp4",
            "size": 2000, "thumb": empty_thumb, "dc_id": 2, "w": 640, "h": 480}},
        7: {"_": "TL_messageMediaAudio_layer45", "audio_unused": {
            "_": "TL_audio_layer45", "id": 9703, "access_hash": secret_i64(9703), "date": D, "duration": 9, "mime_type": "audio/mpeg",
            "size": 3000, "dc_id": 2}},
        8: {"_": "TL_messageMediaDocument_old", "document": {
            "_": "TL_document_old", "id": 9704, "access_hash": secret_i64(9704), "user_id": ANN, "date": D, "file_name": "old.doc",
            "mime_type": "application/msword", "size": 4000, "thumbs": empty_thumb, "dc_id": 2}}}
    for rid, media in legacy.items():
        R[rid] = row(rid, 99 + rid, ANN, enc(l123(99 + rid, rid, "", lu(SELF), from_id=lu(ANN), media=media)))
    return R


# ---------------------------------------------------------------- layers and hostile rows


def minimal_row(typ):
    """The full row of typ with the fewest ungated fields (the first in table order on a tie), or None."""
    tab = O.table()
    rows = [r for r in tab.values() if r.typ == typ and r.body == "full"]
    return min(rows, key=lambda r: len([it for it in r.items if it.kind is not None and it.word is None])) if rows else None


def mandatory(r, typ, i, text, nested=False, depth=0):
    """A value of row r with only its ungated fields (a flags word computed), the text given where it has message. An
    object field of a type with a full row holds that type's smallest full row, filled the same way (plan 4I4b Task 4:
    the layers rows reach past their objects); a type with none holds an ID the table lacks. In a nested object an
    access_hash or a FileLocation secret is a secret_i64 (R7)."""
    v = {"_": r.cls}
    for it in r.items:
        if it.kind is None or it.word is not None or it.kind == "#":
            continue
        name = it.name
        k = it.kind
        if name in v:
            continue
        last = name.split(".")[-1]
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
            if nested and k == "i64" and last in ("access_hash", "secret"):
                v[name] = secret_i64(i)
            else:
                v[name] = {"id": i, "date": D, "user_id": ANN, "chat_id": 2000001, "channel_id": 3000001}.get(last, 0)
            if last == "from_id":
                v[name] = ANN
        elif k == "dbl":
            v[name] = 0.0
        elif k.startswith("vec<"):
            v[name] = []
        elif k == "Peer":
            v[name] = pu(SELF) if name == "peer_id" else pu(ANN)
        elif k == "GeoPoint":
            v[name] = {"_": "TL_geoPointEmpty"}
        else:
            obj = minimal_row(k) if depth < 8 else None
            v[name] = mandatory(obj, k, i, "", True, depth + 1) if obj is not None else {"_raw": u32le(0x0BAD0100)}
    return v


def filled(cls, i, **kw):
    """The value of class cls with its ungated fields filled (mandatory, nested), then kw."""
    r = [x for x in O.table().values() if x.cls == cls][0]
    v = mandatory(r, r.typ, i, "", True)
    v.update(kw)
    return v


def embed(r, rid):
    """A message whose blob reads the full row r (plan 4I4b P10): r with its mandatory fields, in the smallest
    container its type allows."""
    v = mandatory(r, r.typ, rid, "", True)
    msg = lambda **kw: tlmsg(rid, rid, "row " + r.cls, frm=pu(ANN), **kw)
    photo = lambda **kw: {"_": "TL_messageMediaPhoto", "photo": filled("TL_photo", rid, **kw)}
    doc = lambda **kw: doc_media(filled("TL_document", rid, **kw))
    reacts = lambda **kw: filled("TL_messageReactions", rid, **kw)
    by_type = {
        "MessageAction": lambda: service(rid, rid, v, frm=pu(ANN)),
        "MessageMedia": lambda: msg(media=v),
        "Photo": lambda: msg(media={"_": "TL_messageMediaPhoto", "photo": v}),
        "Document": lambda: msg(media=doc_media(v)),
        "WebPage": lambda: msg(media={"_": "TL_messageMediaWebPage", "webpage": v}),
        "PhotoSize": lambda: msg(media=photo(sizes=[v])),
        "VideoSize": lambda: msg(media=photo(video_sizes=[v])),
        "FileLocation": lambda: msg(media=photo(sizes=[filled("TL_photoSize_layer127", rid, location=v)])),
        "DocumentAttribute": lambda: msg(media=doc(attributes=[v])),
        "InputStickerSet": lambda: msg(media=doc(attributes=[filled("TL_documentAttributeSticker_layer55", rid, stickerset=v)])),
        "TL_maskCoords": lambda: msg(media=doc(attributes=[filled("TL_documentAttributeSticker", rid, mask_coords=v)])),
        "Video": lambda: msg(media={"_": "TL_messageMediaVideo_old", "video_unused": v}),
        "Audio": lambda: msg(media={"_": "TL_messageMediaAudio_layer45", "audio_unused": v}),
        "MessageReplies": lambda: msg(replies=v),
        "MessageReactions": lambda: msg(reactions=v),
        "ReactionCount": lambda: msg(reactions=reacts(results=[v])),
        "Reaction": lambda: msg(reactions=reacts(results=[filled("TL_reactionCount", rid, reaction=v)])),
        "MessagePeerReaction": lambda: msg(reactions=reacts(recent_reactions=[v])),
        "MessageReactor": lambda: msg(reactions=reacts(top_reactors=[v])),
        "RestrictionReason": lambda: msg(restriction_reason=[v]),
        "TL_factCheck": lambda: msg(factcheck=v),
        "TL_textWithEntities": lambda: msg(factcheck=filled("TL_factCheck", rid, country="", text=v)),
        "SuggestedPost": lambda: msg(suggested_post=v),
        "TL_stars.StarsAmount": lambda: msg(suggested_post=filled("SuggestedPost", rid, price=v)),
        "TL_messages_emojiGameOutcome": lambda: msg(media=filled("TL_messageMediaDice", rid, game_outcome=v)),
        "WebPageAttribute": lambda: msg(media={"_": "TL_messageMediaWebPage", "webpage": filled("TL_webPage", rid, attributes=[v])}),
        "TL_iv.RichText": lambda: msg(reply_to={"_": "TL_messageReplyHeader", "reply_to_msg_id": 101, "quote_rich": [v]}),
        "InputUser": lambda: msg(entities=[{"_": "TL_inputMessageEntityMentionName", "offset": 0, "length": 1, "user_id": v}]),
    }
    return by_type[r.typ]()


STAGE_A_TYPES = ("PhotoSize", "VideoSize", "DocumentAttribute", "FileLocation", "InputStickerSet", "TL_maskCoords", "Video", "Audio",
                 "MessageReplies", "MessageReactions", "Reaction", "ReactionCount", "MessagePeerReaction", "MessageReactor",
                 "RestrictionReason", "TL_factCheck", "TL_textWithEntities", "SuggestedPost", "TL_stars.StarsAmount",
                 "TL_messages_emojiGameOutcome", "WebPageAttribute", "TL_iv.RichText", "InputUser", "MessageMedia", "Photo",
                 "Document", "WebPage", "MessageAction")


def seen_ids(blobs, typ):
    """The constructor IDs the oracle reads in blobs as typ (the top's and every nested object's)."""
    out = set()
    for b in blobs:
        res = O.decode(b, typ)
        if res["top"] is not None and O.row_of(res["top"], typ) is not None:
            out.add(res["top"])
        out |= {f["id"] for f in res["fields"] if f["kind"] == "object"}
    return out


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
    assert 1501 + len(media) <= 1601
    # a secret-kind field on the wire (plan 4I4 Task 10 fix 1, M7): a phone-call reason that carries an encrypted key
    allow = call_action(9601)
    allow["reason"] = {"_": "TL_phoneCallDiscardReasonAllowGroupCall", "encrypted_key": secret("4i4-layers", 8)}
    rows.append(row(1601, 1601, ANN, enc(service(1601, 1601, allow, frm=pu(ANN)))))
    # plan 4I4b P10: one row per full row of the 4I4b types the scenario rows (and the media rows above) do not read
    scenario = [r["data"] for r in list(core_messages().values()) + list(pre84_rows().values()) + list(v84_rows().values()) + rows
                if isinstance(r.get("data"), (bytes, bytearray))]
    seen = seen_ids(scenario, "Message")
    todo = [r for r in ordered if r.typ in STAGE_A_TYPES and r.body == "full" and r.cid not in seen]
    for i, r in enumerate(todo):
        rid = 1701 + i
        rows.append(row(rid, rid, ANN, enc(embed(r, rid))))
    assert 1701 + len(todo) <= 2001
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
    """H1-H12 of the plan; rowids from 2001; plan 4I4b's from 2401. Returns (rows, [(rowid, column) that hold NaN])."""
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
    # key-bearing rows (plan 4I4 Task 10 fix 1, I2): a secret document cut inside its key and one cut inside its iv
    # (the decode stops at the thumbs object, which the table lacks, before either; the bytes are in the cell)
    doc = enc({"_": "TL_message_secret_layer72", "out": True, "id": -20, "ttl": 0, "from_id": SELF, "peer_id": pu(ANN),
               "date": D + 20, "message": "", "media": enc_document([]), "entities": []})
    for field in (secret("4i4-dockey", 32), secret("4i4-dociv", 32)):
        h(doc[:doc.index(field) + len(E.SYNTHETIC_PREFIX) + 6], uid=S77, out=1, read_state=0)
    out.extend(stage_a_hostile_rows())
    return out, frozenset({(nan_row["rowid"], "uid")})


def cuts(rid, value):
    """R<n>'s blob cut at every distinct boundary of field_offsets(deep=True) before its end, in offset order, as rows
    from rid (plan 4I4b H2401, H2601)."""
    blob = enc(value)
    offs = sorted({o for _, o in E.field_offsets("Message", value, deep=True) if o < len(blob)})
    return [row(rid + i, rid + i, ANN, blob[:o]) for i, o in enumerate(offs)]


def stage_a_hostile_rows():
    """H2401... (R76 cut at every deep boundary), H2601... (R60 likewise) and H2801-H2814 of plan 4I4b."""
    rows = cuts(2401, r76_value())
    assert len(rows) < 200, len(rows)
    rows += cuts(2601, r60_value())
    assert rows[-1]["rowid"] < 2801, rows[-1]["rowid"]

    def photo_with(**kw):
        p = {"_": "TL_photo", "id": 9800, "access_hash": secret_i64(9800), "file_reference": secret("4i4b-h28-fileref", 24), "date": D,
             "dc_id": 2, "sizes": []}
        p.update(kw)
        return {"_": "TL_messageMediaPhoto", "photo": p}

    def doc_with(attributes):
        return doc_media(document(9801, "video/mp4", attributes, secret("4i4b-h28-docref", 24)))
    size = lambda i: {"_": "TL_photoSize", "type": "m", "w": i, "h": i, "size": i}
    video = {"_": "TL_documentAttributeVideo", "duration": 1.0, "w": 2, "h": 2}
    old_count = u32le(0x6FB250D1) + u32le(0) + E.tl_bytes("👍".encode("utf-8")) + struct.pack("<i", 2)
    new_count = u32le(0x6FB250D1) + u32le(0) + E.encode("Reaction", {"_": "TL_reactionEmoji", "emoticon": "👍"}) + struct.pack("<i", 2)
    one = lambda i: {"_": "TL_reactionCount", "reaction": {"_": "TL_reactionEmoji", "emoticon": "e%d" % i}, "count": i}
    reactions = lambda **kw: dict({"_": "TL_messageReactions", "results": []}, **kw)
    media_rows = [
        photo_with(sizes=[size(i) for i in range(17)]),  # 2801: 17 sizes, 16 kept
        photo_with(sizes={"_raw": u32le(E.VECTOR_ID) + struct.pack("<i", 10001)}),  # 2802: a count over MaxVector
        photo_with(video_sizes=[{"_": "TL_videoSize", "flags": 1 << 5, "type": "u", "w": 1, "h": 1, "size": 1}]),  # 2803
        doc_with([dict(video, flags=1 << 9)]),  # 2804: a Video attribute with flags bit 9
        doc_with([dict(video, duration=float("nan"))]),  # 2805: a NaN duration
    ]
    kw_rows = [
        {"reactions": reactions(results=[{"_raw": old_count}])},  # 2806: 0x6fb250d1 as the older pins read it
        {"reactions": reactions(results=[{"_raw": new_count}])},  # 2807: as A-HEAD reads it
        {"reactions": reactions(results=[one(i) for i in range(65)])},  # 2808
        {"restriction_reason": [{"_": "TL_reastrictionReason", "platform": "android", "reason": "r%d" % i, "text": "t"} for i in range(17)]},
        {"entities": [{"_": "TL_messageEntityBold", "offset": i, "length": 1} for i in range(65)]},  # 2810
        {"reactions": reactions(top_reactors=[{"_": "TL_messageReactor", "peer_id": pu(ANN), "count": i} for i in range(17)])},
        {"replies": {"_": "TL_messageReplies", "replies": 17, "replies_pts": 1, "recent_repliers": [pu(1000100 + i) for i in range(17)]}},
    ]
    attr_rows = [
        [{"_": "TL_documentAttributeImageSize", "w": i, "h": i} for i in range(33)],  # 2813: 33 attributes
        [fname("f%d.txt" % i) for i in range(9)],  # 2814: 9 distinct filename attributes
    ]
    values = [dict(media=m) for m in media_rows] + kw_rows + [dict(media=doc_with(a)) for a in attr_rows]
    for i, kw in enumerate(values):
        rid = 2801 + i
        rows.append(row(rid, rid, ANN, enc(tlmsg(rid, rid, "hostile %d" % rid, frm=pu(ANN), **kw))))
    assert rows[-1]["rowid"] == 2814
    return rows


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
    fill(con, plain, enc=enc_chat_rows())  # the enc_chats key columns (plan 4I4 Task 10 fix 1, I2)
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
    """R1, R2, R4 live with the users SELF and ANN, the chats row of R4's group, the enc_chats rows (their key columns
    hold the key-material chains, C1 of plan 4I4 Task 14), and the contacts and users_data rows of ANN and of user
    1000012; in a WAL that is never checkpointed: rows 201-205 (ANN), 206 (an outgoing secret message of chat 77 with
    a secret document, its key and IV) and 207 (a message of the group 2000001) and the users row 1000012 inserted
    (committed), then deleted (a later frame), so each recovered row has live users, chats or enc_chats rows that it
    must not join (B2); R1 edited and R2 rewritten with REPLACE INTO (the pinned write_verb.messages_v2.edit and .put,
    T2: each gets a new rowid; the old image stays in the database file and in earlier frames). secure_delete is ON."""
    path = os.path.join(work, "cache4.db")
    con = create_db(path, "v181.sql", 181)
    R, U = core_messages(), user_rows()
    live = pick(R, (1, 2, 4))
    fill(con, live, pick(U, (1, 2)), pick(chat_rows(), (1,)), enc_chat_rows(), [(ANN, 1), (GONE_USER, 1)],
         [(ANN, "hi there"), (GONE_USER, "gone about")])
    scen = SCENARIOS["telegram-deleted"] = C.Scenario(CLAIMED, alias={"users": "uid", "messages": "mid"})
    scen.write("messages_v2", scen.snap(con, "messages_v2"))
    scen.write("users", scen.snap(con, "users"))
    old = scen.snap(con, "messages_v2", "rowid in (1, 2)")
    con.execute("pragma wal_checkpoint(truncate)")
    con.close()
    gone = [row(200 + i, 200 + i, ANN, enc(tlmsg(200 + i, 200 + i, "deleted message %d" % i, frm=pu(ANN)))) for i in range(1, 6)]
    gone.append(row(206, -12, S77, enc({"_": "TL_message_secret_layer72", "out": True, "id": -12, "ttl": 0, "from_id": SELF,
                                         "peer_id": pu(ANN), "date": D + 206, "message": "deleted secret",
                                         "media": enc_document([]), "entities": []}), out=1))
    gone.append(row(207, 207, -2000001, enc(tlmsg(207, 207, "deleted group message", peer=pc(2000001), frm=pu(BOT)))))
    cols = list(gone[0])
    gone_user = {"uid": GONE_USER, "name": "gone user", "status": 0,
                 "data": E.encode("User", {"_": "TL_user", "access_hash": 1, "id": GONE_USER, "first_name": "Gone", "phone": "15550000012"})}
    edited = dict(R[1], data=enc(tlmsg(101, 1, "Hello Ann here (edited)", frm=pu(ANN), edit_date=D + 600)))
    edited.pop("rowid")
    put = dict(R[2])
    put.pop("rowid")
    stmts = ["pragma secure_delete = ON", "pragma wal_autocheckpoint = 0", "BEGIN"] + [insert_sql("messages_v2", g, cols) for g in gone] + [
        insert_sql("users", gone_user, list(gone_user)), "COMMIT", "delete from messages_v2 where rowid between 201 and 207",
        "delete from users where uid = %d" % GONE_USER,
        insert_sql("messages_v2", edited, cols, "replace"), insert_sql("messages_v2", put, cols, "replace")]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    gone_snap = [(g["rowid"], {k: v for k, v in g.items() if k != "rowid"}) for g in gone]
    scen.write("messages_v2", gone_snap)
    scen.want("messages_v2", gone_snap + old)
    user_snap = [(GONE_USER, gone_user)]
    scen.write("users", user_snap)
    scen.want("users", user_snap)
    return files, oracle(files, "v181.sql", "telegram-deleted", 181)


def recover_expect(variant, files):
    """The <variant>.recover.expect.json: every recovered row of a claimed table mapped with env None (4I4-P6: it joins
    nothing) and its raw.live_key_state from its own key against the live rows of its table (T2): (mid, uid) in
    messages_v2, mid alone in messages (T9: the pre-84 put replaces the row of its mid)."""
    view = C.engine_view(files, ["messages", "messages_v2"])
    keys = {("messages_v2", r.get("mid"), r.get("uid")) for r in view.get("messages_v2", [])}
    keys |= {("messages", r.get("mid"), None) for r in view.get("messages", [])}

    def expect_fn(table, d, env, file):
        if table == "users":
            rec = map_user(d, None, "", file)
            rec.pop("_warn")
            return rec
        rec = map_message(table, d, None, "", file)
        mid, uid = d.get("mid"), d.get("uid")
        if table == "messages":
            uid = None  # the key is mid alone (T9)
        raw = rec["payload"]["raw"]
        if is_int(mid) and (uid is None and table == "messages" or is_int(uid)):
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

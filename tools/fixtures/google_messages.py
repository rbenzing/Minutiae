"""Google Messages / AOSP Messaging (bugle_db) fixtures: variants, scenario rows, oracle (plan 4I5, spec 10).

The oracle (oracle(), message_record(), part_record(), recovered_record()) restates spec 6-8 and the plan's state
vocabularies in Python over the values read back from the engine on a COPY of the finished files (C.engine_view),
never over parser output. Every code, label, contract value, the self rule, the sentinel and the URI authorities come
from schemas/google-messages/facts.tsv; no code, label or contract value is a literal of this module. It shares no code
with the Go parser. Independence note (spec 10): the oracle, the generator and the Go table derive from the same
facts; their independent check is facts.tsv against the pinned AOSP source (CROSSCHECK.md).

Choices the spec leaves open, made here and pinned by the committed oracles (a later task that decides otherwise
regenerates them): the Summary counterparty is the from address, else the first member address, else "" (4I5-P3); raw
members_total is stated on every record whose conversation row exists (the non-self members before the cut); a NULL
received_timestamp is no value, so B5 falls back to the sent time as it does on a stored 0; the join states and their
keys are the plan's "State vocabularies".
"""
import hashlib
import os
import re
import sqlite3
import struct
import shutil
import tempfile

import telephony_common as C

FAMILY = "google-messages"
CLAIMED = ("messages", "parts")  # Meta.Claims of android.googlemessages (spec 8.1)
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle)
SCHEMAS = os.path.join(C.HERE, "schemas", "google-messages")
REMOTE = "/data/data/com.google.android.apps.messaging/databases/bugle_db"
T = 1700000000000  # 2023-11-14T22:13:20Z
MAX_BODY = 256 << 10
MAX_SHORT = 4 << 10  # uri, content_type, mms_content_location, mms_transaction_id
MAX_NAME = 1 << 10
MAX_IDENT = 1 << 10  # identifiers are never cut: over it they are absent with their state
MAX_LIST = 1000
READ_TABLES = ("messages", "parts", "participants", "conversations", "conversation_participants")
MESSAGES_MAPPED = [("conversation_id", True), ("sender_id", True), ("received_timestamp", True), ("message_protocol", True),
                   ("message_status", True), ("sent_timestamp", False), ("seen", False), ("read", False), ("sms_message_uri", False),
                   ("sms_priority", False), ("sms_message_size", False), ("mms_subject", False), ("mms_transaction_id", False),
                   ("mms_content_location", False), ("mms_expiry", False), ("raw_status", False), ("self_id", False),
                   ("retry_start_timestamp", False)]
PARTS_MAPPED = [("message_id", True), ("text", True), ("content_type", True), ("uri", True), ("width", False), ("height", False),
                ("timestamp", False), ("conversation_id", False)]
CONVERSATION_COLS = ("name", "sms_thread_id", "archive_status", "participant_count", "sort_timestamp", "last_read_timestamp", "IS_ENTERPRISE")


# ---------------------------------------------------------------- the facts (never a Python literal)


def load_facts():
    with open(os.path.join(SCHEMAS, "facts.tsv"), encoding="utf-8") as f:
        lines = f.read().split("\n")
    head = lines[0].split("\t")
    facts = {}
    for ln in lines[1:]:
        if ln:
            row = dict(zip(head, ln.split("\t")))
            facts[row["key"]] = row
    return facts


FACTS = load_facts()


def code_table(prefix, strip):
    out = {}
    for key, row in FACTS.items():
        if key.startswith(prefix + "."):
            const = row["constant"]
            if not const.startswith(strip):
                raise SystemExit("facts.tsv: %s does not start with %s" % (const, strip))
            out[int(row["value"])] = (const[len(strip):].lower(), row["contract"])
    return out


PROTOCOLS = code_table("message_protocol", "PROTOCOL_")  # code -> (label, channel)
STATUSES = code_table("message_status", "BUGLE_STATUS_")  # code -> (label, "<direction>/<status>")
OTHER_THAN_SELF = int(FACTS["self.other_than_self_sub_id"]["value"])  # a participant is self when sub_id != this (self.is_self)
CONTACT_UNRESOLVED = int(FACTS["contact_id.unresolved"]["value"])
TIME_SENTINEL = int(FACTS["time.sentinel"]["value"])
if FACTS["time.unit"]["value"] != "ms" or FACTS["self.is_self"]["value"] != "sub_id != %d" % OTHER_THAN_SELF:
    raise SystemExit("facts.tsv: the time unit or the self rule is not the one this oracle restates")
LINK_PREFIXES = {a: "content://%s/" % a for a in ("sms", "mms")
                 if FACTS["uri." + a]["value"] == "content://" + a}
LINK_REUSE = {"sms": "possible" if FACTS["sms_id.autoincrement"]["value"] == "no" else "none",
              "mms": "possible" if FACTS["pdu_id.autoincrement"]["value"] == "no" else "none"}


# ---------------------------------------------------------------- cells


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool) and not isinstance(v, C.Blob)


def int_cell(r, col, unknown, key=None, nan=frozenset(), table="messages"):
    """(value, state): state 'missing' (no such column), 'null', 'int', or 'bad' (REAL, NaN, TEXT or BLOB: listed in
    unknown_columns under key, never read as NULL)."""
    if col not in r:
        return None, "missing"
    v = r[col]
    if (table, r.get("rowid"), col) in nan:
        unknown[key or col] = "type-mismatch"
        return None, "bad"
    if v is None:
        return None, "null"
    if is_int(v):
        return v, "int"
    unknown[key or col] = "type-mismatch"
    return None, "bad"


def text_cell(r, col, unknown, key=None):
    """The TEXT cell as bytes, None for NULL or a missing column; another class is listed in unknown_columns."""
    v = r.get(col)
    if v is None:
        return None
    if isinstance(v, bytes) and not isinstance(v, C.Blob):
        return v
    unknown[key or col] = "type-mismatch"
    return None


def cleaned(raw, field, b, limit):
    """common.CleanText restated for a free-text field without a flag slot of its own: an altered text is listed in
    raw.text_flags."""
    c = C.clean(b, limit)
    if c.flags:
        C.mark(raw, field)
    return c


def ident(raw, key, b, limit=MAX_IDENT):
    """An identifier is never cut: over limit, as stored or once each invalid byte is U+FFFD (ruling 4I5-T4c6), it is absent
    with its state, length and SHA-256."""
    if len(b) <= limit:
        c = C.clean(b, 3 * limit)
        if len(c.text.encode()) <= limit:
            if c.flags:
                C.mark(raw, key)
            return c.text
    raw[key + "_state"] = "too-long"
    raw[key + "_len"] = len(b)
    raw[key + "_sha256"] = hashlib.sha256(b).hexdigest()
    return None


def message_summary(channel, direction, counterparty, text):
    """message.Summary for what the fixtures state: ASCII only, text of at most 80 characters (else not compared)."""
    if not (channel + direction + counterparty + text).isascii() or len(counterparty.encode()) > 64:
        return None
    one_line = re.sub("[" + chr(0) + "-" + chr(32) + chr(127) + "]+", " ", text).strip(" ")
    if len(one_line) > 80:
        return None
    return (" ".join([channel, direction, counterparty]) + ": " + one_line).rstrip(" ")


def body_fields(p, c):
    if c.flags:
        p["body_flags"] = c.flags
    if c.truncated:
        p["body_truncated"], p["body_total_bytes"] = True, c.total
    if c.b64:
        p["body_raw_b64"], p["body_raw_len"] = c.b64, c.rawlen
    if c.sha:
        p["body_raw_sha256"], p["body_raw_len"] = c.sha, c.rawlen


def source_path(file, db_only=False):
    if file == "any":
        return REMOTE if db_only else ""
    return REMOTE + {"db": "", "wal": "-wal", "journal": "-journal"}[file]


# ---------------------------------------------------------------- the live view the joins read


class Env:
    def __init__(self, view, nan=frozenset()):
        self.view, self.nan = view, nan
        self.messages = {r["rowid"]: r for r in view.get("messages", [])}
        self.participants = {r["rowid"]: r for r in view.get("participants", [])}
        self.has_conversations = "conversations" in view
        self.conversations = {r["rowid"]: r for r in view.get("conversations", [])}
        self.members = {}
        for r in view.get("conversation_participants", []):
            if is_int(r.get("conversation_id")):
                self.members.setdefault(r["conversation_id"], []).append(r)
        self.parts = {}
        for r in view.get("parts", []):
            if is_int(r.get("message_id")):
                self.parts.setdefault(r["message_id"], []).append(r)
        self.cut = []  # (conversation id, total) of each conversation cut at MAX_LIST, in first-seen order
        self.conflicts = 0


def self_of(prow, unknown):
    """'self', 'other' or 'undecided' (sub_id NULL or not an integer): the self.is_self rule."""
    v = prow.get("sub_id")
    if is_int(v):
        return "other" if v == OTHER_THAN_SELF else "self"
    if v is not None:
        unknown["participants.sub_id"] = "type-mismatch"
    return "undecided"


def party_of(prow, role, raw, prefix, unknown):
    """The participant dict of a non-self participants row, or None with the reason in raw (R9)."""
    dests = {}
    for col in ("normalized_destination", "send_destination", "display_destination"):
        b = text_cell(prow, col, unknown, "participants." + col)
        if b is not None:
            dests[col] = b
    if prefix:
        shown = {}
        for col, b in dests.items():
            if len(b) <= MAX_IDENT:
                c = C.clean(b, 3 * MAX_IDENT)
                if len(c.text.encode()) <= MAX_IDENT:  # never cut (ruling 4I5-T4c6)
                    if c.flags:
                        C.mark(raw, prefix + "_destinations")
                    shown[col] = c.text
        if shown:
            raw[prefix + "_destinations"] = shown
    chosen = None
    for col in ("send_destination", "normalized_destination", "display_destination"):
        if dests.get(col):
            chosen = col
            break
    if chosen is None:
        if prefix:
            raw[prefix + "_address_state"] = "empty"
        return None
    b = dests[chosen]
    if len(b) > MAX_IDENT or len(C.clean(b, 3 * MAX_IDENT).text.encode()) > MAX_IDENT:
        if prefix:
            raw[prefix + "_address_state"] = "too-long"
            raw[prefix + "_address_len"] = len(b)
            raw[prefix + "_address_sha256"] = hashlib.sha256(b).hexdigest()
        return None
    c = C.clean(b, MAX_IDENT)
    if c.flags:
        if prefix:
            raw[prefix + "_address_state"] = "altered"
            raw[prefix + "_address_text"] = c.text
        return None
    if prefix:
        raw[prefix + "_address_basis"] = chosen
    party = {"role": role, "address": c.text, "kind": C.addr_kind(c.text)}
    full = text_cell(prow, "full_name", unknown, "participants.full_name")
    cid, cst = int_cell(prow, "contact_id", unknown, "participants.contact_id", table="participants")
    if full is not None:
        fc = cleaned(raw, prefix + "_full_name" if prefix else "member_full_name", full, MAX_NAME)
        if prefix and fc.text:
            raw[prefix + "_full_name"] = fc.text
        if cst == "int" and cid != CONTACT_UNRESOLVED and fc.text:
            party["name"] = fc.text
            if prefix:
                raw[prefix + "_name_basis"] = "cached_contact"
    if prefix and cst == "int":
        raw[prefix + "_contact_id"] = cid
    return party


# ---------------------------------------------------------------- a message record


def classify_time(raw, col, v):
    s = C.classify_sms_date(v)
    raw[col] = v
    if s != "valid":
        raw[col + "_status"] = s
    return s


def times_block(r, raw, unknown, direction, nan):
    """B5, 4I5-P2: (Record.Time or None, Times) and raw.time_basis."""
    got = {}
    for col in ("received_timestamp", "sent_timestamp"):
        v, st = int_cell(r, col, unknown, nan=nan)
        if st == "int":
            got[col] = (v, classify_time(raw, col, v))
        else:
            got[col] = (None, st)
    rv, rs = got["received_timestamp"]
    sv, ss = got["sent_timestamp"]
    t, basis = None, None
    if rs in ("valid",):
        t, basis = C.iso(rv, "ms"), "received"
    elif rs in ("sentinel", "invalid") and rv != TIME_SENTINEL:
        basis = "invalid"
    elif rs == "bad":
        basis = "undecided"
    else:  # a stored 0, NULL or no column: the sent time is the fallback
        if ss == "valid" and sv != TIME_SENTINEL:
            t, basis = C.iso(sv, "ms"), "sent"
        elif ss in ("sentinel", "invalid") and sv != TIME_SENTINEL:
            basis = "invalid"
        elif ss == "bad":
            basis = "undecided"
        else:
            basis = "absent"
    raw["time_basis"] = basis
    times = {}
    if rs == "valid" and direction == "in":
        times["received"] = C.iso(rv, "ms")
    if ss == "valid":
        times["sent"] = C.iso(sv, "ms")
    return t, times


def sms_link(raw, unknown, r):
    b = text_cell(r, "sms_message_uri", unknown)
    if b is None:
        return
    text = ident(raw, "sms_message_uri", b)
    if text is None:
        return
    raw["sms_message_uri"] = text
    for auth, prefix in LINK_PREFIXES.items():
        if text.startswith(prefix):
            tail = text[len(prefix):]
            if 1 <= len(tail) <= 19 and tail.isascii() and tail.isdigit() and int(tail) < 2**63:
                raw["sms_link"] = {"authority": auth, "id": int(tail)}
                raw["sms_link_id_reuse"] = LINK_REUSE[auth]
                return
    raw["sms_link_state"] = "unparsed"


def body_of(plist, p, raw, unknown):
    """spec 6.4 and 6.5 over the parts of one message (rowid order): (body text or None, kind)."""
    total = len(plist)
    if total == 0:
        raw["parts"] = 0
        raw["body_state"] = "no-parts"
        return None, "unknown", []
    if total > MAX_LIST:
        raw["parts_total"], raw["parts_truncated"] = total, True
        plist = plist[:MAX_LIST]
    raw["parts"] = len(plist)
    texts, atts, apart, both, empty = [], [], [], 0, 0
    for q in plist:
        tx = text_cell(q, "text", unknown, "parts.text")
        uri = text_cell(q, "uri", unknown, "parts.uri")
        if tx is not None:
            texts.append(tx)
        if uri is not None:
            a = {}
            ct = text_cell(q, "content_type", unknown, "parts.content_type")
            mime = cleaned(raw, "attachments.mime", ct, MAX_SHORT) if ct is not None else None
            if mime and mime.text:
                a["mime"] = mime.text
            ref = cleaned(raw, "attachments.source_ref", uri, MAX_SHORT)
            if ref.text:
                a["source_ref"] = ref.text
            atts.append(a)
            ap = {"part_id": q["rowid"]}
            for col in ("width", "height"):
                v = q.get(col)
                if is_int(v):
                    ap[col] = v
            apart.append(ap)
        if tx is not None and uri is not None:
            both += 1
        if tx is None and uri is None:
            empty += 1
    if both:
        raw["parts_text_and_uri"] = both
    if empty:
        raw["parts_empty"] = empty
    if apart:
        raw["attachment_parts"] = apart
    if atts:
        p["attachments"] = atts
    kind = "media" if atts else ("text" if texts else "unknown")
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
        c = C.clean(buf, MAX_BODY)
        if len(joined) > len(buf):
            c.truncated, c.total = True, len(joined)
            if "truncated" not in c.flags:
                c.flags.append("truncated")
        bt = c.text
        p["text_source"] = "part"
        body_fields(p, c)
    return bt, kind, atts


def thread_and_members(env, r, raw, unknown, p):
    """The conversation join (thread, members, group) of a message row. Returns (thread or None, kept members as
    [(participants id, selfness, party or None)] or None when the conversation is not decided)."""
    cid, cst = int_cell(r, "conversation_id", unknown)
    if cst == "int":
        raw["conversation_id"] = cid
    elif cst == "null":
        raw["conversation_state"] = "null"
        return None, None
    else:
        raw["conversation_state"] = "undecided"
        return None, None
    th = {"id": str(cid)}
    crow = env.conversations.get(cid) if env.has_conversations else None
    if crow is None:
        raw["conversation_state"] = "no-conversations-row"
        return th, None
    for col, key in (("sms_thread_id", "sms_thread_id"), ("archive_status", "archive_status"),
                     ("participant_count", "participant_count"), ("IS_ENTERPRISE", "is_enterprise")):
        if col in crow:
            v, st = int_cell(crow, col, unknown, "conversations." + col)
            if st == "int":
                raw[key] = v
    for col in ("sort_timestamp", "last_read_timestamp"):
        if col in crow:
            v, st = int_cell(crow, col, unknown, "conversations." + col)
            if st == "int":
                classify_time(raw, col, v)
    nm = text_cell(crow, "name", unknown, "conversations.name")
    if nm is not None:
        c = cleaned(raw, "thread.title", nm, MAX_NAME)
        if c.text:
            th["title"] = c.text
    kept, dangling, undecided, blocked = [], 0, False, []
    group_unreadable = False
    for mrow in env.members.get(cid, []):
        pid = mrow.get("participant_id")
        if pid is not None and not is_int(pid):  # unreadable: undecided, never dangling (Task 5 review M2)
            unknown["conversation_participants.participant_id"] = "type-mismatch"
            group_unreadable = True
            continue
        prow = env.participants.get(pid) if is_int(pid) else None
        if prow is None:
            dangling += 1
            continue
        sx = self_of(prow, unknown)
        if sx == "self":
            continue
        undecided = undecided or sx == "undecided"
        kept.append((pid, sx, prow))
    total = len(kept)
    raw["members_total"] = total
    if total > MAX_LIST:
        raw["members_cut"] = True
        kept = kept[:MAX_LIST]
        if (cid, total) not in env.cut:
            env.cut.append((cid, total))
    members, without = [], 0
    for pid, sx, prow in kept:
        party = party_of(prow, "member", {}, None, {})
        if party is None:
            without += 1
        else:
            members.append(party)
        b = prow.get("blocked")
        if is_int(b) and b == 1:
            blocked.append(pid)
    if dangling:
        raw["members_dangling"] = dangling
    if without:
        raw["members_without_address"] = without
    if blocked:
        raw["blocked_member_ids"] = blocked
    if undecided:
        raw["self_state"] = "undecided"
    if undecided or group_unreadable:
        raw["group_state"] = "undecided"
    else:
        th["group"] = total > 1
    if total == 0:
        raw["members_state"] = "none"
    addrs = [m["address"] for m in members]
    if addrs:
        th["members"] = addrs
    return th, members


def message_record(r, env, remote, file, nan=frozenset(), joined=True):
    """spec 6.1-6.8, 7 and 4I5-P3 for one messages row (joined False: a recovered row, spec 8.2, which joins nothing)."""
    raw, unknown = {}, {}
    raw["app"], raw["app_basis"] = "google_messages", "path"
    # codes (R1-R3)
    channel, direction, status = "unknown", "unknown", "unknown"
    v, st = int_cell(r, "message_protocol", unknown, nan=nan)
    if st == "int":
        raw["message_protocol"] = v
        if v in PROTOCOLS:
            raw["label_message_protocol"], channel = PROTOCOLS[v]
    elif st == "null":
        raw["message_protocol_state"] = "null"
    v, st = int_cell(r, "message_status", unknown, nan=nan)
    if st == "int":
        raw["message_status"] = v
        if v in STATUSES:
            raw["label_message_status"] = STATUSES[v][0]
            direction, status = STATUSES[v][1].split("/")
    elif st == "null":
        raw["message_status_state"] = "null"
    for col in ("read", "seen", "sms_priority", "sms_message_size", "raw_status"):
        v, st = int_cell(r, col, unknown, nan=nan)
        if st == "int":
            raw[col] = v
    v, st = int_cell(r, "retry_start_timestamp", unknown, nan=nan)
    if st == "int":
        raw["retry_start_timestamp"] = v
        raw["retry_start_timestamp_unit"] = "unestablished"
    v, st = int_cell(r, "mms_expiry", unknown, nan=nan)
    if st == "int":
        classify_time(raw, "mms_expiry", v)
    for col in ("mms_transaction_id", "mms_content_location"):
        b = text_cell(r, col, unknown)
        if b is not None:
            raw[col] = cleaned(raw, col, b, MAX_SHORT).text
    sms_link(raw, unknown, r)
    rd, rst = int_cell(r, "read", {}, nan=nan)
    p = {"channel": channel, "status": status}
    if rst == "int" and rd in (0, 1):
        p["read"] = rd == 1
    sub = text_cell(r, "mms_subject", unknown)
    if sub is not None:
        c = cleaned(raw, "subject", sub, MAX_BODY)
        if c.text:
            p["subject"] = c.text
    if not joined:
        # a recovered row joins nothing (spec 8.2, 4I3 B2): its own columns only
        for col in ("sender_id", "self_id", "conversation_id"):
            v, st = int_cell(r, col, unknown, nan=nan)
            if st == "int":
                raw[col] = v
        for key in ("body_state", "sender_state", "self_row_state", "conversation_state", "members_state"):
            raw[key] = "not-joined-recovered"
        p["direction"] = direction
        if is_int(r.get("conversation_id")):
            p["thread"] = {"id": str(r["conversation_id"])}
        p["participants"], p["participants_unknown"], p["kind"] = [], True, "unknown"
        t, times = times_block(r, raw, unknown, direction, nan)
        counterparty, bt = "", None
    else:
        # sender, direction cross-check (B7), self row
        sid, sst = int_cell(r, "sender_id", unknown, nan=nan)
        sender, sender_self = None, None
        if sst == "int":
            raw["sender_id"] = sid
            sender = env.participants.get(sid)
            if sender is None:
                raw["sender_state"] = "no-participants-row"
            else:
                sx = self_of(sender, unknown)
                if sx == "undecided":
                    raw["sender_state"] = "self-undecided"
                else:
                    sender_self = sx == "self"
                    raw["sender_is_self"] = sender_self
        elif sst == "null":
            raw["sender_state"] = "absent"
        else:
            raw["sender_state"] = "undecided"
        if direction != "unknown" and sender_self is not None and (direction == "in") == sender_self:
            raw["direction_conflict"] = {"status_direction": direction, "sender_is_self": sender_self}
            direction = "unknown"
            env.conflicts += 1
        p["direction"] = direction
        fid, fst = int_cell(r, "self_id", unknown, nan=nan)
        if fst == "int":
            raw["self_id"] = fid
            selfrow = env.participants.get(fid)
            if selfrow is None:
                raw["self_row_state"] = "no-participants-row"
            else:
                for col, key in (("sub_id", "self_sub_id"), ("sim_slot_id", "self_sim_slot_id")):
                    x, xst = int_cell(selfrow, col, unknown, "participants." + col, table="participants")
                    if xst == "int":
                        raw[key] = x
                        if col == "sub_id" and x >= 0:
                            p["subscription"] = x
                nm = text_cell(selfrow, "subscription_name", unknown, "participants.subscription_name")
                if nm is not None:
                    raw["self_subscription_name"] = cleaned(raw, "self_subscription_name", nm, MAX_NAME).text
        elif fst == "null":
            raw["self_row_state"] = "absent"
        elif fst == "bad":  # a column the store lacks states nothing (B4)
            raw["self_row_state"] = "undecided"
        # conversation, members
        th, members = thread_and_members(env, r, raw, unknown, p)
        if th:
            p["thread"] = th
        # participants (4I5-P3)
        parts_out, counterparty = [], ""
        if direction == "out":
            if members:
                parts_out = members
                raw["members_basis"] = "current"
                counterparty = members[0]["address"]
        elif sender is not None and sender_self is False:
            party = party_of(sender, "from", raw, "from", unknown)
            if party:
                parts_out = [party]
                counterparty = party["address"]
        elif sender_self:
            raw["sender_state"] = "self"
        p["participants"] = parts_out
        if not parts_out:
            p["participants_unknown"] = True
        # time
        t, times = times_block(r, raw, unknown, direction, nan)
        # body and parts
        bt, kind, _ = body_of(env.parts.get(r["rowid"], []), p, raw, unknown)
        p["kind"] = kind
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    return {"table": "messages", "rowid": r["rowid"], "file": file, "type": "message", "source_path": remote, "time": t,
            "time_end": None, "times": times, "deleted": False,
            "summary": message_summary(channel, direction, counterparty, bt or ""), "body": bt, "payload": p}


def part_payload(q, unknown, raw, p):
    """The body keys of a part that is a record of its own (an orphan, a recovered part): (body text, counterparty)."""
    tx = text_cell(q, "text", unknown, "text")
    uri = text_cell(q, "uri", unknown, "uri")
    bt = None
    if tx is not None:
        c = cleaned(raw, "body", tx, MAX_BODY)
        bt = c.text
        p["text_source"] = "part"
        body_fields(p, c)
    atts = []
    if uri is not None:
        a = {}
        ct = text_cell(q, "content_type", unknown, "content_type")
        mime = cleaned(raw, "attachments.mime", ct, MAX_SHORT) if ct is not None else None
        if mime and mime.text:
            a["mime"] = mime.text
        ref = cleaned(raw, "attachments.source_ref", uri, MAX_SHORT)
        if ref.text:
            a["source_ref"] = ref.text
        atts.append(a)
        ap = {"part_id": q["rowid"]}
        for col in ("width", "height"):
            if is_int(q.get(col)):
                ap[col] = q[col]
        raw["attachment_parts"] = [ap]
        p["attachments"] = atts
    if tx is not None and uri is not None:
        raw["parts_text_and_uri"] = 1
    if tx is None and uri is None:
        raw["parts_empty"] = 1
    p["kind"] = "media" if atts else ("text" if tx is not None else "unknown")
    return bt


def part_record(q, env, remote, file, nan=frozenset()):
    """An orphan part of the live parse (R11): its own columns only, joined to nothing."""
    raw, unknown = {}, {}
    raw["app"], raw["app_basis"] = "google_messages", "path"
    p = {"channel": "unknown", "direction": "unknown"}
    raw["part_rowid"] = q["rowid"]
    raw["part_state"] = "orphan"
    mid, mst = int_cell(q, "message_id", unknown, table="parts")
    if mst == "int":
        raw["part_message_id"] = mid
    elif mst == "null":
        raw["part_message_id_state"] = "null"
    ts, tst = int_cell(q, "timestamp", unknown, table="parts")
    if tst == "int":
        classify_time(raw, "timestamp", ts)
    cid, cst = int_cell(q, "conversation_id", unknown, table="parts")
    if cst == "int":
        raw["conversation_id"] = cid
        p["thread"] = {"id": str(cid)}
    bt = part_payload(q, unknown, raw, p)
    p["participants"] = []
    p["participants_unknown"] = True
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    return {"table": "parts", "rowid": q["rowid"], "file": file, "type": "message", "source_path": remote, "time": None,
            "time_end": None, "times": {}, "deleted": False, "summary": message_summary("unknown", "unknown", "", bt or ""),
            "body": bt, "payload": p}


def db_meta(files):
    """user_version, the columns of each read table and the journal mode, from a COPY of the files (a WAL is applied
    to the copy, never to the shipped bytes)."""
    work = tempfile.mkdtemp(prefix="gm-meta-")
    try:
        path = os.path.join(work, "v.db")
        for role, suffix in (("db", ""), ("wal", "-wal"), ("journal", "-journal")):
            if role in files:
                with open(path + suffix, "wb") as f:
                    f.write(files[role])
        con = sqlite3.connect(path)
        uv = con.execute("pragma user_version").fetchone()[0]
        cols = {t: [c[1] for c in con.execute("pragma table_info(%s)" % t)] for t in READ_TABLES}
        con.close()
    finally:
        shutil.rmtree(work, ignore_errors=True)
    head = files["db"]
    jm = "wal" if head[18] == 2 and head[19] == 2 else ("rollback" if head[18] == 1 and head[19] == 1 else "undecided")
    return uv, {t: c for t, c in cols.items() if c}, jm


def oracle(files, ddl_name, variant, nan=frozenset(), refused=False):
    """The <variant>.expect.json content. refused: the parser makes no record of this database (not SQLite, or no
    bugle_db signature, at the app path: unsupported-schema, no Parse)."""
    exp = C.expect_header(FAMILY, variant, ddl_name)
    if refused:
        return exp
    view = C.engine_view(files, list(READ_TABLES))
    env = Env(view, nan)
    uv, cols, jm = db_meta(files)
    lone = set(files) == {"db"}
    remote, file = (REMOTE, "db") if lone else ("", "any")
    for r in view.get("messages", []):
        exp["records"].append(message_record(r, env, remote, file, nan))
    joined = orphans = 0
    for q in view.get("parts", []):
        if is_int(q.get("message_id")) and q["message_id"] in env.messages:
            joined += 1
        else:
            exp["records"].append(part_record(q, env, remote, file, nan))
            orphans += 1
    notes = exp["notes"]
    notes["user_version"] = str(uv)
    notes["user_version_cited"] = str(uv) if uv in (1, 2) else "no"
    notes["journal_mode"] = jm
    notes["app"] = "google_messages"
    notes["database_user"] = "0"
    for t in READ_TABLES:
        if t in cols:
            notes["rows." + t] = str(len(view.get(t, [])))
    C.note_columns(cols["messages"], MESSAGES_MAPPED, "messages", notes)
    C.note_columns(cols["parts"], PARTS_MAPPED, "parts", notes)
    missing = [c for c in CONVERSATION_COLS if c.lower() not in [x.lower() for x in cols.get("conversations", [])]]
    if missing:
        notes["columns_missing.conversations"] = ",".join(missing)
    notes["parts.joined"] = str(joined)
    notes["orphan_parts"] = str(orphans)
    if uv not in (1, 2):
        exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:schema", "contains":
                                "googlemessages-schema-uncited: user_version %d is not a cited AOSP version; read by the AOSP core signature" % uv})
    if env.conflicts:
        exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:table=messages", "contains":
                                "googlemessages-direction-conflict: %d records" % env.conflicts})
    for cid, total in env.cut:
        exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:table=conversation_participants", "contains":
                                "googlemessages-members-cut: conversation %d: %d members, %d kept" % (cid, total, MAX_LIST)})
    return exp


# ---------------------------------------------------------------- the scenario rows (fictional)


def P(i, sub_id, sim, norm, send, disp, full, contact, blocked=0, name=None):
    return {"_id": i, "sub_id": sub_id, "sim_slot_id": sim, "normalized_destination": norm, "send_destination": send,
            "display_destination": disp, "full_name": full, "contact_id": contact, "blocked": blocked, "subscription_name": name}


def participants_core():
    return {1: P(1, 1, 0, None, None, None, None, -1, 0, "SIM One"),
            2: P(2, -2, -1, "+15550000002", "5550000002", "(555) 000-0002", "Ann Example", 7),
            3: P(3, -2, -1, "+15550000003", None, "555-0003", "+1 555-000-0003", -1),
            4: P(4, -2, -1, "+15550000004", "5550000004", "555-0004", "Bo", -1, 1),
            5: P(5, 2, 1, None, None, None, None, -1, 0, "SIM Two"),
            6: P(6, None, -1, "+15550000006", "5550000006", None, None, -1),
            7: P(7, -2, -1, "ann@example.test", None, None, "Ann Mail", 7)}


def conversations_core():
    return {1: {"_id": 1, "name": "Ann Example", "sms_thread_id": 11, "archive_status": 0, "participant_count": 1,
                "sort_timestamp": T + 6000, "last_read_timestamp": T + 2000},
            2: {"_id": 2, "name": "Trio", "sms_thread_id": 12, "archive_status": 0, "participant_count": 3},
            3: {"_id": 3, "name": None, "sms_thread_id": 13, "archive_status": 1, "participant_count": 1},
            4: {"_id": 4, "name": "Mixed", "sms_thread_id": 14, "archive_status": 0, "participant_count": 2},
            5: {"_id": 5, "name": "Mail", "sms_thread_id": 15, "archive_status": 0, "participant_count": 1}}


MEMBERS_CORE = {1: [2], 2: [3, 1, 2, 4], 3: [4], 4: [2, 6], 5: [7]}  # member rows in rowid order (the self row second in C2, B2)


def txt(s):
    return {"text": s}


def media(ct, uri, w=-1, h=-1):
    return {"content_type": ct, "uri": uri, "width": w, "height": h}


def msg(n, parts="default", **kw):
    """One messages row with its parts: the defaults of the plan's CORE scenario."""
    status = kw.get("message_status", 100)
    recv = T + n * 1000
    r = {"_id": n, "conversation_id": 1, "sender_id": 2, "self_id": 1, "sent_timestamp": recv if is_int(status) and 1 <= status <= 9 else recv - 500,
         "received_timestamp": recv, "message_protocol": 0, "message_status": status, "seen": 1, "read": 1}
    r.update(kw)
    return r, ([txt("text %d" % n)] if parts == "default" else parts)


def core_messages():
    S = "sms_message_uri"
    return [
        msg(1, [txt("Hello from Ann")], sms_message_uri="content://sms/41"),
        msg(2, [txt("Hi Ann")], sender_id=1, message_status=1, sms_message_uri="content://sms/42"),
        msg(3, [txt("Group hi")], conversation_id=2, sender_id=1, message_status=2, sms_message_uri="content://sms/43"),
        msg(4, [txt("Look"), media("image/jpeg", "content://mms/part/90", 640, 480), txt("second line")], conversation_id=2,
            sender_id=3, message_protocol=1, read=0, sms_message_uri="content://mms/7", mms_subject="Pics"),
        msg(5, [], message_protocol=2, message_status=101, sent_timestamp=0, read=0, seen=0, sms_message_uri="content://mms/8",
            mms_transaction_id="tx-5", mms_content_location="x-mms://example.invalid/m5", mms_expiry=T + 86400000),
        msg(6, [txt("draft text")], sender_id=1, message_status=3, sent_timestamp=0),
        msg(7, [txt("failed send")], sender_id=1, message_status=8, sms_message_uri="content://sms/47"),
        msg(8, [txt("sending")], sender_id=1, message_status=5),
        msg(9, [], message_protocol=1, message_status=107, sms_message_uri="content://mms/9"),
        msg(10, [txt("status zero")], message_status=0),
        msg(11, [txt("uncited status")], message_status=150),
        msg(12, [txt("protocol minus one")], message_protocol=-1),
        msg(13, [txt("protocol seven")], message_protocol=7),
        msg(14, [txt("protocol null")], message_protocol=None),
        msg(15, [txt("deleted sender")], sender_id=None),
        msg(16, [txt("dangling sender")], sender_id=99),
        msg(17, [txt("dangling self")], sender_id=1, self_id=98, message_status=1),
        msg(18, [txt("dangling conversation")], conversation_id=97),
        msg(19, [txt("in with self sender")], sender_id=1),
        msg(20, [txt("out with other sender")], message_status=1),
        msg(21, [txt("sent only")], received_timestamp=0, sent_timestamp=T + 21000),
        msg(22, [txt("no time")], received_timestamp=0, sent_timestamp=0),
        msg(23, [txt("seconds")], received_timestamp=1700000000),
        msg(24, [txt("negative")], received_timestamp=-5),
        msg(25, [txt("after 2100")], received_timestamp=4102444800001),
        msg(26, [txt("unparsed uri")], **{S: "content://sms/12a"}),
        msg(27, [txt("long uri")], **{S: "content://sms/" + "9" * 1100}),
        msg(28, [txt("overflow uri")], **{S: "content://mms/9223372036854775808"}),
        msg(29, [{"text": "both", "uri": "content://mms/part/91", "content_type": "text/plain"}]),
        msg(30, [{"content_type": "application/octet-stream"}]),
        msg(31, [txt("read two")], read=2),
        msg(32, [txt("from blocked")], conversation_id=3, sender_id=4),
        msg(33, [txt("sent on sim two")], sender_id=5, self_id=5, message_status=1),
        msg(34, [txt("mixed group")], conversation_id=4, sender_id=1, message_status=1),
        msg(35, [txt("undecided sender")], conversation_id=4, sender_id=6),
        msg(36, [txt("email sender")], conversation_id=5, sender_id=7),
    ]


ORPHAN_PARTS = [{"_id": 900, "message_id": 999, "conversation_id": 1, "text": "orphan text"},
                {"_id": 901, "message_id": None, "conversation_id": 1, "text": "null message id"}]


# ---------------------------------------------------------------- writing


def create(work, ddl_name, name="b.db", **conn):
    """A new database with the snapshot's tables and its user_version; foreign keys stay off, as on a device."""
    path = os.path.join(work, name)
    con = C.connect(path, **conn)
    text = C.load_ddl(ddl_name, SCHEMAS)[0]
    con.executescript(text)
    uv = int(re.search(r"^-- user_version (\d+)$", text, re.M).group(1))
    con.execute("pragma user_version = %d" % uv)
    return path, con


def fill(con, participants=(), conversations=(), members=None, messages=(), extra_parts=(), extra=None):
    """participants, conversations: row dicts; members {conversation: [participant ids]} (rows in that order);
    messages [(row, parts)]; extra_parts: parts inserted last with their own ids; extra {table: {col: value}}
    columns added to every row of a table (the synthetic fixture_extra_* cells)."""
    extra = extra or {}
    with C.batch(con):
        for r in participants:
            C.insert(con, "participants", r)
        for r in conversations:
            C.insert(con, "conversations", dict(r, **extra.get("conversations", {})))
        for cid, pids in (members or {}).items():
            for pid in pids:
                C.insert(con, "conversation_participants", {"conversation_id": cid, "participant_id": pid})
        for row, parts in messages:
            C.insert(con, "messages", dict(row, **extra.get("messages", {})))
            for q in parts:
                C.insert(con, "parts", dict(q, message_id=row["_id"], conversation_id=row["conversation_id"], **extra.get("parts", {})))
        for q in extra_parts:
            C.insert(con, "parts", dict(q, **extra.get("parts", {})))


def pick(d, keys):
    return [d[k] for k in keys]


def core_fill(con, messages=None, participants=(1, 2, 3, 4, 5, 6, 7), conversations=(1, 2, 3, 4, 5), orphans=True, extra=None):
    present = set(participants)
    mem = {c: [x for x in MEMBERS_CORE[c] if x in present] for c in conversations}
    fill(con, pick(participants_core(), participants), pick(conversations_core(), conversations), mem,
         core_messages() if messages is None else messages, ORPHAN_PARTS if orphans else (), extra)


def finish(con, path, ddl_name, variant, files=None, **kw):
    con.close()
    files = files or {"db": C.read(path)}
    return files, oracle(files, ddl_name, variant, **kw)


def variant_v2(work):
    path, con = create(work, "aosp-v2.sql")
    core_fill(con)
    return finish(con, path, "aosp-v2.sql", "gm-aosp-v2")


def variant_v1(work):
    path, con = create(work, "aosp-v1.sql")
    core_fill(con, [m for m in core_messages() if m[0]["_id"] in (1, 2, 4)], (1, 2, 3), (1, 2), orphans=False)
    return finish(con, path, "aosp-v1.sql", "gm-aosp-v1")


def small_fill(con, ids=(1, 2)):
    core_fill(con, [m for m in core_messages() if m[0]["_id"] in ids], (1, 2), (1,), orphans=False)


def insert_sql(table, r):
    return "insert into %s(%s) values (%s)" % (table, ",".join(r), ",".join(C.lit(r[k]) for k in r))


def variant_wal(work):
    path, con = create(work, "aosp-v2.sql")
    con.execute("pragma journal_mode = wal")
    small_fill(con)
    con.execute("pragma wal_checkpoint(truncate)")
    con.close()
    m40, p40 = msg(40, [txt("only in wal")])
    m41, p41 = msg(41, [txt("uncommitted M41")])
    stmts = ["pragma wal_autocheckpoint = 0", insert_sql("messages", m40),
             insert_sql("parts", dict(p40[0], message_id=40, conversation_id=1)),
             "pragma cache_size = 1", "pragma cache_spill = 1", "BEGIN", insert_sql("messages", m41),
             insert_sql("parts", dict(p41[0], message_id=41, conversation_id=1)),
             "update parts set text = text || hex(zeroblob(40000)) where message_id = 41"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    if b"uncommitted M41" not in files["wal"]:
        raise SystemExit("gm-aosp-v2-wal: M41 did not reach the WAL (no spill)")
    files, exp = finish(sqlite3.connect(":memory:"), path, "aosp-v2.sql", "gm-aosp-v2-wal", files)
    exp["warnings"].insert(0, {"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
    return files, exp


def variant_journal(work):
    path, con = create(work, "aosp-v2.sql")
    small_fill(con)
    con.close()
    m42, p42 = msg(42, [txt("interrupted M42")])
    stmts = ["pragma cache_size = 10", "pragma cache_spill = 10", "BEGIN", insert_sql("messages", m42),
             insert_sql("parts", dict(p42[0], message_id=42, conversation_id=1)),
             "update parts set text = text || hex(zeroblob(60000)) where message_id = 42"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "journal": C.read(path + "-journal")}
    files, exp = finish(sqlite3.connect(":memory:"), path, "aosp-v2.sql", "gm-journal", files)
    exp["warnings"] = [{"table": "", "rowid": 0, "contains": "journal-hot"}]  # the library's warning, not a row's
    return files, exp


def variant_proprietary(work):
    path, con = create(work, "proprietary-shaped.sql")
    msgs = [m for m in core_messages() if m[0]["_id"] in (1, 2, 13)]
    msgs[0][1][0].update(file_size_bytes=1234, local_cache_path="/data/local/x")
    core_fill(con, msgs, (1, 2), (1,), orphans=False,
              extra={"messages": {"fixture_extra_flag": 1}, "conversations": {"fixture_extra_note": "synthetic note"},
                     "parts": {"fixture_extra_part": "synthetic part"}})
    return finish(con, path, "proprietary-shaped.sql", "gm-proprietary-shaped")


def variant_other_kind(work):
    path = os.path.join(work, "b.db")
    con = C.connect(path)
    con.execute("CREATE TABLE messages (_id INTEGER PRIMARY KEY, body TEXT)")
    con.execute("insert into messages(body) values ('not a bugle_db')")
    con.execute("pragma user_version = 0")
    con.close()
    return finish(sqlite3.connect(":memory:"), path, "inline: CREATE TABLE messages (_id INTEGER PRIMARY KEY, body TEXT)", "gm-other-kind",
                  refused=True)


def variant_not_sqlite(work):
    files = {"db": (b"not a database\n" * 274)[:4096]}
    return files, oracle(files, "none", "gm-not-sqlite", refused=True)


# ---------------------------------------------------------------- gm-hostile (H1-H15 of the plan)

NAN_MARK = 1.2345678901234567e300
NAN_BITS = struct.pack(">Q", 0x7FF8000000000001)


def hostile_rows():
    """H1-H15 as message ids 101-116 (H11 is the two messages 111 and 112). Returns (rows, nan cells, participants,
    conversations, members)."""
    rows, nan = [], set()

    def h(n, parts="default", **kw):
        m, ps = msg(n, parts, **kw)
        rows.append((m, ps))
        return m
    h(101, message_status="100")  # H1 TEXT
    h(102, message_protocol=0.0)  # H2 REAL
    h(103, message_status=NAN_MARK)  # H3 NaN
    nan.add(("messages", 103, "message_status"))
    h(104, message_protocol=b"\x00")  # H4 BLOB
    h(105, received_timestamp="1700000000000")  # H5 TEXT
    h(106, sender_id="2")  # H6 TEXT
    h(107, [{"_id": 1000 + i, "text": "p%04d" % i} for i in range(1001)])  # H7
    h(108, [txt("a" * 4194304)])  # H8
    h(109, [txt(C.Raw(b"nul\x00 bad \xff rtl \xe2\x80\xae end"))])  # H9
    h(110, [{"content_type": "t" * 10240, "uri": "u" * 10240}])  # H10
    h(111, [txt("six one")], conversation_id=6)  # H11
    h(112, [txt("six two")], conversation_id=6, sender_id=1, message_status=1)
    h(113, [txt("long destination")], sender_id=8)  # H12
    h(114, [txt("self with a destination")], sender_id=9, message_status=1, sms_message_uri=C.Raw(bytes([255]) * 400))  # H13; the uri is 400 bytes stored, 1200 cleaned (ruling 4I5-T4c6)
    h(115, [txt("real read")], read=1.0)  # H14
    h(116, [txt("blob uri")], sms_message_uri=b"A")  # H15
    ps = [participants_core()[1], participants_core()[2],
          P(8, -2, -1, "+15550000008", "8" * 2048, None, None, -1),
          P(9, 1, 0, "+15550000002", None, None, None, -1, 0, "SIM Nine")]
    ps += [P(1000 + i, -2, -1, "+1555100%04d" % (i - 1), None, None, None, -1) for i in range(1, 1002)]
    convs = [conversations_core()[1], {"_id": 6, "name": "Crowd", "sms_thread_id": 16, "archive_status": 0, "participant_count": 1001}]
    members = {1: [2], 6: [1000 + i for i in range(1, 1002)]}
    return rows, frozenset(nan), ps, convs, members


def untype(sql):
    """messages as the engine reads it with no column affinity: the INT declarations removed, so TEXT, REAL and BLOB
    cells survive an insert (the schema is restored afterwards)."""
    return re.sub(r" INT\b", "", sql)


def variant_hostile(work):
    path, con = create(work, "aosp-v2.sql")
    rows, nan, ps, convs, members = hostile_rows()
    orig = con.execute("select sql from sqlite_master where name = 'messages'").fetchone()[0]
    con.execute("pragma writable_schema = ON")
    con.execute("update sqlite_master set sql = ? where name = 'messages'", (untype(orig),))
    con.execute("pragma writable_schema = OFF")
    con.close()
    con = C.connect(path)  # the untyped schema is read
    fill(con, ps, convs, members, rows)
    con.execute("pragma writable_schema = ON")
    con.execute("update sqlite_master set sql = ? where name = 'messages'", (orig,))
    con.execute("pragma writable_schema = OFF")
    con.close()
    files = {"db": C.read(path)}
    mark = struct.pack(">d", NAN_MARK)
    if files["db"].count(mark) != 1:
        raise SystemExit("gm-hostile: the NaN marker is in %d places, want 1" % files["db"].count(mark))
    files["db"] = files["db"].replace(mark, NAN_BITS)
    return files, oracle(files, "aosp-v2.sql", "gm-hostile", nan)


# ---------------------------------------------------------------- the typed recovery oracle (spec 8)


def join_basis(files):
    """spec 8.3 condition 1: the live messages _id is INTEGER PRIMARY KEY AUTOINCREMENT and sqlite_sequence holds a
    messages row. (autoincrement, has sequence row)"""
    view = C.engine_view(files, ["sqlite_master", "sqlite_sequence"])
    auto = False
    for m in view.get("sqlite_master", []):
        if m["type"] == b"table" and m["name"] == b"messages":
            sql = m["sql"].decode("utf-8")
            body = sql[sql.index("(") + 1:]
            depth, cur, defs = 0, "", []
            for ch in body:
                if ch == "(":
                    depth += 1
                elif ch == ")":
                    depth -= 1
                if ch == "," and depth == 0:
                    defs.append(cur)
                    cur = ""
                else:
                    cur += ch
            defs.append(cur)
            first = defs[0].upper().split()
            auto = first[0] == "_ID" and "PRIMARY" in first and "KEY" in first and "AUTOINCREMENT" in first
    seq = any(s.get("name") == b"messages" for s in view.get("sqlite_sequence", []))
    return auto, seq


def recovered_record(table, d, env, basis, file):
    """spec 8.2 and 8.3 for one recovered row of a claimed table. env: the live view the joins read."""
    remote = source_path(file)
    if table == "messages":
        rec = message_record(d, env, remote, file, joined=False)
        return rec
    raw, unknown = {}, {}
    raw["app"], raw["app_basis"] = "google_messages", "path"
    p = {"channel": "unknown", "direction": "unknown"}
    raw["part_rowid"] = d["rowid"]
    mid, mst = int_cell(d, "message_id", unknown, "message_id", table="parts")
    if mst == "int":
        raw["part_message_id"] = mid
    elif mst == "null":
        raw["part_message_id_state"] = "null"
    ts, tst = int_cell(d, "timestamp", unknown, "timestamp", table="parts")
    if tst == "int":
        classify_time(raw, "timestamp", ts)
    cid, cst = int_cell(d, "conversation_id", unknown, "conversation_id", table="parts")
    if cst == "int":
        raw["conversation_id"] = cid
    bt = part_payload(d, unknown, raw, p)
    auto, seq = basis
    live = env.messages.get(mid) if mst == "int" else None
    reason = None
    if not auto:
        reason = "no-autoincrement"
    elif not seq:
        reason = "no-sqlite-sequence"
    elif live is None:
        reason = "no-live-message"
    elif not (cst == "int" and live.get("conversation_id") == cid):
        reason = "conversation-differs"
    elif not (tst == "int" and live.get("received_timestamp") == ts):
        reason = "timestamp-differs"
    counterparty = ""
    if reason:
        raw["part_join"], raw["part_join_reason"] = "not-joined", reason
        p["participants"], p["participants_unknown"] = [], True
        if cst == "int":
            p["thread"] = {"id": str(cid)}
    else:
        raw["part_join"], raw["joined_message_rowid"] = "joined", mid
        lrec = message_record(live, Env(env.view, env.nan), "", "any")
        lp = lrec["payload"]
        p["channel"], p["direction"], p["participants"] = lp["channel"], lp["direction"], lp["participants"]
        if lp.get("participants_unknown"):
            p["participants_unknown"] = True
        if cst == "int":  # B1, spec 8.2: the part's own conversation only; nothing else of the live record is copied
            p["thread"] = {"id": str(cid)}
        if p["participants"]:
            counterparty = p["participants"][0]["address"]
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    return {"table": "parts", "rowid": d["rowid"], "file": file, "type": "message", "source_path": remote, "time": None,
            "time_end": None, "times": {}, "deleted": False,
            "summary": message_summary(p["channel"], p["direction"], counterparty, bt or ""), "body": bt, "payload": p}


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario: every recovered row of a claimed table mapped
    by spec 8 against the LIVE view of the committed files."""
    scen = SCENARIOS[variant]
    view = C.engine_view(files, list(READ_TABLES))
    env = Env(view)
    basis = join_basis(files)

    def expect_fn(table, d, e, file):
        return recovered_record(table, d, env, basis, file)

    obj = C.recover_oracle(variant, files, scen, lambda f: env, expect_fn)
    found = {(x["table"], x["rowid"]) for x in obj["digests"]}
    for w in scen.wants:
        key = (w.get("label") or w["table"], w["rowid"])
        if key not in found:
            raise SystemExit("%s: the wanted history version %s rowid %d is in no page image" % (variant, w["table"], w["rowid"]))
    return obj


# ---------------------------------------------------------------- gm-deleted and gm-rebuilt


DEFAULTS = {"messages": {"sms_priority": 0, "sms_message_size": 0, "mms_expiry": 0, "raw_status": 0, "retry_start_timestamp": 0},
            "parts": {"width": -1, "height": -1}}  # what a row inserted without those columns stores


def variant_deleted(work):
    """The live rows P1-P2, C1, M1 and M2 with their parts are checkpointed together with M50, M51 and the parts 50, 51 and
    52. A killed writer then deletes M50, M51 and those parts (committed frames), inserts M53 with its part (committed) and
    deletes both in a later frame, and rewrites M1's part text from "Hello from Ann (old)" to "Hello from Ann"; the WAL is
    never checkpointed. The page images that still hold the deleted rows are the database file's pages under the WAL and the
    superseded WAL frames: a page emptied by DELETE keeps no cell pointers, so a freed page would show none of its rows to a
    page-image oracle (plan 4I5 Task 2 deviation from the freelist wording; secure_delete is OFF)."""
    path, con = create(work, "aosp-v2.sql")
    con.execute("pragma secure_delete = OFF")
    con.execute("pragma journal_mode = wal")
    base = [msg(1, [{"_id": 1, "text": "Hello from Ann (old)"}], sms_message_uri="content://sms/41"),
            msg(2, [{"_id": 2, "text": "Hi Ann"}], sender_id=1, message_status=1, sms_message_uri="content://sms/42")]
    gone = [msg(50, [{"_id": 50, "text": "deleted with its message"}]),
            msg(51, [{"_id": 51, "text": "also deleted"}], sender_id=1, message_status=1)]
    part52 = {"_id": 52, "message_id": 2, "conversation_id": 1, "text": "deleted second part"}
    fill(con, pick(participants_core(), (1, 2)), pick(conversations_core(), (1,)), {1: [2]}, base + gone, [part52])
    scen = SCENARIOS["gm-deleted"] = C.Scenario(CLAIMED)
    scen.write("messages", scen.snap(con, "messages"))
    scen.write("parts", scen.snap(con, "parts"))
    con.execute("pragma wal_checkpoint(truncate)")
    scen.want("messages", scen.snap(con, "messages", "_id in (50, 51)"))
    scen.want("parts", scen.snap(con, "parts", "_id in (50, 51, 52)"))
    old = scen.snap(con, "parts", "_id = 1")
    con.close()
    m53, p53 = msg(53, [{"_id": 53, "text": "deleted in wal"}])
    p53 = dict(p53[0], message_id=53, conversation_id=1)
    stmts = ["pragma secure_delete = OFF", "pragma wal_autocheckpoint = 0",
             "delete from parts where _id in (50, 51, 52)", "delete from messages where _id in (50, 51)",
             "BEGIN", insert_sql("messages", m53), insert_sql("parts", p53), "COMMIT",
             "delete from parts where _id = 53", "delete from messages where _id = 53",
             "update parts set text = 'Hello from Ann' where _id = 1"]
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    m53 = dict(DEFAULTS["messages"], **m53)
    p53 = dict(DEFAULTS["parts"], timestamp=m53["received_timestamp"], **p53)
    for table, row in (("messages", m53), ("parts", p53)):
        scen.write(table, [(row["_id"], row)])
        scen.want(table, [(row["_id"], row)])
    scen.write("parts", old)
    scen.want("parts", old)
    return files, oracle(files, "aosp-v2.sql", "gm-deleted")


def variant_rebuilt(work):
    path, con = create(work, "aosp-v2.sql", page_size=512)
    con.execute("pragma secure_delete = OFF")
    con.execute("create table pad(x)")
    with C.batch(con):
        for i in range(80):
            con.execute("insert into pad values (?)", ("pad %d " % i + "z" * 400,))
    # each old row is padded to over half a leaf page, so each table spans leaf pages below its root: dropping a table zeroes
    # its root page but frees the pages below it whole, with their rows
    before = [msg(1, [{"_id": 60, "text": "before rebuild", "content_type": "c" * 300}], conversation_id=1, received_timestamp=T + 1000,
                  mms_content_location="x-mms://example.invalid/1/" + "p" * 280),
              msg(2, [{"_id": 61, "text": "also before", "content_type": "c" * 300}], conversation_id=1, received_timestamp=T + 2000,
                  mms_content_location="x-mms://example.invalid/2/" + "p" * 280)]
    fill(con, pick(participants_core(), (1, 2)), pick(conversations_core(), (1, 2)), {1: [2], 2: [2]}, before)
    scen = SCENARIOS["gm-rebuilt"] = C.Scenario(CLAIMED)
    scen.write("messages", scen.snap(con, "messages"))
    scen.write("parts", scen.snap(con, "parts"))
    scen.want("messages", scen.snap(con, "messages"), label="messages")
    scen.want("parts", scen.snap(con, "parts"), label="parts")
    # every table dropped and recreated (the upgrade helper's rebuild); the pad table goes last so the new tables take its pages, not the old rows
    with C.batch(con):
        for t in ("conversation_participants", "participants", "conversations", "messages", "parts", "pad"):
            con.execute("drop table " + t)
    ddl = C.load_ddl("aosp-v2.sql", SCHEMAS)[0]
    con.executescript(ddl)
    after = [msg(1, [txt("after rebuild")], conversation_id=2, sender_id=2, received_timestamp=T + 70000),
             msg(2, [txt("second after")], conversation_id=1, sender_id=2, received_timestamp=T + 71000)]
    fill(con, pick(participants_core(), (1, 2)), pick(conversations_core(), (1, 2)), {1: [2], 2: [2]}, after)
    return finish(con, path, "aosp-v2.sql", "gm-rebuilt")


VARIANTS = {"gm-aosp-v2": variant_v2, "gm-aosp-v1": variant_v1, "gm-aosp-v2-wal": variant_wal, "gm-journal": variant_journal,
            "gm-proprietary-shaped": variant_proprietary, "gm-other-kind": variant_other_kind, "gm-not-sqlite": variant_not_sqlite,
            "gm-deleted": variant_deleted, "gm-rebuilt": variant_rebuilt, "gm-hostile": variant_hostile}

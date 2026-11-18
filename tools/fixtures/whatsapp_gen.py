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
import base64
import hashlib
import re
import struct

import telephony_common as C

FAMILY = "whatsapp-android"
SCENARIOS = {}  # variant -> C.Scenario, filled when the variant is generated (the typed recovery oracle, plan 4I task 14)
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
P4 = "15550000004@s.whatsapp.net"  # the admin of a revoke (plan 4I task 7)
P5 = "15550000005@s.whatsapp.net"  # the phone JID jid_map gives for LID1
LID1 = "100000000000001@lid"  # a 1:1 chat addressed by a LID that jid_map maps
LID2 = "100000000000002@lid"  # a group sender addressed by a LID with no jid_map row

MODERN_TYPE = {0: "text", 1: "image", 2: "audio", 3: "video", 4: "contact_card", 5: "location", 9: "document",
               16: "live_location", 7: "system", 13: "gif", 20: "sticker", 24: "group_invite"}
REVOKED_TYPES = (15, 64)
LEGACY_TYPE = {0: "text", 1: "image", 2: "audio", 3: "video", 4: "contact_card", 5: "location"}
VCARD = "BEGIN:VCARD\r\nFN:Ann\r\nEND:VCARD\r\n"
DECIMAL = re.compile(r"(0|-?[1-9][0-9]{0,17})", re.ASCII)  # the strict decimal of a legacy media_wa_type (plan 4I task 6)


class JidRef(str):
    """A jid row a side-table value names by its address: resolved to the jid row id when the row is inserted (the
    jid table is built in first-use order)."""


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


JA, JC, JD, JE, JB, JF = ("155500000%d@s.whatsapp.net" % n for n in (60, 65, 67, 69, 70, 71))  # one chat per scenario


def modern_extras():
    """Rows 30 to 44 of android-modern-2026: revokes, a quote, an edit, system rows, a call notice, locations, LIDs and
    the single-source extras. side is [(table, row)] of the tables keyed to the message."""
    def row(i, **kw):
        base = dict(id=i, chat=C1, me=0, ts=T0 + i * 1000, recv=T0 + i * 1000 + 100, status=0, type=0, key="K%02d" % i)
        base.update(kw)
        return base
    return [
        row(30, type=15, side=[("message_revoked", {"revoked_key_id": "K30R", "admin_jid_row_id": JidRef(P4),
                                                   "revoke_timestamp": T0 + 100000})]),
        row(31, type=64),
        row(32, side=[("message_revoked", {"revoked_key_id": "K32R"})]),
        row(33, type=15, text="secret"),
        row(34, text="reply", side=[("message_quoted", {"key_id": "K02", "from_me": 0, "text_data": "hello", "message_type": 0,
                                                       "timestamp": T0})]),
        row(35, text="edited", side=[("message_edit_info", {"original_key_id": "K35O", "edited_timestamp": T0 + 200000,
                                                           "sender_timestamp": T0 + 199000, "display_type": 1})]),
        row(36, chat=GRP, type=7, text="New name", side=[("message_system", {"action_type": 11}),
                                                         ("message_system_value_change", {"old_data": "Old"})]),
        row(37, chat=GRP, type=7, side=[("message_system", {"action_type": 18})]),
        row(38, chat=GRP, type=7, side=[("message_system", {"action_type": 4}),
                                        ("message_system_number_change", {"old_jid_row_id": JidRef(P4),
                                                                           "new_jid_row_id": JidRef(P5)})]),
        row(39, type=10, side=[("message_call_log", {"call_log_row_id": 1})]),
        row(40, type=5, side=[("message_location", {"latitude": 48.85, "longitude": 2.35, "place_name": "Spot",
                                                    "map_download_status": 2})]),
        row(41, type=16, side=[("message_location", {"latitude": 1.5, "longitude": -2.5, "live_location_share_duration": 900,
                                                    "live_location_sequence_number": 3,
                                                    "live_location_final_latitude": 1.75,
                                                    "live_location_final_longitude": -2.75,
                                                    "live_location_final_timestamp": T0 + 41500})]),
        row(42, chat=LID1, text="lid chat", side=[("jid_map", {"lid_row_id": JidRef(LID1), "jid_row_id": JidRef(P5)})]),
        row(43, chat=GRP, text="lid sender", sender=LID2),
        row(44, text="extras", starred=1, side=[("message_view_once_media", {"state": 1}),
                                                ("message_ephemeral", {"duration": 86400, "expire_timestamp": 1600086400}),
                                                ("message_forwarded", {"forward_score": 2})]),
    ] + [
        # plan 4I task 9: the chats of the deleted_chat_job scenario (4I-R4). 63 is starred with a sort_id below job 1's
        # bound: only the NULL starred bound keeps it live.
        row(60, chat=JA, text="p60"), row(61, chat=JA, text="p61"), row(62, chat=JA, text="p62"),
        row(63, chat=JA, text="p63", starred=1, sort=61), row(64, chat=JA, text="p64"),
        row(65, chat=JC, text="p65"), row(66, chat=JC, text="p66"),
        row(67, chat=JD, text="p67"), row(68, chat=JD, text="p68"),
        row(69, chat=JE, text="p69"),
        row(70, chat=JB, text="p70"),
        row(71, chat=JF, text="p71", starred=1),
    ] + attachment_rows()


def modern_calls(ids=None):
    """call_log rows 1 to 8 (plan 4I task 10). parts are the call_log_participant_v2 rows as (address, call_result);
    row 7 holds seconds in the millisecond column (R10: kept as stored, no floor)."""
    def call(i, **kw):
        base = dict(id=i, jid=C1, me=0, key="C%02d" % i, ts=T0 + 400000 + i * 1000, video=0, dur=0, result=5, bytes=None,
                    group=0, parts=[])
        base.update(kw)
        return base
    rows = [
        call(1, dur=60, bytes=1000),
        call(2, result=2),
        call(3, me=1, result=2),
        call(4, result=3),
        call(5, result=4),
        call(6, video=1, dur=30, group=GRP, parts=[(P2, 5), (P3, 2)]),
        call(7, ts=1600000000, dur=10),
        call(8, jid=LID1),
    ]
    return [r for r in rows if ids is None or r["id"] in ids]


def insert_calls(con, js, calls):
    have = [c[1] for c in con.execute("pragma table_info(call_log)")]
    for c in calls:
        row = {"_id": c["id"], "jid_row_id": js.jid_id(c["jid"]), "from_me": c["me"], "call_id": c["key"],
               "timestamp": c["ts"], "video_call": c["video"], "duration": c["dur"], "call_result": c["result"],
               "bytes_transferred": c["bytes"], "group_jid_row_id": js.jid_id(c["group"]) if c["group"] else 0}
        C.insert(con, "call_log", {k: v for k, v in row.items() if k in have})
        for addr, res in c["parts"]:
            C.insert(con, "call_log_participant_v2", {"call_log_row_id": c["id"], "jid_row_id": js.jid_id(addr), "call_result": res})


MEDIA_KEY = b"\x01\x02\x03\x04"
IMG = "/WhatsApp/Media/WhatsApp Images/"
# the files the scenario holds next to android-modern-2026, which sits at /data/user/10/com.whatsapp/databases (user 10).
# device None is the database's own device (SER1); SER2 is another device.
EXTRA_FILES = [
    ("/storage/emulated/10" + IMG + "IMG-A.jpg", b"JPEG-A", None),                  # same user: the one link (row 80)
    ("/storage/emulated/10" + IMG + "IMG-B.jpg", b"JPEG-B1", None),                 # a legacy and an Android 11 copy:
    ("/storage/emulated/10/Android/media/com.whatsapp" + IMG + "IMG-B.jpg", b"JPEG-B2", None),  # ambiguous (row 81)
    ("/storage/emulated/10" + IMG + "IMG-C.jpg", b"JPEG-C", "SER2"),                # another device (row 82)
    ("/storage/emulated/10/WhatsApp Business/Media/WhatsApp Images/IMG-D.jpg", b"JPEG-D", None),  # the other app (row 83)
    ("/storage/emulated/0" + IMG + "IMG-E.jpg", b"JPEG-E", None),                   # another user (row 84)
    ("/sdcard" + IMG + "IMG-F.jpg", b"JPEG-F", None),                               # names no user: R5a (row 85)
    ("/storage/emulated/10" + IMG + "IMG-G.jpg", b"JPEG-G", None),                  # an absolute file_path (row 86)
]


def media_row(path, name, size=12345, **kw):
    row = {"file_path": path, "file_size": size, "mime_type": "image/jpeg", "media_name": name, "file_length": size + 1,
           "message_url": "https://mmg.example/" + (name or ""), "direct_path": "/v/t62/" + (name or ""),
           "file_hash": "hash-" + (name or ""),
           "transferred": 1, "media_key": MEDIA_KEY}
    row.update(kw)
    return row


def attachment_rows():
    """Rows 80 to 88 of android-modern-2026 (plan 4I task 11, spec 6.7): one image each, with a message_media row."""
    def row(i, side):
        return dict(id=i, chat=C1, me=0, ts=T0 + i * 1000, recv=T0 + i * 1000 + 100, status=0, type=1, key="K%02d" % i,
                    side=[("message_media", side)])
    return [
        row(80, media_row("Media/WhatsApp Images/IMG-A.jpg", "IMG-A.jpg")),
        row(81, media_row("Media/WhatsApp Images/IMG-B.jpg", "IMG-B.jpg")),
        row(82, media_row("Media/WhatsApp Images/IMG-C.jpg", "IMG-C.jpg")),
        row(83, media_row("Media/WhatsApp Images/IMG-D.jpg", "IMG-D.jpg")),
        row(84, media_row("Media/WhatsApp Images/IMG-E.jpg", "IMG-E.jpg")),
        row(85, media_row("Media/WhatsApp Images/IMG-F.jpg", "IMG-F.jpg")),
        row(86, media_row("/storage/emulated/10/WhatsApp/Media/WhatsApp Images/IMG-G.jpg", "IMG-G.jpg")),
        row(87, media_row("Media/../etc/x.jpg", "x.jpg")),
        row(88, media_row(None, None, size=0, mime_type=None, file_length=None, message_url=None, direct_path=None,
                          file_hash=None, media_key=None)),
    ]


def modern_jobs():
    """deleted_chat_job rows 1 to 8 of android-modern-2026 (plan 4I task 9). msg and starred are the bounds as stored;
    cats and singular the lists; a bound of 'x' is a TEXT in an INTEGER column."""
    def job(i, chat, **kw):
        base = dict(id=i, chat=chat, msg=None, starred=None, cats=None, singular=None)
        base.update(kw)
        return base
    return [
        job(1, JA, msg=62),
        job(2, JC, msg=60),
        job(3, JC, msg=66),
        job(4, JD, msg=60, cats='["1"]'),
        job(5, JE, singular='["69"]'),
        job(6, JB, msg="x"),
        job(7, JF, starred=71),
        job(8, 9999),
    ]


def modern_addons():
    """message_add_on rows 1 to 8 of android-modern-2026 (plan 4I task 8, 4I-R20). reaction is the message_add_on_reaction
    row's text (None: no such row), rts its sender_timestamp."""
    def add(i, parent, typ, **kw):
        base = dict(id=i, chat=C1, me=0, key="A%02d" % i, sender=-1, parent=parent, ts=T0 + 300000 + i * 1000, type=typ,
                    reaction=None, rts=None)
        base.update(kw)
        return base
    return [
        add(1, 2, 56, me=1, ts=T0 + 300000, reaction="\U0001F44D", rts=1600000299000),
        add(2, 3, 56, reaction=""),
        add(3, 999, 56, reaction="ok"),
        add(4, 2, 74),
        add(5, 2, 68),
        add(6, 2, 9999),
        add(7, 10, 56, chat=GRP, sender=JidRef(LID2), reaction="\u2764"),
        add(8, 2, 56),
        add(9, 60, 56, chat=JA, reaction="ok"),  # on a message of a cleared chat (task 9)
    ]


def legacy_extras():
    """Rows 45 and 46 of android-transition-legacydata: a quote through messages_quotes, edit_version 7 with status 5
    (raw only), and a revoke through message_revoked keyed to messages._id."""
    return [
        dict(id=45, chat=C1, me=0, text="quoting", ts=T0 + 45000, recv=T0 + 45100, status=5, type=0, key="K45", quoted=1,
             edit_version=7),
        dict(id=46, chat=C1, me=0, ts=T0 + 46000, recv=T0 + 46100, status=0, type=0, key="K46",
             side=[("message_revoked", {"revoked_key_id": "K46R", "revoke_timestamp": T0 + 101000})]),
    ]


LEGACY_QUOTE = {"_id": 1, "key_remote_jid": C1, "key_from_me": 0, "key_id": "K02", "status": 0, "data": "hello",
                "timestamp": T0, "media_wa_type": "0"}


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
    if m["type"] in (1, 2, 3) and not sys_row:
        r.update({"media_name": "WA%04d" % m["id"], "media_mime_type": {1: "image/jpeg", 2: "audio/ogg", 3: "video/mp4"}[m["type"]],
                  "media_size": 1000 + m["id"], "media_url": "https://mmg.example/%d" % m["id"], "media_hash": "H%d" % m["id"]})
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
    if m.get("quoted"):
        r["quoted_row_id"] = m["quoted"]
    if m.get("edit_version") is not None:
        r["edit_version"] = m["edit_version"]
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
         "received_timestamp": m.get("recv", 0), "message_type": m["type"], "text_data": m.get("text"), "sort_id": m.get("sort", m["id"]),
         "starred": m.get("starred", 0), "receipt_server_timestamp": m.get("srv")}
    if m.get("system"):
        r["message_type"] = 7
    return r


# ----------------------------------------------------------------------------------------- the oracle
#
# The oracle restates the spec rules (6.2, 6.4, 7) over the values the engine reads back from a COPY of the finished
# files. It shares no code with the Go parser; where the two must agree on a name (a raw key, a state) the name is
# the one the spec or the plan gives, and a name the spec leaves open is chosen here and pinned by the Go tests.

MODERN_RAW_INTS = ["sort_id", "starred", "origination_flags", "broadcast", "recipient_count", "message_add_on_flags",
                   "view_mode", "lookup_tables"]
LEGACY_RAW_INTS = ["starred", "needs_push", "recipient_count", "forwarded", "lookup_tables", "edit_version",
                   "send_timestamp", "read_device_timestamp", "played_device_timestamp"]
MAX_ADDR = 1024  # the message contract's limit for a participant address and a thread id
UNMAPPED_EXACT = {"message_orphaned_edit", "message_thumbnail", "media_hash_thumbnail", "missed_call_logs", "props", "receipts"}
UNMAPPED_PREFIX = ("receipt_", "group_participant", "newsletter")


def time_of(v):
    """(time or None, status): ms in a message table; a sentinel or invalid value is no time (floor 1e11, spec 7.6)."""
    if not is_int(v):
        return None, None
    st = C.classify(v, "ms")
    if st == "valid" and v < FLOOR_MS:
        st = "invalid"
    return (C.iso(v, "ms") if st == "valid" else None), st


def is_text(v):
    return isinstance(v, bytes) and not isinstance(v, C.Blob)


def text_of(v):
    """The cleaned text of a stored TEXT value (bytes), as common.CleanText reads it."""
    return C.clean(v, MAX_TEXT)


def cleaned(raw, field, b):
    """A TEXT cell kept in raw as a cleaned string; an altered text is listed in raw.text_flags."""
    c = text_of(b)
    if c.flags:
        C.mark(raw, field)
    return c.text


def blob_marker(b):
    import base64
    b = bytes(b)
    return {"blob_b64": base64.b64encode(b).decode()} if len(b) <= 256 else {"blob_len": len(b)}


def stored(raw, name, v):
    """A stored value as raw shows it: an integer, a cleaned text, a finite float or a blob marker; None for NULL."""
    if v is None:
        return None
    if isinstance(v, C.Blob):
        return blob_marker(v)
    if isinstance(v, bytes):
        return cleaned(raw, name, v)
    return v


class Cells:
    """The cell reader of the Go parser: an INTEGER or TEXT cell of the wrong class is stated in unknown_columns."""

    def __init__(self, row, raw, unknown, prefix=""):
        self.row, self.raw, self.unknown, self.prefix = row, raw, unknown, prefix

    def int(self, name):
        v = self.row.get(name)
        if v is None:
            return None
        if is_int(v):
            return v
        self.unknown[self.prefix + name] = "type-mismatch"
        return None

    def text(self, name):
        v = self.row.get(name)
        if v is None:
            return None
        if is_text(v):
            return v
        self.unknown[self.prefix + name] = "type-mismatch"
        return None


def read_code(c, name, text_typed=False):
    """An enum cell: its raw value as stored, the integer when it is usable. A legacy media_wa_type is read strictly."""
    v = c.row.get(name)
    value = None
    if text_typed:
        if is_int(v):
            value = v
        elif is_text(v):
            s = v.decode("ascii", "replace")
            if DECIMAL.fullmatch(s):
                value = int(s)
    else:
        value = c.int(name)
    s = stored(c.raw, name, v)
    if s is not None:
        c.raw[name] = s
    return value


def read_instant(c, name):
    """A message time in ms; the stored value is in raw with its status when it is not a time (spec 7.6, 4I-R10)."""
    v = c.int(name)
    if v is None:
        return None
    t, st = time_of(v)
    c.raw[name] = v
    if st != "valid":
        c.raw[name + "_status"] = st
    return t


def parse_jid(s):
    """decode/wajid.Parse restated for what the classification needs: the (user, server) of a JID, or None."""
    if "\x00" in s or "@" not in s:
        return None
    user, server = s.rsplit("@", 1)
    if not user or not server:
        return None
    if ":" in user:
        base, dev = user.rsplit(":", 1)
        if not re.fullmatch(r"[0-9]+", dev) or int(dev) > 65535:
            return None
        if "." in base:
            _, agent = base.rsplit(".", 1)
            if not re.fullmatch(r"[0-9]+", agent) or int(agent) > 255:
                return None
    return user, server


def addr_of_text(b, raw, field):
    """An address from stored text: never cut, never composed (an over-long one is absent)."""
    if len(b) == 0:
        return {"str": None, "parts": None, "state": "absent"}
    s = cleaned(raw, field, b)
    if len(s.encode("utf-8")) > MAX_ADDR:
        return {"str": None, "parts": None, "state": "too-long"}
    return {"str": s, "parts": None, "state": None}


def addr_of_jid(row, raw, unknown, field):
    """The address of a jid row: its raw_string, or when absent the parts as stored (state no-raw-string)."""
    rs = row.get("raw_string")
    if is_text(rs) and len(rs) > 0:
        a = addr_of_text(rs, raw, field)
        a["rowid"] = row["rowid"]
        return a
    c = Cells(row, raw, unknown, "jid.")
    parts = {}
    for n in ("user", "server"):
        b = c.text(n)
        if b is not None:
            parts[n] = cleaned(raw, field + "_" + n, b)
    for n in ("agent", "device"):
        v = c.int(n)
        if v is not None:
            parts[n] = v
    return {"str": None, "parts": parts, "state": "no-raw-string", "rowid": row["rowid"]}


def group_of(a):
    """True or False when the address names its server, None when it does not."""
    if a["str"]:
        p = parse_jid(a["str"])
        return p[1] == "g.us" if p else None
    if a["parts"] is not None and "server" in a["parts"]:
        return a["parts"]["server"] == "g.us"
    return None


def is_status(a):
    p = parse_jid(a["str"]) if a["str"] else None
    return bool(p) and p == ("status", "broadcast")


ABSENT = {"str": None, "parts": None, "state": "absent"}


def app_of(remote):
    """The app the path names (spec 6.8): the package directory, or the external folder of a backup copy."""
    return "whatsapp_business" if ("/" + PKG_W4B + "/") in remote or "/WhatsApp Business/" in remote else "whatsapp"


def summary_of(direction, counterparty, text):
    plain = text is None or (text.isascii() and text.isprintable() and len(text) <= 80)
    if not plain or not counterparty.isascii() or len(counterparty.encode()) > 64:
        return None
    return ("whatsapp %s %s: %s" % (direction, counterparty, text or "")).rstrip(" ")


def join_text(parts):
    """The vCards of one message joined with a newline, as the Go parser keeps them (at most 2 * MAX_TEXT bytes)."""
    return b"\n".join(parts)


def side(ctx, table, rowid):
    """The row of a message_row_id table for the message, None when the variant lacks the table or has no row."""
    return ctx["rows"].get(table, {}).get(rowid)


def num(c, name):
    """A REAL (or INTEGER) cell as the parser keeps it in raw."""
    v = c.row.get(name)
    if v is None:
        return None
    if isinstance(v, (int, float)) and not isinstance(v, bool):
        return v
    c.unknown[c.prefix + name] = "type-mismatch"
    return None


def jid_state(ctx, raw, unknown, jid_id, field):
    """(address, state) of a jid row named by a feature column (> 0)."""
    row = ctx["jid"].get(jid_id)
    if row is None:
        return None, "no-jid-row"
    a = addr_of_jid(row, raw, unknown, field)
    return a, a["state"]


def features(ctx, r, c, raw, unknown, modern, t, legacy_sys):
    """The side tables of a message (spec 6.5): fills raw and returns the revoke source, or None for a live message."""
    rid = r["rowid"]
    revoke = None
    label = None
    if modern:
        label = "revoked" if t in REVOKED_TYPES else MODERN_TYPE.get(t)
    elif t in LEGACY_TYPE and not legacy_sys:
        label = LEGACY_TYPE[t]

    def cells(table, row):
        return Cells(row, raw, unknown, table + ".")

    rrow = side(ctx, "message_revoked", rid)
    if rrow is not None:
        revoke = "message_revoked"
        rc, rv = cells("message_revoked", rrow), {}
        k = rc.text("revoked_key_id")
        if k is not None:
            rv["revoked_key_id"] = cleaned(raw, "revoke.revoked_key_id", k)
        adm = rc.int("admin_jid_row_id")
        if adm is not None and adm > 0:
            a, st = jid_state(ctx, raw, unknown, adm, "revoke.admin_jid")
            if a is not None and a["str"]:
                rv["admin_jid"] = a["str"]
            elif st:
                rv["admin_jid_state"] = st
                if a is not None and a["parts"] is not None:
                    rv["admin_jid_parts"] = a["parts"]
        ts_ = rc.int("revoke_timestamp")
        if ts_ is not None:
            rv["revoke_timestamp"], rv["revoke_timestamp_unit"] = ts_, "unestablished"
        raw["revoke"] = rv
    elif modern and label == "revoked":
        revoke = "message_type"

    if modern:
        qrow = side(ctx, "message_quoted", rid)
        if qrow is not None:
            qc, q = cells("message_quoted", qrow), {"copy": True}
            k = qc.text("key_id")
            if k is not None:
                q["key_id"] = cleaned(raw, "quoted.key_id", k)
            for n in ("from_me", "message_type", "timestamp"):
                v = qc.int(n)
                if v is not None:
                    q[n] = v
            sid = qc.int("sender_jid_row_id")
            if sid is not None and sid > 0:
                a, st = jid_state(ctx, raw, unknown, sid, "quoted.sender_jid")
                if a is not None and a["str"]:
                    q["sender_jid"] = a["str"]
                elif st:
                    q["sender_jid_state"] = st
            b = qc.text("text_data")
            if b:
                q["text"] = cleaned(raw, "quoted.text", b)
            raw["quoted"] = q
        erow = side(ctx, "message_edit_info", rid)
        if erow is not None:
            ec, e = cells("message_edit_info", erow), {}
            k = ec.text("original_key_id")
            if k is not None:
                e["original_key_id"] = cleaned(raw, "edit.original_key_id", k)
            for n in ("edited_timestamp", "sender_timestamp"):
                v = ec.int(n)
                if v is not None:
                    e[n], e[n + "_unit"] = v, "unestablished"
            v = ec.int("display_type")
            if v is not None:
                e["display_type"] = v
            raw["edit"] = e
        if t == 10:
            crow = side(ctx, "message_call_log", rid)
            if crow is not None:
                v = cells("message_call_log", crow).int("call_log_row_id")
                if v is not None:
                    raw["call_log_row_id"] = v
        if label in ("location", "live_location"):
            lrow = side(ctx, "message_location", rid)
            if lrow is not None:
                lc, loc = cells("message_location", lrow), {}
                for n in ("latitude", "longitude", "live_location_final_latitude", "live_location_final_longitude"):
                    v = num(lc, n)
                    if v is not None:
                        loc[n] = v
                for n in ("place_name", "place_address", "url"):
                    b = lc.text(n)
                    if b is not None:
                        loc[n] = cleaned(raw, "location." + n, b)
                for n in ("live_location_share_duration", "live_location_sequence_number", "live_location_final_timestamp",
                          "map_download_status"):
                    v = lc.int(n)
                    if v is not None:
                        loc[n] = v
                if loc:
                    raw["location"] = loc
    else:
        qid = c.int("quoted_row_id")
        if qid is not None:
            raw["quoted_row_id"] = qid
            if qid > 0:
                if "messages_quotes" not in ctx["rows"]:
                    raw["join_undecided"] = sorted(set(raw.get("join_undecided", [])) | {"messages_quotes"})
                else:
                    qrow = ctx["rows"]["messages_quotes"].get(qid)
                    if qrow is None:
                        raw["quoted_state"] = "no-messages_quotes-row"
                    else:
                        qc, q = cells("messages_quotes", qrow), {"copy": True}
                        k = qc.text("key_id")
                        if k is not None:
                            q["key_id"] = cleaned(raw, "quoted.key_id", k)
                        v = qc.int("key_from_me")
                        if v is not None:
                            q["from_me"] = v
                        v = qc.int("timestamp")
                        if v is not None:
                            q["timestamp"] = v
                        st = stored(raw, "media_wa_type", qrow.get("media_wa_type"))
                        if st is not None:
                            q["message_type"] = st
                        b = qc.text("remote_resource")
                        if b:
                            q["sender_jid"] = cleaned(raw, "quoted.sender_jid", b)
                        b = qc.text("data")
                        if b:
                            q["text"] = cleaned(raw, "quoted.text", b)
                        raw["quoted"] = q
        if label == "location":
            loc = {}
            for n in ("latitude", "longitude"):
                v = num(c, n)
                if v is not None:
                    loc[n] = v
            if loc:
                raw["location"] = loc

    if (modern and label == "system") or legacy_sys:
        srow = side(ctx, "message_system", rid)
        if srow is not None:
            v = cells("message_system", srow).int("action_type")
            if v is not None:
                raw["action_type"] = v
                if v == 18:
                    raw["label_action_type"] = "security_code_changed"
        vrow = side(ctx, "message_system_value_change", rid)
        if vrow is not None:
            b = cells("message_system_value_change", vrow).text("old_data")
            if b is not None:
                raw["old_data"] = cleaned(raw, "old_data", b)
        nrow = side(ctx, "message_system_number_change", rid)
        if nrow is not None:
            nc, n_ = cells("message_system_number_change", nrow), {}
            for n in ("old_jid_row_id", "new_jid_row_id"):
                v = nc.int(n)
                if v is not None:
                    n_[n] = v
            raw["number_change"] = n_

    vrow = side(ctx, "message_view_once_media", rid)
    if vrow is not None:
        v = cells("message_view_once_media", vrow).int("state")
        if v is not None:
            raw["view_once_state"] = v
    erow = side(ctx, "message_ephemeral", rid)
    if erow is not None:
        ec, e = cells("message_ephemeral", erow), {}
        v = ec.int("duration")
        if v is not None:
            e["duration"] = v
        v = ec.int("expire_timestamp")
        if v is not None:
            e["expire_timestamp"], e["expire_timestamp_unit"] = v, "unestablished"
        raw["ephemeral"] = e
    frow = side(ctx, "message_forwarded", rid)
    if frow is not None:
        fc, fw = cells("message_forwarded", frow), {}
        for n in ("forward_score", "forward_origin"):
            v = fc.int(n)
            if v is not None:
                fw[n] = v
        raw["forwarded"] = fw
    return revoke


def lid_raw(ctx, raw, unknown, chat, sender, participants):
    """What jid_map says about the LIDs among the addresses of the record (D6, spec 6.4.4): only the database maps a LID."""
    if "jid_map" not in ctx["rows"]:
        return
    cands = [chat]
    if sender["str"] and any(p["address"] == sender["str"] for p in participants):
        cands.append(sender)
    seen, mapped, unmapped, unresolved = set(), [], [], []
    for a in cands:
        s = a["str"]
        if not s or s in seen:
            continue
        p = parse_jid(s)
        if not p or p[1] != "lid":
            continue
        seen.add(s)
        if a.get("rowid") is not None:
            jid_id = a["rowid"]
        else:
            rows = ctx["jid_by_raw"].get(s.encode("utf-8"), [])
            if len(rows) == 0:
                unmapped.append(s)
                continue
            if len(rows) > 1:
                raw["join_undecided"] = sorted(set(raw.get("join_undecided", [])) | {"jid"})
                continue
            jid_id = rows[0]["rowid"]
        mrow = ctx["rows"]["jid_map"].get(jid_id)
        if mrow is None:
            unmapped.append(s)
            continue
        target = Cells(mrow, raw, unknown, "jid_map.").int("jid_row_id")
        trow = ctx["jid"].get(target) if target is not None else None
        pn = addr_of_jid(trow, raw, unknown, "lid_map.pn") if trow is not None else None
        if pn is None or not pn["str"]:
            unresolved.append(s)
            continue
        mapped.append({"lid": s, "pn": pn["str"], "source": "jid_map"})
    if mapped:
        raw["lid_map"] = mapped
    if unmapped:
        raw["lid_unmapped"] = unmapped
    if unresolved:
        raw["lid_unresolved"] = unresolved


def nonempty(v):
    """A job list cell that holds something: any value that is not NULL and not an empty text or blob."""
    if v is None:
        return False
    if isinstance(v, bytes):
        return len(v) > 0
    return True


def job_set(view, ctx):
    """deleted_chat_job as 4I-R4 reads it: usable jobs by chat (a job of a chat that does not exist is dangling, a job
    whose chat_row_id is not an INTEGER is unusable), the jobs that can make rows undecided, the smallest id read."""
    js = {"by_chat": {}, "unusable": 0, "dangling": 0, "warn": [], "min_id": None, "any": False}
    for r in view.get("deleted_chat_job", []):
        js["any"] = True
        jid = r["rowid"]
        js["min_id"] = jid if js["min_id"] is None else min(js["min_id"], jid)
        cid = r.get("chat_row_id")
        if not is_int(cid):
            js["unusable"] += 1
            js["warn"].append((jid, "chat-row-id-not-integer"))
            continue
        if "chat" in view and cid not in ctx["chat"]:
            js["dangling"] += 1
            continue
        bounds, ok = [], True
        for n in ("deleted_message_row_id", "deleted_starred_message_row_id"):
            v = r.get(n)
            bounds.append(v if is_int(v) else None)
            ok = ok and (v is None or is_int(v))
        lists = nonempty(r.get("deleted_message_categories")) or nonempty(r.get("singular_message_delete_rows_id"))
        js["by_chat"].setdefault(cid, []).append({"id": jid, "msg": bounds[0], "starred": bounds[1], "ok": ok, "lists": lists})
        if lists:
            js["warn"].append((jid, "categories-or-singular-list-not-interpreted"))
        elif not ok:
            js["warn"].append((jid, "job-bound-not-integer"))
    return js


def job_active(js):
    return bool(js["by_chat"]) or js["unusable"] > 0


def purge_eval(js, k):
    """(verdict, job ids, smallest job, reason) of a row key: chat, known, undecided, starred, starred_ok, order,
    order_ok. verdict is None, 'cleared' or 'undecided' (4I-R4 c, d, e)."""
    incomplete = js["unusable"] > 0
    if k["undecided"]:
        if js["any"]:
            return "undecided", [], js["min_id"], "row-chat-undecided"
        return None, [], None, None
    if not k["known"]:
        return None, [], None, None
    jobs = js["by_chat"].get(k["chat"], [])
    if k["starred_ok"] and k["order_ok"]:
        ids = [j["id"] for j in jobs if (j["starred"] if k["starred"] == 1 else j["msg"]) is not None
               and k["order"] <= (j["starred"] if k["starred"] == 1 else j["msg"])]
        if ids:
            return "cleared", ids, ids[0], None
    for j in jobs:
        if j["lists"]:
            return "undecided", [], j["id"], "categories-or-singular-list-not-interpreted"
    for j in jobs:
        if not j["ok"]:
            return "undecided", [], j["id"], "job-bound-not-integer"
    if jobs and not (k["starred_ok"] and k["order_ok"]):
        return "undecided", [], jobs[0]["id"], "row-key-not-integer"
    if incomplete:
        return "undecided", [], (jobs[0]["id"] if jobs else None), "job-set-incomplete"
    return None, [], None, None


def starred_of(v):
    if v is None:
        return 0, True
    if is_int(v):
        return v, v in (0, 1)
    return 0, False


def modern_key(row):
    st, st_ok = starred_of(row.get("starred"))
    cid, order = row.get("chat_row_id"), row.get("sort_id")
    return {"chat": cid if is_int(cid) else None, "known": is_int(cid), "undecided": False, "starred": st,
            "starred_ok": st_ok, "order": order if is_int(order) else None, "order_ok": is_int(order)}


def legacy_key(row, ctx):
    st, st_ok = starred_of(row.get("starred"))
    k = {"chat": None, "known": False, "undecided": False, "starred": st, "starred_ok": st_ok, "order": row["rowid"],
         "order_ok": True}
    kb = row.get("key_remote_jid")
    if kb is None or (is_text(kb) and len(kb) == 0):
        return k
    if not is_text(kb):
        k["undecided"] = True
        return k
    jids = ctx["jid_by_raw"].get(bytes(kb), [])
    if len(jids) > 1:
        k["undecided"] = True
        return k
    if len(jids) == 0:
        return k
    chats = [c for c in ctx["chat"].values() if c.get("jid_row_id") == jids[0]["rowid"]]
    if len(chats) > 1:
        k["undecided"] = True
    elif chats:
        k["chat"], k["known"] = chats[0]["rowid"], True
    return k


def apply_purge(raw, payload, verdict, via=None):
    """Writes the outcome on raw and the payload (4I-R4 g); returns True when the row is deleted by a cleared chat."""
    kind, ids, job, reason = verdict
    if kind == "cleared":
        raw["pending_purge_jobs"] = ids
        if via:
            raw["deleted_via"] = via
        if "deleted" not in payload:
            payload["deleted"] = {"source": "deleted_chat_job", "job": job}
            return True
    elif kind == "undecided":
        raw["pending_purge"] = "undecided"
        if job:
            raw["pending_purge_job"] = job
        raw["pending_purge_reason"] = reason
        if via:
            raw["pending_purge_via"] = via
    return False


MEDIA_LABELS = {"image", "audio", "video", "gif", "document", "sticker"}


def pkg_folder(app):
    return ("com.whatsapp", "WhatsApp") if app == "whatsapp" else ("com.whatsapp.w4b", "WhatsApp Business")


def path_elems(p):
    """The elements of an absolute path a logical glob can express, else None."""
    if not p.startswith("/") or any(ch in p for ch in "*\\\x00"):
        return None
    el = p[1:].split("/")
    if len(el) > 64 or any(e in ("", ".", "..") for e in el):
        return None
    return el


def media_root(el):
    """(user, named): /storage/emulated/<n>, /data/media/<n> and /media/<n> name user n; anything else names none."""
    def user(x):
        return int(x) if re.fullmatch(r"[0-9]{1,6}", x, re.ASCII) else None
    if len(el) == 3 and el[0] == "storage" and el[1] == "emulated" and user(el[2]) is not None:
        return user(el[2]), True
    if len(el) == 3 and el[0] == "data" and el[1] == "media" and user(el[2]) is not None:
        return user(el[2]), True
    if len(el) == 2 and el[0] == "media" and user(el[1]) is not None:
        return user(el[1]), True
    return None, False


def split_media(path, app):
    """(root, rest, form) of a path under the app's own folder, or None: <root>/<folder>/<rest> or
    <root>/Android/media/<package>/<folder>/<rest> (spec 6.7)."""
    pkg, folder = pkg_folder(app)
    el = path_elems(path)
    if el is None:
        return None
    for i in range(len(el)):
        if el[i] == "Android" and i + 4 < len(el) and el[i + 1:i + 4] == ["media", pkg, folder]:
            return media_root(el[:i]), "/".join(el[i + 4:]), "android11"
        if el[i] == folder and i + 1 < len(el):
            return media_root(el[:i]), "/".join(el[i + 1:]), "legacy"
    return None


def known_root(path):
    el = path_elems(path)
    if el is None:
        return False
    if el[0] == "sdcard" or el[:3] == ["storage", "self", "primary"]:
        return True
    if len(el) >= 3 and el[:2] in (["storage", "emulated"], ["data", "media"]):
        return media_root(el[:3])[1]
    return len(el) >= 2 and el[0] == "media" and media_root(el[:2])[1]


def resolve_media(ref, ctx):
    """(state, path): how the file a row names was looked up (R5, R5a, R6). State is none, missing, found, other_source,
    ambiguous, other_user or user_undecided."""
    remote, app = ctx["remote"], app_of(ctx["remote"])
    m = re.match(r"/data/user/([0-9]+)/", remote)
    db_user = int(m.group(1)) if m else 0
    pkg = pkg_folder(app)[0]
    sole = not any(e["path"].endswith("/" + pkg + "/databases/msgstore.db") for e in ctx["extras"])
    if not ref:
        return "none", None
    if ref.startswith("/"):
        if not known_root(ref) or split_media(ref, app) is None:
            return "none", None
        cands = [e for e in ctx["extras"] if e["path"] == ref]
    elif ref.startswith("Media/") and path_elems("/" + ref) is not None:
        cands = []
        for e in ctx["extras"]:
            sp = split_media(e["path"], app)
            if sp is not None and sp[1] == ref:
                cands.append(e)
    else:
        return "none", None
    if not cands:
        return "missing", None
    same = [e for e in cands if e["device"] == "SER1"]
    if not same:
        return "other_source", None
    link, other_user = [], 0
    for e in same:
        (user, named), _, _ = split_media(e["path"], app)
        if named and user == db_user or (not named and sole):
            link.append(e)
        elif named:
            other_user += 1
    if len(link) == 1:
        return "found", link[0]["path"]
    if len(link) > 1:
        return "ambiguous", None
    if other_user:
        return "other_user", None
    return "user_undecided", None


def media_modern(ctx, r, raw, unknown):
    """The media of a modern message: {name, mime, size, ref, raw} from its message_media row, or None. The table present
    and no row for the message is raw.media_state."""
    if "message_media" not in ctx["rows"]:
        return None
    mrow = ctx["rows"]["message_media"].get(r["rowid"])
    if mrow is None:
        raw["media_state"] = "no-message_media-row"
        return None
    c = Cells(mrow, raw, unknown, "message_media.")
    mf = {"name": None, "mime": None, "size": None, "ref": None, "raw": {}}
    b = c.text("media_name")
    if b:
        mf["name"] = cleaned(raw, "attachments.name", b)
    b = c.text("mime_type")
    if b:
        mf["mime"] = cleaned(raw, "attachments.mime", b)
    b = c.text("file_path")
    if b:
        mf["ref"] = cleaned(raw, "attachments.source_ref", b)
    v = c.int("file_size")
    if v is not None:
        mf["raw"]["file_size"] = v
        if v > 0:
            mf["size"] = v
    for n in ("file_length", "transferred"):
        v = c.int(n)
        if v is not None:
            mf["raw"][n] = v
    for n in ("message_url", "direct_path", "file_hash"):
        b = c.text(n)
        if b:
            mf["raw"][n] = cleaned(raw, n, b)
    if mrow.get("media_key") is not None:
        mf["raw"]["media_key_present"] = isinstance(mrow["media_key"], C.Blob)
    return mf


def media_legacy(c, raw):
    """The media columns of a legacy message: no local path, so never linked."""
    mf = {"name": None, "mime": None, "size": None, "ref": None, "raw": {}}
    b = c.text("media_name")
    if b:
        mf["name"] = cleaned(raw, "attachments.name", b)
    b = c.text("media_mime_type")
    if b:
        mf["mime"] = cleaned(raw, "attachments.mime", b)
    v = c.int("media_size")
    if v is not None:
        mf["raw"]["file_size"] = v
        if v > 0:
            mf["size"] = v
    for col, key in (("media_url", "media_url"), ("media_hash", "file_hash")):
        b = c.text(col)
        if b:
            mf["raw"][key] = cleaned(raw, key, b)
    if mf["name"] or mf["mime"] or mf["raw"]:
        return mf
    return None


def apply_media(mf, raw, ctx):
    """The attachments of a media message and the counters of how its file was looked up (R5, R5a, R6)."""
    raw.update(mf["raw"])
    if not (mf["name"] or mf["mime"] or mf["size"] or mf["ref"]):
        return []
    a = {}
    if mf["name"]:
        a["name"] = mf["name"]
    if mf["mime"]:
        a["mime"] = mf["mime"]
    if mf["size"]:
        a["size"] = mf["size"]
    if mf["ref"]:
        a["source_ref"] = mf["ref"]
    state, path = resolve_media(mf["ref"], ctx)
    if state == "found":
        a["artifact_id"], a["artifact_sha256"] = "@id:" + path, "@sha256:" + path
    raw["attachments_resolved"] = 1 if state == "found" else 0
    raw["attachments_unresolved"] = 1 if state in ("none", "missing") else 0
    for st, key in (("ambiguous", "attachments_ambiguous"), ("other_source", "attachments_other_source"),
                    ("other_user", "attachments_other_user"), ("user_undecided", "attachments_user_undecided")):
        if state == st:
            raw[key] = 1
    return [a]


CALL_RESULT = {5: "answered", 2: "not_answered", 3: "unavailable"}
CALL_MAX_DURATION = 10**9
EXACT = 1 << 53


def expect_call(r, ctx, remote):
    """A call_log row (spec 6.6, 7.4, R10): one call. The time is a millisecond value with no plausibility floor."""
    raw = {"source_table": "call_log", "app": app_of(remote), "app_basis": "path"}
    unknown = {}
    c = Cells(r, raw, unknown)
    me = read_code(c, "from_me")
    direction = {0: "in", 1: "out"}.get(me, "unknown") if me is not None else "unknown"
    vc = read_code(c, "video_call")
    res = read_code(c, "call_result")
    outcome = "unknown"
    if res == 5:
        outcome = "answered"
    elif res == 2 and direction == "in":
        outcome = "missed"
    if outcome == "unknown" and res in CALL_RESULT:
        raw["label_call_result"] = CALL_RESULT[res]  # the value is known but no outcome fits (7.4)
    payload = {}
    payload.update({"direction": direction, "outcome": outcome, "service": "whatsapp"})
    if vc in (0, 1):
        payload["kind"] = "video" if vc == 1 else "voice"
    # the time: milliseconds, no floor (R10)
    t, t_end, dur = None, None, c.int("duration")
    ts = c.int("timestamp")
    if ts is not None:
        raw["timestamp"] = ts
        st = C.classify(ts, "ms")
        if st != "valid":
            raw["timestamp_status"] = st
        else:
            t = C.iso(ts, "ms")
            if dur is not None and 0 < dur <= CALL_MAX_DURATION and ts + dur * 1000 < C.MAX_SECONDS * 1000:
                t_end = C.iso(ts + dur * 1000, "ms")
                raw["time_end_basis"] = "start+duration"
    if dur is not None:
        raw["duration"] = dur
        if abs(dur) > EXACT:
            unknown["duration"] = "precision"
        elif dur >= 0:
            payload["duration_s"] = dur
    # the counterparty
    addr = ABSENT
    jid_id = c.int("jid_row_id")
    if jid_id is not None and jid_id > 0:
        jrow = ctx["jid"].get(jid_id)
        if jrow is None:
            addr = {"str": None, "parts": None, "state": "no-jid-row"}
        else:
            addr = addr_of_jid(jrow, raw, unknown, "address")
    if addr["str"]:
        payload["address"] = addr["str"]
    else:
        raw["address_state"] = addr["state"]
        if addr["parts"] is not None:
            raw["address_parts"] = addr["parts"]
    gid = c.int("group_jid_row_id")
    if gid is not None and gid != 0:
        grow = ctx["jid"].get(gid) if gid > 0 else None
        ga = addr_of_jid(grow, raw, unknown, "group_jid") if grow is not None else None
        if ga is not None and ga["str"]:
            raw["group_jid"] = ga["str"]
        else:
            raw["group_jid_state"] = ga["state"] if ga is not None else ("absent" if gid < 0 else "no-jid-row")
    cid = c.text("call_id")
    if cid is not None:
        raw["call_id"] = cleaned(raw, "call_id", cid)
    for n in ("bytes_transferred", "call_type", "call_link_row_id", "is_dnd_mode_on"):
        v = c.int(n)
        if v is not None:
            raw[n] = v
    parts = []
    for prow in ctx["participants"].get(r["rowid"], []):
        pc = Cells(prow, raw, unknown, "call_log_participant_v2.")
        e = {}
        pj = pc.int("jid_row_id")
        pr = ctx["jid"].get(pj) if pj is not None and pj > 0 else None
        pa = addr_of_jid(pr, raw, unknown, "participant_jid") if pr is not None else None
        if pa is not None and pa["str"]:
            e["jid"] = pa["str"]
        else:
            e["jid_state"] = pa["state"] if pa is not None else ("no-jid-row" if pj is not None and pj > 0 else "absent")
        v = pc.int("call_result")
        if v is not None:
            e["call_result"] = v
        parts.append(e)
    if parts:
        raw["participants"] = parts[:1000]
        if len(parts) > 1000:
            raw["participants_total"], raw["participants_truncated"] = len(parts), True
    lid_raw(ctx, raw, unknown, addr, ABSENT, [])
    if unknown:
        raw["unknown_columns"] = unknown
    payload["raw"] = raw
    a = payload.get("address")
    plain = a is None or (a.isascii() and len(a.encode()) <= 64)
    summ = ("call %s %s" % (direction, outcome) + (" " + a if a else "")) if plain else None
    return {"table": "call_log", "rowid": r["rowid"], "file": "any", "type": "call", "source_path": remote, "time": t,
            "time_end": t_end, "times": {}, "deleted": False, "summary": summ, "body": None, "payload": payload}


def modern_chat(ctx, raw, unknown, cid):
    """(chat address, title, chat_state) of a modern chat_row_id (spec 6.4.1): raw.chat_row_id is the key as stored."""
    chat, title, state = ABSENT, None, None
    if cid is None:
        return chat, title, state
    raw["chat_row_id"] = cid
    crow = ctx["chat"].get(cid)
    if crow is None:
        return chat, title, "no-chat-row"
    cc = Cells(crow, raw, unknown, "chat.")
    sub = cc.text("subject")
    if sub:
        title = cleaned(raw, "thread.title", sub)
    jid_id = cc.int("jid_row_id")
    jrow = ctx["jid"].get(jid_id) if jid_id is not None else None
    if jrow is None:
        return chat, title, "no-jid-row"
    chat = addr_of_jid(jrow, raw, unknown, "chat_jid")
    if chat["parts"] is not None:
        raw["chat_jid_parts"] = chat["parts"]
    return chat, title, state


def modern_sender(ctx, raw, unknown, sid):
    """The author address of a modern sender_jid_row_id: above 0 names a jid row, 0, -1 and NULL name none."""
    sender = ABSENT
    if sid is not None:
        raw["sender_jid_row_id"] = sid
    if sid is not None and sid > 0:
        jrow = ctx["jid"].get(sid)
        if jrow is None:
            sender = {"str": None, "parts": None, "state": "no-jid-row"}
        else:
            sender = addr_of_jid(jrow, raw, unknown, "sender_jid")
            if sender["parts"] is not None:
                raw["sender_jid_parts"] = sender["parts"]
    if sender["str"]:
        raw["sender_jid"] = sender["str"]
    return sender


def expect_reaction(r, ctx, remote):
    """A message_add_on row of type 56 (spec 6.5 as amended by 4I-R20): a reaction record, or None for another type.
    The direction and the sender are unknown (each rests on one source): from_me and sender_jid_row_id stay in raw."""
    raw = {"source_table": "message_add_on", "app": app_of(remote), "app_basis": "path"}
    unknown = {}
    c = Cells(r, raw, unknown)
    raw["message_add_on_type"] = r["message_add_on_type"] if is_int(r.get("message_add_on_type")) else None
    if raw["message_add_on_type"] != 56:
        return None
    raw["label_message_add_on_type"] = "reaction"
    s = stored(raw, "from_me", r.get("from_me"))
    if s is not None:
        raw["from_me"] = s
    guid = c.text("key_id")
    guid = cleaned(raw, "guid", guid) if guid else None
    time_t = read_instant(c, "timestamp")
    own = c.int("chat_row_id")
    if own is not None:
        raw["add_on_chat_row_id"] = own
    sender = modern_sender(ctx, raw, unknown, c.int("sender_jid_row_id"))

    # the parent message: its key is the target, its chat is the thread (the add-on's own chat when there is no parent)
    target, chat_key, parent_purge = None, own, (None, [], None, None)
    pid = c.int("parent_message_row_id")
    if pid is None:
        raw["parent_state"] = "no-parent-key"
    else:
        prow = ctx["messages"].get(pid)
        if prow is None:
            raw["parent_state"] = "no-message-row"
        else:
            pc = Cells(prow, raw, unknown, "message.")
            k = pc.text("key_id")
            target = cleaned(raw, "target_guid", k) if k else None
            chat_key = pc.int("chat_row_id")
            if job_active(ctx["jobs"]):
                parent_purge = purge_eval(ctx["jobs"], modern_key(prow))
    chat, title, chat_state = modern_chat(ctx, raw, unknown, chat_key)

    # the reaction text
    body = None
    if not ctx["has_reaction"]:
        raw["reaction_state"] = "no-reaction-table"
    else:
        rrow = ctx["reactions"].get(r["rowid"])
        if rrow is None:
            raw["reaction_state"] = "no-reaction-row"
        else:
            rc = Cells(rrow, raw, unknown, "message_add_on_reaction.")
            txt = rc.text("reaction")
            v = rc.int("sender_timestamp")
            if v is not None:
                raw["sender_timestamp"], raw["sender_timestamp_unit"] = v, "unestablished"
            if txt is None and rrow.get("reaction") is not None:
                raw["reaction_state"] = "unreadable"
            elif not txt:
                raw["reaction_empty"] = True
            else:
                cl = text_of(txt)
                if cl.text:
                    body = cl.text
                    if cl.flags:
                        C.mark(raw, "body")
    if chat["str"]:
        raw["chat_jid"] = chat["str"]
    if is_status(chat):
        raw["chat_class"] = "status"
    if chat_state:
        raw["chat_state"] = chat_state
    elif chat["state"] == "too-long":
        raw["chat_state"] = "too-long"
    group = group_of(chat)
    lid_raw(ctx, raw, unknown, chat, ABSENT, [])
    if unknown:
        raw["unknown_columns"] = unknown
    payload = {"channel": "whatsapp", "direction": "unknown", "kind": "reaction", "participants": [],
               "participants_unknown": True, "status": "unknown", "raw": raw}
    cleared = apply_purge(raw, payload, parent_purge, via="parent")
    thread = {}
    if chat["str"]:
        thread["id"] = chat["str"]
    if title:
        thread["title"] = title
    if group is not None:
        thread["group"] = group
    if thread:
        payload["thread"] = thread
    if guid:
        payload["guid"] = guid
    if target:
        payload["target_guid"] = target
    if body is not None:
        payload["text_source"] = "text"
    return {"table": "message_add_on", "rowid": r["rowid"], "file": "any", "type": "message", "source_path": remote,
            "time": time_t, "time_end": None, "times": {}, "deleted": cleared,
            "summary": summary_of("unknown", chat["str"] or "unknown", body), "body": body, "payload": payload}


def expect_message(table, r, ctx, remote):
    """The expectation of one messages or message row as the engine reads it back (spec 6.4, 7.1, 7.3, 7.6)."""
    modern = table == "message"
    raw = {"source_table": table, "app": app_of(remote), "app_basis": "path"}
    unknown = {}
    c = Cells(r, raw, unknown)
    me_col = "from_me" if modern else "key_from_me"
    me = read_code(c, me_col)
    direction = {0: "in", 1: "out"}.get(me, "unknown") if me is not None else "unknown"
    t = read_code(c, "message_type" if modern else "media_wa_type", text_typed=not modern)
    status = read_code(c, "status")
    legacy_sys = (not modern) and t == 0 and status == 6
    guid = c.text("key_id")
    guid = cleaned(raw, "guid", guid) if guid else None

    # kind and label
    kind = "unknown"
    if modern:
        if t in MODERN_TYPE:
            kind = "text" if t == 0 else "system" if t == 7 else "media"
            raw["label_message_type"] = MODERN_TYPE[t]
        elif t in REVOKED_TYPES:
            raw["label_message_type"] = "revoked"
    elif t in LEGACY_TYPE and not legacy_sys:
        kind = "text" if t == 0 else "media"
        raw["label_media_wa_type"] = LEGACY_TYPE[t]

    # times
    time_t = read_instant(c, "timestamp")
    recv = read_instant(c, "received_timestamp")
    times = {}
    if recv:
        times["received"] = recv
    if not modern:
        deliv = read_instant(c, "receipt_device_timestamp")
        if deliv:
            times["delivered"] = deliv
    v = c.int("receipt_server_timestamp")
    if v is not None:
        raw["receipt_server_timestamp"] = v
        if modern:
            raw["receipt_server_timestamp_unit"] = "unestablished"
    for n in (MODERN_RAW_INTS if modern else LEGACY_RAW_INTS):
        v = c.int(n)
        if v is not None:
            raw[n] = v

    # chat, title and sender
    chat, title, chat_state, sender = ABSENT, None, None, ABSENT
    if modern:
        chat, title, chat_state = modern_chat(ctx, raw, unknown, c.int("chat_row_id"))
        sender = modern_sender(ctx, raw, unknown, c.int("sender_jid_row_id"))
    else:
        kb = c.text("key_remote_jid")
        if kb is not None:
            if len(kb) == 0:
                chat_state = "empty"
            else:
                chat = addr_of_text(kb, raw, "chat_jid")
                if ctx["has_chat_list"]:
                    rows = ctx["chat_list"].get(bytes(kb), [])
                    if len(rows) == 0:
                        raw["chat_list_state"] = "no-chat_list-row"
                    elif len(rows) == 1:
                        sub = Cells(rows[0], raw, unknown, "chat_list.").text("subject")
                        if sub:
                            title = cleaned(raw, "thread.title", sub)
                    else:
                        raw["join_undecided"] = ["chat_list"]
        rr = c.text("remote_resource")
        if rr:
            sender = addr_of_text(rr, raw, "remote_resource")
            raw["remote_resource"] = cleaned(raw, "remote_resource", rr)
            if sender["str"]:
                raw["sender_jid"] = sender["str"]
        cap = c.text("media_caption")
        if cap:
            raw["media_caption"] = cleaned(raw, "media_caption", cap)
        if legacy_sys:
            ms_ = c.int("media_size")
            if ms_ is not None:
                raw["media_size"] = ms_
    if chat["str"]:
        raw["chat_jid"] = chat["str"]
    if is_status(chat):
        raw["chat_class"] = "status"
    if chat_state:
        raw["chat_state"] = chat_state
    elif chat["state"] == "too-long":
        raw["chat_state"] = "too-long"
    group = group_of(chat)

    # participants (spec 6.4.3, R3)
    if sender["state"] in ("no-jid-row", "no-raw-string"):
        raw["sender_state"] = sender["state"]
    participants, unknown_p = [], False
    if direction == "out" and chat["str"]:
        participants = [{"role": "to", "address": chat["str"]}]
    elif direction == "in" and is_status(chat):
        unknown_p = True
    elif direction == "in" and group:
        if sender["str"]:
            participants = [{"role": "from", "address": sender["str"]}]
        else:
            unknown_p = True
            raw["sender_state"] = sender["state"]
            if sender["parts"] is not None:
                raw["from_jid"] = dict(sender["parts"])
    elif direction == "in" and chat["str"]:
        participants = [{"role": "from", "address": chat["str"]}]
    else:
        unknown_p = True
        if chat["parts"] is not None and direction in ("in", "out"):
            raw["from_jid" if direction == "in" else "to_jid"] = dict(chat["parts"])

    # body
    text_source = None
    if modern:
        txt = c.text("text_data")
        text_source = "text"
        if t == 4 and ctx["has_vcard"]:
            cards = ctx["vcards"].get(r["rowid"], [])
            if cards:
                txt, text_source = join_text(cards), "part"
            else:
                raw["vcard_state"] = "no-message_vcard-row"
    else:
        txt = c.text("data")
        text_source = "text"
        if t == 4 and not legacy_sys:
            text_source = "part"
    body = None
    if txt:
        cl = text_of(txt)
        if cl.text:
            body = cl.text
            if cl.flags:
                C.mark(raw, "body")
    revoke = features(ctx, r, c, raw, unknown, modern, t, legacy_sys)
    if revoke:
        kind = "unknown"
        if txt:
            raw["revoked_row_text"] = cleaned(raw, "revoked_row_text", txt)
        txt, text_source = None, None
        body = None
    lid_raw(ctx, raw, unknown, chat, sender, participants)
    attachments = []
    lab = MODERN_TYPE.get(t) if modern else (None if legacy_sys else LEGACY_TYPE.get(t))
    if not revoke and kind == "media" and lab in MEDIA_LABELS:
        mf = media_modern(ctx, r, raw, unknown) if modern else media_legacy(c, raw)
        attachments = apply_media(mf, raw, ctx) if mf else []
    if unknown:
        raw["unknown_columns"] = unknown

    payload = {"channel": "whatsapp", "direction": direction, "kind": kind, "participants": participants,
               "status": "unknown", "raw": raw}
    if attachments:
        payload["attachments"] = attachments
    if unknown_p:
        payload["participants_unknown"] = True
    thread = {}
    if chat["str"]:
        thread["id"] = chat["str"]
    if title:
        thread["title"] = title
    if group is not None:
        thread["group"] = group
    if thread:
        payload["thread"] = thread
    if guid:
        payload["guid"] = guid
    if body is not None:
        payload["text_source"] = text_source
    counter = participants[0]["address"] if participants else (chat["str"] or "unknown")
    if revoke:
        payload["deleted"] = {"source": revoke}
    if job_active(ctx["jobs"]):
        key = modern_key(r) if modern else legacy_key(r, ctx)
        cleared = apply_purge(raw, payload, purge_eval(ctx["jobs"], key))
    else:
        cleared = False
    return {"table": table, "rowid": r["rowid"], "file": "any", "type": "message", "source_path": remote, "time": time_t,
            "time_end": None, "times": times, "deleted": bool(revoke) or cleared,
            "summary": summary_of(direction, counter, "[revoked, no content]" if revoke else body), "body": body,
            "payload": payload}


def context(view):
    """Lookup tables of the stored rows, by key."""
    ctx = {"chat": {}, "jid": {}, "vcards": {}, "chat_list": {}, "has_vcard": "message_vcard" in view,
           "has_chat_list": "chat_list" in view, "jid_by_raw": {}, "rows": {},
           "messages": {r["rowid"]: r for r in view.get("message", [])},
           "has_reaction": "message_add_on_reaction" in view, "participants": {},
           "reactions": {r["rowid"]: r for r in view.get("message_add_on_reaction", [])}}
    ctx["jobs"] = None
    for t in SIDE_TABLES:
        if t in view:
            ctx["rows"][t] = {r["rowid"]: r for r in view[t]}
    for r in view.get("chat", []):
        ctx["chat"][r["rowid"]] = r
    ctx["jobs"] = job_set(view, ctx)
    for r in view.get("call_log_participant_v2", []):
        if is_int(r.get("call_log_row_id")):
            ctx["participants"].setdefault(r["call_log_row_id"], []).append(r)
    for r in view.get("jid", []):
        ctx["jid"][r["rowid"]] = r
        if is_text(r.get("raw_string")):
            ctx["jid_by_raw"].setdefault(bytes(r["raw_string"]), []).append(r)
    for r in view.get("message_vcard", []):
        if is_text(r.get("vcard")) and is_int(r.get("message_row_id")):
            ctx["vcards"].setdefault(r["message_row_id"], []).append(r["vcard"])
    for r in view.get("chat_list", []):
        if is_text(r.get("key_remote_jid")):
            ctx["chat_list"].setdefault(bytes(r["key_remote_jid"]), []).append(r)
    return ctx


SIDE_TABLES = ["message_revoked", "message_quoted", "messages_quotes", "message_edit_info", "message_system",
               "message_system_value_change", "message_system_number_change", "message_call_log", "message_location",
               "message_view_once_media", "message_ephemeral", "message_forwarded", "jid_map", "message_media"]
TABLES = ["messages", "message", "jid", "chat", "chat_list", "message_vcard", "messages_vcards", "deleted_chat_job",
          "message_add_on", "message_add_on_reaction", "call_log",
          "call_log_participant_v2"] + SIDE_TABLES


def master_rows(files):
    """The schema table of the finished files as the engine reads it (WAL applied to a copy): [(type, name, sql)]."""
    work = C.tempfile.mkdtemp(prefix="whatsapp-master-")
    try:
        path = C.os.path.join(work, "v.db")
        for role, suffix in (("db", ""), ("wal", "-wal"), ("journal", "-journal")):
            if role in files:
                with open(path + suffix, "wb") as f:
                    f.write(files[role])
        con = C.sqlite3.connect(path)
        rows = con.execute("select type, name, sql from sqlite_master order by rowid").fetchall()
        con.close()
        return rows
    finally:
        C.shutil.rmtree(work, ignore_errors=True)


def view_source(sql):
    """Whether the SQL of message_view reads the legacy table (the token FROM messages, spec 6.2)."""
    return "messages" if re.search(r"from\s+messages(?![A-Za-z0-9_])", sql, re.I) else "other"


def oracle(files, remote, variant, ddl, generation, extras=()):
    view = C.engine_view(files, TABLES)
    master = master_rows(files)
    ctx = context(view)
    ctx["extras"], ctx["remote"] = [{"path": p, "device": d or "SER1", "sha": hashlib.sha256(b).hexdigest()} for p, b, d in extras], remote
    exp = C.expect_header(FAMILY, variant, ddl)
    tables = {n.lower() for t, n, s in master if t == "table" and not (s or "").upper().startswith("CREATE VIRTUAL")}
    n_messages = n_message = sentinel = 0
    sentinel_warnings = []
    for r in view.get("messages", []):
        if r.get("key_remote_jid") == b"-1":
            sentinel += 1
            sentinel_warnings.append({"table": "messages", "rowid": r["rowid"], "contains": "sentinel"})
            continue
        n_messages += 1
        exp["records"].append(expect_message("messages", r, ctx, remote))
    for r in view.get("message", []):
        n_message += 1
        exp["records"].append(expect_message("message", r, ctx, remote))
    for r in view.get("message_add_on", []):
        if not is_int(r.get("message_add_on_type")):
            exp["warnings"].append({"table": "message_add_on", "rowid": r["rowid"], "contains": "message_add_on_type"})
            continue
        rec = expect_reaction(r, ctx, remote)
        if rec:
            exp["records"].append(rec)
    for r in view.get("call_log", []):
        exp["records"].append(expect_call(r, ctx, remote))
    notes = exp["notes"]
    notes["generation"] = generation
    if "messages" in view:
        notes["rows.messages"] = str(n_messages)
        notes["sentinel_rows"] = str(sentinel)
    if "message" in view:
        notes["rows.message"] = str(n_message)
    vsrc = "absent"
    for t, n, s in master:
        if t == "view" and n == "message_view":
            vsrc = view_source(s or "")
    if generation == "transition":
        notes["message_view_source"] = vsrc
    if generation != "legacy" and "jid_map" not in tables:
        notes["jid_map"] = "absent"
    if "messages" in tables and "chat_list" not in tables:
        notes["table.chat_list"] = "absent"
    unmapped = [n for t, n, s in master if t == "table" and (n.lower() in UNMAPPED_EXACT or n.lower().startswith(UNMAPPED_PREFIX))]
    if unmapped:
        notes["unmapped_tables"] = ",".join(unmapped)
    notes["app"] = app_of(remote)
    notes["database_user"] = "10" if "/user/10/" in remote else "0"
    if "deleted_chat_job" in tables:
        notes["deleted_chat_jobs"] = str(len(view.get("deleted_chat_job", [])))
    for key, n in (("deleted_chat_jobs_unusable", ctx["jobs"]["unusable"]), ("deleted_chat_jobs_dangling", ctx["jobs"]["dangling"])):
        if n:
            notes[key] = str(n)
    for t, n, s in master:
        if t == "view" and n in ("available_message_view", "legacy_available_messages_view"):
            b = (s or "").encode("utf-8")
            notes["view_sql." + n] = "sha256=%s;bytes=%d" % (hashlib.sha256(b).hexdigest(), len(b))
    # the schema warnings come first (before any mapping), then the rows' own
    if n_messages and n_message:
        exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:schema", "contains": "whatsapp-both-generations"})
    if generation == "transition" and vsrc in ("messages", "other"):
        rows = "both" if n_messages and n_message else "messages" if n_messages else "message" if n_message else "none"
        if (vsrc == "messages" and rows == "message") or (vsrc == "other" and rows == "messages"):
            exp["warnings"].append({"table": "", "rowid": 0, "locator": "sqlite:schema",
                                    "contains": "whatsapp-message-view-disagrees"})
    for jid, reason in sorted(ctx["jobs"]["warn"]):
        exp["warnings"].append({"table": "deleted_chat_job", "rowid": jid,
                                "contains": "whatsapp-pending-purge-undecided: deleted_chat_job %d: %s" % (jid, reason)})
    exp["warnings"] += sentinel_warnings
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


def alloc_calls(js, calls):
    for c in calls:
        js.jid_id(c["jid"])
        if c["group"]:
            js.jid_id(c["group"])
        for addr, _ in c["parts"]:
            js.jid_id(addr)


def alloc(js, facts, addons=(), calls=()):
    """Allocates the jid and chat rows the facts name, in first-use order (the order the inserts use)."""
    alloc_calls(js, calls)
    for a in addons:
        js.chat_id(a["chat"])
        if isinstance(a["sender"], JidRef):
            js.jid_id(str(a["sender"]))
    for m in facts:
        js.chat_id(m["chat"])
        if m.get("sender"):
            js.jid_id(m["sender"])
        for _, row in m.get("side", []):
            for v in row.values():
                if isinstance(v, JidRef):
                    js.jid_id(str(v))


def resolve(js, row):
    return {k: (js.jid_id(str(v)) if isinstance(v, JidRef) else v) for k, v in row.items()}


def insert_jobs(con, js, jobs):
    for j in jobs:
        chat = j["chat"] if isinstance(j["chat"], int) else js.chat_id(j["chat"])
        row = {"_id": j["id"], "chat_row_id": chat, "deleted_message_row_id": j["msg"],
               "deleted_starred_message_row_id": j["starred"], "deleted_message_categories": j["cats"]}
        if "singular_message_delete_rows_id" in [c[1] for c in con.execute("pragma table_info(deleted_chat_job)")]:
            row["singular_message_delete_rows_id"] = j["singular"]
        C.insert(con, "deleted_chat_job", row)


def populate_modern(con, facts, js, vcards=True, addons=(), jobs=(), calls=()):
    for m in facts:
        row = modern_row(m, js)
        C.insert(con, "message", row)
    for j in js.jids:
        C.insert(con, "jid", j)
    for c in js.chats:
        C.insert(con, "chat", c)
    for m in facts:
        sides = m.get("side", [])
        for table, row in sides:
            row = resolve(js, row)
            if table != "jid_map":
                row = dict(row, message_row_id=m["id"])
            C.insert(con, table, row)
        if vcards and m["type"] == 4:
            C.insert(con, "message_vcard", {"message_row_id": m["id"], "vcard": m["vcard"]})
        if vcards and m["type"] == 5 and "lat" in m and not any(t == "message_location" for t, _ in sides):
            C.insert(con, "message_location", {"message_row_id": m["id"], "latitude": m["lat"], "longitude": m["lon"]})
    insert_jobs(con, js, jobs)
    insert_calls(con, js, calls)
    for a in addons:
        sender = js.jid_id(str(a["sender"])) if isinstance(a["sender"], JidRef) else a["sender"]
        C.insert(con, "message_add_on", {"_id": a["id"], "chat_row_id": js.chat_id(a["chat"]), "from_me": a["me"],
                                         "key_id": a["key"], "sender_jid_row_id": sender, "parent_message_row_id": a["parent"],
                                         "timestamp": a["ts"], "message_add_on_type": a["type"]})
        if a["reaction"] is not None:
            C.insert(con, "message_add_on_reaction", {"message_add_on_row_id": a["id"], "reaction": a["reaction"],
                                                      "sender_timestamp": a["rts"]})


def modern_fill(facts, with_vcards=True, addons=(), jobs=(), calls=()):
    def f(con):
        # the jid and chat rows first need their ids: allocate by walking the facts in id order
        js = Jids()
        alloc(js, facts, addons, calls)
        populate_modern(con, facts, js, with_vcards, addons, jobs, calls)
    return f


def finish(path, remote, variant, ddl, generation, extra=None, extras=()):
    files = {"db": C.read(path)}
    if C.os.path.exists(path + "-wal"):
        files["wal"] = C.read(path + "-wal")
    exp = oracle(files, remote, variant, ddl, generation, extras)
    if extras:
        exp["extra_files"] = [{"remote_path": p, "content_b64": base64.b64encode(b).decode(), "device": d or ""}
                              for p, b, d in extras]
    return files, exp


def v_legacy(work):
    rows = [sentinel_row()] + [legacy_row(m) for m in core(False)] + [legacy_row(m) for m in legacy_hostile()]
    path = build_db(work, "legacy-2.11.sql", fill_legacy(rows))
    return finish(path, REMOTE, "android-legacy-2.11", "legacy-2.11.sql", "legacy")


def transition_fill(legacy_facts, modern_facts, sentinel=True, jobs=(), calls=()):
    def f(con):
        if sentinel:
            C.insert(con, "messages", sentinel_row())
        for m in legacy_facts:
            C.insert(con, "messages", legacy_row(m))
        js = Jids()
        alloc_calls(js, calls)
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
            if m.get("quoted"):
                C.insert(con, "messages_quotes", LEGACY_QUOTE)
            for table, row in m.get("side", []):
                C.insert(con, table, dict(row, message_row_id=m["id"]))
        for m in modern_facts:
            if m["type"] == 4:
                C.insert(con, "message_vcard", {"message_row_id": m["id"], "vcard": m["vcard"]})
        insert_jobs(con, js, jobs)
        insert_calls(con, js, calls)
    return f


def v_trans_legacy(work):
    """The legacy rows, plus 10 legacy rows (ids 100 to 109) and a legacy sentinel (id 150) that a committed WAL deletes: the
    database file still holds them, so they are history under the WAL (the typed recovery scenario of plan 4I task 14). A
    freelist cannot attribute a legacy row: messages_quotes has the same columns, so the fit is ambiguous. The page that holds
    the rows is the table's own, so the schema says whose they are."""
    jobs = [dict(id=1, chat=GRP, msg=9, starred=None, cats=None, singular=None)]

    def fill(con):
        transition_fill(core(False) + legacy_extras(), [], jobs=jobs, calls=modern_calls([1, 2, 3, 4, 5, 6, 7]))(con)
        for m in bulk_messages(100, 10):
            C.insert(con, "messages", legacy_row(m))
        C.insert(con, "messages", dict(sentinel_row(), _id=150, key_id="K150"))
    path = build_db(work, "transition-2021.sql", fill, journal_wal=True)
    con = C.connect(path)
    scen = new_scenario("android-transition-legacydata", con)
    scen.write("messages", scen.snap(con, "messages"))
    scen.want("messages", scen.snap(con, "messages", "_id between 100 and 109 or _id = 150"))
    con.close()
    C.kill_child(path, ["pragma wal_autocheckpoint = 0", "delete from messages where _id between 100 and 109 or _id = 150"])
    files, exp = finish(path, REMOTE, "android-transition-legacydata", "transition-2021.sql", "transition")
    for rec in exp["records"]:
        rec["source_path"] = ""  # a WAL companion: the source path follows the role of the file that holds the cell
    return files, exp


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
    # the decoder states the frames it does not apply (the uncommitted tail): one reader warning, after the rows' own
    exp["warnings"].append({"table": "", "rowid": 0, "contains": "wal-frames-not-applied"})
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


def modern_variant(work, remote, variant, wal=False, extras=(), addons=(), jobs=(), calls=(), files=(), scenario=False):
    files_extra = files
    facts = core(True)
    if extras:
        facts[1]["srv"] = T0 + 2500  # row 3 carries a receipt_server_timestamp (modern: the unit is not established)
    early, late = (facts[:-4], facts[-4:]) if wal else (facts, [])
    early = early + list(extras)
    if scenario:
        # plan 4I task 14: messages 90 and 91 and call 90 are in the database file; the WAL deletes them, so the file still
        # holds three versions that are history (the typed recovery scenario) and the live rows are unchanged
        early = early + bulk_messages(90, 2)
        calls = list(calls) + bulk_calls(90, 1)
    path = build_db(work, "modern-2026.sql", modern_fill(early, addons=addons, jobs=jobs, calls=calls), journal_wal=wal)
    if scenario:
        con = C.connect(path)
        scen = new_scenario(variant, con)
        scen.write("message", scen.snap(con, "message"))
        scen.write("call_log", scen.snap(con, "call_log"))
        scen.want("message", scen.snap(con, "message", "_id in (90, 91)"))
        scen.want("call_log", scen.snap(con, "call_log", "_id = 90"))
        con.close()
    if wal:
        js = Jids()
        alloc(js, early, addons, calls)
        nj, nc = len(js.jids), len(js.chats)
        stmts = ["pragma wal_autocheckpoint = 0"]
        rows = [modern_row(m, js) for m in late]
        for j in js.jids[nj:]:
            stmts.append(insert_stmt("jid", j))
        for c in js.chats[nc:]:
            stmts.append(insert_stmt("chat", c))
        stmts += [insert_stmt("message", r) for r in rows]
        if scenario:
            stmts += ["delete from message where _id in (90, 91)", "delete from call_log where _id = 90"]
        C.kill_child(path, stmts)
    files, exp = finish(path, remote, variant, "modern-2026.sql", "modern", extras=files_extra)
    if wal:
        for rec in exp["records"]:
            rec["source_path"] = ""
    return files, exp


def v_modern_2026(work):
    return modern_variant(work, REMOTE_USER10, "android-modern-2026", wal=True, extras=modern_extras(), addons=modern_addons(),
                          jobs=modern_jobs(), calls=modern_calls(), files=EXTRA_FILES, scenario=True)


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



# ----------------------------------------------------------------------------------------- typed recovery (plan 4I task 14)
#
# The typed recovery pass maps the history rows of message, messages and call_log with the SAME mappings the live parse
# uses (4I-R4a: a nil job set, so a recovered row states no pending purge). The joins read the LIVE side tables. The
# oracle is computed from the committed files by an independent page walker (telephony_common.recover_oracle) and the
# scenario's own record of what it wrote.

CLAIMED = ("message", "messages", "call_log")


def bulk_messages(first, n, chat=C1):
    """n plain modern text facts with ids first.. (the rows a scenario writes and later deletes)."""
    out = []
    for i in range(first, first + n):
        out.append(dict(id=i, chat=chat, me=i % 2, text="bulk message number %d with some text to fill the page" % i,
                        ts=T0 + 500000 + i * 1000, recv=T0 + 500100 + i * 1000, status=0, type=0, key="B%03d" % i))
    return out


def bulk_calls(first, n):
    return [dict(id=i, jid=C1, me=i % 2, key="BC%03d" % i, ts=T0 + 900000 + i * 1000, video=0, dur=10 + i % 50, result=5,
                 bytes=None, group=0, parts=[]) for i in range(first, first + n)]


def legacy_create_statement():
    """The CREATE TABLE messages statement of the legacy DDL snapshot (one line)."""
    with open(C.os.path.join(SCHEMAS, "legacy-2.11.sql"), encoding="utf-8") as f:
        for line in f:
            if line.upper().startswith("CREATE TABLE MESSAGES "):
                return line.strip()
    raise RuntimeError("no CREATE TABLE messages in legacy-2.11.sql")


def new_scenario(variant, con):
    """The scenario of a variant: it learns the columns of every claimed table the store has (the oracle reads them all)."""
    scen = SCENARIOS[variant] = C.Scenario(CLAIMED)
    for t in CLAIMED:
        if con.execute("select count(*) from sqlite_master where type = 'table' and name = ?", (t,)).fetchone()[0]:
            scen.snap(con, t, "0")
    return scen


def recover_env(files, remote):
    """The live view of the files with no job set: a recovered row states no pending purge (4I-R4a)."""
    view = C.engine_view(files, TABLES)
    ctx = context(view)
    ctx["extras"], ctx["remote"] = [], remote
    ctx["jobs"] = job_set({}, ctx)
    return ctx


def recover_expect(variant, files):
    """The <variant>.recover.expect.json of a variant that has a scenario (ruling 4G-R14)."""
    remote = SCENARIO_REMOTE[variant]

    def one(table, d, env, file):
        if table == "messages" and d.get("key_remote_jid") == b"-1":
            return None  # the legacy sentinel is not a message: a counted unmapped row
        rec = expect_call(d, env, remote) if table == "call_log" else expect_message(table, d, env, remote)
        rec["source_path"], rec["file"] = "", file  # a recovered record: the source path follows the role of the file
        return rec

    return C.recover_oracle(variant, files, SCENARIOS[variant], lambda f: recover_env(f, remote), one, nobody="sentinel")


SCENARIO_REMOTE = {"android-modern-2026": REMOTE_USER10, "android-transition-legacydata": REMOTE, "android-modern-migrated": REMOTE}


def v_migrated(work):
    """A modern store of a user who migrated from the legacy app: modern-2026 DDL, on 1024-byte pages. A messages table was
    created, filled with 20 rows and dropped (secure_delete off), so its pages are on the freelist; 60 message rows and 30
    call_log rows are deleted too. Typed recovery claims message and call_log rows only: the dropped legacy table is no
    live table, so its pages stay with the generic run (R9)."""
    path = work + "/m.db"
    con = C.connect(path, page_size=1024)
    con.executescript(C.load_ddl("modern-2026.sql", SCHEMAS)[0])
    with C.batch(con):
        modern_fill(core(True) + bulk_messages(100, 60), calls=modern_calls([1, 2, 3, 4, 5, 6, 7]) + bulk_calls(100, 30))(con)
    scen = new_scenario("android-modern-migrated", con)
    scen.write("message", scen.snap(con, "message"))
    scen.write("call_log", scen.snap(con, "call_log"))
    con.execute(legacy_create_statement())
    with C.batch(con):
        for r in [sentinel_row()] + [legacy_row(m) for m in core(False)] + [legacy_row(m) for m in legacy_hostile()[:3]]:
            C.insert(con, "messages", r)
    scen.write("messages", scen.snap(con, "messages"))
    con.execute("drop table messages")
    scen.want("message", scen.snap(con, "message", "_id between 100 and 159"), label="message")
    scen.want("call_log", scen.snap(con, "call_log", "_id between 100 and 129"), label="call_log")
    con.execute("delete from message where _id between 100 and 159")
    con.execute("delete from call_log where _id between 100 and 129")
    con.close()
    return finish(path, REMOTE, "android-modern-migrated", "modern-2026.sql", "modern")


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
    "android-modern-migrated": v_migrated,
}

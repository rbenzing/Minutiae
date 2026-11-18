"""Shared helpers of the iOS WhatsApp fixture generators (plan 4I2, tasks 2, 7 and 9). Helpers only: no FAMILY.

The families that build on it are whatsapp_ios_chats.py (ChatStorage.sqlite), whatsapp_ios_calls.py
(CallHistory.sqlite) and whatsapp_ios_contacts.py (ContactsV2.sqlite). Each is run through telephony_gen.py.

Honesty about the oracles (4I2-R13): every oracle built on these helpers is a SECOND READING OF THE SPEC by the same
author as the Go design, written without the Go code. It is not independent of the design: a wrong reading shared by
both would pass. What it does catch is a divergence between the spec's text and the implementation, and the literal
facts the Go tests pin beside it (oracle_facts_test.go). The DDL snapshots are constructed from memory (schemas/
whatsapp-ios/CROSSCHECK.md), so a fixture proves what the parser does with the column set the sources report, not
what an installed app writes.

What is here: Cocoa-second conversion, the Core Data Z_PRIMARYKEY rows, the container metadata plist, the DDL loader
over schemas/whatsapp-ios/, the oracle's cell readers in the style of whatsapp_gen.py (cleaned, stored, read_code,
read_instant, here over Cocoa seconds), the helpers that leave deleted rows and an old WAL frame behind (the
`-deleted` variants, plan 4I2 PF2), and the re-exports jid and vcard of whatsapp_gen.

Python 3 with its bundled sqlite3 only; no Docker.
"""
import plistlib
import re

import ios_common as I
import telephony_common as C
import whatsapp_gen as W

SCHEMAS = C.os.path.join(C.HERE, "schemas", "whatsapp-ios")
MAX_TEXT = W.MAX_TEXT
COCOA_TO_UNIX = I.COCOA_TO_UNIX

# the container metadata a full-file-system extraction keeps beside the app group (4I2-R5, from memory)
META_KEY = "MCMMetadataIdentifier"
GROUP_WA = "group.net.whatsapp.WhatsApp.shared"
GROUP_SMB = "group.net.whatsapp.WhatsAppSMB.shared"

jid = W.jid
vcard = W.vcard
is_int = W.is_int
is_text = W.is_text
text_of = W.text_of
cleaned = W.cleaned
stored = W.stored
blob_marker = W.blob_marker
Cells = W.Cells


def cocoa(unix_s):
    """Unix seconds (an int, or a float for a fraction) to the Cocoa seconds the stores keep (since 2001-01-01)."""
    return unix_s - COCOA_TO_UNIX


def unix_of(cocoa_s):
    """The inverse of cocoa()."""
    return cocoa_s + COCOA_TO_UNIX


def primary_key_rows(entities):
    """The Z_PRIMARYKEY rows of a Core Data store from (name, ent, super) triples. Z_MAX starts at 0: a generator calls
    set_z_max(con) last, once its rows are in, so that Z_MAX is the real maximum and never a caller's guess."""
    return [("Z_PRIMARYKEY", {"Z_ENT": ent, "Z_NAME": name, "Z_SUPER": sup, "Z_MAX": 0})
            for name, ent, sup in entities]


def set_z_max(con):
    """Sets Z_PRIMARYKEY.Z_MAX of every entity to the highest Z_PK of its table (the Core Data convention: the entity
    `WAMessage` is the table `ZWAMESSAGE`), 0 when the table is empty or the store has none. Call it last."""
    for ent, name in con.execute("select Z_ENT, Z_NAME from Z_PRIMARYKEY").fetchall():
        table = "Z" + name.upper()
        assert re.fullmatch(r"\w+", table), table
        exists = con.execute("select count(*) from sqlite_master where type = 'table' and name = ?", (table,)).fetchone()[0]
        top = con.execute("select max(Z_PK) from %s" % table).fetchone()[0] if exists else None
        con.execute("update Z_PRIMARYKEY set Z_MAX = ? where Z_ENT = ?", (top or 0, ent))


def metadata_row(uuid="00000000-0000-4000-8000-000000000002"):
    """The Z_METADATA row (the store UUID and an empty store plist)."""
    return ("Z_METADATA", {"Z_VERSION": 1, "Z_UUID": uuid, "Z_PLIST": I.plist_binary({"NSStoreType": "SQLite"})})


def container_meta_plist(identifier):
    """An XML plist whose MCMMetadataIdentifier is identifier: the container metadata an extraction keeps."""
    return plistlib.dumps({META_KEY: identifier}, fmt=plistlib.FMT_XML, sort_keys=True)


def load_ios_ddl(name):
    """The phases of schemas/whatsapp-ios/<name> (chatstorage.sql, callhistory.sql or contactsv2.sql)."""
    return C.load_ddl(name, SCHEMAS)


def is_number(v):
    return (isinstance(v, (int, float))) and not isinstance(v, bool)


def read_code(c, name, text_typed=False):
    """An enum cell: its raw value as stored, the integer when it is usable (whatsapp_gen.read_code)."""
    return W.read_code(c, name, text_typed)


def read_instant(c, name):
    """A Cocoa-seconds message time: the RFC 3339 instant when valid, else None. The stored value is in raw; its
    status (sentinel or invalid, never a rescale by magnitude, research 7.12) is in raw[name + '_status'] when it is
    not valid. A cell that is neither an INTEGER nor a REAL is stated in unknown_columns as type-mismatch."""
    v = c.row.get(name)
    if v is None:
        return None
    if not is_number(v):
        c.unknown[c.prefix + name] = "type-mismatch"
        return None
    c.raw[name] = v
    st = I.cocoa_status(v, "s")
    if st != "valid":
        c.raw[name + "_status"] = st
        return None
    return I.cocoa_iso(v, "s")


def delete_rows(con, table, pks):
    """Deletes the rows with the given Z_PK in one transaction. secure_delete is OFF (C.connect), so the cell bytes
    stay in the free space: the rows a recovery reads back (the -deleted variants, PF2)."""
    assert re.fullmatch(r"\w+", table), table
    with C.batch(con):
        for pk in pks:
            con.execute("delete from %s where Z_PK = ?" % table, (pk,))


def leave_old_frame(path, statements):
    """Runs the statements in a child that dies without closing, leaving a WAL (the engine's last committed state is
    in the WAL, the pages it replaced stay in earlier frames). Used to build the old-frame variants (PF2)."""
    C.kill_child(path, statements)


def insert_stmt(table, row):
    """An INSERT statement with literal values, for a killed child's statement list (leave_old_frame)."""
    return "insert into %s(%s) values (%s)" % (table, ",".join(row), ",".join(C.lit(v) for v in row.values()))


def uncommitted_frames(wal, page_size):
    """Frames after the last commit frame of a WAL (frames whose commit field is zero at the tail)."""
    n = (len(wal) - 32) // (24 + page_size)
    last = 0
    for i in range(n):
        off = 32 + i * (24 + page_size)
        if int.from_bytes(wal[off + 4:off + 8], "big"):
            last = i + 1
    return n - last

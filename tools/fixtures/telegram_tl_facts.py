"""Telegram Android TL constructor facts: emit and cross-check (plan 4I4, Task 1, writer's call 4I4-P3 b).

    py -3 -I tools/fixtures/telegram_tl_facts.py --emit <srcdir> [--pin <name>] [--absent-from <constructors.tsv>]
    py -3 -I tools/fixtures/telegram_tl_facts.py --check <constructors.tsv> --pin <name> <srcdir>
        [--commit SHA] [--write-report FILE]
    py -3 -I tools/fixtures/telegram_tl_facts.py --assemble <schemasdir> <report.md>... [--ddl <ddl-report.md>...]

<srcdir> holds the app source fetched at one pinned commit, under the repository's paths below
TMessagesProj/src/main/java/org/telegram/ (tgnet/TLRPC.java, tgnet/tl/*.java, tgnet/tl/legacy/*.java and
ui/Stories/*.java are the files it reads classes from; a SHA-256 is reported for every file of the directory).
The files are GPLv2 and untrusted text: this script reads them as text and applies regular expressions to them; nothing in them is executed, imported, or used as a path or a command, and it
never starts a process. Its output holds facts only (constructor IDs, class names, field order, flag bits,
file:line), never source text.

The script finds the base class of each TL type the parser reads (Message, Peer, MessageMedia, ...), reads
the case labels of its fromConstructor/TLdeserialize switch (an ID is the app's "this constructor is read
by this class"), and reduces each class's readParams to the 4I4-P4 notation:

    flags:#   name=F.n (flag-only true, no bytes)   name:i32|i64|dbl|bool|bytes|str|secret
    name:id32@<class> / name:id64@<class> (an id read into a peer the app builds, ruling T5)
    name:Type   name:vec<Type>   [F.n: field ...] (present iff bit n of flags; F2.n: of flags2)

Only what is READ is listed: a default the app synthesizes (`x = new T()` without a read) is not; a value
the app reads and drops is `_discarded_<gate>` (the source keeps no name for it). Bytes named key, iv,
file_reference or encrypted_key are `secret` (spec §9: kept as length only). A statement that touches the
stream and is none of the known forms makes the class MANUAL at that file:line: it is never guessed.

--emit prints constructors.tsv rows for the pin (A-HEAD unless --pin; header first) and lists the MANUAL
rows on stderr; the rows' body follows the selection of plan 4I4 Task 1 (SELECTION below). With
--absent-from it prints only the IDs that table lacks (the rows of IDs an older pin reads and A-HEAD no
longer does). --check compares a constructors.tsv with the sources of one pin: every row pinned there is
`verified`, `MISMATCH` or `MANUAL`; at A-HEAD every ID each type's switch reads must be a row (else
`missing`; MessageAction holds the phone-call action only); at an older pin it reports membership, every
ID of the pinned types' switches `in-table` or `missing`. Exit status 1 on any MISMATCH or missing. Each
report names the SHA-256 of the constructors.tsv and facts.tsv it checked. A MANUAL row is reported as
"MANUAL, read by hand at <file>:<line>" with its reason: the builder reads it there. --assemble writes
<schemasdir>/CROSSCHECK.md from the fixed text below, the reports in the order given, and facts.tsv.
Plan 4I4 Task 4: the reports after --ddl are the runs of ddl_crosscheck.py --family telegram (one per pin); they
are appended last, under the heading "# DDL cross-check (plan 4I4)", so --assemble reproduces the whole file.
"""
import hashlib
import os
import re
import sys

PINS = ("A-HEAD", "A-11.4.2", "release-8.1.1_2431", "release-5.14.0_1851")
HEADER = "type\tid\tclass\tbody\tpin\tsource\tfields"

# The TL types of the table, in output order, and how each row's body is chosen (plan 4I4 Task 1).
#   all-full:    every constructor of the switch, full body.
#   only:        only the listed IDs (full); the type's other constructors are not in the table.
#   some-full:   the listed classes full, every other constructor of the switch opaque.
#   prefix:      every constructor up to its last needed field (spec Q2, T6), full when that is the body.
SELECTION = [
    ("Message", "all-full", None),
    ("Peer", "all-full", None),
    ("MessageFwdHeader", "all-full", None),
    ("MessageReplyHeader", "all-full", None),
    ("MessageEntity", "all-full", None),
    ("MessageAction", "only", {0x80E11A7F}),
    ("PhoneCallDiscardReason", "all-full", None),
    ("MessageMedia", "some-full", {
        "TL_messageMediaPhoto", "TL_messageMediaDocument", "TL_messageMediaGeo", "TL_messageMediaGeoLive",
        "TL_messageMediaContact", "TL_messageMediaVenue", "TL_messageMediaWebPage", "TL_messageMediaPoll"}),
    ("Photo", "some-full", {"TL_photo", "TL_photoEmpty"}),
    ("Document", "some-full", {"TL_document", "TL_documentEncrypted"}),
    ("GeoPoint", "all-full", None),
    ("WebPage", "some-full", {"TL_webPage"}),
    ("User", "prefix", {"id", "first_name", "last_name", "username", "phone"}),
    ("Chat", "prefix", {"id", "title", "username"}),
]
# At an older pin, membership covers every switch ID of each type, except MessageAction, of which the table
# holds the phone-call action only: there only the classes of that family are checked.
MEMBERSHIP_FAMILY = {"MessageAction": "TL_messageActionPhoneCall"}
SECRET_LEAVES = {"key", "iv", "file_reference", "encrypted_key"}
PEER_CLASS = {"TL_peerUser": "user", "TL_peerChat": "chat", "TL_peerChannel": "channel"}
TARGET_CLASS = {"user_id": "user", "chat_id": "chat", "channel_id": "channel"}

READ_KIND = {
    "readInt32": "i32", "readInt64": "i64", "readDouble": "dbl", "readBool": "bool",
    "readString": "str", "readByteArray": "bytes", "readByteBuffer": "bytes",
}
VEC_KIND = {"deserializeInt": "vec<i32>", "deserializeLong": "vec<i64>", "deserializeString": "vec<str>",
            "deserializeByteArray": "vec<bytes>"}

NAME = r"[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*"
QTYPE = r"(?:TLRPC\.)?([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)"
# `flags = stream.readInt32(exception) | MESSAGE_FLAG_HAS_FROM_ID;`: the app ORs constants into the value it
# keeps; the wire holds the int32 read.
RE_READ = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*(?:\(\w+\)\s*)?stream\.(\w+)\(exception\)"
                     r"(?:\s*\|\s*\w+)*;$")
RE_BARE = re.compile(r"^stream\.(\w+)\(exception\);$")
RE_ADDOBJ = re.compile(r"^(?:this\.)?(" + NAME + r")\.add\(" + QTYPE +
                       r"\.TLdeserialize\(stream,\s*stream\.readInt32\(exception\),\s*exception\)\);$")
RE_DESER1 = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*" + QTYPE + r"\.TLdeserialize\(stream,\s*exception\);$")
RE_VECL = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*Vector\.deserialize\(stream,\s*\(\w+,\s*\w+,\s*\w+\)\s*->\s*" +
                     QTYPE + r"\.TLdeserialize\([^;]*\),\s*exception\);$")
RE_TRY = re.compile(r"^try\s*\{$")
RE_CATCH = re.compile(r"^\}\s*catch\s*\(.*\)\s*\{$")
RE_FLAG = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*(.+);$")
RE_DESER = re.compile(r"^(?:(\w+(?:\.\w+)*)\s+)?(?:this\.)?(" + NAME + r")\s*=\s*" + QTYPE +
                      r"\.TLdeserialize\(stream,\s*stream\.readInt32\(exception\),\s*exception(?:,\s*\w+)*\);$")
RE_VEC = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*Vector\.deserialize\(stream,\s*" + QTYPE +
                    r"::TLdeserialize,\s*exception\);$")
RE_VECP = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*Vector\.(deserialize\w+)\(stream,\s*exception\);$")
RE_NEW = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*new\s+" + QTYPE + r"\(\);$")
RE_ALIAS = re.compile(r"^(?:this\.)?(" + NAME + r")\s*=\s*(\w+);$")
RE_IF = re.compile(r"^(?:\}\s*else\s+)?if\s*\((.*)\)\s*\{$")
RE_ELSE = re.compile(r"^\}\s*else\s*\{$")
RE_FOR = re.compile(r"^for\s*\(.*\)\s*\{$")
RE_DELEGATE = re.compile(r"^readParams\(stream,\s*exception,\s*\w+\);$")
RE_MAGIC = re.compile(r"^int\s+magic\s*=\s*stream\.readInt32\(exception\);$")
RE_COUNT = re.compile(r"^int\s+count\s*=\s*stream\.readInt32\(exception\);$")
RE_ELEM = re.compile(r"^(\w+(?:\.\w+)*)\s+(\w+)\s*=\s*" + QTYPE +
                     r"\.TLdeserialize\(stream,\s*stream\.readInt32\(exception\),\s*exception\);$")
RE_ADD = re.compile(r"^(?:this\.)?(" + NAME + r")\.add\((.+)\);$")
RE_ADDREAD = re.compile(r"^stream\.(\w+)\(exception\)$")


# The fixed text of CROSSCHECK.md around the script's reports (--assemble): the pins, the files fetched beyond the
# plan's list, the findings against the recon and the spec, and the docs cross-check (each page fetched once,
# 2026-11-20).
PREAMBLE = """# TL facts cross-check (plan 4I4)

The Telegram Android constructor table `constructors.tsv` and the other pinned facts `facts.tsv` of this directory,
checked by `tools/fixtures/telegram_tl_facts.py` against the app source of the repository
TelegramMessenger/Telegram-Android (GPLv2) at four pinned commits, one `--check` run per pin. The fetched files were held
in scratch only and are not in the repository; each is pinned below by commit and SHA-256 (paths below
`TMessagesProj/src/main/java/org/telegram/`). Fetched 2026-11-20 by plain HTTP, one file at a time, with a generic user
agent. Sources are untrusted text: the script reads them with regular expressions and never runs them. Facts only (IDs,
class names, field order, flag bits, `file:line`): no code is copied.

## Pins

The three tags were peeled with `git ls-remote <repository> refs/tags/<tag> refs/tags/<tag>^{}` on 2026-11-20 (each
is a lightweight tag on the commit below); the head commit is the recon's.

| Pin | Commit | Name at the repository |
|---|---|---|
| A-HEAD | 21f7d109da6ab3506c9e9577f98c6a38771ab335 | master tip of 2026-11-12, TL layer 233, `LAST_DB_VERSION` 181 |
| A-11.4.2 | fb2e545101f41303f1e2712de2e7611a9335f1c3 | tag `release-11.4.2-5469`, `LAST_DB_VERSION` 159 |
| release-8.1.1_2431 | 58aded63e9713ef796d76f3e29a60a5254f803b4 | tag `release-8.1.1_2431`, `LAST_DB_VERSION` 84 |
| release-5.14.0_1851 | 7d6fa267c13bacd48f8d8f10d31a61f38b77bfe4 | tag `release-5.14.0_1851`, `LAST_DB_VERSION` 64 |

Files fetched beyond the plan's list, as evidence for `facts.tsv`: `messenger/MessagesController.java` at every pin (the
caller that stores an edited message) and `messenger/LocaleController.java` at A-HEAD (the call duration's format), and
at A-HEAD and A-11.4.2 `ui/Stories/MessageMediaStoryFull.java` and `MessageMediaStoryFull_old.java` (two classes the
MessageMedia switch reads). Every other `tgnet/tl/*.java` of A-HEAD (18 files) and A-11.4.2 (7 files) was fetched and
searched: none defines a class of the pinned types (no switch case resolves to one, and the checks below pass without
them), so they are not pinned. `tgnet/Vector.java` and `tgnet/tl/legacy/TL_legacy_message.java` exist at A-HEAD only,
`messenger/DatabaseMigrationHelper.java` from A-11.4.2 on.

## How the table was built, and what differs from the recon and the spec

`telegram_tl_facts.py --emit <A-HEAD sources>` wrote the A-HEAD rows; `--emit <sources> --pin <older pin> --absent-from
constructors.tsv` wrote the rows of IDs that only an older pin reads. Three rows were then completed by hand (below). Every
row is then checked at its own pin: the ID is read by its type's switch, the class and the source line are the source's,
the body is the one the selection of plan 4I4 Task 1 asks for, and the fields equal the reduced `readParams` (a `prefix`
row: a proper prefix of it, up to the last needed field, spec Q2/T6). At A-HEAD every ID each type's switch reads is a
row; at the three older pins every ID the pinned types' switches read is a row (membership).

- MessageMedia holds 52 rows, not the 50 of spec §6.6 (recon §3.6 counted classes in `TLRPC.java`): the A-HEAD switch
  also reads two app-local IDs, `0xc79aee1d` `MessageMediaStoryFull` and `0xc79aee1f` `MessageMediaStoryFull_old`
  (`ui/Stories/`, also read at A-11.4.2). They are `opaque` rows.
- Five IDs are read at an older pin and no longer at A-HEAD; each is a row pinned at the newest pin that reads it, so a
  blob an older app wrote stays decodable: Message `0x1c9b1027` `TL_message_layer104_2` and `0x9789dac4`
  `TL_message_layer104_3` (read at release-5.14.0_1851, release-8.1.1_2431 and A-11.4.2; pinned at A-11.4.2), Chat
  `0xa636a3e2` `TL_channel_layer167_2` and `0x0db12acb` `TL_channel_layer167_3` (A-11.4.2), PhoneCallDiscardReason
  `0xafe2b839` `TL_phoneCallDiscardReasonAllowGroupCall` (release-5.14.0_1851; its one field `encrypted_key` is key
  material and is `secret`, spec §9). Message is so 49 rows (46 A-HEAD classes, the alias `0xa4e97f37`, two), Chat 39,
  PhoneCallDiscardReason 6.
- MessageReplyHeader: 9 rows (A-HEAD). At release-8.1.1_2431 the type is one class, `TL_messageReplyHeader`
  `0xa6d57763`, with no switch (it checks its own constructor): that ID is the A-HEAD `TL_messageReplyHeader_layer165`
  row. At release-5.14.0_1851 there is no reply header object (the Message bodies read `reply_to_msg_id` inline).
  MessageEntity: 28 rows.
- A value the app reads and drops is named `_discarded_<gate>` (for example `[F.22: _discarded_f22:str]` in
  `TL_message_layer104_2`): the source keeps no name for it, and none is guessed.
- A read the app ORs constants into (`TL_message_old6`, `TL_message_secret_old`: `flags` with `MESSAGE_FLAG_HAS_FROM_ID`)
  is listed as the wire value read (`flags:#`); the app's in-memory flags differ from the stored ones (recon §2 item 6).
- Read by hand (T5): `TL_messageFwdHeader_layer68` `0xc786ddcb` flag bit 0 builds a `TL_peerChannel` but fills its
  `user_id` (`TLRPC.java:43834-43835`). The layer's own field is the forwarded user: release-5.14.0_1851 reads that value
  into `MessageFwdHeader.from_id` (an int beside a separate `channel_id`, `TLRPC.java:29805-29808` there) and looks it
  up as a user (`MessageObject.java:4845` there). Resolved `user` (`facts.tsv` `wrapper.TL_messageFwdHeader_layer68.from_id`).
- Read by hand (flag-only trues the reduction cannot see): `TL_messageEntityFormattedDate` `0x904ac7c7` computes its
  six flag-only trues (`relative`, `short_time`, `long_time`, `short_date`, `long_date`, `day_of_week`: bits 0 to 5) in a
  method of its own (`TLRPC.java:63806`) called after its last read (`TLRPC.java:63820`); they are listed after `date`,
  where they are computed. `TL_community` `0x65efe954` sets `collapsed_in_dialogs` from bit 20 of `flags2` combined with
  an app switch (`TLRPC.java:40714`); the stored bit is listed as `collapsed_in_dialogs=F2.20`. The script reports a
  flag-only true set by any expression other than one flag test, and any call of a method it does not follow, as MANUAL.
- The old forward headers read two inline ids into the same field `from_id`, one per flag bit (bit 0 a user, bit 1 a
  channel); `facts.tsv` holds one `wrapper.<class>.from_id` row per inline id, each citing the line of its own read.
- `DatabaseMigrationHelper.java` lines: the recon's 815-823 (the pre-84 secret-chat uid) and 818 (the 32-bit mid) are
  819-825 and 819 at A-HEAD (`facts.tsv`).
- This file is written by `telegram_tl_facts.py --assemble` from the four `--check` reports, the docs cross-check and
  `facts.tsv`; each report names the SHA-256 of the `constructors.tsv` and `facts.tsv` it checked, and
  `TestTelegramFactsCiteTheirSource` recomputes both, so an edit of either file without a new run fails the default
  tests.

"""

DOCS = """# Docs cross-check

The public constructor pages (fetched 2026-11-20; their header said layer 225) were compared with the app source for the
constructors that have one. Every other row is app source only (recon §3.1: the pages lag the app by eight layers; the
layer 226-233 IDs, the private secret-chat IDs `0x555555f8`/`f9`/`fa`, `0x55555558` and the app-local IDs have no page).

| Constructor | ID | App source (A-HEAD) | Verdict |
|---|---|---|---|
| messageActionPhoneCall | 0x80e11a7f | `TLRPC.java:26192` (flags, video bit 2, call_id long, reason bit 0, duration int bit 1) | confirmed on the public constructor page, fetched 2026-11-20 |
| phoneCallDiscardReasonMissed | 0x85e42301 | `TLRPC.java:20950` (empty) | confirmed on the public constructor page, fetched 2026-11-20 |
| phoneCallDiscardReasonBusy | 0xfaf7e8c9 | `TLRPC.java:20942` (empty) | confirmed on the public constructor page, fetched 2026-11-20 |
| phoneCallDiscardReasonHangup | 0x57adc690 | `TLRPC.java:20934` (empty) | confirmed on the public constructor page, fetched 2026-11-20 |
| phoneCallDiscardReasonDisconnect | 0xe095c1a0 | `TLRPC.java:20958` (empty) | confirmed on the public constructor page, fetched 2026-11-20 |
| phoneCallDiscardReasonMigrateConferenceCall | 0x9fbbf1f7 | `TLRPC.java:20966` (slug string) | confirmed on the public constructor page, fetched 2026-11-20 |
| boolTrue | 0x997275b5 | `NativeByteBuffer.java:158,449`, `SerializedData.java:158,380` | confirmed on the public constructor page, fetched 2026-11-20 |
| boolFalse | 0xbc799737 | `NativeByteBuffer.java:160,451`, `SerializedData.java:160,382` | confirmed on the public constructor page, fetched 2026-11-20 |
| vector | 0x1cb5c415 | `Vector.java:8` | confirmed on the public serialization page, fetched 2026-11-20 |
| every other row of `constructors.tsv` | - | the rows above | app source only (recon §3.1) |

"""


def flag_ref(cond):
    """The F.n / F2.n of a one-bit flag test, or None."""
    m = re.fullmatch(r"hasFlag\((flags2?),\s*FLAG_(\d+)\)", cond.strip())
    if m:
        return ("F2." if m.group(1) == "flags2" else "F.") + m.group(2)
    m = re.fullmatch(r"\(\s*(flags2?)\s*&\s*(\d+|0x[0-9a-fA-F]+)\s*\)\s*!=\s*0", cond.strip())
    if m:
        v = int(m.group(2), 0)
        if v > 0 and v & (v - 1) == 0:
            return ("F2." if m.group(1) == "flags2" else "F.") + str(v.bit_length() - 1)
    return None


def strip_java(text):
    """Comments removed and string/char literals masked, line structure kept."""
    out, i, n = [], 0, len(text)
    while i < n:
        c = text[i]
        if text.startswith("//", i):
            j = text.find("\n", i)
            i = n if j < 0 else j
        elif text.startswith("/*", i):
            j = text.find("*/", i + 2)
            j = n if j < 0 else j + 2
            out.append("\n" * text.count("\n", i, j))
            i = j
        elif c in "\"'":
            j = i + 1
            while j < n and text[j] != c and text[j] != "\n":
                j += 2 if text[j] == "\\" else 1
            out.append(c + " " * max(0, min(j, n) - i - 1) + c)
            i = j + 1
        else:
            out.append(c)
            i += 1
    return "".join(out)


class JavaClass:
    def __init__(self, fname, line, name, extends, outer):
        self.fname, self.line, self.name, self.extends, self.outer = fname, line, name, extends, outer
        self.end = None
        self.constructor = None
        self.methods = []  # (signature line, first body line index, last body line index)


def index_file(fname, lines):
    """Every class of one file with its constructor constant and its readParams bodies."""
    classes, stack, depth = [], [], 0
    class_re = re.compile(r"\bclass\s+(\w+)(?:\s*<[^>]*>)?(?:\s+extends\s+([\w.]+))?[^{;]*\{")
    cons_re = re.compile(r"\bstatic\s+(?:final\s+)?int\s+constructor\s*=\s*(0x[0-9a-fA-F]+)\s*;")
    meth_re = re.compile(r"\bvoid\s+readParams\(\s*\w+\s+stream\s*,\s*boolean\s+exception\s*(,[^)]*)?\)\s*\{")
    open_methods = []
    for i, line in enumerate(lines):
        m = class_re.search(line)
        if m:
            outer = stack[-1][0] if stack else None
            c = JavaClass(fname, i + 1, m.group(1), m.group(2), outer)
            classes.append(c)
            stack.append((c, depth))
        if stack:
            cur = stack[-1][0]
            cm = cons_re.search(line)
            if cm and cur.constructor is None and depth == stack[-1][1] + 1:
                cur.constructor = int(cm.group(1), 16)
            mm = meth_re.search(line)
            if mm and depth == stack[-1][1] + 1:
                open_methods.append((cur, i, depth, mm.group(1) is not None))
        depth += line.count("{") - line.count("}")
        while open_methods and depth <= open_methods[-1][2]:
            cur, start, _, extra = open_methods.pop()
            cur.methods.append((start, i, extra))
        while stack and depth <= stack[-1][1]:
            stack[-1][0].end = i + 1
            stack.pop()
    return classes


class Source:
    def __init__(self, srcdir):
        self.srcdir = srcdir
        self.files = {}   # base name -> stripped lines
        self.classes = []
        paths = [os.path.join(srcdir, "tgnet", "TLRPC.java")]
        # tgnet/tl and tgnet/tl/legacy hold newer TL classes; ui/Stories holds the two app-local
        # MessageMedia classes (0xc79aee1d, 0xc79aee1f) the MessageMedia switch reads from 11.x on.
        for sub in (("tgnet", "tl"), ("tgnet", "tl", "legacy"), ("ui", "Stories")):
            d = os.path.join(srcdir, *sub)
            if os.path.isdir(d):
                paths += [os.path.join(d, f) for f in sorted(os.listdir(d)) if f.endswith(".java")]
        for p in paths:
            with open(p, encoding="utf-8", errors="replace") as fh:
                lines = strip_java(fh.read()).split("\n")
            base = os.path.basename(p)
            self.files[base] = lines
            self.classes += index_file(base, lines)
        self.by_name = {}
        for c in self.classes:
            self.by_name.setdefault(c.name, []).append(c)

    def resolve(self, ref):
        """A class named by a switch: `Name` (TLRPC first) or `Outer.Name`."""
        parts = ref.split(".")
        cands = self.by_name.get(parts[-1], [])
        if len(parts) > 1:
            cands = [c for c in cands if c.outer is not None and c.outer.name == parts[-2]]
        else:
            tl = [c for c in cands if c.fname == "TLRPC.java"]
            cands = tl or cands
        return cands[0] if len(cands) == 1 else None

    def base(self, typ):
        cands = [c for c in self.by_name.get(typ, []) if c.fname == "TLRPC.java" and c.extends == "TLObject"]
        return cands[0] if len(cands) == 1 else None

    def switch(self, typ):
        """[(id, case line, class)] of the type's constructor switch, or None when the type is not defined."""
        b = self.base(typ)
        if b is None:
            # a type with one constructor and no base class (MessageReplyHeader at release-8.1.1_2431: the
            # class TL_messageReplyHeader extends TLObject and checks its own constructor)
            one = [c for c in self.by_name.get("TL_" + typ[0].lower() + typ[1:], [])
                   if c.fname == "TLRPC.java" and c.extends == "TLObject" and c.constructor is not None]
            return [(one[0].constructor, one[0].line, one[0])] if len(one) == 1 else None
        lines = self.files[b.fname]
        start = None
        for i in range(b.line - 1, b.end):
            if re.search(r"\bswitch\s*\(\s*constructor\s*\)\s*\{", lines[i]):
                start = i
                break
        if start is None:
            return None
        out, labels, depth = [], [], 0
        for i in range(start, b.end):
            line = lines[i].strip()
            depth += lines[i].count("{") - lines[i].count("}")
            for m in re.finditer(r"\bcase\s+([^:]+):", line):
                lab = m.group(1).strip()
                if re.fullmatch(r"0x[0-9a-fA-F]+", lab):
                    labels.append((int(lab, 16), i + 1))
                else:
                    cm = re.fullmatch(r"([\w.]+)\.constructor", lab)
                    c = self.resolve(cm.group(1)) if cm else None
                    if c is None or c.constructor is None:
                        raise SystemExit("unresolved case label at %s:%d" % (b.fname, i + 1))
                    labels.append((c.constructor, i + 1))
            m = re.search(r"(?:return|result\s*=)\s*new\s+([\w.]+)\s*\(", line)
            if m and labels:
                c = self.resolve(m.group(1))
                if c is None:
                    raise SystemExit("unresolved class %s at %s:%d" % (m.group(1), b.fname, i + 1))
                out += [(cid, ln, c) for cid, ln in labels]
                labels = []
            if depth <= 0 and i > start:
                break
        return out


class Manual(Exception):
    def __init__(self, where, why):
        super().__init__(where, why)
        self.where, self.why = where, why


def statements(lines, first, last):
    """(line number, statement) of a method body, joining continuation lines."""
    buf, at = "", None
    for i in range(first + 1, last):
        s = lines[i].strip()
        if not s:
            continue
        if not buf:
            at = i + 1
        buf = (buf + " " + s).strip()
        if buf.endswith((";", "{", "}")):
            # a closing brace followed by a statement on one line is split
            yield at, buf
            buf = ""
    if buf:
        yield at, buf


def body_of(src, c):
    """The readParams body of class c: (file, first, last), following a delegation to its overload."""
    lines = src.files[c.fname]
    plain = [m for m in c.methods if not m[2]]
    if not plain:
        # no readParams: the body is empty when the class extends its TL type's base directly (TLObject's
        # readParams reads nothing), else it is the parent's
        parent = src.resolve(c.extends or "")
        if parent is None:
            raise Manual("%s:%d" % (c.fname, c.line), "no readParams and no parent found")
        if parent.extends == "TLObject":
            return None
        return body_of(src, parent)
    first, last, _ = plain[0]
    stmts = list(statements(lines, first, last))
    if len(stmts) == 1 and RE_DELEGATE.match(stmts[0][1]):
        ext = [m for m in c.methods if m[2]]
        if len(ext) != 1:
            raise Manual("%s:%d" % (c.fname, stmts[0][0]), "delegates to an overload not found")
        first, last, _ = ext[0]
    return c.fname, first, last


def kind_of(name, k):
    leaf = name.rsplit(".", 1)[-1]
    if k == "i32" and re.fullmatch(r"flags\d*", name):
        return "#"
    if k == "bytes" and leaf in SECRET_LEAVES:
        return "secret"
    return k


def reduce_class(src, c):
    """The class's readParams as a list of items (str), or raise Manual."""
    where_body = body_of(src, c)
    if where_body is None:
        return []
    fname, first, last = where_body
    lines = src.files[fname]
    items = []          # finished items
    blocks = []         # stack of (kind, ref, fields, line): kind gate|other|loop|else
    wrappers = {}       # name -> class of a `name = new T()`
    locals_ = {}        # local name -> (container, index) of a field read into a local
    vec = None          # state of a hand-written vector loop

    def where(ln):
        return "%s:%d" % (fname, ln)

    def emit(field, ln):
        for k, _, flds, _ in reversed(blocks):
            if k == "try":
                continue  # the reads in a try block are on the wire as written; only a failure is swallowed
            if k == "gate":
                flds.append(field)
                return flds, len(flds) - 1
            if k in ("other", "else", "catch"):
                raise Manual(where(ln), "a read under a condition that is not one flag bit")
            if k == "loop":
                raise Manual(where(ln), "a read inside a loop that is not a vector")
        items.append(field)
        return items, len(items) - 1

    for ln, s in statements(lines, first, last):
        if s == "}" or RE_ELSE.match(s) or RE_CATCH.match(s) or (s.startswith("}") and RE_IF.match(s)):
            # a closing brace ends a block; `} else {`, `} else if (...) {` and `} catch (...) {` end one and
            # open the next, whose reads are not on a flag bit of their own (MANUAL if any)
            if not blocks:
                raise Manual(where(ln), "unbalanced braces")
            k, ref, flds, _ = blocks.pop()
            if k == "gate" and flds:
                if any(b[0] != "try" for b in blocks):
                    raise Manual(where(ln), "a gate inside another block")
                items.append("[%s: %s]" % (ref, " ".join(flds)))
            if k == "loop" and vec is not None and vec.get("phase") == "loop":
                if not vec.get("name") or not vec.get("elem"):
                    raise Manual(where(vec["line"]), "a vector loop of an unknown shape")
                emit("%s:%s" % (vec["name"], vec["elem"]), vec["line"])
                vec = None
            if RE_CATCH.match(s):
                if k != "try":
                    raise Manual(where(ln), "a catch without its try")
                blocks.append(("catch", None, [], ln))
            elif s != "}":
                blocks.append(("else", None, [], ln))
            continue
        if RE_TRY.match(s):
            blocks.append(("try", None, [], ln))
            continue
        m = RE_IF.match(s)
        if m:
            ref = flag_ref(m.group(1))
            if vec is not None and vec.get("phase") == "magic":
                blocks.append(("vcheck", None, [], ln))
                continue
            blocks.append(("gate" if ref else "other", ref, [], ln))
            continue
        if RE_FOR.match(s):
            if vec is not None and vec.get("phase") == "count":
                vec["phase"] = "loop"
            blocks.append(("loop", None, [], ln))
            continue
        if blocks and blocks[-1][0] == "vcheck":
            if "stream." in s:
                raise Manual(where(ln), "a read inside the vector check")
            continue
        if RE_MAGIC.match(s):
            vec = {"phase": "magic", "line": ln}
            continue
        if RE_COUNT.match(s) and vec is not None and vec["phase"] == "magic":
            vec["phase"] = "count"
            continue
        if vec is not None and vec.get("phase") == "loop":
            m = RE_ELEM.match(s)
            if m:
                vec["elem"] = "vec<%s>" % m.group(3)
                vec["local"] = m.group(2)
                continue
            m = RE_ADD.match(s)
            if m:
                arg = m.group(2).strip()
                rm = RE_ADDREAD.match(arg)
                if rm and rm.group(1) in READ_KIND:
                    vec["elem"] = "vec<%s>" % READ_KIND[rm.group(1)]
                elif arg != vec.get("local"):
                    raise Manual(where(ln), "a vector element of an unknown shape")
                vec["name"] = m.group(1)
                continue
            if "stream." in s:
                raise Manual(where(ln), "a read inside a vector loop of an unknown shape")
            continue
        m = RE_READ.match(s)
        if m and m.group(2) in READ_KIND:
            name, k = m.group(1), READ_KIND[m.group(2)]
            head, _, leaf = name.rpartition(".")
            if head and head in wrappers and leaf in TARGET_CLASS and k in ("i32", "i64"):
                wc, tc = PEER_CLASS.get(wrappers[head]), TARGET_CLASS[leaf]
                cls = wc if wc == tc else "?"
                emit("%s:id%s@%s" % (head, k[1:], cls), ln)
            else:
                emit("%s:%s" % (name, kind_of(name, k)), ln)
            continue
        m = RE_DESER.match(s)
        if m:
            local_type, name, typ = m.group(1), m.group(2), m.group(3)
            if local_type:
                container, idx = emit("%s:%s" % (name, typ), ln)
                locals_[name] = (container, idx)
            else:
                emit("%s:%s" % (name, typ), ln)
            continue
        m = RE_DESER1.match(s)
        if m:
            emit("%s:%s" % (m.group(1), m.group(2)), ln)
            continue
        m = RE_ADDOBJ.match(s)
        if m:
            # one boxed object read straight into a list (TL_documentEncrypted's single thumb)
            emit("%s:%s" % (m.group(1), m.group(2)), ln)
            continue
        m = RE_VECL.match(s)
        if m:
            emit("%s:vec<%s>" % (m.group(1), m.group(2)), ln)
            continue
        m = RE_BARE.match(s)
        if m and m.group(1) in READ_KIND:
            # a value the app reads and discards: named after its gate (or its position), never guessed
            gates = [b[1] for b in blocks if b[0] == "gate"]
            tag = gates[-1].replace(".", "").lower() if gates else str(len(items))
            emit("_discarded_%s:%s" % (tag, READ_KIND[m.group(1)]), ln)
            continue
        m = RE_VEC.match(s)
        if m:
            emit("%s:vec<%s>" % (m.group(1), m.group(2)), ln)
            continue
        m = RE_VECP.match(s)
        if m and m.group(2) in VEC_KIND:
            emit("%s:%s" % (m.group(1), VEC_KIND[m.group(2)]), ln)
            continue
        m = RE_NEW.match(s)
        if m:
            wrappers[m.group(1)] = m.group(2).rsplit(".", 1)[-1]
            continue
        m = RE_ALIAS.match(s)
        if m and m.group(2) in locals_:
            container, idx = locals_.pop(m.group(2))
            old = container[idx]
            container[idx] = m.group(1) + old[old.index(":"):]
            continue
        m = RE_FLAG.match(s)
        if m:
            ref = flag_ref(m.group(2))
            if ref:
                f = "%s=%s" % (m.group(1), ref)
                # a flag-only true is listed where it is computed: in a gate it belongs to the gate
                gates = [b for b in blocks if b[0] == "gate"]
                if any(b[0] in ("other", "else", "loop", "catch") for b in blocks):
                    raise Manual(where(ln), "a flag-only true set under a condition that is not one flag bit")
                if gates:
                    gates[-1][2].append(f)
                else:
                    items.append(f)
                continue
            if re.search(r"hasFlag\(|\(\s*flags\d*\s*&", m.group(2)):
                # a flag-only true computed by a wider expression: never dropped silently (review M2)
                raise Manual(where(ln), "a flag-only true set by an expression that is not one flag test")
        if "stream" in s:
            raise Manual(where(ln), "a statement on the stream of an unknown form")
        if re.fullmatch(r"(?:this\.)?\w+\([^;]*\);", s) and not any(b[0] == "catch" for b in blocks):
            # a call of the class's own method (applyFlags()) may set fields the reduction cannot see
            raise Manual(where(ln), "a call of a method the reduction does not follow")
    if blocks:
        raise Manual(where(last + 1), "unbalanced braces")
    return items


def contradictory_ids(items):
    """The inline ids of a reduced body whose wrapper class and target field disagree (T5): read by hand."""
    return [t for it in items for t in it.strip("[]").split() if re.search(r":id(32|64)@\?$", t)]


def same_items(got, want):
    """Row items equal the reduced items, an inline id of class `?` matching any resolved class (T5)."""
    if len(got) != len(want):
        return False
    for g, w in zip(got, want):
        gt, wt = g.split(), w.split()
        if len(gt) != len(wt):
            return False
        for a, b in zip(gt, wt):
            if a == b:
                continue
            m = re.fullmatch(r"(.*:id(?:32|64)@)\?(\]?)", b)
            if not (m and re.fullmatch(re.escape(m.group(1)) + r"(user|chat|channel|unknown)" +
                                       re.escape(m.group(2)), a)):
                return False
    return True


def split_items(fields):
    """The items of a fields column: a gate `[F.n: a b]` is one item."""
    out, cur = [], None
    for tok in fields.split():
        if cur is not None:
            cur.append(tok)
            if tok.endswith("]"):
                out.append(" ".join(cur))
                cur = None
        elif tok.startswith("["):
            cur = [tok]
        else:
            out.append(tok)
    if cur is not None:
        out.append(" ".join(cur))
    return out


def prefix_cut(items, needed):
    """The index after the last item that holds a needed field, or None."""
    cut = None
    for i, it in enumerate(items):
        for tok in it.strip("[]").split():
            name = tok.split(":", 1)[0].split("=", 1)[0]
            if ":" in tok and name in needed:
                cut = i + 1
    return cut


def row_source(src, typ, cid, case_line, c):
    """A row cites its class line; an alias case label (an ID that is not the class's constant) cites the label."""
    if cid == c.constructor:
        return "%s:%d" % (c.fname, c.line)
    return "%s:%d" % (src.base(typ).fname, case_line)


def build_rows(src, pin, skip=None):
    """The table rows the selection asks for at this pin (IDs in `skip` left out), and the MANUAL notes."""
    rows, manual = [], []
    for typ, mode, arg in SELECTION:
        sw = src.switch(typ)
        if sw is None:
            manual.append("%s: no switch at this pin" % typ)
            continue
        for cid, case_line, c in sorted(sw, key=lambda e: e[0]):
            if mode == "only" and cid not in arg:
                continue
            if skip is not None and cid in skip:
                continue
            body, fields = "full", ""
            if mode == "some-full" and c.name not in arg:
                body = "opaque"
            else:
                try:
                    items = reduce_class(src, c)
                except Manual as e:
                    manual.append("%s 0x%08x %s: MANUAL at %s: %s" % (typ, cid, c.name, e.where, e.why))
                    body, items = ("prefix" if mode == "prefix" else "full"), ["MANUAL"]
                if contradictory_ids(items):
                    manual.append("%s 0x%08x %s: MANUAL at %s:%d: inline id read by hand (T5): %s" % (
                        typ, cid, c.name, c.fname, c.line, " ".join(contradictory_ids(items))))
                if mode == "prefix" and items != ["MANUAL"]:
                    cut = prefix_cut(items, arg)
                    if cut is None:
                        manual.append("%s 0x%08x %s: no needed field" % (typ, cid, c.name))
                    elif cut < len(items):
                        body, items = "prefix", items[:cut]
                fields = " ".join(items)
            rows.append([typ, "0x%08x" % cid, c.name, body, pin, row_source(src, typ, cid, case_line, c), fields])
    return rows, manual


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        h.update(fh.read())
    return h.hexdigest()


def file_table(srcdir):
    rows = []
    for root, _, files in os.walk(srcdir):
        for f in files:
            p = os.path.join(root, f)
            rows.append("| `%s` | %s |" % (os.path.relpath(p, srcdir).replace(os.sep, "/"), sha256(p)))
    return ["| File | SHA-256 |", "|---|---|"] + sorted(rows)


def load_tsv(path):
    with open(path, encoding="utf-8") as fh:
        lines = fh.read().rstrip("\n").split("\n")
    if lines[0] != HEADER:
        raise SystemExit("%s: header is not the 4I4-P4 header" % path)
    return [l.split("\t") for l in lines[1:]]


def verify_row(src, r, ids):
    """The verdict of one table row against the source of its own pin (a MANUAL verdict comes with its reason)."""
    cid = int(r[1], 16)
    if cid not in ids:
        return "MISMATCH (the switch does not read this ID)"
    ln, c = ids[cid]
    if c.name != r[2]:
        return "MISMATCH (class %s)" % c.name
    source = row_source(src, r[0], cid, ln, c)
    if source != r[5]:
        return "MISMATCH (source %s)" % source
    mode = {t: m for t, m, _ in SELECTION}[r[0]]
    arg = {t: a for t, _, a in SELECTION}[r[0]]
    if (mode == "some-full" and c.name not in arg) != (r[3] == "opaque"):
        return "MISMATCH (body %s against the selection)" % r[3]
    if r[3] == "opaque":
        return "verified"
    if r[3] == "prefix" and mode != "prefix":
        return "MISMATCH (a prefix row of a type the selection reads in full)"
    try:
        items = reduce_class(src, c)
    except Manual as e:
        # the builder reads the row by hand at this line (plan 4I4 Task 1); the reason goes in the note
        return "MANUAL, read by hand at %s" % e.where, e.why
    got = split_items(r[6])
    if r[3] == "full" and not same_items(got, items):
        return "MISMATCH (fields %s)" % " ".join(items)
    if r[3] == "prefix" and not (len(got) < len(items) and same_items(got, items[:len(got)])):
        return "MISMATCH (not a proper prefix of %s)" % " ".join(items)
    if contradictory_ids(items[:len(got)]):
        return "MANUAL, read by hand at %s:%d" % (c.fname, c.line), "the class of an inline id (T5)"
    return "verified"


def check(tsv, pin, srcdir, commit):
    """The report of one pin and the number of MISMATCH and missing verdicts in it."""
    src = Source(srcdir)
    table = load_tsv(tsv)
    by_id = {int(r[1], 16): r for r in table}
    switches = {typ: (src.switch(typ) or []) for typ, _, _ in SELECTION}
    out, bad = [], 0
    out += ["## %s (%s)" % (pin, commit or "commit not given"), "",
            "Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):", ""]
    out += file_table(srcdir)
    # review I1: the report names the exact fact files it checked, so a later edit of either is seen
    facts = os.path.join(os.path.dirname(tsv), "facts.tsv")
    out += ["", "Fact files checked (SHA-256 of the committed bytes):", "", "| File | SHA-256 |", "|---|---|",
            "| `constructors.tsv` | %s |" % sha256(tsv), "| `facts.tsv` | %s |" % sha256(facts)]
    out += ["", "Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's "
            "switch, the class, the source line, the body the selection asks for, and the field order reduced from "
            "`readParams`; a `prefix` row is a proper prefix of the reduced body):", "",
            "| Type | ID | Class | Verdict | Source | Note |", "|---|---|---|---|---|---|"]
    own = [r for r in table if r[4] == pin]
    for typ, _, _ in SELECTION:
        ids = {cid: (ln, c) for cid, ln, c in switches[typ]}
        for r in [r for r in own if r[0] == typ]:
            verdict, note = verify_row(src, r, ids), ""
            if isinstance(verdict, tuple):
                verdict, note = verdict
            if verdict.startswith("MISMATCH"):
                bad += 1
            out.append("| %s | %s | %s | %s | %s | %s |" % (r[0], r[1], r[2], verdict, r[5], note))
    if not own:
        out.append("| - | - | - | no row is pinned here | - | - |")
    out.append("")
    if pin == "A-HEAD":
        out += ["Completeness: every ID the switch of each type reads at this pin is a row (MessageAction: the "
                "phone-call action only, by design).", "",
                "| Type | IDs the switch reads | Rows pinned here | Verdict |", "|---|---|---|---|"]
        for typ, mode, _ in SELECTION:
            if mode == "only":
                continue
            ids = {cid: (ln, c) for cid, ln, c in switches[typ]}
            absent = sorted(cid for cid in ids if cid not in by_id or by_id[cid][0] != typ)
            for cid in absent:
                bad += 1
                out.append("| %s | 0x%08x | %s | missing |" % (typ, cid, ids[cid][1].name))
            out.append("| %s | %d | %d | %s |" % (typ, len(ids), len([r for r in own if r[0] == typ]),
                                                  "complete" if not absent else "incomplete"))
    else:
        out += ["Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv` "
                "(MessageAction: the `TL_messageActionPhoneCall` family only).", "",
                "| Pin | Type | ID | Verdict | Class at this pin |", "|---|---|---|---|---|"]
        for typ, _, _ in SELECTION:
            if src.switch(typ) is None:
                out.append("| %s | %s | - | type not defined at this pin | - |" % (pin, typ))
                continue
            fam = MEMBERSHIP_FAMILY.get(typ)
            for cid, ln, c in sorted(switches[typ], key=lambda e: e[0]):
                if fam and not c.name.startswith(fam):
                    continue
                ok = cid in by_id and by_id[cid][0] == typ
                if not ok:
                    bad += 1
                out.append("| %s | %s | 0x%08x | %s | %s (%s:%d) |" % (
                    pin, typ, cid, "in-table" if ok else "missing", c.name, c.fname, c.line))
    out.append("")
    return "\n".join(out) + "\n", bad


DDL_INTRO = """# DDL cross-check (plan 4I4)

The cache4.db snapshots `v181.sql`, `v159.sql`, `v84.sql`, `v64.sql` and `v84-upgraded-v181.sql` of this directory,
checked by `tools/fixtures/ddl_crosscheck.py --family telegram` against `MessagesStorage.java` and, where the pin has
it, `DatabaseMigrationHelper.java` (paths below `TMessagesProj/src/main/java/org/telegram/messenger/`) of the
repository TelegramMessenger/Telegram-Android at the pins above, one run per pin (A-HEAD checks `v181.sql` and
`v84-upgraded-v181.sql`). The fetched files are the ones pinned by commit and SHA-256 in the sections above; they were
held in scratch only. `v181.sql`, `v159.sql`, `v84.sql` and `v64.sql` are `verified` (every CREATE TABLE equal to the
pin's own, column by column); `v84-upgraded-v181.sql` is `historical`: the CREATE TABLE statements of `v84.sql` then
the ALTER TABLE ADD COLUMN statements of the upgrade code at A-HEAD (each cited by line) that reach user_version 181.
It does not hold the tables the upgrade code creates after version 84 (for example `messages_topics`). No sampled tag
has user_version 85, so there is no snapshot for it (`media_v4` is covered by `v159.sql`).

"""


def assemble(schemas, reports, ddl_reports=()):
    """CROSSCHECK.md: the fixed text, the reports in the order given, the docs cross-check, the facts.tsv rows and,
    last, the DDL cross-check section (plan 4I4 Task 4) when ddl_reports are given."""
    out = PREAMBLE + "# Constructor rows, per pin (`telegram_tl_facts.py --check`)\n\n"
    for path in reports:
        with open(path, encoding="utf-8") as fh:
            out += fh.read() + "\n"
    out += DOCS
    out += ("# facts.tsv rows\n\nEach fact was read by hand at the cited line of its pin (the note column of "
            "`facts.tsv` names the other lines read).\n\n| Key | Pin | Source | Value | Read |\n|---|---|---|---|---|\n")
    with open(os.path.join(schemas, "facts.tsv"), encoding="utf-8") as fh:
        for line in fh.read().rstrip("\n").split("\n")[1:]:
            key, value, pin, source, _ = line.split("\t")
            out += "| %s | %s | %s | %s | read by hand |\n" % (key, pin, source, value)
    if ddl_reports:
        out = out.rstrip("\n") + "\n\n" + DDL_INTRO
        for path in ddl_reports:
            with open(path, encoding="utf-8") as fh:
                out += fh.read() + "\n"
    with open(os.path.join(schemas, "CROSSCHECK.md"), "w", encoding="utf-8", newline="\n") as fh:
        fh.write(out.rstrip("\n") + "\n")


def take(args, flag):
    """Remove `flag VALUE` from args and return VALUE (None when absent)."""
    if flag not in args:
        return None
    i = args.index(flag)
    v = args[i + 1]
    del args[i:i + 2]
    return v


def main(argv):
    args = list(argv)
    if args and args[0] == "--emit":
        del args[0]
        pin = take(args, "--pin") or "A-HEAD"
        absent = take(args, "--absent-from")
        if len(args) != 1 or pin not in PINS:
            raise SystemExit(__doc__)
        src = Source(args[0])
        skip = {int(r[1], 16) for r in load_tsv(absent)} if absent else None
        rows, manual = build_rows(src, pin, skip)
        sys.stdout.write(HEADER + "\n")
        for r in rows:
            sys.stdout.write("\t".join(r) + "\n")
        for m in manual:
            sys.stderr.write(m + "\n")
        return 0
    if args and args[0] == "--check":
        del args[0]
        pin, commit, report_path = take(args, "--pin"), take(args, "--commit"), take(args, "--write-report")
        if len(args) != 2 or pin not in PINS:
            raise SystemExit(__doc__)
        report, bad = check(args[0], pin, args[1], commit)
        if report_path:
            with open(report_path, "w", encoding="utf-8", newline="\n") as fh:
                fh.write(report)
        else:
            sys.stdout.write(report)
        sys.stderr.write("%s: %d MISMATCH or missing\n" % (pin, bad))
        return 1 if bad else 0
    if args and args[0] == "--assemble" and len(args) >= 3:
        rest = args[2:]
        ddl = rest[rest.index("--ddl") + 1:] if "--ddl" in rest else []
        assemble(args[1], rest[:rest.index("--ddl")] if "--ddl" in rest else rest, ddl)
        return 0
    raise SystemExit(__doc__)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

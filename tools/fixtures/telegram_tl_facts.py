"""Telegram Android TL constructor facts: emit and cross-check (plan 4I4, Task 1, writer's call 4I4-P3 b).

    py -3 -I tools/fixtures/telegram_tl_facts.py --emit <srcdir> [--pin <name>] [--absent-from <constructors.tsv>]
    py -3 -I tools/fixtures/telegram_tl_facts.py --check <constructors.tsv> --pin <name> <srcdir>
        [--head <A-HEAD srcdir>] [--commit SHA] [--write-report FILE]
    py -3 -I tools/fixtures/telegram_tl_facts.py --assemble <schemasdir> <report.md>... [--ddl <ddl-report.md>...]
    py -3 -I tools/fixtures/telegram_tl_facts.py --hand <schemasdir> <notes.md>
    py -3 -I tools/fixtures/telegram_tl_facts.py --go <tables.go> <constructors.tsv>

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
`missing`) and every object type a full or prefix row names must be a type of the selection (the closure, plan
4I4b design D3: else `missing (type <T> named by <class>)`); at an older pin (which needs --head, the A-HEAD
sources) it reports membership, every ID of the pinned types' switches `in-table` or `missing`, then the body
comparison of plan 4I4b (design D6, ruling B4): every row whose ID the pin's switch reads, its body reduced at
that pin against its A-HEAD reading, `equal`, `CONFLICT` or `membership-only` (the reduction cannot read it there);
a CONFLICT passes only for an ID of PIN_CONFLICTS on an opaque row. Types are spelled as the table spells them at
every pin (design D5: a class with no deserializer of its own names the type whose switch reads it, `owner`; an
`Outer.Name` type is `Name` at a pin without `Outer.java`). Exit status 1 on any MISMATCH, missing or counted
CONFLICT. --hand writes a hand-read section (the notes file, its first line the heading) into CROSSCHECK.md,
replacing the section under that heading, else appending it last (4I4b-P7). Each
report names the SHA-256 of the constructors.tsv and facts.tsv it checked. A MANUAL row is reported as
"MANUAL, read by hand at <file>:<line>" with its reason: the builder reads it there. --assemble writes
<schemasdir>/CROSSCHECK.md from the fixed text below, the reports in the order given, and facts.tsv.
Plan 4I4 Task 4: the reports after --ddl are the runs of ddl_crosscheck.py --family telegram (one per pin); they
are appended last, under the heading "# DDL cross-check (plan 4I4)", so --assemble reproduces the whole file.
Plan 4I4 Task 7: --go writes the parser's Go constructor table (internal/parsers/androidtelegram/tables.go) from the
committed constructors.tsv alone (no sources): one tl.Constructor per row, in ID order, the fields reduced to tl.Field by
the 4I4-P4 grammar (a flags word is KFlags, `name=F.n` a KTrue gated by that flags word, a secret KSecret). A token
outside the grammar stops the run; the output is aligned as gofmt aligns it (TestTelegramTablesGoIsGofmtClean checks
it), so formatting leaves it unchanged.
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
#   some-full:   the listed classes full, every other constructor of the switch opaque, except one whose readParams
#                reads nothing: it is read whole, so it is full with an empty field list (Task 5 review ruling M3).
#   prefix:      every constructor up to its last needed field (spec Q2, T6), full when that is the body.
#
# Plan 4I4b Task 1 (design D1, D3, rulings R2, B2, B9): every type a full or prefix row names is in the selection (the
# closure), so every ID its A-HEAD switch reads is a row; a closure type the parser does not read is `some-full` with
# no class (every row opaque, except a class that reads nothing, rule M3). MessageAction is `some-full` like any other
# type: every action the switch reads is a row (B2).
SELECTION = [
    ("Message", "all-full", None),
    ("Peer", "all-full", None),
    ("MessageFwdHeader", "all-full", None),
    ("MessageReplyHeader", "all-full", None),
    ("MessageEntity", "all-full", None),
    ("MessageAction", "some-full", {"TL_messageActionPhoneCall"}),
    ("PhoneCallDiscardReason", "all-full", None),
    ("MessageMedia", "some-full", {
        "TL_messageMediaPhoto", "TL_messageMediaDocument", "TL_messageMediaGeo", "TL_messageMediaGeoLive",
        "TL_messageMediaContact", "TL_messageMediaVenue", "TL_messageMediaWebPage", "TL_messageMediaPoll",
        # plan 4I4b (recon §3.2): the media containers of every layer
        "TL_messageMediaDice", "TL_messageMediaDocument_old", "TL_messageMediaPhoto_layer68",
        "TL_messageMediaDice_layer220", "TL_messageMediaDocument_layer187", "TL_messageMediaVideo_layer45",
        "TL_messageMediaContact_layer81", "TL_messageMediaDice_layer111", "TL_messageMediaPhoto_layer223",
        "TL_messageMediaVenue_layer71", "TL_messageMediaGeoLive_layer119", "TL_messageMediaDocument_layer74",
        "TL_messageMediaDocument_layer159", "TL_messageMediaVideo_old", "TL_messageMediaWebPage_layer165",
        "TL_messageMediaPhoto_layer74", "TL_messageMediaAudio_layer45", "TL_messageMediaPhoto_old",
        "TL_messageMediaContact_layer131", "TL_messageMediaDocument_layer197_2", "TL_messageMediaDocument_layer197",
        "TL_messageMediaDocument_layer68"}),
    ("Photo", "all-full", None),
    ("Document", "all-full", None),
    ("GeoPoint", "all-full", None),
    ("WebPage", "all-full", None),
    ("User", "prefix", {"id", "first_name", "last_name", "username", "phone"}),
    ("Chat", "prefix", {"id", "title", "username"}),
    # plan 4I4b: the media sub-objects and the tail objects (recon §3.1, §3.3)
    ("PhotoSize", "all-full", None),
    ("VideoSize", "all-full", None),
    ("DocumentAttribute", "all-full", None),
    ("FileLocation", "all-full", None),
    ("InputStickerSet", "all-full", None),
    ("TL_maskCoords", "all-full", None),
    ("Video", "all-full", None),
    ("Audio", "all-full", None),
    ("MessageReplies", "all-full", None),
    ("MessageReactions", "all-full", None),
    ("Reaction", "all-full", None),
    ("MessagePeerReaction", "all-full", None),
    ("MessageReactor", "all-full", None),
    ("RestrictionReason", "all-full", None),
    ("TL_factCheck", "all-full", None),
    ("TL_textWithEntities", "all-full", None),
    ("SuggestedPost", "all-full", None),
    ("TL_stars.StarsAmount", "all-full", None),
    ("TL_messages_emojiGameOutcome", "all-full", None),
    # 0x6fb250d1 is read two ways across the pins (ruling R3): opaque
    ("ReactionCount", "some-full", {"TL_reactionCount"}),
    ("WebPageAttribute", "some-full", {
        "TL_webPageAttributeStickerSet", "TL_webPageAttributeStarGiftCollection", "TL_webPageAttributeAiComposeTone"}),
    # the closure types the parser does not read (design D3): opaque, except a class that reads nothing (M3)
    ("Poll", "some-full", set()),
    ("PollResults", "some-full", set()),
    ("ReplyMarkup", "some-full", set()),
    ("InputUser", "some-full", set()),
    ("TL_iv.RichText", "some-full", set()),
    ("TL_iv.Page", "some-full", set()),
    ("TL_iv.RichMessage", "some-full", set()),
]
# The IDs pinned app versions read two ways (recon §4.2, design D4, ruling R3; coordinator ruling T1b generalises it):
# each row is opaque whatever the selection says, so the table never picks one reading; its older-pin body CONFLICT is
# reported with both readings and passes, but only on an opaque row (4I4b-P7). 0x85849473 and 0x332ba9ed: the older
# classes have no readParams of their own and inherit their parent's (flags and a flag-gated field), A-HEAD overrides
# it with an empty one; 0x48a30254: the rows' element type differs (recon §4.3).
PIN_CONFLICTS = {0x6FB250D1, 0x85849473, 0x332BA9ED, 0x48A30254}
# Ruling T1c (compatible widening): an older pin reads a vector's elements by one self-checking class, A-HEAD by the
# switch of a type that holds that class's ID. {older class: (A-HEAD type, ID)}: the older reading is the A-HEAD one
# when the ID's body is the same at that pin and at A-HEAD (checked by body_check at every pin).
WIDENINGS = {"TL_webPageAttributeTheme": ("WebPageAttribute", 0x54B56617)}
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

Plan 4I4b (Task 1, fetched again 2026-11-26 the same way, every file with the SHA-256 the per-pin tables below give):
the fetch adds every `tgnet/tl/*.java` and `tgnet/tl/legacy/*.java` of each pin, `ui/Cells/ChatMessageCell.java` and
`messenger/LocaleController.java` at every pin (the `edit_date` route of `facts.tsv`, ruling B3) and
`tgnet/VectorLegacy.java` at A-HEAD; the 4I4 statements below about the files fetched hold for 4I4 only. The table grows
to the closure (design D3, rulings R2, B2): every object type a `full` or `prefix` row names is a type of the table,
every ID its A-HEAD switch reads is a row, MessageAction included, and the A-HEAD `--check` reports it. A field typed by
a class with no deserializer of its own names the type whose switch reads it (design D5: `reactions:MessageReactions`
in `TL_messageService` and `_layer204`, `TLRPC.java:27398` and `27428`), and a type nested in another file is
`Outer.Name` (`TL_iv.Page`, `TL_stars.StarsAmount`), `Name` at a pin without that file. Each older pin's report ends
with the body comparison (design D6, rulings B4, T1a): what each row reads there against its A-HEAD reading; field
names that differ on one wire shape are informational. An ID the pins read two ways is `opaque` and its pin conflict
names both readings (rulings R3, T1b): `0x6fb250d1` (a string at release-5.14.0_1851 and release-8.1.1_2431, a boxed
Reaction at A-HEAD), `0x85849473` and `0x332ba9ed` (the older classes inherit their parent's reader, A-HEAD reads
nothing; `0x85849473` was a `full` row in 4I4, corrected here), `0x48a30254` (the rows' element type). `TL_webPage`
`0xe89c45b2` keeps the A-HEAD `vec<WebPageAttribute>`: where an older pin reads the attributes by the single class
`TL_webPageAttributeTheme`, that class's ID `0x54b56617` reads the same fields at that pin and at A-HEAD (a compatible
widening, ruling T1c; its `settings:ThemeSettings` element `0x9c14984a` is read by hand at A-HEAD and not compared). The
hand-read rows of plan 4I4b are in their own section at the end of this file.

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

- A constructor of a type read only in part (`some-full`: MessageMedia, Photo, Document, WebPage) whose `readParams`
  reads nothing, for example `TL_messageMediaEmpty` `0x3ded6320`, is `full` with an empty field list, never `opaque`:
  its id is the whole object, so a decode reads it whole (Task 5 review, coordinator ruling M3). Every other
  constructor of those types outside the selection stays `opaque`.
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
        self.deser = False  # declares a static TLdeserialize or fromConstructor of its own (design D5)
        self.methods = []  # (signature line, first body line index, last body line index)


def index_file(fname, lines):
    """Every class of one file with its constructor constant and its readParams bodies."""
    classes, stack, depth = [], [], 0
    class_re = re.compile(r"\bclass\s+(\w+)(?:\s*<[^>]*>)?(?:\s+extends\s+([\w.]+))?[^{;]*\{")
    cons_re = re.compile(r"\bstatic\s+(?:final\s+)?int\s+constructor\s*=\s*(0x[0-9a-fA-F]+)\s*;")
    deser_re = re.compile(r"\bstatic\s+[\w.<>]+\s+(?:TLdeserialize|fromConstructor)\s*\(")
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
            if deser_re.search(line) and depth == stack[-1][1] + 1:
                cur.deser = True
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

    def resolve(self, ref, near=None):
        """A class named by a switch or a field: `Name` (the file `near` first, then TLRPC) or `Outer.Name`
        (`TLRPC.Name` is `Name`)."""
        parts = ref.split(".")
        if len(parts) > 1 and parts[0] == "TLRPC":
            parts = parts[1:]
        cands = self.by_name.get(parts[-1], [])
        if len(parts) > 1:
            cands = [c for c in cands if c.outer is not None and c.outer.name == parts[-2]]
        else:
            here = [c for c in cands if near is not None and c.fname == near]
            tl = [c for c in cands if c.fname == "TLRPC.java"]
            cands = here or tl or cands
        return cands[0] if len(cands) == 1 else None

    @staticmethod
    def spell(c):
        """A class as a field names it: `Name` for a class of TLRPC.java, `Outer.Name` for one nested elsewhere."""
        return c.name if c.outer is None or c.outer.name == "TLRPC" else c.outer.name + "." + c.name

    def _top(self, name, outer=None):
        """The class `name` (nested in `outer` when given) that extends TLObject, TLRPC.java first, or None."""
        cands = [c for c in self.by_name.get(name, []) if c.extends in ("TLObject", "TLRPC.TLObject")]
        if outer is not None:
            cands = [c for c in cands if c.outer is not None and c.outer.name == outer]
        else:
            cands = [c for c in cands if c.fname == "TLRPC.java"] or cands
        return cands[0] if len(cands) == 1 else None

    def _names(self, typ):
        """(outer, name) lookups of a type at this pin: `Outer.Name` drops its outer prefix at a pin without the file
        `Outer.java` (TL_iv.Page is Page in TLRPC.java at A-11.4.2 and older)."""
        parts = typ.split(".")
        if len(parts) == 1:
            return None, typ
        if parts[0] + ".java" in self.files:
            return parts[0], parts[1]
        return None, parts[1]

    def base(self, typ):
        """The class of the type: the class `typ` extending TLObject (in every loaded file)."""
        outer, name = self._names(typ)
        return self._top(name, outer)

    def type_class(self, typ):
        """The class that reads the type at this pin: its base, else the single class TL_<lowerFirst(name)>."""
        b = self.base(typ)
        if b is not None:
            return b
        outer, name = self._names(typ)
        one = self._top("TL_" + name[0].lower() + name[1:], outer)
        return one if one is not None and one.constructor is not None else None

    def owner(self, name, near=None):
        """Design D5: the class whose static TLdeserialize/fromConstructor a call on `name` reaches (Java static
        methods are inherited through `extends`), spelled as a field names it; None when it cannot be resolved."""
        c, seen = self.resolve(name, near), set()
        while c is not None and id(c) not in seen:
            seen.add(id(c))
            if c.deser:
                return self.spell(c)
            if not c.extends or c.extends in ("TLObject", "TLRPC.TLObject"):
                return None
            c = self.resolve(c.extends, c.fname)
        return None

    def switch(self, typ):
        """[(id, case line, class)] of the type's constructor switch, or None when the type is not defined."""
        b = self.base(typ)
        if b is None:
            # a type with one constructor and no base class (MessageReplyHeader at release-8.1.1_2431: the
            # class TL_messageReplyHeader extends TLObject and checks its own constructor)
            one = self.type_class(typ)
            return [(one.constructor, one.line, one)] if one is not None else None
        lines = self.files[b.fname]
        start = None
        for i in range(b.line - 1, b.end):
            if re.search(r"\bswitch\s*\(\s*constructor\s*\)\s*\{", lines[i]):
                start = i
                break
        if start is None:
            # a self-checking class (SuggestedPost, TL_factCheck): it reads its own constructor only
            return [(b.constructor, b.line, b)] if b.constructor is not None else None
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
                    c = self.resolve(cm.group(1), b.fname) if cm else None
                    if c is None or c.constructor is None:
                        raise SystemExit("unresolved case label at %s:%d" % (b.fname, i + 1))
                    labels.append((c.constructor, i + 1))
            m = re.search(r"(?:return|result\s*=)\s*new\s+([\w.]+)\s*\(", line)
            if m and labels:
                c = self.resolve(m.group(1), b.fname)
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
        parent = src.resolve(c.extends or "", c.fname)
        if parent is None:
            raise Manual("%s:%d" % (c.fname, c.line), "no readParams and no parent found")
        if parent.extends in ("TLObject", "TLRPC.TLObject"):
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
    return "%s:%d" % (src.type_class(typ).fname, case_line)


def pin_types(src):
    """Design D5, ruling B4: {pin spelling: table spelling} of every SELECTION type at this pin. The type's own class
    (its base, or its single class: TL_messageReactions, TL_poll, TL_reactionCount at the old pins; Page for TL_iv.Page
    where TL_iv.java does not exist) and every class whose TLdeserialize call reaches that class (`owner`), so a field
    typed TL_messageReactions names MessageReactions."""
    names = {}
    for typ, _, _ in SELECTION:
        c = src.type_class(typ)
        if c is not None:
            names.setdefault(src.spell(c), typ)
    for c in src.classes:
        if src.spell(c) in names:
            continue
        o = src.owner(src.spell(c), c.fname)
        if o is not None and o in names:
            names[src.spell(c)] = names[o]
    return names


def compare_names(src):
    """Ruling B4: {spelling: comparison name} of every class at this pin, for the older-pin body comparison. A class
    names the type whose deserializer a call on it reaches (design D5, `owner`); a type of the table is spelled as the
    table spells it (pin_types); any other type by its class name without an outer prefix, a self-checking single class
    TL_<lowerFirst(T)> of a pin without a type T as T (the single-class rule of `type_class`)."""
    table = pin_types(src)
    names = {}
    for c in src.classes:
        o = src.owner(src.spell(c), c.fname)
        oc = src.resolve(o, c.fname) if o is not None else None
        oc = oc or c
        key = table.get(src.spell(oc))
        if key is None:
            key = oc.name
            m = re.fullmatch(r"TL_([a-z])(\w*)", key)
            if m and src.base(m.group(1).upper() + m.group(2)) is None:
                key = m.group(1).upper() + m.group(2)
        for spelling in (src.spell(c), c.name):
            if spelling == c.name and src.resolve(c.name) is not c:
                continue  # a bare name another class answers to first
            names.setdefault(spelling, key)
    names.update(table)
    return names


def map_token(tok, names):
    """One field token with its object type spelled as the table spells it (`names` from pin_types)."""
    close = tok.endswith("]")
    t = tok[:-1] if close else tok
    if ":" in t and not t.startswith("["):
        n, k = t.split(":", 1)
        if k.startswith("vec<") and k.endswith(">") and k[4:-1] in names:
            k = "vec<%s>" % names[k[4:-1]]
        elif k in names:
            k = names[k]
        t = n + ":" + k
    return t + ("]" if close else "")


def map_items(items, names):
    """Reduced items with every object type spelled as the table spells it (design D5)."""
    return [" ".join(map_token(t, names) for t in it.split()) for it in items]


def normalize(items, names):
    """Design D6, ruling B4: a body as the older-pin comparison sees it: flag-only trues (`name=F.n`, no bytes) dropped,
    the `mType.` holder stripped, `question.text` read as `question`, the object types spelled as the table spells
    them. A gate left empty is dropped."""
    out = []
    for it in items:
        toks = []
        for t in it.split():
            t = map_token(t, names)
            gate_open = re.fullmatch(r"\[F2?\.\d+:", t)
            close = t.endswith("]") and not gate_open
            body = t[:-1] if close else t
            if not gate_open:
                if re.fullmatch(NAME + r"=F2?\.\d+", body):
                    if close and toks:
                        toks[-1] += "]"
                    continue
                body = body.replace("mType.", "")
                body = re.sub(r"^question\.text:", "question:", body)
            toks.append(body + ("]" if close else ""))
        if toks and re.fullmatch(r"\[F2?\.\d+:\]", toks[-1]):
            toks.pop()
        # a gate whose fields were all dropped: "[F.n:" followed by its closing bracket only
        joined = " ".join(toks)
        joined = re.sub(r"\[F2?\.\d+:\]", "", joined).strip()
        if joined:
            out.append(joined)
    return out


def build_rows(src, pin, skip=None):
    """The table rows the selection asks for at this pin (IDs in `skip` left out), and the MANUAL notes."""
    rows, manual = [], []
    names = pin_types(src)
    for typ, mode, arg in SELECTION:
        sw = src.switch(typ)
        if sw is None:
            manual.append("%s: no switch at this pin" % typ)
            continue
        for cid, case_line, c in sorted(sw, key=lambda e: e[0]):
            if skip is not None and cid in skip:
                continue
            body, fields = "full", ""
            if cid in PIN_CONFLICTS or (mode == "some-full" and c.name not in arg and not reads_nothing(src, c)):
                body = "opaque"
            else:
                try:
                    items = map_items(reduce_class(src, c), names)
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


def reads_nothing(src, c):
    """The class reads no field (an empty reduced body): read whole, so never opaque (ruling M3). A class the reduction
    cannot read (MANUAL) is not known to read nothing."""
    try:
        return reduce_class(src, c) == []
    except Manual:
        return False


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


def verify_row(src, r, ids, names):
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
    if (cid in PIN_CONFLICTS or (mode == "some-full" and c.name not in arg and not reads_nothing(src, c))) != (
            r[3] == "opaque"):
        return "MISMATCH (body %s against the selection)" % r[3]
    if r[3] == "opaque":
        return "verified"
    if r[3] == "prefix" and mode != "prefix":
        return "MISMATCH (a prefix row of a type the selection reads in full)"
    try:
        items = map_items(reduce_class(src, c), names)
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


SCALAR_KINDS = {"#", "i32", "i64", "dbl", "bool", "bytes", "str", "secret", "vec<i32>", "vec<i64>", "vec<str>",
                "vec<bytes>"}


def named_types(fields):
    """The object types a fields column names (a field `name:Type` or `name:vec<Type>`)."""
    out = []
    for tok in fields.split():
        t = tok.rstrip("]")
        if t.startswith("[") or ":" not in t:
            continue
        k = t.split(":", 1)[1]
        if k in SCALAR_KINDS or re.fullmatch(r"id(32|64)@\w+", k):
            continue
        out.append(k[4:-1] if k.startswith("vec<") and k.endswith(">") else k)
    return out


def closure(table):
    """Design D3, ruling R2: (type, class) of every object type a full or prefix row names that is not a SELECTION
    type; the table is closed when this is empty."""
    types = {t for t, _, _ in SELECTION}
    return sorted({(k, r[2]) for r in table if r[3] != "opaque" for k in named_types(r[6]) if k not in types})


def wire_shape(items):
    """Ruling T1a: what a body reads, field names set aside: the gates, the flags words and each field's wire type (an
    inline id as its width)."""
    out = []
    for it in items:
        toks = []
        for t in it.split():
            close = t.endswith("]") and not re.fullmatch(r"\[F2?\.\d+:", t)
            b = t[:-1] if close else t
            if ":" in b and not b.startswith("["):
                k = b.split(":", 1)[1]
                b = re.sub(r"^id(32|64)@.*$", lambda m: "i" + m.group(1), k)
            toks.append(b + ("]" if close else ""))
        out.append(" ".join(toks))
    return out


def name_differences(older, ref):
    """The field tokens two bodies of one wire shape spell differently (`older / ref`), for the informational note."""
    a = " ".join(older).split()
    b = " ".join(ref).split()
    return ["%s / %s" % (x.rstrip("]"), y.rstrip("]")) for x, y in zip(a, b) if x != y]


def class_of_id(src, cid):
    """The class whose constructor constant is cid at this pin (unique), or None."""
    cs = [c for c in src.classes if c.constructor == cid]
    return cs[0] if len(cs) == 1 else None


def widen(src, head, raw, names, hnames):
    """Ruling T1c: the older reading with each WIDENINGS element class read as its A-HEAD type, when that class's ID has
    the same wire shape at this pin and at A-HEAD: (items, notes); notes is empty when nothing was widened."""
    notes, out = [], raw
    for cls, (typ, wid) in WIDENINGS.items():
        widened = map_items(out, {cls: typ})
        if widened == out:
            continue
        oc, hc = class_of_id(src, wid), class_of_id(head, wid)
        try:
            same = (oc is not None and hc is not None and
                    wire_shape(normalize(reduce_class(src, oc), names)) ==
                    wire_shape(normalize(reduce_class(head, hc), hnames)))
        except Manual:
            same = False
        if same:
            out = widened
            notes.append("vec<%s> read as vec<%s>: 0x%08x %s reads the same at %s:%d and at %s:%d" % (
                cls, typ, wid, cls, oc.fname, oc.line, hc.fname, hc.line))
    return out, notes


def body_check(table, src, head, pin):
    """Design D6, ruling B4, 4I4b-P7, rulings T1a-T1c: the body of every row whose ID this pin's switch reads, compared
    with its A-HEAD reading by what is read (normalised at each pin by `normalize`, then `wire_shape`): (report lines,
    number of CONFLICT verdicts that count as bad). Field names that differ on one wire shape are informational
    (`equal on the wire`); an older vector element class of WIDENINGS whose ID reads the same is a `compatible
    widening`. The A-HEAD reading is the reduced body at A-HEAD, or the row's own fields when A-HEAD does not read the
    ID or the row is read by hand there. A row the reduction cannot read at this pin is membership-only."""
    names, hnames = compare_names(src), compare_names(head)
    lines = ["Body comparison with A-HEAD: every row of `constructors.tsv` whose ID the switch of its type reads at "
             "this pin, what its body reads at this pin against its A-HEAD reading (field order, flag bits and wire "
             "types; flag-only trues, `mType.` holders and field names set aside, object types named by the "
             "deserializer a call reaches, ruling T1a). `equal on the wire` lists the field names that differ "
             "(informational); `membership-only` names the line the reduction cannot read.", "",
             "| Pin | Type | ID | Verdict | Class at this pin |", "|---|---|---|---|---|"]
    bad = compared = membership = 0
    for typ, _, _ in SELECTION:
        sw = {cid: (ln, c) for cid, ln, c in (src.switch(typ) or [])}
        hsw = {cid: (ln, c) for cid, ln, c in (head.switch(typ) or [])}
        for r in [r for r in table if r[0] == typ]:
            cid = int(r[1], 16)
            if cid not in sw:
                continue
            ln, c = sw[cid]
            at = "%s (%s:%d)" % (c.name, c.fname, c.line)
            ref, ref_at, ref_manual = None, None, None
            if cid in hsw:
                hc = hsw[cid][1]
                ref_at = "%s:%d" % (hc.fname, hc.line)
                try:
                    ref = normalize(reduce_class(head, hc), hnames)
                except Manual as e:
                    ref_manual = e.where
            from_row = False
            if ref is None and r[3] != "opaque" and "MANUAL" not in r[6]:
                ref, ref_at, from_row = normalize(split_items(r[6]), hnames), "%s@%s" % (r[5], r[4]), True
            try:
                raw = reduce_class(src, c)
            except Manual as e:
                membership += 1
                lines.append("| %s | %s | %s | membership-only (MANUAL at %s) | %s |" % (pin, typ, r[1], e.where, at))
                continue
            if ref is None:
                membership += 1
                lines.append("| %s | %s | %s | membership-only (MANUAL at %s) | %s |" % (
                    pin, typ, r[1], ref_manual or r[5], at))
                continue
            compared += 1
            older = normalize(raw, names)
            wide, notes = widen(src, head, raw, names, hnames)
            wider = normalize(wide, names)
            if from_row and r[3] == "prefix":
                older, wider = older[:len(ref)], wider[:len(ref)]
            if wire_shape(older) == wire_shape(ref):
                diff = name_differences(older, ref)
                verdict = "equal" if not diff else "equal on the wire (names differ: %s)" % "; ".join(diff)
            elif notes and wire_shape(wider) == wire_shape(ref):
                verdict = "compatible widening (%s)" % "; ".join(notes)
            elif cid in PIN_CONFLICTS and r[3] == "opaque":
                verdict = "CONFLICT (opaque; pin conflict: %s at %s:%d, %s at %s)" % (
                    " ".join(older) or "nothing", c.fname, c.line, " ".join(ref) or "nothing", ref_at)
            else:
                verdict = "CONFLICT (%s at %s:%d, %s at %s)" % (
                    " ".join(older) or "nothing", c.fname, c.line, " ".join(ref) or "nothing", ref_at)
                bad += 1
            lines.append("| %s | %s | %s | %s | %s |" % (pin, typ, r[1], verdict, at))
    lines += ["", "Compared %d rows; membership-only %d" % (compared, membership)]
    return lines, bad


def check(tsv, pin, srcdir, commit, head=None):
    """The report of one pin and the number of MISMATCH, missing and CONFLICT verdicts in it (at an older pin `head`
    is the A-HEAD source directory of the body comparison)."""
    src = Source(srcdir)
    names = pin_types(src)
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
            verdict, note = verify_row(src, r, ids, names), ""
            if isinstance(verdict, tuple):
                verdict, note = verdict
            if verdict.startswith("MISMATCH"):
                bad += 1
            out.append("| %s | %s | %s | %s | %s | %s |" % (r[0], r[1], r[2], verdict, r[5], note))
    if not own:
        out.append("| - | - | - | no row is pinned here | - | - |")
    out.append("")
    if pin == "A-HEAD":
        out += ["Completeness: every ID the switch of each type reads at this pin is a row.", "",
                "| Type | IDs the switch reads | Rows pinned here | Verdict |", "|---|---|---|---|"]
        for typ, _, _ in SELECTION:
            ids = {cid: (ln, c) for cid, ln, c in switches[typ]}
            absent = sorted(cid for cid in ids if cid not in by_id or by_id[cid][0] != typ)
            for cid in absent:
                bad += 1
                out.append("| %s | 0x%08x | %s | missing |" % (typ, cid, ids[cid][1].name))
            out.append("| %s | %d | %d | %s |" % (typ, len(ids), len([r for r in own if r[0] == typ]),
                                                  "complete" if not absent else "incomplete"))
        # design D3: every type a full or prefix row names is a type of the table
        out += ["", "Closure: every object type a `full` or `prefix` row names is a type of the table (design D3).", ""]
        gaps = closure(table)
        for typ, cls in gaps:
            bad += 1
            out.append("- missing (type %s named by %s)" % (typ, cls))
        if not gaps:
            out.append("Closed: %d types, none named outside them." % len(SELECTION))
    else:
        out += ["Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv`.", "",
                "| Pin | Type | ID | Verdict | Class at this pin |", "|---|---|---|---|---|"]
        for typ, _, _ in SELECTION:
            if src.switch(typ) is None:
                out.append("| %s | %s | - | type not defined at this pin | - |" % (pin, typ))
                continue
            for cid, ln, c in sorted(switches[typ], key=lambda e: e[0]):
                ok = cid in by_id and by_id[cid][0] == typ
                if not ok:
                    bad += 1
                out.append("| %s | %s | 0x%08x | %s | %s (%s:%d) |" % (
                    pin, typ, cid, "in-table" if ok else "missing", c.name, c.fname, c.line))
        if head is not None:
            lines, nbad = body_check(table, src, Source(head), pin)
            out += [""] + lines
            bad += nbad
    out.append("")
    return "\n".join(out) + "\n", bad


def hand(schemas, notes):
    """4I4b-P7: write the hand-read section of `notes` (its first line is the heading) into CROSSCHECK.md, replacing the
    section under the same heading (up to the next heading of its level or above), else appending it last."""
    with open(notes, encoding="utf-8") as fh:
        text = fh.read().rstrip("\n")
    heading = text.split("\n", 1)[0]
    level = len(heading) - len(heading.lstrip("#"))
    if level == 0 or not heading.startswith("#" * level + " "):
        raise SystemExit("%s: the first line is not a heading" % notes)
    path = os.path.join(schemas, "CROSSCHECK.md")
    with open(path, encoding="utf-8") as fh:
        lines = fh.read().rstrip("\n").split("\n")
    section = text.split("\n")
    if heading in lines:
        i = lines.index(heading)
        j = i + 1
        while j < len(lines) and not re.match(r"^#{1,%d} " % level, lines[j]):
            j += 1
        new = lines[:i] + section + ([""] if j < len(lines) else []) + lines[j:]
    else:
        new = lines + [""] + section
    with open(path, "w", encoding="utf-8", newline="\n") as fh:
        fh.write("\n".join(new).rstrip("\n") + "\n")


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


GO_TYPE_CONST = {
    "Message": "typeMessage", "Peer": "typePeer", "MessageFwdHeader": "typeFwd", "MessageReplyHeader": "typeReply",
    "MessageEntity": "typeEntity", "MessageAction": "typeAction", "PhoneCallDiscardReason": "typeReason",
    "MessageMedia": "typeMedia", "User": "typeUser", "Chat": "typeChat",
    "MessageReplies": "typeReplies",
}
GO_BODY = {"full": "tl.BodyFull", "prefix": "tl.BodyPrefix", "opaque": "tl.BodyOpaque"}
GO_SCALAR = {"i32": "KInt32", "i64": "KInt64", "dbl": "KDouble", "bool": "KBool", "bytes": "KBytes", "str": "KString",
             "secret": "KSecret", "#": "KFlags"}
RE_GO_GATE = re.compile(r"^\[(F2?)\.([0-9]+):$")
RE_GO_FIELD = re.compile(r"^(" + NAME + r")(?:=(F2?)\.([0-9]+)|:(.+?))(\]?)$")


def go_type(name):
    return GO_TYPE_CONST.get(name) or '"%s"' % name


def go_field(name, kind, of="", gate=None, cls=""):
    """One tl.Field literal; gate is (flags word name, bit) or None."""
    parts = ['Name: "%s"' % name, "Kind: tl.%s" % kind]
    if of:
        parts.append("Of: %s" % go_type(of))
    if gate:
        parts.append('Flags: "%s"' % gate[0])
        parts.append("Bit: %d" % gate[1])
    if cls:
        parts.append('Class: "%s"' % cls)
    return "{" + ", ".join(parts) + "},"


def go_fields(row_id, text):
    """The tl.Field literals of one fields column (grammar of 4I4-P4)."""
    out, gate = [], None
    for tok in text.split():
        m = RE_GO_GATE.match(tok)
        if m:
            if gate:
                raise SystemExit("%s: a gate opens inside a gate at %r" % (row_id, tok))
            gate = ("flags2" if m.group(1) == "F2" else "flags", int(m.group(2)))
            continue
        m = RE_GO_FIELD.match(tok)
        if not m:
            raise SystemExit("%s: token %r is outside the 4I4-P4 grammar" % (row_id, tok))
        name, tword, tbit, typ, close = m.groups()
        if tword:
            out.append(go_field(name, "KTrue", gate=("flags2" if tword == "F2" else "flags", int(tbit))))
        elif typ in GO_SCALAR:
            out.append(go_field(name, GO_SCALAR[typ], gate=gate))
        elif re.fullmatch(r"id(32|64)@(user|chat|channel|unknown)", typ):
            out.append(go_field(name, "KID" + typ[2:4], gate=gate, cls=typ.split("@")[1]))
        elif typ == "vec<i32>":
            out.append(go_field(name, "KVecInt32", gate=gate))
        elif typ == "vec<i64>":
            out.append(go_field(name, "KVecInt64", gate=gate))
        elif typ.startswith("vec<") and typ.endswith(">"):
            out.append(go_field(name, "KVector", of=typ[4:-1], gate=gate))
        else:
            out.append(go_field(name, "KObject", of=typ, gate=gate))
        if close:
            if not gate:
                raise SystemExit("%s: %r closes a gate that is not open" % (row_id, tok))
            gate = None
    if gate:
        raise SystemExit("%s: a gate is never closed" % row_id)
    return out


def go_table(rows):
    """The text of tables.go for the rows of a constructors.tsv."""
    seen = set()
    out = ["// Code generated by tools/fixtures/telegram_tl_facts.py --go; DO NOT EDIT.", "",
           "package androidtelegram", "",
           'import "github.com/rbenzing/minutiae/internal/decode/tl"', "",
           "// The TL types the parser names (plan 4I4-P5).", "const ("]
    width = max(len(c) for c in GO_TYPE_CONST.values())  # gofmt aligns the "=" of a const block
    for typ, const in GO_TYPE_CONST.items():
        out.append('\t%s = "%s"' % (const.ljust(width), typ))
    out += [")", "",
            "// newTable returns the Telegram Android constructor table: one entry per row of constructors.tsv, each",
            "// citing its source line at the pin that reads it (plan 4I4 spec 8.2). It is a function, not a package",
            "// variable: a parser holds no package-level state (the purity rule P5), so each Parse builds its own.",
            "func newTable() tl.Table {", "\treturn tl.Table{"]
    for r in sorted(rows, key=lambda r: int(r[1], 16)):
        typ, cid, name, body, pin, source, fields = r
        if cid in seen or body not in GO_BODY:
            raise SystemExit("%s: duplicate id or unknown body %r" % (cid, body))
        seen.add(cid)
        fl = go_fields(cid, fields)
        # gofmt aligns the values of a run of one-line keys: ID, Name, Type and Body, joined by Source when no Fields
        # block ends the run
        w = 4 if fl else 6
        ent = ["%s: {" % cid] + ["\t%s %s," % ((k + ":").ljust(w + 1), v) for k, v in (
            ("ID", cid), ("Name", '"%s"' % name), ("Type", go_type(typ)), ("Body", GO_BODY[body]))]
        if fl:
            ent.append("\tFields: []tl.Field{")
            ent += ["\t\t" + f for f in fl]
            ent.append("\t},")
        ent += ['\tSource: "%s@%s",' % (source, pin), "},"]  # "Source:" is the longest key: never padded
        out += ["\t\t" + e for e in ent]
    out += ["\t}", "}", ""]
    return "\n".join(out)


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
        head = take(args, "--head")
        # 4I4b-P7: an older pin is checked with the A-HEAD sources beside it (the body comparison)
        if len(args) != 2 or pin not in PINS or (pin != "A-HEAD") != (head is not None):
            raise SystemExit(__doc__)
        report, bad = check(args[0], pin, args[1], commit, head)
        if report_path:
            with open(report_path, "w", encoding="utf-8", newline="\n") as fh:
                fh.write(report)
        else:
            sys.stdout.write(report)
        sys.stderr.write("%s: %d MISMATCH, missing or CONFLICT\n" % (pin, bad))
        return 1 if bad else 0
    if args and args[0] == "--hand" and len(args) == 3:
        hand(args[1], args[2])
        return 0
    if args and args[0] == "--go" and len(args) == 3:
        text = go_table(load_tsv(args[2]))
        with open(args[1], "w", encoding="utf-8", newline="\n") as fh:
            fh.write(text)
        return 0
    if args and args[0] == "--assemble" and len(args) >= 3:
        rest = args[2:]
        ddl = rest[rest.index("--ddl") + 1:] if "--ddl" in rest else []
        assemble(args[1], rest[:rest.index("--ddl")] if "--ddl" in rest else rest, ddl)
        return 0
    raise SystemExit(__doc__)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

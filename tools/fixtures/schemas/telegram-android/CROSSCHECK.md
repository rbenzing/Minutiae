# TL facts cross-check (plan 4I4)

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

Plan 4I4c (Task 2, fetched again 2026-11-27 the same way, every file with the SHA-256 the per-pin tables below give):
a row is identified by (type, ID) (design D4). `0x77608b83` is the one ID that is a row of two types,
`TL_keyboard.KeyboardButtonRow` (`TL_keyboard.java:749`) and `TL_keyboard.KeyboardInlineButtonRow` (`:645`), and every
check (the completeness, the membership, the pin conflicts) is per (type, ID). The table adds reply markup, polls,
InputUser and their closure (629 rows). The types of `tgnet/tl/TL_keyboard.java` are spelled `TL_keyboard.<Name>`, inside
that file too (design D2), and its `ButtonType` and `InlineButtonType` extend the class `ButtonTypeProto`
(`TL_keyboard.java:18`, ruling T2b). An ID a type tests ahead of its switch is read as a case label is (`0x11c1a322`
`TL_keyboardInlineButton`, `TL_keyboard.java:671`, ruling T2a). Each A-HEAD button reader tries the other button family
first (`TL_keyboard.java:561-575`, `:735-742`, `:815-822`), so `0x77608b83` under both types and `0x19420af6` read a union
`vec<A|B>` in the app's order; the reduction leaves them to the builder, like the classed id vectors (`vec<id32@user>`,
`vec<id64@user>`, `VectorLegacy.java:28`, `:49`) of three PollResults rows, and the six are read by hand in their own
section at the end of this file. The client-to-server forms stay opaque (design §10). At an older pin an inline button
its single `KeyboardButton` switch reads is `in-table through the union`: A-HEAD reads it as
`TL_keyboard.KeyboardInlineButton` wherever it reads a button, so no older ID is narrowed. `0x48a30254`
TL_replyInlineMarkup_layer228 is no longer a pin conflict (the 4I4b statement above holds for 4I4b only): each older
pin's report ends with the union check (every ID of that switch is a row of one of the two button types that reads the
same bytes there and at A-HEAD; a client-to-server row stops the decode, it never misreads), so its older reading and
that of `0x77608b83` are a compatible widening (union) (Q5, ruling T1c applied to a union). Two adjacent gates of one
flag bit compare as the one gate that holds both (ruling T1a), and a classed id vector compares as the bare vector it
is on the wire.

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

# Constructor rows, per pin (`telegram_tl_facts.py --check`)

## A-HEAD (21f7d109da6ab3506c9e9577f98c6a38771ab335)

Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):

| File | SHA-256 |
|---|---|
| `messenger/DatabaseMigrationHelper.java` | fd2f2d033e9049f306f4874433070511b3d72827e72148a659b15d7be13edc2a |
| `messenger/DialogObject.java` | 8cc4fe65fa873ed97d9de2679a05ed3d2334726b43c57c7d82e35583d4af7120 |
| `messenger/LocaleController.java` | c9c52628e5427ff80c61c27fa31117beb52b1c7b8ea670f93233b5adad4e015e |
| `messenger/MessageObject.java` | 16849a4953b6a17d9fff53217be7b862012070d7505d702acf3b88da67f5f2be |
| `messenger/MessagesController.java` | 5eff04e1a9c1a9fc9de512c56c9c5ce0c99dba9b8fa5a10dde9b99c2ce6282e8 |
| `messenger/MessagesStorage.java` | ba5897a1a30c0add954ea36cf07b65f653192de464822a14ba43e97a559b4603 |
| `messenger/SecretChatHelper.java` | f2b7d2483551f2eb9a0d0c48f70e5974c13cbda13e3cb6c17e2909c93e485507 |
| `tgnet/NativeByteBuffer.java` | 5a35a9296c0e601bb2945d250821e3b0f38cf980b2608472608dbad288fe9f12 |
| `tgnet/SerializedData.java` | ab731c185eef0d24af92b2492331fbb8c78f34226c3b71483b9ca1a16bb0dd9b |
| `tgnet/TLObject.java` | e04c8fa25f8043d5a4d6caa5aa07cb56241f7e9d3b2af2433f27e9d90b771878 |
| `tgnet/TLRPC.java` | 37a1b1ee83d1f84352f90ac65e8d2176a863678f60c902010c3e1c0b1beea86b |
| `tgnet/Vector.java` | b587a58fa7525d062d1602e3289bed35d3d41e8b50e23116ca7d72b9fa99d824 |
| `tgnet/VectorLegacy.java` | ddb561e3fd975d74306f718f94e00f11be984e1fe87d0252cf08edcfcaefac30 |
| `tgnet/tl/TL_account.java` | 44e00cd1251d7332bbc378d2a5da71145c84906c5021715c34d7125d7707fc91 |
| `tgnet/tl/TL_aicompose.java` | 48c51e6aedcbc88dbcc9c58e208eda3726362b6bfd99dc94ee2491bd9ad616a2 |
| `tgnet/tl/TL_bots.java` | cdbed470d6543f3adbfdf2c85e563a98f633e7f449a016809fb9616632b6cdda |
| `tgnet/tl/TL_chatlists.java` | 7199c32d0a2f384fbfa7a27b5be789057d5db85a108cfbdf2dc403f6eb04454d |
| `tgnet/tl/TL_communities.java` | fc89e6760c0f2829e0e6e7217237d966d6d9eacd607baac6023b4bef01df4ef9 |
| `tgnet/tl/TL_ephemeral.java` | 4cc13fb3f8298b677ba39a3ed08ddf1aeb476be1dc41202ba5f949c803226a5f |
| `tgnet/tl/TL_forum.java` | f45162698c6f9d0f7ee3a5c76f1d9897fe987b889f636a1a0b833a1791a512d3 |
| `tgnet/tl/TL_fragment.java` | 0802138ecc6bd8eaf80ae5a6aed5ae7013ae260864db296e3e51ef46ba004f42 |
| `tgnet/tl/TL_iv.java` | c11381124515abc20cd6bbcc8c00fe62f4b2607d0e0c35207e3c0162b87d46ae |
| `tgnet/tl/TL_keyboard.java` | a026f2b766ea4ca29c9abee0a4010773d4c7f38b848939b17eeee1068388b6bc |
| `tgnet/tl/TL_payments.java` | aab40abfe31d6f243ee46613e839107bbfc4db67a0ee5ce91ada88feb940a4f7 |
| `tgnet/tl/TL_phone.java` | 791ee4fe5c45f5a0cd6bdbc5adae4cd557eb93a6d4f444284b300fee3d01d1cc |
| `tgnet/tl/TL_stars.java` | caaa137cc50e22ac62ba3fa2f7162cecdf8f1e5436c2f13c56145adefc264c02 |
| `tgnet/tl/TL_stats.java` | 7fdf0ec100ba3eb3447b2fbcd8d2d20304ec7e98dcb5791b675d3b0382724253 |
| `tgnet/tl/TL_stories.java` | b53315b2118c4ce4790ba332c958d739f79ff1d62780c3bd54122f976790a460 |
| `tgnet/tl/TL_toncenter.java` | 9a7b2554bbe4a1c505a2e7b22587cc2485234b7452baad43fb3f1daf2c29893c |
| `tgnet/tl/TL_update.java` | 09ac654a11fbba8e38b88d988a94b5f243bf70edc63df916b0482723366f9817 |
| `tgnet/tl/TL_wallet.java` | 82d7d71ccde1aaea2775ae77f9d583c1913dd1872c9acf2d5c2b179b38bc810b |
| `tgnet/tl/legacy/TL_legacy_message.java` | 8bcf9fbf58ef78fbb13f22395d1790e697605a787621f21a59b222be8adad43e |
| `ui/Cells/ChatMessageCell.java` | b6e36ca20329379c8d8be3a41a88a3c3940bb675ffc0893d502353113af44580 |
| `ui/Stories/MessageMediaStoryFull.java` | 0691a6ddb1639e2880038bf0377721e20644a6e539c8746a52b673353e289faf |
| `ui/Stories/MessageMediaStoryFull_old.java` | d83f99d50d7a5d2ebc8d90dec1bd23bc4618816b00a33fec6567b8c8f248d093 |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | ba9229aa405b9f9e2cb31698dfa46f667f21a7a2280e07fa64abe68f45037b39 |
| `facts.tsv` | dca5975e28eb17b1009b412ef44c14b9f0fc38864749dea18f767723136e6479 |

Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's switch, the class, the source line, the body the selection asks for, and the field order reduced from `readParams`; a `prefix` row is a proper prefix of the reduced body):

| Type | ID | Class | Verdict | Source | Note |
|---|---|---|---|---|---|
| Message | 0x05f46804 | TL_messageForwarded_old | verified | TLRPC.java:61033 |  |
| Message | 0x1d86f70e | TL_messageService_old2 | verified | TLRPC.java:59717 |  |
| Message | 0x1e4c8a69 | TL_message_layer175 | verified | TL_legacy_message.java:2222 |  |
| Message | 0x22eb6aba | TL_message_old | verified | TLRPC.java:61073 |  |
| Message | 0x2357bf25 | TL_message_layer179 | verified | TL_legacy_message.java:1886 |  |
| Message | 0x286fa604 | TL_messageService_layer123 | verified | TLRPC.java:61265 |  |
| Message | 0x2b085862 | TL_messageService_layer195 | verified | TLRPC.java:61435 |  |
| Message | 0x2bebfa86 | TL_message_old6 | verified | TLRPC.java:60661 |  |
| Message | 0x38116ee0 | TL_message_layer169 | verified | TL_legacy_message.java:2530 |  |
| Message | 0x3ae56482 | TL_message_layer224 | verified | TL_legacy_message.java:251 |  |
| Message | 0x44f9b43d | TL_message_layer104 | verified | TLRPC.java:60550 |  |
| Message | 0x452c0e65 | TL_message_layer117 | verified | TLRPC.java:60431 |  |
| Message | 0x555555f8 | TL_message_secret_old | verified | TLRPC.java:61224 |  |
| Message | 0x555555f9 | TL_message_secret_layer72 | verified | TLRPC.java:61168 |  |
| Message | 0x555555fa | TL_message_secret | verified | TLRPC.java:61106 |  |
| Message | 0x567699b3 | TL_message_old2 | verified | TLRPC.java:60968 |  |
| Message | 0x58ae39c9 | TL_message_layer123 | verified | TL_legacy_message.java:2947 |  |
| Message | 0x5ba66c13 | TL_message_old7 | verified | TLRPC.java:60050 |  |
| Message | 0x7600b9d3 | TL_message | verified | TLRPC.java:60187 |  |
| Message | 0x76bec211 | TL_message_layer173 | verified | TL_legacy_message.java:2379 |  |
| Message | 0x7a800e0a | TL_messageService | verified | TLRPC.java:61311 |  |
| Message | 0x83e5de54 | TL_messageEmpty_layer122 | verified | TLRPC.java:59680 |  |
| Message | 0x85d6cbe2 | TL_message_layer135 | verified | TL_legacy_message.java:2675 |  |
| Message | 0x90a6ca84 | TL_messageEmpty | verified | TLRPC.java:59694 |  |
| Message | 0x90dddc11 | TL_message_layer72 | verified | TLRPC.java:59750 |  |
| Message | 0x94345242 | TL_message_layer195 | verified | TL_legacy_message.java:1520 |  |
| Message | 0x95ef6f2b | TL_message_layer226 | verified | TL_legacy_message.java:15 |  |
| Message | 0x96fdbbe9 | TL_message_layer199 | verified | TL_legacy_message.java:1327 |  |
| Message | 0x9815cec8 | TL_message_layer216 | verified | TL_legacy_message.java:918 |  |
| Message | 0x9cb490e9 | TL_message_layer222 | verified | TL_legacy_message.java:480 |  |
| Message | 0x9e19a1f6 | TL_messageService_layer118 | verified | TLRPC.java:61486 |  |
| Message | 0x9f8d60bb | TL_messageService_old | verified | TLRPC.java:61006 |  |
| Message | 0xa367e716 | TL_messageForwarded_old2 | verified | TLRPC.java:60142 |  |
| Message | 0xa4e97f37 | TL_message_layer179 | verified | TLRPC.java:59522 |  |
| Message | 0xa66c7efc | TL_message_layer176 | verified | TL_legacy_message.java:2059 |  |
| Message | 0xa7ab1991 | TL_message_old3 | verified | TLRPC.java:60911 |  |
| Message | 0xb92f76cf | TL_message_layer220 | verified | TL_legacy_message.java:702 |  |
| Message | 0xbce383d2 | TL_message_layer131 | verified | TL_legacy_message.java:2812 |  |
| Message | 0xbde09c2e | TL_message_layer180 | verified | TL_legacy_message.java:1707 |  |
| Message | 0xc06b9607 | TL_messageService_layer48 | verified | TLRPC.java:60805 |  |
| Message | 0xc09be45f | TL_message_layer68 | verified | TLRPC.java:59851 |  |
| Message | 0xc3060325 | TL_message_old4 | verified | TLRPC.java:60848 |  |
| Message | 0xc992e15c | TL_message_layer47 | verified | TLRPC.java:59952 |  |
| Message | 0xd3d28540 | TL_messageService_layer204 | verified | TLRPC.java:61376 |  |
| Message | 0xeabcdd4d | TL_message_layer205 | verified | TL_legacy_message.java:1128 |  |
| Message | 0xf07814c8 | TL_message_old5 | verified | TLRPC.java:60736 |  |
| Message | 0xf52e6b7f | TL_message_layer118 | verified | TL_legacy_message.java:3076 |  |
| Peer | 0x36c6019a | TL_peerChat | verified | TLRPC.java:3902 |  |
| Peer | 0x59511722 | TL_peerUser | verified | TLRPC.java:3876 |  |
| Peer | 0x9db1bc6d | TL_peerUser_layer131 | verified | TLRPC.java:3915 |  |
| Peer | 0xa2a5371e | TL_peerChannel | verified | TLRPC.java:3889 |  |
| Peer | 0xbad0e5bb | TL_peerChat_layer131 | verified | TLRPC.java:3928 |  |
| Peer | 0xbddde532 | TL_peerChannel_layer131 | verified | TLRPC.java:3863 |  |
| MessageFwdHeader | 0x353a686b | TL_messageFwdHeader_layer118 | verified | TLRPC.java:44001 |  |
| MessageFwdHeader | 0x4e4df4bb | TL_messageFwdHeader | verified | TLRPC.java:43863 |  |
| MessageFwdHeader | 0x559ebe6d | TL_messageFwdHeader_layer96 | verified | TLRPC.java:44166 |  |
| MessageFwdHeader | 0x5f777dce | TL_messageFwdHeader_layer169 | verified | TLRPC.java:43942 |  |
| MessageFwdHeader | 0xc786ddcb | TL_messageFwdHeader_layer68 | MANUAL, read by hand at TLRPC.java:43828 | TLRPC.java:43828 | the class of an inline id (T5) |
| MessageFwdHeader | 0xec338270 | TL_messageFwdHeader_layer112 | verified | TLRPC.java:44066 |  |
| MessageFwdHeader | 0xfadff4ac | TL_messageFwdHeader_layer72 | verified | TLRPC.java:44125 |  |
| MessageReplyHeader | 0x0e5af939 | TL_messageReplyStoryHeader | verified | TLRPC.java:28186 |  |
| MessageReplyHeader | 0x1b97dd66 | TL_messageReplyHeader_layer231 | verified | TLRPC.java:28290 |  |
| MessageReplyHeader | 0x3d5c1693 | TL_messageReplyHeader_layer165_2 | verified | TLRPC.java:28572 |  |
| MessageReplyHeader | 0x6917560b | TL_messageReplyHeader_layer223 | verified | TLRPC.java:28371 |  |
| MessageReplyHeader | 0x6eebcabd | TL_messageReplyHeader_layer166 | verified | TLRPC.java:28511 |  |
| MessageReplyHeader | 0x9c98bfc1 | TL_messageReplyStoryHeader_layer174 | verified | TLRPC.java:28170 |  |
| MessageReplyHeader | 0xa6d57763 | TL_messageReplyHeader_layer165 | verified | TLRPC.java:28625 |  |
| MessageReplyHeader | 0xafbc09db | TL_messageReplyHeader_layer207 | verified | TLRPC.java:28444 |  |
| MessageReplyHeader | 0xddb5ab75 | TL_messageReplyHeader | verified | TLRPC.java:28201 |  |
| MessageEntity | 0x020df5d0 | TL_messageEntityBlockquote_layer180 | verified | TLRPC.java:32950 |  |
| MessageEntity | 0x0652c1c5 | TL_messageEntityDiffDelete | verified | TLRPC.java:63923 |  |
| MessageEntity | 0x208e68c9 | TL_inputMessageEntityMentionName | verified | TLRPC.java:32751 |  |
| MessageEntity | 0x28a20571 | TL_messageEntityCode | verified | TLRPC.java:32862 |  |
| MessageEntity | 0x32ca960f | TL_messageEntitySpoiler | verified | TLRPC.java:32707 |  |
| MessageEntity | 0x352dca58 | TL_messageEntityMentionName_layer131 | verified | TLRPC.java:32734 |  |
| MessageEntity | 0x4c4e743f | TL_messageEntityCashtag | verified | TLRPC.java:32781 |  |
| MessageEntity | 0x64e475c2 | TL_messageEntityEmail | verified | TLRPC.java:32540 |  |
| MessageEntity | 0x6cef8ac7 | TL_messageEntityBotCommand | verified | TLRPC.java:32513 |  |
| MessageEntity | 0x6ed02538 | TL_messageEntityUrl | verified | TLRPC.java:32625 |  |
| MessageEntity | 0x6f635b0d | TL_messageEntityHashtag | verified | TLRPC.java:32835 |  |
| MessageEntity | 0x71777116 | TL_messageEntityDiffInsert | verified | TLRPC.java:63859 |  |
| MessageEntity | 0x73924be0 | TL_messageEntityPre | verified | TLRPC.java:32567 |  |
| MessageEntity | 0x761e6af4 | TL_messageEntityBankCard | verified | TLRPC.java:32992 |  |
| MessageEntity | 0x76a6d327 | TL_messageEntityTextUrl | verified | TLRPC.java:32482 |  |
| MessageEntity | 0x826f8b60 | TL_messageEntityItalic | verified | TLRPC.java:32653 |  |
| MessageEntity | 0x904ac7c7 | TL_messageEntityFormattedDate | MANUAL, read by hand at TLRPC.java:63820 | TLRPC.java:63794 | a call of a method the reduction does not follow |
| MessageEntity | 0x9b69e34b | TL_messageEntityPhone | verified | TLRPC.java:33046 |  |
| MessageEntity | 0x9c4e7e8b | TL_messageEntityUnderline | verified | TLRPC.java:32965 |  |
| MessageEntity | 0xbb92ba95 | TL_messageEntityUnknown | verified | TLRPC.java:32598 |  |
| MessageEntity | 0xbd610bc9 | TL_messageEntityBold | verified | TLRPC.java:32808 |  |
| MessageEntity | 0xbf0693d4 | TL_messageEntityStrike | verified | TLRPC.java:32889 |  |
| MessageEntity | 0xc6c1e5a7 | TL_messageEntityDiffReplace | verified | TLRPC.java:63888 |  |
| MessageEntity | 0xc8cf05f8 | TL_messageEntityCustomEmoji | verified | TLRPC.java:63760 |  |
| MessageEntity | 0xdc7b1140 | TL_messageEntityMentionName | verified | TLRPC.java:33073 |  |
| MessageEntity | 0xf1ccaaac | TL_messageEntityBlockquote | verified | TLRPC.java:32916 |  |
| MessageEntity | 0xfa04579d | TL_messageEntityMention | verified | TLRPC.java:32680 |  |
| MessageEntity | 0xfdabbfcd | TL_messageEntityTonAddress | verified | TLRPC.java:33019 |  |
| MessageAction | 0x01baa035 | TL_messageActionPhoneNumberRequest | verified | TLRPC.java:25763 |  |
| MessageAction | 0x031224c3 | TL_messageActionChatJoinedByLink | verified | TLRPC.java:25877 |  |
| MessageAction | 0x08557637 | TL_messageActionStarGift_layer195 | verified | TLRPC.java:67803 |  |
| MessageAction | 0x0d999256 | TL_messageActionTopicCreate | verified | TLRPC.java:26221 |  |
| MessageAction | 0x15cefd00 | TL_messageActionChatAddUser | verified | TLRPC.java:25691 |  |
| MessageAction | 0x16605e3e | TL_messageActionManagedBotCreated | verified | TLRPC.java:64899 |  |
| MessageAction | 0x26077b99 | TL_messageActionStarGiftUnique_layer197 | verified | TLRPC.java:68543 |  |
| MessageAction | 0x2a9fadc5 | TL_messageActionGiveawayResults_layer186 | verified | TLRPC.java:67301 |  |
| MessageAction | 0x2c8f2a25 | TL_messageActionSuggestBirthday | verified | TLRPC.java:25746 |  |
| MessageAction | 0x2e3ae60e | TL_messageActionStarGiftUnique_layer210 | verified | TLRPC.java:68425 |  |
| MessageAction | 0x2ffe2f7a | TL_messageActionConferenceCall | verified | TLRPC.java:67873 |  |
| MessageAction | 0x31518e9b | TL_messageActionRequestedPeer | verified | TLRPC.java:64991 |  |
| MessageAction | 0x31c48347 | TL_messageActionGiftCode | verified | TLRPC.java:65827 |  |
| MessageAction | 0x332ba9ed | TL_messageActionGiveawayLaunch_layer186 | verified | TLRPC.java:67263 |  |
| MessageAction | 0x34f762f3 | TL_messageActionStarGiftUnique_layer214 | verified | TLRPC.java:68354 |  |
| MessageAction | 0x399674dc | TL_messageActionPollDeleteAnswer | verified | TLRPC.java:25335 |  |
| MessageAction | 0x3c134d7b | TL_messageActionSetMessagesTTL | verified | TLRPC.java:26077 |  |
| MessageAction | 0x3e2793ba | TL_messageActionNoForwardsRequest | verified | TLRPC.java:64876 |  |
| MessageAction | 0x40699cd0 | TL_messageActionPaymentSent_layer140 | verified | TLRPC.java:26320 |  |
| MessageAction | 0x41b3e202 | TL_messageActionPaymentRefunded | verified | TLRPC.java:25380 |  |
| MessageAction | 0x45d5b021 | TL_messageActionGiftStars | verified | TLRPC.java:25409 |  |
| MessageAction | 0x4717e8a4 | TL_messageActionStarGift_layer211 | verified | TLRPC.java:67679 |  |
| MessageAction | 0x4792929b | TL_messageActionScreenshotTaken | verified | TLRPC.java:25738 |  |
| MessageAction | 0x47dd8079 | TL_messageActionWebViewDataSentMe | verified | TLRPC.java:25954 |  |
| MessageAction | 0x488a7337 | TL_messageActionChatAddUser_layer131 | verified | TLRPC.java:25905 |  |
| MessageAction | 0x48e91302 | TL_messageActionGiftPremium | verified | TLRPC.java:26407 |  |
| MessageAction | 0x4a8bfe80 | TL_messageActionChatJoinedViaCommunity | verified | TLRPC.java:65009 |  |
| MessageAction | 0x502f92f7 | TL_messageActionInviteToGroupCall | verified | TLRPC.java:25561 |  |
| MessageAction | 0x5060a3f4 | TL_messageActionSetChatWallPaper | verified | TLRPC.java:26000 |  |
| MessageAction | 0x51bdb021 | TL_messageActionChatMigrateTo_layer131 | verified | TLRPC.java:25704 |  |
| MessageAction | 0x55555550 | TL_messageActionUserJoined | verified | TLRPC.java:26022 |  |
| MessageAction | 0x55555551 | TL_messageActionUserUpdatedPhoto | verified | TLRPC.java:26030 |  |
| MessageAction | 0x55555552 | TL_messageActionTTLChange | verified | TLRPC.java:26064 |  |
| MessageAction | 0x55555557 | TL_messageActionCreatedBroadcastList | verified | TLRPC.java:25931 |  |
| MessageAction | 0x555555f5 | TL_messageActionLoginUnknownLocation | verified | TLRPC.java:25305 |  |
| MessageAction | 0x555555f7 | TL_messageEncryptedAction | verified | TLRPC.java:25505 |  |
| MessageAction | 0x56d03994 | TL_messageActionGiftCode_layer216 | verified | TLRPC.java:65883 |  |
| MessageAction | 0x57de635e | TL_messageActionSuggestProfilePhoto | verified | TLRPC.java:64845 |  |
| MessageAction | 0x5d20bae8 | TL_messageActionChangeCommunity | verified | TLRPC.java:68322 |  |
| MessageAction | 0x5e3cfc4b | TL_messageActionChatAddUser_old | verified | TLRPC.java:26051 |  |
| MessageAction | 0x678c2e09 | TL_messageActionGiftCode_layer189 | verified | TLRPC.java:65933 |  |
| MessageAction | 0x69b8c4bf | TL_messageActionWalletTonConnectRequest | verified | TLRPC.java:26505 |  |
| MessageAction | 0x69f916f8 | TL_messageActionSuggestedPostRefund | verified | TLRPC.java:25364 |  |
| MessageAction | 0x6c6274fa | TL_messageActionGiftPremium_layer216 | verified | TLRPC.java:26443 |  |
| MessageAction | 0x73ada76b | TL_messageActionStarGiftPurchaseOfferDeclined | verified | TLRPC.java:68296 |  |
| MessageAction | 0x76b9f11a | TL_messageActionInviteToGroupCall_layer131 | verified | TLRPC.java:25939 |  |
| MessageAction | 0x774278d4 | TL_messageActionStarGiftPurchaseOffer | verified | TLRPC.java:68264 |  |
| MessageAction | 0x7a0d7f42 | TL_messageActionGroupCall | verified | TLRPC.java:25591 |  |
| MessageAction | 0x7e1c1187 | TL_messageActionStarGiftUnique | verified | TLRPC.java:67972 |  |
| MessageAction | 0x7fcb13a8 | TL_messageActionChatEditPhoto | verified | TLRPC.java:25725 |  |
| MessageAction | 0x80e11a7f | TL_messageActionPhoneCall | verified | TLRPC.java:26192 |  |
| MessageAction | 0x84b88578 | TL_messageActionPaidMessagesPrice | verified | TLRPC.java:67837 |  |
| MessageAction | 0x87e2f155 | TL_messageActionGiveawayResults | verified | TLRPC.java:67276 |  |
| MessageAction | 0x8f31b327 | TL_messageActionPaymentSentMe_layer193 | verified | TLRPC.java:26621 |  |
| MessageAction | 0x92a72876 | TL_messageActionGameScore | verified | TLRPC.java:26392 |  |
| MessageAction | 0x9329ea33 | TL_messageActionGramTransfer | verified | TLRPC.java:65024 |  |
| MessageAction | 0x94bd38ed | TL_messageActionPinMessage | verified | TLRPC.java:26155 |  |
| MessageAction | 0x95728543 | TL_messageActionStarGiftUnique_layer221 | verified | TLRPC.java:68183 |  |
| MessageAction | 0x95d2ac92 | TL_messageActionChannelCreate | verified | TLRPC.java:26127 |  |
| MessageAction | 0x95ddcf69 | TL_messageActionSuggestedPostSuccess | verified | TLRPC.java:25350 |  |
| MessageAction | 0x95e3fbef | TL_messageActionChatDeletePhoto | verified | TLRPC.java:26184 |  |
| MessageAction | 0x96163f56 | TL_messageActionPaymentSent_layer193 | verified | TLRPC.java:26293 |  |
| MessageAction | 0x98e0d697 | TL_messageActionGeoProximityReached | verified | TLRPC.java:26163 |  |
| MessageAction | 0x9bb3ef44 | TL_messageActionStarGift_layer192 | verified | TLRPC.java:67349 |  |
| MessageAction | 0x9da1cd6c | TL_messageActionPollAppendAnswer | verified | TLRPC.java:25320 |  |
| MessageAction | 0x9fbab604 | TL_messageActionHistoryClear | verified | TLRPC.java:25717 |  |
| MessageAction | 0xa43f30cc | TL_messageActionChatDeleteUser | verified | TLRPC.java:25663 |  |
| MessageAction | 0xa6638b9a | TL_messageActionChatCreate_layer131 | verified | TLRPC.java:25546 |  |
| MessageAction | 0xa80f51e4 | TL_messageActionGiveawayLaunch | verified | TLRPC.java:67240 |  |
| MessageAction | 0xa8a3c699 | TL_messageActionGiftTon | verified | TLRPC.java:25476 |  |
| MessageAction | 0xaa1afbfd | TL_messageActionSetMessagesTTL_layer149 | verified | TLRPC.java:26101 |  |
| MessageAction | 0xaa786345 | TL_messageActionSetChatTheme_layer213 | verified | TLRPC.java:25612 |  |
| MessageAction | 0xabe9affe | TL_messageActionBotAllowed_layer153 | verified | TLRPC.java:26369 |  |
| MessageAction | 0xac1f1fcd | TL_messageActionPaidMessagesRefunded | verified | TLRPC.java:67952 |  |
| MessageAction | 0xacdfcb81 | TL_messageActionStarGiftUnique_layer202 | verified | TLRPC.java:68493 |  |
| MessageAction | 0xb00c47a2 | TL_messageActionPrizeStars | verified | TLRPC.java:67317 |  |
| MessageAction | 0xb055eaee | TL_messageActionChannelMigrateFrom_layer131 | verified | TLRPC.java:25890 |  |
| MessageAction | 0xb07ed085 | TL_messageActionNewCreatorPending | verified | TLRPC.java:25461 |  |
| MessageAction | 0xb18a431c | TL_messageActionTopicEdit_layer149 | verified | TLRPC.java:25825 |  |
| MessageAction | 0xb2ae9b0c | TL_messageActionChatDeleteUser_layer131 | verified | TLRPC.java:25918 |  |
| MessageAction | 0xb3a07661 | TL_messageActionGroupCallScheduled | verified | TLRPC.java:26658 |  |
| MessageAction | 0xb4c38cb5 | TL_messageActionWebViewDataSent | verified | TLRPC.java:25972 |  |
| MessageAction | 0xb5a1ce5a | TL_messageActionChatEditTitle | verified | TLRPC.java:26247 |  |
| MessageAction | 0xb6aef7b0 | TL_messageActionEmpty | verified | TLRPC.java:26384 |  |
| MessageAction | 0xb91bbd3a | TL_messageActionSetChatTheme | verified | TLRPC.java:25676 |  |
| MessageAction | 0xbc44a927 | TL_messageActionSetChatWallPaper_layer166 | verified | TLRPC.java:25987 |  |
| MessageAction | 0xbcd71419 | TL_messageActionPaidMessagesPrice_layer203 | verified | TLRPC.java:67856 |  |
| MessageAction | 0xbd47cbad | TL_messageActionChatCreate | verified | TLRPC.java:25531 |  |
| MessageAction | 0xbf7d6572 | TL_messageActionNoForwardsToggle | verified | TLRPC.java:64858 |  |
| MessageAction | 0xc0787d6d | TL_messageActionSetSameChatWallPaper | verified | TLRPC.java:25771 |  |
| MessageAction | 0xc0944820 | TL_messageActionTopicEdit | verified | TLRPC.java:25784 |  |
| MessageAction | 0xc1d2f1c6 | TL_messageActionNoPaidMessagesTokenUsed | verified | TLRPC.java:68343 |  |
| MessageAction | 0xc516d679 | TL_messageActionBotAllowed | verified | TLRPC.java:26335 |  |
| MessageAction | 0xc624b16e | TL_messageActionPaymentSent | verified | TLRPC.java:26260 |  |
| MessageAction | 0xc7edbc83 | TL_messageActionTodoAppendTasks | verified | TLRPC.java:67935 |  |
| MessageAction | 0xc83d6aec | TL_messageActionGiftPremium_layer189 | verified | TLRPC.java:26477 |  |
| MessageAction | 0xcc02aa6d | TL_messageActionBoostApply | verified | TLRPC.java:26557 |  |
| MessageAction | 0xcc7c5c89 | TL_messageActionTodoCompletions | verified | TLRPC.java:67915 |  |
| MessageAction | 0xd2cfdb0e | TL_messageActionGiftCode_layer167 | verified | TLRPC.java:65985 |  |
| MessageAction | 0xd8f4f0a7 | TL_messageActionStarGift_layer197 | verified | TLRPC.java:67749 |  |
| MessageAction | 0xd95c6154 | TL_messageActionSecureValuesSent | verified | TLRPC.java:26140 |  |
| MessageAction | 0xdb596550 | TL_messageActionStarGift_layer219 | verified | TLRPC.java:67502 |  |
| MessageAction | 0xe1037f92 | TL_messageActionChatMigrateTo | verified | TLRPC.java:25864 |  |
| MessageAction | 0xe188503b | TL_messageActionChangeCreator | verified | TLRPC.java:25446 |  |
| MessageAction | 0xe6c31522 | TL_messageActionStarGiftUnique_layer228 | verified | TLRPC.java:68094 |  |
| MessageAction | 0xe7e75f97 | TL_messageActionAttachMenuBotAllowed | verified | TLRPC.java:64959 |  |
| MessageAction | 0xea2c31d3 | TL_messageActionStarGift | verified | TLRPC.java:67379 |  |
| MessageAction | 0xea3948e9 | TL_messageActionChannelMigrateFrom | verified | TLRPC.java:25576 |  |
| MessageAction | 0xebbca3cb | TL_messageActionChatJoinedByRequest | verified | TLRPC.java:25856 |  |
| MessageAction | 0xee7a1596 | TL_messageActionSuggestedPostApproval | verified | TLRPC.java:64914 |  |
| MessageAction | 0xf24de7fa | TL_messageActionStarGift_layer217 | verified | TLRPC.java:67595 |  |
| MessageAction | 0xf3f25f76 | TL_messageActionContactSignUp | verified | TLRPC.java:26043 |  |
| MessageAction | 0xf89cf5e8 | TL_messageActionChatJoinedByLink_layer131 | verified | TLRPC.java:26114 |  |
| MessageAction | 0xfae69f56 | TL_messageActionCustomAction | verified | TLRPC.java:25518 |  |
| MessageAction | 0xfe77345d | TL_messageActionRequestedPeer_layer168 | verified | TLRPC.java:64970 |  |
| MessageAction | 0xffa00ccc | TL_messageActionPaymentSentMe | verified | TLRPC.java:26572 |  |
| PhoneCallDiscardReason | 0x57adc690 | TL_phoneCallDiscardReasonHangup | verified | TLRPC.java:20934 |  |
| PhoneCallDiscardReason | 0x85e42301 | TL_phoneCallDiscardReasonMissed | verified | TLRPC.java:20950 |  |
| PhoneCallDiscardReason | 0x9fbbf1f7 | TL_phoneCallDiscardReasonMigrateConferenceCall | verified | TLRPC.java:20966 |  |
| PhoneCallDiscardReason | 0xe095c1a0 | TL_phoneCallDiscardReasonDisconnect | verified | TLRPC.java:20958 |  |
| PhoneCallDiscardReason | 0xfaf7e8c9 | TL_phoneCallDiscardReasonBusy | verified | TLRPC.java:20942 |  |
| MessageMedia | 0x08cbec07 | TL_messageMediaDice | verified | TLRPC.java:8203 |  |
| MessageMedia | 0x2238f214 | TL_messageMediaWidget | verified | TLRPC.java:66788 |  |
| MessageMedia | 0x29632a36 | TL_messageMediaUnsupported_old | verified | TLRPC.java:7269 |  |
| MessageMedia | 0x2ec0533f | TL_messageMediaVenue | verified | TLRPC.java:7491 |  |
| MessageMedia | 0x2fda2204 | TL_messageMediaDocument_old | verified | TLRPC.java:7804 |  |
| MessageMedia | 0x3d8ce53d | TL_messageMediaPhoto_layer68 | verified | TLRPC.java:8108 |  |
| MessageMedia | 0x3ded6320 | TL_messageMediaEmpty | verified | TLRPC.java:7483 |  |
| MessageMedia | 0x3f7ee58b | TL_messageMediaDice_layer220 | verified | TLRPC.java:8230 |  |
| MessageMedia | 0x4bd6e798 | TL_messageMediaPoll_layer223 | verified | TLRPC.java:7926 |  |
| MessageMedia | 0x4cf4d72d | TL_messageMediaDocument_layer187 | verified | TLRPC.java:7698 |  |
| MessageMedia | 0x52d8ccd9 | TL_messageMediaDocument | verified | TLRPC.java:7554 |  |
| MessageMedia | 0x56e0d474 | TL_messageMediaGeo | verified | TLRPC.java:8258 |  |
| MessageMedia | 0x58260664 | TL_messageMediaGiveaway_layer167 | verified | TLRPC.java:66720 |  |
| MessageMedia | 0x5bcf1675 | TL_messageMediaVideo_layer45 | verified | TLRPC.java:8123 |  |
| MessageMedia | 0x5e7d2f39 | TL_messageMediaContact_layer81 | verified | TLRPC.java:8089 |  |
| MessageMedia | 0x638fe46b | TL_messageMediaDice_layer111 | verified | TLRPC.java:8245 |  |
| MessageMedia | 0x68cb6283 | TL_messageMediaStory | verified | TLRPC.java:65742 |  |
| MessageMedia | 0x695150d7 | TL_messageMediaPhoto_layer223 | verified | TLRPC.java:7869 |  |
| MessageMedia | 0x70322949 | TL_messageMediaContact | verified | TLRPC.java:8159 |  |
| MessageMedia | 0x773f4e66 | TL_messageMediaPoll | verified | TLRPC.java:7898 |  |
| MessageMedia | 0x7912b71f | TL_messageMediaVenue_layer71 | verified | TLRPC.java:7520 |  |
| MessageMedia | 0x7c3c2609 | TL_messageMediaGeoLive_layer119 | verified | TLRPC.java:8061 |  |
| MessageMedia | 0x7c4414d3 | TL_messageMediaDocument_layer74 | verified | TLRPC.java:7771 |  |
| MessageMedia | 0x84551347 | TL_messageMediaInvoice_layer145 | verified | TLRPC.java:7434 |  |
| MessageMedia | 0x8a53b014 | TL_messageMediaToDo | verified | TLRPC.java:66764 |  |
| MessageMedia | 0x9cb070d7 | TL_messageMediaDocument_layer159 | verified | TLRPC.java:7739 |  |
| MessageMedia | 0x9f84f49e | TL_messageMediaUnsupported | verified | TLRPC.java:7475 |  |
| MessageMedia | 0xa2d24290 | TL_messageMediaVideo_old | verified | TLRPC.java:7541 |  |
| MessageMedia | 0xa32dd600 | TL_messageMediaWebPage_layer165 | verified | TLRPC.java:8294 |  |
| MessageMedia | 0xa831bf37 | TL_messageMediaWidget_layer233 | verified | TLRPC.java:66851 |  |
| MessageMedia | 0xa8852491 | TL_messageMediaPaidMedia | verified | TLRPC.java:66749 |  |
| MessageMedia | 0xaa073beb | TL_messageMediaGiveaway | verified | TLRPC.java:66626 |  |
| MessageMedia | 0xb5223b0f | TL_messageMediaPhoto_layer74 | verified | TLRPC.java:7941 |  |
| MessageMedia | 0xb7ed7b0d | TL_messageMediaWidget_old | verified | TLRPC.java:66876 |  |
| MessageMedia | 0xb940c666 | TL_messageMediaGeoLive | verified | TLRPC.java:8032 |  |
| MessageMedia | 0xc6991068 | TL_messageMediaGiveawayResults_layer186 | verified | TLRPC.java:66583 |  |
| MessageMedia | 0xc6b68300 | TL_messageMediaAudio_layer45 | verified | TLRPC.java:7282 |  |
| MessageMedia | 0xc79aee1d | MessageMediaStoryFull | verified | MessageMediaStoryFull.java:11 |  |
| MessageMedia | 0xc79aee1f | MessageMediaStoryFull_old | verified | MessageMediaStoryFull_old.java:9 |  |
| MessageMedia | 0xc8c45a2a | TL_messageMediaPhoto_old | verified | TLRPC.java:7295 |  |
| MessageMedia | 0xca5cab89 | TL_messageMediaVideoStream | verified | TLRPC.java:8180 |  |
| MessageMedia | 0xcbb20d88 | TL_messageMediaStory_layer162 | verified | TLRPC.java:65767 |  |
| MessageMedia | 0xcbf24940 | TL_messageMediaContact_layer131 | verified | TLRPC.java:8138 |  |
| MessageMedia | 0xceaa3ea1 | TL_messageMediaGiveawayResults | verified | TLRPC.java:66517 |  |
| MessageMedia | 0xdaad85b0 | TL_messageMediaGiveaway_layer186 | verified | TLRPC.java:66683 |  |
| MessageMedia | 0xdbbdf614 | TL_messageMediaDocument_layer197_2 | verified | TLRPC.java:7610 |  |
| MessageMedia | 0xdd570bd5 | TL_messageMediaDocument_layer197 | verified | TLRPC.java:7657 |  |
| MessageMedia | 0xddf10c3b | TL_messageMediaWebPage | verified | TLRPC.java:8271 |  |
| MessageMedia | 0xe216eb63 | TL_messageMediaPhoto | verified | TLRPC.java:7832 |  |
| MessageMedia | 0xf3e02ea8 | TL_messageMediaDocument_layer68 | verified | TLRPC.java:7817 |  |
| MessageMedia | 0xf6a548d3 | TL_messageMediaInvoice | verified | TLRPC.java:7386 |  |
| MessageMedia | 0xfdb19008 | TL_messageMediaGame | verified | TLRPC.java:8076 |  |
| Photo | 0x22b56751 | TL_photo_old | verified | TLRPC.java:33275 |  |
| Photo | 0x2331b22d | TL_photoEmpty | verified | TLRPC.java:33262 |  |
| Photo | 0x9288dd29 | TL_photo_layer82 | verified | TLRPC.java:33360 |  |
| Photo | 0x9c477dd8 | TL_photo_layer97 | verified | TLRPC.java:33195 |  |
| Photo | 0xc3838076 | TL_photo_old2 | verified | TLRPC.java:33239 |  |
| Photo | 0xcded42fe | TL_photo_layer55 | verified | TLRPC.java:33220 |  |
| Photo | 0xd07504a5 | TL_photo_layer115 | verified | TLRPC.java:33300 |  |
| Photo | 0xfb197a65 | TL_photo | verified | TLRPC.java:33327 |  |
| Document | 0x1e87342b | TL_document_layer142 | verified | TLRPC.java:29229 |  |
| Document | 0x36f8c871 | TL_documentEmpty | verified | TLRPC.java:29334 |  |
| Document | 0x55555556 | TL_documentEncrypted_old | verified | TLRPC.java:29155 |  |
| Document | 0x55555558 | TL_documentEncrypted | verified | TLRPC.java:29347 |  |
| Document | 0x59534e4c | TL_document_layer92 | verified | TLRPC.java:29097 |  |
| Document | 0x87232bc7 | TL_document_layer82 | verified | TLRPC.java:29126 |  |
| Document | 0x8fd4c4d8 | TL_document | verified | TLRPC.java:29188 |  |
| Document | 0x9ba29cc1 | TL_document_layer113 | verified | TLRPC.java:29270 |  |
| Document | 0x9efc6326 | TL_document_old | verified | TLRPC.java:29305 |  |
| Document | 0xf9a39f4f | TL_document_layer53 | verified | TLRPC.java:29378 |  |
| GeoPoint | 0x0296f104 | TL_geoPoint_layer119 | verified | TLRPC.java:5081 |  |
| GeoPoint | 0x1117dd5f | TL_geoPointEmpty | verified | TLRPC.java:5113 |  |
| GeoPoint | 0x2049d70c | TL_geoPoint_layer81 | verified | TLRPC.java:5098 |  |
| GeoPoint | 0xb2a2f663 | TL_geoPoint | verified | TLRPC.java:5121 |  |
| WebPage | 0x211a1788 | TL_webPageEmpty | verified | TLRPC.java:20333 |  |
| WebPage | 0x5f07b4bc | TL_webPage_layer104 | verified | TLRPC.java:20149 |  |
| WebPage | 0x7311ca11 | TL_webPageNotModified | verified | TLRPC.java:20431 |  |
| WebPage | 0x85849473 | TL_webPageNotModified_layer110 | verified | TLRPC.java:20418 |  |
| WebPage | 0xa31ea0b5 | TL_webPage_old | verified | TLRPC.java:20248 |  |
| WebPage | 0xb0d13e47 | TL_webPagePending | verified | TLRPC.java:20395 |  |
| WebPage | 0xc586da1c | TL_webPagePending_layer165 | verified | TLRPC.java:20380 |  |
| WebPage | 0xca820ed7 | TL_webPage_layer58 | verified | TLRPC.java:20452 |  |
| WebPage | 0xd41a5167 | TL_webPageUrlPending | verified | TLRPC.java:20367 |  |
| WebPage | 0xe89c45b2 | TL_webPage | verified | TLRPC.java:19932 |  |
| WebPage | 0xeb1477e8 | TL_webPageEmpty_layer165 | verified | TLRPC.java:20354 |  |
| WebPage | 0xfa64e172 | TL_webPage_layer107 | MANUAL, read by hand at TLRPC.java:20087 | TLRPC.java:20041 | a statement on the stream of an unknown form |
| User | 0x020b1422 | TL_user_layer216 | verified | TLRPC.java:22479 |  |
| User | 0x075cf7a8 | TL_userForeign_old2 | verified | TLRPC.java:24461 |  |
| User | 0x1c60e608 | TL_userSelf_old3 | verified | TLRPC.java:24356 |  |
| User | 0x200250ba | TL_userEmpty_layer131 | verified | TLRPC.java:24400 |  |
| User | 0x215c4438 | TL_user_layer184 | verified | TLRPC.java:23234 |  |
| User | 0x22e49072 | TL_user_old | verified | TLRPC.java:24627 |  |
| User | 0x22e8ceb0 | TL_userRequest_old | verified | TLRPC.java:24413 |  |
| User | 0x2e13f4c3 | TL_user_layer104 | verified | TLRPC.java:24224 |  |
| User | 0x31774388 | TL_user_layer218 | verified | TLRPC.java:22273 |  |
| User | 0x3ff6ecb0 | TL_user_layer144 | verified | TLRPC.java:23998 |  |
| User | 0x4b46c37e | TL_user_layer199 | verified | TLRPC.java:22678 |  |
| User | 0x4fe9cce0 | TL_user_layer185 | verified | TLRPC.java:23052 |  |
| User | 0x5214c89d | TL_userForeign_old | verified | TLRPC.java:24438 |  |
| User | 0x5d99adee | TL_user_layer147 | verified | TLRPC.java:23875 |  |
| User | 0x7007b451 | TL_userSelf_old2 | verified | TLRPC.java:24708 |  |
| User | 0x720535ec | TL_userSelf_old | verified | TLRPC.java:24331 |  |
| User | 0x83314fca | TL_user_layer195 | verified | TLRPC.java:22868 |  |
| User | 0x8f97c628 | TL_user_layer159 | verified | TLRPC.java:23735 |  |
| User | 0x938458c1 | TL_user_layer131 | verified | TLRPC.java:24113 |  |
| User | 0xabb5f120 | TL_user_layer165 | verified | TLRPC.java:23581 |  |
| User | 0xb1b8cc83 | TL_user | verified | TLRPC.java:22059 |  |
| User | 0xb29ad7cc | TL_userDeleted_old | verified | TLRPC.java:24513 |  |
| User | 0xcab35e18 | TL_userContact_old2 | verified | TLRPC.java:21962 |  |
| User | 0xd10d979a | TL_user_layer65 | verified | TLRPC.java:24530 |  |
| User | 0xd3bc4b7a | TL_userEmpty | verified | TLRPC.java:22014 |  |
| User | 0xd6016d7a | TL_userDeleted_old2 | verified | TLRPC.java:24381 |  |
| User | 0xd9ccc4ef | TL_userRequest_old2 | verified | TLRPC.java:24486 |  |
| User | 0xeb602f25 | TL_user_layer166 | verified | TLRPC.java:23410 |  |
| User | 0xf2fb8319 | TL_userContact_old | verified | TLRPC.java:21989 |  |
| Chat | 0x07328bdb | TL_chatForbidden_layer131 | verified | TLRPC.java:42223 |  |
| Chat | 0x0aadfc8f | TL_channel_layer185 | verified | TLRPC.java:41430 |  |
| Chat | 0x0cb44b1c | TL_channel_layer72 | verified | TLRPC.java:42659 |  |
| Chat | 0x17d493d5 | TL_channelForbidden | verified | TLRPC.java:39960 |  |
| Chat | 0x1981ea7e | TL_channel_layer166 | verified | TLRPC.java:41710 |  |
| Chat | 0x1c32b11c | TL_channel_layer227 | verified | TLRPC.java:40501 |  |
| Chat | 0x289da732 | TL_channelForbidden_layer131 | verified | TLRPC.java:39991 |  |
| Chat | 0x29562865 | TL_chatEmpty | verified | TLRPC.java:61827 |  |
| Chat | 0x2d85832c | TL_channelForbidden_layer52 | verified | TLRPC.java:40098 |  |
| Chat | 0x3bda1bde | TL_chat_layer131 | verified | TLRPC.java:39905 |  |
| Chat | 0x41cbf256 | TL_chat | verified | TLRPC.java:39844 |  |
| Chat | 0x450b7115 | TL_channel_layer77 | verified | TLRPC.java:42582 |  |
| Chat | 0x4b1b7506 | TL_channel_layer48 | verified | TLRPC.java:40043 |  |
| Chat | 0x4df30834 | TL_channel_layer104 | verified | TLRPC.java:42468 |  |
| Chat | 0x6592a1a7 | TL_chatForbidden | verified | TLRPC.java:42208 |  |
| Chat | 0x65efe954 | TL_community | MANUAL, read by hand at TLRPC.java:40714 | TLRPC.java:40705 | a flag-only true set by an expression that is not one flag test |
| Chat | 0x678e9587 | TL_channel_old | verified | TLRPC.java:42299 |  |
| Chat | 0x6e9c9bc7 | TL_chat_old | verified | TLRPC.java:42346 |  |
| Chat | 0x7312bc48 | TL_chat_old2 | verified | TLRPC.java:39811 |  |
| Chat | 0x7482147e | TL_channel_layer203 | verified | TLRPC.java:40939 |  |
| Chat | 0x7ad15240 | TL_channel | verified | TLRPC.java:40115 |  |
| Chat | 0x8261ac61 | TL_channel_layer147 | verified | TLRPC.java:42105 |  |
| Chat | 0x83259464 | TL_channel_layer161 | verified | TLRPC.java:41986 |  |
| Chat | 0x8537784f | TL_channelForbidden_layer67 | verified | TLRPC.java:40020 |  |
| Chat | 0x8e87ccd8 | TL_channel_layer167 | verified | TLRPC.java:41579 |  |
| Chat | 0x94f592db | TL_channel_layer165 | verified | TLRPC.java:41852 |  |
| Chat | 0x9ba2d800 | TL_chatEmpty_layer131 | verified | TLRPC.java:61842 |  |
| Chat | 0xa14dca52 | TL_channel_layer67 | verified | TLRPC.java:42238 |  |
| Chat | 0xc88974ac | TL_channel_layer92 | verified | TLRPC.java:42731 |  |
| Chat | 0xd31a961e | TL_channel_layer131 | verified | TLRPC.java:42371 |  |
| Chat | 0xd49f34c6 | TL_channel_layer232 | verified | TLRPC.java:40312 |  |
| Chat | 0xd91cdd54 | TL_chat_layer92 | verified | TLRPC.java:42808 |  |
| Chat | 0xe00998b7 | TL_channel_layer199 | verified | TLRPC.java:41110 |  |
| Chat | 0xfb0ccc41 | TL_chatForbidden_old | verified | TLRPC.java:39794 |  |
| Chat | 0xfd3cdab8 | TL_communityForbidden | verified | TLRPC.java:40682 |  |
| Chat | 0xfe4478bd | TL_channel_layer195 | verified | TLRPC.java:41273 |  |
| Chat | 0xfe685355 | TL_channel_layer216 | verified | TLRPC.java:40756 |  |
| PhotoSize | 0x021e1ad6 | TL_photoCachedSize | verified | TLRPC.java:44569 |  |
| PhotoSize | 0x0e17e23c | TL_photoSizeEmpty | verified | TLRPC.java:44516 |  |
| PhotoSize | 0x5aa86a51 | TL_photoSizeProgressive_layer127 | verified | TLRPC.java:44471 |  |
| PhotoSize | 0x75c78e60 | TL_photoSize | verified | TLRPC.java:44550 |  |
| PhotoSize | 0x77bfb61b | TL_photoSize_layer127 | verified | TLRPC.java:44495 |  |
| PhotoSize | 0xd8214d41 | TL_photoPathSize | verified | TLRPC.java:58903 |  |
| PhotoSize | 0xe0b0bc2e | TL_photoStrippedSize | verified | TLRPC.java:44455 |  |
| PhotoSize | 0xe9a734fa | TL_photoCachedSize_layer127 | verified | TLRPC.java:44529 |  |
| PhotoSize | 0xfa3efb95 | TL_photoSizeProgressive | verified | TLRPC.java:44588 |  |
| VideoSize | 0x0da082fe | TL_videoSizeStickerMarkup | verified | TLRPC.java:19032 |  |
| VideoSize | 0x435bb987 | TL_videoSize_layer115 | verified | TLRPC.java:19105 |  |
| VideoSize | 0xde33b094 | TL_videoSize | verified | TLRPC.java:19155 |  |
| VideoSize | 0xe831c556 | TL_videoSize_layer127 | verified | TLRPC.java:19126 |  |
| VideoSize | 0xf85c413c | TL_videoSizeEmojiMarkup | verified | TLRPC.java:19015 |  |
| DocumentAttribute | 0x051448e5 | TL_documentAttributeAudio_old | verified | TLRPC.java:1645 |  |
| DocumentAttribute | 0x0ef02ce6 | TL_documentAttributeVideo_layer159 | verified | TLRPC.java:1815 |  |
| DocumentAttribute | 0x11b58939 | TL_documentAttributeAnimated | verified | TLRPC.java:1683 |  |
| DocumentAttribute | 0x15590068 | TL_documentAttributeFilename | verified | TLRPC.java:1691 |  |
| DocumentAttribute | 0x17399fad | TL_documentAttributeVideo_layer187 | verified | TLRPC.java:1747 |  |
| DocumentAttribute | 0x3a556302 | TL_documentAttributeSticker_layer55 | verified | TLRPC.java:1630 |  |
| DocumentAttribute | 0x43c57c48 | TL_documentAttributeVideo | verified | TLRPC.java:1704 |  |
| DocumentAttribute | 0x5910cccb | TL_documentAttributeVideo_layer65 | verified | TLRPC.java:1838 |  |
| DocumentAttribute | 0x6319d612 | TL_documentAttributeSticker | verified | TLRPC.java:1658 |  |
| DocumentAttribute | 0x6c37c15c | TL_documentAttributeImageSize | verified | TLRPC.java:1904 |  |
| DocumentAttribute | 0x9801d2f7 | TL_documentAttributeHasStickers | verified | TLRPC.java:1883 |  |
| DocumentAttribute | 0x9852f9c6 | TL_documentAttributeAudio | verified | TLRPC.java:1919 |  |
| DocumentAttribute | 0x994c9882 | TL_documentAttributeSticker_old2 | verified | TLRPC.java:1891 |  |
| DocumentAttribute | 0xd38ff1c2 | TL_documentAttributeVideo_layer184 | verified | TLRPC.java:1784 |  |
| DocumentAttribute | 0xded218e0 | TL_documentAttributeAudio_layer45 | verified | TLRPC.java:1855 |  |
| DocumentAttribute | 0xfb0a5727 | TL_documentAttributeSticker_old | verified | TLRPC.java:1872 |  |
| DocumentAttribute | 0xfd149899 | TL_documentAttributeCustomEmoji | verified | TLRPC.java:1954 |  |
| FileLocation | 0x091d11eb | TL_fileLocation_layer97 | verified | TLRPC.java:44288 |  |
| FileLocation | 0x53d69076 | TL_fileLocation_layer82 | verified | TLRPC.java:44269 |  |
| FileLocation | 0x55555554 | TL_fileEncryptedLocation | verified | TLRPC.java:44324 |  |
| FileLocation | 0x7c596b46 | TL_fileLocationUnavailable | verified | TLRPC.java:44252 |  |
| FileLocation | 0xbc7fc6cd | TL_fileLocationToBeDeprecated | verified | TLRPC.java:44309 |  |
| InputStickerSet | 0x028703c8 | TL_inputStickerSetAnimatedEmoji | verified | TLRPC.java:30823 |  |
| InputStickerSet | 0x04c4d4ce | TL_inputStickerSetEmojiGenericAnimations | verified | TLRPC.java:30831 |  |
| InputStickerSet | 0x1cf671a0 | TL_inputStickerSetTonGifts | verified | TLRPC.java:30737 |  |
| InputStickerSet | 0x29d0f5ee | TL_inputStickerSetEmojiDefaultStatuses | verified | TLRPC.java:30753 |  |
| InputStickerSet | 0x44c1f8e9 | TL_inputStickerSetEmojiDefaultTopicIcons | verified | TLRPC.java:30839 |  |
| InputStickerSet | 0x49748553 | TL_inputStickerSetEmojiChannelDefaultStatuses | verified | TLRPC.java:30745 |  |
| InputStickerSet | 0x79e21a53 | TL_inputStickerSetDice_layer111 | verified | TLRPC.java:30776 |  |
| InputStickerSet | 0x861cc8a0 | TL_inputStickerSetShortName | verified | TLRPC.java:30810 |  |
| InputStickerSet | 0x9de7a269 | TL_inputStickerSetID | verified | TLRPC.java:30795 |  |
| InputStickerSet | 0xc88b3b02 | TL_inputStickerSetPremiumGifts | verified | TLRPC.java:30729 |  |
| InputStickerSet | 0xe67f520e | TL_inputStickerSetDice | verified | TLRPC.java:30761 |  |
| InputStickerSet | 0xffb62b95 | TL_inputStickerSetEmpty | verified | TLRPC.java:30787 |  |
| TL_maskCoords | 0xaed6dbb2 | TL_maskCoords | verified | TLRPC.java:29623 |  |
| Video | 0x388fa391 | TL_video_old2 | verified | TLRPC.java:27814 |  |
| Video | 0x55555553 | TL_videoEncrypted | verified | TLRPC.java:27744 |  |
| Video | 0x5a04a49f | TL_video_old | verified | TLRPC.java:27781 |  |
| Video | 0xc10658a8 | TL_videoEmpty_layer45 | verified | TLRPC.java:27849 |  |
| Video | 0xee9f4a4d | TL_video_old3 | verified | TLRPC.java:27682 |  |
| Video | 0xf72887d3 | TL_video_layer45 | verified | TLRPC.java:27713 |  |
| Audio | 0x427425e7 | TL_audio_old | verified | TLRPC.java:9704 |  |
| Audio | 0x555555f6 | TL_audioEncrypted | verified | TLRPC.java:9729 |  |
| Audio | 0x586988d8 | TL_audioEmpty_layer45 | verified | TLRPC.java:9666 |  |
| Audio | 0xc7ac6496 | TL_audio_old2 | verified | TLRPC.java:9758 |  |
| Audio | 0xf9e35055 | TL_audio_layer45 | verified | TLRPC.java:9679 |  |
| MessageReplies | 0x4128faac | TL_messageReplies_layer131 | verified | TLRPC.java:9976 |  |
| MessageReplies | 0x83d60fc2 | TL_messageReplies | verified | TLRPC.java:10019 |  |
| MessageReactions | 0x087b6e36 | TL_messageReactionsOld | verified | TLRPC.java:27588 |  |
| MessageReactions | 0x0a339f0b | TL_messageReactions | verified | TLRPC.java:27428 |  |
| MessageReactions | 0x4f2b9479 | TL_messageReactions_layer185 | verified | TLRPC.java:27461 |  |
| MessageReactions | 0xb87a24d1 | TL_messageReactions_layer137 | verified | TLRPC.java:27623 |  |
| Reaction | 0x1b2286b8 | TL_reactionEmoji | verified | TLRPC.java:51781 |  |
| Reaction | 0x523da4eb | TL_reactionPaid | verified | TLRPC.java:51773 |  |
| Reaction | 0x79f5d419 | TL_reactionEmpty | verified | TLRPC.java:51765 |  |
| Reaction | 0x8935fc73 | TL_reactionCustomEmoji | verified | TLRPC.java:51750 |  |
| MessagePeerReaction | 0x51b67eff | TL_messagePeerReaction_layer144 | MANUAL, read by hand at TLRPC.java:2688 | TLRPC.java:2679 | a statement on the stream of an unknown form |
| MessagePeerReaction | 0x8c79b63c | TL_messagePeerReaction | verified | TLRPC.java:2635 |  |
| MessagePeerReaction | 0x932844fa | TL_messagePeerReaction_layer137 | MANUAL, read by hand at TLRPC.java:2712 | TLRPC.java:2704 | a statement on the stream of an unknown form |
| MessagePeerReaction | 0xb156fe9c | TL_messagePeerReaction_layer154 | verified | TLRPC.java:2658 |  |
| MessageReactor | 0x4ba3a95a | TL_messageReactor | verified | TLRPC.java:27350 |  |
| MessageReactor | 0xef00d448 | TL_messageReactor_old | verified | TLRPC.java:27377 |  |
| RestrictionReason | 0xd072acb4 | TL_reastrictionReason | verified | TLRPC.java:42864 |  |
| TL_factCheck | 0xb89bfccf | TL_factCheck | verified | TLRPC.java:69998 |  |
| TL_textWithEntities | 0x751f3146 | TL_textWithEntities | verified | TLRPC.java:45022 |  |
| SuggestedPost | 0x0e8e37e5 | SuggestedPost | verified | TLRPC.java:70661 |  |
| TL_stars.StarsAmount | 0x74aee3e0 | TL_starsTonAmount | verified | TL_stars.java:2545 |  |
| TL_stars.StarsAmount | 0xbbb6b4a3 | TL_starsAmount | verified | TL_stars.java:2563 |  |
| TL_messages_emojiGameOutcome | 0xda2ad647 | TL_messages_emojiGameOutcome | verified | TLRPC.java:71378 |  |
| ReactionCount | 0x6fb250d1 | TL_reactionCount_layer144 | verified | TLRPC.java:45165 |  |
| ReactionCount | 0xa3d1cb80 | TL_reactionCount | MANUAL, read by hand at TLRPC.java:45148 | TLRPC.java:45142 | a read under a condition that is not one flag bit |
| WebPageAttribute | 0x01c641c2 | TL_webPageAttributeStarGiftAuction | verified | TLRPC.java:10389 |  |
| WebPageAttribute | 0x034986ab | TL_webPageAttributeStarGiftAuction_layer219 | verified | TLRPC.java:10441 |  |
| WebPageAttribute | 0x2e94c3e7 | TL_webPageAttributeStory | verified | TLRPC.java:10464 |  |
| WebPageAttribute | 0x31cad303 | TL_webPageAttributeStarGiftCollection | verified | TLRPC.java:10515 |  |
| WebPageAttribute | 0x50cc03d3 | TL_webPageAttributeStickerSet | verified | TLRPC.java:10491 |  |
| WebPageAttribute | 0x54b56617 | TL_webPageAttributeTheme | verified | TLRPC.java:10575 |  |
| WebPageAttribute | 0x7781fe18 | TL_webPageAttributeAiComposeTone | verified | TLRPC.java:10424 |  |
| WebPageAttribute | 0x939a4671 | TL_webPageAttributeStory_layer162 | verified | TLRPC.java:10545 |  |
| WebPageAttribute | 0xcf6f6db8 | TL_webPageAttributeUniqueStarGift | verified | TLRPC.java:10530 |  |
| Poll | 0x58747131 | TL_poll_layer223 | verified | TLRPC.java:34428 |  |
| Poll | 0x86e18161 | TL_poll_layer178 | verified | TLRPC.java:34475 |  |
| Poll | 0x966e2dbf | TL_poll | verified | TLRPC.java:34318 |  |
| Poll | 0xaf746786 | TL_poll_toDelete | verified | TLRPC.java:34515 |  |
| Poll | 0xb8425be9 | TL_poll_layer224 | verified | TLRPC.java:34377 |  |
| Poll | 0xd5529d06 | TL_poll_layer111 | verified | TLRPC.java:34549 |  |
| PollResults | 0x5755785a | TL_pollResults_layer108 | verified | TLRPC.java:4235 |  |
| PollResults | 0x7adf2420 | TL_pollResults_layer223 | verified | TLRPC.java:4392 |  |
| PollResults | 0xba7bb15e | TL_pollResults | verified | TLRPC.java:4336 |  |
| PollResults | 0xbadcc1a3 | TL_pollResults_layer131 | MANUAL, read by hand at TLRPC.java:4308 | TLRPC.java:4295 | a statement on the stream of an unknown form |
| PollResults | 0xc87024a2 | TL_pollResults_layer111 | MANUAL, read by hand at TLRPC.java:4275 | TLRPC.java:4262 | a statement on the stream of an unknown form |
| PollResults | 0xdcb82ea3 | TL_pollResults_layer158 | MANUAL, read by hand at TLRPC.java:4450 | TLRPC.java:4437 | a statement on the stream of an unknown form |
| ReplyMarkup | 0x3502758c | TL_replyKeyboardMarkup_layer129 | verified | TLRPC.java:10279 |  |
| ReplyMarkup | 0x48a30254 | TL_replyInlineMarkup_layer228 | verified | TLRPC.java:10353 |  |
| ReplyMarkup | 0x58f7fcb6 | TL_replyInlineMarkup_layer228_old | verified | TLRPC.java:10336 |  |
| ReplyMarkup | 0x85dd99d1 | TL_replyKeyboardMarkup | verified | TLRPC.java:10208 |  |
| ReplyMarkup | 0x86b40b08 | TL_replyKeyboardForceReply | verified | TLRPC.java:10256 |  |
| ReplyMarkup | 0xa03e5b85 | TL_replyKeyboardHide | verified | TLRPC.java:10241 |  |
| ReplyMarkup | 0xb2b15770 | TL_replyInlineMarkup | verified | TLRPC.java:10317 |  |
| ReplyMarkup | 0xf4108aa0 | TL_replyKeyboardForceReply_layer129 | verified | TLRPC.java:10300 |  |
| InputUser | 0x1da448e2 | TL_inputUserFromMessage | verified | TLRPC.java:18959 |  |
| InputUser | 0x2d117597 | TL_inputUserFromMessage_layer132 | verified | TLRPC.java:18994 |  |
| InputUser | 0xb98886cf | TL_inputUserEmpty | verified | TLRPC.java:18951 |  |
| InputUser | 0xd8292816 | TL_inputUser_layer132 | verified | TLRPC.java:18979 |  |
| InputUser | 0xf21158c6 | TL_inputUser | verified | TLRPC.java:18936 |  |
| InputUser | 0xf7c1b13f | TL_inputUserSelf | verified | TLRPC.java:18928 |  |
| TL_iv.RichText | 0x01a9fbfc | textMentionName | verified | TL_iv.java:718 |  |
| TL_iv.RichText | 0x02ff29d3 | textBotCommand | verified | TL_iv.java:634 |  |
| TL_iv.RichText | 0x034b8621 | textMarked | verified | TL_iv.java:490 |  |
| TL_iv.RichText | 0x081ccf4f | textImage | verified | TL_iv.java:518 |  |
| TL_iv.RichText | 0x1ccb966a | textPhone | verified | TL_iv.java:502 |  |
| TL_iv.RichText | 0x24c26789 | textAutoPhone | verified | TL_iv.java:690 |  |
| TL_iv.RichText | 0x35553762 | textAnchor | verified | TL_iv.java:539 |  |
| TL_iv.RichText | 0x3c2884c1 | textUrl | verified | TL_iv.java:424 |  |
| TL_iv.RichText | 0x3cd2a36a | textTonAddress | verified | TL_iv.java:834 |  |
| TL_iv.RichText | 0x4c2a5d62 | textSpoiler | verified | TL_iv.java:592 |  |
| TL_iv.RichText | 0x519524ea | textHashtag | verified | TL_iv.java:620 |  |
| TL_iv.RichText | 0x6724abc4 | textBold | verified | TL_iv.java:364 |  |
| TL_iv.RichText | 0x6c3f19b9 | textFixed | verified | TL_iv.java:412 |  |
| TL_iv.RichText | 0x744694e0 | textPlain | verified | TL_iv.java:350 |  |
| TL_iv.RichText | 0x7b9e1801 | textCashtag | verified | TL_iv.java:648 |  |
| TL_iv.RichText | 0x7e6260d7 | textConcat | verified | TL_iv.java:454 |  |
| TL_iv.RichText | 0x9686cb50 | textDiff | verified | TL_iv.java:775 |  |
| TL_iv.RichText | 0x9bf8bb95 | textStrike | verified | TL_iv.java:400 |  |
| TL_iv.RichText | 0x9d2eac97 | textMath | verified | TL_iv.java:554 |  |
| TL_iv.RichText | 0xa26156c0 | textCustomEmoji | verified | TL_iv.java:573 |  |
| TL_iv.RichText | 0xa5b45e2b | textDate | verified | TL_iv.java:736 |  |
| TL_iv.RichText | 0xac6a83aa | textAutoUrl | verified | TL_iv.java:662 |  |
| TL_iv.RichText | 0xafc79cd6 | textButton | verified | TL_iv.java:794 |  |
| TL_iv.RichText | 0xb956812d | textBankCard | verified | TL_iv.java:704 |  |
| TL_iv.RichText | 0xc12622c4 | textUnderline | verified | TL_iv.java:388 |  |
| TL_iv.RichText | 0xc556a45d | textAutoEmail | verified | TL_iv.java:676 |  |
| TL_iv.RichText | 0xc7fb5e01 | textSuperscript | verified | TL_iv.java:478 |  |
| TL_iv.RichText | 0xcd24cf44 | textMention | verified | TL_iv.java:606 |  |
| TL_iv.RichText | 0xd912a59c | textItalic | verified | TL_iv.java:376 |  |
| TL_iv.RichText | 0xdc3d824f | textEmpty | verified | TL_iv.java:343 |  |
| TL_iv.RichText | 0xde5a0dd6 | textEmail | verified | TL_iv.java:440 |  |
| TL_iv.RichText | 0xed6a8504 | textSubscript | verified | TL_iv.java:466 |  |
| TL_iv.Page | 0x556ec7aa | TL_pageFull_layer82 | verified | TL_iv.java:162 |  |
| TL_iv.Page | 0x8dee6c44 | TL_pagePart_layer67 | verified | TL_iv.java:145 |  |
| TL_iv.Page | 0x8e3f9ebe | TL_pagePart_layer82 | verified | TL_iv.java:128 |  |
| TL_iv.Page | 0x98657f0d | TL_page | verified | TL_iv.java:48 |  |
| TL_iv.Page | 0xae891bec | TL_page_layer110 | verified | TL_iv.java:80 |  |
| TL_iv.Page | 0xd7a19d69 | TL_pageFull_layer67 | verified | TL_iv.java:178 |  |
| TL_iv.Page | 0xf199a0a8 | TL_page_layer88 | verified | TL_iv.java:104 |  |
| TL_iv.RichMessage | 0xbaf39d8b | RichMessage | verified | TL_iv.java:195 |  |
| TL_keyboard.KeyboardButtonRow | 0x77608b83 | KeyboardButtonRow | MANUAL, read by hand at TL_keyboard.java:760 | TL_keyboard.java:749 | the deserializer of TL_keyboard.KeyboardButton tries KeyboardInlineButton first (TL_keyboard.java:815): a union in the app's order, read by hand (ruling T2a) |
| TL_keyboard.KeyboardInlineButtonRow | 0x19420af6 | TL_keyboardInlineButtonRow | MANUAL, read by hand at TL_keyboard.java:636 | TL_keyboard.java:632 | the deserializer of TL_keyboard.KeyboardInlineButton tries KeyboardButton first (TL_keyboard.java:735): a union in the app's order, read by hand (ruling T2a) |
| TL_keyboard.KeyboardInlineButtonRow | 0x77608b83 | TL_keyboardInlineButtonRow_layer228 | MANUAL, read by hand at TL_keyboard.java:649 | TL_keyboard.java:645 | a statement on the stream of an unknown form |
| TL_keyboard.KeyboardButton | 0x02b78156 | TL_inputKeyboardButtonRequestPeer_layer228 | verified | TL_keyboard.java:1803 |  |
| TL_keyboard.KeyboardButton | 0x0d0b468c | TL_keyboardButtonRequestPeer_layer168 | verified | TL_keyboard.java:1631 |  |
| TL_keyboard.KeyboardButton | 0x2f67a72f | TL_keyboardButton | verified | TL_keyboard.java:831 |  |
| TL_keyboard.KeyboardButton | 0x417efd8f | TL_keyboardButtonRequestPhone_layer228 | verified | TL_keyboard.java:964 |  |
| TL_keyboard.KeyboardButton | 0x53d7bfd8 | TL_keyboardButtonRequestPeer_layer223 | verified | TL_keyboard.java:1612 |  |
| TL_keyboard.KeyboardButton | 0x5b0f15f5 | TL_keyboardButtonRequestPeer_layer228 | verified | TL_keyboard.java:1578 |  |
| TL_keyboard.KeyboardButton | 0x7a11d782 | TL_keyboardButtonRequestPoll_layer228 | verified | TL_keyboard.java:1259 |  |
| TL_keyboard.KeyboardButton | 0x7d170cff | TL_keyboardButton_layer228 | verified | TL_keyboard.java:927 |  |
| TL_keyboard.KeyboardButton | 0xa0c0505c | TL_keyboardButtonSimpleWebView_layer223 | verified | TL_keyboard.java:1563 |  |
| TL_keyboard.KeyboardButton | 0xa2fa4880 | TL_keyboardButton_layer223 | verified | TL_keyboard.java:950 |  |
| TL_keyboard.KeyboardButton | 0xaa40f94d | TL_keyboardButtonRequestGeoLocation_layer228 | verified | TL_keyboard.java:1083 |  |
| TL_keyboard.KeyboardButton | 0xb16a6c29 | TL_keyboardButtonRequestPhone_layer223 | verified | TL_keyboard.java:987 |  |
| TL_keyboard.KeyboardButton | 0xbbc7515d | TL_keyboardButtonRequestPoll_layer223 | verified | TL_keyboard.java:1294 |  |
| TL_keyboard.KeyboardButton | 0xc9662d05 | TL_inputKeyboardButtonRequestPeer_layer221 | verified | TL_keyboard.java:1846 |  |
| TL_keyboard.KeyboardButton | 0xe15c4370 | TL_keyboardButtonSimpleWebView_layer228 | verified | TL_keyboard.java:1533 |  |
| TL_keyboard.KeyboardButton | 0xfc796b3f | TL_keyboardButtonRequestGeoLocation_layer223 | verified | TL_keyboard.java:1106 |  |
| TL_keyboard.KeyboardInlineButton | 0x0568a748 | TL_keyboardButtonSwitchInline_layer157 | verified | TL_keyboard.java:1783 |  |
| TL_keyboard.KeyboardInlineButton | 0x10b78d29 | TL_keyboardButtonUrlAuth_layer223 | verified | TL_keyboard.java:1160 |  |
| TL_keyboard.KeyboardInlineButton | 0x11c1a322 | TL_keyboardInlineButton | verified | TL_keyboard.java:865 |  |
| TL_keyboard.KeyboardInlineButton | 0x13767230 | TL_keyboardButtonWebView_layer223 | verified | TL_keyboard.java:1518 |  |
| TL_keyboard.KeyboardInlineButton | 0x258aff05 | TL_keyboardButtonUrl_layer223 | verified | TL_keyboard.java:1068 |  |
| TL_keyboard.KeyboardInlineButton | 0x308660c1 | TL_keyboardButtonUserProfile_layer223 | verified | TL_keyboard.java:1473 |  |
| TL_keyboard.KeyboardInlineButton | 0x35bbdb6b | TL_keyboardButtonCallback_layer223 | verified | TL_keyboard.java:1681 |  |
| TL_keyboard.KeyboardInlineButton | 0x3fa53905 | TL_keyboardButtonBuy_layer228 | verified | TL_keyboard.java:1316 |  |
| TL_keyboard.KeyboardInlineButton | 0x50f41ccf | TL_keyboardButtonGame_layer223 | verified | TL_keyboard.java:1024 |  |
| TL_keyboard.KeyboardInlineButton | 0x68013e72 | TL_inputKeyboardButtonUrlAuth_layer228 | verified | TL_keyboard.java:1186 |  |
| TL_keyboard.KeyboardInlineButton | 0x683a5e46 | TL_keyboardButtonCallback_layer117 | verified | TL_keyboard.java:1701 |  |
| TL_keyboard.KeyboardInlineButton | 0x75d2698e | TL_keyboardButtonCopy_layer223 | verified | TL_keyboard.java:1383 |  |
| TL_keyboard.KeyboardInlineButton | 0x7d5e07c7 | TL_inputKeyboardButtonUserProfile_layer228 | verified | TL_keyboard.java:1398 |  |
| TL_keyboard.KeyboardInlineButton | 0x89c590f9 | TL_keyboardButtonGame_layer228 | verified | TL_keyboard.java:1001 |  |
| TL_keyboard.KeyboardInlineButton | 0x93b9fbb5 | TL_keyboardButtonSwitchInline_layer223 | verified | TL_keyboard.java:1756 |  |
| TL_keyboard.KeyboardInlineButton | 0x991399fc | TL_keyboardButtonSwitchInline_layer228 | verified | TL_keyboard.java:1716 |  |
| TL_keyboard.KeyboardInlineButton | 0x9c1c0c55 | TL_keyboardInlineButton_legacy | verified | TL_keyboard.java:899 |  |
| TL_keyboard.KeyboardInlineButton | 0xafd93fbb | TL_keyboardButtonBuy_layer223 | verified | TL_keyboard.java:1339 |  |
| TL_keyboard.KeyboardInlineButton | 0xbcc4af10 | TL_keyboardButtonCopy_layer228 | verified | TL_keyboard.java:1353 |  |
| TL_keyboard.KeyboardInlineButton | 0xc0fd5d09 | TL_keyboardButtonUserProfile_layer228 | verified | TL_keyboard.java:1443 |  |
| TL_keyboard.KeyboardInlineButton | 0xd02e7fd4 | TL_inputKeyboardButtonUrlAuth_layer223 | verified | TL_keyboard.java:1229 |  |
| TL_keyboard.KeyboardInlineButton | 0xd80c25ec | TL_keyboardButtonUrl_layer228 | verified | TL_keyboard.java:1038 |  |
| TL_keyboard.KeyboardInlineButton | 0xe62bc960 | TL_keyboardButtonCallback_layer228 | verified | TL_keyboard.java:1648 |  |
| TL_keyboard.KeyboardInlineButton | 0xe846b1a0 | TL_keyboardButtonWebView_layer228 | verified | TL_keyboard.java:1488 |  |
| TL_keyboard.KeyboardInlineButton | 0xe988037b | TL_inputKeyboardButtonUserProfile_layer223 | verified | TL_keyboard.java:1428 |  |
| TL_keyboard.KeyboardInlineButton | 0xf51006f9 | TL_keyboardButtonUrlAuth_layer228 | verified | TL_keyboard.java:1120 |  |
| TL_keyboard.KeyboardButtonStyle | 0x4fdd3430 | KeyboardButtonStyle | verified | TL_keyboard.java:520 |  |
| TL_keyboard.ButtonType | 0x3fe268fe | TL_inputButtonTypeRequestPeer | verified | TL_keyboard.java:142 |  |
| TL_keyboard.ButtonType | 0x4f58a237 | TL_buttonTypeRequestPeer | verified | TL_keyboard.java:103 |  |
| TL_keyboard.ButtonType | 0x9beee140 | TL_buttonTypeRequestGeoLocation | verified | TL_keyboard.java:70 |  |
| TL_keyboard.ButtonType | 0xaacfff84 | TL_buttonTypeRequestPoll | verified | TL_keyboard.java:81 |  |
| TL_keyboard.ButtonType | 0xc01a597a | TL_buttonTypeSimpleWebView | verified | TL_keyboard.java:127 |  |
| TL_keyboard.ButtonType | 0xc9dd90e9 | TL_buttonTypeDefault | verified | TL_keyboard.java:48 |  |
| TL_keyboard.ButtonType | 0xdf3d36f9 | TL_buttonTypeRequestPhone | verified | TL_keyboard.java:59 |  |
| TL_keyboard.InlineButtonType | 0x2955bc38 | TL_inlineButtonTypeCallback | verified | TL_keyboard.java:274 |  |
| TL_keyboard.InlineButtonType | 0x3bcab5b4 | TL_inlineButtonTypeWebView | verified | TL_keyboard.java:259 |  |
| TL_keyboard.InlineButtonType | 0x3fa33fcf | TL_inlineButtonTypeUserProfile | verified | TL_keyboard.java:346 |  |
| TL_keyboard.InlineButtonType | 0x48bad7a5 | TL_inlineButtonTypeBuy | verified | TL_keyboard.java:306 |  |
| TL_keyboard.InlineButtonType | 0x53f3ce5a | TL_inputInlineButtonTypeUserProfile | verified | TL_keyboard.java:424 |  |
| TL_keyboard.InlineButtonType | 0x5cd3709d | TL_inlineButtonTypeGame | verified | TL_keyboard.java:295 |  |
| TL_keyboard.InlineButtonType | 0x93773ff5 | TL_inlineButtonTypeSwitchInline | verified | TL_keyboard.java:317 |  |
| TL_keyboard.InlineButtonType | 0x9961bcb4 | TL_inputInlineButtonTypeUrlAuth | verified | TL_keyboard.java:387 |  |
| TL_keyboard.InlineButtonType | 0xa438619d | TL_inlineButtonTypeDisabled | verified | TL_keyboard.java:376 |  |
| TL_keyboard.InlineButtonType | 0xb41d3272 | TL_inlineButtonTypeCopy | verified | TL_keyboard.java:361 |  |
| TL_keyboard.InlineButtonType | 0xbfd02da2 | TL_inlineButtonTypeUrlAuth | verified | TL_keyboard.java:230 |  |
| TL_keyboard.InlineButtonType | 0xeca4f8d4 | TL_inlineButtonTypeUrl | verified | TL_keyboard.java:215 |  |
| RequestPeerType | 0x339bef6c | TL_requestPeerTypeBroadcast | verified | TLRPC.java:62646 |  |
| RequestPeerType | 0x3e81e078 | TL_requestPeerTypeCreateBot | verified | TLRPC.java:62682 |  |
| RequestPeerType | 0x5f3b8a00 | TL_requestPeerTypeUser | verified | TLRPC.java:62571 |  |
| RequestPeerType | 0xc9f06e1b | TL_requestPeerTypeChat | verified | TLRPC.java:62601 |  |
| TL_chatAdminRights | 0x5fb224d5 | TL_chatAdminRights | verified | TLRPC.java:4104 |  |
| InputPeer | 0x179be863 | TL_inputPeerChat_layer131 | verified | TLRPC.java:51022 |  |
| InputPeer | 0x17bae2e6 | TL_inputPeerUserFromMessage_layer131 | verified | TLRPC.java:51135 |  |
| InputPeer | 0x20adaef8 | TL_inputPeerChannel_layer131 | verified | TLRPC.java:51103 |  |
| InputPeer | 0x27bcbbfc | TL_inputPeerChannel | verified | TLRPC.java:51035 |  |
| InputPeer | 0x35a95cb9 | TL_inputPeerChat | verified | TLRPC.java:51090 |  |
| InputPeer | 0x7b8e7de6 | TL_inputPeerUser_layer131 | verified | TLRPC.java:51050 |  |
| InputPeer | 0x7da07ec9 | TL_inputPeerSelf | verified | TLRPC.java:50982 |  |
| InputPeer | 0x7f3b18ea | TL_inputPeerEmpty | verified | TLRPC.java:51065 |  |
| InputPeer | 0x9c95f7bb | TL_inputPeerChannelFromMessage_layer131 | verified | TLRPC.java:51118 |  |
| InputPeer | 0xa87b0a1c | TL_inputPeerUserFromMessage | verified | TLRPC.java:50990 |  |
| InputPeer | 0xbd2a0840 | TL_inputPeerChannelFromMessage | verified | TLRPC.java:51073 |  |
| InputPeer | 0xdde8a54c | TL_inputPeerUser | verified | TLRPC.java:51007 |  |
| InlineQueryPeerType | 0x0e3b2d0c | TL_inlineQueryPeerTypeBotPM | verified | TLRPC.java:27274 |  |
| InlineQueryPeerType | 0x3081ed9d | TL_inlineQueryPeerTypeSameBotPM | verified | TLRPC.java:27290 |  |
| InlineQueryPeerType | 0x5ec4be43 | TL_inlineQueryPeerTypeMegagroup | verified | TLRPC.java:27282 |  |
| InlineQueryPeerType | 0x6334ee9a | TL_inlineQueryPeerTypeBroadcast | verified | TLRPC.java:27250 |  |
| InlineQueryPeerType | 0x833c0fac | TL_inlineQueryPeerTypePM | verified | TLRPC.java:27266 |  |
| InlineQueryPeerType | 0xd766c50a | TL_inlineQueryPeerTypeChat | verified | TLRPC.java:27258 |  |
| PollAnswer | 0x199fed96 | TL_inputPollAnswer | verified | TLRPC.java:2929 |  |
| PollAnswer | 0x4b7d786a | TL_pollAnswer | verified | TLRPC.java:2951 |  |
| PollAnswer | 0x6ca9c2e9 | TL_pollAnswer_layer178 | verified | TLRPC.java:2999 |  |
| PollAnswer | 0xff16e2ca | TL_pollAnswer_layer223 | verified | TLRPC.java:2984 |  |
| PollAnswerVoters | 0x3645230a | TL_pollAnswerVoters | verified | TLRPC.java:4636 |  |
| PollAnswerVoters | 0x3b6ddad2 | TL_pollAnswerVoters_layer223 | verified | TLRPC.java:4663 |  |

Completeness: every ID the switch of each type reads at this pin is a row.

| Type | IDs the switch reads | Rows pinned here | Verdict |
|---|---|---|---|
| Message | 47 | 47 | complete |
| Peer | 6 | 6 | complete |
| MessageFwdHeader | 7 | 7 | complete |
| MessageReplyHeader | 9 | 9 | complete |
| MessageEntity | 28 | 28 | complete |
| MessageAction | 116 | 116 | complete |
| PhoneCallDiscardReason | 5 | 5 | complete |
| MessageMedia | 52 | 52 | complete |
| Photo | 8 | 8 | complete |
| Document | 10 | 10 | complete |
| GeoPoint | 4 | 4 | complete |
| WebPage | 12 | 12 | complete |
| User | 29 | 29 | complete |
| Chat | 37 | 37 | complete |
| PhotoSize | 9 | 9 | complete |
| VideoSize | 5 | 5 | complete |
| DocumentAttribute | 17 | 17 | complete |
| FileLocation | 5 | 5 | complete |
| InputStickerSet | 12 | 12 | complete |
| TL_maskCoords | 1 | 1 | complete |
| Video | 6 | 6 | complete |
| Audio | 5 | 5 | complete |
| MessageReplies | 2 | 2 | complete |
| MessageReactions | 4 | 4 | complete |
| Reaction | 4 | 4 | complete |
| MessagePeerReaction | 4 | 4 | complete |
| MessageReactor | 2 | 2 | complete |
| RestrictionReason | 1 | 1 | complete |
| TL_factCheck | 1 | 1 | complete |
| TL_textWithEntities | 1 | 1 | complete |
| SuggestedPost | 1 | 1 | complete |
| TL_stars.StarsAmount | 2 | 2 | complete |
| TL_messages_emojiGameOutcome | 1 | 1 | complete |
| ReactionCount | 2 | 2 | complete |
| WebPageAttribute | 9 | 9 | complete |
| Poll | 6 | 6 | complete |
| PollResults | 6 | 6 | complete |
| ReplyMarkup | 8 | 8 | complete |
| InputUser | 6 | 6 | complete |
| TL_iv.RichText | 32 | 32 | complete |
| TL_iv.Page | 7 | 7 | complete |
| TL_iv.RichMessage | 1 | 1 | complete |
| TL_keyboard.KeyboardButtonRow | 1 | 1 | complete |
| TL_keyboard.KeyboardInlineButtonRow | 2 | 2 | complete |
| TL_keyboard.KeyboardButton | 16 | 16 | complete |
| TL_keyboard.KeyboardInlineButton | 26 | 26 | complete |
| TL_keyboard.KeyboardButtonStyle | 1 | 1 | complete |
| TL_keyboard.ButtonType | 7 | 7 | complete |
| TL_keyboard.InlineButtonType | 12 | 12 | complete |
| RequestPeerType | 4 | 4 | complete |
| TL_chatAdminRights | 1 | 1 | complete |
| InputPeer | 12 | 12 | complete |
| InlineQueryPeerType | 6 | 6 | complete |
| PollAnswer | 4 | 4 | complete |
| PollAnswerVoters | 2 | 2 | complete |

Closure: every object type a `full` or `prefix` row names is a type of the table (design D3).

Closed: 55 types, none named outside them.


## A-11.4.2 (fb2e545101f41303f1e2712de2e7611a9335f1c3)

Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):

| File | SHA-256 |
|---|---|
| `messenger/DatabaseMigrationHelper.java` | 7d5c32421e22b541171f5b500b37f5d8bda0ddfe955d70e6516db2f1a402d087 |
| `messenger/DialogObject.java` | d1669afaa7ba84bb616ffbefbb2a5f1471d8bfea730890765cbb0e12fee6356e |
| `messenger/LocaleController.java` | d4563ceb4a28d789de90eb97617b975a7e2406ace2bc3367906eec71bc311f49 |
| `messenger/MessageObject.java` | 9c60073676f13b8cba0f3272dfdbfdfb901410aa9d35c4e16eb18e5f7b889316 |
| `messenger/MessagesController.java` | 5f23a54991b1d6f7b941ca1c3cf7b4cd197eb4a7d83cb981eb043e84a40c25d5 |
| `messenger/MessagesStorage.java` | 1f927f0172ee7a47c239de3534aac119dad718da5fab8f44c6a0acb385917d11 |
| `messenger/SecretChatHelper.java` | bc8e0deb20e13811b10694c77ca4988b128756200c6bd27fa4b18016308e63e1 |
| `tgnet/NativeByteBuffer.java` | 845efe0b45b1d557a77dbba74fa8b3ddb1c15ce2ba7911ee09f1ccfe4839d083 |
| `tgnet/SerializedData.java` | 789a5892bbac33f4061828603fea5c52e0f49298ae45a2c2c1a61a47e045afa5 |
| `tgnet/TLObject.java` | 09ed9fd44708933d1bb72b99467a19f1e87651bbecd869fe2a37c737a2afc7d2 |
| `tgnet/TLRPC.java` | 4ad82d1ea3acbee9d99f27111eee9d7d6b01c44300af31f523628866df67a06a |
| `tgnet/tl/TL_account.java` | 2dc96701142c1596e99b7eec217393d94bb35e189c13ec731995a863e9999ec5 |
| `tgnet/tl/TL_bots.java` | f5e6b1c3728dc9f51b14907e466d5620947d97a562568e1e83b541fd9e84bdff |
| `tgnet/tl/TL_chatlists.java` | 89e515101235f5afb3cda2e7cbd6a0d0586f4d6f09ac207717fae611dc4d1515 |
| `tgnet/tl/TL_fragment.java` | 213c95b4a98703207729639e044a3aef75d368a3249a9e78aa91a08909dfe207 |
| `tgnet/tl/TL_stars.java` | 2b208ca2ebfbf89dd03c2fe4e61c9a10b39e2e6a03b81ed8c6ecb5677d3a2bb2 |
| `tgnet/tl/TL_stats.java` | 990b320030b9a253a37d4958f2a6c840fadfcafa6bdc18e95468fe986e84c69b |
| `tgnet/tl/TL_stories.java` | d9f155cfb1da9c19613c12b10ac4fd0099e4796a9bbbb71f9d1f84ab1a928129 |
| `ui/Cells/ChatMessageCell.java` | 29752bdf78619040168b19c06863a4f009b616e35685321ab3df2a1ce3bdf007 |
| `ui/Stories/MessageMediaStoryFull.java` | d32150c15955e21c39cf5f47dbc99fe2d95bcb7a740324f9470ca16a037cbc69 |
| `ui/Stories/MessageMediaStoryFull_old.java` | 53134ad4bf0ecbbc7ecf6372aeb6afe854ef30e043f56297706f584820179edf |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | ba9229aa405b9f9e2cb31698dfa46f667f21a7a2280e07fa64abe68f45037b39 |
| `facts.tsv` | dca5975e28eb17b1009b412ef44c14b9f0fc38864749dea18f767723136e6479 |

Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's switch, the class, the source line, the body the selection asks for, and the field order reduced from `readParams`; a `prefix` row is a proper prefix of the reduced body):

| Type | ID | Class | Verdict | Source | Note |
|---|---|---|---|---|---|
| Message | 0x1c9b1027 | TL_message_layer104_2 | verified | TLRPC.java:72674 |  |
| Message | 0x9789dac4 | TL_message_layer104_3 | verified | TLRPC.java:72510 |  |
| Chat | 0x0db12acb | TL_channel_layer167_3 | verified | TLRPC.java:49495 |  |
| Chat | 0xa636a3e2 | TL_channel_layer167_2 | verified | TLRPC.java:49322 |  |

Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv`.

| Pin | Type | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| A-11.4.2 | Message | 0x05f46804 | in-table | TL_messageForwarded_old (TLRPC.java:73365) |
| A-11.4.2 | Message | 0x1c9b1027 | in-table | TL_message_layer104_2 (TLRPC.java:72674) |
| A-11.4.2 | Message | 0x1d86f70e | in-table | TL_messageService_old2 (TLRPC.java:69691) |
| A-11.4.2 | Message | 0x1e4c8a69 | in-table | TL_message_layer175 (TLRPC.java:71097) |
| A-11.4.2 | Message | 0x22eb6aba | in-table | TL_message_old (TLRPC.java:73406) |
| A-11.4.2 | Message | 0x2357bf25 | in-table | TL_message_layer179 (TLRPC.java:70683) |
| A-11.4.2 | Message | 0x286fa604 | in-table | TL_messageService_layer123 (TLRPC.java:73633) |
| A-11.4.2 | Message | 0x2b085862 | in-table | TL_messageService (TLRPC.java:73680) |
| A-11.4.2 | Message | 0x2bebfa86 | in-table | TL_message_old6 (TLRPC.java:72949) |
| A-11.4.2 | Message | 0x38116ee0 | in-table | TL_message_layer169 (TLRPC.java:71483) |
| A-11.4.2 | Message | 0x44f9b43d | in-table | TL_message_layer104 (TLRPC.java:72819) |
| A-11.4.2 | Message | 0x452c0e65 | in-table | TL_message_layer117 (TLRPC.java:72352) |
| A-11.4.2 | Message | 0x555555f8 | in-table | TL_message_secret_old (TLRPC.java:73593) |
| A-11.4.2 | Message | 0x555555f9 | in-table | TL_message_secret_layer72 (TLRPC.java:73519) |
| A-11.4.2 | Message | 0x555555fa | in-table | TL_message_secret (TLRPC.java:73439) |
| A-11.4.2 | Message | 0x567699b3 | in-table | TL_message_old2 (TLRPC.java:73298) |
| A-11.4.2 | Message | 0x58ae39c9 | in-table | TL_message_layer123 (TLRPC.java:72019) |
| A-11.4.2 | Message | 0x5ba66c13 | in-table | TL_message_old7 (TLRPC.java:70082) |
| A-11.4.2 | Message | 0x76bec211 | in-table | TL_message_layer173 (TLRPC.java:71293) |
| A-11.4.2 | Message | 0x83e5de54 | in-table | TL_messageEmpty_layer122 (TLRPC.java:69653) |
| A-11.4.2 | Message | 0x85d6cbe2 | in-table | TL_message_layer135 (TLRPC.java:71667) |
| A-11.4.2 | Message | 0x90a6ca84 | in-table | TL_messageEmpty (TLRPC.java:69668) |
| A-11.4.2 | Message | 0x90dddc11 | in-table | TL_message_layer72 (TLRPC.java:69725) |
| A-11.4.2 | Message | 0x94345242 | in-table | TL_message (TLRPC.java:70239) |
| A-11.4.2 | Message | 0x9789dac4 | in-table | TL_message_layer104_3 (TLRPC.java:72510) |
| A-11.4.2 | Message | 0x9e19a1f6 | in-table | TL_messageService_layer118 (TLRPC.java:73732) |
| A-11.4.2 | Message | 0x9f8d60bb | in-table | TL_messageService_old (TLRPC.java:73337) |
| A-11.4.2 | Message | 0xa367e716 | in-table | TL_messageForwarded_old2 (TLRPC.java:70193) |
| A-11.4.2 | Message | 0xa4e97f37 | in-table | TL_message_layer179 (TLRPC.java:70683) |
| A-11.4.2 | Message | 0xa66c7efc | in-table | TL_message_layer176 (TLRPC.java:70895) |
| A-11.4.2 | Message | 0xa7ab1991 | in-table | TL_message_old3 (TLRPC.java:73241) |
| A-11.4.2 | Message | 0xbce383d2 | in-table | TL_message_layer131 (TLRPC.java:71844) |
| A-11.4.2 | Message | 0xbde09c2e | in-table | TL_message_layer180 (TLRPC.java:70465) |
| A-11.4.2 | Message | 0xc06b9607 | in-table | TL_messageService_layer48 (TLRPC.java:73133) |
| A-11.4.2 | Message | 0xc09be45f | in-table | TL_message_layer68 (TLRPC.java:69845) |
| A-11.4.2 | Message | 0xc3060325 | in-table | TL_message_old4 (TLRPC.java:73177) |
| A-11.4.2 | Message | 0xc992e15c | in-table | TL_message_layer47 (TLRPC.java:69965) |
| A-11.4.2 | Message | 0xf07814c8 | in-table | TL_message_old5 (TLRPC.java:73044) |
| A-11.4.2 | Message | 0xf52e6b7f | in-table | TL_message_layer118 (TLRPC.java:72188) |
| A-11.4.2 | Peer | 0x36c6019a | in-table | TL_peerChat (TLRPC.java:4254) |
| A-11.4.2 | Peer | 0x59511722 | in-table | TL_peerUser (TLRPC.java:4226) |
| A-11.4.2 | Peer | 0x9db1bc6d | in-table | TL_peerUser_layer131 (TLRPC.java:4268) |
| A-11.4.2 | Peer | 0xa2a5371e | in-table | TL_peerChannel (TLRPC.java:4240) |
| A-11.4.2 | Peer | 0xbad0e5bb | in-table | TL_peerChat_layer131 (TLRPC.java:4282) |
| A-11.4.2 | Peer | 0xbddde532 | in-table | TL_peerChannel_layer131 (TLRPC.java:4212) |
| A-11.4.2 | MessageFwdHeader | 0x353a686b | in-table | TL_messageFwdHeader_layer118 (TLRPC.java:53003) |
| A-11.4.2 | MessageFwdHeader | 0x4e4df4bb | in-table | TL_messageFwdHeader (TLRPC.java:52864) |
| A-11.4.2 | MessageFwdHeader | 0x559ebe6d | in-table | TL_messageFwdHeader_layer96 (TLRPC.java:53171) |
| A-11.4.2 | MessageFwdHeader | 0x5f777dce | in-table | TL_messageFwdHeader_layer169 (TLRPC.java:52943) |
| A-11.4.2 | MessageFwdHeader | 0xc786ddcb | in-table | TL_messageFwdHeader_layer68 (TLRPC.java:52828) |
| A-11.4.2 | MessageFwdHeader | 0xec338270 | in-table | TL_messageFwdHeader_layer112 (TLRPC.java:53069) |
| A-11.4.2 | MessageFwdHeader | 0xfadff4ac | in-table | TL_messageFwdHeader_layer72 (TLRPC.java:53129) |
| A-11.4.2 | MessageReplyHeader | 0x0e5af939 | in-table | TL_messageReplyStoryHeader (TLRPC.java:31674) |
| A-11.4.2 | MessageReplyHeader | 0x3d5c1693 | in-table | TL_messageReplyHeader_layer165_2 (TLRPC.java:31855) |
| A-11.4.2 | MessageReplyHeader | 0x6eebcabd | in-table | TL_messageReplyHeader_layer166 (TLRPC.java:31775) |
| A-11.4.2 | MessageReplyHeader | 0x9c98bfc1 | in-table | TL_messageReplyStoryHeader_layer174 (TLRPC.java:31658) |
| A-11.4.2 | MessageReplyHeader | 0xa6d57763 | in-table | TL_messageReplyHeader_layer165 (TLRPC.java:31927) |
| A-11.4.2 | MessageReplyHeader | 0xafbc09db | in-table | TL_messageReplyHeader (TLRPC.java:31689) |
| A-11.4.2 | MessageEntity | 0x020df5d0 | in-table | TL_messageEntityBlockquote_layer180 (TLRPC.java:40609) |
| A-11.4.2 | MessageEntity | 0x208e68c9 | in-table | TL_inputMessageEntityMentionName (TLRPC.java:40491) |
| A-11.4.2 | MessageEntity | 0x28a20571 | in-table | TL_messageEntityCode (TLRPC.java:40557) |
| A-11.4.2 | MessageEntity | 0x32ca960f | in-table | TL_messageEntitySpoiler (TLRPC.java:40458) |
| A-11.4.2 | MessageEntity | 0x352dca58 | in-table | TL_messageEntityMentionName_layer131 (TLRPC.java:40473) |
| A-11.4.2 | MessageEntity | 0x4c4e743f | in-table | TL_messageEntityCashtag (TLRPC.java:40510) |
| A-11.4.2 | MessageEntity | 0x64e475c2 | in-table | TL_messageEntityEmail (TLRPC.java:40360) |
| A-11.4.2 | MessageEntity | 0x6cef8ac7 | in-table | TL_messageEntityBotCommand (TLRPC.java:40344) |
| A-11.4.2 | MessageEntity | 0x6ed02538 | in-table | TL_messageEntityUrl (TLRPC.java:40410) |
| A-11.4.2 | MessageEntity | 0x6f635b0d | in-table | TL_messageEntityHashtag (TLRPC.java:40541) |
| A-11.4.2 | MessageEntity | 0x73924be0 | in-table | TL_messageEntityPre (TLRPC.java:40376) |
| A-11.4.2 | MessageEntity | 0x761e6af4 | in-table | TL_messageEntityBankCard (TLRPC.java:40641) |
| A-11.4.2 | MessageEntity | 0x76a6d327 | in-table | TL_messageEntityTextUrl (TLRPC.java:40326) |
| A-11.4.2 | MessageEntity | 0x826f8b60 | in-table | TL_messageEntityItalic (TLRPC.java:40426) |
| A-11.4.2 | MessageEntity | 0x9b69e34b | in-table | TL_messageEntityPhone (TLRPC.java:40657) |
| A-11.4.2 | MessageEntity | 0x9c4e7e8b | in-table | TL_messageEntityUnderline (TLRPC.java:40625) |
| A-11.4.2 | MessageEntity | 0xbb92ba95 | in-table | TL_messageEntityUnknown (TLRPC.java:40394) |
| A-11.4.2 | MessageEntity | 0xbd610bc9 | in-table | TL_messageEntityBold (TLRPC.java:40525) |
| A-11.4.2 | MessageEntity | 0xbf0693d4 | in-table | TL_messageEntityStrike (TLRPC.java:40573) |
| A-11.4.2 | MessageEntity | 0xc8cf05f8 | in-table | TL_messageEntityCustomEmoji (TLRPC.java:76616) |
| A-11.4.2 | MessageEntity | 0xdc7b1140 | in-table | TL_messageEntityMentionName (TLRPC.java:40672) |
| A-11.4.2 | MessageEntity | 0xf1ccaaac | in-table | TL_messageEntityBlockquote (TLRPC.java:40589) |
| A-11.4.2 | MessageEntity | 0xfa04579d | in-table | TL_messageEntityMention (TLRPC.java:40442) |
| A-11.4.2 | MessageAction | 0x01baa035 | in-table | TL_messageActionPhoneNumberRequest (TLRPC.java:28720) |
| A-11.4.2 | MessageAction | 0x031224c3 | in-table | TL_messageActionChatJoinedByLink (TLRPC.java:28837) |
| A-11.4.2 | MessageAction | 0x08557637 | in-table | TL_messageActionStarGift (TLRPC.java:80228) |
| A-11.4.2 | MessageAction | 0x0d999256 | in-table | TL_messageActionTopicCreate (TLRPC.java:29246) |
| A-11.4.2 | MessageAction | 0x15cefd00 | in-table | TL_messageActionChatAddUser (TLRPC.java:28645) |
| A-11.4.2 | MessageAction | 0x2a9fadc5 | in-table | TL_messageActionGiveawayResults_layer186 (TLRPC.java:80150) |
| A-11.4.2 | MessageAction | 0x31518e9b | in-table | TL_messageActionRequestedPeer (TLRPC.java:77813) |
| A-11.4.2 | MessageAction | 0x332ba9ed | in-table | TL_messageActionGiveawayLaunch_layer186 (TLRPC.java:80117) |
| A-11.4.2 | MessageAction | 0x3c134d7b | in-table | TL_messageActionSetMessagesTTL (TLRPC.java:29078) |
| A-11.4.2 | MessageAction | 0x40699cd0 | in-table | TL_messageActionPaymentSent_layer140 (TLRPC.java:29313) |
| A-11.4.2 | MessageAction | 0x41b3e202 | in-table | TL_messageActionPaymentRefunded (TLRPC.java:28343) |
| A-11.4.2 | MessageAction | 0x45d5b021 | in-table | TL_messageActionGiftStars (TLRPC.java:28372) |
| A-11.4.2 | MessageAction | 0x4792929b | in-table | TL_messageActionScreenshotTaken (TLRPC.java:28711) |
| A-11.4.2 | MessageAction | 0x47dd8079 | in-table | TL_messageActionWebViewDataSentMe (TLRPC.java:28950) |
| A-11.4.2 | MessageAction | 0x488a7337 | in-table | TL_messageActionChatAddUser_layer131 (TLRPC.java:28867) |
| A-11.4.2 | MessageAction | 0x502f92f7 | in-table | TL_messageActionInviteToGroupCall (TLRPC.java:28499) |
| A-11.4.2 | MessageAction | 0x5060a3f4 | in-table | TL_messageActionSetChatWallPaper (TLRPC.java:28996) |
| A-11.4.2 | MessageAction | 0x51bdb021 | in-table | TL_messageActionChatMigrateTo_layer131 (TLRPC.java:28674) |
| A-11.4.2 | MessageAction | 0x55555550 | in-table | TL_messageActionUserJoined (TLRPC.java:29018) |
| A-11.4.2 | MessageAction | 0x55555551 | in-table | TL_messageActionUserUpdatedPhoto (TLRPC.java:29027) |
| A-11.4.2 | MessageAction | 0x55555552 | in-table | TL_messageActionTTLChange (TLRPC.java:29064) |
| A-11.4.2 | MessageAction | 0x55555557 | in-table | TL_messageActionCreatedBroadcastList (TLRPC.java:28910) |
| A-11.4.2 | MessageAction | 0x555555f5 | in-table | TL_messageActionLoginUnknownLocation (TLRPC.java:28327) |
| A-11.4.2 | MessageAction | 0x555555f7 | in-table | TL_messageEncryptedAction (TLRPC.java:28409) |
| A-11.4.2 | MessageAction | 0x56d03994 | in-table | TL_messageActionGiftCode (TLRPC.java:78792) |
| A-11.4.2 | MessageAction | 0x57de635e | in-table | TL_messageActionSuggestProfilePhoto (TLRPC.java:77767) |
| A-11.4.2 | MessageAction | 0x5e3cfc4b | in-table | TL_messageActionChatAddUser_old (TLRPC.java:29050) |
| A-11.4.2 | MessageAction | 0x678c2e09 | in-table | TL_messageActionGiftCode_layer189 (TLRPC.java:78847) |
| A-11.4.2 | MessageAction | 0x6c6274fa | in-table | TL_messageActionGiftPremium (TLRPC.java:29402) |
| A-11.4.2 | MessageAction | 0x76b9f11a | in-table | TL_messageActionInviteToGroupCall_layer131 (TLRPC.java:28919) |
| A-11.4.2 | MessageAction | 0x7a0d7f42 | in-table | TL_messageActionGroupCall (TLRPC.java:28546) |
| A-11.4.2 | MessageAction | 0x7fcb13a8 | in-table | TL_messageActionChatEditPhoto (TLRPC.java:28697) |
| A-11.4.2 | MessageAction | 0x80e11a7f | in-table | TL_messageActionPhoneCall (TLRPC.java:29216) |
| A-11.4.2 | MessageAction | 0x87e2f155 | in-table | TL_messageActionGiveawayResults (TLRPC.java:80125) |
| A-11.4.2 | MessageAction | 0x8f31b327 | in-table | TL_messageActionPaymentSentMe (TLRPC.java:29479) |
| A-11.4.2 | MessageAction | 0x92a72876 | in-table | TL_messageActionGameScore (TLRPC.java:29386) |
| A-11.4.2 | MessageAction | 0x94bd38ed | in-table | TL_messageActionPinMessage (TLRPC.java:29177) |
| A-11.4.2 | MessageAction | 0x95d2ac92 | in-table | TL_messageActionChannelCreate (TLRPC.java:29129) |
| A-11.4.2 | MessageAction | 0x95e3fbef | in-table | TL_messageActionChatDeletePhoto (TLRPC.java:29207) |
| A-11.4.2 | MessageAction | 0x96163f56 | in-table | TL_messageActionPaymentSent (TLRPC.java:29286) |
| A-11.4.2 | MessageAction | 0x98e0d697 | in-table | TL_messageActionGeoProximityReached (TLRPC.java:29186) |
| A-11.4.2 | MessageAction | 0x9bb3ef44 | in-table | TL_messageActionStarGift_layer192 (TLRPC.java:80198) |
| A-11.4.2 | MessageAction | 0x9fbab604 | in-table | TL_messageActionHistoryClear (TLRPC.java:28688) |
| A-11.4.2 | MessageAction | 0xa43f30cc | in-table | TL_messageActionChatDeleteUser (TLRPC.java:28631) |
| A-11.4.2 | MessageAction | 0xa6638b9a | in-table | TL_messageActionChatCreate_layer131 (TLRPC.java:28468) |
| A-11.4.2 | MessageAction | 0xa80f51e4 | in-table | TL_messageActionGiveawayLaunch (TLRPC.java:80094) |
| A-11.4.2 | MessageAction | 0xaa1afbfd | in-table | TL_messageActionSetMessagesTTL_layer149 (TLRPC.java:29102) |
| A-11.4.2 | MessageAction | 0xaa786345 | in-table | TL_messageActionSetChatTheme (TLRPC.java:28568) |
| A-11.4.2 | MessageAction | 0xabe9affe | in-table | TL_messageActionBotAllowed_layer153 (TLRPC.java:29362) |
| A-11.4.2 | MessageAction | 0xb00c47a2 | in-table | TL_messageActionPrizeStars (TLRPC.java:80166) |
| A-11.4.2 | MessageAction | 0xb055eaee | in-table | TL_messageActionChannelMigrateFrom_layer131 (TLRPC.java:28851) |
| A-11.4.2 | MessageAction | 0xb18a431c | in-table | TL_messageActionTopicEdit_layer149 (TLRPC.java:28783) |
| A-11.4.2 | MessageAction | 0xb2ae9b0c | in-table | TL_messageActionChatDeleteUser_layer131 (TLRPC.java:28896) |
| A-11.4.2 | MessageAction | 0xb3a07661 | in-table | TL_messageActionGroupCallScheduled (TLRPC.java:29522) |
| A-11.4.2 | MessageAction | 0xb4c38cb5 | in-table | TL_messageActionWebViewDataSent (TLRPC.java:28968) |
| A-11.4.2 | MessageAction | 0xb5a1ce5a | in-table | TL_messageActionChatEditTitle (TLRPC.java:29272) |
| A-11.4.2 | MessageAction | 0xb6aef7b0 | in-table | TL_messageActionEmpty (TLRPC.java:29377) |
| A-11.4.2 | MessageAction | 0xbc44a927 | in-table | TL_messageActionSetChatWallPaper_layer166 (TLRPC.java:28983) |
| A-11.4.2 | MessageAction | 0xbd47cbad | in-table | TL_messageActionChatCreate (TLRPC.java:28437) |
| A-11.4.2 | MessageAction | 0xc0787d6d | in-table | TL_messageActionSetSameChatWallPaper (TLRPC.java:28729) |
| A-11.4.2 | MessageAction | 0xc0944820 | in-table | TL_messageActionTopicEdit (TLRPC.java:28742) |
| A-11.4.2 | MessageAction | 0xc516d679 | in-table | TL_messageActionBotAllowed (TLRPC.java:29328) |
| A-11.4.2 | MessageAction | 0xc83d6aec | in-table | TL_messageActionGiftPremium_layer189 (TLRPC.java:29437) |
| A-11.4.2 | MessageAction | 0xcc02aa6d | in-table | TL_messageActionBoostApply (TLRPC.java:29464) |
| A-11.4.2 | MessageAction | 0xd2cfdb0e | in-table | TL_messageActionGiftCode_layer167 (TLRPC.java:78903) |
| A-11.4.2 | MessageAction | 0xd95c6154 | in-table | TL_messageActionSecureValuesSent (TLRPC.java:29143) |
| A-11.4.2 | MessageAction | 0xe1037f92 | in-table | TL_messageActionChatMigrateTo (TLRPC.java:28823) |
| A-11.4.2 | MessageAction | 0xe7e75f97 | in-table | TL_messageActionAttachMenuBotAllowed (TLRPC.java:77780) |
| A-11.4.2 | MessageAction | 0xea3948e9 | in-table | TL_messageActionChannelMigrateFrom (TLRPC.java:28530) |
| A-11.4.2 | MessageAction | 0xebbca3cb | in-table | TL_messageActionChatJoinedByRequest (TLRPC.java:28814) |
| A-11.4.2 | MessageAction | 0xf3f25f76 | in-table | TL_messageActionContactSignUp (TLRPC.java:29041) |
| A-11.4.2 | MessageAction | 0xf89cf5e8 | in-table | TL_messageActionChatJoinedByLink_layer131 (TLRPC.java:29115) |
| A-11.4.2 | MessageAction | 0xfae69f56 | in-table | TL_messageActionCustomAction (TLRPC.java:28423) |
| A-11.4.2 | MessageAction | 0xfe77345d | in-table | TL_messageActionRequestedPeer_layer168 (TLRPC.java:77792) |
| A-11.4.2 | PhoneCallDiscardReason | 0x57adc690 | in-table | TL_phoneCallDiscardReasonHangup (TLRPC.java:23723) |
| A-11.4.2 | PhoneCallDiscardReason | 0x85e42301 | in-table | TL_phoneCallDiscardReasonMissed (TLRPC.java:23741) |
| A-11.4.2 | PhoneCallDiscardReason | 0xe095c1a0 | in-table | TL_phoneCallDiscardReasonDisconnect (TLRPC.java:23750) |
| A-11.4.2 | PhoneCallDiscardReason | 0xfaf7e8c9 | in-table | TL_phoneCallDiscardReasonBusy (TLRPC.java:23732) |
| A-11.4.2 | MessageMedia | 0x29632a36 | in-table | TL_messageMediaUnsupported_old (TLRPC.java:8525) |
| A-11.4.2 | MessageMedia | 0x2ec0533f | in-table | TL_messageMediaVenue (TLRPC.java:8758) |
| A-11.4.2 | MessageMedia | 0x2fda2204 | in-table | TL_messageMediaDocument_old (TLRPC.java:8991) |
| A-11.4.2 | MessageMedia | 0x3d8ce53d | in-table | TL_messageMediaPhoto_layer68 (TLRPC.java:9237) |
| A-11.4.2 | MessageMedia | 0x3ded6320 | in-table | TL_messageMediaEmpty (TLRPC.java:8749) |
| A-11.4.2 | MessageMedia | 0x3f7ee58b | in-table | TL_messageMediaDice (TLRPC.java:9312) |
| A-11.4.2 | MessageMedia | 0x4bd6e798 | in-table | TL_messageMediaPoll (TLRPC.java:9050) |
| A-11.4.2 | MessageMedia | 0x4cf4d72d | in-table | TL_messageMediaDocument_layer187 (TLRPC.java:8882) |
| A-11.4.2 | MessageMedia | 0x56e0d474 | in-table | TL_messageMediaGeo (TLRPC.java:9343) |
| A-11.4.2 | MessageMedia | 0x58260664 | in-table | TL_messageMediaGiveaway_layer167 (TLRPC.java:79592) |
| A-11.4.2 | MessageMedia | 0x5bcf1675 | in-table | TL_messageMediaVideo_layer45 (TLRPC.java:9253) |
| A-11.4.2 | MessageMedia | 0x5e7d2f39 | in-table | TL_messageMediaContact_layer81 (TLRPC.java:9217) |
| A-11.4.2 | MessageMedia | 0x638fe46b | in-table | TL_messageMediaDice_layer111 (TLRPC.java:9330) |
| A-11.4.2 | MessageMedia | 0x68cb6283 | in-table | TL_messageMediaStory (TLRPC.java:78661) |
| A-11.4.2 | MessageMedia | 0x695150d7 | in-table | TL_messageMediaPhoto (TLRPC.java:9021) |
| A-11.4.2 | MessageMedia | 0x70322949 | in-table | TL_messageMediaContact (TLRPC.java:9290) |
| A-11.4.2 | MessageMedia | 0x7912b71f | in-table | TL_messageMediaVenue_layer71 (TLRPC.java:8787) |
| A-11.4.2 | MessageMedia | 0x7c3c2609 | in-table | TL_messageMediaGeoLive_layer119 (TLRPC.java:9187) |
| A-11.4.2 | MessageMedia | 0x7c4414d3 | in-table | TL_messageMediaDocument_layer74 (TLRPC.java:8957) |
| A-11.4.2 | MessageMedia | 0x84551347 | in-table | TL_messageMediaInvoice_layer145 (TLRPC.java:8699) |
| A-11.4.2 | MessageMedia | 0x9cb070d7 | in-table | TL_messageMediaDocument_layer159 (TLRPC.java:8924) |
| A-11.4.2 | MessageMedia | 0x9f84f49e | in-table | TL_messageMediaUnsupported (TLRPC.java:8740) |
| A-11.4.2 | MessageMedia | 0xa2d24290 | in-table | TL_messageMediaVideo_old (TLRPC.java:8809) |
| A-11.4.2 | MessageMedia | 0xa32dd600 | in-table | TL_messageMediaWebPage_layer165 (TLRPC.java:9379) |
| A-11.4.2 | MessageMedia | 0xa8852491 | in-table | TL_messageMediaPaidMedia (TLRPC.java:79651) |
| A-11.4.2 | MessageMedia | 0xaa073beb | in-table | TL_messageMediaGiveaway (TLRPC.java:79438) |
| A-11.4.2 | MessageMedia | 0xb5223b0f | in-table | TL_messageMediaPhoto_layer74 (TLRPC.java:9068) |
| A-11.4.2 | MessageMedia | 0xb940c666 | in-table | TL_messageMediaGeoLive (TLRPC.java:9157) |
| A-11.4.2 | MessageMedia | 0xc6991068 | in-table | TL_messageMediaGiveawayResults_layer186 (TLRPC.java:79380) |
| A-11.4.2 | MessageMedia | 0xc6b68300 | in-table | TL_messageMediaAudio_layer45 (TLRPC.java:8539) |
| A-11.4.2 | MessageMedia | 0xc79aee1d | in-table | MessageMediaStoryFull (MessageMediaStoryFull.java:9) |
| A-11.4.2 | MessageMedia | 0xc79aee1f | in-table | MessageMediaStoryFull_old (MessageMediaStoryFull_old.java:7) |
| A-11.4.2 | MessageMedia | 0xc8c45a2a | in-table | TL_messageMediaPhoto_old (TLRPC.java:8553) |
| A-11.4.2 | MessageMedia | 0xcbb20d88 | in-table | TL_messageMediaStory_layer162 (TLRPC.java:78686) |
| A-11.4.2 | MessageMedia | 0xcbf24940 | in-table | TL_messageMediaContact_layer131 (TLRPC.java:9269) |
| A-11.4.2 | MessageMedia | 0xceaa3ea1 | in-table | TL_messageMediaGiveawayResults (TLRPC.java:79299) |
| A-11.4.2 | MessageMedia | 0xdaad85b0 | in-table | TL_messageMediaGiveaway_layer186 (TLRPC.java:79525) |
| A-11.4.2 | MessageMedia | 0xdd570bd5 | in-table | TL_messageMediaDocument (TLRPC.java:8823) |
| A-11.4.2 | MessageMedia | 0xddf10c3b | in-table | TL_messageMediaWebPage (TLRPC.java:9356) |
| A-11.4.2 | MessageMedia | 0xf3e02ea8 | in-table | TL_messageMediaDocument_layer68 (TLRPC.java:9005) |
| A-11.4.2 | MessageMedia | 0xf6a548d3 | in-table | TL_messageMediaInvoice (TLRPC.java:8651) |
| A-11.4.2 | MessageMedia | 0xfdb19008 | in-table | TL_messageMediaGame (TLRPC.java:9203) |
| A-11.4.2 | Photo | 0x22b56751 | in-table | TL_photo_old (TLRPC.java:40934) |
| A-11.4.2 | Photo | 0x2331b22d | in-table | TL_photoEmpty (TLRPC.java:40920) |
| A-11.4.2 | Photo | 0x9288dd29 | in-table | TL_photo_layer82 (TLRPC.java:41098) |
| A-11.4.2 | Photo | 0x9c477dd8 | in-table | TL_photo_layer97 (TLRPC.java:40793) |
| A-11.4.2 | Photo | 0xc3838076 | in-table | TL_photo_old2 (TLRPC.java:40877) |
| A-11.4.2 | Photo | 0xcded42fe | in-table | TL_photo_layer55 (TLRPC.java:40838) |
| A-11.4.2 | Photo | 0xd07504a5 | in-table | TL_photo_layer115 (TLRPC.java:40979) |
| A-11.4.2 | Photo | 0xfb197a65 | in-table | TL_photo (TLRPC.java:41026) |
| A-11.4.2 | Document | 0x1e87342b | in-table | TL_document_layer142 (TLRPC.java:32792) |
| A-11.4.2 | Document | 0x36f8c871 | in-table | TL_documentEmpty (TLRPC.java:32995) |
| A-11.4.2 | Document | 0x55555556 | in-table | TL_documentEncrypted_old (TLRPC.java:32660) |
| A-11.4.2 | Document | 0x55555558 | in-table | TL_documentEncrypted (TLRPC.java:33009) |
| A-11.4.2 | Document | 0x59534e4c | in-table | TL_document_layer92 (TLRPC.java:32562) |
| A-11.4.2 | Document | 0x87232bc7 | in-table | TL_document_layer82 (TLRPC.java:32611) |
| A-11.4.2 | Document | 0x8fd4c4d8 | in-table | TL_document (TLRPC.java:32694) |
| A-11.4.2 | Document | 0x9ba29cc1 | in-table | TL_document_layer113 (TLRPC.java:32891) |
| A-11.4.2 | Document | 0x9efc6326 | in-table | TL_document_old (TLRPC.java:32965) |
| A-11.4.2 | Document | 0xf9a39f4f | in-table | TL_document_layer53 (TLRPC.java:33060) |
| A-11.4.2 | GeoPoint | 0x0296f104 | in-table | TL_geoPoint_layer119 (TLRPC.java:5992) |
| A-11.4.2 | GeoPoint | 0x1117dd5f | in-table | TL_geoPointEmpty (TLRPC.java:6026) |
| A-11.4.2 | GeoPoint | 0x2049d70c | in-table | TL_geoPoint_layer81 (TLRPC.java:6010) |
| A-11.4.2 | GeoPoint | 0xb2a2f663 | in-table | TL_geoPoint (TLRPC.java:6035) |
| A-11.4.2 | WebPage | 0x211a1788 | in-table | TL_webPageEmpty (TLRPC.java:22996) |
| A-11.4.2 | WebPage | 0x5f07b4bc | in-table | TL_webPage_layer104 (TLRPC.java:22810) |
| A-11.4.2 | WebPage | 0x7311ca11 | in-table | TL_webPageNotModified (TLRPC.java:23094) |
| A-11.4.2 | WebPage | 0x85849473 | in-table | TL_webPageNotModified_layer110 (TLRPC.java:23085) |
| A-11.4.2 | WebPage | 0xa31ea0b5 | in-table | TL_webPage_old (TLRPC.java:22910) |
| A-11.4.2 | WebPage | 0xb0d13e47 | in-table | TL_webPagePending (TLRPC.java:23061) |
| A-11.4.2 | WebPage | 0xc586da1c | in-table | TL_webPagePending_layer165 (TLRPC.java:23046) |
| A-11.4.2 | WebPage | 0xca820ed7 | in-table | TL_webPage_layer58 (TLRPC.java:23115) |
| A-11.4.2 | WebPage | 0xd41a5167 | in-table | TL_webPageUrlPending (TLRPC.java:23032) |
| A-11.4.2 | WebPage | 0xe89c45b2 | in-table | TL_webPage (TLRPC.java:22561) |
| A-11.4.2 | WebPage | 0xeb1477e8 | in-table | TL_webPageEmpty_layer165 (TLRPC.java:23018) |
| A-11.4.2 | WebPage | 0xfa64e172 | in-table | TL_webPage_layer107 (TLRPC.java:22687) |
| A-11.4.2 | User | 0x075cf7a8 | in-table | TL_userForeign_old2 (TLRPC.java:27309) |
| A-11.4.2 | User | 0x1c60e608 | in-table | TL_userSelf_old3 (TLRPC.java:27199) |
| A-11.4.2 | User | 0x200250ba | in-table | TL_userEmpty_layer131 (TLRPC.java:27245) |
| A-11.4.2 | User | 0x215c4438 | in-table | TL_user_layer184 (TLRPC.java:25872) |
| A-11.4.2 | User | 0x22e49072 | in-table | TL_user_old (TLRPC.java:27479) |
| A-11.4.2 | User | 0x22e8ceb0 | in-table | TL_userRequest_old (TLRPC.java:27259) |
| A-11.4.2 | User | 0x2e13f4c3 | in-table | TL_user_layer104 (TLRPC.java:27065) |
| A-11.4.2 | User | 0x3ff6ecb0 | in-table | TL_user_layer144 (TLRPC.java:26800) |
| A-11.4.2 | User | 0x4fe9cce0 | in-table | TL_user_layer185 (TLRPC.java:25654) |
| A-11.4.2 | User | 0x5214c89d | in-table | TL_userForeign_old (TLRPC.java:27285) |
| A-11.4.2 | User | 0x5d99adee | in-table | TL_user_layer147 (TLRPC.java:26658) |
| A-11.4.2 | User | 0x7007b451 | in-table | TL_userSelf_old2 (TLRPC.java:27561) |
| A-11.4.2 | User | 0x720535ec | in-table | TL_userSelf_old (TLRPC.java:27173) |
| A-11.4.2 | User | 0x83314fca | in-table | TL_user (TLRPC.java:25434) |
| A-11.4.2 | User | 0x8f97c628 | in-table | TL_user_layer159 (TLRPC.java:26480) |
| A-11.4.2 | User | 0x938458c1 | in-table | TL_user_layer131 (TLRPC.java:26934) |
| A-11.4.2 | User | 0xabb5f120 | in-table | TL_user_layer165 (TLRPC.java:26290) |
| A-11.4.2 | User | 0xb29ad7cc | in-table | TL_userDeleted_old (TLRPC.java:27363) |
| A-11.4.2 | User | 0xcab35e18 | in-table | TL_userContact_old2 (TLRPC.java:25366) |
| A-11.4.2 | User | 0xd10d979a | in-table | TL_user_layer65 (TLRPC.java:27381) |
| A-11.4.2 | User | 0xd3bc4b7a | in-table | TL_userEmpty (TLRPC.java:25420) |
| A-11.4.2 | User | 0xd6016d7a | in-table | TL_userDeleted_old2 (TLRPC.java:27225) |
| A-11.4.2 | User | 0xd9ccc4ef | in-table | TL_userRequest_old2 (TLRPC.java:27335) |
| A-11.4.2 | User | 0xeb602f25 | in-table | TL_user_layer166 (TLRPC.java:26084) |
| A-11.4.2 | User | 0xf2fb8319 | in-table | TL_userContact_old (TLRPC.java:25394) |
| A-11.4.2 | Chat | 0x07328bdb | in-table | TL_chatForbidden_layer131 (TLRPC.java:50862) |
| A-11.4.2 | Chat | 0x0aadfc8f | in-table | TL_channel_layer185 (TLRPC.java:49867) |
| A-11.4.2 | Chat | 0x0cb44b1c | in-table | TL_channel_layer72 (TLRPC.java:51321) |
| A-11.4.2 | Chat | 0x0db12acb | in-table | TL_channel_layer167_3 (TLRPC.java:49495) |
| A-11.4.2 | Chat | 0x17d493d5 | in-table | TL_channelForbidden (TLRPC.java:49165) |
| A-11.4.2 | Chat | 0x1981ea7e | in-table | TL_channel_layer166 (TLRPC.java:50219) |
| A-11.4.2 | Chat | 0x289da732 | in-table | TL_channelForbidden_layer131 (TLRPC.java:49195) |
| A-11.4.2 | Chat | 0x29562865 | in-table | TL_chatEmpty (TLRPC.java:73993) |
| A-11.4.2 | Chat | 0x2d85832c | in-table | TL_channelForbidden_layer52 (TLRPC.java:49304) |
| A-11.4.2 | Chat | 0x3bda1bde | in-table | TL_chat_layer131 (TLRPC.java:49109) |
| A-11.4.2 | Chat | 0x41cbf256 | in-table | TL_chat (TLRPC.java:49048) |
| A-11.4.2 | Chat | 0x450b7115 | in-table | TL_channel_layer77 (TLRPC.java:51244) |
| A-11.4.2 | Chat | 0x4b1b7506 | in-table | TL_channel_layer48 (TLRPC.java:49249) |
| A-11.4.2 | Chat | 0x4df30834 | in-table | TL_channel_layer104 (TLRPC.java:51128) |
| A-11.4.2 | Chat | 0x6592a1a7 | in-table | TL_chatForbidden (TLRPC.java:50846) |
| A-11.4.2 | Chat | 0x678e9587 | in-table | TL_channel_old (TLRPC.java:50939) |
| A-11.4.2 | Chat | 0x6e9c9bc7 | in-table | TL_chat_old (TLRPC.java:50986) |
| A-11.4.2 | Chat | 0x7312bc48 | in-table | TL_chat_old2 (TLRPC.java:49015) |
| A-11.4.2 | Chat | 0x8261ac61 | in-table | TL_channel_layer147 (TLRPC.java:50723) |
| A-11.4.2 | Chat | 0x83259464 | in-table | TL_channel_layer161 (TLRPC.java:50566) |
| A-11.4.2 | Chat | 0x8537784f | in-table | TL_channelForbidden_layer67 (TLRPC.java:49225) |
| A-11.4.2 | Chat | 0x8e87ccd8 | in-table | TL_channel_layer167 (TLRPC.java:50052) |
| A-11.4.2 | Chat | 0x94f592db | in-table | TL_channel_layer165 (TLRPC.java:50396) |
| A-11.4.2 | Chat | 0x9ba2d800 | in-table | TL_chatEmpty_layer131 (TLRPC.java:74009) |
| A-11.4.2 | Chat | 0xa14dca52 | in-table | TL_channel_layer67 (TLRPC.java:50878) |
| A-11.4.2 | Chat | 0xa636a3e2 | in-table | TL_channel_layer167_2 (TLRPC.java:49322) |
| A-11.4.2 | Chat | 0xc88974ac | in-table | TL_channel_layer92 (TLRPC.java:51393) |
| A-11.4.2 | Chat | 0xd31a961e | in-table | TL_channel_layer131 (TLRPC.java:51011) |
| A-11.4.2 | Chat | 0xd91cdd54 | in-table | TL_chat_layer92 (TLRPC.java:51470) |
| A-11.4.2 | Chat | 0xfb0ccc41 | in-table | TL_chatForbidden_old (TLRPC.java:48997) |
| A-11.4.2 | Chat | 0xfe4478bd | in-table | TL_channel (TLRPC.java:49674) |
| A-11.4.2 | PhotoSize | 0x021e1ad6 | in-table | TL_photoCachedSize (TLRPC.java:53635) |
| A-11.4.2 | PhotoSize | 0x0e17e23c | in-table | TL_photoSizeEmpty (TLRPC.java:53579) |
| A-11.4.2 | PhotoSize | 0x5aa86a51 | in-table | TL_photoSizeProgressive_layer127 (TLRPC.java:53517) |
| A-11.4.2 | PhotoSize | 0x75c78e60 | in-table | TL_photoSize (TLRPC.java:53615) |
| A-11.4.2 | PhotoSize | 0x77bfb61b | in-table | TL_photoSize_layer127 (TLRPC.java:53557) |
| A-11.4.2 | PhotoSize | 0xd8214d41 | in-table | TL_photoPathSize (TLRPC.java:68490) |
| A-11.4.2 | PhotoSize | 0xe0b0bc2e | in-table | TL_photoStrippedSize (TLRPC.java:53500) |
| A-11.4.2 | PhotoSize | 0xe9a734fa | in-table | TL_photoCachedSize_layer127 (TLRPC.java:53593) |
| A-11.4.2 | PhotoSize | 0xfa3efb95 | in-table | TL_photoSizeProgressive (TLRPC.java:53655) |
| A-11.4.2 | VideoSize | 0x0da082fe | in-table | TL_videoSizeStickerMarkup (TLRPC.java:21514) |
| A-11.4.2 | VideoSize | 0x435bb987 | in-table | TL_videoSize_layer115 (TLRPC.java:21603) |
| A-11.4.2 | VideoSize | 0xde33b094 | in-table | TL_videoSize (TLRPC.java:21655) |
| A-11.4.2 | VideoSize | 0xe831c556 | in-table | TL_videoSize_layer127 (TLRPC.java:21625) |
| A-11.4.2 | VideoSize | 0xf85c413c | in-table | TL_videoSizeEmojiMarkup (TLRPC.java:21482) |
| A-11.4.2 | DocumentAttribute | 0x051448e5 | in-table | TL_documentAttributeAudio_old (TLRPC.java:1835) |
| A-11.4.2 | DocumentAttribute | 0x0ef02ce6 | in-table | TL_documentAttributeVideo_layer159 (TLRPC.java:2009) |
| A-11.4.2 | DocumentAttribute | 0x11b58939 | in-table | TL_documentAttributeAnimated (TLRPC.java:1875) |
| A-11.4.2 | DocumentAttribute | 0x15590068 | in-table | TL_documentAttributeFilename (TLRPC.java:1884) |
| A-11.4.2 | DocumentAttribute | 0x17399fad | in-table | TL_documentAttributeVideo_layer187 (TLRPC.java:1941) |
| A-11.4.2 | DocumentAttribute | 0x3a556302 | in-table | TL_documentAttributeSticker_layer55 (TLRPC.java:1819) |
| A-11.4.2 | DocumentAttribute | 0x43c57c48 | in-table | TL_documentAttributeVideo (TLRPC.java:1898) |
| A-11.4.2 | DocumentAttribute | 0x5910cccb | in-table | TL_documentAttributeVideo_layer65 (TLRPC.java:2032) |
| A-11.4.2 | DocumentAttribute | 0x6319d612 | in-table | TL_documentAttributeSticker (TLRPC.java:1849) |
| A-11.4.2 | DocumentAttribute | 0x6c37c15c | in-table | TL_documentAttributeImageSize (TLRPC.java:2103) |
| A-11.4.2 | DocumentAttribute | 0x9801d2f7 | in-table | TL_documentAttributeHasStickers (TLRPC.java:2080) |
| A-11.4.2 | DocumentAttribute | 0x9852f9c6 | in-table | TL_documentAttributeAudio (TLRPC.java:2119) |
| A-11.4.2 | DocumentAttribute | 0x994c9882 | in-table | TL_documentAttributeSticker_old2 (TLRPC.java:2089) |
| A-11.4.2 | DocumentAttribute | 0xd38ff1c2 | in-table | TL_documentAttributeVideo_layer184 (TLRPC.java:1978) |
| A-11.4.2 | DocumentAttribute | 0xded218e0 | in-table | TL_documentAttributeAudio_layer45 (TLRPC.java:2050) |
| A-11.4.2 | DocumentAttribute | 0xfb0a5727 | in-table | TL_documentAttributeSticker_old (TLRPC.java:2068) |
| A-11.4.2 | DocumentAttribute | 0xfd149899 | in-table | TL_documentAttributeCustomEmoji (TLRPC.java:2155) |
| A-11.4.2 | FileLocation | 0x091d11eb | in-table | TL_fileLocation_layer97 (TLRPC.java:53302) |
| A-11.4.2 | FileLocation | 0x53d69076 | in-table | TL_fileLocation_layer82 (TLRPC.java:53282) |
| A-11.4.2 | FileLocation | 0x55555554 | in-table | TL_fileEncryptedLocation (TLRPC.java:53340) |
| A-11.4.2 | FileLocation | 0x7c596b46 | in-table | TL_fileLocationUnavailable (TLRPC.java:53264) |
| A-11.4.2 | FileLocation | 0xbc7fc6cd | in-table | TL_fileLocationToBeDeprecated (TLRPC.java:53324) |
| A-11.4.2 | InputStickerSet | 0x028703c8 | in-table | TL_inputStickerSetAnimatedEmoji (TLRPC.java:35620) |
| A-11.4.2 | InputStickerSet | 0x04c4d4ce | in-table | TL_inputStickerSetEmojiGenericAnimations (TLRPC.java:35629) |
| A-11.4.2 | InputStickerSet | 0x29d0f5ee | in-table | TL_inputStickerSetEmojiDefaultStatuses (TLRPC.java:35557) |
| A-11.4.2 | InputStickerSet | 0x44c1f8e9 | in-table | TL_inputStickerSetEmojiDefaultTopicIcons (TLRPC.java:35638) |
| A-11.4.2 | InputStickerSet | 0x49748553 | in-table | TL_inputStickerSetEmojiChannelDefaultStatuses (TLRPC.java:35548) |
| A-11.4.2 | InputStickerSet | 0x861cc8a0 | in-table | TL_inputStickerSetShortName (TLRPC.java:35606) |
| A-11.4.2 | InputStickerSet | 0x9de7a269 | in-table | TL_inputStickerSetID (TLRPC.java:35590) |
| A-11.4.2 | InputStickerSet | 0xc88b3b02 | in-table | TL_inputStickerSetPremiumGifts (TLRPC.java:35539) |
| A-11.4.2 | InputStickerSet | 0xe67f520e | in-table | TL_inputStickerSetDice (TLRPC.java:35566) |
| A-11.4.2 | InputStickerSet | 0xffb62b95 | in-table | TL_inputStickerSetEmpty (TLRPC.java:35581) |
| A-11.4.2 | TL_maskCoords | 0xaed6dbb2 | in-table | TL_maskCoords (TLRPC.java:34323) |
| A-11.4.2 | Video | 0x388fa391 | in-table | TL_video_old2 (TLRPC.java:31259) |
| A-11.4.2 | Video | 0x55555553 | in-table | TL_videoEncrypted (TLRPC.java:31187) |
| A-11.4.2 | Video | 0x5a04a49f | in-table | TL_video_old (TLRPC.java:31225) |
| A-11.4.2 | Video | 0xc10658a8 | in-table | TL_videoEmpty_layer45 (TLRPC.java:31295) |
| A-11.4.2 | Video | 0xee9f4a4d | in-table | TL_video_old3 (TLRPC.java:31123) |
| A-11.4.2 | Video | 0xf72887d3 | in-table | TL_video_layer45 (TLRPC.java:31155) |
| A-11.4.2 | Audio | 0x427425e7 | in-table | TL_audio_old (TLRPC.java:11263) |
| A-11.4.2 | Audio | 0x555555f6 | in-table | TL_audioEncrypted (TLRPC.java:11289) |
| A-11.4.2 | Audio | 0x586988d8 | in-table | TL_audioEmpty_layer45 (TLRPC.java:11223) |
| A-11.4.2 | Audio | 0xc7ac6496 | in-table | TL_audio_old2 (TLRPC.java:11319) |
| A-11.4.2 | Audio | 0xf9e35055 | in-table | TL_audio_layer45 (TLRPC.java:11237) |
| A-11.4.2 | MessageReplies | 0x4128faac | in-table | TL_messageReplies_layer131 (TLRPC.java:11594) |
| A-11.4.2 | MessageReplies | 0x83d60fc2 | in-table | TL_messageReplies (TLRPC.java:11657) |
| A-11.4.2 | MessageReactions | 0x087b6e36 | in-table | TL_messageReactionsOld (TLRPC.java:30962) |
| A-11.4.2 | MessageReactions | 0x0a339f0b | in-table | TL_messageReactions (TLRPC.java:30667) |
| A-11.4.2 | MessageReactions | 0x4f2b9479 | in-table | TL_messageReactions_layer185 (TLRPC.java:30757) |
| A-11.4.2 | MessageReactions | 0xb87a24d1 | in-table | TL_messageReactions_layer137 (TLRPC.java:31039) |
| A-11.4.2 | Reaction | 0x1b2286b8 | in-table | TL_reactionEmoji (TLRPC.java:59495) |
| A-11.4.2 | Reaction | 0x523da4eb | in-table | TL_reactionPaid (TLRPC.java:59486) |
| A-11.4.2 | Reaction | 0x79f5d419 | in-table | TL_reactionEmpty (TLRPC.java:59477) |
| A-11.4.2 | Reaction | 0x8935fc73 | in-table | TL_reactionCustomEmoji (TLRPC.java:59462) |
| A-11.4.2 | MessagePeerReaction | 0x51b67eff | in-table | TL_messagePeerReaction_layer144 (TLRPC.java:2889) |
| A-11.4.2 | MessagePeerReaction | 0x8c79b63c | in-table | TL_messagePeerReaction (TLRPC.java:2845) |
| A-11.4.2 | MessagePeerReaction | 0x932844fa | in-table | TL_messagePeerReaction_layer137 (TLRPC.java:2914) |
| A-11.4.2 | MessagePeerReaction | 0xb156fe9c | in-table | TL_messagePeerReaction_layer154 (TLRPC.java:2868) |
| A-11.4.2 | MessageReactor | 0x4ba3a95a | in-table | TL_messageReactor (TLRPC.java:30583) |
| A-11.4.2 | MessageReactor | 0xef00d448 | in-table | TL_messageReactor_old (TLRPC.java:30610) |
| A-11.4.2 | RestrictionReason | 0xd072acb4 | in-table | TL_reastrictionReason (TLRPC.java:51532) |
| A-11.4.2 | TL_factCheck | 0xb89bfccf | in-table | TL_factCheck (TLRPC.java:83712) |
| A-11.4.2 | TL_textWithEntities | 0x751f3146 | in-table | TL_textWithEntities (TLRPC.java:54083) |
| A-11.4.2 | SuggestedPost | - | type not defined at this pin | - |
| A-11.4.2 | TL_stars.StarsAmount | - | type not defined at this pin | - |
| A-11.4.2 | TL_messages_emojiGameOutcome | - | type not defined at this pin | - |
| A-11.4.2 | ReactionCount | 0xa3d1cb80 | in-table | TL_reactionCount (TLRPC.java:54268) |
| A-11.4.2 | WebPageAttribute | 0x2e94c3e7 | in-table | TL_webPageAttributeStory (TLRPC.java:12139) |
| A-11.4.2 | WebPageAttribute | 0x50cc03d3 | in-table | TL_webPageAttributeStickerSet (TLRPC.java:12166) |
| A-11.4.2 | WebPageAttribute | 0x54b56617 | in-table | TL_webPageAttributeTheme (TLRPC.java:12239) |
| A-11.4.2 | WebPageAttribute | 0x939a4671 | in-table | TL_webPageAttributeStory_layer162 (TLRPC.java:12209) |
| A-11.4.2 | Poll | 0x58747131 | in-table | TL_poll (TLRPC.java:42371) |
| A-11.4.2 | Poll | 0x86e18161 | in-table | TL_poll_layer178 (TLRPC.java:42429) |
| A-11.4.2 | Poll | 0xaf746786 | in-table | TL_poll_toDelete (TLRPC.java:42489) |
| A-11.4.2 | Poll | 0xd5529d06 | in-table | TL_poll_layer111 (TLRPC.java:42543) |
| A-11.4.2 | PollResults | 0x5755785a | in-table | TL_pollResults_layer108 (TLRPC.java:4686) |
| A-11.4.2 | PollResults | 0x7adf2420 | in-table | TL_pollResults (TLRPC.java:4904) |
| A-11.4.2 | PollResults | 0xbadcc1a3 | in-table | TL_pollResults_layer131 (TLRPC.java:4803) |
| A-11.4.2 | PollResults | 0xc87024a2 | in-table | TL_pollResults_layer111 (TLRPC.java:4733) |
| A-11.4.2 | PollResults | 0xdcb82ea3 | in-table | TL_pollResults_layer158 (TLRPC.java:5006) |
| A-11.4.2 | ReplyMarkup | 0x3502758c | in-table | TL_replyKeyboardMarkup_layer129 (TLRPC.java:12018) |
| A-11.4.2 | ReplyMarkup | 0x48a30254 | in-table | TL_replyInlineMarkup (TLRPC.java:12077) |
| A-11.4.2 | ReplyMarkup | 0x85dd99d1 | in-table | TL_replyKeyboardMarkup (TLRPC.java:11929) |
| A-11.4.2 | ReplyMarkup | 0x86b40b08 | in-table | TL_replyKeyboardForceReply (TLRPC.java:11994) |
| A-11.4.2 | ReplyMarkup | 0xa03e5b85 | in-table | TL_replyKeyboardHide (TLRPC.java:11978) |
| A-11.4.2 | ReplyMarkup | 0xf4108aa0 | in-table | TL_replyKeyboardForceReply_layer129 (TLRPC.java:12059) |
| A-11.4.2 | InputUser | 0x1da448e2 | in-table | TL_inputUserFromMessage (TLRPC.java:21080) |
| A-11.4.2 | InputUser | 0xb98886cf | in-table | TL_inputUserEmpty (TLRPC.java:21071) |
| A-11.4.2 | InputUser | 0xf21158c6 | in-table | TL_inputUser (TLRPC.java:21055) |
| A-11.4.2 | InputUser | 0xf7c1b13f | in-table | TL_inputUserSelf (TLRPC.java:21046) |
| A-11.4.2 | TL_iv.RichText | 0x034b8621 | in-table | TL_textMarked (TLRPC.java:2286) |
| A-11.4.2 | TL_iv.RichText | 0x081ccf4f | in-table | TL_textImage (TLRPC.java:2209) |
| A-11.4.2 | TL_iv.RichText | 0x1ccb966a | in-table | TL_textPhone (TLRPC.java:2179) |
| A-11.4.2 | TL_iv.RichText | 0x35553762 | in-table | TL_textAnchor (TLRPC.java:2257) |
| A-11.4.2 | TL_iv.RichText | 0x3c2884c1 | in-table | TL_textUrl (TLRPC.java:2240) |
| A-11.4.2 | TL_iv.RichText | 0x6724abc4 | in-table | TL_textBold (TLRPC.java:2375) |
| A-11.4.2 | TL_iv.RichText | 0x6c3f19b9 | in-table | TL_textFixed (TLRPC.java:2299) |
| A-11.4.2 | TL_iv.RichText | 0x744694e0 | in-table | TL_textPlain (TLRPC.java:2327) |
| A-11.4.2 | TL_iv.RichText | 0x7e6260d7 | in-table | TL_textConcat (TLRPC.java:2342) |
| A-11.4.2 | TL_iv.RichText | 0x9bf8bb95 | in-table | TL_textStrike (TLRPC.java:2273) |
| A-11.4.2 | TL_iv.RichText | 0xc12622c4 | in-table | TL_textUnderline (TLRPC.java:2401) |
| A-11.4.2 | TL_iv.RichText | 0xc7fb5e01 | in-table | TL_textSuperscript (TLRPC.java:2196) |
| A-11.4.2 | TL_iv.RichText | 0xd912a59c | in-table | TL_textItalic (TLRPC.java:2388) |
| A-11.4.2 | TL_iv.RichText | 0xdc3d824f | in-table | TL_textEmpty (TLRPC.java:2231) |
| A-11.4.2 | TL_iv.RichText | 0xde5a0dd6 | in-table | TL_textEmail (TLRPC.java:2312) |
| A-11.4.2 | TL_iv.RichText | 0xed6a8504 | in-table | TL_textSubscript (TLRPC.java:2414) |
| A-11.4.2 | TL_iv.Page | 0x556ec7aa | in-table | TL_pageFull_layer82 (TLRPC.java:20713) |
| A-11.4.2 | TL_iv.Page | 0x8dee6c44 | in-table | TL_pagePart_layer67 (TLRPC.java:20488) |
| A-11.4.2 | TL_iv.Page | 0x8e3f9ebe | in-table | TL_pagePart_layer82 (TLRPC.java:20563) |
| A-11.4.2 | TL_iv.Page | 0x98657f0d | in-table | TL_page (TLRPC.java:20871) |
| A-11.4.2 | TL_iv.Page | 0xae891bec | in-table | TL_page_layer110 (TLRPC.java:20788) |
| A-11.4.2 | TL_iv.Page | 0xd7a19d69 | in-table | TL_pageFull_layer67 (TLRPC.java:20638) |
| A-11.4.2 | TL_iv.RichMessage | - | type not defined at this pin | - |
| A-11.4.2 | TL_keyboard.KeyboardButtonRow | 0x77608b83 | in-table | TL_keyboardButtonRow (TLRPC.java:22176) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButtonRow | - | type not defined at this pin | - |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x0568a748 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonSwitchInline_layer157 (TLRPC.java:21235) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x0d0b468c | in-table | TL_keyboardButtonRequestPeer_layer168 (TLRPC.java:75213) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x10b78d29 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonUrlAuth (TLRPC.java:21312) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x13767230 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonWebView (TLRPC.java:75053) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x258aff05 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonUrl (TLRPC.java:21219) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x308660c1 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonUserProfile (TLRPC.java:21417) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x35bbdb6b | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonCallback (TLRPC.java:21432) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x50f41ccf | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonGame (TLRPC.java:21205) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x53d7bfd8 | in-table | TL_keyboardButtonRequestPeer (TLRPC.java:75230) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x683a5e46 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonCallback_layer117 (TLRPC.java:21452) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x75d2698e | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonCopy (TLRPC.java:75252) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x93b9fbb5 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonSwitchInline (TLRPC.java:21254) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xa0c0505c | in-table | TL_keyboardButtonSimpleWebView (TLRPC.java:75730) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xa2fa4880 | in-table | TL_keyboardButton (TLRPC.java:21468) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xafd93fbb | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonBuy (TLRPC.java:21388) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xb16a6c29 | in-table | TL_keyboardButtonRequestPhone (TLRPC.java:21191) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xbbc7515d | in-table | TL_keyboardButtonRequestPoll (TLRPC.java:21366) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xd02e7fd4 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_inputKeyboardButtonUrlAuth (TLRPC.java:21338) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xe988037b | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_inputKeyboardButtonUserProfile (TLRPC.java:21402) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xfc796b3f | in-table | TL_keyboardButtonRequestGeoLocation (TLRPC.java:21298) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | - | type not defined at this pin | - |
| A-11.4.2 | TL_keyboard.KeyboardButtonStyle | - | type not defined at this pin | - |
| A-11.4.2 | TL_keyboard.ButtonType | - | type not defined at this pin | - |
| A-11.4.2 | TL_keyboard.InlineButtonType | - | type not defined at this pin | - |
| A-11.4.2 | RequestPeerType | 0x339bef6c | in-table | TL_requestPeerTypeBroadcast (TLRPC.java:75177) |
| A-11.4.2 | RequestPeerType | 0x5f3b8a00 | in-table | TL_requestPeerTypeUser (TLRPC.java:75102) |
| A-11.4.2 | RequestPeerType | 0xc9f06e1b | in-table | TL_requestPeerTypeChat (TLRPC.java:75132) |
| A-11.4.2 | TL_chatAdminRights | 0x5fb224d5 | in-table | TL_chatAdminRights (TLRPC.java:4574) |
| A-11.4.2 | InputPeer | 0x179be863 | in-table | TL_inputPeerChat_layer131 (TLRPC.java:58257) |
| A-11.4.2 | InputPeer | 0x17bae2e6 | in-table | TL_inputPeerUserFromMessage_layer131 (TLRPC.java:58378) |
| A-11.4.2 | InputPeer | 0x20adaef8 | in-table | TL_inputPeerChannel_layer131 (TLRPC.java:58344) |
| A-11.4.2 | InputPeer | 0x27bcbbfc | in-table | TL_inputPeerChannel (TLRPC.java:58271) |
| A-11.4.2 | InputPeer | 0x35a95cb9 | in-table | TL_inputPeerChat (TLRPC.java:58330) |
| A-11.4.2 | InputPeer | 0x7b8e7de6 | in-table | TL_inputPeerUser_layer131 (TLRPC.java:58287) |
| A-11.4.2 | InputPeer | 0x7da07ec9 | in-table | TL_inputPeerSelf (TLRPC.java:58214) |
| A-11.4.2 | InputPeer | 0x7f3b18ea | in-table | TL_inputPeerEmpty (TLRPC.java:58303) |
| A-11.4.2 | InputPeer | 0x9c95f7bb | in-table | TL_inputPeerChannelFromMessage_layer131 (TLRPC.java:58360) |
| A-11.4.2 | InputPeer | 0xa87b0a1c | in-table | TL_inputPeerUserFromMessage (TLRPC.java:58223) |
| A-11.4.2 | InputPeer | 0xbd2a0840 | in-table | TL_inputPeerChannelFromMessage (TLRPC.java:58312) |
| A-11.4.2 | InputPeer | 0xdde8a54c | in-table | TL_inputPeerUser (TLRPC.java:58241) |
| A-11.4.2 | InlineQueryPeerType | 0x0e3b2d0c | in-table | TL_inlineQueryPeerTypeBotPM (TLRPC.java:30473) |
| A-11.4.2 | InlineQueryPeerType | 0x3081ed9d | in-table | TL_inlineQueryPeerTypeSameBotPM (TLRPC.java:30489) |
| A-11.4.2 | InlineQueryPeerType | 0x5ec4be43 | in-table | TL_inlineQueryPeerTypeMegagroup (TLRPC.java:30481) |
| A-11.4.2 | InlineQueryPeerType | 0x6334ee9a | in-table | TL_inlineQueryPeerTypeBroadcast (TLRPC.java:30449) |
| A-11.4.2 | InlineQueryPeerType | 0x833c0fac | in-table | TL_inlineQueryPeerTypePM (TLRPC.java:30465) |
| A-11.4.2 | InlineQueryPeerType | 0xd766c50a | in-table | TL_inlineQueryPeerTypeChat (TLRPC.java:30457) |
| A-11.4.2 | PollAnswer | 0x6ca9c2e9 | in-table | TL_pollAnswer_layer178 (TLRPC.java:3067) |
| A-11.4.2 | PollAnswer | 0xff16e2ca | in-table | TL_pollAnswer (TLRPC.java:3052) |
| A-11.4.2 | PollAnswerVoters | 0x3b6ddad2 | in-table | TL_pollAnswerVoters (TLRPC.java:5299) |

Body comparison with A-HEAD: every row of `constructors.tsv` whose ID the switch of its type reads at this pin, what its body reads at this pin against its A-HEAD reading (field order, flag bits and wire types; flag-only trues, `mType.` holders and field names set aside, object types named by the deserializer a call reaches, ruling T1a). `equal on the wire` lists the field names that differ (informational); `membership-only` names the line the reduction cannot read; `compatible widening (union)` an older row of the button union that the union check below holds for (Q5, ruling T2a).

| Pin | Type | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| A-11.4.2 | Message | 0x05f46804 | equal | TL_messageForwarded_old (TLRPC.java:73365) |
| A-11.4.2 | Message | 0x1c9b1027 | equal | TL_message_layer104_2 (TLRPC.java:72674) |
| A-11.4.2 | Message | 0x1d86f70e | equal | TL_messageService_old2 (TLRPC.java:69691) |
| A-11.4.2 | Message | 0x1e4c8a69 | equal | TL_message_layer175 (TLRPC.java:71097) |
| A-11.4.2 | Message | 0x22eb6aba | equal | TL_message_old (TLRPC.java:73406) |
| A-11.4.2 | Message | 0x2357bf25 | equal | TL_message_layer179 (TLRPC.java:70683) |
| A-11.4.2 | Message | 0x286fa604 | equal | TL_messageService_layer123 (TLRPC.java:73633) |
| A-11.4.2 | Message | 0x2b085862 | equal | TL_messageService (TLRPC.java:73680) |
| A-11.4.2 | Message | 0x2bebfa86 | equal | TL_message_old6 (TLRPC.java:72949) |
| A-11.4.2 | Message | 0x38116ee0 | equal | TL_message_layer169 (TLRPC.java:71483) |
| A-11.4.2 | Message | 0x44f9b43d | equal | TL_message_layer104 (TLRPC.java:72819) |
| A-11.4.2 | Message | 0x452c0e65 | equal | TL_message_layer117 (TLRPC.java:72352) |
| A-11.4.2 | Message | 0x555555f8 | equal | TL_message_secret_old (TLRPC.java:73593) |
| A-11.4.2 | Message | 0x555555f9 | equal | TL_message_secret_layer72 (TLRPC.java:73519) |
| A-11.4.2 | Message | 0x555555fa | equal | TL_message_secret (TLRPC.java:73439) |
| A-11.4.2 | Message | 0x567699b3 | equal | TL_message_old2 (TLRPC.java:73298) |
| A-11.4.2 | Message | 0x58ae39c9 | equal | TL_message_layer123 (TLRPC.java:72019) |
| A-11.4.2 | Message | 0x5ba66c13 | equal | TL_message_old7 (TLRPC.java:70082) |
| A-11.4.2 | Message | 0x76bec211 | equal | TL_message_layer173 (TLRPC.java:71293) |
| A-11.4.2 | Message | 0x83e5de54 | equal | TL_messageEmpty_layer122 (TLRPC.java:69653) |
| A-11.4.2 | Message | 0x85d6cbe2 | equal | TL_message_layer135 (TLRPC.java:71667) |
| A-11.4.2 | Message | 0x90a6ca84 | equal | TL_messageEmpty (TLRPC.java:69668) |
| A-11.4.2 | Message | 0x90dddc11 | equal | TL_message_layer72 (TLRPC.java:69725) |
| A-11.4.2 | Message | 0x94345242 | equal | TL_message (TLRPC.java:70239) |
| A-11.4.2 | Message | 0x9789dac4 | equal | TL_message_layer104_3 (TLRPC.java:72510) |
| A-11.4.2 | Message | 0x9e19a1f6 | equal | TL_messageService_layer118 (TLRPC.java:73732) |
| A-11.4.2 | Message | 0x9f8d60bb | equal | TL_messageService_old (TLRPC.java:73337) |
| A-11.4.2 | Message | 0xa367e716 | equal | TL_messageForwarded_old2 (TLRPC.java:70193) |
| A-11.4.2 | Message | 0xa4e97f37 | equal | TL_message_layer179 (TLRPC.java:70683) |
| A-11.4.2 | Message | 0xa66c7efc | equal | TL_message_layer176 (TLRPC.java:70895) |
| A-11.4.2 | Message | 0xa7ab1991 | equal | TL_message_old3 (TLRPC.java:73241) |
| A-11.4.2 | Message | 0xbce383d2 | equal | TL_message_layer131 (TLRPC.java:71844) |
| A-11.4.2 | Message | 0xbde09c2e | equal | TL_message_layer180 (TLRPC.java:70465) |
| A-11.4.2 | Message | 0xc06b9607 | equal | TL_messageService_layer48 (TLRPC.java:73133) |
| A-11.4.2 | Message | 0xc09be45f | equal | TL_message_layer68 (TLRPC.java:69845) |
| A-11.4.2 | Message | 0xc3060325 | equal | TL_message_old4 (TLRPC.java:73177) |
| A-11.4.2 | Message | 0xc992e15c | equal | TL_message_layer47 (TLRPC.java:69965) |
| A-11.4.2 | Message | 0xf07814c8 | equal | TL_message_old5 (TLRPC.java:73044) |
| A-11.4.2 | Message | 0xf52e6b7f | equal | TL_message_layer118 (TLRPC.java:72188) |
| A-11.4.2 | Peer | 0x36c6019a | equal | TL_peerChat (TLRPC.java:4254) |
| A-11.4.2 | Peer | 0x59511722 | equal | TL_peerUser (TLRPC.java:4226) |
| A-11.4.2 | Peer | 0x9db1bc6d | equal | TL_peerUser_layer131 (TLRPC.java:4268) |
| A-11.4.2 | Peer | 0xa2a5371e | equal | TL_peerChannel (TLRPC.java:4240) |
| A-11.4.2 | Peer | 0xbad0e5bb | equal | TL_peerChat_layer131 (TLRPC.java:4282) |
| A-11.4.2 | Peer | 0xbddde532 | equal | TL_peerChannel_layer131 (TLRPC.java:4212) |
| A-11.4.2 | MessageFwdHeader | 0x353a686b | equal | TL_messageFwdHeader_layer118 (TLRPC.java:53003) |
| A-11.4.2 | MessageFwdHeader | 0x4e4df4bb | equal | TL_messageFwdHeader (TLRPC.java:52864) |
| A-11.4.2 | MessageFwdHeader | 0x559ebe6d | equal | TL_messageFwdHeader_layer96 (TLRPC.java:53171) |
| A-11.4.2 | MessageFwdHeader | 0x5f777dce | equal | TL_messageFwdHeader_layer169 (TLRPC.java:52943) |
| A-11.4.2 | MessageFwdHeader | 0xc786ddcb | equal | TL_messageFwdHeader_layer68 (TLRPC.java:52828) |
| A-11.4.2 | MessageFwdHeader | 0xec338270 | equal | TL_messageFwdHeader_layer112 (TLRPC.java:53069) |
| A-11.4.2 | MessageFwdHeader | 0xfadff4ac | equal | TL_messageFwdHeader_layer72 (TLRPC.java:53129) |
| A-11.4.2 | MessageReplyHeader | 0x0e5af939 | equal | TL_messageReplyStoryHeader (TLRPC.java:31674) |
| A-11.4.2 | MessageReplyHeader | 0x3d5c1693 | equal | TL_messageReplyHeader_layer165_2 (TLRPC.java:31855) |
| A-11.4.2 | MessageReplyHeader | 0x6eebcabd | equal | TL_messageReplyHeader_layer166 (TLRPC.java:31775) |
| A-11.4.2 | MessageReplyHeader | 0x9c98bfc1 | equal | TL_messageReplyStoryHeader_layer174 (TLRPC.java:31658) |
| A-11.4.2 | MessageReplyHeader | 0xa6d57763 | equal | TL_messageReplyHeader_layer165 (TLRPC.java:31927) |
| A-11.4.2 | MessageReplyHeader | 0xafbc09db | equal | TL_messageReplyHeader (TLRPC.java:31689) |
| A-11.4.2 | MessageEntity | 0x020df5d0 | equal | TL_messageEntityBlockquote_layer180 (TLRPC.java:40609) |
| A-11.4.2 | MessageEntity | 0x208e68c9 | equal | TL_inputMessageEntityMentionName (TLRPC.java:40491) |
| A-11.4.2 | MessageEntity | 0x28a20571 | equal | TL_messageEntityCode (TLRPC.java:40557) |
| A-11.4.2 | MessageEntity | 0x32ca960f | equal | TL_messageEntitySpoiler (TLRPC.java:40458) |
| A-11.4.2 | MessageEntity | 0x352dca58 | equal | TL_messageEntityMentionName_layer131 (TLRPC.java:40473) |
| A-11.4.2 | MessageEntity | 0x4c4e743f | equal | TL_messageEntityCashtag (TLRPC.java:40510) |
| A-11.4.2 | MessageEntity | 0x64e475c2 | equal | TL_messageEntityEmail (TLRPC.java:40360) |
| A-11.4.2 | MessageEntity | 0x6cef8ac7 | equal | TL_messageEntityBotCommand (TLRPC.java:40344) |
| A-11.4.2 | MessageEntity | 0x6ed02538 | equal | TL_messageEntityUrl (TLRPC.java:40410) |
| A-11.4.2 | MessageEntity | 0x6f635b0d | equal | TL_messageEntityHashtag (TLRPC.java:40541) |
| A-11.4.2 | MessageEntity | 0x73924be0 | equal | TL_messageEntityPre (TLRPC.java:40376) |
| A-11.4.2 | MessageEntity | 0x761e6af4 | equal | TL_messageEntityBankCard (TLRPC.java:40641) |
| A-11.4.2 | MessageEntity | 0x76a6d327 | equal | TL_messageEntityTextUrl (TLRPC.java:40326) |
| A-11.4.2 | MessageEntity | 0x826f8b60 | equal | TL_messageEntityItalic (TLRPC.java:40426) |
| A-11.4.2 | MessageEntity | 0x9b69e34b | equal | TL_messageEntityPhone (TLRPC.java:40657) |
| A-11.4.2 | MessageEntity | 0x9c4e7e8b | equal | TL_messageEntityUnderline (TLRPC.java:40625) |
| A-11.4.2 | MessageEntity | 0xbb92ba95 | equal | TL_messageEntityUnknown (TLRPC.java:40394) |
| A-11.4.2 | MessageEntity | 0xbd610bc9 | equal | TL_messageEntityBold (TLRPC.java:40525) |
| A-11.4.2 | MessageEntity | 0xbf0693d4 | equal | TL_messageEntityStrike (TLRPC.java:40573) |
| A-11.4.2 | MessageEntity | 0xc8cf05f8 | equal | TL_messageEntityCustomEmoji (TLRPC.java:76616) |
| A-11.4.2 | MessageEntity | 0xdc7b1140 | equal | TL_messageEntityMentionName (TLRPC.java:40672) |
| A-11.4.2 | MessageEntity | 0xf1ccaaac | equal | TL_messageEntityBlockquote (TLRPC.java:40589) |
| A-11.4.2 | MessageEntity | 0xfa04579d | equal | TL_messageEntityMention (TLRPC.java:40442) |
| A-11.4.2 | MessageAction | 0x01baa035 | equal | TL_messageActionPhoneNumberRequest (TLRPC.java:28720) |
| A-11.4.2 | MessageAction | 0x031224c3 | equal | TL_messageActionChatJoinedByLink (TLRPC.java:28837) |
| A-11.4.2 | MessageAction | 0x08557637 | equal | TL_messageActionStarGift (TLRPC.java:80228) |
| A-11.4.2 | MessageAction | 0x0d999256 | equal | TL_messageActionTopicCreate (TLRPC.java:29246) |
| A-11.4.2 | MessageAction | 0x15cefd00 | equal | TL_messageActionChatAddUser (TLRPC.java:28645) |
| A-11.4.2 | MessageAction | 0x2a9fadc5 | equal | TL_messageActionGiveawayResults_layer186 (TLRPC.java:80150) |
| A-11.4.2 | MessageAction | 0x31518e9b | equal | TL_messageActionRequestedPeer (TLRPC.java:77813) |
| A-11.4.2 | MessageAction | 0x332ba9ed | CONFLICT (opaque; pin conflict: flags:# [F.0: stars:i64] at TLRPC.java:80117, nothing at TLRPC.java:67263) | TL_messageActionGiveawayLaunch_layer186 (TLRPC.java:80117) |
| A-11.4.2 | MessageAction | 0x3c134d7b | equal | TL_messageActionSetMessagesTTL (TLRPC.java:29078) |
| A-11.4.2 | MessageAction | 0x40699cd0 | equal | TL_messageActionPaymentSent_layer140 (TLRPC.java:29313) |
| A-11.4.2 | MessageAction | 0x41b3e202 | equal | TL_messageActionPaymentRefunded (TLRPC.java:28343) |
| A-11.4.2 | MessageAction | 0x45d5b021 | equal | TL_messageActionGiftStars (TLRPC.java:28372) |
| A-11.4.2 | MessageAction | 0x4792929b | equal | TL_messageActionScreenshotTaken (TLRPC.java:28711) |
| A-11.4.2 | MessageAction | 0x47dd8079 | equal | TL_messageActionWebViewDataSentMe (TLRPC.java:28950) |
| A-11.4.2 | MessageAction | 0x488a7337 | membership-only (MANUAL at TLRPC.java:28881) | TL_messageActionChatAddUser_layer131 (TLRPC.java:28867) |
| A-11.4.2 | MessageAction | 0x502f92f7 | equal | TL_messageActionInviteToGroupCall (TLRPC.java:28499) |
| A-11.4.2 | MessageAction | 0x5060a3f4 | equal | TL_messageActionSetChatWallPaper (TLRPC.java:28996) |
| A-11.4.2 | MessageAction | 0x51bdb021 | equal | TL_messageActionChatMigrateTo_layer131 (TLRPC.java:28674) |
| A-11.4.2 | MessageAction | 0x55555550 | equal | TL_messageActionUserJoined (TLRPC.java:29018) |
| A-11.4.2 | MessageAction | 0x55555551 | equal | TL_messageActionUserUpdatedPhoto (TLRPC.java:29027) |
| A-11.4.2 | MessageAction | 0x55555552 | equal | TL_messageActionTTLChange (TLRPC.java:29064) |
| A-11.4.2 | MessageAction | 0x55555557 | equal | TL_messageActionCreatedBroadcastList (TLRPC.java:28910) |
| A-11.4.2 | MessageAction | 0x555555f5 | equal | TL_messageActionLoginUnknownLocation (TLRPC.java:28327) |
| A-11.4.2 | MessageAction | 0x555555f7 | equal | TL_messageEncryptedAction (TLRPC.java:28409) |
| A-11.4.2 | MessageAction | 0x56d03994 | equal | TL_messageActionGiftCode (TLRPC.java:78792) |
| A-11.4.2 | MessageAction | 0x57de635e | equal | TL_messageActionSuggestProfilePhoto (TLRPC.java:77767) |
| A-11.4.2 | MessageAction | 0x5e3cfc4b | equal | TL_messageActionChatAddUser_old (TLRPC.java:29050) |
| A-11.4.2 | MessageAction | 0x678c2e09 | equal | TL_messageActionGiftCode_layer189 (TLRPC.java:78847) |
| A-11.4.2 | MessageAction | 0x6c6274fa | equal | TL_messageActionGiftPremium (TLRPC.java:29402) |
| A-11.4.2 | MessageAction | 0x76b9f11a | membership-only (MANUAL at TLRPC.java:28934) | TL_messageActionInviteToGroupCall_layer131 (TLRPC.java:28919) |
| A-11.4.2 | MessageAction | 0x7a0d7f42 | equal | TL_messageActionGroupCall (TLRPC.java:28546) |
| A-11.4.2 | MessageAction | 0x7fcb13a8 | equal | TL_messageActionChatEditPhoto (TLRPC.java:28697) |
| A-11.4.2 | MessageAction | 0x80e11a7f | equal | TL_messageActionPhoneCall (TLRPC.java:29216) |
| A-11.4.2 | MessageAction | 0x87e2f155 | equal | TL_messageActionGiveawayResults (TLRPC.java:80125) |
| A-11.4.2 | MessageAction | 0x8f31b327 | equal | TL_messageActionPaymentSentMe (TLRPC.java:29479) |
| A-11.4.2 | MessageAction | 0x92a72876 | equal | TL_messageActionGameScore (TLRPC.java:29386) |
| A-11.4.2 | MessageAction | 0x94bd38ed | equal | TL_messageActionPinMessage (TLRPC.java:29177) |
| A-11.4.2 | MessageAction | 0x95d2ac92 | equal | TL_messageActionChannelCreate (TLRPC.java:29129) |
| A-11.4.2 | MessageAction | 0x95e3fbef | equal | TL_messageActionChatDeletePhoto (TLRPC.java:29207) |
| A-11.4.2 | MessageAction | 0x96163f56 | equal | TL_messageActionPaymentSent (TLRPC.java:29286) |
| A-11.4.2 | MessageAction | 0x98e0d697 | equal | TL_messageActionGeoProximityReached (TLRPC.java:29186) |
| A-11.4.2 | MessageAction | 0x9bb3ef44 | equal | TL_messageActionStarGift_layer192 (TLRPC.java:80198) |
| A-11.4.2 | MessageAction | 0x9fbab604 | equal | TL_messageActionHistoryClear (TLRPC.java:28688) |
| A-11.4.2 | MessageAction | 0xa43f30cc | equal | TL_messageActionChatDeleteUser (TLRPC.java:28631) |
| A-11.4.2 | MessageAction | 0xa6638b9a | membership-only (MANUAL at TLRPC.java:28483) | TL_messageActionChatCreate_layer131 (TLRPC.java:28468) |
| A-11.4.2 | MessageAction | 0xa80f51e4 | equal | TL_messageActionGiveawayLaunch (TLRPC.java:80094) |
| A-11.4.2 | MessageAction | 0xaa1afbfd | equal | TL_messageActionSetMessagesTTL_layer149 (TLRPC.java:29102) |
| A-11.4.2 | MessageAction | 0xaa786345 | membership-only (MANUAL at TLRPC.java:25616) | TL_messageActionSetChatTheme (TLRPC.java:28568) |
| A-11.4.2 | MessageAction | 0xabe9affe | equal | TL_messageActionBotAllowed_layer153 (TLRPC.java:29362) |
| A-11.4.2 | MessageAction | 0xb00c47a2 | equal | TL_messageActionPrizeStars (TLRPC.java:80166) |
| A-11.4.2 | MessageAction | 0xb055eaee | equal | TL_messageActionChannelMigrateFrom_layer131 (TLRPC.java:28851) |
| A-11.4.2 | MessageAction | 0xb18a431c | equal | TL_messageActionTopicEdit_layer149 (TLRPC.java:28783) |
| A-11.4.2 | MessageAction | 0xb2ae9b0c | equal | TL_messageActionChatDeleteUser_layer131 (TLRPC.java:28896) |
| A-11.4.2 | MessageAction | 0xb3a07661 | equal | TL_messageActionGroupCallScheduled (TLRPC.java:29522) |
| A-11.4.2 | MessageAction | 0xb4c38cb5 | equal | TL_messageActionWebViewDataSent (TLRPC.java:28968) |
| A-11.4.2 | MessageAction | 0xb5a1ce5a | equal | TL_messageActionChatEditTitle (TLRPC.java:29272) |
| A-11.4.2 | MessageAction | 0xb6aef7b0 | equal | TL_messageActionEmpty (TLRPC.java:29377) |
| A-11.4.2 | MessageAction | 0xbc44a927 | equal | TL_messageActionSetChatWallPaper_layer166 (TLRPC.java:28983) |
| A-11.4.2 | MessageAction | 0xbd47cbad | equal | TL_messageActionChatCreate (TLRPC.java:28437) |
| A-11.4.2 | MessageAction | 0xc0787d6d | equal | TL_messageActionSetSameChatWallPaper (TLRPC.java:28729) |
| A-11.4.2 | MessageAction | 0xc0944820 | equal | TL_messageActionTopicEdit (TLRPC.java:28742) |
| A-11.4.2 | MessageAction | 0xc516d679 | equal | TL_messageActionBotAllowed (TLRPC.java:29328) |
| A-11.4.2 | MessageAction | 0xc83d6aec | equal | TL_messageActionGiftPremium_layer189 (TLRPC.java:29437) |
| A-11.4.2 | MessageAction | 0xcc02aa6d | equal | TL_messageActionBoostApply (TLRPC.java:29464) |
| A-11.4.2 | MessageAction | 0xd2cfdb0e | equal | TL_messageActionGiftCode_layer167 (TLRPC.java:78903) |
| A-11.4.2 | MessageAction | 0xd95c6154 | equal | TL_messageActionSecureValuesSent (TLRPC.java:29143) |
| A-11.4.2 | MessageAction | 0xe1037f92 | equal | TL_messageActionChatMigrateTo (TLRPC.java:28823) |
| A-11.4.2 | MessageAction | 0xe7e75f97 | equal | TL_messageActionAttachMenuBotAllowed (TLRPC.java:77780) |
| A-11.4.2 | MessageAction | 0xea3948e9 | equal | TL_messageActionChannelMigrateFrom (TLRPC.java:28530) |
| A-11.4.2 | MessageAction | 0xebbca3cb | equal | TL_messageActionChatJoinedByRequest (TLRPC.java:28814) |
| A-11.4.2 | MessageAction | 0xf3f25f76 | equal | TL_messageActionContactSignUp (TLRPC.java:29041) |
| A-11.4.2 | MessageAction | 0xf89cf5e8 | equal | TL_messageActionChatJoinedByLink_layer131 (TLRPC.java:29115) |
| A-11.4.2 | MessageAction | 0xfae69f56 | equal | TL_messageActionCustomAction (TLRPC.java:28423) |
| A-11.4.2 | MessageAction | 0xfe77345d | equal | TL_messageActionRequestedPeer_layer168 (TLRPC.java:77792) |
| A-11.4.2 | PhoneCallDiscardReason | 0x57adc690 | equal | TL_phoneCallDiscardReasonHangup (TLRPC.java:23723) |
| A-11.4.2 | PhoneCallDiscardReason | 0x85e42301 | equal | TL_phoneCallDiscardReasonMissed (TLRPC.java:23741) |
| A-11.4.2 | PhoneCallDiscardReason | 0xe095c1a0 | equal | TL_phoneCallDiscardReasonDisconnect (TLRPC.java:23750) |
| A-11.4.2 | PhoneCallDiscardReason | 0xfaf7e8c9 | equal | TL_phoneCallDiscardReasonBusy (TLRPC.java:23732) |
| A-11.4.2 | MessageMedia | 0x29632a36 | equal | TL_messageMediaUnsupported_old (TLRPC.java:8525) |
| A-11.4.2 | MessageMedia | 0x2ec0533f | equal | TL_messageMediaVenue (TLRPC.java:8758) |
| A-11.4.2 | MessageMedia | 0x2fda2204 | equal | TL_messageMediaDocument_old (TLRPC.java:8991) |
| A-11.4.2 | MessageMedia | 0x3d8ce53d | equal | TL_messageMediaPhoto_layer68 (TLRPC.java:9237) |
| A-11.4.2 | MessageMedia | 0x3ded6320 | equal | TL_messageMediaEmpty (TLRPC.java:8749) |
| A-11.4.2 | MessageMedia | 0x3f7ee58b | equal | TL_messageMediaDice (TLRPC.java:9312) |
| A-11.4.2 | MessageMedia | 0x4bd6e798 | equal | TL_messageMediaPoll (TLRPC.java:9050) |
| A-11.4.2 | MessageMedia | 0x4cf4d72d | equal | TL_messageMediaDocument_layer187 (TLRPC.java:8882) |
| A-11.4.2 | MessageMedia | 0x56e0d474 | equal | TL_messageMediaGeo (TLRPC.java:9343) |
| A-11.4.2 | MessageMedia | 0x58260664 | membership-only (MANUAL at TLRPC.java:79619) | TL_messageMediaGiveaway_layer167 (TLRPC.java:79592) |
| A-11.4.2 | MessageMedia | 0x5bcf1675 | equal | TL_messageMediaVideo_layer45 (TLRPC.java:9253) |
| A-11.4.2 | MessageMedia | 0x5e7d2f39 | equal | TL_messageMediaContact_layer81 (TLRPC.java:9217) |
| A-11.4.2 | MessageMedia | 0x638fe46b | equal | TL_messageMediaDice_layer111 (TLRPC.java:9330) |
| A-11.4.2 | MessageMedia | 0x68cb6283 | equal | TL_messageMediaStory (TLRPC.java:78661) |
| A-11.4.2 | MessageMedia | 0x695150d7 | equal | TL_messageMediaPhoto (TLRPC.java:9021) |
| A-11.4.2 | MessageMedia | 0x70322949 | equal | TL_messageMediaContact (TLRPC.java:9290) |
| A-11.4.2 | MessageMedia | 0x7912b71f | equal | TL_messageMediaVenue_layer71 (TLRPC.java:8787) |
| A-11.4.2 | MessageMedia | 0x7c3c2609 | equal | TL_messageMediaGeoLive_layer119 (TLRPC.java:9187) |
| A-11.4.2 | MessageMedia | 0x7c4414d3 | equal | TL_messageMediaDocument_layer74 (TLRPC.java:8957) |
| A-11.4.2 | MessageMedia | 0x84551347 | equal | TL_messageMediaInvoice_layer145 (TLRPC.java:8699) |
| A-11.4.2 | MessageMedia | 0x9cb070d7 | equal | TL_messageMediaDocument_layer159 (TLRPC.java:8924) |
| A-11.4.2 | MessageMedia | 0x9f84f49e | equal | TL_messageMediaUnsupported (TLRPC.java:8740) |
| A-11.4.2 | MessageMedia | 0xa2d24290 | equal | TL_messageMediaVideo_old (TLRPC.java:8809) |
| A-11.4.2 | MessageMedia | 0xa32dd600 | equal | TL_messageMediaWebPage_layer165 (TLRPC.java:9379) |
| A-11.4.2 | MessageMedia | 0xa8852491 | equal | TL_messageMediaPaidMedia (TLRPC.java:79651) |
| A-11.4.2 | MessageMedia | 0xaa073beb | membership-only (MANUAL at TLRPC.java:79476) | TL_messageMediaGiveaway (TLRPC.java:79438) |
| A-11.4.2 | MessageMedia | 0xb5223b0f | equal | TL_messageMediaPhoto_layer74 (TLRPC.java:9068) |
| A-11.4.2 | MessageMedia | 0xb940c666 | equal | TL_messageMediaGeoLive (TLRPC.java:9157) |
| A-11.4.2 | MessageMedia | 0xc6991068 | equal | TL_messageMediaGiveawayResults_layer186 (TLRPC.java:79380) |
| A-11.4.2 | MessageMedia | 0xc6b68300 | equal | TL_messageMediaAudio_layer45 (TLRPC.java:8539) |
| A-11.4.2 | MessageMedia | 0xc79aee1d | equal | MessageMediaStoryFull (MessageMediaStoryFull.java:9) |
| A-11.4.2 | MessageMedia | 0xc79aee1f | equal | MessageMediaStoryFull_old (MessageMediaStoryFull_old.java:7) |
| A-11.4.2 | MessageMedia | 0xc8c45a2a | equal | TL_messageMediaPhoto_old (TLRPC.java:8553) |
| A-11.4.2 | MessageMedia | 0xcbb20d88 | equal | TL_messageMediaStory_layer162 (TLRPC.java:78686) |
| A-11.4.2 | MessageMedia | 0xcbf24940 | equal | TL_messageMediaContact_layer131 (TLRPC.java:9269) |
| A-11.4.2 | MessageMedia | 0xceaa3ea1 | equal | TL_messageMediaGiveawayResults (TLRPC.java:79299) |
| A-11.4.2 | MessageMedia | 0xdaad85b0 | membership-only (MANUAL at TLRPC.java:79553) | TL_messageMediaGiveaway_layer186 (TLRPC.java:79525) |
| A-11.4.2 | MessageMedia | 0xdd570bd5 | equal | TL_messageMediaDocument (TLRPC.java:8823) |
| A-11.4.2 | MessageMedia | 0xddf10c3b | equal | TL_messageMediaWebPage (TLRPC.java:9356) |
| A-11.4.2 | MessageMedia | 0xf3e02ea8 | equal | TL_messageMediaDocument_layer68 (TLRPC.java:9005) |
| A-11.4.2 | MessageMedia | 0xf6a548d3 | equal | TL_messageMediaInvoice (TLRPC.java:8651) |
| A-11.4.2 | MessageMedia | 0xfdb19008 | equal | TL_messageMediaGame (TLRPC.java:9203) |
| A-11.4.2 | Photo | 0x22b56751 | membership-only (MANUAL at TLRPC.java:40954) | TL_photo_old (TLRPC.java:40934) |
| A-11.4.2 | Photo | 0x2331b22d | equal | TL_photoEmpty (TLRPC.java:40920) |
| A-11.4.2 | Photo | 0x9288dd29 | membership-only (MANUAL at TLRPC.java:41117) | TL_photo_layer82 (TLRPC.java:41098) |
| A-11.4.2 | Photo | 0x9c477dd8 | membership-only (MANUAL at TLRPC.java:40813) | TL_photo_layer97 (TLRPC.java:40793) |
| A-11.4.2 | Photo | 0xc3838076 | membership-only (MANUAL at TLRPC.java:40896) | TL_photo_old2 (TLRPC.java:40877) |
| A-11.4.2 | Photo | 0xcded42fe | membership-only (MANUAL at TLRPC.java:40855) | TL_photo_layer55 (TLRPC.java:40838) |
| A-11.4.2 | Photo | 0xd07504a5 | membership-only (MANUAL at TLRPC.java:40999) | TL_photo_layer115 (TLRPC.java:40979) |
| A-11.4.2 | Photo | 0xfb197a65 | membership-only (MANUAL at TLRPC.java:41046) | TL_photo (TLRPC.java:41026) |
| A-11.4.2 | Document | 0x1e87342b | membership-only (MANUAL at TLRPC.java:32814) | TL_document_layer142 (TLRPC.java:32792) |
| A-11.4.2 | Document | 0x36f8c871 | equal | TL_documentEmpty (TLRPC.java:32995) |
| A-11.4.2 | Document | 0x55555556 | membership-only (MANUAL at TLRPC.java:32672) | TL_documentEncrypted_old (TLRPC.java:32660) |
| A-11.4.2 | Document | 0x55555558 | membership-only (MANUAL at TLRPC.java:33019) | TL_documentEncrypted (TLRPC.java:33009) |
| A-11.4.2 | Document | 0x59534e4c | membership-only (MANUAL at TLRPC.java:32573) | TL_document_layer92 (TLRPC.java:32562) |
| A-11.4.2 | Document | 0x87232bc7 | membership-only (MANUAL at TLRPC.java:32621) | TL_document_layer82 (TLRPC.java:32611) |
| A-11.4.2 | Document | 0x8fd4c4d8 | membership-only (MANUAL at TLRPC.java:32715) | TL_document (TLRPC.java:32694) |
| A-11.4.2 | Document | 0x9ba29cc1 | membership-only (MANUAL at TLRPC.java:32913) | TL_document_layer113 (TLRPC.java:32891) |
| A-11.4.2 | Document | 0x9efc6326 | membership-only (MANUAL at TLRPC.java:32977) | TL_document_old (TLRPC.java:32965) |
| A-11.4.2 | Document | 0xf9a39f4f | membership-only (MANUAL at TLRPC.java:33070) | TL_document_layer53 (TLRPC.java:33060) |
| A-11.4.2 | GeoPoint | 0x0296f104 | equal | TL_geoPoint_layer119 (TLRPC.java:5992) |
| A-11.4.2 | GeoPoint | 0x1117dd5f | equal | TL_geoPointEmpty (TLRPC.java:6026) |
| A-11.4.2 | GeoPoint | 0x2049d70c | equal | TL_geoPoint_layer81 (TLRPC.java:6010) |
| A-11.4.2 | GeoPoint | 0xb2a2f663 | equal | TL_geoPoint (TLRPC.java:6035) |
| A-11.4.2 | WebPage | 0x211a1788 | equal | TL_webPageEmpty (TLRPC.java:22996) |
| A-11.4.2 | WebPage | 0x5f07b4bc | equal | TL_webPage_layer104 (TLRPC.java:22810) |
| A-11.4.2 | WebPage | 0x7311ca11 | equal | TL_webPageNotModified (TLRPC.java:23094) |
| A-11.4.2 | WebPage | 0x85849473 | CONFLICT (opaque; pin conflict: flags:# [F.0: cached_page_views:i32] at TLRPC.java:23085, nothing at TLRPC.java:20418) | TL_webPageNotModified_layer110 (TLRPC.java:23085) |
| A-11.4.2 | WebPage | 0xa31ea0b5 | equal | TL_webPage_old (TLRPC.java:22910) |
| A-11.4.2 | WebPage | 0xb0d13e47 | equal | TL_webPagePending (TLRPC.java:23061) |
| A-11.4.2 | WebPage | 0xc586da1c | equal | TL_webPagePending_layer165 (TLRPC.java:23046) |
| A-11.4.2 | WebPage | 0xca820ed7 | equal | TL_webPage_layer58 (TLRPC.java:23115) |
| A-11.4.2 | WebPage | 0xd41a5167 | equal | TL_webPageUrlPending (TLRPC.java:23032) |
| A-11.4.2 | WebPage | 0xe89c45b2 | equal | TL_webPage (TLRPC.java:22561) |
| A-11.4.2 | WebPage | 0xeb1477e8 | equal | TL_webPageEmpty_layer165 (TLRPC.java:23018) |
| A-11.4.2 | WebPage | 0xfa64e172 | equal on the wire (names differ: attribute.documents:vec<Document> / documents:vec<Document>) | TL_webPage_layer107 (TLRPC.java:22687) |
| A-11.4.2 | User | 0x075cf7a8 | equal | TL_userForeign_old2 (TLRPC.java:27309) |
| A-11.4.2 | User | 0x1c60e608 | equal | TL_userSelf_old3 (TLRPC.java:27199) |
| A-11.4.2 | User | 0x200250ba | equal | TL_userEmpty_layer131 (TLRPC.java:27245) |
| A-11.4.2 | User | 0x215c4438 | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_user_layer184 (TLRPC.java:25872) |
| A-11.4.2 | User | 0x22e49072 | equal | TL_user_old (TLRPC.java:27479) |
| A-11.4.2 | User | 0x22e8ceb0 | equal | TL_userRequest_old (TLRPC.java:27259) |
| A-11.4.2 | User | 0x2e13f4c3 | equal | TL_user_layer104 (TLRPC.java:27065) |
| A-11.4.2 | User | 0x3ff6ecb0 | equal | TL_user_layer144 (TLRPC.java:26800) |
| A-11.4.2 | User | 0x4fe9cce0 | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_user_layer185 (TLRPC.java:25654) |
| A-11.4.2 | User | 0x5214c89d | equal | TL_userForeign_old (TLRPC.java:27285) |
| A-11.4.2 | User | 0x5d99adee | equal | TL_user_layer147 (TLRPC.java:26658) |
| A-11.4.2 | User | 0x7007b451 | equal | TL_userSelf_old2 (TLRPC.java:27561) |
| A-11.4.2 | User | 0x720535ec | equal | TL_userSelf_old (TLRPC.java:27173) |
| A-11.4.2 | User | 0x83314fca | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_user (TLRPC.java:25434) |
| A-11.4.2 | User | 0x8f97c628 | equal | TL_user_layer159 (TLRPC.java:26480) |
| A-11.4.2 | User | 0x938458c1 | equal | TL_user_layer131 (TLRPC.java:26934) |
| A-11.4.2 | User | 0xabb5f120 | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_user_layer165 (TLRPC.java:26290) |
| A-11.4.2 | User | 0xb29ad7cc | equal | TL_userDeleted_old (TLRPC.java:27363) |
| A-11.4.2 | User | 0xcab35e18 | equal | TL_userContact_old2 (TLRPC.java:25366) |
| A-11.4.2 | User | 0xd10d979a | equal | TL_user_layer65 (TLRPC.java:27381) |
| A-11.4.2 | User | 0xd3bc4b7a | equal | TL_userEmpty (TLRPC.java:25420) |
| A-11.4.2 | User | 0xd6016d7a | equal | TL_userDeleted_old2 (TLRPC.java:27225) |
| A-11.4.2 | User | 0xd9ccc4ef | equal | TL_userRequest_old2 (TLRPC.java:27335) |
| A-11.4.2 | User | 0xeb602f25 | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_user_layer166 (TLRPC.java:26084) |
| A-11.4.2 | User | 0xf2fb8319 | equal | TL_userContact_old (TLRPC.java:25394) |
| A-11.4.2 | Chat | 0x07328bdb | equal | TL_chatForbidden_layer131 (TLRPC.java:50862) |
| A-11.4.2 | Chat | 0x0aadfc8f | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_channel_layer185 (TLRPC.java:49867) |
| A-11.4.2 | Chat | 0x0cb44b1c | equal | TL_channel_layer72 (TLRPC.java:51321) |
| A-11.4.2 | Chat | 0x0db12acb | equal | TL_channel_layer167_3 (TLRPC.java:49495) |
| A-11.4.2 | Chat | 0x17d493d5 | equal | TL_channelForbidden (TLRPC.java:49165) |
| A-11.4.2 | Chat | 0x1981ea7e | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_channel_layer166 (TLRPC.java:50219) |
| A-11.4.2 | Chat | 0x289da732 | equal | TL_channelForbidden_layer131 (TLRPC.java:49195) |
| A-11.4.2 | Chat | 0x29562865 | equal | TL_chatEmpty (TLRPC.java:73993) |
| A-11.4.2 | Chat | 0x2d85832c | equal | TL_channelForbidden_layer52 (TLRPC.java:49304) |
| A-11.4.2 | Chat | 0x3bda1bde | equal | TL_chat_layer131 (TLRPC.java:49109) |
| A-11.4.2 | Chat | 0x41cbf256 | equal | TL_chat (TLRPC.java:49048) |
| A-11.4.2 | Chat | 0x450b7115 | equal | TL_channel_layer77 (TLRPC.java:51244) |
| A-11.4.2 | Chat | 0x4b1b7506 | equal | TL_channel_layer48 (TLRPC.java:49249) |
| A-11.4.2 | Chat | 0x4df30834 | equal | TL_channel_layer104 (TLRPC.java:51128) |
| A-11.4.2 | Chat | 0x6592a1a7 | equal | TL_chatForbidden (TLRPC.java:50846) |
| A-11.4.2 | Chat | 0x678e9587 | equal | TL_channel_old (TLRPC.java:50939) |
| A-11.4.2 | Chat | 0x6e9c9bc7 | equal | TL_chat_old (TLRPC.java:50986) |
| A-11.4.2 | Chat | 0x7312bc48 | equal | TL_chat_old2 (TLRPC.java:49015) |
| A-11.4.2 | Chat | 0x8261ac61 | equal | TL_channel_layer147 (TLRPC.java:50723) |
| A-11.4.2 | Chat | 0x83259464 | equal | TL_channel_layer161 (TLRPC.java:50566) |
| A-11.4.2 | Chat | 0x8537784f | equal | TL_channelForbidden_layer67 (TLRPC.java:49225) |
| A-11.4.2 | Chat | 0x8e87ccd8 | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_channel_layer167 (TLRPC.java:50052) |
| A-11.4.2 | Chat | 0x94f592db | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_channel_layer165 (TLRPC.java:50396) |
| A-11.4.2 | Chat | 0x9ba2d800 | equal | TL_chatEmpty_layer131 (TLRPC.java:74009) |
| A-11.4.2 | Chat | 0xa14dca52 | equal | TL_channel_layer67 (TLRPC.java:50878) |
| A-11.4.2 | Chat | 0xa636a3e2 | equal | TL_channel_layer167_2 (TLRPC.java:49322) |
| A-11.4.2 | Chat | 0xc88974ac | equal | TL_channel_layer92 (TLRPC.java:51393) |
| A-11.4.2 | Chat | 0xd31a961e | equal | TL_channel_layer131 (TLRPC.java:51011) |
| A-11.4.2 | Chat | 0xd91cdd54 | equal | TL_chat_layer92 (TLRPC.java:51470) |
| A-11.4.2 | Chat | 0xfb0ccc41 | equal | TL_chatForbidden_old (TLRPC.java:48997) |
| A-11.4.2 | Chat | 0xfe4478bd | equal on the wire (names differ: stories_max_id:i32 / stories_max_id.max_id:i32) | TL_channel (TLRPC.java:49674) |
| A-11.4.2 | PhotoSize | 0x021e1ad6 | equal | TL_photoCachedSize (TLRPC.java:53635) |
| A-11.4.2 | PhotoSize | 0x0e17e23c | equal | TL_photoSizeEmpty (TLRPC.java:53579) |
| A-11.4.2 | PhotoSize | 0x5aa86a51 | equal | TL_photoSizeProgressive_layer127 (TLRPC.java:53517) |
| A-11.4.2 | PhotoSize | 0x75c78e60 | equal | TL_photoSize (TLRPC.java:53615) |
| A-11.4.2 | PhotoSize | 0x77bfb61b | equal | TL_photoSize_layer127 (TLRPC.java:53557) |
| A-11.4.2 | PhotoSize | 0xd8214d41 | equal | TL_photoPathSize (TLRPC.java:68490) |
| A-11.4.2 | PhotoSize | 0xe0b0bc2e | equal | TL_photoStrippedSize (TLRPC.java:53500) |
| A-11.4.2 | PhotoSize | 0xe9a734fa | equal | TL_photoCachedSize_layer127 (TLRPC.java:53593) |
| A-11.4.2 | PhotoSize | 0xfa3efb95 | equal | TL_photoSizeProgressive (TLRPC.java:53655) |
| A-11.4.2 | VideoSize | 0x0da082fe | equal | TL_videoSizeStickerMarkup (TLRPC.java:21514) |
| A-11.4.2 | VideoSize | 0x435bb987 | equal | TL_videoSize_layer115 (TLRPC.java:21603) |
| A-11.4.2 | VideoSize | 0xde33b094 | equal | TL_videoSize (TLRPC.java:21655) |
| A-11.4.2 | VideoSize | 0xe831c556 | equal | TL_videoSize_layer127 (TLRPC.java:21625) |
| A-11.4.2 | VideoSize | 0xf85c413c | equal | TL_videoSizeEmojiMarkup (TLRPC.java:21482) |
| A-11.4.2 | DocumentAttribute | 0x051448e5 | equal | TL_documentAttributeAudio_old (TLRPC.java:1835) |
| A-11.4.2 | DocumentAttribute | 0x0ef02ce6 | equal | TL_documentAttributeVideo_layer159 (TLRPC.java:2009) |
| A-11.4.2 | DocumentAttribute | 0x11b58939 | equal | TL_documentAttributeAnimated (TLRPC.java:1875) |
| A-11.4.2 | DocumentAttribute | 0x15590068 | equal | TL_documentAttributeFilename (TLRPC.java:1884) |
| A-11.4.2 | DocumentAttribute | 0x17399fad | equal | TL_documentAttributeVideo_layer187 (TLRPC.java:1941) |
| A-11.4.2 | DocumentAttribute | 0x3a556302 | equal | TL_documentAttributeSticker_layer55 (TLRPC.java:1819) |
| A-11.4.2 | DocumentAttribute | 0x43c57c48 | equal | TL_documentAttributeVideo (TLRPC.java:1898) |
| A-11.4.2 | DocumentAttribute | 0x5910cccb | equal | TL_documentAttributeVideo_layer65 (TLRPC.java:2032) |
| A-11.4.2 | DocumentAttribute | 0x6319d612 | equal | TL_documentAttributeSticker (TLRPC.java:1849) |
| A-11.4.2 | DocumentAttribute | 0x6c37c15c | equal | TL_documentAttributeImageSize (TLRPC.java:2103) |
| A-11.4.2 | DocumentAttribute | 0x9801d2f7 | equal | TL_documentAttributeHasStickers (TLRPC.java:2080) |
| A-11.4.2 | DocumentAttribute | 0x9852f9c6 | equal | TL_documentAttributeAudio (TLRPC.java:2119) |
| A-11.4.2 | DocumentAttribute | 0x994c9882 | equal | TL_documentAttributeSticker_old2 (TLRPC.java:2089) |
| A-11.4.2 | DocumentAttribute | 0xd38ff1c2 | equal | TL_documentAttributeVideo_layer184 (TLRPC.java:1978) |
| A-11.4.2 | DocumentAttribute | 0xded218e0 | equal | TL_documentAttributeAudio_layer45 (TLRPC.java:2050) |
| A-11.4.2 | DocumentAttribute | 0xfb0a5727 | equal | TL_documentAttributeSticker_old (TLRPC.java:2068) |
| A-11.4.2 | DocumentAttribute | 0xfd149899 | equal | TL_documentAttributeCustomEmoji (TLRPC.java:2155) |
| A-11.4.2 | FileLocation | 0x091d11eb | equal | TL_fileLocation_layer97 (TLRPC.java:53302) |
| A-11.4.2 | FileLocation | 0x53d69076 | equal | TL_fileLocation_layer82 (TLRPC.java:53282) |
| A-11.4.2 | FileLocation | 0x55555554 | equal | TL_fileEncryptedLocation (TLRPC.java:53340) |
| A-11.4.2 | FileLocation | 0x7c596b46 | equal | TL_fileLocationUnavailable (TLRPC.java:53264) |
| A-11.4.2 | FileLocation | 0xbc7fc6cd | equal | TL_fileLocationToBeDeprecated (TLRPC.java:53324) |
| A-11.4.2 | InputStickerSet | 0x028703c8 | equal | TL_inputStickerSetAnimatedEmoji (TLRPC.java:35620) |
| A-11.4.2 | InputStickerSet | 0x04c4d4ce | equal | TL_inputStickerSetEmojiGenericAnimations (TLRPC.java:35629) |
| A-11.4.2 | InputStickerSet | 0x29d0f5ee | equal | TL_inputStickerSetEmojiDefaultStatuses (TLRPC.java:35557) |
| A-11.4.2 | InputStickerSet | 0x44c1f8e9 | equal | TL_inputStickerSetEmojiDefaultTopicIcons (TLRPC.java:35638) |
| A-11.4.2 | InputStickerSet | 0x49748553 | equal | TL_inputStickerSetEmojiChannelDefaultStatuses (TLRPC.java:35548) |
| A-11.4.2 | InputStickerSet | 0x861cc8a0 | equal | TL_inputStickerSetShortName (TLRPC.java:35606) |
| A-11.4.2 | InputStickerSet | 0x9de7a269 | equal | TL_inputStickerSetID (TLRPC.java:35590) |
| A-11.4.2 | InputStickerSet | 0xc88b3b02 | equal | TL_inputStickerSetPremiumGifts (TLRPC.java:35539) |
| A-11.4.2 | InputStickerSet | 0xe67f520e | equal | TL_inputStickerSetDice (TLRPC.java:35566) |
| A-11.4.2 | InputStickerSet | 0xffb62b95 | equal | TL_inputStickerSetEmpty (TLRPC.java:35581) |
| A-11.4.2 | TL_maskCoords | 0xaed6dbb2 | equal | TL_maskCoords (TLRPC.java:34323) |
| A-11.4.2 | Video | 0x388fa391 | membership-only (MANUAL at TLRPC.java:31272) | TL_video_old2 (TLRPC.java:31259) |
| A-11.4.2 | Video | 0x55555553 | membership-only (MANUAL at TLRPC.java:31199) | TL_videoEncrypted (TLRPC.java:31187) |
| A-11.4.2 | Video | 0x5a04a49f | membership-only (MANUAL at TLRPC.java:31237) | TL_video_old (TLRPC.java:31225) |
| A-11.4.2 | Video | 0xc10658a8 | equal | TL_videoEmpty_layer45 (TLRPC.java:31295) |
| A-11.4.2 | Video | 0xee9f4a4d | membership-only (MANUAL at TLRPC.java:31134) | TL_video_old3 (TLRPC.java:31123) |
| A-11.4.2 | Video | 0xf72887d3 | membership-only (MANUAL at TLRPC.java:31166) | TL_video_layer45 (TLRPC.java:31155) |
| A-11.4.2 | Audio | 0x427425e7 | equal | TL_audio_old (TLRPC.java:11263) |
| A-11.4.2 | Audio | 0x555555f6 | equal | TL_audioEncrypted (TLRPC.java:11289) |
| A-11.4.2 | Audio | 0x586988d8 | equal | TL_audioEmpty_layer45 (TLRPC.java:11223) |
| A-11.4.2 | Audio | 0xc7ac6496 | equal | TL_audio_old2 (TLRPC.java:11319) |
| A-11.4.2 | Audio | 0xf9e35055 | equal | TL_audio_layer45 (TLRPC.java:11237) |
| A-11.4.2 | MessageReplies | 0x4128faac | equal | TL_messageReplies_layer131 (TLRPC.java:11594) |
| A-11.4.2 | MessageReplies | 0x83d60fc2 | equal | TL_messageReplies (TLRPC.java:11657) |
| A-11.4.2 | MessageReactions | 0x087b6e36 | membership-only (MANUAL at TLRPC.java:31008) | TL_messageReactionsOld (TLRPC.java:30962) |
| A-11.4.2 | MessageReactions | 0x0a339f0b | membership-only (MANUAL at TLRPC.java:30700) | TL_messageReactions (TLRPC.java:30667) |
| A-11.4.2 | MessageReactions | 0x4f2b9479 | membership-only (MANUAL at TLRPC.java:30790) | TL_messageReactions_layer185 (TLRPC.java:30757) |
| A-11.4.2 | MessageReactions | 0xb87a24d1 | equal | TL_messageReactions_layer137 (TLRPC.java:31039) |
| A-11.4.2 | Reaction | 0x1b2286b8 | equal | TL_reactionEmoji (TLRPC.java:59495) |
| A-11.4.2 | Reaction | 0x523da4eb | equal | TL_reactionPaid (TLRPC.java:59486) |
| A-11.4.2 | Reaction | 0x79f5d419 | equal | TL_reactionEmpty (TLRPC.java:59477) |
| A-11.4.2 | Reaction | 0x8935fc73 | equal | TL_reactionCustomEmoji (TLRPC.java:59462) |
| A-11.4.2 | MessagePeerReaction | 0x51b67eff | membership-only (MANUAL at TLRPC.java:2898) | TL_messagePeerReaction_layer144 (TLRPC.java:2889) |
| A-11.4.2 | MessagePeerReaction | 0x8c79b63c | equal | TL_messagePeerReaction (TLRPC.java:2845) |
| A-11.4.2 | MessagePeerReaction | 0x932844fa | membership-only (MANUAL at TLRPC.java:2922) | TL_messagePeerReaction_layer137 (TLRPC.java:2914) |
| A-11.4.2 | MessagePeerReaction | 0xb156fe9c | equal | TL_messagePeerReaction_layer154 (TLRPC.java:2868) |
| A-11.4.2 | MessageReactor | 0x4ba3a95a | equal | TL_messageReactor (TLRPC.java:30583) |
| A-11.4.2 | MessageReactor | 0xef00d448 | equal | TL_messageReactor_old (TLRPC.java:30610) |
| A-11.4.2 | RestrictionReason | 0xd072acb4 | equal | TL_reastrictionReason (TLRPC.java:51532) |
| A-11.4.2 | TL_factCheck | 0xb89bfccf | equal | TL_factCheck (TLRPC.java:83712) |
| A-11.4.2 | TL_textWithEntities | 0x751f3146 | equal | TL_textWithEntities (TLRPC.java:54083) |
| A-11.4.2 | ReactionCount | 0xa3d1cb80 | membership-only (MANUAL at TLRPC.java:54274) | TL_reactionCount (TLRPC.java:54268) |
| A-11.4.2 | WebPageAttribute | 0x2e94c3e7 | equal | TL_webPageAttributeStory (TLRPC.java:12139) |
| A-11.4.2 | WebPageAttribute | 0x50cc03d3 | equal | TL_webPageAttributeStickerSet (TLRPC.java:12166) |
| A-11.4.2 | WebPageAttribute | 0x54b56617 | equal | TL_webPageAttributeTheme (TLRPC.java:12239) |
| A-11.4.2 | WebPageAttribute | 0x939a4671 | membership-only (MANUAL at TLRPC.java:12214) | TL_webPageAttributeStory_layer162 (TLRPC.java:12209) |
| A-11.4.2 | Poll | 0x58747131 | equal | TL_poll (TLRPC.java:42371) |
| A-11.4.2 | Poll | 0x86e18161 | equal | TL_poll_layer178 (TLRPC.java:42429) |
| A-11.4.2 | Poll | 0xaf746786 | equal | TL_poll_toDelete (TLRPC.java:42489) |
| A-11.4.2 | Poll | 0xd5529d06 | equal | TL_poll_layer111 (TLRPC.java:42543) |
| A-11.4.2 | PollResults | 0x5755785a | equal | TL_pollResults_layer108 (TLRPC.java:4686) |
| A-11.4.2 | PollResults | 0x7adf2420 | equal | TL_pollResults (TLRPC.java:4904) |
| A-11.4.2 | PollResults | 0xbadcc1a3 | membership-only (MANUAL at TLRPC.java:4841) | TL_pollResults_layer131 (TLRPC.java:4803) |
| A-11.4.2 | PollResults | 0xc87024a2 | membership-only (MANUAL at TLRPC.java:4771) | TL_pollResults_layer111 (TLRPC.java:4733) |
| A-11.4.2 | PollResults | 0xdcb82ea3 | membership-only (MANUAL at TLRPC.java:5044) | TL_pollResults_layer158 (TLRPC.java:5006) |
| A-11.4.2 | ReplyMarkup | 0x3502758c | equal | TL_replyKeyboardMarkup_layer129 (TLRPC.java:12018) |
| A-11.4.2 | ReplyMarkup | 0x48a30254 | compatible widening (union) (vec<TL_keyboard.KeyboardButtonRow> read as vec<TL_keyboard.KeyboardInlineButtonRow>: the union check below holds at this pin) | TL_replyInlineMarkup (TLRPC.java:12077) |
| A-11.4.2 | ReplyMarkup | 0x85dd99d1 | equal | TL_replyKeyboardMarkup (TLRPC.java:11929) |
| A-11.4.2 | ReplyMarkup | 0x86b40b08 | equal | TL_replyKeyboardForceReply (TLRPC.java:11994) |
| A-11.4.2 | ReplyMarkup | 0xa03e5b85 | equal | TL_replyKeyboardHide (TLRPC.java:11978) |
| A-11.4.2 | ReplyMarkup | 0xf4108aa0 | equal | TL_replyKeyboardForceReply_layer129 (TLRPC.java:12059) |
| A-11.4.2 | InputUser | 0x1da448e2 | equal | TL_inputUserFromMessage (TLRPC.java:21080) |
| A-11.4.2 | InputUser | 0xb98886cf | equal | TL_inputUserEmpty (TLRPC.java:21071) |
| A-11.4.2 | InputUser | 0xf21158c6 | equal | TL_inputUser (TLRPC.java:21055) |
| A-11.4.2 | InputUser | 0xf7c1b13f | equal | TL_inputUserSelf (TLRPC.java:21046) |
| A-11.4.2 | TL_iv.RichText | 0x034b8621 | equal | TL_textMarked (TLRPC.java:2286) |
| A-11.4.2 | TL_iv.RichText | 0x081ccf4f | equal | TL_textImage (TLRPC.java:2209) |
| A-11.4.2 | TL_iv.RichText | 0x1ccb966a | equal | TL_textPhone (TLRPC.java:2179) |
| A-11.4.2 | TL_iv.RichText | 0x35553762 | equal | TL_textAnchor (TLRPC.java:2257) |
| A-11.4.2 | TL_iv.RichText | 0x3c2884c1 | equal | TL_textUrl (TLRPC.java:2240) |
| A-11.4.2 | TL_iv.RichText | 0x6724abc4 | equal | TL_textBold (TLRPC.java:2375) |
| A-11.4.2 | TL_iv.RichText | 0x6c3f19b9 | equal | TL_textFixed (TLRPC.java:2299) |
| A-11.4.2 | TL_iv.RichText | 0x744694e0 | equal | TL_textPlain (TLRPC.java:2327) |
| A-11.4.2 | TL_iv.RichText | 0x7e6260d7 | equal | TL_textConcat (TLRPC.java:2342) |
| A-11.4.2 | TL_iv.RichText | 0x9bf8bb95 | equal | TL_textStrike (TLRPC.java:2273) |
| A-11.4.2 | TL_iv.RichText | 0xc12622c4 | equal | TL_textUnderline (TLRPC.java:2401) |
| A-11.4.2 | TL_iv.RichText | 0xc7fb5e01 | equal | TL_textSuperscript (TLRPC.java:2196) |
| A-11.4.2 | TL_iv.RichText | 0xd912a59c | equal | TL_textItalic (TLRPC.java:2388) |
| A-11.4.2 | TL_iv.RichText | 0xdc3d824f | equal | TL_textEmpty (TLRPC.java:2231) |
| A-11.4.2 | TL_iv.RichText | 0xde5a0dd6 | equal | TL_textEmail (TLRPC.java:2312) |
| A-11.4.2 | TL_iv.RichText | 0xed6a8504 | equal | TL_textSubscript (TLRPC.java:2414) |
| A-11.4.2 | TL_iv.Page | 0x556ec7aa | membership-only (MANUAL at TLRPC.java:20742) | TL_pageFull_layer82 (TLRPC.java:20713) |
| A-11.4.2 | TL_iv.Page | 0x8dee6c44 | membership-only (MANUAL at TLRPC.java:20517) | TL_pagePart_layer67 (TLRPC.java:20488) |
| A-11.4.2 | TL_iv.Page | 0x8e3f9ebe | membership-only (MANUAL at TLRPC.java:20592) | TL_pagePart_layer82 (TLRPC.java:20563) |
| A-11.4.2 | TL_iv.Page | 0x98657f0d | membership-only (MANUAL at TLRPC.java:20905) | TL_page (TLRPC.java:20871) |
| A-11.4.2 | TL_iv.Page | 0xae891bec | membership-only (MANUAL at TLRPC.java:20821) | TL_page_layer110 (TLRPC.java:20788) |
| A-11.4.2 | TL_iv.Page | 0xd7a19d69 | membership-only (MANUAL at TLRPC.java:20667) | TL_pageFull_layer67 (TLRPC.java:20638) |
| A-11.4.2 | TL_keyboard.KeyboardButtonRow | 0x77608b83 | compatible widening (union) (vec<TL_keyboard.KeyboardButton> read as vec<TL_keyboard.KeyboardInlineButton\|TL_keyboard.KeyboardButton>: the union check below holds at this pin) | TL_keyboardButtonRow (TLRPC.java:22176) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x0d0b468c | equal | TL_keyboardButtonRequestPeer_layer168 (TLRPC.java:75213) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x53d7bfd8 | equal | TL_keyboardButtonRequestPeer (TLRPC.java:75230) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xa0c0505c | equal | TL_keyboardButtonSimpleWebView (TLRPC.java:75730) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xa2fa4880 | equal | TL_keyboardButton (TLRPC.java:21468) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xb16a6c29 | equal | TL_keyboardButtonRequestPhone (TLRPC.java:21191) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xbbc7515d | equal | TL_keyboardButtonRequestPoll (TLRPC.java:21366) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xfc796b3f | equal | TL_keyboardButtonRequestGeoLocation (TLRPC.java:21298) |
| A-11.4.2 | RequestPeerType | 0x339bef6c | equal | TL_requestPeerTypeBroadcast (TLRPC.java:75177) |
| A-11.4.2 | RequestPeerType | 0x5f3b8a00 | equal | TL_requestPeerTypeUser (TLRPC.java:75102) |
| A-11.4.2 | RequestPeerType | 0xc9f06e1b | equal | TL_requestPeerTypeChat (TLRPC.java:75132) |
| A-11.4.2 | TL_chatAdminRights | 0x5fb224d5 | equal | TL_chatAdminRights (TLRPC.java:4574) |
| A-11.4.2 | InputPeer | 0x179be863 | equal | TL_inputPeerChat_layer131 (TLRPC.java:58257) |
| A-11.4.2 | InputPeer | 0x17bae2e6 | equal | TL_inputPeerUserFromMessage_layer131 (TLRPC.java:58378) |
| A-11.4.2 | InputPeer | 0x20adaef8 | equal | TL_inputPeerChannel_layer131 (TLRPC.java:58344) |
| A-11.4.2 | InputPeer | 0x27bcbbfc | equal | TL_inputPeerChannel (TLRPC.java:58271) |
| A-11.4.2 | InputPeer | 0x35a95cb9 | equal | TL_inputPeerChat (TLRPC.java:58330) |
| A-11.4.2 | InputPeer | 0x7b8e7de6 | equal | TL_inputPeerUser_layer131 (TLRPC.java:58287) |
| A-11.4.2 | InputPeer | 0x7da07ec9 | equal | TL_inputPeerSelf (TLRPC.java:58214) |
| A-11.4.2 | InputPeer | 0x7f3b18ea | equal | TL_inputPeerEmpty (TLRPC.java:58303) |
| A-11.4.2 | InputPeer | 0x9c95f7bb | equal | TL_inputPeerChannelFromMessage_layer131 (TLRPC.java:58360) |
| A-11.4.2 | InputPeer | 0xa87b0a1c | equal | TL_inputPeerUserFromMessage (TLRPC.java:58223) |
| A-11.4.2 | InputPeer | 0xbd2a0840 | equal | TL_inputPeerChannelFromMessage (TLRPC.java:58312) |
| A-11.4.2 | InputPeer | 0xdde8a54c | equal | TL_inputPeerUser (TLRPC.java:58241) |
| A-11.4.2 | InlineQueryPeerType | 0x0e3b2d0c | equal | TL_inlineQueryPeerTypeBotPM (TLRPC.java:30473) |
| A-11.4.2 | InlineQueryPeerType | 0x3081ed9d | equal | TL_inlineQueryPeerTypeSameBotPM (TLRPC.java:30489) |
| A-11.4.2 | InlineQueryPeerType | 0x5ec4be43 | equal | TL_inlineQueryPeerTypeMegagroup (TLRPC.java:30481) |
| A-11.4.2 | InlineQueryPeerType | 0x6334ee9a | equal | TL_inlineQueryPeerTypeBroadcast (TLRPC.java:30449) |
| A-11.4.2 | InlineQueryPeerType | 0x833c0fac | equal | TL_inlineQueryPeerTypePM (TLRPC.java:30465) |
| A-11.4.2 | InlineQueryPeerType | 0xd766c50a | equal | TL_inlineQueryPeerTypeChat (TLRPC.java:30457) |
| A-11.4.2 | PollAnswer | 0x6ca9c2e9 | equal | TL_pollAnswer_layer178 (TLRPC.java:3067) |
| A-11.4.2 | PollAnswer | 0xff16e2ca | equal | TL_pollAnswer (TLRPC.java:3052) |
| A-11.4.2 | PollAnswerVoters | 0x3b6ddad2 | equal | TL_pollAnswerVoters (TLRPC.java:5299) |

Compared 403 rows; membership-only 44

Union check (Q5, ruling T2a): every button ID of this pin's single `KeyboardButton` switch, read by the A-HEAD union `TL_keyboard.KeyboardButton|TL_keyboard.KeyboardInlineButton` (the first type holding the ID): `equal` (same bytes at both pins), `opaque at A-HEAD` (a stop, not a misread) or `CONFLICT`.

| Pin | Union element | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x0568a748 | equal (TL_keyboardButtonSwitchInline_layer157) | TL_keyboardButtonSwitchInline_layer157 (TLRPC.java:21235) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x0d0b468c | equal (TL_keyboardButtonRequestPeer_layer168) | TL_keyboardButtonRequestPeer_layer168 (TLRPC.java:75213) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x10b78d29 | equal (TL_keyboardButtonUrlAuth_layer223) | TL_keyboardButtonUrlAuth (TLRPC.java:21312) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x13767230 | equal (TL_keyboardButtonWebView_layer223) | TL_keyboardButtonWebView (TLRPC.java:75053) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x258aff05 | equal (TL_keyboardButtonUrl_layer223) | TL_keyboardButtonUrl (TLRPC.java:21219) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x308660c1 | equal (TL_keyboardButtonUserProfile_layer223) | TL_keyboardButtonUserProfile (TLRPC.java:21417) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x35bbdb6b | equal (TL_keyboardButtonCallback_layer223) | TL_keyboardButtonCallback (TLRPC.java:21432) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x50f41ccf | equal (TL_keyboardButtonGame_layer223) | TL_keyboardButtonGame (TLRPC.java:21205) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0x53d7bfd8 | equal (TL_keyboardButtonRequestPeer_layer223) | TL_keyboardButtonRequestPeer (TLRPC.java:75230) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x683a5e46 | equal (TL_keyboardButtonCallback_layer117) | TL_keyboardButtonCallback_layer117 (TLRPC.java:21452) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x75d2698e | equal (TL_keyboardButtonCopy_layer223) | TL_keyboardButtonCopy (TLRPC.java:75252) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0x93b9fbb5 | equal (TL_keyboardButtonSwitchInline_layer223) | TL_keyboardButtonSwitchInline (TLRPC.java:21254) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xa0c0505c | equal (TL_keyboardButtonSimpleWebView_layer223) | TL_keyboardButtonSimpleWebView (TLRPC.java:75730) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xa2fa4880 | equal (TL_keyboardButton_layer223) | TL_keyboardButton (TLRPC.java:21468) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0xafd93fbb | equal (TL_keyboardButtonBuy_layer223) | TL_keyboardButtonBuy (TLRPC.java:21388) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xb16a6c29 | equal (TL_keyboardButtonRequestPhone_layer223) | TL_keyboardButtonRequestPhone (TLRPC.java:21191) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xbbc7515d | equal (TL_keyboardButtonRequestPoll_layer223) | TL_keyboardButtonRequestPoll (TLRPC.java:21366) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0xd02e7fd4 | opaque at A-HEAD (TL_inputKeyboardButtonUrlAuth_layer223): a stop, not a misread | TL_inputKeyboardButtonUrlAuth (TLRPC.java:21338) |
| A-11.4.2 | TL_keyboard.KeyboardInlineButton | 0xe988037b | opaque at A-HEAD (TL_inputKeyboardButtonUserProfile_layer223): a stop, not a misread | TL_inputKeyboardButtonUserProfile (TLRPC.java:21402) |
| A-11.4.2 | TL_keyboard.KeyboardButton | 0xfc796b3f | equal (TL_keyboardButtonRequestGeoLocation_layer223) | TL_keyboardButtonRequestGeoLocation (TLRPC.java:21298) |

Union check at A-11.4.2: holds, every older inline keyboard is readable by the A-HEAD reading (a compatible widening).


## release-8.1.1_2431 (58aded63e9713ef796d76f3e29a60a5254f803b4)

Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):

| File | SHA-256 |
|---|---|
| `messenger/DialogObject.java` | bc803c85f37835054c3bafeb994235d089969c7c139bb2ce858f59e54602bd1e |
| `messenger/LocaleController.java` | 62c0f5b05c96260d82b4d1fe6e1939b04a881712859c89bbab705b747f417ea3 |
| `messenger/MessageObject.java` | 59e1f249d0cd51820d2284f228204908b7e4f3b0f465a464e2c84dc01b61d938 |
| `messenger/MessagesController.java` | a27b11e0de0d3e4aa413e1e98e2b03705a267c9b2108e28e7c3dee7f14fc1fef |
| `messenger/MessagesStorage.java` | 83a94d837ea7df4370880d503c46b57b24f0c5eaab9b04434b0120b3e5122e6c |
| `messenger/SecretChatHelper.java` | 98eb88384f97a37f7609ca657a700a2f4b99c0ab6037630dbc63fe79deec3908 |
| `tgnet/NativeByteBuffer.java` | 6d40177cf326cbcb4f44397471b28d506f82ca1113049a826690b7d6606e5033 |
| `tgnet/SerializedData.java` | 789a5892bbac33f4061828603fea5c52e0f49298ae45a2c2c1a61a47e045afa5 |
| `tgnet/TLObject.java` | 09ed9fd44708933d1bb72b99467a19f1e87651bbecd869fe2a37c737a2afc7d2 |
| `tgnet/TLRPC.java` | 82a26b9545edcf9f0dec632d213e21b436ef1a9262d0807e5f374d24b1c04ca8 |
| `ui/Cells/ChatMessageCell.java` | aa40a74c78080a64f216275454ae8fb71fd46092e30f240607d59b2f8a9a1a15 |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | ba9229aa405b9f9e2cb31698dfa46f667f21a7a2280e07fa64abe68f45037b39 |
| `facts.tsv` | dca5975e28eb17b1009b412ef44c14b9f0fc38864749dea18f767723136e6479 |

Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's switch, the class, the source line, the body the selection asks for, and the field order reduced from `readParams`; a `prefix` row is a proper prefix of the reduced body):

| Type | ID | Class | Verdict | Source | Note |
|---|---|---|---|---|---|
| - | - | - | no row is pinned here | - | - |

Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv`.

| Pin | Type | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| release-8.1.1_2431 | Message | 0x05f46804 | in-table | TL_messageForwarded_old (TLRPC.java:53105) |
| release-8.1.1_2431 | Message | 0x1c9b1027 | in-table | TL_message_layer104_2 (TLRPC.java:52420) |
| release-8.1.1_2431 | Message | 0x1d86f70e | in-table | TL_messageService_old2 (TLRPC.java:50874) |
| release-8.1.1_2431 | Message | 0x22eb6aba | in-table | TL_message_old (TLRPC.java:53146) |
| release-8.1.1_2431 | Message | 0x286fa604 | in-table | TL_messageService_layer123 (TLRPC.java:53373) |
| release-8.1.1_2431 | Message | 0x2b085862 | in-table | TL_messageService (TLRPC.java:53420) |
| release-8.1.1_2431 | Message | 0x2bebfa86 | in-table | TL_message_old6 (TLRPC.java:52693) |
| release-8.1.1_2431 | Message | 0x44f9b43d | in-table | TL_message_layer104 (TLRPC.java:52564) |
| release-8.1.1_2431 | Message | 0x452c0e65 | in-table | TL_message_layer117 (TLRPC.java:52100) |
| release-8.1.1_2431 | Message | 0x555555f8 | in-table | TL_message_secret_old (TLRPC.java:53333) |
| release-8.1.1_2431 | Message | 0x555555f9 | in-table | TL_message_secret_layer72 (TLRPC.java:53259) |
| release-8.1.1_2431 | Message | 0x555555fa | in-table | TL_message_secret (TLRPC.java:53179) |
| release-8.1.1_2431 | Message | 0x567699b3 | in-table | TL_message_old2 (TLRPC.java:53038) |
| release-8.1.1_2431 | Message | 0x58ae39c9 | in-table | TL_message_layer123 (TLRPC.java:51768) |
| release-8.1.1_2431 | Message | 0x5ba66c13 | in-table | TL_message_old7 (TLRPC.java:51262) |
| release-8.1.1_2431 | Message | 0x83e5de54 | in-table | TL_messageEmpty_layer122 (TLRPC.java:50835) |
| release-8.1.1_2431 | Message | 0x85d6cbe2 | in-table | TL_message (TLRPC.java:51418) |
| release-8.1.1_2431 | Message | 0x90a6ca84 | in-table | TL_messageEmpty (TLRPC.java:50850) |
| release-8.1.1_2431 | Message | 0x90dddc11 | in-table | TL_message_layer72 (TLRPC.java:50908) |
| release-8.1.1_2431 | Message | 0x9789dac4 | in-table | TL_message_layer104_3 (TLRPC.java:52257) |
| release-8.1.1_2431 | Message | 0x9e19a1f6 | in-table | TL_messageService_layer118 (TLRPC.java:53472) |
| release-8.1.1_2431 | Message | 0x9f8d60bb | in-table | TL_messageService_old (TLRPC.java:53077) |
| release-8.1.1_2431 | Message | 0xa367e716 | in-table | TL_messageForwarded_old2 (TLRPC.java:51372) |
| release-8.1.1_2431 | Message | 0xa7ab1991 | in-table | TL_message_old3 (TLRPC.java:52982) |
| release-8.1.1_2431 | Message | 0xbce383d2 | in-table | TL_message_layer131 (TLRPC.java:51593) |
| release-8.1.1_2431 | Message | 0xc06b9607 | in-table | TL_messageService_layer48 (TLRPC.java:52875) |
| release-8.1.1_2431 | Message | 0xc09be45f | in-table | TL_message_layer68 (TLRPC.java:51027) |
| release-8.1.1_2431 | Message | 0xc3060325 | in-table | TL_message_old4 (TLRPC.java:52919) |
| release-8.1.1_2431 | Message | 0xc992e15c | in-table | TL_message_layer47 (TLRPC.java:51146) |
| release-8.1.1_2431 | Message | 0xf07814c8 | in-table | TL_message_old5 (TLRPC.java:52787) |
| release-8.1.1_2431 | Message | 0xf52e6b7f | in-table | TL_message_layer118 (TLRPC.java:51937) |
| release-8.1.1_2431 | Peer | 0x36c6019a | in-table | TL_peerChat (TLRPC.java:3351) |
| release-8.1.1_2431 | Peer | 0x59511722 | in-table | TL_peerUser (TLRPC.java:3323) |
| release-8.1.1_2431 | Peer | 0x9db1bc6d | in-table | TL_peerUser_layer131 (TLRPC.java:3365) |
| release-8.1.1_2431 | Peer | 0xa2a5371e | in-table | TL_peerChannel (TLRPC.java:3337) |
| release-8.1.1_2431 | Peer | 0xbad0e5bb | in-table | TL_peerChat_layer131 (TLRPC.java:3379) |
| release-8.1.1_2431 | Peer | 0xbddde532 | in-table | TL_peerChannel_layer131 (TLRPC.java:3309) |
| release-8.1.1_2431 | MessageFwdHeader | 0x353a686b | in-table | TL_messageFwdHeader_layer118 (TLRPC.java:38729) |
| release-8.1.1_2431 | MessageFwdHeader | 0x559ebe6d | in-table | TL_messageFwdHeader_layer96 (TLRPC.java:38897) |
| release-8.1.1_2431 | MessageFwdHeader | 0x5f777dce | in-table | TL_messageFwdHeader (TLRPC.java:38669) |
| release-8.1.1_2431 | MessageFwdHeader | 0xc786ddcb | in-table | TL_messageFwdHeader_layer68 (TLRPC.java:38633) |
| release-8.1.1_2431 | MessageFwdHeader | 0xec338270 | in-table | TL_messageFwdHeader_layer112 (TLRPC.java:38795) |
| release-8.1.1_2431 | MessageFwdHeader | 0xfadff4ac | in-table | TL_messageFwdHeader_layer72 (TLRPC.java:38855) |
| release-8.1.1_2431 | MessageReplyHeader | 0xa6d57763 | in-table | TL_messageReplyHeader (TLRPC.java:22266) |
| release-8.1.1_2431 | MessageEntity | 0x020df5d0 | in-table | TL_messageEntityBlockquote (TLRPC.java:29446) |
| release-8.1.1_2431 | MessageEntity | 0x208e68c9 | in-table | TL_inputMessageEntityMentionName (TLRPC.java:29348) |
| release-8.1.1_2431 | MessageEntity | 0x28a20571 | in-table | TL_messageEntityCode (TLRPC.java:29414) |
| release-8.1.1_2431 | MessageEntity | 0x352dca58 | in-table | TL_messageEntityMentionName_layer131 (TLRPC.java:29330) |
| release-8.1.1_2431 | MessageEntity | 0x4c4e743f | in-table | TL_messageEntityCashtag (TLRPC.java:29367) |
| release-8.1.1_2431 | MessageEntity | 0x64e475c2 | in-table | TL_messageEntityEmail (TLRPC.java:29232) |
| release-8.1.1_2431 | MessageEntity | 0x6cef8ac7 | in-table | TL_messageEntityBotCommand (TLRPC.java:29216) |
| release-8.1.1_2431 | MessageEntity | 0x6ed02538 | in-table | TL_messageEntityUrl (TLRPC.java:29282) |
| release-8.1.1_2431 | MessageEntity | 0x6f635b0d | in-table | TL_messageEntityHashtag (TLRPC.java:29398) |
| release-8.1.1_2431 | MessageEntity | 0x73924be0 | in-table | TL_messageEntityPre (TLRPC.java:29248) |
| release-8.1.1_2431 | MessageEntity | 0x761e6af4 | in-table | TL_messageEntityBankCard (TLRPC.java:29478) |
| release-8.1.1_2431 | MessageEntity | 0x76a6d327 | in-table | TL_messageEntityTextUrl (TLRPC.java:29198) |
| release-8.1.1_2431 | MessageEntity | 0x826f8b60 | in-table | TL_messageEntityItalic (TLRPC.java:29298) |
| release-8.1.1_2431 | MessageEntity | 0x9b69e34b | in-table | TL_messageEntityPhone (TLRPC.java:29494) |
| release-8.1.1_2431 | MessageEntity | 0x9c4e7e8b | in-table | TL_messageEntityUnderline (TLRPC.java:29462) |
| release-8.1.1_2431 | MessageEntity | 0xbb92ba95 | in-table | TL_messageEntityUnknown (TLRPC.java:29266) |
| release-8.1.1_2431 | MessageEntity | 0xbd610bc9 | in-table | TL_messageEntityBold (TLRPC.java:29382) |
| release-8.1.1_2431 | MessageEntity | 0xbf0693d4 | in-table | TL_messageEntityStrike (TLRPC.java:29430) |
| release-8.1.1_2431 | MessageEntity | 0xdc7b1140 | in-table | TL_messageEntityMentionName (TLRPC.java:29509) |
| release-8.1.1_2431 | MessageEntity | 0xfa04579d | in-table | TL_messageEntityMention (TLRPC.java:29314) |
| release-8.1.1_2431 | MessageAction | 0x031224c3 | in-table | TL_messageActionChatJoinedByLink (TLRPC.java:20573) |
| release-8.1.1_2431 | MessageAction | 0x15cefd00 | in-table | TL_messageActionChatAddUser (TLRPC.java:20484) |
| release-8.1.1_2431 | MessageAction | 0x40699cd0 | in-table | TL_messageActionPaymentSent (TLRPC.java:20906) |
| release-8.1.1_2431 | MessageAction | 0x4792929b | in-table | TL_messageActionScreenshotTaken (TLRPC.java:20550) |
| release-8.1.1_2431 | MessageAction | 0x488a7337 | in-table | TL_messageActionChatAddUser_layer131 (TLRPC.java:20603) |
| release-8.1.1_2431 | MessageAction | 0x502f92f7 | in-table | TL_messageActionInviteToGroupCall (TLRPC.java:20364) |
| release-8.1.1_2431 | MessageAction | 0x51bdb021 | in-table | TL_messageActionChatMigrateTo_layer131 (TLRPC.java:20513) |
| release-8.1.1_2431 | MessageAction | 0x55555550 | in-table | TL_messageActionUserJoined (TLRPC.java:20686) |
| release-8.1.1_2431 | MessageAction | 0x55555551 | in-table | TL_messageActionUserUpdatedPhoto (TLRPC.java:20695) |
| release-8.1.1_2431 | MessageAction | 0x55555552 | in-table | TL_messageActionTTLChange (TLRPC.java:20732) |
| release-8.1.1_2431 | MessageAction | 0x55555557 | in-table | TL_messageActionCreatedBroadcastList (TLRPC.java:20646) |
| release-8.1.1_2431 | MessageAction | 0x555555f5 | in-table | TL_messageActionLoginUnknownLocation (TLRPC.java:20258) |
| release-8.1.1_2431 | MessageAction | 0x555555f7 | in-table | TL_messageEncryptedAction (TLRPC.java:20274) |
| release-8.1.1_2431 | MessageAction | 0x5e3cfc4b | in-table | TL_messageActionChatAddUser_old (TLRPC.java:20718) |
| release-8.1.1_2431 | MessageAction | 0x76b9f11a | in-table | TL_messageActionInviteToGroupCall_layer131 (TLRPC.java:20655) |
| release-8.1.1_2431 | MessageAction | 0x7a0d7f42 | in-table | TL_messageActionGroupCall (TLRPC.java:20411) |
| release-8.1.1_2431 | MessageAction | 0x7fcb13a8 | in-table | TL_messageActionChatEditPhoto (TLRPC.java:20536) |
| release-8.1.1_2431 | MessageAction | 0x80e11a7f | in-table | TL_messageActionPhoneCall (TLRPC.java:20862) |
| release-8.1.1_2431 | MessageAction | 0x92a72876 | in-table | TL_messageActionGameScore (TLRPC.java:20946) |
| release-8.1.1_2431 | MessageAction | 0x94bd38ed | in-table | TL_messageActionPinMessage (TLRPC.java:20823) |
| release-8.1.1_2431 | MessageAction | 0x95d2ac92 | in-table | TL_messageActionChannelCreate (TLRPC.java:20775) |
| release-8.1.1_2431 | MessageAction | 0x95e3fbef | in-table | TL_messageActionChatDeletePhoto (TLRPC.java:20853) |
| release-8.1.1_2431 | MessageAction | 0x98e0d697 | in-table | TL_messageActionGeoProximityReached (TLRPC.java:20832) |
| release-8.1.1_2431 | MessageAction | 0x9fbab604 | in-table | TL_messageActionHistoryClear (TLRPC.java:20527) |
| release-8.1.1_2431 | MessageAction | 0xa43f30cc | in-table | TL_messageActionChatDeleteUser (TLRPC.java:20470) |
| release-8.1.1_2431 | MessageAction | 0xa6638b9a | in-table | TL_messageActionChatCreate_layer131 (TLRPC.java:20333) |
| release-8.1.1_2431 | MessageAction | 0xaa1afbfd | in-table | TL_messageActionSetMessagesTTL (TLRPC.java:20746) |
| release-8.1.1_2431 | MessageAction | 0xaa786345 | in-table | TL_messageActionSetChatTheme (TLRPC.java:20433) |
| release-8.1.1_2431 | MessageAction | 0xabe9affe | in-table | TL_messageActionBotAllowed (TLRPC.java:20922) |
| release-8.1.1_2431 | MessageAction | 0xb055eaee | in-table | TL_messageActionChannelMigrateFrom_layer131 (TLRPC.java:20587) |
| release-8.1.1_2431 | MessageAction | 0xb2ae9b0c | in-table | TL_messageActionChatDeleteUser_layer131 (TLRPC.java:20632) |
| release-8.1.1_2431 | MessageAction | 0xb3a07661 | in-table | TL_messageActionGroupCallScheduled (TLRPC.java:20962) |
| release-8.1.1_2431 | MessageAction | 0xb5a1ce5a | in-table | TL_messageActionChatEditTitle (TLRPC.java:20892) |
| release-8.1.1_2431 | MessageAction | 0xb6aef7b0 | in-table | TL_messageActionEmpty (TLRPC.java:20937) |
| release-8.1.1_2431 | MessageAction | 0xbd47cbad | in-table | TL_messageActionChatCreate (TLRPC.java:20302) |
| release-8.1.1_2431 | MessageAction | 0xd95c6154 | in-table | TL_messageActionSecureValuesSent (TLRPC.java:20789) |
| release-8.1.1_2431 | MessageAction | 0xe1037f92 | in-table | TL_messageActionChatMigrateTo (TLRPC.java:20559) |
| release-8.1.1_2431 | MessageAction | 0xea3948e9 | in-table | TL_messageActionChannelMigrateFrom (TLRPC.java:20395) |
| release-8.1.1_2431 | MessageAction | 0xf3f25f76 | in-table | TL_messageActionContactSignUp (TLRPC.java:20709) |
| release-8.1.1_2431 | MessageAction | 0xf89cf5e8 | in-table | TL_messageActionChatJoinedByLink_layer131 (TLRPC.java:20761) |
| release-8.1.1_2431 | MessageAction | 0xfae69f56 | in-table | TL_messageActionCustomAction (TLRPC.java:20288) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0x57adc690 | in-table | TL_phoneCallDiscardReasonHangup (TLRPC.java:17464) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0x85e42301 | in-table | TL_phoneCallDiscardReasonMissed (TLRPC.java:17482) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0xe095c1a0 | in-table | TL_phoneCallDiscardReasonDisconnect (TLRPC.java:17491) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0xfaf7e8c9 | in-table | TL_phoneCallDiscardReasonBusy (TLRPC.java:17473) |
| release-8.1.1_2431 | MessageMedia | 0x29632a36 | in-table | TL_messageMediaUnsupported_old (TLRPC.java:7006) |
| release-8.1.1_2431 | MessageMedia | 0x2ec0533f | in-table | TL_messageMediaVenue (TLRPC.java:7107) |
| release-8.1.1_2431 | MessageMedia | 0x2fda2204 | in-table | TL_messageMediaDocument_old (TLRPC.java:7229) |
| release-8.1.1_2431 | MessageMedia | 0x3d8ce53d | in-table | TL_messageMediaPhoto_layer68 (TLRPC.java:7417) |
| release-8.1.1_2431 | MessageMedia | 0x3ded6320 | in-table | TL_messageMediaEmpty (TLRPC.java:7098) |
| release-8.1.1_2431 | MessageMedia | 0x3f7ee58b | in-table | TL_messageMediaDice (TLRPC.java:7492) |
| release-8.1.1_2431 | MessageMedia | 0x4bd6e798 | in-table | TL_messageMediaPoll (TLRPC.java:7286) |
| release-8.1.1_2431 | MessageMedia | 0x56e0d474 | in-table | TL_messageMediaGeo (TLRPC.java:7523) |
| release-8.1.1_2431 | MessageMedia | 0x5bcf1675 | in-table | TL_messageMediaVideo_layer45 (TLRPC.java:7433) |
| release-8.1.1_2431 | MessageMedia | 0x5e7d2f39 | in-table | TL_messageMediaContact_layer81 (TLRPC.java:7397) |
| release-8.1.1_2431 | MessageMedia | 0x638fe46b | in-table | TL_messageMediaDice_layer111 (TLRPC.java:7510) |
| release-8.1.1_2431 | MessageMedia | 0x695150d7 | in-table | TL_messageMediaPhoto (TLRPC.java:7259) |
| release-8.1.1_2431 | MessageMedia | 0x70322949 | in-table | TL_messageMediaContact (TLRPC.java:7470) |
| release-8.1.1_2431 | MessageMedia | 0x7912b71f | in-table | TL_messageMediaVenue_layer71 (TLRPC.java:7131) |
| release-8.1.1_2431 | MessageMedia | 0x7c3c2609 | in-table | TL_messageMediaGeoLive_layer119 (TLRPC.java:7367) |
| release-8.1.1_2431 | MessageMedia | 0x7c4414d3 | in-table | TL_messageMediaDocument_layer74 (TLRPC.java:7195) |
| release-8.1.1_2431 | MessageMedia | 0x84551347 | in-table | TL_messageMediaInvoice (TLRPC.java:7048) |
| release-8.1.1_2431 | MessageMedia | 0x9cb070d7 | in-table | TL_messageMediaDocument (TLRPC.java:7167) |
| release-8.1.1_2431 | MessageMedia | 0x9f84f49e | in-table | TL_messageMediaUnsupported (TLRPC.java:7089) |
| release-8.1.1_2431 | MessageMedia | 0xa2d24290 | in-table | TL_messageMediaVideo_old (TLRPC.java:7153) |
| release-8.1.1_2431 | MessageMedia | 0xa32dd600 | in-table | TL_messageMediaWebPage (TLRPC.java:7537) |
| release-8.1.1_2431 | MessageMedia | 0xb5223b0f | in-table | TL_messageMediaPhoto_layer74 (TLRPC.java:7304) |
| release-8.1.1_2431 | MessageMedia | 0xb940c666 | in-table | TL_messageMediaGeoLive (TLRPC.java:7337) |
| release-8.1.1_2431 | MessageMedia | 0xc6b68300 | in-table | TL_messageMediaAudio_layer45 (TLRPC.java:7020) |
| release-8.1.1_2431 | MessageMedia | 0xc8c45a2a | in-table | TL_messageMediaPhoto_old (TLRPC.java:7034) |
| release-8.1.1_2431 | MessageMedia | 0xcbf24940 | in-table | TL_messageMediaContact_layer131 (TLRPC.java:7449) |
| release-8.1.1_2431 | MessageMedia | 0xf3e02ea8 | in-table | TL_messageMediaDocument_layer68 (TLRPC.java:7243) |
| release-8.1.1_2431 | MessageMedia | 0xfdb19008 | in-table | TL_messageMediaGame (TLRPC.java:7383) |
| release-8.1.1_2431 | Photo | 0x22b56751 | in-table | TL_photo_old (TLRPC.java:29771) |
| release-8.1.1_2431 | Photo | 0x2331b22d | in-table | TL_photoEmpty (TLRPC.java:29757) |
| release-8.1.1_2431 | Photo | 0x9288dd29 | in-table | TL_photo_layer82 (TLRPC.java:29935) |
| release-8.1.1_2431 | Photo | 0x9c477dd8 | in-table | TL_photo_layer97 (TLRPC.java:29630) |
| release-8.1.1_2431 | Photo | 0xc3838076 | in-table | TL_photo_old2 (TLRPC.java:29714) |
| release-8.1.1_2431 | Photo | 0xcded42fe | in-table | TL_photo_layer55 (TLRPC.java:29675) |
| release-8.1.1_2431 | Photo | 0xd07504a5 | in-table | TL_photo_layer115 (TLRPC.java:29816) |
| release-8.1.1_2431 | Photo | 0xfb197a65 | in-table | TL_photo (TLRPC.java:29863) |
| release-8.1.1_2431 | Document | 0x1e87342b | in-table | TL_document (TLRPC.java:23058) |
| release-8.1.1_2431 | Document | 0x36f8c871 | in-table | TL_documentEmpty (TLRPC.java:23261) |
| release-8.1.1_2431 | Document | 0x55555556 | in-table | TL_documentEncrypted_old (TLRPC.java:23024) |
| release-8.1.1_2431 | Document | 0x55555558 | in-table | TL_documentEncrypted (TLRPC.java:23275) |
| release-8.1.1_2431 | Document | 0x59534e4c | in-table | TL_document_layer92 (TLRPC.java:22926) |
| release-8.1.1_2431 | Document | 0x87232bc7 | in-table | TL_document_layer82 (TLRPC.java:22975) |
| release-8.1.1_2431 | Document | 0x9ba29cc1 | in-table | TL_document_layer113 (TLRPC.java:23157) |
| release-8.1.1_2431 | Document | 0x9efc6326 | in-table | TL_document_old (TLRPC.java:23231) |
| release-8.1.1_2431 | Document | 0xf9a39f4f | in-table | TL_document_layer53 (TLRPC.java:23326) |
| release-8.1.1_2431 | GeoPoint | 0x0296f104 | in-table | TL_geoPoint_layer119 (TLRPC.java:5029) |
| release-8.1.1_2431 | GeoPoint | 0x1117dd5f | in-table | TL_geoPointEmpty (TLRPC.java:5063) |
| release-8.1.1_2431 | GeoPoint | 0x2049d70c | in-table | TL_geoPoint_layer81 (TLRPC.java:5047) |
| release-8.1.1_2431 | GeoPoint | 0xb2a2f663 | in-table | TL_geoPoint (TLRPC.java:5072) |
| release-8.1.1_2431 | WebPage | 0x5f07b4bc | in-table | TL_webPage_layer104 (TLRPC.java:16602) |
| release-8.1.1_2431 | WebPage | 0x7311ca11 | in-table | TL_webPageNotModified (TLRPC.java:16841) |
| release-8.1.1_2431 | WebPage | 0x85849473 | in-table | TL_webPageNotModified_layer110 (TLRPC.java:16832) |
| release-8.1.1_2431 | WebPage | 0xa31ea0b5 | in-table | TL_webPage_old (TLRPC.java:16702) |
| release-8.1.1_2431 | WebPage | 0xc586da1c | in-table | TL_webPagePending (TLRPC.java:16816) |
| release-8.1.1_2431 | WebPage | 0xca820ed7 | in-table | TL_webPage_layer58 (TLRPC.java:16862) |
| release-8.1.1_2431 | WebPage | 0xd41a5167 | in-table | TL_webPageUrlPending (TLRPC.java:16802) |
| release-8.1.1_2431 | WebPage | 0xe89c45b2 | in-table | TL_webPage (TLRPC.java:16354) |
| release-8.1.1_2431 | WebPage | 0xeb1477e8 | in-table | TL_webPageEmpty (TLRPC.java:16788) |
| release-8.1.1_2431 | WebPage | 0xfa64e172 | in-table | TL_webPage_layer107 (TLRPC.java:16479) |
| release-8.1.1_2431 | User | 0x075cf7a8 | in-table | TL_userForeign_old2 (TLRPC.java:19315) |
| release-8.1.1_2431 | User | 0x1c60e608 | in-table | TL_userSelf_old3 (TLRPC.java:19205) |
| release-8.1.1_2431 | User | 0x200250ba | in-table | TL_userEmpty_layer131 (TLRPC.java:19251) |
| release-8.1.1_2431 | User | 0x22e49072 | in-table | TL_user_old (TLRPC.java:19485) |
| release-8.1.1_2431 | User | 0x22e8ceb0 | in-table | TL_userRequest_old (TLRPC.java:19265) |
| release-8.1.1_2431 | User | 0x2e13f4c3 | in-table | TL_user_layer104 (TLRPC.java:19071) |
| release-8.1.1_2431 | User | 0x3ff6ecb0 | in-table | TL_user (TLRPC.java:18809) |
| release-8.1.1_2431 | User | 0x5214c89d | in-table | TL_userForeign_old (TLRPC.java:19291) |
| release-8.1.1_2431 | User | 0x7007b451 | in-table | TL_userSelf_old2 (TLRPC.java:19567) |
| release-8.1.1_2431 | User | 0x720535ec | in-table | TL_userSelf_old (TLRPC.java:19179) |
| release-8.1.1_2431 | User | 0x938458c1 | in-table | TL_user_layer131 (TLRPC.java:18940) |
| release-8.1.1_2431 | User | 0xb29ad7cc | in-table | TL_userDeleted_old (TLRPC.java:19369) |
| release-8.1.1_2431 | User | 0xcab35e18 | in-table | TL_userContact_old2 (TLRPC.java:18741) |
| release-8.1.1_2431 | User | 0xd10d979a | in-table | TL_user_layer65 (TLRPC.java:19387) |
| release-8.1.1_2431 | User | 0xd3bc4b7a | in-table | TL_userEmpty (TLRPC.java:18795) |
| release-8.1.1_2431 | User | 0xd6016d7a | in-table | TL_userDeleted_old2 (TLRPC.java:19231) |
| release-8.1.1_2431 | User | 0xd9ccc4ef | in-table | TL_userRequest_old2 (TLRPC.java:19341) |
| release-8.1.1_2431 | User | 0xf2fb8319 | in-table | TL_userContact_old (TLRPC.java:18769) |
| release-8.1.1_2431 | Chat | 0x07328bdb | in-table | TL_chatForbidden_layer131 (TLRPC.java:36849) |
| release-8.1.1_2431 | Chat | 0x0cb44b1c | in-table | TL_channel_layer72 (TLRPC.java:37308) |
| release-8.1.1_2431 | Chat | 0x17d493d5 | in-table | TL_channelForbidden (TLRPC.java:36561) |
| release-8.1.1_2431 | Chat | 0x289da732 | in-table | TL_channelForbidden_layer131 (TLRPC.java:36591) |
| release-8.1.1_2431 | Chat | 0x29562865 | in-table | TL_chatEmpty (TLRPC.java:53658) |
| release-8.1.1_2431 | Chat | 0x2d85832c | in-table | TL_channelForbidden_layer52 (TLRPC.java:36700) |
| release-8.1.1_2431 | Chat | 0x3bda1bde | in-table | TL_chat_layer131 (TLRPC.java:36505) |
| release-8.1.1_2431 | Chat | 0x41cbf256 | in-table | TL_chat (TLRPC.java:36449) |
| release-8.1.1_2431 | Chat | 0x450b7115 | in-table | TL_channel_layer77 (TLRPC.java:37231) |
| release-8.1.1_2431 | Chat | 0x4b1b7506 | in-table | TL_channel_layer48 (TLRPC.java:36645) |
| release-8.1.1_2431 | Chat | 0x4df30834 | in-table | TL_channel_layer104 (TLRPC.java:37115) |
| release-8.1.1_2431 | Chat | 0x6592a1a7 | in-table | TL_chatForbidden (TLRPC.java:36833) |
| release-8.1.1_2431 | Chat | 0x678e9587 | in-table | TL_channel_old (TLRPC.java:36926) |
| release-8.1.1_2431 | Chat | 0x6e9c9bc7 | in-table | TL_chat_old (TLRPC.java:36973) |
| release-8.1.1_2431 | Chat | 0x7312bc48 | in-table | TL_chat_old2 (TLRPC.java:36416) |
| release-8.1.1_2431 | Chat | 0x8261ac61 | in-table | TL_channel (TLRPC.java:36718) |
| release-8.1.1_2431 | Chat | 0x8537784f | in-table | TL_channelForbidden_layer67 (TLRPC.java:36621) |
| release-8.1.1_2431 | Chat | 0x9ba2d800 | in-table | TL_chatEmpty_layer131 (TLRPC.java:53674) |
| release-8.1.1_2431 | Chat | 0xa14dca52 | in-table | TL_channel_layer67 (TLRPC.java:36865) |
| release-8.1.1_2431 | Chat | 0xc88974ac | in-table | TL_channel_layer92 (TLRPC.java:37380) |
| release-8.1.1_2431 | Chat | 0xd31a961e | in-table | TL_channel_layer131 (TLRPC.java:36998) |
| release-8.1.1_2431 | Chat | 0xd91cdd54 | in-table | TL_chat_layer92 (TLRPC.java:37457) |
| release-8.1.1_2431 | Chat | 0xfb0ccc41 | in-table | TL_chatForbidden_old (TLRPC.java:36398) |
| release-8.1.1_2431 | PhotoSize | 0x021e1ad6 | in-table | TL_photoCachedSize (TLRPC.java:39360) |
| release-8.1.1_2431 | PhotoSize | 0x0e17e23c | in-table | TL_photoSizeEmpty (TLRPC.java:39304) |
| release-8.1.1_2431 | PhotoSize | 0x5aa86a51 | in-table | TL_photoSizeProgressive_layer127 (TLRPC.java:39242) |
| release-8.1.1_2431 | PhotoSize | 0x75c78e60 | in-table | TL_photoSize (TLRPC.java:39340) |
| release-8.1.1_2431 | PhotoSize | 0x77bfb61b | in-table | TL_photoSize_layer127 (TLRPC.java:39282) |
| release-8.1.1_2431 | PhotoSize | 0xd8214d41 | in-table | TL_photoPathSize (TLRPC.java:49935) |
| release-8.1.1_2431 | PhotoSize | 0xe0b0bc2e | in-table | TL_photoStrippedSize (TLRPC.java:39225) |
| release-8.1.1_2431 | PhotoSize | 0xe9a734fa | in-table | TL_photoCachedSize_layer127 (TLRPC.java:39318) |
| release-8.1.1_2431 | PhotoSize | 0xfa3efb95 | in-table | TL_photoSizeProgressive (TLRPC.java:39380) |
| release-8.1.1_2431 | VideoSize | 0x435bb987 | in-table | TL_videoSize_layer115 (TLRPC.java:15469) |
| release-8.1.1_2431 | VideoSize | 0xde33b094 | in-table | TL_videoSize (TLRPC.java:15521) |
| release-8.1.1_2431 | VideoSize | 0xe831c556 | in-table | TL_videoSize_layer127 (TLRPC.java:15491) |
| release-8.1.1_2431 | DocumentAttribute | 0x051448e5 | in-table | TL_documentAttributeAudio_old (TLRPC.java:1348) |
| release-8.1.1_2431 | DocumentAttribute | 0x0ef02ce6 | in-table | TL_documentAttributeVideo (TLRPC.java:1411) |
| release-8.1.1_2431 | DocumentAttribute | 0x11b58939 | in-table | TL_documentAttributeAnimated (TLRPC.java:1388) |
| release-8.1.1_2431 | DocumentAttribute | 0x15590068 | in-table | TL_documentAttributeFilename (TLRPC.java:1397) |
| release-8.1.1_2431 | DocumentAttribute | 0x3a556302 | in-table | TL_documentAttributeSticker_layer55 (TLRPC.java:1332) |
| release-8.1.1_2431 | DocumentAttribute | 0x5910cccb | in-table | TL_documentAttributeVideo_layer65 (TLRPC.java:1434) |
| release-8.1.1_2431 | DocumentAttribute | 0x6319d612 | in-table | TL_documentAttributeSticker (TLRPC.java:1362) |
| release-8.1.1_2431 | DocumentAttribute | 0x6c37c15c | in-table | TL_documentAttributeImageSize (TLRPC.java:1505) |
| release-8.1.1_2431 | DocumentAttribute | 0x9801d2f7 | in-table | TL_documentAttributeHasStickers (TLRPC.java:1482) |
| release-8.1.1_2431 | DocumentAttribute | 0x9852f9c6 | in-table | TL_documentAttributeAudio (TLRPC.java:1521) |
| release-8.1.1_2431 | DocumentAttribute | 0x994c9882 | in-table | TL_documentAttributeSticker_old2 (TLRPC.java:1491) |
| release-8.1.1_2431 | DocumentAttribute | 0xded218e0 | in-table | TL_documentAttributeAudio_layer45 (TLRPC.java:1452) |
| release-8.1.1_2431 | DocumentAttribute | 0xfb0a5727 | in-table | TL_documentAttributeSticker_old (TLRPC.java:1470) |
| release-8.1.1_2431 | FileLocation | 0x091d11eb | in-table | TL_fileLocation_layer97 (TLRPC.java:39028) |
| release-8.1.1_2431 | FileLocation | 0x53d69076 | in-table | TL_fileLocation_layer82 (TLRPC.java:39008) |
| release-8.1.1_2431 | FileLocation | 0x55555554 | in-table | TL_fileEncryptedLocation (TLRPC.java:39066) |
| release-8.1.1_2431 | FileLocation | 0x7c596b46 | in-table | TL_fileLocationUnavailable (TLRPC.java:38990) |
| release-8.1.1_2431 | FileLocation | 0xbc7fc6cd | in-table | TL_fileLocationToBeDeprecated (TLRPC.java:39050) |
| release-8.1.1_2431 | InputStickerSet | 0x028703c8 | in-table | TL_inputStickerSetAnimatedEmoji (TLRPC.java:25441) |
| release-8.1.1_2431 | InputStickerSet | 0x861cc8a0 | in-table | TL_inputStickerSetShortName (TLRPC.java:25427) |
| release-8.1.1_2431 | InputStickerSet | 0x9de7a269 | in-table | TL_inputStickerSetID (TLRPC.java:25411) |
| release-8.1.1_2431 | InputStickerSet | 0xe67f520e | in-table | TL_inputStickerSetDice (TLRPC.java:25387) |
| release-8.1.1_2431 | InputStickerSet | 0xffb62b95 | in-table | TL_inputStickerSetEmpty (TLRPC.java:25402) |
| release-8.1.1_2431 | TL_maskCoords | 0xaed6dbb2 | in-table | TL_maskCoords (TLRPC.java:24543) |
| release-8.1.1_2431 | Video | 0x388fa391 | in-table | TL_video_old2 (TLRPC.java:21930) |
| release-8.1.1_2431 | Video | 0x55555553 | in-table | TL_videoEncrypted (TLRPC.java:21858) |
| release-8.1.1_2431 | Video | 0x5a04a49f | in-table | TL_video_old (TLRPC.java:21896) |
| release-8.1.1_2431 | Video | 0xc10658a8 | in-table | TL_videoEmpty_layer45 (TLRPC.java:21966) |
| release-8.1.1_2431 | Video | 0xee9f4a4d | in-table | TL_video_old3 (TLRPC.java:21794) |
| release-8.1.1_2431 | Video | 0xf72887d3 | in-table | TL_video_layer45 (TLRPC.java:21826) |
| release-8.1.1_2431 | Audio | 0x427425e7 | in-table | TL_audio_old (TLRPC.java:8812) |
| release-8.1.1_2431 | Audio | 0x555555f6 | in-table | TL_audioEncrypted (TLRPC.java:8838) |
| release-8.1.1_2431 | Audio | 0x586988d8 | in-table | TL_audioEmpty_layer45 (TLRPC.java:8772) |
| release-8.1.1_2431 | Audio | 0xc7ac6496 | in-table | TL_audio_old2 (TLRPC.java:8868) |
| release-8.1.1_2431 | Audio | 0xf9e35055 | in-table | TL_audio_layer45 (TLRPC.java:8786) |
| release-8.1.1_2431 | MessageReplies | 0x4128faac | in-table | TL_messageReplies_layer131 (TLRPC.java:9299) |
| release-8.1.1_2431 | MessageReplies | 0x83d60fc2 | in-table | TL_messageReplies (TLRPC.java:9362) |
| release-8.1.1_2431 | MessageReactions | 0xb87a24d1 | in-table | TL_messageReactions (TLRPC.java:21693) |
| release-8.1.1_2431 | Reaction | - | type not defined at this pin | - |
| release-8.1.1_2431 | MessagePeerReaction | - | type not defined at this pin | - |
| release-8.1.1_2431 | MessageReactor | - | type not defined at this pin | - |
| release-8.1.1_2431 | RestrictionReason | 0xd072acb4 | in-table | TL_restrictionReason (TLRPC.java:37496) |
| release-8.1.1_2431 | TL_factCheck | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_textWithEntities | - | type not defined at this pin | - |
| release-8.1.1_2431 | SuggestedPost | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_stars.StarsAmount | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_messages_emojiGameOutcome | - | type not defined at this pin | - |
| release-8.1.1_2431 | ReactionCount | 0x6fb250d1 | in-table | TL_reactionCount (TLRPC.java:39751) |
| release-8.1.1_2431 | WebPageAttribute | - | type not defined at this pin | - |
| release-8.1.1_2431 | Poll | 0x86e18161 | in-table | TL_poll (TLRPC.java:31208) |
| release-8.1.1_2431 | Poll | 0xaf746786 | in-table | TL_poll_toDelete (TLRPC.java:31267) |
| release-8.1.1_2431 | Poll | 0xd5529d06 | in-table | TL_poll_layer111 (TLRPC.java:31320) |
| release-8.1.1_2431 | PollResults | 0x5755785a | in-table | TL_pollResults_layer108 (TLRPC.java:3799) |
| release-8.1.1_2431 | PollResults | 0xbadcc1a3 | in-table | TL_pollResults_layer131 (TLRPC.java:3914) |
| release-8.1.1_2431 | PollResults | 0xc87024a2 | in-table | TL_pollResults_layer111 (TLRPC.java:3846) |
| release-8.1.1_2431 | PollResults | 0xdcb82ea3 | in-table | TL_pollResults (TLRPC.java:4013) |
| release-8.1.1_2431 | ReplyMarkup | 0x3502758c | in-table | TL_replyKeyboardMarkup_layer129 (TLRPC.java:9693) |
| release-8.1.1_2431 | ReplyMarkup | 0x48a30254 | in-table | TL_replyInlineMarkup (TLRPC.java:9752) |
| release-8.1.1_2431 | ReplyMarkup | 0x85dd99d1 | in-table | TL_replyKeyboardMarkup (TLRPC.java:9606) |
| release-8.1.1_2431 | ReplyMarkup | 0x86b40b08 | in-table | TL_replyKeyboardForceReply (TLRPC.java:9669) |
| release-8.1.1_2431 | ReplyMarkup | 0xa03e5b85 | in-table | TL_replyKeyboardHide (TLRPC.java:9653) |
| release-8.1.1_2431 | ReplyMarkup | 0xf4108aa0 | in-table | TL_replyKeyboardForceReply_layer129 (TLRPC.java:9734) |
| release-8.1.1_2431 | InputUser | 0x1da448e2 | in-table | TL_inputUserFromMessage (TLRPC.java:15120) |
| release-8.1.1_2431 | InputUser | 0xb98886cf | in-table | TL_inputUserEmpty (TLRPC.java:15111) |
| release-8.1.1_2431 | InputUser | 0xf21158c6 | in-table | TL_inputUser (TLRPC.java:15095) |
| release-8.1.1_2431 | InputUser | 0xf7c1b13f | in-table | TL_inputUserSelf (TLRPC.java:15086) |
| release-8.1.1_2431 | TL_iv.RichText | 0x034b8621 | in-table | TL_textMarked (TLRPC.java:1672) |
| release-8.1.1_2431 | TL_iv.RichText | 0x081ccf4f | in-table | TL_textImage (TLRPC.java:1590) |
| release-8.1.1_2431 | TL_iv.RichText | 0x1ccb966a | in-table | TL_textPhone (TLRPC.java:1557) |
| release-8.1.1_2431 | TL_iv.RichText | 0x35553762 | in-table | TL_textAnchor (TLRPC.java:1639) |
| release-8.1.1_2431 | TL_iv.RichText | 0x3c2884c1 | in-table | TL_textUrl (TLRPC.java:1620) |
| release-8.1.1_2431 | TL_iv.RichText | 0x6724abc4 | in-table | TL_textBold (TLRPC.java:1767) |
| release-8.1.1_2431 | TL_iv.RichText | 0x6c3f19b9 | in-table | TL_textFixed (TLRPC.java:1687) |
| release-8.1.1_2431 | TL_iv.RichText | 0x744694e0 | in-table | TL_textPlain (TLRPC.java:1719) |
| release-8.1.1_2431 | TL_iv.RichText | 0x7e6260d7 | in-table | TL_textConcat (TLRPC.java:1734) |
| release-8.1.1_2431 | TL_iv.RichText | 0x9bf8bb95 | in-table | TL_textStrike (TLRPC.java:1657) |
| release-8.1.1_2431 | TL_iv.RichText | 0xc12622c4 | in-table | TL_textUnderline (TLRPC.java:1797) |
| release-8.1.1_2431 | TL_iv.RichText | 0xc7fb5e01 | in-table | TL_textSuperscript (TLRPC.java:1575) |
| release-8.1.1_2431 | TL_iv.RichText | 0xd912a59c | in-table | TL_textItalic (TLRPC.java:1782) |
| release-8.1.1_2431 | TL_iv.RichText | 0xdc3d824f | in-table | TL_textEmpty (TLRPC.java:1611) |
| release-8.1.1_2431 | TL_iv.RichText | 0xde5a0dd6 | in-table | TL_textEmail (TLRPC.java:1702) |
| release-8.1.1_2431 | TL_iv.RichText | 0xed6a8504 | in-table | TL_textSubscript (TLRPC.java:1812) |
| release-8.1.1_2431 | TL_iv.Page | 0x556ec7aa | in-table | TL_pageFull_layer82 (TLRPC.java:14753) |
| release-8.1.1_2431 | TL_iv.Page | 0x8dee6c44 | in-table | TL_pagePart_layer67 (TLRPC.java:14528) |
| release-8.1.1_2431 | TL_iv.Page | 0x8e3f9ebe | in-table | TL_pagePart_layer82 (TLRPC.java:14603) |
| release-8.1.1_2431 | TL_iv.Page | 0x98657f0d | in-table | TL_page (TLRPC.java:14911) |
| release-8.1.1_2431 | TL_iv.Page | 0xae891bec | in-table | TL_page_layer110 (TLRPC.java:14828) |
| release-8.1.1_2431 | TL_iv.Page | 0xd7a19d69 | in-table | TL_pageFull_layer67 (TLRPC.java:14678) |
| release-8.1.1_2431 | TL_iv.RichMessage | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_keyboard.KeyboardButtonRow | 0x77608b83 | in-table | TL_keyboardButtonRow (TLRPC.java:15978) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButtonRow | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0x0568a748 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonSwitchInline (TLRPC.java:15248) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0x10b78d29 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonUrlAuth (TLRPC.java:15282) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0x258aff05 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonUrl (TLRPC.java:15232) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0x35bbdb6b | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonCallback (TLRPC.java:15372) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0x50f41ccf | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonGame (TLRPC.java:15218) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0x683a5e46 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonCallback_layer117 (TLRPC.java:15392) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xa2fa4880 | in-table | TL_keyboardButton (TLRPC.java:15408) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xafd93fbb | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonBuy (TLRPC.java:15358) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xb16a6c29 | in-table | TL_keyboardButtonRequestPhone (TLRPC.java:15204) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xbbc7515d | in-table | TL_keyboardButtonRequestPoll (TLRPC.java:15336) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xd02e7fd4 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_inputKeyboardButtonUrlAuth (TLRPC.java:15308) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xfc796b3f | in-table | TL_keyboardButtonRequestGeoLocation (TLRPC.java:15268) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_keyboard.KeyboardButtonStyle | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_keyboard.ButtonType | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_keyboard.InlineButtonType | - | type not defined at this pin | - |
| release-8.1.1_2431 | RequestPeerType | - | type not defined at this pin | - |
| release-8.1.1_2431 | TL_chatAdminRights | 0x5fb224d5 | in-table | TL_chatAdminRights (TLRPC.java:3671) |
| release-8.1.1_2431 | InputPeer | 0x179be863 | in-table | TL_inputPeerChat_layer131 (TLRPC.java:41456) |
| release-8.1.1_2431 | InputPeer | 0x17bae2e6 | in-table | TL_inputPeerUserFromMessage_layer131 (TLRPC.java:41577) |
| release-8.1.1_2431 | InputPeer | 0x20adaef8 | in-table | TL_inputPeerChannel_layer131 (TLRPC.java:41543) |
| release-8.1.1_2431 | InputPeer | 0x27bcbbfc | in-table | TL_inputPeerChannel (TLRPC.java:41470) |
| release-8.1.1_2431 | InputPeer | 0x35a95cb9 | in-table | TL_inputPeerChat (TLRPC.java:41529) |
| release-8.1.1_2431 | InputPeer | 0x7b8e7de6 | in-table | TL_inputPeerUser_layer131 (TLRPC.java:41486) |
| release-8.1.1_2431 | InputPeer | 0x7da07ec9 | in-table | TL_inputPeerSelf (TLRPC.java:41413) |
| release-8.1.1_2431 | InputPeer | 0x7f3b18ea | in-table | TL_inputPeerEmpty (TLRPC.java:41502) |
| release-8.1.1_2431 | InputPeer | 0x9c95f7bb | in-table | TL_inputPeerChannelFromMessage_layer131 (TLRPC.java:41559) |
| release-8.1.1_2431 | InputPeer | 0xa87b0a1c | in-table | TL_inputPeerUserFromMessage (TLRPC.java:41422) |
| release-8.1.1_2431 | InputPeer | 0xbd2a0840 | in-table | TL_inputPeerChannelFromMessage (TLRPC.java:41511) |
| release-8.1.1_2431 | InputPeer | 0xdde8a54c | in-table | TL_inputPeerUser (TLRPC.java:41440) |
| release-8.1.1_2431 | InlineQueryPeerType | - | type not defined at this pin | - |
| release-8.1.1_2431 | PollAnswer | 0x6ca9c2e9 | in-table | TL_pollAnswer (TLRPC.java:2331) |
| release-8.1.1_2431 | PollAnswerVoters | 0x3b6ddad2 | in-table | TL_pollAnswerVoters (TLRPC.java:4279) |

Body comparison with A-HEAD: every row of `constructors.tsv` whose ID the switch of its type reads at this pin, what its body reads at this pin against its A-HEAD reading (field order, flag bits and wire types; flag-only trues, `mType.` holders and field names set aside, object types named by the deserializer a call reaches, ruling T1a). `equal on the wire` lists the field names that differ (informational); `membership-only` names the line the reduction cannot read; `compatible widening (union)` an older row of the button union that the union check below holds for (Q5, ruling T2a).

| Pin | Type | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| release-8.1.1_2431 | Message | 0x05f46804 | equal | TL_messageForwarded_old (TLRPC.java:53105) |
| release-8.1.1_2431 | Message | 0x1c9b1027 | equal | TL_message_layer104_2 (TLRPC.java:52420) |
| release-8.1.1_2431 | Message | 0x1d86f70e | equal | TL_messageService_old2 (TLRPC.java:50874) |
| release-8.1.1_2431 | Message | 0x22eb6aba | equal | TL_message_old (TLRPC.java:53146) |
| release-8.1.1_2431 | Message | 0x286fa604 | equal | TL_messageService_layer123 (TLRPC.java:53373) |
| release-8.1.1_2431 | Message | 0x2b085862 | equal | TL_messageService (TLRPC.java:53420) |
| release-8.1.1_2431 | Message | 0x2bebfa86 | equal | TL_message_old6 (TLRPC.java:52693) |
| release-8.1.1_2431 | Message | 0x44f9b43d | equal | TL_message_layer104 (TLRPC.java:52564) |
| release-8.1.1_2431 | Message | 0x452c0e65 | equal | TL_message_layer117 (TLRPC.java:52100) |
| release-8.1.1_2431 | Message | 0x555555f8 | equal | TL_message_secret_old (TLRPC.java:53333) |
| release-8.1.1_2431 | Message | 0x555555f9 | equal | TL_message_secret_layer72 (TLRPC.java:53259) |
| release-8.1.1_2431 | Message | 0x555555fa | equal | TL_message_secret (TLRPC.java:53179) |
| release-8.1.1_2431 | Message | 0x567699b3 | equal | TL_message_old2 (TLRPC.java:53038) |
| release-8.1.1_2431 | Message | 0x58ae39c9 | equal | TL_message_layer123 (TLRPC.java:51768) |
| release-8.1.1_2431 | Message | 0x5ba66c13 | equal | TL_message_old7 (TLRPC.java:51262) |
| release-8.1.1_2431 | Message | 0x83e5de54 | equal | TL_messageEmpty_layer122 (TLRPC.java:50835) |
| release-8.1.1_2431 | Message | 0x85d6cbe2 | equal | TL_message (TLRPC.java:51418) |
| release-8.1.1_2431 | Message | 0x90a6ca84 | equal | TL_messageEmpty (TLRPC.java:50850) |
| release-8.1.1_2431 | Message | 0x90dddc11 | equal | TL_message_layer72 (TLRPC.java:50908) |
| release-8.1.1_2431 | Message | 0x9789dac4 | equal | TL_message_layer104_3 (TLRPC.java:52257) |
| release-8.1.1_2431 | Message | 0x9e19a1f6 | equal | TL_messageService_layer118 (TLRPC.java:53472) |
| release-8.1.1_2431 | Message | 0x9f8d60bb | equal | TL_messageService_old (TLRPC.java:53077) |
| release-8.1.1_2431 | Message | 0xa367e716 | equal | TL_messageForwarded_old2 (TLRPC.java:51372) |
| release-8.1.1_2431 | Message | 0xa7ab1991 | equal | TL_message_old3 (TLRPC.java:52982) |
| release-8.1.1_2431 | Message | 0xbce383d2 | equal | TL_message_layer131 (TLRPC.java:51593) |
| release-8.1.1_2431 | Message | 0xc06b9607 | equal | TL_messageService_layer48 (TLRPC.java:52875) |
| release-8.1.1_2431 | Message | 0xc09be45f | equal | TL_message_layer68 (TLRPC.java:51027) |
| release-8.1.1_2431 | Message | 0xc3060325 | equal | TL_message_old4 (TLRPC.java:52919) |
| release-8.1.1_2431 | Message | 0xc992e15c | equal | TL_message_layer47 (TLRPC.java:51146) |
| release-8.1.1_2431 | Message | 0xf07814c8 | equal | TL_message_old5 (TLRPC.java:52787) |
| release-8.1.1_2431 | Message | 0xf52e6b7f | equal | TL_message_layer118 (TLRPC.java:51937) |
| release-8.1.1_2431 | Peer | 0x36c6019a | equal | TL_peerChat (TLRPC.java:3351) |
| release-8.1.1_2431 | Peer | 0x59511722 | equal | TL_peerUser (TLRPC.java:3323) |
| release-8.1.1_2431 | Peer | 0x9db1bc6d | equal | TL_peerUser_layer131 (TLRPC.java:3365) |
| release-8.1.1_2431 | Peer | 0xa2a5371e | equal | TL_peerChannel (TLRPC.java:3337) |
| release-8.1.1_2431 | Peer | 0xbad0e5bb | equal | TL_peerChat_layer131 (TLRPC.java:3379) |
| release-8.1.1_2431 | Peer | 0xbddde532 | equal | TL_peerChannel_layer131 (TLRPC.java:3309) |
| release-8.1.1_2431 | MessageFwdHeader | 0x353a686b | equal | TL_messageFwdHeader_layer118 (TLRPC.java:38729) |
| release-8.1.1_2431 | MessageFwdHeader | 0x559ebe6d | equal | TL_messageFwdHeader_layer96 (TLRPC.java:38897) |
| release-8.1.1_2431 | MessageFwdHeader | 0x5f777dce | equal | TL_messageFwdHeader (TLRPC.java:38669) |
| release-8.1.1_2431 | MessageFwdHeader | 0xc786ddcb | equal | TL_messageFwdHeader_layer68 (TLRPC.java:38633) |
| release-8.1.1_2431 | MessageFwdHeader | 0xec338270 | equal | TL_messageFwdHeader_layer112 (TLRPC.java:38795) |
| release-8.1.1_2431 | MessageFwdHeader | 0xfadff4ac | equal | TL_messageFwdHeader_layer72 (TLRPC.java:38855) |
| release-8.1.1_2431 | MessageReplyHeader | 0xa6d57763 | equal | TL_messageReplyHeader (TLRPC.java:22266) |
| release-8.1.1_2431 | MessageEntity | 0x020df5d0 | equal | TL_messageEntityBlockquote (TLRPC.java:29446) |
| release-8.1.1_2431 | MessageEntity | 0x208e68c9 | equal | TL_inputMessageEntityMentionName (TLRPC.java:29348) |
| release-8.1.1_2431 | MessageEntity | 0x28a20571 | equal | TL_messageEntityCode (TLRPC.java:29414) |
| release-8.1.1_2431 | MessageEntity | 0x352dca58 | equal | TL_messageEntityMentionName_layer131 (TLRPC.java:29330) |
| release-8.1.1_2431 | MessageEntity | 0x4c4e743f | equal | TL_messageEntityCashtag (TLRPC.java:29367) |
| release-8.1.1_2431 | MessageEntity | 0x64e475c2 | equal | TL_messageEntityEmail (TLRPC.java:29232) |
| release-8.1.1_2431 | MessageEntity | 0x6cef8ac7 | equal | TL_messageEntityBotCommand (TLRPC.java:29216) |
| release-8.1.1_2431 | MessageEntity | 0x6ed02538 | equal | TL_messageEntityUrl (TLRPC.java:29282) |
| release-8.1.1_2431 | MessageEntity | 0x6f635b0d | equal | TL_messageEntityHashtag (TLRPC.java:29398) |
| release-8.1.1_2431 | MessageEntity | 0x73924be0 | equal | TL_messageEntityPre (TLRPC.java:29248) |
| release-8.1.1_2431 | MessageEntity | 0x761e6af4 | equal | TL_messageEntityBankCard (TLRPC.java:29478) |
| release-8.1.1_2431 | MessageEntity | 0x76a6d327 | equal | TL_messageEntityTextUrl (TLRPC.java:29198) |
| release-8.1.1_2431 | MessageEntity | 0x826f8b60 | equal | TL_messageEntityItalic (TLRPC.java:29298) |
| release-8.1.1_2431 | MessageEntity | 0x9b69e34b | equal | TL_messageEntityPhone (TLRPC.java:29494) |
| release-8.1.1_2431 | MessageEntity | 0x9c4e7e8b | equal | TL_messageEntityUnderline (TLRPC.java:29462) |
| release-8.1.1_2431 | MessageEntity | 0xbb92ba95 | equal | TL_messageEntityUnknown (TLRPC.java:29266) |
| release-8.1.1_2431 | MessageEntity | 0xbd610bc9 | equal | TL_messageEntityBold (TLRPC.java:29382) |
| release-8.1.1_2431 | MessageEntity | 0xbf0693d4 | equal | TL_messageEntityStrike (TLRPC.java:29430) |
| release-8.1.1_2431 | MessageEntity | 0xdc7b1140 | equal | TL_messageEntityMentionName (TLRPC.java:29509) |
| release-8.1.1_2431 | MessageEntity | 0xfa04579d | equal | TL_messageEntityMention (TLRPC.java:29314) |
| release-8.1.1_2431 | MessageAction | 0x031224c3 | equal | TL_messageActionChatJoinedByLink (TLRPC.java:20573) |
| release-8.1.1_2431 | MessageAction | 0x15cefd00 | equal | TL_messageActionChatAddUser (TLRPC.java:20484) |
| release-8.1.1_2431 | MessageAction | 0x40699cd0 | equal | TL_messageActionPaymentSent (TLRPC.java:20906) |
| release-8.1.1_2431 | MessageAction | 0x4792929b | equal | TL_messageActionScreenshotTaken (TLRPC.java:20550) |
| release-8.1.1_2431 | MessageAction | 0x488a7337 | membership-only (MANUAL at TLRPC.java:20617) | TL_messageActionChatAddUser_layer131 (TLRPC.java:20603) |
| release-8.1.1_2431 | MessageAction | 0x502f92f7 | equal | TL_messageActionInviteToGroupCall (TLRPC.java:20364) |
| release-8.1.1_2431 | MessageAction | 0x51bdb021 | equal | TL_messageActionChatMigrateTo_layer131 (TLRPC.java:20513) |
| release-8.1.1_2431 | MessageAction | 0x55555550 | equal | TL_messageActionUserJoined (TLRPC.java:20686) |
| release-8.1.1_2431 | MessageAction | 0x55555551 | equal | TL_messageActionUserUpdatedPhoto (TLRPC.java:20695) |
| release-8.1.1_2431 | MessageAction | 0x55555552 | equal | TL_messageActionTTLChange (TLRPC.java:20732) |
| release-8.1.1_2431 | MessageAction | 0x55555557 | equal | TL_messageActionCreatedBroadcastList (TLRPC.java:20646) |
| release-8.1.1_2431 | MessageAction | 0x555555f5 | equal | TL_messageActionLoginUnknownLocation (TLRPC.java:20258) |
| release-8.1.1_2431 | MessageAction | 0x555555f7 | equal | TL_messageEncryptedAction (TLRPC.java:20274) |
| release-8.1.1_2431 | MessageAction | 0x5e3cfc4b | equal | TL_messageActionChatAddUser_old (TLRPC.java:20718) |
| release-8.1.1_2431 | MessageAction | 0x76b9f11a | membership-only (MANUAL at TLRPC.java:20670) | TL_messageActionInviteToGroupCall_layer131 (TLRPC.java:20655) |
| release-8.1.1_2431 | MessageAction | 0x7a0d7f42 | equal | TL_messageActionGroupCall (TLRPC.java:20411) |
| release-8.1.1_2431 | MessageAction | 0x7fcb13a8 | equal | TL_messageActionChatEditPhoto (TLRPC.java:20536) |
| release-8.1.1_2431 | MessageAction | 0x80e11a7f | equal | TL_messageActionPhoneCall (TLRPC.java:20862) |
| release-8.1.1_2431 | MessageAction | 0x92a72876 | equal | TL_messageActionGameScore (TLRPC.java:20946) |
| release-8.1.1_2431 | MessageAction | 0x94bd38ed | equal | TL_messageActionPinMessage (TLRPC.java:20823) |
| release-8.1.1_2431 | MessageAction | 0x95d2ac92 | equal | TL_messageActionChannelCreate (TLRPC.java:20775) |
| release-8.1.1_2431 | MessageAction | 0x95e3fbef | equal | TL_messageActionChatDeletePhoto (TLRPC.java:20853) |
| release-8.1.1_2431 | MessageAction | 0x98e0d697 | equal | TL_messageActionGeoProximityReached (TLRPC.java:20832) |
| release-8.1.1_2431 | MessageAction | 0x9fbab604 | equal | TL_messageActionHistoryClear (TLRPC.java:20527) |
| release-8.1.1_2431 | MessageAction | 0xa43f30cc | equal | TL_messageActionChatDeleteUser (TLRPC.java:20470) |
| release-8.1.1_2431 | MessageAction | 0xa6638b9a | membership-only (MANUAL at TLRPC.java:20348) | TL_messageActionChatCreate_layer131 (TLRPC.java:20333) |
| release-8.1.1_2431 | MessageAction | 0xaa1afbfd | equal | TL_messageActionSetMessagesTTL (TLRPC.java:20746) |
| release-8.1.1_2431 | MessageAction | 0xaa786345 | membership-only (MANUAL at TLRPC.java:25616) | TL_messageActionSetChatTheme (TLRPC.java:20433) |
| release-8.1.1_2431 | MessageAction | 0xabe9affe | equal | TL_messageActionBotAllowed (TLRPC.java:20922) |
| release-8.1.1_2431 | MessageAction | 0xb055eaee | equal | TL_messageActionChannelMigrateFrom_layer131 (TLRPC.java:20587) |
| release-8.1.1_2431 | MessageAction | 0xb2ae9b0c | equal | TL_messageActionChatDeleteUser_layer131 (TLRPC.java:20632) |
| release-8.1.1_2431 | MessageAction | 0xb3a07661 | equal | TL_messageActionGroupCallScheduled (TLRPC.java:20962) |
| release-8.1.1_2431 | MessageAction | 0xb5a1ce5a | equal | TL_messageActionChatEditTitle (TLRPC.java:20892) |
| release-8.1.1_2431 | MessageAction | 0xb6aef7b0 | equal | TL_messageActionEmpty (TLRPC.java:20937) |
| release-8.1.1_2431 | MessageAction | 0xbd47cbad | equal | TL_messageActionChatCreate (TLRPC.java:20302) |
| release-8.1.1_2431 | MessageAction | 0xd95c6154 | equal | TL_messageActionSecureValuesSent (TLRPC.java:20789) |
| release-8.1.1_2431 | MessageAction | 0xe1037f92 | equal | TL_messageActionChatMigrateTo (TLRPC.java:20559) |
| release-8.1.1_2431 | MessageAction | 0xea3948e9 | equal | TL_messageActionChannelMigrateFrom (TLRPC.java:20395) |
| release-8.1.1_2431 | MessageAction | 0xf3f25f76 | equal | TL_messageActionContactSignUp (TLRPC.java:20709) |
| release-8.1.1_2431 | MessageAction | 0xf89cf5e8 | equal | TL_messageActionChatJoinedByLink_layer131 (TLRPC.java:20761) |
| release-8.1.1_2431 | MessageAction | 0xfae69f56 | equal | TL_messageActionCustomAction (TLRPC.java:20288) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0x57adc690 | equal | TL_phoneCallDiscardReasonHangup (TLRPC.java:17464) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0x85e42301 | equal | TL_phoneCallDiscardReasonMissed (TLRPC.java:17482) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0xe095c1a0 | equal | TL_phoneCallDiscardReasonDisconnect (TLRPC.java:17491) |
| release-8.1.1_2431 | PhoneCallDiscardReason | 0xfaf7e8c9 | equal | TL_phoneCallDiscardReasonBusy (TLRPC.java:17473) |
| release-8.1.1_2431 | MessageMedia | 0x29632a36 | equal | TL_messageMediaUnsupported_old (TLRPC.java:7006) |
| release-8.1.1_2431 | MessageMedia | 0x2ec0533f | equal | TL_messageMediaVenue (TLRPC.java:7107) |
| release-8.1.1_2431 | MessageMedia | 0x2fda2204 | equal | TL_messageMediaDocument_old (TLRPC.java:7229) |
| release-8.1.1_2431 | MessageMedia | 0x3d8ce53d | equal | TL_messageMediaPhoto_layer68 (TLRPC.java:7417) |
| release-8.1.1_2431 | MessageMedia | 0x3ded6320 | equal | TL_messageMediaEmpty (TLRPC.java:7098) |
| release-8.1.1_2431 | MessageMedia | 0x3f7ee58b | equal | TL_messageMediaDice (TLRPC.java:7492) |
| release-8.1.1_2431 | MessageMedia | 0x4bd6e798 | equal | TL_messageMediaPoll (TLRPC.java:7286) |
| release-8.1.1_2431 | MessageMedia | 0x56e0d474 | equal | TL_messageMediaGeo (TLRPC.java:7523) |
| release-8.1.1_2431 | MessageMedia | 0x5bcf1675 | equal | TL_messageMediaVideo_layer45 (TLRPC.java:7433) |
| release-8.1.1_2431 | MessageMedia | 0x5e7d2f39 | equal | TL_messageMediaContact_layer81 (TLRPC.java:7397) |
| release-8.1.1_2431 | MessageMedia | 0x638fe46b | equal | TL_messageMediaDice_layer111 (TLRPC.java:7510) |
| release-8.1.1_2431 | MessageMedia | 0x695150d7 | equal | TL_messageMediaPhoto (TLRPC.java:7259) |
| release-8.1.1_2431 | MessageMedia | 0x70322949 | equal | TL_messageMediaContact (TLRPC.java:7470) |
| release-8.1.1_2431 | MessageMedia | 0x7912b71f | equal | TL_messageMediaVenue_layer71 (TLRPC.java:7131) |
| release-8.1.1_2431 | MessageMedia | 0x7c3c2609 | equal | TL_messageMediaGeoLive_layer119 (TLRPC.java:7367) |
| release-8.1.1_2431 | MessageMedia | 0x7c4414d3 | equal | TL_messageMediaDocument_layer74 (TLRPC.java:7195) |
| release-8.1.1_2431 | MessageMedia | 0x84551347 | equal | TL_messageMediaInvoice (TLRPC.java:7048) |
| release-8.1.1_2431 | MessageMedia | 0x9cb070d7 | equal | TL_messageMediaDocument (TLRPC.java:7167) |
| release-8.1.1_2431 | MessageMedia | 0x9f84f49e | equal | TL_messageMediaUnsupported (TLRPC.java:7089) |
| release-8.1.1_2431 | MessageMedia | 0xa2d24290 | equal | TL_messageMediaVideo_old (TLRPC.java:7153) |
| release-8.1.1_2431 | MessageMedia | 0xa32dd600 | equal | TL_messageMediaWebPage (TLRPC.java:7537) |
| release-8.1.1_2431 | MessageMedia | 0xb5223b0f | equal | TL_messageMediaPhoto_layer74 (TLRPC.java:7304) |
| release-8.1.1_2431 | MessageMedia | 0xb940c666 | equal | TL_messageMediaGeoLive (TLRPC.java:7337) |
| release-8.1.1_2431 | MessageMedia | 0xc6b68300 | equal | TL_messageMediaAudio_layer45 (TLRPC.java:7020) |
| release-8.1.1_2431 | MessageMedia | 0xc8c45a2a | equal | TL_messageMediaPhoto_old (TLRPC.java:7034) |
| release-8.1.1_2431 | MessageMedia | 0xcbf24940 | equal | TL_messageMediaContact_layer131 (TLRPC.java:7449) |
| release-8.1.1_2431 | MessageMedia | 0xf3e02ea8 | equal | TL_messageMediaDocument_layer68 (TLRPC.java:7243) |
| release-8.1.1_2431 | MessageMedia | 0xfdb19008 | equal | TL_messageMediaGame (TLRPC.java:7383) |
| release-8.1.1_2431 | Photo | 0x22b56751 | membership-only (MANUAL at TLRPC.java:29791) | TL_photo_old (TLRPC.java:29771) |
| release-8.1.1_2431 | Photo | 0x2331b22d | equal | TL_photoEmpty (TLRPC.java:29757) |
| release-8.1.1_2431 | Photo | 0x9288dd29 | membership-only (MANUAL at TLRPC.java:29954) | TL_photo_layer82 (TLRPC.java:29935) |
| release-8.1.1_2431 | Photo | 0x9c477dd8 | membership-only (MANUAL at TLRPC.java:29650) | TL_photo_layer97 (TLRPC.java:29630) |
| release-8.1.1_2431 | Photo | 0xc3838076 | membership-only (MANUAL at TLRPC.java:29733) | TL_photo_old2 (TLRPC.java:29714) |
| release-8.1.1_2431 | Photo | 0xcded42fe | membership-only (MANUAL at TLRPC.java:29692) | TL_photo_layer55 (TLRPC.java:29675) |
| release-8.1.1_2431 | Photo | 0xd07504a5 | membership-only (MANUAL at TLRPC.java:29836) | TL_photo_layer115 (TLRPC.java:29816) |
| release-8.1.1_2431 | Photo | 0xfb197a65 | membership-only (MANUAL at TLRPC.java:29883) | TL_photo (TLRPC.java:29863) |
| release-8.1.1_2431 | Document | 0x1e87342b | membership-only (MANUAL at TLRPC.java:23080) | TL_document (TLRPC.java:23058) |
| release-8.1.1_2431 | Document | 0x36f8c871 | equal | TL_documentEmpty (TLRPC.java:23261) |
| release-8.1.1_2431 | Document | 0x55555556 | membership-only (MANUAL at TLRPC.java:23036) | TL_documentEncrypted_old (TLRPC.java:23024) |
| release-8.1.1_2431 | Document | 0x55555558 | membership-only (MANUAL at TLRPC.java:23285) | TL_documentEncrypted (TLRPC.java:23275) |
| release-8.1.1_2431 | Document | 0x59534e4c | membership-only (MANUAL at TLRPC.java:22937) | TL_document_layer92 (TLRPC.java:22926) |
| release-8.1.1_2431 | Document | 0x87232bc7 | membership-only (MANUAL at TLRPC.java:22985) | TL_document_layer82 (TLRPC.java:22975) |
| release-8.1.1_2431 | Document | 0x9ba29cc1 | membership-only (MANUAL at TLRPC.java:23179) | TL_document_layer113 (TLRPC.java:23157) |
| release-8.1.1_2431 | Document | 0x9efc6326 | membership-only (MANUAL at TLRPC.java:23243) | TL_document_old (TLRPC.java:23231) |
| release-8.1.1_2431 | Document | 0xf9a39f4f | membership-only (MANUAL at TLRPC.java:23336) | TL_document_layer53 (TLRPC.java:23326) |
| release-8.1.1_2431 | GeoPoint | 0x0296f104 | equal | TL_geoPoint_layer119 (TLRPC.java:5029) |
| release-8.1.1_2431 | GeoPoint | 0x1117dd5f | equal | TL_geoPointEmpty (TLRPC.java:5063) |
| release-8.1.1_2431 | GeoPoint | 0x2049d70c | equal | TL_geoPoint_layer81 (TLRPC.java:5047) |
| release-8.1.1_2431 | GeoPoint | 0xb2a2f663 | equal | TL_geoPoint (TLRPC.java:5072) |
| release-8.1.1_2431 | WebPage | 0x5f07b4bc | equal | TL_webPage_layer104 (TLRPC.java:16602) |
| release-8.1.1_2431 | WebPage | 0x7311ca11 | equal | TL_webPageNotModified (TLRPC.java:16841) |
| release-8.1.1_2431 | WebPage | 0x85849473 | CONFLICT (opaque; pin conflict: flags:# [F.0: cached_page_views:i32] at TLRPC.java:16832, nothing at TLRPC.java:20418) | TL_webPageNotModified_layer110 (TLRPC.java:16832) |
| release-8.1.1_2431 | WebPage | 0xa31ea0b5 | equal | TL_webPage_old (TLRPC.java:16702) |
| release-8.1.1_2431 | WebPage | 0xc586da1c | equal | TL_webPagePending (TLRPC.java:16816) |
| release-8.1.1_2431 | WebPage | 0xca820ed7 | equal | TL_webPage_layer58 (TLRPC.java:16862) |
| release-8.1.1_2431 | WebPage | 0xd41a5167 | equal | TL_webPageUrlPending (TLRPC.java:16802) |
| release-8.1.1_2431 | WebPage | 0xe89c45b2 | compatible widening (vec<TL_webPageAttributeTheme> read as vec<WebPageAttribute>: 0x54b56617 TL_webPageAttributeTheme reads the same at TLRPC.java:9785 and at TLRPC.java:10575) | TL_webPage (TLRPC.java:16354) |
| release-8.1.1_2431 | WebPage | 0xeb1477e8 | equal | TL_webPageEmpty (TLRPC.java:16788) |
| release-8.1.1_2431 | WebPage | 0xfa64e172 | equal on the wire (names differ: attribute.documents:vec<Document> / documents:vec<Document>) | TL_webPage_layer107 (TLRPC.java:16479) |
| release-8.1.1_2431 | User | 0x075cf7a8 | equal | TL_userForeign_old2 (TLRPC.java:19315) |
| release-8.1.1_2431 | User | 0x1c60e608 | equal | TL_userSelf_old3 (TLRPC.java:19205) |
| release-8.1.1_2431 | User | 0x200250ba | equal | TL_userEmpty_layer131 (TLRPC.java:19251) |
| release-8.1.1_2431 | User | 0x22e49072 | equal | TL_user_old (TLRPC.java:19485) |
| release-8.1.1_2431 | User | 0x22e8ceb0 | equal | TL_userRequest_old (TLRPC.java:19265) |
| release-8.1.1_2431 | User | 0x2e13f4c3 | equal | TL_user_layer104 (TLRPC.java:19071) |
| release-8.1.1_2431 | User | 0x3ff6ecb0 | equal | TL_user (TLRPC.java:18809) |
| release-8.1.1_2431 | User | 0x5214c89d | equal | TL_userForeign_old (TLRPC.java:19291) |
| release-8.1.1_2431 | User | 0x7007b451 | equal | TL_userSelf_old2 (TLRPC.java:19567) |
| release-8.1.1_2431 | User | 0x720535ec | equal | TL_userSelf_old (TLRPC.java:19179) |
| release-8.1.1_2431 | User | 0x938458c1 | equal | TL_user_layer131 (TLRPC.java:18940) |
| release-8.1.1_2431 | User | 0xb29ad7cc | equal | TL_userDeleted_old (TLRPC.java:19369) |
| release-8.1.1_2431 | User | 0xcab35e18 | equal | TL_userContact_old2 (TLRPC.java:18741) |
| release-8.1.1_2431 | User | 0xd10d979a | equal | TL_user_layer65 (TLRPC.java:19387) |
| release-8.1.1_2431 | User | 0xd3bc4b7a | equal | TL_userEmpty (TLRPC.java:18795) |
| release-8.1.1_2431 | User | 0xd6016d7a | equal | TL_userDeleted_old2 (TLRPC.java:19231) |
| release-8.1.1_2431 | User | 0xd9ccc4ef | equal | TL_userRequest_old2 (TLRPC.java:19341) |
| release-8.1.1_2431 | User | 0xf2fb8319 | equal | TL_userContact_old (TLRPC.java:18769) |
| release-8.1.1_2431 | Chat | 0x07328bdb | equal | TL_chatForbidden_layer131 (TLRPC.java:36849) |
| release-8.1.1_2431 | Chat | 0x0cb44b1c | equal | TL_channel_layer72 (TLRPC.java:37308) |
| release-8.1.1_2431 | Chat | 0x17d493d5 | equal | TL_channelForbidden (TLRPC.java:36561) |
| release-8.1.1_2431 | Chat | 0x289da732 | equal | TL_channelForbidden_layer131 (TLRPC.java:36591) |
| release-8.1.1_2431 | Chat | 0x29562865 | equal | TL_chatEmpty (TLRPC.java:53658) |
| release-8.1.1_2431 | Chat | 0x2d85832c | equal | TL_channelForbidden_layer52 (TLRPC.java:36700) |
| release-8.1.1_2431 | Chat | 0x3bda1bde | equal | TL_chat_layer131 (TLRPC.java:36505) |
| release-8.1.1_2431 | Chat | 0x41cbf256 | equal | TL_chat (TLRPC.java:36449) |
| release-8.1.1_2431 | Chat | 0x450b7115 | equal | TL_channel_layer77 (TLRPC.java:37231) |
| release-8.1.1_2431 | Chat | 0x4b1b7506 | equal | TL_channel_layer48 (TLRPC.java:36645) |
| release-8.1.1_2431 | Chat | 0x4df30834 | equal | TL_channel_layer104 (TLRPC.java:37115) |
| release-8.1.1_2431 | Chat | 0x6592a1a7 | equal | TL_chatForbidden (TLRPC.java:36833) |
| release-8.1.1_2431 | Chat | 0x678e9587 | equal | TL_channel_old (TLRPC.java:36926) |
| release-8.1.1_2431 | Chat | 0x6e9c9bc7 | equal | TL_chat_old (TLRPC.java:36973) |
| release-8.1.1_2431 | Chat | 0x7312bc48 | equal | TL_chat_old2 (TLRPC.java:36416) |
| release-8.1.1_2431 | Chat | 0x8261ac61 | equal | TL_channel (TLRPC.java:36718) |
| release-8.1.1_2431 | Chat | 0x8537784f | equal | TL_channelForbidden_layer67 (TLRPC.java:36621) |
| release-8.1.1_2431 | Chat | 0x9ba2d800 | equal | TL_chatEmpty_layer131 (TLRPC.java:53674) |
| release-8.1.1_2431 | Chat | 0xa14dca52 | equal | TL_channel_layer67 (TLRPC.java:36865) |
| release-8.1.1_2431 | Chat | 0xc88974ac | equal | TL_channel_layer92 (TLRPC.java:37380) |
| release-8.1.1_2431 | Chat | 0xd31a961e | equal | TL_channel_layer131 (TLRPC.java:36998) |
| release-8.1.1_2431 | Chat | 0xd91cdd54 | equal | TL_chat_layer92 (TLRPC.java:37457) |
| release-8.1.1_2431 | Chat | 0xfb0ccc41 | equal | TL_chatForbidden_old (TLRPC.java:36398) |
| release-8.1.1_2431 | PhotoSize | 0x021e1ad6 | equal | TL_photoCachedSize (TLRPC.java:39360) |
| release-8.1.1_2431 | PhotoSize | 0x0e17e23c | equal | TL_photoSizeEmpty (TLRPC.java:39304) |
| release-8.1.1_2431 | PhotoSize | 0x5aa86a51 | equal | TL_photoSizeProgressive_layer127 (TLRPC.java:39242) |
| release-8.1.1_2431 | PhotoSize | 0x75c78e60 | equal | TL_photoSize (TLRPC.java:39340) |
| release-8.1.1_2431 | PhotoSize | 0x77bfb61b | equal | TL_photoSize_layer127 (TLRPC.java:39282) |
| release-8.1.1_2431 | PhotoSize | 0xd8214d41 | equal | TL_photoPathSize (TLRPC.java:49935) |
| release-8.1.1_2431 | PhotoSize | 0xe0b0bc2e | equal | TL_photoStrippedSize (TLRPC.java:39225) |
| release-8.1.1_2431 | PhotoSize | 0xe9a734fa | equal | TL_photoCachedSize_layer127 (TLRPC.java:39318) |
| release-8.1.1_2431 | PhotoSize | 0xfa3efb95 | equal | TL_photoSizeProgressive (TLRPC.java:39380) |
| release-8.1.1_2431 | VideoSize | 0x435bb987 | equal | TL_videoSize_layer115 (TLRPC.java:15469) |
| release-8.1.1_2431 | VideoSize | 0xde33b094 | equal | TL_videoSize (TLRPC.java:15521) |
| release-8.1.1_2431 | VideoSize | 0xe831c556 | equal | TL_videoSize_layer127 (TLRPC.java:15491) |
| release-8.1.1_2431 | DocumentAttribute | 0x051448e5 | equal | TL_documentAttributeAudio_old (TLRPC.java:1348) |
| release-8.1.1_2431 | DocumentAttribute | 0x0ef02ce6 | equal | TL_documentAttributeVideo (TLRPC.java:1411) |
| release-8.1.1_2431 | DocumentAttribute | 0x11b58939 | equal | TL_documentAttributeAnimated (TLRPC.java:1388) |
| release-8.1.1_2431 | DocumentAttribute | 0x15590068 | equal | TL_documentAttributeFilename (TLRPC.java:1397) |
| release-8.1.1_2431 | DocumentAttribute | 0x3a556302 | equal | TL_documentAttributeSticker_layer55 (TLRPC.java:1332) |
| release-8.1.1_2431 | DocumentAttribute | 0x5910cccb | equal | TL_documentAttributeVideo_layer65 (TLRPC.java:1434) |
| release-8.1.1_2431 | DocumentAttribute | 0x6319d612 | equal | TL_documentAttributeSticker (TLRPC.java:1362) |
| release-8.1.1_2431 | DocumentAttribute | 0x6c37c15c | equal | TL_documentAttributeImageSize (TLRPC.java:1505) |
| release-8.1.1_2431 | DocumentAttribute | 0x9801d2f7 | equal | TL_documentAttributeHasStickers (TLRPC.java:1482) |
| release-8.1.1_2431 | DocumentAttribute | 0x9852f9c6 | equal | TL_documentAttributeAudio (TLRPC.java:1521) |
| release-8.1.1_2431 | DocumentAttribute | 0x994c9882 | equal | TL_documentAttributeSticker_old2 (TLRPC.java:1491) |
| release-8.1.1_2431 | DocumentAttribute | 0xded218e0 | equal | TL_documentAttributeAudio_layer45 (TLRPC.java:1452) |
| release-8.1.1_2431 | DocumentAttribute | 0xfb0a5727 | equal | TL_documentAttributeSticker_old (TLRPC.java:1470) |
| release-8.1.1_2431 | FileLocation | 0x091d11eb | equal | TL_fileLocation_layer97 (TLRPC.java:39028) |
| release-8.1.1_2431 | FileLocation | 0x53d69076 | equal | TL_fileLocation_layer82 (TLRPC.java:39008) |
| release-8.1.1_2431 | FileLocation | 0x55555554 | equal | TL_fileEncryptedLocation (TLRPC.java:39066) |
| release-8.1.1_2431 | FileLocation | 0x7c596b46 | equal | TL_fileLocationUnavailable (TLRPC.java:38990) |
| release-8.1.1_2431 | FileLocation | 0xbc7fc6cd | equal | TL_fileLocationToBeDeprecated (TLRPC.java:39050) |
| release-8.1.1_2431 | InputStickerSet | 0x028703c8 | equal | TL_inputStickerSetAnimatedEmoji (TLRPC.java:25441) |
| release-8.1.1_2431 | InputStickerSet | 0x861cc8a0 | equal | TL_inputStickerSetShortName (TLRPC.java:25427) |
| release-8.1.1_2431 | InputStickerSet | 0x9de7a269 | equal | TL_inputStickerSetID (TLRPC.java:25411) |
| release-8.1.1_2431 | InputStickerSet | 0xe67f520e | equal | TL_inputStickerSetDice (TLRPC.java:25387) |
| release-8.1.1_2431 | InputStickerSet | 0xffb62b95 | equal | TL_inputStickerSetEmpty (TLRPC.java:25402) |
| release-8.1.1_2431 | TL_maskCoords | 0xaed6dbb2 | equal | TL_maskCoords (TLRPC.java:24543) |
| release-8.1.1_2431 | Video | 0x388fa391 | membership-only (MANUAL at TLRPC.java:21943) | TL_video_old2 (TLRPC.java:21930) |
| release-8.1.1_2431 | Video | 0x55555553 | membership-only (MANUAL at TLRPC.java:21870) | TL_videoEncrypted (TLRPC.java:21858) |
| release-8.1.1_2431 | Video | 0x5a04a49f | membership-only (MANUAL at TLRPC.java:21908) | TL_video_old (TLRPC.java:21896) |
| release-8.1.1_2431 | Video | 0xc10658a8 | equal | TL_videoEmpty_layer45 (TLRPC.java:21966) |
| release-8.1.1_2431 | Video | 0xee9f4a4d | membership-only (MANUAL at TLRPC.java:21805) | TL_video_old3 (TLRPC.java:21794) |
| release-8.1.1_2431 | Video | 0xf72887d3 | membership-only (MANUAL at TLRPC.java:21837) | TL_video_layer45 (TLRPC.java:21826) |
| release-8.1.1_2431 | Audio | 0x427425e7 | equal | TL_audio_old (TLRPC.java:8812) |
| release-8.1.1_2431 | Audio | 0x555555f6 | equal | TL_audioEncrypted (TLRPC.java:8838) |
| release-8.1.1_2431 | Audio | 0x586988d8 | equal | TL_audioEmpty_layer45 (TLRPC.java:8772) |
| release-8.1.1_2431 | Audio | 0xc7ac6496 | equal | TL_audio_old2 (TLRPC.java:8868) |
| release-8.1.1_2431 | Audio | 0xf9e35055 | equal | TL_audio_layer45 (TLRPC.java:8786) |
| release-8.1.1_2431 | MessageReplies | 0x4128faac | equal | TL_messageReplies_layer131 (TLRPC.java:9299) |
| release-8.1.1_2431 | MessageReplies | 0x83d60fc2 | equal | TL_messageReplies (TLRPC.java:9362) |
| release-8.1.1_2431 | MessageReactions | 0xb87a24d1 | equal | TL_messageReactions (TLRPC.java:21693) |
| release-8.1.1_2431 | RestrictionReason | 0xd072acb4 | equal | TL_restrictionReason (TLRPC.java:37496) |
| release-8.1.1_2431 | ReactionCount | 0x6fb250d1 | CONFLICT (opaque; pin conflict: flags:# reaction:str count:i32 at TLRPC.java:39751, flags:# reaction:Reaction count:i32 at TLRPC.java:45165) | TL_reactionCount (TLRPC.java:39751) |
| release-8.1.1_2431 | Poll | 0x86e18161 | equal | TL_poll (TLRPC.java:31208) |
| release-8.1.1_2431 | Poll | 0xaf746786 | equal | TL_poll_toDelete (TLRPC.java:31267) |
| release-8.1.1_2431 | Poll | 0xd5529d06 | equal | TL_poll_layer111 (TLRPC.java:31320) |
| release-8.1.1_2431 | PollResults | 0x5755785a | equal | TL_pollResults_layer108 (TLRPC.java:3799) |
| release-8.1.1_2431 | PollResults | 0xbadcc1a3 | membership-only (MANUAL at TLRPC.java:3951) | TL_pollResults_layer131 (TLRPC.java:3914) |
| release-8.1.1_2431 | PollResults | 0xc87024a2 | membership-only (MANUAL at TLRPC.java:3883) | TL_pollResults_layer111 (TLRPC.java:3846) |
| release-8.1.1_2431 | PollResults | 0xdcb82ea3 | equal on the wire (names differ: recent_voters:vec<i64> / recent_voters:vec<id64@user>) | TL_pollResults (TLRPC.java:4013) |
| release-8.1.1_2431 | ReplyMarkup | 0x3502758c | equal | TL_replyKeyboardMarkup_layer129 (TLRPC.java:9693) |
| release-8.1.1_2431 | ReplyMarkup | 0x48a30254 | compatible widening (union) (vec<TL_keyboard.KeyboardButtonRow> read as vec<TL_keyboard.KeyboardInlineButtonRow>: the union check below holds at this pin) | TL_replyInlineMarkup (TLRPC.java:9752) |
| release-8.1.1_2431 | ReplyMarkup | 0x85dd99d1 | equal | TL_replyKeyboardMarkup (TLRPC.java:9606) |
| release-8.1.1_2431 | ReplyMarkup | 0x86b40b08 | equal | TL_replyKeyboardForceReply (TLRPC.java:9669) |
| release-8.1.1_2431 | ReplyMarkup | 0xa03e5b85 | equal | TL_replyKeyboardHide (TLRPC.java:9653) |
| release-8.1.1_2431 | ReplyMarkup | 0xf4108aa0 | equal | TL_replyKeyboardForceReply_layer129 (TLRPC.java:9734) |
| release-8.1.1_2431 | InputUser | 0x1da448e2 | equal | TL_inputUserFromMessage (TLRPC.java:15120) |
| release-8.1.1_2431 | InputUser | 0xb98886cf | equal | TL_inputUserEmpty (TLRPC.java:15111) |
| release-8.1.1_2431 | InputUser | 0xf21158c6 | equal | TL_inputUser (TLRPC.java:15095) |
| release-8.1.1_2431 | InputUser | 0xf7c1b13f | equal | TL_inputUserSelf (TLRPC.java:15086) |
| release-8.1.1_2431 | TL_iv.RichText | 0x034b8621 | equal | TL_textMarked (TLRPC.java:1672) |
| release-8.1.1_2431 | TL_iv.RichText | 0x081ccf4f | equal | TL_textImage (TLRPC.java:1590) |
| release-8.1.1_2431 | TL_iv.RichText | 0x1ccb966a | equal | TL_textPhone (TLRPC.java:1557) |
| release-8.1.1_2431 | TL_iv.RichText | 0x35553762 | equal | TL_textAnchor (TLRPC.java:1639) |
| release-8.1.1_2431 | TL_iv.RichText | 0x3c2884c1 | equal | TL_textUrl (TLRPC.java:1620) |
| release-8.1.1_2431 | TL_iv.RichText | 0x6724abc4 | equal | TL_textBold (TLRPC.java:1767) |
| release-8.1.1_2431 | TL_iv.RichText | 0x6c3f19b9 | equal | TL_textFixed (TLRPC.java:1687) |
| release-8.1.1_2431 | TL_iv.RichText | 0x744694e0 | equal | TL_textPlain (TLRPC.java:1719) |
| release-8.1.1_2431 | TL_iv.RichText | 0x7e6260d7 | equal | TL_textConcat (TLRPC.java:1734) |
| release-8.1.1_2431 | TL_iv.RichText | 0x9bf8bb95 | equal | TL_textStrike (TLRPC.java:1657) |
| release-8.1.1_2431 | TL_iv.RichText | 0xc12622c4 | equal | TL_textUnderline (TLRPC.java:1797) |
| release-8.1.1_2431 | TL_iv.RichText | 0xc7fb5e01 | equal | TL_textSuperscript (TLRPC.java:1575) |
| release-8.1.1_2431 | TL_iv.RichText | 0xd912a59c | equal | TL_textItalic (TLRPC.java:1782) |
| release-8.1.1_2431 | TL_iv.RichText | 0xdc3d824f | equal | TL_textEmpty (TLRPC.java:1611) |
| release-8.1.1_2431 | TL_iv.RichText | 0xde5a0dd6 | equal | TL_textEmail (TLRPC.java:1702) |
| release-8.1.1_2431 | TL_iv.RichText | 0xed6a8504 | equal | TL_textSubscript (TLRPC.java:1812) |
| release-8.1.1_2431 | TL_iv.Page | 0x556ec7aa | membership-only (MANUAL at TLRPC.java:14782) | TL_pageFull_layer82 (TLRPC.java:14753) |
| release-8.1.1_2431 | TL_iv.Page | 0x8dee6c44 | membership-only (MANUAL at TLRPC.java:14557) | TL_pagePart_layer67 (TLRPC.java:14528) |
| release-8.1.1_2431 | TL_iv.Page | 0x8e3f9ebe | membership-only (MANUAL at TLRPC.java:14632) | TL_pagePart_layer82 (TLRPC.java:14603) |
| release-8.1.1_2431 | TL_iv.Page | 0x98657f0d | membership-only (MANUAL at TLRPC.java:14945) | TL_page (TLRPC.java:14911) |
| release-8.1.1_2431 | TL_iv.Page | 0xae891bec | membership-only (MANUAL at TLRPC.java:14861) | TL_page_layer110 (TLRPC.java:14828) |
| release-8.1.1_2431 | TL_iv.Page | 0xd7a19d69 | membership-only (MANUAL at TLRPC.java:14707) | TL_pageFull_layer67 (TLRPC.java:14678) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButtonRow | 0x77608b83 | compatible widening (union) (vec<TL_keyboard.KeyboardButton> read as vec<TL_keyboard.KeyboardInlineButton\|TL_keyboard.KeyboardButton>: the union check below holds at this pin) | TL_keyboardButtonRow (TLRPC.java:15978) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xa2fa4880 | equal | TL_keyboardButton (TLRPC.java:15408) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xb16a6c29 | equal | TL_keyboardButtonRequestPhone (TLRPC.java:15204) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xbbc7515d | equal | TL_keyboardButtonRequestPoll (TLRPC.java:15336) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xfc796b3f | equal | TL_keyboardButtonRequestGeoLocation (TLRPC.java:15268) |
| release-8.1.1_2431 | TL_chatAdminRights | 0x5fb224d5 | equal | TL_chatAdminRights (TLRPC.java:3671) |
| release-8.1.1_2431 | InputPeer | 0x179be863 | equal | TL_inputPeerChat_layer131 (TLRPC.java:41456) |
| release-8.1.1_2431 | InputPeer | 0x17bae2e6 | equal | TL_inputPeerUserFromMessage_layer131 (TLRPC.java:41577) |
| release-8.1.1_2431 | InputPeer | 0x20adaef8 | equal | TL_inputPeerChannel_layer131 (TLRPC.java:41543) |
| release-8.1.1_2431 | InputPeer | 0x27bcbbfc | equal | TL_inputPeerChannel (TLRPC.java:41470) |
| release-8.1.1_2431 | InputPeer | 0x35a95cb9 | equal | TL_inputPeerChat (TLRPC.java:41529) |
| release-8.1.1_2431 | InputPeer | 0x7b8e7de6 | equal | TL_inputPeerUser_layer131 (TLRPC.java:41486) |
| release-8.1.1_2431 | InputPeer | 0x7da07ec9 | equal | TL_inputPeerSelf (TLRPC.java:41413) |
| release-8.1.1_2431 | InputPeer | 0x7f3b18ea | equal | TL_inputPeerEmpty (TLRPC.java:41502) |
| release-8.1.1_2431 | InputPeer | 0x9c95f7bb | equal | TL_inputPeerChannelFromMessage_layer131 (TLRPC.java:41559) |
| release-8.1.1_2431 | InputPeer | 0xa87b0a1c | equal | TL_inputPeerUserFromMessage (TLRPC.java:41422) |
| release-8.1.1_2431 | InputPeer | 0xbd2a0840 | equal | TL_inputPeerChannelFromMessage (TLRPC.java:41511) |
| release-8.1.1_2431 | InputPeer | 0xdde8a54c | equal | TL_inputPeerUser (TLRPC.java:41440) |
| release-8.1.1_2431 | PollAnswer | 0x6ca9c2e9 | equal on the wire (names differ: text:str / text.text:str) | TL_pollAnswer (TLRPC.java:2331) |
| release-8.1.1_2431 | PollAnswerVoters | 0x3b6ddad2 | equal | TL_pollAnswerVoters (TLRPC.java:4279) |

Compared 288 rows; membership-only 32

Union check (Q5, ruling T2a): every button ID of this pin's single `KeyboardButton` switch, read by the A-HEAD union `TL_keyboard.KeyboardButton|TL_keyboard.KeyboardInlineButton` (the first type holding the ID): `equal` (same bytes at both pins), `opaque at A-HEAD` (a stop, not a misread) or `CONFLICT`.

| Pin | Union element | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0x0568a748 | equal (TL_keyboardButtonSwitchInline_layer157) | TL_keyboardButtonSwitchInline (TLRPC.java:15248) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0x10b78d29 | equal (TL_keyboardButtonUrlAuth_layer223) | TL_keyboardButtonUrlAuth (TLRPC.java:15282) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0x258aff05 | equal (TL_keyboardButtonUrl_layer223) | TL_keyboardButtonUrl (TLRPC.java:15232) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0x35bbdb6b | equal (TL_keyboardButtonCallback_layer223) | TL_keyboardButtonCallback (TLRPC.java:15372) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0x50f41ccf | equal (TL_keyboardButtonGame_layer223) | TL_keyboardButtonGame (TLRPC.java:15218) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0x683a5e46 | equal (TL_keyboardButtonCallback_layer117) | TL_keyboardButtonCallback_layer117 (TLRPC.java:15392) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xa2fa4880 | equal (TL_keyboardButton_layer223) | TL_keyboardButton (TLRPC.java:15408) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0xafd93fbb | equal (TL_keyboardButtonBuy_layer223) | TL_keyboardButtonBuy (TLRPC.java:15358) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xb16a6c29 | equal (TL_keyboardButtonRequestPhone_layer223) | TL_keyboardButtonRequestPhone (TLRPC.java:15204) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xbbc7515d | equal (TL_keyboardButtonRequestPoll_layer223) | TL_keyboardButtonRequestPoll (TLRPC.java:15336) |
| release-8.1.1_2431 | TL_keyboard.KeyboardInlineButton | 0xd02e7fd4 | opaque at A-HEAD (TL_inputKeyboardButtonUrlAuth_layer223): a stop, not a misread | TL_inputKeyboardButtonUrlAuth (TLRPC.java:15308) |
| release-8.1.1_2431 | TL_keyboard.KeyboardButton | 0xfc796b3f | equal (TL_keyboardButtonRequestGeoLocation_layer223) | TL_keyboardButtonRequestGeoLocation (TLRPC.java:15268) |

Union check at release-8.1.1_2431: holds, every older inline keyboard is readable by the A-HEAD reading (a compatible widening).


## release-5.14.0_1851 (7d6fa267c13bacd48f8d8f10d31a61f38b77bfe4)

Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):

| File | SHA-256 |
|---|---|
| `messenger/DialogObject.java` | 3aebae538c5f7805641e41ed7e62319d340cb0ef9611d382441a68b28b4d65e5 |
| `messenger/LocaleController.java` | fea232ba9359bbb7b2d16e68f2ddbea9ee6308b78fdca2f5c63e5983c5378e8b |
| `messenger/MessageObject.java` | 70d7c1ef27de0ea413bd9614d9eed37ff1622c1a215173a300890af20a290cdf |
| `messenger/MessagesController.java` | 47d629cac7f3a4f2c5891ed25a5d0f3c65ce163dbdcb8bd771e8b7e135cd837b |
| `messenger/MessagesStorage.java` | dae79902fb2c548b873c50238b03eeaf3666294c605454929a7a6e97952563af |
| `messenger/SecretChatHelper.java` | 222742f943bedcc0356699b7d4dae3f044ea942045da0a950fee316d23aaad1b |
| `tgnet/NativeByteBuffer.java` | 645322968184a9a76c94ebe523bd43cb68e3894dd5e91761beb0ea84bd4d4368 |
| `tgnet/SerializedData.java` | a3e041460deea68565ab6b8b6d83a2b89a27bbfe3edc397d675cdc3c76135d49 |
| `tgnet/TLObject.java` | 09ed9fd44708933d1bb72b99467a19f1e87651bbecd869fe2a37c737a2afc7d2 |
| `tgnet/TLRPC.java` | 990b58441399718bec3da1f399ece395ef9a546ebd46c888534b3b0e631d1547 |
| `ui/Cells/ChatMessageCell.java` | eb239ec8c7dadf544a93636c35ca89a3a07b0884bdf20ba69884f652ed091eb4 |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | ba9229aa405b9f9e2cb31698dfa46f667f21a7a2280e07fa64abe68f45037b39 |
| `facts.tsv` | dca5975e28eb17b1009b412ef44c14b9f0fc38864749dea18f767723136e6479 |

Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's switch, the class, the source line, the body the selection asks for, and the field order reduced from `readParams`; a `prefix` row is a proper prefix of the reduced body):

| Type | ID | Class | Verdict | Source | Note |
|---|---|---|---|---|---|
| PhoneCallDiscardReason | 0xafe2b839 | TL_phoneCallDiscardReasonAllowGroupCall | verified | TLRPC.java:12808 |  |

Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv`.

| Pin | Type | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| release-5.14.0_1851 | Message | 0x05f46804 | in-table | TL_messageForwarded_old (TLRPC.java:41577) |
| release-5.14.0_1851 | Message | 0x1c9b1027 | in-table | TL_message_layer104_2 (TLRPC.java:40907) |
| release-5.14.0_1851 | Message | 0x1d86f70e | in-table | TL_messageService_old2 (TLRPC.java:40028) |
| release-5.14.0_1851 | Message | 0x22eb6aba | in-table | TL_message_old (TLRPC.java:41616) |
| release-5.14.0_1851 | Message | 0x2bebfa86 | in-table | TL_message_old6 (TLRPC.java:41176) |
| release-5.14.0_1851 | Message | 0x44f9b43d | in-table | TL_message_layer104 (TLRPC.java:41049) |
| release-5.14.0_1851 | Message | 0x452c0e65 | in-table | TL_message (TLRPC.java:40591) |
| release-5.14.0_1851 | Message | 0x555555f8 | in-table | TL_message_secret_old (TLRPC.java:41798) |
| release-5.14.0_1851 | Message | 0x555555f9 | in-table | TL_message_secret_layer72 (TLRPC.java:41726) |
| release-5.14.0_1851 | Message | 0x555555fa | in-table | TL_message_secret (TLRPC.java:41648) |
| release-5.14.0_1851 | Message | 0x567699b3 | in-table | TL_message_old2 (TLRPC.java:41512) |
| release-5.14.0_1851 | Message | 0x5ba66c13 | in-table | TL_message_old7 (TLRPC.java:40426) |
| release-5.14.0_1851 | Message | 0x83e5de54 | in-table | TL_messageEmpty (TLRPC.java:40013) |
| release-5.14.0_1851 | Message | 0x90dddc11 | in-table | TL_message_layer72 (TLRPC.java:40061) |
| release-5.14.0_1851 | Message | 0x9789dac4 | in-table | TL_message_layer104_3 (TLRPC.java:40746) |
| release-5.14.0_1851 | Message | 0x9e19a1f6 | in-table | TL_messageService (TLRPC.java:41837) |
| release-5.14.0_1851 | Message | 0x9f8d60bb | in-table | TL_messageService_old (TLRPC.java:41550) |
| release-5.14.0_1851 | Message | 0xa367e716 | in-table | TL_messageForwarded_old2 (TLRPC.java:40547) |
| release-5.14.0_1851 | Message | 0xa7ab1991 | in-table | TL_message_old3 (TLRPC.java:41459) |
| release-5.14.0_1851 | Message | 0xc06b9607 | in-table | TL_messageService_layer48 (TLRPC.java:41352) |
| release-5.14.0_1851 | Message | 0xc09be45f | in-table | TL_message_layer68 (TLRPC.java:40178) |
| release-5.14.0_1851 | Message | 0xc3060325 | in-table | TL_message_old4 (TLRPC.java:41399) |
| release-5.14.0_1851 | Message | 0xc992e15c | in-table | TL_message_layer47 (TLRPC.java:40299) |
| release-5.14.0_1851 | Message | 0xf07814c8 | in-table | TL_message_old5 (TLRPC.java:41267) |
| release-5.14.0_1851 | Peer | 0x9db1bc6d | in-table | TL_peerUser (TLRPC.java:2820) |
| release-5.14.0_1851 | Peer | 0xbad0e5bb | in-table | TL_peerChat (TLRPC.java:2834) |
| release-5.14.0_1851 | Peer | 0xbddde532 | in-table | TL_peerChannel (TLRPC.java:2806) |
| release-5.14.0_1851 | MessageFwdHeader | 0x559ebe6d | in-table | TL_messageFwdHeader_layer96 (TLRPC.java:29972) |
| release-5.14.0_1851 | MessageFwdHeader | 0xc786ddcb | in-table | TL_messageFwdHeader_layer68 (TLRPC.java:29840) |
| release-5.14.0_1851 | MessageFwdHeader | 0xec338270 | in-table | TL_messageFwdHeader (TLRPC.java:29874) |
| release-5.14.0_1851 | MessageFwdHeader | 0xfadff4ac | in-table | TL_messageFwdHeader_layer72 (TLRPC.java:29932) |
| release-5.14.0_1851 | MessageReplyHeader | - | type not defined at this pin | - |
| release-5.14.0_1851 | MessageEntity | 0x020df5d0 | in-table | TL_messageEntityBlockquote (TLRPC.java:22674) |
| release-5.14.0_1851 | MessageEntity | 0x208e68c9 | in-table | TL_inputMessageEntityMentionName (TLRPC.java:22576) |
| release-5.14.0_1851 | MessageEntity | 0x28a20571 | in-table | TL_messageEntityCode (TLRPC.java:22642) |
| release-5.14.0_1851 | MessageEntity | 0x352dca58 | in-table | TL_messageEntityMentionName (TLRPC.java:22557) |
| release-5.14.0_1851 | MessageEntity | 0x4c4e743f | in-table | TL_messageEntityCashtag (TLRPC.java:22595) |
| release-5.14.0_1851 | MessageEntity | 0x64e475c2 | in-table | TL_messageEntityEmail (TLRPC.java:22459) |
| release-5.14.0_1851 | MessageEntity | 0x6cef8ac7 | in-table | TL_messageEntityBotCommand (TLRPC.java:22443) |
| release-5.14.0_1851 | MessageEntity | 0x6ed02538 | in-table | TL_messageEntityUrl (TLRPC.java:22509) |
| release-5.14.0_1851 | MessageEntity | 0x6f635b0d | in-table | TL_messageEntityHashtag (TLRPC.java:22626) |
| release-5.14.0_1851 | MessageEntity | 0x73924be0 | in-table | TL_messageEntityPre (TLRPC.java:22475) |
| release-5.14.0_1851 | MessageEntity | 0x76a6d327 | in-table | TL_messageEntityTextUrl (TLRPC.java:22425) |
| release-5.14.0_1851 | MessageEntity | 0x826f8b60 | in-table | TL_messageEntityItalic (TLRPC.java:22525) |
| release-5.14.0_1851 | MessageEntity | 0x9b69e34b | in-table | TL_messageEntityPhone (TLRPC.java:22706) |
| release-5.14.0_1851 | MessageEntity | 0x9c4e7e8b | in-table | TL_messageEntityUnderline (TLRPC.java:22690) |
| release-5.14.0_1851 | MessageEntity | 0xbb92ba95 | in-table | TL_messageEntityUnknown (TLRPC.java:22493) |
| release-5.14.0_1851 | MessageEntity | 0xbd610bc9 | in-table | TL_messageEntityBold (TLRPC.java:22610) |
| release-5.14.0_1851 | MessageEntity | 0xbf0693d4 | in-table | TL_messageEntityStrike (TLRPC.java:22658) |
| release-5.14.0_1851 | MessageEntity | 0xfa04579d | in-table | TL_messageEntityMention (TLRPC.java:22541) |
| release-5.14.0_1851 | MessageAction | 0x40699cd0 | in-table | TL_messageActionPaymentSent (TLRPC.java:15179) |
| release-5.14.0_1851 | MessageAction | 0x4792929b | in-table | TL_messageActionScreenshotTaken (TLRPC.java:14918) |
| release-5.14.0_1851 | MessageAction | 0x488a7337 | in-table | TL_messageActionChatAddUser (TLRPC.java:14943) |
| release-5.14.0_1851 | MessageAction | 0x51bdb021 | in-table | TL_messageActionChatMigrateTo (TLRPC.java:14881) |
| release-5.14.0_1851 | MessageAction | 0x55555550 | in-table | TL_messageActionUserJoined (TLRPC.java:14995) |
| release-5.14.0_1851 | MessageAction | 0x55555551 | in-table | TL_messageActionUserUpdatedPhoto (TLRPC.java:15004) |
| release-5.14.0_1851 | MessageAction | 0x55555552 | in-table | TL_messageActionTTLChange (TLRPC.java:15041) |
| release-5.14.0_1851 | MessageAction | 0x55555557 | in-table | TL_messageActionCreatedBroadcastList (TLRPC.java:14986) |
| release-5.14.0_1851 | MessageAction | 0x555555f5 | in-table | TL_messageActionLoginUnknownLocation (TLRPC.java:14784) |
| release-5.14.0_1851 | MessageAction | 0x555555f7 | in-table | TL_messageEncryptedAction (TLRPC.java:14800) |
| release-5.14.0_1851 | MessageAction | 0x5e3cfc4b | in-table | TL_messageActionChatAddUser_old (TLRPC.java:15027) |
| release-5.14.0_1851 | MessageAction | 0x7a0d7f42 | in-table | TL_messageActionGroupCall (TLRPC.java:14859) |
| release-5.14.0_1851 | MessageAction | 0x7fcb13a8 | in-table | TL_messageActionChatEditPhoto (TLRPC.java:14904) |
| release-5.14.0_1851 | MessageAction | 0x80e11a7f | in-table | TL_messageActionPhoneCall (TLRPC.java:15135) |
| release-5.14.0_1851 | MessageAction | 0x92a72876 | in-table | TL_messageActionGameScore (TLRPC.java:15219) |
| release-5.14.0_1851 | MessageAction | 0x94bd38ed | in-table | TL_messageActionPinMessage (TLRPC.java:15117) |
| release-5.14.0_1851 | MessageAction | 0x95d2ac92 | in-table | TL_messageActionChannelCreate (TLRPC.java:15069) |
| release-5.14.0_1851 | MessageAction | 0x95e3fbef | in-table | TL_messageActionChatDeletePhoto (TLRPC.java:15126) |
| release-5.14.0_1851 | MessageAction | 0x9fbab604 | in-table | TL_messageActionHistoryClear (TLRPC.java:14895) |
| release-5.14.0_1851 | MessageAction | 0xa6638b9a | in-table | TL_messageActionChatCreate (TLRPC.java:14828) |
| release-5.14.0_1851 | MessageAction | 0xabe9affe | in-table | TL_messageActionBotAllowed (TLRPC.java:15195) |
| release-5.14.0_1851 | MessageAction | 0xb055eaee | in-table | TL_messageActionChannelMigrateFrom (TLRPC.java:14927) |
| release-5.14.0_1851 | MessageAction | 0xb2ae9b0c | in-table | TL_messageActionChatDeleteUser (TLRPC.java:14972) |
| release-5.14.0_1851 | MessageAction | 0xb5a1ce5a | in-table | TL_messageActionChatEditTitle (TLRPC.java:15165) |
| release-5.14.0_1851 | MessageAction | 0xb6aef7b0 | in-table | TL_messageActionEmpty (TLRPC.java:15210) |
| release-5.14.0_1851 | MessageAction | 0xd95c6154 | in-table | TL_messageActionSecureValuesSent (TLRPC.java:15083) |
| release-5.14.0_1851 | MessageAction | 0xf3f25f76 | in-table | TL_messageActionContactSignUp (TLRPC.java:15018) |
| release-5.14.0_1851 | MessageAction | 0xf89cf5e8 | in-table | TL_messageActionChatJoinedByLink (TLRPC.java:15055) |
| release-5.14.0_1851 | MessageAction | 0xfae69f56 | in-table | TL_messageActionCustomAction (TLRPC.java:14814) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0x57adc690 | in-table | TL_phoneCallDiscardReasonHangup (TLRPC.java:12790) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0x85e42301 | in-table | TL_phoneCallDiscardReasonMissed (TLRPC.java:12822) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0xafe2b839 | in-table | TL_phoneCallDiscardReasonAllowGroupCall (TLRPC.java:12808) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0xe095c1a0 | in-table | TL_phoneCallDiscardReasonDisconnect (TLRPC.java:12831) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0xfaf7e8c9 | in-table | TL_phoneCallDiscardReasonBusy (TLRPC.java:12799) |
| release-5.14.0_1851 | MessageMedia | 0x29632a36 | in-table | TL_messageMediaUnsupported_old (TLRPC.java:5565) |
| release-5.14.0_1851 | MessageMedia | 0x2ec0533f | in-table | TL_messageMediaVenue (TLRPC.java:5666) |
| release-5.14.0_1851 | MessageMedia | 0x2fda2204 | in-table | TL_messageMediaDocument_old (TLRPC.java:5788) |
| release-5.14.0_1851 | MessageMedia | 0x3d8ce53d | in-table | TL_messageMediaPhoto_layer68 (TLRPC.java:5946) |
| release-5.14.0_1851 | MessageMedia | 0x3ded6320 | in-table | TL_messageMediaEmpty (TLRPC.java:5657) |
| release-5.14.0_1851 | MessageMedia | 0x4bd6e798 | in-table | TL_messageMediaPoll (TLRPC.java:5845) |
| release-5.14.0_1851 | MessageMedia | 0x56e0d474 | in-table | TL_messageMediaGeo (TLRPC.java:5999) |
| release-5.14.0_1851 | MessageMedia | 0x5bcf1675 | in-table | TL_messageMediaVideo_layer45 (TLRPC.java:5962) |
| release-5.14.0_1851 | MessageMedia | 0x5e7d2f39 | in-table | TL_messageMediaContact_layer81 (TLRPC.java:5926) |
| release-5.14.0_1851 | MessageMedia | 0x695150d7 | in-table | TL_messageMediaPhoto (TLRPC.java:5818) |
| release-5.14.0_1851 | MessageMedia | 0x7912b71f | in-table | TL_messageMediaVenue_layer71 (TLRPC.java:5690) |
| release-5.14.0_1851 | MessageMedia | 0x7c3c2609 | in-table | TL_messageMediaGeoLive (TLRPC.java:5896) |
| release-5.14.0_1851 | MessageMedia | 0x7c4414d3 | in-table | TL_messageMediaDocument_layer74 (TLRPC.java:5754) |
| release-5.14.0_1851 | MessageMedia | 0x84551347 | in-table | TL_messageMediaInvoice (TLRPC.java:5607) |
| release-5.14.0_1851 | MessageMedia | 0x9cb070d7 | in-table | TL_messageMediaDocument (TLRPC.java:5726) |
| release-5.14.0_1851 | MessageMedia | 0x9f84f49e | in-table | TL_messageMediaUnsupported (TLRPC.java:5648) |
| release-5.14.0_1851 | MessageMedia | 0xa2d24290 | in-table | TL_messageMediaVideo_old (TLRPC.java:5712) |
| release-5.14.0_1851 | MessageMedia | 0xa32dd600 | in-table | TL_messageMediaWebPage (TLRPC.java:6013) |
| release-5.14.0_1851 | MessageMedia | 0xb5223b0f | in-table | TL_messageMediaPhoto_layer74 (TLRPC.java:5863) |
| release-5.14.0_1851 | MessageMedia | 0xc6b68300 | in-table | TL_messageMediaAudio_layer45 (TLRPC.java:5579) |
| release-5.14.0_1851 | MessageMedia | 0xc8c45a2a | in-table | TL_messageMediaPhoto_old (TLRPC.java:5593) |
| release-5.14.0_1851 | MessageMedia | 0xcbf24940 | in-table | TL_messageMediaContact (TLRPC.java:5978) |
| release-5.14.0_1851 | MessageMedia | 0xf3e02ea8 | in-table | TL_messageMediaDocument_layer68 (TLRPC.java:5802) |
| release-5.14.0_1851 | MessageMedia | 0xfdb19008 | in-table | TL_messageMediaGame (TLRPC.java:5912) |
| release-5.14.0_1851 | Photo | 0x22b56751 | in-table | TL_photo_old (TLRPC.java:22960) |
| release-5.14.0_1851 | Photo | 0x2331b22d | in-table | TL_photoEmpty (TLRPC.java:22946) |
| release-5.14.0_1851 | Photo | 0x9288dd29 | in-table | TL_photo_layer82 (TLRPC.java:23052) |
| release-5.14.0_1851 | Photo | 0x9c477dd8 | in-table | TL_photo_layer97 (TLRPC.java:22819) |
| release-5.14.0_1851 | Photo | 0xc3838076 | in-table | TL_photo_old2 (TLRPC.java:22903) |
| release-5.14.0_1851 | Photo | 0xcded42fe | in-table | TL_photo_layer55 (TLRPC.java:22864) |
| release-5.14.0_1851 | Photo | 0xd07504a5 | in-table | TL_photo (TLRPC.java:23005) |
| release-5.14.0_1851 | Document | 0x36f8c871 | in-table | TL_documentEmpty (TLRPC.java:17211) |
| release-5.14.0_1851 | Document | 0x55555556 | in-table | TL_documentEncrypted_old (TLRPC.java:17073) |
| release-5.14.0_1851 | Document | 0x55555558 | in-table | TL_documentEncrypted (TLRPC.java:17225) |
| release-5.14.0_1851 | Document | 0x59534e4c | in-table | TL_document_layer92 (TLRPC.java:16975) |
| release-5.14.0_1851 | Document | 0x87232bc7 | in-table | TL_document_layer82 (TLRPC.java:17024) |
| release-5.14.0_1851 | Document | 0x9ba29cc1 | in-table | TL_document (TLRPC.java:17107) |
| release-5.14.0_1851 | Document | 0x9efc6326 | in-table | TL_document_old (TLRPC.java:17181) |
| release-5.14.0_1851 | Document | 0xf9a39f4f | in-table | TL_document_layer53 (TLRPC.java:17276) |
| release-5.14.0_1851 | GeoPoint | 0x0296f104 | in-table | TL_geoPoint (TLRPC.java:3912) |
| release-5.14.0_1851 | GeoPoint | 0x1117dd5f | in-table | TL_geoPointEmpty (TLRPC.java:3946) |
| release-5.14.0_1851 | GeoPoint | 0x2049d70c | in-table | TL_geoPoint_layer81 (TLRPC.java:3930) |
| release-5.14.0_1851 | WebPage | 0x5f07b4bc | in-table | TL_webPage_layer104 (TLRPC.java:11861) |
| release-5.14.0_1851 | WebPage | 0x85849473 | in-table | TL_webPageNotModified (TLRPC.java:12091) |
| release-5.14.0_1851 | WebPage | 0xa31ea0b5 | in-table | TL_webPage_old (TLRPC.java:11961) |
| release-5.14.0_1851 | WebPage | 0xc586da1c | in-table | TL_webPagePending (TLRPC.java:12075) |
| release-5.14.0_1851 | WebPage | 0xca820ed7 | in-table | TL_webPage_layer58 (TLRPC.java:12100) |
| release-5.14.0_1851 | WebPage | 0xd41a5167 | in-table | TL_webPageUrlPending (TLRPC.java:12061) |
| release-5.14.0_1851 | WebPage | 0xe89c45b2 | in-table | TL_webPage (TLRPC.java:11613) |
| release-5.14.0_1851 | WebPage | 0xeb1477e8 | in-table | TL_webPageEmpty (TLRPC.java:12047) |
| release-5.14.0_1851 | WebPage | 0xfa64e172 | in-table | TL_webPage_layer107 (TLRPC.java:11738) |
| release-5.14.0_1851 | User | 0x075cf7a8 | in-table | TL_userForeign_old2 (TLRPC.java:14024) |
| release-5.14.0_1851 | User | 0x1c60e608 | in-table | TL_userSelf_old3 (TLRPC.java:13914) |
| release-5.14.0_1851 | User | 0x200250ba | in-table | TL_userEmpty (TLRPC.java:13960) |
| release-5.14.0_1851 | User | 0x22e49072 | in-table | TL_user_old (TLRPC.java:14194) |
| release-5.14.0_1851 | User | 0x22e8ceb0 | in-table | TL_userRequest_old (TLRPC.java:13974) |
| release-5.14.0_1851 | User | 0x2e13f4c3 | in-table | TL_user_layer104 (TLRPC.java:13780) |
| release-5.14.0_1851 | User | 0x5214c89d | in-table | TL_userForeign_old (TLRPC.java:14000) |
| release-5.14.0_1851 | User | 0x7007b451 | in-table | TL_userSelf_old2 (TLRPC.java:14276) |
| release-5.14.0_1851 | User | 0x720535ec | in-table | TL_userSelf_old (TLRPC.java:13888) |
| release-5.14.0_1851 | User | 0x938458c1 | in-table | TL_user (TLRPC.java:13653) |
| release-5.14.0_1851 | User | 0xb29ad7cc | in-table | TL_userDeleted_old (TLRPC.java:14078) |
| release-5.14.0_1851 | User | 0xcab35e18 | in-table | TL_userContact_old2 (TLRPC.java:13599) |
| release-5.14.0_1851 | User | 0xd10d979a | in-table | TL_user_layer65 (TLRPC.java:14096) |
| release-5.14.0_1851 | User | 0xd6016d7a | in-table | TL_userDeleted_old2 (TLRPC.java:13940) |
| release-5.14.0_1851 | User | 0xd9ccc4ef | in-table | TL_userRequest_old2 (TLRPC.java:14050) |
| release-5.14.0_1851 | User | 0xf2fb8319 | in-table | TL_userContact_old (TLRPC.java:13627) |
| release-5.14.0_1851 | Chat | 0x07328bdb | in-table | TL_chatForbidden (TLRPC.java:28345) |
| release-5.14.0_1851 | Chat | 0x0cb44b1c | in-table | TL_channel_layer72 (TLRPC.java:28795) |
| release-5.14.0_1851 | Chat | 0x289da732 | in-table | TL_channelForbidden (TLRPC.java:28218) |
| release-5.14.0_1851 | Chat | 0x2d85832c | in-table | TL_channelForbidden_layer52 (TLRPC.java:28327) |
| release-5.14.0_1851 | Chat | 0x3bda1bde | in-table | TL_chat (TLRPC.java:28166) |
| release-5.14.0_1851 | Chat | 0x450b7115 | in-table | TL_channel_layer77 (TLRPC.java:28718) |
| release-5.14.0_1851 | Chat | 0x4b1b7506 | in-table | TL_channel_layer48 (TLRPC.java:28272) |
| release-5.14.0_1851 | Chat | 0x4df30834 | in-table | TL_channel_layer104 (TLRPC.java:28603) |
| release-5.14.0_1851 | Chat | 0x678e9587 | in-table | TL_channel_old (TLRPC.java:28422) |
| release-5.14.0_1851 | Chat | 0x6e9c9bc7 | in-table | TL_chat_old (TLRPC.java:28469) |
| release-5.14.0_1851 | Chat | 0x7312bc48 | in-table | TL_chat_old2 (TLRPC.java:28133) |
| release-5.14.0_1851 | Chat | 0x8537784f | in-table | TL_channelForbidden_layer67 (TLRPC.java:28248) |
| release-5.14.0_1851 | Chat | 0x9ba2d800 | in-table | TL_chatEmpty (TLRPC.java:42021) |
| release-5.14.0_1851 | Chat | 0xa14dca52 | in-table | TL_channel_layer67 (TLRPC.java:28361) |
| release-5.14.0_1851 | Chat | 0xc88974ac | in-table | TL_channel_layer92 (TLRPC.java:28867) |
| release-5.14.0_1851 | Chat | 0xd31a961e | in-table | TL_channel (TLRPC.java:28494) |
| release-5.14.0_1851 | Chat | 0xd91cdd54 | in-table | TL_chat_layer92 (TLRPC.java:28944) |
| release-5.14.0_1851 | Chat | 0xfb0ccc41 | in-table | TL_chatForbidden_old (TLRPC.java:28115) |
| release-5.14.0_1851 | PhotoSize | 0x0e17e23c | in-table | TL_photoSizeEmpty (TLRPC.java:30304) |
| release-5.14.0_1851 | PhotoSize | 0x77bfb61b | in-table | TL_photoSize (TLRPC.java:30282) |
| release-5.14.0_1851 | PhotoSize | 0xe0b0bc2e | in-table | TL_photoStrippedSize (TLRPC.java:30265) |
| release-5.14.0_1851 | PhotoSize | 0xe9a734fa | in-table | TL_photoCachedSize (TLRPC.java:30318) |
| release-5.14.0_1851 | VideoSize | - | type not defined at this pin | - |
| release-5.14.0_1851 | DocumentAttribute | 0x051448e5 | in-table | TL_documentAttributeAudio_old (TLRPC.java:951) |
| release-5.14.0_1851 | DocumentAttribute | 0x0ef02ce6 | in-table | TL_documentAttributeVideo (TLRPC.java:1014) |
| release-5.14.0_1851 | DocumentAttribute | 0x11b58939 | in-table | TL_documentAttributeAnimated (TLRPC.java:991) |
| release-5.14.0_1851 | DocumentAttribute | 0x15590068 | in-table | TL_documentAttributeFilename (TLRPC.java:1000) |
| release-5.14.0_1851 | DocumentAttribute | 0x3a556302 | in-table | TL_documentAttributeSticker_layer55 (TLRPC.java:935) |
| release-5.14.0_1851 | DocumentAttribute | 0x5910cccb | in-table | TL_documentAttributeVideo_layer65 (TLRPC.java:1037) |
| release-5.14.0_1851 | DocumentAttribute | 0x6319d612 | in-table | TL_documentAttributeSticker (TLRPC.java:965) |
| release-5.14.0_1851 | DocumentAttribute | 0x6c37c15c | in-table | TL_documentAttributeImageSize (TLRPC.java:1108) |
| release-5.14.0_1851 | DocumentAttribute | 0x9801d2f7 | in-table | TL_documentAttributeHasStickers (TLRPC.java:1085) |
| release-5.14.0_1851 | DocumentAttribute | 0x9852f9c6 | in-table | TL_documentAttributeAudio (TLRPC.java:1124) |
| release-5.14.0_1851 | DocumentAttribute | 0x994c9882 | in-table | TL_documentAttributeSticker_old2 (TLRPC.java:1094) |
| release-5.14.0_1851 | DocumentAttribute | 0xded218e0 | in-table | TL_documentAttributeAudio_layer45 (TLRPC.java:1055) |
| release-5.14.0_1851 | DocumentAttribute | 0xfb0a5727 | in-table | TL_documentAttributeSticker_old (TLRPC.java:1073) |
| release-5.14.0_1851 | FileLocation | 0x091d11eb | in-table | TL_fileLocation_layer97 (TLRPC.java:30101) |
| release-5.14.0_1851 | FileLocation | 0x53d69076 | in-table | TL_fileLocation_layer82 (TLRPC.java:30081) |
| release-5.14.0_1851 | FileLocation | 0x55555554 | in-table | TL_fileEncryptedLocation (TLRPC.java:30139) |
| release-5.14.0_1851 | FileLocation | 0x7c596b46 | in-table | TL_fileLocationUnavailable (TLRPC.java:30063) |
| release-5.14.0_1851 | FileLocation | 0xbc7fc6cd | in-table | TL_fileLocationToBeDeprecated (TLRPC.java:30123) |
| release-5.14.0_1851 | InputStickerSet | 0x028703c8 | in-table | TL_inputStickerSetAnimatedEmoji (TLRPC.java:19317) |
| release-5.14.0_1851 | InputStickerSet | 0x861cc8a0 | in-table | TL_inputStickerSetShortName (TLRPC.java:19303) |
| release-5.14.0_1851 | InputStickerSet | 0x9de7a269 | in-table | TL_inputStickerSetID (TLRPC.java:19287) |
| release-5.14.0_1851 | InputStickerSet | 0xffb62b95 | in-table | TL_inputStickerSetEmpty (TLRPC.java:19278) |
| release-5.14.0_1851 | TL_maskCoords | 0xaed6dbb2 | in-table | TL_maskCoords (TLRPC.java:18493) |
| release-5.14.0_1851 | Video | 0x388fa391 | in-table | TL_video_old2 (TLRPC.java:16179) |
| release-5.14.0_1851 | Video | 0x55555553 | in-table | TL_videoEncrypted (TLRPC.java:16107) |
| release-5.14.0_1851 | Video | 0x5a04a49f | in-table | TL_video_old (TLRPC.java:16145) |
| release-5.14.0_1851 | Video | 0xc10658a8 | in-table | TL_videoEmpty_layer45 (TLRPC.java:16215) |
| release-5.14.0_1851 | Video | 0xee9f4a4d | in-table | TL_video_old3 (TLRPC.java:16043) |
| release-5.14.0_1851 | Video | 0xf72887d3 | in-table | TL_video_layer45 (TLRPC.java:16075) |
| release-5.14.0_1851 | Audio | 0x427425e7 | in-table | TL_audio_old (TLRPC.java:7164) |
| release-5.14.0_1851 | Audio | 0x555555f6 | in-table | TL_audioEncrypted (TLRPC.java:7190) |
| release-5.14.0_1851 | Audio | 0x586988d8 | in-table | TL_audioEmpty_layer45 (TLRPC.java:7124) |
| release-5.14.0_1851 | Audio | 0xc7ac6496 | in-table | TL_audio_old2 (TLRPC.java:7220) |
| release-5.14.0_1851 | Audio | 0xf9e35055 | in-table | TL_audio_layer45 (TLRPC.java:7138) |
| release-5.14.0_1851 | MessageReplies | - | type not defined at this pin | - |
| release-5.14.0_1851 | MessageReactions | 0xb87a24d1 | in-table | TL_messageReactions (TLRPC.java:15942) |
| release-5.14.0_1851 | Reaction | - | type not defined at this pin | - |
| release-5.14.0_1851 | MessagePeerReaction | - | type not defined at this pin | - |
| release-5.14.0_1851 | MessageReactor | - | type not defined at this pin | - |
| release-5.14.0_1851 | RestrictionReason | 0xd072acb4 | in-table | TL_restrictionReason (TLRPC.java:28983) |
| release-5.14.0_1851 | TL_factCheck | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_textWithEntities | - | type not defined at this pin | - |
| release-5.14.0_1851 | SuggestedPost | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_stars.StarsAmount | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_messages_emojiGameOutcome | - | type not defined at this pin | - |
| release-5.14.0_1851 | ReactionCount | 0x6fb250d1 | in-table | TL_reactionCount (TLRPC.java:30615) |
| release-5.14.0_1851 | WebPageAttribute | - | type not defined at this pin | - |
| release-5.14.0_1851 | Poll | 0xd5529d06 | in-table | TL_poll (TLRPC.java:24034) |
| release-5.14.0_1851 | PollResults | 0x5755785a | in-table | TL_pollResults_layer108 (TLRPC.java:3093) |
| release-5.14.0_1851 | PollResults | 0xc87024a2 | in-table | TL_pollResults (TLRPC.java:3140) |
| release-5.14.0_1851 | ReplyMarkup | 0x3502758c | in-table | TL_replyKeyboardMarkup (TLRPC.java:7586) |
| release-5.14.0_1851 | ReplyMarkup | 0x48a30254 | in-table | TL_replyInlineMarkup (TLRPC.java:7519) |
| release-5.14.0_1851 | ReplyMarkup | 0xa03e5b85 | in-table | TL_replyKeyboardHide (TLRPC.java:7552) |
| release-5.14.0_1851 | ReplyMarkup | 0xf4108aa0 | in-table | TL_replyKeyboardForceReply (TLRPC.java:7568) |
| release-5.14.0_1851 | InputUser | 0x2d117597 | in-table | TL_inputUserFromMessage (TLRPC.java:10743) |
| release-5.14.0_1851 | InputUser | 0xb98886cf | in-table | TL_inputUserEmpty (TLRPC.java:10734) |
| release-5.14.0_1851 | InputUser | 0xd8292816 | in-table | TL_inputUser (TLRPC.java:10718) |
| release-5.14.0_1851 | InputUser | 0xf7c1b13f | in-table | TL_inputUserSelf (TLRPC.java:10709) |
| release-5.14.0_1851 | TL_iv.RichText | 0x034b8621 | in-table | TL_textMarked (TLRPC.java:1275) |
| release-5.14.0_1851 | TL_iv.RichText | 0x081ccf4f | in-table | TL_textImage (TLRPC.java:1193) |
| release-5.14.0_1851 | TL_iv.RichText | 0x1ccb966a | in-table | TL_textPhone (TLRPC.java:1160) |
| release-5.14.0_1851 | TL_iv.RichText | 0x35553762 | in-table | TL_textAnchor (TLRPC.java:1242) |
| release-5.14.0_1851 | TL_iv.RichText | 0x3c2884c1 | in-table | TL_textUrl (TLRPC.java:1223) |
| release-5.14.0_1851 | TL_iv.RichText | 0x6724abc4 | in-table | TL_textBold (TLRPC.java:1370) |
| release-5.14.0_1851 | TL_iv.RichText | 0x6c3f19b9 | in-table | TL_textFixed (TLRPC.java:1290) |
| release-5.14.0_1851 | TL_iv.RichText | 0x744694e0 | in-table | TL_textPlain (TLRPC.java:1322) |
| release-5.14.0_1851 | TL_iv.RichText | 0x7e6260d7 | in-table | TL_textConcat (TLRPC.java:1337) |
| release-5.14.0_1851 | TL_iv.RichText | 0x9bf8bb95 | in-table | TL_textStrike (TLRPC.java:1260) |
| release-5.14.0_1851 | TL_iv.RichText | 0xc12622c4 | in-table | TL_textUnderline (TLRPC.java:1400) |
| release-5.14.0_1851 | TL_iv.RichText | 0xc7fb5e01 | in-table | TL_textSuperscript (TLRPC.java:1178) |
| release-5.14.0_1851 | TL_iv.RichText | 0xd912a59c | in-table | TL_textItalic (TLRPC.java:1385) |
| release-5.14.0_1851 | TL_iv.RichText | 0xdc3d824f | in-table | TL_textEmpty (TLRPC.java:1214) |
| release-5.14.0_1851 | TL_iv.RichText | 0xde5a0dd6 | in-table | TL_textEmail (TLRPC.java:1305) |
| release-5.14.0_1851 | TL_iv.RichText | 0xed6a8504 | in-table | TL_textSubscript (TLRPC.java:1415) |
| release-5.14.0_1851 | TL_iv.Page | 0x556ec7aa | in-table | TL_pageFull_layer82 (TLRPC.java:10467) |
| release-5.14.0_1851 | TL_iv.Page | 0x8dee6c44 | in-table | TL_pagePart_layer67 (TLRPC.java:10242) |
| release-5.14.0_1851 | TL_iv.Page | 0x8e3f9ebe | in-table | TL_pagePart_layer82 (TLRPC.java:10317) |
| release-5.14.0_1851 | TL_iv.Page | 0xae891bec | in-table | TL_page (TLRPC.java:10542) |
| release-5.14.0_1851 | TL_iv.Page | 0xd7a19d69 | in-table | TL_pageFull_layer67 (TLRPC.java:10392) |
| release-5.14.0_1851 | TL_iv.RichMessage | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_keyboard.KeyboardButtonRow | 0x77608b83 | in-table | TL_keyboardButtonRow (TLRPC.java:11358) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButtonRow | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0x0568a748 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonSwitchInline (TLRPC.java:10867) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0x10b78d29 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonUrlAuth (TLRPC.java:10901) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0x258aff05 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonUrl (TLRPC.java:10851) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0x50f41ccf | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonGame (TLRPC.java:10837) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0x683a5e46 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonCallback (TLRPC.java:10991) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xa2fa4880 | in-table | TL_keyboardButton (TLRPC.java:11007) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xafd93fbb | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_keyboardButtonBuy (TLRPC.java:10977) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xb16a6c29 | in-table | TL_keyboardButtonRequestPhone (TLRPC.java:10823) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xbbc7515d | in-table | TL_keyboardButtonRequestPoll (TLRPC.java:10955) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xd02e7fd4 | in-table through the union (A-HEAD reads it as TL_keyboard.KeyboardInlineButton; every field of type TL_keyboard.KeyboardButton is a union with it, ruling T2a) | TL_inputKeyboardButtonUrlAuth (TLRPC.java:10927) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xfc796b3f | in-table | TL_keyboardButtonRequestGeoLocation (TLRPC.java:10887) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_keyboard.KeyboardButtonStyle | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_keyboard.ButtonType | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_keyboard.InlineButtonType | - | type not defined at this pin | - |
| release-5.14.0_1851 | RequestPeerType | - | type not defined at this pin | - |
| release-5.14.0_1851 | TL_chatAdminRights | 0x5fb224d5 | in-table | TL_chatAdminRights (TLRPC.java:3013) |
| release-5.14.0_1851 | InputPeer | 0x179be863 | in-table | TL_inputPeerChat (TLRPC.java:32069) |
| release-5.14.0_1851 | InputPeer | 0x17bae2e6 | in-table | TL_inputPeerUserFromMessage (TLRPC.java:32083) |
| release-5.14.0_1851 | InputPeer | 0x20adaef8 | in-table | TL_inputPeerChannel (TLRPC.java:32123) |
| release-5.14.0_1851 | InputPeer | 0x7b8e7de6 | in-table | TL_inputPeerUser (TLRPC.java:32053) |
| release-5.14.0_1851 | InputPeer | 0x7da07ec9 | in-table | TL_inputPeerSelf (TLRPC.java:32044) |
| release-5.14.0_1851 | InputPeer | 0x7f3b18ea | in-table | TL_inputPeerEmpty (TLRPC.java:32139) |
| release-5.14.0_1851 | InputPeer | 0x9c95f7bb | in-table | TL_inputPeerChannelFromMessage (TLRPC.java:32103) |
| release-5.14.0_1851 | InlineQueryPeerType | - | type not defined at this pin | - |
| release-5.14.0_1851 | PollAnswer | 0x6ca9c2e9 | in-table | TL_pollAnswer (TLRPC.java:1938) |
| release-5.14.0_1851 | PollAnswerVoters | 0x3b6ddad2 | in-table | TL_pollAnswerVoters (TLRPC.java:3303) |

Body comparison with A-HEAD: every row of `constructors.tsv` whose ID the switch of its type reads at this pin, what its body reads at this pin against its A-HEAD reading (field order, flag bits and wire types; flag-only trues, `mType.` holders and field names set aside, object types named by the deserializer a call reaches, ruling T1a). `equal on the wire` lists the field names that differ (informational); `membership-only` names the line the reduction cannot read; `compatible widening (union)` an older row of the button union that the union check below holds for (Q5, ruling T2a).

| Pin | Type | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| release-5.14.0_1851 | Message | 0x05f46804 | equal on the wire (names differ: fwd_from.from_id:i32 / fwd_from.from_id:id32@user; from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_messageForwarded_old (TLRPC.java:41577) |
| release-5.14.0_1851 | Message | 0x1c9b1027 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_layer104_2 (TLRPC.java:40907) |
| release-5.14.0_1851 | Message | 0x1d86f70e | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_messageService_old2 (TLRPC.java:40028) |
| release-5.14.0_1851 | Message | 0x22eb6aba | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_message_old (TLRPC.java:41616) |
| release-5.14.0_1851 | Message | 0x2bebfa86 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; fwd_from.from_id:i32 / fwd_from.from_id:id32@user; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_old6 (TLRPC.java:41176) |
| release-5.14.0_1851 | Message | 0x44f9b43d | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_layer104 (TLRPC.java:41049) |
| release-5.14.0_1851 | Message | 0x452c0e65 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message (TLRPC.java:40591) |
| release-5.14.0_1851 | Message | 0x555555f8 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_message_secret_old (TLRPC.java:41798) |
| release-5.14.0_1851 | Message | 0x555555f9 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_random_id:i64 / reply_to.reply_to_random_id:i64) | TL_message_secret_layer72 (TLRPC.java:41726) |
| release-5.14.0_1851 | Message | 0x555555fa | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_random_id:i64 / reply_to.reply_to_random_id:i64) | TL_message_secret (TLRPC.java:41648) |
| release-5.14.0_1851 | Message | 0x567699b3 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_message_old2 (TLRPC.java:41512) |
| release-5.14.0_1851 | Message | 0x5ba66c13 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; peer:Peer / fwd_from.from_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_old7 (TLRPC.java:40426) |
| release-5.14.0_1851 | Message | 0x83e5de54 | equal | TL_messageEmpty (TLRPC.java:40013) |
| release-5.14.0_1851 | Message | 0x90dddc11 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_layer72 (TLRPC.java:40061) |
| release-5.14.0_1851 | Message | 0x9789dac4 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_layer104_3 (TLRPC.java:40746) |
| release-5.14.0_1851 | Message | 0x9e19a1f6 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_messageService (TLRPC.java:41837) |
| release-5.14.0_1851 | Message | 0x9f8d60bb | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_messageService_old (TLRPC.java:41550) |
| release-5.14.0_1851 | Message | 0xa367e716 | equal on the wire (names differ: fwd_from.from_id:i32 / fwd_from.from_id:id32@user; from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_messageForwarded_old2 (TLRPC.java:40547) |
| release-5.14.0_1851 | Message | 0xa7ab1991 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; fwd_from.from_id:i32 / fwd_from.from_id:id32@user; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_old3 (TLRPC.java:41459) |
| release-5.14.0_1851 | Message | 0xc06b9607 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer) | TL_messageService_layer48 (TLRPC.java:41352) |
| release-5.14.0_1851 | Message | 0xc09be45f | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_layer68 (TLRPC.java:40178) |
| release-5.14.0_1851 | Message | 0xc3060325 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; fwd_from.from_id:i32 / fwd_from.from_id:id32@user; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_old4 (TLRPC.java:41399) |
| release-5.14.0_1851 | Message | 0xc992e15c | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; peer:Peer / fwd_from.from_id:Peer; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_layer47 (TLRPC.java:40299) |
| release-5.14.0_1851 | Message | 0xf07814c8 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; to_id:Peer / peer_id:Peer; fwd_from.from_id:i32 / fwd_from.from_id:id32@user; reply_to_msg_id:i32 / reply_to.reply_to_msg_id:i32) | TL_message_old5 (TLRPC.java:41267) |
| release-5.14.0_1851 | Peer | 0x9db1bc6d | equal | TL_peerUser (TLRPC.java:2820) |
| release-5.14.0_1851 | Peer | 0xbad0e5bb | equal | TL_peerChat (TLRPC.java:2834) |
| release-5.14.0_1851 | Peer | 0xbddde532 | equal | TL_peerChannel (TLRPC.java:2806) |
| release-5.14.0_1851 | MessageFwdHeader | 0x559ebe6d | equal on the wire (names differ: from_id:i32 / from_id:id32@user; channel_id:i32 / from_id:id32@channel) | TL_messageFwdHeader_layer96 (TLRPC.java:29972) |
| release-5.14.0_1851 | MessageFwdHeader | 0xc786ddcb | equal on the wire (names differ: from_id:i32 / from_id:id32@?; channel_id:i32 / from_id:id32@channel) | TL_messageFwdHeader_layer68 (TLRPC.java:29840) |
| release-5.14.0_1851 | MessageFwdHeader | 0xec338270 | equal on the wire (names differ: from_id:i32 / from_id:id32@user; channel_id:i32 / from_id:id32@channel) | TL_messageFwdHeader (TLRPC.java:29874) |
| release-5.14.0_1851 | MessageFwdHeader | 0xfadff4ac | equal on the wire (names differ: from_id:i32 / from_id:id32@user; channel_id:i32 / from_id:id32@channel) | TL_messageFwdHeader_layer72 (TLRPC.java:29932) |
| release-5.14.0_1851 | MessageEntity | 0x020df5d0 | equal | TL_messageEntityBlockquote (TLRPC.java:22674) |
| release-5.14.0_1851 | MessageEntity | 0x208e68c9 | equal | TL_inputMessageEntityMentionName (TLRPC.java:22576) |
| release-5.14.0_1851 | MessageEntity | 0x28a20571 | equal | TL_messageEntityCode (TLRPC.java:22642) |
| release-5.14.0_1851 | MessageEntity | 0x352dca58 | equal | TL_messageEntityMentionName (TLRPC.java:22557) |
| release-5.14.0_1851 | MessageEntity | 0x4c4e743f | equal | TL_messageEntityCashtag (TLRPC.java:22595) |
| release-5.14.0_1851 | MessageEntity | 0x64e475c2 | equal | TL_messageEntityEmail (TLRPC.java:22459) |
| release-5.14.0_1851 | MessageEntity | 0x6cef8ac7 | equal | TL_messageEntityBotCommand (TLRPC.java:22443) |
| release-5.14.0_1851 | MessageEntity | 0x6ed02538 | equal | TL_messageEntityUrl (TLRPC.java:22509) |
| release-5.14.0_1851 | MessageEntity | 0x6f635b0d | equal | TL_messageEntityHashtag (TLRPC.java:22626) |
| release-5.14.0_1851 | MessageEntity | 0x73924be0 | equal | TL_messageEntityPre (TLRPC.java:22475) |
| release-5.14.0_1851 | MessageEntity | 0x76a6d327 | equal | TL_messageEntityTextUrl (TLRPC.java:22425) |
| release-5.14.0_1851 | MessageEntity | 0x826f8b60 | equal | TL_messageEntityItalic (TLRPC.java:22525) |
| release-5.14.0_1851 | MessageEntity | 0x9b69e34b | equal | TL_messageEntityPhone (TLRPC.java:22706) |
| release-5.14.0_1851 | MessageEntity | 0x9c4e7e8b | equal | TL_messageEntityUnderline (TLRPC.java:22690) |
| release-5.14.0_1851 | MessageEntity | 0xbb92ba95 | equal | TL_messageEntityUnknown (TLRPC.java:22493) |
| release-5.14.0_1851 | MessageEntity | 0xbd610bc9 | equal | TL_messageEntityBold (TLRPC.java:22610) |
| release-5.14.0_1851 | MessageEntity | 0xbf0693d4 | equal | TL_messageEntityStrike (TLRPC.java:22658) |
| release-5.14.0_1851 | MessageEntity | 0xfa04579d | equal | TL_messageEntityMention (TLRPC.java:22541) |
| release-5.14.0_1851 | MessageAction | 0x40699cd0 | equal | TL_messageActionPaymentSent (TLRPC.java:15179) |
| release-5.14.0_1851 | MessageAction | 0x4792929b | equal | TL_messageActionScreenshotTaken (TLRPC.java:14918) |
| release-5.14.0_1851 | MessageAction | 0x488a7337 | membership-only (MANUAL at TLRPC.java:25909) | TL_messageActionChatAddUser (TLRPC.java:14943) |
| release-5.14.0_1851 | MessageAction | 0x51bdb021 | equal | TL_messageActionChatMigrateTo (TLRPC.java:14881) |
| release-5.14.0_1851 | MessageAction | 0x55555550 | equal | TL_messageActionUserJoined (TLRPC.java:14995) |
| release-5.14.0_1851 | MessageAction | 0x55555551 | equal | TL_messageActionUserUpdatedPhoto (TLRPC.java:15004) |
| release-5.14.0_1851 | MessageAction | 0x55555552 | equal | TL_messageActionTTLChange (TLRPC.java:15041) |
| release-5.14.0_1851 | MessageAction | 0x55555557 | equal | TL_messageActionCreatedBroadcastList (TLRPC.java:14986) |
| release-5.14.0_1851 | MessageAction | 0x555555f5 | equal | TL_messageActionLoginUnknownLocation (TLRPC.java:14784) |
| release-5.14.0_1851 | MessageAction | 0x555555f7 | equal | TL_messageEncryptedAction (TLRPC.java:14800) |
| release-5.14.0_1851 | MessageAction | 0x5e3cfc4b | equal | TL_messageActionChatAddUser_old (TLRPC.java:15027) |
| release-5.14.0_1851 | MessageAction | 0x7a0d7f42 | equal | TL_messageActionGroupCall (TLRPC.java:14859) |
| release-5.14.0_1851 | MessageAction | 0x7fcb13a8 | equal | TL_messageActionChatEditPhoto (TLRPC.java:14904) |
| release-5.14.0_1851 | MessageAction | 0x80e11a7f | equal | TL_messageActionPhoneCall (TLRPC.java:15135) |
| release-5.14.0_1851 | MessageAction | 0x92a72876 | equal | TL_messageActionGameScore (TLRPC.java:15219) |
| release-5.14.0_1851 | MessageAction | 0x94bd38ed | equal | TL_messageActionPinMessage (TLRPC.java:15117) |
| release-5.14.0_1851 | MessageAction | 0x95d2ac92 | equal | TL_messageActionChannelCreate (TLRPC.java:15069) |
| release-5.14.0_1851 | MessageAction | 0x95e3fbef | equal | TL_messageActionChatDeletePhoto (TLRPC.java:15126) |
| release-5.14.0_1851 | MessageAction | 0x9fbab604 | equal | TL_messageActionHistoryClear (TLRPC.java:14895) |
| release-5.14.0_1851 | MessageAction | 0xa6638b9a | membership-only (MANUAL at TLRPC.java:25551) | TL_messageActionChatCreate (TLRPC.java:14828) |
| release-5.14.0_1851 | MessageAction | 0xabe9affe | equal | TL_messageActionBotAllowed (TLRPC.java:15195) |
| release-5.14.0_1851 | MessageAction | 0xb055eaee | equal | TL_messageActionChannelMigrateFrom (TLRPC.java:14927) |
| release-5.14.0_1851 | MessageAction | 0xb2ae9b0c | equal | TL_messageActionChatDeleteUser (TLRPC.java:14972) |
| release-5.14.0_1851 | MessageAction | 0xb5a1ce5a | equal | TL_messageActionChatEditTitle (TLRPC.java:15165) |
| release-5.14.0_1851 | MessageAction | 0xb6aef7b0 | equal | TL_messageActionEmpty (TLRPC.java:15210) |
| release-5.14.0_1851 | MessageAction | 0xd95c6154 | equal | TL_messageActionSecureValuesSent (TLRPC.java:15083) |
| release-5.14.0_1851 | MessageAction | 0xf3f25f76 | equal | TL_messageActionContactSignUp (TLRPC.java:15018) |
| release-5.14.0_1851 | MessageAction | 0xf89cf5e8 | equal | TL_messageActionChatJoinedByLink (TLRPC.java:15055) |
| release-5.14.0_1851 | MessageAction | 0xfae69f56 | equal | TL_messageActionCustomAction (TLRPC.java:14814) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0x57adc690 | equal | TL_phoneCallDiscardReasonHangup (TLRPC.java:12790) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0x85e42301 | equal | TL_phoneCallDiscardReasonMissed (TLRPC.java:12822) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0xafe2b839 | equal | TL_phoneCallDiscardReasonAllowGroupCall (TLRPC.java:12808) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0xe095c1a0 | equal | TL_phoneCallDiscardReasonDisconnect (TLRPC.java:12831) |
| release-5.14.0_1851 | PhoneCallDiscardReason | 0xfaf7e8c9 | equal | TL_phoneCallDiscardReasonBusy (TLRPC.java:12799) |
| release-5.14.0_1851 | MessageMedia | 0x29632a36 | equal | TL_messageMediaUnsupported_old (TLRPC.java:5565) |
| release-5.14.0_1851 | MessageMedia | 0x2ec0533f | equal | TL_messageMediaVenue (TLRPC.java:5666) |
| release-5.14.0_1851 | MessageMedia | 0x2fda2204 | equal | TL_messageMediaDocument_old (TLRPC.java:5788) |
| release-5.14.0_1851 | MessageMedia | 0x3d8ce53d | equal | TL_messageMediaPhoto_layer68 (TLRPC.java:5946) |
| release-5.14.0_1851 | MessageMedia | 0x3ded6320 | equal | TL_messageMediaEmpty (TLRPC.java:5657) |
| release-5.14.0_1851 | MessageMedia | 0x4bd6e798 | equal | TL_messageMediaPoll (TLRPC.java:5845) |
| release-5.14.0_1851 | MessageMedia | 0x56e0d474 | equal | TL_messageMediaGeo (TLRPC.java:5999) |
| release-5.14.0_1851 | MessageMedia | 0x5bcf1675 | equal | TL_messageMediaVideo_layer45 (TLRPC.java:5962) |
| release-5.14.0_1851 | MessageMedia | 0x5e7d2f39 | equal | TL_messageMediaContact_layer81 (TLRPC.java:5926) |
| release-5.14.0_1851 | MessageMedia | 0x695150d7 | equal | TL_messageMediaPhoto (TLRPC.java:5818) |
| release-5.14.0_1851 | MessageMedia | 0x7912b71f | equal | TL_messageMediaVenue_layer71 (TLRPC.java:5690) |
| release-5.14.0_1851 | MessageMedia | 0x7c3c2609 | equal | TL_messageMediaGeoLive (TLRPC.java:5896) |
| release-5.14.0_1851 | MessageMedia | 0x7c4414d3 | equal | TL_messageMediaDocument_layer74 (TLRPC.java:5754) |
| release-5.14.0_1851 | MessageMedia | 0x84551347 | equal | TL_messageMediaInvoice (TLRPC.java:5607) |
| release-5.14.0_1851 | MessageMedia | 0x9cb070d7 | equal | TL_messageMediaDocument (TLRPC.java:5726) |
| release-5.14.0_1851 | MessageMedia | 0x9f84f49e | equal | TL_messageMediaUnsupported (TLRPC.java:5648) |
| release-5.14.0_1851 | MessageMedia | 0xa2d24290 | equal | TL_messageMediaVideo_old (TLRPC.java:5712) |
| release-5.14.0_1851 | MessageMedia | 0xa32dd600 | equal | TL_messageMediaWebPage (TLRPC.java:6013) |
| release-5.14.0_1851 | MessageMedia | 0xb5223b0f | equal | TL_messageMediaPhoto_layer74 (TLRPC.java:5863) |
| release-5.14.0_1851 | MessageMedia | 0xc6b68300 | equal | TL_messageMediaAudio_layer45 (TLRPC.java:5579) |
| release-5.14.0_1851 | MessageMedia | 0xc8c45a2a | equal | TL_messageMediaPhoto_old (TLRPC.java:5593) |
| release-5.14.0_1851 | MessageMedia | 0xcbf24940 | equal | TL_messageMediaContact (TLRPC.java:5978) |
| release-5.14.0_1851 | MessageMedia | 0xf3e02ea8 | equal | TL_messageMediaDocument_layer68 (TLRPC.java:5802) |
| release-5.14.0_1851 | MessageMedia | 0xfdb19008 | equal | TL_messageMediaGame (TLRPC.java:5912) |
| release-5.14.0_1851 | Photo | 0x22b56751 | equal | TL_photo_old (TLRPC.java:22960) |
| release-5.14.0_1851 | Photo | 0x2331b22d | equal | TL_photoEmpty (TLRPC.java:22946) |
| release-5.14.0_1851 | Photo | 0x9288dd29 | equal | TL_photo_layer82 (TLRPC.java:23052) |
| release-5.14.0_1851 | Photo | 0x9c477dd8 | equal | TL_photo_layer97 (TLRPC.java:22819) |
| release-5.14.0_1851 | Photo | 0xc3838076 | equal | TL_photo_old2 (TLRPC.java:22903) |
| release-5.14.0_1851 | Photo | 0xcded42fe | equal | TL_photo_layer55 (TLRPC.java:22864) |
| release-5.14.0_1851 | Photo | 0xd07504a5 | equal | TL_photo (TLRPC.java:23005) |
| release-5.14.0_1851 | Document | 0x36f8c871 | equal | TL_documentEmpty (TLRPC.java:17211) |
| release-5.14.0_1851 | Document | 0x55555556 | equal | TL_documentEncrypted_old (TLRPC.java:17073) |
| release-5.14.0_1851 | Document | 0x55555558 | equal | TL_documentEncrypted (TLRPC.java:17225) |
| release-5.14.0_1851 | Document | 0x59534e4c | equal | TL_document_layer92 (TLRPC.java:16975) |
| release-5.14.0_1851 | Document | 0x87232bc7 | equal | TL_document_layer82 (TLRPC.java:17024) |
| release-5.14.0_1851 | Document | 0x9ba29cc1 | equal | TL_document (TLRPC.java:17107) |
| release-5.14.0_1851 | Document | 0x9efc6326 | equal | TL_document_old (TLRPC.java:17181) |
| release-5.14.0_1851 | Document | 0xf9a39f4f | equal | TL_document_layer53 (TLRPC.java:17276) |
| release-5.14.0_1851 | GeoPoint | 0x0296f104 | equal | TL_geoPoint (TLRPC.java:3912) |
| release-5.14.0_1851 | GeoPoint | 0x1117dd5f | equal | TL_geoPointEmpty (TLRPC.java:3946) |
| release-5.14.0_1851 | GeoPoint | 0x2049d70c | equal | TL_geoPoint_layer81 (TLRPC.java:3930) |
| release-5.14.0_1851 | WebPage | 0x5f07b4bc | equal | TL_webPage_layer104 (TLRPC.java:11861) |
| release-5.14.0_1851 | WebPage | 0x85849473 | equal | TL_webPageNotModified (TLRPC.java:12091) |
| release-5.14.0_1851 | WebPage | 0xa31ea0b5 | equal | TL_webPage_old (TLRPC.java:11961) |
| release-5.14.0_1851 | WebPage | 0xc586da1c | equal | TL_webPagePending (TLRPC.java:12075) |
| release-5.14.0_1851 | WebPage | 0xca820ed7 | equal | TL_webPage_layer58 (TLRPC.java:12100) |
| release-5.14.0_1851 | WebPage | 0xd41a5167 | equal | TL_webPageUrlPending (TLRPC.java:12061) |
| release-5.14.0_1851 | WebPage | 0xe89c45b2 | compatible widening (vec<TL_webPageAttributeTheme> read as vec<WebPageAttribute>: 0x54b56617 TL_webPageAttributeTheme reads the same at TLRPC.java:7627 and at TLRPC.java:10575) | TL_webPage (TLRPC.java:11613) |
| release-5.14.0_1851 | WebPage | 0xeb1477e8 | equal | TL_webPageEmpty (TLRPC.java:12047) |
| release-5.14.0_1851 | WebPage | 0xfa64e172 | equal on the wire (names differ: attribute.documents:vec<Document> / documents:vec<Document>) | TL_webPage_layer107 (TLRPC.java:11738) |
| release-5.14.0_1851 | User | 0x075cf7a8 | equal | TL_userForeign_old2 (TLRPC.java:14024) |
| release-5.14.0_1851 | User | 0x1c60e608 | equal | TL_userSelf_old3 (TLRPC.java:13914) |
| release-5.14.0_1851 | User | 0x200250ba | equal | TL_userEmpty (TLRPC.java:13960) |
| release-5.14.0_1851 | User | 0x22e49072 | equal | TL_user_old (TLRPC.java:14194) |
| release-5.14.0_1851 | User | 0x22e8ceb0 | equal | TL_userRequest_old (TLRPC.java:13974) |
| release-5.14.0_1851 | User | 0x2e13f4c3 | equal | TL_user_layer104 (TLRPC.java:13780) |
| release-5.14.0_1851 | User | 0x5214c89d | equal | TL_userForeign_old (TLRPC.java:14000) |
| release-5.14.0_1851 | User | 0x7007b451 | equal | TL_userSelf_old2 (TLRPC.java:14276) |
| release-5.14.0_1851 | User | 0x720535ec | equal | TL_userSelf_old (TLRPC.java:13888) |
| release-5.14.0_1851 | User | 0x938458c1 | equal | TL_user (TLRPC.java:13653) |
| release-5.14.0_1851 | User | 0xb29ad7cc | equal | TL_userDeleted_old (TLRPC.java:14078) |
| release-5.14.0_1851 | User | 0xcab35e18 | equal | TL_userContact_old2 (TLRPC.java:13599) |
| release-5.14.0_1851 | User | 0xd10d979a | equal | TL_user_layer65 (TLRPC.java:14096) |
| release-5.14.0_1851 | User | 0xd6016d7a | equal | TL_userDeleted_old2 (TLRPC.java:13940) |
| release-5.14.0_1851 | User | 0xd9ccc4ef | equal | TL_userRequest_old2 (TLRPC.java:14050) |
| release-5.14.0_1851 | User | 0xf2fb8319 | equal | TL_userContact_old (TLRPC.java:13627) |
| release-5.14.0_1851 | Chat | 0x07328bdb | equal | TL_chatForbidden (TLRPC.java:28345) |
| release-5.14.0_1851 | Chat | 0x0cb44b1c | equal | TL_channel_layer72 (TLRPC.java:28795) |
| release-5.14.0_1851 | Chat | 0x289da732 | equal | TL_channelForbidden (TLRPC.java:28218) |
| release-5.14.0_1851 | Chat | 0x2d85832c | equal | TL_channelForbidden_layer52 (TLRPC.java:28327) |
| release-5.14.0_1851 | Chat | 0x3bda1bde | equal | TL_chat (TLRPC.java:28166) |
| release-5.14.0_1851 | Chat | 0x450b7115 | equal | TL_channel_layer77 (TLRPC.java:28718) |
| release-5.14.0_1851 | Chat | 0x4b1b7506 | equal | TL_channel_layer48 (TLRPC.java:28272) |
| release-5.14.0_1851 | Chat | 0x4df30834 | equal | TL_channel_layer104 (TLRPC.java:28603) |
| release-5.14.0_1851 | Chat | 0x678e9587 | equal | TL_channel_old (TLRPC.java:28422) |
| release-5.14.0_1851 | Chat | 0x6e9c9bc7 | equal | TL_chat_old (TLRPC.java:28469) |
| release-5.14.0_1851 | Chat | 0x7312bc48 | equal | TL_chat_old2 (TLRPC.java:28133) |
| release-5.14.0_1851 | Chat | 0x8537784f | equal | TL_channelForbidden_layer67 (TLRPC.java:28248) |
| release-5.14.0_1851 | Chat | 0x9ba2d800 | equal | TL_chatEmpty (TLRPC.java:42021) |
| release-5.14.0_1851 | Chat | 0xa14dca52 | equal | TL_channel_layer67 (TLRPC.java:28361) |
| release-5.14.0_1851 | Chat | 0xc88974ac | equal | TL_channel_layer92 (TLRPC.java:28867) |
| release-5.14.0_1851 | Chat | 0xd31a961e | equal | TL_channel (TLRPC.java:28494) |
| release-5.14.0_1851 | Chat | 0xd91cdd54 | equal | TL_chat_layer92 (TLRPC.java:28944) |
| release-5.14.0_1851 | Chat | 0xfb0ccc41 | equal | TL_chatForbidden_old (TLRPC.java:28115) |
| release-5.14.0_1851 | PhotoSize | 0x0e17e23c | equal | TL_photoSizeEmpty (TLRPC.java:30304) |
| release-5.14.0_1851 | PhotoSize | 0x77bfb61b | equal | TL_photoSize (TLRPC.java:30282) |
| release-5.14.0_1851 | PhotoSize | 0xe0b0bc2e | equal | TL_photoStrippedSize (TLRPC.java:30265) |
| release-5.14.0_1851 | PhotoSize | 0xe9a734fa | equal | TL_photoCachedSize (TLRPC.java:30318) |
| release-5.14.0_1851 | DocumentAttribute | 0x051448e5 | equal | TL_documentAttributeAudio_old (TLRPC.java:951) |
| release-5.14.0_1851 | DocumentAttribute | 0x0ef02ce6 | equal | TL_documentAttributeVideo (TLRPC.java:1014) |
| release-5.14.0_1851 | DocumentAttribute | 0x11b58939 | equal | TL_documentAttributeAnimated (TLRPC.java:991) |
| release-5.14.0_1851 | DocumentAttribute | 0x15590068 | equal | TL_documentAttributeFilename (TLRPC.java:1000) |
| release-5.14.0_1851 | DocumentAttribute | 0x3a556302 | equal | TL_documentAttributeSticker_layer55 (TLRPC.java:935) |
| release-5.14.0_1851 | DocumentAttribute | 0x5910cccb | equal | TL_documentAttributeVideo_layer65 (TLRPC.java:1037) |
| release-5.14.0_1851 | DocumentAttribute | 0x6319d612 | equal | TL_documentAttributeSticker (TLRPC.java:965) |
| release-5.14.0_1851 | DocumentAttribute | 0x6c37c15c | equal | TL_documentAttributeImageSize (TLRPC.java:1108) |
| release-5.14.0_1851 | DocumentAttribute | 0x9801d2f7 | equal | TL_documentAttributeHasStickers (TLRPC.java:1085) |
| release-5.14.0_1851 | DocumentAttribute | 0x9852f9c6 | equal | TL_documentAttributeAudio (TLRPC.java:1124) |
| release-5.14.0_1851 | DocumentAttribute | 0x994c9882 | equal | TL_documentAttributeSticker_old2 (TLRPC.java:1094) |
| release-5.14.0_1851 | DocumentAttribute | 0xded218e0 | equal | TL_documentAttributeAudio_layer45 (TLRPC.java:1055) |
| release-5.14.0_1851 | DocumentAttribute | 0xfb0a5727 | equal | TL_documentAttributeSticker_old (TLRPC.java:1073) |
| release-5.14.0_1851 | FileLocation | 0x091d11eb | equal | TL_fileLocation_layer97 (TLRPC.java:30101) |
| release-5.14.0_1851 | FileLocation | 0x53d69076 | equal | TL_fileLocation_layer82 (TLRPC.java:30081) |
| release-5.14.0_1851 | FileLocation | 0x55555554 | equal | TL_fileEncryptedLocation (TLRPC.java:30139) |
| release-5.14.0_1851 | FileLocation | 0x7c596b46 | equal | TL_fileLocationUnavailable (TLRPC.java:30063) |
| release-5.14.0_1851 | FileLocation | 0xbc7fc6cd | equal | TL_fileLocationToBeDeprecated (TLRPC.java:30123) |
| release-5.14.0_1851 | InputStickerSet | 0x028703c8 | equal | TL_inputStickerSetAnimatedEmoji (TLRPC.java:19317) |
| release-5.14.0_1851 | InputStickerSet | 0x861cc8a0 | equal | TL_inputStickerSetShortName (TLRPC.java:19303) |
| release-5.14.0_1851 | InputStickerSet | 0x9de7a269 | equal | TL_inputStickerSetID (TLRPC.java:19287) |
| release-5.14.0_1851 | InputStickerSet | 0xffb62b95 | equal | TL_inputStickerSetEmpty (TLRPC.java:19278) |
| release-5.14.0_1851 | TL_maskCoords | 0xaed6dbb2 | equal | TL_maskCoords (TLRPC.java:18493) |
| release-5.14.0_1851 | Video | 0x388fa391 | equal | TL_video_old2 (TLRPC.java:16179) |
| release-5.14.0_1851 | Video | 0x55555553 | equal | TL_videoEncrypted (TLRPC.java:16107) |
| release-5.14.0_1851 | Video | 0x5a04a49f | equal | TL_video_old (TLRPC.java:16145) |
| release-5.14.0_1851 | Video | 0xc10658a8 | equal | TL_videoEmpty_layer45 (TLRPC.java:16215) |
| release-5.14.0_1851 | Video | 0xee9f4a4d | equal | TL_video_old3 (TLRPC.java:16043) |
| release-5.14.0_1851 | Video | 0xf72887d3 | equal | TL_video_layer45 (TLRPC.java:16075) |
| release-5.14.0_1851 | Audio | 0x427425e7 | equal | TL_audio_old (TLRPC.java:7164) |
| release-5.14.0_1851 | Audio | 0x555555f6 | equal | TL_audioEncrypted (TLRPC.java:7190) |
| release-5.14.0_1851 | Audio | 0x586988d8 | equal | TL_audioEmpty_layer45 (TLRPC.java:7124) |
| release-5.14.0_1851 | Audio | 0xc7ac6496 | equal | TL_audio_old2 (TLRPC.java:7220) |
| release-5.14.0_1851 | Audio | 0xf9e35055 | equal | TL_audio_layer45 (TLRPC.java:7138) |
| release-5.14.0_1851 | MessageReactions | 0xb87a24d1 | equal | TL_messageReactions (TLRPC.java:15942) |
| release-5.14.0_1851 | RestrictionReason | 0xd072acb4 | equal | TL_restrictionReason (TLRPC.java:28983) |
| release-5.14.0_1851 | ReactionCount | 0x6fb250d1 | CONFLICT (opaque; pin conflict: flags:# reaction:str count:i32 at TLRPC.java:30615, flags:# reaction:Reaction count:i32 at TLRPC.java:45165) | TL_reactionCount (TLRPC.java:30615) |
| release-5.14.0_1851 | Poll | 0xd5529d06 | equal | TL_poll (TLRPC.java:24034) |
| release-5.14.0_1851 | PollResults | 0x5755785a | equal | TL_pollResults_layer108 (TLRPC.java:3093) |
| release-5.14.0_1851 | PollResults | 0xc87024a2 | equal on the wire (names differ: recent_voters:vec<i32> / recent_voters:vec<id32@user>) | TL_pollResults (TLRPC.java:3140) |
| release-5.14.0_1851 | ReplyMarkup | 0x3502758c | equal | TL_replyKeyboardMarkup (TLRPC.java:7586) |
| release-5.14.0_1851 | ReplyMarkup | 0x48a30254 | compatible widening (union) (vec<TL_keyboard.KeyboardButtonRow> read as vec<TL_keyboard.KeyboardInlineButtonRow>: the union check below holds at this pin) | TL_replyInlineMarkup (TLRPC.java:7519) |
| release-5.14.0_1851 | ReplyMarkup | 0xa03e5b85 | equal | TL_replyKeyboardHide (TLRPC.java:7552) |
| release-5.14.0_1851 | ReplyMarkup | 0xf4108aa0 | equal | TL_replyKeyboardForceReply (TLRPC.java:7568) |
| release-5.14.0_1851 | InputUser | 0x2d117597 | equal | TL_inputUserFromMessage (TLRPC.java:10743) |
| release-5.14.0_1851 | InputUser | 0xb98886cf | equal | TL_inputUserEmpty (TLRPC.java:10734) |
| release-5.14.0_1851 | InputUser | 0xd8292816 | equal | TL_inputUser (TLRPC.java:10718) |
| release-5.14.0_1851 | InputUser | 0xf7c1b13f | equal | TL_inputUserSelf (TLRPC.java:10709) |
| release-5.14.0_1851 | TL_iv.RichText | 0x034b8621 | equal | TL_textMarked (TLRPC.java:1275) |
| release-5.14.0_1851 | TL_iv.RichText | 0x081ccf4f | equal | TL_textImage (TLRPC.java:1193) |
| release-5.14.0_1851 | TL_iv.RichText | 0x1ccb966a | equal | TL_textPhone (TLRPC.java:1160) |
| release-5.14.0_1851 | TL_iv.RichText | 0x35553762 | equal | TL_textAnchor (TLRPC.java:1242) |
| release-5.14.0_1851 | TL_iv.RichText | 0x3c2884c1 | equal | TL_textUrl (TLRPC.java:1223) |
| release-5.14.0_1851 | TL_iv.RichText | 0x6724abc4 | equal | TL_textBold (TLRPC.java:1370) |
| release-5.14.0_1851 | TL_iv.RichText | 0x6c3f19b9 | equal | TL_textFixed (TLRPC.java:1290) |
| release-5.14.0_1851 | TL_iv.RichText | 0x744694e0 | equal | TL_textPlain (TLRPC.java:1322) |
| release-5.14.0_1851 | TL_iv.RichText | 0x7e6260d7 | equal | TL_textConcat (TLRPC.java:1337) |
| release-5.14.0_1851 | TL_iv.RichText | 0x9bf8bb95 | equal | TL_textStrike (TLRPC.java:1260) |
| release-5.14.0_1851 | TL_iv.RichText | 0xc12622c4 | equal | TL_textUnderline (TLRPC.java:1400) |
| release-5.14.0_1851 | TL_iv.RichText | 0xc7fb5e01 | equal | TL_textSuperscript (TLRPC.java:1178) |
| release-5.14.0_1851 | TL_iv.RichText | 0xd912a59c | equal | TL_textItalic (TLRPC.java:1385) |
| release-5.14.0_1851 | TL_iv.RichText | 0xdc3d824f | equal | TL_textEmpty (TLRPC.java:1214) |
| release-5.14.0_1851 | TL_iv.RichText | 0xde5a0dd6 | equal | TL_textEmail (TLRPC.java:1305) |
| release-5.14.0_1851 | TL_iv.RichText | 0xed6a8504 | equal | TL_textSubscript (TLRPC.java:1415) |
| release-5.14.0_1851 | TL_iv.Page | 0x556ec7aa | membership-only (MANUAL at TLRPC.java:10496) | TL_pageFull_layer82 (TLRPC.java:10467) |
| release-5.14.0_1851 | TL_iv.Page | 0x8dee6c44 | membership-only (MANUAL at TLRPC.java:10271) | TL_pagePart_layer67 (TLRPC.java:10242) |
| release-5.14.0_1851 | TL_iv.Page | 0x8e3f9ebe | membership-only (MANUAL at TLRPC.java:10346) | TL_pagePart_layer82 (TLRPC.java:10317) |
| release-5.14.0_1851 | TL_iv.Page | 0xae891bec | membership-only (MANUAL at TLRPC.java:10575) | TL_page (TLRPC.java:10542) |
| release-5.14.0_1851 | TL_iv.Page | 0xd7a19d69 | membership-only (MANUAL at TLRPC.java:10421) | TL_pageFull_layer67 (TLRPC.java:10392) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButtonRow | 0x77608b83 | compatible widening (union) (vec<TL_keyboard.KeyboardButton> read as vec<TL_keyboard.KeyboardInlineButton\|TL_keyboard.KeyboardButton>: the union check below holds at this pin) | TL_keyboardButtonRow (TLRPC.java:11358) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xa2fa4880 | equal | TL_keyboardButton (TLRPC.java:11007) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xb16a6c29 | equal | TL_keyboardButtonRequestPhone (TLRPC.java:10823) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xbbc7515d | equal | TL_keyboardButtonRequestPoll (TLRPC.java:10955) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xfc796b3f | equal | TL_keyboardButtonRequestGeoLocation (TLRPC.java:10887) |
| release-5.14.0_1851 | TL_chatAdminRights | 0x5fb224d5 | equal | TL_chatAdminRights (TLRPC.java:3013) |
| release-5.14.0_1851 | InputPeer | 0x179be863 | equal | TL_inputPeerChat (TLRPC.java:32069) |
| release-5.14.0_1851 | InputPeer | 0x17bae2e6 | equal | TL_inputPeerUserFromMessage (TLRPC.java:32083) |
| release-5.14.0_1851 | InputPeer | 0x20adaef8 | equal | TL_inputPeerChannel (TLRPC.java:32123) |
| release-5.14.0_1851 | InputPeer | 0x7b8e7de6 | equal | TL_inputPeerUser (TLRPC.java:32053) |
| release-5.14.0_1851 | InputPeer | 0x7da07ec9 | equal | TL_inputPeerSelf (TLRPC.java:32044) |
| release-5.14.0_1851 | InputPeer | 0x7f3b18ea | equal | TL_inputPeerEmpty (TLRPC.java:32139) |
| release-5.14.0_1851 | InputPeer | 0x9c95f7bb | equal | TL_inputPeerChannelFromMessage (TLRPC.java:32103) |
| release-5.14.0_1851 | PollAnswer | 0x6ca9c2e9 | equal on the wire (names differ: text:str / text.text:str) | TL_pollAnswer (TLRPC.java:1938) |
| release-5.14.0_1851 | PollAnswerVoters | 0x3b6ddad2 | equal | TL_pollAnswerVoters (TLRPC.java:3303) |

Compared 249 rows; membership-only 7

Union check (Q5, ruling T2a): every button ID of this pin's single `KeyboardButton` switch, read by the A-HEAD union `TL_keyboard.KeyboardButton|TL_keyboard.KeyboardInlineButton` (the first type holding the ID): `equal` (same bytes at both pins), `opaque at A-HEAD` (a stop, not a misread) or `CONFLICT`.

| Pin | Union element | ID | Verdict | Class at this pin |
|---|---|---|---|---|
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | 0x0568a748 | equal (TL_keyboardButtonSwitchInline_layer157) | TL_keyboardButtonSwitchInline (TLRPC.java:10867) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | 0x10b78d29 | equal (TL_keyboardButtonUrlAuth_layer223) | TL_keyboardButtonUrlAuth (TLRPC.java:10901) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | 0x258aff05 | equal (TL_keyboardButtonUrl_layer223) | TL_keyboardButtonUrl (TLRPC.java:10851) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | 0x50f41ccf | equal (TL_keyboardButtonGame_layer223) | TL_keyboardButtonGame (TLRPC.java:10837) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | 0x683a5e46 | equal (TL_keyboardButtonCallback_layer117) | TL_keyboardButtonCallback (TLRPC.java:10991) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xa2fa4880 | equal (TL_keyboardButton_layer223) | TL_keyboardButton (TLRPC.java:11007) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | 0xafd93fbb | equal (TL_keyboardButtonBuy_layer223) | TL_keyboardButtonBuy (TLRPC.java:10977) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xb16a6c29 | equal (TL_keyboardButtonRequestPhone_layer223) | TL_keyboardButtonRequestPhone (TLRPC.java:10823) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xbbc7515d | equal (TL_keyboardButtonRequestPoll_layer223) | TL_keyboardButtonRequestPoll (TLRPC.java:10955) |
| release-5.14.0_1851 | TL_keyboard.KeyboardInlineButton | 0xd02e7fd4 | opaque at A-HEAD (TL_inputKeyboardButtonUrlAuth_layer223): a stop, not a misread | TL_inputKeyboardButtonUrlAuth (TLRPC.java:10927) |
| release-5.14.0_1851 | TL_keyboard.KeyboardButton | 0xfc796b3f | equal (TL_keyboardButtonRequestGeoLocation_layer223) | TL_keyboardButtonRequestGeoLocation (TLRPC.java:10887) |

Union check at release-5.14.0_1851: holds, every older inline keyboard is readable by the A-HEAD reading (a compatible widening).


# Docs cross-check

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

# facts.tsv rows

Each fact was read by hand at the cited line of its pin (the note column of `facts.tsv` names the other lines read).

| Key | Pin | Source | Value | Read |
|---|---|---|---|---|
| write_verb.messages_v2.put | A-HEAD | MessagesStorage.java:12355 | REPLACE INTO | read by hand |
| write_verb.messages_v2.put | A-11.4.2 | MessagesStorage.java:11289 | REPLACE INTO | read by hand |
| write_verb.messages_v2.put | release-8.1.1_2431 | MessagesStorage.java:8820 | REPLACE INTO | read by hand |
| write_verb.messages_v2.edit | A-HEAD | MessagesStorage.java:16429 | REPLACE INTO | read by hand |
| write_verb.messages_v2.edit | A-11.4.2 | MessagesStorage.java:14782 | REPLACE INTO | read by hand |
| write_verb.messages_v2.edit | release-8.1.1_2431 | MessagesStorage.java:10743 | REPLACE INTO | read by hand |
| write_verb.messages.put | release-5.14.0_1851 | MessagesStorage.java:5550 | REPLACE INTO | read by hand |
| write_verb.messages.edit | release-5.14.0_1851 | MessagesStorage.java:7252 | REPLACE INTO | read by hand |
| unit.call_duration | A-HEAD | MessageObject.java:5822 | seconds | read by hand |
| unit.edit_date | A-HEAD | ChatMessageCell.java:15657 | seconds | read by hand |
| unit.edit_date | A-11.4.2 | ChatMessageCell.java:8330 | seconds | read by hand |
| unit.edit_date | release-8.1.1_2431 | ChatMessageCell.java:4968 | seconds | read by hand |
| unit.edit_date | release-5.14.0_1851 | ChatMessageCell.java:4050 | seconds | read by hand |
| unit.blob_times | A-HEAD | MessageObject.java:2099 | unestablished | read by hand |
| predicate.pre84_secret_uid | A-HEAD | DatabaseMigrationHelper.java:820-825 | the low 32 bits of uid, cast to int, are 0; the chat id is the high 32 bits as a signed int (uid >> 32); the v84 migration rewrites uid to the secret-chat dialog id of that chat id | read by hand |
| predicate.pre84_channel_mid | A-HEAD | DatabaseMigrationHelper.java:819 | before v84 a channel message's mid is its id with the channel id shifted left 32 bits OR-ed in, and its uid is minus the channel id; the v84 migration keeps the low 32 bits of mid as a signed int | read by hand |
| predicate.secret_dialog | A-HEAD | DialogObject.java:109-111 | bit 62 of the dialog id is set and bit 63 is clear | read by hand |
| predicate.folder_dialog | A-HEAD | DialogObject.java:34-36 | bit 61 of the dialog id is set and bit 63 is clear | read by hand |
| cast.encrypted_chat_id | A-HEAD | DialogObject.java:117-119 | int32 | read by hand |
| line.v85_write | A-HEAD | DatabaseMigrationHelper.java:1032 | 85 | read by hand |
| columns.messages.v64 | release-5.14.0_1851 | MessagesStorage.java:227 | mid,uid,read_state,send_state,date,data,out,ttl,media,replydata,imp,mention | read by hand |
| wrapper.TL_messageForwarded_old.fwd_from.from_id | A-HEAD | TLRPC.java:61040 | user | read by hand |
| wrapper.TL_messageForwarded_old.from_id | A-HEAD | TLRPC.java:61044 | user | read by hand |
| wrapper.TL_messageService_old2.from_id | A-HEAD | TLRPC.java:59728 | user | read by hand |
| wrapper.TL_message_old.from_id | A-HEAD | TLRPC.java:61079 | user | read by hand |
| wrapper.TL_message_old6.from_id | A-HEAD | TLRPC.java:60672 | user | read by hand |
| wrapper.TL_message_old6.fwd_from.from_id | A-HEAD | TLRPC.java:60677 | user | read by hand |
| wrapper.TL_message_layer104.from_id | A-HEAD | TLRPC.java:60565 | user | read by hand |
| wrapper.TL_message_layer117.from_id | A-HEAD | TLRPC.java:60447 | user | read by hand |
| wrapper.TL_message_secret_old.from_id | A-HEAD | TLRPC.java:61236 | user | read by hand |
| wrapper.TL_message_secret_layer72.from_id | A-HEAD | TLRPC.java:61180 | user | read by hand |
| wrapper.TL_message_secret.from_id | A-HEAD | TLRPC.java:61118 | user | read by hand |
| wrapper.TL_message_old2.from_id | A-HEAD | TLRPC.java:60979 | user | read by hand |
| wrapper.TL_message_old7.from_id | A-HEAD | TLRPC.java:60062 | user | read by hand |
| wrapper.TL_message_layer72.from_id | A-HEAD | TLRPC.java:59763 | user | read by hand |
| wrapper.TL_messageService_layer118.from_id | A-HEAD | TLRPC.java:61501 | user | read by hand |
| wrapper.TL_messageService_old.from_id | A-HEAD | TLRPC.java:61012 | user | read by hand |
| wrapper.TL_messageForwarded_old2.fwd_from.from_id | A-HEAD | TLRPC.java:60154 | user | read by hand |
| wrapper.TL_messageForwarded_old2.from_id | A-HEAD | TLRPC.java:60158 | user | read by hand |
| wrapper.TL_message_old3.from_id | A-HEAD | TLRPC.java:60922 | user | read by hand |
| wrapper.TL_message_old3.fwd_from.from_id | A-HEAD | TLRPC.java:60927 | user | read by hand |
| wrapper.TL_messageService_layer48.from_id | A-HEAD | TLRPC.java:60819 | user | read by hand |
| wrapper.TL_message_layer68.from_id | A-HEAD | TLRPC.java:59866 | user | read by hand |
| wrapper.TL_message_old4.from_id | A-HEAD | TLRPC.java:60859 | user | read by hand |
| wrapper.TL_message_old4.fwd_from.from_id | A-HEAD | TLRPC.java:60864 | user | read by hand |
| wrapper.TL_message_layer47.from_id | A-HEAD | TLRPC.java:59964 | user | read by hand |
| wrapper.TL_message_old5.from_id | A-HEAD | TLRPC.java:60747 | user | read by hand |
| wrapper.TL_message_old5.fwd_from.from_id | A-HEAD | TLRPC.java:60752 | user | read by hand |
| wrapper.TL_message_layer118.from_id | A-HEAD | TL_legacy_message.java:3092 | user | read by hand |
| wrapper.TL_messageFwdHeader_layer118.from_id | A-HEAD | TLRPC.java:44008 | user | read by hand |
| wrapper.TL_messageFwdHeader_layer118.from_id | A-HEAD | TLRPC.java:44016 | channel | read by hand |
| wrapper.TL_messageFwdHeader_layer96.from_id | A-HEAD | TLRPC.java:44173 | user | read by hand |
| wrapper.TL_messageFwdHeader_layer96.from_id | A-HEAD | TLRPC.java:44178 | channel | read by hand |
| wrapper.TL_messageFwdHeader_layer68.from_id | A-HEAD | TLRPC.java:43835 | user | read by hand |
| wrapper.TL_messageFwdHeader_layer68.from_id | A-HEAD | TLRPC.java:43840 | channel | read by hand |
| wrapper.TL_messageFwdHeader_layer112.from_id | A-HEAD | TLRPC.java:44073 | user | read by hand |
| wrapper.TL_messageFwdHeader_layer112.from_id | A-HEAD | TLRPC.java:44081 | channel | read by hand |
| wrapper.TL_messageFwdHeader_layer72.from_id | A-HEAD | TLRPC.java:44132 | user | read by hand |
| wrapper.TL_messageFwdHeader_layer72.from_id | A-HEAD | TLRPC.java:44137 | channel | read by hand |
| wrapper.TL_messageReplyStoryHeader_layer174.peer | A-HEAD | TLRPC.java:28175 | user | read by hand |
| wrapper.TL_message_layer104_2.from_id | A-11.4.2 | TLRPC.java:72691 | user | read by hand |
| wrapper.TL_message_layer104_3.from_id | A-11.4.2 | TLRPC.java:72527 | user | read by hand |
| wrapper.TL_messagePeerReaction_layer137.peer_id | A-HEAD | TLRPC.java:2710 | user | read by hand |
| wrapper.TL_pollResults_layer111.recent_voters | A-HEAD | VectorLegacy.java:28 | user | read by hand |
| wrapper.TL_pollResults_layer131.recent_voters | A-HEAD | VectorLegacy.java:28 | user | read by hand |
| wrapper.TL_pollResults_layer158.recent_voters | A-HEAD | VectorLegacy.java:49 | user | read by hand |

# DDL cross-check (plan 4I4)

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

# DDL cross-check (plan 4D, Task 4b)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `master`.

- TelegramMessenger/Telegram-Android commit `21f7d109da6ab3506c9e9577f98c6a38771ab335`

Fetched files (not in the repository), SHA-256:

- DatabaseMigrationHelper.java `fd2f2d033e9049f306f4874433070511b3d72827e72148a659b15d7be13edc2a`
- MessagesStorage.java `ba5897a1a30c0add954ea36cf07b65f653192de464822a14ba43e97a559b4603`

The definition at the tag is the first CREATE TABLE of the table in the file; statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| v181.sql | chats | verified | MessagesStorage.java:632, 3 columns, names, order and definitions equal |
| v181.sql | contacts | verified | MessagesStorage.java:636, 2 columns, names, order and definitions equal |
| v181.sql | dialogs | verified | MessagesStorage.java:579, 18 columns, names, order and definitions equal |
| v181.sql | enc_chats | verified | MessagesStorage.java:633, 19 columns, names, order and definitions equal |
| v181.sql | media_v4 | verified | MessagesStorage.java:604, 6 columns, names, order and definitions equal |
| v181.sql | messages_holes | verified | MessagesStorage.java:541, 4 columns, names, order and definitions equal |
| v181.sql | messages_seq | verified | MessagesStorage.java:598, 3 columns, names, order and definitions equal |
| v181.sql | messages_topics | verified | MessagesStorage.java:693, 21 columns, names, order and definitions equal |
| v181.sql | messages_v2 | verified | MessagesStorage.java:553, 21 columns, names, order and definitions equal |
| v181.sql | randoms_v2 | verified | MessagesStorage.java:592, 4 columns, names, order and definitions equal |
| v181.sql | scheduled_messages_v2 | verified | MessagesStorage.java:547, 9 columns, names, order and definitions equal |
| v181.sql | user_contacts_v7 | verified | MessagesStorage.java:575, 5 columns, names, order and definitions equal |
| v181.sql | user_phones_v7 | verified | MessagesStorage.java:576, 5 columns, names, order and definitions equal |
| v181.sql | users | verified | MessagesStorage.java:631, 4 columns, names, order and definitions equal |
| v181.sql | users_data | verified | MessagesStorage.java:630, 2 columns, names, order and definitions equal |
| v84-upgraded-v181.sql | messages_v2.reply_to_message_id | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1036 |
| v84-upgraded-v181.sql | scheduled_messages_v2.reply_to_message_id | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1037 |
| v84-upgraded-v181.sql | dialogs.unread_reactions | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1055 |
| v84-upgraded-v181.sql | messages_v2.custom_params | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1088 |
| v84-upgraded-v181.sql | messages_v2.group_id | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1127 |
| v84-upgraded-v181.sql | dialogs.last_mid_group | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1128 |
| v84-upgraded-v181.sql | dialogs.ttl_period | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1217 |
| v84-upgraded-v181.sql | messages_v2.reply_to_story_id | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1273 |
| v84-upgraded-v181.sql | dialogs.unread_poll_votes | historical | ALTER TABLE ADD COLUMN at DatabaseMigrationHelper.java:1613 |
| v84-upgraded-v181.sql | chats | historical | 3 columns, all defined at MessagesStorage.java:632 |
| v84-upgraded-v181.sql | contacts | historical | 2 columns, all defined at MessagesStorage.java:636 |
| v84-upgraded-v181.sql | dialogs | historical | 14 columns, all defined at MessagesStorage.java:579 |
| v84-upgraded-v181.sql | enc_chats | historical | 19 columns, all defined at MessagesStorage.java:633 |
| v84-upgraded-v181.sql | media_v3 | historical | 6 columns, all defined at DatabaseMigrationHelper.java:597 |
| v84-upgraded-v181.sql | messages_holes | historical | 4 columns, all defined at MessagesStorage.java:541 |
| v84-upgraded-v181.sql | messages_seq | historical | 3 columns, all defined at MessagesStorage.java:598 |
| v84-upgraded-v181.sql | messages_v2 | historical | 17 columns, all defined at MessagesStorage.java:553 |
| v84-upgraded-v181.sql | randoms_v2 | historical | 4 columns, all defined at MessagesStorage.java:592 |
| v84-upgraded-v181.sql | scheduled_messages_v2 | historical | 8 columns, all defined at MessagesStorage.java:547 |
| v84-upgraded-v181.sql | user_contacts_v7 | historical | 5 columns, all defined at MessagesStorage.java:575 |
| v84-upgraded-v181.sql | user_phones_v7 | historical | 5 columns, all defined at MessagesStorage.java:576 |
| v84-upgraded-v181.sql | users | historical | 4 columns, all defined at MessagesStorage.java:631 |
| v84-upgraded-v181.sql | users_data | historical | 2 columns, all defined at MessagesStorage.java:630 |

# DDL cross-check (plan 4D, Task 4b)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `release-11.4.2-5469`.

- TelegramMessenger/Telegram-Android commit `fb2e545101f41303f1e2712de2e7611a9335f1c3`

Fetched files (not in the repository), SHA-256:

- DatabaseMigrationHelper.java `7d5c32421e22b541171f5b500b37f5d8bda0ddfe955d70e6516db2f1a402d087`
- MessagesStorage.java `1f927f0172ee7a47c239de3534aac119dad718da5fab8f44c6a0acb385917d11`

The definition at the tag is the first CREATE TABLE of the table in the file; statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| v159.sql | chats | verified | MessagesStorage.java:605, 3 columns, names, order and definitions equal |
| v159.sql | contacts | verified | MessagesStorage.java:609, 2 columns, names, order and definitions equal |
| v159.sql | dialogs | verified | MessagesStorage.java:554, 17 columns, names, order and definitions equal |
| v159.sql | enc_chats | verified | MessagesStorage.java:606, 19 columns, names, order and definitions equal |
| v159.sql | media_v4 | verified | MessagesStorage.java:579, 6 columns, names, order and definitions equal |
| v159.sql | messages_holes | verified | MessagesStorage.java:517, 4 columns, names, order and definitions equal |
| v159.sql | messages_seq | verified | MessagesStorage.java:573, 3 columns, names, order and definitions equal |
| v159.sql | messages_topics | verified | MessagesStorage.java:666, 21 columns, names, order and definitions equal |
| v159.sql | messages_v2 | verified | MessagesStorage.java:529, 21 columns, names, order and definitions equal |
| v159.sql | randoms_v2 | verified | MessagesStorage.java:567, 4 columns, names, order and definitions equal |
| v159.sql | scheduled_messages_v2 | verified | MessagesStorage.java:523, 9 columns, names, order and definitions equal |
| v159.sql | user_contacts_v7 | verified | MessagesStorage.java:550, 5 columns, names, order and definitions equal |
| v159.sql | user_phones_v7 | verified | MessagesStorage.java:551, 5 columns, names, order and definitions equal |
| v159.sql | users | verified | MessagesStorage.java:604, 4 columns, names, order and definitions equal |
| v159.sql | users_data | verified | MessagesStorage.java:603, 2 columns, names, order and definitions equal |

# DDL cross-check (plan 4D, Task 4b)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `release-8.1.1_2431`.

- TelegramMessenger/Telegram-Android commit `58aded63e9713ef796d76f3e29a60a5254f803b4`

Fetched files (not in the repository), SHA-256:

- MessagesStorage.java `83a94d837ea7df4370880d503c46b57b24f0c5eaab9b04434b0120b3e5122e6c`

The definition at the tag is the first CREATE TABLE of the table in the file; statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| v84.sql | chats | verified | MessagesStorage.java:365, 3 columns, names, order and definitions equal |
| v84.sql | contacts | verified | MessagesStorage.java:369, 2 columns, names, order and definitions equal |
| v84.sql | dialogs | verified | MessagesStorage.java:317, 14 columns, names, order and definitions equal |
| v84.sql | enc_chats | verified | MessagesStorage.java:366, 19 columns, names, order and definitions equal |
| v84.sql | media_v3 | verified | MessagesStorage.java:342, 6 columns, names, order and definitions equal |
| v84.sql | messages_holes | verified | MessagesStorage.java:291, 4 columns, names, order and definitions equal |
| v84.sql | messages_seq | verified | MessagesStorage.java:336, 3 columns, names, order and definitions equal |
| v84.sql | messages_v2 | verified | MessagesStorage.java:301, 17 columns, names, order and definitions equal |
| v84.sql | randoms_v2 | verified | MessagesStorage.java:330, 4 columns, names, order and definitions equal |
| v84.sql | scheduled_messages_v2 | verified | MessagesStorage.java:297, 8 columns, names, order and definitions equal |
| v84.sql | user_contacts_v7 | verified | MessagesStorage.java:313, 5 columns, names, order and definitions equal |
| v84.sql | user_phones_v7 | verified | MessagesStorage.java:314, 5 columns, names, order and definitions equal |
| v84.sql | users | verified | MessagesStorage.java:364, 4 columns, names, order and definitions equal |
| v84.sql | users_data | verified | MessagesStorage.java:363, 2 columns, names, order and definitions equal |

# DDL cross-check (plan 4D, Task 4b)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `release-5.14.0_1851`.

- TelegramMessenger/Telegram-Android commit `7d6fa267c13bacd48f8d8f10d31a61f38b77bfe4`

Fetched files (not in the repository), SHA-256:

- MessagesStorage.java `dae79902fb2c548b873c50238b03eeaf3666294c605454929a7a6e97952563af`

The definition at the tag is the first CREATE TABLE of the table in the file; statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| v64.sql | chats | verified | MessagesStorage.java:285, 3 columns, names, order and definitions equal |
| v64.sql | contacts | verified | MessagesStorage.java:289, 2 columns, names, order and definitions equal |
| v64.sql | dialogs | verified | MessagesStorage.java:242, 14 columns, names, order and definitions equal |
| v64.sql | enc_chats | verified | MessagesStorage.java:286, 19 columns, names, order and definitions equal |
| v64.sql | media_v2 | verified | MessagesStorage.java:262, 5 columns, names, order and definitions equal |
| v64.sql | messages | verified | MessagesStorage.java:227, 12 columns, names, order and definitions equal |
| v64.sql | messages_holes | verified | MessagesStorage.java:217, 4 columns, names, order and definitions equal |
| v64.sql | messages_seq | verified | MessagesStorage.java:256, 3 columns, names, order and definitions equal |
| v64.sql | user_contacts_v7 | verified | MessagesStorage.java:238, 5 columns, names, order and definitions equal |
| v64.sql | user_phones_v7 | verified | MessagesStorage.java:239, 5 columns, names, order and definitions equal |
| v64.sql | users | verified | MessagesStorage.java:284, 4 columns, names, order and definitions equal |
| v64.sql | users_data | verified | MessagesStorage.java:283, 2 columns, names, order and definitions equal |

## Hand-read rows, first read (plan 4I4b Task 1)

The four rows the reduction cannot read at A-HEAD (`--check` reports each as "MANUAL, read by hand at"), read by hand
from `TLRPC.java` at A-HEAD (21f7d109da6ab3506c9e9577f98c6a38771ab335) by the Task 1 builder; ruling B11 asks a second
read by another task's agent, recorded in a section of its own.

| ID | Class | Source | Body read | Verdict |
|---|---|---|---|---|
| 0xa3d1cb80 | TL_reactionCount | TLRPC.java:45142 (reads 45146-45151) | flags:# chosen=F.0 [F.0: chosen_order:i32] reaction:Reaction count:i32 (chosen is set by the test of bit 0 that gates chosen_order, 45147) | read by hand |
| 0x51b67eff | TL_messagePeerReaction_layer144 | TLRPC.java:2679 (reads 2683-2688) | flags:# big=F.0 unread=F.1 peer_id:Peer reaction.emoticon:str (the app builds a TL_reactionEmoji around the string, 2687-2688) | read by hand |
| 0x932844fa | TL_messagePeerReaction_layer137 | TLRPC.java:2704 (reads 2710-2714) | peer_id:id64@user reaction.emoticon:str (the int64 read at 2710 is put in the TL_peerUser built at 2713-2714; the string at 2712 in a TL_reactionEmoji) | read by hand |
| 0xfa64e172 | TL_webPage_layer107 | TLRPC.java:20041 (reads 20045-20093) | flags:# id:i64 url:str display_url:str hash:i32 [F.0: type:str] [F.1: site_name:str] [F.2: title:str] [F.3: description:str] [F.4: photo:Photo] [F.5: embed_url:str] [F.5: embed_type:str] [F.6: embed_width:i32] [F.6: embed_height:i32] [F.7: duration:i32] [F.8: author:str] [F.9: document:Document] [F.11: documents:vec<Document>] [F.10: cached_page:TL_iv.Page] (bit 11 is read before bit 10, 20086-20093; the app wraps the documents in a TL_webPageAttributeTheme) | read by hand |

## Hand-read rows, second read (plan 4I4b Task 2, B11)

The same four rows, read a second time by the Task 2 builder, a different agent from Task 1, from `TLRPC.java` at
A-HEAD (21f7d109da6ab3506c9e9577f98c6a38771ab335; fetched into scratch, SHA-256
37a1b1ee83d1f84352f90ac65e8d2176a863678f60c902010c3e1c0b1beea86b, deleted after). Each body was written from the
class's `readParams` before `constructors.tsv`, this file or the recon were opened, then compared token for token
with the row's `fields`. All four agree in what is read (IDs, field order, flag bits, wire types). Reading notes:
`0xa3d1cb80` reads 45146-45151, `chosen` is the flag-only test of bit 0 that also gates `chosen_order`;
`0x51b67eff` reads 2683-2688, the string goes into a `TL_reactionEmoji` the app builds (not read);
`0x932844fa` reads 2710-2712, one int64 then one string: the int64 is held in `user_id` and becomes the user id of
the `TL_peerUser` the app builds (2713-2714), which the T5 notation names `peer_id:id64@user` (as the other inline
ids of the table name the peer the app builds); `0xfa64e172` reads 20045-20093, bit 11 (a vector of documents the app
wraps in a `TL_webPageAttributeTheme`) is read before bit 10 (`cached_page`).

| ID | Class | Source | Body read | Verdict |
|---|---|---|---|---|
| 0xa3d1cb80 | TL_reactionCount | TLRPC.java:45142 | flags:# chosen=F.0 [F.0: chosen_order:i32] reaction:Reaction count:i32 | agrees |
| 0x51b67eff | TL_messagePeerReaction_layer144 | TLRPC.java:2679 | flags:# big=F.0 unread=F.1 peer_id:Peer reaction.emoticon:str | agrees |
| 0x932844fa | TL_messagePeerReaction_layer137 | TLRPC.java:2704 | peer_id:id64@user reaction.emoticon:str | agrees |
| 0xfa64e172 | TL_webPage_layer107 | TLRPC.java:20041 | flags:# id:i64 url:str display_url:str hash:i32 [F.0: type:str] [F.1: site_name:str] [F.2: title:str] [F.3: description:str] [F.4: photo:Photo] [F.5: embed_url:str] [F.5: embed_type:str] [F.6: embed_width:i32] [F.6: embed_height:i32] [F.7: duration:i32] [F.8: author:str] [F.9: document:Document] [F.11: documents:vec<Document>] [F.10: cached_page:TL_iv.Page] | agrees |

## Hand-read rows, first read (plan 4I4c Task 2)

The six Stage B rows the reduction cannot read at A-HEAD (`--check` reports each as "MANUAL, read by hand at"), read by
hand at A-HEAD (21f7d109da6ab3506c9e9577f98c6a38771ab335) by the Task 2 builder from `tgnet/tl/TL_keyboard.java`
(SHA-256 a026f2b766ea4ca29c9abee0a4010773d4c7f38b848939b17eeee1068388b6bc), `tgnet/TLRPC.java` (SHA-256
37a1b1ee83d1f84352f90ac65e8d2176a863678f60c902010c3e1c0b1beea86b) and `tgnet/VectorLegacy.java` (SHA-256
ddb561e3fd975d74306f718f94e00f11be984e1fe87d0252cf08edcfcaefac30); ruling B11 asks a second read by another task's
agent, recorded in a section of its own. Reading notes: `0x77608b83` `TL_keyboardInlineButtonRow_layer228` reads its
buttons at `TL_keyboard.java:649` through `TLdeserializeLegacy` (`:561-575`): `KeyboardButton.fromConstructor` first
(`:562`), then `KeyboardInlineButton.fromConstructor`, else an error; it then keeps only the inline buttons (`:650-654`),
so a `TL_keyboard.KeyboardButton` read there is on the wire but dropped by the A-HEAD app (design D5, ruling C3).
`0x77608b83` `KeyboardButtonRow` reads its buttons at `:760` by `KeyboardButton.TLdeserialize`, which tries
`KeyboardInlineButton.fromConstructor` first (`:815-822`, the inline button read whole and kept as a default button) and
then its own switch (`:825`). `0x19420af6` `TL_keyboardInlineButtonRow` reads its buttons at `:636` by
`KeyboardInlineButton.TLdeserialize`, which tries `KeyboardButton.fromConstructor` first (`:735-742`, kept as a disabled
inline button) and then its own (`:745`). The two button switches share no ID, so the order of a union never changes
what is read (ruling T2a). `0xc87024a2` and `0xbadcc1a3` read `recent_voters` (bit 3) through
`VectorLegacy.deserialize_IntUserIdAsPeer` (`TLRPC.java:4275`, `:4308`): a boxed bare int vector (`VectorLegacy.java:28`)
whose every id becomes the `user_id` of a `TL_peerUser` the app builds (`:31-32`); `0xdcb82ea3` through
`deserialize_LongUserIdAsPeer` (`TLRPC.java:4450`; `VectorLegacy.java:49`, `:52-53`), a bare long vector. `0xbadcc1a3`
(reads 4299-4313) and `0xdcb82ea3` (reads 4441-4455) read `solution` and `solution_entities` under one test of bit 4;
`0xc87024a2` reads 4266-4276.

| ID | Class | Source | Body read | Verdict |
|---|---|---|---|---|
| 0x77608b83 | TL_keyboardInlineButtonRow_layer228 | TL_keyboard.java:645 | buttons:vec<TL_keyboard.KeyboardButton\|TL_keyboard.KeyboardInlineButton> | read by hand |
| 0x77608b83 | KeyboardButtonRow | TL_keyboard.java:749 | buttons:vec<TL_keyboard.KeyboardInlineButton\|TL_keyboard.KeyboardButton> | read by hand |
| 0x19420af6 | TL_keyboardInlineButtonRow | TL_keyboard.java:632 | buttons:vec<TL_keyboard.KeyboardButton\|TL_keyboard.KeyboardInlineButton> | read by hand |
| 0xc87024a2 | TL_pollResults_layer111 | TLRPC.java:4262 | flags:# min=F.0 [F.1: results:vec<PollAnswerVoters>] [F.2: total_voters:i32] [F.3: recent_voters:vec<id32@user>] | read by hand |
| 0xbadcc1a3 | TL_pollResults_layer131 | TLRPC.java:4295 | flags:# min=F.0 [F.1: results:vec<PollAnswerVoters>] [F.2: total_voters:i32] [F.3: recent_voters:vec<id32@user>] [F.4: solution:str solution_entities:vec<MessageEntity>] | read by hand |
| 0xdcb82ea3 | TL_pollResults_layer158 | TLRPC.java:4437 | flags:# min=F.0 [F.1: results:vec<PollAnswerVoters>] [F.2: total_voters:i32] [F.3: recent_voters:vec<id64@user>] [F.4: solution:str solution_entities:vec<MessageEntity>] | read by hand |

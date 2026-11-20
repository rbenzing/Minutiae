# TL facts cross-check (plan 4I4)

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
| `tgnet/tl/legacy/TL_legacy_message.java` | 8bcf9fbf58ef78fbb13f22395d1790e697605a787621f21a59b222be8adad43e |
| `ui/Stories/MessageMediaStoryFull.java` | 0691a6ddb1639e2880038bf0377721e20644a6e539c8746a52b673353e289faf |
| `ui/Stories/MessageMediaStoryFull_old.java` | d83f99d50d7a5d2ebc8d90dec1bd23bc4618816b00a33fec6567b8c8f248d093 |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | d45538b70f1d7d0dd81e02800445454a0a228ada8864b83a65c21c75691365a1 |
| `facts.tsv` | a4f462c2bf624b44e6f28d405a2f672fb49eae06b8948cd901075428cfe11782 |

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
| MessageAction | 0x80e11a7f | TL_messageActionPhoneCall | verified | TLRPC.java:26192 |  |
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
| WebPage | 0xfa64e172 | TL_webPage_layer107 | verified | TLRPC.java:20041 |  |
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

Completeness: every ID the switch of each type reads at this pin is a row (MessageAction: the phone-call action only, by design).

| Type | IDs the switch reads | Rows pinned here | Verdict |
|---|---|---|---|
| Message | 47 | 47 | complete |
| Peer | 6 | 6 | complete |
| MessageFwdHeader | 7 | 7 | complete |
| MessageReplyHeader | 9 | 9 | complete |
| MessageEntity | 28 | 28 | complete |
| PhoneCallDiscardReason | 5 | 5 | complete |
| MessageMedia | 52 | 52 | complete |
| Photo | 8 | 8 | complete |
| Document | 10 | 10 | complete |
| GeoPoint | 4 | 4 | complete |
| WebPage | 12 | 12 | complete |
| User | 29 | 29 | complete |
| Chat | 37 | 37 | complete |


## A-11.4.2 (fb2e545101f41303f1e2712de2e7611a9335f1c3)

Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):

| File | SHA-256 |
|---|---|
| `messenger/DatabaseMigrationHelper.java` | 7d5c32421e22b541171f5b500b37f5d8bda0ddfe955d70e6516db2f1a402d087 |
| `messenger/DialogObject.java` | d1669afaa7ba84bb616ffbefbb2a5f1471d8bfea730890765cbb0e12fee6356e |
| `messenger/MessageObject.java` | 9c60073676f13b8cba0f3272dfdbfdfb901410aa9d35c4e16eb18e5f7b889316 |
| `messenger/MessagesController.java` | 5f23a54991b1d6f7b941ca1c3cf7b4cd197eb4a7d83cb981eb043e84a40c25d5 |
| `messenger/MessagesStorage.java` | 1f927f0172ee7a47c239de3534aac119dad718da5fab8f44c6a0acb385917d11 |
| `messenger/SecretChatHelper.java` | bc8e0deb20e13811b10694c77ca4988b128756200c6bd27fa4b18016308e63e1 |
| `tgnet/NativeByteBuffer.java` | 845efe0b45b1d557a77dbba74fa8b3ddb1c15ce2ba7911ee09f1ccfe4839d083 |
| `tgnet/SerializedData.java` | 789a5892bbac33f4061828603fea5c52e0f49298ae45a2c2c1a61a47e045afa5 |
| `tgnet/TLObject.java` | 09ed9fd44708933d1bb72b99467a19f1e87651bbecd869fe2a37c737a2afc7d2 |
| `tgnet/TLRPC.java` | 4ad82d1ea3acbee9d99f27111eee9d7d6b01c44300af31f523628866df67a06a |
| `ui/Stories/MessageMediaStoryFull.java` | d32150c15955e21c39cf5f47dbc99fe2d95bcb7a740324f9470ca16a037cbc69 |
| `ui/Stories/MessageMediaStoryFull_old.java` | 53134ad4bf0ecbbc7ecf6372aeb6afe854ef30e043f56297706f584820179edf |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | d45538b70f1d7d0dd81e02800445454a0a228ada8864b83a65c21c75691365a1 |
| `facts.tsv` | a4f462c2bf624b44e6f28d405a2f672fb49eae06b8948cd901075428cfe11782 |

Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's switch, the class, the source line, the body the selection asks for, and the field order reduced from `readParams`; a `prefix` row is a proper prefix of the reduced body):

| Type | ID | Class | Verdict | Source | Note |
|---|---|---|---|---|---|
| Message | 0x1c9b1027 | TL_message_layer104_2 | verified | TLRPC.java:72674 |  |
| Message | 0x9789dac4 | TL_message_layer104_3 | verified | TLRPC.java:72510 |  |
| Chat | 0x0db12acb | TL_channel_layer167_3 | verified | TLRPC.java:49495 |  |
| Chat | 0xa636a3e2 | TL_channel_layer167_2 | verified | TLRPC.java:49322 |  |

Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv` (MessageAction: the `TL_messageActionPhoneCall` family only).

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
| A-11.4.2 | MessageAction | 0x80e11a7f | in-table | TL_messageActionPhoneCall (TLRPC.java:29216) |
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


## release-8.1.1_2431 (58aded63e9713ef796d76f3e29a60a5254f803b4)

Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):

| File | SHA-256 |
|---|---|
| `messenger/DialogObject.java` | bc803c85f37835054c3bafeb994235d089969c7c139bb2ce858f59e54602bd1e |
| `messenger/MessageObject.java` | 59e1f249d0cd51820d2284f228204908b7e4f3b0f465a464e2c84dc01b61d938 |
| `messenger/MessagesController.java` | a27b11e0de0d3e4aa413e1e98e2b03705a267c9b2108e28e7c3dee7f14fc1fef |
| `messenger/MessagesStorage.java` | 83a94d837ea7df4370880d503c46b57b24f0c5eaab9b04434b0120b3e5122e6c |
| `messenger/SecretChatHelper.java` | 98eb88384f97a37f7609ca657a700a2f4b99c0ab6037630dbc63fe79deec3908 |
| `tgnet/NativeByteBuffer.java` | 6d40177cf326cbcb4f44397471b28d506f82ca1113049a826690b7d6606e5033 |
| `tgnet/SerializedData.java` | 789a5892bbac33f4061828603fea5c52e0f49298ae45a2c2c1a61a47e045afa5 |
| `tgnet/TLObject.java` | 09ed9fd44708933d1bb72b99467a19f1e87651bbecd869fe2a37c737a2afc7d2 |
| `tgnet/TLRPC.java` | 82a26b9545edcf9f0dec632d213e21b436ef1a9262d0807e5f374d24b1c04ca8 |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | d45538b70f1d7d0dd81e02800445454a0a228ada8864b83a65c21c75691365a1 |
| `facts.tsv` | a4f462c2bf624b44e6f28d405a2f672fb49eae06b8948cd901075428cfe11782 |

Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's switch, the class, the source line, the body the selection asks for, and the field order reduced from `readParams`; a `prefix` row is a proper prefix of the reduced body):

| Type | ID | Class | Verdict | Source | Note |
|---|---|---|---|---|---|
| - | - | - | no row is pinned here | - | - |

Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv` (MessageAction: the `TL_messageActionPhoneCall` family only).

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
| release-8.1.1_2431 | MessageAction | 0x80e11a7f | in-table | TL_messageActionPhoneCall (TLRPC.java:20862) |
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


## release-5.14.0_1851 (7d6fa267c13bacd48f8d8f10d31a61f38b77bfe4)

Fetched files (paths below `TMessagesProj/src/main/java/org/telegram/`):

| File | SHA-256 |
|---|---|
| `messenger/DialogObject.java` | 3aebae538c5f7805641e41ed7e62319d340cb0ef9611d382441a68b28b4d65e5 |
| `messenger/MessageObject.java` | 70d7c1ef27de0ea413bd9614d9eed37ff1622c1a215173a300890af20a290cdf |
| `messenger/MessagesController.java` | 47d629cac7f3a4f2c5891ed25a5d0f3c65ce163dbdcb8bd771e8b7e135cd837b |
| `messenger/MessagesStorage.java` | dae79902fb2c548b873c50238b03eeaf3666294c605454929a7a6e97952563af |
| `messenger/SecretChatHelper.java` | 222742f943bedcc0356699b7d4dae3f044ea942045da0a950fee316d23aaad1b |
| `tgnet/NativeByteBuffer.java` | 645322968184a9a76c94ebe523bd43cb68e3894dd5e91761beb0ea84bd4d4368 |
| `tgnet/SerializedData.java` | a3e041460deea68565ab6b8b6d83a2b89a27bbfe3edc397d675cdc3c76135d49 |
| `tgnet/TLObject.java` | 09ed9fd44708933d1bb72b99467a19f1e87651bbecd869fe2a37c737a2afc7d2 |
| `tgnet/TLRPC.java` | 990b58441399718bec3da1f399ece395ef9a546ebd46c888534b3b0e631d1547 |

Fact files checked (SHA-256 of the committed bytes):

| File | SHA-256 |
|---|---|
| `constructors.tsv` | d45538b70f1d7d0dd81e02800445454a0a228ada8864b83a65c21c75691365a1 |
| `facts.tsv` | a4f462c2bf624b44e6f28d405a2f672fb49eae06b8948cd901075428cfe11782 |

Rows of `constructors.tsv` pinned here, against the source of this pin (the ID read by the type's switch, the class, the source line, the body the selection asks for, and the field order reduced from `readParams`; a `prefix` row is a proper prefix of the reduced body):

| Type | ID | Class | Verdict | Source | Note |
|---|---|---|---|---|---|
| PhoneCallDiscardReason | 0xafe2b839 | TL_phoneCallDiscardReasonAllowGroupCall | verified | TLRPC.java:12808 |  |

Membership: every ID the pinned types' switches read at this pin is a row of `constructors.tsv` (MessageAction: the `TL_messageActionPhoneCall` family only).

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
| release-5.14.0_1851 | MessageAction | 0x80e11a7f | in-table | TL_messageActionPhoneCall (TLRPC.java:15135) |
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
| unit.edit_date | A-HEAD | MessageObject.java:10185 | unestablished | read by hand |
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

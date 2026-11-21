# Google Messages facts and DDL cross-check (plan 4I5)

Facts only, Apache-2.0 sources: no source text is copied here; each fact cites a file and line at a pinned commit. The fetched sources are not in the repository.

## Pins

- M: platform/packages/apps/Messaging at commit `de315b762312dd1a5d2bbd16e62ef2bd123f61e5` (main, 2025-02-26).
- T10: platform/packages/providers/TelephonyProvider at tag android-10.0.0_r1, which peels to the single commit `86d1d9ea4e2e2cd92e0f7fadaff05580f9ef2405`.
- F10: platform/frameworks/base at tag android-10.0.0_r1, which peels to the single commit `57bb140be9e48cf08acba131f7463e461777bb8e`.

## Fetched files (SHA-256)

Pin M (Java files as hashed by the cross-check tool, plus two resource files):

- BugleDatabaseOperations.java `ad2c440e74460b8e6bcfaec76f89ee56bcb592019f309d66a39bc57b2bfd7b31`
- DatabaseHelper.java `33b5fbc849c7c01068bc50543cbac8784174fad009660401d15b9587f1821e29`
- DatabaseMessages.java `c0daa2d78989b7c1ee73767eb7888276b3e62c2a6964d2221febfcb5d96ad793`
- DatabaseUpgradeHelper.java `da91ed249bfad2ffffc69d77668b5d803a3fd98bae378e1062ca0107501df4f4`
- DeleteMessageAction.java `a20edee91cdae8136f5dda92d0f24d76259a1b00156b6af70c38666c69387229`
- MessageData.java `e4dbb5ac8011788f04ac322eada0ff16eed0d03a79209562b744abaa92fe328a`
- MessagingContentProvider.java `d4860765f8d28c9d64e001d8db439b1ad74187751906fb1078d948f9bb27471a`
- MmsUtils.java `d1d0ef9ce9eb7e85cf94850af0303b634fdd151c9a6252843eb512d7f7f72b1b`
- ParticipantData.java `b5af3f70f034617f34f1b22edd54555a958a0f396769d7e26ef5ab683ca22c08`
- SyncMessageBatch.java `a4c4c8b18023846c67fc205db532b2a680c82f747ffeafdce7948643e9de1f02`
- UpdateDestinationBlockedAction.java `a5b03234ffbdeea01a6474d60ba8ea6da47c0fdc576ff7bc052e3c42c37b5946`
- versions.xml `4c8b40237fa2c4939231f41795560c8cf7f5c7bdead58ad8e2afd1b55edf61b5`
- AndroidManifest.xml `c85ac794dcdb1c337f3341a55d1bcae63b8381577ca0115d9d7a1baab697035e`

Pin T10:

- MmsSmsDatabaseHelper.java `69b4a7f0ad6efdc5520b507f5210585f9732cb8c31013c89cd7217a343e05fbb`

Pin F10:

- Telephony.java `ca31e29a42905289f2f8e7b0f9e8af724b7edeea4574e47d644baca149d661d5`

MessagingContentProvider.java (in the Pin M list) was fetched beyond the brief's list: it holds UNSPECIFIED_SIZE = -1 (line 144), which the parts DDL uses, so the cross-check can evaluate the parts table.

## Facts

Each row of facts.tsv: `verified at` means a script confirmed the cited lines hold the stated constant, value or text; `MANUAL, read by hand at` means a person read the lines.

| Key | Check |
|---|---|
| message_protocol.-1 | verified at MessageData.java:114 |
| message_protocol.0 | verified at MessageData.java:115 |
| message_protocol.1 | verified at MessageData.java:116 |
| message_protocol.2 | verified at MessageData.java:117 |
| message_status.0 | verified at MessageData.java:120 |
| message_status.1 | verified at MessageData.java:123 |
| message_status.2 | verified at MessageData.java:124 |
| message_status.3 | verified at MessageData.java:126 |
| message_status.4 | verified at MessageData.java:127 |
| message_status.5 | verified at MessageData.java:128 |
| message_status.6 | verified at MessageData.java:129 |
| message_status.7 | verified at MessageData.java:130 |
| message_status.8 | verified at MessageData.java:131 |
| message_status.9 | verified at MessageData.java:132 |
| message_status.100 | verified at MessageData.java:135 |
| message_status.101 | verified at MessageData.java:136 |
| message_status.102 | verified at MessageData.java:137 |
| message_status.103 | verified at MessageData.java:138 |
| message_status.104 | verified at MessageData.java:139 |
| message_status.105 | verified at MessageData.java:140 |
| message_status.106 | verified at MessageData.java:141 |
| message_status.107 | verified at MessageData.java:142 |
| self.other_than_self_sub_id | MANUAL, read by hand at ParticipantData.java:51,54 |
| self.is_self | verified at ParticipantData.java:505-507 |
| self.default_sub_id | verified at DatabaseHelper.java:390 |
| self.destinations_null | verified at DatabaseHelper.java:468-474,811 |
| members.exclude_self | verified at DatabaseHelper.java:476-477 |
| contact_id.unresolved | verified at ParticipantData.java:60 |
| time.unit | verified at DatabaseMessages.java:161-163,371-372,381 |
| time.sentinel | verified at DatabaseHelper.java:278-293 |
| trigger.parts_timestamp_insert | verified at DatabaseHelper.java:364-372 |
| trigger.parts_timestamp_update | verified at DatabaseHelper.java:374-380 |
| messages.id_autoincrement | verified at DatabaseHelper.java:275 |
| rebuild.drop_recreate | verified at DatabaseUpgradeHelper.java:41,93 |
| version.current | verified at versions.xml:19 |
| version.upgrade_1_2 | verified at DatabaseUpgradeHelper.java:48-50,59-61 |
| uri.sms | verified at Telephony.java:395 |
| uri.mms | verified at Telephony.java:2790 |
| uri.written | verified at DatabaseMessages.java:169,387 |
| uri.mms_part | MANUAL, read by hand at MmsUtils.java:1753-1756 |
| sms_id.autoincrement | verified at MmsSmsDatabaseHelper.java:995-996 |
| pdu_id.autoincrement | verified at MmsSmsDatabaseHelper.java:728-729 |
| parts.text_or_uri | verified at DatabaseHelper.java:315 |
| parts.dims_unspecified | verified at DatabaseHelper.java:352-355 |
| read.values | verified at DatabaseHelper.java:231-234,282-283 |
| sync.status_mapping | verified at SyncMessageBatch.java:199-222 |
| delete.row_removal | verified at BugleDatabaseOperations.java:1253-1254 |
| blocked.archives | verified at UpdateDestinationBlockedAction.java:111-120 |
| path.package_aosp | verified at AndroidManifest.xml:18 |
| path.database | verified at DatabaseHelper.java:46 |

## Re-pinned lines

- `sync.status_mapping`: the recon cites SyncMessageBatch.java:198-222; the method starts at line 199 (198 is blank). Pinned as 199-222.
- `parts.dims_unspecified`: the brief cites DatabaseHelper.java:351-354; the two UNSPECIFIED_SIZE uses are at lines 353 and 355. Pinned as 352-355.

## DDL cross-check

The four sources are first fetched at the pinned commit of pin M into scratch (`<src>`, never committed), one file at a time, and each file's SHA-256 is verified against the hash listed below before the command runs. The section below this paragraph is the unedited output of one command (exit status 0), run from the repository root with the
four fetched files DatabaseHelper.java, DatabaseUpgradeHelper.java, MessagingContentProvider.java and ParticipantData.java of
pin M in `<src>` (the hashes below are theirs). The statement of `participants` uses a constant of a library that no fetched
source defines, so its two rows are stated by the explicit `--manual` input, not by the evaluator:

```
py -3 -I tools/fixtures/ddl_crosscheck.py <src> tools/fixtures/schemas/google-messages --family google-messages     --snapshot aosp-v2.sql --snapshot aosp-v1.sql --tag main     --commit platform/packages/apps/Messaging=de315b762312dd1a5d2bbd16e62ef2bd123f61e5     --manual "participants=DatabaseHelper.java:446-467: ParticipantData.OTHER_THAN_SELF_SUB_ID is DEFAULT_SELF_SUB_ID - 1 (ParticipantData.java:54) and DEFAULT_SELF_SUB_ID is MmsManager.DEFAULT_SUB_ID (ParticipantData.java:51), a constant of the library androidx.appcompat.mms that no fetched source defines; its value -1 is stated by the comment at ParticipantData.java:50, so OTHER_THAN_SELF_SUB_ID is -2; the 15 columns in order and their definitions were read by hand and equal DatabaseHelper.java:447-466, with INVALID_SLOT_ID -1 (ParticipantData.java:57) and the contact id default -1 (ParticipantData.java:60)"     --write-report <report>
```

# DDL cross-check (plan 4I5)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `main`.

- platform/packages/apps/Messaging commit `de315b762312dd1a5d2bbd16e62ef2bd123f61e5`

Fetched files (not in the repository), SHA-256:

- DatabaseHelper.java `33b5fbc849c7c01068bc50543cbac8784174fad009660401d15b9587f1821e29`
- DatabaseUpgradeHelper.java `da91ed249bfad2ffffc69d77668b5d803a3fd98bae378e1062ca0107501df4f4`
- MessagingContentProvider.java `d4860765f8d28c9d64e001d8db439b1ad74187751906fb1078d948f9bb27471a`
- ParticipantData.java `b5af3f70f034617f34f1b22edd54555a958a0f396769d7e26ef5ab683ca22c08`

The definition at the tag is the first CREATE TABLE of the table in the file; statements the script could not evaluate: 1.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| aosp-v1.sql | conversation_participants | historical | 5 columns, all defined at DatabaseHelper.java:488 |
| aosp-v1.sql | conversations | historical | 27 columns, all defined at DatabaseHelper.java:162 |
| aosp-v1.sql | messages | historical | 20 columns, all defined at DatabaseHelper.java:274 |
| aosp-v1.sql | participants | MANUAL, read by hand at DatabaseHelper.java:446-467 | DatabaseHelper.java:446-467: ParticipantData.OTHER_THAN_SELF_SUB_ID is DEFAULT_SELF_SUB_ID - 1 (ParticipantData.java:54) and DEFAULT_SELF_SUB_ID is MmsManager.DEFAULT_SUB_ID (ParticipantData.java:51), a constant of the library androidx.appcompat.mms that no fetched source defines; its value -1 is stated by the comment at ParticipantData.java:50, so OTHER_THAN_SELF_SUB_ID is -2; the 15 columns in order and their definitions were read by hand and equal DatabaseHelper.java:447-466, with INVALID_SLOT_ID -1 (ParticipantData.java:57) and the contact id default -1 (ParticipantData.java:60) |
| aosp-v1.sql | parts | historical | 10 columns, all defined at DatabaseHelper.java:346 |
| aosp-v2.sql | conversation_participants | verified | DatabaseHelper.java:488, 5 columns, names, order and definitions equal |
| aosp-v2.sql | conversations | verified | DatabaseHelper.java:162, 28 columns, names, order and definitions equal |
| aosp-v2.sql | messages | verified | DatabaseHelper.java:274, 20 columns, names, order and definitions equal |
| aosp-v2.sql | participants | MANUAL, read by hand at DatabaseHelper.java:446-467 | DatabaseHelper.java:446-467: ParticipantData.OTHER_THAN_SELF_SUB_ID is DEFAULT_SELF_SUB_ID - 1 (ParticipantData.java:54) and DEFAULT_SELF_SUB_ID is MmsManager.DEFAULT_SUB_ID (ParticipantData.java:51), a constant of the library androidx.appcompat.mms that no fetched source defines; its value -1 is stated by the comment at ParticipantData.java:50, so OTHER_THAN_SELF_SUB_ID is -2; the 15 columns in order and their definitions were read by hand and equal DatabaseHelper.java:447-466, with INVALID_SLOT_ID -1 (ParticipantData.java:57) and the contact id default -1 (ParticipantData.java:60) |
| aosp-v2.sql | parts | verified | DatabaseHelper.java:346, 10 columns, names, order and definitions equal |

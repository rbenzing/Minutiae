# DDL cross-check (plan 4E, Task 4): iOS system databases

No public source defines the schema of `sms.db`, `CallHistory.storedata`, `call_history.db`, `AddressBook.sqlitedb`,
`AddressBookImages.sqlitedb` or the `Manifest.db` of an iOS backup (plan 4E ruling 4E-R13). Every snapshot in this
directory is written from memory of those databases. None was fetched, extracted or compared with any definition, no
real device database was used, and no script cross-checks them: there is nothing to compare them with. Each snapshot
states `(verdict no-public-source)` and `-- Source: memory` in its header, and is listed in `docs/limitations.md`
("iOS schemas and backup formats are from memory").

The fixtures generated from these snapshots (plan 4E, Tasks 7a, 7, 11 and 13) are validated only against the same
memory, so a fixture proves that the parser reads the remembered layout, never that a real database has it. A real
database that differs degrades through the parser's probe (`unsupported-schema`, `not-applicable`) or through absent
optional fields. Running the parsers over real databases (plan 4H, `realdata`) is required before relying on them.

Version names (iOS 10 to 12, 14 and 15, 16 and 17; "recent", "older") are from memory too: which release added
or dropped a column is not established by anything here. The variants are shapes the parsers must read, not claims
about when a release changed.

## DDL verdicts

| File | Table | Verdict | Evidence |
|---|---|---|---|
| addressbook-current.sql | ABPerson | no-public-source | memory |
| addressbook-current.sql | ABMultiValue | no-public-source | memory |
| addressbook-current.sql | ABMultiValueLabel | no-public-source | memory |
| addressbook-current.sql | ABMultiValueEntry | no-public-source | memory |
| addressbook-current.sql | ABMultiValueEntryKey | no-public-source | memory |
| addressbook-current.sql | ABStore | no-public-source | memory |
| addressbook-current.sql | ABGroup | no-public-source | memory |
| addressbook-current.sql | ABGroupMembers | no-public-source | memory |
| addressbook-older.sql | ABPerson | no-public-source | memory |
| addressbook-older.sql | ABMultiValue | no-public-source | memory |
| addressbook-older.sql | ABMultiValueLabel | no-public-source | memory |
| addressbook-older.sql | ABMultiValueEntry | no-public-source | memory |
| addressbook-older.sql | ABMultiValueEntryKey | no-public-source | memory |
| addressbook-older.sql | ABGroup | no-public-source | memory |
| addressbook-older.sql | ABGroupMembers | no-public-source | memory |
| addressbookimages.sql | ABThumbnailImage | no-public-source | memory |
| addressbookimages.sql | ABFullSizeImage | no-public-source | memory |
| backup-manifest.sql | Files | no-public-source | memory |
| backup-manifest.sql | Properties | no-public-source | memory |
| callhistory-coredata.sql | ZCALLRECORD | no-public-source | memory |
| callhistory-coredata.sql | Z_PRIMARYKEY | no-public-source | memory |
| callhistory-coredata.sql | Z_METADATA | no-public-source | memory |
| callhistory-legacy.sql | call | no-public-source | memory |
| sms-ios10.sql | message | no-public-source | memory |
| sms-ios10.sql | handle | no-public-source | memory |
| sms-ios10.sql | chat | no-public-source | memory |
| sms-ios10.sql | chat_message_join | no-public-source | memory |
| sms-ios10.sql | chat_handle_join | no-public-source | memory |
| sms-ios10.sql | attachment | no-public-source | memory |
| sms-ios10.sql | message_attachment_join | no-public-source | memory |
| sms-ios14.sql | message | no-public-source | memory |
| sms-ios14.sql | handle | no-public-source | memory |
| sms-ios14.sql | chat | no-public-source | memory |
| sms-ios14.sql | chat_message_join | no-public-source | memory |
| sms-ios14.sql | chat_handle_join | no-public-source | memory |
| sms-ios14.sql | attachment | no-public-source | memory |
| sms-ios14.sql | message_attachment_join | no-public-source | memory |
| sms-ios17.sql | message | no-public-source | memory |
| sms-ios17.sql | handle | no-public-source | memory |
| sms-ios17.sql | chat | no-public-source | memory |
| sms-ios17.sql | chat_message_join | no-public-source | memory |
| sms-ios17.sql | chat_handle_join | no-public-source | memory |
| sms-ios17.sql | attachment | no-public-source | memory |
| sms-ios17.sql | message_attachment_join | no-public-source | memory |
| sms-ios17.sql | chat_recoverable_message_join | no-public-source | memory |
| sms-ios17.sql | recoverable_message_part | no-public-source | memory |

## Non-DDL items

Fetched on 2026-10-25 as untrusted text into the project scratch directory, never built or executed. The source is the
open-source libimobiledevice library, tag `1.3.0`, commit `15f8652126664e3a4b980e5d1c039b9053ce8566` (the tag resolved
through the GitHub REST API, `git/ref/tags/1.3.0`). File `tools/idevicebackup2.c`, SHA-256
`41472f454209082f1c02469b04f25261d70c19f03124710efb6950f7876365cf`.

Re-fetch: `curl -fsS https://raw.githubusercontent.com/libimobiledevice/libimobiledevice/1.3.0/tools/idevicebackup2.c`.

| Item | Source | Verdict | Evidence |
|---|---|---|---|
| Manifest.plist IsEncrypted | libimobiledevice 1.3.0 tools/idevicebackup2.c | verified | The tool reads `Manifest.plist` and looks up the key `IsEncrypted` in its top-level dictionary (line 1727: `plist_dict_get_item(manifest_plist, "IsEncrypted")`); only a node of type boolean is used (lines 1728-1729), and only for its restore and unback commands (line 1707). So the key name, its place in the top-level dictionary and its boolean type are confirmed. |
| IsEncrypted of a non-boolean or absent value | libimobiledevice 1.3.0 tools/idevicebackup2.c | not-adopted | The tool leaves its flag false (not encrypted) when the key is absent or not a boolean (flag initialised to 0 at line 1696, set only at line 1729). Minutiae does not copy that: an absent or non-boolean value is `EncUnknown` (`iosbackup.ReadEncryption`), never "not encrypted". The tool's default is a convenience of a restore command, not a statement about the backup. |
| Manifest.db columns | libimobiledevice 1.3.0 tools/idevicebackup2.c | no-public-source | The tool does not open `Manifest.db` or SQLite at this tag (a search for `Manifest.db`, `sqlite` and `Files` finds only the `DLMessage*Files` message names and `iTunes Files`). `backup-manifest.sql` stays from memory. |
| Files.flags value 1, the sha1 fileID rule, domains and relative paths | none consulted | no-public-source | From memory (`docs/limitations.md`). |
| sms.db reaction ranges and prefixes, attachment domain `MediaDomain` | none consulted | no-public-source | From memory (`docs/limitations.md`). |
| ZCALLTYPE codes, AddressBook property ids 3, 4, 5 and 22, label syntax, `member_type` 0 | none consulted | no-public-source | From memory (`docs/limitations.md`). |
| attributedBody typedstream layout | see plan 4E Task 4b | not yet checked | Task 4b records its own rows here. |

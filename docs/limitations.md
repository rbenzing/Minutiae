# Known limitations

What the current build does not do, does approximately, or has validated only on builder data.
The items are split by area, so a change reads the limitations of the code it touches and not the
whole set. The rules a change must keep are in [invariants.md](invariants.md).

## How to use it

- A change that closes or adds a limitation edits its item in the area file, in the same commit.
- A format whose layout was written from memory is also listed by name in the table at the end of
  this file (`TestIOSDDLSnapshotsStateTheirProvenance` checks that every iOS schema snapshot
  without a public source is named here).

## Areas

| Area | Covers |
|---|---|
| [case](limitations/case.md) | Derived-artifact cycles in `case verify`, verify cancel, artifact files on Windows, the manifest cache |
| [acquisition](limitations/acquisition.md) | Android sync and hostile devices, iOS backup, AFC and lockdown, hardware validation |
| [images](limitations/images.md) | Image analysis, E01 sets, `image info --verify`, extract guards, encrypted content |
| [filesystems](limitations/filesystems.md) | Limitations every filesystem reader shares: deleted listings, warnings, truncated images, the `File.Runs` prefix rule, fixtures |
| [ext4](limitations/ext4.md) | The ext2/3/4 reader |
| [fat](limitations/fat.md) | The FAT12/16/32 and exFAT readers |
| [f2fs](limitations/f2fs.md) | The F2FS reader |
| [hfsplus](limitations/hfsplus.md) | The HFS+ and HFSX reader |
| [apfs](limitations/apfs.md) | The APFS reader |
| [recovery](limitations/recovery.md) | Deleted-file recovery on every filesystem, carving, slack, the recovery checks of `case verify` |
| [sqlite](limitations/sqlite.md) | The SQLite file library, the table decoder, typed and `db_row` record recovery |
| [records](limitations/records.md) | The records database, full-text search, JSON output, scale and performance |
| [parsers](limitations/parsers.md) | The parse host, parser isolation, the decoders (including the TL decoder `decode/tl`), the Android (telephony, WhatsApp, calendar, Telegram and Google Messages), browser and iOS (telephony, WhatsApp and calendar) parsers, real-data runs |

## Formats written from memory

Where no public source was available, a layout was written from memory. Each such format is
named here; what is from memory, and how far it is checked, is in the item the last column names.

| Format | Code | Item |
|---|---|---|
| The iOS databases `sms.db`, `CallHistory.storedata`, `call_history.db`, `AddressBook.sqlitedb`, `AddressBookImages.sqlitedb` and `Manifest.db`, and their nine schema snapshots `backup-manifest.sql`, `sms-ios17.sql`, `sms-ios14.sql`, `sms-ios10.sql`, `callhistory-coredata.sql`, `callhistory-legacy.sql`, `addressbook-current.sql`, `addressbook-older.sql` and `addressbookimages.sql` | `internal/parsers/iossms`, `ioscalls`, `iosaddressbook`, `internal/artparse/backup.go`, `tools/fixtures/schemas/ios/` | [parsers](limitations/parsers.md): "iOS schemas and backup formats are from memory", "iOS backup mapping" |
| Safari `History.db`, the Chromium package-to-browser table | `internal/parsers/safari`, `internal/parsers/chromium` | [parsers](limitations/parsers.md): "Browser history parsers" |
| The legacy typedstream archive | `internal/decode/typedstream` | [parsers](limitations/parsers.md): "Decoders" |
| Carving layouts: JPEG, PNG, GIF, RIFF/WebP, WAV, Ogg, gzip, BMP, ICO and CUR, TIFF and camera raw, ISO-BMFF, the JPEG XL container, SQLite, binary plist, ZIP and PDF | `internal/carve/validate_*.go` | [recovery](limitations/recovery.md): the "Carving, …" items |
| F2FS deleted-file recovery (the node scan) | `internal/filesys/f2fs/recover*.go` | [recovery](limitations/recovery.md): "F2FS deleted-file recovery" |
| F2FS roll-forward (the warm-node chain and the replay rules) | `internal/filesys/f2fs/rollforward*.go` | [recovery](limitations/recovery.md): "F2FS roll-forward" |
| The ext4 jbd2 journal (superblock, block types, tag formats, checksums, the scan and revoke rules) | `internal/filesys/ext4/journal*.go`, `recover_journal.go` | [recovery](limitations/recovery.md): "ext4 journal (plan 3G)" |
| The Android sync v2 wire | `internal/android/adb` | [acquisition](limitations/acquisition.md): "Android sync" |
| The AFC wire, the mobilebackup2 end marker, the `Manifest.db` layout read during a backup | `internal/ios/afc`, `internal/ios/mb2`, `internal/ios/manifestdb` | [acquisition](limitations/acquisition.md): "iOS AFC", "iOS backup transfer" |
| The HFS+ journal header, the ignorable code units, the directory-link markers | `internal/filesys/hfsplus` | [hfsplus](limitations/hfsplus.md) |
| The APFS decmpfs type numbers, the Unicode 9.0 name-hash cutoff, `apfs_expect.sh` | `internal/filesys/apfs`, `tools/fixtures/apfs_expect.sh` | [apfs](limitations/apfs.md) |
| The WhatsApp `wa.db` table `wa_contacts` (`wa-2.11.sql`, `wa-current.sql`: no DDL of `wa.db` exists in any source, and the four stub tables of `wa-current.sql` name one column each), the WhatsApp Business external folder name `WhatsApp Business`, and the Android 11+ media location `Android/media/<package>/WhatsApp` | `internal/parsers/androidwacontacts`, `internal/parsers/androidwhatsapp/attach.go`, `tools/fixtures/schemas/whatsapp-android/` | [parsers](limitations/parsers.md): "WhatsApp parsers (plan 4I)" |
| The iOS WhatsApp Core Data tables (`chatstorage.sql`, `callhistory.sql`, `contactsv2.sql`: no DDL of any iOS WhatsApp table exists in any source, and the column types are not checked against a public definition), the `Z_PRIMARYKEY` entity names (`WAMessage`, `WAChatSession`, and the other entities by the Core Data naming convention), the `Z1CALLEVENTS` column of `ZWACDCALLEVENT` that names a call's aggregate (the spec's name: another `Z<n>CALLEVENTS` is `no-aggregate-column`), the container metadata key `MCMMetadataIdentifier` with its two identifiers (`group.net.whatsapp.WhatsApp.shared`, `group.net.whatsapp.WhatsAppSMB.shared`), and the WhatsApp Business backup domain (a single source, research 2.2) | `internal/parsers/ioswhatsapp`, `internal/parsers/ioswacalls`, `internal/parsers/ioswacontacts`, `tools/fixtures/schemas/whatsapp-ios/` | [parsers](limitations/parsers.md): "WhatsApp iOS parsers (plan 4I2)" |
| The iOS calendar store `Calendar.sqlitedb` (plan 4I3; no public definition exists): its tables and columns, the `entity_type` values 2 (event) and 3 (task), the floating-time marker `_float`, the storage of every date as Cocoa seconds, the all-day convention (a date stored at a UTC midnight), the join columns `location_id`, `organizer_id`, `item_id`, `identity_id`, `owner_id`, `calendaritem_owner_id` and `orig_item_id`, and the snapshot `calendar.sql` | `internal/parsers/ioscalendar`, `tools/fixtures/schemas/calendar-ios/` | [parsers](limitations/parsers.md): "Calendar parsers (plan 4I3)" |
| The Google Messages `bugle_db` beyond the AOSP core, and its snapshot `proprietary-shaped.sql` | `internal/parsers/androidgooglemessages` | [parsers](limitations/parsers.md): "Google Messages parser" |

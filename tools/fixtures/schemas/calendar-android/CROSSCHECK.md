# Calendar provider DDL cross-check (plan 4I3)

The Android `calendar.db` snapshots of this directory, checked by `tools/fixtures/ddl_crosscheck.py --family calendar`
against the public AOSP sources, one run per tag (each run checks only the snapshots of its tag, `--snapshot`). The
fetched files were held in scratch only and are not in the repository; each is pinned below by repository, commit (the
peeled tag, `git ls-remote <repo> refs/tags/<tag>^{}`) and SHA-256. Sources are untrusted text: the script reads them
with regular expressions and never runs them.

Files and their repository paths (base names as in the reports below):

- `CalendarDatabaseHelper.java`, `CalendarProvider2.java`, `CalendarInstancesHelper.java`, `CalendarCache.java`:
  platform/packages/providers/CalendarProvider, `src/com/android/providers/calendar/`
- `CalendarContract.java`, `BaseColumns.java`, `SyncStateContract.java`: platform/frameworks/base, `core/java/android/provider/`
- `Duration.java`: platform/frameworks/opt/calendar, `src/com/android/calendarcommon2/`

The seven files spec §2 pins were fetched again and their SHA-256 equal the spec's. The frameworks/base commit at
android-14.0.0_r1 is `299fe6f5d6fc6f1af7c3411dcf4e5efdf7217368`, equal to the plan 4D pin. Pinned here for the first
time: `CalendarDatabaseHelper.java` at android-4.2_r1, `CalendarContract.java` at each tag, and three small constant
files the script needs to evaluate the bootstrap's concatenations without falling back to older upgrade-code text:
`BaseColumns.java` (`_ID`), `SyncStateContract.java` (the `Colors` account columns) and `CalendarCache.java` (the
`CalendarCache` column names).

Rule of the calendar helper (`--family calendar`): it keeps every historical `CREATE TABLE` in a method named after its
version (`createCalendarsTable303` comes before `createCalendarsTable` in the file) or in upgrade code
(`upgradeToVersion61`), so the definition at the tag is the first `CREATE TABLE` outside such a method. The plain
first-in-file rule of plan 4D would have taken the version-303 `Calendars`.

## Tables of each snapshot

Every snapshot holds the eleven tables the bootstrap (`bootstrapDB`) creates at its tag: `Colors`, `Calendars`,
`Events`, `EventsRawTimes`, `Instances`, `CalendarMetaData`, `CalendarCache`, `Attendees`, `Reminders`,
`CalendarAlerts`, `ExtendedProperties` (all eleven exist at every tag). Not in any snapshot: the view `view_events`,
the triggers and the indexes (the parser reads tables only); and `_sync_state` and `_sync_state_metadata` (ruling F06):
`CalendarDatabaseHelper.java` does not define them at any of the four tags. It names them
(`CalendarDatabaseHelper.java:152-153` at android-14.0.0_r1) and has the framework's `SyncStateContentProviderHelper`
create them (`mSyncState.createDatabase(db)`, line 391), so no extra source file is pinned for them and the parser
only names them in `unmapped_tables`.

| Snapshot | Tag of the CREATE TABLE text | Tag of the ALTERs | `user_version` | Verdict |
|---|---|---|---|---|
| `601.sql` | android-14.0.0_r1 | | 601 | verified |
| `600.sql` | android-14.0.0_r1; the same text verified at android-9.0.0_r1 | | 600 | verified |
| `502.sql` | android-4.2_r1 | | 502 | verified |
| `403.sql` | android-4.1.1_r1 | | 403 | verified |
| `403-upgraded-601.sql` | android-4.1.1_r1 (the text of `403.sql`, verified in the android-4.1.1_r1 run below) | android-14.0.0_r1 (its one `-- tag` line) | 601 | historical |

Column differences between the bootstraps: `Events` gains `isOrganizer` (501), `uid2445` (502) and `mutators` (600);
`Calendars` gains `isPrimary` (501) and `mutators` (600). In a fresh bootstrap they sit in their place in the column
list; in the upgraded database they are appended by `ALTER TABLE ... ADD COLUMN`, so rows written before the upgrade
are short records.

## Facts the parser and the oracle rely on (spec §2), at android-14.0.0_r1 unless stated

- `DATABASE_VERSION`: 601 at `CalendarDatabaseHelper.java:77` (android-14.0.0_r1); 600 at `CalendarDatabaseHelper.java:77`
  (android-9.0.0_r1); 502 at `CalendarDatabaseHelper.java:76` (android-4.2_r1); 403 at `CalendarDatabaseHelper.java:76`
  (android-4.1.1_r1). Version 601 changed no table: "There are no table changes in 601, but recreating the events view
  is required", `CalendarDatabaseHelper.java:1415-1416`. The helper's comment "6xx db version is for K release"
  (`CalendarDatabaseHelper.java:1493`) disagrees with the tags (600 already at android-9.0.0_r1 and, per spec §2, from
  android-4.3_r1); the fixtures cite the tags.
- Upgrades (`CalendarDatabaseHelper.java:1400-1411`): `upgradeToVersion501` adds `Events.isOrganizer` and
  `Calendars.isPrimary` (`:1515-1516`); `upgradeToVersion502` adds `Events.uid2445` (`:1524`); `upgradeToVersion600`
  adds `Events.mutators` and `Calendars.mutators` (`:1501-1502`).
- `Events._id` is `INTEGER PRIMARY KEY AUTOINCREMENT` (`CalendarDatabaseHelper.java:523`); `Events.organizer` is
  declared `STRING` (`:564`), which has NUMERIC affinity in SQLite.
- `dtstart` and `dtend` are "UTC millis since epoch" (`CalendarContract.java:1146`, `:1152`); `originalInstanceTime`
  and `lastDate` are "millis since epoch" (`CalendarContract.java:1287`, `:1301`); `duration` is RFC 2445 text
  (`CalendarContract.java:1158-1161`).
- All-day rule: "If allDay is set to 1 eventTimezone must be "UTC" and the time must correspond to a midnight
  boundary", `CalendarContract.java:1684-1685`. Insert contract: `dtend` if non-recurring, `duration` and
  `rrule`/`rdate` if recurring, `CalendarContract.java:1645-1651`. The instance expander forces UTC for all-day events
  ("in the events table, allDay events start at midnight", `CalendarInstancesHelper.java:209-216`, and `:334-335`).
- End rule: a recurring event's instance ends at `begin + duration.getMillis()` (`CalendarInstancesHelper.java:340-347`);
  a non-recurring event ends at `dtstart + duration` when `duration` is stored, else at `dtend`, else at `dtstart`
  (`CalendarInstancesHelper.java:372-379`). `Duration.getMillis()` counts a week as 7·24·60·60 and a day as 24·60·60
  seconds in an `int` sum (`Duration.java:145-152`).
- Soft delete: an app delete of an event on the server sets `deleted = 1`, `dirty = 1` and a mutator ("we "soft
  delete", i.e. mark as deleted so that the sync adapter has a chance to tell the server about the deletion",
  `CalendarProvider2.java:3538-3545`); `SyncColumns.deleted`: "A deleted row should be ignored"
  (`CalendarContract.java:391-397`). A hard delete fires `events_cleanup_delete` (`CalendarDatabaseHelper.java:500`),
  whose SQL (`EVENTS_CLEANUP_TRIGGER_SQL`, `:165`) deletes the event's `Instances`, `EventsRawTimes`, `Attendees`,
  `Reminders`, `CalendarAlerts` and `ExtendedProperties` rows.
- `duplicateEvent` (`CalendarDatabaseHelper.java:3375`): for a calendar with `canPartiallyUpdate`, before an app
  edits a synced (`dirty = 0`) event, the provider inserts a copy with `dirty = 0, lastSynced = 1`
  (`SELECT ..., 0, 1`, `:3386-3389`); the provider's app queries add `lastSynced = 0` (`CalendarProvider2.java:4528`).

## Enum constants (`CalendarContract.java` at android-14.0.0_r1)

| Column | Value | Label | Constant | Line |
|---|---|---|---|---|
| `eventStatus` | 0 | tentative | `STATUS_TENTATIVE` | CalendarContract.java:1060 |
| `eventStatus` | 1 | confirmed | `STATUS_CONFIRMED` | CalendarContract.java:1061 |
| `eventStatus` | 2 | cancelled | `STATUS_CANCELED` | CalendarContract.java:1062 |
| `accessLevel` | 0 | default | `ACCESS_DEFAULT` | CalendarContract.java:1192 |
| `accessLevel` | 1 | confidential | `ACCESS_CONFIDENTIAL` | CalendarContract.java:1196 |
| `accessLevel` | 2 | private | `ACCESS_PRIVATE` | CalendarContract.java:1200 |
| `accessLevel` | 3 | public | `ACCESS_PUBLIC` | CalendarContract.java:1205 |
| `availability` | 0 | busy | `AVAILABILITY_BUSY` | CalendarContract.java:1221 |
| `availability` | 1 | free | `AVAILABILITY_FREE` | CalendarContract.java:1226 |
| `availability` | 2 | tentative | `AVAILABILITY_TENTATIVE` | CalendarContract.java:1231 |
| `attendeeRelationship` | 0 | none | `RELATIONSHIP_NONE` | CalendarContract.java:891 |
| `attendeeRelationship` | 1 | attendee | `RELATIONSHIP_ATTENDEE` | CalendarContract.java:892 |
| `attendeeRelationship` | 2 | organizer | `RELATIONSHIP_ORGANIZER` | CalendarContract.java:893 |
| `attendeeRelationship` | 3 | performer | `RELATIONSHIP_PERFORMER` | CalendarContract.java:894 |
| `attendeeRelationship` | 4 | speaker | `RELATIONSHIP_SPEAKER` | CalendarContract.java:895 |
| `attendeeType` | 0 | none | `TYPE_NONE` | CalendarContract.java:906 |
| `attendeeType` | 1 | required | `TYPE_REQUIRED` | CalendarContract.java:907 |
| `attendeeType` | 2 | optional | `TYPE_OPTIONAL` | CalendarContract.java:908 |
| `attendeeType` | 3 | resource | `TYPE_RESOURCE` | CalendarContract.java:913 |
| `attendeeStatus`, `selfAttendeeStatus` | 0 | none | `ATTENDEE_STATUS_NONE` | CalendarContract.java:921 |
| `attendeeStatus`, `selfAttendeeStatus` | 1 | accepted | `ATTENDEE_STATUS_ACCEPTED` | CalendarContract.java:922 |
| `attendeeStatus`, `selfAttendeeStatus` | 2 | declined | `ATTENDEE_STATUS_DECLINED` | CalendarContract.java:923 |
| `attendeeStatus`, `selfAttendeeStatus` | 3 | invited | `ATTENDEE_STATUS_INVITED` | CalendarContract.java:924 |
| `attendeeStatus`, `selfAttendeeStatus` | 4 | tentative | `ATTENDEE_STATUS_TENTATIVE` | CalendarContract.java:925 |
| `Reminders.minutes` | -1 | default | `MINUTES_DEFAULT` | CalendarContract.java:2289 |
| `Reminders.method` | 0 | default | `METHOD_DEFAULT` | CalendarContract.java:2301 |
| `Reminders.method` | 1 | alert | `METHOD_ALERT` | CalendarContract.java:2302 |
| `Reminders.method` | 2 | email | `METHOD_EMAIL` | CalendarContract.java:2303 |
| `Reminders.method` | 3 | sms | `METHOD_SMS` | CalendarContract.java:2304 |
| `Reminders.method` | 4 | alarm | `METHOD_ALARM` | CalendarContract.java:2305 |
| `CalendarAlerts.state` | 0 | scheduled | `STATE_SCHEDULED` | CalendarContract.java:2405 |
| `CalendarAlerts.state` | 1 | fired | `STATE_FIRED` | CalendarContract.java:2410 |
| `CalendarAlerts.state` | 2 | dismissed | `STATE_DISMISSED` | CalendarContract.java:2415 |

`selfAttendeeStatus` takes the `ATTENDEE_STATUS_*` values (`CalendarContract.java:1072`). The payload spells status 2
`cancelled` (the contract's enum); the constant is `STATUS_CANCELED`.

## Run at android-14.0.0_r1 (601.sql and 600.sql verified, 403-upgraded-601.sql historical)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `android-14.0.0_r1`.

- platform/packages/providers/CalendarProvider commit `94405b4ad2ca9ddec5fcad85d42cd308229b61f0`
- platform/frameworks/base commit `299fe6f5d6fc6f1af7c3411dcf4e5efdf7217368`
- platform/frameworks/opt/calendar commit `c522b14186560a2faa5e846371cb97a038a0de13`

Fetched files (not in the repository), SHA-256:

- BaseColumns.java `e235c07ba8d869a169ea67591b74cf51a4521ca7162a8cfe3de29bcd21371b33`
- CalendarCache.java `203311cc2a5397e6a40c7721289e4d0e6c0ed64a1dd83e1b40927527bf18b0af`
- CalendarContract.java `583f650ea70d3aa53f7a5beb1c6d5ed5d929c372ccd37bbe4f96f6ffd005ae8a`
- CalendarDatabaseHelper.java `c7c33b87650be8f3e4c2e881e737abe9cddd23d194695711e05f26476cf39a3c`
- CalendarInstancesHelper.java `04b9f6d643172c2d47b7245a806fa0bd48f3befe4d1ea8922fc04bdd3fcdb3e0`
- CalendarProvider2.java `37c13f9830df96df538f6dfd4b21d3369b7b1bdeeb8f1ce4d59d6012cfad3dd7`
- Duration.java `395135e614a724103bfe6d4bfbfc462ea88d03f7f82425b554e2e1066eb9afca`
- SyncStateContract.java `6277c962d1d3ca7e81a8e0edfe199cce2df0ea80043c21efbf9abcc010e19622`

The definition at the tag is the first CREATE TABLE of the table in the file outside a method named after a version (create...Table303, upgradeToVersion...); statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| 403-upgraded-601.sql | Events.isOrganizer | historical | ALTER TABLE ADD COLUMN at CalendarDatabaseHelper.java:1515 |
| 403-upgraded-601.sql | Calendars.isPrimary | historical | ALTER TABLE ADD COLUMN at CalendarDatabaseHelper.java:1516 |
| 403-upgraded-601.sql | Events.uid2445 | historical | ALTER TABLE ADD COLUMN at CalendarDatabaseHelper.java:1524 |
| 403-upgraded-601.sql | Events.mutators | historical | ALTER TABLE ADD COLUMN at CalendarDatabaseHelper.java:1501 |
| 403-upgraded-601.sql | Calendars.mutators | historical | ALTER TABLE ADD COLUMN at CalendarDatabaseHelper.java:1502 |
| 403-upgraded-601.sql | Attendees | historical | 9 columns, all defined at CalendarDatabaseHelper.java:432 |
| 403-upgraded-601.sql | CalendarAlerts | historical | 11 columns, all defined at CalendarDatabaseHelper.java:461 |
| 403-upgraded-601.sql | CalendarCache | historical | 3 columns, all defined at CalendarDatabaseHelper.java:1018 |
| 403-upgraded-601.sql | CalendarMetaData | historical | 4 columns, all defined at CalendarDatabaseHelper.java:995 |
| 403-upgraded-601.sql | Calendars | historical | 33 columns, all defined at CalendarDatabaseHelper.java:766 |
| 403-upgraded-601.sql | Colors | historical | 7 columns, all defined at CalendarDatabaseHelper.java:749 |
| 403-upgraded-601.sql | Events | historical | 49 columns, all defined at CalendarDatabaseHelper.java:522 |
| 403-upgraded-601.sql | EventsRawTimes | historical | 7 columns, all defined at CalendarDatabaseHelper.java:399 |
| 403-upgraded-601.sql | ExtendedProperties | historical | 4 columns, all defined at CalendarDatabaseHelper.java:485 |
| 403-upgraded-601.sql | Instances | historical | 9 columns, all defined at CalendarDatabaseHelper.java:409 |
| 403-upgraded-601.sql | Reminders | historical | 4 columns, all defined at CalendarDatabaseHelper.java:448 |
| 600.sql | Attendees | verified | CalendarDatabaseHelper.java:432, 9 columns, names, order and definitions equal |
| 600.sql | CalendarAlerts | verified | CalendarDatabaseHelper.java:461, 11 columns, names, order and definitions equal |
| 600.sql | CalendarCache | verified | CalendarDatabaseHelper.java:1018, 3 columns, names, order and definitions equal |
| 600.sql | CalendarMetaData | verified | CalendarDatabaseHelper.java:995, 4 columns, names, order and definitions equal |
| 600.sql | Calendars | verified | CalendarDatabaseHelper.java:766, 35 columns, names, order and definitions equal |
| 600.sql | Colors | verified | CalendarDatabaseHelper.java:749, 7 columns, names, order and definitions equal |
| 600.sql | Events | verified | CalendarDatabaseHelper.java:522, 52 columns, names, order and definitions equal |
| 600.sql | EventsRawTimes | verified | CalendarDatabaseHelper.java:399, 7 columns, names, order and definitions equal |
| 600.sql | ExtendedProperties | verified | CalendarDatabaseHelper.java:485, 4 columns, names, order and definitions equal |
| 600.sql | Instances | verified | CalendarDatabaseHelper.java:409, 9 columns, names, order and definitions equal |
| 600.sql | Reminders | verified | CalendarDatabaseHelper.java:448, 4 columns, names, order and definitions equal |
| 601.sql | Attendees | verified | CalendarDatabaseHelper.java:432, 9 columns, names, order and definitions equal |
| 601.sql | CalendarAlerts | verified | CalendarDatabaseHelper.java:461, 11 columns, names, order and definitions equal |
| 601.sql | CalendarCache | verified | CalendarDatabaseHelper.java:1018, 3 columns, names, order and definitions equal |
| 601.sql | CalendarMetaData | verified | CalendarDatabaseHelper.java:995, 4 columns, names, order and definitions equal |
| 601.sql | Calendars | verified | CalendarDatabaseHelper.java:766, 35 columns, names, order and definitions equal |
| 601.sql | Colors | verified | CalendarDatabaseHelper.java:749, 7 columns, names, order and definitions equal |
| 601.sql | Events | verified | CalendarDatabaseHelper.java:522, 52 columns, names, order and definitions equal |
| 601.sql | EventsRawTimes | verified | CalendarDatabaseHelper.java:399, 7 columns, names, order and definitions equal |
| 601.sql | ExtendedProperties | verified | CalendarDatabaseHelper.java:485, 4 columns, names, order and definitions equal |
| 601.sql | Instances | verified | CalendarDatabaseHelper.java:409, 9 columns, names, order and definitions equal |
| 601.sql | Reminders | verified | CalendarDatabaseHelper.java:448, 4 columns, names, order and definitions equal |
## Run at android-9.0.0_r1 (600.sql verified: the bootstrap text is identical at android-9.0.0_r1 and android-14.0.0_r1)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `android-9.0.0_r1`.

- platform/packages/providers/CalendarProvider commit `3cb31b1bf2c8bda809db0864dd9f31a42ac64582`
- platform/frameworks/base commit `6549309f6c473b792ec62d1aebadec62bcf07827`

Fetched files (not in the repository), SHA-256:

- BaseColumns.java `bf11ca5fd1b7e86dba93e6df2aefbbb26a7e9220f50a5cc5528fcc6b50896e46`
- CalendarCache.java `203311cc2a5397e6a40c7721289e4d0e6c0ed64a1dd83e1b40927527bf18b0af`
- CalendarContract.java `9c8883b0399399dcf624aad329b230a3e4e6bbc1b6c8c5e495ebb6563de7fb6b`
- CalendarDatabaseHelper.java `1c9ddb0d8b36d8f889945b3dccd1f78fd224e97ade6c1116ad46aa1fd0cf0663`
- SyncStateContract.java `6277c962d1d3ca7e81a8e0edfe199cce2df0ea80043c21efbf9abcc010e19622`

The definition at the tag is the first CREATE TABLE of the table in the file outside a method named after a version (create...Table303, upgradeToVersion...); statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| 600.sql | Attendees | verified | CalendarDatabaseHelper.java:432, 9 columns, names, order and definitions equal |
| 600.sql | CalendarAlerts | verified | CalendarDatabaseHelper.java:461, 11 columns, names, order and definitions equal |
| 600.sql | CalendarCache | verified | CalendarDatabaseHelper.java:1018, 3 columns, names, order and definitions equal |
| 600.sql | CalendarMetaData | verified | CalendarDatabaseHelper.java:995, 4 columns, names, order and definitions equal |
| 600.sql | Calendars | verified | CalendarDatabaseHelper.java:766, 35 columns, names, order and definitions equal |
| 600.sql | Colors | verified | CalendarDatabaseHelper.java:749, 7 columns, names, order and definitions equal |
| 600.sql | Events | verified | CalendarDatabaseHelper.java:522, 52 columns, names, order and definitions equal |
| 600.sql | EventsRawTimes | verified | CalendarDatabaseHelper.java:399, 7 columns, names, order and definitions equal |
| 600.sql | ExtendedProperties | verified | CalendarDatabaseHelper.java:485, 4 columns, names, order and definitions equal |
| 600.sql | Instances | verified | CalendarDatabaseHelper.java:409, 9 columns, names, order and definitions equal |
| 600.sql | Reminders | verified | CalendarDatabaseHelper.java:448, 4 columns, names, order and definitions equal |
## Run at android-4.2_r1 (502.sql verified)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `android-4.2_r1`.

- platform/packages/providers/CalendarProvider commit `49b7d3c4222a6b9e8e4639ba6d5128df5eac7e73`
- platform/frameworks/base commit `9a7debe5857ffc7c71cfc4082b1b6d72a5cf81cd`

Fetched files (not in the repository), SHA-256:

- BaseColumns.java `bf11ca5fd1b7e86dba93e6df2aefbbb26a7e9220f50a5cc5528fcc6b50896e46`
- CalendarCache.java `203311cc2a5397e6a40c7721289e4d0e6c0ed64a1dd83e1b40927527bf18b0af`
- CalendarContract.java `50bbba0d596ed8f902236ecf7b54e809d659e3c0b031611da78142530afc38bc`
- CalendarDatabaseHelper.java `0af101a1e6126a71139eef264cd04e4f6430bd75a357f93cde66eda2562cbbfd`
- SyncStateContract.java `6277c962d1d3ca7e81a8e0edfe199cce2df0ea80043c21efbf9abcc010e19622`

The definition at the tag is the first CREATE TABLE of the table in the file outside a method named after a version (create...Table303, upgradeToVersion...); statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| 502.sql | Attendees | verified | CalendarDatabaseHelper.java:431, 9 columns, names, order and definitions equal |
| 502.sql | CalendarAlerts | verified | CalendarDatabaseHelper.java:460, 11 columns, names, order and definitions equal |
| 502.sql | CalendarCache | verified | CalendarDatabaseHelper.java:1015, 3 columns, names, order and definitions equal |
| 502.sql | CalendarMetaData | verified | CalendarDatabaseHelper.java:992, 4 columns, names, order and definitions equal |
| 502.sql | Calendars | verified | CalendarDatabaseHelper.java:764, 34 columns, names, order and definitions equal |
| 502.sql | Colors | verified | CalendarDatabaseHelper.java:747, 7 columns, names, order and definitions equal |
| 502.sql | Events | verified | CalendarDatabaseHelper.java:521, 51 columns, names, order and definitions equal |
| 502.sql | EventsRawTimes | verified | CalendarDatabaseHelper.java:398, 7 columns, names, order and definitions equal |
| 502.sql | ExtendedProperties | verified | CalendarDatabaseHelper.java:484, 4 columns, names, order and definitions equal |
| 502.sql | Instances | verified | CalendarDatabaseHelper.java:408, 9 columns, names, order and definitions equal |
| 502.sql | Reminders | verified | CalendarDatabaseHelper.java:447, 4 columns, names, order and definitions equal |
## Run at android-4.1.1_r1 (403.sql verified, which is also the CREATE TABLE text of 403-upgraded-601.sql)

Generated by tools/fixtures/ddl_crosscheck.py. Tag: `android-4.1.1_r1`.

- platform/packages/providers/CalendarProvider commit `35225e0167fee2675cabfffb94c6f9e4b679db20`
- platform/frameworks/base commit `c292a619c71a2ee56e42ca5ef45fbc62e5c09f1e`

Fetched files (not in the repository), SHA-256:

- BaseColumns.java `bf11ca5fd1b7e86dba93e6df2aefbbb26a7e9220f50a5cc5528fcc6b50896e46`
- CalendarCache.java `203311cc2a5397e6a40c7721289e4d0e6c0ed64a1dd83e1b40927527bf18b0af`
- CalendarContract.java `21eb5275c9ec55c371dd62fc262205ca9e80d13e0b340173c265cf8743f7cf2e`
- CalendarDatabaseHelper.java `23b4f9fa397203268ae4aa1ed2d9b0e292f3993419eab120cc80f4bab1a1e507`
- SyncStateContract.java `6277c962d1d3ca7e81a8e0edfe199cce2df0ea80043c21efbf9abcc010e19622`

The definition at the tag is the first CREATE TABLE of the table in the file outside a method named after a version (create...Table303, upgradeToVersion...); statements the script could not evaluate: 0.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| 403.sql | Attendees | verified | CalendarDatabaseHelper.java:429, 9 columns, names, order and definitions equal |
| 403.sql | CalendarAlerts | verified | CalendarDatabaseHelper.java:458, 11 columns, names, order and definitions equal |
| 403.sql | CalendarCache | verified | CalendarDatabaseHelper.java:1010, 3 columns, names, order and definitions equal |
| 403.sql | CalendarMetaData | verified | CalendarDatabaseHelper.java:987, 4 columns, names, order and definitions equal |
| 403.sql | Calendars | verified | CalendarDatabaseHelper.java:760, 33 columns, names, order and definitions equal |
| 403.sql | Colors | verified | CalendarDatabaseHelper.java:743, 7 columns, names, order and definitions equal |
| 403.sql | Events | verified | CalendarDatabaseHelper.java:519, 49 columns, names, order and definitions equal |
| 403.sql | EventsRawTimes | verified | CalendarDatabaseHelper.java:396, 7 columns, names, order and definitions equal |
| 403.sql | ExtendedProperties | verified | CalendarDatabaseHelper.java:482, 4 columns, names, order and definitions equal |
| 403.sql | Instances | verified | CalendarDatabaseHelper.java:406, 9 columns, names, order and definitions equal |
| 403.sql | Reminders | verified | CalendarDatabaseHelper.java:445, 4 columns, names, order and definitions equal |

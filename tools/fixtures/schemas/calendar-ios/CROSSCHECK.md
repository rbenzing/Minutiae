# DDL cross-check (plan 4I3, Task 7): iOS Calendar.sqlitedb

No public source defines the schema of the iOS calendar store `Calendar.sqlitedb` (spec ruling 3). The snapshot
`calendar.sql` is written from memory: its tables, columns, declared types, the join columns (`location_id`,
`organizer_id`, `item_id`, `identity_id`, `owner_id`, `calendaritem_owner_id`, `orig_item_id`), the `entity_type`
values 2 (event) and 3 (task), the floating-time marker `_float`, the storage of every date as Cocoa seconds and the
all-day convention (a date stored at a UTC midnight) are memory, not facts read from a definition. Nothing was fetched,
extracted or compared, no real device database was used, and no script cross-checks the snapshot: there is nothing to
compare it with. The snapshot states `-- from memory` and `-- verdict no-public-source` in its header, and the format
is listed in the from-memory table of `docs/limitations.md`.

The fixtures generated from it (`tools/fixtures/calendar_ios.py`) are validated only against the same memory, so a
fixture proves that the parser reads the remembered layout, never that a real database has it. A real database that
differs degrades through the parser's probe (`unsupported-schema` at the system path) or through absent optional
joins. The real-data run of plan 4I3 is required before relying on any of it.

## DDL verdicts

| File | Table | Verdict | Evidence |
|---|---|---|---|
| calendar.sql | CalendarItem | no-public-source | memory |
| calendar.sql | Location | no-public-source | memory |
| calendar.sql | Participant | no-public-source | memory |
| calendar.sql | Identity | no-public-source | memory |
| calendar.sql | Calendar | no-public-source | memory |
| calendar.sql | Store | no-public-source | memory |
| calendar.sql | Recurrence | no-public-source | memory |
| calendar.sql | ExceptionDate | no-public-source | memory |
| calendar.sql | Alarm | no-public-source | memory |
| calendar.sql | OccurrenceCache | no-public-source | memory |
| calendar.sql | CalendarItemChanges | no-public-source | memory |

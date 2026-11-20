# DDL cross-check (plan 4I3, Task 7): iOS Calendar.sqlitedb

No public source defines the schema of the iOS calendar store `Calendar.sqlitedb` (spec ruling 3). The snapshot
`calendar.sql` is written from memory: its tables, columns, declared types, the join columns (`location_id`,
`organizer_id`, `item_id`, `identity_id`, `owner_id`, `calendaritem_owner_id`, `orig_item_id`), the `entity_type`
values 2 (event) and 3 (task), the floating-time marker `_float`, the storage of every date as Cocoa seconds and the
all-day convention (a date stored at a UTC midnight) are memory, not facts read from a definition. When it was written nothing was fetched,
extracted or compared, and no script cross-checks the snapshot: there is nothing to compare it with. A real store was
read once afterwards (below). The snapshot states `-- from memory` and `-- verdict no-public-source` in its header, and the format
is listed in the from-memory table of `docs/limitations.md`.

The fixtures generated from it (`tools/fixtures/calendar_ios.py`) are validated only against the same memory, so a
fixture proves that the parser reads the remembered layout, never that a real database has it. A real database that
differs degrades through the parser's probe (`unsupported-schema` at the system path) or through absent optional
joins. The one real-data run of plan 4I3 (below) checked part of it and left the rest unanswered, so what it did not answer is still memory.

## The real-data run (plan 4I3, Task 12)

One real iOS 13.3.1 store was read (see "Real-data runs" in `docs/limitations/parsers.md`). Counts and names only are
recorded; the store is not kept. It was read after this snapshot was written, and it differs from it in three places:

- `Identity` declares no ROWID column; its rows are keyed by the implicit rowid. The snapshot declares one. The parser
  now keys a join table that declares none by its implicit rowid.
- `Participant` has `owner_id` and `entity_type` and no `item_id`, and the table was empty, so nothing shows what
  `owner_id` names. The snapshot keeps `item_id` (the remembered layout); the fixture variant
  `calendar-ios-participant-owner` has the real columns and pins that attendees stay join-undecided (B12).
- A key that names nothing is stored as 0, not NULL (`location_id`, `organizer_id`, `orig_item_id`,
  `self_attendee_id`), which the remembered layout did not say. The parser states such a key `zero-key` (B13).

What the run confirmed of the remembered layout: the columns of `CalendarItem`, `Location`, `Calendar`, `Store`,
`Recurrence`, `ExceptionDate`, `Alarm` and `OccurrenceCache`; every date stored as REAL Cocoa seconds; `entity_type`
2 for an event; the `_float` marker; all-day starts at a UTC midnight; and the joins `calendar_id`, `store_id`,
`location_id` and `Recurrence.owner_id`. What it left unanswered, because the store holds no row to show it:
`entity_type` 3, every `Participant`, `Identity` and `Alarm` code and join, `orig_date`, and deletion. Those, and
everything about a layout other than iOS 13.3.1, remain from memory. The verdicts below are unchanged: the run read a
store, it did not read a definition.

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

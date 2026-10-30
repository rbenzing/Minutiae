-- Legacy iOS call_history.db (before the Core Data store): the call table. Nothing here is checked against a public definition.
-- Source: memory (no public schema source; written from memory of iOS call_history.db databases of releases before the Core Data store, docs/limitations.md) (verdict no-public-source).
-- Per-table verdict: tools/fixtures/schemas/ios/CROSSCHECK.md (plan 4E, Task 4).
CREATE TABLE call (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, address TEXT, date INTEGER, duration INTEGER, flags INTEGER, id INTEGER, name TEXT, country_code TEXT, network_code TEXT, read INTEGER, assisted INTEGER, face_time_data BLOB, originalAddress TEXT);

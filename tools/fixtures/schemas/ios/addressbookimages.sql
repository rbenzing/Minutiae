-- iOS AddressBookImages.sqlitedb: thumbnail and full-size contact photos. Nothing here is checked against a public definition.
-- Source: memory (no public schema source; written from memory of iOS AddressBookImages.sqlitedb databases, docs/limitations.md) (verdict no-public-source).
-- Per-table verdict: tools/fixtures/schemas/ios/CROSSCHECK.md (plan 4E, Task 4).
CREATE TABLE ABThumbnailImage (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, record_id INTEGER, format INTEGER, derived_from_format INTEGER, data BLOB, UNIQUE(record_id, format));
CREATE TABLE ABFullSizeImage (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, record_id INTEGER, crop_x INTEGER, crop_y INTEGER, crop_width INTEGER, crop_height INTEGER, data BLOB, UNIQUE(record_id));

-- iOS backup Manifest.db (iOS 10 and later backups): Files and Properties tables. Nothing here is checked against a public definition.
-- Source: memory (no public schema source; written from memory of iOS 10+ backup Manifest.db files, docs/limitations.md) (verdict no-public-source).
-- Per-table verdict: tools/fixtures/schemas/ios/CROSSCHECK.md (plan 4E, Task 4).
CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB);
CREATE TABLE Properties (key TEXT PRIMARY KEY, value BLOB);

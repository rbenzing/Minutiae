-- iOS AddressBook.sqlitedb with stores, display names and GUIDs. Nothing here is checked against a public definition.
-- Source: memory (no public schema source; written from memory of recent iOS AddressBook.sqlitedb databases, docs/limitations.md) (verdict no-public-source).
-- Per-table verdict: tools/fixtures/schemas/ios/CROSSCHECK.md (plan 4E, Task 4).
CREATE TABLE ABPerson (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, First TEXT, Last TEXT, Middle TEXT, FirstPhonetic TEXT, MiddlePhonetic TEXT, LastPhonetic TEXT, Organization TEXT, Department TEXT, Note TEXT, Kind INTEGER, Birthday TEXT, JobTitle TEXT, Nickname TEXT, Prefix TEXT, Suffix TEXT, FirstSort TEXT, LastSort TEXT, CreationDate INTEGER, ModificationDate INTEGER, CompositeNameFallback TEXT, ExternalIdentifier TEXT, ExternalModificationTag TEXT, ExternalUUID TEXT, StoreID INTEGER, DisplayName TEXT, ExternalRepresentation BLOB, guid TEXT);
CREATE TABLE ABMultiValue (UID INTEGER PRIMARY KEY, record_id INTEGER, property INTEGER, identifier INTEGER, label INTEGER, value TEXT, guid TEXT);
CREATE TABLE ABMultiValueLabel (value TEXT, UNIQUE(value));
CREATE TABLE ABMultiValueEntry (parent_id INTEGER, key INTEGER, value TEXT, UNIQUE(parent_id, key));
CREATE TABLE ABMultiValueEntryKey (value TEXT, UNIQUE(value));
CREATE TABLE ABStore (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, Name TEXT, Type INTEGER, ConstraintsPath TEXT, ExternalModificationTag TEXT, ExternalSyncTag TEXT, AccountID INTEGER, Enabled INTEGER);
CREATE TABLE ABGroup (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, Name TEXT, StoreID INTEGER);
CREATE TABLE ABGroupMembers (UID INTEGER PRIMARY KEY, group_id INTEGER, member_type INTEGER, member_id INTEGER, UNIQUE(group_id, member_type, member_id));

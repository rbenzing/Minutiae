-- iOS Calendar.sqlitedb (HomeDomain Library/Calendar), the tables ios.calendar reads and the tables it only counts (plan 4I3, spec 8, ruling 3).
-- from memory
-- verdict no-public-source
-- No public definition of this database is known: no table, column, declared type, join column or code value here is checked against a public definition, and no real database was read to write it.
-- The declared types (REAL Cocoa seconds for every date, INTEGER for the keys and codes, TEXT for zones and texts, BLOB for external_rep) are the writer's memory too.
-- Per-table verdict: tools/fixtures/schemas/calendar-ios/CROSSCHECK.md; the from-memory facts are listed in docs/limitations.md.
CREATE TABLE CalendarItem (ROWID INTEGER PRIMARY KEY, summary TEXT, location_id INTEGER, description TEXT, start_date REAL, start_tz TEXT, end_date REAL, end_tz TEXT, all_day INTEGER, calendar_id INTEGER, orig_item_id INTEGER, orig_date REAL, organizer_id INTEGER, status INTEGER, availability INTEGER, privacy_level INTEGER, url TEXT, creation_date REAL, last_modified REAL, sequence_num INTEGER, birthday_id INTEGER, external_id TEXT, unique_identifier TEXT, external_rep BLOB, hidden INTEGER, has_recurrences INTEGER, has_attendees INTEGER, invitation_status INTEGER, entity_type INTEGER);
CREATE TABLE Location (ROWID INTEGER PRIMARY KEY, title TEXT, address TEXT, latitude REAL, longitude REAL);
CREATE TABLE Participant (ROWID INTEGER PRIMARY KEY, item_id INTEGER, identity_id INTEGER, email TEXT, role INTEGER, type INTEGER, status INTEGER);
CREATE TABLE Identity (ROWID INTEGER PRIMARY KEY, display_name TEXT, address TEXT);
CREATE TABLE Calendar (ROWID INTEGER PRIMARY KEY, title TEXT, store_id INTEGER);
CREATE TABLE Store (ROWID INTEGER PRIMARY KEY, name TEXT, type INTEGER);
CREATE TABLE Recurrence (ROWID INTEGER PRIMARY KEY, owner_id INTEGER, frequency INTEGER, interval INTEGER, count INTEGER, end_date REAL, specifier TEXT);
CREATE TABLE ExceptionDate (ROWID INTEGER PRIMARY KEY, owner_id INTEGER, date REAL);
CREATE TABLE Alarm (ROWID INTEGER PRIMARY KEY, calendaritem_owner_id INTEGER, trigger_interval INTEGER, trigger_date REAL, type INTEGER);
CREATE TABLE OccurrenceCache (day REAL, event_id INTEGER, occurrence_date REAL);
CREATE TABLE CalendarItemChanges (ROWID INTEGER PRIMARY KEY, record INTEGER, type INTEGER, calendar_id INTEGER);

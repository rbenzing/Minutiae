-- Android call log (calllog.db), early schema without presentation, features or subscription columns.
-- Source: AOSP platform/packages/providers/ContactsProvider, tag android-14.0.0_r1, src/com/android/providers/contacts/CallLogDatabaseHelper.java; the columns added by its upgrade steps (verdict historical).
-- Per-table file:line and verdict: tools/fixtures/schemas/android-telephony/CROSSCHECK.md (Task 4b).
CREATE TABLE calls (_id INTEGER PRIMARY KEY AUTOINCREMENT, number TEXT, date INTEGER, duration INTEGER, type INTEGER, new INTEGER, name TEXT, numbertype INTEGER, numberlabel TEXT);

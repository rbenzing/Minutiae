-- Android calendar provider (com.android.providers.calendar), calendar.db: a version-403 database upgraded in place to 601.
-- CREATE TABLE text: the android-4.1.1_r1 bootstrap, exactly as 403.sql (CROSSCHECK.md cites android-4.1.1_r1 for it).
-- ALTER TABLE text: upgradeToVersion501, upgradeToVersion502 and upgradeToVersion600 of CalendarDatabaseHelper.java at the tag below
-- (the upgrades 403 -> 501 -> 502 -> 600; 601 changed no table), so the added columns come last and rows written before
-- the upgrade are short records. Source: AOSP platform/packages/providers/CalendarProvider and platform/frameworks/base.
-- tag android-14.0.0_r1
-- verdict historical
-- user_version 601
-- Not in this snapshot: the view, triggers and indexes (the parser reads tables only), and _sync_state and
-- _sync_state_metadata, which CalendarDatabaseHelper.java does not define (the framework's SyncStateContentProviderHelper creates them).
-- The generator inserts the pre-upgrade rows between the CREATE TABLE statements and the ALTERs (the @phase marker).
CREATE TABLE Colors (_id INTEGER PRIMARY KEY, account_name TEXT NOT NULL, account_type TEXT NOT NULL, data TEXT, color_type INTEGER NOT NULL, color_index TEXT NOT NULL, color INTEGER NOT NULL);
CREATE TABLE Calendars (_id INTEGER PRIMARY KEY, account_name TEXT, account_type TEXT, _sync_id TEXT, dirty INTEGER, name TEXT, calendar_displayName TEXT, calendar_color INTEGER, calendar_color_index TEXT, calendar_access_level INTEGER, visible INTEGER NOT NULL DEFAULT 1, sync_events INTEGER NOT NULL DEFAULT 0, calendar_location TEXT, calendar_timezone TEXT, ownerAccount TEXT, canOrganizerRespond INTEGER NOT NULL DEFAULT 1, canModifyTimeZone INTEGER DEFAULT 1, canPartiallyUpdate INTEGER DEFAULT 0, maxReminders INTEGER DEFAULT 5, allowedReminders TEXT DEFAULT '0,1', allowedAvailability TEXT DEFAULT '0,1', allowedAttendeeTypes TEXT DEFAULT '0,1,2', deleted INTEGER NOT NULL DEFAULT 0, cal_sync1 TEXT, cal_sync2 TEXT, cal_sync3 TEXT, cal_sync4 TEXT, cal_sync5 TEXT, cal_sync6 TEXT, cal_sync7 TEXT, cal_sync8 TEXT, cal_sync9 TEXT, cal_sync10 TEXT);
CREATE TABLE Events (_id INTEGER PRIMARY KEY AUTOINCREMENT, _sync_id TEXT, dirty INTEGER, lastSynced INTEGER DEFAULT 0, calendar_id INTEGER NOT NULL, title TEXT, eventLocation TEXT, description TEXT, eventColor INTEGER, eventColor_index TEXT, eventStatus INTEGER, selfAttendeeStatus INTEGER NOT NULL DEFAULT 0, dtstart INTEGER, dtend INTEGER, eventTimezone TEXT, duration TEXT, allDay INTEGER NOT NULL DEFAULT 0, accessLevel INTEGER NOT NULL DEFAULT 0, availability INTEGER NOT NULL DEFAULT 0, hasAlarm INTEGER NOT NULL DEFAULT 0, hasExtendedProperties INTEGER NOT NULL DEFAULT 0, rrule TEXT, rdate TEXT, exrule TEXT, exdate TEXT, original_id INTEGER, original_sync_id TEXT, originalInstanceTime INTEGER, originalAllDay INTEGER, lastDate INTEGER, hasAttendeeData INTEGER NOT NULL DEFAULT 0, guestsCanModify INTEGER NOT NULL DEFAULT 0, guestsCanInviteOthers INTEGER NOT NULL DEFAULT 1, guestsCanSeeGuests INTEGER NOT NULL DEFAULT 1, organizer STRING, deleted INTEGER NOT NULL DEFAULT 0, eventEndTimezone TEXT, customAppPackage TEXT, customAppUri TEXT, sync_data1 TEXT, sync_data2 TEXT, sync_data3 TEXT, sync_data4 TEXT, sync_data5 TEXT, sync_data6 TEXT, sync_data7 TEXT, sync_data8 TEXT, sync_data9 TEXT, sync_data10 TEXT);
CREATE TABLE EventsRawTimes (_id INTEGER PRIMARY KEY, event_id INTEGER NOT NULL, dtstart2445 TEXT, dtend2445 TEXT, originalInstanceTime2445 TEXT, lastDate2445 TEXT, UNIQUE (event_id));
CREATE TABLE Instances (_id INTEGER PRIMARY KEY, event_id INTEGER, begin INTEGER, end INTEGER, startDay INTEGER, endDay INTEGER, startMinute INTEGER, endMinute INTEGER, UNIQUE (event_id, begin, end));
CREATE TABLE CalendarMetaData (_id INTEGER PRIMARY KEY, localTimezone TEXT, minInstance INTEGER, maxInstance INTEGER);
CREATE TABLE CalendarCache (_id INTEGER PRIMARY KEY, key TEXT NOT NULL, value TEXT);
CREATE TABLE Attendees (_id INTEGER PRIMARY KEY, event_id INTEGER, attendeeName TEXT, attendeeEmail TEXT, attendeeStatus INTEGER, attendeeRelationship INTEGER, attendeeType INTEGER, attendeeIdentity TEXT, attendeeIdNamespace TEXT);
CREATE TABLE Reminders (_id INTEGER PRIMARY KEY, event_id INTEGER, minutes INTEGER, method INTEGER NOT NULL DEFAULT 0);
CREATE TABLE CalendarAlerts (_id INTEGER PRIMARY KEY, event_id INTEGER, begin INTEGER NOT NULL, end INTEGER NOT NULL, alarmTime INTEGER NOT NULL, creationTime INTEGER NOT NULL DEFAULT 0, receivedTime INTEGER NOT NULL DEFAULT 0, notifyTime INTEGER NOT NULL DEFAULT 0, state INTEGER NOT NULL, minutes INTEGER, UNIQUE (alarmTime, begin, event_id));
CREATE TABLE ExtendedProperties (_id INTEGER PRIMARY KEY, event_id INTEGER, name TEXT, value TEXT);
-- @phase 1
ALTER TABLE Events ADD COLUMN isOrganizer INTEGER;
ALTER TABLE Calendars ADD COLUMN isPrimary INTEGER;
ALTER TABLE Events ADD COLUMN uid2445 TEXT;
ALTER TABLE Events ADD COLUMN mutators TEXT;
ALTER TABLE Calendars ADD COLUMN mutators TEXT;

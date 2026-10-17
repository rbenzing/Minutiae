-- Same provider before an upgrade that added sub_id, creator, text_only with ALTER TABLE ADD COLUMN.
-- Source: AOSP platform/packages/providers/TelephonyProvider, tag android-14.0.0_r1, src/com/android/providers/telephony/MmsSmsDatabaseHelper.java; the columns added by the upgrade steps of the same file (verdict historical).
-- Per-table file:line and verdict: tools/fixtures/schemas/android-telephony/CROSSCHECK.md (Task 4b).
-- Phase 0 is the old schema, phase 1 the ALTERs.
CREATE TABLE sms (_id INTEGER PRIMARY KEY AUTOINCREMENT, thread_id INTEGER, address TEXT, person INTEGER, date INTEGER, date_sent INTEGER DEFAULT 0, protocol INTEGER, read INTEGER DEFAULT 0, status INTEGER DEFAULT -1, type INTEGER, reply_path_present INTEGER, subject TEXT, body TEXT, service_center TEXT, locked INTEGER DEFAULT 0, error_code INTEGER DEFAULT 0, seen INTEGER DEFAULT 0);
CREATE TABLE threads (_id INTEGER PRIMARY KEY AUTOINCREMENT, date INTEGER DEFAULT 0, message_count INTEGER DEFAULT 0, recipient_ids TEXT, snippet TEXT, snippet_cs INTEGER DEFAULT 0, read INTEGER DEFAULT 1, archived INTEGER DEFAULT 0, type INTEGER DEFAULT 0, error INTEGER DEFAULT 0, has_attachment INTEGER DEFAULT 0);
CREATE TABLE canonical_addresses (_id INTEGER PRIMARY KEY AUTOINCREMENT, address TEXT);
CREATE TABLE pdu (_id INTEGER PRIMARY KEY AUTOINCREMENT, thread_id INTEGER, date INTEGER, date_sent INTEGER DEFAULT 0, msg_box INTEGER, read INTEGER DEFAULT 0, m_id TEXT, sub TEXT, sub_cs INTEGER, ct_t TEXT, ct_l TEXT, exp INTEGER, m_cls TEXT, m_type INTEGER, v INTEGER, m_size INTEGER, pri INTEGER, rr INTEGER, rpt_a INTEGER, resp_st INTEGER, st INTEGER, tr_id TEXT, retr_st INTEGER, retr_txt TEXT, retr_txt_cs INTEGER, read_status INTEGER, ct_cls INTEGER, resp_txt TEXT, d_tm INTEGER, d_rpt INTEGER, locked INTEGER DEFAULT 0, seen INTEGER DEFAULT 0);
CREATE TABLE addr (_id INTEGER PRIMARY KEY AUTOINCREMENT, msg_id INTEGER, contact_id INTEGER, address TEXT, type INTEGER, charset INTEGER);
CREATE TABLE part (_id INTEGER PRIMARY KEY AUTOINCREMENT, mid INTEGER, seq INTEGER DEFAULT 0, ct TEXT, name TEXT, chset INTEGER, cd TEXT, fn TEXT, cid TEXT, cl TEXT, ctt_s INTEGER, ctt_t TEXT, _data TEXT, text TEXT);
-- @phase 1
ALTER TABLE sms ADD COLUMN sub_id INTEGER DEFAULT -1;
ALTER TABLE sms ADD COLUMN creator TEXT;
ALTER TABLE pdu ADD COLUMN sub_id INTEGER DEFAULT -1;
ALTER TABLE pdu ADD COLUMN creator TEXT;
ALTER TABLE pdu ADD COLUMN text_only INTEGER DEFAULT 0;

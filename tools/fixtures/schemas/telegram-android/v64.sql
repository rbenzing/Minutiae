-- Telegram Android cache4.db schema snapshot (plan 4I4); the CREATE TABLE statements of the pinned source
-- tag release-5.14.0_1851
-- commit 7d6fa267c13bacd48f8d8f10d31a61f38b77bfe4
-- verdict verified
-- user_version 64
-- tables only; indexes and triggers are not part of the snapshot
CREATE TABLE messages (mid INTEGER PRIMARY KEY, uid INTEGER, read_state INTEGER, send_state INTEGER, date INTEGER, data BLOB, out INTEGER, ttl INTEGER, media INTEGER, replydata BLOB, imp INTEGER, mention INTEGER);
CREATE TABLE users (uid INTEGER PRIMARY KEY, name TEXT, status INTEGER, data BLOB);
CREATE TABLE users_data (uid INTEGER PRIMARY KEY, about TEXT);
CREATE TABLE chats (uid INTEGER PRIMARY KEY, name TEXT, data BLOB);
CREATE TABLE enc_chats (uid INTEGER PRIMARY KEY, user INTEGER, name TEXT, data BLOB, g BLOB, authkey BLOB, ttl INTEGER, layer INTEGER, seq_in INTEGER, seq_out INTEGER, use_count INTEGER, exchange_id INTEGER, key_date INTEGER, fprint INTEGER, fauthkey BLOB, khash BLOB, in_seq_no INTEGER, admin_id INTEGER, mtproto_seq INTEGER);
CREATE TABLE contacts (uid INTEGER PRIMARY KEY, mutual INTEGER);
CREATE TABLE media_v2 (mid INTEGER PRIMARY KEY, uid INTEGER, date INTEGER, type INTEGER, data BLOB);
CREATE TABLE dialogs (did INTEGER PRIMARY KEY, date INTEGER, unread_count INTEGER, last_mid INTEGER, inbox_max INTEGER, outbox_max INTEGER, last_mid_i INTEGER, unread_count_i INTEGER, pts INTEGER, date_i INTEGER, pinned INTEGER, flags INTEGER, folder_id INTEGER, data BLOB);
CREATE TABLE messages_holes (uid INTEGER, start INTEGER, end INTEGER, PRIMARY KEY(uid, start));
CREATE TABLE messages_seq (mid INTEGER PRIMARY KEY, seq_in INTEGER, seq_out INTEGER);
CREATE TABLE user_contacts_v7 (key TEXT PRIMARY KEY, uid INTEGER, fname TEXT, sname TEXT, imported INTEGER);
CREATE TABLE user_phones_v7 (key TEXT, phone TEXT, sphone TEXT, deleted INTEGER, PRIMARY KEY (key, phone));

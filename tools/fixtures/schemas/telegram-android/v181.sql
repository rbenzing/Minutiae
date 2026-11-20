-- Telegram Android cache4.db schema snapshot (plan 4I4); the CREATE TABLE statements of the pinned source
-- tag master
-- commit 21f7d109da6ab3506c9e9577f98c6a38771ab335
-- verdict verified
-- user_version 181
-- tables only; indexes and triggers are not part of the snapshot
CREATE TABLE messages_v2 (mid INTEGER, uid INTEGER, read_state INTEGER, send_state INTEGER, date INTEGER, data BLOB, out INTEGER, ttl INTEGER, media INTEGER, replydata BLOB, imp INTEGER, mention INTEGER, forwards INTEGER, replies_data BLOB, thread_reply_id INTEGER, is_channel INTEGER, reply_to_message_id INTEGER, custom_params BLOB, group_id INTEGER, reply_to_story_id INTEGER, PRIMARY KEY(mid, uid));
CREATE TABLE users (uid INTEGER PRIMARY KEY, name TEXT, status INTEGER, data BLOB);
CREATE TABLE users_data (uid INTEGER PRIMARY KEY, about TEXT);
CREATE TABLE chats (uid INTEGER PRIMARY KEY, name TEXT, data BLOB);
CREATE TABLE enc_chats (uid INTEGER PRIMARY KEY, user INTEGER, name TEXT, data BLOB, g BLOB, authkey BLOB, ttl INTEGER, layer INTEGER, seq_in INTEGER, seq_out INTEGER, use_count INTEGER, exchange_id INTEGER, key_date INTEGER, fprint INTEGER, fauthkey BLOB, khash BLOB, in_seq_no INTEGER, admin_id INTEGER, mtproto_seq INTEGER);
CREATE TABLE contacts (uid INTEGER PRIMARY KEY, mutual INTEGER);
CREATE TABLE media_v4 (mid INTEGER, uid INTEGER, date INTEGER, type INTEGER, data BLOB, PRIMARY KEY(mid, uid, type));
CREATE TABLE dialogs (did INTEGER PRIMARY KEY, date INTEGER, unread_count INTEGER, last_mid INTEGER, inbox_max INTEGER, outbox_max INTEGER, last_mid_i INTEGER, unread_count_i INTEGER, pts INTEGER, date_i INTEGER, pinned INTEGER, flags INTEGER, folder_id INTEGER, data BLOB, unread_reactions INTEGER, last_mid_group INTEGER, ttl_period INTEGER, unread_poll_votes INTEGER);
CREATE TABLE messages_topics (mid INTEGER, uid INTEGER, topic_id INTEGER, read_state INTEGER, send_state INTEGER, date INTEGER, data BLOB, out INTEGER, ttl INTEGER, media INTEGER, replydata BLOB, imp INTEGER, mention INTEGER, forwards INTEGER, replies_data BLOB, thread_reply_id INTEGER, is_channel INTEGER, reply_to_message_id INTEGER, custom_params BLOB, reply_to_story_id INTEGER, PRIMARY KEY(mid, topic_id, uid));
CREATE TABLE scheduled_messages_v2 (mid INTEGER, uid INTEGER, send_state INTEGER, date INTEGER, data BLOB, ttl INTEGER, replydata BLOB, reply_to_message_id INTEGER, PRIMARY KEY(mid, uid));
CREATE TABLE messages_holes (uid INTEGER, start INTEGER, end INTEGER, PRIMARY KEY(uid, start));
CREATE TABLE randoms_v2 (random_id INTEGER, mid INTEGER, uid INTEGER, PRIMARY KEY (random_id, mid, uid));
CREATE TABLE messages_seq (mid INTEGER PRIMARY KEY, seq_in INTEGER, seq_out INTEGER);
CREATE TABLE user_contacts_v7 (key TEXT PRIMARY KEY, uid INTEGER, fname TEXT, sname TEXT, imported INTEGER);
CREATE TABLE user_phones_v7 (key TEXT, phone TEXT, sphone TEXT, deleted INTEGER, PRIMARY KEY (key, phone));

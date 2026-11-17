-- WhatsApp Android msgstore.db, transition era (both messages and message), plan 4I.
-- source S2 https://github.com/andreas-mausch/whatsapp-viewer/blob/2aa6388beefdccb8600038dd3b4fffcf9e20ab87/data/msgstore.db.schema.sql 2aa6388beefdccb8600038dd3b4fffcf9e20ab87 2021-08-24
-- Copyright (c) 2014-2018 Andreas Mausch, MIT License. The statements below are quoted verbatim from the
-- file named above, at the commit named above, as a subset: only the tables, indexes, views and triggers
-- that the msgstore parser reads or names in a note (the full dump is not copied).
-- verdict quoted
CREATE TABLE props (_id INTEGER PRIMARY KEY AUTOINCREMENT, key TEXT UNIQUE, value TEXT);
CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, key_remote_jid TEXT NOT NULL, key_from_me INTEGER, key_id TEXT NOT NULL, status INTEGER, needs_push INTEGER, data TEXT, timestamp INTEGER, media_url TEXT, media_mime_type TEXT, media_wa_type TEXT, media_size INTEGER, media_name TEXT, media_caption TEXT, media_hash TEXT, media_duration INTEGER, origin INTEGER, latitude REAL, longitude REAL, thumb_image TEXT, remote_resource TEXT, received_timestamp INTEGER, send_timestamp INTEGER, receipt_server_timestamp INTEGER, receipt_device_timestamp INTEGER, read_device_timestamp INTEGER, played_device_timestamp INTEGER, raw_data BLOB, recipient_count INTEGER, participant_hash TEXT, starred INTEGER, quoted_row_id INTEGER, mentioned_jids TEXT, multicast_id TEXT, edit_version INTEGER, media_enc_hash TEXT, payment_transaction_id TEXT, forwarded INTEGER, preview_type INTEGER, send_count INTEGER, lookup_tables INTEGER, future_message_type INTEGER);
CREATE UNIQUE INDEX messages_key_index on messages (key_remote_jid, key_from_me, key_id);
CREATE INDEX media_hash_index on messages (media_hash);
CREATE INDEX media_type_index on messages (media_wa_type);
CREATE INDEX media_type_jid_index on messages (key_remote_jid, media_wa_type);
CREATE INDEX messages_jid_id_index on messages (key_remote_jid, _id);
CREATE INDEX starred_index on messages (starred);
CREATE TABLE message (_id INTEGER PRIMARY KEY AUTOINCREMENT, chat_row_id INTEGER NOT NULL, from_me INTEGER NOT NULL, key_id TEXT NOT NULL, sender_jid_row_id INTEGER, status INTEGER, broadcast INTEGER, recipient_count INTEGER, participant_hash TEXT, origination_flags INTEGER, origin INTEGER, timestamp INTEGER, received_timestamp INTEGER, receipt_server_timestamp INTEGER, message_type INTEGER, text_data TEXT, starred INTEGER, lookup_tables INTEGER, sort_id INTEGER NOT NULL DEFAULT 0 );
CREATE INDEX message_sort_id_index ON message (sort_id);
CREATE UNIQUE INDEX message_key_index on message (chat_row_id, from_me, key_id, sender_jid_row_id);
CREATE INDEX message_starred_index on message (starred);
CREATE INDEX message_type_chat_index on message (chat_row_id, message_type);
CREATE INDEX message_chat_id_index ON message (chat_row_id,_id);
CREATE INDEX message_type_index ON message(message_type);
CREATE TABLE messages_quotes (_id INTEGER PRIMARY KEY AUTOINCREMENT, key_remote_jid TEXT NOT NULL, key_from_me INTEGER, key_id TEXT NOT NULL, status INTEGER, needs_push INTEGER, data TEXT, timestamp INTEGER, media_url TEXT, media_mime_type TEXT, media_wa_type TEXT, media_size INTEGER, media_name TEXT, media_caption TEXT, media_hash TEXT, media_duration INTEGER, origin INTEGER, latitude REAL, longitude REAL, thumb_image TEXT, remote_resource TEXT, received_timestamp INTEGER, send_timestamp INTEGER, receipt_server_timestamp INTEGER, receipt_device_timestamp INTEGER, read_device_timestamp INTEGER, played_device_timestamp INTEGER, raw_data BLOB, recipient_count INTEGER, participant_hash TEXT, starred INTEGER, quoted_row_id INTEGER, mentioned_jids TEXT, multicast_id TEXT, edit_version INTEGER, media_enc_hash TEXT, payment_transaction_id TEXT, forwarded INTEGER, preview_type INTEGER, send_count INTEGER, lookup_tables INTEGER, future_message_type INTEGER);
CREATE TABLE message_quoted (message_row_id INTEGER PRIMARY KEY AUTOINCREMENT, chat_row_id INTEGER NOT NULL, parent_message_chat_row_id INTEGER NOT NULL, from_me INTEGER NOT NULL, sender_jid_row_id INTEGER, key_id TEXT NOT NULL, timestamp INTEGER, message_type INTEGER, origin INTEGER, text_data TEXT, payment_transaction_id TEXT, lookup_tables INTEGER);
CREATE TABLE messages_vcards (_id INTEGER PRIMARY KEY AUTOINCREMENT, message_row_id INTEGER, sender_jid TEXT, chat_jid TEXT, vcard TEXT);
CREATE TABLE message_location (message_row_id INTEGER PRIMARY KEY, chat_row_id INTEGER, latitude REAL, longitude REAL, place_name TEXT, place_address TEXT, url TEXT, live_location_share_duration INTEGER, live_location_sequence_number INTEGER, live_location_final_latitude REAL, live_location_final_longitude REAL, live_location_final_timestamp INTEGER, map_download_status INTEGER);
CREATE TABLE message_media (  message_row_id INTEGER PRIMARY KEY, chat_row_id INTEGER, autotransfer_retry_enabled INTEGER, multicast_id TEXT, media_job_uuid TEXT, transferred INTEGER, transcoded INTEGER, file_path TEXT, file_size INTEGER, suspicious_content INTEGER, trim_from INTEGER, trim_to INTEGER, face_x INTEGER, face_y INTEGER, media_key BLOB, media_key_timestamp INTEGER, width INTEGER, height INTEGER, has_streaming_sidecar INTEGER, gif_attribution INTEGER, thumbnail_height_width_ratio REAL, direct_path TEXT, first_scan_sidecar BLOB, first_scan_length INTEGER, message_url TEXT, mime_type TEXT, file_length INTEGER, media_name TEXT, file_hash TEXT, media_duration INTEGER, page_count INTEGER, enc_file_hash TEXT, partial_media_hash TEXT, partial_media_enc_hash TEXT, is_animated_sticker INTEGER, original_file_hash TEXT, mute_video INTEGER DEFAULT 0);
CREATE INDEX message_media_chat_index ON message_media(chat_row_id);
CREATE INDEX message_media_hash_index ON message_media(file_hash);
CREATE INDEX message_media_original_file_hash_index ON message_media(original_file_hash);
CREATE TABLE receipts (_id INTEGER PRIMARY KEY AUTOINCREMENT, key_remote_jid TEXT NOT NULL, key_id TEXT NOT NULL, remote_resource TEXT, receipt_device_timestamp INTEGER, read_device_timestamp INTEGER, played_device_timestamp INTEGER);
CREATE INDEX receipts_key_index on receipts (key_remote_jid, key_id);
CREATE TABLE message_vcard (_id INTEGER PRIMARY KEY AUTOINCREMENT, message_row_id INTEGER, vcard TEXT);
CREATE UNIQUE INDEX message_vcard_index ON message_vcard (message_row_id, vcard);
CREATE TABLE group_participants (_id INTEGER PRIMARY KEY AUTOINCREMENT, gjid TEXT NOT NULL, jid TEXT NOT NULL, admin INTEGER, pending INTEGER, sent_sender_key INTEGER);
CREATE UNIQUE INDEX group_participants_index ON group_participants (gjid, jid);
CREATE TABLE deleted_chat_job
(        _id INTEGER PRIMARY KEY AUTOINCREMENT,
        chat_row_id INTEGER NOT NULL,
        block_size INTEGER,
        deleted_message_row_id INTEGER,
        deleted_starred_message_row_id INTEGER,
        deleted_messages_remove_files BOOLEAN,
        deleted_categories_message_row_id INTEGER,
        deleted_categories_starred_message_row_id INTEGER,
        deleted_categories_remove_files BOOLEAN,
        deleted_message_categories TEXT);
CREATE INDEX deleted_chat_job_index ON deleted_chat_job (chat_row_id, _id);
CREATE TABLE call_log (_id INTEGER PRIMARY KEY AUTOINCREMENT, jid_row_id INTEGER, from_me INTEGER, call_id TEXT, transaction_id INTEGER, timestamp INTEGER, video_call INTEGER, duration INTEGER, call_result INTEGER, bytes_transferred INTEGER, group_jid_row_id INTEGER NOT NULL DEFAULT 0, is_joinable_group_call INTEGER, call_creator_device_jid_row_id INTEGER NOT NULL DEFAULT 0, call_random_id TEXT);
CREATE UNIQUE INDEX call_log_key_index ON call_log (jid_row_id, from_me, call_id, transaction_id);
CREATE TABLE call_log_participant_v2 (    _id             INTEGER PRIMARY KEY AUTOINCREMENT,    call_log_row_id INTEGER,    jid_row_id      INTEGER,    call_result     INTEGER);
CREATE UNIQUE INDEX call_log_participant_key_index on call_log_participant_v2 (call_log_row_id, jid_row_id);
CREATE TABLE missed_call_logs (_id INTEGER PRIMARY KEY AUTOINCREMENT, message_row_id INTEGER, timestamp INTEGER, video_call INTEGER, group_jid_row_id INTEGER NOT NULL DEFAULT 0, is_joinable_group_call INTEGER);
CREATE UNIQUE INDEX missed_call_logs_key_index ON missed_call_logs (message_row_id);
CREATE TABLE jid (_id INTEGER PRIMARY KEY AUTOINCREMENT, user TEXT NOT NULL, server TEXT NOT NULL, agent INTEGER, device INTEGER, type INTEGER, raw_string TEXT);
CREATE UNIQUE INDEX jid_key_new_index ON jid (    user,    server,    agent,    device,    type);
CREATE UNIQUE INDEX jid_raw_string_index ON jid (    raw_string);
CREATE TABLE chat (_id INTEGER PRIMARY KEY AUTOINCREMENT,jid_row_id INTEGER UNIQUE,hidden INTEGER,subject TEXT,created_timestamp INTEGER,display_message_row_id INTEGER,last_message_row_id INTEGER,last_read_message_row_id INTEGER,last_read_receipt_sent_message_row_id INTEGER,last_important_message_row_id INTEGER,archived INTEGER,sort_timestamp INTEGER,mod_tag INTEGER,gen REAL,spam_detection INTEGER,unseen_earliest_message_received_time INTEGER,unseen_message_count INTEGER,unseen_missed_calls_count INTEGER,unseen_row_count INTEGER,plaintext_disabled INTEGER,vcard_ui_dismissed INTEGER,change_number_notified_message_row_id INTEGER,show_group_description INTEGER,ephemeral_expiration INTEGER,last_read_ephemeral_message_row_id INTEGER,ephemeral_setting_timestamp INTEGER,ephemeral_disappearing_messages_initiator INTEGER,unseen_important_message_count INTEGER NOT NULL DEFAULT 0 );
CREATE TABLE message_forwarded(message_row_id INTEGER PRIMARY KEY, forward_score INTEGER);
CREATE TABLE message_revoked (message_row_id INTEGER PRIMARY KEY, revoked_key_id TEXT NOT NULL);
CREATE TABLE message_system (message_row_id INTEGER PRIMARY KEY, action_type INTEGER NOT NULL);
CREATE TABLE message_system_value_change (message_row_id INTEGER PRIMARY KEY, old_data TEXT);
CREATE TABLE message_view_once_media (message_row_id INTEGER PRIMARY KEY, state INTEGER NOT NULL );
CREATE INDEX message_view_once_media_state_index ON message_view_once_media (state);
CREATE TABLE message_ephemeral(message_row_id INTEGER PRIMARY KEY, duration INTEGER NOT NULL, expire_timestamp INTEGER NOT NULL);
CREATE INDEX message_ephemeral_expire_timestamp_index on message_ephemeral(expire_timestamp);
CREATE VIEW legacy_available_messages_view AS
 SELECT messages.*, chat._id AS chat_row_id,expire_timestamp
 FROM messages AS messages
    JOIN
        jid AS jid
    ON
        jid.raw_string = messages.key_remote_jid
    JOIN
        chat AS chat
    ON
        chat.jid_row_id = jid._id
    LEFT JOIN
        deleted_chat_job as job
    ON 
        job.chat_row_id = chat._id    LEFT JOIN
        message_ephemeral AS message_ephemeral
    ON messages._id = message_ephemeral.message_row_id
 WHERE IFNULL(NOT((IFNULL(messages.starred, 0) = 0 AND messages._id <= IFNULL(job.deleted_message_row_id, 1)) OR (IFNULL(messages.starred, 0) = 1 AND messages._id <= IFNULL(job.deleted_starred_message_row_id, 1)) OR ( (job.deleted_message_categories IS NOT NULL) AND   (job.deleted_message_categories LIKE '%"' || messages.media_wa_type || '"%') AND   ( (IFNULL(messages.starred, 0) = 0 AND messages._id <= IFNULL(job.deleted_categories_message_row_id, 1)) OR (IFNULL(messages.starred, 0) = 1 AND messages._id <= IFNULL(job.deleted_categories_starred_message_row_id, 1)) ))), 0)
/* legacy_available_messages_view(_id,key_remote_jid,key_from_me,key_id,status,needs_push,data,timestamp,media_url,media_mime_type,media_wa_type,media_size,media_name,media_caption,media_hash,media_duration,origin,latitude,longitude,thumb_image,remote_resource,received_timestamp,send_timestamp,receipt_server_timestamp,receipt_device_timestamp,read_device_timestamp,played_device_timestamp,raw_data,recipient_count,participant_hash,starred,quoted_row_id,mentioned_jids,multicast_id,edit_version,media_enc_hash,payment_transaction_id,forwarded,preview_type,send_count,lookup_tables,future_message_type,chat_row_id,expire_timestamp) */;
CREATE VIEW message_view AS SELECT messages._id AS _id, messages._id AS sort_id, chat._id AS chat_row_id, key_from_me AS from_me, key_id, -1 AS sender_jid_row_id, remote_resource AS sender_jid_raw_string, status, needs_push AS broadcast, recipient_count, participant_hash, forwarded AS origination_flags, origin, timestamp, received_timestamp, receipt_server_timestamp, CAST (CASE WHEN (messages.media_wa_type = 0 AND messages.status=6) THEN 7 ELSE messages.media_wa_type END AS INTEGER) AS message_type, '' as text_data, starred, lookup_tables, data, media_url, media_mime_type, media_size, media_name, media_caption, media_hash, media_duration, latitude, longitude, thumb_image, raw_data, quoted_row_id, mentioned_jids, multicast_id, edit_version, media_enc_hash, payment_transaction_id, preview_type, receipt_device_timestamp, read_device_timestamp, played_device_timestamp, future_message_type, 1 AS table_version FROM messages JOIN jid AS chat_jid ON messages.key_remote_jid= chat_jid.raw_string JOIN chat AS chat ON chat.jid_row_id = chat_jid._id
/* message_view(_id,sort_id,chat_row_id,from_me,key_id,sender_jid_row_id,sender_jid_raw_string,status,broadcast,recipient_count,participant_hash,origination_flags,origin,timestamp,received_timestamp,receipt_server_timestamp,message_type,text_data,starred,lookup_tables,data,media_url,media_mime_type,media_size,media_name,media_caption,media_hash,media_duration,latitude,longitude,thumb_image,raw_data,quoted_row_id,mentioned_jids,multicast_id,edit_version,media_enc_hash,payment_transaction_id,preview_type,receipt_device_timestamp,read_device_timestamp,played_device_timestamp,future_message_type,table_version) */;
CREATE VIEW available_message_view AS SELECT messages._id AS _id, messages._id AS sort_id, chat._id AS chat_row_id, key_from_me AS from_me, key_id, -1 AS sender_jid_row_id, remote_resource AS sender_jid_raw_string, status, needs_push AS broadcast, recipient_count, participant_hash, forwarded AS origination_flags, origin, timestamp, received_timestamp, receipt_server_timestamp, CAST (CASE WHEN (messages.media_wa_type = 0 AND messages.status=6) THEN 7 ELSE messages.media_wa_type END AS INTEGER) AS message_type, '' as text_data, starred, lookup_tables, data, media_url, media_mime_type, media_size, media_name, media_caption, media_hash, media_duration, latitude, longitude, thumb_image, raw_data, quoted_row_id, mentioned_jids, multicast_id, edit_version, media_enc_hash, payment_transaction_id, preview_type, receipt_device_timestamp, read_device_timestamp, played_device_timestamp, future_message_type, 1 AS table_version, expire_timestamp FROM messages JOIN jid AS chat_jid ON messages.key_remote_jid= chat_jid.raw_string JOIN chat AS chat ON chat.jid_row_id = chat_jid._id LEFT JOIN message_ephemeral AS message_ephemeral ON messages._id = message_ephemeral.message_row_id LEFT JOIN deleted_chat_job AS job ON job.chat_row_id = chat._id WHERE IFNULL(NOT((IFNULL(messages.starred, 0) = 0 AND messages._id <= IFNULL(job.deleted_message_row_id, 1)) OR (IFNULL(messages.starred, 0) = 1 AND messages._id <= IFNULL(job.deleted_starred_message_row_id, 1)) OR ( (job.deleted_message_categories IS NOT NULL) AND   (job.deleted_message_categories LIKE '%"' || messages.media_wa_type || '"%') AND   ( (IFNULL(messages.starred, 0) = 0 AND messages._id <= IFNULL(job.deleted_categories_message_row_id, 1)) OR (IFNULL(messages.starred, 0) = 1 AND messages._id <= IFNULL(job.deleted_categories_starred_message_row_id, 1)) ))), 0)
/* available_message_view(_id,sort_id,chat_row_id,from_me,key_id,sender_jid_row_id,sender_jid_raw_string,status,broadcast,recipient_count,participant_hash,origination_flags,origin,timestamp,received_timestamp,receipt_server_timestamp,message_type,text_data,starred,lookup_tables,data,media_url,media_mime_type,media_size,media_name,media_caption,media_hash,media_duration,latitude,longitude,thumb_image,raw_data,quoted_row_id,mentioned_jids,multicast_id,edit_version,media_enc_hash,payment_transaction_id,preview_type,receipt_device_timestamp,read_device_timestamp,played_device_timestamp,future_message_type,table_version,expire_timestamp) */;
CREATE TRIGGER messages_bd_for_message_media_trigger BEFORE DELETE ON messages BEGIN DELETE FROM message_media WHERE message_row_id=old._id; END;
CREATE TRIGGER messages_bd_for_message_trigger BEFORE DELETE ON messages BEGIN DELETE FROM message WHERE _id=old._id; END;

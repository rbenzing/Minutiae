-- WhatsApp Android msgstore.db, modern schema, plan 4I.
-- source S3 https://github.com/rsalinas/whatsapp-db-schema 8c9e8000ce9894b70eeef3ee324e81df52357b2f 2026-11-15
-- The repository of the source above carries no licence. Nothing is copied from it: every statement below is
-- RETYPED from the published column lists (table names, column names, declared types and constraints) recorded in
-- research section 4, and no view or trigger text of the source is reproduced. Only the tables the msgstore
-- parser reads or names in a note are listed, with the columns the research notes record for them; a column the
-- notes abridge with an ellipsis is not listed.
-- verdict retyped (no licence)
CREATE TABLE props (_id INTEGER PRIMARY KEY AUTOINCREMENT, key TEXT UNIQUE, value TEXT);
CREATE TABLE jid (_id INTEGER PRIMARY KEY AUTOINCREMENT, user TEXT NOT NULL, server TEXT NOT NULL, agent INTEGER, device INTEGER, type INTEGER, raw_string TEXT);
CREATE UNIQUE INDEX jid_raw_string_index ON jid (raw_string);
CREATE TABLE jid_map (lid_row_id INTEGER PRIMARY KEY NOT NULL, jid_row_id INTEGER NOT NULL, sort_id INTEGER);
CREATE TABLE chat (_id INTEGER PRIMARY KEY AUTOINCREMENT, jid_row_id INTEGER UNIQUE, hidden INTEGER, subject TEXT, created_timestamp INTEGER, display_message_row_id INTEGER, last_message_row_id INTEGER, last_read_message_row_id INTEGER, archived INTEGER, sort_timestamp INTEGER, ephemeral_expiration INTEGER, ephemeral_setting_timestamp INTEGER, ephemeral_disappearing_messages_initiator INTEGER, group_type INTEGER NOT NULL DEFAULT 0, chat_lock INTEGER, account_jid_row_id INTEGER, group_member_count INTEGER);
CREATE TABLE message (_id INTEGER PRIMARY KEY AUTOINCREMENT, chat_row_id INTEGER NOT NULL, from_me INTEGER NOT NULL, key_id TEXT NOT NULL, sender_jid_row_id INTEGER, status INTEGER, broadcast INTEGER, recipient_count INTEGER, participant_hash TEXT, origination_flags INTEGER, origin INTEGER, timestamp INTEGER, received_timestamp INTEGER, receipt_server_timestamp INTEGER, message_type INTEGER, text_data TEXT, starred INTEGER, lookup_tables INTEGER, message_add_on_flags INTEGER, view_mode INTEGER, sort_id INTEGER NOT NULL DEFAULT 0, translated_text TEXT, view_replies_thread_id INTEGER, server_sts INTEGER);
CREATE INDEX message_sort_id_index ON message (sort_id);
CREATE UNIQUE INDEX message_key_index ON message (chat_row_id, from_me, key_id, sender_jid_row_id);
CREATE TABLE message_media (message_row_id INTEGER PRIMARY KEY, chat_row_id INTEGER, transferred INTEGER, file_path TEXT, file_size INTEGER, media_key BLOB, width INTEGER, height INTEGER, direct_path TEXT, message_url TEXT, mime_type TEXT, file_length INTEGER, media_name TEXT, file_hash TEXT, media_duration INTEGER, page_count INTEGER, enc_file_hash TEXT, is_animated_sticker INTEGER, original_file_hash TEXT, media_caption TEXT, raw_transcription_text TEXT, first_viewed_timestamp INTEGER, e2ee_media_key BLOB);
CREATE TABLE message_quoted (message_row_id INTEGER PRIMARY KEY, chat_row_id INTEGER, parent_message_chat_row_id INTEGER, from_me INTEGER, sender_jid_row_id INTEGER, key_id TEXT, timestamp INTEGER, message_type INTEGER, origin INTEGER, text_data TEXT, payment_transaction_id TEXT, lookup_tables INTEGER, quoted_source INTEGER, quoted_type INTEGER);
CREATE TABLE message_revoked (message_row_id INTEGER PRIMARY KEY, revoked_key_id TEXT NOT NULL, admin_jid_row_id INTEGER, revoke_timestamp INTEGER);
CREATE TABLE message_system (message_row_id INTEGER PRIMARY KEY, action_type INTEGER NOT NULL);
CREATE TABLE message_system_value_change (message_row_id INTEGER PRIMARY KEY, old_data TEXT);
CREATE TABLE message_system_number_change (message_row_id INTEGER PRIMARY KEY, old_jid_row_id INTEGER, new_jid_row_id INTEGER);
CREATE TABLE message_edit_info (message_row_id INTEGER PRIMARY KEY, original_key_id TEXT NOT NULL, edited_timestamp INTEGER NOT NULL, sender_timestamp INTEGER NOT NULL, display_type INTEGER);
CREATE TABLE message_ephemeral (message_row_id INTEGER PRIMARY KEY, duration INTEGER NOT NULL, expire_timestamp INTEGER NOT NULL, keep_in_chat INTEGER, ephemeral_trigger INTEGER, ephemeral_initiated_by_me INTEGER, after_read_duration INTEGER);
CREATE TABLE message_view_once_media (message_row_id INTEGER PRIMARY KEY, state INTEGER NOT NULL);
CREATE TABLE message_forwarded (message_row_id INTEGER PRIMARY KEY, forward_score INTEGER, forward_origin INTEGER);
CREATE TABLE message_call_log (message_row_id INTEGER PRIMARY KEY, call_log_row_id INTEGER);
CREATE TABLE message_location (message_row_id INTEGER PRIMARY KEY, chat_row_id INTEGER, latitude REAL, longitude REAL, place_name TEXT, place_address TEXT, url TEXT, live_location_share_duration INTEGER, live_location_sequence_number INTEGER, live_location_final_latitude REAL, live_location_final_longitude REAL, live_location_final_timestamp INTEGER, map_download_status INTEGER);
CREATE TABLE message_vcard (_id INTEGER PRIMARY KEY AUTOINCREMENT, message_row_id INTEGER, vcard TEXT);
CREATE TABLE message_add_on (_id INTEGER PRIMARY KEY AUTOINCREMENT, chat_row_id INTEGER, from_me INTEGER, key_id TEXT, sender_jid_row_id INTEGER, parent_message_row_id INTEGER, timestamp INTEGER, status INTEGER, message_add_on_type INTEGER, received_timestamp INTEGER, expiry_duration_in_secs INTEGER, server_timestamp INTEGER, expiry_timestamp INTEGER, expiry_type INTEGER);
CREATE TABLE message_add_on_reaction (message_add_on_row_id INTEGER PRIMARY KEY, reaction TEXT, sender_timestamp INTEGER);
CREATE TABLE call_log (_id INTEGER PRIMARY KEY AUTOINCREMENT, jid_row_id INTEGER, from_me INTEGER, call_id TEXT, transaction_id INTEGER, timestamp INTEGER, video_call INTEGER, duration INTEGER, call_result INTEGER, is_dnd_mode_on INTEGER, bytes_transferred INTEGER, group_jid_row_id INTEGER NOT NULL DEFAULT 0, is_joinable_group_call INTEGER, call_creator_device_jid_row_id INTEGER NOT NULL DEFAULT 0, call_random_id TEXT, call_link_row_id INTEGER, call_type INTEGER, offer_silence_reason INTEGER, scheduled_id TEXT, telecom_uuid TEXT, terminated_by_device_switch INTEGER);
CREATE TABLE call_log_participant_v2 (_id INTEGER PRIMARY KEY AUTOINCREMENT, call_log_row_id INTEGER, jid_row_id INTEGER, call_result INTEGER);
CREATE TABLE missed_call_logs (_id INTEGER PRIMARY KEY AUTOINCREMENT, message_row_id INTEGER, timestamp INTEGER, video_call INTEGER, group_jid_row_id INTEGER, is_joinable_group_call INTEGER);
CREATE TABLE deleted_chat_job (_id INTEGER PRIMARY KEY AUTOINCREMENT, chat_row_id INTEGER, block_size INTEGER, singular_message_delete_rows_id TEXT, deleted_message_row_id INTEGER, deleted_starred_message_row_id INTEGER, deleted_messages_remove_files INTEGER, deleted_categories_message_row_id INTEGER, deleted_categories_starred_message_row_id INTEGER, deleted_categories_remove_files INTEGER, deleted_message_categories TEXT, delete_files_singular_delete INTEGER);
CREATE TABLE newsletter (chat_row_id INTEGER PRIMARY KEY, name TEXT, description TEXT, subscribers_count INTEGER, membership INTEGER, privacy INTEGER, verified INTEGER, muted INTEGER);

-- WhatsApp Android wa.db, the current contact table, plan 4I.
-- source S9 https://arxiv.org/html/1507.07739v1 2014 (the common column set)
-- source S10 (column list; cited by ID only)
-- source S4 (columns checked before reading; cited by ID only)
-- source S8 https://thebinaryhick.blog/2022/06/09/new-msgstore-who-dis-a-look-at-an-updated-whatsapp-on-android/ 2022-06-09
-- No DDL of wa.db was found in any source: wa_contacts is CONSTRUCTED from the full common column set the sources
-- report (research section 5), with declared types and constraints chosen by the writer. The four tables after it
-- are one-column STUBS (a column named only to make the table exist): their real columns are not established.
-- It is from memory.
-- verdict from memory
CREATE TABLE wa_contacts (_id INTEGER PRIMARY KEY AUTOINCREMENT, jid TEXT NOT NULL, is_whatsapp_user INTEGER, status TEXT, status_timestamp INTEGER, number TEXT, raw_contact_id INTEGER, display_name TEXT, phone_type INTEGER, phone_label TEXT, unseen_msg_count INTEGER, photo_ts INTEGER, thumb_ts INTEGER, photo_id_timestamp INTEGER, given_name TEXT, family_name TEXT, wa_name TEXT, sort_name TEXT, nickname TEXT, company TEXT, title TEXT, offset INTEGER);
CREATE TABLE wa_group_admin_settings (jid TEXT);
CREATE TABLE wa_block_list (jid TEXT);
CREATE TABLE wa_vnames (jid TEXT);
CREATE TABLE wa_biz_profiles (jid TEXT);

-- WhatsApp Android wa.db, the WA 2.11 contact table, plan 4I.
-- source S9 https://arxiv.org/html/1507.07739v1 2014 (WA 2.11)
-- source S10 (column list; cited by ID only)
-- No DDL of wa.db was found in any source: this statement is CONSTRUCTED from the column lists the sources
-- report (research section 5), with declared types and constraints chosen by the writer. It is from memory.
-- verdict from memory
CREATE TABLE wa_contacts (_id INTEGER PRIMARY KEY AUTOINCREMENT, jid TEXT NOT NULL, is_whatsapp_user INTEGER, status TEXT, status_timestamp INTEGER, number TEXT, raw_contact_id INTEGER, display_name TEXT, phone_type INTEGER, phone_label TEXT, unseen_msg_count INTEGER, photo_ts INTEGER, thumb_ts INTEGER, photo_id_timestamp INTEGER, given_name TEXT, family_name TEXT, wa_name TEXT, sort_name TEXT);

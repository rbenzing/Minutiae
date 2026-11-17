# WhatsApp Android DDL snapshots: cross-check verdicts

One row per `CREATE TABLE` of every snapshot in this directory (plan 4I, ruling 4I-R6). The third cell is the
verdict and must equal the file's own verdict line:

- `quoted`: the statement is quoted verbatim from a source whose licence permits it (the file carries the notice).
- `retyped`: the source has no licence, so the statement is retyped from its published column list.
- `from-memory`: no DDL of the table exists in any source; the statement is constructed from reported columns.
  Every `from-memory` table is listed in the from-memory table of `docs/limitations.md`.

Sources are cited by the IDs of the research notes (`docs/superpowers/research/`, local only). Sources S4 to S7 and
S10 to S13 are cited by ID only.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| legacy-2.11.sql | messages | retyped | research 3.1, S23 column set ending at participant_hash, S9 2.11 |
| legacy-2.11.sql | chat_list | retyped | research 3.2, S23 DDL, S9 2.11 had only three columns |
| legacy-2.11.sql | messages_quotes | retyped | research 3.2, same column set as messages |
| legacy-2.11.sql | messages_vcards | retyped | research 3.2, S2 and S1 |
| legacy-2.11.sql | group_participants | retyped | research 3.2, S2, S23 and S4 |
| legacy-2.11.sql | receipts | retyped | research 3.2, S2 and S23 column lists |
| modern-2026.sql | props | retyped | research 3.2, S2 and S3 |
| modern-2026.sql | jid | retyped | research 4.2, S2 and S3 column lists |
| modern-2026.sql | jid_map | retyped | research 4.2, S3 DDL and README |
| modern-2026.sql | chat | retyped | research 4.2, S3 abridged column list (columns the notes abridge are not listed) |
| modern-2026.sql | message | retyped | research 4.1, S3 column list at 8c9e800 |
| modern-2026.sql | message_media | retyped | research 4.3, S3 abridged column list |
| modern-2026.sql | message_quoted | retyped | research 4.4, S3 column list |
| modern-2026.sql | message_revoked | retyped | research 4.7, S3 column list |
| modern-2026.sql | message_system | retyped | research 4.9, S2 and S3 |
| modern-2026.sql | message_system_value_change | retyped | research 4.9 |
| modern-2026.sql | message_system_number_change | retyped | research 4.9 |
| modern-2026.sql | message_edit_info | retyped | research 4.6, S3 column list |
| modern-2026.sql | message_ephemeral | retyped | research 4.11, S3 column list |
| modern-2026.sql | message_view_once_media | retyped | research 4.11, S2 and S3 |
| modern-2026.sql | message_forwarded | retyped | research 4.11, S3 column list |
| modern-2026.sql | message_call_log | retyped | research 4.13, S3 and S6 |
| modern-2026.sql | message_location | retyped | research 4.3, S2 and S3 column list |
| modern-2026.sql | message_vcard | retyped | research 4.3, S2 and S3 |
| modern-2026.sql | message_add_on | retyped | research 4.5, S3 column list |
| modern-2026.sql | message_add_on_reaction | retyped | research 4.5, S3 column list |
| modern-2026.sql | call_log | retyped | research 4.13, S3 column list |
| modern-2026.sql | call_log_participant_v2 | retyped | research 4.13, S2 and S3 |
| modern-2026.sql | missed_call_logs | retyped | research 3.2, S2 and S1 |
| modern-2026.sql | deleted_chat_job | retyped | research 4.10, S3 column list (singular_message_delete_rows_id) |
| modern-2026.sql | newsletter | retyped | research 4.14, S3 abridged column list |
| transition-2021.sql | props | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | messages | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | messages_quotes | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_quoted | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | messages_vcards | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_location | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_media | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | receipts | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_vcard | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | group_participants | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | call_log | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | call_log_participant_v2 | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | missed_call_logs | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | jid | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | chat | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_revoked | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_system | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_system_value_change | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| transition-2021.sql | message_view_once_media | quoted | research section 3.3 and the table's own section; quoted from S2 at 2aa6388, MIT |
| wa-2.11.sql | wa_contacts | from-memory | research 5, column list of S9 (WA 2.11) and S10; no DDL exists |
| wa-current.sql | wa_contacts | from-memory | research 5, full common column set of S9, S10, S4 and S8; no DDL exists |
| wa-current.sql | wa_group_admin_settings | from-memory | research 5: a one-column stub, the real columns are not established |
| wa-current.sql | wa_block_list | from-memory | research 5: a one-column stub, the real columns are not established |
| wa-current.sql | wa_vnames | from-memory | research 5: a one-column stub, the real columns are not established |
| wa-current.sql | wa_biz_profiles | from-memory | research 5: a one-column stub, the real columns are not established |

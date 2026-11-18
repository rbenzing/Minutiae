# WhatsApp iOS DDL snapshots: cross-check verdicts

One row per `CREATE TABLE` of every snapshot in this directory (plan 4I2, ruling 4I2-R13). Every table is
`from-memory`: no DDL of any iOS WhatsApp table was found in any source, so each statement is constructed from the
column lists the sources report, in Core Data form, with declared types chosen by the writer. The column types are
not checked against a public definition. Every `from-memory` table is listed in the from-memory table of
`docs/limitations.md`.

Sources are cited by the IDs of the research notes (`docs/superpowers/research/`, local only), never by name or URL.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| chatstorage.sql | ZWACHATSESSION | from-memory | research 6.2, S16 insert list, S1 and S7 columns |
| chatstorage.sql | ZWAMESSAGE | from-memory | research 6.2 table, S16 full insert list, S7, S1 |
| chatstorage.sql | ZWAMEDIAITEM | from-memory | research 6.2, S16 insert list, S1, S7 |
| chatstorage.sql | ZWAMESSAGEINFO | from-memory | research 6.2, S19 label, S7 (only the receipt blob and the message link are listed) |
| chatstorage.sql | ZWAGROUPMEMBER | from-memory | research 6.2, S16, S7, S17 |
| chatstorage.sql | ZWAGROUPINFO | from-memory | research 6.2, S16 |
| chatstorage.sql | ZWAPROFILEPUSHNAME | from-memory | research 6.2, S1 and S7 |
| chatstorage.sql | ZWAGROUPMEMBERSCHANGE | from-memory | research 6.2, S7 |
| chatstorage.sql | Z_PRIMARYKEY | from-memory | research 6.1, Core Data bookkeeping as callhistory-coredata.sql in schemas/ios |
| chatstorage.sql | Z_METADATA | from-memory | research 6.1, Core Data bookkeeping as callhistory-coredata.sql in schemas/ios |
| callhistory.sql | ZWACDCALLEVENT | from-memory | research 6.3, S1, S7, S12, S18 |
| callhistory.sql | ZWAAGGREGATECALLEVENT | from-memory | research 6.3, S1, S7, S18 |
| callhistory.sql | ZWACDCALLEVENTPARTICIPANT | from-memory | research 6.3, S7, S18 |
| callhistory.sql | Z_PRIMARYKEY | from-memory | research 6.1, Core Data bookkeeping |
| callhistory.sql | Z_METADATA | from-memory | research 6.1, Core Data bookkeeping |
| contactsv2.sql | ZWAADDRESSBOOKCONTACT | from-memory | research 6.4, S7, S12, S1 (ZLID absent on some images, S7) |
| contactsv2.sql | Z_PRIMARYKEY | from-memory | research 6.1, Core Data bookkeeping |
| contactsv2.sql | Z_METADATA | from-memory | research 6.1, Core Data bookkeeping |

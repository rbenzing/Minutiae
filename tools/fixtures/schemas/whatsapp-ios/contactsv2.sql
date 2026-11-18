-- WhatsApp iOS ContactsV2.sqlite, Core Data store (plan 4I2).
-- source S16 (insert lists)
-- source S1
-- source S7
-- No DDL of this store was found in any source: the table is CONSTRUCTED from the column list the sources report
-- (research section 6.4), in Core Data form (Z_PK, Z_ENT, Z_OPT, then the columns), with declared types chosen by the
-- writer (INTEGER, VARCHAR, TIMESTAMP, FLOAT, BLOB, as callhistory-coredata.sql uses). The column types are not checked
-- against a public definition. ZLID is absent on some images (S7). Per-table verdict: CROSSCHECK.md.
-- verdict from memory
CREATE TABLE ZWAADDRESSBOOKCONTACT (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZABOUTTIMESTAMP TIMESTAMP, ZABOUTTEXT VARCHAR, ZFULLNAME VARCHAR, ZGIVENNAME VARCHAR, ZHIGHLIGHTEDNAME VARCHAR, ZIDENTIFIER VARCHAR, ZLID VARCHAR, ZPHONENUMBER VARCHAR, ZPHONENUMBERLABEL VARCHAR, ZWHATSAPPID VARCHAR);
CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER);
CREATE TABLE Z_METADATA (Z_VERSION INTEGER PRIMARY KEY, Z_UUID VARCHAR(255), Z_PLIST BLOB);

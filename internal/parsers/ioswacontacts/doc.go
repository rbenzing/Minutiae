// Package ioswacontacts is the iOS WhatsApp contacts parser of plan 4I2: ios.whatsapp.contacts, which reads ContactsV2.sqlite
// (the address-book table ZWAADDRESSBOOKCONTACT of a Core Data store) for WhatsApp and WhatsApp Business. It is a pure parser:
// it reads only the files the host hands it, imports no other parser and holds no state.
//
// A row that names no person (a group, broadcast or other non-user JID, a JID with no name or number) is a counted warning,
// never a contact; two rows with one JID are two contacts, never merged; the LID statement is the row's own ZLID and is never
// inferred. The package also holds the test fixtures and their oracles (tools/fixtures/whatsapp_ios_contacts.py).
package ioswacontacts

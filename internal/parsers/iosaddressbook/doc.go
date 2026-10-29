// Package iosaddressbook parses the address book of an iOS device, AddressBook.sqlitedb, into contact
// records, one per ABPerson row. The layout is written from memory (plan 4E ruling 4E-R13): no public
// source defines it, so a real database that differs degrades loudly (unsupported-schema, absent
// fields, a counted warning), never silently.
//
// Rules fixed by the plan and pinned by the fixtures of this package:
//   - A birthday is not derived (4E-R23): turning a stored instant into a calendar date needs a time
//     zone the source does not hold, so payload.birthday stays absent and raw.birthday keeps the
//     stored value.
//   - The display name is the stored DisplayName column when it is present and non-empty
//     (raw.display_name_source "stored"), else Prefix First Middle Last Suffix joined by single spaces
//     over the non-empty parts ("composed"), else absent (4E-R24).
//   - Photo presence comes only from the companion AddressBookImages.sqlitedb and is false only when
//     both image tables are decided for the person (4E-R25, 4E-R41).
//
// This package holds the fixtures and their scenario oracle (Task 13); the parser follows in Tasks 14
// and 15.
package iosaddressbook

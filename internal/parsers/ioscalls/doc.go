// Package ioscalls parses the call history of an iOS device into call records: the Core Data store
// CallHistory.storedata (table ZCALLRECORD) and the older call_history.db (table call). Both layouts
// are written from memory (plan 4E ruling 4E-R13): no public source defines them, so a real database
// that differs degrades loudly (not-applicable, unsupported-schema, absent fields), never silently.
//
// Two rules depart from guesswork on purpose (rulings 4E-R21, R22 and R35):
//   - The bits of the legacy call.flags column are not interpreted: direction and outcome are
//     "unknown" and the stored value is kept in raw.flags.
//   - An outgoing Core Data call with ZANSWERED = 0 has the outcome "unknown", never "cancelled": the
//     value also covers busy, failed and not answered. raw.ZANSWERED, raw.ZDURATION and
//     raw.ZDISCONNECTED_CAUSE keep what the database holds.
//
// This package holds the fixtures and their scenario oracle (Task 11); the parser follows in Task 12.
package ioscalls

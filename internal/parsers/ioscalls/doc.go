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
// The parser is a RowMapper over rowrun with two optional mappings, ZCALLRECORD and call (ruling 4E-R30):
// the file is identified by which table set it holds, never by its name or by a guess. Numbers and Apple-epoch
// instants are read through cellx; a cell of another storage class is named in raw.unknown_columns and gives no
// value; a non-finite duration is stated by its IEEE bits (raw.ZDURATION_ieee), never as a value.
package ioscalls

// Package safari parses the Safari browsing history of an iOS device (History.db: history_items,
// history_visits and history_tombstones) into web_visit records. The layout is written from memory
// (plan 4F ruling 4F-R12): no public source defines it, so a real database that differs degrades
// loudly (not-applicable, unsupported-schema, absent fields), never silently.
//
// Records, in step order:
//   - history_items: only an item without any visit, and only on a decided miss of the visits join
//     (raw.visitless = true; no Time, no visit id). When the join cannot be decided the record is emitted
//     with raw.join_undecided = ["history_visits"] and no raw.visitless.
//   - history_visits: one record per visit, joined to its item by rowid (the item's id cell must equal the
//     rowid). A visit whose item is absent, unreadable or without a usable URL is a warning, never a record
//     with an invented URL. Time is visit_time (Cocoa seconds) when it converts.
//   - history_tombstones: a row with a usable url is a record that is Deleted (the application's own marker,
//     no Recovery), has no Time and states its range as the Times deleted_range_start and deleted_range_end.
//     Rows without a usable url (NULL, empty or not text) are counted in the note tombstones_without_url.
//
// Raw keys (payload.raw), beyond the shared vocabulary of the 4D parsers (unknown_columns,
// joined_unknown_columns keyed "history_items.<column>", join_undecided, text_flags, row_flags,
// <column>_status):
//   - visit_time, start_time, end_time: the stored REAL when finite; <column>_status "sentinel" (0, -0, -1) or
//     "invalid"; a non-finite REAL is stated only as <column>_ieee, 16 hex digits of its IEEE bits.
//   - redirect_source, redirect_destination, http_non_get, synthesized, origin, generation, attributes, score:
//     the stored integers, uninterpreted. status_code 0 (not recorded) is stated here, not in the payload.
//   - url_truncated, url_total_bytes, title_total_bytes, <field>_raw_b64, <field>_raw_sha256, <field>_raw_len,
//     body_truncated, body_total_bytes: the caps and the original bytes of altered text.
//   - visitless, join_undecided: see history_items above.
//
// unknown_columns values are "type-mismatch", a decoder state name, "negative" (a negative visit_count) or
// "not-boolean" (load_successful holding an integer other than 0 and 1).
package safari

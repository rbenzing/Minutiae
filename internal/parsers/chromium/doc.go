// Package chromium is the chromium.history parser of sub-project 4 (plan 4F): Chromium-family and embedded WebView
// History databases, recognised by schema signature (urls, visits, keyword_search_terms, downloads and meta).
//
// The browser, profile and package come from the logical path alone: the last app_chrome or app_webview segment
// decides (a webview directory is "webview", an unrecognised package "unknown-chromium"). Times are WebKit
// microseconds read through decode/ts. visits.transition is read as a uint32 (a server redirect is stored negative):
// the core is named from the cross-checked table, the qualifiers follow in ascending bit order, a core that is no
// known transition is "unknown".
//
// A visits row needs its urls row, found by rowid (urls.id must be the rowid alias, which the mapper checks on the
// found row): an absent, undecidable or url-less urls row is a row error (a counted analysis.warning), never an
// invented URL. A urls row is a record only when no visit names it, decided by a clean miss in visits
// (raw.visitless = true); when that cannot be decided the record states raw.join_undecided = ["visits"] and claims
// neither. A urls row that visits name gives no record of its own.
//
// Raw keys (payload.raw): package; url_id, visit_time, last_visit_time, transition, transition_unknown_bits,
// from_visit, opener_visit, originator_visit_id, visit_duration (stored integers; <col>_status when an instant is a
// sentinel or invalid); originator_cache_guid, external_referrer_url (stored text); visitless; join_undecided;
// unknown_columns (cells of the mapped row: type-mismatch, a decoder state name, precision, negative, out-of-range,
// not-boolean); joined_unknown_columns ("urls.<col>" of the joined urls row, plus column-missing); text_flags and
// <field>_raw_b64 / _raw_sha256 / _raw_len / _total_bytes (cleaned or cut text), url_truncated; body_truncated and
// body_total_bytes (the 64 KiB cap per URL or title in the searchable body); row_flags. The meta table gives no
// record: it notes meta.version and meta.last_compatible_version ("non-numeric" for a value that is not decimal, and
// "unknown" for a value the decoder cannot vouch for, never "non-numeric").
//
// A keyword_search_terms row is a web_search: its time and url come from the urls row named by url_id and are stated
// as that URL's last visit (time_source "urls.last_visit_time"); a url_id with no urls row is raw.url_state
// "no-urls-row", an undecidable one raw.join_undecided ["urls"]; the engine is never stated. A downloads row is a
// download: its url_chain is the downloads_url_chains rows ordered by chain_index (ties by rowid) and its url the last
// entry; raw.url_chain_state is "no-chain-rows", "gaps", "duplicate-index", "unknown-index" (a chain_index that is
// not an integer) or "unknown-url" (an entry with no URL text), the last two stating no chain at all. At most 1000
// entries and 2 MiB of URL bytes are kept: raw.url_chain_total, url_chain_truncated, url_chain_bytes_truncated and
// url_chain_bytes_total say what was cut. state, danger_type and interrupt_reason are the names of the cross-checked
// integer tables, any other value "unknown" (the integer is raw.<column>); total_bytes 0 is "not recorded" (no
// total_bytes, raw.total_bytes 0). A search or download keeps its profile in raw.profile. Further raw keys: start_time,
// end_time, last_access_time (+_status), guid, original_mime_type, tab_referrer_url, site_url, http_method, transient,
// hash (hex), body_truncated and body_total_bytes (parts cut at 64 KiB, the whole body at 1 MiB).
package chromium

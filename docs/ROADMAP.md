# Minutiae Roadmap

Goal: a mobile-device forensics tool that finds the smallest details on a
device — including hidden and deleted data — competitive with commercial
forensic suites.

This roadmap is binding. Every piece of work belongs to exactly one
sub-project below. Each sub-project goes through its own
spec → plan → implementation cycle and its status is updated here when it
changes. Do not start a sub-project before its dependencies are `Done`
unless this file is updated to say why.

Status values: `Not started` · `In progress` · `Done` (the detail is in the status notes under each table)

| # | Sub-project | Scope | Depends on | Status |
|---|---|---|---|---|
| 1 | Foundation + Acquisition | Case/evidence core (hash-chained audit log, manifest, `artifacts.db`, verify), device abstraction, USB serial transport + raw console, Android ADB (info, pull/push, logical, rooted partition imaging), iOS (info, AFC pull, logical backup), CLI | — | Done |
| 2 | Image & filesystem layer | Open raw/dd and E01 images; GPT/MBR; read ext4, F2FS, APFS, HFS+, FAT, exFAT; expose unallocated space | 1 | Done |
| 3 | Deleted data recovery | SQLite freelist/WAL/journal record recovery; signature-based file carving; slack space; ext4 journal | 2 | In progress |
| 4 | Artifact parsers (plugin system) | SMS/MMS, calls, contacts, calendar, browser history, WhatsApp, Telegram, Signal (where decryptable), Instagram and others, as plugins writing to `artifacts.db` | 2, 3I, 5 (4G also 3A, 3J) | In progress |
| 5 | Unified artifact database | Indexed store of all parsed records with provenance (artifact, path, offset, deleted flag); full-text keyword search | 1 | Done |
| 6 | Analytics | Timeline, geolocation/map, communication graph, keyword lists, hash sets (NSRL / known-bad) | 4, 5 | Not started |
| 7 | Reporting | HTML/PDF/CSV reports with hashes and chain of custody | 5 | Not started |
| 8 | Protocol drivers | AT commands, Qualcomm EDL/diag, MediaTek BROM, UART console | 1 | Not started |
| 9 | Desktop GUI | Case browser, viewers, timeline/map UI, built on `internal/` core | 1, 6 | Not started |
| 10 | Encrypted data / keychain | Backup password handling, app database decryption where keys are available | 4 | Not started |
| 11 | Artifact classification & categorization | Classifier model automatically assigns categories (e.g. image content classes, document types, conversation topics) to artifacts and records, with confidence scores; filter and navigate the case by category. CLI first, GUI view via 9 | 4, 5 | Not started |
| 12 | AI artifact search & analysis | Natural-language chat over the artifact collection: searches `artifacts.db`, retrieves specific details and returns numbers, keywords and other structured answers, each citing the artifacts and records it came from. CLI first, GUI panel via 9 | 5 | Not started |

Status notes (sub-projects):
- 3 Deleted data recovery: 3A foundation, 3I SQLite file library and 3J SQLite record recovery done: `sqlite recover` turns deleted, WAL, journal and slack rows into `db_row` records; 3B FAT/exFAT deleted-file recovery done; 3D F2FS deleted-file recovery done (`f2fs-node-scan`, kernel-written nodiscard and discard fixtures; the F2FS reader's `offsetBitShift` was corrected from 7 to 3 on kernel evidence, which changed the live reader only for one hostile-input edge case); 3E signature carving done: `image carve` carves JPEG, PNG, GIF, WebP, ISO-BMFF (MP4, MOV, 3GP, HEIC/HEIF), SQLite, binary plist, ZIP (APK, JAR, OOXML), PDF, gzip, WAV and Ogg objects from unallocated space, volume gaps, raw scopes and artifacts; 3E2 more image formats done: BMP, TIFF/BigTIFF with DNG, CR2, ORF and RW2 identity, AVIF, CR3, the JPEG XL container and ICO/CUR in the built-in set `minutiae-sigset/2`; camera-raw types builder-only; the bare JPEG XL codestream is not carved; 3C, 3F-3H, 3K pending
- 4 Artifact parsers (plugin system): 4D done: android.mmssms, android.calllog and android.contacts on the 4B decoder; 4F done: `chromium.history` (Chromium-family and WebView `History` by schema signature: visits, visitless URLs, searches, downloads) and `safari.history` (visits, visitless items, application tombstones) with cross-checked (Chromium) and from-memory (Safari) fixtures; 4G done: `parse recover` and `sqlite recover` type the recovered rows of the tables the five shipped parsers claim (`android.mmssms`: `sms`, `pdu`; `android.calllog`: `calls`; `android.contacts`: `raw_contacts`, `deleted_contacts`; `chromium.history`: `visits`, `keyword_search_terms`, `downloads`; `safari.history`: `history_visits`) under `<name>.recover`, every recovered row covered once as a typed record, a counted unmapped warning or a `db_row`; recovered and carved artifacts are parser input with `--include-recovered`; 4E done: the `ios-backup` namer maps backup files from each backup's hash-checked `Manifest.db`, encrypted backups are reported unparsed (sub-project 10); `ios.sms`, `ios.calls` and `ios.addressbook` on from-memory schemas; 4H done: `parse plan --scan` lists renamed or relocated databases as candidates (run only with `--artifact`); `case verify` cross-checks parse jobs against their ingests; every first-set SQLite parser claims exactly its claimable tables (`ios.sms`: `message`; `ios.calls`: `ZCALLRECORD`, `call`; `ios.addressbook`: `ABPerson`) and typed and generic recovery read iOS backup bundles; Chromium keeps the bits of a REAL in an integer column; a `realdata` harness exists (no real-data run recorded); parse throughput and memory are measured (`docs/performance.md`); parsers stay in-process (spec 17 Q7). The first parser set is complete; the row stays In progress through the app parsers (4I+); 4A started before 3I and 5 are Done: it needs only the finished records writer of 5A, 4B needs 3I, 4G needs 3A and 3J; 3J is done, so 4G can read `db_row` records, applies table claims and the `fit`/`guess` basis rule itself, and owns timestamps
- 5 Unified artifact database: 5A to 5D: schema v3 store, verified full-text search, checked provenance chain, scale work. Measured at 10,000,000 records on the reference laptop: search word 121 ms and substring 128 ms pass, list p95 19.6 ms passes; recorded misses: `case verify` 3,094 s, 3,232 records/s against the amendment A10 target of 3,700 (87.3%, the gap is SQLite's own `PRAGMA integrity_check`, P10, which cannot be split), stats by type 42.8 s against 3 s, ingest 1,630 records/s against 40,000 and database size 3.90 against 1.6; see `docs/performance.md` and `docs/limitations.md`
- 6 Analytics: may start before row 4 is Done (parsers spec, coordinator ruling 2): timeline and graph need only the first set's record types, complete with 4H

AI-derived results (11, 12) are investigative leads, never evidence:
- they are stored apart from acquired artifacts and never change them;
- each result records the model identity, version and file hash, plus a confidence score where the model gives one;
- every answer cites the artifact and record IDs it is based on;
- models run offline by default. Case data leaves the host only if the examiner explicitly configures a remote model, and that choice is audited.

The model and its runtime are chosen in each sub-project's spec, within the default build staying single-binary and cgo-free; cgo is allowed only as the cgo policy in docs/architecture.md permits (a needed capability with no pure-Go route, fenced in dedicated packages with a `!cgo` fallback).

Deferred (needs a separate legal/authority discussion before any spec):
cloud account extraction.

## Limitations fix program (LX)

Plans that close or mitigate entries in `docs/limitations.md` without weakening any invariant.
Each belongs to the sub-project that owns the code it changes and does not change that
sub-project's scope.

| Plan | Area | Sub-project | Status |
|---|---|---|---|
| LX-0 | Stale docs | — (docs only) | Done |
| LX-1 | Acquisition hardening (`internal/ios`, `internal/android`) | 1 | Done |
| LX-2 | APFS and HFS+ gaps, shared decmpfs | 2 | Done |
| LX-3 | EWF and image commands | 2 | Done |
| LX-4 | SQLite reader and table decoder | 3 (3I), 4 (4B) | Done |
| LX-5 | Verify and audit | 1, 5 | Done |
| LX-6 | Records, full-text search and JSON output | 5 | Done |
| LX-7a | ext4, F2FS and shared filesystem readers | 2 | Done |
| LX-7b | FAT and exFAT readers | 2 | Done |
| LX-8 | Parse contract, plist decoder and fuzz tooling | 4 | Done |

Status notes (LX plans):
- LX-5: L67/L101 audit anchors, L98 allocation recheck, L69 records --verify and the not-verified notice, L76, L77, L97, L100, acquisition-gap recompute, cut recovery trail
- LX-6: L72 terminal-safe JSON, L70 run index, L85 --scan, L86 CJK letters as tokens with pipeline version 4, L90 quote-aware scanner
- LX-8: follow-up: the post-LX-8 fuzz campaigns are pending, see below

Follow-ups from sub-project 5 (5D, plan `2026-10-08-5d-scale.md` with the verify amendment; none is scheduled):
- (a) Whether throttling the record-stream and index-pass workers while `PRAGMA integrity_check` (P10) runs reduces contention: P10 took 3,093 s beside the other work against 2,555 s alone, and the verify time at 10,000,000 records is P10 time (3,094 s against the 2,703 s budget of the 3,700 records/s target).
- (b) Single-pass localization of failing full-text buckets: each failing batch costs about 76 s at 2,000,000 records and 64 failing batches projected to about 12 hours at 10,000,000.
- (c) A partial index `records_recovered` (schema v4) so the recovered count in `records stats` is index-only (44.0 s cold at 10,000,000 records).
- (d) A non-indexed selective filter (a path prefix with a common word) costs 1,043 ms at p95 at 10,000,000 records (finding S13); it needs an index or a probe for that filter.
- Also open: a time-ordered index for artifact, parser, path-prefix and untimed-range filters if casework needs it (no schema v4 in 5D), `Reader.Get` reads the whole audit log (172 to 274 ms), and converting the 5C timing assertions in default tests to work counters.

Follow-up from LX-8: the three post-LX-8 fuzz campaigns (the plist decoder, the readers profile and the parsers profile) are not yet run. The Docker VM had 1 GiB, and the engine died on each attempt. Run them once the Docker VM has at least 4 GiB, using the commands in `docs/testing.md` ("Re-run after LX-8"). Then replace the "pending" notes in `docs/testing.md` and `docs/limitations.md` with the results.

Follow-ups from 4G (none scheduled; 4H closed the claims of the 4E parsers and the claims check of every first-set parser): `guess`-basis rows stay generic (4G-R3: identity never from a guess; revisit only with casework); typed recovery is unvalidated on real device databases (class C; the `realdata` harness exists, no run is recorded).

Follow-ups from 4E (none scheduled; 4H closed the iOS claims and the backup siblings of `dbrecover`): the Linux fuzz-rate measurement (the plan's 0.4 exec/s bar for `FuzzParseIOSSMS`, `FuzzParseIOSCalls` and `FuzzParseAddressBook` is a Linux figure, still unmeasured; Windows measured 0.08 and 0.15 exec/s, see `docs/testing.md`; it needs Docker with at least 4 GiB); and the plan's backlog: unify `internal/ios/manifestdb` with `internal/decode/iosbackup` (a sub-project 1 refactor with no evidence gap); the previous texts of edits (`recoverable_message_part`, `message_summary_info`) and the `payload_data` fields (new extraction from memory without a sample, an addendum for 4I+); AddressBook change history (same); a `parse plan` grouping of unnamed backup artifacts by reason (the counts by reason exist; no further defect is stated); REAL bits for the seven non-Chromium parsers; a recorded real-data run (an operator step); Chromium `visit_source` and Chrome-on-iOS globs (`--scan` finds such files as candidates).

Follow-ups from LX-5, LX-6 and LX-7b (none is scheduled):
- Every case indexed before LX-6 (full-text pipeline version 3) needs `records reindex` before a text search; `case verify` reports it as a notice, not a problem.
- The race gate's named `internal/evidence` tests take about 22 of their 30 minutes under `-race` on a loaded machine; profile the list before it outgrows the limit.
- `records search --scan` over a tampered case may read one oversized record body before it refuses it (bounded to that record).
- The manifest reader has no per-line length cap.
- The TexFAT handling of the exFAT reader is checked on builder images only; a real TexFAT image would be its first independent oracle.

## Specs and plans

Specs and plans live in `docs/superpowers/` (gitignored, local only).

| # | Spec | Plan |
|---|---|---|
| 1 | `docs/superpowers/specs/2026-10-02-foundation-acquisition-design.md` | `docs/superpowers/plans/2026-10-02-1a-foundation.md`, `…-1b-serial.md`, `…-1c-android.md`, `…-1d-ios.md` |
| 2 | `docs/superpowers/specs/2026-10-03-image-filesystem-design.md` | `docs/superpowers/plans/2026-10-03-2a-foundation.md`, then `…-2b-ext4.md`, `…-2c-fat-exfat.md`, `…-2d-f2fs.md`, `…-2e-ewf.md`, `…-2f-apfs.md`, `…-2g-hfsplus.md` |
| 3 | `docs/superpowers/specs/2026-10-05-deleted-data-recovery-design.md` | `docs/superpowers/plans/2026-10-05-3i-sqlite-file.md` (3I SQLite file library; done: the pure-Go reader `internal/sqlitefile` with `Live()` and `History()`) and `docs/superpowers/plans/2026-10-05-3a-recovery-foundation.md` (3A foundation; done), `docs/superpowers/plans/2026-10-08-3j-sqlite-record-recovery.md` (3J SQLite record recovery; done: `internal/dbrecover`, `sqlite recover`), `docs/superpowers/plans/2026-10-08-3b-fat-exfat-recovery.md` (3B FAT/exFAT deleted-file recovery; done), `docs/superpowers/plans/2026-10-24-3d-f2fs-recovery.md` (3D F2FS deleted-file recovery; done), `docs/superpowers/plans/2026-10-24-3e-carving.md` (3E signature carving; done), `docs/superpowers/specs/2026-10-29-3e2-image-formats-design.md` with `docs/superpowers/plans/2026-10-29-3e2-image-formats.md` (3E2 more image formats; done), then 3C-3H, 3K as they appear |
| 4 | `docs/superpowers/specs/2026-10-05-artifact-parsers-design.md` | `docs/superpowers/plans/2026-10-05-4a-parser-contract.md` (4A, parser contract, host, registry, `parse list\|plan\|run`), `docs/superpowers/plans/2026-10-08-4b-table-decoder.md` (4B, SQLite table decoder), `docs/superpowers/plans/2026-10-08-4c-other-decoders.md` (4C, done: timestamp, plist and typedstream decoders), `docs/superpowers/plans/2026-10-08-4d-android-telephony.md` (4D, done: Android SMS/MMS, call log and contacts parsers), `docs/superpowers/plans/2026-10-24-4f-browsers.md` (4F, done: Chromium and Safari browser history parsers), `docs/superpowers/plans/2026-10-24-4g-typed-recovery.md` (4G, done: typed recovery and claims), `docs/superpowers/plans/2026-10-24-4e-ios-system.md` (4E, done: iOS backup namer, iOS SMS, call history and AddressBook parsers), `docs/superpowers/plans/2026-10-30-4h-first-set-closeout.md` (4H, done: first-set close-out), then 4I+ as they appear (4B needs plan 3I) |
| 5 | `docs/superpowers/specs/2026-10-04-artifact-database-design.md` | `docs/superpowers/plans/2026-10-04-5a-records-core.md` (5A, done: schema v2, audited writer, verify, reader, `records list`, `show`, `stats`), `docs/superpowers/plans/2026-10-05-5b-full-text.md` (5B, done: schema v3, verified full-text index, `records search`, `records reindex`, P11), `docs/superpowers/plans/2026-10-08-5c-provenance.md` (5C, done: checked provenance chain, image-relative offsets, runs sidecar reader, recovered-data status in `records show`), `docs/superpowers/plans/2026-10-08-5d-scale.md` (5D, done: scale numbers, tuned ingest, capped stats, merged multi-value lists, search plan bands, parallel verify with the P11 digest, recorded misses) |
| LX | `.superpowers/sdd/limitations-program.md` (program and triage) | `docs/superpowers/plans/2026-10-08-lx1-acquisition-hardening.md`, `…-lx2-apfs-hfs.md`, `…-lx3-ewf-image.md`, `docs/superpowers/plans/2026-10-14-lx4-sqlite.md`, `docs/superpowers/plans/2026-10-23-lx5-verify-audit.md`, `…-lx7a-ext4-f2fs-filesys.md`, `…-lx7b-fat-exfat.md`, `…-lx8-parse-plist-fuzz.md`, `docs/superpowers/plans/2026-10-23-lx6-records-fts-json.md` |

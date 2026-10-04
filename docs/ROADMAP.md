# Minutiae Roadmap

Goal: a mobile-device forensics tool that finds the smallest details on a
device — including hidden and deleted data — competitive with commercial
forensic suites.

This roadmap is binding. Every piece of work belongs to exactly one
sub-project below. Each sub-project goes through its own
spec → plan → implementation cycle and its status is updated here when it
changes. Do not start a sub-project before its dependencies are `Done`
unless this file is updated to say why.

Status values: `Not started` · `Spec` · `Planned` · `In progress` · `Done`

| # | Sub-project | Scope | Depends on | Status |
|---|---|---|---|---|
| 1 | Foundation + Acquisition | Case/evidence core (hash-chained audit log, manifest, `artifacts.db`, verify), device abstraction, USB serial transport + raw console, Android ADB (info, pull/push, logical, rooted partition imaging), iOS (info, AFC pull, logical backup), CLI | — | Done |
| 2 | Image & filesystem layer | Open raw/dd and E01 images; GPT/MBR; read ext4, F2FS, APFS, HFS+, FAT, exFAT; expose unallocated space | 1 | Done |
| 3 | Deleted data recovery | SQLite freelist/WAL/journal record recovery; signature-based file carving; slack space; ext4 journal | 2 | Spec |
| 4 | Artifact parsers (plugin system) | SMS/MMS, calls, contacts, calendar, browser history, WhatsApp, Telegram, Signal (where decryptable), Instagram and others, as plugins writing to `artifacts.db` | 1, 2 | Spec |
| 5 | Unified artifact database | Indexed store of all parsed records with provenance (artifact, path, offset, deleted flag); full-text keyword search | 1 | In progress |
| 6 | Analytics | Timeline, geolocation/map, communication graph, keyword lists, hash sets (NSRL / known-bad) | 4, 5 | Not started |
| 7 | Reporting | HTML/PDF/CSV reports with hashes and chain of custody | 5 | Not started |
| 8 | Protocol drivers | AT commands, Qualcomm EDL/diag, MediaTek BROM, UART console | 1 | Not started |
| 9 | Desktop GUI | Case browser, viewers, timeline/map UI, built on `internal/` core | 1, 6 | Not started |
| 10 | Encrypted data / keychain | Backup password handling, app database decryption where keys are available | 4 | Not started |
| 11 | Artifact classification & categorization | Classifier model automatically assigns categories (e.g. image content classes, document types, conversation topics) to artifacts and records, with confidence scores; filter and navigate the case by category. CLI first, GUI view via 9 | 4, 5 | Not started |
| 12 | AI artifact search & analysis | Natural-language chat over the artifact collection: searches `artifacts.db`, retrieves specific details and returns numbers, keywords and other structured answers, each citing the artifacts and records it came from. CLI first, GUI panel via 9 | 5 | Not started |

AI-derived results (11, 12) are investigative leads, never evidence:
- they are stored apart from acquired artifacts and never change them;
- each result records the model identity, version and file hash, plus a confidence score where the model gives one;
- every answer cites the artifact and record IDs it is based on;
- models run offline by default. Case data leaves the host only if the examiner explicitly configures a remote model, and that choice is audited.

The model and its runtime are chosen in each sub-project's spec, within the no-cgo, single-binary constraint.

Deferred (needs a separate legal/authority discussion before any spec):
cloud account extraction.

## Specs and plans

Specs and plans live in `docs/superpowers/` (gitignored, local only).

| # | Spec | Plan |
|---|---|---|
| 1 | `docs/superpowers/specs/2026-10-02-foundation-acquisition-design.md` | `docs/superpowers/plans/2026-10-02-1a-foundation.md`, `…-1b-serial.md`, `…-1c-android.md`, `…-1d-ios.md` |
| 2 | `docs/superpowers/specs/2026-10-03-image-filesystem-design.md` | `docs/superpowers/plans/2026-10-03-2a-foundation.md`, then `…-2b-ext4.md`, `…-2c-fat-exfat.md`, `…-2d-f2fs.md`, `…-2e-ewf.md`, `…-2f-apfs.md`, `…-2g-hfsplus.md` |
| 3 | `docs/superpowers/specs/2026-10-05-deleted-data-recovery-design.md` | `docs/superpowers/plans/2026-10-05-3a-…` and on (3A foundation, 3I SQLite file library first) |
| 4 | `docs/superpowers/specs/2026-10-05-artifact-parsers-design.md` | `docs/superpowers/plans/2026-10-05-4a-…` and on (4A contract and host; 4B needs plan 3I) |
| 5 | `docs/superpowers/specs/2026-10-04-artifact-database-design.md` | `docs/superpowers/plans/2026-10-04-5a-records-core.md` (5A, done: schema v2, audited writer, verify, reader, `records list`, `show`, `stats`), then `…-5b-…`, `…-5c-…` and `…-5d-…` as they appear |

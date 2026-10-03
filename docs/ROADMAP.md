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
| 2 | Image & filesystem layer | Open raw/dd and E01 images; GPT/MBR; read ext4, F2FS, APFS, HFS+, FAT, exFAT; expose unallocated space | 1 | Not started |
| 3 | Deleted data recovery | SQLite freelist/WAL/journal record recovery; signature-based file carving; slack space; ext4 journal | 2 | Not started |
| 4 | Artifact parsers (plugin system) | SMS/MMS, calls, contacts, calendar, browser history, WhatsApp, Telegram, Signal (where decryptable), Instagram and others, as plugins writing to `artifacts.db` | 1, 2 | Not started |
| 5 | Unified artifact database | Indexed store of all parsed records with provenance (artifact, path, offset, deleted flag); full-text keyword search | 1 | Not started |
| 6 | Analytics | Timeline, geolocation/map, communication graph, keyword lists, hash sets (NSRL / known-bad) | 4, 5 | Not started |
| 7 | Reporting | HTML/PDF/CSV reports with hashes and chain of custody | 5 | Not started |
| 8 | Protocol drivers | AT commands, Qualcomm EDL/diag, MediaTek BROM, UART console | 1 | Not started |
| 9 | Desktop GUI | Case browser, viewers, timeline/map UI, built on `internal/` core | 1, 6 | Not started |
| 10 | Encrypted data / keychain | Backup password handling, app database decryption where keys are available | 4 | Not started |

Deferred (needs a separate legal/authority discussion before any spec):
cloud account extraction.

## Specs and plans

Specs and plans live in `docs/superpowers/` (gitignored, local only).

| # | Spec | Plan |
|---|---|---|
| 1 | `docs/superpowers/specs/2026-10-02-foundation-acquisition-design.md` | `docs/superpowers/plans/2026-10-02-1a-foundation.md`, `…-1b-serial.md`, `…-1c-android.md`, `…-1d-ios.md` |

# Minutiae

<div align="center">

[![Release](https://img.shields.io/github/v/release/rbenzing/Minutiae?style=for-the-badge)](https://github.com/rbenzing/Minutiae/releases/latest)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey?style=for-the-badge)](https://github.com/rbenzing/Minutiae/releases)
[![CGO](https://img.shields.io/badge/cgo-none-success?style=for-the-badge)](#-building-from-source)
[![Buy Me A Coffee](https://img.shields.io/badge/Buy%20Me%20A%20Coffee-FFDD00?style=for-the-badge&logo=buymeacoffee&logoColor=black)](https://buymeacoffee.com/russellbenzing)

**Forensically sound acquisition for mobile devices — finding the smallest details, including hidden and deleted data**

🔐 **Hash-Chained Audit Log** • 📱 **Android · iOS · USB Serial** • 🧾 **Verifiable Evidence** • 🐹 **Go**

[Features](#-features) • [Installation](#-installation) • [Usage](#-usage) • [Security](#-security-model) • [Roadmap](docs/ROADMAP.md)

</div>

---

**Minutiae** is a mobile-device forensics tool written in Go. Version 1.0 is the
foundation: it acquires evidence from Android phones (ADB), iPhones/iPads
(usbmuxd) and USB serial ports into a **case** where every byte is hashed,
every action is recorded in a tamper-evident audit log, and the whole case can
be re-verified at any time.

> Evidence integrity is the product. If a convenient shortcut weakens it, it's a bug.

> ⚠️ **v1.0.0 status:** the device backends have been tested extensively against
> protocol-faithful fakes but have **not yet been validated on real hardware**.
> Run the [hardware acceptance steps](#-hardware-acceptance) on authorized test
> devices before relying on Minutiae in casework.

Use Minutiae only on devices you are authorized to examine.

---

## ✨ Features

- **Tamper-evident audit log** — every action is an append-only, SHA-256 hash-chained JSON line; editing, deleting or reordering any entry is detected
- **Hashed, never-overwritten artifacts** — every acquired file is streamed through SHA-256 + MD5, created with `O_EXCL`, and recorded in `manifest.jsonl`, `artifacts.db` and the audit log
- **`case verify`** — re-hashes every artifact and cross-checks files ↔ manifest ↔ database ↔ audit chain (including each artifact's recorded provenance); any discrepancy exits with code `4`
- **Partial evidence is kept, never hidden** — a cancelled (Ctrl-C), interrupted or short transfer keeps its bytes and is flagged `incomplete`
- **Read-only by default** — writing to a device (`android push`, serial transmit, DTR/RTS) requires `--allow-device-write` and is audited *before* any byte is sent
- **Android over ADB** — native pure-Go ADB client: device info, file listing/pull, logical acquisition of `/sdcard` (plus `getprop` and package list), and partition imaging on rooted devices with exact size verification
- **iOS over usbmuxd** — device info, AFC media listing/pull, and full logical backups via an in-house mobilebackup2 (DeviceLink) implementation hardened against hostile devices
- **USB serial** — port enumeration with VID/PID, receive-only raw console capture (`rx.bin` + timestamped transcript), modem lines de-asserted on open
- **Image analysis** — import disk images (raw/dd, split raw, **E01/EWF** with multiple segments) into a case, read MBR/EBR and GPT partition tables, browse filesystems read-only, extract files and export unallocated space as new hashed artifacts that record their full provenance (parent image, partition, filesystem entry, byte runs). **ext2/ext3/ext4** (extents or block maps, htree and inline directories, inline data, metadata checksums), **FAT12/16/32** (long names, 2-second local times) and **exFAT** (entry sets, validated checksums, `ValidDataLength`), **F2FS** (checkpoint packs, NAT/SIT journals, inline data and dentries, encrypted names), **HFS+/HFSX** (classic-HFS wrapper, B-tree catalog with extents overflow, hard links, zlib-compressed files, per-file data-protection flag) and **APFS** (checkpoint ring, object maps, B-trees, extents with holes and clones, hard links, extended-attribute names, name hashes with normalization-insensitive lookup, read-only snapshots as views, space-manager free space; encrypted volumes are detected and refused, compressed files are listed but not extracted) filesystems are readable, with deleted-entry flagging where the format keeps deleted names (names only; HFS+ and APFS keep none) and exact unallocated-space export; each reader is verified against real images built by the standard filesystem tools with independent expected results (tree, hashes, times, cluster chains or data extents, free space; the populated HFS+ and APFS images are written by Linux kernel drivers under QEMU, with nothing loaded into the host kernel) and fuzzed against hostile input; E01 images are verified against the hashes the acquirer stored (`image info --verify`, audited) and an unreadable chunk is an error, never zeros; see [Known limitations](CLAUDE.md#9-known-limitations)
- **Unified records database** — parsed records (messages, calls, contacts, web visits, files, events ...) live in `artifacts.db` with full provenance (artifact, source path, locator, byte range, parser identity and version, deleted/recovered flags, microsecond timestamps with their time-zone basis). Every batch of records is announced in the hash-chained audit log with a digest *before* any row is written, the record tables are immutable (triggers) and `case verify` recomputes every digest, run, supersession and range, so an altered, deleted, injected or repointed record exits `4`. A newer complete run of a parser supersedes an older one without deleting it. Existing cases are moved to the new schema only by the explicit, audited `case upgrade`. Read it with `records list|show|stats` (filters, stable keyset pagination, terminal-safe output) and search it with `records search` (a verified full-text index). Parsers that fill it arrive with sub-project 4; see [Known limitations](CLAUDE.md#9-known-limitations)
- **Windows-safe evidence names** — device file names that are illegal on Windows (`:`, `?`, `CON`, case/8.3 collisions, names over 200 bytes) are stored under safe local names while the original remote path is preserved
- **Single static binary** — pure Go, no cgo; one cross-platform `go run ./tools/check` gate (tidy, vet, lint, build, cross-builds, tests)

---

## 📋 Requirements

| Requirement | Notes |
|---|---|
| Windows 10/11, Linux or macOS (x64 / arm64) | Primary development platform is Windows 11 |
| [Go 1.26+](https://go.dev/dl/) | Only to build from source; `go.mod` pins `toolchain go1.26.8` and auto-downloads it |
| [Android SDK Platform-Tools](https://developer.android.com/tools/releases/platform-tools) | For Android: provides the `adb` server (`adb start-server`) |
| Apple Mobile Device Service | For iOS on Windows: installed with [iTunes](https://www.apple.com/itunes/) or the Apple Devices app |
| [usbmuxd](https://github.com/libimobiledevice/usbmuxd) | For iOS on Linux (built in on macOS) |
| USB serial driver | For serial: the vendor driver for your adapter (FTDI, CP210x, CH340, Qualcomm, …) |

---

## 📦 Installation

### 1. Download the release binary

Grab the binary for your platform from the
[latest release](https://github.com/rbenzing/Minutiae/releases/latest) — or
[build from source](#-building-from-source).

### 2. Install the device services you need

- **Android:** install Platform-Tools, then `adb start-server`. On the phone, enable USB debugging and accept the RSA prompt.
- **iOS:** install iTunes / Apple Devices (Windows) or usbmuxd (Linux). Unlock the device and tap **Trust**.
- **Serial:** install the adapter driver and note the port (`COM3`, `/dev/ttyUSB0`, …).

### 3. Check what Minutiae can see

```bash
minutiae devices
```

Backends that are unavailable are reported as warnings; the others still list
their devices.

---

## 🚀 Usage

### Create a case

```bash
minutiae case new --dir ./cases --id CASE01 --examiner "Jane Doe" --description "Seized handset"
```

### Acquire

```bash
# Android
minutiae android info    --case ./cases/CASE01
minutiae android logical --case ./cases/CASE01 --root /sdcard
minutiae android pull    --case ./cases/CASE01 /sdcard/DCIM/IMG_0001.jpg
minutiae android partitions                                  # rooted devices
minutiae android image   --case ./cases/CASE01 --partition userdata

# iOS
minutiae ios info   --case ./cases/CASE01
minutiae ios ls     /DCIM
minutiae ios backup --case ./cases/CASE01

# USB serial (receive-only unless --allow-device-write)
minutiae serial list
minutiae serial console --port COM3 --baud 115200 --case ./cases/CASE01
```

Device selection: `--serial` (Android) or `--udid` (iOS) is optional when
exactly one device of that kind is connected.

### Analyse images

Images are imported into the case first, so every analysis works on a hashed,
read-only copy and every result is tied back to it. `<ref>` is the image's
artifact path in the case (printed by `import`; for a split image, any one of its
segments) or its artifact id (`import --json`).

```bash
# Import one image: the files are its segments 1..N, in the order given (nothing is sorted)
minutiae image import --case ./cases/CASE01 --device seized-sd disk.img
minutiae image import --case ./cases/CASE01 disk.001 disk.002 disk.003

# Container, partition table and filesystems
minutiae image info    --case ./cases/CASE01 <ref>

# E01 only: recompute MD5/SHA-1 of the media and compare with the hashes the acquirer stored
# (one audited image.verify entry; match/absent exit 0, mismatch exit 4, unverified exit 1)
minutiae image info    --case ./cases/CASE01 <ref> --verify

# Browse a filesystem (-p picks the partition; optional when only one holds a recognized filesystem)
minutiae image ls      --case ./cases/CASE01 <ref> -p 1 -r /Users
minutiae image ls      --case ./cases/CASE01 <ref> -p 1 --deleted /
minutiae image stat    --case ./cases/CASE01 <ref> -p 1 /Users/jane/notes.txt

# APFS only: read a volume as it was in a snapshot (xid:<n>, name:<name>, or a bare value that matches exactly one)
minutiae image ls      --case ./cases/CASE01 <ref> -p 1 --snapshot xid:261 /Data/docs

# Copy files out of the image into the case (new hashed artifacts with provenance)
minutiae image extract --case ./cases/CASE01 <ref> -p 1 -r /Users/jane
minutiae image extract --case ./cases/CASE01 <ref> -p 1 -r --snapshot name:snap1 /Data/docs   # APFS snapshot

# Export unallocated space: a filesystem's free space, or the gaps outside every partition
minutiae image unalloc --case ./cases/CASE01 <ref> -p 1
minutiae image unalloc --case ./cases/CASE01 <ref> --volume
```

One `import` call imports exactly one image; run it once per image. Deleted
entries are listed and flagged but not recovered, and encrypted files are
extracted as ciphertext. ext2, ext3 and ext4 are read as found (the journal is
not replayed), FAT12/16/32 and exFAT likewise (a FAT file whose cluster chain
ends early is extracted as an `incomplete` partial artifact), and F2FS is read as
of its last checkpoint (fsync'd data written after it is not applied; compressed
files are not supported), HFS+ and HFSX are read as found (the journal is not replayed, and a volume with pending journal transactions refuses unallocated-space export; decmpfs-compressed files are decompressed only for the zlib types), and anomalies a
reader notices are written to the audit log as `analysis.warning` entries. E01 sets
are read like raw images (an incomplete set opens with a warning, and a corrupt
chunk fails the read instead of returning zeros; a file that reaches one is
extracted as an `incomplete` partial artifact up to that chunk). APFS is read
as of its newest valid checkpoint (older checkpoints and deleted data are not
interpreted), each volume shows its snapshots under a synthetic `.snapshots`
directory that recursive `ls` and `extract` do not enter and say so in the audit
log (read a snapshot with `--snapshot <xid:N|name:NAME>` on `image ls` and
`image extract`, which record the snapshot in each artifact's provenance), an
encrypted APFS volume is reported but not read, and decmpfs-compressed APFS
files are listed but not extracted.

### Records

Parsed records are stored in the case database. A case created before the records
database existed is upgraded explicitly (audited, never automatic):

```bash
minutiae case upgrade --case ./cases/CASE01

# Newest first, 50 per page; text lines show id, time, type, markers and summary
minutiae records list  --case ./cases/CASE01 --type message --from 2026-10-01T00:00:00Z --sort -ts --limit 50
minutiae records list  --case ./cases/CASE01 --deleted only --recovered any --parser sms-parser@1.2
minutiae records list  --case ./cases/CASE01 --limit 50 --cursor <next cursor of the previous page>

# One record with its artifact, source, parser, batch and run (--payload adds the structured payload)
minutiae records show  --case ./cases/CASE01 1234 --payload

# Counts, overall and by type | parser | artifact | deleted | run
minutiae records stats --case ./cases/CASE01 --by parser
```

A cursor only continues the listing (filters, `--sort`, case) that produced it.
Records of a superseded run are hidden unless you pass `--all-runs`. Text output
escapes every control, escape-sequence and bidirectional character a record
carries; `--json` keeps the strings as stored.

These commands read the database as it is and do not verify it: run
`case verify` first if the listing matters (a forged row, or a forged
supersession pair that hides a run, is only found by `case verify`). They do
refuse a database whose tables, indexes, views or triggers are not the ones this
build defines (exit 4). Hiding a superseded run uses the stored supersession
table, which `case verify` proves equal to its recomputation from the runs.

#### Full-text search

`records search` finds records whose summary or body contains your words. The
index is derived data (it never replaces a record) and `case verify` proves it
equals what the records say. A case created before full-text search is moved in
two explicit steps, both audited:

```bash
minutiae case upgrade   --case ./cases/CASE01     # schema v3; records are kept
minutiae records reindex --case ./cases/CASE01    # builds the index from the records

minutiae records search --case ./cases/CASE01 'meeting "second floor" -cancelled'
minutiae records search --case ./cases/CASE01 --in body --type message --from 2026-10-01T00:00:00Z invoice OR receipt
minutiae records search --case ./cases/CASE01 --substring 'ab-12'     # literal text, 3+ characters
minutiae records search --case ./cases/CASE01 --rank --limit 20 password   # best matches first, no paging
minutiae records search --case ./cases/CASE01 -- -x                       # a query that starts with -
```

| Query | Meaning |
|---|---|
| `foo bar` or `foo AND bar` | both words (AND is optional; upper case only) |
| `foo OR bar` | either (`OR` in upper case; lower-case `or` is a word) |
| `"foo bar"` | the words next to each other, in this order |
| `foo*` | words starting with `foo` (2 or more letters or digits before the star) |
| `-foo` or `-"foo bar"` | records without it (every group needs a positive term) |
| anything else | plain text: `*` inside a word, `:`, parentheses, `NEAR`, `^` mean nothing special |

Text is matched without case, accents, width or compatibility differences
(`Straße` finds `STRASSE`, `café` finds `cafe`). Punctuation separates words, so
`c++` searches the word `c` and `a-b` the phrase `a b`; use `--substring` to
match punctuation. A run of CJK characters is one word, and emoji are not words
(a term with no letter or digit is refused). Hits come in the order of
`records list`, with a snippet per matching column (the match is shown between
`⟦` and `⟧`; a snippet line without one means that column did not match);
`--no-snippets` omits them. `--timeout` (default 30 s) bounds a search. Search
reads only: it never changes the case.

If the index is not current (a case just upgraded, or a build with a newer
normalization), `records search` and ingesting refuse until `records reindex`
has run; `case verify` says so in a notice.

### Verify

```bash
minutiae case info   --case ./cases/CASE01
minutiae case verify --case ./cases/CASE01
```

`case verify` also recomputes the full-text index from the records (its temporary
files live under `<case>/tmp/`, removed when it ends). It prints each problem as a `PROBLEM:` line and each notice (an
upgrade announced but not concluded, a recovered ingest) as a `NOTICE:` line,
with case text escaped, and exits 4 when it found a problem.

Every command accepts `--json` for machine-readable output.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | General error (also a search that ran past `--timeout`, and a reindex or an ingest refused because another ingest or reindex is active) |
| `2` | Usage error (bad flag, missing argument, unknown command, a bad filter, cursor, page or search query, a case that needs `case upgrade`, or a full-text index that is not current: run `records reindex`) |
| `3` | Device error (not found, unauthorized, not rooted, unsupported, write not allowed) |
| `4` | Integrity failure (`case verify` found a problem, such as a full-text index that does not equal the records; the audit log/database is missing or corrupt, or the records database schema is not the one this build defines) |

### Case layout

```
cases/CASE01/
  case.json        id, examiner, created, tool version, host
  audit.jsonl      append-only, SHA-256 hash-chained action log
  manifest.jsonl   one record per artifact (path, size, sha256, md5, source, incomplete)
  artifacts.db     SQLite index (schema v3: artifacts, unified records tables, full-text index)
  case.lock        held while a command has the case open
  artifacts/<device>/<acquisition>/...
```

---

## 🏗️ Architecture

Minutiae is a single Go binary. Every byte from a device flows through one
choke point — `evidence.Case` — which hashes, records and audits it.

```
device ──► backend (android | ios | serial) ──► evidence.Case.Capture ──► artifact + manifest + db + audit
                                                         ▲
                              cli (cobra) ───────────────┘
```

| Layer | Package | Role |
|---|---|---|
| CLI | `internal/cli` | Thin cobra layer: flags, progress, exit codes |
| Evidence | `internal/evidence` | Case, hash-chained audit log, manifest, `artifacts.db`, verify, case lock |
| Device | `internal/device` | Backend-neutral interfaces, registry, audited acquisition helpers |
| Serial | `internal/transport/serial` | Port enumeration and configuration (`go.bug.st/serial`) |
| Protocol | `internal/protocol` | Raw console with audited, gated transmit |
| ADB | `internal/android/adb` | Native ADB host + sync protocol client (no Minutiae imports) |
| Android | `internal/android` | Info, pull/push, logical acquisition, root detection, imaging |
| mobilebackup2 | `internal/ios/mb2` | DeviceLink host, binary-plist validator, staging handlers |
| iOS | `internal/ios` | go-ios adapter (usbmuxd, lockdown, AFC), backup → staging → artifacts |
| Image | `internal/image` | Disk-image containers (raw, split raw, E01) as read-only `io.ReaderAt`; `ewf` reads EWF v1 (E01) segment sets with chunk-table trust rules and stored-hash verification, `ewftest` is its builder |
| Volume | `internal/volume` | MBR/EBR and GPT partition tables; unallocated gaps between partitions |
| Filesystem | `internal/filesys` | Filesystem interface, entries and byte runs, block cache; `detect` probes drivers, `f2fs` reads F2FS, `ext4` reads ext2/3/4, `fat` reads FAT12/16/32, `exfat` reads exFAT, `hfsplus` reads HFS+/HFSX, `apfs` reads APFS (volumes, snapshots as views), `fstest` is the test filesystem |
| Examine | `internal/examine` | The only bridge from parsers to the case: import, sessions, extract, unallocated export, provenance |
| Records | `internal/records` | Unified record database: record model and type registry, the audited batch writer and ingest lifecycle (the only code that writes the record tables), supersession, and the read-only reader behind `records list\|show\|stats`; `recordstest` holds its fixtures and tamper helpers |
| Arch test | `internal/archtest` | Enforces the package dependency rule and the single-writer rule for the record tables |

Image analysis follows the same rule: the parsers (`image`, `volume`,
`filesys`) read an `io.ReaderAt` and import no Minutiae package, so they cannot
write to a case; only `examine` turns their results into artifacts, through
`evidence.Case`.

```
image import ──► artifact (hashed segments) ──► examine.Open ──► image ► volume ► filesystem
                                                      │
                                   extract / unalloc ─┴──► derived artifacts (parent id + sha256, runs)
```

### Acquisition lifecycle

```
acquire.start ──► artifact.create (× N, each hashed) ──► acquire.end
      │                         │
      └──── error / Ctrl-C ─────┴──► partial artifacts kept (incomplete) ──► acquire.error
```

---

## 🔨 Building from source

```bash
# Prerequisites: Go 1.26+ (git clone, then the pinned toolchain auto-downloads)
git clone https://github.com/rbenzing/Minutiae.git
cd Minutiae
go build -o bin/minutiae.exe ./cmd/minutiae
```

Release build with version stamping:

```bash
go build -ldflags "-X github.com/rbenzing/minutiae/internal/version.Version=v1.0.0 -X github.com/rbenzing/minutiae/internal/version.Commit=$(git rev-parse --short HEAD)" -o bin/minutiae.exe ./cmd/minutiae
```

Run the full verification gate (tidy, vet, golangci-lint incl. gofumpt, build,
no-cgo cross-builds, tests — with `-race` when cgo is available):

```bash
go run ./tools/check
```

---

## 🧪 Hardware acceptance

The default test suite uses fakes (an in-process ADB server, an in-memory
serial port, a scripted iOS device) and never needs hardware. To validate on
real, authorized devices:

```bash
go test -tags hardware ./... -v
minutiae case new --dir ./cases --id HW1 --examiner "<name>"
minutiae android logical --case ./cases/HW1
minutiae ios backup      --case ./cases/HW1
minutiae case verify     --case ./cases/HW1
```

---

## 🔒 Security model

- **Read-only acquisition by default** — no command writes to a device unless `--allow-device-write` is given; every write is audited before it happens, and the bytes actually sent are hashed and compared
- **Fail closed** — a missing, empty, corrupt or torn audit log or database makes every command refuse the case (exit `4`); they are never silently recreated
- **Exclusive case lock** — two processes can't interleave writes into one case's audit chain
- **Hostile-device hardening** — device-supplied lengths are capped, binary plists are validated against expansion bombs and integer overflow before decoding, device paths are confined to the staging directory, and shell commands only ever contain validated values
- **Known limitations** are documented in [`CLAUDE.md` §9](CLAUDE.md#9-known-limitations) (e.g. ADB sync v1 32-bit sizes, encrypted iOS backups captured as-is)

---

## 🗺️ Roadmap

v1.0.0 completes **Sub-project 1: Foundation + Acquisition**. **Sub-project 2:
Image & filesystem layer** is complete: the image, partition-table and
analysis foundation is in place, APFS, F2FS, ext2/3/4, FAT12/16/32, exFAT and HFS+/HFSX can be read and E01 images can be
read and verified, which completes sub-project 2. Next up: deleted-data recovery (SQLite freelist/WAL, carving),
artifact parsers, analytics, reporting, protocol drivers (EDL/BROM/AT), a
desktop GUI, automatic artifact classification, and AI-assisted search and
analysis over the artifact collection (offline by default, every answer cites
its source records). **Sub-project 5: Unified artifact database** is in
progress: its core (the audited, verifiable record store, `case upgrade` and
`records list|show|stats`, full-text `records search` and `records reindex`) is in place; the provenance chain
with image-offset translation, and scale work follow. See
[docs/ROADMAP.md](docs/ROADMAP.md).

---

## 📄 License

No open-source license has been chosen yet. Until one is added, all rights are
reserved by the author.

---

## 👤 About the Author

Built by **Russell Benzing**. Minutiae is an auditable, open-development
alternative for mobile-device forensic acquisition.

---

## 🆘 Support

- **Issues**: [GitHub Issues](https://github.com/rbenzing/Minutiae/issues)
- **Releases**: [github.com/rbenzing/Minutiae/releases](https://github.com/rbenzing/Minutiae/releases)

If Minutiae is useful to you, you can support the work:

<div align="center">

[![Buy Me A Coffee](https://img.shields.io/badge/Buy%20Me%20A%20Coffee-FFDD00?style=for-the-badge&logo=buymeacoffee&logoColor=black)](https://buymeacoffee.com/russellbenzing)

</div>

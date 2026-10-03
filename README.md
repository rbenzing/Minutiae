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
- **Image analysis** — import disk images (raw/dd, split raw) into a case, read MBR/EBR and GPT partition tables, browse filesystems read-only, extract files and export unallocated space as new hashed artifacts that record their full provenance (parent image, partition, filesystem entry, byte runs). **ext2/ext3/ext4** filesystems (extents or block maps, htree and inline directories, inline data, metadata checksums, deleted-entry flagging, unallocated space) are readable today, verified against real `mke2fs`-built images with independent expected results and fuzzed against hostile input; more readers arrive progressively (FAT/exFAT, F2FS, E01, APFS, HFS+); see [Known limitations](CLAUDE.md#9-known-limitations)
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

# Browse a filesystem (-p picks the partition; optional when only one holds a recognized filesystem)
minutiae image ls      --case ./cases/CASE01 <ref> -p 1 -r /Users
minutiae image ls      --case ./cases/CASE01 <ref> -p 1 --deleted /
minutiae image stat    --case ./cases/CASE01 <ref> -p 1 /Users/jane/notes.txt

# Copy files out of the image into the case (new hashed artifacts with provenance)
minutiae image extract --case ./cases/CASE01 <ref> -p 1 -r /Users/jane

# Export unallocated space: a filesystem's free space, or the gaps outside every partition
minutiae image unalloc --case ./cases/CASE01 <ref> -p 1
minutiae image unalloc --case ./cases/CASE01 <ref> --volume
```

One `import` call imports exactly one image; run it once per image. Deleted
entries are listed and flagged but not recovered, and encrypted files are
extracted as ciphertext. ext2, ext3 and ext4 are read as found (the journal is
not replayed) and anomalies the reader notices are written to the audit log as
`analysis.warning` entries. Other filesystem readers arrive progressively
(FAT/exFAT, F2FS, E01, APFS, HFS+); until they land, partitions are listed but
those filesystems are not yet readable.

### Verify

```bash
minutiae case info   --case ./cases/CASE01
minutiae case verify --case ./cases/CASE01
```

Every command accepts `--json` for machine-readable output.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | General error |
| `2` | Usage error (bad flag, missing argument, unknown command) |
| `3` | Device error (not found, unauthorized, not rooted, unsupported, write not allowed) |
| `4` | Integrity failure (`case verify` found a problem, or the audit log/database is missing or corrupt) |

### Case layout

```
cases/CASE01/
  case.json        id, examiner, created, tool version, host
  audit.jsonl      append-only, SHA-256 hash-chained action log
  manifest.jsonl   one record per artifact (path, size, sha256, md5, source, incomplete)
  artifacts.db     SQLite index (schema v1) for later analysis
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
| Image | `internal/image` | Disk-image containers (raw, split raw) as read-only `io.ReaderAt` |
| Volume | `internal/volume` | MBR/EBR and GPT partition tables; unallocated gaps between partitions |
| Filesystem | `internal/filesys` | Filesystem interface, entries and byte runs, block cache; `detect` probes drivers, `ext4` reads ext2/3/4, `fstest` is the test filesystem |
| Examine | `internal/examine` | The only bridge from parsers to the case: import, sessions, extract, unallocated export, provenance |
| Arch test | `internal/archtest` | Enforces the package dependency rule |

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
Image & filesystem layer** is in progress: the image, partition-table and
analysis foundation is in place, ext2/3/4 can be read, and the remaining
readers (FAT/exFAT, F2FS, E01, APFS, HFS+) are arriving progressively. Next up: deleted-data recovery (SQLite freelist/WAL, carving),
artifact parsers, analytics, reporting, protocol drivers (EDL/BROM/AT), a
desktop GUI, automatic artifact classification, and AI-assisted search and
analysis over the artifact collection (offline by default, every answer cites
its source records). See [docs/ROADMAP.md](docs/ROADMAP.md).

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

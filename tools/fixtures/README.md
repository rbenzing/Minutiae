# Real-image fixture toolchain

Small disk and filesystem images that the Go tests decompress and parse. Each
fixture has an independent oracle (`<name>.expect.json`) computed by the
partitioning/filesystem tools or from the source tree, never by Minutiae.

Generating fixtures needs Docker. Running the tests never does: the generated
`*.img.gz` and `*.expect.json` files are committed under each package's
`testdata/`.

## Generate

From the repository root:

```bash
docker build -t minutiae-fixtures tools/fixtures
docker run --rm --privileged -v "$PWD:/work" -w /work minutiae-fixtures bash tools/fixtures/gen.sh all
```

`gen.sh <fixture>` builds one fixture (`volume-gpt`, `volume-mbr`, `ext4`, `fat`, `exfat`; `hfsplus` needs its own image, see "HFS+ fixtures").
`--privileged` is needed only by `exfat` (see below); every other fixture runs without it.

With Git Bash on Windows, stop MSYS rewriting the container paths:

```bash
MSYS_NO_PATHCONV=1 docker run --rm --privileged -v "C:/GIT/Minutiae:/work" -w /work minutiae-fixtures bash tools/fixtures/gen.sh all
```

Scripts must keep LF line endings (`.gitattributes` enforces this).

## Fixtures

| Script | Output (`internal/volume/testdata/`) | Oracle source |
|---|---|---|
| `volume-gpt.sh` | `gpt-disk.img.gz`, `gpt-disk.expect.json` | `sfdisk --json`, cross-checked with `sgdisk -i` |
| `volume-mbr.sh` | `mbr-disk.img.gz`, `mbr-disk.expect.json` | `sfdisk --json` |
| `ext4.sh` + `ext4_oracle.py` + `ext4_freeblocks.py` | `ext4-4k-csum`, `ext4-1k-blockmap-ext2`, `ext4-inline`, `ext4-1k-metabg-uninit` (`.img.gz` + `.expect.json`, in `internal/filesys/ext4/testdata/`) | the source tree handed to `mke2fs -d` (walked by `ext4_oracle.py`: type, size, sha256, mode, mtime, symlink targets), the generator's own list of `debugfs rm` operations (deleted names), `dumpe2fs -h` (label, uuid, block size, features) and the full `dumpe2fs` report (`free_blocks`: the free block ranges, inclusive `[first, last]`, parsed by `ext4_freeblocks.py`; the tests compare the reader's unallocated space with them exactly) |
| `fat.sh` + `fat_tree.sh` + `fat_oracle.py` + `fat_chains.py` | `fat12`, `fat16`, `fat32` (`.img.gz` + `.expect.json`, in `internal/filesys/fat/testdata/`) | the source tree (`fat_oracle.py`: type, size, sha256, mtime), the generator's own list of deleted names, `fsck.fat -v` (geometry, data-area offset, cluster count, clusters in use) and `mshowfat` (the cluster chain of every live file and directory; the free clusters are the ones no live chain holds, `fat_chains.py` fails unless that count equals what `fsck.fat -v` reports in use; the tests compare the reader's unallocated space and every file's runs with them exactly) |
| `exfat.sh` + `exfat_chains.py` (also `fat_tree.sh`, `fat_oracle.py`) | `exfat` (`.img.gz` + `.expect.json`, in `internal/filesys/exfat/testdata/`) | the source tree, the generator's deleted names, and `dump.exfat` (boot report: geometry, serial, label; `-c -d <path>`: the cluster chain of every live path; the free clusters are the ones no chain, bitmap, up-case table or root holds, cross-checked against dump.exfat's "Free Clusters") |
| `hfsplus.sh` + `hfsplus_oracle.py` + `hfsplus_normalize.py` (image `minutiae-fixtures-hfs`) | `hfsplus-empty`, `hfsx-empty`, `hfsplus-journal`, `hfsplus-1k`, `hfsplus-wrapped` (`.img.gz` + `.expect.json`, in `internal/filesys/hfsplus/testdata/`) | an independent Python parse of the volume header, B-trees, allocation bitmap and journal files, cross-checked against `fsck.hfsplus` and `blkid` (see "HFS+ fixtures") |

## Determinism

Disk and partition GUIDs, the MBR disk id and all layouts are fixed in the
scripts, and images are compressed with `gzip -n -9`, so regenerating with the
same package versions yields identical files. Each `expect.json` records a
`generator` object: `image_sha256` (the sha256 of the uncompressed image, which
the tests check so an image and its oracle cannot drift apart), the versions of
the packages used (from `dpkg-query`) and
the exact commands.

### ext4 determinism

The ext4 images are byte-identical across runs (checked by generating twice and
comparing every output sha256). That needs:

- `E2FSPROGS_FAKE_TIME=1700000000`, a fixed `-U` uuid, `-E hash_seed=...,root_owner=0:0`;
- a source tree on tmpfs (`/dev/shm`, so directory enumeration is creation order; not
  proven necessary) created in sorted order, every mtime set
  with `touch -h -d` to a distinct value older than `SOURCE_DATE_EPOCH`
  (the source ctime is "now"; mke2fs clamps it to `SOURCE_DATE_EPOCH`, which the
  Dockerfile sets, so the image does not depend on the real clock);
- `debugfs -w -R "ssv lastcheck 1700000000"` after `e2fsck -fyD`: `e2fsck`
  stamps `s_lastcheck` with the real clock and ignores the fake time, which
  was the only source of nondeterminism found.

The oracle excludes the files later removed with `debugfs rm` (the generator's
`deleted` list) and lists `/lost+found` (created by `mke2fs`, not in the source
tree) under `mke2fs_created`. Directory mtimes are the source tree's: `e2fsck -D`
and `debugfs rm` do not touch them.

### FAT and exFAT determinism

Both are byte-identical across runs (checked by generating twice and comparing
every output sha256). `mkfs.fat`, `mcopy`, `mkfs.exfat` and `exfat-fuse` stamp
creation/access times (and `mkfs.exfat` the volume serial) from the clock, so
they run under `faketime -f '2023-11-14 22:13:20'` (a frozen clock; with
`FAKETIME_DONT_FAKE_MONOTONIC=1`, else the FUSE daemon hangs, and `NO_FAKE_STAT=1`,
else the source files' mtimes shift and `mcopy -m` copies the shifted ones). Source
mtimes are even (FAT stores 2 s steps). The FAT volume id is fixed (`-i 1234ABCD`);
`mkfs.exfat 1.2.9` has no serial option, so the serial comes from the frozen
clock and the oracle reads it from `dump.exfat`.

Fixture details worth knowing:

- Long names, a lower-case 8.3 name (NTRes flags), a mixed-case long name, accents and CJK
  are in both. The FAT images also hold an 8.3-only name with non-ASCII characters
  (`/ÉTÉ.TXT`, no long-name entries; `mcopy` stores É as the byte 0x90). The volume does not
  record its code page, so the reader decodes bytes at or above 0x80 as code page 437 and
  keeps the raw bytes in `RawName`; the oracle's expected name is the source tree's. The
  file is added by `fat_tree.sh` (`tree_oem=1`, fat only) after the source mtimes are
  assigned, so the exFAT image is unchanged and no other entry's time moves.
  A surrogate pair (an emoji) is only in the exFAT image: `mtools` cannot store one.
- `/frag/c-large-fragmented.bin` is fragmented: a is written, b is written, a is
  deleted, c is written. On FAT32 `mtools` allocates from the FSInfo next-free hint, which
  skips a's clusters, so the script resets the hint to 0xFFFFFFFF (unknown) first.
  a's directory slot is reused by c, so a is not in the `deleted` list; the
  other deleted names are long ones (a deleted short entry loses its first character)
  and `/gone-directory` is removed with its content (`mdeltree` / `rm -r`).
- FAT32 needs 65525 clusters, so the smallest image (512-byte clusters) is 34 MiB raw
  (about 400 KiB gzipped); the fuzz tests cut their seeds after the last used cluster.
- Single-character names are avoided: `dump.exfat -d` fails to resolve `/deep/a`.

### exFAT needs a privileged container

Nothing in exfatprogs copies files into an image, and the Docker Desktop kernel
has no exfat driver (`mount -t exfat` fails). `exfat.sh` attaches the image to a loop
device (`losetup -f --show`) and mounts it with the FUSE driver `mount.exfat-fuse`
(package `exfat-fuse`, installed by the Dockerfile), which needs `/dev/fuse` and
`CAP_SYS_ADMIN`: run the container with `--privileged`. The script checks for both and
stops with a message that says so. If a machine cannot run privileged containers,
the committed `exfat.img.gz` is still used by the tests; only regenerating it is
impossible there, and the builder-based tests (`exfattest`) are the coverage that
needs no image tools.

## HFS+ fixtures

`gen.sh hfsplus` runs in a SEPARATE image, `minutiae-fixtures-hfs`
(`Dockerfile.hfs`), because `hfsprogs` (`mkfs.hfsplus`, `fsck.hfsplus`) is not
installable in the main image and that image's pinned layers must not change
(every existing fixture stays byte-identical; checked by running `gen.sh all`
and seeing no change under `git status`). `hfsplus` is deliberately not part of
`all`; without `mkfs.hfsplus` `gen.sh` stops with a message naming the image.

```bash
docker build -f tools/fixtures/Dockerfile.hfs -t minutiae-fixtures-hfs tools/fixtures
docker run --rm -v "$PWD:/work" -w /work minutiae-fixtures-hfs bash tools/fixtures/gen.sh hfsplus
```

No `--privileged` is needed.

### Feasibility probe (2026-10-03, Docker Desktop, kernel 6.6.87.2-microsoft-standard-WSL2)

| Probe | Result |
|---|---|
| `debian:bookworm-slim` main / trixie / bullseye main | `hfsprogs`: no candidate |
| bullseye `non-free` on `archive.debian.org` | `540.1.linux3-4` (not needed) |
| **bookworm `non-free`** (chosen) | **`540.1.linux3-5+b1`**: installs directly, no source build |
| base image | `debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251` (pinned in `Dockerfile.hfs`) |
| `mkfs.hfsplus` flags | `-v` label, `-s` case-sensitive (HFSX), `-J [size]` journaled, `-b` block size, `-w` HFS wrapper, `-N` dry run, `-i` first CNID, `-n`/`-c` node and clump sizes |
| `fsck.hfsplus -n -f` | works on a plain file image (read-only check) |
| `grep hfsplus /proc/filesystems` | absent |
| `modprobe hfsplus` | `Module hfsplus not found` (the kernel ships no module) |
| `--privileged` `mount -t hfsplus -o loop` | `unknown filesystem type 'hfsplus'` |

Consequence: tier 2 (a kernel-populated image) is NOT reachable here, and
nothing in hfsprogs copies files into an image, so every fixture is an EMPTY
volume. There is no `hfsplus-populated`. On a machine whose kernel mounts
`hfsplus`, a populated tier would belong in `hfsplus.sh` (not written: it could
not be tested).

### Images (8 MiB raw each, `internal/filesys/hfsplus/testdata/`)

| Image | `mkfs.hfsplus` | What it holds |
|---|---|---|
| `hfsplus-empty` | `-v FIXTURE` | HFS+ (H+, version 4), 4 KiB blocks, root folder and system files only |
| `hfsx-empty` | `-s -v FIXTURE` | HFSX (HX, version 5), case-sensitive (binary key compare 0xBC) |
| `hfsplus-journal` | `-J -v FIXTURE` | journaled: real `.journal_info_block` (CNID 17) and `.journal` (CNID 16, 512 KiB) catalog files in the root |
| `hfsplus-1k` | `-b 1024 -v FIXTURE` | 1 KiB allocation blocks (smaller than the 4 KiB B-tree node size) |
| `hfsplus-wrapped` | `-w -v FIXTURE` | classic HFS wrapper (master directory block `BD`) embedding the HFS+ volume at byte 45056 (`volume_offset` in the oracle) |

What they validate against Apple's own formatter: the volume header (both
copies), the five special-file forks, B-tree headers and node maps, the
catalog key/record/thread layouts, the allocation bitmap, the journal files and
the wrapper embedding. What they do NOT: a populated multi-level tree, hard
links, attributes, fragmented files, overflow extents, decomposed Unicode,
deletion, compression. Until a populated real image exists, those behaviours are
verified against the Go builder and Apple's fsck on builder images (tier 3,
`hfsplus.sh check-builder <dir>`), not against macOS- or kernel-written volumes.

### Oracle (`hfsplus_oracle.py`)

A second, hand-written parse (never Minutiae) plus external tools; the
generator FAILS on any disagreement (each passed check is listed in `checks`
of the `expect.json`). Cross-checks: header fields and both header copies;
allocation-bitmap popcount = `totalBlocks - freeBlocks`; the set of allocated
blocks equals EXACTLY the header blocks plus every extent of the special files
and of every catalog file fork; catalog walked top-down and along the leaf
chain (same leaves), keys strictly increasing, threads match records,
valence, `fileCount`/`folderCount`, node map vs `freeNodes`; journal info and
files agree and the journal region is zero; `fsck.hfsplus -n -f` exit status;
`blkid -p -o export` TYPE, LABEL and UUID (the UUID is also recomputed from
the volume id: MD5 of Apple's namespace plus the id, version-3 bits); the image
sha256 is unchanged after the read-only tools. `expect.json` carries
`free_blocks` (inclusive block ranges counted from the volume start, so for the
wrapped image add `volume_offset` for byte offsets), `free_count`, every
catalog entry (path, CNID, type, size, sha256, extents, mode, dates) and
`populated: false`. Stated limitation: the Python parse is a second
implementation by the same author as the reader; its independence rests on
`fsck.hfsplus` and `blkid` agreeing, not on a different author.

Facts the oracle recorded about `mkfs.hfsplus` output (they shape the reader's tests):

- Volume dates are the frozen clock (1700000000); `.journal` is created one second later.
- The root folder and the journal files have no BSD info: `fileMode` is 0 for the root and `0o100000` (type bits only) for files, so `mode&0o7777` is 0; `attributeModDate`, `accessDate`, `backupDate` are 0 (absent).
- `-J` leaves the journal uninitialised: journal info flags are `in-FS|need-init` (0x5) and the whole journal is zero; there is no journal header to parse (the first mount writes it).
- **HFSX quirk:** `mkfs.hfsplus -s` leaves the root folder record without `kHFSHasFolderCountMask` (0x10), and Apple's `fsck_hfs` reports `HasFolderCount flag needs to be set (id = 2)` and exits 8; nothing else is reported. The generator keeps the formatter's real output, requires exactly that report (`--fsck-issue`) and records it in `external_tools.fsck.known_issues`. A builder HFSX image must set the flag to pass `check-builder`.
- `blkid` reports `TYPE=hfsplus` for HFSX and for the wrapped volume.

### Determinism

Byte-identical across runs (generated twice, every output sha256 compared).
`mkfs.hfsplus` takes its dates from the clock, frozen with `faketime -f
'2023-11-14 22:13:20'` (`FAKETIME_DONT_FAKE_MONOTONIC=1`, `NO_FAKE_STAT=1`), but
draws the 64-bit volume id (finderInfo words 6-7) from a random source; two
runs differed only in those 8 bytes, in both header copies.
`hfsplus_normalize.py` rewrites the id (`MINUTIAE`, `4d494e5554494145`) in both
copies (in the embedded volume for the wrapped image). Also `gzip -n -9`, a
fixed work directory, `umask 022`, a root check and a cleanup trap.

### Builder images (`hfsplus.sh check-builder <dir>`)

Runs `fsck.hfsplus -n -f` on every `*.img` in `<dir>` and exits non-zero when
any fails. The Go test that writes the builder's images
(`MINUTIAE_WRITE_BUILDER_IMAGES=<dir>`) lands with the reader tests; results are
recorded here when it does (not yet run).

## Adding a fixture

1. Add `<name>.sh` taking the output directory as `$1`; source `lib.sh` and
   wrap commands whose invocation belongs in the oracle with `run`.
2. Register it in `gen.sh`.
3. Keep images small: a few hundred KiB compressed at most. The raw size is set by
   the filesystem (the smallest FAT32 image is 34 MiB, see above; every other fixture
   is at most 16 MiB raw).

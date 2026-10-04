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

`gen.sh <fixture>` builds one fixture (`volume-gpt`, `volume-mbr`, `ext4`, `fat`, `exfat`, `apfs`).
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
| `apfs.sh` + `apfs_oracle.py` | `apfs-ci`, `apfs-cs`, `apfs-multichunk` (`.img.gz` + `.expect.json`, in `internal/filesys/apfs/testdata/`) | the `mkapfs` arguments (label, UUIDs, case flags) and an independent Python decoder of the finished image (see "APFS determinism and limits"); `apfsck` must accept the image |

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

### APFS determinism and limits

`apfs.sh` makes EMPTY containers with `mkapfs` (package `apfsprogs` 0.2.0-1, a
separate last layer of the Dockerfile so every earlier layer and fixture is
unchanged) on regular files; no privileges are needed. `apfsck` must accept each
image and `apfs_oracle.py` must pass all of its cross-checks, or the script fails.

| Fixture (`internal/filesys/apfs/testdata/`) | mkapfs arguments | Notes |
|---|---|---|
| `apfs-ci` | defaults, `-L fixture`, 64 MiB | case- and normalization-insensitive (incompatible features 0x1), hashed directory-record keys |
| `apfs-cs` | `-s -z -L fixture-cs`, 64 MiB, other UUIDs | case- and normalization-sensitive (incompatible features 0), plain directory-record keys |
| `apfs-multichunk` | defaults, `-L multichunk`, 129 MiB | two space-manager chunks; the second (256 blocks, all free) has no bitmap block (`ci_bitmap_addr` 0); the Go tests may skip it under `-short` |

Determinism (checked by generating twice and comparing every output sha256, and by
the generator itself, which builds each image twice and `cmp`s them):

- `mkapfs` stamps the volume's creation time, the root and private-dir inode
  times and the directory records' `date_added` from the clock, so it runs under
  `faketime -f '2023-11-14 22:13:20'` (frozen; `FAKETIME_DONT_FAKE_MONOTONIC=1`,
  `NO_FAKE_STAT=1`). Without it two runs differ in about 60 bytes.
- the container and volume UUIDs are arguments (`-U`, `-u`); without them they are
  random. The label is `-L`.
- `mkapfs` writes only the blocks it needs and never zeroes the rest of the device:
  every image starts from a brand-new sparse file (`truncate -s`), never a reused one.
- Minimum size: 512 KiB (`such tiny containers are not supported` below). The
  fixtures are 64 MiB (one chunk) and 129 MiB (two chunks; chunk = 128 MiB at 4 KiB
  blocks). More than 126 chunks (16 GiB) would be needed for a CAB layer, which
  is too large to commit: CABs are builder-only.
- `mkapfs` (this version) has `-s` (case-sensitive: normalization-insensitive
  flag only) and `-z` (normalization-sensitive: no flags, so also case-sensitive);
  the fixtures use the two extreme combinations.

The oracle (`apfs_oracle.py`, written from Apple's APFS reference with `struct`,
sharing no code with Minutiae) decodes the container superblock and checkpoint
ring, the ephemeral objects, the container and volume object maps, the volume
superblock, every fs-tree record, the (empty) snapshot-metadata and
extent-reference trees, the space manager with every chunk-info record and bitmap,
and the free block ranges. It fails the generator unless: every object checksum
verifies; the label, UUIDs and case flags equal the `mkapfs` arguments;
`sm_free_count` = the sum of `ci_free_count` = the zero-bit count of the bitmaps;
the chunk and CIB counts follow from the block count; no free range overlaps block
0, the checkpoint areas or the internal pool; every verified object block is
allocated; and every non-zero block of the image is a verified object or a known
raw block. Its `measured` section records what the reference leaves open (ring
shape, drec key form, bitmap polarity, tree storage types, ...).

Limits: these are EMPTY volumes. Nothing on Linux can populate an APFS volume, so
there are no files, no extra directory records (only `root` and `private-dir`),
no xattrs, hard links, symlinks, file extents, snapshots (`apfs-snap` needs the
kernel module) or encrypted volumes, and a single checkpoint (no ring
wrap-around or fallback). Those paths are covered by the synthetic builder only
and, for real data, by the manual `realimages` test.

## Adding a fixture

1. Add `<name>.sh` taking the output directory as `$1`; source `lib.sh` and
   wrap commands whose invocation belongs in the oracle with `run`.
2. Register it in `gen.sh`.
3. Keep images small: a few hundred KiB compressed at most. The raw size is set by
   the filesystem (the smallest FAT32 image is 34 MiB, see above; every other fixture
   is at most 16 MiB raw).

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

`gen.sh <fixture>` builds one fixture (`volume-gpt`, `volume-mbr`, `ext4`, `fat`, `exfat`, `f2fs`, `ewf`; `hfsplus` needs its own image, see "HFS+ fixtures").
`--privileged` is needed by `exfat` (see below) and is used by `f2fs` only to try a kernel mount (see "F2FS determinism"); every other fixture runs without it.

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
| `hfsplus.sh` + `hfsplus_oracle.py` + `hfsplus_normalize.py` (image `minutiae-fixtures-hfs`) | `hfsplus-empty`, `hfsx-empty`, `hfsplus-journal`, `hfsplus-1k`, `hfsplus-wrapped`, `hfsplus-populated` (`.img.gz` + `.expect.json`, in `internal/filesys/hfsplus/testdata/`; also `hfsplus_populate.sh` + `hfsplus_tree.py`) | an independent Python parse of the volume header, B-trees (extents-overflow records included), allocation bitmap and journal files, cross-checked against `fsck.hfsplus` and `blkid`; for the populated image also the source tree the Linux driver was fed (see "HFS+ fixtures") |
| `ewf.sh` + `ewf_inspect.py` + `ewf_normalize.py` | `ewf-disk.img.gz` (the raw source disk), `ewf-<variant>.E0N.gz` (one `.gz` per segment file of `single-none`, `single-best`, `multi-none`, `seed-small`) and `ewf-fixtures.expect.json`, in `internal/image/ewf/testdata/` | the raw disk (its sha256/md5/sha1/size, built from the committed ext4 and fat12 fixtures), an independent Python decoder (`ewf_inspect.py`: every section and Adler-32, every chunk, the rebuilt media equals the raw disk byte for byte), `ewfverify`, `ewfinfo` (whitelisted geometry and hash fields) and `ewfexport -f raw` (sha256 equals the raw disk) |
| `f2fs.sh` + `f2fs_tree.sh` + `f2fs_oracle.py` + `f2fs_normalize.py` | `f2fs-extra-attr`, `f2fs-default` (`.img.gz` + `.expect.json`, in `internal/filesys/f2fs/testdata/`) | the source tree handed to `sload.f2fs` (`f2fs_oracle.py tree`: type, size, sha256, mode, mtime, symlink targets), and the reports of `fsck.f2fs -l/-f/-t/-M`, `dump.f2fs -s/-n` and `blkid` on the finished image (`f2fs_oracle.py layout`: superblock and checkpoint fields, features, uuid, label, the NAT block address of every inode, the data-block extents of every file, the free main-area blocks from the SIT bitmaps). The oracle cross-checks the tools against each other (fsck vs dump on every shared field, SIT popcount vs `valid_block_count`, fsck's tree vs the source tree, the file map vs file sizes) and the generator fails on any mismatch, on an unclean `fsck.f2fs`, or when no file is fragmented |

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

### EWF determinism

`ewf.sh` acquires a GPT disk (8 MiB; partition 1 = the smallest committed ext4 fixture, partition 2 =
`fat12.img.gz`; partitions are 4 KiB aligned because the smallest ext4 image is 6.25 MiB, so 1 MiB
alignment would not fit 8 MiB) with the real `ewfacquire` (the tool's default format, `-d sha1`), under a
frozen `faketime` clock (the acquisition and system dates are in the header sections). The four E01 sets
were byte-identical across two generation runs (compared by hand on every output sha256; `ewf.sh` does not check it) and
`ewfacquire 20140816` needs no further normalisation: it writes an all-zero set identifier (the volume
payload field at offset 64), so `ewf_normalize.py` rewrites nothing (the generator log prints "0 section(s)
rewritten"). The normaliser stays in the pipeline as a safeguard: it pins a non-zero set identifier to a
fixed GUID and recomputes the payload Adler-32, nothing else, so a tool version that randomises it does
not break reproducibility. If a later `ewf-tools` makes the outputs differ run to run in another field,
extend the normaliser for that one field.

Order of work: acquire, normalise, `ewf_inspect.py` (fails unless the decoded media equals the raw disk and
the stored hashes match), `ewfverify`, `ewfinfo`, `ewfexport`, and only then `gzip -n -9`. The generator
fails on any disagreement and when the committed outputs would exceed 2 MiB. `expect.json` holds the raw
disk hashes, the partitions (with the source fixture of each), and per variant the segment files (size and
sha256 of the E01 bytes), geometry, chunk counts by kind, the stored hashes, every table and section, the
header fields and the location of the first compressed and first uncompressed chunk. Only whitelisted
fields of the tools' output are parsed; none is stored verbatim.

Regenerate with `docker run --rm -v "$PWD:/work" -w /work minutiae-fixtures bash tools/fixtures/gen.sh ewf`
(about 30 s, not privileged; it reads the committed ext4 and fat12 fixtures from the tree, so regenerate
those first if they ever change, and commit the new `ewf` outputs with them).

Committed in `internal/image/ewf/testdata/` (bytes, as stored; 1,389,704 bytes of `.gz` files plus the 31,915-byte
oracle, 1,421,619 in all):

| File | Bytes | Content |
|---|---|---|
| `ewf-disk.img.gz` | 346,007 | the raw 8 MiB GPT disk every variant wraps (the oracle for the media) |
| `ewf-single-none.E01.gz` | 349,468 | one segment, `-c none`: 256 chunks, all uncompressed |
| `ewf-single-best.E01.gz` | 336,508 | one segment, `-c best`: 128 chunks (126 compressed, 2 uncompressed) |
| `ewf-multi-none.E01.gz` .. `.E05.gz` | 11,913 / 3,156 / 2,992 / 330,118 / 650 | five segments, `-c none`, 63+63+63+63+4 chunks |
| `ewf-seed-small.E01.gz` | 8,892 | the first MiB only (32 compressed chunks): the fuzz seed |
| `ewf-fixtures.expect.json` | 31,915 | the oracle (see above) |

The tests gunzip each file in memory; no `.E01` is stored uncompressed. The `error2` section, table base 0 and
other layouts the real writer does not produce are covered by the `ewftest` builder only.

If `sgdisk` hangs in the container, an earlier container left a dead FUSE mount (a stuck `exfat` run) that
blocks the VM's global sync; remove that container, or abort its connection (`mount -t fusectl none /mnt`
in a privileged container, then `echo 1 > /mnt/<id>/abort` for the connection with a pending request).

The `error2` section is not produced by any variant (ewfacquire only writes it after a read error); its
layout was checked once against an image acquired from a device-mapper `error` target, outside the generator.

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

### F2FS determinism

Both F2FS images are byte-identical across runs (checked by generating twice and
comparing every output sha256). That needs:

- `faketime -f '2023-11-14 22:13:20'` around `mkfs.f2fs` and `sload.f2fs`, with
  `FAKETIME_DONT_FAKE_MONOTONIC=1` and `NO_FAKE_STAT=1` (the source files' own mtimes are
  what `sload.f2fs` copies; it stamps atime and ctime with the same value and leaves the
  creation time zero);
- `mkfs.f2fs -r`: the checkpoint version is seeded from `rand()` and `-r` fixes the seed
  (the version is the constant 1804289383); a fixed `-U` uuid and `-l` label;
- `f2fs_normalize.py`: `mkfs.f2fs` and `sload.f2fs` write the running kernel's `uname`
  string into the superblock's `version` and `init_version` fields, so the Docker Desktop
  kernel would end up in the fixture. The script replaces both fields in both superblock
  copies with a fixed text and recomputes each copy's checksum (it checks the stored
  checksums first; `fsck.f2fs` then verifies the new ones). This is the only host
  dependence found; the oracle's `generator.commands` lists it;
- the images live in `/tmp` and the source tree in `/dev/shm` (Docker caps `/dev/shm` at
  64 MiB); in every run the inode numbers followed the sorted names.

What `sload.f2fs` does with the tree: small files and symlinks (the largest inline one here is the 584-byte symlink; the 5000-byte file is not inline) become
inline data, the 600-entry directory gets several dentry blocks, and **holes are written
out as zero blocks**, so no F2FS fixture file has a hole (the "sparse" file is fully
allocated, 1026 blocks, and is the one whose addresses spill into a direct node). It also
fails with "Can't find free block" for a single file of 7 MiB (7000000 bytes work, 7340032 do not), whatever the
image size, so no fixture file needs an indirect node. Holes, indirect and double-indirect
nodes, and node-chain damage are left to the `f2fstest` builder tests. The fragmented
file is `/sparse-islands.bin` (the warm-data log skips the segment the cold-data log holds);
the generator fails unless one file has more than one extent. The two images differ only in
features: `f2fs-extra-attr` is `-O extra_attr,inode_checksum,sb_checksum,inode_crtime`,
`f2fs-default` uses the `mkfs.f2fs` defaults (no feature bits, no checksums).

The oracle's facts come from the tools, not from Minutiae: `fsck.f2fs -l` and `dump.f2fs
-d 1` (superblock and checkpoint fields, which must agree), `fsck.f2fs -t` (path to inode
number, which must list exactly the source tree), `dump.f2fs -n0~-1` (NAT: inode number to
block address), `fsck.f2fs -d 1 -M` (the data-block extents of every file with data blocks,
in file order; inline files and directories do not appear), `dump.f2fs -s0~-1` (the SIT
bitmap of every main-area segment; the free blocks are the clear bits, as inclusive
absolute block ranges in `free_blocks`, with their count; the popcount must equal the
checkpoint's `valid_block_count`, which fsck must also report as matching), `fsck.f2fs -f
--dry-run` (every check must be `[Ok..]`) and `blkid -p` (type, label, uuid). The image
hash is checked unchanged after the read-only tools ran. The tools print the feature bits
in unprefixed hex (`928` is `0x928`).

### F2FS deleted entries need a mountable kernel

F2FS keeps no deletion time and `sload.f2fs` cannot delete, so deleted entries exist only if
files are removed through a kernel mount. `f2fs.sh` tries `mount -t f2fs -o loop` and, when
it works, removes `/dir/removable-1.txt`, `-2` and `-3` and puts `/dir`'s mtime back to the
source tree's. The Docker Desktop kernel (6.6.87.2-microsoft-standard-WSL2) has no f2fs
module (`/proc/filesystems` lacks it, there is no `/lib/modules`, and the mount fails even
with `--privileged`), so the committed fixtures have `"deleted": []` and a `generator.note`
saying so; the three removable files are then ordinary live files in the oracle. Deleted-entry
coverage is left to the `f2fstest` builder tests. The mount path has never run on a kernel
with f2fs; if it does, the kernel stamps ctimes and checkpoint data from the real clock, so
such images are not byte-reproducible (the note says so) and the oracle (read from the final
image) still holds.

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

Consequence for the mkfs-only route: nothing in hfsprogs copies files into an
image, so those five fixtures are EMPTY volumes. The populated fixture
(`hfsplus-populated`) uses the route below instead.

### Populated image: the Linux hfsplus driver under QEMU (tier 2, reached)

Docker Desktop's kernel has no `hfsplus` module, but the Debian kernel that can
be installed in the image has (`CONFIG_HFSPLUS_FS=m`, `linux-image-6.1.0-53-amd64`
from bookworm). `hfsplus_populate.sh` boots it under QEMU software emulation
(`qemu-system-x86_64 -accel tcg`: no `/dev/kvm`, no `--privileged`, about 20 s)
with a busybox initramfs, the modules `virtio_blk`, `hfsplus`, `nls_utf8` and
the tarballs written by `hfsplus_tree.py`; the guest mounts the 16 MiB image
(`mkfs.hfsplus -c d=1,r=1`: one block of clump, because the driver preallocates
one clump per file and never gives it back, which fills a small volume),
extracts phase 1 (the tree and 400 one-block fillers), deletes the odd fillers,
extracts phase 2 (a 150-block file that fills the holes: 18 extents, so the
extents-overflow tree has records), unmounts and powers off. Run it with
`--shm-size=1g` (the work directory is under `/dev/shm`; it falls back to
`/tmp` if that is smaller than 256 MiB):

```bash
docker run --rm --shm-size=1g -v "$PWD:/work" -w /work minutiae-fixtures-hfs bash tools/fixtures/gen.sh hfsplus
```

What it holds (the oracle's `checks` fail the generator if any of it is missing):
a catalog of depth 3 (1674 leaf records), a 600-entry directory, a 3 MiB file,
a file with 18 extents plus extents-overflow records, symlinks (absolute,
relative, dangling), a three-link hard-link group with its hidden private
folder, mode/owner/mtime variety, UTF-8 names (stored decomposed), a
255-unit ASCII name and an 85-character CJK one.

Findings about a real kernel-written volume (all recorded in the oracle, all
reader-visible):

- **The private folder is named with four real NUL units** (`\0\0\0\0HFS+ Private Data`),
  as macOS does (the driver's source spells them U+2400 and maps them to NUL), and it
  sorts LAST among the root's children, after names up to U+65E5 (`oracle.private_folder`,
  `root_children_in_leaf_order`). `fsck.hfsplus` walks the tree with Apple's own
  comparison and accepts that order, so NUL folds above every other unit, as the reader
  assumes (`foldUnit(0) = 0xFFFF`); `TestRealPrivateFolderSortsLastAndIsFoundByDescent`
  checks every catalog key pair of this image against the reader's comparison and that
  the descent finds the folder with no scan. This settles the earlier "unverified" ruling
  for the HFS+ (case-folding) catalog.
- The hidden inode is named `iNode<random number below 2^30>` (the driver draws it
  with `get_random_bytes`); its link-record special fields hold the number, its own
  special field the link count.
- The driver writes the backup volume header when it MOUNTS (attributes: inconsistent
  set, unmounted clear) and does not refresh it at unmount; the primary header is
  correct. The oracle accepts a backup header that differs from the primary in the
  attributes word and the modify date only.
- Names are decomposed (NFD): `café` is stored `cafe` + U+0301. Names outside the BMP
  cannot be created (the utf8 NLS turns each byte of a 4-byte sequence into `?`).
- Hard-link records carry the original's dates and permissions only through the iNode:
  the reader (like macOS) takes everything but name and CNID from the iNode.
- A directory's valence, `fileCount` (counts the iNode files too) and `folderCount`
  (does not count the private folder) are recorded in the oracle.

Not covered by the populated image: deleted entries (HFS+ keeps none), decmpfs
compression (macOS-only), extended attributes (the driver can only write them
through `setfattr`; the attributes file is empty here), non-BMP names, journaling
(the driver mounts a journaled volume read-only), HFSX.

Reproducibility: the guest clock is virtual and frozen to the fixture clock
(`-rtc clock=vm -icount`), so every date is fixed. The one random number, the
hidden inode's name, is rewritten to `100000001` by
`hfsplus_normalize.py linkid` (the guest runs again when the random number has a
different number of digits). Regenerated 9 times with near-final scripts: the five empty fixtures were
byte-identical every time and `hfsplus-populated.img.gz` was byte-identical in 8
of the 9 runs (the odd one differed, cause not analysed: the oracle accepted it).
Earlier, one run was rejected by the oracle itself because the primary header's
modify date was a second after the backup header's (the oracle now accepts up
to 120 s). Treat byte-identity of the populated image as likely, not
guaranteed; the committed `expect.json` carries the sha256 of the committed
image and the tests check it first, so always regenerate `hfsplus-populated.img.gz` and
`hfsplus-populated.expect.json` TOGETHER (a different image with the old oracle fails the
sha256 check at once).

### Images (8 MiB raw each except the populated one, `internal/filesys/hfsplus/testdata/`)

| Image | `mkfs.hfsplus` | What it holds |
|---|---|---|
| `hfsplus-empty` | `-v FIXTURE` | HFS+ (H+, version 4), 4 KiB blocks, root folder and system files only |
| `hfsx-empty` | `-s -v FIXTURE` | HFSX (HX, version 5), case-sensitive (binary key compare 0xBC) |
| `hfsplus-journal` | `-J -v FIXTURE` | journaled: real `.journal_info_block` (CNID 17) and `.journal` (CNID 16, 512 KiB) catalog files in the root |
| `hfsplus-1k` | `-b 1024 -v FIXTURE` | 1 KiB allocation blocks (smaller than the 4 KiB B-tree node size) |
| `hfsplus-wrapped` | `-w -v FIXTURE` | classic HFS wrapper (master directory block `BD`) embedding the HFS+ volume at byte 45056 (`volume_offset` in the oracle) |
| `hfsplus-populated` (16 MiB) | `-c d=1,r=1 -v FIXTURE`, then filled by the Linux hfsplus driver (see "Populated image") | a real populated tree: 3-level catalog, 600-entry folder, 3 MiB file, 18-extent file with extents-overflow records, symlinks, hard links with the private folder, UTF-8 names |

What the five mkfs images validate against Apple's own formatter: the volume header (both
copies), the five special-file forks, B-tree headers and node maps, the
catalog key/record/thread layouts, the allocation bitmap, the journal files and
the wrapper embedding. What they do NOT: a populated tree. The populated image
covers a multi-level catalog, hard links, fragmented files, overflow extents,
decomposed Unicode and symlinks against a kernel-written volume. Still
builder-only (verified against the Go builder and Apple's fsck on builder images,
tier 3, `hfsplus.sh check-builder <dir>`, never against a macOS-written volume):
extended attributes, decmpfs compression, resource forks, deletion, HFSX with
content, journals with transactions, non-BMP names. macOS- and iOS-written
volumes are covered only by the manual `realimages` test.

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
`populated`. With `--source-tree` (the populated image) the oracle also resolves
the extents-overflow records (a fork's extents are its 8 inline ones plus the
records, which must continue block for block and none may be orphaned), finds
the private folder and the hard-link records, and compares the whole visible
tree with the source tree: names (NFD), types, modes, sizes, content sha256,
file mtimes, symlink targets and the hard-link groups. Checks of that image are
aggregated (digits normalized) in `checks`; the order of the catalog keys is
checked only for adjacent ASCII names and parent ids, the rest by `fsck.hfsplus`. Stated limitation: the Python parse is a second
implementation by the same author as the reader; its independence rests on
`fsck.hfsplus` and `blkid` agreeing, not on a different author.

Facts the oracle recorded about `mkfs.hfsplus` output (they shape the reader's tests):

- Volume dates are the frozen clock (1700000000); `.journal` is created one second later.
- The root folder and the journal files have no BSD info: `fileMode` is 0 for the root and `0o100000` (type bits only) for files, so `mode&0o7777` is 0; `attributeModDate`, `accessDate`, `backupDate` are 0 (absent).
- `-J` leaves the journal uninitialised: journal info flags are `in-FS|need-init` (0x5) and the whole journal is zero; there is no journal header to parse (the first mount writes it).
- **HFSX quirk:** `mkfs.hfsplus -s` leaves the root folder record without `kHFSHasFolderCountMask` (0x10), and Apple's `fsck_hfs` reports `HasFolderCount flag needs to be set (id = 2)` and exits 8; nothing else is reported. The generator keeps the formatter's real output, requires exactly that report (`--fsck-issue`) and records it in `external_tools.fsck.known_issues`. A builder HFSX image must set the flag to pass `check-builder`.
- `blkid` reports `TYPE=hfsplus` for HFSX and for the wrapped volume.

### Determinism

The five mkfs images are byte-identical across runs (generated twice, every output sha256 compared; the populated image: see above).
`mkfs.hfsplus` takes its dates from the clock, frozen with `faketime -f
'2023-11-14 22:13:20'` (`FAKETIME_DONT_FAKE_MONOTONIC=1`, `NO_FAKE_STAT=1`), but
draws the 64-bit volume id (finderInfo words 6-7) from a random source; two
runs differed only in those 8 bytes, in both header copies.
`hfsplus_normalize.py` rewrites the id (`MINUTIAE`, `4d494e5554494145`) in both
copies (in the embedded volume for the wrapped image). Also `gzip -n -9`, a
fixed work directory, `umask 022`, a root check and a cleanup trap.

### Builder images (`hfsplus.sh check-builder <dir>`)

Runs `fsck.hfsplus -n -f` on every `*.img` directly in `<dir>` and exits non-zero
when any fails. `MINUTIAE_WRITE_BUILDER_IMAGES=<dir> go test -run TestBuilderImagesPassFsck ./internal/filesys/hfsplus`
writes the builder's images (`builderImages()`: plain, multi-level catalog, HFSX
case-sensitive and case-folding, hard links, overflow extents, a catalog file
with overflow extents, attributes, resource fork, compressed files, wrapper,
wrapped HFSX, journaled, 1 KiB blocks); the ones fsck cannot accept go to
`<dir>/exempt/` with the reasons in `REASONS.txt`.

Result (2026-10-04): 12 of the 15 images pass `fsck.hfsplus -n -f`. Before that, the
run found two BUILDER defects, now fixed (and pinned by
`TestBuilderMatchesFsckExpectations`): a file with extended attributes must carry
`kHFSHasAttributesMask` (0x04) in its catalog flags (fsck counts the files that have
attributes: "Incorrect number of extended attributes"), and a hard-link record's
createDate must be the private folder's ("Bad hard link creation date"). The three
exempt images are accepted by the reader on purpose:

- `hardlinks-owned` (hard links whose files have a non-zero uid/gid): fsck says
  "filelink prime buckets do not match" / "Incorrect number of file hard links"; with
  uid = gid = 0 it passes (the builder's `hardlinks` image and the Linux-written
  fixture). The builder writes no link chain (prev/next link ids, `kHFSHasLinkChainMask`),
  which the Linux driver does not write either; the cause of the owner dependence is
  unconfirmed.
- `journaled-clean`: the builder lays out the journal info block and journal but
  adds no `.journal_info_block` and `.journal` catalog files, so fsck reports
  orphaned blocks (the real journal fixture has the files).
- `wrapped-hfsx`: Apple never embeds HFSX in a wrapper (the embedded signature must
  be `H+`); the reader accepts both on purpose.

## Adding a fixture

1. Add `<name>.sh` taking the output directory as `$1`; source `lib.sh` and
   wrap commands whose invocation belongs in the oracle with `run`.
2. Register it in `gen.sh`.
3. Keep images small: a few hundred KiB compressed at most. The raw size is set by
   the filesystem (the smallest FAT32 image is 34 MiB, see above; every other fixture
   is at most 16 MiB raw).

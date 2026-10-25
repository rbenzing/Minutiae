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

`gen.sh <fixture>` builds one fixture (`volume-gpt`, `volume-mbr`, `ext4`, `fat`, `exfat`, `f2fs`, `ewf`, `apfs`; `hfsplus`, `apfs-populated` and `ext4-xattr` each need their own image, see "HFS+ fixtures", "APFS populated fixture" and the `ext4_xattr.sh` row below).
`--privileged` is needed by `exfat` only (see below; `f2fs` no longer mounts, see "F2FS deleted entries come from the QEMU guest"); every other fixture runs without it.

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
| `ext4_xattr.sh` (+ `ext4_oracle.py`, `ext4_freeblocks.py`; NOT part of `all`, runs in the pinned `minutiae-recover-fixtures` image: `docker run --rm -v "<repo>:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/gen.sh ext4-xattr`, no `--privileged`) | `ext4-xattr` (`.img.gz` + `.expect.json`, in `internal/filesys/ext4/testdata/`; 4 MiB, 1 KiB blocks, `metadata_csum,64bit,ea_inode`) | as `ext4.sh` (source tree, `debugfs rm`, `dumpe2fs`) plus `xattrs`: for each attribute, its name and size from `debugfs ea_list`, the SHA-256 of its value from `debugfs ea_get -f` then `sha256sum`, and its storage kind (`inode`, `block`, `ea_inode`), asserted by the script against `debugfs stat` (the File ACL block of `/file1`, the EA-inode flag `0x200000`); `ea_inode` is the number of the inode that holds `user.huge`. libext2fs never shares an xattr block, so there is no shared block here (the shared case is tested with the builder only). `debugfs ea_set -f` reads at most one block, so the EA-inode value is 1024 bytes. `debugfs ea_set` leaves the parent's `i_blocks` short by the EA-inode value, so the script runs `e2fsck -fy` (exit 0 or 1; its report is printed, and the xattr block of `/file1` must be byte-identical before and after, so the repair provably touched only the parent's `i_blocks`) and pins `s_lastcheck`; it also pins the ctime and atime `mke2fs -d` copied from the real clock (`sif`), except the EA inode's (they hold the hash and reference count). Byte-identical across runs (generated twice, three seconds apart). It never regenerates a committed fixture: the generator image `minutiae-fixtures` has an unpinned base |
| `fat.sh` + `fat_tree.sh` + `fat_oracle.py` + `fat_chains.py` | `fat12`, `fat16`, `fat32` (`.img.gz` + `.expect.json`, in `internal/filesys/fat/testdata/`) | the source tree (`fat_oracle.py`: type, size, sha256, mtime), the generator's own list of deleted names, `fsck.fat -v` (geometry, data-area offset, cluster count, clusters in use) and `mshowfat` (the cluster chain of every live file and directory; the free clusters are the ones no live chain holds, `fat_chains.py` fails unless that count equals what `fsck.fat -v` reports in use; the tests compare the reader's unallocated space and every file's runs with them exactly) |
| `exfat.sh` + `exfat_chains.py` (also `fat_tree.sh`, `fat_oracle.py`) | `exfat` (`.img.gz` + `.expect.json`, in `internal/filesys/exfat/testdata/`) | the source tree, the generator's deleted names, and `dump.exfat` (boot report: geometry, serial, label; `-c -d <path>`: the cluster chain of every live path; the free clusters are the ones no chain, bitmap, up-case table or root holds, cross-checked against dump.exfat's "Free Clusters") |
| `apfs.sh` + `apfs_oracle.py` | `apfs-ci`, `apfs-cs`, `apfs-multichunk` (`.img.gz` + `.expect.json`, in `internal/filesys/apfs/testdata/`) | the `mkapfs` arguments (label, UUIDs, case flags) and an independent Python decoder of the finished image (see "APFS determinism and limits"); `apfsck` must accept the image |
| `apfs-populated.sh` + `apfs_tree.py` + `apfs_populate.sh` + `apfs_guest.c` + `apfs_populated_oracle.py` (image `minutiae-fixtures-apfs`, `Dockerfile.apfs`) | `apfs-populated` (`.img.gz` + `.expect.json`, in `internal/filesys/apfs/testdata/`) | a container populated by the real `linux-apfs-rw` kernel driver under QEMU from a source tree, a snapshot included; an independent Python decode of the image compared with that source tree, plus `apfsck` (see "APFS populated fixture") |
| `hfsplus.sh` + `hfsplus_oracle.py` + `hfsplus_normalize.py` (image `minutiae-fixtures-hfs`) | `hfsplus-empty`, `hfsx-empty`, `hfsplus-journal`, `hfsplus-1k`, `hfsplus-wrapped`, `hfsplus-populated` (`.img.gz` + `.expect.json`, in `internal/filesys/hfsplus/testdata/`; also `hfsplus_populate.sh` + `hfsplus_tree.py`) | an independent Python parse of the volume header, B-trees (extents-overflow records included), allocation bitmap and journal files, cross-checked against `fsck.hfsplus` and `blkid`; for the populated image also the source tree the Linux driver was fed (see "HFS+ fixtures") |
| `ewf.sh` + `ewf_inspect.py` + `ewf_normalize.py` | `ewf-disk.img.gz` (the raw source disk), `ewf-<variant>.E0N.gz` (one `.gz` per segment file of `single-none`, `single-best`, `multi-none`, `seed-small`) and `ewf-fixtures.expect.json`, in `internal/image/ewf/testdata/` | the raw disk (its sha256/md5/sha1/size, built from the committed ext4 and fat12 fixtures), an independent Python decoder (`ewf_inspect.py`: every section and Adler-32, every chunk, the rebuilt media equals the raw disk byte for byte), `ewfverify`, `ewfinfo` (whitelisted geometry and hash fields) and `ewfexport -f raw` (sha256 equals the raw disk) |
| `f2fs.sh` + `f2fs_tree.sh` + `f2fs_oracle.py` + `f2fs_normalize.py` | `f2fs-extra-attr`, `f2fs-default` (`.img.gz` + `.expect.json`, in `internal/filesys/f2fs/testdata/`) | the source tree handed to `sload.f2fs` (`f2fs_oracle.py tree`: type, size, sha256, mode, mtime, symlink targets), and the reports of `fsck.f2fs -l/-f/-t/-M`, `dump.f2fs -s/-n` and `blkid` on the finished image (`f2fs_oracle.py layout`: superblock and checkpoint fields, features, uuid, label, the NAT block address of every inode, the data-block extents of every file, the free main-area blocks from the SIT bitmaps). The oracle cross-checks the tools against each other (fsck vs dump on every shared field, SIT popcount vs `valid_block_count`, fsck's tree vs the source tree, the file map vs file sizes) and the generator fails on any mismatch, on an unclean `fsck.f2fs`, or when no file is fragmented |
| `fat_recover.sh` + `fat_recover_oracle.py` (image `minutiae-recover-fixtures`, `Dockerfile.recover`; `gen.sh fat-recover`) | `fat12-recover`, `fat16-recover`, `fat32-recover` (`.img.gz` + `.expect.json`, in `internal/filesys/fat/testdata/recover/`) | volumes written and deleted by the kernel `vfat` driver in the QEMU guest; an independent Python reader of the finished image checked against `mshowfat`, `fsck.fat -v` and the source tree (see "FAT recovery fixtures (3B)") |
| `exfat_recover.sh` + `exfat_recover_oracle.py` (image `minutiae-recover-fixtures`; `gen.sh exfat-recover`) | `exfat-recover` (`.img.gz` + `.expect.json`, in `internal/filesys/exfat/testdata/recover/`) | a volume written and deleted by the kernel `exfat` driver in the QEMU guest; an independent Python parse of the finished image checked against the allocation bitmap, `dump.exfat` and the source tree (see "exFAT recovery fixture (3B)") |

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

Caveat (ruling F38): `losetup` can autoload the `loop` kernel module in the Docker
Desktop VM or on a host where loop is a module rather than built in, and `/dev/fuse`
needs the fuse driver. This pre-dates the rule that kernel modules load only in a QEMU
guest and is allowed per construct by the guard test; moving the exFAT fixture to the
QEMU guest is a backlog item, not done here.

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
  blocks). A CAB layer needs more than `sm_cibs_per_cab` = 507 CIBs, that is more
  than 63,882 chunks (507 x 126), about 8 TiB: far too large to make or commit, so the
  CAB layout is builder-only.
- `apfs-big.sh` makes ONE empty container of more than 16 GiB (default 17 GiB, 136
  chunks) to learn what the real `mkapfs` writes for several chunk-info blocks (CIBs)
  and whether a chunk-info address block (CAB) appears. Its image is never committed
  (`*.big.img` and `*.sparse.img` are gitignored) and the script refuses to write one
  inside the repository. Every file it writes goes under `$APFS_BIG_WORK`, a host
  directory bind-mounted into the container, never the container's own filesystem or a
  Docker volume (those grow the Docker Desktop virtual disk, which never shrinks).
  The script resolves both paths with `pwd -P` and refuses (exit 2) any that lies inside
  the repository BEFORE it creates anything. It also refuses (exit 2, printing the
  figures from `df -Pk`) unless the free space at the scratch location is at least the
  image size plus 30 GiB.
  Probe (writes only under the scratch directory, which it empties again):
  `MSYS_NO_PATHCONV=1 docker run --rm -v "$PWD:/work" -v "$SCRATCH:/scratch" -e APFS_BIG_WORK=/scratch -w /work minutiae-fixtures bash tools/fixtures/apfs-big.sh`.
  Keep the image and write the oracle file: add `internal/filesys/apfs/testdata /scratch/apfs-bigcib.sparse.img`
  as the two arguments. The image is built once, in place, as `<keep>.partial` and
  renamed to the keep path only when every gate has passed, so a failed run leaves no
  image. Disk cost: a bind mount on Windows (Docker Desktop) may not preserve holes, so
  the image may occupy its full 17 GiB; the true peak is ONE image (about 17 GiB, plus
  the megabyte-sized oracle files), so keep at least 47 GiB free at the scratch location
  (or lower `APFS_BIG_SIZE`, which must stay above 15.75 GiB). The run is not part of
  `gen.sh all`. Determinism across runs is checked by hand, with one image on disk at a
  time: build, record the image's sha256, delete the image, build again at the same
  path and compare the two sha256 values; differing values mean the generator is not
  deterministic.
  Run the test on the kept image with `MINUTIAE_APFS_BIG_IMAGE=<keep path>`
  (`TestAPFSBigContainerMatchesOracle`; without the variable it SKIPS). The image and
  its oracle file `apfs-bigcib.expect.json` are regenerated together.
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

Limits: these three are EMPTY volumes (the populated one is "APFS populated fixture", below), so
there are no files, no extra directory records (only `root` and `private-dir`),
no xattrs, hard links, symlinks, file extents, snapshots or encrypted volumes, and a single checkpoint (no ring
wrap-around or fallback). Files, extents, xattrs, hard links, symlinks, clones and a snapshot are
covered by the populated fixture; encryption, compression, a second volume, several snapshots and
ring wrap-around by the synthetic builder only and, for real data, by the manual `realimages` test.

### Supplying a real APFS image (manual `realimages` test)

The committed `apfs-ci`, `apfs-cs` and `apfs-multichunk` fixtures are empty; `apfs-populated`
covers files, extents, clones, symlinks, xattrs, hard links, one snapshot and name hashes against an image
written by a Linux driver. Images written by Apple software (compression, encryption, several snapshots,
fusion layouts, extended inode fields) are validated against the synthetic builder only. An examiner can
validate them against a real, populated image with the build-tagged test
`internal/filesys/apfs/realimages_test.go`:

```bash
MINUTIAE_TEST_IMAGES=/path go test -tags realimages ./internal/filesys/apfs -run TestRealImages -v
```

It reads every `<name>.img` or `<name>.img.gz` in `$MINUTIAE_TEST_IMAGES/apfs/`
that has a `<name>.expect.json` beside it, opens the image with `apfs.Open` and
asserts: no checksum warning (other warnings are listed); the volumes, with role,
encryption and case-sensitivity; for every unencrypted volume the exact live tree
(path, type, size, SHA-256, `mode&07777`, mtime in seconds, symlink target,
hard-link groups) and the exact tree of each snapshot the JSON names under
`/<volume>/.snapshots/<name>/`; `ErrEncrypted` for an encrypted volume; the number
of unallocated blocks when given; and that no free run overlaps any file's runs.
The expected-output JSON is documented in the header of the test file:

```json
{"image_sha256": "<hex>", "unallocated_blocks": 12345,
 "volumes": [{"name": "Test", "role": "none", "encrypted": false, "case_insensitive": true,
   "tree": [{"path": "/a.txt", "type": "file", "size": 5, "sha256": "<hex>", "mode": 420,
             "mtime": 1700000000, "hardlink_group": "g1"},
            {"path": "/sym", "type": "symlink", "link": "a.txt", "mode": 511, "mtime": 1700000000}],
   "snapshots": [{"name": "snap1", "tree": []}]}]}
```

How to produce an image (from memory, NOT verified on a Mac here: check each
command on your machine):

1. Create a container image without a partition map:
   `hdiutil create -size 64m -layout NONE -fs APFS -volname Test test.dmg`, mount it.
2. Populate it: files, a hard link (`ln`), a symlink (`ln -s`), a sparse file
   (`dd if=/dev/zero of=sparse bs=1 count=0 seek=1048576` or `dd ... seek=`), a clone
   (`cp -c`), extended attributes (`xattr -w`).
3. Create a snapshot on the mounted test volume (`tmutil localsnapshot` only works
   on a boot volume; for a test volume use `fs_snapshot_create(2)`, or the
   `snapUtil` tool), then modify the live tree.
4. With the volume still mounted print the live tree:
   `tools/fixtures/apfs_expect.sh /Volumes/Test` (the `tree` array), and for each
   snapshot mount it (`mount_apfs -s <snapshot> <device> <dir>`) and run the same
   script on that mount point.
5. Unmount, detach, convert to a raw image (`hdiutil convert test.dmg -format UDTO -o test`
   writes `test.cdr`; rename it to `test.img`) and check that the first block holds
   "NXSB" at byte offset 32.
6. Put `test.img` (or `.img.gz`) and the JSON in `$MINUTIAE_TEST_IMAGES/apfs/`.

A data volume of an iOS or other device image works the same when it is
unencrypted; an encrypted volume is only asserted to be `ErrEncrypted`.
`go vet -tags realimages ./...` compiles the test (the normal check does not set the tag).

#### Populating a real APFS image on Linux: Docker Desktop kernel ruled out (Task 8)

Goal: a populated real fixture (files, extents, symlinks, xattrs, hard links, sparse
files, snapshots, name hashes on non-root names) with an oracle from the source tree
and `apfsck`. The first attempt was loading the out-of-tree `linux-apfs-rw` module into
the Docker Desktop VM kernel (`6.6.87.2-microsoft-standard-WSL2`, `CONFIG_MODVERSIONS=y`,
no module tree). `apfs.ko` built against the matching Microsoft source, but loading it
failed (`Invalid relocation target`, then `disagrees about version of symbol
module_layout`: the rebuilt kernel differs from the running one). The only remaining
way, editing the module's `__versions` table to defeat the ABI check, would load
out-of-tree code into the kernel every container on the machine shares, so it was
refused and not done: **no module is ever loaded into the Docker Desktop VM or the
host kernel**. (The one `insmod -f` attempt tainted that kernel until Docker Desktop was
restarted; `/proc/filesystems` never had `apfs`.) The route below runs the module in a
guest kernel of its own, inside QEMU, and needs no privileges at all.

### APFS populated fixture

`apfs-populated` is written by the real `linux-apfs-rw` kernel driver, so file data,
extents, clones, xattrs, hard links, symlinks, a snapshot and non-root name hashes are
validated against an image that neither the test builder nor Minutiae wrote. It lives in
a SEPARATE image, `minutiae-fixtures-apfs` (`Dockerfile.apfs`), so the main image's pinned
layers and every other fixture stay byte-identical (checked: `gen.sh apfs` in the main
image leaves `git status` clean). `apfs-populated` is deliberately not part of `all`.

```bash
docker build -f tools/fixtures/Dockerfile.apfs -t minutiae-fixtures-apfs tools/fixtures
MSYS_NO_PATHCONV=1 docker run --rm -v "$PWD:/work" -w /work minutiae-fixtures-apfs bash tools/fixtures/gen.sh apfs-populated
```

No `--privileged` is needed. A run takes about 1 minute.

How it works (the same route as the HFS+ fixture):

- `Dockerfile.apfs` (Debian 12 `bookworm-slim`, pinned by digest) installs the Debian kernel
  `linux-image-6.1.0-53-amd64` and its `linux-headers`, `qemu-system-x86`, `busybox-static`,
  and builds from pinned git commits `apfsprogs` v0.2.1 (`mkapfs`, `apfsck`, `apfs-snap`;
  commit `3721463b...`) and `linux-apfs-rw` v0.3.21 (commit `8a376002...`, built out of tree
  against those headers: `apfs.ko`, vermagic `6.1.0-53-amd64 SMP preempt mod_unload modversions`).
  The apt packages are pinned by version name but fetched from the live Debian mirror, which
  drops superseded kernel versions: a rebuild after that fails until the image is moved to
  `snapshot.debian.org` (as `Dockerfile.hfs` already is); the committed fixtures are unaffected.
- `apfs-populated.sh` formats a 32 MiB image with `mkapfs -L populated` (default:
  case-insensitive, hashed directory-record keys) under `faketime`, with fixed UUIDs, then
  `apfs_populate.sh` boots that Debian kernel under QEMU software emulation (`-accel tcg`, no
  `/dev/kvm`) with a busybox initramfs holding `virtio_blk`, `libcrc32c`, `apfs.ko`, the tarballs
  and operation lists made by `apfs_tree.py`, and a static helper `apfs_guest.c` (`FICLONE`,
  `setxattr`, `pwrite`, `utimensat`, the snapshot ioctl). The guest mounts the image `-o readwrite`,
  extracts the tree, writes the fragmented and sparse files, takes the snapshot `snap1`, changes the
  tree (a deleted file, a rewritten file, a new directory), unmounts cleanly and powers off. The
  guest clock starts at the fixture clock (`-rtc clock=vm -icount`). The module is never loaded
  anywhere but in that guest.
- the generator then requires `apfsck` to accept the image (exit status 0; it also checks that
  `apfsck` did not change it) and runs the oracle, `apfs_populated_oracle.py`.

What the image holds (the oracle fails the generator if any of it is missing): an fs tree of 2
levels (2913 records, 72 nodes; a 600-entry directory); a 3 MiB file; two files of 128 blocks
written alternately block by block with an `fsync` after each, so each is in over 100 extents; four
sparse files (hole at the end, hole first, hole between data, no data at all); six symlinks
(relative, absolute, dangling, to a directory, a 203-byte target, non-ASCII name); xattrs
(embedded, empty, binary, a stream-stored 6000-byte value, and one on a directory); a three-link
hard-link group; a clone pair sharing every extent (`FICLONE`: one physical run); NFC names
(Latin, Greek, Cyrillic, Hangul, Japanese, an emoji, `ß`, `İ`, a titlecase digraph) and NFD names
in another directory, case variants; 255-byte names; setuid, sticky and read-only modes, a
non-root owner; deep nesting; the snapshot `snap1` (xid 261) whose tree differs from the live tree;
free queues with pending blocks; four checkpoints in the ring.

The oracle (`apfs_populated_oracle.py`) extends `apfs_oracle.py` (written from Apple's APFS
reference, sharing no code with Minutiae). From the image alone it decodes the newest
checkpoint, the container and volume object maps (with their snapshot tree), the volume
superblock, the snapshot metadata tree, every fs-tree record of the live tree and of the
snapshot (inodes and their extended fields, directory records and their name hashes, xattrs,
file extents, sibling links and maps), the physical extent tree, the free queues and the space
manager with every bitmap. It compares the result with the SOURCE TREE (`apfs_tree.py`'s
`final/` and `snap1/` directories, `specials.json` for xattrs and clones), never with Minutiae,
and fails unless every path, type, size, SHA-256 of the content read through the decoded
extents, mode, owner, mtime to the nanosecond, symlink target, xattr name and value, and
hard-link group is equal for the live tree (678 paths) and for the snapshot (677). Its other
checks: every object checksum; every directory-record hash equals CRC-32C of the NFD,
case-folded name as UTF-32 without the NUL (true for all 678 records, non-ASCII ones included);
the snapshot metadata, name records, omap snapshot tree and `apfs_num_snapshots` agree;
`sm_free_count` = the sum of `ci_free_count` = the zero bits of the bitmaps; no block that any
reachable structure or file uses is free in a bitmap (blocks a bitmap marks allocated that
nothing reaches are listed: here one stale metadata object of an older transaction; the main
free queue's pending range is accounted for). The Go side compares the reader with it in
`TestAPFSMatchesOracle/populated` (tree, types, modes, owners, sizes, mtimes, hashes, xattr
names, per-file runs, content rebuilt from the runs, snapshot tree, record counts, exact
`Unallocated`) and in the examine end-to-end test `TestExtractFromPopulatedAPFS` (artifacts,
provenance, runs, snapshot extraction, exact unallocated export, `case verify`).

Findings about a real driver-written image (all recorded in the oracle):

- The driver stores symlink inodes with mode `0755` (Linux symlinks are `0777`); the oracle
  asserts every symlink is `0755`. Symlink targets are in the `com.apple.fs.symlink` xattr.
- Linux xattr names carry the fake prefix `osx.`; on the volume the names are bare
  (`osx.com.example.small` is stored as `com.example.small`).
- The snapshot ioctl does not flush inode changes still in memory (a symlink time change was
  missing from the snapshot until the guest ran `sync`): the guest syncs before `snap`.
- The driver keeps few physical-extent records (4 here); most files have `file_extent` records only.
- Deleting or rewriting a file after the snapshot leaves its old blocks allocated (the snapshot
  uses them); blocks freed in the newest transaction sit in a free queue and stay allocated in
  the bitmap until a later transaction (`Unallocated` does not report them, matching the oracle).
- The 22-bit directory-record name hash is CRC-32C (no final complement) of the UTF-32 code
  points of the NFD name WITHOUT the terminating NUL, after Unicode case folding on a
  case-insensitive volume (Python `casefold()` reproduces the driver's table for the names here:
  `ß`, `İ`, `ǅ`, Greek, Cyrillic, Hangul). The Go reader verifies this hash for every name that is valid
  UTF-8 and made only of code points assigned in Unicode 9.0 (all 678 stored hashes here verify).

Reproducibility: NOT byte-identical. Generated twice with the final scripts, the two images
had different sha256 values (`d1fb63ef...` and `4454c83c...`; with three earlier runs, five
different images in five runs). The cause is the number and timing of the transactions the
driver commits while the guest's background writeback runs, which QEMU's emulation does not
make deterministic; the logical content (every path, size, hash, mtime, xattr), the xid of the
final checkpoint (264) and the record counts were the same in every run checked, while block
addresses and some ids moved. So `expect.json` carries `generator.image_sha256` and the tests
check it first; always regenerate `apfs-populated.img.gz` and `apfs-populated.expect.json`
TOGETHER (a different image with the old oracle fails the sha256 check at once). `mkapfs`
itself is deterministic under `faketime`.

Not covered by the populated image: encryption, compression (`com.apple.decmpfs`), a second
volume, several snapshots or a deleted snapshot, a case-sensitive volume (the empty `apfs-cs` has
the flags only), fusion drives, space-manager CIB/CAB layers, deleted-entry recovery, and
anything a macOS writer does differently from this driver. Lookup of a name that differs from
the stored one only in Unicode normalization or case is found by the reader (the volume is
normalization-insensitive; `TestAPFSMatchesOracle/populated/name_lookup` asserts NFC and NFD queries).

`apfs-reoracle.sh <outdir>` re-emits the expectation file for the committed image with the current oracle (no QEMU): it refuses an image whose sha256 differs from the old oracle and any change other than the added `xattr_values`. The fixture's snapshot holds the same xattrs as the live tree, so the oracle cannot tell the two views apart: the snapshot view of xattr values is pinned by the builder test `TestXattrsReadFromTheSnapshotView`, not by this fixture.

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

### F2FS deleted entries come from the QEMU guest

F2FS keeps no deletion time and `sload.f2fs` cannot delete, so deleted entries exist only if
files are removed through a kernel mount. `f2fs.sh` no longer tries one (ruling F37, 3B): a
kernel mount of a modular filesystem can load a module into the Docker Desktop VM or the
host, which this repository never does; kernel mounts happen only in the QEMU guest (3A's
guest mounts f2fs, see "Recovery fixtures: toolchain probe"). The committed fixtures were
always built without the mount path (the Docker Desktop kernel has no f2fs module), so
they have `"deleted": []` and a `generator.note` saying so; the three removable files are
ordinary live files in the oracle, and deleted-entry coverage is left to the `f2fstest`
builder tests. Removing the mount path changes no committed fixture or oracle.

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

Reproducibility: the guest clock is virtual (`-rtc clock=vm -icount`) and the
guest rewinds it to the fixture time every 0.2 virtual seconds
(`hfsplus_populate.sh`), so every date the driver stamps is `2023-11-14 22:13:20`
whatever the host load. The cause of the earlier instability, found by comparing
a differing run with the committed image (`cmp -l`): exactly ONE byte differed,
the low byte of the primary volume header's modify date (offset 1047, `0x80` vs
`0x81`). Under `-icount` the guest needs about one virtual second from setting
its clock to unmounting, so the unmount stamp sat on the boundary of the next
second and host timing decided which side it fell on (3 of 9 regenerations
differed, more often on a loaded host); the oracle accepted every variant. The
rewinding loop removes the boundary, so the kernel-written field needs no
post-write normalization. The one random number, the hidden inode's name, is
rewritten to `100000001` by `hfsplus_normalize.py linkid` (the guest runs again
when the random number has a different number of digits). After the fix the
populated image was regenerated 15 times (12 of them with a second generator
running at the same time on the same host, 2 of them in a freshly rebuilt pinned image)
and was byte-identical to the committed one every time, as were all
12 committed files in the 5 runs compared in full (the five empty fixtures were
identical in every run compared). The package set is pinned in
`Dockerfile.hfs`: apt reads `snapshot.debian.org` as of 2026-10-03 and every
recorded package (`generator.packages`) is installed at its exact version, so a
rebuilt image either has the same toolchain or the build fails; a rebuild of the
pinned Dockerfile gave the same versions and byte-identical fixtures. The
snapshot service must stay reachable for a rebuild (an existing image keeps
working without it). Still, a different host CPU or QEMU could in principle
change timing, so the committed `expect.json` carries the sha256 of the
committed image and the tests check it first: always regenerate
`hfsplus-populated.img.gz` and `hfsplus-populated.expect.json` TOGETHER (a
different image with the old oracle fails the sha256 check at once).

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

## Recovery fixtures: toolchain probe (2026-10-07)

Sub-project 3 (deleted-data recovery) needs images in which a real kernel has
written and then deleted files. This section is the measured base that plans
3B-3H start from; nothing here is a Go test and nothing runs in the default test
run. Files: `Dockerfile.recover`, `recover_probe.sh`.

Build and run (no `--privileged`; the guest kernel runs under QEMU TCG, and every
`modprobe`/`insmod` happens inside that guest, never in the host or the Docker VM
kernel):

```bash
docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures
docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/recover_probe.sh
```

(Git Bash on Windows rewrites `/work`: set `MSYS_NO_PATHCONV=1` and pass the host
path in Windows form.) The build took about 4.5 minutes, the probe 40-70 s.

### Pins

- Base: `debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251`
  (the digest `Dockerfile.hfs` pins).
- apt reads `snapshot.debian.org` at `SNAPSHOT=20261003T000000Z` (main only; the
  same snapshot date as the HFS+ image). Every package is pinned to an exact
  version: `qemu-system-x86=1:7.2+dfsg-7+deb12u18+b3`,
  `linux-image-amd64=6.1.187-1`, `linux-image-6.1.0-53-amd64=6.1.187-1`,
  `busybox-static=1:1.35.0-4+deb12u1+b1`, `e2fsprogs=1.47.0-2+b2`,
  `f2fs-tools=1.15.0-1`, `exfatprogs=1.2.0-1+deb12u1`,
  `exfat-fuse=1.3.0+git20220115-2`, `dosfstools=4.2-1`,
  `mtools=4.0.33-1+really4.0.32-1`, `sqlite3=3.40.1-2+deb12u2`,
  `faketime=0.9.10-2.1`, `python3=3.11.2-1+b1`, `jq=1.6-2.1+deb12u2`,
  `gzip=1.12-1`, `kmod=30+20221128-1`, `cpio=2.13+dfsg-7.1`. The kernel is the
  concrete package `linux-image-6.1.0-53-amd64` (the metapackage `linux-image-amd64`
  is pinned to the same version). Built image id: `minutiae-recover-fixtures`
  (manifest sha256 `2d0f691d26bd...`, local build, not pushed).

### Measured facts (this run; nothing is estimated)

| Fact | Measured |
|---|---|
| Guest kernel | `6.1.0-53-amd64`, `/boot/vmlinuz-6.1.0-53-amd64` (8230848 bytes) |
| QEMU | `QEMU emulator version 7.2.22 (Debian 1:7.2+dfsg-7+deb12u18+b3)`, `-accel tcg`, no KVM, `-m 512` |
| ext4 driver | module (`CONFIG_EXT4_FS=m`; needs `crc32c_generic`, `jbd2`, `mbcache`, `crc16`); mounts under TCG: yes |
| f2fs driver | module (`CONFIG_F2FS_FS=m`, `CONFIG_F2FS_FS_COMPRESSION=y`; needs `zstd_compress`, `lz4_compress`, `lz4hc_compress`, `crc32_generic`); mounts under TCG: yes |
| exfat driver | module (`CONFIG_EXFAT_FS=m`, no dependencies); mounts under TCG: yes |
| vfat driver | modules `fat` + `vfat` (`CONFIG_FAT_FS=m`, `CONFIG_VFAT_FS=m`); mounts under TCG: yes |
| Guest actions per filesystem | write one 3-block file (12288 bytes), `sync`, `rm`, `sync`, `umount`: every step ok on all four |
| Optional modules | `crc32c-intel` and `crc32-pclmul` fail to insert in the TCG guest (`No such device`, the CPU model lacks the feature); the generic modules work, so they are not needed |
| Boot time (TCG) | kernel uptime 5.13 s when the modules are loaded (3.95 s and 4.28 s in two earlier runs); the whole four-filesystem guest run took 11, 12 and 16 s of wall time (qemu start to exit) |
| Guest clock | `date -s "2023-11-14 22:13:20"` succeeds, reads back `2023-11-14 22:13:20`, and advances (`22:13:21` after `sleep 1`). The probe does not pin it against drift: `hfsplus_populate.sh` shows how (`-rtc clock=vm`, `-icount`, a rewind loop) |
| Host tools | `mke2fs 1.47.0`, `mkfs.f2fs 1.15.0 (2022-05-13)`, `fsck.f2fs 1.15.0`, `exfatprogs 1.2.0` (`mkfs.exfat`, `dump.exfat`), `mkfs.fat`/`fsck.fat 4.2`, `mtools 4.0.32` (`mcopy`), `debugfs 1.47.0`, `sqlite3 3.40.1`, `faketime 0.9.10`, `Python 3.11.2`, `jq-1.6`, `gzip 1.12`, `BusyBox v1.35.0`, `exfat-fuse 1.3.0+git20220115-2` |
| `faketime -f "2023-11-14 22:13:20"` on host tools | `mke2fs` (with `-U` and `-E hash_seed`): two runs identical; `mkfs.f2fs -U`: identical; `mkfs.exfat` (no serial option; the serial comes from the clock): identical; `mkfs.fat -i`: identical; `mkfs.fat` without `-i`: identical (the id comes from the clock); `dumpe2fs` shows `Filesystem created: Tue Nov 14 22:13:20 2023` |
| Guest has no mkfs | the mkfs tools are dynamically linked and the initramfs holds only busybox: the host formats each 64 MiB disk, the guest mounts it (the brief's "mkfs inside the guest" was not done; formatting on the host is equivalent for these probes) |
| Unclean quit | `quit` on the QEMU monitor (`-monitor unix:<sock>,server,nowait`) ends the guest at once with no flush. The probe wrote 3 blocks to ext4 with no `sync`, quit, and `fsck.ext4 -n` reported `clean, 11/16384 files, 9513/65536 blocks`, exactly the figures of a freshly formatted disk: the unsynced write never reached the image |
| Host read-back | the guest disk is a plain raw file; after the guest exits, `e2fsck -n`, `fsck.f2fs` (on a copy; it has no dry-run switch and left the copy byte-identical), `fsck.fat -n`, `dump.exfat` and `debugfs` read it with no mount and no privileges |

A guest that must be stopped uncleanly (a crash image for journal or roll-forward
probes) is stopped with the monitor command `quit`. A build step reads the image back as above or with the Go
readers.

Reading these facts: the `/lib/modules/6.1.0-53-amd64/kernel/fs` listing in the probe
output also shows `hfsplus`, `hfs`, `btrfs`, `xfs`, `jfs` and others as
modules; only the four above were exercised.

### QEMU images are not byte-reproducible

The faketime result above holds for host-side `mkfs` only. An image a guest kernel
has written depends on transaction timing (the number and order of journal and
writeback commits under QEMU), as the populated HFS+ and APFS fixtures already
showed (their determinism notes above). Regenerate such an image TOGETHER with its
`expect.json` oracle, never one without the other, and have each test pin the image
sha256 against the oracle first. The vfat disk's volume serial in the probe differed
in every run (`0xeaeefad8`, `0x6af67b21`, `0x7beefb71`): the guest-side formatting
in the probe used no fixed id and no faketime.

### Unknown until the part's own probe

Not measured here and not to be assumed (each gets its own probe in the named part,
and the spec §18 "from memory" items stay marked until then):

- [ ] ext4 `i_block` / extent tree residue after unlink (3C)
- [x] `exfat-fuse` FAT-chain residue after delete (3B): superseded by ruling F3 (the
      fixtures use the KERNEL drivers in the QEMU guest, never exfat-fuse); the kernel
      exfat driver, on delete, clears the InUse bits of the entry set and the allocation-
      bitmap bits and KEEPS the FAT entries of a chained file (see "FAT/exFAT recovery probe (3B)", P3)
- [x] F2FS node-block survival after unlink, and `nodiscard` mount behaviour (3D): measured, see
      "F2FS recovery probe (3D)"
- [ ] jbd2 stale journal region contents (3G)
- [ ] F2FS roll-forward node chain after an fsync'd write and an unclean quit (3H)
- [ ] `sqlite3` WAL / rollback-journal leftovers (3J)
- [ ] whether the guest clock holds a fixed time without the rewind loop for a longer run

## FAT/exFAT recovery probe (3B)

Measured 2026-10-08 with `recover_fat_probe.sh` (library: `recover_guest.sh`) in the
`minutiae-recover-fixtures` image of the section above: kernel `6.1.0-53-amd64`, the
`vfat` and `exfat` drivers inside the QEMU TCG guest (`-smp 1`, no KVM, no
`--privileged`, no FUSE, no loop device, no host mount; the guest's own `/init`
loads the modules). The host formats (FAT16 4 MiB `mkfs.fat -F 16 -s 1 -i 1234ABCD`,
exFAT 8 MiB `mkfs.exfat -L PROBE -c 512`, both under `faketime`, both with
512-byte clusters) and READS the finished images with a raw Python reader
(cross-checked with `mshowfat`, `fsck.fat` and `dump.exfat`). Run:

```bash
docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/recover_fat_probe.sh
```

The probe took about 3 minutes. Differences from the plan's wording, forced by
measurement: the vfat guest also needs `nls_ascii` (the kernel's default
`iocharset` is `ascii`; without it the mount fails with `IO charset ascii not
found`); the guest links its applets itself (`busybox --install` in `/init`), so
the host side of the scripts never names a module command; each op list runs one
boot per stage and every stage image is kept, which is how "before" and "after" are
both read from files.

The module guard (`TestRecoverFixtureScriptsLoadNoKernelModules`, internal/archtest) checks every
`tools/fixtures/*.sh`, every Dockerfile's RUN lines and the subprocess calls of every `*.py`.
It is a tripwire against accidental host-side kernel or privilege use, not a sandbox:
deliberately obfuscated commands (string-built names such as `ins""mod`, indirection
through files a script writes itself) are out of scope, and reviewers read every
fixture script.

Stage sequence per filesystem (each stage is one guest boot on the previous
stage's image; files `A.BIN`..`F.BIN`; sizes in 512-byte clusters: A 6, B 4, C 8,
D 4, E 6, F 14): s1w write A,B,C,D; s2d `rm` B,D; s3w write E; s4f `dd
if=/dev/zero` until ENOSPC (`FILL.BIN`); s5d `rm` A,C; s6w write F (14 clusters =
A + C); s7d `rm` F; s9n (from an empty volume, ONE mount) write A,B,C, `rm` B,
write D, E. exFAT only: s8v length probes.

### Probe facts

| Id | Fact (this run, measured) |
|---|---|
| P1 | vfat, `rm`: the kernel sets the directory entry's first byte to `0xE5` and keeps everything else, including first cluster and size (checked for contiguous B and D and for the two-run F). It ZEROES every FAT entry of the chain (all 4 of B, all 14 of F, the end-of-chain mark included). A deleted vfat file keeps only (first cluster, size) in its directory entry; the chain is gone |
| P2 | vfat allocation order: ascending next-fit from the last allocated cluster, with wrap. Cluster 2 is not the first allocation (the first file of a fresh volume starts at cluster 3; cluster 2 is reached only after the search wraps: `FILL.BIN` = 23..8096 then 2). Within ONE mount a deleted file's hole is NOT reused while later clusters are free (s9n: A=3-8, B=9-12, C=13-20, `rm` B, D=21-24, E=25-30). After a fresh mount (every stage is one) the search restarts at cluster 3 and takes the lowest free clusters: after `rm` B,D, E (6 clusters) = 9-12,21-22. On a full volume with only A and C freed, F took them ascending: 3-8 then 13-20 |
| P3 | exFAT, `rm`: (a) contiguous file (B, D; also A, C): `NoFatChain` set, entry-set types `85 C0 C1` become `05 40 41` (InUse cleared), first cluster, DataLength and ValidDataLength kept, allocation-bitmap bits CLEARED, FAT entries never written (all 0 before and after). (b) fragmented file F (clusters 18-23 and 28-35, written into the two holes of a full volume; `NoFatChain` clear): InUse cleared (`85 C0 C1` to `05 40 41`), first cluster and size kept, bitmap bits CLEARED, and the FAT entries are KEPT byte for byte (13 links and the end-of-chain `ffffffff` are all still present after the delete). **F1 gate: the kernel exFAT driver keeps the FAT entries of a deleted chained file, so Task 8 (exfat-fatchain) PROCEEDS**; the Task 13 fixture must prove it (`fat_residue: "chain-kept"`). Not tested: how long the entries survive once the freed clusters are reallocated (a reallocation rewrites them) |
| P4 | exFAT allocation order: as vfat, except that the first allocation is the first free cluster of the heap (18 on this volume). Wrap-around to the heap start was NOT observed on exFAT (the fill ran to the last cluster, 12289, without needing it). Within one mount: next-fit (s9n: A=18-23, B=24-27, C=28-35, `rm` B, D=36-39, E=40-45; B's hole is not reused). After a fresh mount: lowest free clusters first (s3w: E = 24-27 then 36-37, a two-run file with `NoFatChain` clear). Full volume, A and C freed: F = 18-23 then 28-35, ascending |
| P5 | exFAT `ValidDataLength` < `DataLength` with busybox tools: NO. `truncate -s 5000` on an empty file, `truncate -s 4000` on a 100-byte file, `dd seek=6 count=1` (a write past the end) and `dd seek=8 count=0` all gave ValidDataLength == DataLength (5000, 4000, 3584, 4096). The `valid-data-length` scenario is therefore builder-only (a hand-built exfattest image), not a kernel fixture |
| P6 | `dd if=/dev/zero of=FILL.BIN bs=512` until ENOSPC leaves ZERO free clusters on both volumes (vfat 8095 clusters, exFAT 12288; free count derived from the FAT / bitmap is 0, and `dump.exfat` agrees: `Free Clusters: 0`). After `rm` of A and C (the only files freed) the free set is exactly A's and C's clusters (vfat 3-8,13-20; exFAT 18-23,28-35), and the file written next occupies exactly those clusters |

Decisions that follow: the F1 gate is open (Task 8 proceeds; a stale exFAT chain
is real on the kernel image); the `valid-data-length` scenario is builder-only
(P5). For the F20 check of Tasks 12-13: P2 and P4 are ascending within the free
set after a fresh mount and next-fit within a mount, so the write-order table
may assume "ascending, no reuse of a hole inside one mount while later clusters
are free"; the full-volume technique (free set exactly the target's clusters)
does not depend on it and stays the rule (ruling F19).

### Raw evidence (the `HEX` lines of the probe output)

vfat B.BIN (32-byte directory entry, clusters 9-12) and its FAT16 entries, before
and after `rm`:

```
entry BEFORE  422020202020202042494e200005aab16e576e570000aab16e570900d0070000
entry AFTER   e52020202020202042494e200005aab16e576e570000aab16e570900d0070000
FAT BEFORE    0a000b000c00ffff
FAT AFTER     0000000000000000
```

vfat F.BIN (two runs, 3-8 and 13-20):

```
entry BEFORE  462020202020202042494e200003aab16e576e570000aab16e570300581b0000
entry AFTER   e52020202020202042494e200003aab16e576e570000aab16e570300581b0000
FAT BEFORE    040005000600070008000d000e000f0010001100120013001400ffff
FAT AFTER     00000000000000000000000000000000000000000000000000000000
```

exFAT B.BIN (contiguous, clusters 24-27; the entry set is 3 x 32 bytes, printed
here one line per entry). Stream flags byte `03` = AllocationPossible + NoFatChain:

```
BEFORE  8502d12b20000000aab16e57aab16e57aab16e57080880808000000000000000
        c003000532ad0000d0070000000000000000000018000000d007000000000000
        c10042002e00420049004e000000000000000000000000000000000000000000
AFTER   0502d12b20000000aab16e57aab16e57aab16e57080880808000000000000000
        4003000532ad0000d0070000000000000000000018000000d007000000000000
        410042002e00420049004e000000000000000000000000000000000000000000
FAT bytes of clusters 24-27, before and after: 00000000000000000000000000000000
bitmap bytes 2-3 (clusters 18-33) before: ffff, after: 3ffc   (clusters 24-27 cleared, 18-23 and 28-33 untouched)
```

exFAT F.BIN (fragmented, clusters 18-23 and 28-35). Stream flags byte `01` =
AllocationPossible only, the chain is in the FAT:

```
BEFORE  850289ea20000000aab16e57aab16e57aab16e57050580808000000000000000
        c001000532af0000581b0000000000000000000012000000581b000000000000
        c10046002e00420049004e000000000000000000000000000000000000000000
AFTER   050289ea20000000aab16e57aab16e57aab16e57050580808000000000000000
        4001000532af0000581b0000000000000000000012000000581b000000000000
        410046002e00420049004e000000000000000000000000000000000000000000
FAT BEFORE  13000000140000001500000016000000170000001c0000001d0000001e0000001f00000020000000210000002200000023000000ffffffff
FAT AFTER   13000000140000001500000016000000170000001c0000001d0000001e0000001f00000020000000210000002200000023000000ffffffff
bitmap bytes 2-4 (clusters 18-41) before: ffffff, after: c003fc   (clusters 18-23 and 28-35 cleared; 24-27 belong to E and stay set)
```

exFAT A.BIN (contiguous, clusters 18-23), C.BIN (28-35), D.BIN (36-39), deleted by `rm`
(`HEX` lines; bitmap byte N covers clusters 2+8N .. 9+8N; FAT bytes of all these
clusters are zero before and after, the files being `NoFatChain`):

```
A  bitmap byte 2 (clusters 18-25)  before ff  after c0      (18-23 cleared)
C  bitmap bytes 3-4 (26-41)        before ffff after 03fc   (28-35 cleared)
D  bitmap byte 4 (clusters 34-41)  before 3f  after 03      (36-39 cleared; 34-35 stay set, they are C; 40-41 were free)
entry-set types for A, C, D: 85 c0 c1 before, 05 40 41 after
```

(The words below are abridged by hand from the probe output; the raw log is not stored. Rerunning the probe regenerates it, and the stage images differ only in timestamps and serial numbers.)
Allocation evidence (P2 vfat: FAT16 entries of clusters 2-31, two bytes each,
little endian; P4 exFAT: bitmap bytes 0-7 = clusters 2-65). The stage names are those
above; `ENTRY` lines are the reader's listing of the same image.

```
vfat s0  (empty)          000000000000000000...  all zero
vfat s1w (A,B,C,D)        0000 0400 0500 0600 0700 0800 ffff 0a00 0b00 0c00 ffff 0e00 ... 1400 ffff 1600 1700 1800 ffff
                          cluster 2 free, A starts at 3 (3->4->...->8 EOC), B 9-12, C 13-20, D 21-24
vfat s2d (rm B,D)         0000 0400 ... 0800 ffff 0000 0000 0000 0000 0e00 ... 1400 ffff 0000 0000 ...
vfat s3w (E)              ... 0a00 0b00 0c00 1500 ... 1600 ffff   E = 9-12 then 21-22 (12->21, 22 EOC)
vfat s4f (full)           ffff 0400 ...                  cluster 2 = ffff: FILL.BIN ends there
vfat s4f tail, 8090-8096  9b1f 9c1f 9d1f 9e1f 9f1f a01f 0200   cluster 8096 -> 2 (wrap), 2 -> ffff
vfat s5d (rm A,C)         ffff 0000 ... (3-8 zero)  0a00 0b00 0c00 1500 0000 ... (13-20 zero)
vfat s6w (F)              ffff 0400 0500 0600 0700 0800 0d00 0a00 ...   F = 3-8 then 13-20 (8->13)
vfat s9n (one boot)       0000 0400 ... 0800 ffff 0000*4 0e00 ... 1400 ffff 1600 1700 1800 ffff 1a00 1b00 1c00 1d00 1e00 ffff
                          A 3-8, (B 9-12 deleted, free), C 13-20, D 21-24, E 25-30: no hole reuse
exfat s0 (empty)          ffff000000000000      clusters 2-17 in use (bitmap, upcase, root), 18+ free
exfat s1w                 ffffffff3f000000      A 18-23, B 24-27, C 28-35, D 36-39
exfat s2d (rm B,D)        ffff3ffc03000000
exfat s3w (E)             ffffffff0f000000      E = 24-27 and 36-37 (hole B, then hole D)
exfat s4f (full)          ffffffffffffffff      and bitmap bytes for clusters 12234-12289: ffffffffffffff
exfat s5d (rm A,C)        ffffc003fcffffff      18-23 and 28-35 free
exfat s6w (F)             ffffffffffffffff      F = 18-23 then 28-35
exfat s9n (one boot)      ffff3ffcff0f0000      A 18-23, hole 24-27 (B), C 28-35, D 36-39, E 40-45
```

Listings (`ENTRY` lines of the probe output):

```
vfat  s4f   A 3-8, E 9-12,21-22, C 13-20, FILL.BIN start 23 size 4134400 chain 23-8096,2
vfat  s9n   A 3-8, D 21-24, C 13-20, E 25-30
exfat s4f   A 18-23, E 24-27,36-37 (NoFatChain clear), C 28-35, FILL.BIN 38-12289 (NoFatChain set)
exfat s9n   A 18-23, D 36-39, C 28-35, E 40-45
```

exFAT length probes (P5): ValidDataLength (stream offset 8) and DataLength (offset
24) in each entry set as the guest left it:

```
T1 truncate -s 5000 on an empty file   VDL 8813000000000000  DataLength 8813000000000000
T2 100-byte file, truncate -s 4000     VDL a00f000000000000  DataLength a00f000000000000
T3 dd seek=6 count=1                   VDL 000e000000000000  DataLength 000e000000000000
T4 dd seek=8 count=0                   VDL 0010000000000000  DataLength 0010000000000000
```

## FAT recovery fixtures (3B)

`fat_recover.sh` (+ `fat_recover_oracle.py`, library `recover_guest.sh`) builds three FAT volumes that the KERNEL
`vfat` driver writes and deletes inside the QEMU TCG guest (ruling F3: no mtools writes, no `mdel`, no kernel module on
the host, no `--privileged`). Output, in a subdirectory so `fixture_test.go`'s `testdata/*.img.gz` glob and its
old-schema oracle never see it (ruling F15): `internal/filesys/fat/testdata/recover/{fat12,fat16,fat32}-recover.img.gz`
and `.expect.json` (each at most about 150 KiB gzipped, ruling F6). Regenerate image and oracle together (QEMU images are
not byte-reproducible; `generator.image_sha256` binds them, every test checks it first):

```bash
docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures
docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/gen.sh fat-recover
```

`fat-recover` and `exfat-recover` are in the not-in-`all` list of `gen.sh`; `gen.sh` stops, naming the image to build,
when QEMU or the guest kernel is missing (checked for every target). `exfat-fuse`, installed by `Dockerfile.recover`,
is unused. Self-check: `TestRecoverFixturesHoldWhatTheyClaim` (internal/filesys/fat).

Images (`mkfs.fat -i 1234ABCD -n FIXTURE -s 1` under `faketime`, 512-byte clusters, guest `-t vfat -o utf8`):

| Image | Size | Notes |
|---|---|---|
| `fat12-recover` | 1440 KiB | |
| `fat16-recover` | 8192 KiB | |
| `fat32-recover` | 33040 KiB, `-f 1` | the minimum for a 512-byte-cluster FAT32 (65536 clusters) with ONE FAT copy: the FAT of a full volume is nearly incompressible, and anything larger exceeds the 150 KiB budget |

### How the scenarios are made (placement is never trusted: rulings F19, F22)

Probe facts P2 and P6 (above) give the allocator model: next-fit with wrap inside one mount, the lowest free cluster
at or above the first data cluster after a fresh mount, and a free set that is EXACTLY the freed files' clusters on a
full volume. The script relies only on this: every scenario family has its own directory (deleting a target frees only
that family's clusters and a new file cannot take its directory slot); the volume is FULL (`fill.bin` written until
ENOSPC) before every hole-consuming or reuse step; writes go only to writer directories made in boot 1; the writing
phase W precedes the deleting phase D (only `rm`), so the final free set is the union of the deleted files' clusters
whatever order the allocator chose. Four guest boots, each a fresh mount on the previous boot's image:

1. populate: every directory first, then the family files (the volume is not full);
2. `fill.bin` until ENOSPC, then the hole steps: `rm a.bin c.bin` and write the 6-cluster `f.bin` (the only free
   clusters are a's and c's, so `f.bin` is fragmented around `b.bin` / `D.BIN`) in `frag-live` and `frag-dead`;
3. phase W, one family at a time, each ending with the volume full again (the "ascending" row of F22: the writes take
   the freed clusters in ascending order);
4. phase D: `rm` only.

The host reads the image after boots 1, 2 and 3 (`fat_recover_oracle.py snap`: the true chain of every live file,
needed for files deleted later; the volume must be full after boot 2 and have exactly the lfn file's 3 clusters free
after boot 3) and after boot 4 (`final`). Four boots instead of the plan's two because the true chain of a file the
guest deletes before the image is read cannot be known otherwise (`c.bin` of the frag families, the files written in
phase W).

| Family (directory) | Steps | Final state |
|---|---|---|
| `contiguous` | `target.bin` (5 clusters), `rm` in D | intact on disk: contiguous-equal |
| `zero-size` | `target.dat` (0 bytes), `rm` in D | skip `empty` |
| `reuse-all` | `rm target.bin` (6) in W, then `/reuse-all-w/took.bin` (6) | every cluster live: skip `no-free-bytes` |
| `reuse-head` | `rm target.bin` (6), `/reuse-head-w/took.bin` (2) then `rest.bin` (4) | head and rest live: skip `no-free-bytes` |
| `cut-l` | `rm target.bin` (6), `/cut-l-w/filler.bin` (4, listed overwriter, `rm` in D) then `tail.bin` (2, live) | target cut after 4 clusters, overlapping the listed `filler.bin` (confidence 20; filler 30) |
| `cut-h` | `rm target.bin` (6), `/cut-h-hid/filler.bin` (4) then `/cut-h-w/tail.bin` (2, live), `rm -r /cut-h-hid` in D | target cut after 4 clusters holding the hidden overwriter's bytes (not equal to the source, no listed overlap) |
| `frag-live` | `a.bin` (2), `b.bin` (3, stays), `c.bin` (4); `rm a c`; `f.bin` (6) takes both holes; `rm f.bin` in D | `f.bin` is fragmented around the live `b.bin`: cut after its first fragment |
| `frag-dead` | `a.bin` (2), `D.BIN` (3, an upper-case 8.3 name: no long-name entries, so its deleted entry is short-only, F104), `c.bin` (4); `rm a c`; `f.bin` (6); `rm D.BIN f.bin` in D | `f.bin`'s contiguous range is all free but holds `D.BIN`'s bytes: complete, not equal to the source |
| `lfn` | the ONLY entry `a-long-deleted-name-for-slot-reuse.bin` (3 clusters, 3 long-name entries); `rm`, then create the empty `NEW.TXT` | `NEW.TXT` takes the first slot (the first on-disk long-name entry, the end of the name): `lfn-tail-lost` |
| `gone` | `gone/inner.bin` (2), `rm -r /gone` in D | the directory is a deleted `not-a-file`; `inner.bin` is under a deleted directory (`listed: false`) |

Helpers of a scenario that are deleted entries too (the `c.bin` hole, `D.BIN`, the writers' `filler.bin`, the hidden
`cut-h-hid` directory) carry the family `<family>-aux`, so the self-check's per-family assertions (for example "every
`frag-*` entry has two fragments") apply to the scenario's own file. The slot of a hole's `a.bin` is taken by `f.bin`
(the same slot size), so only `c.bin` remains as a deleted entry there; the oracle states what the image holds either way.

### The oracle (`fat_recover_oracle.py`, never Minutiae)

An independent Python reader of the finished image (BPB, FAT12/16/32, directories, long names) cross-checked against
`mshowfat` (every live chain), `fsck.fat -v` (geometry, clusters in use) and the source tree the guest was fed (every
live file's bytes). From the image alone it derives, per deleted short entry (in every live directory, plus the first
cluster of a deleted directory): `range` = the first cluster for ceil(size/512) clusters; `captured` = the leading part
of `range` whose clusters are free (no live chain holds them; the count equals the FAT's zero entries and `fsck.fat -v`);
`cut`; `outcome` (skip `not-a-file`, `empty`, `no-free-bytes`, or recover with method `fat-contiguous`);
`equal_source` (the final bytes of `captured` equal the source, uncut only); `overlaps` (other LISTED entries whose
captured ranges intersect); `confidence` = min(55, 20 if cut, 30 if overlaps); `name_state` (`lfn-intact` when the
deleted long-name entries, checksummed against the short name with its first byte recovered, spell the source name,
`lfn-tail-lost` when they spell a proper prefix, `short-only` without any); `true_chain` from the snapshot that saw the
file live. A deleted entry is tied to its source file by directory, kind and size (and long-name text), and the script
fails when that is not unique. The generator exits 1 with `family <name> missing: ...; re-rule, do not edit the ops`
when a required outcome family (`contiguous-equal`, `empty`, `cut-prefix`, `skip-no-free-bytes`, `not-equal-complete`,
`overlap`, `lfn-tail-lost`, `short-only`) occurs in none of the three images, and when a scenario family or the shape of the
contiguous / fragmented files is missing from one. No test compares timestamps from these images.

NOT produced by a real driver (builder tests only): cross-linked chains, bad or reserved clusters inside a range, orphan
LFN sets, a first cluster outside the volume, a size larger than the volume.

## exFAT recovery fixture (3B)

`exfat_recover.sh` (+ `exfat_recover_oracle.py`, library `recover_guest.sh`) builds one exFAT volume that the KERNEL
`exfat` driver writes and deletes inside the QEMU TCG guest (ruling F3: no `exfat-fuse`, no loop device, no
`--privileged`, no kernel module on the host). Output, in a subdirectory (ruling F15):
`internal/filesys/exfat/testdata/recover/exfat-recover.img.gz` (130494 bytes) and `.expect.json`. Regenerate image and
oracle together (QEMU images are not byte-reproducible; `generator.image_sha256` binds them):

```bash
docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/gen.sh exfat-recover
```

The script exits 1 when it wrote no image (ruling F102), so `gen.sh exfat-recover` never reports success for nothing.
Self-check: `TestExfatRecoverFixturesHoldWhatTheyClaim` (internal/filesys/exfat). `exfat-fuse`, installed by
`Dockerfile.recover`, is unused.

The image is 8 MiB, `mkfs.exfat -L FIXTURE -c 4096` under `faketime` (1536 clusters of 4096 bytes), guest `-t exfat`. The
method is that of "FAT recovery fixtures (3B)" (rulings F19, F22): one directory per family, the volume FULL (`fill.bin`
until ENOSPC) before every reuse step, writer directories made first, phase W before phase D (only `rm`), four fresh-mount
guest boots with host snapshots after boots 1-3 for the true chains. Sizes are `k*4096-7` bytes.

| Family (directory) | Steps | Final state |
|---|---|---|
| `contiguous` | `target.bin` (5 clusters), `rm` in D | the kernel keeps `NoFatChain` for it: `exfat-nofatchain`, equal to the source, 75 |
| `zero-size` | `target.dat` (0 bytes), `rm` in D | skip `empty` |
| `reuse-head` | `rm target.bin` (6), `/reuse-head-w/took.bin` (2) then `rest.bin` (4) | every cluster live: skip `no-free-bytes` |
| `cut-h` | `rm target.bin` (6), `/cut-h-hid/filler.bin` (4) then `/cut-h-w/tail.bin` (2, live), `rm -r /cut-h-hid` in D | target cut after 4 clusters holding the hidden overwriter's bytes (20) |
| `chained` | `a.bin` (2), `b.bin` (3, stays), `c.bin` (4); `rm a c`; `f.bin` (6) takes both holes, so it is fragmented and gets a FAT chain; `rm f.bin` in D | `f.bin`: `no_fat_chain` false, `fat_residue` `chain-kept` (probe fact P3: the kernel keeps the FAT entries), `exfat-fatchain` 65 (30 with the overlap); the hole's `c.bin` is `NoFatChain`, complete and not equal to the source |
| `gone` | `gone/inner.bin` (2), `rm -r /gone` in D | a deleted directory (`not-a-file`); `inner.bin` is under it (`listed: false`) |

`valid-data-length` is builder-only: probe fact P5 (`ValidDataLength` below `DataLength` cannot be reached with the guest's
tools), so the oracle exits 1 if any entry has them differ. Helper entries carry the family `<family>-aux` (as in the FAT
fixtures). The oracle is an independent Python parse of the finished image (boot sector, FAT, allocation bitmap, entry
sets), cross-checked against the allocation bitmap (its zero bits equal the clusters no live chain holds), `dump.exfat`
(cluster size, total clusters, `Free Clusters`), and the source tree (every live file's bytes). The pinned `dump.exfat`
(exfatprogs 1.2.0) has no per-file option, so `no_fat_chain` is cross-checked against the FAT instead: a `NoFatChain` file's
FAT entries are zero, a chained file's are all written (and the flag must not change between the snapshot that saw the file
live and its deleted entry). The flag is read from the Stream entry (bit 1 of its flags), independently of the FAT; the FAT check runs over the file's own run at SNAPSHOT time only (not on the final image) and fails the generator closed on a mismatch, so a NoFatChain file that sits on clusters holding stale FAT entries of an earlier chained file stops the run instead of being guessed past. `exfat_chains.py` (the exFAT listing fixture) needs `dump.exfat -c -d <path>`, which exfatprogs 1.2.0 (this recover image) lacks: it belongs to the default `minutiae-fixtures` image, not to `minutiae-recover-fixtures`. Per deleted file the oracle adds `no_fat_chain`, `data_length`, `valid_data_length` and
`fat_residue` (`not-chained`, `chain-kept` or `chain-cleared`, read from the FINAL FAT along the true chain); `expect.method`
is `exfat-nofatchain` (75) when `no_fat_chain`, `exfat-fatchain` (65) when the chain is kept, else `exfat-contiguous` (55),
with the caps 20 (cut) and 30 (overlap). The generator exits 1 naming a missing family (`nofatchain-equal`, `empty`,
`chained`, `skip-no-free-bytes`, `cut-prefix`, `not-equal-complete`), and exits 1 with "re-rule F1" if the chained file's
FAT chain is not kept. The FAT-fixture-only fields `name_state` do not apply (exFAT names are always whole entries).

## F2FS recovery probe (3D)

What does the KERNEL f2fs driver (Debian `6.1.0-53-amd64`, QEMU TCG guest) leave behind after
unlink, rename and a fill-to-ENOSPC, with and without discard? Plan 3D's index, SSA check,
version rule and fixture families branch on the answers. Files: `f2fs_recover_probe.sh` (stages),
`f2fs_recover_oracle.py` (independent decoder, subcommand `probe`; Task 9 adds `snap` and `final`),
`recover_guest.sh` (new `f2fs` arm; `GUEST_DRIVE_OPTS` is appended to the `-drive` value and
`GUEST_TIMEOUT` sets the time limit, default 900 s), `lib.sh` (`stamp <bytes> <seed>`: every 4 KiB
block is the 32-byte record `<seed>:<block>`, NUL-padded and repeated, so any block found anywhere
is traceable and the payload compresses).

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures \
  bash tools/fixtures/f2fs_recover_probe.sh > probe.txt 2>&1
```

No `--privileged`; every write runs in the guest, the host formats (128 MiB, `faketime`,
`mkfs.f2fs -O extra_attr,inode_checksum,sb_checksum`, `f2fs_normalize.py`) and reads the finished
images. Work files live under `/tmp` inside the container (removed on exit). Guest stages plus the
read-only tool captures took 103 s of wall time (run of 2026-10-25); the decode takes seconds.
Development aid: `PROBE_KEEP=<dir>` copies the stage images and captures out so the decoder can be
re-run without the guest.

### What ran

Two workloads, each on a fresh volume (the large one is an ADDITION to the plan's p1-p5, made because the small one cannot decide G2, G3 and G5, see below). **small** (the plan's): p1 writes `/p/A` (20 KiB), `/p/B`
(5 MiB), `/p/C` (12 MiB), `/p/H` (blocks 0 and 3 only), `/p/I` (100 bytes, inline), `/p/R`, and
`/s/x/f1..f3` (a small, inline directory); p2 `mv /p/R /p/R2`; p3 removes A B C H I R2 and f1;
p4 creates `/p/N1..N8`; p5 `dd if=/dev/zero` to ENOSPC (`df` showed 100 %), `sync`, `rm`.
Mounted `nodiscard,noatime`, one boot per stage, every stage image kept. **large**: the same files
between 300 and 900 empty files in `/g`, created `f2/f3` after the second batch, and `rm -r /g` with
the unlinks (stages l1, l2, l3, fill l5). The large workload exists because in the small one every
stale node block still lies in a CURRENT segment (no SSA block is written for those, nothing is
freed), so G2 and G3 cannot be decided there. Discard images: d1/d3 (small) and m1/m3 (large) with
`-o discard,noatime` and QEMU `discard=unmap`. After each boot `fsck.f2fs -f --dry-run` ran on a
copy (exit status 0 for all 14 captured images), plus `fsck.f2fs -t`, `dump.f2fs -d 1 -s0~-1`, `-n0~-1` and
`-a0~-1`. The decoder's superblock and checkpoint fields, every NAT entry and every SIT segment
(type, valid count, valid map) agree with `dump.f2fs` on all 14 compared images, `fsck.f2fs -t`
agrees with the decoded paths and inode numbers, and the inode checksum of every decoded inode
verifies (`CROSSCHECK` lines).

### Facts (small = the plan's workload, large = with `/g`; both agree unless stated)

| Q | Fact | Value | Evidence (`HEX` lines of the probe output, below) |
|---|---|---|---|
| Q1 | stale inode, dnode and indirect blocks at the pre-delete NAT address | byte-identical to stage W for A, B, C and also H, I, R2, f1, in both workloads (12 node blocks in each) | `Q1 inode A head/footer`, `Q6 A inode ... before/after` |
| Q2 | NAT after the delete | every deleted nid resolves to address 0 (version 1, ino kept); the other copy still holds the old address; the NAT journal holds 0 entries, the SIT journal 1 | `Q2 NAT entry` |
| Q3 | SIT of the stale node segments | the deletes leave the segment with `vblocks` 0 and its node type (warm node, type 4; the cold-node segment of C's indirect node is type 5); the type survives the free | `Q3 SIT entry` (`0x1000` = type 4, count 0) |
| Q4 | SSA of a stale node block in a closed segment | `nid` equals the footer nid, version 0, ofs 0, footer type byte 1 (node); in a current segment the SSA block is not written (nid 0, type 0). `cur_node_segno` is main-relative and the block at main + seg*512 + blkoff - 1 is the last written node with its SIT bit set (checked on all three node cursors of stage W). The SSA part rests on 11 blocks from one segment (large) | `Q4 SSA entry` |
| Q5 | footer `cp_ver` of stale copies | low 32 bits = the checkpoint version in force when the block was written (in the small and large workloads: 1804289383 = mkfs's; 1804289387 for the renamed inode's second copy), all below stage D's 1804289396; the high 32 bits are 0 for mkfs's version and a CRC afterwards (`0xe69e1e81`, `0xaa1d15fb` in two runs); `CP_CRC_RECOVERY_FLAG` (0x40) is set in every checkpoint (`ckpt_flags` 0xc5, compact summaries) | `Q5 footer` |
| Q6 | discard (`discard,noatime` + `discard=unmap`) | large: data blocks outside the current segments 4096 of 4096 zero; stale node blocks outside the current segments 11 of 11 zero (small: no block lies outside the current segments, undecidable). Inside the current segments (large): 12 of 268 data blocks and 0 of 1 node blocks (C's indirect node, cold node cursor) are zero. The zeroing inside current segments is unexplained (observed, not a rule); "gone" is measured outside them, on kernel 6.1 with QEMU `discard=unmap` only | `Q6 B data`, `Q6 A inode` |
| Q7 | node numbering | B uses `i_nid[0]`; C uses `i_nid[0]`, `i_nid[1]` and `i_nid[2]` (indirect, one direct child); every dnode and indirect node has footer ino == owner and `ofs` equal to the 3D-R26 table (1, 1, 2, 3, 4) | `Q7` lines |
| Q8 | copies per deleted nid | 1 copy each, 2 for the renamed inode (versions 1804289383 and 1804289387); no same-version tie among the 12 nids | `Q8` lines |
| Q9 | rename | the newest copy of the inode (version 1804289387) has `i_name` `R2`; the older copy (1804289383) has `R`; both survive the delete | `Q9 inode ... i_name` |
| Q10 | nid reuse | small: `/p/N1..N8` took the freed nids 7-14 (lowest first). With the `/g` churn (an EARLIER run of this probe with a different workload: `/g` files, and f2/f3 created before the second batch; raw output `f2fs-probe/probe-run2-earlier-workload.txt`, the only churned-volume measurement) they took 455-462, new nids, and no freed nid | `Q10 NAT entry` |
| Q11 | fill to ENOSPC (`foreground GC` 0, `background GC` 0) | small: B's and C's data blocks (1280 and 3072) are overwritten while their stale inodes stay intact; A, H, R2 data blocks are untouched. large: the freed node segment is reused as a DATA segment (SIT type 1): 11 of the 13 stale node copies are overwritten, the cold-node copy and the copy in the current warm segment stay | `Q11 B data` |
| Q12 | inline directory `/s/x` after `rm f1` | the dentry bitmap bit is cleared and the record keeps hash, ino (18) and name `f1`; the other slots are `.`, `..`, `f2`, `f3` | `Q12 dir inode inline area` |
| Q13 | `dd seek=3 conv=notrunc` | the hole blocks 1 and 2 are `NULL_ADDR` (0), the written blocks 0 and 3 have addresses | `Q13 H i_addr` |

### Gates

| Gate | Verdict | Basis |
|---|---|---|
| G1 | HOLDS | A, B, C (and H, I, R2, f1) stale node blocks byte-identical to stage W in both workloads; every `ofs` equals `nodeOffset` |
| G2 | KEEP | large: 11 stale node blocks in non-current segments (all from one segment), 0 with `ssa.nid != footer.nid` (small: undecidable, all in current segments) |
| G3 | SIT | large: all 11 stale copies lie in segment 1, left at `vblocks` 0 and still typed warm node (type 4); SSA footer type is node as well |
| G4 | HOLDS | every copy of every deleted nid has `low32(cp_ver)` below stage D's checkpoint version, in both workloads |
| G5 | gone | stale node blocks outside the current segments are zero after a discard mount, and so are the data blocks (large: 11/11 and 4096/4096); inside current segments the zeroing is unexplained and not a rule; kernel 6.1 with QEMU `discard=unmap` only |
| G6 renamed | real | Q9, in both workloads |
| G6 nidreuse | real, SMALL workload only | Q10: ran only in the small workload (not run in the large one); the earlier churned run took new nids |
| G6 reuse | real, SMALL workload only | Q11: freed data blocks overwritten while the stale inode is intact: true for B and C, false for A, H and R2; large workload: builder-only (the fill overwrites the stale inodes too) |

Ruling 3D-C1: R20's "a mixed result is a stop" binds the reader-rule gates G1, G2, G3 and G4. G6 is a
fixture-family constructibility gate: a family is kernel-made if the probe shows it at least once in a
stated workload; mixed per-file results are recorded, not a stop. For Task 9: build the `nidreuse` and
`reuse` fixtures from the small workload, and take the G2, G3 and G5 expectations from the large one.

### From-memory facts this probe confirmed (kernel 6.1 only)

Footer `cp_ver` (low half = checkpoint version, high half = CRC), `CP_CRC_RECOVERY_FLAG` 0x40,
`CP_COMPACT_SUM_FLAG` 0x4, the checkpoint cursor offsets 36/68/84/116 and main-relative segment
numbers, SSA entry layout (7 bytes) and footer type byte 4091 (`SUM_TYPE_NODE` 1), SIT segment types 4 and 5
(warm and cold node) and the 74-byte entry with MSB-first valid bits, the NAT/SIT version
bitmaps and the NAT/SIT journals in the compact summary, the inode layout (`i_nid` at 4052, footer at 4072, `ofs` = flag >> 3, inode checksum), and the
inline dentry layout. Other kernels and vendor forks are unmeasured.

### Raw evidence (`HEX` lines)

```
HEX[small] Q1 inode A head(0..96)                       a48100210000000000000000010000000050000000000000060000000000000000f153650000000000f153650000000000f1536500000000ff741903ff741903ff741903b0519dd300000000000000000000
HEX[small] Q1 inode A footer                            07000000070000000100000067458b6b0000000001120000
HEX[small] Q2 NAT entry nid=7 copy0 (ver,ino,addr)      000700000000120000
HEX[small] Q2 NAT entry nid=7 copy1 (current=1)         010700000000000000
HEX[large] Q3 SIT entry seg=1 (block), 74 bytes         0010000000000000...
HEX[large] Q4 SSA entry of blk=4908 (7 bytes) + footer type byte 3401000000000001
HEX[small] Q5 footer nid=7 blk=4608                     07000000070000000100000067458b6b0000000001120000
HEX[small] Q6 A inode block 4608 last 24 bytes, before  07000000070000000100000067458b6b0000000001120000
HEX[small] Q6 A inode block 4608 last 24 bytes, after   000000000000000000000000000000000000000000000000
HEX[small] Q6 B data block 17920 first 16 bytes, before 423a3000000000000000000000000000
HEX[small] Q6 B data block 17920 first 16 bytes, after  00000000000000000000000000000000
HEX[small] Q9 inode 17 copy blk=4617 i_namelen+i_name   010000005200000000000000
HEX[small] Q9 inode 17 copy blk=4621 i_namelen+i_name   020000005232000000000000
HEX[small] Q10 NAT entry nid=7 in p4 (ver,ino,addr)     00070000000e120000
HEX[small] Q11 B data block 17920 first 16 bytes, stage D 423a3000000000000000000000000000
HEX[small] Q11 B data block 17920 first 16 bytes, after fill 00000000000000000000000000000000
HEX[small] Q12 dir inode inline area head               000000001b0000000000000000000000000000000000000000000000000000000000000000000000060000000100020000000005000000020002b85bf5fe1200000002000126b02f9b13000000020001d5b1
HEX[small] Q13 H i_addr[0..5]                           061600000000000000000000071600000000000000000000
```

The `Q6 A inode` pair is the discard image (d1, d3): the node block is all zero after the delete;
`Q11 B data` is the nodiscard fill. The whole run's output (guest logs, the `FACT[...]` and `GATE[...]`
lines) is committed as `f2fs-probe/probe-run3.txt`. Provenance: the `HEX` lines for Q2, Q3, Q4, Q6, Q9,
Q10 and Q11 were added to the decoder AFTER the container run and were produced host-side (Python 3.13)
from the retained images of that run, so the committed oracle differs from the one that ran in the
container by those lines; the non-HEX output of the host-side run was diffed identical to the
container's. The `HEX` lines are appended at the end of `probe-run3.txt`. `f2fs-probe/` also holds the
earlier run (`probe-run2-earlier-workload.txt` and `.note`), a different workload (f2/f3 created before
the second batch), cited only for the churned-volume figures of Q10.

## SQLite fixtures

Committed under `internal/sqlitefile/testdata/` (plan 3I, Task 14), consumed by
`internal/sqlitefile/fixtures_test.go`. Each fixture is `<name>.db.gz`, plus
`<name>.db-wal.gz` and/or `<name>.db-journal.gz`, plus `<name>.expect.json`
(the oracle). The tests that read the fixtures need no engine and no Docker (`TestFixtureGeneratorIsDeterministic` and `TestGenerateFixtures` use `modernc.org/sqlite`, a test dependency, to generate and compare). The `expect.json` files are large because they list every cell (up to 1.7 MB for `hot-journal`, 8.6 MB in all); the images are small.

| Fixture | Class | Written by | Holds |
|---|---|---|---|
| `basic-utf8-4k` | A | engine | every value kind, an index, a view, 600 rows |
| `utf16le-1k`, `utf16be-512` | A | engine | non-ASCII text, small pages, multi-level trees |
| `overflow-wide` | A | engine | overflow chains of 1, 3 and 40 pages, a 120-column table |
| `without-rowid` | A | engine | WITHOUT ROWID with a composite key inserted out of order, a STRICT table |
| `addcolumn-short` | A | engine | rows written before `ALTER TABLE ADD COLUMN` (defaults) |
| `autovacuum-incr` | A | engine | incremental auto-vacuum, pointer-map pages, deleted rows, freelist |
| `freelist-dropped` | A | engine | dropped tables whose freelist leaves keep their rows (fit, guess and none tiers), `DELETE` residue |
| `secure-delete` | A | engine | the same with `secure_delete=ON` (negative control: nothing recoverable) |
| `builder-wal-generations` | A | `sqlitetest` | database + WAL: four commits over two generations, an uncommitted tail, stale frames |
| `builder-hot-journal`, `builder-journal-multiseg` | A | `sqlitetest` | database with spilled pages + hot journal; three segments and a page journaled twice |
| `encrypted-like` | A | generator | 8 KiB of sha256 counter-mode bytes (not SQLite) |
| `wal-uncheckpointed`, `wal-stale-generation` | B | engine | WAL with 4 transactions; WAL restarted after `wal_checkpoint(RESTART)` with stale frames beyond the new end |
| `hot-journal`, `persist-journal` | B | engine | database + journal copied in the middle of a transaction; `journal_mode=PERSIST` leftover (zeroed header) |
| `sqlite3-freelist`, `sqlite3-utf16` | C | Debian `sqlite3` (python) | the freelist and UTF-16 shapes from the container's engine |
| `sqlite3-wal-killed`, `sqlite3-hot-journal-killed` | C | Debian `sqlite3` (python) | a child process killed with `os._exit` leaves an uncheckpointed WAL / a hot journal, with no copy of open files |

### Regenerate

Class A and B (Go, no Docker; the engine is `modernc.org/sqlite`, a test
dependency only):

    MINUTIAE_REGEN_SQLITE_FIXTURES=1 go test ./internal/sqlitefile -run TestGenerateFixtures

Class C (Docker; the image's last layer installs the Debian `sqlite3` package,
whose libsqlite3 python's `sqlite3` module links; no `--privileged`):

    docker build -t minutiae-fixtures tools/fixtures
    docker run --rm -v "$PWD:/work" -w /work minutiae-fixtures bash tools/fixtures/gen.sh sqlite

Regenerating one class never touches the other: the Go generator writes only
the A and B names, `gen.sh sqlite` only the four C names. Regenerate a fixture
TOGETHER with its `expect.json`: every test checks the sha256 of each
uncompressed file against `generator.file_sha256` first, so a stale oracle (or
a stale image) fails before anything else.

Engine versions: the class C files are made by the Debian `sqlite3` and
`libsqlite3-0` packages pinned in the Dockerfile to 3.46.1-7+deb13u2 (SQLite
3.46.1; the base image `debian:stable-slim` is trixie), and their expect.json
record `libsqlite3 3.46.1; shell: 3.46.1`. The class A and B oracles use the
engine of `modernc.org/sqlite` (SQLite 3.53.4 at the pinned module version),
recorded in each expect.json. A version bump of either regenerates its class.

The pinned class C layer was built and verified on 2026-10-08: `dpkg-query` shows `sqlite3` and `libsqlite3-0` at 3.46.1-7+deb13u2, `sqlite3 --version` is 3.46.1 and the layer's own version tests passed; no fixture was regenerated.

The Dockerfile change is a NEW FINAL layer, so every earlier layer, and every
fixture generated from it, is as built; no other fixture was regenerated.
Note: Debian builds libsqlite3 with `SQLITE_SECURE_DELETE`, so a database made
there erases freed content unless `pragma secure_delete = OFF` is set; the
`sqlite3-freelist` scenario sets it.

### The oracles (never Minutiae)

`expect.json` is `{generator, header, schema, live, cells, info, freelist,
wal, journal, as_found, history, unreachable, warnings, extra}`; values are
typed (`{"t":"int","v":"7"}`, text, blob as base64, null, float as its bit
pattern in hex). It differs from `lib.sh generator_json` in that `file_sha256`
holds one hash per file (a fixture can have three files), not one image hash.

* The ENGINE answers for the schema (`sqlite_schema`), the live rows (a
  hot-journal fixture's `live` is the post-rollback state the engine returns;
  `as_found` is the raw file plus WAL, never a rollback), `integrity_check`,
  `freelist_count` and `page_count`, always on a COPY of the files. For the
  container family it also records the shell's `.dbinfo` and `dbstat` output
  (in `extra`).
* A small independent WALKER answers for bytes: the header, every cell
  (file, page, offset, length and the cell's bytes in `cells`), WAL frames
  (salts, checksum chain, state, generation), journal records and the
  freelist. It lives in `internal/sqlitefile/fixturewalk_test.go` (Go: class A
  and B) and `sqlite_oracle.py` (class C); the two share no code with
  `internal/sqlitefile` or with each other, and the class C files agree with
  the Go tests, which is a third reading.
* `history` is what the generator's own statements prove reachable: each
  version a statement wrote (or deleted) is searched for in the non-live page
  images (database pages under a committed frame, superseded, uncommitted and
  stale frames, journal before-images, database pages a hot journal hides,
  freelist leaves). Each place it is found is an entry, labelled by the rules
  of the plan: method, origin, table basis (schema; fit or guess with the
  identity-by-fit-only note and relation unknown; none), relation to live and
  confidence. A row identical to its live row is not an entry. The rows of a
  stale WAL generation have no as-of schema, so they carry the fit basis and
  `owner-changed` (rulings C44 and C46). `unreachable` lists deleted rows and
  `secure_delete` markers the library must not deliver.

### Determinism classes

* **A, byte-reproducible:** builder-written files (database, WAL, journal)
  and engine-written database-only files (the header holds no random data;
  the engine version is pinned by `go.mod`). `TestFixtureGeneratorIsDeterministic`
  runs the Go generator twice in one process and compares every byte.
* **B, not reproducible:** engine-written WAL and journal files. The engine
  draws salts and nonces from its PRNG and exposes no seed. Committed with
  their oracle; regenerate image and oracle together. The determinism test
  excludes exactly these four by name.
* **C, container family:** not guaranteed byte-reproducible. `sqlite.sh`
  generates twice and says which files differ; in the runs made the
  database-only fixtures and the databases next to a killed writer were
  identical and the WAL and journal files differed (class B behaviour), but
  the script tolerates a difference in the killed-writer family. Regenerate
  image and oracle together.

### Tags

`go test -tags sqlitematrix ./internal/sqlitefile` runs the large engine
matrix of Task 13 (45 live scenarios, 1448 WAL mutations); the fixtures do not
need it. One fuzz corpus seed is committed (`internal/sqlitefile/testdata/fuzz/FuzzCreateParse/d65cbe711e2fe660`, a CREATE INDEX statement found by fuzzing and kept as a regression input); the fuzz targets otherwise run only their in-code seeds.

## Casefold lookup oracle

`gen.sh casefold` (not part of `all`) asks a real Linux kernel which names a casefold directory
treats as the same name. A QEMU guest (TCG, no KVM, the 6.1 kernel of `minutiae-recover-fixtures`)
mounts an ext4 and an F2FS image whose directories are casefold, stores names in them and, for each
probe, answers "does this query resolve to that stored name". The answers are the independent oracle
for the `utf8fold` package (LX-7a Task 3): they come from the kernel's lookups, never from Minutiae.
Files: `casefold.sh` (host driver), `casefold_guest.sh` (self-contained guest library; it does not
source the recovery fixtures' guest library), `casefold_probes.py` (probe list; parses the Unicode
12.1.0 files itself and shares nothing with the Go generator).

Three outputs, committed together because the images are not byte-reproducible (see "QEMU images are
not byte-reproducible"):

- `internal/filesys/utf8fold/testdata/kernel-vectors.json.gz` (gzip, level 9, mtime 0, written by the Python gzip module; only the decompressed content is pinned) and
  `kernel-vectors.meta.json`: the probes and the verdicts (every measured row is kept), with
  `generator.image_sha256` pinning both images and `generator.fsck_exit` recording the checkers' exit
  statuses. The meta file pins the SHA-256 of the DECOMPRESSED JSON and the row count; the tests
  decompress the file and compare;
- `internal/filesys/ext4/testdata/ext4-casefold.img.gz` and `.expect.json`;
- `internal/filesys/f2fs/testdata/f2fs-casefold.img.gz` and `.expect.json`. The directory lists are read
  from the finished image by `debugfs ls -p` and `fsck.f2fs -t` / `dump.f2fs -i`, never by Minutiae. Each
  directory records `count` and `names_sha256` (SHA-256 over the sorted, de-duplicated lower-case hex
  names, each followed by a newline); the full `names_hex` list is kept only for `/plain` and `/ambig`.
  `TestExpectJSONWellFormed` derives each bucket's expected count and digest from the vectors' stored
  names, so the listing of the image and the vectors must agree.

The two images stay out of `TestExt4MatchesOracle` and `TestF2FSMatchesOracle` (their `fixturePaths`
skip `*-casefold.img.gz`): the casefold `.expect.json` has directory name lists only.

Size exception: the oracle commits about 2.0 MB in all (two images of 690 KB and 585 KB gzipped, the
vectors 121 KB gzipped, two expect files of about 90 KB, 450 KB of Unicode data), above the "few hundred
KiB" guideline of "Adding a fixture". The images are what the kernel wrote (they carry ~6000 inodes and
cannot be thinned without changing what the kernel produced), and the vectors keep every measured row
because Task 3's tables are checked against all of them. The vectors were 2.2 MB uncompressed before
they were gzipped.

A run that timed out, stopped early, or whose checker reported a problem is a FAILED run: the generator
exits non-zero (the guest log must end with `GUEST-DONE`, every probe needs a verdict, `fsck.ext4 -fn` and
`fsck.f2fs -f --dry-run` must exit 0) and nothing from it may be committed.

### Regenerate

```bash
docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures
MSYS_NO_PATHCONV=1 docker run --rm --shm-size=512m -v "C:/GIT/Minutiae:/work" -v "<scratch>:/scratch" -w /work \
  -e CASEFOLD_WORK=/scratch/work minutiae-recover-fixtures bash tools/fixtures/gen.sh casefold
```

No `--privileged`: every `mount`, module load and attribute change happens inside the guest (the host
segments of the scripts contain none of those words; the tripwire in `internal/archtest` checks it). The
images (a few tens of MiB each) live in `CASEFOLD_WORK`, a bind mount of a scratch folder, never in the
container's filesystem. The payload and the initramfs live on tmpfs (`--shm-size`): a Windows bind mount
is case-insensitive and would merge names that differ only in case. The guest run took 170-271 s of
wall time when the machine was idle and 630 s once under load (`timeout 1500` in `casefold_run`; a timeout
is a failed run).

### Probe protocol

`casefold_probes.py --payload` writes each probe as FILE NAMES (no name is escaped, hex-decoded or
`read` in the guest): `p/<bucket>/<id>/q/<query bytes>` and `.../s/<stored bytes>`, plus
`s/<bucket>/<stored bytes>`. One guest boot, two virtio drives (`vda` ext4, `vdb` F2FS), mount points
per filesystem (`/mnt/ext4-cf`, `/mnt/ext4-plain`, `/mnt/f2fs-cf`, `/mnt/f2fs-plain`; the plain ones are
bind mounts of the `/plain` directory). The guest copies the stored names into `/cf/<bucket>` and the
first bucket's names into `/plain`, then for each probe prints `V <id> abcd` with `a`/`c` = the stored
name exists and `b`/`d` = `[ query -ef stored ]` (ext4, F2FS). The host aborts unless every stored name
was present (`a` and `c` are 1; a refused stored name would record a false negative) and unless
`debugfs ls -p` / `fsck.f2fs -t` list every stored spelling byte for byte in its bucket directory.

Buckets: a bucket's stored names are pairwise distinct under the script's own fold, because a casefold
directory keeps one entry for two names the kernel calls equal. A stored name that is not valid UTF-8
gets a bucket of its own: the first run showed why (the kernel resolved `e` U+0301 0x80 to the entry
already stored for U+00C9 0x80, so the second spelling was never created). 222 bucket directories.
The probe list has one row per (stored, query) pair: the first category to produce a pair keeps it.

Probe categories (15988 probes): `casefold` (every `C`/`F` folding of `CaseFolding.txt`), `decomposition`
(every canonical decomposition), `ignorable` (every `Default_Ignorable_Code_Point`: stored `ab`, query
`a`+cp+`b`), `unmapped` (2000 assigned code points across all planes, identity, `x`/`X` and `x`/`y`
controls), `reorder` (combining-mark reordering, runs of 2 to 40 marks, Hangul syllables versus jamo),
`special` (U+0130, U+0131, U+1E9E, U+00DF, U+03C2/U+03C3/U+03A3, U+212A, U+2126, U+FB03, ...),
`post-12.1` (code points assigned after Unicode 12.1: U+A7C7/U+A7C8, U+A7C9/U+A7CA, U+A7F5/U+A7F6 from
13.0, U+A7C0/U+A7C1 and U+061D from 14.0, U+0CF3 from 15.0; the script asserts none is assigned in the
12.1.0 `UnicodeData.txt`) and `invalid` (611 rows: all 64 lone continuation bytes 0x80-0xBF after `x`,
each against its upper-case twin, against the bare prefix and against `x\x80`; every lead byte
0xC0-0xFF truncated at the end of a name; 0x80 at the start, between letters, doubled, after a
multi-byte character and after combining marks; overlong forms, a UTF-16 surrogate in UTF-8,
U+FFFE/U+FFFF, U+10FFFF, bytes beyond U+10FFFF). Every category also carries negative controls (the stored name with a trailing `z`, expected 0).
A name containing a newline, a slash or a NUL cannot be a file name here and is never generated.

### Routes and measurements (kernel `6.1.0-53-amd64`, Debian `6.1.187-1`, `Dockerfile.recover` pins)

- Kernel: `CONFIG_UNICODE=y` (built in; `unicode.ko`/`utf8data.ko` appear in `modules.builtin`),
  `CONFIG_EXT4_FS=m`, `CONFIG_F2FS_FS=m`, so no `Dockerfile.casefold` exists. Both mounts succeed;
  `/proc/filesystems` lists both; `crc32c-intel` and `crc32-pclmul` fail to insert under TCG (`rc=19`) and
  are not needed.
- ext4: `mke2fs -t ext4 -O casefold -E encoding=utf8` on the host under the fake clock; `dumpe2fs -h`
  prints `Character encoding: utf8-12.1`. The flag route is host-side `debugfs -w -R "sif /cf/NN flags
  <old|0x40000000>"` on empty directories (`Flags: 0x80000` becomes `0x40080000`); the kernel shows `F`
  in `lsattr`.
- F2FS: `mkfs.f2fs -O casefold -C utf8`; `dump.f2fs -d 1` prints only `superblock features = 1000 :
  casefold` (no encoding line). Route (a) does NOT hold: the root inode's `i_flags` is 0 after mkfs, and a
  directory made in the guest is not casefold. Route (b) works and is the one used: `chattr +F` (the
  e2fsprogs binary and its libraries copied into the initramfs) on each empty directory inside the guest;
  `i_flags` of a bucket directory is `0x40001000`. `/plain` is created the same way and has `i_flags` 0.
  Route (c) was not needed.
- F2FS superblock, by diffing `mkfs.f2fs` images with and without `-O casefold -C utf8` (with
  `-C utf8:strict` as a third): the feature word at superblock offset 2180 gains `0x1000`; `s_encoding`
  is a u16 at superblock byte offset 2758 (1 = utf8-12.1), `s_encoding_flags` a u16 at 2760 (bit 0 is
  strict). Absolute image offsets 3782 and 3784.
- Shared xattr blocks (ruling Z17): the image has no `setfattr`/`getfattr` and busybox has no `*fattr`
  applet, so this guest cannot create them (it would need the `attr` package or a static helper); they stay
  builder-only. Not attempted.
- ext4 and F2FS gave the same verdict on every one of the 15988 probes (`exceptions` is absent).
- Checker exit statuses of the committed images (recorded in `generator.fsck_exit`): `fsck.ext4 -fn` 0 and
  `fsck.f2fs -f --dry-run` 0. The gate was shown to bite on damaged copies: `fsck.ext4 -fn` exits 4 when the
  group descriptors are zeroed, `fsck.f2fs` exits 255 with both superblocks zeroed and 1 with 600 main-area
  blocks zeroed (the earlier `| tail || true` form would have recorded 0 for all of them). The F2FS gate
  has a known blind spot: `fsck.f2fs` exited 0 when the checkpoint area was zeroed, so the gate does not
  detect a zeroed checkpoint area.
- A small F2FS image has little user space: 64 MiB leaves 16 MiB and `-o 5` reserves 48 segments; the
  F2FS image is 128 MiB with the default overprovisioning (about 6000 inodes need ~24 MiB).
- `e2fsck -fn` was clean on the final ext4 image. An earlier run, with a different bucket layout, got
  `block #84 has bad max hash` for the 5000-name htree directory `/cf/00`: the image was kept as the
  kernel wrote it, never repaired.
- Fold-equal pair: the KERNEL refuses the second name. In a fresh casefold directory the guest created
  `Straße` and then `STRASSE` with `mkdir` on both filesystems: the second `mkdir` failed with status 1 and the
  directory held 1 entry (ext4 and F2FS). The pair in the ext4 image is therefore a HOST-TOOL state:
  after the guest unmounted, `debugfs write` (libext2fs 1.47.0, which does not refuse it) created
  `/ambig/Straße` and `/ambig/STRASSE` and `e2fsck -fn` accepts them. `ext4-casefold.expect.json` carries it in
  `ambiguous_pair` with `"origin": "host-tool (libext2fs debugfs), not kernel-written"` and the measured
  kernel behaviour in `kernel_behavior`. F2FS has no host-side writer, so its `ambiguous_pair` is `null`.

Verdicts the Go code must copy (the vectors are the authority; each is a measurement, not an assumption):

- every `Default_Ignorable_Code_Point` is ignored, including the unassigned ones (U+00AD, U+034F,
  U+061C, U+115F-U+1160, U+17B4-U+17B5, U+180B-U+180E, U+200B-U+200F, U+202A-U+202E, U+2060-U+206F,
  U+3164, U+FE00-U+FE0F, U+FEFF, U+FFA0, U+FFF0-U+FFF8, U+1BCA0-U+1BCA3, U+1D173-U+1D17A, U+E0000-U+E0FFF);
- every `C`/`F` folding and every canonical decomposition of 12.1.0 resolves; Hangul syllables equal their
  jamo; combining marks are reordered (runs of up to 40 marks included);
- no Turkic mapping (U+0131 is not `I`), full folding (U+00DF equals `ss`), U+212A equals `k`;
- a code point assigned after 12.1 has NO case mapping (U+A7C7 is not U+A7C8); it still takes part in
  mark reordering;
- invalid UTF-8, measured by 611 `invalid` rows (ext4 and F2FS agree on all of them):
  - a STRAY CONTINUATION BYTE (0x80-0xBF where a character should start) that comes after at least one
    byte of the name is an ordinary character that folds to itself: `x`+b equals `X`+b for all 64 bytes, the
    text before and after it is case-folded and normalized (`é`+0x80 equals `É`+0x80, `a`+U+0323+0x80 equals
    `A`+U+0323+0x80, `a`+0x80+`b` equals `A`+0x80+`B`, doubled stray bytes too), but it is never ignored
    (`x`+b differs from `x`) and different bytes differ (`x\x80` equals `x`+b only for b = 0x80);
  - the stray byte also behaves as text when the valid text after it is non-ASCII or a combining mark
    (`a\x80é` equals `A\x80É`, `a\x80e`+U+0301 equals `a\x80é`, `a\x80`+U+0323+U+0301 equals
    `a\x80`+U+0301+U+0323, a CJK or an emoji after it) and when several stray bytes have valid text between
    them (`a\x80b\x81c` equals `A\x80B\x81C` but differs from `a\x81b\x80c`; also `a\x80b\x80c`, with a
    non-ASCII or a combining mark between);
  - a stray continuation byte at the START of a name makes the whole name opaque: `\x80` equals itself,
    but `\x80x` differs from `\x80X`, and `\x80x` from `\x81x`; likewise `\x80\x80x` (doubled), `\x80é`,
    `\x80e`+U+0301 against `\x80é` and `\x81é` against `\x81É` are all different (opaque), while a
    CJK or identical name equals itself;
  - a stray byte mixed with another invalid form makes the whole name opaque: `x\x80\xff`, `x\x80\xc1\x81`,
    `x\x80\xe2\x82`, `x\x80`+ED A0 80, `x\x80`+F4 90 80 80 and `é\x80\xff` differ from their case twins;
    a stray byte AFTER an invalid lead (`x\xff\x80`, `x\xfe\x80`, `x\xc0\x80`, `x\xe2\x80`, `x\xf8\x80`,
    `é\xff\x80`) is likewise opaque, and `x\xff\x80` differs from `x\xff\x81`;
  - every other invalid form makes the whole name opaque (compared byte for byte, no case folding, no
    normalization): all 64 lead bytes 0xC0-0xFF truncated at the end of a name (`x`+lead differs from
    `X`+lead and from `x`), overlong forms (C0 AF, C1 81), a truncated sequence (E2 82), a UTF-16 surrogate
    (ED A0 80), code points above U+10FFFF (F4 90 80 80, F8 88 80 80 80), FF, FE FE;
  - an overlong form does not resolve to its character (`xA` differs from `x` C1 81), U+FFFD does not equal
    a bad byte;
  - U+FFFE, U+FFFF and U+10FFFF are ordinary valid names (folded and normalized like any other).
  The raw guest logs of each run are kept locally by whoever generated them (they are not committed); the
  committed vectors are the authority.

### Pins and licence

No new image: `minutiae-recover-fixtures` (`Dockerfile.recover`: Debian bookworm snapshot 20261003,
`linux-image-6.1.0-53-amd64 6.1.187-1`, `qemu-system-x86 1:7.2+dfsg-7+deb12u18+b3`, `busybox-static
1:1.35.0-4+deb12u1+b1`, `e2fsprogs 1.47.0-2+b2`, `f2fs-tools 1.15.0-1`). The Unicode 12.1.0 files
(`UnicodeData.txt`, `CaseFolding.txt`, `DerivedCoreProperties.txt`) are committed gzipped in
`internal/filesys/utf8fold/testdata/ucd-12.1.0/` with the Unicode licence (`LICENSE-unicode.txt`) and
`SOURCES` (URLs and SHA-256 of each file).

## Adding a fixture

1. Add `<name>.sh` taking the output directory as `$1`; source `lib.sh` and
   wrap commands whose invocation belongs in the oracle with `run`.
2. Register it in `gen.sh`.
3. Keep images small: a few hundred KiB compressed at most. The raw size is set by
   the filesystem (the smallest FAT32 image is 34 MiB, see above; every other fixture
   is at most 16 MiB raw).

## DDL provenance (Android telephony, plan 4D)

The eight snapshots in `tools/fixtures/schemas/android-telephony/*.sql` are cross-checked by
`tools/fixtures/ddl_crosscheck.py` against the public provider sources at AOSP tag
`android-14.0.0_r1` (the nearest `android-14.0.0_r*` tag present in every repository; the
script is run with `py -3 -I <sourcedir> <schemasdir> --write-report ...`). Commits:
`platform/packages/providers/TelephonyProvider` a1e5ac7164cbb4556226279de386518c5814c115,
`platform/packages/providers/ContactsProvider` 224cfeaf5f85106b1ca7d323403d6764b78920f0,
`platform/frameworks/base` 299fe6f5d6fc6f1af7c3411dcf4e5efdf7217368,
`platform/packages/apps/Messaging` d921817eec79a8ca2354254d9e0771a8740c5867 (only `PduHeaders.java`, see below).
The fetched files (untrusted text, kept outside the repository, never executed) and their SHA-256 are listed in
`schemas/android-telephony/CROSSCHECK.md`, which also holds one verdict row per snapshot table:

- `verified`: the snapshot is the CREATE TABLE the tag builds (`mmssms-aosp-baseline`, `calllog-features-subid`,
  `contacts-aosp-baseline`, `contacts-deleted-contacts`; generated with `--emit`, compared column by column on name,
  order and whole definition text). These replaced the earlier from-memory text, which lacked columns the tag creates
  (for example `sub_id`, `creator`, `text_only`, `error_code DEFAULT -1`, `raw_contacts.account_id` instead of
  `account_name`/`account_type`, `data.hash_id`, 36 `raw_contacts` and 54 `calls` columns) and so did not match it.
  `data15` is declared TEXT by the source although it holds BLOB values.
- `historical`: older or reduced variants (`mmssms-aosp-subid-alter`, `mmssms-oem-extended`,
  `calllog-pre-presentation`, `calllog-in-contacts2`) that only use columns a CREATE TABLE of the same sources defines
  or that the upgrade code adds with ALTER TABLE ADD COLUMN (each with its file:line in CROSSCHECK.md). The three
  `mmssms-oem-extended` vendor columns are invented on purpose. `ContactsDatabaseHelper.java` no longer creates a
  `calls` table at this tag (line 2687 says it was extracted), so `calllog-in-contacts2` is the layout of older
  releases built from the call log columns, not a definition of the tag.
  A historical snapshot is checked on column names only: the historical definitions (column affinity, the
  `DEFAULT` values such as `error_code DEFAULT 0`, `AUTOINCREMENT` on the key) are not compared with the tag's
  text, which differs from them.
- `no-public-source`: none.

Constants, checked by reading the fetched files (file:line at the tag): `Telephony.java` `Sms.MESSAGE_TYPE_*` lines
139-157 (0 all, 1 inbox, 2 sent, 3 draft, 4 outbox, 5 failed, 6 queued); `Telephony.java` `BaseMmsColumns.MESSAGE_BOX_*`
lines 1477-1487 (0 all, 1 inbox, 2 sent, 3 drafts, 4 outbox, 5 failed); `CallLog.java` `Calls.*_TYPE` lines 862-878
(1 incoming, 2 outgoing, 3 missed, 4 voicemail, 5 rejected, 6 blocked, 7 answered externally), `PRESENTATION_*` lines
936-944 (1 allowed, 2 restricted, 3 unknown, 4 payphone, 5 unavailable), `FEATURES_VIDEO` line 888 (1 << 0);
`ContactsContract.java` `CommonDataKinds.Im.PROTOCOL_*` lines 6940-6985 (-1 custom, 0 AIM, 1 MSN, 2 Yahoo, 3 Skype,
4 QQ, 5 Google Talk, 6 ICQ, 7 Jabber, 8 NetMeeting); `Phone.TYPE_*` lines 6332-6351 (1 home, 2 mobile, 3 work,
4 fax work, 5 fax home, 6 pager, 7 other, 8 callback, 9 car, 10 company main, 11 ISDN, 12 main, 13 other fax,
14 radio, 15 telex, 16 TTY/TDD, 17 work mobile, 18 work pager, 19 assistant, 20 MMS); `Email.TYPE_*` lines 6606-6609
(1 home, 2 work, 3 other, 4 mobile); `StructuredPostal.TYPE_*` lines 6757-6759 (1 home, 2 work, 3 other);
`Website.TYPE_*` lines 7667-7673 (1 homepage, 2 blog, 3 profile, 4 home, 5 work, 6 FTP, 7 other).
The MMS address types come from `PduHeaders.java`, which is not in `frameworks/opt/mms` at this tag (no such
repository) nor in `frameworks/opt/telephony`; the copy in `packages/apps/Messaging`
(`src/com/android/messaging/mmslib/pdu/PduHeaders.java`, same tag) gives FROM 0x89 = 137 (line 38), TO 0x97 = 151
(line 54), CC 0x82 = 130 (line 31) and BCC 0x81 = 129 (line 30). The plan brief had CC and BCC swapped (written from
memory); the public source wins (ruling T42), and the mapper and the oracle of the owning task follow it.

## DDL provenance (Chromium history, plan 4F)

The four snapshots in `tools/fixtures/schemas/chromium/*.sql` are cross-checked by
`tools/fixtures/ddl_crosscheck_chromium.py` against the public Chromium sources at release tag
`154.0.8037.100` (the newest Stable release at least 14 days old on 2026-10-24, picked from the Chromium release
dashboard; it resolves to chromium/src commit `81ab43db91f352ce9134362efe32529b9761c4eb`). The fetched files (untrusted
text, kept outside the repository, never executed or imported; the script tokenises them as text) and their SHA-256 are
listed in `schemas/chromium/CROSSCHECK.md`. To re-run, fetch each listed path with
`curl -fsS "https://chromium.googlesource.com/chromium/src/+/refs/tags/154.0.8037.100/<path>?format=TEXT" | base64 -d > <basename>`
into one empty directory, then:

```
py -3 -I tools/fixtures/ddl_crosscheck_chromium.py <srcdir> tools/fixtures/schemas/chromium \
  --tag 154.0.8037.100 --commit 81ab43db91f352ce9134362efe32529b9761c4eb --taken 2026-10-24 \
  --subst name=urls --rename visits_tmp=visits --write-report tools/fixtures/schemas/chromium/CROSSCHECK.md
```

(`--emit TABLE[:LINE]` prints a table's one-line CREATE from the sources; `--subst` gives the temporary-table name
parameter of `urls`; the report keeps its hand-written notes from the `<!-- notes -->` marker on.) Snapshots are named
by role, and each header carries `-- verdict`, `-- source chromium <tag> <file>:<line>, ...` and `-- meta.version`:

- `chromium-current.sql`: `verified`, version 70 (`kCurrentVersionNumber`): `urls`, `visits`, `keyword_search_terms`,
  `downloads`, `downloads_url_chains`, `meta` and `visit_source` (never read by the parsers), each equal to one CREATE
  TABLE statement of the sources.
- `chromium-minimal.sql`: `verified`, version 70: the current `urls`, `visits`, `meta` only.
- `chromium-mid.sql`: `historical`, `-- @phase 1` = the label for version 48 (the `visits` table as the migration builds it before
  adding `opener_visit`), `-- @phase 2` = the ALTER that migration runs (version 49). Statements are source text of
  migration code under a temporary table name; phase 1 combines the `visits_tmp` shape with the tag's current downloads
  text and matches no real file (never a real version 48 database); deviations are in the notes of CROSSCHECK.md.
- `chromium-old.sql`: `historical`, `-- @phase 1` alone, without the downloads tables (a synthetic omission, stated in
  the notes).

`verified` means the statement equals the tag's source text after whitespace collapse; `historical` means it equals
migration-code text and describes an older shape. The Constants table of CROSSCHECK.md (header
`| enum | int | name | source |`) is written by the script from the enums and macros: the page transition core values
and qualifier masks, the history database integers of download state, danger type and interrupt reason (the history
database stores plain casts of its own enums, which differ from the in-memory danger enum at 18, 24 and 25), the
history version numbers and the migration that adds `visits.opener_visit` (48 to 49).

## Browser fixtures (plan 4F)

`tools/fixtures/browser_chromium.py` is a family module of `telephony_gen.py` (same driver, same shared helpers, host
Python `sqlite3` only, no Docker):

```
py -3 tools/fixtures/telephony_gen.py --family chromium --out internal/parsers/chromium/testdata \
    --pins-go internal/parsers/chromium/pins_test.go --pins-package chromium_test
```

The oracle restates the plan's Chromium mapping rules in Python, takes the transition, download state, danger and
interrupt names from the Constants table of `schemas/chromium/CROSSCHECK.md` at generation time, and reads every stored
value back from the engine on a copy of the finished files. Databases are byte-reproducible; the `-wal` of
`current-wal` holds salts that differ on every run. The synthetic scenario rows are literal in the module.

| Variant (`internal/parsers/chromium/testdata/`) | DDL snapshot | Remote path (browser, profile) | Holds |
|---|---|---|---|
| `current` | `chromium-current.sql` | `/data/data/com.android.chrome/app_chrome/Default/History` (chrome, Default) | 11 `urls` (visitless, sentinel time, 100 KiB data URL, invalid UTF-8 and NUL, empty and missing titles, bidi and ZWJ, empty url, wrong storage class, `hidden` 2), 22 `visits` (typed with qualifiers, negative server-redirect transition, invalid and sentinel times, unknown core, out-of-range transition, dangling and empty-url visits, synced, durations, TEXT time, opener, external referrer, unknown qualifier bits), 5 `keyword_search_terms`, 8 `downloads` with `downloads_url_chains` (out-of-order, gaps, no chain, no target and no chain, unknown state and danger, obsolete interrupt 32, 5000-byte path) and `meta` |
| `current-wal` | `chromium-current.sql` | as `current` | the `current` rows, then a killed writer commits 3 visits and 1 download that exist only in the `-wal` and edits the title of URL 1 there |
| `mid-upgraded` | `chromium-mid.sql` | `/data/data/com.microsoft.emmx/app_chrome/Default/History` (edge, Default) | rows before and after the phase 2 `ALTER` (`opener_visit` absent on the old visits); phase 1 matches no real file and the variant is synthetic data, not a real version 48 or 49 database |
| `old` | `chromium-old.sql` | `/data/user/10/com.brave.browser/app_chrome/Profile 1/History` (brave, Profile 1) | synthetic: no `downloads` tables |
| `minimal` | `chromium-minimal.sql` | `/data/data/com.example.reader/app_webview/Default/History` (webview, Default) | `urls`, `visits`, `meta` only |

Facts of the scenario: `downloads_url_chains` has `PRIMARY KEY (id, chain_index)`, so no conforming file holds a repeated
chain index (the oracle states `duplicate-index` for a hostile file; Go tests build one in memory). The oracle chooses
(and the parser of Tasks 3 and 4 follows) the raw vocabulary and the schema-note column order listed in the module
docstring.

## Safari fixtures (plan 4F)

`tools/fixtures/browser_safari.py` is a family module of `telephony_gen.py` (host Python `sqlite3` only, no Docker).
There is no public source for Safari's `History.db`: `schemas/safari/safari-current.sql` and
`safari-no-tombstones.sql` are written from memory (`-- verdict no-public-source`, `-- source memory`), and
`schemas/safari/CROSSCHECK.md` says so per table. The fixtures prove that the parser reads the remembered layout, not
that a real database has it (`docs/limitations.md`).

```
py -3 tools/fixtures/telephony_gen.py --family safari --out internal/parsers/safari/testdata \
    --pins-go internal/parsers/safari/pins_test.go --pins-package safari_test
```

The oracle states the scenario rules in Python and reads every stored value back from the engine on a copy of the
finished files. A Safari fixture is bundled as an iOS artifact (`parsertest.BundleIOS`) at
`/private/var/mobile/Library/Safari/History.db` (logical `ios:HomeDomain/Library/Safari/History.db`). Databases are
byte-reproducible except the `-wal` of `current-wal`, whose salts differ on every run: a WAL pin never regenerates
byte-for-byte, so a regeneration rewrites the pins with the files.

| Variant (`internal/parsers/safari/testdata/`) | DDL snapshot | Holds |
|---|---|---|
| `current` | `safari-current.sql` | all four tables; 6 items, 17 visits (sentinel and out-of-range times, the year 2100, nanosecond rounding, a dangling visit, wrong storage classes), 5 tombstones, a 100 KiB data URL |
| `current-wal` | `safari-current.sql` | the `current` rows, then three visits and one tombstone that exist only in the `-wal` |
| `no-tombstones` | `safari-no-tombstones.sql` | the older shape without `history_tombstones` (the schema note `table.history_tombstones = absent`) |

The unmapped `history_items` columns (`domain_expansion`, `daily_visit_counts`, `weekly_visit_counts`,
`autocomplete_triggers`, `should_recompute_derived_visit_counts`, `visit_count_score`) are stated as the schema note
`unmapped_columns.history_items`. Both oracles (Chromium and Safari) state a `summary` only for a plain ASCII text of
at most 80 characters; the other summaries are pinned against the shared summarizer by a Go unit test.

## Telephony fixtures (plan 4D)

`tools/fixtures/telephony_gen.py` builds the Android telephony databases with the host Python's bundled `sqlite3`
(Python 3 only: no Docker, no kernel, no mounts; `py -3` on Windows, `python3` on Linux and macOS):

```
py -3 tools/fixtures/telephony_gen.py --family mmssms --out internal/parsers/androidmms/testdata \
    --pins-go internal/parsers/androidmms/pins_test.go --pins-package androidmms_test
py -3 tools/fixtures/telephony_gen.py --family mmssms --out <dir> --variant utf16 hot-journal   # some variants
py -3 tools/fixtures/telephony_gen.py --family mmssms --check                                  # generate, discard
py -3 tools/fixtures/telephony_gen.py --family calllog --out internal/parsers/androidcalllog/testdata \n    --pins-go internal/parsers/androidcalllog/pins_test.go --pins-package androidcalllog_test
```

`--pins-go` writes the complete, already formatted `fixturePins` table (the SHA-256 of every decompressed file) and needs
every variant. `telephony_common.py` holds the shared helpers, `telephony_mmssms.py` the scenario rows and the oracle.

The oracle is the scenario oracle of spec 10.3: Python restates the mapping rules and reads the stored values back
from the engine on a COPY of the finished files (a WAL or hot journal is applied to the copy, never to the shipped
bytes), so a record is expected from what the engine says is in the database, not from what the generator meant to
write. It shares no code with the Go parsers. `expect.json` names the engine (`generator.sqlite`) and the SHA-256 of
each file; the database header differs between SQLite versions, so a fixture is regenerated together with its oracle
and its pin. Databases are byte-reproducible; a WAL or journal holds salts drawn from the engine's PRNG and differs on
every run. MMS address types follow the public `PduHeaders.java` (137 from, 151 to, 129 bcc, 130 cc; ruling T42); the
fixture rows keep the raw type codes.

| Variant | DDL snapshot | Database state | What it exercises |
|---|---|---|---|
| `aosp-baseline` | `mmssms-aosp-baseline.sql` | plain database | 22 `sms` and 6 `pdu` rows: every `type`, sentinel, seconds-in-ms and year-2100 dates, NUL and invalid UTF-8, 300 KiB body, address kinds, missing thread and recipient rows, MMS parts, attachments (one present, others unresolved), roles `from,to,to,cc,bcc,member` |
| `aosp-subid-alter` | `mmssms-aosp-subid-alter.sql` | rows written before and after `ALTER TABLE ADD COLUMN` (`@phase 1`) | `sub_id`, `creator`, `text_only` appear mid-history |
| `oem-extended` | `mmssms-oem-extended.sql` | plain | three invented vendor columns: `unmapped_columns.sms` |
| `utf16` | `mmssms-aosp-baseline.sql` | page size 1024, UTF-16LE text | the text decoding path |
| `av-freelist-512` | `mmssms-aosp-baseline.sql` | page size 512, `auto_vacuum=2`, 151 rows deleted | freelist pages |
| `wal-uncheckpointed` | `mmssms-aosp-baseline.sql` | WAL left by a killed writer (`os._exit`) | rows only in the WAL; an edited row (`file: wal`) |
| `hot-journal` | `mmssms-aosp-baseline.sql` | rollback journal left by a killed writer | the `journal-hot` reader warning; the oracle shows the rolled-back rows |

`telephony_calllog.py` holds the call log scenario rows and oracle. Its three variants are lone databases (no WAL, no
journal), so the oracle states the db path as `source_path` of every row (rulings T48, T51): `in-contacts2` states the
`contacts2.db` path, the other two the `calllog.db` path.

| Variant | DDL snapshot | Database state | What it exercises |
|---|---|---|---|
| `features-subid` | `calllog-features-subid.sql` | plain, 31 `calls` rows | every `type` 1 to 7 and 99, outgoing calls with duration 0, NULL and negative, hidden numbers (`presentation` 2, 3, 4), sentinel and year-2100 dates, a 4e9 s duration, `features` video, an ICCID-length and a decimal `subscription_id`, invalid UTF-8 in `number` and `name`, a BLOB `subscription_id`, a duration of 2^53+1 (not exactly a float64), and `subscription_id` text that Python `int()` accepts but `ParseInt` refuses (a non-ASCII digit, an underscore, 34 bytes) and a negative one; every refused text is flagged `unknown_columns.subscription_id = "unparseable"` (T66) |
| `pre-presentation` | `calllog-pre-presentation.sql` | plain, 16 rows | a table without `presentation`, `features`, `is_read`, `countryiso`, `geocoded_location` and `subscription_id` |
| `in-contacts2` | `calllog-in-contacts2.sql` | plain, 9 rows, path `contacts2.db` | the call log inside the older contacts database |

`telephony_contacts.py` holds the contacts provider scenario rows and oracle (`py -3 tools/fixtures/telephony_gen.py --family contacts
--out internal/parsers/androidcontacts/testdata --pins-go internal/parsers/androidcontacts/pins_test.go --pins-package androidcontacts_test`).
Its six variants are lone databases (the sixth, `duplicate-keys`, carries an empty `-wal`, so the oracle states no path for its
rows and the comparer derives it from the file role), so the oracle states the `contacts2.db` path as `source_path` of every row
(rulings T48, T51). The oracle also pins the summary (display name and first phone through the summarizer's rules, a bidi or
format character shown as `<U+XXXX>`; a name it does not restate is left unpinned, T81). The oracle restates the `contact` mapping of spec 9.3 (one record per `raw_contacts` row, `data` rows typed through
`mimetypes`, every `dataN` read as text, `deleted_contacts` tombstones) and reads every stored value back from the engine. The
snapshot declares `COLLATE PHONEBOOK`, which the platform registers; the generator registers a stand-in collation (no index uses it).
The pinned tag's `raw_contacts` has no `account_name`, `account_type` or time column, so the two contacts snapshots carry the
tag's `accounts` and `contacts` tables too (verbatim, CROSSCHECK.md; ruling T68) and the oracle follows the joins (T69): the
account comes only from `accounts` through `raw_contacts.account_id` (inline `account_name`/`account_type` count only on a
schema that has them); a NULL `account_id` is no account, an `account_id` with no `accounts` row is stated
(`raw.account_state = "no-accounts-row"`), never "no account". The time is the AGGREGATE contact's
`contacts.contact_last_updated_timestamp` through `raw.contact_id`: it is stated as `times.aggregate_last_updated` with
`raw.contact_last_updated_source`, never as `Record.Time` (it is not the raw contact's own modification time); a NULL
`contact_id` is `raw.aggregate_state = "no-contact-id"`, a dangling one `"no-contacts-row"`, never zero (T72). A join that cannot be
decided (a missing `accounts` or `contacts` table, or unreadable pages) puts the table name in `raw.join_undecided`; a fixture cannot
force that, so the parser's own tests build such databases in memory (as they do for a joined cell of the wrong storage class,
`raw.joined_unknown_columns`, and for a variant without `account_id` or `contact_id`, `raw.account_state = "no-account-id-column"` /
`raw.aggregate_state = "no-contact-id-column"`; the oracle states these branches too, though no fixture reaches them). The scenario's dangling aggregate id and the cut-off below
which `Builder.contact` creates a `contacts` row are the named constants `DANGLING_CONTACT_ID` and `FIRST_DANGLING_CONTACT` (T74).

| Variant | DDL snapshot | Database state | What it exercises |
|---|---|---|---|
| `aosp-baseline` | `contacts-aosp-baseline.sql` | plain, 13 `raw_contacts` (12 records plus 1 warning for the contact that identifies nobody), 20 `data` rows for contact 1, 3 `accounts`, 11 `contacts` | accounts: two distinct, a NULL `account_id` (no account) and a dangling one (`no-accounts-row`); aggregate times: valid, zero (sentinel), year 2100 (invalid), a NULL `contact_id` (`no-contact-id`) and a dangling one (`no-contacts-row`); every mimetype, labels (named, custom, unknown type), a skipped empty value, a second name and organization row, an IM with the custom marker and AIM, a photo, a dangling `mimetype_id`, a `group_membership` row, a deleted contact, a tombstone-shaped contact (no name, source id), a contact that identifies nobody (a warning), NUL and invalid UTF-8 (`raw.text_flags`), a 400-digit number |
| `deleted-contacts` | `contacts-deleted-contacts.sql` | plain, 2 contacts and 2 `deleted_contacts` rows | the tombstones (a valid and a zero deletion time) |
| `multi-account` | `contacts-aosp-baseline.sql` | plain, 3 raw contacts of one person (one aggregate contact) | three distinct accounts (google, samsung, whatsapp with a `data_set`), one record per raw contact, one aggregate time |
| `unicode-rtl` | `contacts-aosp-baseline.sql` | plain, 4 contacts | Hebrew, Arabic with a right-to-left override, CJK and emoji names and labels |
| `huge-contact` | `contacts-aosp-baseline.sql` | plain, 1 contact with 1200 phones, 1100 e-mails and 130 notes | the list cap of 1000 and the note cap of 100 with their `_total` and `_truncated` counters |
| `duplicate-keys` | `contacts-aosp-baseline.sql`, with `accounts` and `contacts` re-created with a plain `_id integer` column (T79) | plain plus an empty `-wal`, 3 raw contacts, 3 `accounts` rows (key 1 twice), 4 `contacts` rows (key 1 three times) | a join key naming several rows: `raw.join_undecided = ["accounts","contacts"]` with `raw.join_duplicate = {"accounts":2,"contacts":3}`, no account and no time; a unique key and a dangling one beside it. The empty `-wal` makes the probe (which refuses a table without the rowid alias) defer to Parse |

The DDL snapshots and their public sources: `schemas/android-telephony/CROSSCHECK.md` (see "DDL provenance" above).

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

`gen.sh <fixture>` builds one fixture (`volume-gpt`, `volume-mbr`, `ext4`, `fat`, `exfat`, `f2fs`, `apfs`).
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
| `apfs.sh` + `apfs_oracle.py` | `apfs-ci`, `apfs-cs`, `apfs-multichunk` (`.img.gz` + `.expect.json`, in `internal/filesys/apfs/testdata/`) | the `mkapfs` arguments (label, UUIDs, case flags) and an independent Python decoder of the finished image (see "APFS determinism and limits"); `apfsck` must accept the image |
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

Limits: these are EMPTY volumes. Nothing in the Docker toolchain can populate an APFS volume (see "Populating a real APFS image on Linux: what was tried"), so
there are no files, no extra directory records (only `root` and `private-dir`),
no xattrs, hard links, symlinks, file extents, snapshots (`apfs-snap` needs the
kernel module) or encrypted volumes, and a single checkpoint (no ring
wrap-around or fallback). Those paths are covered by the synthetic builder only
and, for real data, by the manual `realimages` test.

### Supplying a real APFS image (manual `realimages` test)

The committed APFS fixtures are empty, so files, extents, clones, symlinks,
extended attributes, hard links, snapshots and directory-record name hashes on
non-root names are validated against the synthetic builder only. An examiner can
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

#### Populating a real APFS image on Linux: what was tried (Task 8)

Goal: a populated real fixture (files, extents, symlinks, xattrs, hard links, sparse
files, snapshots, name hashes on non-root names) with an oracle from the source tree
and `apfsck`. It was NOT achieved; the committed fixtures stay empty and the
populated coverage is the builder plus the manual `realimages` test above.

- `apfsprogs` (`mkapfs`, `apfsck`) only creates and checks empty containers; there
  is no tool in it that writes files. No userspace APFS writer exists in Debian.
- The Linux `apfs` kernel module with write support (`linux-apfs-rw`, git
  `923526a`) would populate a loop-mounted `mkapfs` image. The Docker Desktop
  kernel (`6.6.87.2-microsoft-standard-WSL2`) has `CONFIG_MODULES=y`,
  `CONFIG_MODVERSIONS=y`, `CONFIG_MODULE_FORCE_LOAD=y`, no module tree and no
  `apfs` module, so the module had to be built out of tree against the matching
  Microsoft kernel source (tag `linux-msft-wsl-6.6.87.2`, `/proc/config.gz`,
  `make modules_prepare`). That works: `apfs.ko` builds (it needs `genver.sh` run
  by hand, and `KBUILD_MODPOST_WARN=1` or a full `make vmlinux` plus
  `cp vmlinux.symvers Module.symvers` for the exported-symbol CRCs).
- Loading it did not work: with no `Module.symvers`, `insmod -f` fails with
  `Invalid relocation target, existing value is nonzero for type 1`; with
  `Module.symvers` from a full `vmlinux` build (about 35 minutes on 12 cores) the
  load fails with `disagrees about version of symbol module_layout`, because the
  rebuilt kernel differs from the running one in `struct module` (the running
  kernel has `CONFIG_DEBUG_INFO_BTF_MODULES`, which needs `pahole` and a vmlinux
  BTF section to reproduce). The remaining way is to edit the module's
  `__versions` table so the version check passes, which deliberately defeats the
  kernel's ABI check and loads out-of-tree code into the shared Docker Desktop VM
  kernel; that was judged unsafe on a shared machine and not done.
- Even with a loadable module the image would not be reproducible: the module stamps
  create/change/access times from the kernel's real-time clock, which `faketime`
  cannot freeze, so a normalizer that rewrites every inode time and re-checksums
  the blocks would also be needed; and `linux-apfs-rw` cannot create snapshots or
  clones, so those would stay builder-only anyway.
- The first load attempt (`insmod -f`) tainted the Docker Desktop VM kernel (the
  taint disappears when Docker Desktop is restarted); no module was ever loaded
  (`/proc/filesystems` has no `apfs`) and the build volume was removed.

A machine that can run a stock Debian or Ubuntu kernel with `apfs.ko` (a VM or a
physical Linux box, not Docker Desktop) is the way forward: `mkapfs`, `mount -t apfs`,
populate, `umount`, `apfsck`, then a normalizer for the inode times.

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

## Adding a fixture

1. Add `<name>.sh` taking the output directory as `$1`; source `lib.sh` and
   wrap commands whose invocation belongs in the oracle with `run`.
2. Register it in `gen.sh`.
3. Keep images small: a few hundred KiB compressed at most. The raw size is set by
   the filesystem (the smallest FAT32 image is 34 MiB, see above; every other fixture
   is at most 16 MiB raw).

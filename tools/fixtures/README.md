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

`gen.sh <fixture>` builds one fixture (`volume-gpt`, `volume-mbr`, `ext4`, `fat`, `exfat`, `f2fs`, `ewf`).
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
are byte-identical across runs (checked by generating twice and comparing every output sha256) and
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

## Adding a fixture

1. Add `<name>.sh` taking the output directory as `$1`; source `lib.sh` and
   wrap commands whose invocation belongs in the oracle with `run`.
2. Register it in `gen.sh`.
3. Keep images small: a few hundred KiB compressed at most. The raw size is set by
   the filesystem (the smallest FAT32 image is 34 MiB, see above; every other fixture
   is at most 16 MiB raw).

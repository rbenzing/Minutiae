#!/bin/bash
# F2FS fixtures built by the real mkfs.f2fs and sload.f2fs from a deterministic
# source tree (f2fs_tree.sh).
# Usage: f2fs.sh <outdir>   (normally called by gen.sh)
#
#   f2fs-extra-attr  64 MiB, label "fixture": mkfs.f2fs -O extra_attr,
#                    inode_checksum,sb_checksum,inode_crtime (the Android
#                    layout: 24-byte inode extra area, inode and superblock
#                    checksums), populated by sload.f2fs
#   f2fs-default     64 MiB, label "fixture2": mkfs.f2fs default features (no
#                    extra_attr), the same source tree
#
# Both hold inline files and symlinks, a 600-entry directory, a 3 MiB file, a
# 4 MiB "sparse" file (sload writes its holes as zero blocks, and its block
# addresses spill into a direct node), a fragmented file (the generator fails
# unless one exists), UTF-8 names and a 255-byte name.
#
# Oracle: f2fs_oracle.py walks the SOURCE TREE (never the image, never
# Minutiae) for the files; the superblock, checkpoint, inode -> NAT block
# address, per-file data blocks and the free main-area blocks come from the
# external tools' reports on the finished image (fsck.f2fs -l/-t/-M/-f,
# dump.f2fs -s/-n, blkid); the oracle cross-checks them against each other and
# fails the generator on any mismatch (see f2fs_oracle.py).
#
# Deleted entries need a mounted F2FS (sload.f2fs cannot delete, and F2FS has
# no dtime): when the kernel can mount it the three /dir/removable-* files are
# removed through the mount; the Docker Desktop kernel has no f2fs module, so
# the fixtures then have "deleted": [] and a generator.note (README.md, "F2FS
# determinism").
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "f2fs.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=f2fs_tree.sh
. "$here/f2fs_tree.sh"

out=$(cd "${1:?outdir required}" && pwd)

# Reproducible images: mkfs.f2fs and sload.f2fs stamp times from the clock, so
# they run under a frozen faketime clock (real time still ticks for monotonic
# timers); NO_FAKE_STAT keeps the source files' own mtimes, which sload copies.
# mkfs.f2fs seeds its checkpoint version from rand(): -r fixes it. uuid and
# label are fixed. The superblock also records the kernel's version string,
# which f2fs_normalize.py replaces.
export LC_ALL=C.UTF-8
export NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20"

# frun <cmd...>: run under the frozen clock and record the command.
frun() {
  CMDS+=("faketime -f '$clock' $*")
  faketime -f "$clock" "$@"
}

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-f2fs # fixed name: the commands recorded in the oracle mention it
# The images live outside tmpfs: Docker caps /dev/shm at 64 MiB, which two images
# (and the sources) do not fit in. The source tree stays on tmpfs so directory
# enumeration order is creation order (a precaution, as in ext4.sh).
imgs=${TMPDIR:-/tmp}/minutiae-f2fs-images
cleanup() {
  umount "$work/mnt" 2>/dev/null || true
  rm -rf "$work" "$imgs"
}
trap cleanup EXIT
rm -rf "$work" "$imgs"
mkdir "$work" "$imgs"

# remove_through_mount <img> <src> <paths...>: delete files through a kernel
# mount of the image. Returns 1, changing nothing, when the kernel cannot mount
# F2FS (the Docker Desktop kernel cannot). The mount stamps directory ctimes
# and checkpoints from the real clock, so images made this way are not
# byte-reproducible; the parent directory's mtime is put back to the source
# tree's.
remove_through_mount() {
  local img=$1 src=$2 p
  shift 2
  local mnt="$work/mnt"
  mkdir -p "$mnt"
  mount -t f2fs -o loop "$img" "$mnt" 2>/dev/null || return 1
  CMDS+=("mount -t f2fs -o loop <img> <mnt>")
  for p in "$@"; do
    run rm "$mnt$p"
    run touch -r "$src$(dirname "$p")" "$mnt$(dirname "$p")"
  done
  run umount "$mnt"
}

# build_fixture <name> <label> <uuid> <mkfs.f2fs extra args...>
build_fixture() {
  local name=$1 label=$2 fsuuid=$3
  shift 3
  local src="$work/$name.src" img="$imgs/$name.img" cap="$work/$name.cap" p
  mkdir "$src" "$cap"
  make_tree "$src"
  CMDS=()
  CMDS+=("# source tree: make_tree in tools/fixtures/f2fs_tree.sh")

  run truncate -s 64M "$img"
  frun mkfs.f2fs -q -f -l "$label" -U "$fsuuid" "$@" -r "$img"
  frun sload.f2fs -f "$src" -t / "$img" >"$work/$name.sload.log" 2>&1 || { tail -c 2000 "$work/$name.sload.log"; echo "sload.f2fs failed" >&2; exit 1; }

  local deleted=() note=""
  if remove_through_mount "$img" "$src" $f2fs_removable; then
    deleted=($f2fs_removable)
    note="the /dir/removable-* files were removed through a kernel f2fs mount, which stamps times from the real clock: this image is not byte-reproducible"
  else
    note="no deleted entries: the kernel of the generator container cannot mount f2fs (no module), sload.f2fs cannot delete and F2FS keeps no dtime; deleted-entry coverage comes from the builder tests"
  fi

  CMDS+=("python3 tools/fixtures/f2fs_normalize.py <img>   # replace the kernel version string in both superblock copies, fix their checksums")
  python3 "$here/f2fs_normalize.py" "$img"

  # The oracle's files come from the source tree before anything reads the
  # image (the removed files excluded), so they cannot depend on the image.
  local exclude=()
  for p in "${deleted[@]}"; do exclude+=(--exclude "$p"); done
  CMDS+=("python3 tools/fixtures/f2fs_oracle.py tree <src> ${exclude[*]}")
  python3 "$here/f2fs_oracle.py" tree "$src" "${exclude[@]}" >"$work/$name.files.json"

  # Facts the tools report about the finished image (read-only: --dry-run, and
  # the image hash is checked afterwards). dump.f2fs writes dump_sit and
  # dump_nat into the current directory, hence the cd.
  local before after
  before=$(sha256sum "$img")
  (
    cd "$cap"
    tool() { # tool <capture file> <cmd...>: save stdout+stderr, fail unless the tool succeeds
      local f=$1
      shift
      "$@" >"$cap/$f" 2>&1 || { cat "$cap/$f"; echo "$* failed" >&2; exit 1; }
    }
    tool fsck-l.txt fsck.f2fs -l "$img"
    tool dump-i.txt dump.f2fs -d 1 -s0~-1 "$img"
    tool dump-n.txt dump.f2fs -n0~-1 "$img"
    tool fsck-f.txt fsck.f2fs -f --dry-run "$img"
    tool tree.txt fsck.f2fs -t --dry-run "$img"
    tool map.txt fsck.f2fs -d 1 -M --dry-run "$img"
    tool blkid.txt blkid -p -o export "$img"
  )
  after=$(sha256sum "$img")
  [ "$before" = "$after" ] || { echo "a read-only tool changed $name.img" >&2; exit 1; }
  CMDS+=("fsck.f2fs -l <img>   # superblock, checkpoint")
  CMDS+=("dump.f2fs -d 1 -s0~-1 <img>   # superblock, checkpoint, per-segment valid-block bitmaps (dump_sit)")
  CMDS+=("dump.f2fs -n0~-1 <img>   # NAT: nid -> block address (dump_nat)")
  CMDS+=("fsck.f2fs -f --dry-run <img>   # must report every check Ok")
  CMDS+=("fsck.f2fs -t --dry-run <img>   # directory tree with inode numbers")
  CMDS+=("fsck.f2fs -d 1 -M --dry-run <img>   # data blocks of every file")
  CMDS+=("blkid -p -o export <img>   # type, label, uuid")
  CMDS+=("python3 tools/fixtures/f2fs_oracle.py layout <captures> <files.json>")
  python3 "$here/f2fs_oracle.py" layout "$cap" "$work/$name.files.json" >"$work/$name.layout.json"
  [ "$(jq -r .uuid "$work/$name.layout.json")" = "$fsuuid" ] || { echo "blkid uuid differs from the one passed to mkfs.f2fs" >&2; exit 1; }
  [ "$(jq '.fragmented | length' "$work/$name.layout.json")" -ge 1 ] || { echo "no fragmented file in $name: change f2fs_tree.sh" >&2; exit 1; }

  local gen del
  gen=$(generator_json "$img" f2fs-tools util-linux python3 coreutils jq gzip faketime | jq --arg note "$note" '. + {note: $note}')
  if [ "${#deleted[@]}" -eq 0 ]; then
    del='[]'
  else
    del=$(printf '%s\n' "${deleted[@]}" | jq -R . | jq -s .)
  fi
  jq -n --argjson g "$gen" --slurpfile f "$work/$name.files.json" --slurpfile l "$work/$name.layout.json" --argjson d "$del" \
    '$l[0] + {generator: $g, files: $f[0].files, deleted: $d}' >"$out/$name.expect.json"
  gzip -n -9 -c "$img" >"$out/$name.img.gz"
  echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"
}

build_fixture f2fs-extra-attr fixture 1f2e3d4c-5b6a-4978-8695-a4b3c2d1e0f9 \
  -O extra_attr,inode_checksum,sb_checksum,inode_crtime
build_fixture f2fs-default fixture2 2a3b4c5d-6e7f-4a80-91a2-b3c4d5e6f708

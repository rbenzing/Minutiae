#!/bin/bash
# exFAT fixture: mkfs.exfat (exfatprogs) formats, the exfat-fuse driver
# populates the image through a loop device, from the deterministic source
# tree of fat_tree.sh.
# Usage: exfat.sh <outdir>   (normally called by gen.sh)
#
# Needs a privileged container (docker run --privileged): /dev/fuse and a loop
# device. The Docker Desktop kernel has no exfat driver, hence the FUSE one;
# exfatprogs itself cannot add files. See README.md.
#
#   exfat  16 MiB, 4 KiB clusters, a 3584-cluster heap
#
# The image holds long names (several name entries), UTF-8 names (accents,
# CJK, a surrogate pair), a directory of 120 entries (several clusters), an
# empty file, a fragmented file (FAT chain; the others are contiguous with
# NoFatChain), deleted files and a deleted directory with content.
#
# Oracle: fat_oracle.py walks the SOURCE TREE (never the image); the deleted
# names are the generator's own list; the geometry, serial, label and the
# cluster chain of every live path come from dump.exfat, and the free clusters
# are the ones no live chain (nor the bitmap, up-case table and root
# directory) holds; exfat_chains.py cross-checks their number against the
# "Free Clusters" statistic dump.exfat prints. Never Minutiae.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "exfat.sh must run as root (the container does)" >&2
  exit 1
fi
for tool in mkfs.exfat mount.exfat-fuse dump.exfat fsck.exfat faketime losetup mountpoint; do
  command -v "$tool" >/dev/null || { echo "exfat.sh needs $tool (rebuild the minutiae-fixtures image)" >&2; exit 1; }
done
if [ ! -e /dev/fuse ] || ! losetup -f >/dev/null 2>&1; then
  echo "exfat.sh needs /dev/fuse and a loop device: run the container with --privileged (see README.md)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=fat_tree.sh
. "$here/fat_tree.sh"

out=$(cd "${1:?outdir required}" && pwd)

# Reproducible image: mkfs.exfat derives the volume serial from the clock and
# exfat-fuse stamps created/accessed times from it, so both run under a frozen
# faketime clock (real time still ticks for monotonic timers, else the FUSE
# daemon hangs); NO_FAKE_STAT keeps the source files' own mtimes. Modification
# times are set from the source tree at the end.
export LC_ALL=C.UTF-8
export NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20"
tree_emoji=1

# frun <cmd...>: run under the frozen clock and record the command.
frun() {
  CMDS+=("faketime -f '$clock' $*")
  faketime -f "$clock" "$@"
}

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-exfat # fixed name: the commands recorded in the oracle mention it
mnt=$work/mnt
loopdev=
cleanup() {
  mountpoint -q "$mnt" 2>/dev/null && umount "$mnt" || true
  [ -z "$loopdev" ] || losetup -d "$loopdev" 2>/dev/null || true
  rm -rf "$work"
}
# fuse_running: succeeds while a mount.exfat-fuse process for $loopdev exists
# (the container has no pgrep or fuser, so /proc is scanned).
fuse_running() {
  local c
  for c in /proc/[0-9]*/cmdline; do
    case $(tr '\0' ' ' <"$c" 2>/dev/null) in
      *mount.exfat-fuse*"$loopdev"*) return 0 ;;
    esac
  done
  return 1
}

rm -rf "$work"
mkdir "$work" "$mnt"
trap cleanup EXIT

name=exfat
src="$work/$name.src"
img="$work/$name.img"
mkdir "$src"
make_tree "$src"
CMDS=()
CMDS+=("# source tree: make_tree in tools/fixtures/fat_tree.sh (with the surrogate-pair name)")

exclude=()
for p in $fat_deleted $fat_overwritten; do exclude+=(--exclude "$p"); done
for p in $fat_deleted_dirs; do exclude+=(--exclude-tree "$p"); done
CMDS+=("python3 tools/fixtures/fat_oracle.py <src> ${exclude[*]}")
python3 "$here/fat_oracle.py" "$src" "${exclude[@]}" >"$work/$name.files.json"

CMDS+=("truncate -s 16M <img>")
truncate -s 16M "$img"
frun mkfs.exfat -L FIXTURE "$img" >/dev/null

CMDS+=("losetup -f --show <img>   # /dev/loopN")
loopdev=$(losetup -f --show "$img")
CMDS+=("faketime -f '$clock' mount.exfat-fuse -o uid=0,gid=0 /dev/loopN <mnt>   # in the background")
faketime -f "$clock" mount.exfat-fuse -o uid=0,gid=0 "$loopdev" "$mnt" &
daemon=$!
for _ in $(seq 1 100); do
  mountpoint -q "$mnt" && break
  sleep 0.1
done
mountpoint -q "$mnt" || { echo "exfat-fuse did not mount $img" >&2; exit 1; }

# Everything except /frag in sorted order (a parent sorts before its content),
# then /frag as: a, b, delete a, c.
CMDS+=("# copy: mkdir/cp of every source entry except /frag, in sorted order (parents first)")
while IFS= read -r -d '' p; do
  rel=${p#"$src"}
  case $rel in /frag | /frag/*) continue ;; esac
  if [ -d "$p" ]; then mkdir "$mnt$rel"; else cp --no-preserve=all "$p" "$mnt$rel"; fi
done < <(find "$src" -mindepth 1 -print0 | sort -z)
CMDS+=("# copy /frag: mkdir; cp a; cp b; rm a; cp c")
mkdir "$mnt/frag"
cp --no-preserve=all "$src/frag/a-first-file.bin" "$src/frag/b-second-file.bin" "$mnt/frag/"
rm "$mnt/frag/a-first-file.bin"
cp --no-preserve=all "$src/frag/c-large-fragmented.bin" "$mnt/frag/"

for p in $fat_deleted; do
  CMDS+=("rm <mnt>$p")
  rm "$mnt$p"
done
for p in $fat_deleted_dirs; do
  CMDS+=("rm -r <mnt>$p")
  rm -r "$mnt$p"
done

# Modification (and access) times from the source tree: files first, then
# directories deepest first (adding or removing an entry touches its parent).
CMDS+=("touch -d @<mtime of the source entry> <mnt><path>   # every live entry, files then directories deepest first")
jq -r '.files[] | select(.type == "file") | "\(.mtime)\t\(.path)"' "$work/$name.files.json" |
  while IFS=$'\t' read -r t p; do touch -d "@$t" "$mnt$p"; done
jq -r '[.files[] | select(.type == "dir")] | sort_by(.path) | reverse | .[] | "\(.mtime)\t\(.path)"' "$work/$name.files.json" |
  while IFS=$'\t' read -r t p; do touch -d "@$t" "$mnt$p"; done

CMDS+=("umount <mnt>; losetup -d /dev/loopN")
umount "$mnt"
wait "$daemon" || true
# mount.exfat-fuse detaches itself, so the process started above can be gone
# while the daemon still flushes the image: poll until none is left (it holds
# the loop device) before the image is checked and compressed.
for _ in $(seq 1 100); do
  fuse_running || break
  sleep 0.1
done
if fuse_running; then
  echo "the exfat-fuse daemon is still running 10 s after umount" >&2
  exit 1
fi
losetup -d "$loopdev"
loopdev=

CMDS+=("fsck.exfat -n <img>   # must report a clean filesystem")
fsck.exfat -n "$img" >"$work/$name.fsck.log" 2>&1 || { cat "$work/$name.fsck.log"; echo "final fsck.exfat not clean" >&2; exit 1; }
CMDS+=("python3 tools/fixtures/exfat_chains.py <img> <files.json>   # dump.exfat chains, geometry, free clusters")
python3 "$here/exfat_chains.py" "$img" "$work/$name.files.json" >"$work/$name.chains.json"

gen=$(generator_json "$img" exfatprogs exfat-fuse python3 coreutils jq gzip faketime util-linux)
del=$(printf '%s\n' $fat_deleted $fat_deleted_dirs | jq -R . | jq -s .)
jq -n --argjson g "$gen" --slurpfile f "$work/$name.files.json" --slurpfile c "$work/$name.chains.json" \
  --argjson d "$del" '
  $c[0] as $c
  | {generator: $g, type: "exfat", label: $c.label, uuid: ($c.serial[0:4] + "-" + $c.serial[4:]),
     block_size: $c.cluster_size, size: $c.size, sector_size: $c.sector_size,
     data_start: $c.data_start, cluster_count: $c.cluster_count,
     files: [$f[0].files[] | . + {clusters: $c.chains[.path]}],
     deleted: $d, free_count: $c.free_count, free_clusters: $c.free_clusters}' >"$out/$name.expect.json"
gzip -n -9 -c "$img" >"$out/$name.img.gz"
echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"

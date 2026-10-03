#!/bin/bash
# FAT12, FAT16 and FAT32 fixtures built by the real mkfs.fat and mtools from a
# deterministic source tree (fat_tree.sh).
# Usage: fat.sh <outdir>   (normally called by gen.sh)
#
#   fat12  1440 KiB floppy, 512-byte clusters
#   fat16  16 MiB, 2 KiB clusters
#   fat32  34 MiB (the smallest a 512-byte-cluster FAT32 can be: it needs
#          65525 clusters), 512-byte clusters
#
# Every image holds long names (VFAT), mixed-case and lower-case 8.3 names, an
# OEM-only short name, UTF-8 names (accents, CJK, a surrogate pair), a
# directory of 120 entries (several clusters), an empty file, a fragmented
# file (a written, b written, a deleted, c written), deleted files with long
# names and a deleted directory with content.
#
# Oracle: fat_oracle.py walks the SOURCE TREE (never the image); the deleted
# names are the generator's own list; geometry and the cluster chains of every
# live path come from fsck.fat -v and mshowfat, and the free clusters are the
# ones no live chain holds (fat_chains.py cross-checks the count against
# fsck.fat). Never Minutiae.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "fat.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=fat_tree.sh
. "$here/fat_tree.sh"

out=$(cd "${1:?outdir required}" && pwd)

# Reproducible images: mkfs.fat and mtools stamp creation and access times from
# the clock, so they run under a frozen faketime clock (real time still ticks
# for monotonic timers); NO_FAKE_STAT keeps the source files' own mtimes, which
# mcopy -m copies. The volume id is fixed.
export LC_ALL=C.UTF-8
export MTOOLS_SKIP_CHECK=1 NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20"

# frun <cmd...>: run under the frozen clock and record the command.
frun() {
  CMDS+=("faketime -f '$clock' $*")
  faketime -f "$clock" "$@"
}

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-fat # fixed name: the commands recorded in the oracle mention it
rm -rf "$work"
mkdir "$work"


# build_fixture <name> <fat type> <size in KiB> <mkfs.fat extra args...>
build_fixture() {
  local name=$1 type=$2 kb=$3
  shift 3
  local src="$work/$name.src" img="$work/$name.img" stage="$work/$name.stage" p top
  mkdir "$src" "$stage"
  make_tree "$src"
  CMDS=()
  CMDS+=("# source tree: make_tree in tools/fixtures/fat_tree.sh")

  # The oracle comes from the source tree before anything touches the image.
  local exclude=()
  for p in $fat_deleted $fat_overwritten; do exclude+=(--exclude "$p"); done
  for p in $fat_deleted_dirs; do exclude+=(--exclude-tree "$p"); done
  CMDS+=("python3 tools/fixtures/fat_oracle.py <src> ${exclude[*]}")
  python3 "$here/fat_oracle.py" "$src" "${exclude[@]}" >"$work/$name.files.json"

  frun mkfs.fat -F "$type" -i 1234ABCD -n FIXTURE "$@" -C "$img" "$kb" >/dev/null

  # Everything except /frag in sorted order, then /frag as a, b, delete a, c.
  # The empty directory staged for /frag carries its source mtime into the image.
  mkdir "$stage/frag"
  touch -d "@$(stat -c %Y "$src/frag")" "$stage/frag"
  local tops=()
  while IFS= read -r -d '' p; do
    top=${p#"$src"/}
    [ "$top" = frag ] || tops+=("$p")
  done < <(find "$src" -mindepth 1 -maxdepth 1 -print0 | sort -z)
  CMDS+=("# copy: top-level entries of the source tree except frag, in sorted order")
  frun mcopy -s -m -i "$img" "${tops[@]}" ::/
  frun mcopy -s -m -i "$img" "$stage/frag" ::/
  frun mcopy -m -i "$img" "$src/frag/a-first-file.bin" "$src/frag/b-second-file.bin" ::/frag/
  frun mdel -i "$img" ::/frag/a-first-file.bin
  if [ "$type" = 32 ]; then
    # FAT32: mtools allocates from the FSInfo next-free hint, which would skip a's
    # freed clusters. Set the hint to 0xFFFFFFFF ("unknown", what other writers
    # leave there) so c is allocated from the start of the volume, in a's place.
    CMDS+=("printf '\xff\xff\xff\xff' | dd of=<img> bs=1 seek=1004 conv=notrunc   # FSInfo next-free hint")
    printf '\xff\xff\xff\xff' | dd of="$img" bs=1 seek=1004 conv=notrunc 2>/dev/null
  fi
  frun mcopy -m -i "$img" "$src/frag/c-large-fragmented.bin" ::/frag/

  for p in $fat_deleted; do
    frun mdel -i "$img" "::$p"
  done
  for p in $fat_deleted_dirs; do
    frun mdeltree -i "$img" "::$p"
  done

  CMDS+=("fsck.fat -n <img>   # must report a clean filesystem")
  fsck.fat -n "$img" >"$work/$name.fsck.log" 2>&1 || { cat "$work/$name.fsck.log"; echo "final fsck.fat not clean" >&2; exit 1; }
  CMDS+=("fsck.fat -v -n <img>   # geometry, clusters in use")
  fsck.fat -v -n "$img" >"$work/$name.fsckv.log" 2>&1
  CMDS+=("python3 tools/fixtures/fat_chains.py <img> <files.json> <fsck.fat -v output>   # mshowfat chains, free clusters")
  python3 "$here/fat_chains.py" "$img" "$work/$name.files.json" "$work/$name.fsckv.log" >"$work/$name.chains.json"

  local gen del dd
  gen=$(generator_json "$img" dosfstools mtools python3 coreutils jq gzip faketime)
  del=$(printf '%s\n' $fat_deleted $fat_deleted_dirs | jq -R . | jq -s .)
  jq -n --argjson g "$gen" --slurpfile f "$work/$name.files.json" --slurpfile c "$work/$name.chains.json" \
    --argjson d "$del" --arg type "fat$type" --arg label FIXTURE --arg serial 1234-ABCD '
    $c[0] as $c
    | {generator: $g, type: $type, label: $label, uuid: $serial,
       block_size: $c.cluster_size, size: $c.size, sector_size: $c.sector_size,
       data_start: $c.data_start, cluster_count: $c.cluster_count,
       files: [$f[0].files[] | . + {clusters: $c.chains[.path]}],
       deleted: $d, free_count: $c.free_count, free_clusters: $c.free_clusters}' >"$out/$name.expect.json"
  gzip -n -9 -c "$img" >"$out/$name.img.gz"
  echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"
}

build_fixture fat12 12 1440 -s 1
build_fixture fat16 16 16384 -s 4
build_fixture fat32 32 34816 -s 1

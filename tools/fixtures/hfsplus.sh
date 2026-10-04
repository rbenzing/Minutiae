#!/bin/bash
# HFS+ fixtures: mkfs.hfsplus (hfsprogs) formats four 8 MiB images, a second,
# independent parse (hfsplus_oracle.py) plus fsck.hfsplus and blkid describe
# them. Usage: hfsplus.sh <outdir>   (normally called by gen.sh hfsplus)
#        hfsplus.sh check-builder <dir>   (fsck every *.img under <dir>)
#
# Runs in the SEPARATE image minutiae-fixtures-hfs (Dockerfile.hfs); the main
# minutiae-fixtures image has no hfsprogs. No --privileged needed.
#
#   hfsplus-empty    mkfs.hfsplus -v FIXTURE            (HFS+, 4 KiB blocks)
#   hfsx-empty       mkfs.hfsplus -s -v FIXTURE         (HFSX, case-sensitive)
#   hfsplus-journal  mkfs.hfsplus -J -v FIXTURE         (journaled: the real
#                    .journal_info_block and .journal files in the root)
#   hfsplus-1k       mkfs.hfsplus -b 1024 -v FIXTURE    (1 KiB blocks)
#   hfsplus-wrapped  mkfs.hfsplus -w -v FIXTURE         (HFS+ embedded in a
#                    classic HFS wrapper; the volume starts at byte 45056)
#
# These are EMPTY volumes: nothing in hfsprogs copies files in, and the Docker
# Desktop kernel has no hfsplus driver (mount -t hfsplus fails; see
# README.md, "HFS+ fixtures"), so there is no populated real image. If a
# machine's kernel can mount hfsplus, a populated tier belongs here (not yet
# written: it could not be tested).
#
# Determinism: mkfs.hfsplus takes its dates from the clock (frozen with
# faketime) but draws the 64-bit volume id from a random source;
# hfsplus_normalize.py rewrites that id (both header copies) to a fixed value.
# Regenerating gives byte-identical images.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [ "${1:-}" = check-builder ]; then
  dir=${2:?usage: hfsplus.sh check-builder <dir>}
  command -v fsck.hfsplus >/dev/null || { echo "hfsplus.sh needs fsck.hfsplus (use the minutiae-fixtures-hfs image, see README.md)" >&2; exit 1; }
  fail=0
  n=0
  for img in "$dir"/*.img; do
    [ -e "$img" ] || continue
    n=$((n + 1))
    if out=$(fsck.hfsplus -n -f "$img" 2>&1); then
      echo "OK    $img"
    else
      echo "FAIL  $img"
      printf '%s\n' "$out" | sed 's/^/      /'
      fail=1
    fi
  done
  [ "$n" -gt 0 ] || { echo "no *.img files in $dir" >&2; exit 1; }
  exit "$fail"
fi

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "hfsplus.sh must run as root (the container does)" >&2
  exit 1
fi
for tool in mkfs.hfsplus fsck.hfsplus blkid faketime jq python3 gzip; do
  command -v "$tool" >/dev/null || { echo "hfsplus.sh needs $tool: build and use the minutiae-fixtures-hfs image (README.md, \"HFS+ fixtures\")" >&2; exit 1; }
done

# shellcheck source=lib.sh
. "$here/lib.sh"

out=$(cd "${1:?outdir required}" && pwd)

export LC_ALL=C TZ=UTC
export NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20" # unix 1700000000
create_unix=1700000000
volume_id=4d494e5554494145 # "MINUTIAE"

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-hfsplus # fixed name: the commands recorded in the oracle mention it
cleanup() { rm -rf "$work"; }
rm -rf "$work"
mkdir "$work"
trap cleanup EXIT

# name|oracle type|mkfs flags|block size|journaled|wrapper
variants=(
  "hfsplus-empty|hfsplus||4096|0|0"
  "hfsx-empty|hfsx|-s|4096|0|0"
  "hfsplus-journal|hfsplus|-J|4096|1|0"
  "hfsplus-1k|hfsplus|-b 1024|1024|0|0"
  "hfsplus-wrapped|hfsplus|-w|4096|0|1"
)

note="Empty volume made by mkfs.hfsplus (hfsprogs): real root folder, system files and, with -J, the real journal files. No populated multi-level tree, hard links, attributes, fragmented files, overflow extents, decomposed Unicode or deletions: nothing in hfsprogs copies files in and the Docker Desktop kernel has no hfsplus driver (populated: false). The volume id (finderInfo words 6-7) is normalized by hfsplus_normalize.py; mkfs draws it at random."

for v in "${variants[@]}"; do
  IFS='|' read -r name type flags bs journaled wrapped <<<"$v"
  img="$work/$name.img"
  CMDS=()
  CMDS+=("truncate -s 8M <img>")
  truncate -s 8M "$img"
  # shellcheck disable=SC2086 # $flags is a short word list on purpose
  CMDS+=("faketime -f '$clock' mkfs.hfsplus $flags -v FIXTURE <img>")
  # shellcheck disable=SC2086
  faketime -f "$clock" mkfs.hfsplus $flags -v FIXTURE "$img" >"$work/$name.mkfs.log" 2>&1 || { cat "$work/$name.mkfs.log"; echo "mkfs.hfsplus failed" >&2; exit 1; }
  CMDS+=("python3 tools/fixtures/hfsplus_normalize.py <img> $volume_id")
  python3 "$here/hfsplus_normalize.py" "$img" "$volume_id"
  # mkfs.hfsplus -s leaves the root folder without kHFSHasFolderCountMask,
  # which Apple's fsck_hfs reports (and nothing else); the oracle demands
  # exactly that report, see README.md.
  quirk=()
  if [ "$wrapped" = 1 ]; then quirk+=(--wrapper); fi
  if [ "$type" = hfsx ]; then
    quirk+=(--fsck-issue "HasFolderCount flag needs to be set (id = 2)" --fsck-issue "(It should be 0x10 instead of 0)")
  fi
  CMDS+=("python3 tools/fixtures/hfsplus_oracle.py <img> --type $type --label FIXTURE --block-size $bs --journaled $journaled --volume-id $volume_id --create-unix $create_unix ${quirk[*]}   # also runs fsck.hfsplus -n -f and blkid -p -o export")
  python3 "$here/hfsplus_oracle.py" "$img" --type "$type" --label FIXTURE --block-size "$bs" \
    --journaled "$journaled" --volume-id "$volume_id" --create-unix "$create_unix" "${quirk[@]}" >"$work/$name.oracle.json"

  gen=$(generator_json "$img" hfsprogs util-linux faketime python3 coreutils jq gzip)
  jq -n --argjson g "$gen" --arg note "$note" --slurpfile o "$work/$name.oracle.json" \
    '{generator: ($g + {note: $note})} + $o[0]' >"$out/$name.expect.json"
  gzip -n -9 -c "$img" >"$out/$name.img.gz"
  echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"
done

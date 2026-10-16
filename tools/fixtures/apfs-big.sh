#!/bin/bash
# Large APFS container made by the real mkapfs: more chunks than one chunk-info
# block (CIB) holds (126), so the space manager has several CIBs. The image is
# SPARSE and is NEVER committed (*.big.img and *.sparse.img are gitignored, and
# this script refuses to write the image anywhere inside the repository).
#
# Every file this script writes goes under $APFS_BIG_WORK, a directory the caller
# bind-mounts from the host and which lies outside the repository mount. The
# script never writes the image into the container's own filesystem: that would
# grow the Docker Desktop virtual disk, which never shrinks.
#
# Usage:
#   APFS_BIG_WORK=<dir> apfs-big.sh
#       probe: print the space-manager counts, write nothing else
#   APFS_BIG_WORK=<dir> apfs-big.sh <outdir> <keep-image-path>
#       write <outdir>/apfs-bigcib.expect.json and keep the sparse image at
#       <keep-image-path> (outside the repo; the test reads it through
#       MINUTIAE_APFS_BIG_IMAGE)
#
# Gates, all of which fail the script: apfsck accepts the image (read-only, the
# image does not change); the independent decoder apfs_oracle.py passes every
# cross-check; a second mkapfs run under the same frozen clock and UUIDs
# produces the same bytes.
set -euo pipefail
umask 022
if [ "$(id -u)" != 0 ]; then
  echo "apfs-big.sh must run as root (the container does)" >&2
  exit 1
fi
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

: "${APFS_BIG_WORK:?set APFS_BIG_WORK to a bind-mounted host directory outside the repository (never the container filesystem)}"
mkdir -p "$APFS_BIG_WORK"
work=$(cd "$APFS_BIG_WORK" && pwd)/minutiae-apfs-big
case "$work/" in
  "$root"/*) echo "refusing to write a large image inside the repository ($root): choose a scratch directory outside it" >&2; exit 2 ;;
esac

keep=""
if [ $# -ge 1 ]; then
  [ $# -eq 2 ] || { echo "usage: apfs-big.sh [<outdir> <keep-image-path>]" >&2; exit 2; }
  out=$(cd "$1" && pwd)
  mkdir -p "$(dirname "$2")"
  keep=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
  case "$keep/" in
    "$root"/*) echo "refusing to write a large image inside the repository ($root): choose a scratch directory outside it" >&2; exit 2 ;;
  esac
fi

size=${APFS_BIG_SIZE:-17G} # 17 GiB = 136 chunks of 128 MiB: more than the 126 one CIB holds
label=bigcib
cu=0b9c5d7e-3a41-4f28-9e6d-c2a8714f50b3
vu=e47a1f90-d5c2-4b68-83a7-6f0b29d1c845
clock="2023-11-14 22:13:20"
export LC_ALL=C.UTF-8 NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1

trap 'rm -rf "$work"' EXIT
rm -rf "$work"
mkdir "$work"

CMDS=()
make_image() {
  rm -f "$1"
  CMDS+=("truncate -s $size <img>")
  truncate -s "$size" "$1"
  CMDS+=("faketime -f '$clock' mkapfs -L $label -U $cu -u $vu <img>")
  faketime -f "$clock" mkapfs -L "$label" -U "$cu" -u "$vu" "$1" >"$work/mkapfs.log" 2>&1 || { cat "$work/mkapfs.log"; echo "mkapfs failed for $size" >&2; exit 1; }
}

time make_image "$work/a.big.img"
before=$(sha256sum "$work/a.big.img" | cut -d' ' -f1)
CMDS+=("apfsck <img>   # exit status 0 = structurally valid")
apfsck "$work/a.big.img" >"$work/apfsck.txt" 2>&1 || { cat "$work/apfsck.txt"; echo "apfsck rejected the $size image" >&2; exit 1; }
after=$(sha256sum "$work/a.big.img" | cut -d' ' -f1)
[ "$before" = "$after" ] || { echo "apfsck changed the image" >&2; exit 1; }
CMDS+=("python3 tools/fixtures/apfs_oracle.py <img> $label 0 0 $cu $vu")
time python3 "$here/apfs_oracle.py" "$work/a.big.img" "$label" 0 0 "$cu" "$vu" >"$work/oracle.json"
jq '.spaceman | with_entries(select(.key | test("cib|cab|chunk")))' "$work/oracle.json"
jq '{free_blocks, chunks: (.chunks | length), free_ranges: (.free_ranges | length)}' "$work/oracle.json"
ls -ls "$work/a.big.img" "$work/oracle.json"

[ -n "$keep" ] || exit 0
saved=("${CMDS[@]}")
make_image "$work/b.big.img"
CMDS=("${saved[@]}")
cmp "$work/a.big.img" "$work/b.big.img" || { echo "mkapfs output for $size is not deterministic" >&2; exit 1; }
rm -f "$work/b.big.img"
gen=$(generator_json "$work/a.big.img" apfsprogs util-linux python3 coreutils jq faketime |
  jq --arg note "empty $size container made by mkapfs: more chunks than one CIB holds. The image is sparse and is not committed; MINUTIAE_APFS_BIG_IMAGE names a regenerated copy (apfs-big.sh <outdir> <keep-image-path>, outside the repo)." '. + {note: $note}')
jq -n --argjson g "$gen" --slurpfile o "$work/oracle.json" '$o[0] + {generator: $g}' >"$out/apfs-bigcib.expect.json"
cp --sparse=always "$work/a.big.img" "$keep"
echo "kept the sparse image at $keep ($(sha256sum "$keep" | cut -d' ' -f1))"
echo "wrote $out/apfs-bigcib.expect.json"

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
# Gates, all of which fail the script: the free space at the scratch location is at
# least one image plus 30 GiB (exit 2, with the figures, before anything is written);
# apfsck accepts the image (read-only, the image does not change); the independent
# decoder apfs_oracle.py passes every cross-check. The image is built once, in place,
# as <keep-image-path>.partial and renamed only when complete (a failed run leaves no
# image), so the peak is ONE image. Determinism is checked ACROSS runs: build, record
# the sha256, delete, build again at the same path and compare (tools/fixtures/README.md).
set -euo pipefail
umask 022
if [ "$(id -u)" != 0 ]; then
  echo "apfs-big.sh must run as root (the container does)" >&2
  exit 1
fi
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
root=$(cd "$here/../.." && pwd -P)
# shellcheck source=lib.sh
. "$here/lib.sh"

: "${APFS_BIG_WORK:?set APFS_BIG_WORK to a bind-mounted host directory outside the repository (never the container filesystem)}"

# resolve_path prints the physical (symlink-free, absolute) form of a path that may not
# exist yet: the deepest existing ancestor is resolved with pwd -P and the rest appended.
# It creates nothing, so the repository check below runs before any mkdir.
resolve_path() {
  local p=$1 tail=""
  case "$p" in /*) ;; *) p=$PWD/$p ;; esac
  while [ ! -d "$p" ]; do
    tail=/$(basename "$p")$tail
    p=$(dirname "$p")
  done
  printf '%s%s\n' "$(cd "$p" && pwd -P)" "$tail"
}
inside_repo() {
  case "$1/" in "$root"/*) return 0 ;; esac
  return 1
}
refuse_repo() {
  echo "refusing to write a large image inside the repository ($root): choose a scratch directory outside it" >&2
  exit 2
}

size=${APFS_BIG_SIZE:-17G} # 17 GiB = 136 chunks of 128 MiB: more than the 126 one CIB holds
work=$(resolve_path "$APFS_BIG_WORK")/minutiae-apfs-big
inside_repo "$work" && refuse_repo

keep=""
out=""
if [ $# -ge 1 ]; then
  [ $# -eq 2 ] || { echo "usage: apfs-big.sh [<outdir> <keep-image-path>]" >&2; exit 2; }
  keep=$(resolve_path "$(dirname "$2")")/$(basename "$2")
  inside_repo "$keep" && refuse_repo
  [ -d "$1" ] || { echo "the output directory $1 does not exist" >&2; exit 2; }
  out=$(cd "$1" && pwd -P)
fi

# Disk guard (the run's peak is ONE image, because the keep image is built in place and
# never copied or built twice at once): refuse unless the free space at the directory the
# image lands in is at least the image size plus 30 GiB of headroom.
need_kib=$(( $(numfmt --from=iec "$size") / 1024 + 30 * 1024 * 1024 ))
guard() {
  local d=$1 avail
  while [ ! -d "$d" ]; do d=$(dirname "$d"); done
  avail=$(df -Pk "$d" | awk 'NR==2 {print $4}')
  if [ "$avail" -lt "$need_kib" ]; then
    echo "not enough free space at $d: $avail KiB available, the run needs $need_kib KiB (a $size image plus 30 GiB headroom)" >&2
    exit 2
  fi
  echo "free space at $d: $avail KiB (the run needs $need_kib KiB)"
}
if [ -n "$keep" ]; then guard "$(dirname "$keep")"; else guard "$APFS_BIG_WORK"; fi

mkdir -p "$(dirname "$work")"
label=bigcib
cu=0b9c5d7e-3a41-4f28-9e6d-c2a8714f50b3
vu=e47a1f90-d5c2-4b68-83a7-6f0b29d1c845
clock="2023-11-14 22:13:20"
export LC_ALL=C.UTF-8 NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1

# A failed run never leaves a partial image: the keep image is written as <keep>.partial
# and renamed into place only when every gate passed.
trap 'rm -rf "$work"; [ -z "$keep" ] || rm -f "$keep.partial"' EXIT
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

# The image is built where it will stay: at <keep>.partial in a keep run, so no second
# copy of it ever exists.
img="$work/a.big.img"
[ -z "$keep" ] || img="$keep.partial"
time make_image "$img"
before=$(sha256sum "$img" | cut -d' ' -f1)
CMDS+=("apfsck <img>   # exit status 0 = structurally valid")
apfsck "$img" >"$work/apfsck.txt" 2>&1 || { cat "$work/apfsck.txt"; echo "apfsck rejected the $size image" >&2; exit 1; }
after=$(sha256sum "$img" | cut -d' ' -f1)
[ "$before" = "$after" ] || { echo "apfsck changed the image" >&2; exit 1; }
CMDS+=("python3 tools/fixtures/apfs_oracle.py <img> $label 0 0 $cu $vu")
time python3 "$here/apfs_oracle.py" "$img" "$label" 0 0 "$cu" "$vu" >"$work/oracle.json"
jq '.spaceman | with_entries(select(.key | test("cib|cab|chunk")))' "$work/oracle.json"
jq '{free_blocks, chunks: (.chunks | length), free_ranges: (.free_ranges | length)}' "$work/oracle.json"
ls -ls "$img" "$work/oracle.json"

[ -n "$keep" ] || exit 0
gen=$(generator_json "$img" apfsprogs util-linux python3 coreutils jq faketime |
  jq --arg note "empty $size container made by mkapfs: more chunks than one CIB holds. The image is sparse and is not committed; MINUTIAE_APFS_BIG_IMAGE names a regenerated copy (apfs-big.sh <outdir> <keep-image-path>, outside the repo)." '. + {note: $note}')
jq -n --argjson g "$gen" --slurpfile o "$work/oracle.json" '$o[0] + {generator: $g}' >"$out/apfs-bigcib.expect.json"
mv "$img" "$keep"
echo "kept the sparse image at $keep ($(sha256sum "$keep" | cut -d' ' -f1))"
echo "wrote $out/apfs-bigcib.expect.json"

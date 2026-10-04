#!/bin/bash
# APFS fixtures: EMPTY containers made by the real mkapfs (apfsprogs) on a
# regular file. Nothing on Linux can populate an APFS volume, so these hold the
# container and one volume with its two directories (root, private-dir) and
# nothing else: no files, no extra directory records, no xattrs, no snapshots,
# no encrypted volumes (see README.md, "APFS determinism and limits").
# Usage: apfs.sh <outdir>   (normally called by gen.sh)
#
#   apfs-ci          64 MiB, mkapfs defaults (case- and normalization-
#                    insensitive), label "fixture"
#   apfs-cs          64 MiB, mkapfs -s -z (case- and normalization-sensitive),
#                    label "fixture-cs", other container and volume UUIDs
#   apfs-multichunk  129 MiB (two space-manager chunks: the second one is a
#                    256-block remainder that mkapfs gives no bitmap block),
#                    defaults, label "multichunk". The Go tests skip it
#                    under -short.
#
# Oracle: apfs_oracle.py, an independent decoder written from Apple's APFS
# reference (never Minutiae). It decodes every object it can reach, verifies
# every checksum and cross-checks the result against the mkapfs arguments, the
# free-space counters and the bitmaps; a failed check fails this script. apfsck
# must also accept every image (its exit status is the verdict).
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "apfs.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

out=$(cd "${1:?outdir required}" && pwd)

# Reproducible images: mkapfs stamps the volume's creation time, the root and
# private-dir inode times and the directory records' date_added from the clock,
# so it runs under a frozen faketime clock (real time still ticks for monotonic
# timers). UUIDs and the label are arguments. mkapfs writes only the blocks it
# needs and never zeroes the rest of the device, so every image starts from a
# brand-new sparse file (reusing a file would keep stale blocks of an earlier
# image).
export LC_ALL=C.UTF-8
export NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20"

# frun <cmd...>: run under the frozen clock and record the command.
frun() {
  CMDS+=("faketime -f '$clock' $*")
  faketime -f "$clock" "$@"
}

# The images live outside /dev/shm: Docker caps it at 64 MiB.
imgs=${TMPDIR:-/tmp}/minutiae-apfs-images
work=${TMPDIR:-/tmp}/minutiae-apfs-work # fixed name: the commands recorded in the oracle mention it
cleanup() { rm -rf "$imgs" "$work"; }
trap cleanup EXIT
rm -rf "$imgs" "$work"
mkdir "$imgs" "$work"

# make_image <img> <size> <label> <container uuid> <volume uuid> <mkapfs flags...>
make_image() {
  local img=$1 size=$2 label=$3 cu=$4 vu=$5
  shift 5
  rm -f "$img"
  run truncate -s "$size" "$img"
  frun mkapfs "$@" -L "$label" -U "$cu" -u "$vu" "$img"
}

# build_fixture <name> <size> <label> <container uuid> <volume uuid> <case_sensitive 0|1> <norm_sensitive 0|1>
# (-s is passed for case_sensitive=1, -z for norm_sensitive=1)
build_fixture() {
  local name=$1 size=$2 label=$3 cu=$4 vu=$5 cs=$6 ns=$7
  local img="$imgs/$name.img" twin="$imgs/$name.twin.img" flags=()
  [ "$cs" = 1 ] && flags+=(-s)
  [ "$ns" = 1 ] && flags+=(-z)
  CMDS=()

  make_image "$img" "$size" "$label" "$cu" "$vu" "${flags[@]}"

  # Gate: a second image from the same inputs must be byte-identical (the
  # clock is frozen and every id is fixed); this catches any new source of
  # nondeterminism at generation time.
  local saved=("${CMDS[@]}")
  make_image "$twin" "$size" "$label" "$cu" "$vu" "${flags[@]}"
  CMDS=("${saved[@]}")
  cmp "$img" "$twin" || { echo "mkapfs output for $name is not deterministic" >&2; exit 1; }
  rm -f "$twin"

  # Gate: the structural checker accepts the image, read-only.
  local before after
  before=$(sha256sum "$img")
  if ! apfsck "$img" >"$work/$name.apfsck.txt" 2>&1; then
    cat "$work/$name.apfsck.txt"
    echo "apfsck rejected $name" >&2
    exit 1
  fi
  after=$(sha256sum "$img")
  [ "$before" = "$after" ] || { echo "apfsck changed $name.img" >&2; exit 1; }
  CMDS+=("apfsck $img   # exit status 0 = structurally valid")

  # The independent decoder; it fails (non-zero exit) on any cross-check.
  CMDS+=("python3 tools/fixtures/apfs_oracle.py <img> $label $cs $ns $cu $vu")
  python3 "$here/apfs_oracle.py" "$img" "$label" "$cs" "$ns" "$cu" "$vu" >"$work/$name.oracle.json"

  local gen
  gen=$(generator_json "$img" apfsprogs util-linux python3 coreutils jq gzip faketime |
    jq --arg note "empty container only: one volume holding the root directory and private-dir; no files, drecs, xattrs, snapshots or encryption (mkapfs cannot create them)" '. + {note: $note}')
  jq -n --argjson g "$gen" --slurpfile o "$work/$name.oracle.json" '$o[0] + {generator: $g}' >"$out/$name.expect.json"
  gzip -n -9 -c "$img" >"$out/$name.img.gz"
  echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"
}

build_fixture apfs-ci 64M fixture 7a1f3c52-9b84-4d6e-a0c3-5e2f81b7d409 c4e6a892-13d5-4b7f-8e20-9a6c3f1d5b78 0 0
build_fixture apfs-cs 64M fixture-cs 2d9e4b71-c3a8-4f05-b6d2-81e7a09c5f34 e8b05d3a-6f12-4c97-a4e1-70c2b9d8f356 1 1
build_fixture apfs-multichunk 129M multichunk 5b3c8e07-d142-4a96-9f7b-c0e4a2d61895 91a7f4c8-2e5b-4d03-b86a-3f9d17e0c24b 0 0

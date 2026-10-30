#!/bin/bash
# Carving fixtures (3E): real-encoder objects and three ground-truth images.
# Usage: carve_gen.sh <imagedir> [<objectsdir>]   (normally called by gen.sh carve)
#
# Runs in the pinned minutiae-carve-fixtures image (Dockerfile.carve), never in minutiae-fixtures. It needs no
# QEMU, no guest kernel and no privileges: carving reads free bytes and never consults deleted metadata, so how
# a deletion happened does not matter (mtools mdel and debugfs rm free clusters and blocks without erasing them).
#
#   carve_objects.py  encodes one object per family and variant with real encoders into <objectsdir>
#                     (default internal/carve/testdata/objects) and writes objects.expect.json
#   carve_place.py    builds carve-fat12, carve-ext4 and carve-raw (.img.gz) and carve.expect.json in <imagedir>
#
# Oracle: the tools (mshowfat, debugfs blocks, fsck.fat, dumpe2fs, Pillow, ffprobe, heif-info, sqlite3, zipfile,
# wave, plistlib, gzip), never Minutiae. The whole run uses a frozen clock so that the finished bytes are
# reproducible.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "carve_gen.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)

out=$(cd "${1:?image directory required}" && pwd)
objs=${2:-$root/internal/carve/testdata/objects}
mkdir -p "$objs"
objs=$(cd "$objs" && pwd)

export LC_ALL=C.UTF-8 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20" # an absolute date is a frozen clock

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-carve # fixed name: the commands recorded in the oracles are scrubbed of it
rm -rf "$work"
mkdir "$work"
trap 'rm -rf "$work"' EXIT

# the fixed clock covers the Pillow PDF writer (it stamps the creation date) and every encoder
faketime -f "$clock" python3 "$here/carve_objects.py" "$objs" "$work/objects"
python3 "$here/carve_place.py" "$objs" "$out" "$work/place"

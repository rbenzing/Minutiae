#!/bin/bash
# Image-format carving fixtures (3E2): real-encoder objects and one ground-truth image.
# Usage: carve_images_gen.sh <imagedir> [<objectsdir>]   (normally called by gen.sh carve-images)
#
# Runs in the pinned minutiae-carve-fixtures image (Dockerfile.carve), no QEMU, no privileges, no network.
#
#   carve_images_objects.py  encodes the BMP, TIFF, ICO/CUR, AVIF and JPEG XL objects into <objectsdir>
#                            (default internal/carve/testdata/objects) and writes images.expect.json
#   carve_images_place.py    builds carve-images.img.gz and carve-images.expect.json in <imagedir>
#
# It reads jpeg-baseline.jpg of the 3E objects (the JPEG XL transcode input) and writes none of the 3E files, so
# `gen.sh carve` and `gen.sh carve-images` can be run in either order. The oracle is the encoders' own info tools
# (identify, tiffinfo, tiffdump, icotool, avifdec, jxlinfo, Pillow), never Minutiae. The run uses a frozen clock.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "carve_images_gen.sh must run as root (the container does)" >&2
  exit 1
fi

for t in cjxl avifenc icotool tiffcp convert; do
  command -v "$t" >/dev/null || { echo "carve_images_gen.sh needs $t: rebuild the image (docker build -f tools/fixtures/Dockerfile.carve -t minutiae-carve-fixtures tools/fixtures)" >&2; exit 1; }
done

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
work=$base/minutiae-carve-images # fixed name: the commands recorded in the oracle are scrubbed of it
rm -rf "$work"
mkdir "$work"
trap 'rm -rf "$work"' EXIT

faketime -f "$clock" python3 "$here/carve_images_objects.py" "$objs" "$work/objects"
python3 "$here/carve_images_place.py" "$objs" "$out"

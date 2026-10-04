#!/bin/bash
# Entry point: gen.sh <fixture>|all
# Run from the repository root inside the minutiae-fixtures container (see
# README.md). Output goes to the owning package's testdata/ directory.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)

declare -A fixtures=(
  [volume-gpt]="internal/volume/testdata"
  [volume-mbr]="internal/volume/testdata"
  [ext4]="internal/filesys/ext4/testdata"
  [fat]="internal/filesys/fat/testdata"
  [exfat]="internal/filesys/exfat/testdata"
  [f2fs]="internal/filesys/f2fs/testdata"
  [apfs]="internal/filesys/apfs/testdata"
  [apfs-populated]="internal/filesys/apfs/testdata"
)
order=(volume-gpt volume-mbr ext4 fat exfat f2fs apfs)
# Not in "all": needs the other image (Dockerfile.apfs, minutiae-fixtures-apfs).
separate=(apfs-populated)

usage() {
  echo "usage: gen.sh <fixture>|all" >&2
  echo "fixtures: ${order[*]} (all), ${separate[*]} (not in all: run it in the minutiae-fixtures-apfs image)" >&2
  exit 2
}

[ $# -eq 1 ] || usage

if [ "$1" = all ]; then
  targets=("${order[@]}")
else
  [ -n "${fixtures[$1]:-}" ] || usage
  targets=("$1")
fi

if [ "${targets[0]}" = apfs-populated ] && [ ! -f /usr/local/lib/apfs/apfs.ko ]; then
  echo "gen.sh apfs-populated needs the linux-apfs-rw module and QEMU, which the minutiae-fixtures image does not have." >&2
  echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.apfs -t minutiae-fixtures-apfs tools/fixtures" >&2
  echo "(see tools/fixtures/README.md, \"APFS populated fixture\")." >&2
  exit 1
fi

for name in "${targets[@]}"; do
  out="$root/${fixtures[$name]}"
  mkdir -p "$out"
  echo "== $name -> ${fixtures[$name]}"
  bash "$here/$name.sh" "$out"
done

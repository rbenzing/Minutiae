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
  [ewf]="internal/image/ewf/testdata"
  [apfs]="internal/filesys/apfs/testdata"
  [apfs-populated]="internal/filesys/apfs/testdata"
  [fat-recover]="internal/filesys/fat/testdata/recover"
  [exfat-recover]="internal/filesys/exfat/testdata/recover"
  [hfsplus]="internal/filesys/hfsplus/testdata"
  [sqlite]="internal/sqlitefile/testdata"
  # casefold.sh derives the ext4 and F2FS testdata directories from $root: it writes all three outputs
  [casefold]="internal/filesys/utf8fold/testdata"
  # ext4_xattr.sh: runs in the Dockerfile.recover image (pinned e2fsprogs) and writes only ext4-xattr.*
  [ext4-xattr]="internal/filesys/ext4/testdata"
)
order=(volume-gpt volume-mbr ext4 fat exfat f2fs ewf apfs sqlite)
# Not in "all": each needs its own image (hfsplus: Dockerfile.hfs, minutiae-fixtures-hfs;
# apfs-populated: Dockerfile.apfs, minutiae-fixtures-apfs; fat-recover, exfat-recover and casefold:
# Dockerfile.recover, minutiae-recover-fixtures, which holds QEMU and the guest kernel).
separate=(hfsplus apfs-populated fat-recover exfat-recover casefold ext4-xattr)

usage() {
  echo "usage: gen.sh <fixture>|all" >&2
  echo "fixtures: ${order[*]} (all), ${separate[*]} (not in all: run hfsplus in the minutiae-fixtures-hfs image, apfs-populated in the minutiae-fixtures-apfs image, fat-recover, exfat-recover and ext4-xattr in the minutiae-recover-fixtures image)" >&2
  exit 2
}

[ $# -eq 1 ] || usage

if [ "$1" = all ]; then
  targets=("${order[@]}")
else
  [ -n "${fixtures[$1]:-}" ] || usage
  targets=("$1")
fi

# Every target is guarded, not only the first: a fixture that needs another image stops here
# with the name of the image to build.
for name in "${targets[@]}"; do
  case $name in
  hfsplus)
    if ! command -v mkfs.hfsplus >/dev/null; then
      echo "gen.sh hfsplus needs mkfs.hfsplus (hfsprogs), which the minutiae-fixtures image does not have." >&2
      echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.hfs -t minutiae-fixtures-hfs tools/fixtures" >&2
      echo "(see tools/fixtures/README.md, \"HFS+ fixtures\")." >&2
      exit 1
    fi
    ;;
  apfs-populated)
    if [ ! -f /usr/local/lib/apfs/apfs.ko ]; then
      echo "gen.sh apfs-populated needs the linux-apfs-rw module and QEMU, which the minutiae-fixtures image does not have." >&2
      echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.apfs -t minutiae-fixtures-apfs tools/fixtures" >&2
      echo "(see tools/fixtures/README.md, \"APFS populated fixture\")." >&2
      exit 1
    fi
    ;;
  fat-recover | exfat-recover | ext4-xattr)
    if ! command -v qemu-system-x86_64 >/dev/null || ! compgen -G "/boot/vmlinuz-*" >/dev/null; then
      echo "gen.sh $name needs QEMU and the guest kernel, which the minutiae-fixtures image does not have." >&2
      echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures" >&2
      echo "(see tools/fixtures/README.md, \"FAT recovery fixtures (3B)\")." >&2
      exit 1
    fi
    ;;
  esac
done

if [ "${targets[0]}" = casefold ] && { ! command -v qemu-system-x86_64 >/dev/null || ! compgen -G '/boot/vmlinuz-*' >/dev/null; }; then
  echo "gen.sh casefold needs qemu-system-x86_64 and /boot/vmlinuz-*, which the minutiae-fixtures image does not have." >&2
  echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures" >&2
  echo "(see tools/fixtures/README.md, \"Casefold lookup oracle\")." >&2
  exit 1
fi

for name in "${targets[@]}"; do
  out="$root/${fixtures[$name]}"
  mkdir -p "$out"
  echo "== $name -> ${fixtures[$name]}"
  script=$name
  case $name in fat-recover | exfat-recover | ext4-xattr) script=${name//-/_} ;; esac # fat_recover.sh, exfat_recover.sh, ext4_xattr.sh
  bash "$here/$script.sh" "$out"
done

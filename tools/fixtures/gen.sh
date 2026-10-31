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
  # ext4_recover.sh: runs in the Dockerfile.recover510 image (the 5.10 guest kernel next to the 6.1 one) and writes
  # ext4-recover, ext3-recover, ext4-recover-sameboot and ext4-recover-wiped
  [ext4-recover]="internal/filesys/ext4/testdata/recover"
  [f2fs-recover]="internal/filesys/f2fs/testdata/recover"
  [hfsplus]="internal/filesys/hfsplus/testdata"
  [sqlite]="internal/sqlitefile/testdata"
  # casefold.sh derives the ext4 and F2FS testdata directories from $root: it writes all three outputs
  [casefold]="internal/filesys/utf8fold/testdata"
  # ext4_xattr.sh: runs in the Dockerfile.recover image (pinned e2fsprogs) and writes only ext4-xattr.*
  [ext4-xattr]="internal/filesys/ext4/testdata"
  # carve_gen.sh: runs in the Dockerfile.carve image; the images go here and the objects to internal/carve/testdata/objects
  [carve]="internal/examine/testdata/carve"
  # carve_images_gen.sh: the same image; objects go to internal/carve/testdata/objects, the raw image here
  [carve-images]="internal/examine/testdata/carve"
  # slack.sh: runs in the Dockerfile.recover image and writes four directories: the FAT one here, the exFAT, ext4 and F2FS ones
  # derived from $root, all named testdata/slack
  [slack]="internal/filesys/fat/testdata/slack"
)
order=(volume-gpt volume-mbr ext4 fat exfat f2fs ewf apfs sqlite)
# Not in "all": each needs its own image (hfsplus: Dockerfile.hfs, minutiae-fixtures-hfs;
# apfs-populated: Dockerfile.apfs, minutiae-fixtures-apfs; fat-recover, exfat-recover, f2fs-recover and casefold:
# Dockerfile.recover, minutiae-recover-fixtures, which holds QEMU and the guest kernel; carve: Dockerfile.carve,
# minutiae-carve-fixtures, which holds the encoders and no QEMU).
separate=(hfsplus apfs-populated fat-recover exfat-recover f2fs-recover ext4-recover casefold ext4-xattr carve carve-images slack)

usage() {
  echo "usage: gen.sh <fixture>|all" >&2
  echo "fixtures: ${order[*]} (all), ${separate[*]} (not in all: run hfsplus in the minutiae-fixtures-hfs image, apfs-populated in the minutiae-fixtures-apfs image, fat-recover, exfat-recover, f2fs-recover, ext4-xattr and slack in the minutiae-recover-fixtures image, ext4-recover in the minutiae-recover-fixtures-510 image, carve in the minutiae-carve-fixtures image)" >&2
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
  fat-recover | exfat-recover | f2fs-recover | ext4-xattr | slack)
    if ! command -v qemu-system-x86_64 >/dev/null || ! compgen -G "/boot/vmlinuz-*" >/dev/null; then
      echo "gen.sh $name needs QEMU and the guest kernel, which the minutiae-fixtures image does not have." >&2
      echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures" >&2
      echo "(see tools/fixtures/README.md, \"FAT recovery fixtures (3B)\" and \"Slack fixtures (3F)\")." >&2
      exit 1
    fi
    ;;
  ext4-recover)
    if ! command -v qemu-system-x86_64 >/dev/null || ! compgen -G "/boot/vmlinuz-5.10*" >/dev/null || ! compgen -G "/boot/vmlinuz-6.1*" >/dev/null; then
      echo "gen.sh ext4-recover needs QEMU and BOTH guest kernels (5.10 for the positive images, 6.1 for the wiped-record image)," >&2
      echo "which the minutiae-fixtures image does not have." >&2
      echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures &&" >&2
      echo "  docker build -f tools/fixtures/Dockerfile.recover510 -t minutiae-recover-fixtures-510 tools/fixtures" >&2
      echo "(see tools/fixtures/README.md, \"ext4 recovery fixtures (3C)\")." >&2
      exit 1
    fi
    ;;
  carve | carve-images)
    if ! command -v heif-enc >/dev/null || ! command -v ffmpeg >/dev/null; then
      echo "gen.sh carve needs heif-enc (libheif-examples) and ffmpeg, which the minutiae-fixtures image does not have." >&2
      echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.carve -t minutiae-carve-fixtures tools/fixtures" >&2
      echo "(see tools/fixtures/README.md, \"Carving fixtures (3E)\")." >&2
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
  case $name in fat-recover | exfat-recover | f2fs-recover | ext4-xattr | ext4-recover) script=${name//-/_} ;; esac # fat_recover.sh, exfat_recover.sh, f2fs_recover.sh, ext4_xattr.sh, ext4_recover.sh
  case $name in carve) script=carve_gen ;; esac # carve_gen.sh
  case $name in carve-images) script=carve_images_gen ;; esac # carve_images_gen.sh
  bash "$here/$script.sh" "$out"
done

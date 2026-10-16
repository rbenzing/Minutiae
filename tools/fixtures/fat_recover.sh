#!/bin/bash
# FAT12, FAT16 and FAT32 RECOVERY fixtures of plan 3B: volumes written and deleted by
# the KERNEL vfat driver inside the QEMU TCG guest (recover_guest.sh), with an
# independent oracle (fat_recover_oracle.py). No mtools writes, no mdel, no kernel
# module on the host (ruling F3): the host only formats (mkfs.fat under faketime)
# and READS finished images (mshowfat, fsck.fat -n, the oracle).
# Usage: fat_recover.sh <outdir>   (normally: gen.sh fat-recover, in the
# minutiae-recover-fixtures image; see README.md, "FAT recovery fixtures (3B)")
#
# Placement is never trusted (rulings F19, F22): every scenario family has its own
# directory; the volume is FULL (fill.bin written until ENOSPC) before every reuse
# step, so the free set is exactly the freed file's clusters and the write has
# nowhere else to go; writes happen only in writer directories created first; the
# deleting phase has only rm. The oracle derives every outcome from the finished
# image and this script exits 1 naming a missing family.
#
# Boots (one guest boot each, a fresh mount each, on the previous boot's image):
#   1  populate: every directory, then the family files
#   2  fill the volume, then the hole steps of frag-live and frag-dead
#   3  phase W: one family at a time, rm of the target, then the writes
#   4  phase D: only rm
# The host reads the image after boots 1, 2 and 3 (the true chains of everything
# deleted later) and after boot 4 (the finished image).
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "fat_recover.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

out=$(cd "${1:?outdir required}" && pwd)
export MTOOLS_SKIP_CHECK=1 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20"
order=ascending # the allocation-order row of ruling F22 (probe facts P2, P6)
max_gz=153600   # about 150 KiB (ruling F6)

work=$(mktemp -d /tmp/fat-recover.XXXXXX)
trap 'rm -rf "$work"' EXIT
src=$work/src
mkdir "$src" "$work/final"

# mk <path> <clusters>: a source file of clusters*512-7 bytes (the last cluster is partial)
mk() {
  mkdir -p "$(dirname "$src/$1")"
  pattern $(($2 * 512 - 7)) "$1" >"$src/$1"
}
mk /contiguous/target.bin 5
mkdir -p "$src/zero-size"
: >"$src/zero-size/target.dat"
mk /reuse-all/target.bin 6
mk /reuse-all-w/took.bin 6
mk /reuse-head/target.bin 6
mk /reuse-head-w/took.bin 2
mk /reuse-head-w/rest.bin 4
mk /cut-l/target.bin 6
mk /cut-l-w/filler.bin 4
mk /cut-l-w/tail.bin 2
mk /cut-h/target.bin 6
mk /cut-h-hid/filler.bin 4
mk /cut-h-w/tail.bin 2
mk /frag-live/a.bin 2
mk /frag-live/b.bin 3
mk /frag-live/c.bin 4
mk /frag-live/f.bin 6
mk /frag-dead/a.bin 2
mk /frag-dead/D.BIN 3
mk /frag-dead/c.bin 4
mk /frag-dead/f.bin 6
mk /lfn/a-long-deleted-name-for-slot-reuse.bin 3
: >"$src/lfn/NEW.TXT"
mk /gone/inner.bin 2

# The ops of each boot, one shell command per line (run in order inside the guest,
# the volume on /mnt, the source tree on /payload).
cat >"$work/ops1" <<'EOF'
mkdir /mnt/contiguous /mnt/zero-size /mnt/reuse-all /mnt/reuse-head /mnt/cut-l /mnt/cut-h /mnt/frag-live /mnt/frag-dead /mnt/lfn /mnt/gone
mkdir /mnt/reuse-all-w /mnt/reuse-head-w /mnt/cut-l-w /mnt/cut-h-w /mnt/cut-h-hid
cp /payload/contiguous/target.bin /mnt/contiguous/target.bin
cp /payload/zero-size/target.dat /mnt/zero-size/target.dat
cp /payload/reuse-all/target.bin /mnt/reuse-all/target.bin
cp /payload/reuse-head/target.bin /mnt/reuse-head/target.bin
cp /payload/cut-l/target.bin /mnt/cut-l/target.bin
cp /payload/cut-h/target.bin /mnt/cut-h/target.bin
cp /payload/frag-live/a.bin /mnt/frag-live/a.bin
cp /payload/frag-live/b.bin /mnt/frag-live/b.bin
cp /payload/frag-live/c.bin /mnt/frag-live/c.bin
cp /payload/frag-dead/a.bin /mnt/frag-dead/a.bin
cp /payload/frag-dead/D.BIN /mnt/frag-dead/D.BIN
cp /payload/frag-dead/c.bin /mnt/frag-dead/c.bin
cp /payload/lfn/a-long-deleted-name-for-slot-reuse.bin /mnt/lfn/a-long-deleted-name-for-slot-reuse.bin
cp /payload/gone/inner.bin /mnt/gone/inner.bin
EOF
cat >"$work/ops2" <<'EOF'
dd if=/dev/zero of=/mnt/fill.bin bs=512 2>/dev/null || true
sync
rm /mnt/frag-live/a.bin /mnt/frag-live/c.bin
cp /payload/frag-live/f.bin /mnt/frag-live/f.bin
rm /mnt/frag-dead/a.bin /mnt/frag-dead/c.bin
cp /payload/frag-dead/f.bin /mnt/frag-dead/f.bin
EOF
# Phase W, the "ascending" row of ruling F22: with the volume full the free set is the
# target's clusters, and the writes take them in ascending order, so the first write
# takes the head and the last the tail. The oracle checks nothing of this by trust: it
# reads the outcomes from the finished image.
cat >"$work/ops3" <<'EOF'
rm /mnt/reuse-all/target.bin
cp /payload/reuse-all-w/took.bin /mnt/reuse-all-w/took.bin
rm /mnt/reuse-head/target.bin
cp /payload/reuse-head-w/took.bin /mnt/reuse-head-w/took.bin
cp /payload/reuse-head-w/rest.bin /mnt/reuse-head-w/rest.bin
rm /mnt/cut-l/target.bin
cp /payload/cut-l-w/filler.bin /mnt/cut-l-w/filler.bin
cp /payload/cut-l-w/tail.bin /mnt/cut-l-w/tail.bin
rm /mnt/cut-h/target.bin
cp /payload/cut-h-hid/filler.bin /mnt/cut-h-hid/filler.bin
cp /payload/cut-h-w/tail.bin /mnt/cut-h-w/tail.bin
rm /mnt/lfn/a-long-deleted-name-for-slot-reuse.bin
: > /mnt/lfn/NEW.TXT
EOF
cat >"$work/ops4" <<'EOF'
rm /mnt/contiguous/target.bin /mnt/zero-size/target.dat
rm /mnt/cut-l-w/filler.bin
rm -r /mnt/cut-h-hid
rm /mnt/frag-live/f.bin
rm /mnt/frag-dead/D.BIN /mnt/frag-dead/f.bin
rm -r /mnt/gone
EOF

# stage <image> <name> <boot number>: one guest boot with that boot's ops, then the image is read
stage() {
  local img=$1 name=$2 n=$3 line
  guest_initrd "$work/rd" vfat utf8 "$src" "$work/ops$n"
  guest_run "$img" "$work/rd" "$work/guest-$name-$n.log"
  while IFS= read -r line; do CMDS+=("guest boot $n: $line"); done <"$work/ops$n"
}

# build_fixture <name> <fat type> <size in KiB> [mkfs.fat extra args]
build_fixture() {
  local name=$1 type=$2 kb=$3 img="$work/$1.img" gen
  shift 3
  CMDS=()
  CMDS+=("# source tree: mk calls of tools/fixtures/fat_recover.sh (patterns from lib.sh); guest: vfat -o utf8; allocation-order row: $order")
  CMDS+=("faketime -f '$clock' mkfs.fat -F $type -i 1234ABCD -s 1 -n FIXTURE $* -C <img> $kb")
  faketime -f "$clock" mkfs.fat -F "$type" -i 1234ABCD -s 1 -n FIXTURE "$@" -C "$img" "$kb" >/dev/null

  stage "$img" "$name" 1
  fsck.fat -n "$img" >"$work/$name.fsck1.log" 2>&1 || { cat "$work/$name.fsck1.log"; echo "fsck.fat not clean after boot 1" >&2; exit 1; }
  python3 "$here/fat_recover_oracle.py" snap "$img" "$src" "$work/$name.snap1.json" any

  stage "$img" "$name" 2
  fsck.fat -n "$img" >"$work/$name.fsck2.log" 2>&1 || { cat "$work/$name.fsck2.log"; echo "fsck.fat not clean after boot 2" >&2; exit 1; }
  python3 "$here/fat_recover_oracle.py" snap "$img" "$src" "$work/$name.snap2.json" 0 # the volume is full

  stage "$img" "$name" 3
  fsck.fat -n "$img" >"$work/$name.fsck3.log" 2>&1 || { cat "$work/$name.fsck3.log"; echo "fsck.fat not clean after boot 3" >&2; exit 1; }
  python3 "$here/fat_recover_oracle.py" snap "$img" "$src" "$work/$name.snap3.json" 3 # only the lfn file's 3 clusters are free

  stage "$img" "$name" 4
  CMDS+=("fsck.fat -n <img>   # must report a clean filesystem")
  fsck.fat -n "$img" >"$work/$name.fsck.log" 2>&1 || { cat "$work/$name.fsck.log"; echo "final fsck.fat not clean" >&2; exit 1; }
  CMDS+=("fsck.fat -v -n <img>   # geometry, clusters in use")
  fsck.fat -v -n "$img" >"$work/$name.fsckv.log" 2>&1
  CMDS+=("python3 tools/fixtures/fat_recover_oracle.py final <img> <src> <fsck -v output> <snapshots after boots 1-3>   # mshowfat chains, free clusters, outcomes")
  python3 "$here/fat_recover_oracle.py" final "$img" "$src" "$work/$name.fsckv.log" \
    "$work/$name.snap1.json" "$work/$name.snap2.json" "$work/$name.snap3.json" >"$work/$name.oracle.json"

  gen=$(generator_json "$img" dosfstools mtools python3 coreutils jq gzip faketime qemu-system-x86 linux-image-6.1.0-53-amd64 busybox-static kmod cpio)
  jq -n --argjson g "$gen" --slurpfile o "$work/$name.oracle.json" '{generator: $g} + $o[0]' >"$work/final/$name.expect.json"
  gzip -n -9 -c "$img" >"$work/final/$name.img.gz"
  echo "$name: $(stat -c %s "$work/final/$name.img.gz") bytes gzipped"
}

build_fixture fat12-recover 12 1440
build_fixture fat16-recover 16 8192
# FAT32 at its minimum size (65525 clusters of 512 bytes) with one FAT copy: the FAT of a full volume is nearly incompressible,
# and a second copy or a larger volume would push the image over the F6 budget
build_fixture fat32-recover 32 33040 -f 1

# the required outcome families must occur across the three images together (exit 1 naming one that does not)
python3 "$here/fat_recover_oracle.py" families "$work"/final/*.expect.json

for f in "$work"/final/*.img.gz; do
  size=$(stat -c %s "$f")
  if [ "$size" -gt "$max_gz" ]; then
    echo "$(basename "$f") is $size bytes, over the $max_gz budget (ruling F6)" >&2
    exit 1
  fi
done

cp "$work"/final/* "$out/"
for f in fat12-recover fat16-recover fat32-recover; do
  echo "wrote $out/$f.img.gz and $f.expect.json"
done

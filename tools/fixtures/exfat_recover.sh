#!/bin/bash
# exFAT RECOVERY fixture of plan 3B: a volume written and deleted by the KERNEL exfat
# driver inside the QEMU TCG guest (recover_guest.sh), with an independent oracle
# (exfat_recover_oracle.py). No exfat-fuse, no loop device, no --privileged and no
# kernel module on the host (ruling F3): the host only formats (mkfs.exfat under
# faketime) and READS finished images (dump.exfat, fsck.exfat -n, the oracle).
# Usage: exfat_recover.sh <outdir>   (normally: gen.sh exfat-recover, in the
# minutiae-recover-fixtures image; see README.md, "exFAT recovery fixture (3B)")
#
# The method is that of fat_recover.sh (rulings F19, F22): one directory per family,
# the volume FULL (fill.bin until ENOSPC) before every reuse step, writes only in
# writer directories made first, the writing phase W before the deleting phase D, and
# four guest boots, each a fresh mount, with host snapshots after boots 1-3 that give
# the true chains of files deleted later. The oracle derives every outcome from the
# finished image and this script exits 1 naming a missing family. It also exits 1
# when it wrote no image (ruling F102): gen.sh never reports success for nothing.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "exfat_recover.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

out=$(cd "${1:?outdir required}" && pwd)
clock="2023-11-14 22:13:20"
order=ascending # the allocation-order row of ruling F22 (probe facts P2, P4, P6)
max_gz=153600   # about 150 KiB (ruling F6)
name=exfat-recover

work=$(mktemp -d /tmp/exfat-recover.XXXXXX)
trap 'rm -rf "$work"' EXIT
src=$work/src
mkdir "$src" "$work/final"

# mk <path> <clusters>: a source file of clusters*4096-7 bytes (the last cluster is partial)
mk() {
  mkdir -p "$(dirname "$src/$1")"
  pattern $(($2 * 4096 - 7)) "$1" >"$src/$1"
}
mk /contiguous/target.bin 5
mkdir -p "$src/zero-size"
: >"$src/zero-size/target.dat"
mk /reuse-head/target.bin 6
mk /reuse-head-w/took.bin 2
mk /reuse-head-w/rest.bin 4
mk /cut-h/target.bin 6
mk /cut-h-hid/filler.bin 4
mk /cut-h-w/tail.bin 2
mk /chained/a.bin 2
mk /chained/b.bin 3
mk /chained/c.bin 4
mk /chained/f.bin 6
mk /gone/inner.bin 2

cat >"$work/ops1" <<'EOF'
mkdir /mnt/contiguous /mnt/zero-size /mnt/reuse-head /mnt/cut-h /mnt/chained /mnt/gone
mkdir /mnt/reuse-head-w /mnt/cut-h-w /mnt/cut-h-hid
cp /payload/contiguous/target.bin /mnt/contiguous/target.bin
cp /payload/zero-size/target.dat /mnt/zero-size/target.dat
cp /payload/reuse-head/target.bin /mnt/reuse-head/target.bin
cp /payload/cut-h/target.bin /mnt/cut-h/target.bin
cp /payload/chained/a.bin /mnt/chained/a.bin
cp /payload/chained/b.bin /mnt/chained/b.bin
cp /payload/chained/c.bin /mnt/chained/c.bin
cp /payload/gone/inner.bin /mnt/gone/inner.bin
EOF
cat >"$work/ops2" <<'EOF'
dd if=/dev/zero of=/mnt/fill.bin bs=4096 2>/dev/null || true
sync
rm /mnt/chained/a.bin /mnt/chained/c.bin
cp /payload/chained/f.bin /mnt/chained/f.bin
EOF
# Phase W, the "ascending" row of ruling F22 (see fat_recover.sh)
cat >"$work/ops3" <<'EOF'
rm /mnt/reuse-head/target.bin
cp /payload/reuse-head-w/took.bin /mnt/reuse-head-w/took.bin
cp /payload/reuse-head-w/rest.bin /mnt/reuse-head-w/rest.bin
rm /mnt/cut-h/target.bin
cp /payload/cut-h-hid/filler.bin /mnt/cut-h-hid/filler.bin
cp /payload/cut-h-w/tail.bin /mnt/cut-h-w/tail.bin
EOF
cat >"$work/ops4" <<'EOF'
rm /mnt/contiguous/target.bin /mnt/zero-size/target.dat
rm -r /mnt/cut-h-hid
rm /mnt/chained/f.bin
rm -r /mnt/gone
EOF

# stage <image> <boot number>: one guest boot with that boot's ops
stage() {
  local img=$1 n=$2 line
  guest_initrd "$work/rd" exfat "" "$src" "$work/ops$n"
  guest_run "$img" "$work/rd" "$work/guest-$name-$n.log"
  while IFS= read -r line; do CMDS+=("guest boot $n: $line"); done <"$work/ops$n"
}

snap() { # <boot number> <expect-free|any>
  python3 "$here/exfat_recover_oracle.py" snap "$img" "$src" "$work/snap$1.json" "$2"
  fsck.exfat -n "$img" >"$work/fsck$1.log" 2>&1 || { cat "$work/fsck$1.log"; echo "fsck.exfat not clean after boot $1" >&2; exit 1; }
}

img=$work/$name.img
CMDS=()
CMDS+=("# source tree: mk calls of tools/fixtures/exfat_recover.sh (patterns from lib.sh); guest: exfat; allocation-order row: $order")
CMDS+=("truncate -s 8M <img>")
CMDS+=("faketime -f '$clock' mkfs.exfat -L FIXTURE -c 4096 <img>")
truncate -s 8M "$img"
faketime -f "$clock" mkfs.exfat -L FIXTURE -c 4096 "$img" >/dev/null

stage "$img" 1
snap 1 any
stage "$img" 2
snap 2 0 # the volume is full
stage "$img" 3
snap 3 0 # and full again after phase W
stage "$img" 4
CMDS+=("fsck.exfat -n <img>   # must report a clean filesystem")
fsck.exfat -n "$img" >"$work/fsck.log" 2>&1 || { cat "$work/fsck.log"; echo "final fsck.exfat not clean" >&2; exit 1; }
CMDS+=("dump.exfat <img>   # geometry and Free Clusters, cross-checked by the oracle")
CMDS+=("python3 tools/fixtures/exfat_recover_oracle.py final <img> <src> <snapshots after boots 1-3>   # chains, bitmap, outcomes")
python3 "$here/exfat_recover_oracle.py" final "$img" "$src" "$work/snap1.json" "$work/snap2.json" "$work/snap3.json" >"$work/oracle.json"

gen=$(generator_json "$img" exfatprogs python3 coreutils jq gzip faketime qemu-system-x86 linux-image-6.1.0-53-amd64 busybox-static kmod cpio)
jq -n --argjson g "$gen" --slurpfile o "$work/oracle.json" '{generator: $g} + $o[0]' >"$work/final/$name.expect.json"
gzip -n -9 -c "$img" >"$work/final/$name.img.gz"
echo "$name: $(stat -c %s "$work/final/$name.img.gz") bytes gzipped"

python3 "$here/exfat_recover_oracle.py" families "$work/final/$name.expect.json"

size=$(stat -c %s "$work/final/$name.img.gz")
if [ "$size" -gt "$max_gz" ]; then
  echo "$name.img.gz is $size bytes, over the $max_gz budget (ruling F6)" >&2
  exit 1
fi

cp "$work/final/$name.img.gz" "$work/final/$name.expect.json" "$out/"
# ruling F102: never report success for nothing
for f in "$out/$name.img.gz" "$out/$name.expect.json"; do
  [ -s "$f" ] || { echo "exfat_recover.sh wrote no $f" >&2; exit 1; }
  echo "wrote $f"
done

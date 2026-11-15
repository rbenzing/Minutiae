#!/bin/bash
# Feasibility probe of plan 3C: what does the KERNEL ext4 driver leave behind after an
# unlink (extent files, a depth-1 extent tree, ext3 block-mapped files, inline data,
# directory records), is an inode number reused, and does a full volume behave? The
# four gates (GATE-L/K/S/U, plan 3C-R2) are printed at the end.
# Run inside the minutiae-recover-fixtures image (README.md, "ext4 recovery probe (3C)"):
#   docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/recover_ext4_probe.sh
# No --privileged, no FUSE, no loop devices, no host mounts. Every write and delete runs
# in the QEMU TCG guest (recover_guest.sh), whose own init loads the modules; the host
# only formats the images (under faketime), boots the guest and READS the finished
# images with ext4_recover_probe.py, never inside the guest.
set -eu
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

work=$(mktemp -d /tmp/recover-ext4-probe.XXXXXX)
trap 'rm -rf "$work"' EXIT
out=$work/img
pay=$work/payload
mkdir -p "$out" "$pay"
py="python3 -I $here/ext4_recover_probe.py"
ft="2023-11-14 22:13:20"
uuid=11111111-2222-3333-4444-555555555555
t0=$ft
t1=$(date -u -d "$ft UTC + 1 hour" +'%F %T')
t2=$(date -u -d "$ft UTC + 2 hours" +'%F %T')
t3=$(date -u -d "$ft UTC + 3 hours" +'%F %T')

# payload files: pseudo-random, so a block is told from every other by its bytes
pattern 3072 e1 >"$pay/e1"
pattern 40 e5 >"$pay/e5"
pattern 100 e5b >"$pay/e5b"
pattern 4096 e8 >"$pay/e8"
pattern 204800 deep >"$pay/deep"
pattern 204800 pad >"$pay/pad"
pattern 204800 deep2 >"$pay/deep2"
pattern 204800 pad2 >"$pay/pad2"
pattern 61440 nx >"$pay/nx"
pattern 20480 m20 >"$pay/m20"

# stage <name> <from image> <mount opts> <guest clock>: the ops come on stdin, the guest
# boots on the named image and the result is kept as <name>.img
stage() {
  local name=$1 from=$2 opts=$3 clock=$4 t0s rc=0
  cat >"$work/ops"
  cp "$out/$from.img" "$work/cur.img"
  guest_initrd "$work/rd" ext4 "$opts" "$pay" "$work/ops" "$clock"
  t0s=$(date +%s)
  guest_run "$work/cur.img" "$work/rd" "$work/guest-$name.log"
  echo "FACT E10.boot image=$name seconds=$(($(date +%s) - t0s)) clock=\"$clock\" bytes=$(stat -c %s "$work/cur.img")"
  cp "$work/cur.img" "$out/$name.img"
  e2fsck -fn "$out/$name.img" >"$work/fsck-$name.txt" 2>&1 || rc=$?
  echo "FACT E9.e2fsck image=$name exit=$rc summary=\"$(tail -n 1 "$work/fsck-$name.txt")\""
  rm -f "$work/cur.img"
}

echo "FACT KERNEL guest_kernel=$kver"
echo "== formatting (host side, faketime $ft)"
truncate -s 16M "$out/ext4-s0.img"
faketime -f "$ft" mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 4096 \
  -U "$uuid" -E "hash_seed=$uuid" "$out/ext4-s0.img"
truncate -s 8M "$out/ext3-s0.img"
faketime -f "$ft" mke2fs -q -F -t ext3 -b 1024 -O ^dir_index -m 0 -J size=1 -N 2048 \
  -U "$uuid" -E "hash_seed=$uuid" "$out/ext3-s0.img"
dumpe2fs -h "$out/ext4-s0.img" 2>/dev/null | grep -E 'Filesystem features|Inode size|Block size|Inode count|Journal'
dumpe2fs -h "$out/ext3-s0.img" 2>/dev/null | grep -E 'Filesystem features|Inode size|Block size|Inode count|Journal'
e2fsck -fn "$out/ext4-s0.img" 2>&1 | tail -n 1
e2fsck -fn "$out/ext3-s0.img" 2>&1 | tail -n 1

echo "== ext4 stage s1w: files, a directory of 90 entries, two files written block by block in turn (deep is the depth-1 fragmented one), then the volume filled"
stage ext4-s1w ext4-s0 "" "$t0" <<'OPS'
cp /payload/e1 /mnt/e1; cp /payload/e5 /mnt/e5; cp /payload/e5b /mnt/e5b; sync
dd if=/payload/e8 of=/mnt/e8 bs=1024 count=2 2>/dev/null; dd if=/payload/e8 of=/mnt/e8 bs=1024 seek=10 count=2 conv=notrunc 2>/dev/null; sync
mkdir /mnt/d; i=100; while [ $i -lt 190 ]; do : > /mnt/d/file_name_number_$i; i=$((i+1)); done; sync
i=0; while [ $i -lt 200 ]; do dd if=/payload/deep of=/mnt/deep bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; dd if=/payload/pad of=/mnt/pad bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; sync; i=$((i+1)); done
dd if=/dev/zero of=/mnt/fill bs=1024 2>/dev/null || true; sync
dd if=/dev/zero of=/mnt/fill2 bs=1024 2>/dev/null || true; sync
echo "PROBE-DF s1w $(df -k /mnt | tail -n 1)"
OPS

echo "== ext4 stage s2f: fill every free block (E7), the pre-delete image"
stage ext4-s2f ext4-s1w "" "$t1" <<'OPS'
dd if=/dev/zero of=/mnt/fill3 bs=1024 2>/dev/null || true; sync
dd if=/dev/zero of=/mnt/fill4 bs=1024 2>/dev/null || true; sync
echo "PROBE-DF s2f $(df -k /mnt | tail -n 1)"
OPS

# two live names of block 1 of the directory: its first record and a middle one
picks=$($py pick "$out/ext4-s2f.img" /d)
echo "$picks" >"$out/ext4.picks"
set -- $picks
echo "FACT E4.picks fs=ext4 first=$1 mid=$2"
first4=$1
mid4=$2

echo "== ext4 stage s3d: the kernel unlinks e1 e5 e5b e8, then deep, then two directory entries"
{
  cat <<'OPS'
rm /mnt/e1; rm /mnt/e5; rm /mnt/e5b; rm /mnt/e8; sync
rm /mnt/deep; sync
OPS
  printf 'rm /mnt/d/%s; rm /mnt/d/%s; sync\n' "$first4" "$mid4"
  echo 'echo "PROBE-DF s3d $(df -k /mnt | tail -n 1)"'
} | stage ext4-s3d ext4-s2f "" "$t2"

echo "== ext4 branches of s3d: new files at the SAME clock as the delete boot (rs) and one hour later (rp)"
for br in rs:"$t2" rp:"$t3"; do
  stage "ext4-${br%%:*}" ext4-s3d "" "${br#*:}" <<'OPS'
cp /payload/e1 /mnt/r1; cp /payload/e1 /mnt/r2; cp /payload/e1 /mnt/r3; cp /payload/nx /mnt/nx; sync
echo "PROBE-LS $(ls -i /mnt)"
OPS
done

echo "== ext4 branch of s3d: a depth-1 file (interleaved with a pad file) written and unlinked inside ONE boot (sb)"
stage ext4-sb ext4-s3d "" "$t2" <<'OPS'
i=0; while [ $i -lt 200 ]; do dd if=/payload/deep2 of=/mnt/deep2 bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; dd if=/payload/pad2 of=/mnt/pad2 bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; sync; i=$((i+1)); done
echo "PROBE-SB-LS $(ls -i /mnt/deep2)"
rm /mnt/deep2; sync
OPS
grep -a '^PROBE-SB-LS' "$work/guest-ext4-sb.log" | tr -d '\r' | head -n 1 | awk '{print $2}' >"$out/ext4-sb.ino"

echo "== ext3 stages (block map, linear directory), mounted as ext4"
stage ext3-s1w ext3-s0 "" "$t0" <<'OPS'
cp /payload/m20 /mnt/m20; cp /payload/e1 /mnt/m3; sync
mkdir /mnt/d; i=100; while [ $i -lt 190 ]; do : > /mnt/d/file_name_number_$i; i=$((i+1)); done; sync
OPS
picks=$($py pick "$out/ext3-s1w.img" /d)
echo "$picks" >"$out/ext3.picks"
set -- $picks
echo "FACT E4.picks fs=ext3 first=$1 mid=$2"
{
  echo 'rm /mnt/m20; rm /mnt/m3; sync'
  printf 'rm /mnt/d/%s; rm /mnt/d/%s; sync\n' "$1" "$2"
} | stage ext3-s2d ext3-s1w "" "$t1"

echo "== guest PROBE lines"
grep -ah '^PROBE-' "$work"/guest-*.log | tr -d '\r' || true
echo "== independent tool cross-checks (debugfs, dumpe2fs, e2fsck)"
for s in ext4-s2f ext4-s3d; do
  echo "-- dumpe2fs -h $s: free blocks and inodes"
  dumpe2fs -h "$out/$s.img" 2>/dev/null | grep -E 'Free blocks|Free inodes|Filesystem state|Filesystem features'
done
echo "-- debugfs ex /deep (pre-delete image)"
debugfs -R "ex /deep" "$out/ext4-s2f.img" 2>&1 | head -n 12
echo "-- debugfs stat /e8 (sparse file, pre-delete image)"
debugfs -R "ex /e8" "$out/ext4-s2f.img" 2>&1 | head -n 6
echo "-- image sizes (E10): raw and gzipped"
for f in "$out"/*.img; do
  echo "FACT E10.image name=$(basename "$f") bytes=$(stat -c %s "$f") gz_bytes=$(gzip -9 -n <"$f" | wc -c)"
done
echo "== host-side read of the finished images (read-only)"
$py report "$out" "$pay"
echo "PROBE COMPLETE"

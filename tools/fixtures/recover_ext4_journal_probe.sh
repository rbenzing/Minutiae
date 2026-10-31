#!/bin/bash
# Feasibility probe of plan 3G (Task 1): what does the KERNEL ext4 driver write into its jbd2 journal, on both guest
# kernels (Debian 5.10.223 and 6.1.0-53), for a clean unmount, for a no-sync crash and for the kernel's own replay of a
# copy? The eleven gates GATE-J1..J11 (plan 3G-R20) are printed at the end by ext4_journal_oracle.py.
# Run inside the minutiae-recover-fixtures-510 image (README.md, "ext4 journal probe (3G)"):
#   docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures-510 bash tools/fixtures/recover_ext4_journal_probe.sh
# No --privileged, no FUSE, no loop devices, no host mounts. Every write, delete and crash runs in the QEMU TCG guest
# (recover_guest.sh), whose own /init loads the kernel modules: ext4.ko and jbd2.ko are never loaded on the host or in the
# Docker VM. The host only formats the images (mke2fs under faketime), boots the guest and READS the finished images with
# debugfs, dumpe2fs, e2fsck and ext4_journal_oracle.py, never inside the guest. All work happens in a mktemp directory
# INSIDE the container, removed by the trap; only stdout reaches the caller.
#
# Environment: JSIZE (mke2fs -J size in MiB, default 1), PROBE_KEEP_DIR (a debugging aid: copy the stage images and guest
# logs there; never set by gen.sh).
#
# Stages per kernel (one guest boot each, the guest clock one hour later than the previous boot):
#   ext4 p1  write live, a depth-1 file (deep, interleaved with pad), small and three fillers; clean unmount
#   ext4 p2  rm small and deep; clean unmount
#   ext4 p3  write and rm a depth-1 file (same) inside ONE boot; clean unmount
#   ext4 p4  write x, sync, rm x, sync, write and fsync y, append to live and fsync, then an unsynced z; GUEST_HALT=crash
#   ext4 p5  the kernel's replay of a COPY of p4, listing every file as "RF <path> <size> <sha256>"
#   ext3 p1/p2  the same idea with a block-mapped file (bm), handled by the ext4 driver
set -eu
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
jsize=${JSIZE:-1}
k510=5.10.0-32-amd64
k61=6.1.0-53-amd64
if [ "$(id -u)" != 0 ]; then echo "recover_ext4_journal_probe.sh must run as root (the container does)" >&2; exit 1; fi
for k in "$k510" "$k61"; do
  [ -e "/lib/modules/$k" ] && [ -e "/boot/vmlinuz-$k" ] || { echo "kernel $k is missing: use the minutiae-recover-fixtures-510 image" >&2; exit 1; }
done
# shellcheck source=lib.sh
. "$here/lib.sh"
work=$(mktemp -d /tmp/jprobe.XXXXXX)
trap 'if [ -n "${PROBE_KEEP_DIR:-}" ]; then mkdir -p "$PROBE_KEEP_DIR"; cp -r "$work"/k* "$work"/payload "$PROBE_KEEP_DIR"/ 2>/dev/null || true; fi; rm -rf "$work"' EXIT
pay=$work/payload
mkdir -p "$pay"
ft="2023-11-14 22:13:20"
uuid=11111111-2222-3333-4444-555555555555
clock() { date -u -d "$ft UTC + $1 hours" +'%F %T'; }
# pseudo-random payloads, so a block is told from every other by its bytes; whole-block sizes minus a few bytes
pattern 3000 small >"$pay/small"
pattern 700 live >"$pay/live"
pattern 900 app >"$pay/app"
pattern $((24 * 1024 - 9)) deep >"$pay/deep"
pattern $((24 * 1024 - 9)) pad >"$pay/pad"
pattern $((24 * 1024 - 7)) same >"$pay/same"
pattern $((24 * 1024 - 7)) spad >"$pay/spad"
pattern $((20 * 1024 - 11)) bm >"$pay/bm"
pattern 1500 x >"$pay/x"
pattern 2048 y >"$pay/y"
echo "FACT PROBE jsize_mib=$jsize host_date=\"$(date -u +'%F %T')\" mke2fs=\"$(mke2fs -V 2>&1 | head -n 1)\" qemu=\"$(qemu-system-x86_64 --version | head -n 1)\""

# one_kernel <image dir>: the whole workload on the guest kernel that GUEST_KVER names (set by the caller before this function sources
# the guest library, which reads it once)
one_kernel() {
  local out=$1 jsbblk it0
  # shellcheck source=recover_guest.sh
  . "$here/recover_guest.sh"
  echo "FACT KERNEL guest_kernel=$kver"

  # stage <name> <from image> <guest clock>: the ops come on stdin; the guest boots on a COPY of the named image and the result is
  # kept as <name>.img (GUEST_HALT=crash in the caller's environment ends the boot with the no-sync reboot)
  stage() {
    local name=$1 from=$2 clk=$3 t0s rc=0
    cat >"$work/ops"
    cp "$out/$from.img" "$work/cur.img"
    guest_initrd "$work/rd" ext4 "" "$pay" "$work/ops" "$clk"
    t0s=$(date +%s)
    guest_run "$work/cur.img" "$work/rd" "$work/guest-$name.log"
    echo "FACT BOOT kernel=$kver image=$name seconds=$(($(date +%s) - t0s)) clock=\"$clk\" halt=${GUEST_HALT:-clean} bytes=$(stat -c %s "$work/cur.img")"
    cp "$work/cur.img" "$out/$name.img"
    cp "$work/guest-$name.log" "$out/guest-$name.log"
    e2fsck -fn "$out/$name.img" >"$work/fsck-$name.txt" 2>&1 || rc=$?
    echo "FACT E2FSCK kernel=$kver image=$name exit=$rc summary=\"$(tail -n 1 "$work/fsck-$name.txt")\""
    rm -f "$work/cur.img"
  }
  # ino <names...>: the guest op that prints "PROBE-INO <name> <inode number>" for each file
  ino() {
    printf '%s\n' "for f in $*; do set -- \$(ls -i /mnt/\$f); echo \"PROBE-INO \$f \$1\"; done"
  }
  # j5 <label>: the guest op that reads, straight from the disk (O_DIRECT), the journal superblock's sequence and start and the
  # home inode-table slot of x (inode number in /tmp/xino, once it is known)
  j5() {
    printf '%s\n' "jsb=\$(dd if=/dev/vda bs=1024 skip=$jsbblk count=1 iflag=direct 2>/dev/null | dd bs=1 skip=24 count=8 2>/dev/null | xxd -p | tr -d '\\n'); slot=-; if [ -s /tmp/xino ]; then n=\$(cat /tmp/xino); off=\$(( (n-1)*256 )); blk=\$(( $it0 + off/1024 )); o=\$(( off%1024 )); slot=\$(dd if=/dev/vda bs=1024 skip=\$blk count=1 iflag=direct 2>/dev/null | dd bs=1 skip=\$o count=256 2>/dev/null | xxd -p | tr -d '\\n'); fi; echo \"PROBE-J5 $1 jsb=\$jsb xslot=\$slot\""
  }

  echo "== formatting (host side, faketime $ft, -J size=$jsize)"
  truncate -s 16M "$out/ext4-p0.img"
  faketime -f "$ft" mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,^orphan_file -m 0 -J size="$jsize" -N 4096 \
    -U "$uuid" -E "hash_seed=$uuid" "$out/ext4-p0.img"
  truncate -s 8M "$out/ext3-p0.img"
  faketime -f "$ft" mke2fs -q -F -t ext3 -b 1024 -O ^dir_index,^orphan_file -m 0 -J size="$jsize" -N 2048 \
    -U "$uuid" -E "hash_seed=$uuid" "$out/ext3-p0.img"
  for f in ext4-p0 ext3-p0; do
    echo "-- dumpe2fs -h $f"
    dumpe2fs -h "$out/$f.img" 2>/dev/null | grep -E 'Filesystem features|Inode size|Block size|Inode count|Journal|Default mount'
    e2fsck -fn "$out/$f.img" 2>&1 | tail -n 1
  done
  # constants the in-guest reader needs: the file-system block of journal block 0 and the first block of the group-0 inode table
  jsbblk=$(debugfs -R "bmap <8> 0" "$out/ext4-p0.img" 2>/dev/null | tr -d '[:space:]')
  it0=$(dumpe2fs "$out/ext4-p0.img" 2>/dev/null | sed -n 's/^ *Inode table at \([0-9]*\)-.*/\1/p' | head -n 1)
  echo "FACT J5.env kernel=$kver jsb_fs_block=$jsbblk inode_table_group0=$it0"

  echo "== ext4 p1: live, deep+pad interleaved block by block (a depth-1 tree), small, three fillers; clean unmount"
  {
    cat <<'OPS'
cp /payload/live /mnt/live; sync
i=0; while [ $i -lt 24 ]; do dd if=/payload/deep of=/mnt/deep bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; dd if=/payload/pad of=/mnt/pad bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; sync; i=$((i+1)); done
cp /payload/small /mnt/small; sync
i=1; while [ $i -le 3 ]; do echo filler$i > /mnt/f$i; sync; i=$((i+1)); done
OPS
    ino live deep pad small f1 f2 f3
  } | stage ext4-p1 ext4-p0 "$(clock 0)"

  echo "== ext4 p2: rm small and deep; clean unmount"
  stage ext4-p2 ext4-p1 "$(clock 1)" <<'OPS'
rm /mnt/small; rm /mnt/deep; sync
OPS

  echo "== ext4 p3: a depth-1 file written and removed inside ONE boot; clean unmount"
  {
    cat <<'OPS'
i=0; while [ $i -lt 24 ]; do dd if=/payload/same of=/mnt/same bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; dd if=/payload/spad of=/mnt/spad bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; sync; i=$((i+1)); done
OPS
    ino same spad
    echo 'rm /mnt/same; sync'
  } | stage ext4-p3 ext4-p2 "$(clock 2)"

  echo "== ext4 p4: the crash image (x created and removed with a sync each, y created with fsync, live appended with fsync, z unsynced)"
  {
    j5 j0
    echo 'cp /payload/x /mnt/x; set -- $(ls -i /mnt/x); echo $1 > /tmp/xino; echo "PROBE-INO x $1"; sync'
    j5 j1
    echo 'rm /mnt/x; sync'
    j5 j2
    echo 'dd if=/payload/y of=/mnt/y bs=1024 conv=fsync 2>/dev/null'
    ino y live
    j5 j3
    echo 'dd if=/payload/app of=/mnt/live bs=1024 oflag=append conv=notrunc,fsync 2>/dev/null'
    j5 j4
    echo 'echo unsynced > /mnt/z'
  } | GUEST_HALT=crash stage ext4-p4 ext4-p3 "$(clock 3)"

  echo "== ext4 p5: the kernel's own replay of a COPY of the crash image (p4 itself is never booted again)"
  p4sha=$(sha256sum "$out/ext4-p4.img" | cut -d' ' -f1)
  echo "PROBE-SHA p4-before $p4sha" >>"$out/guest-host.log"
  stage ext4-p5 ext4-p4 "$(clock 4)" <<'OPS'
dmesg | grep -i -E 'EXT4-fs|JBD2|recover' | head -n 12 | while read -r l; do echo "PROBE-DMESG $l"; done
find /mnt -type f | while read -r f; do echo "RF $f $(wc -c < $f) $(sha256sum $f | cut -d' ' -f1)"; done
OPS
  echo "PROBE-SHA p4-after $(sha256sum "$out/ext4-p4.img" | cut -d' ' -f1)" >>"$out/guest-host.log"
  echo "FACT P4.sha256 kernel=$kver before_replay=$p4sha after_replay=$(sha256sum "$out/ext4-p4.img" | cut -d' ' -f1)"

  echo "== ext3 p1 and p2 (block map, handled by the ext4 driver)"
  {
    cat <<'OPS'
cp /payload/live /mnt/live; sync
cp /payload/bm /mnt/bm; sync
i=1; while [ $i -le 3 ]; do echo filler$i > /mnt/f$i; sync; i=$((i+1)); done
OPS
    ino live bm f1 f2 f3
  } | stage ext3-p1 ext3-p0 "$(clock 0)"
  stage ext3-p2 ext3-p1 "$(clock 1)" <<'OPS'
rm /mnt/bm; sync
OPS

  echo "== guest PROBE lines"
  grep -ah '^PROBE-' "$work"/guest-*.log | tr -d '\r' || true
  echo "== independent tool cross-checks on the stage images"
  debugfs -R "stat /deep" "$out/ext4-p1.img" 2>&1 | head -n 20
  echo "-- debugfs stat <8> (journal inode) of ext4-p1 and ext3-p1"
  debugfs -R "stat <8>" "$out/ext4-p1.img" 2>&1 | head -n 12
  debugfs -R "stat <8>" "$out/ext3-p1.img" 2>&1 | head -n 14
  echo "-- debugfs logdump -a of the crash image (head)"
  debugfs -R "logdump -a" "$out/ext4-p4.img" 2>&1 | grep -v '^ *[0-9a-f]\{4\} ' | head -n 60
  echo "-- dumpe2fs -h of the crash image"
  dumpe2fs -h "$out/ext4-p4.img" 2>/dev/null | grep -E 'features|state|Journal|Last mount|Last write'
  echo "-- image sizes"
  for f in "$out"/*.img; do echo "FACT IMAGE name=$(basename "$f") bytes=$(stat -c %s "$f") gz_bytes=$(gzip -9 -n <"$f" | wc -c)"; done
}

for k in "$k510" "$k61"; do
  mkdir -p "$work/k$k"
  echo "== kernel $k"
  (
    export GUEST_KVER=$k
    one_kernel "$work/k$k"
  )
done
echo "== host-side decode and cross-checks (ext4_journal_oracle.py probe)"
rc=0
python3 -I "$here/ext4_journal_oracle.py" probe --payload "$pay" "5.10=$work/k$k510" "6.1=$work/k$k61" || rc=$?
echo "PROBE COMPLETE exit=$rc"
exit $rc

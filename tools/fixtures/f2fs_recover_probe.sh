#!/bin/bash
# Feasibility probe of plan 3D: what does the KERNEL f2fs driver leave behind when files
# are unlinked, renamed and overwritten, and what does discard do to it? The answers fix
# the stale-node index, the SSA check, the version rule and the fixture families.
# Run inside the minutiae-recover-fixtures image (README.md, "F2FS recovery probe (3D)"):
#   MSYS_NO_PATHCONV=1 docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures \
#     bash tools/fixtures/f2fs_recover_probe.sh > probe.txt 2>&1
# No --privileged, no FUSE, no loop devices, no host mounts. Every write, rename, delete
# and fill runs in the QEMU TCG guest (recover_guest.sh); the host only formats the images
# (under faketime) and READS the finished images (f2fs_recover_oracle.py and the f2fs
# tools). All work files live in a container-local directory under /tmp INSIDE the
# container, removed on exit (ruling 3D-R28); nothing is written to the /work bind mount
# except what the caller redirects.
# Output: "FACT Q<n> ..." lines with "HEX" evidence lines and "GATE G<n> ..." verdicts.
#
# PROBE_KEEP=<dir> (development only) additionally copies the stage images and the tool
# captures to <dir>, so that the decoder can be run again without the guest stages.
set -eu
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

work=$(mktemp -d /tmp/f2fs-recover.XXXXXX)
trap 'rm -rf "$work"' EXIT
imgs=$work/img
tools=$work/tools
mkdir -p "$imgs" "$tools" "$work/pay1" "$work/pay4"
ft="2023-11-14 22:13:20"
export LC_ALL=C.UTF-8 NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
t0=$(date +%s)

# payloads. Small files: pattern; big files: one record per 4 KiB block (stamp), so any
# block found anywhere is traceable to its file and offset.
pattern 20480 A >"$work/pay1/A"
stamp $((5 * 1024 * 1024)) B >"$work/pay1/B"
stamp $((12 * 1024 * 1024)) C >"$work/pay1/C"
pattern 4096 H0 >"$work/pay1/H0"
pattern 4096 H3 >"$work/pay1/H3"
pattern 100 I >"$work/pay1/I"
pattern 20480 R >"$work/pay1/R"
pattern 1000 f1 >"$work/pay1/f1"
pattern 1000 f2 >"$work/pay1/f2"
pattern 1000 f3 >"$work/pay1/f3"
pattern 20480 N >"$work/pay4/N"

# format <name>: a fresh 128 MiB volume (host side, frozen clock, fixed uuid)
format() {
  truncate -s 128M "$imgs/$1.img"
  faketime -f "$ft" mkfs.f2fs -q -f -r -l probe -U 3d3d3d3d-0000-4000-8000-000000000001 \
    -O extra_attr,inode_checksum,sb_checksum "$imgs/$1.img"
  python3 "$here/f2fs_normalize.py" "$imgs/$1.img"
}

# stage <from> <to> <payload dir> <mount opts>: the ops come on stdin; the guest boots on
# the <from> image and the result is kept as <to>
stage() {
  local from=$1 to=$2 pay=$3 opts=$4
  cat >"$work/ops"
  cp "$imgs/$from.img" "$imgs/$to.img"
  guest_initrd "$work/rd" f2fs "$opts" "$pay" "$work/ops"
  guest_run "$imgs/$to.img" "$work/rd" "$work/guest-$to.log"
  echo "STAGE $to done after $(($(date +%s) - t0)) s"
}

# capture <name>: run the read-only f2fs tools on a COPY of the stage image
capture() {
  local d=$tools/$1 rc
  mkdir -p "$d"
  cp "$imgs/$1.img" "$d/img"
  (
    cd "$d"
    rc=0; fsck.f2fs -f --dry-run img >fsck-f.txt 2>&1 || rc=$?; echo "$rc" >fsck-f.rc
    rc=0; fsck.f2fs -t --dry-run img >tree.txt 2>&1 || rc=$?; echo "$rc" >tree.rc
    rc=0; dump.f2fs -d 1 -s0~-1 img >dump-i.txt 2>&1 || rc=$?; echo "$rc" >dump-i.rc
    rc=0; dump.f2fs -n0~-1 img >dump-n.txt 2>&1 || rc=$?; echo "$rc" >dump-n.rc
    rc=0; dump.f2fs -a0~-1 img >dump-a.txt 2>&1 || rc=$?; echo "$rc" >dump-a.rc
  )
  rm -f "$d/img"
  echo "TOOLS $1 fsck-rc=$(cat "$d/fsck-f.rc") tree-rc=$(cat "$d/tree.rc") dump-rc=$(cat "$d/dump-i.rc"),$(cat "$d/dump-n.rc"),$(cat "$d/dump-a.rc")"
}

# report_fill <stage>: the lines of the fill stage's guest log that matter (volume full, GC counters)
report_fill() {
  grep -a -e '^SYSFS ' -e ' /mnt/fill$' -e '^/dev/vda' "$work/guest-$1.log" | grep -a -i -e 'gc' -e 'cp_' -e 'discard' -e 'fill' -e 'vda' || true
}

# ops_write <small|large>: the write workload. "large" puts 300 files (/g/a*) before and 900 (/g/b*)
# after the files of interest, so the warm-node log closes the segments around them and the unlinks
# (including rm -r /g) can free whole node segments; f2 and f3 are created after the second batch so
# the segment that holds the stale inodes keeps no live node. "small" is the plain workload.
ops_write() {
  echo 'mkdir /mnt/p /mnt/s /mnt/s/x'
  if [ "$1" = large ]; then
    echo 'mkdir /mnt/g'
    echo 'i=0; while [ $i -lt 300 ]; do : > /mnt/g/a$i; i=$((i+1)); done'
  fi
  cat <<'OPS'
cp /payload/A /mnt/p/A
cp /payload/B /mnt/p/B
cp /payload/C /mnt/p/C
dd if=/payload/H0 of=/mnt/p/H bs=4096 count=1 2>/dev/null
dd if=/payload/H3 of=/mnt/p/H bs=4096 seek=3 conv=notrunc 2>/dev/null
cp /payload/I /mnt/p/I
cp /payload/R /mnt/p/R
cp /payload/f1 /mnt/s/x/f1
OPS
  if [ "$1" = large ]; then
    echo 'i=0; while [ $i -lt 900 ]; do : > /mnt/g/b$i; i=$((i+1)); done'
  fi
  cat <<'OPS'
cp /payload/f2 /mnt/s/x/f2
cp /payload/f3 /mnt/s/x/f3
sync
OPS
}

# ops_unlink <suffix of the renamed file: R2 or R> <small|large>
ops_unlink() {
  echo "rm /mnt/p/A /mnt/p/B /mnt/p/C /mnt/p/H /mnt/p/I /mnt/p/$1 /mnt/s/x/f1"
  if [ "$2" = large ]; then echo 'rm -r /mnt/g'; fi
  echo sync
}

# fill_ops: write zeros until the volume is full (ENOSPC), record the kernel's counters, remove the file
fill_ops() {
  cat <<'OPS'
dd if=/dev/zero of=/mnt/fill bs=4096 2>/dev/null || true
ls -l /mnt/fill
df /mnt
sync
for f in /sys/fs/f2fs/vda/*; do echo "SYSFS $f $(cat $f 2>/dev/null)"; done
rm /mnt/fill
sync
OPS
}

rename_ops() {
  printf '%s\n' 'mv /mnt/p/R /mnt/p/R2' sync
}

echo "== formatting (host side, faketime $ft)"
format p0
cp "$imgs/p0.img" "$imgs/d0.img"
capture p0

# small workload (the plan's): p1 write, p2 rename, p3 unlink, p4 create 8 files, p5 fill
echo "== small workload, nodiscard (guest option nodiscard,noatime)"
ops_write small | stage p0 p1 "$work/pay1" nodiscard,noatime
capture p1
rename_ops | stage p1 p2 "$work/pay4" nodiscard,noatime
capture p2
ops_unlink R2 small | stage p2 p3 "$work/pay4" nodiscard,noatime
capture p3
stage p3 p4 "$work/pay4" nodiscard,noatime <<'OPS'
for i in 1 2 3 4 5 6 7 8; do cp /payload/N /mnt/p/N$i; done
sync
OPS
capture p4
echo "== small workload: fill to ENOSPC (GUEST_TIMEOUT=2400)"
fill_ops | GUEST_TIMEOUT=2400 stage p4 p5 "$work/pay4" nodiscard,noatime
report_fill p5
capture p5

# large workload: the same files between two batches of /g files
echo "== large workload, nodiscard"
ops_write large | stage p0 l1 "$work/pay1" nodiscard,noatime
capture l1
rename_ops | stage l1 l2 "$work/pay4" nodiscard,noatime
capture l2
ops_unlink R2 large | stage l2 l3 "$work/pay4" nodiscard,noatime
capture l3
echo "== large workload: fill to ENOSPC"
fill_ops | GUEST_TIMEOUT=2400 stage l3 l5 "$work/pay4" nodiscard,noatime
report_fill l5
capture l5

echo "== large and small workload, discard (guest option discard,noatime; qemu discard=unmap)"
ops_write small | GUEST_DRIVE_OPTS=,discard=unmap stage d0 d1 "$work/pay1" discard,noatime
capture d1
ops_unlink R small | GUEST_DRIVE_OPTS=,discard=unmap stage d1 d3 "$work/pay4" discard,noatime
capture d3
ops_write large | GUEST_DRIVE_OPTS=,discard=unmap stage d0 m1 "$work/pay1" discard,noatime
capture m1
ops_unlink R large | GUEST_DRIVE_OPTS=,discard=unmap stage m1 m3 "$work/pay4" discard,noatime
capture m3
echo "RUNTIME guest stages and tools: $(($(date +%s) - t0)) s"

if [ -n "${PROBE_KEEP:-}" ]; then
  mkdir -p "$PROBE_KEEP"
  cp -r --sparse=always "$imgs" "$tools" "$PROBE_KEEP"/
fi

echo "== decoding (f2fs_recover_oracle.py probe)"
python3 "$here/f2fs_recover_oracle.py" probe --images "$imgs" --tools "$tools"

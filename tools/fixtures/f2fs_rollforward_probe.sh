#!/bin/bash
# Feasibility probe of plan 3H: what does the KERNEL f2fs driver leave in the warm-node log when
# files are written and fsynced and the machine then dies without a checkpoint, and what does the
# kernel's own recovery make of it on the next mount? The answers fix the chain rule, the footer
# marks, the oracle route and the fixture families of the F2FS roll-forward reader.
# Run inside the minutiae-recover-fixtures image (README.md, "F2FS roll-forward probe (3H)"):
#   MSYS_NO_PATHCONV=1 docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures \
#     bash tools/fixtures/f2fs_rollforward_probe.sh > probe.txt 2>&1
# No --privileged, no FUSE, no loop devices, no host mounts. The kernel f2fs module is loaded
# ONLY by the guest's own /init (recover_guest.sh); the host formats the image (under faketime)
# and READS the finished images (f2fs_recover_oracle.py rfprobe and the f2fs tools, on copies).
# All work files live in a container-local directory under /tmp INSIDE the container, removed on
# exit; nothing is written to the /work bind mount except what the caller redirects.
#
# Three guest boots (one stage each):
#   base     /rf/ and /upd/append.bin (20 KiB), sync, clean unmount
#   crash    (GUEST_CRASH=1) cp_interval 3600, sync (the base checkpoint), then fsynced writes:
#            /rf/small.bin (20 KiB), /rf/direct.bin (5 MiB), /rf/indirect.bin (12 MiB), 8 KiB
#            appended to /upd/append.bin; the host quits QEMU through the monitor socket
#   replayed the kernel's recovery on a COPY of the crash image: a normal mount, listing, hashing
# Output: "FACT H<n> ..." lines with "HEX" evidence lines, "BRANCH H<n> ..." and "GATE H<n>" verdicts.
#
# PROBE_KEEP=<dir> (development only) additionally copies the stage images and the tool captures
# to <dir>, so that the decoder can be run again without the guest stages.
set -eu
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

work=$(mktemp -d /tmp/f2fs-rf.XXXXXX)
trap 'rm -rf "$work"' EXIT
imgs=$work/img
tools=$work/tools
pay1=$work/pay1
pay2=$work/pay2
mkdir -p "$imgs" "$tools" "$pay1" "$pay2"
ft="2023-11-14 22:13:20"
export LC_ALL=C.UTF-8 NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
t0=$(date +%s)

# payloads. Small files: pattern; big files: one record per 4 KiB block (stamp).
pattern 20480 ap >"$pay1/append.bin"
pattern 20480 S >"$pay2/small.bin"
stamp $((5 * 1024 * 1024)) D >"$pay2/direct.bin"
stamp $((12 * 1024 * 1024)) I >"$pay2/indirect.bin"
pattern 8192 ADD >"$pay2/add.bin"

# the fsync route (H4): the guest's busybox is the one in this container
if busybox --list | grep -qx fsync; then
  route="busybox fsync (applet present)"
else
  route="dd conv=fsync (busybox has no fsync applet)"
fi
echo "FSYNC-ROUTE $route"
echo "BUSYBOX $(busybox 2>&1 | head -1)"

# format: a fresh 128 MiB volume (host side, frozen clock, fixed uuid)
truncate -s 128M "$imgs/fmt.img"
faketime -f "$ft" mkfs.f2fs -q -f -r -l rfprobe -U 3d3d3d3d-0000-4000-8000-000000000003 \
  -O extra_attr,inode_checksum,sb_checksum "$imgs/fmt.img"
python3 "$here/f2fs_normalize.py" "$imgs/fmt.img"

# stage <from> <to> <payload dir> <mount opts>: the ops come on stdin; the guest boots on a
# copy of <from> and the result is kept as <to>
stage() {
  local from=$1 to=$2 pay=$3 opts=$4
  cat >"$work/ops"
  cp "$imgs/$from.img" "$imgs/$to.img"
  guest_initrd "$work/rd" f2fs "$opts" "$pay" "$work/ops"
  guest_run "$imgs/$to.img" "$work/rd" "$work/guest-$to.log"
  echo "STAGE $to done after $(($(date +%s) - t0)) s"
}

# capture <name> [d3]: the read-only f2fs tools on a COPY of the stage image; with d3 also
# fsck.f2fs -d 3 -f --dry-run (H3) and whether the copy changed
capture() {
  local d=$tools/$1 rc
  mkdir -p "$d"
  cp "$imgs/$1.img" "$d/img"
  (
    cd "$d"
    rc=0; fsck.f2fs -f --dry-run img >fsck-f.txt 2>&1 || rc=$?; echo "$rc" >fsck-f.rc
    rc=0; dump.f2fs -d 1 -s0~-1 img >dump-i.txt 2>&1 || rc=$?; echo "$rc" >dump-i.rc
    rc=0; dump.f2fs -n0~-1 img >dump-n.txt 2>&1 || rc=$?; echo "$rc" >dump-n.rc
  )
  if [ "${2:-}" = d3 ]; then
    (cd "$d" && rc=0; fsck.f2fs -d 3 -f --dry-run img >fsck-d3.txt 2>&1 || rc=$?; echo "$rc" >fsck-d3.rc)
    if cmp -s "$d/img" "$imgs/$1.img"; then echo yes >"$d/fsck-d3.same"; else echo no >"$d/fsck-d3.same"; fi
  fi
  rm -f "$d/img"
  echo "TOOLS $1 fsck-rc=$(cat "$d/fsck-f.rc") dump-rc=$(cat "$d/dump-i.rc"),$(cat "$d/dump-n.rc")"
}

# the checkpoint block numbers, read by the guest from the raw device (H5): the superblock holds cp_blkaddr
cp0=$(python3 -c "
import struct, sys
with open(sys.argv[1], 'rb') as f:
    f.seek(1024)
    print(struct.unpack_from('<I', f.read(3072), 76)[0])" "$imgs/fmt.img")
cp1=$((cp0 + 512))
echo "CHECKPOINT-BLOCKS $cp0 $cp1"

# cpver_ops <tag>: print the first 8 bytes (checkpoint_ver) of both checkpoint packs, straight from the disk
cpver_ops() {
  sed -e "s/@TAG@/$1/g" -e "s/@CP0@/$cp0/g" -e "s/@CP1@/$cp1/g" <<'OPS'
echo 3 > /proc/sys/vm/drop_caches
for b in @CP0@ @CP1@; do echo "CPVER @TAG@ $b $(dd if=/dev/vda bs=4096 skip=$b count=1 2>/dev/null | od -An -tx1 -N8 | tr -d ' \n')"; done
OPS
}

# wr <payload> <target>: write a new file and fsync it; app: append and fsync
wr() {
  case $route in
    busybox*) echo "cp /payload/$1 $2 && fsync $2" ;;
    *) echo "dd if=/payload/$1 of=$2 bs=4096 conv=fsync 2>/dev/null" ;;
  esac
}
app() {
  case $route in
    busybox*) echo "cat /payload/$1 >> $2 && fsync $2" ;;
    *) echo "dd if=/payload/$1 of=$2 bs=4096 seek=5 conv=notrunc,fsync 2>/dev/null" ;;
  esac
}

echo "== formatting (host side, faketime $ft)"
echo "== base boot: /rf, /upd/append.bin, sync, clean unmount"
cat <<'OPS' | stage fmt base "$pay1" nodiscard,noatime,background_gc=off
mkdir /mnt/rf /mnt/upd
cp /payload/append.bin /mnt/upd/append.bin
sync
OPS
capture base

echo "== crash boot: fsynced writes, then the host quits QEMU (GUEST_CRASH=1)"
{
  echo 'echo 3600 > /sys/fs/f2fs/vda/cp_interval'
  echo 'echo "CPINT $(cat /sys/fs/f2fs/vda/cp_interval)"'
  echo 'sync'
  cpver_ops A
  wr small.bin /mnt/rf/small.bin
  wr direct.bin /mnt/rf/direct.bin
  wr indirect.bin /mnt/rf/indirect.bin
  app add.bin /mnt/upd/append.bin
  cpver_ops B
  echo 'echo WRITES-DONE'
} >"$work/crash.ops"
GUEST_CRASH=1 stage base crash "$pay2" nodiscard,noatime,background_gc=off <"$work/crash.ops"
grep -a -e '^CPINT ' -e '^CPVER ' -e '^WRITES-DONE' "$work/guest-crash.log" || true
if grep -l qemu /proc/[0-9]*/comm >/dev/null 2>&1; then echo "FAIL: a qemu process is still running after the crash stage"; exit 1; fi
echo "QEMU gone after the quit: yes"
capture crash d3

echo "== replay boot: the guest opens a COPY of the crash image normally (the kernel's own recovery)"
cat <<'OPS' | stage crash replayed "$pay1" nodiscard,noatime
dmesg | grep -ai f2fs || true
ls -lR /mnt || true
for f in /mnt/rf/* /mnt/upd/*; do echo "RF $f $(wc -c < $f | tr -d ' ') $(sha256sum $f | cut -d' ' -f1)"; done
OPS
grep -a -e '^RF ' -e 'F2FS-fs' -e 'f2fs' "$work/guest-replayed.log" | head -40 || true
capture replayed
echo "RUNTIME guest stages and tools: $(($(date +%s) - t0)) s"

if [ -n "${PROBE_KEEP:-}" ]; then
  mkdir -p "$PROBE_KEEP"
  cp -r --sparse=always "$imgs" "$tools" "$PROBE_KEEP"/
  cp "$work"/guest-*.log "$PROBE_KEEP"/
  cp -r "$pay1" "$pay2" "$PROBE_KEEP"/
fi

echo "== decoding (f2fs_recover_oracle.py rfprobe)"
python3 "$here/f2fs_recover_oracle.py" rfprobe --base "$imgs/base.img" --crash "$imgs/crash.img" \
  --replayed "$imgs/replayed.img" --tools "$tools" --crash-log "$work/guest-crash.log" \
  --replay-log "$work/guest-replayed.log" --payload1 "$pay1" --payload2 "$pay2" --fsync-route "$route"
echo "PROBE COMPLETE"

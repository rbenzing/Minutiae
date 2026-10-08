#!/bin/bash
# Measure what the recovery-fixture toolchain can do. Run inside the
# minutiae-recover-fixtures image (see README.md, "Recovery fixtures: toolchain
# probe"):
#   docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/recover_probe.sh
# No privileges: the guest kernel is booted under QEMU TCG (software emulation,
# no KVM) and every modprobe/insmod happens INSIDE that guest, never on the
# host or the Docker VM kernel. The script only measures; it asserts nothing
# about what a deleted file leaves behind (the probes of 3B-3H do).
set -eu

work=$(mktemp -d /tmp/recover-probe.XXXXXX)
trap 'rm -rf "$work"' EXIT
fsl="ext4 f2fs exfat vfat"

echo "== tool versions"
qemu-system-x86_64 --version | head -1
for t in "mke2fs -V" "mkfs.f2fs -V" "mkfs.exfat -V" "mkfs.fat --help" "mcopy --version" "sqlite3 --version" "faketime --version" "python3 --version" "jq --version" "gzip --version" "debugfs -V" "dump.exfat -V" "fsck.f2fs -V" "fsck.fat --help"; do
  echo "-- $t"
  sh -c "$t" 2>&1 | head -2 || true
done
echo "-- busybox"
busybox | head -1 || true
echo "-- exfat-fuse"
dpkg -s exfat-fuse | grep -E '^(Package|Version)' || true

kver=$(ls /lib/modules | head -1)
echo "== kernel"
echo "kernel version: $kver"
ls -l /boot/vmlinuz-"$kver"
echo "-- /lib/modules/$kver/kernel/fs"
ls /lib/modules/"$kver"/kernel/fs
echo "-- built in or module (modules.dep / modules.builtin)"
for mn in ext4 f2fs exfat vfat fat; do
  if grep -q "/${mn}\.ko" /lib/modules/"$kver"/modules.dep; then k=module
  elif grep -q "/${mn}\.ko" /lib/modules/"$kver"/modules.builtin; then k=built-in
  else k=absent; fi
  echo "$mn: $k"
done
grep -E 'CONFIG_(EXT4_FS|F2FS_FS|EXFAT_FS|VFAT_FS|FAT_FS|F2FS_FS_COMPRESSION)=' /boot/config-"$kver" || true

echo "== faketime on the host side (mkfs run twice at 2023-11-14 22:13:20 UTC)"
ft="2023-11-14 22:13:20"
mk() { # name, mkfs command (the image path is appended)
  n=$1; shift
  for i in 1 2; do
    rm -f "$work/ft-$n-$i.img"; truncate -s 64M "$work/ft-$n-$i.img"
    faketime -f "$ft" "$@" "$work/ft-$n-$i.img" >/dev/null 2>&1 || { echo "$n: mkfs failed under faketime"; return; }
  done
  a=$(sha256sum <"$work/ft-$n-1.img" | cut -c1-16); b=$(sha256sum <"$work/ft-$n-2.img" | cut -c1-16)
  if [ "$a" = "$b" ]; then echo "$n: identical ($a)"; else echo "$n: differ ($a vs $b)"; fi
}
mk ext4 mke2fs -q -t ext4 -U 11111111-2222-3333-4444-555555555555 -E hash_seed=11111111-2222-3333-4444-555555555555
mk f2fs mkfs.f2fs -q -U 11111111-2222-3333-4444-555555555555
mk exfat mkfs.exfat
mk vfat mkfs.fat -i 12345678
mk vfat-noid mkfs.fat
rm -f "$work"/ft-*.img
faketime -f "$ft" date -u +'faketime date: %F %T'
truncate -s 64M "$work/t.img"
faketime -f "$ft" mke2fs -q -t ext4 -F "$work/t.img" >/dev/null 2>&1 || true
echo "ext4 under faketime: $(dumpe2fs -h "$work/t.img" 2>/dev/null | grep -i 'Filesystem created')"
rm -f "$work/t.img"

echo "== guest"
rd=$work/rd
mkdir -p "$rd"/bin "$rd"/dev "$rd"/proc "$rd"/sys "$rd"/mnt "$rd"/mods
cp "$(command -v busybox)" "$rd/bin/busybox"
for a in sh mount umount insmod date sleep poweroff cat mkdir rm sync echo dd ls cut tr sed; do ln -s busybox "$rd/bin/$a"; done
: >"$rd/modules.list"
for m in virtio_blk virtio_pci ext4 f2fs exfat vfat nls_cp437 nls_ascii nls_utf8 nls_iso8859-1; do
  deps=$(modprobe -S "$kver" -d / --show-depends "$m" 2>/dev/null | awk '$1=="insmod"{print $2}') || true
  echo "module $m: ${deps:-(none: built in or absent)}" | tr '\n' ' '; echo
  for p in $deps; do
    b=$(basename "$p")
    [ -e "$rd/mods/$b" ] || { cp "$p" "$rd/mods/$b"; echo "/mods/$b" >>"$rd/modules.list"; }
  done
done
# busybox insmod cannot read compressed modules: unpack them
for z in "$rd"/mods/*.xz; do [ -e "$z" ] && xz -d "$z"; done
for z in "$rd"/mods/*.zst; do [ -e "$z" ] && zstd -d --rm -q "$z"; done
sed -i -E 's/\.(xz|zst)$//' "$rd/modules.list"

for fs in $fsl; do
  img=$work/disk-$fs.img
  rm -f "$img"; truncate -s 64M "$img"
  case $fs in
    ext4) mke2fs -q -t ext4 -F "$img" ;;
    f2fs) mkfs.f2fs -q -f "$img" ;;
    exfat) mkfs.exfat "$img" >/dev/null ;;
    vfat) mkfs.fat -F 32 "$img" >/dev/null ;;
  esac
done
echo "(the guest has no mkfs binaries (they are dynamically linked): the host formats each disk; the guest mounts, writes, deletes, unmounts)"

cat >"$rd/init" <<'EOT'
#!/bin/sh
r() { echo "PROBE $*"; }
mount -t proc none /proc; mount -t sysfs none /sys; mount -t devtmpfs none /dev; mkdir -p /tmp
r "uptime-at-init $(cat /proc/uptime)"
while read -r m; do insmod "$m" 2>&1 | sed "s|^|PROBE insmod $m: |"; done </modules.list
r "uptime-after-modules $(cat /proc/uptime)"
i=0; while [ ! -e /dev/vda ] && [ $i -lt 50 ]; do sleep 0.2; i=$((i+1)); done
r "block devices: $(ls /dev/vd* 2>&1)"
date -s "2023-11-14 22:13:20" >/dev/null 2>&1 && r "clock-set-ok $(date -u '+%F %T')" || r "clock-set-failed"
r "clock-after-1s-sleep $(sleep 1; date -u '+%F %T')"
sed 's/^/PROBE fs-registered: /' /proc/filesystems
n=0
for fs in ext4 f2fs exfat vfat; do
  dev=/dev/vd$(echo a b c d | cut -d' ' -f$((n+1))); n=$((n+1))
  mkdir -p /mnt/$fs
  if mount -t $fs $dev /mnt/$fs 2>/tmp/err; then r "$fs mount=ok"; else r "$fs mount=FAILED $(cat /tmp/err)"; continue; fi
  dd if=/dev/urandom of=/mnt/$fs/probe.bin bs=4096 count=3 2>/dev/null && r "$fs write=ok size=$(ls -l /mnt/$fs/probe.bin | tr -s ' ' | cut -d' ' -f5)" || r "$fs write=FAILED"
  sync; r "$fs sync1=ok"
  rm /mnt/$fs/probe.bin && r "$fs delete=ok" || r "$fs delete=FAILED"
  sync; r "$fs sync2=ok"
  umount /mnt/$fs && r "$fs umount=ok" || r "$fs umount=FAILED"
done
r "uptime-end $(cat /proc/uptime)"
echo PROBE-GUEST-DONE
poweroff -f
EOT
chmod +x "$rd/init"
(cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -9) >"$work/rd.cpio.gz"

drives=""
for fs in $fsl; do drives="$drives -drive file=$work/disk-$fs.img,format=raw,if=virtio"; done
start=$(date +%s)
# shellcheck disable=SC2086
timeout 900 qemu-system-x86_64 -accel tcg -m 512 -nographic -no-reboot \
  -kernel "/boot/vmlinuz-$kver" -initrd "$work/rd.cpio.gz" \
  -append "console=ttyS0 panic=-1 loglevel=4" $drives >"$work/guest.log" 2>&1 || echo "qemu exit status $?"
end=$(date +%s)
grep -a '^PROBE' "$work/guest.log" || true
grep -aq PROBE-GUEST-DONE "$work/guest.log" || { echo "guest did not finish"; tail -n 30 "$work/guest.log"; }
echo "TCG guest wall time (qemu start to exit): $((end - start)) s"
echo "-- guest log lines mentioning errors"
grep -aiE 'error|fail|unknown filesystem' "$work/guest.log" | grep -av '^PROBE' | head -10 || true

echo "== host-side read back of the disks after the guest (read-only)"
fsck.ext4 -n "$work/disk-ext4.img" 2>&1 | tail -2 || true
cp "$work/disk-f2fs.img" "$work/f2fs-copy.img"  # fsck.f2fs has no dry-run switch: check a copy
fsck.f2fs "$work/f2fs-copy.img" 2>&1 | tail -6 || true
cmp "$work/disk-f2fs.img" "$work/f2fs-copy.img" && echo "f2fs fsck left the copy unchanged" || echo "f2fs fsck changed the copy"
fsck.fat -n "$work/disk-vfat.img" 2>&1 | tail -2 || true
dump.exfat "$work/disk-exfat.img" 2>&1 | grep -iE 'free clusters|volume serial' || true
debugfs -R 'ls -d /' "$work/disk-ext4.img" 2>&1 | head -3 || true

echo "== unclean quit: the guest writes and does NOT sync or unmount; the host sends quit to the QEMU monitor"
img=$work/crash.img
rm -f "$img"; truncate -s 64M "$img"; mke2fs -q -t ext4 -F "$img"
cat >"$rd/init" <<'EOT'
#!/bin/sh
mount -t proc none /proc; mount -t sysfs none /sys; mount -t devtmpfs none /dev; mkdir -p /tmp; i=0; while [ ! -e /dev/vda ] && [ $i -lt 50 ]; do sleep 0.2; i=$((i+1)); done
while read -r m; do insmod "$m" 2>/dev/null; done </modules.list
i=0; while [ ! -e /dev/vda ] && [ $i -lt 50 ]; do sleep 0.2; i=$((i+1)); done
mkdir -p /mnt; mount -t ext4 /dev/vda /mnt || echo PROBE-CRASH-MOUNT-FAILED
dd if=/dev/urandom of=/mnt/crash.bin bs=4096 count=3 2>/dev/null
echo PROBE-CRASH-READY
sleep 600
EOT
(cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -9) >"$work/rd2.cpio.gz"
sock=$work/mon.sock
timeout 300 qemu-system-x86_64 -accel tcg -m 512 -nographic -no-reboot \
  -kernel "/boot/vmlinuz-$kver" -initrd "$work/rd2.cpio.gz" \
  -append "console=ttyS0 panic=-1 loglevel=4" -drive "file=$img,format=raw,if=virtio" \
  -monitor "unix:$sock,server,nowait" >"$work/guest2.log" 2>&1 &
qpid=$!
for _ in $(seq 1 120); do grep -aq PROBE-CRASH-READY "$work/guest2.log" 2>/dev/null && break; sleep 1; done
grep -a PROBE-CRASH "$work/guest2.log" || echo "no PROBE-CRASH-READY seen"
python3 - "$sock" <<'PY'
import socket, sys, time
s = socket.socket(socket.AF_UNIX); s.settimeout(10); s.connect(sys.argv[1])
time.sleep(0.5); s.recv(4096); s.sendall(b"quit\n"); print("monitor: sent quit")
PY
for _ in $(seq 1 20); do kill -0 $qpid 2>/dev/null || break; sleep 1; done
if kill -0 $qpid 2>/dev/null; then echo "qemu still running after quit; killing"; kill -9 $qpid; fi
wait $qpid 2>/dev/null || true
echo "-- crash image read back (fsck.ext4 -n; never writes):"
fsck.ext4 -n "$img" 2>&1 | tail -4 || true
echo "PROBE COMPLETE"

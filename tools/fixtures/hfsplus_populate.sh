#!/bin/bash
# Populate a freshly formatted HFS+ image with a REAL kernel driver, inside a
# throwaway virtual machine. Usage: hfsplus_populate.sh <image> <work dir>
#
# Docker Desktop's kernel has no hfsplus module, so the container boots the
# Debian kernel installed in the image (which does have it) under QEMU (software
# emulation: /dev/kvm is not needed), with a minimal initramfs: busybox, the
# modules virtio_blk/hfsplus/nls_utf8 and the tarballs made by hfsplus_tree.py.
# The guest mounts the image read-write, extracts phase1.tar (the tree plus 400
# one-block filler files), deletes the odd fillers, extracts phase2.tar (a
# 150-block file that fills the holes, so it has many extents and needs
# extents-overflow records), unmounts and powers off. Hard links make the
# driver create the private folder. The virtual clock is frozen to the
# fixture clock (-rtc clock=vm with -icount), so every date the driver stamps
# is deterministic.
#
# Run as root in the minutiae-fixtures-hfs image (see README.md, "HFS+ fixtures").
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
img=${1:?image}
work=${2:?work dir}
clock_iso=2023-11-14T22:13:20
clock_set="2023-11-14 22:13:20"

for tool in qemu-system-x86_64 busybox cpio gzip modprobe python3; do
  command -v "$tool" >/dev/null || { echo "hfsplus_populate.sh needs $tool (the minutiae-fixtures-hfs image has it)" >&2; exit 1; }
done
kver=$(ls /lib/modules | head -1)
[ -f "/boot/vmlinuz-$kver" ] || { echo "no kernel /boot/vmlinuz-$kver" >&2; exit 1; }

rd=$work/rd
rm -rf "$rd" "$work/tree"
mkdir -p "$rd"/{bin,dev,proc,sys,mnt,mods}
python3 "$here/hfsplus_tree.py" "$work/tree"

cp "$(command -v busybox)" "$rd/bin/busybox"
for a in sh mount umount insmod date poweroff cat tar mkdir rm sync echo; do ln -s busybox "$rd/bin/$a"; done
: >"$rd/modules.list"
for m in hfsplus nls_utf8 virtio_blk virtio_pci; do
  modprobe -S "$kver" -d / --show-depends "$m" | awk '{print $2}' | while read -r p; do
    b=$(basename "$p")
    [ -e "$rd/mods/$b" ] || { cp "$p" "$rd/mods/$b"; echo "/mods/$b" >>"$rd/modules.list"; }
  done
done
cp "$work/tree/phase1.tar" "$work/tree/phase2.tar" "$work/tree/rmlist" "$rd/"
cat >"$rd/init" <<EOT
#!/bin/sh
fail() { echo "MINUTIAE-GUEST-FAIL: \$*"; poweroff -f; }
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev
while read -r m; do insmod "\$m" || fail "insmod \$m"; done </modules.list
date -s "$clock_set" >/dev/null
mount -t hfsplus /dev/vda /mnt || fail mount
cd /mnt || fail cd
tar xf /phase1.tar || fail "extract phase 1"
while read -r p; do rm "./\$p" || fail "rm \$p"; done </rmlist
tar xf /phase2.tar || fail "extract phase 2"
cd /
sync
umount /mnt || fail umount
echo MINUTIAE-GUEST-OK
poweroff -f
EOT
chmod +x "$rd/init"
(cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -9) >"$work/rd.cpio.gz"

timeout 900 qemu-system-x86_64 -accel tcg -m 512 -nographic -no-reboot \
  -kernel "/boot/vmlinuz-$kver" -initrd "$work/rd.cpio.gz" \
  -append "console=ttyS0 panic=-1 loglevel=4" \
  -drive "file=$img,format=raw,if=virtio" \
  -rtc "base=$clock_iso,clock=vm" -icount shift=0,sleep=off,align=off >"$work/guest.log" 2>&1 || true
if ! grep -aq MINUTIAE-GUEST-OK "$work/guest.log"; then
  tail -n 40 "$work/guest.log" >&2
  echo "the guest did not finish (no MINUTIAE-GUEST-OK)" >&2
  exit 1
fi
if grep -aq "MINUTIAE-GUEST-FAIL" "$work/guest.log"; then
  grep -a "MINUTIAE-GUEST-FAIL" "$work/guest.log" >&2
  exit 1
fi

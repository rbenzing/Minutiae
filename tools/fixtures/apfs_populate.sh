#!/bin/bash
# Populate a freshly formatted APFS image with the REAL linux-apfs-rw kernel
# driver, inside a throwaway virtual machine. Usage: apfs_populate.sh <image> <work dir>
#
# Docker Desktop's kernel has no apfs module (and none may be loaded into it),
# so the container boots the Debian kernel installed in the image (the very one
# the module was built against) under QEMU (software emulation: /dev/kvm is not
# needed), with a minimal initramfs: busybox, a static helper (apfs_guest.c),
# the modules virtio_blk, libcrc32c and apfs, and the tarballs and operation
# lists made by apfs_tree.py. The guest mounts the image read-write (-o
# readwrite), extracts phase 1 (the tree), completes it with apfs_guest (two
# files written alternately block by block so both are fragmented, sparse
# files, a clone, extended attributes, modes, owners, times), takes the
# snapshot "snap1", then deletes, rewrites and adds files, unmounts and powers
# off. The virtual clock is frozen to the fixture clock (-rtc clock=vm with
# -icount), so every date the driver stamps is deterministic.
#
# Run as root in the minutiae-fixtures-apfs image (see README.md).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
img=${1:?image}
work=${2:?work dir}
clock_iso=2023-11-14T22:13:20
clock_set="2023-11-14 22:13:20"

for tool in qemu-system-x86_64 busybox cpio gzip modprobe python3 gcc; do
  command -v "$tool" >/dev/null || { echo "apfs_populate.sh needs $tool (the minutiae-fixtures-apfs image has it)" >&2; exit 1; }
done
kver=$(ls /lib/modules | head -1)
[ -f "/boot/vmlinuz-$kver" ] || { echo "no kernel /boot/vmlinuz-$kver" >&2; exit 1; }
[ -f /usr/local/lib/apfs/apfs.ko ] || { echo "no /usr/local/lib/apfs/apfs.ko" >&2; exit 1; }

rd=$work/rd
rm -rf "$rd" "$work/tree"
mkdir -p "$rd"/{bin,dev,proc,sys,mnt,mods}
python3 "$here/apfs_tree.py" "$work/tree"
gcc -O2 -static -o "$rd/bin/apfs_guest" "$here/apfs_guest.c"

cp "$(command -v busybox)" "$rd/bin/busybox"
for a in sh mount umount insmod date poweroff cat tar mkdir rm sync echo dmesg; do ln -s busybox "$rd/bin/$a"; done
: >"$rd/modules.list"
for m in virtio_blk virtio_pci libcrc32c; do
  modprobe -S "$kver" -d / --show-depends "$m" | awk '{print $2}' | while read -r p; do
    b=$(basename "$p")
    case $b in crc32c-intel.ko*) continue ;; esac # needs CPU features that software emulation lacks; crc32c_generic serves
    [ -e "$rd/mods/$b" ] || { cp "$p" "$rd/mods/$b"; echo "/mods/$b" >>"$rd/modules.list"; }
  done
done
cp /usr/local/lib/apfs/apfs.ko "$rd/mods/apfs.ko"
echo /mods/apfs.ko >>"$rd/modules.list"
for f in phase1.tar phase3.tar ops1 ops2 ops3; do cp "$work/tree/$f" "$rd/$f"; done
cat >"$rd/init" <<EOT
#!/bin/sh
fail() { echo "MINUTIAE-GUEST-FAIL: \$*"; dmesg | tail -n 30; poweroff -f; }
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev
while read -r m; do insmod "\$m" || fail "insmod \$m"; done </modules.list
date -s "$clock_set" >/dev/null
umask 0
mount -t apfs -o readwrite /dev/vda /mnt || fail mount
cd /mnt || fail cd
tar xf /phase1.tar || fail "extract phase 1"
apfs_guest /ops1 || fail "ops1"
apfs_guest /ops2 || fail "ops2"
tar xf /phase3.tar || fail "extract phase 3"
apfs_guest /ops3 || fail "ops3"
cd /
sync
umount /mnt || fail umount
echo MINUTIAE-GUEST-OK
poweroff -f
EOT
chmod +x "$rd/init"
(cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -9) >"$work/rd.cpio.gz"

timeout 1500 qemu-system-x86_64 -accel tcg -m 1024 -nographic -no-reboot \
  -kernel "/boot/vmlinuz-$kver" -initrd "$work/rd.cpio.gz" \
  -append "console=ttyS0 panic=-1 loglevel=4" \
  -drive "file=$img,format=raw,if=virtio" \
  -rtc "base=$clock_iso,clock=vm" -icount shift=0,sleep=off,align=off >"$work/guest.log" 2>&1 || true
if grep -aq "MINUTIAE-GUEST-FAIL" "$work/guest.log"; then
  grep -a -A30 "MINUTIAE-GUEST-FAIL" "$work/guest.log" >&2
  exit 1
fi
if ! grep -aq MINUTIAE-GUEST-OK "$work/guest.log"; then
  tail -n 40 "$work/guest.log" >&2
  echo "the guest did not finish (no MINUTIAE-GUEST-OK)" >&2
  exit 1
fi

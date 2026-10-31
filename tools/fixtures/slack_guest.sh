#!/bin/bash
# Guest library of the slack fixtures (sourced by slack_probe.sh and, later, slack.sh; never
# run on its own). Self-contained, like casefold_guest.sh: it does not source the recovery
# fixtures' guest library. Runs as root INSIDE the minutiae-recover-fixtures image, no
# privileges, no --privileged container. The kernel vfat, exfat, ext4 and F2FS drivers run
# only in the QEMU guest (TCG, no KVM); the guest's own /init loads its modules and does
# every file operation. The host side only lists module files, builds the initramfs and boots
# the guest.
#
#   slack_initrd <rd dir> <payload dir> <ops file>
#   slack_run <rd dir> <log> <img1> <img2> <img3> <img4>
#
# Four virtio drives: vda is the FAT image (mounted on /mnt/fat as vfat), vdb the exFAT image
# (/mnt/exfat), vdc the ext4 image (/mnt/ext4), vdd the F2FS image (/mnt/f2fs). ONE boot works
# on all four. The ops file holds one shell command per line, run in order inside the guest
# with the payload files in /payload; a failing command ends the boot.
#
# Guest log lines (all start with G, OP or GUEST):
#   G <text>            diagnostics: /proc/filesystems, the exit status of each module load
#                       and of each mount and unmount
#   OP <n> <command>    the command about to run
#   OP-FAIL <n>         the command failed (or OP-FAIL umount <fs>); the run is a failure
#   GUEST-MOUNT-FAIL <fs>   a volume would not mount (the boot ends)
#   GUEST-DONE          the run reached its end

kver=$(ls /lib/modules | head -1)

slack_initrd() {
  local rd=$1 payload=$2 ops=$3 m deps listing p b z
  rm -rf "$rd"
  mkdir -p "$rd"/bin "$rd"/dev "$rd"/proc "$rd"/sys "$rd"/mnt "$rd"/mods "$rd"/payload "$rd"/tmp
  cp "$(command -v busybox)" "$rd/bin/busybox"
  # the applets (sh mount umount date sleep poweroff cat mkdir rm cp mv sync echo dd ls head truncate
  # and the module loader) are linked by the guest's own /init with busybox --install
  : >"$rd/modules.list"
  # ext4 and f2fs may be built in: a "builtin" line is accepted, the guest decides success by the mounts
  for m in virtio_blk virtio_pci fat vfat nls_cp437 nls_ascii nls_utf8 exfat ext4 f2fs; do
    listing=$(modprobe -S "$kver" -d / --show-depends "$m" 2>&1) || { echo "slack_initrd: cannot resolve module $m: $listing" >&2; return 1; }
    printf '%s\n' "$listing" | grep -Eq '^(insmod|builtin) ' || { echo "slack_initrd: module $m resolved to nothing" >&2; return 1; }
    deps=$(printf '%s\n' "$listing" | awk '$1=="insmod"{print $2}')
    for p in $deps; do
      b=$(basename "$p")
      [ -e "$rd/mods/$b" ] || { cp "$p" "$rd/mods/$b"; echo "/mods/$b" >>"$rd/modules.list"; }
    done
  done
  # the guest's busybox cannot read compressed modules: unpack them
  for z in "$rd"/mods/*.xz; do [ -e "$z" ] && xz -d "$z"; done
  for z in "$rd"/mods/*.zst; do [ -e "$z" ] && zstd -d --rm -q "$z"; done
  sed -i -E 's/\.(xz|zst)$//' "$rd/modules.list"
  cp "$ops" "$rd/ops"
  [ -d "$payload" ] && cp -r "$payload"/. "$rd/payload/"
  cat >"$rd/init" <<'EOT'
#!/bin/busybox sh
/bin/busybox --install -s /bin
mount -t proc none /proc; mount -t sysfs none /sys; mount -t devtmpfs none /dev; mkdir -p /tmp
echo "G kernel: $(cat /proc/version)"
date -s "2023-11-14 22:13:20" >/dev/null 2>&1
while read -r m; do insmod "$m" 2>&1; echo "G insmod $m rc=$?"; done </modules.list
echo "G /proc/filesystems:"; cat /proc/filesystems
i=0; while [ ! -e /dev/vdd ] && [ $i -lt 100 ]; do sleep 0.2; i=$((i+1)); done
echo "G block devices: $(ls /dev/vd*)"
mkdir -p /mnt/fat /mnt/exfat /mnt/ext4 /mnt/f2fs
mount -t vfat /dev/vda /mnt/fat || { echo "GUEST-MOUNT-FAIL vfat"; poweroff -f; }
mount -t exfat /dev/vdb /mnt/exfat || { echo "GUEST-MOUNT-FAIL exfat"; poweroff -f; }
mount -t ext4 /dev/vdc /mnt/ext4 || { echo "GUEST-MOUNT-FAIL ext4"; poweroff -f; }
mount -t f2fs /dev/vdd /mnt/f2fs || { echo "GUEST-MOUNT-FAIL f2fs"; poweroff -f; }
echo "G mounted: $(grep -E ' /mnt/' /proc/mounts | tr '\n' ';')"
n=0
while IFS= read -r cmd; do
  n=$((n+1)); echo "OP $n $cmd"
  sh -c "$cmd" </dev/null || { echo "OP-FAIL $n"; poweroff -f; }
done </ops
sync
for fsn in fat exfat ext4 f2fs; do
  umount /mnt/$fsn; rc=$?; echo "G umount $fsn rc=$rc"
  [ $rc -eq 0 ] || echo "OP-FAIL umount $fsn"
done
echo GUEST-DONE
poweroff -f
EOT
  chmod +x "$rd/init"
}

slack_run() {
  local rd=$1 log=$2 img1=$3 img2=$4 img3=$5 img4=$6
  (cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -1) >"$rd.cpio.gz"
  timeout 1200 qemu-system-x86_64 -accel tcg -m 512 -smp 1 -nographic -no-reboot \
    -kernel "/boot/vmlinuz-$kver" -initrd "$rd.cpio.gz" \
    -append "console=ttyS0 panic=-1 loglevel=4" \
    -drive "file=$img1,format=raw,if=virtio" -drive "file=$img2,format=raw,if=virtio" \
    -drive "file=$img3,format=raw,if=virtio" -drive "file=$img4,format=raw,if=virtio" >"$log" 2>&1 || echo "qemu exit status $?" >&2
  grep -a -e '^G ' -e 'OP [0-9]' -e 'OP-FAIL' -e 'GUEST-' "$log" || true
  grep -aq 'GUEST-DONE' "$log" || { echo "guest did not finish: $log" >&2; tail -n 20 "$log" >&2; return 1; }
  if grep -aq 'OP-FAIL' "$log"; then echo "guest op failed: $log" >&2; return 1; fi
}

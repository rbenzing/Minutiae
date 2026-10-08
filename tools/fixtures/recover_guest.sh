#!/bin/bash
# Shared guest library of the 3B recovery fixtures (sourced, never run on its own).
# Runs as root INSIDE the minutiae-recover-fixtures image; no privileges, no
# --privileged container. The kernel vfat/exfat driver runs only in the QEMU
# guest (TCG, no KVM); the guest's own /init loads its modules. The host side
# only lists module files and boots the guest. Modelled on recover_probe.sh.
#
#   guest_initrd <rd dir> <fstype: vfat|exfat> <mount opts> <payload dir> <ops file>
#   guest_run <img> <rd dir> <log>
#
# The ops file holds one shell command per line, run in order inside the guest
# with the volume mounted on /mnt and the payload files in /payload.

kver=$(ls /lib/modules | head -1)

guest_initrd() {
  local rd=$1 fstype=$2 opts=$3 payload=$4 ops=$5 m deps p b z
  rm -rf "$rd"
  mkdir -p "$rd"/bin "$rd"/dev "$rd"/proc "$rd"/sys "$rd"/mnt "$rd"/mods "$rd"/payload
  cp "$(command -v busybox)" "$rd/bin/busybox"
  # the applets (sh mount umount date sleep poweroff cat mkdir rm cp mv sync echo dd ls tr cut sed truncate tar
  # and the module loader) are linked by the guest's own /init with busybox --install
  case $fstype in
    vfat) set -- virtio_blk virtio_pci fat vfat nls_cp437 nls_ascii nls_utf8 ;;
    exfat) set -- virtio_blk virtio_pci exfat ;;
    *) echo "guest_initrd: unknown fstype $fstype" >&2; return 1 ;;
  esac
  : >"$rd/modules.list"
  for m in "$@"; do
    deps=$(modprobe -S "$kver" -d / --show-depends "$m" 2>/dev/null | awk '$1=="insmod"{print $2}') || true
    for p in $deps; do
      b=$(basename "$p")
      [ -e "$rd/mods/$b" ] || { cp "$p" "$rd/mods/$b"; echo "/mods/$b" >>"$rd/modules.list"; }
    done
  done
  # the guest's busybox cannot read compressed modules: unpack them
  for z in "$rd"/mods/*.xz; do [ -e "$z" ] && xz -d "$z"; done
  for z in "$rd"/mods/*.zst; do [ -e "$z" ] && zstd -d --rm -q "$z"; done
  sed -i -E 's/\.(xz|zst)$//' "$rd/modules.list"
  echo "$fstype" >"$rd/fstype"
  echo "$opts" >"$rd/mntopts"
  cp "$ops" "$rd/ops"
  [ -d "$payload" ] && cp -r "$payload"/. "$rd/payload/"
  cat >"$rd/init" <<'EOT'
#!/bin/busybox sh
/bin/busybox --install -s /bin
mount -t proc none /proc; mount -t sysfs none /sys; mount -t devtmpfs none /dev; mkdir -p /tmp
fstype=$(cat /fstype); opts=$(cat /mntopts)
while read -r m; do insmod "$m" 2>&1 | sed "s|^|GUEST insmod $m: |"; done </modules.list
i=0; while [ ! -e /dev/vda ] && [ $i -lt 50 ]; do sleep 0.2; i=$((i+1)); done
date -s "2023-11-14 22:13:20" >/dev/null 2>&1
if [ -n "$opts" ]; then o="-o $opts"; else o=""; fi
# shellcheck disable=SC2086
mount -t "$fstype" $o /dev/vda /mnt || { echo "GUEST-MOUNT-FAIL"; poweroff -f; }
n=0
while IFS= read -r cmd; do
  n=$((n+1)); echo "OP $n $cmd"
  sh -c "$cmd" </dev/null || { echo "OP-FAIL $n"; poweroff -f; }
done </ops
sync
umount /mnt || echo "OP-FAIL umount"
echo GUEST-DONE
poweroff -f
EOT
  chmod +x "$rd/init"
}

guest_run() {
  local img=$1 rd=$2 log=$3
  (cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -9) >"$rd.cpio.gz"
  timeout 900 qemu-system-x86_64 -accel tcg -m 512 -smp 1 -nographic -no-reboot \
    -kernel "/boot/vmlinuz-$kver" -initrd "$rd.cpio.gz" \
    -append "console=ttyS0 panic=-1 loglevel=4" \
    -drive "file=$img,format=raw,if=virtio" >"$log" 2>&1 || echo "qemu exit status $?" >&2
  grep -a -e 'OP [0-9]' -e 'OP-FAIL' -e 'GUEST-' "$log" || true
  grep -aq 'GUEST-DONE' "$log" || { echo "guest did not finish: $log" >&2; tail -n 20 "$log" >&2; return 1; }
  if grep -aq 'OP-FAIL' "$log"; then echo "guest op failed: $log" >&2; return 1; fi
}

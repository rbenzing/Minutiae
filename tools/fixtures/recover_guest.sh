#!/bin/bash
# Shared guest library of the 3B recovery fixtures (sourced, never run on its own).
# Runs as root INSIDE the minutiae-recover-fixtures image; no privileges, no
# --privileged container. The kernel vfat/exfat driver runs only in the QEMU
# guest (TCG, no KVM); the guest's own /init loads its modules. The host side
# only lists module files and boots the guest. Modelled on recover_probe.sh.
#
#   guest_initrd <rd dir> <fstype: vfat|exfat|ext4|f2fs> <mount opts> <payload dir> <ops file> [guest clock]
#   guest_run <img> <rd dir> <log>
#
# guest_run reads two optional variables: GUEST_DRIVE_OPTS is appended to the -drive
# value (empty by default; for example ",discard=unmap") and GUEST_TIMEOUT is the
# time limit in seconds (default 900; the F2FS fill-to-ENOSPC stage sets 2400).
#
# The ops file holds one shell command per line, run in order inside the guest
# with the volume mounted on /mnt and the payload files in /payload. The guest
# clock defaults to 2023-11-14 22:13:20 (the 3B fixtures); a sixth argument sets
# another one.

kver=${GUEST_KVER:-$(ls /lib/modules | head -1)}

# module_listing <name>: "insmod <file>" lines (the walker prints "dep <file>"; dependencies first) or "builtin <name>", like
# `modprobe --show-depends`. A kernel directory without modules.dep (the 5.10 kernel that
# Dockerfile.recover510 unpacks without installing it) is walked with modinfo -F depends.
module_listing() {
  if [ -e "/lib/modules/$kver/modules.dep" ]; then
    modprobe -S "$kver" -d / --show-depends "$1" 2>&1
    return
  fi
  module_walk "$1"
}
_seen=" "
module_walk() {
  local m=$1 f d
  case "$_seen" in *" $m "*) return 0 ;; esac
  _seen="$_seen$m "
  f=$(find "/lib/modules/$kver" -type f \( -name "$m.ko" -o -name "$m.ko.xz" -o -name "$m.ko.zst" -o -name "${m//_/-}.ko" -o -name "${m//_/-}.ko.xz" \) | head -n 1)
  if [ -z "$f" ]; then
    if grep -Eq "/(${m}|${m//_/-})\.ko" "/lib/modules/$kver/modules.builtin"; then echo "builtin $m"; return 0; fi
    echo "module $m not found in /lib/modules/$kver"
    return 1
  fi
  for d in $(modinfo -F depends "$f" | tr ',' ' '); do module_walk "$d" || return 1; done
  echo "dep $f"
}

guest_initrd() {
  local rd=$1 fstype=$2 opts=$3 payload=$4 ops=$5 clock=${6:-2023-11-14 22:13:20} m deps listing p b z
  rm -rf "$rd"
  mkdir -p "$rd"/bin "$rd"/dev "$rd"/proc "$rd"/sys "$rd"/mnt "$rd"/mods "$rd"/payload
  cp "$(command -v busybox)" "$rd/bin/busybox"
  # the applets (sh mount umount date sleep poweroff cat mkdir rm cp mv sync echo dd ls tr cut sed truncate tar
  # and the module loader) are linked by the guest's own /init with busybox --install
  case $fstype in
    vfat) set -- virtio_blk virtio_pci fat vfat nls_cp437 nls_ascii nls_utf8 ;;
    exfat) set -- virtio_blk virtio_pci exfat ;;
    ext4) set -- virtio_blk virtio_pci crc32c_generic ext4 ;;
    f2fs) set -- virtio_blk virtio_pci f2fs ;;
    *) echo "guest_initrd: unknown fstype $fstype" >&2; return 1 ;;
  esac
  : >"$rd/modules.list"
  for m in "$@"; do
    listing=$(module_listing "$m") || { echo "guest_initrd: cannot resolve module $m: $listing" >&2; return 1; }
    printf '%s\n' "$listing" | grep -Eq '^(insmod|builtin|dep) ' || { echo "guest_initrd: module $m resolved to nothing" >&2; return 1; }
    deps=$(printf '%s\n' "$listing" | awk '$1=="insmod"||$1=="dep"{print $2}')
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
  echo "$clock" >"$rd/clock"
  cp "$ops" "$rd/ops"
  [ -d "$payload" ] && cp -r "$payload"/. "$rd/payload/"
  cat >"$rd/init" <<'EOT'
#!/bin/busybox sh
/bin/busybox --install -s /bin
mount -t proc none /proc; mount -t sysfs none /sys; mount -t devtmpfs none /dev; mkdir -p /tmp
fstype=$(cat /fstype); opts=$(cat /mntopts)
while read -r m; do insmod "$m" 2>&1 | sed "s|^|GUEST insmod $m: |"; done </modules.list
i=0; while [ ! -e /dev/vda ] && [ $i -lt 50 ]; do sleep 0.2; i=$((i+1)); done
date -s "$(cat /clock)" >/dev/null 2>&1
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
  timeout "${GUEST_TIMEOUT:-900}" qemu-system-x86_64 -accel tcg -m 512 -smp 1 -nographic -no-reboot \
    -kernel "/boot/vmlinuz-$kver" -initrd "$rd.cpio.gz" \
    -append "console=ttyS0 panic=-1 loglevel=4" \
    -drive "file=$img,format=raw,if=virtio${GUEST_DRIVE_OPTS:-}" >"$log" 2>&1 || echo "qemu exit status $?" >&2
  grep -a -e 'OP [0-9]' -e 'OP-FAIL' -e 'GUEST-' "$log" || true
  grep -aq 'GUEST-DONE' "$log" || { echo "guest did not finish: $log" >&2; tail -n 20 "$log" >&2; return 1; }
  if grep -aq 'OP-FAIL' "$log"; then echo "guest op failed: $log" >&2; return 1; fi
}

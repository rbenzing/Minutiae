#!/bin/bash
# Shared guest library of the 3B recovery fixtures (sourced, never run on its own).
# Runs as root INSIDE the minutiae-recover-fixtures image; no privileges, no
# --privileged container. The kernel vfat/exfat driver runs only in the QEMU
# guest (TCG, no KVM); the guest's own /init loads its modules. The host side
# only lists module files and boots the guest. Modelled on recover_probe.sh.
#
#   guest_initrd <rd dir> <fstype: vfat|exfat|f2fs> <mount opts> <payload dir> <ops file>
#   guest_run <img> <rd dir> <log>
#
# guest_run reads two optional variables: GUEST_DRIVE_OPTS is appended to the -drive
# value (empty by default; for example ",discard=unmap") and GUEST_TIMEOUT is the
# time limit in seconds (default 900; the F2FS fill-to-ENOSPC stage sets 2400).
#
# Crash mode (plan 3H): GUEST_CRASH=1 makes the guest print GUEST-CRASH-READY after the last op
# and sleep, with no sync and no unmount; guest_run then starts QEMU with a monitor socket,
# waits for the marker (at most GUEST_TIMEOUT seconds), sends "quit" through the socket (the
# recover_probe.sh recipe: the guest dies with its dirty state), waits up to 20 s, kills QEMU if
# it is still there and fails when the marker never came or QEMU is not gone. GUEST_CRASH must
# have the same value for guest_initrd and guest_run.
#
# The ops file holds one shell command per line, run in order inside the guest
# with the volume mounted on /mnt and the payload files in /payload.

kver=$(ls /lib/modules | head -1)

guest_initrd() {
  local rd=$1 fstype=$2 opts=$3 payload=$4 ops=$5 m deps listing p b z
  rm -rf "$rd"
  mkdir -p "$rd"/bin "$rd"/dev "$rd"/proc "$rd"/sys "$rd"/mnt "$rd"/mods "$rd"/payload
  cp "$(command -v busybox)" "$rd/bin/busybox"
  # the applets (sh mount umount date sleep poweroff cat mkdir rm cp mv sync echo dd ls tr cut sed truncate tar
  # and the module loader) are linked by the guest's own /init with busybox --install
  case $fstype in
    vfat) set -- virtio_blk virtio_pci fat vfat nls_cp437 nls_ascii nls_utf8 ;;
    exfat) set -- virtio_blk virtio_pci exfat ;;
    f2fs) set -- virtio_blk virtio_pci f2fs ;;
    *) echo "guest_initrd: unknown fstype $fstype" >&2; return 1 ;;
  esac
  : >"$rd/modules.list"
  for m in "$@"; do
    listing=$(modprobe -S "$kver" -d / --show-depends "$m" 2>&1) || { echo "guest_initrd: cannot resolve module $m: $listing" >&2; return 1; }
    printf '%s\n' "$listing" | grep -Eq '^(insmod|builtin) ' || { echo "guest_initrd: module $m resolved to nothing" >&2; return 1; }
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
  echo "$fstype" >"$rd/fstype"
  echo "$opts" >"$rd/mntopts"
  if [ "${GUEST_CRASH:-}" = 1 ]; then echo 1 >"$rd/crash"; fi
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
if [ -e /crash ]; then echo GUEST-CRASH-READY; sleep 600; poweroff -f; fi
sync
umount /mnt || echo "OP-FAIL umount"
echo GUEST-DONE
poweroff -f
EOT
  chmod +x "$rd/init"
}

guest_run() {
  local img=$1 rd=$2 log=$3 sock qpid i
  (cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -9) >"$rd.cpio.gz"
  if [ "${GUEST_CRASH:-}" = 1 ]; then
    sock=$rd.mon
    rm -f "$sock"
    qemu-system-x86_64 -accel tcg -m 512 -smp 1 -nographic -no-reboot \
      -kernel "/boot/vmlinuz-$kver" -initrd "$rd.cpio.gz" \
      -append "console=ttyS0 panic=-1 loglevel=4" \
      -drive "file=$img,format=raw,if=virtio${GUEST_DRIVE_OPTS:-}" \
      -monitor "unix:$sock,server,nowait" >"$log" 2>&1 &
    qpid=$!
    i=0
    while [ $i -lt "${GUEST_TIMEOUT:-900}" ]; do
      grep -aq 'GUEST-CRASH-READY' "$log" 2>/dev/null && break
      kill -0 "$qpid" 2>/dev/null || break
      sleep 1; i=$((i + 1))
    done
    if grep -aq 'GUEST-CRASH-READY' "$log"; then
      python3 - "$sock" <<'PY'
import socket, sys, time
s = socket.socket(socket.AF_UNIX); s.settimeout(10); s.connect(sys.argv[1])
time.sleep(0.5); s.recv(4096); s.sendall(b"quit\n"); print("monitor: sent quit")
PY
    fi
    i=0
    while [ $i -lt 20 ] && kill -0 "$qpid" 2>/dev/null; do sleep 1; i=$((i + 1)); done
    if kill -0 "$qpid" 2>/dev/null; then echo "qemu still running after quit (or the marker never came); killing" >&2; kill -9 "$qpid"; fi
    wait "$qpid" 2>/dev/null || true
    if kill -0 "$qpid" 2>/dev/null; then echo "qemu process $qpid is still alive" >&2; return 1; fi
    grep -a -e 'OP [0-9]' -e 'OP-FAIL' -e 'GUEST-' "$log" || true
    grep -aq 'GUEST-CRASH-READY' "$log" || { echo "guest never reached the crash marker: $log" >&2; tail -n 20 "$log" >&2; return 1; }
    if grep -aq 'OP-FAIL' "$log"; then echo "guest op failed: $log" >&2; return 1; fi
    return 0
  fi
  timeout "${GUEST_TIMEOUT:-900}" qemu-system-x86_64 -accel tcg -m 512 -smp 1 -nographic -no-reboot \
    -kernel "/boot/vmlinuz-$kver" -initrd "$rd.cpio.gz" \
    -append "console=ttyS0 panic=-1 loglevel=4" \
    -drive "file=$img,format=raw,if=virtio${GUEST_DRIVE_OPTS:-}" >"$log" 2>&1 || echo "qemu exit status $?" >&2
  grep -a -e 'OP [0-9]' -e 'OP-FAIL' -e 'GUEST-' "$log" || true
  grep -aq 'GUEST-DONE' "$log" || { echo "guest did not finish: $log" >&2; tail -n 20 "$log" >&2; return 1; }
  if grep -aq 'OP-FAIL' "$log"; then echo "guest op failed: $log" >&2; return 1; fi
}

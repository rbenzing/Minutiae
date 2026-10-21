#!/bin/bash
# Guest library of the casefold lookup oracle (sourced by casefold.sh, never run
# on its own). Self-contained: it does not source the recovery fixtures' guest
# library. Runs as root INSIDE the minutiae-recover-fixtures image, no privileges.
# The kernel ext4 and F2FS drivers run only in the QEMU guest (TCG, no KVM); the
# guest's own /init loads its modules and does every file operation. The host
# side only lists module files, builds the initramfs and boots the guest.
#
#   casefold_initrd <rd dir> <payload dir>
#   casefold_run <ext4 img> <f2fs img> <rd dir> <log>
#
# Two virtio drives: vda is the ext4 image, vdb the F2FS image. ONE boot answers
# both filesystems. Mount points are per filesystem (/mnt/ext4-cf, /mnt/ext4-plain,
# /mnt/f2fs-cf, /mnt/f2fs-plain); the plain ones are bind mounts of the plain
# directory of each image.
#
# Guest log lines (all start with G or V):
#   G <text>            diagnostics: /proc/filesystems, the exit status of EACH
#                       guest-side step, the attribute listing of every directory
#   V <id> abcd         the verdicts of probe <id>: a/b = ext4 stored present /
#                       query resolves to the stored name, c/d = the same for F2FS
#   GUEST-DONE          the run reached its end

kver=$(ls /lib/modules | head -1)

# The busybox applets the guest init uses. Each must exist in the image's busybox.
# (read is a shell builtin, not an applet.)
casefold_initrd() {
  local rd=$1 payload=$2 m listing deps p b z a lib applets
  rm -rf "$rd"
  mkdir -p "$rd"/bin "$rd"/dev "$rd"/proc "$rd"/sys "$rd"/mnt "$rd"/mods "$rd"/payload "$rd"/tmp
  cp "$(command -v busybox)" "$rd/bin/busybox"
  applets=$(busybox --list) # a variable: grep -q on a pipe would end busybox early (SIGPIPE under pipefail)
  for a in sh mount umount cp ls cat sync echo mkdir rmdir poweroff insmod sleep date; do
    printf '%s\n' "$applets" | grep -qx "$a" || { echo "casefold_initrd: busybox has no applet $a" >&2; return 1; }
    ln -s busybox "$rd/bin/$a"
  done
  # chattr and lsattr are dynamically linked (busybox chattr has no F): copy them
  # with the loader and the shared libraries they name
  mkdir -p "$rd"/sbin
  for a in "$(dirname "$(command -v lsattr)")"/*attr; do
    cp "$a" "$rd/sbin/"
    for lib in $(ldd "$a" | awk '$2=="=>"{print $3} $1 ~ /^\/.*ld-linux/{print $1}'); do
      mkdir -p "$rd$(dirname "$lib")"
      [ -e "$rd$lib" ] || cp -L "$lib" "$rd$lib"
    done
  done
  # modules: ext4 and f2fs may be built in; a "builtin" line is accepted, and the
  # guest decides success by /proc/filesystems and the exit status of each mount
  : >"$rd/modules.list"
  for m in virtio_blk virtio_pci ext4 f2fs; do
    listing=$(modprobe -S "$kver" -d / --show-depends "$m" 2>&1) || { echo "casefold_initrd: cannot resolve module $m: $listing" >&2; return 1; }
    printf '%s\n' "$listing" | grep -Eq '^(insmod|builtin) ' || { echo "casefold_initrd: module $m resolved to nothing" >&2; return 1; }
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
  cp -r "$payload"/. "$rd/payload/"
  cat >"$rd/init" <<'EOT'
#!/bin/busybox sh
PATH=/bin:/sbin
mount -t proc none /proc; mount -t sysfs none /sys; mount -t devtmpfs none /dev
echo "G kernel: $(cat /proc/version)"
date -s "2023-11-14 22:13:20" >/dev/null 2>&1
while read -r m; do insmod "$m" 2>&1; echo "G insmod $m rc=$?"; done </modules.list
echo "G /proc/filesystems:"; cat /proc/filesystems
for fsn in ext4 f2fs; do
  if [ -d /sys/module/$fsn ]; then echo "G module $fsn: present"; else echo "G module $fsn: ABSENT"; fi
done
i=0; while [ ! -e /dev/vdb ] && [ $i -lt 100 ]; do sleep 0.2; i=$((i+1)); done
echo "G block devices: $(ls /dev/vd*)"
mkdir -p /mnt/ext4-cf /mnt/ext4-plain /mnt/f2fs-cf /mnt/f2fs-plain
mount -t ext4 /dev/vda /mnt/ext4-cf; echo "G mount ext4 rc=$?"
mount -t f2fs /dev/vdb /mnt/f2fs-cf; echo "G mount f2fs rc=$?"
# F2FS: the directories are made here. After mkfs the root inode may already be
# casefold (route a: then the new directories inherit it); otherwise chattr +F
# on each EMPTY directory (route b). /plain must NOT be casefold.
mkdir /mnt/f2fs-cf/cf /mnt/f2fs-cf/plain; echo "G f2fs mkdir cf plain rc=$?"
echo "G f2fs root: $(lsattr -d /mnt/f2fs-cf)"
echo "G f2fs cf after mkdir: $(lsattr -d /mnt/f2fs-cf/cf)"
for d in /payload/s/*; do
  b=${d##*/}
  mkdir /mnt/f2fs-cf/cf/$b || echo "G f2fs mkdir cf/$b FAILED"
  chattr +F /mnt/f2fs-cf/cf/$b || echo "G f2fs chattr +F cf/$b FAILED"
done
chattr -F /mnt/f2fs-cf/plain; echo "G f2fs chattr -F plain rc=$?"
echo "G f2fs plain: $(lsattr -d /mnt/f2fs-cf/plain)"
echo "G f2fs cf/00: $(lsattr -d /mnt/f2fs-cf/cf/00)"
echo "G ext4 cf/00: $(lsattr -d /mnt/ext4-cf/cf/00)"
echo "G ext4 plain: $(lsattr -d /mnt/ext4-cf/plain)"
mount --bind /mnt/ext4-cf/plain /mnt/ext4-plain; echo "G bind ext4-plain rc=$?"
mount --bind /mnt/f2fs-cf/plain /mnt/f2fs-plain; echo "G bind f2fs-plain rc=$?"
cp -a /payload/s/. /mnt/ext4-cf/cf/; echo "G cp ext4 cf rc=$?"
cp -a /payload/s/. /mnt/f2fs-cf/cf/; echo "G cp f2fs cf rc=$?"
cp -a /payload/s/00/. /mnt/ext4-plain/; echo "G cp ext4 plain rc=$?"
cp -a /payload/s/00/. /mnt/f2fs-plain/; echo "G cp f2fs plain rc=$?"
sync
# What does the kernel do with the second of two names it calls equal? Create Strasse (U+00DF) then STRASSE
# in a fresh casefold directory of each filesystem, report the status of the second mkdir and the entries
# left, and remove the directory again (the image keeps nothing of it).
for fsn in ext4 f2fs; do
  kd=/mnt/$fsn-cf/kambig
  mkdir $kd; chattr +F $kd; echo "G kambig $fsn setup rc=$?"
  mkdir "$kd/Straße"; echo "G kambig $fsn first rc=$?"
  mkdir "$kd/STRASSE"; echo "G kambig $fsn second rc=$?"
  n=0; for kf in "$kd"/*; do n=$((n+1)); done; echo "G kambig $fsn entries=$n"
  rmdir "$kd"/* 2>/dev/null; rmdir $kd; echo "G kambig $fsn cleanup rc=$?"
done
echo "G probes start: $(cat /proc/uptime)"
for bd in /payload/p/*; do
  b=${bd##*/}
  for d in "$bd"/*; do
    id=${d##*/}
    q=; s=
    for qf in "$d"/q/* "$d"/q/.[!.]* "$d"/q/..?*; do [ -e "$qf" ] || continue; q=${qf##*/}; done
    for sf in "$d"/s/* "$d"/s/.[!.]* "$d"/s/..?*; do [ -e "$sf" ] || continue; s=${sf##*/}; done
    a=0; bb=0; c=0; dd=0
    [ -e "/mnt/ext4-cf/cf/$b/$s" ] && a=1
    [ "/mnt/ext4-cf/cf/$b/$q" -ef "/mnt/ext4-cf/cf/$b/$s" ] && bb=1
    [ -e "/mnt/f2fs-cf/cf/$b/$s" ] && c=1
    [ "/mnt/f2fs-cf/cf/$b/$q" -ef "/mnt/f2fs-cf/cf/$b/$s" ] && dd=1
    echo "V $id $a$bb$c$dd"
  done
done
echo "G probes end: $(cat /proc/uptime)"
sync
umount /mnt/ext4-plain; echo "G umount ext4-plain rc=$?"
umount /mnt/f2fs-plain; echo "G umount f2fs-plain rc=$?"
umount /mnt/ext4-cf; echo "G umount ext4 rc=$?"
umount /mnt/f2fs-cf; echo "G umount f2fs rc=$?"
echo GUEST-DONE
poweroff -f
EOT
  chmod +x "$rd/init"
}

casefold_run() {
  local ext4=$1 f2fs=$2 rd=$3 log=$4
  (cd "$rd" && find . | LC_ALL=C sort | cpio -o -H newc --quiet | gzip -n -1) >"$rd.cpio.gz"
  timeout 1500 qemu-system-x86_64 -accel tcg -m 1024 -smp 1 -nographic -no-reboot \
    -kernel "/boot/vmlinuz-$kver" -initrd "$rd.cpio.gz" \
    -append "console=ttyS0 panic=-1 loglevel=4" \
    -drive "file=$ext4,format=raw,if=virtio" -drive "file=$f2fs,format=raw,if=virtio" >"$log" 2>&1 || echo "qemu exit status $?" >&2
  grep -a '^G ' "$log" || true
  grep -aq 'GUEST-DONE' "$log" || { echo "guest did not finish: $log" >&2; tail -n 20 "$log" >&2; return 1; }
}

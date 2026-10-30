#!/bin/bash
# F2FS RECOVERY fixtures of plan 3D: volumes written and deleted by the KERNEL f2fs driver inside the QEMU
# TCG guest (recover_guest.sh), with an independent oracle (f2fs_recover_oracle.py). No kernel module on the
# host and no --privileged container: the guest's own /init loads the modules and mounts the volume; the host
# only formats (mkfs.f2fs under faketime, f2fs_normalize.py) and READS the finished images (fsck.f2fs,
# dump.f2fs and the oracle's raw decoder).
# Usage: f2fs_recover.sh <outdir>   (normally: gen.sh f2fs-recover, in the minutiae-recover-fixtures image;
# see README.md, "F2FS recovery fixtures (3D)"). No --privileged; all work files live in a container-local
# directory under /tmp inside the container, removed on exit (ruling 3D-R28).
#
# Two images: f2fs-recover-nodiscard (mount nodiscard,noatime) and f2fs-recover-discard (mount
# discard,noatime, QEMU drive discard=unmap). Every family has its own directory. Phase W writes, phase D only
# deletes, each in its own boot; the extra stages exist only for the families the probe (README, "F2FS
# recovery probe (3D)", gates G6) showed the kernel to make. The oracle derives every expectation from the
# finished image and from the bytes the guest was told to write; this script exits 1 naming a missing family.
#
# Stages (STAGES; one function per stage, one guest boot each, on the previous stage's image; 3H adds its
# crash stage to this list):
#   w  write every family; renamed/a, nidreuse/target.bin and reuse/target.bin are written here
#   m  (nodiscard only) mv renamed/a renamed/b; rm nidreuse/target.bin; sync
#   n  (nodiscard only) create nidreuse-w/new1.bin and new2.bin in a fresh boot: the first takes the freed inode number
#   f  (nodiscard only) create /fill; rm -r sac (64 decoy inode numbers) and reuse/target.bin; sync; fill the
#      volume to ENOSPC (GUEST_TIMEOUT=2400): the freed segments of reuse/target.bin are written again
#   d  rm of the targets of every family (only rm), rm -r gone/gonedir, rm renamed/b
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "f2fs_recover.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

out=$(cd "${1:?outdir required}" && pwd)
clock="2023-11-14 22:13:20"
uuid=3d3d3d3d-0000-4000-8000-000000000002
max_gz=1048576 # at most 1 MiB gzipped per image
export LC_ALL=C.UTF-8 NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1

work=$(mktemp -d /tmp/f2fs-recover.XXXXXX)
trap 'rm -rf "$work"' EXIT
mkdir "$work/final"
t0=$(date +%s)

# F2FS_RECOVER_KEEP=<dir> (development only): keep every stage image and its tool captures under <dir>/<variant>/,
# so that the oracle can be run again without the guest stages.
keep() { # keep <variant> <stage> <image>
  [ -n "${F2FS_RECOVER_KEEP:-}" ] || return 0
  mkdir -p "$F2FS_RECOVER_KEEP/$1/img"
  cp --sparse=always "$3" "$F2FS_RECOVER_KEEP/$1/img/$2.img"
  rm -rf "$F2FS_RECOVER_KEEP/$1/tools/$2"
  mkdir -p "$F2FS_RECOVER_KEEP/$1/tools"
  cp -r "$work/$1/tools/$2" "$F2FS_RECOVER_KEEP/$1/tools/$2"
}

# stage <variant> <name> <mount opts> <payload dir> <ops file>: one guest boot on the variant's image, then the
# read-only tools run on the finished image (sha256 before and after; fsck.f2fs -f must exit 0)
stage() {
  local v=$1 name=$2 opts=$3 pay=$4 ops=$5 img=$work/$1/vol.img d=$work/$1/tools/$2 line before after rc
  guest_initrd "$work/rd" f2fs "$opts" "$pay" "$ops"
  guest_run "$img" "$work/rd" "$work/$v/guest-$name.log"
  while IFS= read -r line; do CMDS+=("guest stage $name: $line"); done <"$ops"
  echo "STAGE $v/$name done after $(($(date +%s) - t0)) s"
  mkdir -p "$d"
  before=$(sha256sum "$img")
  (
    cd "$d"
    rc=0; fsck.f2fs -f --dry-run "$img" >fsck-f.txt 2>&1 || rc=$?; echo "$rc" >fsck-f.rc
    fsck.f2fs -t --dry-run "$img" >tree.txt 2>&1 || true
    fsck.f2fs -d 1 -M --dry-run "$img" >map.txt 2>&1 || true
    dump.f2fs -d 1 -s0~-1 "$img" >dump-i.txt 2>&1 || true # writes dump_sit here
    dump.f2fs -n0~-1 "$img" >dump-n.txt 2>&1 || true      # writes dump_nat here
  )
  after=$(sha256sum "$img")
  [ "$before" = "$after" ] || { echo "a read-only tool changed the $v image after stage $name" >&2; exit 1; }
  rc=$(cat "$d/fsck-f.rc")
  [ "$rc" = 0 ] || { cat "$d/fsck-f.txt"; echo "fsck.f2fs -f --dry-run exited $rc after stage $v/$name" >&2; exit 1; }
  FSCK_EXIT[$name]=0
  keep "$v" "$name" "$img"
}

# ops files: one shell command per line, run in the guest with the volume on /mnt and the payload on /payload
ops_w() { # ops_w <variant>
  if [ "$1" = nodiscard ]; then
    echo 'mkdir /mnt/sac'
  fi
  echo 'mkdir /mnt/contig /mnt/twofrag /mnt/holes /mnt/direct /mnt/indirect /mnt/inline /mnt/empty /mnt/gone /mnt/gone/gonedir /mnt/live /mnt/live/small'
  if [ "$1" = nodiscard ]; then
    echo 'mkdir /mnt/reuse /mnt/renamed /mnt/nidreuse /mnt/nidreuse-w'
    # 64 decoy inode numbers BELOW every later file's: the fill creates dnodes, which take the lowest free node ids,
    # and must not take the inode number of reuse/target.bin
    echo 'i=0; while [ $i -lt 64 ]; do : > /mnt/sac/s$i; i=$((i+1)); done'
  fi
  cat <<'OPS'
cp /payload/contig/target.bin /mnt/contig/target.bin
for k in 0 1 2 3 4; do dd if=/payload/twofrag/a.bin of=/mnt/twofrag/a.bin bs=8192 skip=$k seek=$k count=1 conv=notrunc 2>/dev/null; sync; dd if=/payload/twofrag/target.bin of=/mnt/twofrag/target.bin bs=8192 skip=$k seek=$k count=1 conv=notrunc 2>/dev/null; sync; done
OPS
  if [ "$1" = nodiscard ]; then
    echo 'cp /payload/reuse/target.bin /mnt/reuse/target.bin'
  fi
  cat <<'OPS'
dd if=/payload/holes/b0 of=/mnt/holes/target.bin bs=4096 count=1 conv=notrunc 2>/dev/null
dd if=/payload/holes/b3 of=/mnt/holes/target.bin bs=4096 seek=3 count=1 conv=notrunc 2>/dev/null
cp /payload/direct/target.bin /mnt/direct/target.bin
cp /payload/indirect/target.bin /mnt/indirect/target.bin
cp /payload/inline/target.bin /mnt/inline/target.bin
: > /mnt/empty/target.bin
cp /payload/gone/gonedir/inner.bin /mnt/gone/gonedir/inner.bin
OPS
  if [ "$1" = nodiscard ]; then
    echo 'cp /payload/renamed/a /mnt/renamed/a'
    echo 'cp /payload/nidreuse/target.bin /mnt/nidreuse/target.bin'
  fi
  cat <<'OPS'
cp /payload/live/indirect.bin /mnt/live/indirect.bin
dd if=/payload/live/lb0 of=/mnt/live/holes.bin bs=4096 count=1 conv=notrunc 2>/dev/null
dd if=/payload/live/lb3 of=/mnt/live/holes.bin bs=4096 seek=3 count=1 conv=notrunc 2>/dev/null
cp /payload/live/inline.txt /mnt/live/inline.txt
cp /payload/live/small/s1 /mnt/live/small/s1
cp /payload/live/small/s2 /mnt/live/small/s2
cp /payload/live/small/s3 /mnt/live/small/s3
sync
OPS
}

ops_d() { # ops_d <variant>
  echo 'rm /mnt/contig/target.bin /mnt/twofrag/target.bin /mnt/direct/target.bin /mnt/indirect/target.bin /mnt/holes/target.bin /mnt/inline/target.bin /mnt/empty/target.bin'
  echo 'rm -r /mnt/gone/gonedir'
  if [ "$1" = nodiscard ]; then
    echo 'rm /mnt/renamed/b'
  fi
  echo sync
}

# the files the guest is told to write: the expected tree (what ought to be on the volume) and the pieces
# the shell assembles sparse files from
mk_sources() { # mk_sources <variant>
  local v=$1 e=$work/$1/exp s=$work/$1/src
  mkdir -p "$e"/{contig,twofrag,holes,direct,indirect,inline,empty,gone/gonedir,live/small} "$s"/{holes,live}
  pattern 20480 contig >"$e/contig/target.bin"
  pattern 40960 twofrag-a >"$e/twofrag/a.bin"
  pattern 40960 twofrag-t >"$e/twofrag/target.bin"
  { pattern 4096 H0; head -c 8192 /dev/zero; pattern 4096 H3; } >"$e/holes/target.bin" # blocks 0 and 3 written, 1 and 2 are holes
  pattern 4096 H0 >"$s/holes/b0"
  pattern 4096 H3 >"$s/holes/b3"
  stamp $((5 * 1024 * 1024)) DI >"$e/direct/target.bin"
  stamp $((12 * 1024 * 1024)) IN >"$e/indirect/target.bin"
  pattern 100 inline >"$e/inline/target.bin"
  : >"$e/empty/target.bin"
  pattern 8192 gone >"$e/gone/gonedir/inner.bin"
  stamp $((12 * 1024 * 1024)) LI >"$e/live/indirect.bin"
  { pattern 4096 LH0; head -c 8192 /dev/zero; pattern 4096 LH3; } >"$e/live/holes.bin"
  pattern 4096 LH0 >"$s/live/lb0"
  pattern 4096 LH3 >"$s/live/lb3"
  pattern 100 live-inline >"$e/live/inline.txt"
  for n in 1 2 3; do pattern 1000 "small$n" >"$e/live/small/s$n"; done
  if [ "$v" = nodiscard ]; then
    mkdir -p "$e"/{reuse,renamed,nidreuse,nidreuse-w}
    stamp $((6 * 1024 * 1024)) RU >"$e/reuse/target.bin" # three segments: the middle one is free as a whole
    pattern 20480 renamed >"$e/renamed/a"
    cp "$e/renamed/a" "$e/renamed/b"
    pattern 20480 nidreuse >"$e/nidreuse/target.bin"
    pattern 8192 new1 >"$e/nidreuse-w/new1.bin"
    pattern 4096 new2 >"$e/nidreuse-w/new2.bin"
  fi
}

# payload <variant> <dir name> <relative path>...: a stage payload from the expected tree and the pieces
payload() {
  local v=$1 dst=$work/$1/pay-$2 r
  shift 2
  rm -rf "$dst"
  mkdir -p "$dst"
  for r in "$@"; do
    if [ -e "$work/$v/exp/$r" ]; then (cd "$work/$v/exp" && cp -r --parents "$r" "$dst"); else (cd "$work/$v/src" && cp -r --parents "$r" "$dst"); fi
  done
}

# build_variant <nodiscard|discard>
build_variant() {
  local v=$1 opts drive img gen stages=() snaps=() ora=$here/f2fs_recover_oracle.py
  local pay_w=(contig twofrag direct indirect inline gone holes/b0 holes/b3 live/indirect.bin live/lb0 live/lb3 live/inline.txt live/small)
  CMDS=()
  declare -gA FSCK_EXIT=()
  mkdir -p "$work/$v/tools"
  img=$work/$v/vol.img
  mk_sources "$v"
  if [ "$v" = nodiscard ]; then
    opts=nodiscard,noatime
    drive=""
    pay_w+=(reuse renamed/a nidreuse)
  else
    opts=discard,noatime
    drive=,discard=unmap
  fi
  export GUEST_DRIVE_OPTS=$drive
  unset GUEST_TIMEOUT

  CMDS+=("# expected tree: mk_sources of tools/fixtures/f2fs_recover.sh (patterns and stamps from lib.sh); guest: f2fs -o $opts${drive:+, qemu -drive ...$drive}")
  CMDS+=("truncate -s 128M <img>")
  CMDS+=("faketime -f '$clock' mkfs.f2fs -q -f -r -l f2fsrec -U $uuid -O extra_attr,inode_checksum,sb_checksum <img>")
  CMDS+=("python3 tools/fixtures/f2fs_normalize.py <img>")
  truncate -s 128M "$img"
  faketime -f "$clock" mkfs.f2fs -q -f -r -l f2fsrec -U "$uuid" -O extra_attr,inode_checksum,sb_checksum "$img"
  python3 "$here/f2fs_normalize.py" "$img"

  echo "== $v: stage w"
  payload "$v" w "${pay_w[@]}"
  ops_w "$v" >"$work/$v/ops-w"
  stage "$v" w "$opts" "$work/$v/pay-w" "$work/$v/ops-w"
  CMDS+=("python3 tools/fixtures/f2fs_recover_oracle.py snap --stage w   # decoder == fsck.f2fs -t, -M, dump.f2fs -n; content against the expected tree")
  python3 "$ora" snap --image "$img" --tools "$work/$v/tools" --stage w --expected "$work/$v/exp" --out "$work/$v/snap-w.json"
  stages=(w)
  snaps=("$work/$v/snap-w.json")

  if [ "$v" = nodiscard ]; then
    echo "== $v: stage m (rename, unlink, checkpoint)"
    printf '%s\n' 'mv /mnt/renamed/a /mnt/renamed/b' 'rm /mnt/nidreuse/target.bin' sync >"$work/$v/ops-m"
    payload "$v" m
    stage "$v" m "$opts" "$work/$v/pay-m" "$work/$v/ops-m"
    CMDS+=("python3 tools/fixtures/f2fs_recover_oracle.py snap --stage m")
    python3 "$ora" snap --image "$img" --tools "$work/$v/tools" --stage m --expected "$work/$v/exp" --out "$work/$v/snap-m.json"
    stages+=(m)
    snaps+=("$work/$v/snap-m.json")

    echo "== $v: stage n (new files in a fresh boot)"
    printf '%s\n' 'cp /payload/nidreuse-w/new1.bin /mnt/nidreuse-w/new1.bin' 'cp /payload/nidreuse-w/new2.bin /mnt/nidreuse-w/new2.bin' sync >"$work/$v/ops-n"
    payload "$v" n nidreuse-w
    stage "$v" n "$opts" "$work/$v/pay-n" "$work/$v/ops-n"
    stages+=(n)

    echo "== $v: stage f (fill to ENOSPC, GUEST_TIMEOUT=2400)"
    cat >"$work/$v/ops-f" <<'OPS'
: > /mnt/fill
rm -r /mnt/sac
rm /mnt/reuse/target.bin
sync
dd if=/dev/zero of=/mnt/fill bs=4096 2>/dev/null || true
ls -l /mnt/fill
df /mnt
sync
OPS
    payload "$v" f
    GUEST_TIMEOUT=2400 stage "$v" f "$opts" "$work/$v/pay-f" "$work/$v/ops-f"
    grep -a -e ' /mnt/fill$' -e '^/dev/vda' "$work/$v/guest-f.log" || true
    stages+=(f)
  fi

  echo "== $v: stage d (only rm)"
  ops_d "$v" >"$work/$v/ops-d"
  payload "$v" d
  stage "$v" d "$opts" "$work/$v/pay-d" "$work/$v/ops-d"
  stages+=(d)
  CMDS+=("python3 tools/fixtures/f2fs_recover_oracle.py final --variant $v   # free blocks, live files, deleted dentries and their expectations")
  python3 "$ora" final --image "$img" --tools "$work/$v/tools" --stage d --expected "$work/$v/exp" --variant "$v" \
    --snaps "${snaps[@]}" --report-unlisted gone --out "$work/$v/oracle.json"

  local name=f2fs-recover-$v
  gen=$(generator_json "$img" f2fs-tools util-linux python3 coreutils jq gzip faketime qemu-system-x86 linux-image-6.1.0-53-amd64 busybox-static kmod cpio)
  local fsck_json
  fsck_json=$(for s in "${stages[@]}"; do echo "$s ${FSCK_EXIT[$s]}"; done | jq -R 'split(" ") | {(.[0]): (.[1] | tonumber)}' | jq -s add)
  jq -n --argjson g "$gen" --arg v "$v" --argjson st "$(printf '%s\n' "${stages[@]}" | jq -R . | jq -s .)" --argjson fe "$fsck_json" \
    --slurpfile o "$work/$v/oracle.json" '{generator: ($g + {variant: $v, stages: $st, fsck_exit: $fe})} + $o[0]' >"$work/final/$name.expect.json"
  gzip -n -9 -c "$img" >"$work/final/$name.img.gz"
  echo "$name: $(stat -c %s "$work/final/$name.img.gz") bytes gzipped, sha256 $(jq -r .generator.image_sha256 "$work/final/$name.expect.json")"
  rm -f "$img"
}

build_variant nodiscard
build_variant discard

for f in "$work"/final/*.img.gz; do
  size=$(stat -c %s "$f")
  if [ "$size" -gt "$max_gz" ]; then
    echo "$(basename "$f") is $size bytes, over the $max_gz budget" >&2
    exit 1
  fi
done

cp "$work"/final/* "$out/"
for f in f2fs-recover-nodiscard f2fs-recover-discard; do
  echo "wrote $out/$f.img.gz and $f.expect.json"
done
echo "RUNTIME $(($(date +%s) - t0)) s"

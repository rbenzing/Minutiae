#!/bin/bash
# ext4 and ext3 RECOVERY fixtures of plan 3C: volumes written and deleted by the KERNEL ext4 driver inside the
# QEMU TCG guest (recover_guest.sh), with an independent oracle (ext4_recover_oracle.py). The host formats
# (mke2fs under faketime), deletes the intact-debugfs-* targets (debugfs -w rm) and READS finished images; no kernel
# module is loaded on the host, no loop device, no --privileged.
# Usage: ext4_recover.sh <outdir>   (normally: gen.sh ext4-recover, in the minutiae-recover-fixtures-510 image;
# see README.md, "ext4 recovery fixtures (3C)")
#
# Kernels (ruling 3C-C1): the positive fixtures run on the 5.10.223 guest kernel, which keeps the inode number and the
# name of a deleted directory record; the 6.1.0-53 kernel wipes them, so a tiny image (ext4-recover-wiped) is written
# by 6.1 and proves that a wiped record lists nothing. A third small image, ext4-recover-sameboot, holds a file that is
# written and removed in one boot (ruling 3C-C2, amended). The probe facts (README.md, "ext4 recovery probe (3C)")
# that this script relies on:
#   - a kernel rm clears the in-inode map and zeroes i_size (GATE-K closed, GATE-S zeroed), keeps the freed leaf
#     blocks of a depth-1 file at their home location (GATE-L open, clean boundary) and frees the inode (GATE-U: the
#     next file takes the lowest freed inode number).
# Placement is never trusted (rulings 3C-R13, F19, F22): the oracle reads every outcome from the finished image, and the
# generator exits 1 naming a family that did not occur. What the script does to make the scenarios happen:
#   - the free space is first a CHECKERBOARD (hundreds of one-block files in the directories pad/a, kept, and pad/b,
#     removed: the small fill files of ruling 3C-R30) behind one big zero file that takes the rest, so that the free blocks
#     are holes of one block and a file written afterwards is fragmented, with one extent per block (a depth-1 tree, two
#     leaves for 96 blocks); the ordinary writers prefer the longest free run, then the lowest block;
#   - every boot that fills or reuses space first sets /sys/fs/ext4/vda/reserved_clusters to 0 (the kernel keeps 2% of
#     the volume out of reach of ordinary writes), so a full volume has NO free block, and after an rm the free set is
#     exactly the removed file's blocks: a reuser written then takes the lowest of them;
#   - a reuser would take the lowest freed INODE number, which is the target's own: a "donor" (an empty file with a
#     lower number) is removed first and the reuser takes the donor's number.
#
# ext4-recover boots (one guest boot each, on the previous boot's image, guest clock = base + N hours):
#   1 layout   directories (never inline), the small families, the checkerboard over the whole volume
#   2 deep     the fragmented files, written into the holes (the tail half of deep-reuse-tail first)
#   3 fill     dd zeros to ENOSPC: the volume is full
#   4-7 reuse  rm a donor and a target, sync, write the reuser (tail, head, leaf); the last step takes the freed inode number
#   8 last     the kernel rm of the remaining kernel targets, nothing written afterwards
#   then       host debugfs rm of the intact-debugfs-* targets
# ext3-recover boots: layout, deep (the reuse target, indirect block first), fill, (host debugfs rm of the reuse target),
# reuse, last (kernel rm), then host debugfs rm of the intact-debugfs-indirect target.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "ext4_recover.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

out=$(cd "${1:?outdir required}" && pwd)
k510=5.10.0-32-amd64
k61=6.1.0-53-amd64
if [ "$kver" != "$k510" ] || [ ! -e "/lib/modules/$k61" ] || [ ! -e "/boot/vmlinuz-$k61" ]; then
  echo "ext4_recover.sh needs the 5.10 guest kernel (GUEST_KVER=$k510) and the 6.1 kernel of the same image." >&2
  echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures &&" >&2
  echo "  docker build -f tools/fixtures/Dockerfile.recover510 -t minutiae-recover-fixtures-510 tools/fixtures" >&2
  echo "(see tools/fixtures/README.md, \"ext4 recovery fixtures (3C)\")." >&2
  exit 1
fi
max_gz=409600 # 400 KiB (plan 3C Task 11)
base="2023-11-14 22:13:20"
at() { date -u -d "$base UTC + $1 hours" +'%F %T'; }
uuid4=33333333-4444-5555-6666-777777777777
uuid3=33333333-4444-5555-6666-888888888888
uuid6=33333333-4444-5555-6666-999999999999
uuid5=33333333-4444-5555-6666-aaaaaaaaaaaa
keeper="keeper_$(printf 'k%.0s' $(seq 1 193))" # 200 characters: a directory holding it is a block directory, never inline
noreserve="echo 0 > /sys/fs/ext4/vda/reserved_clusters" # the guest op that takes the kernel's 2% reserve away

work=$(mktemp -d /tmp/ext4-recover.XXXXXX)
keep() { # FIXTURE_KEEP_DIR=<dir>: copy the stage images and guest logs there (a debugging aid, never set by gen.sh)
  if [ -n "${FIXTURE_KEEP_DIR:-}" ]; then
    mkdir -p "$FIXTURE_KEEP_DIR"
    cp -r "$work"/snap* "$work"/guest-*.log "$work"/manifest-* "$work"/payload "$FIXTURE_KEEP_DIR"/ 2>/dev/null || true
  fi
}
trap 'keep; rm -rf "$work"' EXIT
mkdir -p "$work/final"

pay=$work/payload
mkdir -p "$pay"
# sizes in blocks (1 KiB); the deep files are written block by block into one-block holes, so they are fragmented
n_dk=96 # deep-kernel: at least 90 fragments, so two leaves
n_dt=16 # deep-reuse-tail
n_dh=16 # deep-reuse-head
n_lr=16 # leaf-reused
n_dd=12 # intact-debugfs-deep
ku_dt=3 # blocks the tail reuser takes: the lowest holes are the file's tail half, written first
ku_dh=2 # blocks the head reuser takes
ku_lr=9 # blocks the leaf reuser takes: enough to include the leaf block

mkpay() { pattern "$2" "$3" >"$pay/$1"; } # name bytes seed
mkpay sk_s.bin 3067 sk
mkpay in_i.dat 40 in
mkpay ir_f.bin 60 ir
mkpay hl_h.bin 2000 hl
mkpay ds_s.bin 2500 ds
mkpay dk.src $((n_dk * 1024 - 13)) dk
mkpay dt.src $((n_dt * 1024 - 9)) dt
mkpay dh.src $((n_dh * 1024 - 11)) dh
mkpay lr.src $((n_lr * 1024 - 3)) lr
mkpay dd.src $((n_dd * 1024 - 17)) dd
mkpay u_dt $((ku_dt * 1024)) udt
mkpay u_dh $((ku_dh * 1024)) udh
mkpay u_lr $((ku_lr * 1024)) ulr
mkpay bk_d.bin 5000 bk
mkpay bi_i.bin $((20 * 1024 - 11)) bi
mkpay di_x.bin $((20 * 1024 - 5)) di
mkpay dr_r.bin $((16 * 1024 - 7)) dr
mkpay u_dr 1024 udr
mkpay wa.bin 3000 wa
mkpay wb.bin 2000 wb
mkpay sb.src $((200 * 1024 - 21)) sb
# sp.src: nine data blocks at the even logical blocks 0..16 with a hole after each (a sparse file); the last block is partial
python3 -I - "$pay/sp.src" <<'EOT'
import hashlib, sys
out = bytearray()
for blk in range(9):
    d = bytearray()
    i = 0
    while len(d) < 1024:
        d += hashlib.sha256(f"sp:{blk}:{i}".encode()).digest()
        i += 1
    out += bytes(d[:1024])
    out += bytes(1024)
open(sys.argv[1], "wb").write(bytes(out[:16 * 1024 + 700]))
EOT

# mkpad <payload dir> <pairs>: pad.tar of the checkerboard: directories pad/a/dNN (kept) and pad/b/dNN (removed) with 20
# one-block zero files each, created alternately
mkpad() {
  python3 -I - "$1/pad.tar" "$2" <<'EOT'
import io, sys, tarfile
t = tarfile.open(sys.argv[1], "w", format=tarfile.USTAR_FORMAT)
def add(name, data=None):
    ti = tarfile.TarInfo(name)
    ti.mtime, ti.uid, ti.gid, ti.uname, ti.gname = 1700000000, 0, 0, "", ""
    if data is None:
        ti.type, ti.mode = tarfile.DIRTYPE, 0o755
        t.addfile(ti)
    else:
        ti.mode, ti.size = 0o644, len(data)
        t.addfile(ti, io.BytesIO(data))
for d in ("pad", "pad/a", "pad/b"):
    add(d)
n = int(sys.argv[2])
for k in range(n):
    g = "g%02d" % (k // 50)  # at most 50 subdirectories per group directory: none of them needs a hash index
    if k % 50 == 0:
        add("pad/a/" + g)
        add("pad/b/" + g)
    add("pad/a/%s/d%03d" % (g, k))
    add("pad/b/%s/d%03d" % (g, k))
    for j in range(20):
        add("pad/a/%s/d%03d/f%02d" % (g, k, j), bytes(1024))
        add("pad/b/%s/d%03d/f%02d" % (g, k, j), bytes(1024))
t.close()
EOT
}
pairs=15 # pairs of pad directories (20 one-block files each): 300 holes after the b directories are removed
# bigfill_blocks <image>: the blocks of one zero file that leave the checkerboard its room: the free blocks of a fresh image
# minus the pairs (42 blocks each) and a margin of 1400 (the files made before, the group directories, slack)
bigfill_blocks() { echo $(($(freeblocks "$1") - 1400 - pairs * 42)); }
# pad_ops <bigfill blocks> [extra op]: the big zero file, the archive, every remaining free block taken by a second zero
# file, then the b files removed (and the extra op, run at the same point)
pad_ops() {
  echo "dd if=/dev/zero of=/mnt/bigfill bs=1024 count=$1 2>/dev/null || true"
  echo "sync"
  echo "tar -x -C /mnt -f /payload/pad.tar || true"
  echo "sync"
  echo "dd if=/dev/zero of=/mnt/tailfill bs=1024 2>/dev/null || true"
  echo "sync"
  echo "rm -r /mnt/pad/b"
  [ -n "${2:-}" ] && echo "$2"
  echo "sync"
}

MANIFEST=()
want() { # <path> <family> <payload source name, or - for an empty file> [alias path: another name of the same inode] [content: the true map is found by content]
  MANIFEST+=("$(jq -n --arg p "$1" --arg f "$2" --arg s "$3" --arg a "${4:-}" --arg c "${5:-}"     '{path: $p, family: $f, src: $s} + (if $a == "" then {} else {alias: $a} end) + (if $c == "" then {} else {true: $c} end)')")
}
write_manifest() { # <file> <fs type> <snapshot image...>
  local f=$1 t=$2
  shift 2
  printf '%s\n' "${MANIFEST[@]}" | jq -s --arg t "$t" --args '{fs_type: $t, files: ., snapshots: $ARGS.positional}' "$@" >"$f"
}

stage() { # <image dir> <name> <from> <clock> [kernel]   (ops on stdin: call it as `stage ... < <(ops)`, never in a pipeline, which would lose CMDS) -> $dir/<name>.img, e2fsck -fn clean
  local dir=$1 name=$2 from=$3 clock=$4 saved=$kver rc=0 t0s line
  [ -n "${5:-}" ] && kver=$5
  cat >"$work/ops-$name"
  cp "$dir/$from.img" "$work/cur.img"
  guest_initrd "$work/rd" ext4 "" "${stage_pay:-$pay}" "$work/ops-$name" "$clock"
  t0s=$(date +%s)
  guest_run "$work/cur.img" "$work/rd" "$work/guest-$name.log"
  echo "FACT boot image=$name kernel=$kver clock=\"$clock\" seconds=$(($(date +%s) - t0s))"
  while IFS= read -r line; do CMDS+=("guest boot $name ($clock, kernel $kver): $line"); done <"$work/ops-$name"
  kver=$saved
  mv "$work/cur.img" "$dir/$name.img"
  e2fsck -fn "$dir/$name.img" >"$work/fsck-$name.txt" 2>&1 || rc=$?
  echo "FACT e2fsck image=$name exit=$rc free_blocks=$(freeblocks "$dir/$name.img")"
  if [ "$rc" != 0 ]; then
    cat "$work/fsck-$name.txt"
    echo "e2fsck -fn is not clean after boot $name" >&2
    exit 1
  fi
}
freeblocks() { dumpe2fs -h "$1" 2>/dev/null | sed -n 's/^Free blocks: *//p'; }
fsck_clean() { # <image> <what>
  local rc=0
  e2fsck -fn "$1" >"$work/fsck-last.txt" 2>&1 || rc=$?
  if [ "$rc" != 0 ]; then
    cat "$work/fsck-last.txt"
    echo "e2fsck -fn is not clean after $2" >&2
    exit 1
  fi
  echo "FACT e2fsck after=$2 exit=0"
}
full() { # <image>: the volume must have no free block
  if [ "$(freeblocks "$1")" != 0 ]; then
    echo "$1 has $(freeblocks "$1") free blocks, not a full volume: the reuse steps would not be confined to the removed file's blocks" >&2
    exit 1
  fi
}
# deep_ops <file> <source> <first lblk> <count> [sync]: dd the blocks first..first+count-1 in order (a sync after each with a fifth argument)
deep_ops() {
  local f=$1 s=$2 first=$3 count=$4 each=${5:+sync;}
  echo "i=$first; while [ \$i -lt $((first + count)) ]; do dd if=/payload/$s of=$f bs=1024 skip=\$i seek=\$i count=1 conv=notrunc 2>/dev/null; ${each} i=\$((i+1)); done"
  echo sync
}
gates_json='{"L": "open", "K": "closed", "S": "zeroed", "U": "reuse"}'

# finish <name> <fs type> <final image> <kernel> <snapshot images...>: oracle, generator section, gzip
finish() {
  local name=$1 type=$2 img=$3 kern=$4 gen
  shift 4
  write_manifest "$work/manifest-$name.json" "$type" "$@"
  CMDS+=("e2fsck -fn <img>   # must exit 0")
  fsck_clean "$img" "$name final"
  CMDS+=("python3 tools/fixtures/ext4_recover_oracle.py <final> <payload> <manifest>   # debugfs/dumpe2fs cross-checks, own raw reader, the stage images")
  python3 -I "$here/ext4_recover_oracle.py" "$img" "$pay" "$work/manifest-$name.json" >"$work/oracle-$name.json"
  gen=$(generator_json "$img" e2fsprogs python3 jq gzip faketime qemu-system-x86 busybox-static kmod cpio linux-image-6.1.0-53-amd64)
  jq -n --argjson g "$gen" --arg k "$kern" --argjson gates "$gates_json" --slurpfile o "$work/oracle-$name.json" \
    '{generator: ($g + {kernel: $k, fsck_exit: 0, gates: $gates, packages: ($g.packages + [{package: "linux-image-5.10.0-32-amd64", version: "5.10.223-1"}])})} + $o[0]' \
    >"$work/final/$name.expect.json"
  gzip -n -9 -c "$img" >"$work/final/$name.img.gz"
  echo "$name: $(stat -c %s "$img") bytes raw, $(stat -c %s "$work/final/$name.img.gz") bytes gzipped"
}

# ====================================== ext4-recover (5.10 kernel) ======================================
d4=$work/snap4
mkdir -p "$d4"
CMDS=()
CMDS+=("# sources: pattern bytes (tools/fixtures/lib.sh); sp.src and pad.tar are built by ext4_recover.sh itself; guest kernel $k510")
echo "== ext4-recover: format"
truncate -s 16M "$d4/s0.img"
CMDS+=("faketime -f '$base' mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 16384 -U $uuid4 -E hash_seed=$uuid4 <img>")
faketime -f "$base" mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 16384 \
  -U "$uuid4" -E "hash_seed=$uuid4" "$d4/s0.img"
fsck_clean "$d4/s0.img" "mke2fs"
bigfill=$(bigfill_blocks "$d4/s0.img")
echo "FACT checkerboard pairs=$pairs bigfill_blocks=$bigfill"
mkdir "$work/pay-b1"
cp "$pay"/* "$work/pay-b1/"
mkpad "$work/pay-b1" "$pairs"

MANIFEST=()
want /t/sk/s.bin small-kernel sk_s.bin
want /t/zs/z.dat zero-size -
want /t/in/i.dat inline in_i.dat
want /t/hl/h1 hardlink hl_h.bin /t/hl/h2
want /t/sp/sp.bin sparse-kernel sp.src
want /t/dk/deep.bin deep-kernel dk.src
want /t/dt/deep.bin deep-reuse-tail dt.src
want /t/dh/deep.bin deep-reuse-head dh.src
want /t/lr/deep.bin leaf-reused lr.src
want /t/ir/f.bin inode-reused ir_f.bin
want /t/w/u_dh deep-reuse-head-aux u_dh
want /t/dn/l1 inode-donor-aux -
want /t/dn/l2 inode-donor-aux -
want /t/dn/l3 inode-donor-aux -
want /t/ds/s.bin intact-debugfs-small ds_s.bin
want /t/dd/deep.bin intact-debugfs-deep dd.src
want /pad/b pad-aux -

echo "== ext4-recover boot 1: layout and checkerboard"
stage_pay="$work/pay-b1" stage "$d4" b1-layout s0 "$(at 0)" < <(
{
  echo "$noreserve"
  echo "mkdir /mnt/t /mnt/t/ir /mnt/t/sk /mnt/t/zs /mnt/t/in /mnt/t/hl /mnt/t/sp /mnt/t/dk /mnt/t/dt /mnt/t/dh /mnt/t/lr /mnt/t/ds /mnt/t/dd /mnt/t/w /mnt/t/dn /mnt/t/ir/sub"
  echo "for d in ir sk zs in hl sp dk dt dh lr ds dd w dn; do : > /mnt/t/\$d/$keeper; done"
  echo "cp /payload/ir_f.bin /mnt/t/ir/f.bin"
  echo ": > /mnt/t/dn/l1; : > /mnt/t/dn/l2; : > /mnt/t/dn/l3"
  echo "cp /payload/sk_s.bin /mnt/t/sk/s.bin; : > /mnt/t/zs/z.dat; cp /payload/in_i.dat /mnt/t/in/i.dat"
  echo "cp /payload/hl_h.bin /mnt/t/hl/h1; ln /mnt/t/hl/h1 /mnt/t/hl/h2; sync; rm /mnt/t/hl/h1"
  echo "cp /payload/ds_s.bin /mnt/t/ds/s.bin; sync"
  pad_ops "$bigfill"
}
)

echo "== ext4-recover boot 2: the deep files, written into the holes (lr, dh, dt, dk, dd, the sparse file)"
stage "$d4" b2-deep b1-layout "$(at 1)" < <(
{
  echo "$noreserve" # the checkerboard has fewer free blocks than the kernel's reserve
  deep_ops /mnt/t/lr/deep.bin lr.src 0 $n_lr
  deep_ops /mnt/t/dh/deep.bin dh.src 0 $n_dh
  deep_ops /mnt/t/dt/deep.bin dt.src $((n_dt / 2)) $((n_dt / 2)) sync
  deep_ops /mnt/t/dt/deep.bin dt.src 0 $((n_dt / 2)) sync
  deep_ops /mnt/t/dk/deep.bin dk.src 0 $n_dk
  deep_ops /mnt/t/dd/deep.bin dd.src 0 $n_dd
  echo "for i in 0 2 4 6 8 10 12 14 16; do dd if=/payload/sp.src of=/mnt/t/sp/sp.bin bs=1024 skip=\$i seek=\$i count=1 conv=notrunc 2>/dev/null; done; sync"
}
)

echo "== ext4-recover boot 3: fill the volume"
stage "$d4" b3-fill b2-deep "$(at 2)" < <(
{
  echo "$noreserve"
  echo "dd if=/dev/zero of=/mnt/fill1 bs=1024 2>/dev/null || true"
  echo "sync"
}
)
full "$d4/b3-fill.img"

echo "== ext4-recover boots 4-7: the reuse steps (the reuser takes the lowest freed inode number: the donor's, not the target's)"
stage "$d4" b4-reuse-tail b3-fill "$(at 3)" < <(
{
  echo "$noreserve"
  echo "rm /mnt/t/dn/l1; rm /mnt/t/dt/deep.bin; sync"
  echo "cp /payload/u_dt /mnt/t/w/u_dt; sync"
}
)
stage "$d4" b5-reuse-head b4-reuse-tail "$(at 4)" < <(
{
  echo "$noreserve"
  echo "rm /mnt/t/dn/l2; rm /mnt/t/dh/deep.bin; sync"
  echo "cp /payload/u_dh /mnt/t/w/u_dh; sync"
}
)
stage "$d4" b6-reuse-leaf b5-reuse-head "$(at 5)" < <(
{
  echo "$noreserve"
  echo "rm /mnt/t/dn/l3; rm /mnt/t/lr/deep.bin; sync"
  echo "cp /payload/u_lr /mnt/t/w/u_lr; sync"
}
)
stage "$d4" b7-reuse-inode b6-reuse-leaf "$(at 6)" < <(
{
  echo "$noreserve"
  echo "rm /mnt/t/ir/f.bin; sync"
  echo ": > /mnt/t/ir/sub/newer; sync"
}
)

echo "== ext4-recover boot 8: the kernel rm of the remaining targets"
stage "$d4" b8-last b7-reuse-inode "$(at 7)" < <(
{
  echo "rm /mnt/t/sk/s.bin /mnt/t/zs/z.dat /mnt/t/in/i.dat /mnt/t/sp/sp.bin /mnt/t/dk/deep.bin /mnt/t/w/u_dh; sync"
}
)

echo "== ext4-recover: host debugfs rm of the intact-debugfs targets"
cp "$d4/b8-last.img" "$d4/final.img"
CMDS+=("debugfs -w -R 'rm /t/ds/s.bin' <img>")
debugfs -w -R "rm /t/ds/s.bin" "$d4/final.img" >/dev/null
CMDS+=("debugfs -w -R 'rm /t/dd/deep.bin' <img>")
debugfs -w -R "rm /t/dd/deep.bin" "$d4/final.img" >/dev/null
# the snapshots are the stage images in order; the oracle finds the true map of a file in the last one that still had it
finish ext4-recover ext4 "$d4/final.img" "$k510" "$d4"/b1-layout.img "$d4"/b2-deep.img "$d4"/b3-fill.img "$d4"/b4-reuse-tail.img \
  "$d4"/b5-reuse-head.img "$d4"/b6-reuse-leaf.img "$d4"/b7-reuse-inode.img "$d4"/b8-last.img

# ====================================== ext3-recover (5.10 kernel) ======================================
d3=$work/snap3
mkdir -p "$d3"
CMDS=()
CMDS+=("# sources: pattern bytes (tools/fixtures/lib.sh); pad.tar is built by ext4_recover.sh itself; guest kernel $k510, the ext3 image mounted by the ext4 driver")
echo "== ext3-recover: format"
truncate -s 8M "$d3/s0.img"
CMDS+=("faketime -f '$base' mke2fs -q -F -t ext3 -b 1024 -O ^dir_index,^orphan_file -m 0 -J size=1 -N 8192 -U $uuid3 -E hash_seed=$uuid3 <img>")
faketime -f "$base" mke2fs -q -F -t ext3 -b 1024 -O ^dir_index,^orphan_file -m 0 -J size=1 -N 8192 \
  -U "$uuid3" -E "hash_seed=$uuid3" "$d3/s0.img"
fsck_clean "$d3/s0.img" "mke2fs"
bigfill=$(bigfill_blocks "$d3/s0.img")
echo "FACT checkerboard pairs=$pairs bigfill_blocks=$bigfill"
rm -rf "$work/pay-b1"
mkdir "$work/pay-b1"
cp "$pay"/* "$work/pay-b1/"
mkpad "$work/pay-b1" "$pairs"

echo "== ext3-recover boot 1: layout (a 90-entry directory, the files) and checkerboard"
stage_pay="$work/pay-b1" stage "$d3" b1-layout s0 "$(at 0)" < <(
{
  echo "$noreserve"
  echo "mkdir /mnt/t /mnt/t/bk /mnt/t/bi /mnt/t/di /mnt/t/dr /mnt/t/fr /mnt/t/w /mnt/t/dn"
  echo ": > /mnt/t/dn/l1"
  # a spacer of 8 contiguous blocks, removed with the b files: the four tail blocks of dr, written in one request, take four
  # of them and are the longest free run when the reuser comes (the indirect block is allocated apart)
  echo "dd if=/dev/zero of=/mnt/spacer bs=1024 count=8 2>/dev/null"
  echo "cp /payload/bk_d.bin /mnt/t/bk/d.bin; cp /payload/bi_i.bin /mnt/t/bi/i.bin; cp /payload/di_x.bin /mnt/t/di/x.bin; sync"
  echo "i=100; while [ \$i -lt 190 ]; do : > /mnt/t/fr/file_name_number_\$i; i=\$((i+1)); done; sync"
  pad_ops "$bigfill" "rm /mnt/spacer"
}
)
# the first live record of directory block 1 (the one whose deletion zeroes the inode number)
picks=$(python3 -I "$here/ext4_recover_probe.py" pick "$d3/b1-layout.img" /t/fr)
set -- $picks
first=$1

echo "== ext3-recover boot 2: dr, the logical blocks 12..15 first (the indirect block and the data after it get the lowest holes), then 0..11"
stage "$d3" b2-deep b1-layout "$(at 1)" < <(
{
  echo "$noreserve"
  # the indirect block and the data after it in ONE request (one dd of four blocks): the longest free run takes them all
  echo "dd if=/payload/dr_r.bin of=/mnt/t/dr/r.bin bs=1024 skip=12 seek=12 count=4 conv=notrunc 2>/dev/null; sync"
  deep_ops /mnt/t/dr/r.bin dr_r.bin 0 12 sync
}
)

echo "== ext3-recover boot 3: fill the volume"
stage "$d3" b3-fill b2-deep "$(at 2)" < <(
{
  echo "$noreserve"
  echo "dd if=/dev/zero of=/mnt/fill1 bs=1024 2>/dev/null || true"
  echo "sync"
}
)
full "$d3/b3-fill.img"

echo "== ext3-recover: host debugfs rm of the reuse target (the volume is full, so the free set is its blocks)"
cp "$d3/b3-fill.img" "$d3/b3-rm.img"
CMDS+=("debugfs -w -R 'rm /t/dr/r.bin' <img>")
debugfs -w -R "rm /t/dr/r.bin" "$d3/b3-rm.img" >/dev/null
fsck_clean "$d3/b3-rm.img" "debugfs rm of the reuse target"

echo "== ext3-recover boot 4: the guest takes the lowest free block"
stage "$d3" b4-reuse b3-rm "$(at 3)" < <(
{
  echo "$noreserve"
  echo "rm /mnt/t/dn/l1; sync"
  echo "cp /payload/u_dr /mnt/t/w/u_dr; sync"
}
)

echo "== ext3-recover boot 5: the kernel rm"
stage "$d3" b5-last b4-reuse "$(at 4)" < <(
{
  echo "rm /mnt/t/bk/d.bin /mnt/t/bi/i.bin; sync"
  echo "rm /mnt/t/fr/$first; sync"
}
)

echo "== ext3-recover: host debugfs rm of the intact-debugfs-indirect target"
cp "$d3/b5-last.img" "$d3/final.img"
CMDS+=("debugfs -w -R 'rm /t/di/x.bin' <img>")
debugfs -w -R "rm /t/di/x.bin" "$d3/final.img" >/dev/null

MANIFEST=()
want /t/bk/d.bin blockmap-kernel-direct bk_d.bin
want /t/bi/i.bin blockmap-kernel-indirect bi_i.bin
want /t/di/x.bin intact-debugfs-indirect di_x.bin
want /t/dr/r.bin intact-debugfs-reused dr_r.bin
want /t/dn/l1 inode-donor-aux -
want "/t/fr/$first" first-record -
want /pad/b pad-aux -
finish ext3-recover ext3 "$d3/final.img" "$k510" "$d3"/b1-layout.img "$d3"/b2-deep.img "$d3"/b3-fill.img "$d3"/b3-rm.img "$d3"/b4-reuse.img "$d3"/b5-last.img

# ====================================== ext4-recover-sameboot (5.10 kernel) ======================================
# A depth-1 file written AND removed in one boot (ruling 3C-C2, amended): the freed leaves at their home location are
# older versions of the leaf (the journal revokes the last one), so the leaf method may find a partial chain. Nothing
# else is in the volume to disturb it, and the file is never visible in a stage image, so the oracle finds its true
# blocks by their content. The interleaved pad file keeps the file fragmented, as in the probe (recover_ext4_probe.sh).
d5=$work/snap5
mkdir -p "$d5"
CMDS=()
CMDS+=("# sources: pattern bytes (tools/fixtures/lib.sh); guest kernel $k510")
echo "== ext4-recover-sameboot: format"
truncate -s 4M "$d5/s0.img"
CMDS+=("faketime -f '$base' mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 256 -U $uuid5 -E hash_seed=$uuid5 <img>")
faketime -f "$base" mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 256   -U "$uuid5" -E "hash_seed=$uuid5" "$d5/s0.img"
fsck_clean "$d5/s0.img" "mke2fs"
stage "$d5" b1-layout s0 "$(at 0)" < <(
  echo "mkdir /mnt/t; : > /mnt/t/$keeper"
)
stage "$d5" b2-sameboot b1-layout "$(at 1)" < <(
  echo "i=0; while [ \$i -lt 200 ]; do dd if=/payload/sb.src of=/mnt/t/sb.bin bs=1024 skip=\$i seek=\$i count=1 conv=notrunc 2>/dev/null; dd if=/dev/zero of=/mnt/t/pad.bin bs=1024 seek=\$i count=1 conv=notrunc 2>/dev/null; sync; i=\$((i+1)); done"
  echo "rm /mnt/t/sb.bin; sync"
)
MANIFEST=()
want /t/sb.bin deep-same-boot sb.src "" content
finish ext4-recover-sameboot ext4 "$d5/b2-sameboot.img" "$k510" "$d5"/b1-layout.img "$d5"/b2-sameboot.img

# ====================================== ext4-recover-wiped (6.1 kernel, the negative image) ======================================
d6=$work/snap6
mkdir -p "$d6"
CMDS=()
CMDS+=("# sources: pattern bytes (tools/fixtures/lib.sh); guest kernel $k61 (it wipes a deleted directory record)")
echo "== ext4-recover-wiped: format"
truncate -s 4M "$d6/s0.img"
CMDS+=("faketime -f '$base' mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 256 -U $uuid6 -E hash_seed=$uuid6 <img>")
faketime -f "$base" mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 256 \
  -U "$uuid6" -E "hash_seed=$uuid6" "$d6/s0.img"
fsck_clean "$d6/s0.img" "mke2fs"
stage "$d6" b1-layout s0 "$(at 0)" "$k61" < <(
{
  echo "mkdir /mnt/t; : > /mnt/t/$keeper"
  echo "cp /payload/wa.bin /mnt/t/wa.bin; cp /payload/wb.bin /mnt/t/wb.bin; sync"
}
)
stage "$d6" b2-last b1-layout "$(at 1)" "$k61" < <(
{
  echo "rm /mnt/t/wa.bin /mnt/t/wb.bin; sync"
}
)
MANIFEST=()
want /t/wa.bin wiped-by-kernel wa.bin
want /t/wb.bin wiped-by-kernel wb.bin
finish ext4-recover-wiped ext4 "$d6/b2-last.img" "$k61" "$d6"/b1-layout.img "$d6"/b2-last.img

# ====================================== the families, the budget, the output ======================================
python3 -I "$here/ext4_recover_oracle.py" families "$work"/final/*.expect.json
for f in "$work"/final/*.img.gz; do
  size=$(stat -c %s "$f")
  if [ "$size" -gt "$max_gz" ]; then
    echo "$(basename "$f") is $size bytes, over the $max_gz budget" >&2
    exit 1
  fi
done
cp "$work"/final/* "$out/"
for f in ext4-recover ext3-recover ext4-recover-sameboot ext4-recover-wiped; do
  echo "wrote $out/$f.img.gz and $f.expect.json"
done

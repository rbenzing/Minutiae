#!/bin/bash
# ext4 and ext3 JOURNAL fixtures of plan 3G (Task 13): volumes whose jbd2 journal was written by the KERNEL ext4 driver inside the
# QEMU TCG guest (recover_guest.sh), with an independent oracle (ext4_journal_oracle.py oracle). The host formats (mke2fs under
# faketime) and READS the finished images (debugfs, dumpe2fs, e2fsck, the oracle); no kernel module is loaded on the host, no loop
# device, no --privileged. Usage: ext4_journal.sh <outdir>   (normally: gen.sh ext4-journal, in the minutiae-recover-fixtures-510
# image; see README.md, "ext4 journal fixtures (3G)").
#
# Three images, all written by the 5.10.223 guest kernel (3C-C1: it keeps the name and inode number of a deleted directory record).
# Every boot ends with a clean unmount, except the last boot of the crash image (GUEST_HALT=crash: `echo b > /proc/sysrq-trigger`,
# no sync, no unmount). The guest clock is one hour later at each boot. Facts the script relies on (README.md, "ext4 journal probe
# (3G)"): a mount restarts logging at s_first, so an older log tail survives only beyond the new boot's tail; -J size=1 (1024 blocks)
# keeps the copies the families need; the kernel rm clears the in-inode map and zeroes i_size; a freed inode number is taken by the
# next file created in the same group, lowest first.
#
# What this script does so that the scenarios occur (placement is never trusted: the oracle reads every outcome from the finished
# image, and the generator exits 1 naming a family that did not occur, never improvising one):
#   - every target lives in its own directory whose first entry is a 200-character name (a block directory, never inline, as in 3C),
#     so that a later insertion never overwrites the deleted record of a removed name;
#   - every inode that is needed after the first rm is created BEFORE it (empty placeholders, written later), and two donor files are
#     created first: a later creation takes the lowest free inode, which would otherwise be the removed target's own;
#   - the free space is first a zero file that takes every block (reserved_clusters = 0), cut back to a 256-block hole, so that the
#     targets are written into one run and the volume can be made full again (the reuse steps need a full volume);
#   - a churn of chmod transactions wraps the log once (jgone), and another one pushes the targets to high log positions, beyond the
#     reach of the shorter boots that follow (a mount overwrites from the head).
#
# ext4-journal-clean boots (16 MiB, metadata_csum,64bit,inline_data; one guest boot each, on the previous boot's image):
#   b1 layout    directories, donors, jgone, jin (a 40-byte inline file), placeholders for every file a later boot writes
#   b2 rm        rm jgone
#   b3 churn     130 rounds of chmod+sync over 24 files (the log wraps), then a zero file to ENOSPC cut back to a 256-block hole
#   b4 write     churn again, then jreuse (head), jdeep and pad (interleaved: a depth-1 tree), jtrunc, jsmall, jh1+jh2, jswap; full
#   b5 reuse     rm the block donor (6 blocks), churn, jreuse (a 4-block tail in one request: the longest free run when the reuser comes), jtrunc truncated to 0; full again
#   b6 delete    jswap rm, jswapb written and removed over its blocks, jreuse rm and a one-block reuser, then rm of the rest
# ext4-journal-crash (same format): b1 setup, b2 x created+sync, x removed+sync, jc created and fsynced, live appended and fsynced,
#   an unsynced file; then the crash halt. The kernel's replay of a COPY of it (never committed) is the replay oracle.
# ext3-journal-clean (8 MiB, block maps, no journal checksums): b1 write, b2 rm.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "ext4_journal.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"
# shellcheck source=ext4_lib.sh
. "$here/ext4_lib.sh"

out=$(cd "${1:?outdir required}" && pwd)
k510=5.10.0-32-amd64
if [ "$kver" != "$k510" ] || [ ! -e "/boot/vmlinuz-$k510" ]; then
  echo "ext4_journal.sh needs the 5.10 guest kernel (GUEST_KVER=$k510)." >&2
  echo "Build and use the separate image: docker build -f tools/fixtures/Dockerfile.recover -t minutiae-recover-fixtures tools/fixtures &&" >&2
  echo "  docker build -f tools/fixtures/Dockerfile.recover510 -t minutiae-recover-fixtures-510 tools/fixtures" >&2
  echo "(see tools/fixtures/README.md, \"ext4 journal fixtures (3G)\")." >&2
  exit 1
fi
max_gz=409600 # 400 KiB (the fixture budget of plan 3C Task 11)
base="2023-11-14 22:13:20"
at() { date -u -d "$base UTC + $1 hours" +'%F %T'; }
uuidc=44444444-5555-6666-7777-888888888888
uuidk=44444444-5555-6666-7777-999999999999
uuid3=44444444-5555-6666-7777-aaaaaaaaaaaa
keeper="keeper_$(printf 'k%.0s' $(seq 1 193))" # 200 characters: a directory holding it is a block directory, never inline
gates_json='{"J1": "holds", "J2": "holds", "J4": "holds", "J9": "-J size=1"}'

work=$(mktemp -d /tmp/ext4-journal.XXXXXX)
keep() { # FIXTURE_KEEP_DIR=<dir>: copy the stage images, guest logs, manifests and payload there (a debugging aid, never set by gen.sh)
  if [ -n "${FIXTURE_KEEP_DIR:-}" ]; then
    mkdir -p "$FIXTURE_KEEP_DIR"
    cp -r "$work"/snap* "$work"/guest-*.log "$work"/manifest-* "$work"/gen-* "$work"/payload "$FIXTURE_KEEP_DIR"/ 2>/dev/null || true
  fi
}
trap 'keep; rm -rf "$work"' EXIT
mkdir -p "$work/final"

pay=$work/payload
mkdir -p "$pay"
mkpay() { pattern "$2" "$3" >"$pay/$1"; } # name bytes seed
mkpay jgone 3000 jg
mkpay jsmall 3067 js
mkpay jtrunc 2500 jt
mkpay jh 2000 jh
mkpay jin 40 ji
mkpay jdeep $((24 * 1024 - 9)) jd
mkpay pad $((24 * 1024 - 9)) pd
mkpay jreuse $((7 * 1024 - 11)) jr
mkpay reuser 1024 ru
mkpay jswap $((3 * 1024 - 5)) jw
mkpay jswapb $((3 * 1024 - 3)) jb
mkpay live 700 lv
mkpay app 300 ap
mkpay x 1500 xx
mkpay jc 2048 jc
mkpay cgap $((16 * 1024)) cg
mkpay bm_small $((5 * 1024 - 11)) bs
mkpay bm_ind $((20 * 1024 - 11)) bi

MANIFEST=()
LIVE=()
JONLY=()
want() { # <path> <family> <payload source name, or - for an empty file> [alias path: another name of the same inode] [content: the true map is found by content]
  MANIFEST+=("$(jq -n --arg p "$1" --arg f "$2" --arg s "$3" --arg a "${4:-}" --arg c "${5:-}" '{path: $p, family: $f, src: $s} + (if $a == "" then {} else {alias: $a} end) + (if $c == "" then {} else {true: $c} end)')")
}
wantlive() { # <path> <family> <payload source name>: a live file of the as-found view that must hold its source
  LIVE+=("$(jq -n --arg p "$1" --arg f "$2" --arg s "$3" '{path: $p, family: $f, src: $s}')")
}
wantjo() { # <path> <family> <payload source names...>: a file that exists only in committed, unreplayed transactions or differs after the replay
  local p=$1 f=$2
  shift 2
  JONLY+=("$(jq -n --arg p "$p" --arg f "$f" --args '{path: $p, family: $f, src: $ARGS.positional}' "$@")")
}
write_manifest() { # <file> <fs type> <replay absent paths, space separated> <snapshot image...>
  local f=$1 t=$2 absent=$3
  shift 3
  local fl lv jo
  fl=$(printf '%s\n' "${MANIFEST[@]}" | jq -s .)
  lv=$([ ${#LIVE[@]} -gt 0 ] && printf '%s\n' "${LIVE[@]}" | jq -s . || echo '[]')
  jo=$([ ${#JONLY[@]} -gt 0 ] && printf '%s\n' "${JONLY[@]}" | jq -s . || echo '[]')
  # shellcheck disable=SC2086
  jq -n --arg t "$t" --argjson files "$fl" --argjson live "$lv" --argjson jo "$jo" --arg absent "$absent" --args \
    '{fs_type: $t, files: $files, live: $live, journal_only: $jo, replay_absent: ($absent | split(" ") | map(select(. != ""))), snapshots: $ARGS.positional}' "$@" >"$f"
}

# stage <image dir> <name> <from> <clock>   (ops on stdin: call it as `stage ... < <(ops)`, never in a pipeline, which would lose CMDS)
# -> $dir/<name>.img. GUEST_HALT=crash in the environment ends the boot with the no-sync reboot; e2fsck -fn must then be run and recorded,
# and is clean after every other boot.
stage() {
  local dir=$1 name=$2 from=$3 clock=$4 rc=0 t0s line
  cat >"$work/ops-$name"
  cp "$dir/$from.img" "$work/cur.img"
  guest_initrd "$work/rd" ext4 "" "$pay" "$work/ops-$name" "$clock"
  t0s=$(date +%s)
  guest_run "$work/cur.img" "$work/rd" "$work/guest-$name.log"
  echo "FACT boot image=$name kernel=$kver clock=\"$clock\" halt=${GUEST_HALT:-clean} seconds=$(($(date +%s) - t0s))"
  while IFS= read -r line; do CMDS+=("guest boot $name ($clock, kernel $kver, halt ${GUEST_HALT:-clean}): $line"); done <"$work/ops-$name"
  mv "$work/cur.img" "$dir/$name.img"
  e2fsck -fn "$dir/$name.img" >"$work/fsck-$name.txt" 2>&1 || rc=$?
  echo "FACT e2fsck image=$name exit=$rc free_blocks=$(freeblocks "$dir/$name.img")"
  echo "$rc" >"$work/fsck-exit-$name"
  if [ "$rc" != 0 ] && [ "${GUEST_HALT:-}" != crash ]; then
    cat "$work/fsck-$name.txt"
    echo "e2fsck -fn is not clean after boot $name" >&2
    exit 1
  fi
}

# finish <name> <fs type> <final image> <manifest file> <fsck exit> <logdump -O: true|false> <replay log or ->: generator section, oracle, gzip
finish() {
  local name=$1 type=$2 img=$3 man=$4 fsck=$5 old=$6 replay=$7 gen ro=false args=()
  CMDS+=("python3 tools/fixtures/ext4_journal_oracle.py oracle <final> <payload> <manifest>   # the decoder, logdump, dumpe2fs, debugfs, the 3C oracle, the kernel's replay")
  gen=$(generator_json "$img" e2fsprogs python3 jq gzip faketime qemu-system-x86 busybox-static kmod cpio)
  [ "$replay" != - ] && ro=true
  jq -n --argjson g "$gen" --arg k "$k510" --argjson gates "$gates_json" --argjson fe "$fsck" --argjson lo "$old" --argjson ro "$ro" \
    '$g + {kernel: $k, fsck_exit: $fe, gates: $gates, logdump_old: $lo, replay_oracle: $ro, packages: ($g.packages + [{package: "linux-image-5.10.0-32-amd64", version: "5.10.223-1"}])}' \
    >"$work/gen-$name.json"
  args=("$img" "$pay" "$man" --generator "$work/gen-$name.json" --out "$work/final/$name.expect.json")
  [ "$replay" != - ] && args+=(--replay "$replay")
  [ "$old" = true ] && args+=(--old-logdump)
  python3 -I "$here/ext4_journal_oracle.py" oracle "${args[@]}"
  gzip -n -9 -c "$img" >"$work/final/$name.img.gz"
  echo "$name: $(stat -c %s "$img") bytes raw, $(stat -c %s "$work/final/$name.img.gz") bytes gzipped"
}

mkfs_ext4() { # <image> <uuid>
  truncate -s 16M "$1"
  CMDS+=("faketime -f '$base' mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 4096 -U $2 -E hash_seed=$2 <img>")
  faketime -f "$base" mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,inline_data,^orphan_file -m 0 -J size=1 -N 4096 \
    -U "$2" -E "hash_seed=$2" "$1"
  fsck_clean "$1" "mke2fs"
}

# ============================================================ ext4-journal-clean ============================================================
dc=$work/snapc
mkdir -p "$dc"
CMDS=()
CMDS+=("# sources: pattern bytes (tools/fixtures/lib.sh); guest kernel $k510; every boot ends with a clean unmount")
echo "== ext4-journal-clean: format"
mkfs_ext4 "$dc/s0.img" "$uuidc"

want /t/gone/jgone jgone jgone
want /t/small/jsmall jsmall jsmall
want /t/deep/jdeep jdeep jdeep
want /t/trunc/jtrunc jtrunc jtrunc
want /t/reuse/jreuse jreuse jreuse
want /t/hl/jh1 jhardlink jh /t/hl/jh2
want /t/in/jin jinline jin
want /t/swap/jswap jswap jswap
want /t/dn/jdn aux-donor -
want /t/dn/jdn2 aux-donor -
want /t/dn/jdonor aux-donor -
wantlive /t/deep/pad live-file pad
wantlive /t/hl/jh2 live-file jh
wantlive /t/w/reuser live-file reuser

echo "== ext4-journal-clean boot 1: layout, donors, jgone, placeholders"
stage "$dc" b1-layout s0 "$(at 0)" < <(
  {
    echo "mkdir /mnt/t /mnt/t/gone /mnt/t/ch /mnt/t/small /mnt/t/deep /mnt/t/trunc /mnt/t/hl /mnt/t/in /mnt/t/reuse /mnt/t/swap /mnt/t/dn /mnt/t/w /mnt/t/wb"
    echo "for d in gone ch small deep trunc hl in reuse swap dn w wb; do : > /mnt/t/\$d/$keeper; done"
    # the two donors first (the lowest inode numbers of the directory group), then jgone, then every file that a later boot needs
    echo ": > /mnt/t/dn/jdn; : > /mnt/t/dn/jdn2"
    echo "cp /payload/jgone /mnt/t/gone/jgone; sync"
    echo "i=0; while [ \$i -lt 24 ]; do : > /mnt/t/ch/c\$i; i=\$((i+1)); done"
    echo ": > /mnt/t/dn/jdonor; : > /mnt/t/reuse/jreuse; : > /mnt/t/deep/jdeep; : > /mnt/t/deep/pad; : > /mnt/t/trunc/jtrunc"
    echo ": > /mnt/t/small/jsmall; : > /mnt/t/hl/jh1; : > /mnt/t/swap/jswap"
    echo "cp /payload/jin /mnt/t/in/jin; : > /mnt/fill1; : > /mnt/fill2; : > /mnt/fill3"
    echo "sync"
  }
)

echo "== ext4-journal-clean boot 2: rm jgone"
stage "$dc" b2-rm b1-layout "$(at 1)" < <(echo "rm /mnt/t/gone/jgone; sync")

echo "== ext4-journal-clean boot 3: churn (the log wraps), then the volume is filled and a 256-block hole is cut back"
stage "$dc" b3-churn b2-rm "$(at 2)" < <(
  cat <<'OPS'
echo 0 > /sys/fs/ext4/vda/reserved_clusters
n=0; while [ $n -lt 130 ]; do m=60$((n % 2)); i=0; while [ $i -lt 24 ]; do chmod $m /mnt/t/ch/c$i; i=$((i+1)); done; sync; n=$((n+1)); done
dd if=/dev/zero of=/mnt/fill1 bs=1024 2>/dev/null || true
sync
sz=$(wc -c < /mnt/fill1); truncate -s $((sz - 256 * 1024)) /mnt/fill1
sync
OPS
)
hole=$(freeblocks "$dc/b3-churn.img")
echo "FACT hole free_blocks=$hole"
if [ "$hole" -lt 240 ] || [ "$hole" -gt 320 ]; then
  echo "the hole after b3 is $hole blocks, want about 256" >&2
  exit 1
fi

echo "== ext4-journal-clean boot 4: the targets are written into the hole (the log position of a target is high: a churn comes first)"
stage "$dc" b4-write b3-churn "$(at 3)" < <(
  cat <<'OPS'
echo 0 > /sys/fs/ext4/vda/reserved_clusters
n=0; while [ $n -lt 100 ]; do chmod 60$((n % 2)) /mnt/t/ch/c0 /mnt/t/ch/c1; sync; n=$((n+1)); done
dd if=/dev/zero of=/mnt/t/dn/jdonor bs=1024 count=6 2>/dev/null; sync
dd if=/payload/jreuse of=/mnt/t/reuse/jreuse bs=1024 count=3 2>/dev/null; sync
i=0; while [ $i -lt 24 ]; do dd if=/payload/jdeep of=/mnt/t/deep/jdeep bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; dd if=/payload/pad of=/mnt/t/deep/pad bs=1024 skip=$i seek=$i count=1 conv=notrunc 2>/dev/null; sync; i=$((i+1)); done
cp /payload/jtrunc /mnt/t/trunc/jtrunc; sync
cp /payload/jsmall /mnt/t/small/jsmall; sync
cp /payload/jh /mnt/t/hl/jh1; ln /mnt/t/hl/jh1 /mnt/t/hl/jh2; sync
cp /payload/jswap /mnt/t/swap/jswap; sync
dd if=/dev/zero of=/mnt/fill2 bs=1024 2>/dev/null || true
sync
OPS
)
full "$dc/b4-write.img"

echo "== ext4-journal-clean boot 5: the block donor goes, jreuse grows into its blocks, jtrunc is truncated"
stage "$dc" b5-reuse b4-write "$(at 4)" < <(
  cat <<'OPS'
echo 0 > /sys/fs/ext4/vda/reserved_clusters
rm /mnt/t/dn/jdonor; sync
n=0; while [ $n -lt 40 ]; do chmod 60$((n % 2)) /mnt/t/ch/c0 /mnt/t/ch/c1; sync; n=$((n+1)); done
dd if=/payload/jreuse of=/mnt/t/reuse/jreuse bs=1024 skip=3 seek=3 count=4 conv=notrunc 2>/dev/null; sync
: > /mnt/t/trunc/jtrunc; sync
dd if=/dev/zero of=/mnt/fill3 bs=1024 2>/dev/null || true
sync
OPS
)
full "$dc/b5-reuse.img"

echo "== ext4-journal-clean boot 6: the deletes (jswap first, the donors make the reusers take their inode numbers, not the targets')"
stage "$dc" b6-delete b5-reuse "$(at 5)" < <(
  cat <<'OPS'
echo 0 > /sys/fs/ext4/vda/reserved_clusters
rm /mnt/t/dn/jdn2 /mnt/t/swap/jswap; sync
cp /payload/jswapb /mnt/t/wb/jswapb; sync
rm /mnt/t/wb/jswapb; sync
rm /mnt/t/dn/jdn /mnt/t/reuse/jreuse; sync
cp /payload/reuser /mnt/t/w/reuser; sync
rm /mnt/t/small/jsmall /mnt/t/deep/jdeep /mnt/t/hl/jh1 /mnt/t/in/jin /mnt/t/trunc/jtrunc; sync
OPS
)
fsck_clean "$dc/b6-delete.img" "ext4-journal-clean final"
write_manifest "$work/manifest-clean.json" ext4 "" "$dc"/b1-layout.img "$dc"/b2-rm.img "$dc"/b3-churn.img "$dc"/b4-write.img "$dc"/b5-reuse.img
finish ext4-journal-clean ext4 "$dc/b6-delete.img" "$work/manifest-clean.json" 0 true -

# ============================================================ ext4-journal-crash ============================================================
dk=$work/snapk
mkdir -p "$dk"
CMDS=()
MANIFEST=()
LIVE=()
JONLY=()
CMDS+=("# sources: pattern bytes (tools/fixtures/lib.sh); guest kernel $k510; the last boot ends with GUEST_HALT=crash (sysrq b, no sync, no unmount)")
echo "== ext4-journal-crash: format"
mkfs_ext4 "$dk/s0.img" "$uuidk"

want /t/cx/x crash-live-copy x "" content
want /t/cx/cdn aux-donor -
want /t/cx/cgap aux-gap -
wantlive /t/live/live live-file live
wantjo /t/jc/jc crash-created jc
wantjo /t/live/live crash-update live app

echo "== ext4-journal-crash boot 1: setup (live, and a big zero file that takes the volume into its second group)"
stage "$dk" b1-setup s0 "$(at 0)" < <(
  {
    echo "mkdir /mnt/t /mnt/t/live /mnt/t/cx /mnt/t/jc"
    echo "for d in live cx jc; do : > /mnt/t/\$d/$keeper; done"
    echo "cp /payload/live /mnt/t/live/live; : > /mnt/t/live/churn; sync"
    # sixty one-block transactions: the crash boot restarts the log at its head and leaves this boot's tail behind it, as stale fragments
    echo 'n=0; while [ $n -lt 60 ]; do chmod 60$((n % 2)) /mnt/t/live/churn; sync; n=$((n+1)); done'
    echo "dd if=/dev/zero of=/mnt/bigf bs=1024 count=9000 2>/dev/null; sync"
  }
)

echo "== ext4-journal-crash boot 2: x created and removed with a sync each (the live region keeps its copies; the home inode is deleted), then jc and the append fsynced; crash"
GUEST_HALT=crash stage "$dk" b2-crash b1-setup "$(at 1)" < <(
  cat <<'OPS'
: > /mnt/t/cx/cdn
cp /payload/cgap /mnt/t/cx/cgap; sync
cp /payload/x /mnt/t/cx/x; sync
rm /mnt/t/cx/cdn /mnt/t/cx/cgap /mnt/t/cx/x; sync
dd if=/payload/jc of=/mnt/t/jc/jc bs=1024 conv=fsync 2>/dev/null
dd if=/payload/app of=/mnt/t/live/live bs=1024 oflag=append conv=notrunc,fsync 2>/dev/null
echo unsynced > /mnt/t/jc/z
OPS
)
cp "$dk/b2-crash.img" "$dk/final.img"
crash_sha=$(sha256sum "$dk/final.img" | cut -d' ' -f1)
crash_fsck=$(cat "$work/fsck-exit-b2-crash")
echo "FACT crash e2fsck_exit=$crash_fsck sha256=$crash_sha"

echo "== ext4-journal-crash: the kernel's own replay of a COPY (the crash image itself is never booted again)"
stage "$dk" r-replay b2-crash "$(at 2)" < <(
  cat <<'OPS'
dmesg | grep -i -E 'EXT4-fs|JBD2|recover' | head -n 12 | while read -r l; do echo "PROBE-DMESG $l"; done
for f in /t/jc/jc /t/live/live /t/cx/x; do set -- $(ls -i /mnt$f 2>/dev/null); echo "PROBE-INO $f ${1:-0}"; done
find /mnt -type f | while read -r f; do echo "RF $f $(wc -c < $f) $(sha256sum $f | cut -d' ' -f1)"; done
OPS
)
if [ "$(sha256sum "$dk/final.img" | cut -d' ' -f1)" != "$crash_sha" ]; then
  echo "the crash image changed while its copy was replayed" >&2
  exit 1
fi
write_manifest "$work/manifest-crash.json" ext4 "/t/cx/x /t/jc/z" "$dk"/b1-setup.img
finish ext4-journal-crash ext4 "$dk/final.img" "$work/manifest-crash.json" "$crash_fsck" false "$work/guest-r-replay.log"

# ============================================================ ext3-journal-clean ============================================================
d3=$work/snap3
mkdir -p "$d3"
CMDS=()
MANIFEST=()
LIVE=()
JONLY=()
CMDS+=("# sources: pattern bytes (tools/fixtures/lib.sh); guest kernel $k510, the ext3 image mounted by the ext4 driver")
echo "== ext3-journal-clean: format"
truncate -s 8M "$d3/s0.img"
CMDS+=("faketime -f '$base' mke2fs -q -F -t ext3 -b 1024 -O ^dir_index,^orphan_file -m 0 -J size=1 -N 2048 -U $uuid3 -E hash_seed=$uuid3 <img>")
faketime -f "$base" mke2fs -q -F -t ext3 -b 1024 -O ^dir_index,^orphan_file -m 0 -J size=1 -N 2048 -U "$uuid3" -E "hash_seed=$uuid3" "$d3/s0.img"
fsck_clean "$d3/s0.img" "mke2fs"
want /t/s/bm_small bm-small bm_small
want /t/i/bm_ind bm-indirect bm_ind

echo "== ext3-journal-clean boot 1: fillers, then the two block-mapped targets"
stage "$d3" b1-write s0 "$(at 0)" < <(
  cat <<'OPS'
mkdir /mnt/t /mnt/t/s /mnt/t/i
i=1; while [ $i -le 3 ]; do echo filler$i > /mnt/f$i; sync; i=$((i+1)); done
cp /payload/bm_small /mnt/t/s/bm_small; sync
cp /payload/bm_ind /mnt/t/i/bm_ind; sync
OPS
)
echo "== ext3-journal-clean boot 2: rm"
stage "$d3" b2-rm b1-write "$(at 1)" < <(echo "rm /mnt/t/s/bm_small /mnt/t/i/bm_ind; sync")
fsck_clean "$d3/b2-rm.img" "ext3-journal-clean final"
write_manifest "$work/manifest-ext3.json" ext3 "" "$d3"/b1-write.img
finish ext3-journal-clean ext3 "$d3/b2-rm.img" "$work/manifest-ext3.json" 0 true -

# ============================================================ the families, the budget, the output ============================================================
python3 -I "$here/ext4_journal_oracle.py" families "$work"/final/*.expect.json
for f in "$work"/final/*.img.gz; do
  size=$(stat -c %s "$f")
  if [ "$size" -gt "$max_gz" ]; then
    echo "$(basename "$f") is $size bytes, over the $max_gz budget" >&2
    exit 1
  fi
done
cp "$work"/final/* "$out/"
for f in ext4-journal-clean ext4-journal-crash ext3-journal-clean; do
  echo "wrote $out/$f.img.gz and $f.expect.json"
done

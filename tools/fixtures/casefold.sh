#!/bin/bash
# Kernel casefold lookup oracle (L26 / L38).
# Usage: casefold.sh <utf8fold testdata dir>   (normally called by gen.sh casefold)
#
# Writes THREE outputs: <outdir>/kernel-vectors.json, the ext4 image and its
# .expect.json in internal/filesys/ext4/testdata, and the F2FS image and its
# .expect.json in internal/filesys/f2fs/testdata. Run inside the
# minutiae-recover-fixtures image (QEMU + the 6.1 guest kernel):
#
#   docker run --rm --shm-size=512m -v "$PWD:/work" -v "$SCRATCH:/scratch" -w /work \
#     -e CASEFOLD_WORK=/scratch/work minutiae-recover-fixtures bash tools/fixtures/gen.sh casefold
#
# The oracle is the guest kernel's own lookups: a casefold directory per bucket
# holds the stored names, and the guest asks "does this query resolve to that
# stored name" (casefold_guest.sh). Nothing here uses Minutiae. The probe list
# comes from casefold_probes.py, which parses the Unicode 12.1.0 files itself.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "casefold.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=casefold_guest.sh
. "$here/casefold_guest.sh"

out=$(cd "${1:?outdir required}" && pwd)
ext4out=$root/internal/filesys/ext4/testdata
f2fsout=$root/internal/filesys/f2fs/testdata
mkdir -p "$ext4out" "$f2fsout"

export E2FSPROGS_FAKE_TIME=1700000000
export LC_ALL=C

# Large and intermediate files go in $CASEFOLD_WORK (a bind mount of the
# scratch folder), never in the container's filesystem. The payload tree holds
# one tiny directory per probe: tmpfs, so run the container with --shm-size.
work=${CASEFOLD_WORK:-/tmp/minutiae-casefold}
payload=/dev/shm/minutiae-casefold-payload
rm -rf "$work" "$payload"
mkdir -p "$work" "$payload"
trap 'rm -rf "$payload" "$rd" "$rd.cpio.gz"' EXIT

fsuuid=0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d
hashseed=11112222-3333-4444-5555-666677778888
probes=$here/casefold_probes.py

echo "== probe list"
run python3 "$probes" --payload "$payload"
python3 "$probes" --list >"$work/probes.json"
buckets=$(jq -r '.[].bucket' "$work/probes.json" | sort -u)
echo "buckets: $(echo "$buckets" | wc -l), probes: $(jq length "$work/probes.json")"

echo "== images"
ext4img=$work/ext4-casefold.img
f2fsimg=$work/f2fs-casefold.img
rm -f "$ext4img" "$f2fsimg"
truncate -s 32M "$ext4img"
truncate -s 128M "$f2fsimg"
# the casefold flag (0x40000000) is set on the directories while they are empty
run mke2fs -q -t ext4 -O casefold -E "encoding=utf8,hash_seed=$hashseed" -U "$fsuuid" -N 24000 -L casefold "$ext4img"
{
  echo "mkdir /cf"
  echo "mkdir /plain"
  for b in $buckets; do echo "mkdir /cf/$b"; done
} >"$work/ext4-mkdir.cmd"
run debugfs -w -f "$work/ext4-mkdir.cmd" "$ext4img" >/dev/null
for b in $buckets; do
  old=$(debugfs -R "stat /cf/$b" "$ext4img" 2>/dev/null | sed -n 's/.*Flags: *\(0x[0-9a-fA-F]*\).*/\1/p')
  [ -n "$old" ] || { echo "no flags for /cf/$b" >&2; exit 1; }
  printf 'sif /cf/%s flags 0x%x\n' "$b" $((old | 0x40000000))
done >"$work/ext4-flags.cmd"
run debugfs -w -f "$work/ext4-flags.cmd" "$ext4img" >/dev/null
CMDS+=("debugfs -w: sif /cf/<bucket> flags <old|0x40000000> for every bucket")
debugfs -R "stat /cf/00" "$ext4img" 2>/dev/null | grep -i flags
dumpe2fs -h "$ext4img" 2>/dev/null | grep -iE 'features|encoding|character'

run faketime -f '2023-11-14 22:13:20' mkfs.f2fs -q -f -r -O casefold -C utf8 -U "$fsuuid" -l casefold "$f2fsimg"
run python3 "$here/f2fs_normalize.py" "$f2fsimg"
echo "-- dump.f2fs superblock (casefold)"
dump.f2fs -d 1 "$f2fsimg" 2>&1 | grep -iE 'encoding|feature|root_ino' || true

echo "== guest"
rd=/dev/shm/minutiae-casefold-rd # tmpfs: names that differ only in case must not collide (a bind mount of a Windows folder would)
casefold_initrd "$rd" "$payload"
start=$(date +%s)
casefold_run "$ext4img" "$f2fsimg" "$rd" "$work/guest.log"
guest_secs=$(($(date +%s) - start))
echo "TCG guest wall time: $guest_secs s"
rm -f "$rd.cpio.gz"
grep -a '^V ' "$work/guest.log" >"$work/verdicts.txt"
grep -a '^G ' "$work/guest.log" >"$work/guest-notes.txt"
if grep -aE '^G mount (ext4|f2fs) rc=[1-9]' "$work/guest.log"; then echo "a guest filesystem step failed" >&2; exit 1; fi
if grep -aE '^G (cp|f2fs mkdir|bind|umount|f2fs chattr|kambig [a-z0-9]+ (setup|first|cleanup)).*rc=[1-9]|^G .*FAILED' "$work/guest.log"; then echo "a guest file step failed" >&2; exit 1; fi
grep -aqE '[[:space:]]ext4[[:space:]]*$' "$work/guest.log" || { echo "ext4 is not in /proc/filesystems" >&2; exit 1; }
grep -aqE '[[:space:]]f2fs[[:space:]]*$' "$work/guest.log" || { echo "f2fs is not in /proc/filesystems" >&2; exit 1; }

echo "== fold-equal pair (host side, after the guest has unmounted)"
# The kernel creates only one of two names it calls equal (the guest measured that: kernel_second_name in
# the meta), so the pair is written with the host tool: libext2fs 1.47.0 does NOT refuse the second name.
# The result is a host-tool state, not a kernel-written one, and the expect file says so (origin). Should
# a later libext2fs refuse it, the pair is dropped and ambiguous_pair stays null.
: >"$work/empty"
printf 'mkdir /ambig\n' >"$work/ext4-ambig1.cmd"
run debugfs -w -f "$work/ext4-ambig1.cmd" "$ext4img" >/dev/null
old=$(debugfs -R "stat /ambig" "$ext4img" 2>/dev/null | sed -n 's/.*Flags: *\(0x[0-9a-fA-F]*\).*/\1/p')
printf 'sif /ambig flags 0x%x\n' $((old | 0x40000000)) >"$work/ext4-ambig2.cmd"
printf 'write %s /ambig/Stra\303\237e\nwrite %s /ambig/STRASSE\n' "$work/empty" "$work/empty" >>"$work/ext4-ambig2.cmd"
run debugfs -w -f "$work/ext4-ambig2.cmd" "$ext4img" 2>&1 | tail -5
CMDS+=("debugfs -w: sif /ambig flags <old|0x40000000>; write an empty file named Strasse (U+00DF) and one named STRASSE into it")
ambig=$(debugfs -R "ls -p /ambig" "$ext4img" 2>/dev/null | awk -F/ 'NF>=8 && $6!="." && $6!=".."' | wc -l)
echo "names in /ambig: $ambig"
if [ "$ambig" != 2 ]; then
  echo "libext2fs refused the fold-equal pair: dropping /ambig, ambiguous_pair stays null" >&2
  printf 'rm /ambig/Stra\303\237e\nrm /ambig/STRASSE\nrmdir /ambig\n' >"$work/ext4-ambig3.cmd"
  debugfs -w -f "$work/ext4-ambig3.cmd" "$ext4img" >/dev/null 2>&1 || true
fi

echo "== read back (the exit status of each filesystem checker gates the run: a non-zero status is a failed run)"
# no `| tail` and no `|| true`: the status is the checker's own, and it is recorded in the generator
if fsck.ext4 -fn "$ext4img" >"$work/fsck-ext4.txt" 2>&1; then ext4fsck=0; else ext4fsck=$?; fi
cat "$work/fsck-ext4.txt"
cp "$f2fsimg" "$work/f2fs-copy.img"
if fsck.f2fs -f --dry-run "$work/f2fs-copy.img" >"$work/fsck-f2fs.txt" 2>&1; then f2fsfsck=0; else f2fsfsck=$?; fi
tail -12 "$work/fsck-f2fs.txt"
rm -f "$work/f2fs-copy.img"
echo "fsck exit status: ext4 $ext4fsck, f2fs $f2fsfsck"
if [ "$ext4fsck" != 0 ] || [ "$f2fsfsck" != 0 ]; then
  echo "a filesystem checker reported a problem on an image the kernel wrote: not committing (see $work/fsck-*.txt)" >&2
  exit 1
fi
# a run that timed out or stopped early is a failed run: the guest log must end with GUEST-DONE (checked
# by casefold_run) and every probe must have a verdict (checked by casefold_probes.py --vectors); never
# commit partial vectors.

kernel=$(sed -n '/G kernel: /{s/.*G kernel: //;p;q}' "$work/guest.log" | tr -d '\r')
[ -n "$kernel" ] || { echo "no kernel version in the guest log" >&2; exit 1; }
ext4sha=$(sha256sum <"$ext4img"); ext4sha=${ext4sha%% *}
f2fssha=$(sha256sum <"$f2fsimg"); f2fssha=${f2fssha%% *}
# what the kernel did with the second name of a fold-equal pair (the guest created Strasse then STRASSE)
kernel_second() { # <fs>: the guest's mkdir status of the second name and the entries left in the directory
  local rc n
  rc=$(sed -n "s/.*G kambig $1 second rc=\([0-9]*\).*/\1/p" "$work/guest.log" | tr -d '\r')
  n=$(sed -n "s/.*G kambig $1 entries=\([0-9]*\).*/\1/p" "$work/guest.log" | tr -d '\r')
  [ -n "$rc" ] && [ -n "$n" ] || { echo "no kambig measurement for $1" >&2; return 1; }
  echo "measured in the guest: creating the second of two fold-equal names failed with status $rc (0 means it succeeded), the directory then held $n entry(ies)"
}
k4=$(kernel_second ext4)
kf=$(kernel_second f2fs)

jq -n --arg kernel "$kernel" --arg e "$ext4sha" --arg f "$f2fssha" --argjson ec "$ext4fsck" --argjson fc "$f2fsfsck" \
  --argjson secs "$guest_secs" --arg k4 "$k4" --arg kf "$kf" --args \
  '{kernel: $kernel, unicode: "12.1.0", image_sha256: {ext4: $e, f2fs: $f}, fsck_exit: {ext4: $ec, f2fs: $fc}, guest_seconds: $secs, kernel_second_name: {ext4: $k4, f2fs: $kf}, commands: $ARGS.positional}' "${CMDS[@]}" >"$work/meta.json"

python3 "$probes" --vectors "$out/kernel-vectors.json.gz" --verdicts "$work/verdicts.txt" --meta "$work/meta.json"
python3 "$probes" --expect ext4 --image "$ext4img" --out "$ext4out/ext4-casefold.expect.json" --meta "$work/meta.json"
python3 "$probes" --expect f2fs --image "$f2fsimg" --out "$f2fsout/f2fs-casefold.expect.json" --meta "$work/meta.json"

gzip -n -9 -c "$ext4img" >"$ext4out/ext4-casefold.img.gz"
gzip -n -9 -c "$f2fsimg" >"$f2fsout/f2fs-casefold.img.gz"
ls -l "$ext4out/ext4-casefold.img.gz" "$f2fsout/f2fs-casefold.img.gz" "$out/kernel-vectors.json.gz" "$out/kernel-vectors.meta.json"
echo "ext4 image sha256 $ext4sha"
echo "f2fs image sha256 $f2fssha"

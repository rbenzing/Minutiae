#!/bin/bash
# SLACK fixtures of plan 3F: four filesystems (FAT16 with 2 KiB clusters, exFAT with 32 KiB clusters, ext4
# with 1 KiB blocks, F2FS) written and deleted by the KERNEL drivers inside the QEMU TCG guest
# (slack_guest.sh), with an independent oracle (slack_oracle.py). Raw table-less images pre-filled with a
# 64-byte sentinel so that the tail of a file's last allocation unit shows what the driver left there.
# No loop device, no FUSE, no --privileged, no kernel module on the host: the host formats (mkfs under
# faketime) and READS the finished images (fsck.fat, mshowfat, dump.exfat, dumpe2fs, debugfs, fsck.f2fs,
# dump.f2fs and the oracle's own raw reads). Nothing here uses Minutiae.
# Usage: slack.sh <outdir>   (normally: gen.sh slack, in the minutiae-recover-fixtures image; see
# README.md, "Slack fixtures (3F)"). <outdir> is the FAT directory; the other three are derived from the
# repository root, as casefold.sh does.
#
# Boot 1 writes the scenario on all four volumes (slack_oracle.py ops), the host reads every file's units
# (slack_oracle.py units), boot 2 removes /gone.txt, the host checks the images with the filesystems' own
# checkers, reads the units again (the live files must not have moved), appends the volume tail
# (3F-R15) and writes the expectation. The images are NOT byte-reproducible (QEMU, timestamps from the
# guest clock); the generator exits 1 unless the scenario produced what the fixtures need.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "slack.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=slack_guest.sh
. "$here/slack_guest.sh"

out_fat=$(cd "${1:?outdir required}" && pwd)
declare -A outdir=(
  [fat]="$out_fat"
  [exfat]="$root/internal/filesys/exfat/testdata/slack"
  [ext4]="$root/internal/filesys/ext4/testdata/slack"
  [f2fs]="$root/internal/filesys/f2fs/testdata/slack"
)
declare -A name=([fat]=fat16-slack [exfat]=exfat-slack [ext4]=ext4-slack [f2fs]=f2fs-slack)
declare -A mib=([fat]=16 [exfat]=16 [ext4]=8 [f2fs]=64)
fss=(fat exfat ext4 f2fs)
clock="2023-11-14 22:13:20"
max_gz=204800 # 200 KiB (3F-R15)
oracle="$here/slack_oracle.py"

if [ -n "${SLACK_WORK:-}" ]; then
  work=$SLACK_WORK
  mkdir -p "$work"
  trap '[ -n "${SLACK_KEEP:-}" ] || rm -rf "$work"/* "$work"/rd "$work"/rd.cpio.gz' EXIT
else
  work=$(mktemp -d /tmp/slack-gen.XXXXXX)
  trap 'rm -rf "$work"' EXIT
fi
mkdir -p "$work/payload" "$work/final"
export E2FSPROGS_FAKE_TIME=1700000000
fsuuid=0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d
hashseed=11112222-3333-4444-5555-666677778888

# fillimg <file> <MiB>: the 64-byte sentinel repeated (phase offset % 64)
fillimg() {
  python3 -c '
import sys
s = b"SLACK-SENTINEL-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLM"
n = int(sys.argv[2]) * 1048576
with open(sys.argv[1], "wb") as f:
    for _ in range(n // 65536):
        f.write(s * 1024)' "$1" "$2"
}

CMDS=()
CMDS+=("# scenario: tools/fixtures/slack_oracle.py (scenario, ops); payload: 201 copies of 'pattern 997 slack-fixture' (lib.sh), neither zero nor the sentinel")
CMDS+=("# every image is first filled with the 64-byte sentinel SLACK-SENTINEL-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLM at phase offset % 64")
CMDS+=("faketime -f '$clock' mkfs.fat -F 16 -s 4 -i 1234ABCD -n SLACK <fat img, 16 MiB>")
CMDS+=("faketime -f '$clock' mkfs.exfat -c 32768 -L SLACK <exfat img, 16 MiB>")
CMDS+=("faketime -f '$clock' mke2fs -q -t ext4 -b 1024 -U $fsuuid -E nodiscard,hash_seed=$hashseed -L slack <ext4 img, 8 MiB>")
CMDS+=("faketime -f '$clock' mkfs.f2fs -t 0 -U $fsuuid -l slack <f2fs img, 64 MiB>")

# the payload repeats a 997-byte pseudo-random block (never zero, never the sentinel): the committed images
# stay small because the file contents compress, while a unit that held another file's bytes would still be
# found by the oracle's offsets
pattern 997 slack-fixture >"$work/block"
for _ in $(seq 201); do cat "$work/block"; done >"$work/payload/pat"
for f in "${fss[@]}"; do
  fillimg "$work/$f.img" "${mib[$f]}"
done
faketime -f "$clock" mkfs.fat -F 16 -s 4 -i 1234ABCD -n SLACK "$work/fat.img" >/dev/null
faketime -f "$clock" mkfs.exfat -c 32768 -L SLACK "$work/exfat.img" >/dev/null
faketime -f "$clock" mke2fs -q -t ext4 -b 1024 -U $fsuuid -E nodiscard,hash_seed=$hashseed -L slack "$work/ext4.img"
faketime -f "$clock" mkfs.f2fs -t 0 -U $fsuuid -l slack "$work/f2fs.img" >/dev/null

# boot 1: the scenario on all four volumes
: >"$work/ops1"
: >"$work/ops2"
for f in "${fss[@]}"; do
  python3 "$oracle" ops "$f" "/mnt/$f" >>"$work/ops1"
  python3 "$oracle" ops-delete "$f" "/mnt/$f" >>"$work/ops2"
  while IFS= read -r line; do CMDS+=("guest boot 1 ($f): $line"); done < <(python3 "$oracle" ops "$f" "/mnt/$f")
  while IFS= read -r line; do CMDS+=("guest boot 2 ($f): $line"); done < <(python3 "$oracle" ops-delete "$f" "/mnt/$f")
done

boot() { # <ops file> <n>
  slack_initrd "$work/rd" "$work/payload" "$1"
  echo "== guest boot $2"
  slack_run "$work/rd" "$work/guest-$2.log" "$work/fat.img" "$work/exfat.img" "$work/ext4.img" "$work/f2fs.img"
}

boot "$work/ops1" 1
CMDS+=("mshowfat -i <fat img> ::<path>   # FAT chains")
CMDS+=("dump.exfat <exfat img>; the root directory's entry sets, FAT and boot sector parsed by slack_oracle.py (dump.exfat of exfatprogs 1.2.0 has no chain listing)")
CMDS+=("debugfs -R 'stat <path>' <ext4 img>   # extents with logical positions")
CMDS+=("fsck.f2fs -t --dry-run <f2fs img>; dump.f2fs -i <ino> <f2fs img>   # i_addr[] with logical positions (fsck.f2fs -M is not used: its trailing 0 token is unresolved)")
for f in "${fss[@]}"; do
  python3 "$oracle" units "$f" "$work/$f.img" "$work/payload/pat" "$work/units1-$f.json" all
done

boot "$work/ops2" 2

CMDS+=("fsck.fat -n <fat img>; fsck.exfat -n <exfat img>; e2fsck -fn <ext4 img>; fsck.f2fs -f --dry-run <copy of the f2fs img>   # all must accept the images (before the tail is appended)")
fsck.fat -n "$work/fat.img" >"$work/fsck-fat.log" 2>&1 || { cat "$work/fsck-fat.log"; echo "fsck.fat not clean" >&2; exit 1; }
fsck.exfat -n "$work/exfat.img" >"$work/fsck-exfat.log" 2>&1 || { cat "$work/fsck-exfat.log"; echo "fsck.exfat not clean" >&2; exit 1; }
e2fsck -fn "$work/ext4.img" >"$work/fsck-ext4.log" 2>&1 || { cat "$work/fsck-ext4.log"; echo "e2fsck not clean" >&2; exit 1; }
cp "$work/f2fs.img" "$work/f2fs-check.img"
fsck.f2fs -f --dry-run "$work/f2fs-check.img" >"$work/fsck-f2fs.log" 2>&1 || { cat "$work/fsck-f2fs.log"; echo "fsck.f2fs not clean" >&2; exit 1; }
rm -f "$work/f2fs-check.img"

for f in "${fss[@]}"; do
  python3 "$oracle" units "$f" "$work/$f.img" "$work/payload/pat" "$work/units2-$f.json" live
  python3 "$oracle" same "$work/units1-$f.json" "$work/units2-$f.json"
  fs_size=$(jq .fs_size "$work/units1-$f.json")
  [ "$fs_size" -eq $((${mib[$f]} * 1048576)) ] || { echo "$f: the tool states a filesystem of $fs_size bytes, the image is ${mib[$f]} MiB" >&2; exit 1; }
done

CMDS+=("slack_oracle.py tail <img> <filesystem size>   # appends 512 KiB of sentinel, 512 KiB of zeros, 4196 bytes of sentinel (the filesystem never addresses them)")
CMDS+=("slack_oracle.py expect <fs> <img> <units> <generator>   # reads the image bytes at every range it states")
for f in "${fss[@]}"; do
  python3 "$oracle" tail "$work/$f.img" "$(jq .fs_size "$work/units1-$f.json")"
  generator_json "$work/$f.img" dosfstools mtools exfatprogs e2fsprogs f2fs-tools python3 coreutils jq gzip faketime qemu-system-x86 linux-image-6.1.0-53-amd64 busybox-static kmod cpio >"$work/gen-$f.json"
  python3 "$oracle" expect "$f" "$work/$f.img" "$work/units1-$f.json" "$work/gen-$f.json" "$work/final/${name[$f]}.expect.json"
  gzip -n -9 -c "$work/$f.img" >"$work/final/${name[$f]}.img.gz"
  size=$(stat -c %s "$work/final/${name[$f]}.img.gz")
  echo "${name[$f]}: $size bytes gzipped"
  if [ "$size" -gt "$max_gz" ]; then
    echo "${name[$f]}.img.gz is $size bytes, over the $max_gz budget (3F-R15)" >&2
    exit 1
  fi
done

for f in "${fss[@]}"; do
  mkdir -p "${outdir[$f]}"
  cp "$work/final/${name[$f]}.img.gz" "$work/final/${name[$f]}.expect.json" "${outdir[$f]}/"
  # never report success for nothing
  for p in "${outdir[$f]}/${name[$f]}.img.gz" "${outdir[$f]}/${name[$f]}.expect.json"; do
    [ -s "$p" ] || { echo "slack.sh wrote no $p" >&2; exit 1; }
    echo "wrote $p"
  done
done

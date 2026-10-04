#!/bin/bash
# Populated APFS fixture: a 32 MiB container formatted by mkapfs and then
# filled by the REAL linux-apfs-rw kernel driver, booted under QEMU
# (apfs_populate.sh), from the deterministic source tree of apfs_tree.py.
# Usage: apfs-populated.sh <outdir>   (normally called by gen.sh apfs-populated)
#
# Runs in the SEPARATE image minutiae-fixtures-apfs (Dockerfile.apfs); the main
# minutiae-fixtures image has neither the module nor QEMU. No --privileged.
#
#   apfs-populated   mkapfs -L populated (case-insensitive, the default), 32 MiB;
#                    a snapshot "snap1" taken part-way
#
# Gates, all of which fail the script: apfsck accepts the image (exit status 0,
# and does not change it); apfs_populated_oracle.py decodes the image alone and
# every cross-check passes: the live tree and the snapshot's tree equal the
# source trees (apfs_tree.py's snap1/ and final/) in every path, type, size,
# SHA-256, mode, owner, mtime, symlink target, xattr and hard-link group.
#
# Reproducibility: NOT byte-identical across runs. The guest clock is virtual
# and fixed and the volume's UUIDs are arguments, but the number of
# transactions the driver commits (hence every xid, object id and block
# address after the first few) depends on the timing of the guest's background
# writeback, which QEMU's emulation does not make deterministic. Regenerate the
# image and its expect.json TOGETHER; the Go tests check the image's sha256
# against the oracle first, so a stale pair fails at once.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "apfs-populated.sh must run as root (the container does)" >&2
  exit 1
fi
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
for tool in mkapfs apfsck faketime jq python3 gzip qemu-system-x86_64 gcc; do
  command -v "$tool" >/dev/null || { echo "apfs-populated.sh needs $tool: build and use the minutiae-fixtures-apfs image (README.md, \"APFS populated fixture\")" >&2; exit 1; }
done
[ -f /usr/local/lib/apfs/apfs.ko ] || { echo "no /usr/local/lib/apfs/apfs.ko: use the minutiae-fixtures-apfs image" >&2; exit 1; }

# shellcheck source=lib.sh
. "$here/lib.sh"

out=$(cd "${1:?outdir required}" && pwd)

export LC_ALL=C.UTF-8
export NO_FAKE_STAT=1 FAKETIME_DONT_FAKE_MONOTONIC=1
clock="2023-11-14 22:13:20" # unix 1700000000

base=${TMPDIR:-/tmp}
work=$base/minutiae-apfs-populated # fixed name: the commands recorded in the oracle mention it
cleanup() { rm -rf "$work"; }
rm -rf "$work"
mkdir "$work"
trap cleanup EXIT

name=apfs-populated
label=populated
cu=3f6b2c14-8a5e-4d70-b9c1-d4e7f0a35b86
vu=9c1d7e42-5b3a-4f68-a2e0-17b6c8d94f53
img=$work/$name.img

CMDS=()
CMDS+=("truncate -s 32M <img>")
truncate -s 32M "$img"
CMDS+=("faketime -f '$clock' mkapfs -L $label -U $cu -u $vu <img>")
faketime -f "$clock" mkapfs -L "$label" -U "$cu" -u "$vu" "$img" >"$work/mkapfs.log" 2>&1 || { cat "$work/mkapfs.log"; echo "mkapfs failed" >&2; exit 1; }
CMDS+=("bash tools/fixtures/apfs_populate.sh <img> <work>   # QEMU + Debian kernel + busybox initramfs + linux-apfs-rw; phase tars and ops from apfs_tree.py")
bash "$here/apfs_populate.sh" "$img" "$work/populate"

before=$(sha256sum "$img")
if ! apfsck "$img" >"$work/apfsck.txt" 2>&1; then
  cat "$work/apfsck.txt"
  echo "apfsck rejected the populated image" >&2
  exit 1
fi
after=$(sha256sum "$img")
[ "$before" = "$after" ] || { echo "apfsck changed the image" >&2; exit 1; }
CMDS+=("apfsck <img>   # exit status 0 = structurally valid")

CMDS+=("python3 tools/fixtures/apfs_populated_oracle.py <img> <populate/tree> $label $cu $vu")
python3 "$here/apfs_populated_oracle.py" "$img" "$work/populate/tree" "$label" "$cu" "$vu" >"$work/oracle.json"

note="Populated container written by the linux-apfs-rw kernel driver (booted under QEMU by apfs_populate.sh) from the deterministic source tree of apfs_tree.py, then taken through a snapshot (snap1) and further changes. Holds: a 2-level fs tree (600-entry directory), a 3 MiB file, two files written alternately block by block (fragmented, each in over 100 extents), four sparse files (holes first, between, last, only), symlinks (relative, absolute, dangling, to a directory, a 203-byte target, non-ASCII), xattrs (embedded, empty, binary, stream-stored, on a directory), a three-link hard-link group, a clone pair sharing every extent, NFC and NFD non-ASCII names (Latin, Greek, Cyrillic, Hangul, Japanese, an emoji, special case folds) with their name hashes, modes (setuid, sticky, read-only) and owners, a snapshot that differs from the live tree (a deleted file, a rewritten file, a new directory). Not covered: encryption, compression, a second volume, several snapshots, snapshot deletion, case-sensitive volumes, deleted-file recovery. The image is NOT reproducible byte for byte (see apfs-populated.sh); the expect.json belongs to this exact image (generator.image_sha256)."
gen=$(generator_json "$img" util-linux faketime python3 coreutils jq gzip qemu-system-x86 linux-image-6.1.0-53-amd64 busybox-static cpio gcc-12 linux-headers-6.1.0-53-amd64 |
  jq --arg note "$note" --arg progs "$APFSPROGS_COMMIT" --arg rw "$APFS_RW_COMMIT" --arg ko "$(modinfo -F version /usr/local/lib/apfs/apfs.ko)" \
    --rawfile ck "$work/apfsck.txt" \
    '. + {note: $note, sources: {apfsprogs: ("linux-apfs/apfsprogs v0.2.1 commit " + $progs), "linux-apfs-rw": ("linux-apfs/linux-apfs-rw " + $ko + " commit " + $rw)}, apfsck_output: $ck}')
# python, not jq, merges the two: jq turns the oracle's 64-bit nanosecond times into doubles
printf '%s' "$gen" >"$work/generator.json"
python3 - "$work/oracle.json" "$work/generator.json" >"$out/$name.expect.json" <<'EOP'
import json, sys
o = json.load(open(sys.argv[1]))
o["generator"] = json.load(open(sys.argv[2]))
json.dump(o, sys.stdout, separators=(",", ":"), ensure_ascii=False)
print()
EOP
gzip -n -9 -c "$img" >"$out/$name.img.gz"
echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"

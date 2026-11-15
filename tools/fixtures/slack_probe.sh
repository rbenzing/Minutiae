#!/bin/bash
# Residue probe of plan 3F (slack): what does each route leave in the tail of a file's last
# allocation unit? Run inside the minutiae-recover-fixtures image (README.md, "Slack probe (3F)"):
#   docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/slack_probe.sh
# No --privileged, no FUSE, no loop devices, no host mounts. Every file operation runs in the QEMU
# TCG guest (slack_guest.sh); the host only formats raw images (under faketime) that were filled
# with a 64-byte sentinel first, and READS the finished images with the filesystems' own tools
# (fsck.fat, mshowfat, dump.exfat, dumpe2fs, debugfs, fsck.f2fs) and a raw reader. Nothing here
# uses Minutiae.
#
# Output lines:
#   S1 <fs> <variant> ...      sentinel cells (64-byte, aligned) of the data area left after mkfs
#   GEOM <fs> <variant> ...    the geometry the tools reported
#   TAIL <fs> <variant> <path> <size> ...   the bytes after EOF in the file's last unit (S2)
#   RETAIL ...                 the same range re-read after a delete
#   HOSTTAIL ...               the same for the host-tool routes (S3)
#   S4 ...                     mkfs with a size smaller than the device (informational)
#   FSCK ...                   the consistency checkers' exit status on the finished images
#
# SLACK_PROBE_WORK names the directory for images (default /tmp/slack-probe; give it a bind
# mount to keep the container's own filesystem small).
set -eu
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=slack_guest.sh
. "$here/slack_guest.sh"

work=${SLACK_PROBE_WORK:-/tmp/slack-probe}
rm -rf "$work"
mkdir -p "$work/payload"
rd=/tmp/slack-probe-rd
trap 'rm -rf "$rd" "$rd.cpio.gz"' EXIT
ft="2023-11-14 22:13:20"
export E2FSPROGS_FAKE_TIME=1700000000
fsuuid=0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d
hashseed=11112222-3333-4444-5555-666677778888

# The measuring helper: a raw reader plus the tools' own listings (never Minutiae).
cat >"$work/measure.py" <<'PY'
import re
import subprocess
import sys

SENT = b"SLACK-SENTINEL-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLM"
assert len(SENT) == 64


def tool(*args):
    res = subprocess.run(list(args), capture_output=True, text=True)
    return res.stdout + res.stderr if res.returncode else res.stdout


def num(text, pattern):
    m = re.search(pattern, text, re.M)
    if not m:
        sys.exit("pattern %r not found in:\n%s" % (pattern, text[:2000]))
    return int(m.group(1))


def geom(fs, img):
    """data_start, size (bytes of the filesystem as its own tool states it)."""
    if fs == "fat":
        t = tool("fsck.fat", "-v", "-n", img)
        return num(t, r"^Data area starts at byte (\d+)"), num(t, r"^\s*(\d+) sectors total") * num(t, r"^\s*(\d+) bytes per logical sector")
    if fs == "exfat":
        t = tool("dump.exfat", img)
        ss = 1 << num(t, r"^Sector Size Bits:\s+(\d+)")
        return num(t, r"^Cluster Heap Offset \(sector offset\):\s+(\d+)") * ss, num(t, r"^Volume Length\(sectors\):\s+(\d+)") * ss
    if fs == "ext4":
        t = tool("dumpe2fs", img)
        bs = num(t, r"^Block size:\s+(\d+)")
        end = num(t, r"Inode table at \d+-(\d+)")
        return (end + 1) * bs, num(t, r"^Block count:\s+(\d+)") * bs
    if fs == "f2fs":
        t = tool("fsck.f2fs", "-l", img)
        return num(t, r"^main_blkaddr:\s+(\d+)") * 4096, num(t, r"^block_count:\s+(\d+)") * 4096
    sys.exit("unknown fs " + fs)


def classify(buf, off):
    z = s = o = 0
    for i, b in enumerate(buf):
        if b == 0:
            z += 1
        elif b == SENT[(off + i) % 64]:
            s += 1
        else:
            o += 1
    lead0 = len(buf) - len(buf.lstrip(b"\0"))
    uniform = 1 if buf and buf.count(buf[:1]) == len(buf) else 0
    first = "0x%02x" % buf[0] if buf else "-"
    return "tail=%d zero=%d lead0=%d sentinel=%d other=%d uniform=%d first=%s" % (len(buf), z, lead0, s, o, uniform, first)


def unit_map(fs, img, path, unit, data_start):
    """{logical unit index: (image offset, note)} for the file's allocated units."""
    out = {}
    if fs == "fat":
        t = tool("mshowfat", "-i", img, "::" + path).strip().splitlines()[-1]
        m = re.fullmatch(r"::(.*?)((?: <[0-9,\-]*>)+)", t)
        if not m:
            return out
        idx = 0
        for part in filter(None, re.split(r"[ <>,]+", m.group(2))):
            lo, _, hi = part.partition("-")
            for c in range(int(lo), int(hi or lo) + 1):
                out[idx] = (data_start + (c - 2) * unit, "")
                idx += 1
    elif fs == "exfat":
        # dump.exfat of this image has no chain option: read the root directory's entry sets
        # ourselves (boot sector, FAT, 0x85 file / 0xC0 stream / 0xC1 name; every probe file is in the root)
        with open(img, "rb") as f:
            d = f.read()
        bps = 1 << d[108]
        cs = bps << d[109]
        fat_off = int.from_bytes(d[80:84], "little") * bps
        ncl = int.from_bytes(d[92:96], "little")

        def fat(c):
            return int.from_bytes(d[fat_off + 4 * c:fat_off + 4 * c + 4], "little")

        def clusters(first, nofat, size):
            need = -(-size // cs)
            if nofat:
                return list(range(first, first + need))
            res, c = [], first
            while 2 <= c <= ncl + 1 and len(res) < need:
                res.append(c)
                c = fat(c)
            return res

        root = clusters(int.from_bytes(d[96:100], "little"), False, ncl * cs)
        raw = b"".join(d[data_start + (c - 2) * cs:data_start + (c - 1) * cs] for c in root)
        i = 0
        while i + 32 <= len(raw) and raw[i] != 0:
            if raw[i] == 0x85:
                cnt = raw[i + 1]
                st = raw[i + 32:i + 64]
                nlen = st[3]
                name = b"".join(raw[i + 64 + 32 * k + 2:i + 64 + 32 * k + 32] for k in range(cnt - 1)).decode("utf-16-le")[:nlen]
                if "/" + name == path:
                    first = int.from_bytes(st[20:24], "little")
                    size = int.from_bytes(st[24:32], "little")
                    vdl = int.from_bytes(st[8:16], "little")
                    for idx, c in enumerate(clusters(first, bool(st[1] & 2), size)):
                        out[idx] = (data_start + (c - 2) * cs, "datalen=%d vdl=%d nofatchain=%d" % (size, vdl, 1 if st[1] & 2 else 0))
                i += 32 * (1 + cnt)
            else:
                i += 32
    elif fs == "ext4":
        t = tool("debugfs", "-R", "stat " + path, img)
        for m in re.finditer(r"\((\d+)(?:-(\d+))?( \[uninit\])?\):(\d+)(?:-(\d+))?", t):
            lo = int(m.group(1))
            hi = int(m.group(2) or lo)
            for k in range(hi - lo + 1):
                out[lo + k] = ((int(m.group(4)) + k) * unit, "uninit" if m.group(3) else "")
    elif fs == "f2fs":
        text = tool("fsck.f2fs", "-d", "1", "-M", "--dry-run", img)
        for chunk in re.split(r"[\r\n]|\x1b\[2K", text):
            if not chunk.startswith(path + " "):
                continue
            idx = 0
            for tok in chunk[len(path) + 1:].split(" "):
                if not re.fullmatch(r"\d+(-\d+)?", tok):
                    continue
                if tok == "0":
                    idx += 1
                    continue
                a, _, b = tok.partition("-")
                for blk in range(int(a), int(b or a) + 1):
                    out[idx] = (blk * unit, "")
                    idx += 1
            print("RAWMAP f2fs %s %s" % (path, chunk.strip()))
    return out


def main():
    mode = sys.argv[1]
    if mode == "geom":
        fs, img = sys.argv[2:4]
        ds, size = geom(fs, img)
        print("data_start=%d fs_size=%d" % (ds, size))
    elif mode == "cells":
        # sentinel cells in [start, end) of the image: cells <img> <start> <end>
        img, start, end = sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
        with open(img, "rb") as f:
            f.seek(start)
            buf = f.read(end - start)
        total = len(buf) // 64
        hit = sum(1 for i in range(total) if buf[i * 64:(i + 1) * 64] == SENT)
        print("cells=%d sentinel_cells=%d" % (total, hit))
    elif mode == "files":
        # files <fs> <img> <unit> <variant> <label> path:size ...
        fs, img, unit, variant, label = sys.argv[2], sys.argv[3], int(sys.argv[4]), sys.argv[5], sys.argv[6]
        ds, size = geom(fs, img)
        print("GEOM %s %s %s data_start=%d fs_size=%d unit=%d" % (label, fs, variant, ds, size, unit))
        f = open(img, "rb")
        for spec in sys.argv[7:]:
            path, _, sz = spec.rpartition(":")
            sz = int(sz)
            um = unit_map(fs, img, path, unit, ds)
            if not um:
                print("%s %s %s %s %d NO-UNITS (inline, empty or not listed)" % (label, fs, variant, path, sz))
                continue
            last = (sz - 1) // unit
            if last not in um:
                print("%s %s %s %s %d units=%d HOLE-AT-LAST-UNIT (index %d)" % (label, fs, variant, path, sz, len(um), last))
                continue
            off, note = um[last]
            used = sz - last * unit
            tail_off = off + used
            f.seek(tail_off)
            buf = f.read(unit - used)
            print("%s %s %s %s %d units=%d unit_off=%d tail_off=%d %s %s" % (label, fs, variant, path, sz, len(um), off, tail_off, classify(buf, tail_off), note))
    elif mode == "reread":
        # reread <img> <label> <fs> <variant> <path> <tail_off> <len>
        img, label, fs, variant, path, off, ln = sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6], int(sys.argv[7]), int(sys.argv[8])
        with open(img, "rb") as f:
            f.seek(off)
            buf = f.read(ln)
        print("%s %s %s %s tail_off=%d %s" % (label, fs, variant, path, off, classify(buf, off)))
    else:
        sys.exit("unknown mode " + mode)


main()
PY
meas() { python3 "$work/measure.py" "$@"; }

# the 64-byte sentinel, repeated: an image filled with it has the sentinel at phase offset % 64
fillimg() { # <file> <MiB>
  python3 -c '
import sys
s = b"SLACK-SENTINEL-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLM"
n = int(sys.argv[2]) * 1048576
with open(sys.argv[1], "wb") as f:
    for _ in range(n // 65536):
        f.write(s * 1024)' "$1" "$2"
}

# payload: one deterministic pattern; each file is a prefix of it (never zero, never the sentinel)
pattern 200000 slack-probe >"$work/payload/pat"
# the ops file of one boot is built from these file specs (name:size), unit-dependent
specs() { # <unit>: path:size ...
  local u=$1
  echo "/s1:1 /s600:600 /sum1:$((u - 1)) /su:$u /sup1:$((u + 1)) /s3u5:$((3 * u + 5)) /s5000:5000 /sgrow:2500 /ssparse:5000 /sgone:$((u >= 4096 ? 5000 : 1500))"
}
write_ops() { # <ops file> <unit fat> <unit exfat> <unit ext4> <unit f2fs>
  local ops=$1 fsn u
  : >"$ops"
  for pair in "fat:$2" "exfat:$3" "ext4:$4" "f2fs:$5"; do
    fsn=${pair%%:*}; u=${pair##*:}
    local d=/mnt/$fsn
    {
      echo "head -c 1 /payload/pat >$d/s1"
      echo "head -c 600 /payload/pat >$d/s600"
      echo "head -c $((u - 1)) /payload/pat >$d/sum1"
      echo "head -c $u /payload/pat >$d/su"
      echo "head -c $((u + 1)) /payload/pat >$d/sup1"
      echo "head -c $((3 * u + 5)) /payload/pat >$d/s3u5"
      echo "head -c 5000 /payload/pat >$d/s5000"
      echo "head -c 1000 /payload/pat >$d/sgrow"
      echo "sync"
      echo "head -c 1500 /payload/pat >>$d/sgrow"
      echo "head -c 700 /payload/pat >$d/ssparse"
      echo "truncate -s 5000 $d/ssparse"
      echo "head -c $((u >= 4096 ? 5000 : 1500)) /payload/pat >$d/sgone"
      echo "sync"
    } >>"$ops"
  done
}

# format <variant> <fat -s> <exfat -c> <ext4 -b>: four sentinel-filled images, formatted under faketime
format_variant() {
  local v=$1 fats=$2 exc=$3 e4b=$4 d=$work/$1
  mkdir -p "$d"
  fillimg "$d/fat.img" 16; fillimg "$d/exfat.img" 16; fillimg "$d/ext4.img" 8; fillimg "$d/f2fs.img" 64
  faketime -f "$ft" mkfs.fat -F 16 -s "$fats" -i 1234ABCD -n SLACK "$d/fat.img" >/dev/null
  faketime -f "$ft" mkfs.exfat -c "$exc" -L SLACK "$d/exfat.img" >/dev/null
  faketime -f "$ft" mke2fs -q -t ext4 -b "$e4b" -U $fsuuid -E nodiscard,hash_seed=$hashseed -L slack "$d/ext4.img"
  faketime -f "$ft" mkfs.f2fs -t 0 -U $fsuuid -l slack "$d/f2fs.img" >/dev/null
  # S1: sentinel cells of the data area that survived mkfs
  local fsn gs
  for fsn in fat exfat ext4 f2fs; do
    gs=$(meas geom "$fsn" "$d/$fsn.img")
    ds=${gs%% *}; ds=${ds#data_start=}
    sz=${gs##*fs_size=}
    echo "S1 $fsn $v $gs $(meas cells "$d/$fsn.img" "$ds" "$sz")"
  done
}

units_of() { # <variant> -> sets uf ue u4 (fat, exfat, ext4 units)
  case $1 in
    A) uf=2048; ue=32768; u4=1024 ;;
    B) uf=512; ue=4096; u4=4096 ;;
  esac
}

boot() { # <variant> <stage> <ops file>
  local v=$1 st=$2 ops=$3 d=$work/$1
  slack_initrd "$rd" "$work/payload" "$ops"
  echo "== guest boot $v/$st"
  slack_run "$rd" "$work/guest-$v-$st.log" "$d/fat.img" "$d/exfat.img" "$d/ext4.img" "$d/f2fs.img"
}

measure_all() { # <variant> <label>
  local v=$1 label=$2 d=$work/$1
  units_of "$v"
  # shellcheck disable=SC2046
  meas files fat "$d/fat.img" $uf "$v" "$label" $(specs $uf)
  # shellcheck disable=SC2046
  meas files exfat "$d/exfat.img" $ue "$v" "$label" $(specs $ue)
  # shellcheck disable=SC2046
  meas files ext4 "$d/ext4.img" $u4 "$v" "$label" $(specs $u4)
  # shellcheck disable=SC2046
  meas files f2fs "$d/f2fs.img" 4096 "$v" "$label" $(specs 4096)
}

checkers() { # <variant>
  local d=$work/$1 rc
  fsck.fat -n "$d/fat.img" >/dev/null 2>&1 && rc=0 || rc=$?; echo "FSCK fat $1 rc=$rc"
  fsck.exfat -n "$d/exfat.img" >/dev/null 2>&1 && rc=0 || rc=$?; echo "FSCK exfat $1 rc=$rc"
  e2fsck -fn "$d/ext4.img" >/dev/null 2>&1 && rc=0 || rc=$?; echo "FSCK ext4 $1 rc=$rc"
  cp "$d/f2fs.img" "$work/f2fs-check.img"
  fsck.f2fs -f --dry-run "$work/f2fs-check.img" >/dev/null 2>&1 && rc=0 || rc=$?; echo "FSCK f2fs $1 rc=$rc"
  rm -f "$work/f2fs-check.img"
}

echo "== variant A: FAT16 -s 4 (2048), exFAT 32768, ext4 1024, F2FS 4096"
format_variant A 4 32768 1024
write_ops "$work/ops-A1" 2048 32768 1024 4096
boot A write "$work/ops-A1"
measure_all A TAIL
checkers A
# the deleted file's last unit: where did its tail go, and does the delete change it? Re-read the
# range the TAIL line printed after the guest removed the file.
{
  for fsn in fat exfat ext4 f2fs; do echo "rm /mnt/$fsn/sgone"; done
  echo "sync"
} >"$work/ops-A2"
meas files fat "$work/A/fat.img" 2048 A TAIL /sgone:1500 >"$work/gone-fat.txt"
meas files exfat "$work/A/exfat.img" 32768 A TAIL /sgone:5000 >"$work/gone-exfat.txt"
meas files ext4 "$work/A/ext4.img" 1024 A TAIL /sgone:1500 >"$work/gone-ext4.txt"
meas files f2fs "$work/A/f2fs.img" 4096 A TAIL /sgone:5000 >"$work/gone-f2fs.txt"
boot A delete "$work/ops-A2"
for fsn in fat exfat ext4 f2fs; do
  # the line "TAIL <fs> A /sgone <size> units=.. unit_off=.. tail_off=<n> tail=<len> ..." names the range
  line=$(grep -a '/sgone' "$work/gone-$fsn.txt" | grep -a 'tail_off=') || { echo "no tail for the deleted file on $fsn"; continue; }
  off=${line##* tail_off=}; off=${off%% *}
  len=${line##* tail=}; len=${len%% *}
  meas reread "$work/A/$fsn.img" RETAIL "$fsn" A /sgone "$off" "$len"
done
checkers A

echo "== variant B: FAT16 -s 1 (512), exFAT 4096, ext4 4096, F2FS 4096"
format_variant B 1 4096 4096
write_ops "$work/ops-B1" 512 4096 4096 4096
boot B write "$work/ops-B1"
measure_all B TAIL
checkers B

echo "== S3: host-tool routes on fresh sentinel images (FAT16 -s 4, ext4 1024, F2FS)"
src=$work/src
mkdir -p "$src"
mkdir -p "$src/fat" "$src/ext4" "$src/f2fs"
hostfiles() { # <dir> <unit>: the same files, plain (the sparse and grown ones as their final size)
  local dir=$1 u=$2 spec p s
  for spec in $(specs "$u"); do
    p=${spec%%:*}; s=${spec##*:}
    pattern "$s" "host$p" >"$dir$p"
  done
}
hostfiles "$src/fat" 2048
hostfiles "$src/ext4" 1024
hostfiles "$src/f2fs" 4096
h=$work/host; mkdir -p "$h"
fillimg "$h/fat.img" 16
faketime -f "$ft" mkfs.fat -F 16 -s 4 -i 1234ABCD -n SLACK "$h/fat.img" >/dev/null
mcopy -i "$h/fat.img" "$src"/fat/* :: && echo "S3 mcopy rc=0" || echo "S3 mcopy rc=$?"
# shellcheck disable=SC2046
meas files fat "$h/fat.img" 2048 mcopy HOSTTAIL $(specs 2048)
fillimg "$h/ext4.img" 8
faketime -f "$ft" mke2fs -q -t ext4 -b 1024 -U $fsuuid -E nodiscard,hash_seed=$hashseed -L slack -d "$src/ext4" "$h/ext4.img" && echo "S3 mke2fs -d rc=0" || echo "S3 mke2fs -d rc=$?"
# shellcheck disable=SC2046
meas files ext4 "$h/ext4.img" 1024 mke2fs-d HOSTTAIL $(specs 1024)
fillimg "$h/f2fs.img" 64
faketime -f "$ft" mkfs.f2fs -t 0 -U $fsuuid -l slack "$h/f2fs.img" >/dev/null
if command -v sload.f2fs >/dev/null; then
  (cd "$h" && sload.f2fs -f "$src/f2fs" -t / "$h/f2fs.img" >"$h/sload.txt" 2>&1) && echo "S3 sload.f2fs rc=0" || { echo "S3 sload.f2fs rc=$?"; tail -n 5 "$h/sload.txt"; }
  # shellcheck disable=SC2046
  meas files f2fs "$h/f2fs.img" 4096 sload HOSTTAIL $(specs 4096)
else
  echo "S3 sload.f2fs not installed"
fi

echo "== S4: mkfs with a size smaller than the device (informational)"
fillimg "$work/s4.img" 16
mkfs.fat -F 16 -s 1 -i 1234ABCD "$work/s4.img" 8192 >/dev/null 2>&1 && echo "S4 mkfs.fat with a block count rc=0" || echo "S4 mkfs.fat with a block count rc=$?"
echo "S4 mkfs.fat with a block count: $(meas geom fat "$work/s4.img" 2>&1 | tail -n 1); past 8 MiB: $(meas cells "$work/s4.img" 8388608 16777216)"
fillimg "$work/s4.img" 16
mkfs.exfat -c 4096 "$work/s4.img" 2048 >/dev/null 2>&1 && echo "S4 mkfs.exfat with a size argument rc=0" || echo "S4 mkfs.exfat with a size argument rc=$? (its usage lists no size option)"
fillimg "$work/s4.img" 16
mke2fs -q -t ext4 -b 1024 -E nodiscard "$work/s4.img" 8192 >/dev/null 2>&1 && echo "S4 mke2fs with a block count rc=0" || echo "S4 mke2fs with a block count rc=$?"
echo "S4 mke2fs with a block count: $(meas geom ext4 "$work/s4.img" 2>&1 | tail -n 1); past 8 MiB: $(meas cells "$work/s4.img" 8388608 16777216)"
fillimg "$work/s4.img" 96
mkfs.f2fs -t 0 "$work/s4.img" 131072 >/dev/null 2>&1 && echo "S4 mkfs.f2fs with a sector count rc=0" || echo "S4 mkfs.f2fs with a sector count rc=$?"
echo "S4 mkfs.f2fs with a sector count: $(meas geom f2fs "$work/s4.img" 2>&1 | tail -n 1); past 64 MiB: $(meas cells "$work/s4.img" 67108864 100663296)"
echo "SLACK-PROBE-DONE"

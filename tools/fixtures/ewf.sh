#!/bin/bash
# EWF (E01) fixtures acquired by the real ewfacquire from a GPT disk built out
# of two COMMITTED fixtures, decoded by an independent decoder and cross-checked
# with ewfverify, ewfinfo and ewfexport.
# Usage: ewf.sh <outdir>   (normally called by gen.sh; the repo root must be the
#        container's /work, because the source fixtures are read from the tree)
#
# Source disk (8 MiB, 512-byte sectors, GPT):
#   partition 1  the smallest committed ext4 fixture (internal/filesys/ext4/testdata)
#   partition 2  internal/filesys/fat/testdata/fat12.img.gz
#
# Variants (the tool's default format; -d sha1 so a digest section is written):
#   single-none  1 segment,  no compression,   64-sector chunks
#   single-best  1 segment,  best compression, 128-sector chunks
#   multi-none   >= 3 segments (-S 2 MiB), no compression, 64-sector chunks
#   seed-small   1 segment,  best compression, 64-sector chunks, first MiB only
#
# Order of work: acquire, normalise (ewf_normalize.py), decode and compare with
# the raw disk (ewf_inspect.py), cross-check with the external tools, and only
# THEN gzip. Only whitelisted numeric / hash fields are parsed out of the tools'
# output; no tool output is stored verbatim. Never Minutiae.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "ewf.sh must run as root (the container does)" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
repo=$(cd "$here/../.." && pwd)
out=$(cd "${1:?outdir required}" && pwd)

# ewfacquire stamps the acquisition and system dates from the clock, so it runs
# under a frozen faketime clock (real time still ticks for monotonic timers).
export FAKETIME_DONT_FAKE_MONOTONIC=1 NO_FAKE_STAT=1
clock="2023-11-14 22:13:20"

# frun <cmd...>: run under the frozen clock and record the command.
frun() {
  CMDS+=("faketime -f '$clock' $*")
  faketime -f "$clock" "$@"
}

die() {
  echo "ewf.sh: $*" >&2
  exit 1
}

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-ewf # fixed name: the commands recorded in the oracle mention it
cleanup() { rm -rf "$work"; }
trap cleanup EXIT
rm -rf "$work"
mkdir "$work"
cd "$work"

# ---------------------------------------------------------------------------
# 1. The raw source disk, built from committed fixtures.

ext4_src=
ext4_size=
for f in "$repo"/internal/filesys/ext4/testdata/*.img.gz; do
  s=$(gzip -dc "$f" | wc -c)
  if [ -z "$ext4_size" ] || [ "$s" -lt "$ext4_size" ]; then
    ext4_size=$s
    ext4_src=$f
  fi
done
[ -n "$ext4_src" ] || die "no ext4 fixture found under $repo"
fat_src=$repo/internal/filesys/fat/testdata/fat12.img.gz
[ -f "$fat_src" ] || die "missing $fat_src"
ext4_rel=${ext4_src#"$repo"/}
fat_rel=${fat_src#"$repo"/}

gzip -dc "$ext4_src" >part1.img
gzip -dc "$fat_src" >part2.img
p1_bytes=$(stat -c %s part1.img)
p2_bytes=$(stat -c %s part2.img)
p1_sha=$(sha256sum part1.img | cut -d' ' -f1)
p2_sha=$(sha256sum part2.img | cut -d' ' -f1)

# 8 MiB disk, 512-byte sectors. The smallest committed ext4 image is 6.25 MiB,
# so the usual 1 MiB partition alignment cannot fit two partitions in 8 MiB:
# partitions are aligned to 4 KiB (8 sectors) and the first starts at sector 40,
# just after the GPT entries (the layout is about the container, not the
# filesystems; both partitions are written at their exact offsets).
disk_sectors=16384
align=8
p1_start=40
p1_sectors=$(((p1_bytes + 511) / 512))
p1_sectors=$(((p1_sectors + align - 1) / align * align))
p1_end=$((p1_start + p1_sectors - 1))
p2_start=$((p1_end + 1))
p2_sectors=$(((p2_bytes + 511) / 512))
p2_sectors=$(((p2_sectors + align - 1) / align * align))
p2_end=$((p2_start + p2_sectors - 1))
last_usable=$((disk_sectors - 34))
[ "$p2_end" -le "$last_usable" ] || die "the two source fixtures do not fit an 8 MiB disk (partition 2 ends at sector $p2_end, last usable $last_usable)"

img=disk.img
diskguid=6f1c5b2e-3a4d-4e8b-9c07-1d2e3f405162
guid1=11111111-2222-4333-8444-555555555511
guid2=11111111-2222-4333-8444-555555555512

run truncate -s "$((disk_sectors * 512))" "$img"
run sgdisk -a 1 -U "$diskguid" \
  -n "1:$p1_start:$p1_end" -t 1:8300 -c 1:ext4 -u "1:$guid1" \
  -n "2:$p2_start:$p2_end" -t 2:0700 -c 2:fat12 -u "2:$guid2" \
  "$img" >/dev/null
run dd if=part1.img of="$img" bs=512 seek="$p1_start" conv=notrunc status=none
run dd if=part2.img of="$img" bs=512 seek="$p2_start" conv=notrunc status=none
rm -f part1.img part2.img

raw_size=$(stat -c %s "$img")
[ "$raw_size" -le $((8 * 1024 * 1024)) ] || die "raw disk is $raw_size bytes (> 8 MiB)"
raw_sha256=$(sha256sum "$img" | cut -d' ' -f1)
raw_md5=$(md5sum "$img" | cut -d' ' -f1)
raw_sha1=$(sha1sum "$img" | cut -d' ' -f1)

# Partition table oracle (the partitioning tool, never Minutiae).
sfdisk --json "$img" | jq --arg f1 "$ext4_rel" --arg f2 "$fat_rel" \
  --arg h1 "$p1_sha" --arg h2 "$p2_sha" \
  --argjson b1 "$p1_bytes" --argjson b2 "$p2_bytes" '
  .partitiontable as $t
  | {
      scheme: "gpt",
      sector_size: $t.sectorsize,
      disk_guid: ($t.id | ascii_downcase),
      partitions: [ $t.partitions[] | (.node | capture("[.]img(?<n>[0-9]+)$").n | tonumber) as $i | {
        index: $i,
        start: (.start * $t.sectorsize),
        length: (.size * $t.sectorsize),
        type: (.type | ascii_downcase),
        name: .name,
        guid: (.uuid | ascii_downcase),
        source_fixture: (if $i == 1 then $f1 else $f2 end),
        source_size: (if $i == 1 then $b1 else $b2 end),
        source_sha256: (if $i == 1 then $h1 else $h2 end)
      } ]
    }' >partitions.json
CMDS+=("sfdisk --json disk.img | jq <normalise to expect shape, plus source_fixture/source_size/source_sha256>")
[ "$(jq '.partitions | length' partitions.json)" = 2 ] || die "expected two partitions"
[ "$(jq '.partitions[0].start' partitions.json)" = "$((p1_start * 512))" ] || die "partition 1 not at the planned offset"
[ "$(jq '.partitions[1].start' partitions.json)" = "$((p2_start * 512))" ] || die "partition 2 not at the planned offset"

# ---------------------------------------------------------------------------
# 2. Acquire, normalise, decode, cross-check.

case_no=MINUTIAE-EWF-1
desc="Minutiae EWF test disk"
examiner="Test Examiner"
evidence=EV-0001
notes="Fixture generated for tests"

# acquire <name> <compression> <sectors-per-chunk> [extra ewfacquire args...]
acquire() {
  local name=$1 comp=$2 spc=$3
  shift 3
  frun ewfacquire -u -q -c "$comp" -b "$spc" -C "$case_no" -D "$desc" \
    -e "$examiner" -E "$evidence" -N "$notes" -d sha1 "$@" -t "ewf-$name" "$img" >/dev/null
}

# info_field <label> reads one whitelisted "Label:<tabs>value" line of ewfinfo.
info_field() {
  sed -n "s/^[[:space:]]*$1:[[:space:]]*//p" <<<"$info" | head -n 1
}

must_hex() { # must_hex <what> <value> <length>
  [[ $2 =~ ^[0-9a-f]+$ ]] && [ "${#2}" -eq "$3" ] || die "$1: '$2' is not $3 hex digits"
}

variants=(single-none single-best multi-none seed-small)
declare -A comp=([single-none]=none [single-best]=best [multi-none]=none [seed-small]=best)
declare -A spc=([single-none]=64 [single-best]=128 [multi-none]=64 [seed-small]=64)
declare -A extra=([single-none]="" [single-best]="" [multi-none]="-S 2097152" [seed-small]="-B 1048576")

for v in "${variants[@]}"; do
  # shellcheck disable=SC2086 # extra is a deliberate word list
  acquire "$v" "${comp[$v]}" "${spc[$v]}" ${extra[$v]}
done

cmp_bytes=$raw_size
for v in "${variants[@]}"; do
  mapfile -t files < <(ls "ewf-$v".E[0-9][0-9] | sort)
  [ "${#files[@]}" -ge 1 ] || die "$v: ewfacquire wrote no segment files"
  want=$raw_size
  inspect_extra=()
  if [ "$v" = seed-small ]; then
    want=1048576
    inspect_extra=(--raw-bytes "$want")
  fi
  # The normaliser runs BEFORE anything reads the files.
  run python3 "$here/ewf_normalize.py" "${files[@]}"

  # Independent decode: also fails unless the rebuilt media equals the raw disk.
  CMDS+=("python3 tools/fixtures/ewf_inspect.py ewf-$v.E01.. --raw disk.img ${inspect_extra[*]}")
  python3 "$here/ewf_inspect.py" "${files[@]}" --raw "$img" "${inspect_extra[@]}" >"inspect-$v.json"

  want_md5=$(head -c "$want" "$img" | md5sum | cut -d' ' -f1)
  want_sha1=$(head -c "$want" "$img" | sha1sum | cut -d' ' -f1)
  want_sha256=$(head -c "$want" "$img" | sha256sum | cut -d' ' -f1)
  [ "$(jq -r .media.md5 "inspect-$v.json")" = "$want_md5" ] || die "$v: decoder md5 != raw md5"
  [ "$(jq -r .media.sha1 "inspect-$v.json")" = "$want_sha1" ] || die "$v: decoder sha1 != raw sha1"
  [ "$(jq -r .media.sha256 "inspect-$v.json")" = "$want_sha256" ] || die "$v: decoder sha256 != raw sha256"
  [ "$(jq -r .digest.md5 "inspect-$v.json")" = "$want_md5" ] || die "$v: stored md5 != raw md5"
  [ "$(jq -r .digest.sha1 "inspect-$v.json")" = "$want_sha1" ] || die "$v: stored sha1 != raw sha1"
  [ "$(jq -r .hash.md5 "inspect-$v.json")" = "$want_md5" ] || die "$v: hash section md5 != raw md5"
  [ "$(jq -r .media.size "inspect-$v.json")" = "$want" ] || die "$v: media size != $want"

  # ewfverify: success, and its stored/calculated md5 (and sha1) are the raw ones.
  CMDS+=("ewfverify -q ewf-$v.E01..")
  ver=$(ewfverify -q "${files[@]}") || die "$v: ewfverify failed"
  grep -q 'ewfverify: SUCCESS' <<<"$ver" || die "$v: ewfverify did not report success"
  ver_stored=$(sed -n 's/^MD5 hash stored in file:[[:space:]]*//p' <<<"$ver" | head -n 1)
  ver_calc=$(sed -n 's/^MD5 hash calculated over data:[[:space:]]*//p' <<<"$ver" | head -n 1)
  ver_sha1=$(sed -n 's/^SHA1:[[:space:]]*//p' <<<"$ver" | head -n 1)
  [ "$ver_stored" = "$want_md5" ] || die "$v: ewfverify stored md5 '$ver_stored' != raw md5"
  [ "$ver_calc" = "$want_md5" ] || die "$v: ewfverify calculated md5 '$ver_calc' != raw md5"
  [ "$ver_sha1" = "$want_sha1" ] || die "$v: ewfverify sha1 '$ver_sha1' != raw sha1"

  # ewfinfo: whitelisted geometry and hash fields equal the decoder's JSON.
  CMDS+=("ewfinfo ewf-$v.E01..")
  info=$(ewfinfo "${files[@]}")
  i_spc=$(info_field "Sectors per chunk")
  i_bps=$(info_field "Bytes per sector")
  i_sectors=$(info_field "Number of sectors")
  i_size=$(info_field "Media size" | sed -n 's/.*(\([0-9]*\) bytes).*/\1/p')
  i_md5=$(info_field "MD5")
  i_sha1=$(info_field "SHA1")
  i_level=$(info_field "Compression level")
  case $i_level in
    "no compression") i_level_n=0 ;;
    "fast compression") i_level_n=1 ;;
    "best compression") i_level_n=2 ;;
    *) die "$v: unexpected ewfinfo compression level" ;;
  esac
  must_hex "$v ewfinfo md5" "$i_md5" 32
  must_hex "$v ewfinfo sha1" "$i_sha1" 40
  j() { jq -r "$1" "inspect-$v.json"; }
  [ "$i_spc" = "$(j .volume.spc)" ] || die "$v: ewfinfo sectors per chunk $i_spc != decoder"
  [ "$i_bps" = "$(j .volume.bps)" ] || die "$v: ewfinfo bytes per sector $i_bps != decoder"
  [ "$i_sectors" = "$(j .volume.sectors)" ] || die "$v: ewfinfo sectors $i_sectors != decoder"
  [ "$i_size" = "$(j .media.size)" ] || die "$v: ewfinfo media size $i_size != decoder"
  [ "$i_md5" = "$want_md5" ] || die "$v: ewfinfo md5 != raw md5"
  [ "$i_sha1" = "$want_sha1" ] || die "$v: ewfinfo sha1 != raw sha1"
  [ "$i_level_n" = "$(j .volume.compression_level)" ] || die "$v: ewfinfo compression level != decoder"
  [ "$i_spc" = "${spc[$v]}" ] || die "$v: sectors per chunk $i_spc != requested ${spc[$v]}"

  # ewfexport to a raw file: byte-identical to the raw disk (range).
  CMDS+=("ewfexport -u -q -f raw -t export-$v ewf-$v.E01..")
  ewfexport -u -q -f raw -t "export-$v" "${files[@]}" >/dev/null 2>&1
  [ "$(sha256sum "export-$v.raw" | cut -d' ' -f1)" = "$want_sha256" ] || die "$v: ewfexport raw != raw disk"
  rm -f "export-$v.raw" "export-$v.raw.info"

  # Structural assertions.
  nseg=${#files[@]}
  [ "$nseg" = "$(j '.segments | length')" ] || die "$v: segment count mismatch"
  comp_chunks=$(j .chunks.compressed)
  unc_chunks=$(j .chunks.uncompressed)
  case $v in
    single-none)
      [ "$comp_chunks" = 0 ] && [ "$unc_chunks" -gt 0 ] || die "$v: expected only uncompressed chunks"
      [ "$nseg" = 1 ] || die "$v: expected 1 segment, got $nseg"
      ;;
    single-best)
      [ "$comp_chunks" -gt 0 ] && [ "$unc_chunks" -gt 0 ] || die "$v: expected compressed and uncompressed chunks"
      [ "$nseg" = 1 ] || die "$v: expected 1 segment, got $nseg"
      ;;
    multi-none)
      [ "$nseg" -ge 3 ] || die "$v: expected >= 3 segments, got $nseg"
      [ "$comp_chunks" = 0 ] || die "$v: expected only uncompressed chunks"
      ;;
    seed-small)
      [ "$nseg" = 1 ] || die "$v: expected 1 segment, got $nseg"
      ;;
  esac
done

# ---------------------------------------------------------------------------
# 3. Hash the (normalised) E01 files, then gzip.

mkdir gz
gzip -n -9 -c "$img" >gz/ewf-disk.img.gz
variants_json=()
for v in "${variants[@]}"; do
  mapfile -t files < <(ls "ewf-$v".E[0-9][0-9] | sort)
  fj=()
  for f in "${files[@]}"; do
    fj+=("$(jq -n --arg n "$f" --argjson s "$(stat -c %s "$f")" \
      --arg h "$(sha256sum "$f" | cut -d' ' -f1)" '{name: $n, size: $s, sha256: $h}')")
    gzip -n -9 -c "$f" >"gz/$f.gz"
  done
  files_json=$(printf '%s\n' "${fj[@]}" | jq -s .)
  variants_json+=("$(jq --arg name "$v" --argjson files "$files_json" '{
      name: $name,
      files: $files,
      segments: (.segments | length),
      media_size: .media.size,
      media_sha256: .media.sha256,
      bytes_per_sector: .volume.bps,
      sectors_per_chunk: .volume.spc,
      sectors: .volume.sectors,
      chunks: .chunks.total,
      compressed_chunks: .chunks.compressed,
      uncompressed_chunks: .chunks.uncompressed,
      chunk_kinds: .chunks.kinds,
      first_compressed_chunk: .chunks.first_compressed,
      first_uncompressed_chunk: .chunks.first_uncompressed,
      compression_level: .volume.compression_level,
      stored_md5: .digest.md5,
      stored_sha1: .digest.sha1,
      stored_hash_md5: .hash.md5,
      volume: .volume,
      header2: .header.header2[0].main,
      header: .header.header[0].main,
      tables: .tables,
      sections: [ .segments[] as $s | $s.sections[] | {segment: $s.number, type, offset, next, size} ],
      segment_files: [ .segments[] | {file, number, size} ],
      observations: .observations
    }' "inspect-$v.json")")
done

# The committed outputs must stay small: drop a variant rather than commit more.
total=0
for f in gz/*; do total=$((total + $(stat -c %s "$f"))); done
[ "$total" -le $((2 * 1024 * 1024)) ] || die "committed EWF outputs would be $total bytes (> 2 MiB); drop a variant"

gen=$(generator_json "$img" ewf-tools gdisk fdisk faketime util-linux jq gzip coreutils python3)
printf '%s\n' "${variants_json[@]}" | jq -s \
  --argjson raw "$(jq -n --argjson size "$raw_size" --arg sha256 "$raw_sha256" --arg md5 "$raw_md5" --arg sha1 "$raw_sha1" \
    '{size: $size, sha256: $sha256, md5: $md5, sha1: $sha1}')" \
  --slurpfile parts partitions.json \
  --argjson gen "$gen" \
  '{raw: $raw, partitions: $parts[0], variants: ., generator: $gen}' >gz/ewf-fixtures.expect.json

# Remove stale outputs of a previous run with a different segment count, then publish.
rm -f "$out"/ewf-disk.img.gz "$out"/ewf-*.E[0-9][0-9].gz "$out"/ewf-fixtures.expect.json
cp gz/* "$out"/
echo "wrote $(ls gz | wc -l) files ($total bytes) to $out"

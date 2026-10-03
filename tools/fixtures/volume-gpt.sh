#!/bin/bash
# GPT disk fixture: 8 MiB, 512-byte sectors, 3 partitions with gaps.
# Usage: volume-gpt.sh <outdir>   (normally called by gen.sh)
# Oracle: sfdisk --json, cross-checked against sgdisk -i. Never Minutiae.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

out=$(cd "${1:?outdir required}" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
img=disk.img

diskguid=6f1c5b2e-3a4d-4e8b-9c07-1d2e3f405162
guid1=11111111-2222-4333-8444-555555555501
guid2=11111111-2222-4333-8444-555555555502
guid3=11111111-2222-4333-8444-555555555503

run truncate -s 8M "$img"
run sgdisk -U "$diskguid" \
  -n 1:2048:6143 -t 1:8300 -c 1:system -u "1:$guid1" \
  -n 2:6144:10239 -t 2:0700 -c 2:data -u "2:$guid2" \
  -n 3:12288:14335 -t 3:8300 -c 3:userdata -u "3:$guid3" \
  "$img" >/dev/null

# Oracle from the partitioning tool.
sfdisk --json "$img" | jq '
  .partitiontable as $t
  | {
      scheme: "gpt",
      sector_size: $t.sectorsize,
      disk_guid: ($t.id | ascii_downcase),
      partitions: [ $t.partitions[] | {
        index: (.node | capture("[.]img(?<n>[0-9]+)$").n | tonumber),
        start: (.start * $t.sectorsize),
        length: (.size * $t.sectorsize),
        type: (.type | ascii_downcase),
        name: .name,
        guid: (.uuid | ascii_downcase)
      } ]
    }' >oracle.json
CMDS+=("sfdisk --json disk.img | jq <normalise to expect shape>")

# Independent cross-check with sgdisk -i: both tools must agree.
n=$(jq '.partitions | length' oracle.json)
for ((i = 1; i <= n; i++)); do
  info=$(sgdisk -i "$i" "$img")
  s_type=$(sed -n 's/^Partition GUID code: \([0-9A-Fa-f-]*\) .*/\1/p' <<<"$info" | tr 'A-F' 'a-f')
  s_guid=$(sed -n 's/^Partition unique GUID: //p' <<<"$info" | tr 'A-F' 'a-f')
  s_name=$(sed -n "s/^Partition name: '\(.*\)'\$/\1/p" <<<"$info")
  s_first=$(sed -n 's/^First sector: \([0-9]*\) .*/\1/p' <<<"$info")
  s_last=$(sed -n 's/^Last sector: \([0-9]*\) .*/\1/p' <<<"$info")
  want=$(jq -c --argjson i "$i" '.partitions[] | select(.index == $i)' oracle.json)
  got=$(jq -nc --argjson i "$i" --arg t "$s_type" --arg g "$s_guid" --arg n "$s_name" \
    --argjson f "$s_first" --argjson l "$s_last" \
    '{index: $i, start: ($f * 512), length: (($l - $f + 1) * 512), type: $t, name: $n, guid: $g}')
  if [ "$want" != "$got" ]; then
    echo "sfdisk/sgdisk disagree on partition $i:" >&2
    echo " sfdisk $want" >&2
    echo " sgdisk $got" >&2
    exit 1
  fi
done
CMDS+=("sgdisk -i <n> disk.img   # cross-checked against the sfdisk output")

gen=$(generator_json gdisk fdisk util-linux jq gzip coreutils)
jq --argjson g "$gen" '. + {generator: $g}' oracle.json >"$out/gpt-disk.expect.json"

gzip -n -9 -c "$img" >"$out/gpt-disk.img.gz"
echo "wrote $out/gpt-disk.img.gz and gpt-disk.expect.json"

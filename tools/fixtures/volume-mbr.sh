#!/bin/bash
# MBR disk fixture: 8 MiB, 512-byte sectors, 2 primaries, an extended
# partition and 2 logicals.
# Usage: volume-mbr.sh <outdir>   (normally called by gen.sh)
# Oracle: sfdisk --json. Never Minutiae.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

out=$(cd "${1:?outdir required}" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
img=disk.img

cat >layout.sfdisk <<'LAYOUT'
label: dos
label-id: 0x5ec0de01
unit: sectors

start=2048,  size=2048, type=0c
start=4096,  size=2048, type=83
start=8192,  size=8192, type=0f
start=10240, size=2048, type=83
start=14336, size=2048, type=07
LAYOUT

run truncate -s 8M "$img"
CMDS+=("sfdisk -q disk.img < layout.sfdisk   # layout.sfdisk lines joined by '|': $(paste -sd '|' layout.sfdisk)")
sfdisk -q "$img" <layout.sfdisk >/dev/null

# The extended container is not a partition holding data; the oracle lists
# primaries and logicals only (index 1, 2, 5, 6).
sfdisk --json "$img" | jq '
  .partitiontable as $t
  | {
      scheme: "mbr",
      sector_size: $t.sectorsize,
      partitions: [ $t.partitions[]
        | (.type | ascii_downcase | if length == 1 then "0" + . else . end) as $ty
        | select(["05", "0f", "85"] | index($ty) | not)
        | {
            index: (.node | capture("[.]img(?<n>[0-9]+)$").n | tonumber),
            start: (.start * $t.sectorsize),
            length: (.size * $t.sectorsize),
            type: ("0x" + $ty),
            name: "",
            guid: ""
          } ]
    }' >oracle.json
CMDS+=("sfdisk --json disk.img | jq <drop extended containers, normalise to expect shape>")

gen=$(generator_json fdisk util-linux jq gzip coreutils)
jq --argjson g "$gen" '. + {generator: $g}' oracle.json >"$out/mbr-disk.expect.json"

gzip -n -9 -c "$img" >"$out/mbr-disk.img.gz"
echo "wrote $out/mbr-disk.img.gz and mbr-disk.expect.json"

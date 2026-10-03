#!/bin/bash
# ext4 fixtures built by the real mke2fs from a deterministic source tree.
# Usage: ext4.sh <outdir>   (normally called by gen.sh)
#
#   ext4-4k-csum          ext4, 4 KiB blocks, metadata_csum + 64bit (+ the
#                         mke2fs.conf ext4 defaults: flex_bg, journal, ...),
#                         htree directory, depth-1 extent tree, deleted entries
#   ext4-1k-blockmap-ext2 ext2, 1 KiB blocks, 128-byte inodes: block maps with
#                         indirect blocks, htree directory, deleted entries
#   ext4-inline           ext4 with inline_data (small files and directories
#                         stored in the inode), deleted entries
#
# Oracle: ext4_oracle.py walks the SOURCE TREE (never the image); the
# "deleted" list is the generator's own list of `debugfs rm` operations;
# label, uuid, block size and size come from dumpe2fs. Never Minutiae.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

out=$(cd "${1:?outdir required}" && pwd)

# Reproducible images: every timestamp mke2fs/debugfs/e2fsck writes itself
# comes from the fake clock, source times are all older than SOURCE_DATE_EPOCH
# (mke2fs clamps newer ones to it), the source tree lives on tmpfs so
# directory enumeration order is creation order, uuid and hash seed are fixed.
export E2FSPROGS_FAKE_TIME=1700000000
export LC_ALL=C.UTF-8
fsuuid=0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d
hashseed=11112222-3333-4444-5555-666677778888

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-ext4 # fixed name: the commands recorded in the oracle mention it
rm -rf "$work"
mkdir "$work"
trap 'rm -rf "$work"' EXIT

# pattern <bytes> <seed>: deterministic pseudo-random bytes (sha256 counter mode).
pattern() {
  python3 -c '
import hashlib, sys
n, seed = int(sys.argv[1]), sys.argv[2]
out, i = bytearray(), 0
while len(out) < n:
    out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
    i += 1
sys.stdout.buffer.write(bytes(out[:n]))' "$1" "$2"
}

# island <file> <offset> <text>: write text (repeated to 6000 bytes) at offset.
island() {
  python3 -c '
import sys
f, off, text = sys.argv[1], int(sys.argv[2]), sys.argv[3].encode()
with open(f, "r+b") as fh:
    fh.seek(off)
    fh.write((text * (6000 // len(text) + 1))[:6000])' "$@"
}

# longname <n>: the n-th 31-byte name of the big directory.
longname() {
  printf 'entry-with-a-long-name-%04d.txt' "$1"
}

# stamp_tree <dir>: give every entry a distinct, increasing mtime/atime, all
# before 1700000000. Parents are stamped before children, so adding nothing
# afterwards keeps every directory mtime as set here.
stamp_tree() {
  local d=$1 i=0 p
  while IFS= read -r -d '' p; do
    touch -h -d "@$((1500000000 + i * 4321))" "$p"
    i=$((i + 1))
  done < <(find "$d" -mindepth 1 -print0 | sort -z)
}

# make_full_tree <dir>: the larger tree used by the 4k and block-map fixtures.
make_full_tree() {
  local d=$1 n i
  mkdir -p "$d/notes" "$d/dir" "$d/big" "$d/deep/a/b/c" "$d/utf8"
  printf 'Hello, ext4 fixture.\n' >"$d/readme.txt"
  printf 'alpha\n' >"$d/notes/a.txt"
  printf '# beta\n\nsetuid on purpose.\n' >"$d/notes/b.md"
  printf 'old draft that will be deleted\n' >"$d/notes/old-draft.txt"
  printf 'kept\n' >"$d/dir/keep.txt"
  printf 'deleted-me contents\n' >"$d/dir/deleted-me.txt"
  printf 'second deleted\n' >"$d/dir/second-deleted.log"
  printf 'leaf\n' >"$d/deep/a/b/c/leaf.txt"
  printf 'accents\n' >"$d/utf8/héllo-wörld.txt"
  printf 'cjk\n' >"$d/utf8/日本語.txt"
  printf 'emoji\n' >"$d/utf8/emoji-😀.txt"
  for n in $(seq 1 300); do
    printf 'file %04d\n' "$n" >"$d/big/$(longname "$n")"
  done
  # 3 MiB of 64-byte numbered lines: every 1 KiB block is distinct (a misplaced
  # block is detected) yet the image stays small when compressed.
  python3 -c '
import sys
for i in range(3 << 14):
    sys.stdout.write(("line %07d of blob-3m " % i).ljust(63, ".") + "\n")' >"$d/blob-3m.bin"
  : >"$d/empty.dat"
  # Sparse: 4 MiB with ten 6000-byte islands of data (a depth-1 extent tree
  # with ext4, double-indirect block maps with ext2) and a hole at the end.
  truncate -s 4M "$d/sparse-islands.bin"
  for i in $(seq 0 9); do island "$d/sparse-islands.bin" $((i * 409600)) "island-$i;"; done
  truncate -s 100000 "$d/sparse-tail.bin"
  island "$d/sparse-tail.bin" 90000 "tail;"
  ln -s readme.txt "$d/link-short"
  ln -s notes "$d/link-dir"
  ln -s /nonexistent/target "$d/link-dangling"
  ln -s "$(printf 'notes/../%.0s' $(seq 1 10))notes/a.txt" "$d/link-long"
  ln -s "$(printf 'deep/a/b/c/../../../../%.0s' $(seq 1 14))readme.txt" "$d/link-verylong"
  chmod 0600 "$d/notes/a.txt"
  chmod 4755 "$d/notes/b.md"
  chmod 0750 "$d/deep"
  chmod 1777 "$d/dir"
  chmod 0444 "$d/readme.txt"
  stamp_tree "$d"
}

# make_small_tree <dir>: small files and directories, for inline_data.
make_small_tree() {
  local d=$1 n
  mkdir -p "$d/notes" "$d/dir" "$d/many" "$d/utf8"
  printf 'hi\n' >"$d/readme.txt"
  printf 'alpha\n' >"$d/notes/a.txt"
  printf 'old draft\n' >"$d/notes/old-draft.txt"
  printf 'kept\n' >"$d/dir/keep.txt"
  printf 'deleted-me contents\n' >"$d/dir/deleted-me.txt"
  printf 'cjk\n' >"$d/utf8/日本語.txt"
  printf '%0120d' 7 >"$d/mid-120.txt"
  pattern 5000 mid5k >"$d/five-k.bin"
  : >"$d/empty.dat"
  for n in $(seq 1 20); do
    printf 'item %02d\n' "$n" >"$d/many/e$(printf '%02d' "$n").txt"
  done
  ln -s readme.txt "$d/link-short"
  ln -s "$(printf 'notes/../%.0s' $(seq 1 9))notes/a.txt" "$d/link-long"
  chmod 0640 "$d/notes/a.txt"
  stamp_tree "$d"
}

# build_fixture <name> <full|small> <label> "<deleted paths>" <size> <mke2fs args...> -- <e2fsck args | none>
build_fixture() {
  local name=$1 tree=$2 label=$3 deleted=$4 size=$5
  shift 5
  local mk=()
  while [ "$1" != -- ]; do mk+=("$1"); shift; done
  shift
  local fsck=("$@")

  local src="$work/$name.src" img="$work/$name.img" p
  mkdir "$src"
  "make_${tree}_tree" "$src"
  CMDS=()
  CMDS+=("# source tree: make_${tree}_tree in tools/fixtures/ext4.sh")

  # The oracle comes from the source tree before anything touches the image
  # (the later-deleted files excluded), so it cannot depend on the image.
  local exclude=()
  for p in $deleted; do exclude+=(--exclude "$p"); done
  CMDS+=("python3 tools/fixtures/ext4_oracle.py <src> ${exclude[*]}")
  python3 "$here/ext4_oracle.py" "$src" "${exclude[@]}" >"$work/$name.files.json"

  # -U and the hash seed are fixed; root_owner pins the root directory owner.
  run mke2fs -q -F -U "$fsuuid" -E "hash_seed=$hashseed,root_owner=0:0" -L "$label" -d "$src" "${mk[@]}" "$img" "$size"
  if [ "${fsck[0]}" != none ]; then
    # Rebuild every directory with the hash seed above: large directories
    # become htree directories (mke2fs -d writes linear ones).
    CMDS+=("e2fsck ${fsck[*]} <img>   # exit 0 or 1 (= optimised) accepted")
    local rc=0
    e2fsck "${fsck[@]}" "$img" >"$work/$name.fsck.log" 2>&1 || rc=$?
    [ "$rc" -le 1 ] || { cat "$work/$name.fsck.log"; echo "e2fsck failed ($rc)" >&2; exit 1; }
    # e2fsck stamps s_lastcheck with the real clock (E2FSPROGS_FAKE_TIME is
    # not honoured there); pin it so regenerating yields identical images.
    run debugfs -w -R "ssv lastcheck $E2FSPROGS_FAKE_TIME" "$img" >/dev/null
  fi
  for p in $deleted; do
    run debugfs -w -R "rm $p" "$img" >/dev/null
  done
  CMDS+=("e2fsck -fn <img>   # must report a clean filesystem")
  e2fsck -fn "$img" >"$work/$name.fsck2.log" 2>&1 || { cat "$work/$name.fsck2.log"; echo "final e2fsck not clean" >&2; exit 1; }

  # Facts the filesystem itself reports (independent of the reader).
  CMDS+=("dumpe2fs -h <img>   # label, uuid, block size, block count, features")
  local hdr bsize bcount uuid lab feats
  hdr=$(dumpe2fs -h "$img" 2>/dev/null)
  field() { printf '%s\n' "$hdr" | sed -n "s/^$1: *//p"; }
  bsize=$(field 'Block size')
  bcount=$(field 'Block count')
  uuid=$(field 'Filesystem UUID')
  lab=$(field 'Filesystem volume name')
  feats=$(field 'Filesystem features')
  if [ "$uuid" != "$fsuuid" ] || [ "$lab" != "$label" ]; then
    echo "dumpe2fs identity mismatch: uuid=$uuid label=$lab" >&2
    exit 1
  fi

  local gen del fstype=ext4 i
  for i in "${!mk[@]}"; do [ "${mk[$i]}" = -t ] && fstype=${mk[$((i + 1))]}; done
  gen=$(generator_json "$img" e2fsprogs python3 coreutils jq gzip)
  del=$(printf '%s\n' $deleted | jq -R . | jq -s .)
  jq -n --argjson g "$gen" --slurpfile f "$work/$name.files.json" --argjson d "$del" \
    --arg type "$fstype" --arg label "$label" --arg uuid "$uuid" --argjson bs "$bsize" --argjson bc "$bcount" --arg feats "$feats" '
    {generator: $g, type: $type, label: $label, uuid: $uuid, block_size: $bs,
     size: ($bs * $bc), features: ($feats | split(" ")),
     mke2fs_created: ["/lost+found"], files: $f[0].files, deleted: $d}' >"$out/$name.expect.json"
  gzip -n -9 -c "$img" >"$out/$name.img.gz"
  echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"
}

full_deleted="/dir/deleted-me.txt /notes/old-draft.txt /big/$(longname 150)"
small_deleted="/dir/deleted-me.txt /notes/old-draft.txt /many/e07.txt"

build_fixture ext4-4k-csum full fixture4k "$full_deleted" 16M \
  -t ext4 -b 4096 -O metadata_csum,64bit -- -fyD
build_fixture ext4-1k-blockmap-ext2 full fixture1k "$full_deleted" 12M \
  -t ext2 -b 1024 -I 128 -- -fyD
build_fixture ext4-inline small fixtureinl "$small_deleted" 8M \
  -t ext4 -b 1024 -O inline_data -- none

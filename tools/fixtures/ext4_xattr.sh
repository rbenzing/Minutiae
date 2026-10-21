#!/bin/bash
# ext4-xattr: an ext4 image with metadata_csum, 64bit and ea_inode whose files carry extended attributes in the
# inode body, in an xattr block and in an EA inode (LX-7a Tasks 7 and 8).
# Usage: ext4_xattr.sh <outdir>   (normally called by gen.sh ext4-xattr)
#
# It runs in the pinned minutiae-recover-fixtures image (e2fsprogs 1.47.0), never in minutiae-fixtures, whose
# base is not pinned: it writes only the two new files and cannot change a committed fixture. It does not touch
# ext4.sh.
#
#   /file1         user.small (4 bytes, in the inode body) and user.block (300 bytes, in the xattr block)
#   /file2         user.huge (1024 bytes, held in an EA inode: the value does not fit a 1 KiB xattr block)
#   /dir/file3     user.note (in the inode body)
#   /dir/gone.txt  deleted with debugfs (the "deleted" list)
#
# Oracle: ext4_oracle.py walks the SOURCE TREE (never the image) for "files"; "xattrs" comes from debugfs on the
# final image (ea_list for the names and sizes, ea_get -f then sha256sum for each value, never Minutiae) and the
# storage of each attribute is asserted against what debugfs stat reports (the File ACL block of /file1, the
# EA-inode flag of the inode that holds user.huge). libext2fs never shares an xattr block between inodes, so no
# shared block exists here: the shared case is tested with the test builder only (xattr_csum_test.go).
#
# debugfs ea_set stores the value of an EA inode without updating the parent's i_blocks, so e2fsck -fy repairs
# that (exit 0 or 1 accepted) and s_lastcheck is pinned again, as ext4.sh does.
set -euo pipefail

umask 022
if [ "$(id -u)" != 0 ]; then
  echo "ext4_xattr.sh must run as root (the container does): mke2fs -d records source ownership" >&2
  exit 1
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

out=$(cd "${1:?outdir required}" && pwd)

export E2FSPROGS_FAKE_TIME=1700000000
export LC_ALL=C.UTF-8
fsuuid=0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d
hashseed=11112222-3333-4444-5555-666677778888
name=ext4-xattr
label=fixturexattr

base=/dev/shm
[ -w "$base" ] || base=${TMPDIR:-/tmp}
work=$base/minutiae-ext4-xattr # fixed name: the commands recorded in the oracle mention it
rm -rf "$work"
mkdir "$work"
trap 'rm -rf "$work"' EXIT

src="$work/src"
img="$work/$name.img"
mkdir -p "$src/dir"
printf 'file one\n' >"$src/file1"
printf 'file two\n' >"$src/file2"
printf 'file three\n' >"$src/dir/file3"
printf 'removed later\n' >"$src/dir/gone.txt"
i=0
while IFS= read -r -d '' p; do
  touch -h -d "@$((1500000000 + i * 4321))" "$p"
  i=$((i + 1))
done < <(find "$src" -mindepth 1 -print0 | sort -z)

CMDS=()
CMDS+=("# source tree: file1, file2, dir/file3, dir/gone.txt in tools/fixtures/ext4_xattr.sh")
deleted="/dir/gone.txt"
CMDS+=("python3 tools/fixtures/ext4_oracle.py <src> --exclude $deleted")
python3 "$here/ext4_oracle.py" "$src" --exclude "$deleted" >"$work/files.json"

pattern 300 xattr-block >"$work/v_block"
pattern 1024 xattr-ea-inode >"$work/v_huge"

run mke2fs -q -F -t ext4 -b 1024 -O metadata_csum,64bit,ea_inode -U "$fsuuid" \
  -E "hash_seed=$hashseed,root_owner=0:0" -L "$label" -d "$src" "$img" 4M
run debugfs -w -R "ea_set /file1 user.small tiny" "$img" >/dev/null
run debugfs -w -R "ea_set -f $work/v_block /file1 user.block" "$img" >/dev/null
run debugfs -w -R "ea_set -f $work/v_huge /file2 user.huge" "$img" >/dev/null
run debugfs -w -R "ea_set /dir/file3 user.note remark" "$img" >/dev/null

CMDS+=("e2fsck -fy <img>   # exit 0 or 1 accepted: repairs the parent i_blocks of the EA-inode value")
rc=0
e2fsck -fy "$img" >"$work/fsck.log" 2>&1 || rc=$?
[ "$rc" -le 1 ] || {
  cat "$work/fsck.log"
  echo "e2fsck failed ($rc)" >&2
  exit 1
}
run debugfs -w -R "ssv lastcheck $E2FSPROGS_FAKE_TIME" "$img" >/dev/null
# mke2fs -d copies the source ctime and atime, which are the real clock (the generator just created and read the
# files), so pin them; an EA inode is left alone (its atime and ctime hold the value hash and reference count).
for p in / /lost+found /dir /file1 /file2 /dir/file3 "$deleted"; do
  run debugfs -w -R "sif $p ctime $E2FSPROGS_FAKE_TIME" "$img" >/dev/null
  run debugfs -w -R "sif $p atime $E2FSPROGS_FAKE_TIME" "$img" >/dev/null
done
run debugfs -w -R "rm $deleted" "$img" >/dev/null
CMDS+=("e2fsck -fn <img>   # must report a clean filesystem")
e2fsck -fn "$img" >"$work/fsck2.log" 2>&1 || {
  cat "$work/fsck2.log"
  echo "final e2fsck not clean" >&2
  exit 1
}

# Facts the filesystem itself reports.
CMDS+=("dumpe2fs <img> | python3 tools/fixtures/ext4_freeblocks.py   # free block ranges")
free=$(dumpe2fs "$img" 2>/dev/null | python3 "$here/ext4_freeblocks.py")
CMDS+=("dumpe2fs -h <img>   # label, uuid, block size, block count, features")
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
case " $feats " in *" metadata_csum "*) ;; *)
  echo "metadata_csum missing from the features: $feats" >&2
  exit 1
  ;;
esac

# xattrs: names and sizes from ea_list, values from ea_get. attr <path> <name> <storage> prints one JSON object.
CMDS+=("debugfs -R 'ea_list <path>' <img>   # attribute names and sizes")
CMDS+=("debugfs -R 'ea_get -f <out> <path> <name>' <img>; sha256sum <out>   # value hashes")
attr() {
  local path=$1 aname=$2 storage=$3 size sum
  size=$(debugfs -R "ea_list $path" "$img" 2>/dev/null | sed -n "s/^ *$aname (\([0-9]*\)).*/\1/p")
  [ -n "$size" ] || {
    echo "ea_list shows no $aname on $path" >&2
    exit 1
  }
  rm -f "$work/val"
  debugfs -R "ea_get -f $work/val $path $aname" "$img" >/dev/null 2>&1
  [ "$(stat -c %s "$work/val")" = "$size" ] || {
    echo "ea_get of $aname wrote a different size than ea_list says" >&2
    exit 1
  }
  sum=$(sha256sum "$work/val")
  jq -n --arg p "$path" --arg n "$aname" --arg s "$storage" --argjson z "$size" --arg h "${sum%% *}" \
    '{path: $p, name: $n, storage: $s, size: $z, sha256: $h}'
}

# The storage kinds are asserted, not assumed: /file1 has an xattr block (File ACL set), /file2 and /dir/file3
# have none, and the inode that holds user.huge is the only inode with the EA-inode flag.
acl() { debugfs -R "stat $1" "$img" 2>/dev/null | sed -n 's/^File ACL: *\([0-9]*\).*/\1/p'; }
[ "$(acl /file1)" != 0 ] || {
  echo "/file1 has no xattr block" >&2
  exit 1
}
{ [ "$(acl /file2)" = 0 ] && [ "$(acl /dir/file3)" = 0 ]; } || {
  echo "/file2 or /dir/file3 unexpectedly has an xattr block" >&2
  exit 1
}
inodes_count=$(field 'Inode count')
eainode=0
for n in $(seq 1 "$inodes_count"); do
  flags=$(debugfs -R "stat <$n>" "$img" 2>/dev/null | sed -n 's/^Inode: [0-9]* .*Flags: \(0x[0-9a-f]*\).*/\1/p')
  [ -n "$flags" ] || continue
  if [ $((flags & 0x200000)) -ne 0 ]; then
    [ "$eainode" = 0 ] || {
      echo "more than one EA inode" >&2
      exit 1
    }
    eainode=$n
  fi
done
[ "$eainode" != 0 ] || {
  echo "no EA inode found" >&2
  exit 1
}
CMDS+=("debugfs -R 'stat <n>' <img>   # File ACL of each file, the EA-inode flag (0x200000) of each inode")

xattrs=$(
  {
    attr /file1 user.small inode
    attr /file1 user.block block
    attr /file2 user.huge ea_inode
    attr /dir/file3 user.note inode
  } | jq -s .
)

gen=$(generator_json "$img" e2fsprogs python3 coreutils jq gzip)
del=$(printf '%s\n' $deleted | jq -R . | jq -s .)
jq -n --argjson g "$gen" --slurpfile f "$work/files.json" --argjson d "$del" --argjson x "$xattrs" \
  --arg lbl "$label" --arg uuid "$uuid" --argjson bs "$bsize" --argjson bc "$bcount" --arg feats "$feats" \
  --argjson free "$free" --argjson ea "$eainode" '
  {generator: $g, type: "ext4", label: $lbl, uuid: $uuid, block_size: $bs,
   size: ($bs * $bc), features: ($feats | split(" ")),
   mke2fs_created: ["/lost+found"], files: $f[0].files, deleted: $d, free_blocks: $free,
   ea_inode: $ea, xattrs: $x}' >"$out/$name.expect.json"
gzip -n -9 -c "$img" >"$out/$name.img.gz"
echo "wrote $out/$name.img.gz and $name.expect.json ($(stat -c %s "$img") bytes raw)"

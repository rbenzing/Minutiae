#!/bin/bash
# Examiner helper (macOS; NOT run in CI, written from memory: check the stat(1)
# and shasum(1) options on the machine you use). Prints the "tree" array of the
# expected-output JSON of internal/filesys/apfs/realimages_test.go for a MOUNTED
# APFS volume, or for a mounted snapshot (mount_apfs -s <snapshot> <device> <dir>):
#
#   tools/fixtures/apfs_expect.sh /Volumes/Test > tree.json
#
# Paths are relative to the volume root. Each entry has path, type
# (file|dir|symlink), size and sha256 (files), mode (decimal, the permission bits
# only), mtime (whole seconds), link (symlinks) and hardlink_group ("ino<inode>"
# for a file with more than one link). Names holding a double quote, a backslash
# or a control character are not supported (the script stops on them): rename
# such files in the test volume.
set -euo pipefail

root=${1:?usage: apfs_expect.sh <mounted volume path>}
[ -d "$root" ] || { echo "$root is not a directory" >&2; exit 2; }
root=${root%/}

first=1
echo "["
# -print0 / read -d '' so any name is read intact; sorted for a stable output.
find "$root" -mindepth 1 -print0 | sort -z | while IFS= read -r -d '' p; do
  rel=${p#"$root"}
  case $rel in
    /.fseventsd | /.fseventsd/* | /.Spotlight-V100 | /.Spotlight-V100/* | /.Trashes | /.Trashes/* | /.DocumentRevisions-V100 | /.DocumentRevisions-V100/*)
      continue ;; # system folders the OS creates when a volume is mounted
  esac
  if printf '%s' "$rel" | LC_ALL=C grep -q '["\[:cntrl:]]'; then
    echo "unsupported character in the name of $rel" >&2
    exit 3
  fi
  mode=$((8#$(stat -f '%Lp' "$p")))
  mtime=$(stat -f '%m' "$p")
  line=$(printf '{"path": "%s"' "$rel")
  if [ -L "$p" ]; then
    target=$(readlink "$p")
    case $target in *\"* | *\*) echo "unsupported character in the target of $rel" >&2; exit 3 ;; esac
    line="$line, \"type\": \"symlink\", \"link\": \"$target\""
  elif [ -d "$p" ]; then
    line="$line, \"type\": \"dir\""
  else
    size=$(stat -f '%z' "$p")
    sum=$(shasum -a 256 "$p" | cut -d' ' -f1)
    line="$line, \"type\": \"file\", \"size\": $size, \"sha256\": \"$sum\""
    if [ "$(stat -f '%l' "$p")" -gt 1 ]; then
      line="$line, \"hardlink_group\": \"ino$(stat -f '%i' "$p")\""
    fi
  fi
  line="$line, \"mode\": $mode, \"mtime\": $mtime}"
  if [ $first = 1 ]; then first=0; else echo ","; fi
  printf '  %s' "$line"
done
echo
echo "]"

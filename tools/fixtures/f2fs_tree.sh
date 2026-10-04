# The deterministic source tree the F2FS fixtures are populated from (sload.f2fs).
# Source it after lib.sh (pattern); do not run it.
#
# make_tree <dir>   builds the tree. Every entry gets a distinct, increasing
#                   mtime before 1700000000 (stamp_tree).
#
# Notes on what sload.f2fs does with it (checked against fsck.f2fs/dump.f2fs):
# small files and symlinks become inline data, larger files get their own data
# blocks, and holes of a sparse source file are written out as zero blocks
# (so no F2FS fixture file has a hole; hole coverage comes from the builder
# tests).

# longname <n>: the n-th 31-byte name of the big directory.
longname() {
  printf 'entry-with-a-long-name-%04d.txt' "$1"
}

# name255: a file name of exactly 255 bytes (F2FS_NAME_LEN).
name255() {
  printf 'long-name-%s-end' "$(printf 'x%.0s' $(seq 1 241))"
}

# numbered <bytes> <tag>: <bytes> of text in which every 64-byte line (and so
# every 4 KiB block) is distinct, yet which compresses to almost nothing.
numbered() {
  python3 -c '
import sys
n, tag = int(sys.argv[1]), sys.argv[2]
out, i = bytearray(), 0
while len(out) < n:
    out += (("line %07d of %s " % (i, tag)).ljust(63, ".") + "\n").encode()
    i += 1
sys.stdout.buffer.write(bytes(out[:n]))' "$1" "$2"
}

stamp_tree() {
  local d=$1 i=0 p
  while IFS= read -r -d '' p; do
    touch -h -d "@$((1500000000 + i * 4321))" "$p"
    i=$((i + 1))
  done < <(find "$d" -mindepth 1 -print0 | sort -z)
}

make_tree() {
  local d=$1 n i
  mkdir -p "$d/notes" "$d/dir" "$d/big" "$d/deep/level-a/level-b/level-c" "$d/utf8" "$d/frag"
  printf 'Hello, F2FS fixture.\n' >"$d/readme.txt"
  : >"$d/empty.dat"
  printf 'alpha\n' >"$d/notes/a.txt"
  printf '# beta\n\nsetuid on purpose.\n' >"$d/notes/b.md"
  printf 'kept\n' >"$d/dir/keep.txt"
  printf 'removable one\n' >"$d/dir/removable-1.txt"
  printf 'removable two\n' >"$d/dir/removable-2.txt"
  printf 'removable three\n' >"$d/dir/removable-3.txt"
  printf 'leaf\n' >"$d/deep/level-a/level-b/level-c/leaf.txt"
  printf 'accents\n' >"$d/utf8/héllo-wörld.txt"
  printf 'cjk\n' >"$d/utf8/日本語.txt"
  printf 'emoji\n' >"$d/utf8/emoji-😀.txt"
  printf 'name of exactly 255 bytes\n' >"$d/$(name255)"
  # 600 entries with 31-byte names: a directory of several dentry blocks.
  for n in $(seq 1 600); do
    printf 'file %04d\n' "$n" >"$d/big/$(longname "$n")"
  done
  numbered 3145728 blob-3m >"$d/blob-3m.bin"
  # Files of a few hundred KiB, written one after the other in the warm-data
  # log; whichever of these (or the sparse file below) straddles a segment the
  # allocator has to skip is fragmented. The generator requires one.
  numbered 300000 frag-a >"$d/frag/a-first-file.bin"
  numbered 180000 frag-b >"$d/frag/b-second-file.bin"
  numbered 900000 frag-c >"$d/frag/c-large-file.bin"
  # Not inline (5000 bytes), and a 25-block file (the sparse file below, with
  # 1026 blocks, is the one whose addresses spill into a direct node).
  pattern 5000 mid5k >"$d/five-k.bin"
  numbered 100000 mid-100k >"$d/mid-100k.bin"
  # "Sparse": 4 MiB with ten islands of data and a hole at the end (sload
  # writes the holes as zero blocks).
  truncate -s 4M "$d/sparse-islands.bin"
  for i in $(seq 0 9); do
    python3 -c '
import sys
f, off, text = sys.argv[1], int(sys.argv[2]), sys.argv[3].encode()
with open(f, "r+b") as fh:
    fh.seek(off)
    fh.write((text * (6000 // len(text) + 1))[:6000])' "$d/sparse-islands.bin" $((i * 409600)) "island-$i;"
  done
  ln -s readme.txt "$d/link-short"
  ln -s notes "$d/link-dir"
  ln -s /nonexistent/target "$d/link-dangling"
  ln -s "$(printf 'notes/../%.0s' $(seq 1 10))notes/a.txt" "$d/link-long"
  ln -s "$(printf 'deep/level-a/level-b/level-c/../../../../%.0s' $(seq 1 14))readme.txt" "$d/link-verylong"
  chmod 0600 "$d/notes/a.txt"
  chmod 4755 "$d/notes/b.md"
  chmod 0750 "$d/deep"
  chmod 1777 "$d/dir"
  chmod 0444 "$d/readme.txt"
  stamp_tree "$d"
}

# Files the generator removes through a mounted filesystem when the kernel can
# mount F2FS (see f2fs.sh); the oracle excludes them only then.
f2fs_removable="/dir/removable-1.txt /dir/removable-2.txt /dir/removable-3.txt"

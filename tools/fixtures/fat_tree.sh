# Shared by fat.sh and exfat.sh: the deterministic source tree both fixtures
# are populated from. Source it; do not run it.
#
# make_tree <dir>   builds the tree. The paths in fat_deleted, fat_overwritten
#                   and fat_deleted_dirs, which the scripts remove again, are
#                   part of it; the oracle excludes them.
# stamp_tree <dir>  gives every entry a distinct, increasing, even mtime before
#                   1700000000 (FAT stores times in 2 second steps).

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

# longname <n>: the n-th 31-byte name of the big directory.
longname() {
  printf 'entry-with-a-long-name-%04d.txt' "$1"
}

# Files removed after population (full path, long names only: a deleted short
# entry loses its first character, a deleted long one keeps its name in the
# VFAT entries) and directories removed with their contents.
fat_deleted="/dir/deleted-me.txt /notes/old-draft.txt /big/$(longname 90)"
# Deleted before /frag/c-large-fragmented.bin is written, which then takes its
# directory slot: no deleted entry remains to be found.
fat_overwritten="/frag/a-first-file.bin"
fat_deleted_dirs="/gone-directory"

stamp_tree() {
  local d=$1 i=0 p
  while IFS= read -r -d '' p; do
    touch -h -d "@$((1500000000 + i * 4322))" "$p"
    i=$((i + 1))
  done < <(find "$d" -mindepth 1 -print0 | sort -z)
}

make_tree() {
  local d=$1 n
  mkdir -p "$d/notes" "$d/dir" "$d/big" "$d/deep/level-a/level-b/level-c" "$d/utf8" "$d/frag" "$d/gone-directory/sub-directory"
  printf 'Hello, FAT fixture.\n' >"$d/readme.txt"
  printf 'mixed case long name\n' >"$d/Mixed-Case.Txt"
  printf 'lower case short name\n' >"$d/lower.txt"
  printf 'UPPER CASE SHORT NAME\n' >"$d/UPPER.TXT"
  printf 'tiny\n' >"$d/A.TXT"
  : >"$d/empty.dat"
  printf 'alpha\n' >"$d/notes/a.txt"
  printf '# beta\n\nlong name with spaces.\n' >"$d/notes/b markdown notes.md"
  printf 'old draft that will be deleted\n' >"$d/notes/old-draft.txt"
  printf 'kept\n' >"$d/dir/keep.txt"
  printf 'deleted-me contents\n' >"$d/dir/deleted-me.txt"
  printf 'leaf\n' >"$d/deep/level-a/level-b/level-c/leaf.txt"
  printf 'accents\n' >"$d/utf8/héllo-wörld.txt"
  printf 'cjk\n' >"$d/utf8/日本語.txt"
  # A surrogate pair: mtools cannot store one (it drops the character), so only
  # exfat.sh asks for it.
  if [ "${tree_emoji:-}" = 1 ]; then printf 'emoji\n' >"$d/utf8/emoji-😀.txt"; fi
  printf 'inside a deleted directory\n' >"$d/gone-directory/inner-file.txt"
  printf 'nested in a deleted directory\n' >"$d/gone-directory/sub-directory/nested-file.txt"
  # 120 long names: a directory of several clusters on every fixture.
  for n in $(seq 1 120); do
    printf 'file %04d\n' "$n" >"$d/big/$(longname "$n")"
  done
  pattern 200000 blob >"$d/blob.bin"
  # a, b, then a is deleted and c written: c reuses a's clusters and continues
  # after b, so it is fragmented (the scripts do the sequence).
  pattern 40000 frag-a >"$d/frag/a-first-file.bin"
  pattern 30000 frag-b >"$d/frag/b-second-file.bin"
  pattern 90000 frag-c >"$d/frag/c-large-fragmented.bin"
  stamp_tree "$d"
}

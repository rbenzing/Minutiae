#!/bin/bash
# Helpers shared by the ext4 fixture scripts (ext4_recover.sh of plan 3C, ext4_journal.sh of plan 3G). Source it after lib.sh and
# recover_guest.sh; never run it. The functions move here unchanged from ext4_recover.sh, which sources this file and behaves as
# before (its images are not regenerated):
#   noreserve          the guest op that takes the kernel's 2% reserve away (/sys/fs/ext4/vda/reserved_clusters = 0), so a full volume
#                      really has no free block
#   mkpad <dir> <n>    pad.tar of the checkerboard: n pairs of directories pad/a/dNN (kept) and pad/b/dNN (removed), 20 one-block files each
#   bigfill_blocks <image>   (needs $pairs) the blocks of the big zero file that leave the checkerboard its room
#   pad_ops <blocks> [extra op]   the guest ops that lay the checkerboard
#   freeblocks <image>           the free block count of an image (dumpe2fs)
#   fsck_clean <image> <what>    e2fsck -fn must exit 0 (needs $work)
#   full <image>                 the volume must have no free block
#   deep_ops <file> <source> <first lblk> <count> [sync]   the guest ops that write blocks one by one
noreserve="echo 0 > /sys/fs/ext4/vda/reserved_clusters" # the guest op that takes the kernel's 2% reserve away

# mkpad <payload dir> <pairs>: pad.tar of the checkerboard: directories pad/a/dNN (kept) and pad/b/dNN (removed) with 20
# one-block zero files each, created alternately
mkpad() {
  python3 -I - "$1/pad.tar" "$2" <<'EOT'
import io, sys, tarfile
t = tarfile.open(sys.argv[1], "w", format=tarfile.USTAR_FORMAT)
def add(name, data=None):
    ti = tarfile.TarInfo(name)
    ti.mtime, ti.uid, ti.gid, ti.uname, ti.gname = 1700000000, 0, 0, "", ""
    if data is None:
        ti.type, ti.mode = tarfile.DIRTYPE, 0o755
        t.addfile(ti)
    else:
        ti.mode, ti.size = 0o644, len(data)
        t.addfile(ti, io.BytesIO(data))
for d in ("pad", "pad/a", "pad/b"):
    add(d)
n = int(sys.argv[2])
for k in range(n):
    g = "g%02d" % (k // 50)  # at most 50 subdirectories per group directory: none of them needs a hash index
    if k % 50 == 0:
        add("pad/a/" + g)
        add("pad/b/" + g)
    add("pad/a/%s/d%03d" % (g, k))
    add("pad/b/%s/d%03d" % (g, k))
    for j in range(20):
        add("pad/a/%s/d%03d/f%02d" % (g, k, j), bytes(1024))
        add("pad/b/%s/d%03d/f%02d" % (g, k, j), bytes(1024))
t.close()
EOT
}
# bigfill_blocks <image>: the blocks of one zero file that leave the checkerboard its room: the free blocks of a fresh image
# minus the pairs (42 blocks each) and a margin of 1400 (the files made before, the group directories, slack)
bigfill_blocks() { echo $(($(freeblocks "$1") - 1400 - pairs * 42)); }
# pad_ops <bigfill blocks> [extra op]: the big zero file, the archive, every remaining free block taken by a second zero
# file, then the b files removed (and the extra op, run at the same point)
pad_ops() {
  echo "dd if=/dev/zero of=/mnt/bigfill bs=1024 count=$1 2>/dev/null || true"
  echo "sync"
  echo "tar -x -C /mnt -f /payload/pad.tar || true"
  echo "sync"
  echo "dd if=/dev/zero of=/mnt/tailfill bs=1024 2>/dev/null || true"
  echo "sync"
  echo "rm -r /mnt/pad/b"
  [ -n "${2:-}" ] && echo "$2"
  echo "sync"
}

freeblocks() { dumpe2fs -h "$1" 2>/dev/null | sed -n 's/^Free blocks: *//p'; }
fsck_clean() { # <image> <what>
  local rc=0
  e2fsck -fn "$1" >"$work/fsck-last.txt" 2>&1 || rc=$?
  if [ "$rc" != 0 ]; then
    cat "$work/fsck-last.txt"
    echo "e2fsck -fn is not clean after $2" >&2
    exit 1
  fi
  echo "FACT e2fsck after=$2 exit=0"
}
full() { # <image>: the volume must have no free block
  if [ "$(freeblocks "$1")" != 0 ]; then
    echo "$1 has $(freeblocks "$1") free blocks, not a full volume: the reuse steps would not be confined to the removed file's blocks" >&2
    exit 1
  fi
}
# deep_ops <file> <source> <first lblk> <count> [sync]: dd the blocks first..first+count-1 in order (a sync after each with a fifth argument)
deep_ops() {
  local f=$1 s=$2 first=$3 count=$4 each=${5:+sync;}
  echo "i=$first; while [ \$i -lt $((first + count)) ]; do dd if=/payload/$s of=$f bs=1024 skip=\$i seek=\$i count=1 conv=notrunc 2>/dev/null; ${each} i=\$((i+1)); done"
  echo sync
}

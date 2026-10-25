# Shared helpers for the fixture scripts. Source it; do not run it.
#
# run <cmd...>            executes the command and records it, so the
#                         "generator" section of an oracle lists the commands
#                         that actually ran.
# pkg_versions <pkg...>   prints "package version" lines from dpkg.
# generator_json <image> <pkg...>
#                         prints the "generator" JSON object: the sha256 of
#                         the uncompressed <image> (binds the oracle to the
#                         exact bytes it describes), the tool versions and the
#                         recorded commands. Call it after the image is final.
# stamp <bytes> <seed>    per-block records (see the function); additive.
# pattern <bytes> <seed>  prints deterministic pseudo-random bytes (sha256
#                         counter mode); shared by the source trees.

CMDS=()

run() {
  CMDS+=("$*")
  "$@"
}

pkg_versions() {
  dpkg-query -W -f '${Package} ${Version}\n' "$@"
}

generator_json() {
  local image=${1:?image file required} sum pkgs
  shift
  sum=$(sha256sum "$image")
  sum=${sum%% *}
  pkgs=$(pkg_versions "$@" | jq -R 'split(" ") | {package: .[0], version: .[1]}' | jq -s .)
  jq -n --arg sha "$sum" --argjson packages "$pkgs" --args \
    '{image_sha256: $sha, packages: $packages, commands: $ARGS.positional}' "${CMDS[@]}"
}

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

# stamp <bytes> <seed>    prints <bytes> bytes in which every 4 KiB block is the
#                         32-byte record "<seed>:<logical block>", NUL-padded and
#                         repeated: unique per block (a block found anywhere can be
#                         traced to its file offset) and compressible (a 12 MiB file
#                         costs the guest's initramfs a few hundred KiB).
stamp() {
  python3 -c '
import sys
n, seed = int(sys.argv[1]), sys.argv[2]
out = bytearray()
for blk in range((n + 4095) // 4096):
    rec = ("%s:%d" % (seed, blk)).encode()
    if len(rec) > 32:
        sys.exit("stamp: record longer than 32 bytes")
    out += rec.ljust(32, b"\0") * 128
sys.stdout.buffer.write(bytes(out[:n]))' "$1" "$2"
}

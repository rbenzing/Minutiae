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

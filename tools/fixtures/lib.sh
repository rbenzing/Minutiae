# Shared helpers for the fixture scripts. Source it; do not run it.
#
# run <cmd...>            executes the command and records it, so the
#                         "generator" section of an oracle lists the commands
#                         that actually ran.
# pkg_versions <pkg...>   prints "package version" lines from dpkg.
# generator_json <pkg...> prints the "generator" JSON object (tool versions
#                         and the recorded commands).

CMDS=()

run() {
  CMDS+=("$*")
  "$@"
}

pkg_versions() {
  dpkg-query -W -f '${Package} ${Version}\n' "$@"
}

generator_json() {
  local pkgs
  pkgs=$(pkg_versions "$@" | jq -R 'split(" ") | {package: .[0], version: .[1]}' | jq -s .)
  jq -n --argjson packages "$pkgs" --args \
    '{packages: $packages, commands: $ARGS.positional}' "${CMDS[@]}"
}

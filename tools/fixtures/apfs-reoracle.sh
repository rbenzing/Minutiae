#!/bin/bash
# Re-emit the expectation file of the committed populated APFS fixture with the
# current oracle (apfs_populated_oracle.py), for the SAME image: no QEMU, no
# regeneration. Usage: apfs-reoracle.sh <outdir>   (the directory that holds
# apfs-populated.img.gz and apfs-populated.expect.json; the new
# apfs-populated.expect.json is written there).
#
# Gates, all of which fail the script: the committed image's sha256 equals the
# old oracle's generator.image_sha256; the oracle's own cross-checks (image
# against the deterministic source tree of apfs_tree.py) pass; and, once the
# keys this revision adds are removed, the new file equals the old one in every
# remaining field.
set -euo pipefail
umask 022
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
for tool in python3 jq gzip sha256sum; do
  command -v "$tool" >/dev/null || { echo "apfs-reoracle.sh needs $tool: use the minutiae-fixtures image" >&2; exit 1; }
done
out=$(cd "${1:?outdir required}" && pwd)
old=$out/apfs-populated.expect.json
gz=$out/apfs-populated.img.gz
[ -f "$old" ] && [ -f "$gz" ] || { echo "missing $old or $gz" >&2; exit 1; }

work=${TMPDIR:-/tmp}/minutiae-apfs-reoracle
rm -rf "$work"
mkdir "$work"
trap 'rm -rf "$work"' EXIT

gzip -dc "$gz" >"$work/img"
want=$(jq -r '.generator.image_sha256' "$old")
got=$(sha256sum "$work/img" | cut -d' ' -f1)
[ "$want" = "$got" ] || { echo "image sha256 $got differs from the oracle's $want" >&2; exit 1; }

label=$(jq -r '.volume.name' "$old")
cu=$(jq -r '.container.uuid' "$old")
vu=$(jq -r '.volume.uuid' "$old")
python3 "$here/apfs_tree.py" "$work/tree"
python3 "$here/apfs_populated_oracle.py" "$work/img" "$work/tree" "$label" "$cu" "$vu" >"$work/oracle.json"
jq '.generator' "$old" >"$work/generator.json"

python3 - "$work/oracle.json" "$work/generator.json" "$old" >"$work/new.json" <<'EOP'
import json, sys
o = json.load(open(sys.argv[1]))
o["generator"] = json.load(open(sys.argv[2]))
o["generator"]["oracle_reemit"] = "expect.json re-emitted from the committed image by tools/fixtures/apfs-reoracle.sh to add xattr_values; the image is unchanged"
json.dump(o, sys.stdout, separators=(",", ":"), ensure_ascii=False)
print()
EOP

python3 - "$old" "$work/new.json" <<'EOP'
import json, sys
old = json.load(open(sys.argv[1]))
new = json.load(open(sys.argv[2]))
def strip(doc):
    # only the keys this revision adds, only where it adds them: the node
    # lists of the live tree and of each snapshot, and generator.oracle_reemit
    doc = json.loads(json.dumps(doc))
    trees = [doc["live"]["tree"]] + [s["tree"] for s in doc["snapshots"]]
    for tree in trees:
        for node in tree:
            node.pop("xattr_values", None)
    doc["generator"].pop("oracle_reemit", None)
    return doc
if strip(old) != strip(new):
    sys.exit("the re-emitted oracle differs from the committed one in a field other than xattr_values")
EOP
# same directory, then rename: a failed copy never leaves a half-written oracle
tmp=$(mktemp "$out/.apfs-populated.expect.json.XXXXXX")
trap 'rm -rf "$work" "$tmp"' EXIT
cp "$work/new.json" "$tmp" && chmod 644 "$tmp" && mv "$tmp" "$old"
echo "re-emitted $old ($(stat -c %s "$old") bytes)"

#!/bin/bash
# SQLite fixtures of the container family (class C, see README.md "SQLite
# fixtures"). Usage: sqlite.sh <outdir>   (called by gen.sh with
# internal/sqlitefile/testdata).
#
# Writes <name>.db.gz, <name>.db-wal.gz, <name>.db-journal.gz and
# <name>.expect.json for the scenarios of sqlite_gen.py, then generates them a
# second time into a scratch directory and compares the sha256 of every
# uncompressed file: a database-only fixture must be byte-identical (class A
# behaviour), a WAL or journal file is expected to differ (class B behaviour:
# salts and nonces come from the engine's PRNG), and the script says which.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
out=${1:?output directory required}
mkdir -p "$out"
export PYTHONPATH="$here"

python3 "$here/sqlite_gen.py" "$out"

second=$(mktemp -d)
trap 'rm -rf "$second"' EXIT
python3 "$here/sqlite_gen.py" "$second" >/dev/null

status=0
for f in "$second"/*.gz; do
  base=$(basename "$f")
  a=$(gunzip -c "$out/$base" | sha256sum | cut -d' ' -f1)
  b=$(gunzip -c "$f" | sha256sum | cut -d' ' -f1)
  case "$base" in
    *.db-wal.gz|*.db-journal.gz) kind="engine-written companion (not reproducible)" ;;
    *) kind="database file" ;;
  esac
  if [ "$a" = "$b" ]; then
    echo "identical across two generations: $base ($kind)"
  else
    echo "differs across two generations:   $base ($kind)"
    case "$base" in
      *.db-wal.gz|*.db-journal.gz) ;;
      *) # a database next to a killed writer's companion is not reproducible either
         if [ -e "$out/${base%.db.gz}.db-wal.gz" ] || [ -e "$out/${base%.db.gz}.db-journal.gz" ]; then :; else status=1; fi ;;
    esac
  fi
done
exit $status

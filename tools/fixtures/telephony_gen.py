"""Generator of the Android telephony fixtures (plan 4D).

    py -3 tools/fixtures/telephony_gen.py --family mmssms --out internal/parsers/androidmms/testdata
    py -3 tools/fixtures/telephony_gen.py --family mmssms --out <dir> --variant utf16 hot-journal
    py -3 tools/fixtures/telephony_gen.py --family mmssms --check      # smoke test: generate into a temp dir and discard
    py -3 tools/fixtures/telephony_gen.py --family mmssms --recover-oracle --out internal/parsers/androidmms/testdata

--recover-oracle writes <variant>.recover.expect.json for every variant that has a scenario (plan 4G): the typed
recovery oracle, computed from the COMMITTED <variant> files in --out by an independent page walker
(sqlite_oracle.build_history) and the scenario oracle. The variant is regenerated in a scratch directory only to
obtain the scenario (what it wrote and deleted); the committed files are never rewritten, and their SHA-256 must
equal the one in <variant>.expect.json. --check also builds the recover oracle of the freshly generated files.

With --pins-go it writes the complete fixturePins Go file (every variant, so no --variant);
the file is written already gofmt-formatted and committed as generated.
Needs Python 3 with its bundled sqlite3 only (no Docker).
Databases written by a different SQLite version differ in the header bytes, so a
fixture is always regenerated together with its pin and its oracle.
"""
import argparse
import gzip
import hashlib
import json
import os
import shutil
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import telephony_common as C  # noqa: E402
import telephony_mmssms  # noqa: E402

FAMILIES = {"mmssms": telephony_mmssms}


def load_optional():
    for name in ("telephony_calllog", "telephony_contacts", "browser_chromium", "browser_safari"):
        try:
            mod = __import__(name)
        except ImportError:
            continue
        FAMILIES[mod.FAMILY] = mod


def main(argv):
    load_optional()
    ap = argparse.ArgumentParser()
    ap.add_argument("--family", required=True, choices=sorted(FAMILIES))
    ap.add_argument("--out")
    ap.add_argument("--variant", nargs="*")
    ap.add_argument("--check", action="store_true")
    ap.add_argument("--recover-oracle", action="store_true", help="write <variant>.recover.expect.json from the committed files in --out")
    ap.add_argument("--pins-go", help="write the complete fixturePins Go file (all variants)")
    ap.add_argument("--pins-package", default="")
    a = ap.parse_args(argv)
    if a.pins_go and (a.variant or not a.pins_package):
        ap.error("--pins-go needs every variant and --pins-package")
    fam = FAMILIES[a.family]
    names = a.variant or list(fam.VARIANTS)
    if a.recover_oracle:
        if not a.out or a.check or a.pins_go:
            ap.error("--recover-oracle needs --out (the directory of the committed fixtures) and no --check or --pins-go")
        recover_oracles(fam, a.out, names)
        return
    out = a.out or tempfile.mkdtemp(prefix="telephony-check-")
    pins = {}
    for name in names:
        work = tempfile.mkdtemp(prefix="telephony-gen-")
        try:
            files, exp = fam.VARIANTS[name](work)
        finally:
            shutil.rmtree(work, ignore_errors=True)
        hashes = C.write_outputs(out, name, files, exp)
        for role in hashes:
            pins["%s.%s" % (name, role)] = hashes[role]
        print("wrote", name, sorted(files), len(exp["records"]), "records")
    if a.pins_go:
        write_pins(a.pins_go, a.pins_package, pins)
        print("wrote", a.pins_go)
    if a.check:
        recover_oracles(fam, out, [n for n in names if n in getattr(fam, "SCENARIOS", {})], check=True)
        shutil.rmtree(out, ignore_errors=True)


def read_files(out, name):
    files = {}
    for role, suffix in C.SUFFIX.items():
        path = os.path.join(out, name + suffix)
        if os.path.exists(path):
            with gzip.open(path, "rb") as f:
                files[role] = f.read()
    return files


def recover_oracles(fam, out, names, check=False):
    """Writes (or, with check, only builds) the recover oracle of every named variant that has a scenario."""
    for name in names:
        work = tempfile.mkdtemp(prefix="telephony-gen-")
        try:
            fam.VARIANTS[name](work)  # only for fam.SCENARIOS[name]: what the variant wrote and deleted
        finally:
            shutil.rmtree(work, ignore_errors=True)
        if name not in fam.SCENARIOS:
            continue
        files = read_files(out, name)
        with open(os.path.join(out, name + ".expect.json"), encoding="utf-8") as f:
            want = json.load(f)["generator"]["files"]
        got = {role: hashlib.sha256(data).hexdigest() for role, data in files.items()}
        if got != want:
            raise SystemExit("%s: the committed files do not match %s.expect.json (%s != %s)" % (name, name, got, want))
        obj = fam.recover_expect(name, files)
        n = len(obj["records"])
        if check:
            print("recover oracle", name, n, "records (not written)")
        else:
            print("wrote", C.write_recover_expect(out, name, obj), n, "records,", len(obj["written"]), "written versions")


def write_pins(path, package, pins):
    lines = ["// Code generated by tools/fixtures/telephony_gen.py; DO NOT EDIT.", "", "package " + package, "",
             "// fixturePins is the SHA-256 of every decompressed fixture file, by \"<variant>.<role>\". A changed fixture",
             "// changes this file in the same commit, so a silent regeneration shows in review.",
             "var fixturePins = map[string]string{"]
    width = max(len(k) for k in pins)  # gofmt aligns the values of a map literal
    lines += ['\t"%s": %s"%s",' % (k, " " * (width - len(k)), pins[k]) for k in sorted(pins)]
    lines += ["}", ""]
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(lines))


if __name__ == "__main__":
    main(sys.argv[1:])

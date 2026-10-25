"""Cross-check of the Chromium history DDL snapshots against the public sources (plan 4F, Task 1).

    py -3 -I tools/fixtures/ddl_crosscheck_chromium.py <srcdir> <schemasdir>
        [--tag TAG] [--commit SHA] [--taken DATE] [--subst NAME=VALUE]... [--rename OLD=NEW]
        [--write-report FILE] [--emit TABLE[:LINE]]...

<srcdir> holds the fetched C++ files under their base names. They are untrusted text: this
script tokenises them as text and nothing in them is executed, imported, included or used as a
path or a command. It never starts a process.

The tokenizer drops comments and preprocessor lines (it keeps `#define NAME "literal"` macros),
and decodes string literals. A statement is a run of adjacent literals (and macro / --subst
identifiers, and base::StrCat({...}) pieces) whose text starts with CREATE TABLE or ALTER TABLE.
An identifier it cannot resolve makes the statement MANUAL (reported with file:line, never
guessed). SQL text is compared after collapsing whitespace.

Every statement of a snapshot (`<schemasdir>/chromium-*.sql`) must appear in the sources:
  verdict verified     the literal statement is in the sources;
  verdict historical   the statement is in the sources after --rename OLD=NEW is applied to the
                       source text (the migration code builds the old shape under a temporary
                       name), so it is migration-code evidence, not a statement of the tag's
                       current schema.
The header line `-- source chromium <tag> <file>:<line>, ...` must name exactly the locations
the statements matched. A statement with no match is MISMATCH (or MANUAL when a source
statement of that table could not be resolved). A MANUAL row is accepted only when the existing
report file already carries `manual-verified <file>:<lines>` on the row of that statement.

The constants (page transition values, the history database integers of download state and
danger type, the interrupt reasons, the history version numbers and the migration that adds
visits.opener_visit) are extracted from the enums and macros and written as the Constants table
of the report. A constant that cannot be found makes the run fail.

--emit TABLE[:LINE] prints the one-line CREATE TABLE of TABLE from the sources (LINE chooses
among several definitions). Exit status 0 only when every row is verified, historical or
manual-verified.
"""
import hashlib
import os
import re
import sys

# Fetched file base name -> path in the Chromium repository (for the report).
FILES = {
    "url_database.cc": "components/history/core/browser/url_database.cc",
    "visit_database.cc": "components/history/core/browser/visit_database.cc",
    "download_database.cc": "components/history/core/browser/download_database.cc",
    "history_database.cc": "components/history/core/browser/history_database.cc",
    "download_types.cc": "components/history/core/browser/download_types.cc",
    "download_types.h": "components/history/core/browser/download_types.h",
    "download_constants.h": "components/history/core/browser/download_constants.h",
    "page_transition_types.h": "ui/base/page_transition_types.h",
    "download_danger_type.h": "components/download/public/common/download_danger_type.h",
    "download_interrupt_reasons.h": "components/download/public/common/download_interrupt_reasons.h",
    "download_interrupt_reason_values.h": "components/download/public/common/download_interrupt_reason_values.h",
    "meta_table.cc": "sql/meta_table.cc",
}
NOTES_MARK = "<!-- notes -->"
SQL_FILES = ["url_database.cc", "visit_database.cc", "download_database.cc", "meta_table.cc"]
IDENT_START = re.compile(r"[A-Za-z_]")
IDENT_RE = re.compile(r"[A-Za-z_]\w*")
NUM_RE = re.compile(r"\d[\w']*")
DEFINE_RE = re.compile(r'#\s*define\s+(\w+)\s+((?:"(?:[^"\\]|\\.)*"\s*)+)$')
ESC = {"n": "\n", "t": "\t", '"': '"', "\\": "\\", "'": "'"}


class SourceError(Exception):
    pass


def tokenize(text, name):
    """Returns (tokens, macros): tokens are (kind, value, line); kinds s (string), i, n, p."""
    toks, macros = [], {}
    i, n, line = 0, len(text), 1
    at_line_start = True
    while i < n:
        c = text[i]
        if c == "\n":
            line += 1
            i += 1
            at_line_start = True
            continue
        if c in " \t\r":
            i += 1
            continue
        if c == "/" and text.startswith("//", i):
            while i < n and text[i] != "\n":
                i += 1
            continue
        if c == "/" and text.startswith("/*", i):
            j = text.find("*/", i + 2)
            j = n if j < 0 else j + 2
            line += text.count("\n", i, j)
            i = j
            continue
        if c == "#" and at_line_start:
            j = i
            while j < n and text[j] != "\n":
                if text[j] == "\\" and j + 1 < n and text[j + 1] == "\n":
                    line += 1
                    j += 1
                j += 1
            directive = text[i:j].replace("\\\n", " ")
            m = DEFINE_RE.match(directive)
            if m:
                parts = re.findall(r'"((?:[^"\\]|\\.)*)"', m.group(2))
                macros[m.group(1)] = "".join(re.sub(r"\\(.)", lambda e: ESC.get(e.group(1), e.group(1)), p) for p in parts)
            i = j
            continue
        at_line_start = False
        if c == '"':
            j, buf = i + 1, []
            while j < n and text[j] != '"':
                if text[j] == "\n":
                    raise SourceError("%s:%d: unterminated string literal" % (name, line))
                if text[j] == "\\" and j + 1 < n:
                    buf.append(ESC.get(text[j + 1], text[j + 1]))
                    j += 2
                else:
                    buf.append(text[j])
                    j += 1
            toks.append(("s", "".join(buf), line))
            i = j + 1
            continue
        if c == "'":
            j = i + 1
            while j < n and text[j] != "'":
                j += 2 if text[j] == "\\" else 1
            i = j + 1
            continue
        if IDENT_START.match(c):
            m = IDENT_RE.match(text, i)
            if m.group(0) in ("R", "LR", "u8R", "uR", "UR") and text.startswith('"', m.end()):
                op = text.find("(", m.end())
                if op < 0 or op - m.end() > 17:
                    raise SourceError("%s:%d: malformed raw string literal" % (name, line))
                close = ")" + text[m.end() + 1:op] + '"'
                end = text.find(close, op)
                if end < 0:
                    raise SourceError("%s:%d: unterminated raw string literal" % (name, line))
                toks.append(("s", text[op + 1:end], line))
                line += text.count("\n", i, end)
                i = end + len(close)
                continue
            toks.append(("i", m.group(0), line))
            i = m.end()
            continue
        if c.isdigit():
            m = NUM_RE.match(text, i)
            toks.append(("n", m.group(0), line))
            i = m.end()
            continue
        toks.append(("p", c, line))
        i += 1
    return toks, macros


def canon(sql):
    s = re.sub(r"\s+", " ", sql).strip().rstrip(";").strip()
    return re.sub(r"\s*([(),])\s*", r"\1", s)


def extract_statements(toks, macros, subst, fname):
    """CREATE TABLE / ALTER TABLE statements: list of dicts (file, line, sql, unresolved)."""
    out, i, n = [], 0, len(toks)
    while i < n:
        kind, val, line = toks[i]
        if kind != "s" or not re.match(r"\s*(CREATE|ALTER)\s+TABLE\b", val):
            i += 1
            continue
        braced = i > 0 and toks[i - 1][:2] == ("p", "{")
        pieces, unresolved, j = [], [], i
        while j < n:
            k, v, _ = toks[j]
            if k == "s":
                pieces.append(v)
                j += 1
            elif k == "i":
                if j + 1 < n and toks[j + 1][:2] == ("p", "("):
                    break
                if v in macros:
                    pieces.append(macros[v])
                elif v in subst:
                    pieces.append(subst[v])
                else:
                    unresolved.append(v)
                    pieces.append("?" + v + "?")
                j += 1
            elif k == "p" and v in ",+" and (v == "+" or braced) and j + 1 < n and toks[j + 1][0] in "si":
                j += 1
            else:
                break
        out.append({"file": fname, "line": line, "sql": "".join(pieces), "unresolved": unresolved})
        i = max(j, i + 1)
    return out


def function_ranges(toks):
    """(name, first line, last line) of every function definition `A::b(...) ... { ... }`."""
    out, i, n = [], 0, len(toks)
    while i + 4 < n:
        if toks[i][0] == "i" and toks[i + 1][1] == ":" and toks[i + 2][1] == ":" and toks[i + 3][0] == "i" and toks[i + 4][1] == "(":
            name, start, j, depth = toks[i + 3][1], toks[i][2], i + 4, 0
            while j < n:
                if toks[j][1] == "(" and toks[j][0] == "p":
                    depth += 1
                elif toks[j][1] == ")" and toks[j][0] == "p":
                    depth -= 1
                    if depth == 0:
                        break
                j += 1
            j += 1
            while j < n and toks[j][0] == "i" and toks[j][1] in ("const", "override", "noexcept"):
                j += 1
            if j < n and toks[j][:2] == ("p", "{"):
                depth = 0
                while j < n:
                    if toks[j][0] == "p" and toks[j][1] == "{":
                        depth += 1
                    elif toks[j][0] == "p" and toks[j][1] == "}":
                        depth -= 1
                        if depth == 0:
                            break
                    j += 1
                out.append((name, start, toks[min(j, n - 1)][2]))
                i = j
        i += 1
    return out


def parse_int(tokens):
    """Evaluates `-N`, `N`, `0xN`, `1 << n` token lists; None for anything else (an alias)."""
    def one(t):
        t = t.replace("'", "").rstrip("uUlL")
        return int(t, 16) if t.lower().startswith("0x") else int(t, 10)
    vals = [t for t in tokens]
    try:
        if len(vals) == 1 and vals[0][0] == "n":
            return one(vals[0][1])
        if len(vals) == 2 and vals[0][1] == "-" and vals[1][0] == "n":
            return -one(vals[1][1])
        if len(vals) == 4 and vals[0][0] == "n" and vals[1][1] == "<" and vals[2][1] == "<" and vals[3][0] == "n":
            return one(vals[0][1]) << one(vals[3][1])
    except ValueError:
        return None
    return None


def enum_entries(toks, enum_name):
    """{ENTRY: (value, line)} of `enum [class] NAME [: type] { ... }`; aliases of earlier entries
    take their value, other non-literal expressions are skipped."""
    n = len(toks)
    for i in range(n - 1):
        if toks[i][:2] != ("i", "enum"):
            continue
        j = i + 1
        if toks[j][:2] == ("i", "class"):
            j += 1
        if toks[j][:2] != ("i", enum_name):
            continue
        while j < n and toks[j][:2] != ("p", "{"):
            j += 1
        j += 1
        out, prev = {}, -1
        while j < n and toks[j][:2] != ("p", "}"):
            if toks[j][0] != "i":
                j += 1
                continue
            name, line = toks[j][1], toks[j][2]
            j += 1
            expr = []
            if toks[j][:2] == ("p", "="):
                j += 1
                while toks[j][:2] not in (("p", ","), ("p", "}")):
                    expr.append(toks[j])
                    j += 1
                val = parse_int(expr)
                if val is None and len(expr) == 1 and expr[0][0] == "i" and expr[0][1] in out:
                    val = out[expr[0][1]][0]  # alias of an earlier entry
                if val is None:
                    continue  # a mask expression
            else:
                val = prev + 1
            prev = val
            out[name] = (val, line)
            if toks[j][:2] == ("p", ","):
                j += 1
        return out
    raise SourceError("enum %s not found" % enum_name)


def split_phases(text):
    out, cur = {0: []}, 0
    for ln in text.splitlines():
        s = ln.strip()
        if s.startswith("-- @phase "):
            cur = int(s.split()[2])
            out.setdefault(cur, [])
            continue
        out[cur].append(ln)
    return {k: "\n".join(v) for k, v in out.items()}


def sql_statements(text):
    body = "\n".join(l for l in text.splitlines() if not l.strip().startswith("--"))
    return [s.strip() for s in body.split(";") if s.strip()]


def label(stmt):
    m = re.match(r"(CREATE|ALTER)\s+TABLE\s+(\w+)(?:\s+ADD\s+COLUMN\s+(\w+))?", canon(stmt))
    if not m:
        return "unrecognised statement"
    return "%s TABLE %s%s" % (m.group(1), m.group(2), (" ADD COLUMN " + m.group(3)) if m.group(3) else "")


def table_of(sql):
    m = re.match(r"\s*(?:CREATE|ALTER)\s+TABLE\s+(\w+)", sql)
    return m.group(1) if m else ""


def load_sources(srcdir):
    parsed, shas = {}, {}
    for base in sorted(FILES):
        path = os.path.join(srcdir, base)
        if not os.path.isfile(path):
            raise SourceError("missing fetched file %s" % base)
        with open(path, "rb") as f:
            raw = f.read()
        shas[base] = hashlib.sha256(raw).hexdigest()
        parsed[base] = tokenize(raw.decode("utf-8", "replace"), base)
    return parsed, shas


def constants(parsed):
    """Rows (enum, int, name, source) for the Constants table; raises SourceError when absent."""
    rows = []
    pt = enum_entries(parsed["page_transition_types.h"][0], "PageTransition")
    for ent, (val, line) in pt.items():
        short = ent[len("PAGE_TRANSITION_"):].lower()
        if ent.endswith(("_MASK", "_FIRST", "_LAST_CORE")):
            continue  # masks and the range markers of the core values
        src = "page_transition_types.h:%d" % line
        if 0 <= val <= 0xFF:
            rows.append(("transition_core", val, short, src))
        else:
            mask = val & 0xFFFFFFFF
            rows.append(("transition_qualifier", mask, short, src + (" (declared %d)" % val if val < 0 else "")))
    if sum(1 for r in rows if r[0] == "transition_core") != 11:
        raise SourceError("expected the 11 core transitions 0..10")
    for enum, name, ek in (("download_state", "DownloadState", "download_constants.h"), ("download_danger", "DownloadDangerType", "download_constants.h")):
        for ent, (val, line) in enum_entries(parsed[ek][0], name).items():
            if val >= 0:
                rows.append((enum, val, ent.lower(), "%s:%d" % (ek, line)))
    # The history database stores static_cast<int>(value) of both enums (download_types.cc).
    types_cc = " ".join(t[1] for t in parsed["download_types.cc"][0])
    for fn in ("DownloadStateToInt", "DownloadDangerTypeToInt", "DownloadInterruptReasonToInt"):
        if not re.search(r"int %s \( \w+ \w+ \) \{.*?return static_cast < int > \(" % fn, types_cc):
            raise SourceError("%s is not a plain static_cast any more: read download_types.cc" % fn)
    none = enum_entries(parsed["download_interrupt_reasons.h"][0], "DownloadInterruptReason")
    for ent, (val, line) in none.items():
        rows.append(("download_interrupt", val, ent[len("DOWNLOAD_INTERRUPT_REASON_"):].lower(), "download_interrupt_reasons.h:%d" % line))
    vt = parsed["download_interrupt_reason_values.h"][0]
    for i in range(len(vt) - 5):
        if vt[i][1] == "INTERRUPT_REASON" and vt[i + 1][1] == "(" and vt[i + 3][1] == "," and vt[i + 4][0] == "n":
            rows.append(("download_interrupt", int(vt[i + 4][1]), vt[i + 2][1].lower(), "download_interrupt_reason_values.h:%d" % vt[i][2]))
    if sum(1 for r in rows if r[0] == "download_interrupt") < 20:
        raise SourceError("interrupt reasons not found")
    ht = parsed["history_database.cc"][0]
    for i in range(len(ht) - 5):
        if ht[i][1] in ("kCurrentVersionNumber", "kCompatibleVersionNumber") and ht[i + 1][1] == "=" and ht[i + 2][0] == "n":
            rows.append(("history_version", int(ht[i + 2][1]), "current" if "Current" in ht[i][1] else "compatible", "history_database.cc:%d" % ht[i][2]))
    if sorted(r[2] for r in rows if r[0] == "history_version") != ["compatible", "current"]:
        raise SourceError("kCurrentVersionNumber / kCompatibleVersionNumber not found")
    return rows, opener_rows(parsed)


def opener_rows(parsed):
    vtoks = parsed["visit_database.cc"][0]
    stmts = extract_statements(vtoks, parsed["visit_database.cc"][1], {}, "visit_database.cc")
    funcs = function_ranges(vtoks)
    hits = [s for s in stmts if re.search(r"ADD COLUMN opener_visit\b", s["sql"])]
    if len(hits) != 1:
        raise SourceError("expected exactly one ALTER adding visits.opener_visit, found %d" % len(hits))
    fn = [f for f in funcs if f[1] <= hits[0]["line"] <= f[2]]
    if len(fn) != 1:
        raise SourceError("cannot find the function around visit_database.cc:%d" % hits[0]["line"])
    name = fn[0][0]
    ht = parsed["history_database.cc"][0]
    for i in range(len(ht) - 8):
        if [t[1] for t in ht[i:i + 3]] == ["cur_version", "=", "="] and ht[i + 3][0] == "n":
            frm = int(ht[i + 3][1])
            j, depth, seen, bumped = i + 4, 0, False, False
            while j < len(ht):
                if ht[j][:2] == ("p", "{"):
                    depth += 1
                elif ht[j][:2] == ("p", "}"):
                    depth -= 1
                    if depth == 0:
                        break
                elif ht[j][1] == name:
                    seen = True
                elif ht[j][1] in ("cur_version",) and (ht[j - 1][1] == "+" or ht[j + 1][1] == "+"):
                    bumped = True
                j += 1
            if seen:
                if not bumped:
                    raise SourceError("the block of cur_version == %d does not increment the version" % frm)
                loc = "history_database.cc:%d, visit_database.cc:%d" % (ht[i][2], hits[0]["line"])
                return [("opener_visit_version", frm, "migrates_from", loc), ("opener_visit_version", frm + 1, "migrates_to", loc)]
    raise SourceError("%s is not called from a cur_version block" % name)


def parse_header(text):
    lines = text.splitlines()
    hdr = {}
    for ln in lines[:3]:
        m = re.match(r"-- (verdict|source|meta\.version)\s+(.*)$", ln)
        if m:
            hdr[m.group(1)] = m.group(2).strip()
    return hdr


def manual_accepted(report_path):
    acc = set()
    if report_path and os.path.isfile(report_path):
        with open(report_path, encoding="utf-8") as f:
            for ln in f:
                if "manual-verified" in ln and ln.startswith("|"):
                    cells = [c.strip() for c in ln.strip().strip("|").split("|")]
                    if len(cells) >= 5:
                        acc.add((cells[0], cells[1]))
    return acc


def main(argv):
    args, flags = [], {"subst": {}, "emit": [], "rename": None, "tag": "", "commit": "", "taken": "", "report": ""}
    it = iter(argv)
    for a in it:
        if a == "--subst":
            k, _, v = next(it).partition("=")
            flags["subst"][k] = v
        elif a == "--rename":
            flags["rename"] = tuple(next(it).split("=", 1))
        elif a == "--emit":
            flags["emit"].append(next(it))
        elif a in ("--tag", "--commit", "--taken"):
            flags[a[2:]] = next(it)
        elif a == "--write-report":
            flags["report"] = next(it)
        elif a.startswith("--"):
            print("unknown flag " + a, file=sys.stderr)
            return 2
        else:
            args.append(a)
    if len(args) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    srcdir, schemas = args
    try:
        parsed, shas = load_sources(srcdir)
        stmts = []
        for base in SQL_FILES:
            toks, macros = parsed[base]
            stmts += extract_statements(toks, macros, flags["subst"], base)
        crows, orows = constants(parsed)
    except SourceError as e:
        print("SOURCE ERROR: %s" % e, file=sys.stderr)
        return 1

    if flags["emit"]:
        for spec in flags["emit"]:
            tbl, _, ln = spec.partition(":")
            cand = [s for s in stmts if table_of(s["sql"]) == tbl and s["sql"].lstrip().startswith("CREATE")]
            if ln:
                cand = [s for s in cand if s["line"] == int(ln)]
            if len(cand) != 1:
                print("EMIT %s: %d definitions (%s); choose with TABLE:LINE" % (spec, len(cand), ", ".join("%s:%d" % (s["file"], s["line"]) for s in cand)), file=sys.stderr)
                return 1
            if cand[0]["unresolved"]:
                print("EMIT %s: unresolved identifiers %s (use --subst)" % (spec, cand[0]["unresolved"]), file=sys.stderr)
                return 1
            print(re.sub(r"\s+", " ", cand[0]["sql"]).strip() + ";   -- " + "%s:%d" % (cand[0]["file"], cand[0]["line"]))
        return 0

    accepted = manual_accepted(flags["report"])
    rows, bad = [], 0
    snaps = sorted(f for f in os.listdir(schemas) if re.match(r"chromium-.*\.sql$", f))
    if not snaps:
        print("no chromium-*.sql in %s" % schemas, file=sys.stderr)
        return 1
    for snap in snaps:
        with open(os.path.join(schemas, snap), encoding="utf-8") as f:
            text = f.read()
        hdr = parse_header(text)
        verdict = hdr.get("verdict", "")
        problems = []
        if verdict not in ("verified", "historical"):
            problems.append("header verdict %r is neither verified nor historical" % verdict)
        if "meta.version" not in hdr or not re.fullmatch(r"\d+(,\d+)*", hdr["meta.version"]):
            problems.append("header meta.version missing or malformed")
        src = hdr.get("source", "")
        m = re.match(r"chromium (\S+) (.+)$", src)
        listed = set()
        if not m:
            problems.append("header source line malformed")
        else:
            if flags["tag"] and m.group(1) != flags["tag"]:
                problems.append("header tag %s differs from --tag %s" % (m.group(1), flags["tag"]))
            listed = {x.strip() for x in m.group(2).split(",")}
        used = set()
        for ph, body in sorted(split_phases(text).items()):
            for st in sql_statements(body):
                c = canon(st)
                tbl = table_of(st)
                cand = []
                for s in stmts:
                    sc = canon(s["sql"])
                    if s["unresolved"]:
                        continue
                    if sc == c:
                        cand.append(dict(s, renamed=False))
                    elif verdict == "historical" and flags["rename"] and sc.replace(*flags["rename"]) == c:
                        cand.append(dict(s, renamed=True))
                locs = ["%s:%d" % (s["file"], s["line"]) for s in cand]
                pick = [l for l in locs if l in listed] or locs[:1]
                if pick:
                    used.add(pick[0])
                    v = verdict if verdict in ("verified", "historical") else "MISMATCH"
                    renamed = [s for s in cand if "%s:%d" % (s["file"], s["line"]) == pick[0]][0]["renamed"]
                    ev = pick[0] + (" (source text under the temporary name %s)" % flags["rename"][0] if renamed else "")
                    if pick[0] not in listed:
                        v, ev = "MISMATCH", "matches %s but the header source line does not list it" % pick[0]
                else:
                    unres = [s for s in stmts if table_of(s["sql"]) == tbl and s["unresolved"]]
                    if unres and (snap, label(st)) in accepted:
                        v, ev = "manual-verified", "see report"
                    elif unres:
                        v, ev = "MANUAL", "source statement at %s:%d has unresolved %s" % (unres[0]["file"], unres[0]["line"], unres[0]["unresolved"])
                    else:
                        v, ev = "MISMATCH", "no statement of the sources equals it"
                rows.append((snap, "phase %d" % ph if ph else "-", label(st), v, ev))
        for l in sorted(listed - used):
            problems.append("header lists %s but no statement of the snapshot matched it" % l)
        for p in problems:
            rows.append((snap, "-", "header", "MISMATCH", p))
    bad = sum(1 for r in rows if r[3] in ("MISMATCH", "MANUAL"))
    for r in rows:
        print("%-24s %-8s %-48s %-14s %s" % r)

    if flags["report"] and bad == 0:
        write_report(flags, shas, rows, crows + orows)
    print("DDL CROSSCHECK: %d rows, %d failing" % (len(rows), bad))
    return 1 if bad else 0


def write_report(flags, shas, rows, consts):
    L = ["# DDL cross-check (plan 4F, Task 1)", ""]
    L.append("Generated by tools/fixtures/ddl_crosscheck_chromium.py. Tag: `%s`, taken %s." % (flags["tag"], flags["taken"]))
    L += ["", "- chromium/src commit `%s`" % flags["commit"], "", "Fetched files (not in the repository, never executed), SHA-256:", ""]
    for b in sorted(shas):
        L.append("- %s (`%s`) `%s`" % (b, FILES[b], shas[b]))
    L += ["", "Re-fetch: `curl -fsS \"https://chromium.googlesource.com/chromium/src/+/refs/tags/%s/<path>?format=TEXT\" | base64 -d > <basename>`." % flags["tag"], ""]
    L += ["## Verdicts", "", "| Snapshot | Statement | Phase | Verdict | Evidence |", "|---|---|---|---|---|"]
    for snap, ph, lab, v, ev in rows:
        L.append("| %s | %s | %s | %s | %s |" % (snap, lab, ph, v, ev))
    L += ["", "## Constants", "", "| enum | int | name | source |", "|---|---|---|---|"]
    for e, v, n, s in consts:
        L.append("| %s | %d | %s | %s |" % (e, v, n, s))
    L.append("")
    if os.path.isfile(flags["report"]):
        with open(flags["report"], encoding="utf-8") as f:
            prev = f.read()
        k = prev.find(NOTES_MARK)
        if k >= 0:
            L.append(prev[k:].rstrip("\n") + "\n")  # hand-written notes survive a regeneration
    with open(flags["report"], "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(L))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

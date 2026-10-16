"""Cross-check of the DDL snapshots against the public provider sources (plan 4D, Task 4b).

    py -3 -I tools/fixtures/ddl_crosscheck.py <sourcedir> <schemasdir>
        [--tag TAG] [--commit REPO=SHA]... [--write-report FILE] [--emit TABLE]...

<sourcedir> holds the fetched Java files under their base names. They are untrusted text:
this script reads them as text and applies regular expressions to them; nothing in them is
executed, imported or used as a path or a command. It never starts a process.

The script strips the Java comments, masks the string literals, walks the braces to know
which class a constant belongs to, and evaluates the string concatenations that build a
CREATE TABLE or ALTER TABLE statement (literals, integer constants, "Class.NAME" constants
with the class inheritance of the sources, static imports). A statement it cannot evaluate
is reported, never guessed.

For every CREATE TABLE of a snapshot it finds the first CREATE TABLE of the same name in
the sources (the definition of the tag; older definitions inside upgrade code come later in
the files). A snapshot whose header says "verdict verified" must match it column by column
(name, order and the whole definition text); one that says "verdict historical" must only
use columns the sources define in some CREATE TABLE of that table or add with ALTER TABLE
ADD COLUMN (each cited by file and line); a table the sources never create is
no-public-source; a table whose statement the script cannot evaluate is MANUAL and must be
read by a person. Exit status 1 on any MISMATCH or MANUAL.

--emit TABLE prints the tag's definition of TABLE as a one-line CREATE TABLE.
"""
import hashlib
import os
import re
import sys

SRC_FILES = [
    "MmsSmsDatabaseHelper.java",
    "ContactsDatabaseHelper.java",
    "CallLogDatabaseHelper.java",
]
# The only columns a historical snapshot may use that no source defines: invented on purpose
# for the unmapped_columns check (named in the header of mmssms-oem-extended.sql).
INVENTED = {"sim_slot", "is_favorite", "msg_hash"}
CONSTRAINT = {"PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "CONSTRAINT"}
CLASS_RE = re.compile(
    r"\b(?:class|interface|enum)\s+(\w+)(?:\s*<[^{]*?>)?"
    r"(?:\s+extends\s+([\w.,\s<>]+?))?(?:\s+implements\s+([\w.,\s<>]+?))?\s*$"
)
FIELD_RE = re.compile(
    r"^(?:(?:public|private|protected|static|final)\s+)*(?:String|int|long|char|boolean)\s+(\w+)\s*=\s*(.+)$",
    re.S,
)
TOKEN_RE = re.compile(
    r"\s*(?:(?P<str>\x01\d+\x02)|(?P<int>-?(?:0[xX][0-9a-fA-F]+|\d+))|(?P<id>[A-Za-z_][\w.]*)|(?P<op>[+()]))"
)


class Unresolved(Exception):
    pass


def strip_and_mask(text):
    """Return (masked, strings): comments removed, every string literal replaced by \\x01N\\x02."""
    out, strings, i, n = [], [], 0, len(text)
    while i < n:
        c = text[i]
        if c == '"':
            j = i + 1
            while j < n and text[j] != '"':
                j += 2 if text[j] == "\\" else 1
            raw = text[i + 1:j]
            strings.append(re.sub(r"\\(.)", lambda m: {"n": "\n", "t": "\t"}.get(m.group(1), m.group(1)), raw))
            out.append("\x01%d\x02" % (len(strings) - 1))
            i = j + 1
        elif c == "'":
            j = i + 1
            while j < n and text[j] != "'":
                j += 2 if text[j] == "\\" else 1
            out.append("0")  # a char literal is never part of a statement we evaluate
            i = j + 1
        elif text.startswith("//", i):
            j = text.find("\n", i)
            i = n if j < 0 else j
        elif text.startswith("/*", i):
            j = text.find("*/", i + 2)
            seg = text[i:] if j < 0 else text[i:j + 2]
            out.append("\n" * seg.count("\n"))  # keep line numbers
            i = n if j < 0 else j + 2
        else:
            out.append(c)
            i += 1
    return "".join(out), strings


class Source:
    """One fetched Java file: masked text, strings, constants by class, static imports."""

    def __init__(self, name, path):
        self.name = name
        with open(path, encoding="utf-8", errors="replace") as f:
            raw = f.read()
        self.sha256 = hashlib.sha256(open(path, "rb").read()).hexdigest()
        self.text, self.strings = strip_and_mask(raw)
        self.consts = {}  # (class, NAME) -> expr
        self.supers = {}  # class -> [super simple names]
        self.static_imports = dict(
            (m.group(2), m.group(1).split(".")[-1])
            for m in re.finditer(r"import\s+static\s+([\w.]+)\.(\w+)\s*;", raw)
        )
        self._walk()

    def line_of(self, pos):
        return self.text.count("\n", 0, pos) + 1

    def _walk(self):
        stack, start = [], 0
        for i, c in enumerate(self.text):
            if c not in ";{}":
                continue
            seg = self.text[start:i]
            if c == "{":
                m = CLASS_RE.search(re.sub(r"@\w+(?:\([^)]*\))?", " ", seg).strip().replace("\n", " "))
                if m:
                    sup = []
                    for g in (m.group(2), m.group(3)):
                        for s in re.split(r",", g or ""):
                            s = re.sub(r"<.*", "", s).strip().split(".")[-1]
                            if s:
                                sup.append(s)
                    stack.append(m.group(1))
                    self.supers.setdefault(m.group(1), []).extend(sup)
                else:
                    stack.append(None)
            elif c == "}":
                if stack:
                    stack.pop()
            else:  # ';'
                if stack and stack[-1]:
                    s = re.sub(r"@\w+(?:\([^)]*\))?", " ", seg).strip()
                    m = FIELD_RE.match(s)
                    if m:
                        self.consts.setdefault((stack[-1], m.group(1)), m.group(2))
            start = i + 1


class World:
    def __init__(self, srcdir):
        self.files = {}
        for fn in sorted(os.listdir(srcdir)):
            if fn.endswith(".java"):
                self.files[fn] = Source(fn, os.path.join(srcdir, fn))

    def find(self, cls, name, seen=None):
        """[(source, expr)] of constant name in class cls or a super type of it (the class's own
        definitions first)."""
        seen = seen if seen is not None else set()
        if cls in seen:
            return []
        seen.add(cls)
        hits = [(src, src.consts[(cls, name)]) for src in self.files.values() if (cls, name) in src.consts]
        for src in self.files.values():
            for sup in src.supers.get(cls, []):
                hits += self.find(sup, name, seen)
        return hits

    def lookup(self, src, ident):
        parts = ident.split(".")
        if len(parts) >= 2:
            hits = self.find(parts[-2], parts[-1])
        else:
            hits = [(src, expr) for (cls, name), expr in src.consts.items() if name == ident]
            if not hits and ident in src.static_imports:
                hits = self.find(src.static_imports[ident], ident)
        if not hits:
            raise Unresolved(ident)
        return hits

    def evaluate(self, src, expr, depth=0):
        if depth > 8:
            raise Unresolved("depth")
        toks, pos = [], 0
        expr = expr.strip()
        while pos < len(expr):
            m = TOKEN_RE.match(expr, pos)
            if not m:
                raise Unresolved(expr[pos:pos + 30].strip() or "end")
            toks.append(m)
            pos = m.end()
        val, i = self._sum(src, toks, 0, depth)
        if i != len(toks):
            raise Unresolved("trailing")
        return val

    def _sum(self, src, toks, i, depth):
        acc, i = self._term(src, toks, i, depth)
        while i < len(toks) and toks[i].group("op") == "+":
            nxt, i = self._term(src, toks, i + 1, depth)
            acc = str(acc) + str(nxt) if isinstance(acc, str) or isinstance(nxt, str) else acc + nxt
        return acc, i

    def _term(self, src, toks, i, depth):
        if i >= len(toks):
            raise Unresolved("end")
        t = toks[i]
        if t.group("str"):
            return src.strings[int(t.group("str")[1:-1])], i + 1
        if t.group("int"):
            return int(t.group("int"), 0), i + 1
        if t.group("op") == "(":
            v, j = self._sum(src, toks, i + 1, depth)
            if j >= len(toks) or toks[j].group("op") != ")":
                raise Unresolved("paren")
            return v, j + 1
        if t.group("id"):
            if i + 1 < len(toks) and toks[i + 1].group("op") == "(":
                raise Unresolved(t.group("id") + "(")
            for hit_src, hit_expr in self.lookup(src, t.group("id")):
                try:
                    return self.evaluate(hit_src, hit_expr, depth + 1), i + 1
                except Unresolved:
                    continue  # a self-referencing definition: try the next one
            raise Unresolved(t.group("id"))
        raise Unresolved("token")


def expression_at(text, pos):
    """The Java expression that starts at pos: up to ; , or an unmatched ) at depth 0."""
    depth = 0
    for j in range(pos, len(text)):
        c = text[j]
        if c == "(":
            depth += 1
        elif c == ")":
            if depth == 0:
                return text[pos:j]
            depth -= 1
        elif c in ";," and depth == 0:
            return text[pos:j]
    return text[pos:]


def split_items(body):
    depth, cur, items = 0, "", []
    for ch in body:
        depth += ch == "("
        depth -= ch == ")"
        if ch == "," and depth == 0:
            items.append(cur)
            cur = ""
        else:
            cur += ch
    items.append(cur)
    return [" ".join(i.split()) for i in items if i.strip()]


def parse_create(sql):
    """(table, [(column, definition)]) of a CREATE TABLE statement, or None."""
    m = re.match(r"\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\((.*)\)\s*;?\s*$", sql, re.S | re.I)
    if not m:
        return None
    cols = []
    for it in split_items(m.group(2)):
        w = it.split()
        cols.append(("<constraint>" if w[0].upper() in CONSTRAINT else w[0], it))
    return m.group(1), cols


def scan_statements(world, src, kind):
    """[(line, table, cols_or_column)] for every CREATE TABLE (kind 'create') or ALTER TABLE ADD COLUMN
    (kind 'alter') in src; the ones that cannot be evaluated come back as (line, None, reason)."""
    out = []
    head = re.compile(r"\s*(CREATE\s+TABLE|ALTER\s+TABLE)\b", re.I)
    for m in re.finditer(r"\x01(\d+)\x02", src.text):
        h = head.match(src.strings[int(m.group(1))])
        if not h or h.group(1).split()[0].upper() != ("CREATE" if kind == "create" else "ALTER"):
            continue
        expr = expression_at(src.text, m.start())
        line = src.line_of(m.start())
        try:
            sql = world.evaluate(src, expr)
        except Unresolved as e:
            out.append((line, None, "cannot evaluate %s" % e))
            continue
        if kind == "create":
            p = parse_create(sql)
            if p:
                out.append((line, p[0], p[1]))
        else:
            am = re.match(r"\s*ALTER\s+TABLE\s+(\w+)\s+ADD\s+(?:COLUMN\s+)?(\w+)\s", sql + " ", re.I)
            if am:
                out.append((line, am.group(1), am.group(2)))
    return out


def snapshot(path):
    with open(path, encoding="utf-8") as f:
        text = f.read()
    verdict = "verified" if "verdict verified" in text else "historical" if "verdict historical" in text else "no-public-source"
    tabs = {}
    for m in re.finditer(r"^CREATE TABLE (\w+) \((.*)\);\s*$", text, re.M):
        tabs[m.group(1)] = parse_create(m.group(0))[1]
    adds = re.findall(r"^ALTER TABLE (\w+) ADD COLUMN (\w+) ", text, re.M)
    return verdict, tabs, adds


def norm(d):
    return re.sub(r"\s+", " ", d).strip().upper()


def main(argv):
    pos = [a for a in argv if not a.startswith("--")]
    opt = lambda k: [argv[i + 1] for i, a in enumerate(argv) if a == k]
    srcdir, schemas = pos[0], pos[1]
    world = World(srcdir)
    creates, alters, unresolved = {}, {}, []
    for name in SRC_FILES:
        src = world.files.get(name)
        if src is None:
            continue
        for line, table, cols in scan_statements(world, src, "create"):
            if table is None:
                unresolved.append((name, line, cols))
            else:
                creates.setdefault(table, []).append((name, line, cols))
        for line, table, col in scan_statements(world, src, "alter"):
            if table is not None:
                alters.setdefault(table, []).append((name, line, col))
    for t in opt("--emit"):
        name, line, cols = creates[t][0]
        print("CREATE TABLE %s (%s);" % (t, ", ".join(d for _, d in cols)))
    rows, bad = [], False
    for fn in sorted(f for f in os.listdir(schemas) if f.endswith(".sql")):
        verdict, tabs, adds = snapshot(os.path.join(schemas, fn))
        for tbl, col in adds:  # a column the snapshot adds with ALTER TABLE must be added by the sources' upgrade code
            hit = [(f, l) for f, l, c in alters.get(tbl, []) if c == col]
            if hit:
                rows.append((fn, tbl + "." + col, "historical", "ALTER TABLE ADD COLUMN at %s:%d" % hit[0]))
            else:
                bad = True
                rows.append((fn, tbl + "." + col, "MISMATCH", "no ALTER TABLE %s ADD COLUMN %s in the sources" % (tbl, col)))
        for tbl, cols in sorted(tabs.items()):
            defs = creates.get(tbl)
            if not defs:
                if unresolved:
                    rows.append((fn, tbl, "MANUAL", "no evaluated CREATE TABLE of this name, and %d statements could not be evaluated: %s" % (
                        len(unresolved), "; ".join("%s:%d %s" % u for u in unresolved[:3]))))
                    bad = True
                else:
                    rows.append((fn, tbl, "no-public-source", "no CREATE TABLE of this name in the fetched files"))
                continue
            sname, sline, scols = defs[0]
            have = [(c, norm(d)) for c, d in cols]
            want = [(c, norm(d)) for c, d in scols]
            if verdict == "verified":
                if have == want:
                    rows.append((fn, tbl, "verified", "%s:%d, %d columns, names, order and definitions equal" % (sname, sline, len(want))))
                else:
                    bad = True
                    hn, wn = [c for c, _ in have], [c for c, _ in want]
                    diff = [c for (c, d) in have if (c, d) not in want]
                    rows.append((fn, tbl, "MISMATCH", "%s:%d snapshot-only %s, source-only %s, differing definitions or order: %s" % (
                        sname, sline, [c for c in hn if c not in wn], [c for c in wn if c not in hn], diff)))
            else:  # historical
                known = {}
                for fname, line, cs in defs:
                    for c, _ in cs:
                        known.setdefault(c, "%s:%d" % (fname, line))
                for fname, line, c in alters.get(tbl, []):
                    known.setdefault(c, "%s:%d (ALTER TABLE ADD COLUMN)" % (fname, line))
                extra = [c for c, _ in have if c not in known]
                invented = [c for c in extra if c in INVENTED]
                missing = [c for c in extra if c not in INVENTED]
                if missing:
                    bad = True
                    rows.append((fn, tbl, "MISMATCH", "%s:%d columns not defined by the sources: %s" % (sname, sline, missing)))
                else:
                    cited = sorted(set(known[c] for c, _ in have if c in known))
                    note = "; invented on purpose: %s" % sorted(invented) if invented else ""
                    rows.append((fn, tbl, "historical", "%d columns, all defined at %s%s" % (len(have), ", ".join(cited), note)))
    for r in rows:
        print("%-16s %-34s %-22s %s" % (r[2], r[0], r[1], r[3]))
    for u in unresolved:
        print("unevaluated statement %s:%d: %s" % u)
    report = opt("--write-report")
    if report:
        tag = (opt("--tag") or ["?"])[0]
        with open(report[0], "w", encoding="utf-8", newline="\n") as f:
            f.write("# DDL cross-check (plan 4D, Task 4b)\n\n")
            f.write("Generated by tools/fixtures/ddl_crosscheck.py. Tag: `%s`.\n\n" % tag)
            for c in opt("--commit"):
                repo, _, sha = c.partition("=")
                f.write("- %s commit `%s`\n" % (repo, sha))
            f.write("\nFetched files (not in the repository), SHA-256:\n\n")
            for fn, s in sorted(world.files.items()):
                f.write("- %s `%s`\n" % (fn, s.sha256))
            f.write("\nThe definition at the tag is the first CREATE TABLE of the table in the file; "
                    "statements the script could not evaluate: %d.\n\n" % len(unresolved))
            f.write("| File | Table | Verdict | Evidence |\n|---|---|---|---|\n")
            for fn, tbl, v, ev in rows:
                f.write("| %s | %s | %s | %s |\n" % (fn, tbl, v, ev))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

"""Shared pieces of the Android telephony fixture generators (plan 4D).

The generators build schema-accurate synthetic databases with Python's sqlite3,
read the stored values back from the engine on a COPY of the finished files, and
state the expected parser output literally (the scenario oracle of spec 10.3). The
oracle restates the spec's mapping rules in Python; it shares no code with the Go
parsers. Nothing here touches the kernel, mounts or Docker.
"""
import base64
import contextlib
import datetime
import gzip
import hashlib
import json
import os
import platform
import shutil
import sqlite3
import sys
import tempfile

import sqlite_gen

HERE = os.path.dirname(os.path.abspath(__file__))
SCHEMAS = os.path.join(HERE, "schemas", "android-telephony")
UTC = datetime.timezone.utc
EPOCH = datetime.datetime(1970, 1, 1, tzinfo=UTC)
I64_MAX = 2**63 - 1
SENTINELS = {0, -1, I64_MAX, -(2**63)}
MAX_SECONDS = 4102444800  # 2100-01-01T00:00:00Z, exclusive


class Raw:
    """Bytes stored as TEXT without any validation (CAST(x AS TEXT)): invalid UTF-8 and NUL survive."""

    def __init__(self, b):
        self.b = b


class Blob(bytes):
    """A BLOB cell as the engine reads it back (engine_view reads TEXT as plain bytes)."""


def classify(v, unit):
    """decode/ts restated: unit 'ms' or 's'. 'valid', 'sentinel' or 'invalid'."""
    if v in SENTINELS:
        return "sentinel"
    limit = MAX_SECONDS * (1000 if unit == "ms" else 1)
    return "valid" if 0 <= v < limit else "invalid"


def classify_sms_date(v):
    """Spec 9.1 / D6: a Unix-ms value below 1e11 is invalid, not guessed."""
    st = classify(v, "ms")
    return "invalid" if st == "valid" and v < 100_000_000_000 else st


def iso(v, unit):
    if unit == "ms":
        t = EPOCH + datetime.timedelta(milliseconds=v)
    else:
        t = EPOCH + datetime.timedelta(seconds=v)
    s = t.strftime("%Y-%m-%dT%H:%M:%S")
    if t.microsecond:
        s += (".%06d" % t.microsecond).rstrip("0")
    return s + "Z"


def utf8_rune(raw, i):
    """The valid UTF-8 character at raw[i:], or None (Go's utf8.DecodeRune: one invalid byte at a time)."""
    for n in (1, 2, 3, 4):
        try:
            ch = raw[i:i + n].decode("utf-8")
        except UnicodeDecodeError:
            continue
        if len(ch) == 1:
            return ch, n
    return None


class Cleaned:
    pass


def clean(raw, limit=256 << 10):
    """common.CleanText restated for bytes: invalid bytes and NUL become U+FFFD, flags, raw kept, cut at limit."""
    out, i, invalid, nul = [], 0, False, False
    while i < len(raw):
        r = utf8_rune(raw, i)
        if r is None:
            invalid = True
            out.append("\ufffd")
            i += 1
            continue
        ch, n = r
        if ch == "\x00":
            nul = True
            ch = "\ufffd"
        out.append(ch)
        i += n
    text = "".join(out)
    c = Cleaned()
    c.flags, c.truncated, c.total = [], False, 0
    enc = text.encode("utf-8")
    if len(enc) > limit:
        cut = limit
        while cut > 0 and (enc[cut] & 0xC0) == 0x80:
            cut -= 1
        text, c.truncated, c.total = enc[:cut].decode("utf-8"), True, len(raw)
    c.text, c.rawlen = text, len(raw)
    if invalid:
        c.flags.append("invalid_utf8")
    if nul:
        c.flags.append("nul")
    if c.truncated:
        c.flags.append("truncated")
    c.b64, c.sha = None, None
    if invalid or nul:
        if len(raw) <= 64 << 10:
            c.b64 = base64.b64encode(raw).decode()
        else:
            c.sha = hashlib.sha256(raw).hexdigest()
    return c


def addr_kind(a):
    """common.AddressKind restated for the shapes the fixtures use."""
    if a.count("@") == 1:
        local, dom = a.split("@")
        if local and "." in dom.strip(".") and not any(c.isspace() or ord(c) < 32 for c in a):
            return "email"
    digits = sum(c.isdigit() and c.isascii() for c in a)
    body = a[1:] if a.startswith("+") else a
    if body and all(c in "0123456789 -()." for c in body) and 7 <= digits <= 15:
        return "phone"
    if a.isascii() and a.isdigit() and 3 <= len(a) <= 6:
        return "shortcode"
    if 1 <= len(a) <= 11 and all(c.isascii() and (c.isalnum() or c == " ") for c in a) and any(c.isalpha() for c in a):
        return "alnum"
    return "unknown"


def mark(raw, field):
    """Lists field in raw.text_flags (sorted): its text was altered by cleaning."""
    raw["text_flags"] = sorted(set(raw.get("text_flags", [])) | {field})


def phases(sql_text):
    """Splits a DDL snapshot on '-- @phase N' lines; text before the first marker is phase 0."""
    out, cur = {0: []}, 0
    for line in sql_text.splitlines():
        s = line.strip()
        if s.startswith("-- @phase "):
            cur = int(s.split()[2])
            out.setdefault(cur, [])
            continue
        out[cur].append(line)
    return {k: "\n".join(v) for k, v in out.items()}


def load_ddl(name, schemas=SCHEMAS):
    with open(os.path.join(schemas, name), encoding="utf-8") as f:
        return phases(f.read())


def connect(path, page_size=4096, encoding="UTF-8", auto_vacuum=0):
    con = sqlite3.connect(path, isolation_level=None)
    con.execute("pragma page_size = %d" % page_size)
    con.execute("pragma encoding = '%s'" % encoding)
    con.execute("pragma auto_vacuum = %d" % auto_vacuum)
    con.execute("pragma secure_delete = OFF")  # some builds turn it ON by default
    return con


@contextlib.contextmanager
def batch(con):
    con.execute("begin")
    try:
        yield
    except BaseException:
        con.execute("rollback")
        raise
    con.execute("commit")


def columns(con, table):
    return [r[1] for r in con.execute("pragma table_info(%s)" % table)]


def insert(con, table, row):
    """Inserts the columns of row that the table has (a variant without a column drops it)."""
    cols = columns(con, table)
    keys = [k for k in row if k in cols]
    marks = ["cast(? as text)" if isinstance(row[k], Raw) else "?" for k in keys]
    vals = [row[k].b if isinstance(row[k], Raw) else row[k] for k in keys]
    con.execute("insert into %s(%s) values (%s)" % (table, ",".join(keys), ",".join(marks)), vals)


def lit(v):
    """A SQL literal, for statements a killed child process runs."""
    if v is None:
        return "NULL"
    if isinstance(v, int):
        return str(v)
    if isinstance(v, Raw):
        return "cast(x'%s' as text)" % v.b.hex()
    return "'" + str(v).replace("'", "''") + "'"


def read(path):
    with open(path, "rb") as f:
        return f.read()


def engine_view(files, tables):
    """The engine's view of finished files: copies them to a temporary directory (a WAL
    or hot journal is then applied to the COPY, never to the shipped bytes) and returns
    {table: [dict with 'rowid' and every column; TEXT as bytes]} in rowid order."""
    work = tempfile.mkdtemp(prefix="telephony-view-")
    try:
        path = os.path.join(work, "v.db")
        for role, suffix in (("db", ""), ("wal", "-wal"), ("journal", "-journal")):
            if role in files:
                with open(path + suffix, "wb") as f:
                    f.write(files[role])
        con = sqlite3.connect(path)
        con.text_factory = bytes
        out = {}
        for t in tables:
            try:
                cur = con.execute("select rowid, * from %s order by rowid" % t)
            except sqlite3.OperationalError:
                continue
            names = ["rowid"] + [d[0] for d in cur.description][1:]
            rows = [dict(zip(names, r)) for r in cur.fetchall()]
            kinds = con.execute("select %s from %s order by rowid" % (
                ",".join('typeof("%s")' % n for n in names[1:]), t)).fetchall()
            for row, kind in zip(rows, kinds):
                for n, k in zip(names[1:], kind):
                    if k == b"blob":  # text_factory=bytes also returns typeof() as bytes
                        row[n] = Blob(row[n])
            out[t] = rows
        con.close()
        return out
    finally:
        shutil.rmtree(work, ignore_errors=True)


def kill_child(path, statements):
    """Runs the statements in a child that dies with os._exit without closing: the WAL or
    journal it leaves is what a killed writer leaves (no copy of open files)."""
    sqlite_gen.run_child(path, statements)


def generator_block():
    return {"tool": "tools/fixtures/telephony_gen.py", "python": platform.python_version(),
            "sqlite": sqlite3.sqlite_version, "files": {}}


SUFFIX = {"db": ".db.gz", "wal": ".db-wal.gz", "journal": ".db-journal.gz"}


def write_outputs(out_dir, name, files, expect):
    """Writes <name>.{db,db-wal,db-journal}.gz (deterministic gzip) and <name>.expect.json;
    returns {role: sha256} for the Go pin table."""
    os.makedirs(out_dir, exist_ok=True)
    hashes = {role: hashlib.sha256(data).hexdigest() for role, data in files.items()}
    expect["generator"]["files"] = hashes
    for role, data in files.items():
        with open(os.path.join(out_dir, name + SUFFIX[role]), "wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0, compresslevel=9) as z:
                z.write(data)
    with open(os.path.join(out_dir, name + ".expect.json"), "w", encoding="utf-8", newline="\n") as f:
        json.dump(expect, f, ensure_ascii=False, sort_keys=True, separators=(",", ":"))  # compact: one line (ruling T49)
        f.write("\n")
    return hashes


def expect_header(family, variant, ddl):
    return {"generator": generator_block(), "family": family, "variant": variant, "ddl": ddl,
            "notes": {}, "records": [], "warnings": [], "extra_files": []}


def note_columns(con_cols, mapped, table, notes):
    """The notes rowrun writes: schema.<t> and unmapped_columns.<t>. mapped is [(name, required)] in
    the Go mapping's order; the rowid alias _id is never reported unmapped."""
    names = [n for n, _ in mapped]
    opt = [n for n, req in mapped if not req and n.lower() in [c.lower() for c in con_cols]]
    v = "columns:%d" % len(con_cols)
    if opt:
        v += ";optional:" + ",".join(opt)
    notes["schema." + table] = v
    un = [c for c in con_cols if c.lower() not in [n.lower() for n in names] and c != "_id"]
    if un:
        notes["unmapped_columns." + table] = ",".join(un)
    return notes


# ---------------------------------------------------------------- typed recovery oracle (plan 4G, ruling 4G-R14)
#
# The recover oracle states what the typed recovery pass must make of a fixture. It is computed from the
# COMMITTED files of the variant: sqlite_oracle.build_history (an independent page walker that shares no code
# with the Go reader) finds each version the scenario says should be history in the page images and gives its
# method, relation, basis, origin, confidence and cell; the scenario oracle (expect_sms, expect_pdu, ...) then
# states the typed fields of that version. The scenario's own record of what it wrote is the only other input.
# Nothing here reads Go output.

import sqlite_oracle as O  # noqa: E402


class Scenario:
    """What a variant's generator wrote and deleted in the claimed tables: every version it stored
    (written) and the versions that should be history (wants), as engine values."""

    def __init__(self, claimed, alias="_id", full=None):
        # full(table, rowid) -> bool: which wants get a full expected record in "records" (default all); every want
        # gets a digest of its canonical expected record in "digests" (the kit checks every emitted record with it)
        self.claimed, self.alias, self.full = tuple(claimed), alias, full
        self.columns, self.written, self.wants = {}, [], []

    def snap(self, con, table, where="1"):
        """The engine's rows of table as [(rowid, {column: value})]; also remembers the column order."""
        cols = columns(con, table)
        self.columns.setdefault(table, cols)
        return [(r[0], dict(zip(cols, r[1:]))) for r in con.execute("select rowid, * from %s where %s order by rowid" % (table, where))]

    def write(self, table, rows):
        """rows: [(rowid, dict)]: versions the scenario stored."""
        self.written += [(table, rowid, dict(d)) for rowid, d in rows]

    def want(self, table, rows, label=None):
        """rows should be found in the page images. label: they lie on freelist pages of a table the live schema
        may no longer own, so the oracle expects basis fit, attributed to label (the claimed table)."""
        for rowid, d in rows:
            w = dict(table=table, rowid=rowid, vals=self.vals(table, rowid, d))
            if label is not None:
                w.update(basis="fit", label=label)
            self.wants.append(w)

    def vals(self, table, rowid, d):
        return [None if c == self.alias else d.get(c) for c in self.columns[table]]


def record_digest(rec):
    """SHA-256 (hex) of the canonical JSON of the typed fields of an expected record: type, time, time_end, times,
    body and payload (the summary is derived from them and is not always stated). The Go kit builds the same text
    from an emitted record: sorted keys, no spaces, no HTML escaping, UTC times in RFC 3339."""
    obj = {"type": rec["type"], "time": rec["time"], "time_end": rec["time_end"], "times": rec["times"] or {},
           "body": rec["body"] or "", "payload": rec["payload"]}
    text = json.dumps(obj, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def dec(e):
    """Inverse of sqlite_oracle.enc."""
    t = e["t"]
    if t == "null":
        return None
    if t == "int":
        return int(e["v"])
    if t == "text":
        return e["v"]
    if t == "blob":
        return base64.b64decode(e["v"])
    raise ValueError(t)


def jv(v):
    """A stored value as JSON: null, number, string, or {"blob_b64": ...}."""
    if isinstance(v, (bytes, bytearray)):
        return {"blob_b64": base64.b64encode(bytes(v)).decode()}
    return v


def engine_bytes(v):
    """A python-sqlite3 (text as str) value as engine_view returns it (text as bytes)."""
    return v.encode("utf-8") if isinstance(v, str) else v


def recover_oracle(variant, files, scen, make_env, expect_fn):
    """The <variant>.recover.expect.json content."""
    st = O.State(files["db"], files.get("wal"), files.get("journal"), True, True)
    roots = {s["name"].lower(): s["root"] for s in O.schema_rows(st) if s["type"] == "table" and s["root"] > 0}
    live_raw = {}
    for name, root in roots.items():
        raw = live_raw[name] = {}

        def visit(c, raw=raw):
            if c["has_rowid"]:
                raw[c["rowid"]] = c["vals"]
        O.walk(st, root, visit)
    hist = O.build_history(variant, files, st, live_raw, scen.wants, 0)
    env = make_env(files)
    out = {"generator": {"tool": "tools/fixtures/telephony_gen.py --recover-oracle", "files": {}}, "variant": variant,
           "claimed": list(scen.claimed), "columns": scen.columns, "records": [], "digests": [], "unmapped": [], "written": []}
    for role, data in files.items():
        out["generator"]["files"][role] = hashlib.sha256(data).hexdigest()
    for h in hist:
        table = h["table"]
        if table.lower() not in scen.claimed:
            continue
        vals = [dec(v) for v in h["values"]]
        cols = scen.columns[table]
        d = {c: (h["rowid"] if c == scen.alias else (vals[i] if i < len(vals) else None)) for i, c in enumerate(cols)}
        d = {k: engine_bytes(v) for k, v in d.items()}
        d["rowid"] = h["rowid"]
        cell = h["cell"]
        rec = expect_fn(table, d, env, cell["file"])
        out["digests"].append({"table": table, "rowid": h["rowid"], "file": cell["file"], "page": cell["page"],
                               "offset": cell["offset"], "length": cell["length"], "digest": record_digest(rec)})
        if scen.full is None or scen.full(table, h["rowid"]):
            out["records"].append({
                "table": table, "rowid": h["rowid"], "method": h["method"], "relation": h["relation"], "basis": h["basis"],
                "origin": h["origin"], "confidence": h["confidence"], "notes": h["notes"], "file": cell["file"],
                "page": cell["page"], "offset": cell["offset"], "length": cell["length"], "record": rec})
    # Written: the versions the scenario stored (and the live ones at the end) that occur in some history page image.
    # A typed record may carry only such a version: anything else was invented or misread.
    # A history row of basis schema that equals a live row is dropped as a duplicate of live (relation absent from
    # the output), so live versions can only come back as records when the scenario has freelist rows, whose basis
    # is fit: a page freed by a rebalance holds a stale copy of a row that is still live.
    versions = {}
    for table, rowid, d in scen.written:
        versions.setdefault((table, rowid), []).append(scen.vals(table, rowid, d))
    live = engine_view(files, list(scen.claimed))
    livevals = {}
    for table, rows in live.items():
        for r in rows:
            vals = [None if c == scen.alias else (r[c].decode("utf-8") if type(r.get(c)) is bytes else r.get(c)) for c in scen.columns[table]]
            livevals[(table, r["rowid"])] = vals
    if any(w.get("basis") == "fit" for w in scen.wants):
        for key, vals in livevals.items():
            versions.setdefault(key, []).append(vals)
    else:
        for key, vl in list(versions.items()):
            versions[key] = [v for v in vl if not O.same(v, livevals.get(key, [object()]))]
    imgs, _ = O.collect_images(files, st)
    seen = set()
    for im in imgs:
        b = O.hbase(im["pgno"])
        if len(im["data"]) <= b + 8 or im["data"][b] != 0x0D:
            continue
        try:
            cells = O.leaf_cells(st, im["data"], im["pgno"], im["file"], im["off"])
        except ValueError:
            continue
        for c in cells:
            for (table, rowid), vl in versions.items():
                if rowid != c["rowid"]:
                    continue
                for vals in vl:
                    if O.same(c["vals"], vals) and (table, rowid, repr(vals)) not in seen:
                        seen.add((table, rowid, repr(vals)))
                        full = [rowid if col == scen.alias else v for col, v in zip(scen.columns[table], vals)]
                        out["written"].append({"table": table, "rowid": rowid, "values": [jv(v) for v in full]})
    out["written"].sort(key=lambda w: (w["table"], w["rowid"], json.dumps(w["values"], sort_keys=True)))
    return out


def write_recover_expect(out_dir, variant, obj):
    path = os.path.join(out_dir, variant + ".recover.expect.json")
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(obj, f, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        f.write("\n")
    return path

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


def load_ddl(name):
    with open(os.path.join(SCHEMAS, name), encoding="utf-8") as f:
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

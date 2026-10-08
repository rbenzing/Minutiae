"""Generator of the container-family SQLite fixtures (plan 3I, Task 14).

Run inside the fixtures image by tools/fixtures/sqlite.sh:

    python3 sqlite_gen.py <outdir> [--scenario NAME ...]

SCENARIOS is the registry sub-projects 4B and 4D extend: a scenario is a
function (workdir) -> dict(files, statements, wants, unreachable,
min_history). `files` holds the bytes of the database ("db") and optional
"wal" / "journal". The engine is the Debian libsqlite3 behind python's sqlite3
module. The WAL and journal fixtures are made by a child process that dies
with os._exit while the engine still holds its files open, so the leftovers
are what a killed writer leaves, with no copy of open files involved.

Determinism classes (see the README): the database-only scenarios are
byte-reproducible; a WAL or journal holds salts and nonces drawn from the
engine's PRNG, so those files differ on every run and their oracle is
regenerated with them (generator.file_sha256 binds the two).
"""
import os
import subprocess
import sys

import sqlite_oracle as oracle


def cte(n):
    return "with recursive c(i) as (select 1 union all select i+1 from c where i < %d) " % n


def lorem(i, n):
    return "substr(printf('%%08d', %s) || replace(hex(zeroblob(%d)), '00', 'abcdefghijklmnop'), 1, %d)" % (i, n // 16 + 1, n)


def lorem_py(i, n):
    return (("%08d" % i) + "abcdefghijklmnop" * (n // 16 + 1))[:n]


class Engine:
    def __init__(self, path):
        import sqlite3
        self.path = path
        self.con = sqlite3.connect(path, isolation_level=None)
        self.statements = []

    def x(self, q):
        self.statements.append(q)
        self.con.execute(q)

    def tx(self, *qs):
        self.statements.append("BEGIN")
        self.con.execute("BEGIN")
        for q in qs:
            self.statements.append(q)
            self.con.execute(q)
        self.statements.append("COMMIT")
        self.con.execute("COMMIT")

    def close(self):
        self.con.close()


def read(path):
    with open(path, "rb") as f:
        return f.read()


def db_only(e, path):
    e.close()
    return {"db": read(path)}


def scen_freelist(workdir):
    path = os.path.join(workdir, "f.db")
    e = Engine(path)
    e.x("pragma secure_delete = OFF")  # the Debian build turns it ON by default
    e.x("pragma user_version = 3")
    e.x("create table people(id integer primary key, name text, age integer, email text not null)")
    e.x(cte(40) + "insert into people select i, 'resident-' || i, 20 + i % 50, 'r' || i || '@example.test' from c")
    e.x("create table events(ts integer, kind text)")
    e.x(cte(20) + "insert into events select 1700000000 + i * 60, 'kind-' || (i % 4) from c")
    e.x("create table decoy(a text)")
    e.x(cte(5) + "insert into decoy select 'decoy-' || i from c")
    e.x("create table d_fit(id integer primary key, name text, age integer, email text not null)")
    e.x(cte(12) + "insert into d_fit select 100 + i, 'fitted-old-' || i, 30 + i, 'f' || i || '@old.test' from c")
    e.x("create table d_guess(id integer primary key, name text, age integer)")
    e.x(cte(12) + "insert into d_guess select 200 + i, 'guessed-old-' || i, 40 + i from c")
    e.x("create table d_none(a integer, b integer, c text, d text, e blob)")
    e.x(cte(12) + "insert into d_none select 300 + i, i * i, 'none-old-' || i, 'second-' || i, x'cafe' from c")
    e.x("delete from people where id between 10 and 15")
    for t in ("decoy", "d_fit", "d_guess", "d_none"):
        e.x("drop table " + t)
    files = db_only(e, path)
    wants = []
    for i in range(1, 13):
        wants.append(dict(table="d_fit", rowid=100 + i, vals=[None, "fitted-old-%d" % i, 30 + i, "f%d@old.test" % i], basis="fit", label="people", required=True))
        wants.append(dict(table="d_guess", rowid=200 + i, vals=[None, "guessed-old-%d" % i, 40 + i], basis="guess", label="people", required=True))
        wants.append(dict(table="d_none", rowid=None, vals=[300 + i, i * i, "none-old-%d" % i, "second-%d" % i, b"\xca\xfe"], basis="none", required=True))
    unreach = [dict(table="people", rowid=i, values=[oracle.enc(None), oracle.enc("resident-%d" % i), oracle.enc(20 + i % 50),
                                                       oracle.enc("r%d@example.test" % i)], marker="") for i in range(10, 16)]
    return dict(files=files, statements=e.statements, wants=wants, unreachable=unreach, min_history=36)


def scen_utf16(workdir):
    path = os.path.join(workdir, "u.db")
    e = Engine(path)
    e.x("pragma page_size = 1024")
    e.x("pragma encoding = 'UTF-16le'")
    e.x("create table docs(id integer primary key, title text, body text, n integer)")
    e.x(cte(200) + "insert into docs select i, case i % 5 when 0 then 'Zürich' when 1 then '東京' "
        "when 2 then 'Привет' when 3 then 'café' else '\U0001F600 smile' end || ' ' || i, "
        "'Ünïcödé body ' || i, i * 3 from c")
    e.x("create index idx_docs_title on docs(title)")
    return dict(files=db_only(e, path), statements=e.statements, wants=[], unreachable=[], min_history=0)


CHILD_WAL = r'''
import os, sqlite3, sys
con = sqlite3.connect(sys.argv[1], isolation_level=None)
for q in sys.argv[2:]:
    if q == "--die":
        os._exit(0)
    con.execute(q)
'''


def run_child(path, queries):
    """Runs the statements in a child that dies (os._exit) after the last one,
    without closing the connection: its WAL or journal stays as it was."""
    subprocess.run([sys.executable, "-c", CHILD_WAL, path] + queries + ["--die"], check=True)


def scen_wal_killed(workdir):
    path = os.path.join(workdir, "w.db")
    e = Engine(path)
    e.x("create table kv(k integer primary key, v text, n integer)")
    e.x(cte(300) + "insert into kv select i, 'init-' || i || '-' || " + lorem("i", 30) + ", i from c")
    e.x("pragma journal_mode = wal")
    e.close()
    stmts = list(e.statements)
    child = ["pragma wal_autocheckpoint = 0",
             "update kv set v = 'a1-' || k || '-' || %s where k between 1 and 300" % lorem("k", 30),
             "update kv set v = 'a2-' || k || '-' || %s where k between 100 and 200" % lorem("k", 30),
             "delete from kv where k between 250 and 255",
             "insert into kv values (1001, 'new-1001', 1)"]
    run_child(path, child)
    stmts += ["-- a child process runs the following and dies with os._exit (no close, no checkpoint):"] + child
    wal = read(path + "-wal")
    db = read(path)
    wants = []
    for k in range(1, 301):
        wants.append(dict(table="kv", rowid=k, vals=[None, "init-%d-%s" % (k, lorem_py(k, 30)), k]))
        wants.append(dict(table="kv", rowid=k, vals=[None, "a1-%d-%s" % (k, lorem_py(k, 30)), k]))
    for k in range(100, 201):
        wants.append(dict(table="kv", rowid=k, vals=[None, "a2-%d-%s" % (k, lorem_py(k, 30)), k]))
    return dict(files=dict(db=db, wal=wal), statements=stmts, wants=wants, unreachable=[], min_history=20)


def scen_hot_killed(workdir):
    path = os.path.join(workdir, "h.db")
    e = Engine(path)
    e.x("pragma journal_mode = delete")
    e.x("create table a(id integer primary key, v text)")
    e.x(cte(1500) + "insert into a select i, " + lorem("i", 120) + " from c")
    e.close()
    stmts = list(e.statements)
    child = ["pragma cache_size = 10", "pragma cache_spill = 10", "BEGIN",
             "update a set v = 'interrupted-' || id || substr(v, 18) where id % 7 = 0"]
    run_child(path, child)
    stmts += ["-- a child process runs the following and dies with os._exit in the middle of the transaction:"] + child
    db, journal = read(path), read(path + "-journal")
    wants = [dict(table="a", rowid=i, vals=[None, "interrupted-%d" % i + lorem_py(i, 120)[17:]]) for i in range(7, 1501, 7)]
    return dict(files=dict(db=db, journal=journal), statements=stmts, wants=wants, unreachable=[], min_history=20)


SCENARIOS = {
    "sqlite3-freelist": ("C", scen_freelist),
    "sqlite3-utf16": ("C", scen_utf16),
    "sqlite3-wal-killed": ("C", scen_wal_killed),
    "sqlite3-hot-journal-killed": ("C", scen_hot_killed),
}


def generate(name, workdir):
    cls, fn = SCENARIOS[name]
    os.makedirs(workdir, exist_ok=True)
    s = fn(workdir)
    exp = oracle.build_expect(name, cls, s["statements"], s["files"], s["wants"], s["unreachable"], s["min_history"])
    return s["files"], exp


def main(argv):
    import json
    import shutil
    import tempfile
    out = argv[1]
    names = argv[argv.index("--scenario") + 1:] if "--scenario" in argv else list(SCENARIOS)
    for name in names:
        work = tempfile.mkdtemp(prefix="sqlite-gen-")
        try:
            files, exp = generate(name, work)
        finally:
            shutil.rmtree(work, ignore_errors=True)
        suffixes = {"db": ".db", "wal": ".db-wal", "journal": ".db-journal"}
        for k, data in files.items():
            oracle.write_gz(os.path.join(out, name + suffixes[k] + ".gz"), data)
        with open(os.path.join(out, name + ".expect.json"), "w", encoding="utf-8", newline="\n") as f:
            json.dump(exp, f, indent=1, ensure_ascii=False)
            f.write("\n")
        print("wrote", name, sorted(files))


if __name__ == "__main__":
    main(sys.argv)

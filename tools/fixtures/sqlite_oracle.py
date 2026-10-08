"""Independent oracle for the SQLite fixtures (plan 3I, Task 14).

It describes a database (plus optional WAL and rollback journal) the way the
Go fixture tests expect (internal/sqlitefile/fixtures_test.go, type fxExpect):

  * the ENGINE (the Debian libsqlite3 behind python's sqlite3 module, and the
    sqlite3 shell for .dbinfo and dbstat) answers for schema, live rows and
    pragmas, on a COPY of the files (the engine recovers a journal and replays
    a WAL on open);
  * this file's own minimal walker answers for bytes: header, cells, WAL
    frames, journal records, the freelist and the page images of the history.

It shares no code with internal/sqlitefile and is written from the file
format description. It is used only on files the generator wrote itself, so
it is strict and small, not a general reader.
"""
import base64
import hashlib
import json
import os
import shutil
import sqlite3
import struct
import subprocess
import tempfile


# ---- typed values -------------------------------------------------------

def enc(v):
    if v is None:
        return {"t": "null"}
    if isinstance(v, bool):
        v = int(v)
    if isinstance(v, int):
        return {"t": "int", "v": str(v)}
    if isinstance(v, float):
        return {"t": "float", "v": format(struct.unpack(">Q", struct.pack(">d", v))[0], "x")}
    if isinstance(v, str):
        return {"t": "text", "v": v}
    if isinstance(v, (bytes, bytearray)):
        return {"t": "blob", "v": base64.b64encode(bytes(v)).decode()}
    raise TypeError(type(v))


def same(a, b):
    if len(a) != len(b):
        return False
    for x, y in zip(a, b):
        if type(x) is not type(y) and not (isinstance(x, (bytes, bytearray)) and isinstance(y, (bytes, bytearray))):
            return False
        if x != y:
            return False
    return True


def be32(b, o=0):
    return struct.unpack_from(">I", b, o)[0]


def be16(b, o=0):
    return struct.unpack_from(">H", b, o)[0]


def varint(b, o=0):
    v = 0
    for i in range(8):
        if o + i >= len(b):
            return 0, 0
        c = b[o + i]
        v = (v << 7) | (c & 0x7F)
        if not c & 0x80:
            return v, i + 1
    if o + 8 >= len(b):
        return 0, 0
    return (v << 8) | b[o + 8], 9


# ---- WAL ----------------------------------------------------------------

def wal_sum(data, big, s0, s1):
    fmt = ">II" if big else "<II"
    for i in range(0, len(data) - 7, 8):
        x0, x1 = struct.unpack_from(fmt, data, i)
        s0 = (s0 + x0 + s1) & 0xFFFFFFFF
        s1 = (s1 + x1 + s0) & 0xFFFFFFFF
    return s0, s1


class Wal:
    def __init__(self, wal):
        self.valid = False
        self.big = False
        self.frames = []
        self.gens = []
        self.trailing = 0
        self.valid_len = 0
        self.last_commit = 0
        if wal is None or len(wal) < 32:
            return
        magic = be32(wal)
        if magic not in (0x377F0682, 0x377F0683):
            return
        self.big = magic == 0x377F0683
        self.ps = be32(wal, 8)
        self.ckpt, self.salt1, self.salt2 = be32(wal, 12), be32(wal, 16), be32(wal, 20)
        c0, c1 = wal_sum(wal[:24], self.big, 0, 0)
        if (c0, c1) != (be32(wal, 24), be32(wal, 28)) or be32(wal, 4) != 3007000 or \
                self.ps < 512 or self.ps > 65536 or self.ps & (self.ps - 1):
            return
        self.valid = True
        slot = 24 + self.ps
        n = (len(wal) - 32) // slot
        self.trailing = len(wal) - 32 - n * slot
        prev = (be32(wal, 24), be32(wal, 28))
        chain = True
        brokenseen = False
        for i in range(n):
            off = 32 + i * slot
            f = wal[off:off + slot]
            fr = dict(slot=i + 1, page=be32(f), dbsize=be32(f, 4), salt1=be32(f, 8), salt2=be32(f, 12),
                      check1=be32(f, 16), check2=be32(f, 20), off=off, state="", linked=False, gen=0)
            a, b = wal_sum(f[:8], self.big, *prev)
            a, b = wal_sum(f[24:], self.big, a, b)
            ok = (a, b) == (fr["check1"], fr["check2"]) and fr["page"] != 0
            same_salt = (fr["salt1"], fr["salt2"]) == (self.salt1, self.salt2)
            if chain and same_salt and ok:
                fr["state"] = "valid"
            elif chain:
                chain = False
                fr["state"] = "broken" if same_salt else "stale"
                brokenseen = True
            elif not same_salt:
                fr["state"] = "stale"
            else:
                fr["state"] = "detached" if brokenseen else "detached"
            prev = (fr["check1"], fr["check2"])
            self.frames.append(fr)
        for fr in self.frames:
            if fr["state"] == "valid":
                self.valid_len += 1
                if fr["dbsize"]:
                    self.last_commit = fr["slot"]
        for fr in self.frames:
            if fr["state"] == "valid":
                fr["state"] = "committed" if fr["slot"] <= self.last_commit else "uncommitted"
        p = (be32(wal, 24), be32(wal, 28))
        for i, fr in enumerate(self.frames):
            off = fr["off"]
            f = wal[off:off + slot]
            a, b = wal_sum(f[:8], self.big, *p)
            a, b = wal_sum(f[24:], self.big, a, b)
            fr["linked"] = (a, b) == (fr["check1"], fr["check2"])
            p = (fr["check1"], fr["check2"])
        for i, fr in enumerate(self.frames):
            if i > 0:
                pf = self.frames[i - 1]
                if (pf["salt1"], pf["salt2"]) == (fr["salt1"], fr["salt2"]) and fr["linked"]:
                    fr["gen"] = pf["gen"]
                    g = self.gens[fr["gen"]]
                    g["slots"] += 1
                    if fr["dbsize"]:
                        g["commits"] += 1
                    continue
            self.gens.append(dict(salt1=fr["salt1"], salt2=fr["salt2"], first_slot=fr["slot"], slots=1,
                                  commits=1 if fr["dbsize"] else 0))
            fr["gen"] = len(self.gens) - 1
        for g in self.gens:
            g["age"] = (self.salt1 - g["salt1"]) & 0xFFFFFFFF
            f1 = self.frames[g["first_slot"] - 1]
            off = f1["off"]
            f = wal[off:off + slot]
            a, b = wal_sum(f[:8], self.big, be32(wal, 24), be32(wal, 28))
            a, b = wal_sum(f[24:], self.big, a, b)
            g["anchored"] = (g["salt1"], g["salt2"]) == (self.salt1, self.salt2) and g["first_slot"] == 1 \
                and (a, b) == (f1["check1"], f1["check2"])

    def expect(self):
        e = dict(header_valid=self.valid, big_endian=self.big, page_size=0, checkpoint_seq=0, salt1=0, salt2=0,
                 frame_slots=0, trailing_bytes=0, frames_valid=0, frames_committed=0, last_commit=0, commits=0,
                 frames_uncommitted=0, frames_broken=0, frames_detached=0, frames_stale=0,
                 db_pages_after_commit=0, max_page_number=0, frames=[], generations=[])
        if not self.valid:
            return e
        e.update(page_size=self.ps, checkpoint_seq=self.ckpt, salt1=self.salt1, salt2=self.salt2,
                 frame_slots=len(self.frames), trailing_bytes=self.trailing, frames_valid=self.valid_len,
                 last_commit=self.last_commit)
        for f in self.frames:
            st = f["state"]
            if st == "committed":
                e["frames_committed"] += 1
                if f["dbsize"]:
                    e["commits"] += 1
                e["max_page_number"] = max(e["max_page_number"], f["page"])
            elif st == "uncommitted":
                e["frames_uncommitted"] += 1
            elif st == "broken":
                e["frames_broken"] += 1
            elif st == "detached":
                e["frames_detached"] += 1
            elif st == "stale":
                e["frames_stale"] += 1
            if f["slot"] == self.last_commit:
                e["db_pages_after_commit"] = f["dbsize"]
            e["frames"].append(dict(slot=f["slot"], page=f["page"], db_size=f["dbsize"], salt1=f["salt1"],
                                    salt2=f["salt2"], check1=f["check1"], check2=f["check2"], state=st,
                                    linked=f["linked"], generation=f["gen"], offset=f["off"]))
        for g in self.gens:
            e["generations"].append(dict(salt1=g["salt1"], salt2=g["salt2"], first_slot=g["first_slot"],
                                         slots=g["slots"], anchored=g["anchored"], age=g["age"], commits=g["commits"]))
        return e


# ---- journal ------------------------------------------------------------

JMAGIC = bytes([0xD9, 0xD5, 0x05, 0xF9, 0x20, 0xA1, 0x63, 0xD7])


class Journal:
    def __init__(self, j, dbps):
        self.present = bool(j)
        self.hot = self.header_valid = self.zeroed = False
        self.ps = self.sector = self.initial = self.nonce = 0
        self.segs = []
        self.recs = []
        self.winner = {}
        if j is None or len(j) < 512:
            return
        if j[:28] == bytes(28):
            self.zeroed = True
            return
        if j[:8] != JMAGIC:
            return
        self.header_valid = self.hot = True
        self.sector, self.ps = be32(j, 20), be32(j, 24)
        if self.ps == 0:
            self.ps = dbps
        self.initial, self.nonce = be32(j, 16), be32(j, 12)
        ps, sector = self.ps, self.sector
        step = 4 + ps + 4
        off = seg = 0
        while off + sector <= len(j) and j[off:off + 8] == JMAGIC:
            n = be32(j, off + 8)
            nonce = be32(j, off + 12)
            p = off + sector
            cnt = 0
            start = len(self.recs)
            while (n == 0xFFFFFFFF or cnt < n) and p + step <= len(j):
                pg = be32(j, p)
                data = j[p + 4:p + 4 + ps]
                s = nonce
                i = ps - 200
                while i > 0:
                    s = (s + data[i]) & 0xFFFFFFFF
                    i -= 200
                self.recs.append(dict(index=len(self.recs), segment=seg, page=pg, off=p + 4,
                                      ok=s == be32(j, p + 4 + ps), applied=False))
                p += step
                cnt += 1
            self.segs.append(dict(offset=off, declared_records=n, records=len(self.recs) - start))
            off = (p + sector - 1) // sector * sector
            seg += 1
        for i, r in enumerate(self.recs):
            if r["ok"] and r["page"] != 0:
                if r["page"] in self.winner:
                    self.recs[self.winner[r["page"]]]["applied"] = False
                self.winner[r["page"]] = i
                r["applied"] = True

    def expect(self):
        e = dict(hot=self.hot, header_valid=self.header_valid, zeroed_header=self.zeroed, page_size=self.ps,
                 sector_size=self.sector, initial_pages=self.initial, nonce=self.nonce, applied=self.hot,
                 not_applied_reason="", records_total=0, records_valid=0, applied_records=0, segments=[], records=[])
        if self.zeroed:
            e["not_applied_reason"] = "zeroed-header"
            e["segments"] = None
            e["records"] = None
            return e
        if not self.hot:
            e["not_applied_reason"] = "header-invalid"
        for s in self.segs:
            e["segments"].append(s)
        for r in self.recs:
            e["records_total"] += 1
            e["records_valid"] += 1 if r["ok"] else 0
            e["applied_records"] += 1 if r["applied"] else 0
            e["records"].append(dict(index=r["index"], segment=r["segment"], page=r["page"], offset=r["off"],
                                     checksum_ok=r["ok"], applied=r["applied"]))
        return e


# ---- page source and cells ----------------------------------------------

class State:
    def __init__(self, db, wal, journal, use_wal, use_journal):
        self.db, self.walbytes, self.jbytes = db, wal, journal
        self.ps = be16(db, 16)
        if self.ps == 1:
            self.ps = 65536
        self.wal = Wal(wal) if use_wal and wal is not None else None
        self.use_wal = bool(self.wal and self.wal.valid)
        self.latest = {}
        if self.use_wal:
            for f in self.wal.frames:
                if f["state"] == "committed":
                    self.latest[f["page"]] = f["slot"]
        self.jr = Journal(journal, self.ps) if use_journal and journal is not None else None
        self.use_journal = bool(self.jr and self.jr.hot)

    def get(self, n):
        """-> (data, file, fileoffset) or None"""
        if n == 0:
            return None
        if self.use_journal:
            if n > self.jr.initial:
                return None
            if n in self.jr.winner:
                r = self.jr.recs[self.jr.winner[n]]
                return self.jbytes[r["off"]:r["off"] + self.ps], "journal", r["off"]
        if self.use_wal and n in self.latest:
            f = self.wal.frames[self.latest[n] - 1]
            o = f["off"] + 24
            return self.walbytes[o:o + self.ps], "wal", o
        o = (n - 1) * self.ps
        if o + self.ps > len(self.db):
            return None
        return self.db[o:o + self.ps], "db", o

    def usable(self):
        return self.ps - self.db[20]

    def encoding(self):
        return be32(self.db, 56)


def local_size(usable, leaf_table, p):
    x = usable - 35 if leaf_table else (usable - 12) * 64 // 255 - 23
    if p <= x:
        return p, False
    m = (usable - 12) * 32 // 255 - 23
    k = m + (p - m) % (usable - 4)
    return (k, True) if k <= x else (m, True)


def decode_record(b, encoding):
    hl, n = varint(b)
    if n == 0 or hl > len(b):
        raise ValueError("record header")
    serials = []
    p = n
    while p < hl:
        v, k = varint(b, p)
        if k == 0:
            raise ValueError("serial")
        serials.append(v)
        p += k
    p = hl
    out = []
    sizes = {1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}
    for st in serials:
        if st == 0:
            out.append(None)
        elif st in sizes:
            x = b[p:p + sizes[st]]
            if len(x) != sizes[st]:
                raise ValueError("short")
            out.append(int.from_bytes(x, "big", signed=True))
            p += sizes[st]
        elif st == 7:
            out.append(struct.unpack_from(">d", b, p)[0])
            p += 8
        elif st == 8:
            out.append(0)
        elif st == 9:
            out.append(1)
        elif st >= 12 and st % 2 == 0:
            k = (st - 12) // 2
            out.append(bytes(b[p:p + k]))
            p += k
        elif st >= 13:
            k = (st - 13) // 2
            raw = bytes(b[p:p + k])
            p += k
            if encoding == 1:
                out.append(raw.decode("utf-8"))
            else:
                out.append(raw.decode("utf-16-le" if encoding == 2 else "utf-16-be"))
        else:
            raise ValueError("reserved serial")
        if p > len(b):
            raise ValueError("record body short")
    return out


def hbase(pgno):
    return 100 if pgno == 1 else 0


def cell_at(st, page, pgno, file, pageoff, typ, co):
    p = co + (4 if typ in (0x02, 0x05) else 0)
    c = dict(page=pgno, file=file, rowid=0, has_rowid=False)
    if typ == 0x0D:
        payload, n = varint(page, p)
        p += n
        rid, n = varint(page, p)
        p += n
        c["rowid"], c["has_rowid"] = rid - (1 << 64) if rid >= 1 << 63 else rid, True
    elif typ in (0x0A, 0x02):
        payload, n = varint(page, p)
        p += n
    else:
        raise ValueError("page type %#x" % typ)
    local, spills = local_size(st.usable(), typ == 0x0D, payload)
    if p + local > len(page) or (spills and p + local + 4 > len(page)):
        raise ValueError("cell past page")
    buf = bytearray(page[p:p + local])
    end = p + local
    if spills:
        nxt = be32(page, end)
        end += 4
        while nxt and len(buf) < payload:
            op = st.get(nxt)
            if op is None:
                raise ValueError("overflow unavailable")
            take = min(payload - len(buf), st.usable() - 4)
            buf += op[0][4:4 + take]
            nxt = be32(op[0])
    c["vals"] = decode_record(bytes(buf), st.encoding())
    c["off"] = pageoff + co
    c["length"] = end - co
    c["hex"] = bytes(page[co:end]).hex()
    return c


def leaf_cells(st, page, pgno, file, pageoff):
    b = hbase(pgno)
    typ = page[b]
    if typ not in (0x0D, 0x0A):
        raise ValueError("not a leaf")
    n = be16(page, b + 3)
    return [cell_at(st, page, pgno, file, pageoff, typ, be16(page, b + 8 + 2 * i)) for i in range(n)]


def walk(st, pgno, visit, depth=0):
    if depth > 20:
        raise ValueError("deep")
    g = st.get(pgno)
    if g is None:
        raise ValueError("page %d unavailable" % pgno)
    page, file, off = g
    b = hbase(pgno)
    typ = page[b]
    n = be16(page, b + 3)
    if typ in (0x0D, 0x0A):
        for c in leaf_cells(st, page, pgno, file, off):
            visit(c)
    elif typ in (0x05, 0x02):
        for i in range(n):
            co = be16(page, b + 12 + 2 * i)
            walk(st, be32(page, co), visit, depth + 1)
            if typ == 0x02:
                visit(cell_at(st, page, pgno, file, off, typ, co))
        walk(st, be32(page, b + 8), visit, depth + 1)
    else:
        raise ValueError("page %d type %#x" % (pgno, typ))


def freelist_of(st):
    p1 = st.get(1)
    trunks, leaves = [], []
    if p1 is None:
        return trunks, leaves
    t = be32(p1[0], 32)
    seen = set()
    while t and t not in seen:
        seen.add(t)
        g = st.get(t)
        if g is None:
            break
        trunks.append(t)
        n = be32(g[0], 4)
        for i in range(n):
            if 8 + 4 * i + 4 <= len(g[0]):
                leaves.append(be32(g[0], 8 + 4 * i))
        t = be32(g[0])
    return trunks, leaves


def schema_rows(st):
    out = []

    def v(c):
        if len(c["vals"]) >= 5:
            r = c["vals"]
            out.append(dict(type=r[0], name=r[1], tbl=r[2], root=r[3] if isinstance(r[3], int) else 0, sql=r[4] or ""))
    walk(st, 1, v)
    return out


def header(db):
    h = {}
    h["page_size"] = be16(db, 16) if be16(db, 16) != 1 else 65536
    h["reserved"] = db[20]
    h["encoding"] = {1: "UTF-8", 2: "UTF-16le", 3: "UTF-16be"}.get(be32(db, 56), "encoding(%d)" % be32(db, 56))
    h["header_pages"] = be32(db, 28)
    cc = be32(db, 24)
    h["header_pages_valid"] = h["header_pages"] != 0 and cc == be32(db, 92)
    h["file_pages"] = (len(db) + h["page_size"] - 1) // h["page_size"]
    h["change_counter"] = cc
    h["version_valid_for"] = be32(db, 92)
    h["sqlite_version"] = be32(db, 96)
    h["schema_cookie"] = be32(db, 40)
    h["schema_format"] = be32(db, 44)
    h["freelist_trunk"] = be32(db, 32)
    h["freelist_count"] = be32(db, 36)
    lr = be32(db, 52)
    h["largest_root"] = lr
    h["auto_vacuum"] = 0 if lr == 0 else (2 if be32(db, 64) else 1)
    h["user_version"] = struct.unpack(">i", db[60:64])[0]
    h["application_id"] = be32(db, 68)
    h["write_version"], h["read_version"] = db[18], db[19]
    return h


# ---- the engine -----------------------------------------------------------

def engine_answers(files, tool_dir=None):
    d = tempfile.mkdtemp(prefix="sqlite-oracle-")
    try:
        path = os.path.join(d, "o.db")
        for suffix, data in (("", files["db"]), ("-wal", files.get("wal")), ("-journal", files.get("journal"))):
            if data is not None:
                with open(path + suffix, "wb") as f:
                    f.write(data)
        con = sqlite3.connect(path, isolation_level=None)
        a = {}
        a["version"] = con.execute("select sqlite_version()").fetchone()[0]
        a["schema"] = [dict(type=r[0], name=r[1], tbl_name=r[2], rootpage=r[3], sql=r[4])
                       for r in con.execute("select type, name, tbl_name, rootpage, coalesce(sql, '') from sqlite_schema order by rowid")]
        names = [r[0] for r in con.execute("select name from sqlite_schema where type = 'table' and sql not like 'create virtual%' and rootpage > 0")]
        a["rows"], a["wr"] = {}, {}
        for n in sorted(names):
            q = '"' + n.replace('"', '""') + '"'
            cols = ['"%s"' % r[1].replace('"', '""') for r in con.execute("pragma table_xinfo(%s)" % q) if r[6] in (0, 3)]
            sql = [s for s in a["schema"] if s["name"] == n and s["type"] == "table"][0]["sql"].lower()
            wr = "without rowid" in sql
            a["wr"][n.lower()] = wr
            if not cols:
                a["rows"][n.lower()] = []
                continue
            if wr:
                cur = con.execute("select %s from %s not indexed" % (", ".join(cols), q))
            else:
                cur = con.execute("select rowid, %s from %s not indexed order by rowid" % (", ".join(cols), q))
            a["rows"][n.lower()] = [list(r) for r in cur]
        a["info"] = dict(integrity_check=con.execute("pragma integrity_check").fetchone()[0],
                         freelist_count=con.execute("pragma freelist_count").fetchone()[0],
                         page_count=con.execute("pragma page_count").fetchone()[0])
        con.close()
        # the shell's own reports, on a fresh copy (the engine above may have changed the first)
        shutil.rmtree(d)
        os.mkdir(d)
        for suffix, data in (("", files["db"]), ("-wal", files.get("wal")), ("-journal", files.get("journal"))):
            if data is not None:
                with open(path + suffix, "wb") as f:
                    f.write(data)
        a["dbinfo"] = subprocess.run(["sqlite3", path, ".dbinfo"], capture_output=True, text=True).stdout
        a["dbstat"] = subprocess.run(["sqlite3", path, "select name, path, pageno, pagetype, ncell, payload, unused, mx_payload from dbstat order by path"],
                                     capture_output=True, text=True).stdout
        return a
    finally:
        shutil.rmtree(d, ignore_errors=True)


# ---- history ------------------------------------------------------------

METHOD_BASE = {"sqlite-wal-prior": 75, "sqlite-journal-before": 70, "sqlite-freelist": 60, "sqlite-wal-stale": 55,
               "sqlite-wal-uncommitted": 45, "sqlite-journal-rolledback": 45, "sqlite-journal-persist": 35}
BASIS_CAP = {"schema": 100, "fit": 60, "guess": 40, "none": 30}


def method_of(origin, zeroed):
    return {"db-under-wal": "sqlite-wal-prior", "wal-superseded": "sqlite-wal-prior",
            "wal-uncommitted": "sqlite-wal-uncommitted", "wal-stale": "sqlite-wal-stale",
            "wal-unverified": "sqlite-wal-stale", "db-rolled-back": "sqlite-journal-rolledback",
            "journal-before": "sqlite-journal-persist" if zeroed else "sqlite-journal-before",
            "freelist-leaf": "sqlite-freelist"}[origin]


def persist_records(j, ps, dbpages):
    best = []
    for sector in (512, 1024, 2048, 4096):
        imgs = []
        p = sector
        while p + 4 + ps + 4 <= len(j):
            pg = be32(j, p)
            data = j[p + 4:p + 4 + ps]
            b = hbase(pg)
            if pg == 0 or pg > dbpages + 16 or len(data) <= b or data[b] not in (0x0D, 0x05, 0x0A, 0x02):
                break
            imgs.append(dict(pgno=pg, file="journal", off=p + 4, data=data, origin="journal-before"))
            p += 4 + ps + 4
        if len(imgs) > len(best):
            best = imgs
    return best


def collect_images(files, st):
    ps = st.ps
    db = files["db"]
    dbpages = len(db) // ps
    imgs = []
    zeroed = False

    def dbimg(p, origin):
        o = (p - 1) * ps
        if p >= 1 and o + ps <= len(db):
            imgs.append(dict(pgno=p, file="db", off=o, data=db[o:o + ps], origin=origin))
    if st.wal is not None and st.wal.valid:
        w = st.wal
        per = {}
        for i, f in enumerate(w.frames):
            if f["state"] == "committed":
                per.setdefault(f["page"], []).append(i)
        for p in sorted(per):
            dbimg(p, "db-under-wal")
        for i, f in enumerate(w.frames):
            s = f["state"]
            if s == "committed":
                if per[f["page"]][-1] == i:
                    continue
                origin = "wal-superseded"
            elif s == "uncommitted":
                origin = "wal-uncommitted"
            elif s == "stale":
                origin = "wal-stale"
            else:
                origin = "wal-unverified"
            if f["page"] == 0:
                continue
            o = f["off"] + 24
            imgs.append(dict(pgno=f["page"], file="wal", off=o, data=files["wal"][o:o + ps], origin=origin))
    if st.jr is not None:
        j = st.jr
        if j.hot:
            for r in j.recs:
                if r["ok"] and r["page"] != 0:
                    imgs.append(dict(pgno=r["page"], file="journal", off=r["off"],
                                     data=files["journal"][r["off"]:r["off"] + ps], origin="journal-before"))
            for p in sorted(j.winner):
                if p <= j.initial:
                    dbimg(p, "db-rolled-back")
        elif j.zeroed:
            zeroed = True
            imgs += persist_records(files["journal"], ps, dbpages)
    if files.get("wal") is None and files.get("journal") is None:
        _, leaves = freelist_of(st)
        for p in leaves:
            dbimg(p, "freelist-leaf")
    return imgs, zeroed


def norm(v):
    return v


def build_history(name, files, st, live_raw, wants, min_history):
    out = []
    if not wants:
        return out
    imgs, zeroed = collect_images(files, st)
    found = [0] * len(wants)
    for im in imgs:
        b = hbase(im["pgno"])
        if len(im["data"]) <= b + 8 or im["data"][b] != 0x0D:
            continue
        try:
            cells = leaf_cells(st, im["data"], im["pgno"], im["file"], im["off"])
        except ValueError:
            continue
        for c in cells:
            for wi, w in enumerate(wants):
                if w.get("rowid") is not None and w["rowid"] != c["rowid"]:
                    continue
                if not same(c["vals"], w["vals"]):
                    continue
                basis = w.get("basis") or "schema"
                extra = []
                if basis == "schema" and im["origin"] in ("wal-stale", "wal-unverified"):
                    basis = "fit"   # no as-of schema for a stale generation (rulings C44/C46)
                    extra = ["owner-changed"]
                method = method_of(im["origin"], zeroed)
                table = w["table"] if not w.get("basis") else w.get("label", "")
                if basis == "none":
                    table = ""
                rel = "unknown"
                unc = im["origin"] in ("wal-uncommitted", "db-rolled-back")
                if unc and basis == "schema":
                    rel = "uncommitted"   # a fit or guess row stays unknown (ruling C47)
                if basis == "schema":
                    lr = live_raw.get(w["table"].lower(), {}).get(c["rowid"])
                    if lr is not None and same(lr, c["vals"]):
                        continue
                    if not unc:
                        rel = "absent-from-live" if lr is None else "superseded-version"
                notes = []
                if basis in ("fit", "guess"):
                    notes = extra + ["identity-by-fit-only"]
                out.append(dict(table=table, rowid=c["rowid"], values=[enc(v) for v in c["vals"]], method=method,
                                relation=rel, origin=im["origin"], basis=basis,
                                confidence=min(METHOD_BASE[method], BASIS_CAP[basis]), notes=notes,
                                cell=dict(file=c["file"], page=c["page"], offset=c["off"], length=c["length"],
                                          cell_hex=c["hex"])))
                found[wi] += 1
    for wi, w in enumerate(wants):
        if w.get("required") and not found[wi]:
            raise SystemExit("%s: required history version %r is in no page image" % (name, w))
    if len(out) < min_history:
        raise SystemExit("%s: the oracle finds %d history entries, at least %d needed" % (name, len(out), min_history))
    return out


# ---- the whole description -----------------------------------------------

def build_expect(name, cls, statements, files, wants=(), unreachable=(), min_history=0, tool="tools/fixtures/sqlite_gen.py"):
    exp = {"generator": dict(tool=tool, class_=cls, statements=statements, file_sha256={})}
    exp["generator"]["class"] = exp["generator"].pop("class_")
    exp["warnings"] = {"live": []}
    for suffix, k in ((".db", "db"), (".db-wal", "wal"), (".db-journal", "journal")):
        if files.get(k) is not None:
            exp["generator"]["file_sha256"][name + suffix] = hashlib.sha256(files[k]).hexdigest()
    eng = engine_answers(files)
    exp["generator"]["version"] = "python sqlite3 module, libsqlite3 %s; shell: %s" % (
        eng["version"], subprocess.run(["sqlite3", "--version"], capture_output=True, text=True).stdout.split()[0])
    db = files["db"]
    exp["header"] = header(db)
    exp["schema"] = eng["schema"]
    exp["info"] = eng["info"]
    exp["extra"] = {"dbinfo": eng["dbinfo"], "dbstat": eng["dbstat"]}
    live = State(db, files.get("wal"), files.get("journal"), True, True)
    asfound = State(db, files.get("wal"), files.get("journal"), True, False)
    exp["live"], exp["cells"] = {}, {}
    live_raw = {}
    roots = {s["name"].lower(): s["rootpage"] for s in eng["schema"]
             if s["type"] == "table" and s["rootpage"] > 0 and not s["sql"].lower().startswith("create virtual")}
    for n in sorted(eng["rows"]):
        wr = eng["wr"][n]
        rows = eng["rows"][n]
        exp["live"][n] = dict(without_rowid=wr, rows=[[enc(v) for v in r] for r in rows])
        cells, raw = [], {}

        def visit(c, cells=cells, raw=raw):
            cells.append(dict(rowid=c["rowid"], file=c["file"], page=c["page"], offset=c["off"], length=c["length"], cell_hex=c["hex"]))
            if c["has_rowid"]:
                raw[c["rowid"]] = c["vals"]
        walk(live, roots[n], visit)
        if len(cells) != len(rows):
            raise SystemExit("%s: table %s: walker %d cells, engine %d rows" % (name, n, len(cells), len(rows)))
        if not wr:
            for c, r in zip(cells, rows):
                if c["rowid"] != r[0]:
                    raise SystemExit("%s: table %s: walker rowid %d, engine %d" % (name, n, c["rowid"], r[0]))
        exp["cells"][n] = cells
        live_raw[n] = raw
    exp["as_found"] = {}
    for o in schema_rows(asfound):
        if o["type"] != "table" or o["root"] == 0 or o["sql"].lower().startswith("create virtual"):
            continue
        rows = []

        def visit(c, rows=rows):
            rows.append(([enc(c["rowid"])] if c["has_rowid"] else []) + [enc(v) for v in c["vals"]])
        walk(asfound, o["root"], visit)
        exp["as_found"][o["name"].lower()] = rows
    codes = set()
    if files.get("wal") is None and files.get("journal") is None:
        tr, lv = freelist_of(live)
        exp["freelist"] = dict(trunks=tr, leaves=lv)
        if len(tr) + len(lv) != eng["info"]["freelist_count"]:
            raise SystemExit("%s: walker %d free pages, engine %d" % (name, len(tr) + len(lv), eng["info"]["freelist_count"]))
    if files.get("wal") is not None:
        w = Wal(files["wal"])
        exp["wal"] = w.expect()
        if any(f["state"] in ("uncommitted", "broken", "detached", "stale") for f in w.frames):
            codes.add("wal-frames-not-applied")
        if w.trailing:
            codes.add("wal-torn-tail")
    if files.get("journal") is not None:
        j = Journal(files["journal"], exp["header"]["page_size"])
        exp["journal"] = j.expect()
        if j.zeroed:
            codes.add("journal-header-invalid")
        if j.hot:
            codes.add("journal-hot")
            seen = {}
            for r in j.recs:
                if r["ok"]:
                    seen[r["page"]] = seen.get(r["page"], 0) + 1
                    if seen[r["page"]] == 2:
                        codes.add("journal-duplicate-page")
    exp["warnings"]["live"] = sorted(codes)
    exp["history"] = build_history(name, files, live, live_raw, list(wants), min_history)
    exp["unreachable"] = list(unreachable)
    return exp


def write_gz(path, data):
    import gzip
    with open(path, "wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=9, mtime=0) as z:
            z.write(data)

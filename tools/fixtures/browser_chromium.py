"""Chromium History fixtures (plan 4F, Task 2): scenario rows and the independent oracle.

    py -3 tools/fixtures/telephony_gen.py --family chromium --out internal/parsers/chromium/testdata \\
        --pins-go internal/parsers/chromium/pins_test.go --pins-package chromium_test

The oracle restates the mapping rules of rulings 4F-R4..R11, R15, R21 and R23 of the plan from the plan text and from
the Constants table of schemas/chromium/CROSSCHECK.md (read at generation time); it shares no code with the Go parser.
Stored values are read back from the engine on a COPY of the finished files (C.engine_view).

Choices the plan leaves to the oracle (the parser of Task 3/4 follows them):
  * `summary` is stated only for plain ASCII of at most 80 characters (pick_summary); otherwise it is null and the
    comparer skips it (the Go unit tests of Tasks 3/4 pin the bidi/ZWJ and over-long cases against common.SummarizeChars);
  * a search or download payload has no profile slot: the profile is `raw.profile` there (a visit has `payload.profile`);
  * a search states `time_source` only when it states a Time; a visit-less `urls` row lists its own unusable cells in
    `raw.unknown_columns`, a visit lists the joined ones in `raw.joined_unknown_columns` as "urls.<col>";
  * raw values: an integer cell is kept as stored (`raw.<col>`, with `raw.<col>_status` when its instant is not
    valid), a NULL cell is absent, a cell of another storage class is `unknown_columns.<col> = "type-mismatch"`;
  * row warnings are matched by a fragment of the reason: "urls" (a visit without a usable urls row), "term" (an empty
    search term), "target_path" (a download with neither a target path nor a chain URL);
  * the schema notes assume this column order of the Go mappings (required first as listed, then the optional ones):
    urls url,last_visit_time,title,visit_count,typed_count,hidden; visits url,visit_time,transition,from_visit,
    visit_duration,opener_visit,originator_cache_guid,originator_visit_id,external_referrer_url; keyword_search_terms
    keyword_id,url_id,term,normalized_term; downloads id,target_path,start_time then current_path,guid,received_bytes,
    total_bytes,state,danger_type,interrupt_reason,hash,end_time,opened,last_access_time,transient,referrer,site_url,
    tab_url,tab_referrer_url,http_method,mime_type,original_mime_type; meta key,value.
The scenario has no repeated chain_index: downloads_url_chains has PRIMARY KEY (id, chain_index), so a conforming file
cannot hold one (the oracle states "duplicate-index" for a hostile file; the Go tests build it in memory).
Not modelled (the scenario never reaches them): a URL over 2 MiB, a chain over 1000 entries or 2 MiB, a body over
1 MiB, a non-integer chain_index, a non-integer keyword_id.
"""
import datetime
import os
import re

import telephony_common as C

FAMILY = "chromium"
SCHEMAS = os.path.join(C.HERE, "schemas", "chromium")
CROSSCHECK = os.path.join(SCHEMAS, "CROSSCHECK.md")

REMOTE_CHROME = "/data/data/com.android.chrome/app_chrome/Default/History"
REMOTE_EDGE = "/data/data/com.microsoft.emmx/app_chrome/Default/History"
REMOTE_BRAVE = "/data/user/10/com.brave.browser/app_chrome/Profile 1/History"
REMOTE_WEBVIEW = "/data/data/com.example.reader/app_webview/Default/History"

MAX_URL = 2 << 20
MAX_TEXT = 256 << 10
MAX_BODY_PART = 64 << 10
MAX_PATH = 4096
EXACT = 2**53
WEBKIT_TO_UNIX = 11644473600
WEBKIT_EPOCH = datetime.datetime(1601, 1, 1, tzinfo=C.UTC)

TABLES = ["urls", "visits", "keyword_search_terms", "downloads", "meta"]
ALIAS = {"urls": "id", "visits": "id", "downloads": "id"}
MAPPED = {
    "urls": [("url", True), ("last_visit_time", True), ("title", False), ("visit_count", False), ("typed_count", False), ("hidden", False)],
    "visits": [("url", True), ("visit_time", True), ("transition", True), ("from_visit", False), ("visit_duration", False),
               ("opener_visit", False), ("originator_cache_guid", False), ("originator_visit_id", False),
               ("external_referrer_url", False)],
    "keyword_search_terms": [("keyword_id", True), ("url_id", True), ("term", True), ("normalized_term", False)],
    "downloads": [("id", True), ("target_path", True), ("start_time", True), ("current_path", False), ("guid", False),
                  ("received_bytes", False), ("total_bytes", False), ("state", False), ("danger_type", False),
                  ("interrupt_reason", False), ("hash", False), ("end_time", False), ("opened", False),
                  ("last_access_time", False), ("transient", False), ("referrer", False), ("site_url", False),
                  ("tab_url", False), ("tab_referrer_url", False), ("http_method", False), ("mime_type", False),
                  ("original_mime_type", False)],
    "meta": [("key", True), ("value", False)],
}
PACKAGES = {
    "com.android.chrome": "chrome", "com.chrome.beta": "chrome", "com.chrome.dev": "chrome", "com.chrome.canary": "chrome",
    "org.chromium.chrome": "chromium",
    "com.brave.browser": "brave", "com.brave.browser_beta": "brave", "com.brave.browser_nightly": "brave",
    "com.microsoft.emmx": "edge", "com.microsoft.emmx.beta": "edge", "com.microsoft.emmx.dev": "edge",
    "com.microsoft.emmx.canary": "edge",
    "com.opera.browser": "opera", "com.opera.browser.beta": "opera",
}
ENUMS = ("transition_core", "transition_qualifier", "download_state", "download_danger", "download_interrupt")
_CONST = {}


def constants():
    """The integer tables of CROSSCHECK.md (header | enum | int | name | source |)."""
    if not _CONST:
        with open(CROSSCHECK, encoding="utf-8") as f:
            for line in f:
                if not line.startswith("| "):
                    continue
                cells = [c.strip() for c in line.strip().strip("|").split("|")]
                if len(cells) == 4 and cells[0] in ENUMS and cells[1].lstrip("-").isdigit():
                    _CONST.setdefault(cells[0], {})[int(cells[1])] = cells[2]
        for e in ENUMS:
            if not _CONST.get(e):
                raise SystemExit("CROSSCHECK.md has no %s rows" % e)
    return _CONST


def meta_versions(ddl_name):
    """The per-phase meta.version list of the snapshot header (line 3)."""
    with open(os.path.join(SCHEMAS, ddl_name), encoding="utf-8") as f:
        lines = f.read().splitlines()
    m = re.fullmatch(r"-- meta\.version (\d+(?:,\d+)*)", lines[2])
    if not m:
        raise SystemExit("%s: no meta.version header" % ddl_name)
    return [int(x) for x in m.group(1).split(",")]


# ---------------------------------------------------------------- time and text


def wk(y, mo, d, h=0, mi=0, s=0, us=0):
    """WebKit microseconds (since 1601-01-01 UTC) of a UTC instant, by integer arithmetic."""
    delta = datetime.datetime(y, mo, d, h, mi, s, us, tzinfo=C.UTC) - WEBKIT_EPOCH
    return (delta.days * 86400 + delta.seconds) * 10**6 + delta.microseconds


def wk_status(v):
    if v in C.SENTINELS:
        return "sentinel"
    sec = v // 10**6 - WEBKIT_TO_UNIX
    return "valid" if 0 <= sec < C.MAX_SECONDS else "invalid"


def wk_iso(v):
    t = C.EPOCH + datetime.timedelta(seconds=v // 10**6 - WEBKIT_TO_UNIX, microseconds=v % 10**6)
    s = t.strftime("%Y-%m-%dT%H:%M:%S")
    if t.microsecond:
        s += (".%06d" % t.microsecond).rstrip("0")
    return s + "Z"


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def is_text(v):
    return isinstance(v, bytes) and not isinstance(v, C.Blob)


def plain(s):
    """Plain ASCII of at most 80 characters, one line, no repeated or edge spaces: the only text whose summary the
    oracle states (SummarizeChars leaves it as it is). Any other text gets no stated summary (the comparer skips it)."""
    return 0 < len(s) <= 80 and s.isascii() and s.isprintable() and "  " not in s and s == s.strip(" ")


def pick_summary(*candidates):
    """The summary source is the first non-empty candidate; it is stated only when it is plain."""
    for c in candidates:
        if c:
            return c if plain(c) else None
    return None


def cut_text(s, limit):
    enc = s.encode("utf-8")
    if len(enc) <= limit:
        return s, False
    cut = limit
    while cut > 0 and (enc[cut] & 0xC0) == 0x80:
        cut -= 1
    return enc[:cut].decode("utf-8"), True


def clean_into(raw, field, b, limit=MAX_TEXT):
    """common.CleanText: the cleaned text; the flags, the original bytes and a cut are stated in raw."""
    c = C.clean(b, limit)
    if c.flags:
        C.mark(raw, field)
        if c.b64:
            raw[field + "_raw_b64"] = c.b64
        if c.sha:
            raw[field + "_raw_sha256"] = c.sha
            raw[field + "_raw_len"] = c.rawlen
        if c.truncated:
            raw[field + "_total_bytes"] = c.total
            if field == "url":
                raw["url_truncated"] = True
    return c.text


def text_raw(raw, key, v, limit=MAX_TEXT):
    """A text cell kept in raw (non-empty text only); another storage class is a type mismatch."""
    if v is None or v == b"":
        return None
    if not is_text(v):
        return "type-mismatch"
    raw[key] = clean_into(raw, key, v, limit)
    return None


def instant(raw, unknown, col, v):
    """The WebKit cell v of column col: its ISO time when valid; raw.<col> and raw.<col>_status otherwise."""
    if v is None:
        return None
    if not is_int(v):
        unknown[col] = "type-mismatch"
        return None
    raw[col] = v
    st = wk_status(v)
    if st != "valid":
        raw[col + "_status"] = st
        return None
    return wk_iso(v)


def count(unknown, key, v):
    """A count: the stored integer when it is one and not negative."""
    if v is None:
        return None
    if not is_int(v):
        unknown[key] = "type-mismatch"
        return None
    if v < 0:
        unknown[key] = "negative"
        return None
    return v


# ---------------------------------------------------------------- label (4F-R5, R32)


def label(remote):
    parts = remote.split("/")
    idx = [i for i, p in enumerate(parts) if p in ("app_chrome", "app_webview")]
    if not idx:
        return "unknown-chromium", "", ""
    i = idx[-1]
    pkg = parts[i - 1] if i >= 1 else ""
    profile = parts[i + 1] if parts[-1] == "History" and i == len(parts) - 3 else ""
    if parts[i] == "app_webview":
        return "webview", profile, pkg
    return PACKAGES.get(pkg, "unknown-chromium"), profile, pkg


class Ctx:
    def __init__(self, remote, db_only):
        self.remote, self.db_only = remote, db_only
        self.browser, self.profile, self.pkg = label(remote)


def stamp(ctx, raw, with_profile):
    if ctx.pkg:
        raw["package"] = ctx.pkg
    if with_profile and ctx.profile:
        raw["profile"] = ctx.profile


# ---------------------------------------------------------------- transition (4F-R7, R29)


def transition(tv, p, raw, unknown):
    if tv is None:
        return
    if not is_int(tv):
        unknown["transition"] = "type-mismatch"
        return
    raw["transition"] = tv
    if not -(2**31) <= tv <= 2**32 - 1:
        unknown["transition"] = "out-of-range"
        return
    u = tv & 0xFFFFFFFF
    p["transition"] = constants()["transition_core"].get(u & 0xFF, "unknown")
    masks = constants()["transition_qualifier"]
    quals = [masks[m] for m in sorted(masks) if u & m]
    if quals:
        p["transition_qualifiers"] = quals
    left = (u & ~0xFF) & ~sum(masks)
    if left:
        raw["transition_unknown_bits"] = left


# ---------------------------------------------------------------- urls and visits


def urls_facts(u, p, unknown, prefix):
    """visit_count, typed_count and hidden of a urls row into the payload; unusable cells into unknown (prefixed)."""
    for col, key in (("visit_count", "url_visit_count"), ("typed_count", "typed_count")):
        n = count(unknown, prefix + col, u.get(col))
        if n is not None:
            p[key] = n
    h = u.get("hidden")
    if is_int(h) and h in (0, 1):
        p["hidden"] = h == 1
    elif h is not None:
        unknown[prefix + "hidden"] = "type-mismatch" if not is_int(h) else "not-boolean"


def body_of(raw, url, title):
    ub, uc = cut_text(url, MAX_BODY_PART)
    tb, tc = cut_text(title, MAX_BODY_PART)
    if uc or tc:
        full = url if not title else (title if not url else url + "\n" + title)
        raw["body_truncated"] = True
        raw["body_total_bytes"] = len(full.encode("utf-8"))
    return tb if not ub else (ub if not tb else ub + "\n" + tb)


def source_path(ctx, file):
    if file == "wal":
        return ctx.remote + "-wal"
    return ctx.remote if ctx.db_only else ""


def record(ctx, table, rowid, file, typ, time, times, summary, body, payload):
    return {"table": table, "rowid": rowid, "file": file, "type": typ, "source_path": source_path(ctx, file), "time": time,
            "time_end": None, "times": times, "deleted": False, "summary": summary, "body": body, "payload": payload}


def usable_url(u, raw):
    v = u.get("url")
    if not is_text(v) or v == b"":
        return None
    return clean_into(raw, "url", v, MAX_URL)


def title_of(u, raw, unknown, prefix):
    v = u.get("title")
    if v is None:
        return ""
    if not is_text(v):
        unknown[prefix + "title"] = "type-mismatch"
        return ""
    return clean_into(raw, "title", v)


def expect_visit(ctx, v, u, file):
    """The record of a visits row, or None when its urls row is absent or holds no usable url."""
    raw, unknown, joined = {}, {}, {}
    if u is None:
        return None
    url = usable_url(u, raw)
    if url is None:
        return None
    title = title_of(u, raw, joined, "urls.")
    p = {"url": url, "browser": ctx.browser, "visit_id": str(v["rowid"])}
    if title:
        p["title"] = title
    if ctx.profile:
        p["profile"] = ctx.profile
    urls_facts(u, p, joined, "urls.")
    transition(v.get("transition"), p, raw, unknown)
    fv = v.get("from_visit")
    if is_int(fv):
        raw["from_visit"] = fv
        if fv > 0:
            p["referrer"] = {"visit_id": str(fv)}
    elif fv is not None:
        unknown["from_visit"] = "type-mismatch"
    vd = v.get("visit_duration")
    if is_int(vd):
        raw["visit_duration"] = vd
        if vd > EXACT:
            unknown["visit_duration"] = "precision"
        elif vd > 0:
            sec = vd / 1e6
            p["duration_s"] = int(sec) if sec == int(sec) else sec
        elif vd < 0:
            unknown["visit_duration"] = "negative"
    elif vd is not None:
        unknown["visit_duration"] = "type-mismatch"
    t = instant(raw, unknown, "visit_time", v.get("visit_time"))
    if is_int(v.get("url")):
        raw["url_id"] = v["url"]
    for col in ("opener_visit", "originator_visit_id"):
        x = v.get(col)
        if is_int(x):
            raw[col] = x
        elif x is not None:
            unknown[col] = "type-mismatch"
    for col in ("originator_cache_guid", "external_referrer_url"):
        bad = text_raw(raw, col, v.get(col), MAX_URL if col == "external_referrer_url" else MAX_TEXT)
        if bad:
            unknown[col] = bad
    stamp(ctx, raw, False)
    if unknown:
        raw["unknown_columns"] = unknown
    if joined:
        raw["joined_unknown_columns"] = joined
    body = body_of(raw, url, title)
    p["raw"] = raw
    return record(ctx, "visits", v["rowid"], file, "web_visit", t, {}, pick_summary(title, url), body, p)


def expect_url(ctx, u, file):
    """The record of a visitless urls row (4F-R9), or None when it holds no usable url."""
    raw, unknown = {}, {}
    url = usable_url(u, raw)
    if url is None:
        return None
    title = title_of(u, raw, unknown, "")
    p = {"url": url, "browser": ctx.browser}
    if title:
        p["title"] = title
    if ctx.profile:
        p["profile"] = ctx.profile
    urls_facts(u, p, unknown, "")
    t = instant(raw, unknown, "last_visit_time", u.get("last_visit_time"))
    raw["visitless"] = True
    stamp(ctx, raw, False)
    if unknown:
        raw["unknown_columns"] = unknown
    body = body_of(raw, url, title)
    p["raw"] = raw
    return record(ctx, "urls", u["rowid"], file, "web_visit", t, {}, pick_summary(title, url), body, p)


# ---------------------------------------------------------------- searches


def expect_search(ctx, s, urls, file):
    raw, unknown = {}, {}
    term_b = s.get("term")
    if not is_text(term_b) or term_b == b"":
        return None
    term = clean_into(raw, "term", term_b)
    p = {"term": term, "browser": ctx.browser}
    norm = s.get("normalized_term")
    if is_text(norm) and norm != b"":
        p["normalized_term"] = clean_into(raw, "normalized_term", norm)
    elif norm is not None and norm != b"":
        unknown["normalized_term"] = "type-mismatch"
    p["engine_id"] = str(s["keyword_id"])
    t = None
    uid = s.get("url_id")
    if is_int(uid):
        raw["url_id"] = uid
        u = urls.get(uid)
        if u is None:
            raw["url_state"] = "no-urls-row"
        else:
            url = usable_url(u, raw)
            if url is not None:
                p["url"] = url
            t = instant(raw, unknown, "last_visit_time", u.get("last_visit_time"))
            if t is not None:
                p["time_source"] = "urls.last_visit_time"
    else:
        raise SystemExit("not modelled: a non-integer url_id")
    stamp(ctx, raw, True)
    if unknown:
        raw["unknown_columns"] = unknown
    p["raw"] = raw
    return record(ctx, "keyword_search_terms", s["rowid"], file, "web_search", t, {}, pick_summary(term, p.get("normalized_term", "")), term, p)


# ---------------------------------------------------------------- downloads


def enum_name(enum, v, raw, unknown, col):
    if v is None:
        return None
    if not is_int(v):
        unknown[col] = "type-mismatch"
        return None
    raw[col] = v
    return constants()[enum].get(v, "unknown")


def chain_of(chains, did, raw):
    """The ordered chain of a download and its state (4F-R11)."""
    rows = [c for c in chains if c.get("id") == did]
    if not rows:
        raw["url_chain_state"] = "no-chain-rows"
        return []
    if any(not is_int(c.get("chain_index")) for c in rows):
        raise SystemExit("not modelled: a non-integer chain_index")
    if len(rows) > 1000:
        raise SystemExit("not modelled: a chain over 1000 entries")
    rows.sort(key=lambda c: (c["chain_index"], c["rowid"]))
    idx = [c["chain_index"] for c in rows]
    if len(set(idx)) < len(idx):
        raw["url_chain_state"] = "duplicate-index"
    elif idx != list(range(len(idx))):
        raw["url_chain_state"] = "gaps"
    out = []
    for c in rows:
        b = c.get("url")
        if not is_text(b):
            raise SystemExit("not modelled: a chain URL that is not text")
        out.append(clean_into(raw, "url_chain", b, MAX_URL))
    return out


def expect_download(ctx, d, chains, file):
    raw, unknown = {}, {}
    chain = chain_of(chains, d["rowid"], raw)
    p = {"browser": ctx.browser}
    for col in ("target_path", "current_path"):
        v = d.get(col)
        if is_text(v) and v != b"":
            p[col] = clean_into(raw, col, v, MAX_PATH)
        elif v is not None and not is_text(v):
            unknown[col] = "type-mismatch"
    if not p.get("target_path") and not chain:
        return None
    if chain:
        p["url"] = chain[-1]
        p["url_chain"] = chain
    for col, key in (("referrer", "referrer"), ("tab_url", "tab_url"), ("mime_type", "mime")):
        v = d.get(col)
        if is_text(v) and v != b"":
            p[key] = clean_into(raw, key, v)
    tb = d.get("total_bytes")
    if is_int(tb):
        raw["total_bytes"] = tb
        if tb > 0:
            p["total_bytes"] = tb
        elif tb < 0:
            unknown["total_bytes"] = "negative"
    elif tb is not None:
        unknown["total_bytes"] = "type-mismatch"
    rb = count(unknown, "received_bytes", d.get("received_bytes"))
    if rb is not None:
        p["received_bytes"] = rb
    for col, key, enum in (("state", "state", "download_state"), ("danger_type", "danger_type", "download_danger"),
                           ("interrupt_reason", "interrupt_reason", "download_interrupt")):
        name = enum_name(enum, d.get(col), raw, unknown, col)
        if name:
            p[key] = name
    op = d.get("opened")
    if is_int(op) and op in (0, 1):
        p["opened"] = op == 1
    elif op is not None:
        unknown["opened"] = "type-mismatch" if not is_int(op) else "not-boolean"
    t = instant(raw, unknown, "start_time", d.get("start_time"))
    times = {}
    for col, kind in (("end_time", "ended"), ("last_access_time", "accessed")):
        x = instant(raw, unknown, col, d.get(col))
        if x is not None:
            times[kind] = x
    for col in ("guid", "original_mime_type", "tab_referrer_url", "site_url", "http_method"):
        bad = text_raw(raw, col, d.get(col))
        if bad:
            unknown[col] = bad
    tr = d.get("transient")
    if is_int(tr):
        raw["transient"] = tr
    elif tr is not None:
        unknown["transient"] = "type-mismatch"
    h = d.get("hash")
    if isinstance(h, C.Blob) and h:
        raw["hash"] = bytes(h).hex()
    elif h is not None and h != b"" and not isinstance(h, C.Blob):
        unknown["hash"] = "type-mismatch"
    stamp(ctx, raw, True)
    if unknown:
        raw["unknown_columns"] = unknown
    parts = ([p["target_path"]] if p.get("target_path") else []) + chain
    capped, cutflag = [], False
    for x in parts:
        y, c = cut_text(x, MAX_BODY_PART)
        capped.append(y)
        cutflag = cutflag or c
    if cutflag:
        raw["body_truncated"] = True
        raw["body_total_bytes"] = len("\n".join(parts).encode("utf-8"))
    body = "\n".join(capped)
    p["raw"] = raw
    return record(ctx, "downloads", d["rowid"], file, "download", t, times, pick_summary(*[re.split(r"[/\\]", p.get(k, ""))[-1] for k in ("target_path", "current_path")], p.get("url", "")), body, p)


# ---------------------------------------------------------------- the oracle


def schema_notes(con, notes):
    for t in TABLES:
        cols = C.columns(con, t)
        if not cols:
            notes["table." + t] = "absent"
            continue
        low = [c.lower() for c in cols]
        mapped = MAPPED[t]
        names = [n.lower() for n, _ in mapped]
        v = "columns:%d" % len(cols)
        opt = [n for n, req in mapped if not req and n.lower() in low]
        if opt:
            v += ";optional:" + ",".join(opt)
        notes["schema." + t] = v
        un = [c for c in cols if c.lower() not in names and c != ALIAS.get(t)]
        if un:
            notes["unmapped_columns." + t] = ",".join(un)


def meta_notes(rows, notes):
    for r in rows:
        key = r.get("key")
        if key not in (b"version", b"last_compatible_version"):
            continue
        v = r.get("value")
        if is_int(v):
            val = str(v)
        elif is_text(v) and len(v) <= 16 and re.fullmatch(rb"[0-9]+", v):
            val = v.decode()
        elif v is None:
            continue
        else:
            val = "non-numeric"
        notes["meta." + key.decode()] = val


def oracle(files, ddl_name, variant, remote, cols_con, wal_touched=()):
    view = C.engine_view(files, ["urls", "visits", "keyword_search_terms", "downloads", "downloads_url_chains", "meta"])
    ctx = Ctx(remote, set(files) == {"db"})
    exp = C.expect_header(FAMILY, variant, ddl_name)
    urls = {r["rowid"]: r for r in view.get("urls", [])}
    visits = view.get("visits", [])
    seen = {v["url"] for v in visits if is_int(v.get("url"))}
    warn = []

    def where(table, r):
        return "wal" if (table, r["rowid"]) in wal_touched else "any"

    for u in view.get("urls", []):
        if u["rowid"] in seen:
            continue
        rec = expect_url(ctx, u, where("urls", u))
        if rec:
            exp["records"].append(rec)
        else:
            warn.append({"table": "urls", "rowid": u["rowid"], "contains": "url"})
    for v in visits:
        rec = expect_visit(ctx, v, urls.get(v.get("url")), where("visits", v))
        if rec:
            exp["records"].append(rec)
        else:
            warn.append({"table": "visits", "rowid": v["rowid"], "contains": "urls"})
    for s in view.get("keyword_search_terms", []):
        rec = expect_search(ctx, s, urls, where("keyword_search_terms", s))
        if rec:
            exp["records"].append(rec)
        else:
            warn.append({"table": "keyword_search_terms", "rowid": s["rowid"], "contains": "term"})
    for d in view.get("downloads", []):
        rec = expect_download(ctx, d, view.get("downloads_url_chains", []), where("downloads", d))
        if rec:
            exp["records"].append(rec)
        else:
            warn.append({"table": "downloads", "rowid": d["rowid"], "contains": "target_path"})
    exp["warnings"] = warn
    schema_notes(cols_con, exp["notes"])
    for t in TABLES:
        n = sum(1 for w in warn if w["table"] == t)
        if n:
            exp["notes"]["unmapped_rows." + t] = str(n)
    meta_notes(view.get("meta", []), exp["notes"])
    return exp


# ---------------------------------------------------------------- scenario rows


def url_row(i, url, title="", vc=1, tc=0, last=None, hidden=0):
    return {"id": i, "url": url, "title": title, "visit_count": vc, "typed_count": tc,
            "last_visit_time": wk(2023, 5, 1, 10, 0, 0) + i * 60 * 10**6 if last is None else last, "hidden": hidden}


def visit_row(i, url, **kw):
    r = {"id": i, "url": url, "visit_time": wk(2023, 5, 1, 10, 0, 0) + i * 60 * 10**6, "from_visit": 0, "transition": 0,
         "visit_duration": 0}
    r.update(kw)
    return r


def current_urls():
    big = "data:text/plain," + "A" * (100 * 1024 - len("data:text/plain,"))
    return [
        url_row(1, "https://example.com/a", "Example A", vc=2, tc=1, last=wk(2023, 5, 1, 10, 0, 0)),
        url_row(2, "https://example.org/cleared", "Cleared", vc=3, last=wk(2023, 5, 2, 8, 30, 0, 250000)),
        url_row(3, "https://example.net/never", "Never", last=0),
        url_row(4, big, "Big data URL"),
        url_row(5, C.Raw(b"https://example.com/bad\xffbyte"), C.Raw(b"Nul\x00title")),
        url_row(6, "https://example.com/notitle", ""),
        url_row(7, "https://example.com/hidden", "Hidden", hidden=1),
        url_row(8, "https://example.com/bidi", "Re‮tsooB \U0001F469‍\U0001F4BB"),
        url_row(9, "", "Empty URL"),
        url_row(10, "https://example.com/counts", "Counts", vc="many"),
        url_row(11, "https://example.com/hiddentwo", "Hidden two", hidden=2),
    ]


def current_visits():
    t = wk(2023, 5, 1, 10, 0, 0)
    return [
        visit_row(1, 1, visit_time=t, transition=838860801, visit_duration=5500000),
        visit_row(2, 1, from_visit=1, transition=-1610612736),
        visit_row(3, 4, transition=1),
        visit_row(4, 1, visit_time=0),
        visit_row(5, 1, visit_time=wk(2100, 1, 1)),
        visit_row(6, 1, visit_time=10_000_000_000_000_000),
        visit_row(7, 1, transition=42),
        visit_row(8, 1, transition=2**33),
        visit_row(9, 999),
        visit_row(10, 9),
        visit_row(11, 1, originator_cache_guid="dev-b-guid", originator_visit_id=77),
        visit_row(12, 1, visit_duration=90_000_000),
        visit_row(13, 1, visit_duration=-3),
        visit_row(14, 1, visit_time="yesterday"),
        visit_row(15, 5),
        visit_row(16, 1, opener_visit=1),
        visit_row(17, 1, external_referrer_url="https://ref.example.org/x?y=1"),
        visit_row(18, 1, transition=1 | 0x100 | 0x10000000),
        visit_row(19, 6, visit_duration=1234567),
        visit_row(20, 7),
        visit_row(21, 8),
        visit_row(22, 10),
    ]


def current_searches():
    return [
        {"keyword_id": 2, "url_id": 1, "term": "Minutiae Forensics", "normalized_term": "minutiae forensics"},
        {"keyword_id": 2, "url_id": 999, "term": "dangling term", "normalized_term": "dangling term"},
        {"keyword_id": 3, "url_id": 1, "term": C.Raw(b"bad \xfe term"), "normalized_term": C.Raw(b"bad \xfe term")},
        {"keyword_id": 2, "url_id": 1, "term": "", "normalized_term": ""},
        {"keyword_id": 2, "url_id": 3, "term": "sentinel time", "normalized_term": "sentinel time"},
    ]


def dl_row(i, target, **kw):
    t = wk(2023, 5, 1, 11, 0, 0) + i * 3600 * 10**6
    r = {"id": i, "guid": "GUID-%d" % i, "current_path": target, "target_path": target, "start_time": t, "received_bytes": 100,
         "total_bytes": 100, "state": 1, "danger_type": 0, "interrupt_reason": 0, "hash": b"", "end_time": t + 30 * 10**6,
         "opened": 0, "last_access_time": 0, "transient": 0, "referrer": "", "site_url": "", "embedder_download_data": "",
         "tab_url": "", "tab_referrer_url": "", "http_method": "", "by_ext_id": "", "by_ext_name": "", "by_web_app_id": "",
         "etag": "", "last_modified": "", "mime_type": "", "original_mime_type": ""}
    r.update(kw)
    return r


DL = "/storage/emulated/0/Download/"


def current_downloads():
    h = bytes(range(32))
    t = wk(2023, 5, 1, 11, 0, 0)
    return [
        dl_row(1, DL + "report.pdf", opened=1, received_bytes=5000, total_bytes=5000, hash=h, end_time=t + 3600 * 10**6 + 30 * 10**6,
               last_access_time=t + 3600 * 10**6 + 300 * 10**6, mime_type="application/pdf", original_mime_type="application/pdf",
               referrer="https://example.com/a", tab_url="https://example.com/a", tab_referrer_url="https://example.com/",
               site_url="https://example.com", http_method="GET", transient=0),
        dl_row(2, DL + "big.iso", state=4, interrupt_reason=22, received_bytes=1024, total_bytes=0),
        dl_row(3, DL + "pending.zip", state=0, received_bytes=0, total_bytes=0),
        dl_row(4, "", current_path="", received_bytes=0, total_bytes=0),
        dl_row(5, DL + "gaps.bin"),
        dl_row(6, DL + "bug-140687.bin", state=3),
        dl_row(7, DL + "odd.bin", state=9, danger_type=99, interrupt_reason=32, end_time=0),
        dl_row(9, DL + "a" * (5000 - len(DL)), received_bytes=7, total_bytes=7),
    ]


def current_chains():
    return [
        {"id": 1, "chain_index": 1, "url": "https://cdn.example.com/report.pdf"},
        {"id": 1, "chain_index": 0, "url": "https://example.com/dl?id=1"},
        {"id": 2, "chain_index": 0, "url": "https://example.com/big.iso"},
        {"id": 5, "chain_index": 0, "url": "https://example.com/gaps-0"},
        {"id": 5, "chain_index": 2, "url": "https://example.com/gaps-2"},
        {"id": 6, "chain_index": 0, "url": "https://example.com/bug-140687.bin"},
        {"id": 7, "chain_index": 0, "url": "https://example.com/odd.bin"},
        {"id": 9, "chain_index": 0, "url": "https://example.com/long-path"},
    ]


def meta_rows(version, extra=()):
    rows = [{"key": "version", "value": str(version)}, {"key": "last_compatible_version", "value": str(constants_compat())}]
    return rows + [{"key": k, "value": v} for k, v in extra]


def constants_compat():
    return 16  # history_version "compatible" row of CROSSCHECK.md (checked below)


def check_compat():
    with open(CROSSCHECK, encoding="utf-8") as f:
        if not re.search(r"^\| history_version \| 16 \| compatible \|", f.read(), re.M):
            raise SystemExit("CROSSCHECK.md no longer says compatible = 16")


def fill(con, urls=(), visits=(), searches=(), downloads=(), chains=(), meta=()):
    with C.batch(con):
        for t, rows in (("urls", urls), ("visits", visits), ("keyword_search_terms", searches), ("downloads", downloads),
                        ("downloads_url_chains", chains), ("meta", meta)):
            for r in rows:
                C.insert(con, t, r)


def build_current(work, ddl_name="chromium-current.sql"):
    check_compat()
    path = work + "/History"
    ph = C.load_ddl(ddl_name, SCHEMAS)
    con = C.connect(path)
    con.executescript(ph[0])
    ver = meta_versions(ddl_name)[0]
    fill(con, current_urls(), current_visits(), current_searches(), current_downloads(), current_chains(),
         meta_rows(ver, [("mmap_status", "-1")]))
    return path, con


def variant_current(work):
    path, con = build_current(work)
    files = {"db": C.read(path)}
    exp = oracle(files, "chromium-current.sql", "current", REMOTE_CHROME, con)
    con.close()
    return files, exp


def lit(v):
    if isinstance(v, bytes):
        return "x'%s'" % v.hex()
    return C.lit(v)


def insert_sql(table, row):
    cols = list(row)
    return "insert into %s(%s) values (%s)" % (table, ",".join(cols), ",".join(lit(row[c]) for c in cols))


def variant_wal(work):
    path, con = build_current(work)
    con.execute("pragma journal_mode = wal")
    con.close()
    new_visits = [visit_row(23, 1, transition=1), visit_row(24, 6, transition=0), visit_row(25, 8, transition=7)]
    new_dl = dl_row(10, DL + "wal-only.apk", received_bytes=42, total_bytes=42)
    stmts = ["pragma wal_autocheckpoint = 0"]
    stmts += [insert_sql("visits", v) for v in new_visits]
    stmts.append(insert_sql("downloads", new_dl))
    stmts.append(insert_sql("downloads_url_chains", {"id": 10, "chain_index": 0, "url": "https://example.com/wal-only.apk"}))
    stmts.append("update urls set title = 'Example A (edited in the WAL)' where id = 1")
    C.kill_child(path, stmts)
    files = {"db": C.read(path), "wal": C.read(path + "-wal")}
    schema = build_schema_only(work, "chromium-current.sql")
    touched = {("visits", 23), ("visits", 24), ("visits", 25), ("downloads", 10)}
    exp = oracle(files, "chromium-current.sql", "current-wal", REMOTE_CHROME, schema, touched)
    schema.close()
    return files, exp


def build_schema_only(work, ddl_name):
    import sqlite3
    con = sqlite3.connect(":memory:", isolation_level=None)
    con.executescript(C.load_ddl(ddl_name, SCHEMAS)[0])
    return con


def variant_mid(work):
    ddl = "chromium-mid.sql"
    path = work + "/History"
    ph = C.load_ddl(ddl, SCHEMAS)
    v1, v2 = meta_versions(ddl)
    con = C.connect(path)
    con.executescript(ph[1])
    check_compat()
    t = wk(2023, 3, 1, 9, 0, 0)
    fill(con, urls=[url_row(1, "https://old.example.com/", "Old page", vc=2, last=t),
                    url_row(2, "https://old.example.com/gone", "Gone", last=t + 10**6)],
         visits=[visit_row(1, 1, visit_time=t, transition=1), visit_row(2, 1, visit_time=t + 60 * 10**6, from_visit=1)],
         searches=[{"keyword_id": 2, "url_id": 1, "term": "old search", "normalized_term": "old search"}],
         downloads=[dl_row(1, DL + "old.txt", mime_type="text/plain")],
         chains=[{"id": 1, "chain_index": 0, "url": "https://old.example.com/old.txt"}], meta=meta_rows(v1))
    con.executescript(ph[2])
    with C.batch(con):
        con.execute("update meta set value = ? where key = 'version'", (str(v2),))
    t2 = wk(2023, 6, 1, 9, 0, 0)
    fill(con, urls=[url_row(3, "https://new.example.com/", "New page", last=t2)],
         visits=[visit_row(3, 3, visit_time=t2, transition=0, opener_visit=2), visit_row(4, 1, visit_time=t2 + 60 * 10**6, opener_visit=3)])
    files = {"db": C.read(path)}
    exp = oracle(files, ddl, "mid-upgraded", REMOTE_EDGE, con)
    con.close()
    return files, exp


def variant_old(work):
    ddl = "chromium-old.sql"
    path = work + "/History"
    ph = C.load_ddl(ddl, SCHEMAS)
    check_compat()
    con = C.connect(path)
    con.executescript(ph[1])
    t = wk(2022, 11, 5, 18, 0, 0)
    fill(con, urls=[url_row(1, "https://brave.example.com/", "Brave page", last=t), url_row(2, "https://brave.example.com/b", "B", last=t + 10**6),
                    url_row(3, "https://brave.example.com/visitless", "Visitless", last=t + 2 * 10**6)],
         visits=[visit_row(1, 1, visit_time=t, transition=1), visit_row(2, 2, visit_time=t + 10**6, from_visit=1),
                 visit_row(3, 1, visit_time=t + 2 * 10**6, transition=-2147483648)],
         searches=[{"keyword_id": 4, "url_id": 2, "term": "brave search", "normalized_term": "brave search"}],
         meta=meta_rows(meta_versions(ddl)[0]))
    files = {"db": C.read(path)}
    exp = oracle(files, ddl, "old", REMOTE_BRAVE, con)
    con.close()
    return files, exp


def variant_minimal(work):
    ddl = "chromium-minimal.sql"
    path = work + "/History"
    ph = C.load_ddl(ddl, SCHEMAS)
    con = C.connect(path)
    con.executescript(ph[0])
    t = wk(2024, 1, 15, 7, 30, 0)
    fill(con, urls=[url_row(1, "https://reader.example.com/article", "Article", last=t), url_row(2, "https://reader.example.com/unvisited", "Unvisited", last=t + 10**6)],
         visits=[visit_row(1, 1, visit_time=t, transition=1), visit_row(2, 1, visit_time=t + 60 * 10**6), visit_row(3, 1, visit_time=t + 120 * 10**6, transition=8)],
         meta=[{"key": "version", "value": str(meta_versions(ddl)[0])}])
    files = {"db": C.read(path)}
    exp = oracle(files, ddl, "minimal", REMOTE_WEBVIEW, con)
    con.close()
    return files, exp


VARIANTS = {"current": variant_current, "current-wal": variant_wal, "mid-upgraded": variant_mid, "old": variant_old, "minimal": variant_minimal}

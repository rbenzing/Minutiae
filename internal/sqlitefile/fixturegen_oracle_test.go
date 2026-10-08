package sqlitefile_test

// The oracle half of the fixture generator: turns the files of a fxScenario into
// the expect.json that the fixture tests compare the library with. The engine
// answers for schema, live rows and pragmas (on a COPY of the files, which it
// may recover or checkpoint), and the walker of fixturewalk_test.go answers
// for bytes: cells, WAL frames, journal records, the freelist. No library
// code is used.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func fxSha256Of(b []byte) [32]byte { return sha256.Sum256(b) }

// fxEngineAnswers is what the engine says about the files.
type fxEngineAnswers struct {
	version string
	schema  []fxSchemaRow
	rows    map[string][]engineRow
	wr      map[string]bool
	info    fxInfo
}

func fxRunEngine(t *testing.T, f fxFiles) fxEngineAnswers {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "o.db")
	if err := os.WriteFile(path, f.db, 0o600); err != nil {
		t.Fatal(err)
	}
	if f.wal != nil {
		if err := os.WriteFile(path+"-wal", f.wal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if f.journal != nil {
		if err := os.WriteFile(path+"-journal", f.journal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db := openEngine(t, path)
	defer func() { _ = db.Close() }() // an engine left open changes how later scenarios spill their cache
	var a fxEngineAnswers
	a.wr = map[string]bool{}
	if err := db.QueryRow("select sqlite_version()").Scan(&a.version); err != nil {
		t.Fatalf("engine version: %v", err)
	}
	r, err := db.Query("select type, name, tbl_name, rootpage, coalesce(sql, '') from sqlite_schema order by rowid")
	if err != nil {
		t.Fatalf("engine schema: %v", err)
	}
	for r.Next() {
		var s fxSchemaRow
		if err := r.Scan(&s.Type, &s.Name, &s.TblName, &s.Rootpage, &s.SQL); err != nil {
			t.Fatal(err)
		}
		a.schema = append(a.schema, s)
	}
	_ = r.Close()
	a.rows = engineDumpRows(t, db)
	for _, n := range engineTableNames(t, db) {
		a.wr[strings.ToLower(n)] = engineWithoutRowid(t, db, n)
	}
	a.info.IntegrityCheck = pragmaString(t, db, "integrity_check")
	fxScanInt(t, db, "pragma freelist_count", &a.info.FreelistCount)
	fxScanInt(t, db, "pragma page_count", &a.info.PageCount)
	return a
}

func fxScanInt(t *testing.T, db *sql.DB, q string, out *int64) {
	t.Helper()
	if err := db.QueryRow(q).Scan(out); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// fxFileHeader parses the 100-byte header.
func fxFileHeader(db []byte) *fxHeader {
	h := &fxHeader{}
	h.PageSize = be16(db[16:])
	if h.PageSize == 1 {
		h.PageSize = 65536
	}
	h.Reserved = int(db[20])
	h.Encoding = map[uint32]string{1: "UTF-8", 2: "UTF-16le", 3: "UTF-16be"}[be32(db[56:])]
	if h.Encoding == "" {
		h.Encoding = fmt.Sprintf("encoding(%d)", be32(db[56:]))
	}
	h.HeaderPages = be32(db[28:])
	h.ChangeCounter = be32(db[24:])
	h.VersionValidFor = be32(db[92:])
	h.HeaderPagesValid = h.HeaderPages != 0 && h.ChangeCounter == h.VersionValidFor
	h.FilePages = uint32((len(db) + h.PageSize - 1) / h.PageSize)
	h.SQLiteVersion = be32(db[96:])
	h.SchemaCookie = be32(db[40:])
	h.SchemaFormat = be32(db[44:])
	h.FreelistTrunk = be32(db[32:])
	h.FreelistCount = be32(db[36:])
	// auto_vacuum: the largest root page (offset 52) non-zero means on; the
	// incremental flag is offset 64.
	h.LargestRoot = be32(db[52:])
	switch {
	case h.LargestRoot == 0:
		h.AutoVacuum = 0
	case be32(db[64:]) != 0:
		h.AutoVacuum = 2
	default:
		h.AutoVacuum = 1
	}
	h.UserVersion = int32(be32(db[60:]))
	h.ApplicationID = be32(db[68:])
	h.WriteVersion, h.ReadVersion = int(db[18]), int(db[19])
	return h
}

func fxLocOf(c fxCellV) fxLoc {
	return fxLoc{File: c.file, Page: c.page, Offset: c.off, Length: int64(c.length), CellHex: hex.EncodeToString(c.hex)}
}

// ---- finishing a fixture ----

func fxFinish(t *testing.T, spec fixtureSpec, sc *fxScenario) *fxGen {
	t.Helper()
	g := &fxGen{name: spec.Name, class: spec.Class, files: map[string][]byte{}}
	g.files[spec.Name+".db"] = sc.files.db
	if spec.WAL != (sc.files.wal != nil) || spec.Journal != (sc.files.journal != nil) {
		t.Fatalf("%s: the fxScenario's companion files do not match the fixture table", spec.Name)
	}
	if sc.files.wal != nil {
		g.files[spec.Name+".db-wal"] = sc.files.wal
	}
	if sc.files.journal != nil {
		g.files[spec.Name+".db-journal"] = sc.files.journal
	}
	e := &g.expect
	e.Generator = fxGenerator{
		Tool: "internal/sqlitefile/fixturegen_test.go", Class: spec.Class, Statements: sc.stmts, FileSHA256: map[string]string{},
	}
	for n, b := range g.files {
		s := fxSha256Of(b)
		e.Generator.FileSHA256[n] = hex.EncodeToString(s[:])
	}
	e.Warnings.Live = []string{}
	if spec.NotSQLite {
		e.NotSQLite = true
		return g
	}
	for _, m := range sc.markers {
		for n, b := range g.files {
			if bytes.Contains(b, []byte(m)) {
				t.Fatalf("%s: marker %q is still in %s (secure_delete should have erased it)", spec.Name, m, n)
			}
		}
	}
	eng := fxRunEngine(t, sc.files)
	e.Generator.Version = "engine sqlite " + eng.version + " (modernc.org/sqlite)"
	if sc.class == "A" && strings.HasPrefix(spec.Name, "builder-") {
		e.Generator.Version = "sqlitetest builder; oracle engine sqlite " + eng.version + " (modernc.org/sqlite)"
	}
	e.Header = fxFileHeader(sc.files.db)
	e.Schema = eng.schema
	e.Info = &eng.info
	live := fxNewState(sc.files, true, true)
	asFound := fxNewState(sc.files, true, false)

	// live rows and cells
	e.Live = map[string]fxLiveTable{}
	e.Cells = map[string][]fxCellRow{}
	liveRaw := map[string]map[int64][]any{}
	roots := map[string]uint32{}
	for _, s := range eng.schema {
		if s.Type == "table" && s.Rootpage > 0 && !strings.HasPrefix(strings.ToLower(s.SQL), "create virtual") {
			roots[strings.ToLower(s.Name)] = s.Rootpage
		}
	}
	names := make([]string, 0, len(eng.rows))
	for n := range eng.rows {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		lt := fxLiveTable{WithoutRowid: eng.wr[n], Rows: [][]fxVal{}}
		for _, r := range eng.rows[n] {
			var row []fxVal
			if r.HasRowid {
				row = append(row, fxEnc(r.Rowid))
			}
			for _, v := range r.Vals {
				row = append(row, fxEnc(v))
			}
			lt.Rows = append(lt.Rows, row)
		}
		e.Live[n] = lt
		root, ok := roots[n]
		if !ok {
			t.Fatalf("%s: the engine lists table %s, the schema has no root for it", spec.Name, n)
		}
		var cells []fxCellRow
		raw := map[int64][]any{}
		if err := live.walk(root, func(c fxCellV) {
			cells = append(cells, fxCellRow{Rowid: c.rowid, fxLoc: fxLocOf(c)})
			if c.hasRowid {
				raw[c.rowid] = c.vals
			}
		}); err != nil {
			t.Fatalf("%s: walking %s: %v", spec.Name, n, err)
		}
		if len(cells) != len(eng.rows[n]) {
			t.Fatalf("%s: table %s: the walker finds %d cells, the engine %d rows", spec.Name, n, len(cells), len(eng.rows[n]))
		}
		if !eng.wr[n] {
			for i, r := range eng.rows[n] {
				if cells[i].Rowid != r.Rowid {
					t.Fatalf("%s: table %s row %d: the walker says rowid %d, the engine %d", spec.Name, n, i, cells[i].Rowid, r.Rowid)
				}
			}
		}
		e.Cells[n] = cells
		liveRaw[n] = raw
	}

	// as found: the raw file plus the WAL overlay, never a journal rollback
	e.AsFound = map[string][][]fxVal{}
	objs, err := asFound.schemaRows()
	if err != nil {
		t.Fatalf("%s: as-found schema: %v", spec.Name, err)
	}
	for _, o := range objs {
		if o.typ != "table" || o.root == 0 || strings.HasPrefix(strings.ToLower(o.sql), "create virtual") {
			continue
		}
		rows := [][]fxVal{}
		if err := asFound.walk(o.root, func(c fxCellV) {
			var r []fxVal
			if c.hasRowid {
				r = append(r, fxEnc(c.rowid))
			}
			for _, v := range c.vals {
				r = append(r, fxEnc(v))
			}
			rows = append(rows, r)
		}); err != nil {
			t.Fatalf("%s: as-found walk of %s: %v", spec.Name, o.name, err)
		}
		e.AsFound[strings.ToLower(o.name)] = rows
	}

	if !spec.WAL && !spec.Journal {
		tr, lv := fxFreelistOf(live)
		e.Freelist = &fxFreelist{Trunks: nz(tr), Leaves: nz(lv)}
		if int64(len(tr)+len(lv)) != eng.info.FreelistCount {
			t.Fatalf("%s: the walker finds %d free pages, the engine says freelist_count %d", spec.Name, len(tr)+len(lv), eng.info.FreelistCount)
		}
	}
	codes := map[string]bool{}
	if spec.WAL {
		w := fxScanWAL(sc.files.wal)
		e.WAL = w.expect()
		for _, f := range w.frames {
			switch f.state {
			case "uncommitted", "broken", "detached", "stale":
				codes["wal-frames-not-applied"] = true
			}
		}
		if w.trailing > 0 {
			codes["wal-torn-tail"] = true
		}
	}
	if spec.Journal {
		j := fxScanJournal(sc.files.journal, e.Header.PageSize)
		e.Journal = j.expect()
		if j.zeroed {
			codes["journal-header-invalid"] = true // a zeroed header is not a valid header
			e.Journal.Records = nil
			e.Journal.Segments = nil
		}
		if j.hot {
			codes["journal-hot"] = true
			seen := map[uint32]int{}
			for _, r := range j.recs {
				if r.ckOK {
					seen[r.page]++
					if seen[r.page] == 2 {
						codes["journal-duplicate-page"] = true
					}
				}
			}
		}
	}
	for c := range codes {
		e.Warnings.Live = append(e.Warnings.Live, c)
	}
	sort.Strings(e.Warnings.Live)

	e.History = fxBuildHistory(t, spec, sc, live, liveRaw)
	e.Unreachable = sc.unreach
	return g
}

// ---- history ----

type fxImage struct {
	pgno   uint32
	file   string
	off    int64
	data   []byte
	origin string
}

var fxMethodBase = map[string]int{
	"sqlite-wal-prior": 75, "sqlite-journal-before": 70, "sqlite-freelist": 60, "sqlite-wal-stale": 55,
	"sqlite-wal-uncommitted": 45, "sqlite-journal-rolledback": 45, "sqlite-journal-persist": 35,
}

var fxBasisCap = map[string]int{"schema": 100, "fit": 60, "guess": 40, "none": 30}

func fxMethodOf(origin string, zeroedJournal bool) string {
	switch origin {
	case "db-under-wal", "wal-superseded":
		return "sqlite-wal-prior"
	case "wal-uncommitted":
		return "sqlite-wal-uncommitted"
	case "wal-stale", "wal-unverified":
		return "sqlite-wal-stale"
	case "db-rolled-back":
		return "sqlite-journal-rolledback"
	case "journal-before":
		if zeroedJournal {
			return "sqlite-journal-persist"
		}
		return "sqlite-journal-before"
	case "freelist-leaf":
		return "sqlite-freelist"
	}
	return ""
}

func fxIsLeafTable(page []byte, pgno uint32) bool {
	b := fxHeaderBase(pgno)
	return len(page) > b+8 && page[b] == 0x0d
}

// fxPersistRecords finds the before-images in a journal whose header was zeroed:
// the first record follows the header sector; the sector size is the one for
// which the most records look like b-tree pages.
func fxPersistRecords(j []byte, ps int, dbPages uint32) []fxImage {
	var best []fxImage
	for _, sector := range []int{512, 1024, 2048, 4096} {
		var imgs []fxImage
		for p := sector; p+4+ps+4 <= len(j); p += 4 + ps + 4 {
			pg := be32(j[p:])
			data := j[p+4 : p+4+ps]
			b := fxHeaderBase(pg)
			if pg == 0 || pg > dbPages+16 || len(data) <= b || (data[b] != 0x0d && data[b] != 0x05 && data[b] != 0x0a && data[b] != 0x02) {
				break
			}
			imgs = append(imgs, fxImage{pgno: pg, file: "journal", off: int64(p + 4), data: data, origin: "journal-before"})
		}
		if len(imgs) > len(best) {
			best = imgs
		}
	}
	return best
}

func fxCollectImages(f fxFiles, live *fxState) (imgs []fxImage, zeroed bool) {
	ps := live.ps
	dbPages := uint32(len(f.db) / ps)
	dbImg := func(p uint32, origin string) {
		o := int64(p-1) * int64(ps)
		if p >= 1 && o+int64(ps) <= int64(len(f.db)) {
			imgs = append(imgs, fxImage{pgno: p, file: "db", off: o, data: f.db[o : o+int64(ps)], origin: origin})
		}
	}
	if live.wal != nil && live.wal.headerValid {
		w := live.wal
		perPage := map[uint32][]int{}
		for i, fr := range w.frames {
			if fr.state == "committed" {
				perPage[fr.page] = append(perPage[fr.page], i)
			}
		}
		pages := make([]uint32, 0, len(perPage))
		for p := range perPage {
			pages = append(pages, p)
		}
		sort.Slice(pages, func(a, b int) bool { return pages[a] < pages[b] })
		for _, p := range pages {
			dbImg(p, "db-under-wal")
		}
		for i, fr := range w.frames {
			var origin string
			switch fr.state {
			case "committed":
				l := perPage[fr.page]
				if l[len(l)-1] == i {
					continue
				}
				origin = "wal-superseded"
			case "uncommitted":
				origin = "wal-uncommitted"
			case "stale":
				origin = "wal-stale"
			default:
				origin = "wal-unverified"
			}
			if fr.page == 0 {
				continue
			}
			o := fr.off + 24
			imgs = append(imgs, fxImage{pgno: fr.page, file: "wal", off: o, data: f.wal[o : o+int64(ps)], origin: origin})
		}
	}
	if live.jr != nil {
		j := live.jr
		if j.hot {
			for _, r := range j.recs {
				if r.ckOK && r.page != 0 {
					imgs = append(imgs, fxImage{pgno: r.page, file: "journal", off: r.off, data: f.journal[r.off : r.off+int64(ps)], origin: "journal-before"})
				}
			}
			pages := make([]uint32, 0, len(j.winner))
			for p := range j.winner {
				pages = append(pages, p)
			}
			sort.Slice(pages, func(a, b int) bool { return pages[a] < pages[b] })
			for _, p := range pages {
				if p <= j.initial {
					dbImg(p, "db-rolled-back")
				}
			}
		} else if j.zeroed {
			zeroed = true
			imgs = append(imgs, fxPersistRecords(f.journal, ps, dbPages)...)
		}
	}
	if f.wal == nil && f.journal == nil {
		_, leaves := fxFreelistOf(live)
		for _, p := range leaves {
			dbImg(p, "freelist-leaf")
		}
	}
	return imgs, zeroed
}

func fxSameRaw(a, b []any) bool { return sameAll(a, b) }

func fxBuildHistory(t *testing.T, spec fixtureSpec, sc *fxScenario, live *fxState, liveRaw map[string]map[int64][]any) []fxHistory {
	t.Helper()
	out := []fxHistory{}
	if len(sc.wants) == 0 {
		return out
	}
	imgs, zeroed := fxCollectImages(sc.files, live)
	found := make([]int, len(sc.wants))
	for _, im := range imgs {
		if !fxIsLeafTable(im.data, im.pgno) {
			continue
		}
		cells, err := live.leafCells(im.data, im.pgno, im.file, im.off)
		if err != nil {
			continue // a damaged image yields what it yields; the walker is strict
		}
		for _, c := range cells {
			for wi, w := range sc.wants {
				if w.rowid != nil && *w.rowid != c.rowid {
					continue
				}
				if !fxSameRaw(c.vals, fxNormalizeWant(w.vals)) {
					continue
				}
				basis := w.basis
				if basis == "" {
					basis = "schema"
				}
				var extraNotes []string
				// A stale, broken or detached WAL frame has no as-of schema (the
				// database state of its time is unknown), so the table of its rows
				// can only be named by the fit (rulings C44/C46): fit basis, relation
				// unknown, owner-changed and identity-by-fit-only. The fixtures keep
				// one table per fit, so the fit names the table the statements wrote.
				if basis == "schema" && (im.origin == "wal-stale" || im.origin == "wal-unverified") {
					basis = "fit"
					extraNotes = []string{"owner-changed"}
				}
				method := fxMethodOf(im.origin, zeroed)
				table := w.label
				if w.basis == "" {
					table = w.table
				}
				if basis == "schema" {
					table = w.table
				}
				if basis == "none" {
					table = ""
				}
				rel := "unknown"
				uncommitted := im.origin == "wal-uncommitted" || im.origin == "db-rolled-back"
				if uncommitted && basis == "schema" {
					rel = "uncommitted" // a fit or guess row stays unknown (ruling C47)
				}
				if basis == "schema" {
					lr, ok := liveRaw[strings.ToLower(w.table)][c.rowid]
					switch {
					case ok && fxSameRaw(lr, c.vals):
						continue // identical to the live row: not a recovered row, whatever its origin
					case uncommitted:
					case !ok:
						rel = "absent-from-live"
					default:
						rel = "superseded-version"
					}
				}
				h := fxHistory{
					Table: table, Method: method, Relation: rel, Origin: im.origin, Basis: basis,
					Confidence: min(fxMethodBase[method], fxBasisCap[basis]), Notes: []string{}, Cell: fxLocOf(c),
				}
				rid := c.rowid
				h.Rowid = &rid
				for _, v := range c.vals {
					h.Values = append(h.Values, fxEnc(v))
				}
				if basis == "fit" || basis == "guess" {
					h.Notes = append(h.Notes, extraNotes...)
					h.Notes = append(h.Notes, "identity-by-fit-only")
				}
				out = append(out, h)
				found[wi]++
			}
		}
	}
	for wi, w := range sc.wants {
		if w.required && found[wi] == 0 {
			t.Fatalf("%s: required history version of %s rowid %v (%v) is in no page image", spec.Name, w.table, w.rowid, w.vals)
		}
	}
	if len(out) < sc.minHistory {
		t.Fatalf("%s: the oracle finds %d history entries, the scenario needs at least %d (%d images, journal hot %v, magic % x)", spec.Name, len(out), sc.minHistory, len(imgs), live.useJournal, sc.files.journal[:min(8, len(sc.files.journal))])
	}
	return out
}

// fxNormalizeWant maps Go ints to int64 so they compare with decoded records.
func fxNormalizeWant(vs []any) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		switch x := v.(type) {
		case int:
			out[i] = int64(x)
		case int32:
			out[i] = int64(x)
		default:
			out[i] = v
		}
	}
	return out
}

var _ = math.Pi

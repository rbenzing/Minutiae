package iosaddressbook_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

var createRE = regexp.MustCompile(`(?i)^CREATE TABLE (\w+) \((.*)\);$`)

// ddlTable is one CREATE TABLE of a DDL snapshot: its text and its column names in order.
type ddlTable struct {
	ddl  string
	cols []string
	pk   bool
}

// snapshot reads the tables of an iOS DDL snapshot of tools/fixtures/schemas/ios, so the tests build
// databases with exactly the layout the fixtures use.
func snapshot(t testing.TB, file string) map[string]ddlTable {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(schemasPath, file))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ddlTable{}
	for _, line := range strings.Split(string(data), "\n") {
		m := createRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		var cols []string
		depth, start := 0, 0
		body := m[2]
		flush := func(end int) {
			part := strings.TrimSpace(body[start:end])
			if part != "" && !strings.HasPrefix(strings.ToUpper(part), "UNIQUE") {
				cols = append(cols, strings.Fields(part)[0])
			}
		}
		for i, c := range body {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					flush(i)
					start = i + 1
				}
			}
		}
		flush(len(body))
		out[m[1]] = ddlTable{ddl: strings.TrimSuffix(line, ";"), cols: cols, pk: strings.Contains(strings.ToUpper(m[2]), "INTEGER PRIMARY KEY")}
	}
	return out
}

// book is an address book database under construction.
type book struct {
	t      testing.TB
	b      *sqlitetest.Builder
	defs   map[string]ddlTable
	tables map[string]*sqlitetest.Table
	uid    int64
	eid    int64
}

// newBook creates the named tables of the snapshot (all of them when none is named).
func newBook(t testing.TB, file string, names ...string) *book {
	t.Helper()
	k := &book{t: t, b: sqlitetest.New(sqlitetest.Options{PageSize: 1024}), defs: snapshot(t, file), tables: map[string]*sqlitetest.Table{}}
	if len(names) == 0 {
		for n := range k.defs {
			names = append(names, n)
		}
	}
	for _, n := range names {
		d, ok := k.defs[n]
		if !ok {
			t.Fatalf("%s has no table %s", file, n)
		}
		k.tables[n] = k.b.CreateTable(n, d.ddl)
	}
	return k
}

// row inserts a row by column name; a column not named is NULL. The rowid alias column, when the table has
// one, takes the rowid.
func (k *book) row(table string, id int64, kv map[string]any) {
	k.t.Helper()
	d, tb := k.defs[table], k.tables[table]
	if tb == nil {
		k.t.Fatalf("table %s was not created", table)
	}
	vals := make([]any, len(d.cols))
	for i, c := range d.cols {
		if v, ok := kv[c]; ok {
			vals[i] = v
		} else if i == 0 && d.pk {
			vals[i] = id
		}
	}
	for key := range kv {
		found := false
		for _, c := range d.cols {
			found = found || c == key
		}
		if !found {
			k.t.Fatalf("table %s has no column %s", table, key)
		}
	}
	tb.Insert(id, vals...)
}

// person inserts an ABPerson row (Kind defaults to 0).
func (k *book) person(id int64, kv map[string]any) {
	k.t.Helper()
	m := map[string]any{"Kind": int64(0)}
	for key, v := range kv {
		m[key] = v
	}
	k.row("ABPerson", id, m)
}

// mv inserts an ABMultiValue row with the next UID and returns it.
func (k *book) mv(rec, prop int64, label any, value any) int64 {
	k.t.Helper()
	k.uid++
	k.row("ABMultiValue", k.uid, map[string]any{"record_id": rec, "property": prop, "label": label, "value": value})
	return k.uid
}

// labels / keys insert the rows of the two name tables; the first has rowid 1.
func (k *book) labels(names ...string) {
	for i, n := range names {
		k.row("ABMultiValueLabel", int64(i+1), map[string]any{"value": n})
	}
}

func (k *book) keys(names ...string) {
	for i, n := range names {
		k.row("ABMultiValueEntryKey", int64(i+1), map[string]any{"value": n})
	}
}

func (k *book) entry(uid, key int64, value any) {
	k.t.Helper()
	k.eid++
	k.row("ABMultiValueEntry", k.eid, map[string]any{"parent_id": uid, "key": key, "value": value})
}

func (k *book) bytes() []byte { return k.b.Bytes() }

// runBook runs the parser over a database placed as a file acquisition.
func runBook(t *testing.T, db []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}}.BundleIOS(h, remote, logical)
	p := iosaddressbook.Parser{}
	return h.RunLenient(p, b.Input(h, p, false))
}

// runWithImages runs the parser over a backup holding the images database (and its WAL when given).
func runWithImages(t *testing.T, db, images, imagesWAL []byte) parsertest.Result {
	t.Helper()
	return runWithImagesP(t, iosaddressbook.Parser{}, db, images, imagesWAL)
}

// runWithImagesP is runWithImages with a given parser value.
func runWithImagesP(t *testing.T, p iosaddressbook.Parser, db, images, imagesWAL []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	files := map[string][]byte{"db": db}
	if images != nil {
		files["images"] = images
	}
	if imagesWAL != nil {
		files["images_wal"] = imagesWAL
	}
	b := parsertest.Fixture{Name: "inline", Files: files}.BundleIOSBackup(h, "HomeDomain", relative)
	return h.RunLenient(p, b.Input(h, p, false))
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func asList(v any) []any { l, _ := v.([]any); return l }

func rawIn(r records.Record) map[string]any { return asMap(r.Payload["raw"]) }

func byRow(t *testing.T, res parsertest.Result) map[int64]records.Record {
	t.Helper()
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		var id int64
		rest := strings.TrimPrefix(r.Locator, "sqlite:table=ABPerson;rowid=")
		for _, c := range rest {
			if c < '0' || c > '9' {
				break
			}
			id = id*10 + int64(c-'0')
		}
		if id == 0 {
			t.Fatalf("locator %q", r.Locator)
		}
		out[id] = r
	}
	return out
}

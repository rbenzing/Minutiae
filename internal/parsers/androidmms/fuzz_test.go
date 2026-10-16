package androidmms_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// FuzzParseMMSSMS mutates whole databases (and their WAL and journal) and requires the
// contract to hold: no panic, no refused record, and only typed errors. Seeds run under
// go test; go test -fuzz is run manually.
func FuzzParseMMSSMS(f *testing.F) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(f, "testdata", v)
		f.Add(fx.Files["db"], fx.Files["wal"], fx.Files["journal"])
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, androidmms.Parser{}, remotePath, db, wal, journal)
	})
}

// Page-level hostile mutations of the small UTF-16 fixture, deterministic: zeroed pages,
// a flipped page type, an absurd cell count, in every page.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "utf16")
	db := fx.Files["db"]
	ps := int(db[16])<<8 | int(db[17])
	if ps == 1 {
		ps = 65536
	}
	pages := len(db) / ps
	for pg := 0; pg < pages; pg++ {
		for name, mut := range map[string]func(p []byte){
			"zeroed":      func(p []byte) { clear(p) },
			"type flip":   func(p []byte) { p[0] ^= 0x08 },
			"cell count":  func(p []byte) { p[3], p[4] = 0xff, 0xff },
			"first free":  func(p []byte) { p[1], p[2] = 0xff, 0xff },
			"cell offset": func(p []byte) { p[8], p[9] = 0xff, 0xf0 },
		} {
			m := append([]byte(nil), db...)
			page := m[pg*ps : (pg+1)*ps]
			if pg == 0 {
				page = page[100:] // keep the file header: it is covered by its own test
			}
			mut(page)
			t.Run(name, func(t *testing.T) { parsertest.FuzzParse(t, androidmms.Parser{}, remotePath, m, nil, nil) })
		}
	}
}

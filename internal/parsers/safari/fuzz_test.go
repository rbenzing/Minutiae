package safari_test

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
)

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every execution, so the
// fuzzer explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalSeed is a hand-built database of 512-byte pages: one item with one visit, and one tombstone.
func minimalSeed() []byte {
	d := newDB(512)
	d.item(1, "https://a.example/", int64(1))
	d.visit(1, 1, tVisit, "A")
	d.tomb.Insert(1, int64(1), tVisit-60, tVisit, "https://gone.example/", int64(0))
	return d.b.Bytes()
}

// seedVariants are the committed fixtures small enough to seed a fuzzer; current and current-wal (240 KiB
// databases) are not, and are covered by the page-level mutation tests instead.
var seedVariants = []string{"no-tombstones"}

// fuzzSeeds are the hand-built minimal database and the small committed fixtures.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := []fuzzSeed{{name: "minimal", db: minimalSeed()}}
	for _, v := range seedVariants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"]})
	}
	return seeds
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	seeds := fuzzSeeds(t)
	if len(seeds) != len(seedVariants)+1 {
		t.Errorf("%d seeds for %d fixtures and the minimal database", len(seeds), len(seedVariants))
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
}

// FuzzParseSafari mutates whole databases (and their WAL and journal) and requires the contract to hold:
// no panic, no refused record, and only typed errors. It runs through the iOS entry point, because the
// harness refuses an iOS parser's record on an Android artifact (4F-R25). Seeds run under go test;
// go test -fuzz is run manually.
func FuzzParseSafari(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParseIOS(t, safari.Parser{}, remoteSafari, logicalSafari, db, wal, journal)
	})
}

// The fuzz bundle must reach the parser as an iOS bundle: the minimal seed gives records under the strict
// harness, which refuses a record whose artifact platform the parser does not declare (4F-R25). A seed that
// only produced refusals would let the fuzz target pass without ever testing a mapper.
func TestSafariFuzzSeedEmitsRecords(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "fuzz", Files: map[string][]byte{"db": minimalSeed()}}.BundleIOS(h, remoteSafari, logicalSafari)
	p := safari.Parser{}
	res := h.Run(p, b.Input(h, p, false))
	if res.Err != nil || len(res.Violations) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("err %v, violations %v, warnings %+v", res.Err, res.Violations, res.Warnings)
	}
	var visits, tombs int
	for _, r := range res.Records {
		if isTombstone(r.Locator) {
			tombs++
		} else {
			visits++
		}
	}
	if visits != 1 || tombs != 1 {
		t.Errorf("%d visit and %d tombstone records, want 1 and 1", visits, tombs)
	}
}

func pageSize(db []byte) int {
	ps := int(binary.BigEndian.Uint16(db[16:18]))
	if ps == 1 {
		return 65536
	}
	return ps
}

// samplePages lists the page indexes to damage: all of them up to limit, else page 0 and limit-1 more spread
// evenly over the file (a 60-page database costs 0.3 s a run, so every page of it is not affordable).
func samplePages(pages, limit int) []int {
	if pages <= limit {
		out := make([]int, pages)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := []int{0}
	for i := 1; i < limit; i++ {
		out = append(out, i*(pages-1)/(limit-1))
	}
	return out
}

// Page-level hostile mutations, deterministic: zeroed pages, a flipped page type, an absurd cell count, a
// wild first-free block and a wild cell offset, in every page of no-tombstones and in ten pages of the
// current-wal database spread over the file, each fuzzed through the iOS entry point.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte){
		"zeroed":      func(p []byte) { clear(p) },
		"type flip":   func(p []byte) { p[0] ^= 0x08 },
		"cell count":  func(p []byte) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte) { p[8], p[9] = 0xff, 0xf0 },
	}
	for _, v := range []string{"no-tombstones", "current-wal"} {
		fx := parsertest.LoadFixture(t, "testdata", v)
		db, wal := fx.Files["db"], fx.Files["wal"]
		ps := pageSize(db)
		if len(db) < 2*ps {
			t.Fatalf("%s: %d bytes is not a database with a table to damage", v, len(db))
		}
		for _, pg := range samplePages(len(db)/ps, 10) {
			for name, mut := range muts {
				m := append([]byte(nil), db...)
				page := m[pg*ps : (pg+1)*ps]
				if pg == 0 {
					page = page[100:] // keep the file header: it is covered by its own test
				}
				mut(page)
				t.Run(fmt.Sprintf("%s/page %d/%s", v, pg, name), func(t *testing.T) {
					parsertest.FuzzParseIOS(t, safari.Parser{}, remoteSafari, logicalSafari, m, wal, nil)
				})
			}
		}
	}
}

// The WAL of current-wal with its file header and each frame header damaged: the frames are a 24-byte
// header (page number, commit size, salts, checksums) before each page.
func TestWALHeaderMutationsNeverBreakTheContract(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "current-wal")
	db, wal := fx.Files["db"], fx.Files["wal"]
	ps := pageSize(db)
	const fileHdr, frameHdr = 32, 24
	if len(wal) < fileHdr+frameHdr+ps {
		t.Fatalf("the WAL holds %d bytes, not a frame", len(wal))
	}
	fileMuts := map[string]func(h []byte){
		"zeroed":   func(h []byte) { clear(h) },
		"magic":    func(h []byte) { h[0] ^= 0x01 },
		"version":  func(h []byte) { h[4], h[5], h[6], h[7] = 0xff, 0xff, 0xff, 0xff },
		"pagesize": func(h []byte) { h[8], h[9], h[10], h[11] = 0xff, 0xff, 0xff, 0xff },
		"salt":     func(h []byte) { h[16] ^= 0xff },
		"checksum": func(h []byte) { h[24] ^= 0xff },
	}
	frameMuts := map[string]func(h []byte){
		"zeroed":      func(h []byte) { clear(h) },
		"page number": func(h []byte) { h[0], h[1], h[2], h[3] = 0xff, 0xff, 0xff, 0xff },
		"page zero":   func(h []byte) { h[0], h[1], h[2], h[3] = 0, 0, 0, 0 },
		"commit size": func(h []byte) { h[4], h[5], h[6], h[7] = 0xff, 0xff, 0xff, 0xff },
		"checksum":    func(h []byte) { h[16] ^= 0xff },
	}
	for name, mut := range fileMuts {
		m := append([]byte(nil), wal...)
		mut(m[:fileHdr])
		t.Run("file header/"+name, func(t *testing.T) {
			parsertest.FuzzParseIOS(t, safari.Parser{}, remoteSafari, logicalSafari, db, m, nil)
		})
	}
	frames := (len(wal) - fileHdr) / (frameHdr + ps)
	if frames == 0 {
		t.Fatal("no complete WAL frame")
	}
	for f := range frames {
		at := fileHdr + f*(frameHdr+ps)
		for name, mut := range frameMuts {
			m := append([]byte(nil), wal...)
			mut(m[at : at+frameHdr])
			t.Run(fmt.Sprintf("frame %d/%s", f, name), func(t *testing.T) {
				parsertest.FuzzParseIOS(t, safari.Parser{}, remoteSafari, logicalSafari, db, m, nil)
			})
		}
	}
}

package chromium_test

import (
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func probeBytes(t *testing.T, db []byte) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := chromium.Parser{}
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}}.Bundle(h, remoteChrome)
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMetaAndProbe(t *testing.T) {
	p := chromium.Parser{}
	m := p.Meta()
	if err := parse.ValidateMeta(m); err != nil || m.Name != "chromium.history" {
		t.Fatalf("meta %+v: %v", m, err)
	}
	if chromium.Name != "chromium.history" || m.Version != chromium.Version {
		t.Errorf("identity %q %q", chromium.Name, chromium.Version)
	}
	if len(m.Emits) != 3 || m.Emits[0].Type != "web_visit" || m.Emits[1].Type != "web_search" || m.Emits[2].Type != "download" {
		t.Errorf("emits %+v, want web_visit, web_search, download", m.Emits)
	}
	if !slices.Equal(m.Platforms, []string{parse.PlatformAndroid}) {
		t.Errorf("platforms %v, want android only", m.Platforms)
	}
	if len(m.Inputs) != 3 || len(m.Inputs[0].Globs) != 2 || !m.Inputs[0].Required ||
		m.Inputs[0].Globs[0] != "android:**/app_chrome/*/History" || m.Inputs[0].Globs[1] != "android:**/app_webview/**/History" ||
		m.Inputs[1].Role != "wal" || !slices.Equal(m.Inputs[1].Companions, []string{"-wal"}) ||
		m.Inputs[2].Role != "journal" || !slices.Equal(m.Inputs[2].Companions, []string{"-journal"}) {
		t.Errorf("inputs %+v", m.Inputs)
	}
	if got := chromium.ClaimableTables(); !slices.Equal(got, []string{"visits", "keyword_search_terms", "downloads"}) {
		t.Errorf("ClaimableTables = %v", got)
	}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
		if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}

	db := func(tables ...[2]string) []byte {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
		for _, tb := range tables {
			b.CreateTable(tb[0], tb[1])
		}
		return b.Bytes()
	}
	if got := probeBytes(t, db([2]string{"urls", urlsDDL})); got.Status != parse.NotApplicable {
		t.Errorf("only urls: %+v, want not-applicable", got)
	}
	got := probeBytes(t, db([2]string{"urls", urlsDDL}, [2]string{"visits", "create table visits(id integer primary key, url integer, visit_time integer)"}))
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "visits") || !strings.Contains(got.Reason, "transition") {
		t.Errorf("visits without transition: %+v, want unsupported-schema naming visits and transition", got)
	}
	got = probeBytes(t, db([2]string{"history_items", "create table history_items(id integer primary key, url text)"},
		[2]string{"history_visits", "create table history_visits(id integer primary key, history_item integer, visit_time real)"}))
	if got.Status != parse.NotApplicable {
		t.Errorf("a Safari-shaped database: %+v, want not-applicable", got)
	}
	got = probeBytes(t, db([2]string{"urls", urlsDDL}, [2]string{"visits", visitsDDL},
		[2]string{"downloads", "create table downloads(id integer primary key, target_path text, start_time integer)"}))
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "downloads_url_chains") {
		t.Errorf("downloads without downloads_url_chains: %+v, want unsupported-schema", got)
	}
	// urls whose id is not the rowid alias cannot be joined by rowid (4F-R30)
	got = probeBytes(t, db([2]string{"urls", "create table urls(id integer, url text, title text, last_visit_time integer)"}, [2]string{"visits", visitsDDL}))
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "rowid alias") {
		t.Errorf("urls.id that is no rowid alias: %+v, want unsupported-schema", got)
	}
	// 4096 bytes of hash output look like ciphertext
	var enc []byte
	for i := uint64(0); len(enc) < 4096; i++ {
		var c [8]byte
		binary.LittleEndian.PutUint64(c[:], i)
		sum := sha256.Sum256(c[:])
		enc = append(enc, sum[:]...)
	}
	if got := probeBytes(t, enc); got.Status != parse.Encrypted {
		t.Errorf("high-entropy data: %+v, want encrypted", got)
	}
}

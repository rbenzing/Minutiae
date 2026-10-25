package safari_test

import (
	"crypto/sha256"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func probeOf(t *testing.T, db []byte) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}}.BundleIOS(h, remoteSafari, logicalSafari)
	p := safari.Parser{}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return got
}

func TestMetaAndProbe(t *testing.T) {
	p := safari.Parser{}
	m := p.Meta()
	if err := parse.ValidateMeta(m); err != nil || len(m.Claims) != 0 || m.Name != safari.Name || m.Version != safari.Version {
		t.Fatalf("meta %+v: %v", m, err)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS || len(m.Emits) != 1 || m.Emits[0].Type != "web_visit" {
		t.Errorf("platforms %v emits %v", m.Platforms, m.Emits)
	}
	roles := map[string]bool{}
	for _, in := range m.Inputs {
		roles[in.Role] = true
	}
	if len(m.Inputs) != 3 || !roles["db"] || !roles["wal"] || !roles["journal"] || !m.Inputs[0].Required || m.Inputs[0].Globs[0] != "ios:*/Library/Safari/History.db" {
		t.Errorf("inputs %+v", m.Inputs)
	}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", v).BundleIOS(h, remoteSafari, logicalSafari)
		if got, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}

	// a Chromium-shaped database has none of Safari's identifying tables
	bld := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("urls", "create table urls(id integer primary key, url text, title text, last_visit_time integer)")
	bld.CreateTable("visits", "create table visits(id integer primary key, url integer, visit_time integer)")
	if got := probeOf(t, bld.Bytes()); got.Status != parse.NotApplicable {
		t.Errorf("chromium-shaped database: %+v, want not-applicable", got)
	}

	// history_visits without visit_time is a loud unsupported schema
	bld = sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("history_items", itemsDDL)
	bld.CreateTable("history_visits", "create table history_visits(id integer primary key, history_item integer, title text)")
	if got := probeOf(t, bld.Bytes()); got.Status != parse.UnsupportedSchema {
		t.Errorf("history_visits without visit_time: %+v, want unsupported-schema", got)
	}

	// the join by rowid is only right when id is the rowid alias
	bld = sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("history_items", "create table history_items(id integer, url text, visit_count integer)")
	bld.CreateTable("history_visits", visitsDDL)
	if got := probeOf(t, bld.Bytes()); got.Status != parse.UnsupportedSchema {
		t.Errorf("history_items.id that is not the rowid alias: %+v, want unsupported-schema", got)
	}

	noise := make([]byte, 0, 16384)
	for seed := byte(0); len(noise) < 16384; seed++ {
		sum := sha256.Sum256([]byte{seed})
		noise = append(noise, sum[:]...)
	}
	if got := probeOf(t, noise); got.Status != parse.Encrypted {
		t.Errorf("high-entropy bytes: %+v, want encrypted", got)
	}
}

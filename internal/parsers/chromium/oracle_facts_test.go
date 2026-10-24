package chromium_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// packageOf is the directory before the last app_chrome / app_webview segment of a remote path (4F-R32).
func packageOf(remote string) string {
	parts := strings.Split(remote, "/")
	for i := len(parts) - 1; i > 0; i-- {
		if parts[i] == "app_chrome" || parts[i] == "app_webview" {
			return parts[i-1]
		}
	}
	return ""
}

func rawOf(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	raw, ok := p["raw"].(map[string]any)
	if !ok {
		t.Fatalf("payload without raw: %v", p)
	}
	return raw
}

// The browser comes from the path, never from the data (facts written from the plan, 4F-R5).
func TestOracleStatesBrowserFromThePath(t *testing.T) {
	browser := map[string]string{"current": "chrome", "current-wal": "chrome", "mid-upgraded": "edge", "old": "brave", "minimal": "webview"}
	profile := map[string]string{"current": "Default", "current-wal": "Default", "mid-upgraded": "Default", "old": "Profile 1", "minimal": "Default"}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			t.Fatalf("%s: no records", v)
		}
		for _, r := range fx.Expect.Records {
			if r.Payload["browser"] != browser[v] {
				t.Errorf("%s %s rowid %d: browser %v, want %s", v, r.Table, r.Rowid, r.Payload["browser"], browser[v])
			}
			raw := rawOf(t, r.Payload)
			if raw["package"] != packageOf(remoteFor(v)) {
				t.Errorf("%s %s rowid %d: raw.package %v, want %s", v, r.Table, r.Rowid, raw["package"], packageOf(remoteFor(v)))
			}
			// only a web_visit has a profile slot in its payload; searches and downloads keep it in raw
			got := r.Payload["profile"]
			if r.Type != "web_visit" {
				got = raw["profile"]
			}
			if got != profile[v] {
				t.Errorf("%s %s rowid %d: profile %v, want %s", v, r.Table, r.Rowid, got, profile[v])
			}
		}
	}
}

func TestOracleStatesTheSourcePathOfEveryRow(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		for _, r := range fx.Expect.Records {
			want := remoteFor(v)
			switch {
			case r.File == "wal":
				want += "-wal"
			case v == "current-wal":
				want = "" // the cell may lie in either file: the comparer derives the path from the locator
			}
			if r.SourcePath != want {
				t.Errorf("%s %s rowid %d: source_path %q, want %q", v, r.Table, r.Rowid, r.SourcePath, want)
			}
		}
	}
}

// A server redirect is stored as a negative int32 (the qualifier bit 0x80000000); the oracle reads it as uint32.
func TestOracleStatesTheServerRedirectVisit(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "current")
	n := 0
	for _, r := range fx.Expect.Records {
		if r.Table != "visits" {
			continue
		}
		if tr, _ := rawOf(t, r.Payload)["transition"].(json.Number); tr.String() != "-1610612736" {
			continue
		}
		n++
		q, _ := r.Payload["transition_qualifiers"].([]any)
		if r.Payload["transition"] != "link" || len(q) != 2 || q[0] != "chain_end" || q[1] != "server_redirect" {
			t.Errorf("visit %d: transition %v qualifiers %v", r.Rowid, r.Payload["transition"], q)
		}
	}
	if n != 1 {
		t.Errorf("%d visits stored as -1610612736, want 1", n)
	}
}

// A visit whose urls row is missing, or holds no usable url, is a warning and never a record (4F-R4).
func TestOracleStatesDanglingAndEmptyURLVisitsAsWarnings(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "current")
	var warned []int64
	for _, w := range fx.Expect.Warnings {
		if w.Table == "visits" {
			warned = append(warned, w.Rowid)
		}
	}
	if len(warned) != 2 {
		t.Fatalf("visits warnings %v, want two", warned)
	}
	for _, r := range fx.Expect.Records {
		for _, w := range warned {
			if r.Table == "visits" && r.Rowid == w {
				t.Errorf("visits rowid %d has both a warning and a record", w)
			}
		}
	}
	if fx.Expect.Notes["unmapped_rows.visits"] != "2" {
		t.Errorf("note unmapped_rows.visits = %q, want 2", fx.Expect.Notes["unmapped_rows.visits"])
	}
}

func TestOracleWALRowsComeFromTheWAL(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "current-wal")
	if _, ok := fx.Files["wal"]; !ok {
		t.Fatal("current-wal has no wal file")
	}
	n := 0
	for _, r := range fx.Expect.Records {
		if r.Table == "visits" && r.File == "wal" {
			n++
		}
	}
	if n < 3 {
		t.Errorf("%d visits rows in the wal, want at least 3", n)
	}
}

// Every committed file is at most 32 KiB compressed, every oracle at most 400 KB, all of them under 2 MiB.
func TestFixtureFilesAreCompact(t *testing.T) {
	total := int64(0)
	m, err := filepath.Glob("testdata/*")
	if err != nil || len(m) == 0 {
		t.Fatalf("no fixture files (%v)", err)
	}
	for _, p := range m {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		limit := int64(32 << 10)
		if strings.HasSuffix(p, ".expect.json") {
			limit = 400_000
		}
		if st.Size() > limit {
			t.Errorf("%s is %d bytes, over %d", p, st.Size(), limit)
		}
		total += st.Size()
	}
	if total > 2<<20 {
		t.Errorf("fixtures total %d bytes, over 2 MiB", total)
	}
}

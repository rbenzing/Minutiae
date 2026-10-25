package chromium_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

const ddlDir = "../../../tools/fixtures/schemas/chromium"

// Every DDL snapshot is the schema of at least one fixture, and every fixture names a Chromium snapshot.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(ddlDir, "chromium-*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		used[parsertest.LoadFixture(t, "testdata", v).Expect.DDL] = true
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "chromium-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "current").Bundle(h, remoteChrome)
	p := chromium.Parser{}
	parsertest.AssertDeterministic(t, func() parsertest.Result {
		res := h.RunLenient(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
			t.Errorf("err %v, violations %v, %d records", res.Err, res.Violations, len(res.Records))
		}
		return res
	})
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "current").Bundle(h, remoteChrome)
	p := chromium.Parser{}
	res := h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
	noViolations(t, res)
}

// labelsOf is the browser, profile and package a record states. A web_visit has a profile slot in its payload; a
// search and a download keep the profile in raw (4F-C1).
func labelsOf(r records.Record) (browser, profile, pkg string) {
	raw := asMap(r.Payload["raw"])
	browser, _ = r.Payload["browser"].(string)
	pkg, _ = raw["package"].(string)
	if r.Type == "web_visit" {
		profile, _ = r.Payload["profile"].(string)
	} else {
		profile, _ = raw["profile"].(string)
	}
	return browser, profile, pkg
}

// unlabelled is the record without the four path-derived label keys and without the bundle's own identity
// (artifact id and source path).
func unlabelled(r records.Record) records.Record {
	p := map[string]any{}
	for k, v := range r.Payload {
		p[k] = v
	}
	delete(p, "browser")
	delete(p, "profile")
	raw := map[string]any{}
	for k, v := range asMap(r.Payload["raw"]) {
		raw[k] = v
	}
	delete(raw, "package")
	delete(raw, "profile")
	p["raw"] = raw
	r.Payload = p
	r.ArtifactID, r.SourcePath = "", ""
	return r
}

// Review focus 3: the browser, profile and package come from the path alone. The same database under four paths
// gives the same records except for exactly those labels, and the labels are the path's.
func TestSameFixtureRelabelledByPath(t *testing.T) {
	type label struct{ browser, profile, pkg string }
	cases := []struct {
		remote string
		want   label
	}{
		{remoteChrome, label{"chrome", "Default", "com.android.chrome"}},
		{remoteBrave, label{"brave", "Profile 1", "com.brave.browser"}},
		{"/data/data/com.example.x/app_chrome/Default/History", label{"unknown-chromium", "Default", "com.example.x"}},
		{remoteWebView, label{"webview", "Default", "com.example.reader"}},
	}
	var base []records.Record
	for i, tc := range cases {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", "current").Bundle(h, tc.remote)
		p := chromium.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		noViolations(t, res)
		if res.Err != nil || len(res.Records) == 0 {
			t.Fatalf("%s: err %v, %d records", tc.remote, res.Err, len(res.Records))
		}
		got := make([]records.Record, len(res.Records))
		for j, r := range res.Records {
			if br, pr, pk := labelsOf(r); (label{br, pr, pk}) != tc.want {
				t.Fatalf("%s: record %s labelled %s/%s/%s, want %+v", tc.remote, r.Locator, br, pr, pk, tc.want)
			}
			got[j] = unlabelled(r)
		}
		if i == 0 {
			base = got
			continue
		}
		if len(got) != len(base) {
			t.Fatalf("%s: %d records, %d under the first path", tc.remote, len(got), len(base))
		}
		for j := range got {
			if !reflect.DeepEqual(got[j], base[j]) {
				t.Fatalf("%s: record %d (%s) differs beyond its labels:\n%+v\n%+v", tc.remote, j, got[j].Locator, got[j], base[j])
			}
		}
	}
}

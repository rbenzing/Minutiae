package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func rptr[T any](v T) *T { return &v }

type recCase struct {
	dir       string
	art, art2 evidence.ManifestRecord
}

var recParser = records.Parser{Name: "sms-parser", Version: "1.0", Hash: "abc123"}

// recDataset is a closed case holding five handcrafted records of the parser
// sms-parser 1.0 (ids 1..5) and, when second is set, two records of "calls 2.0"
// over a second artifact (ids 6, 7).
func recDataset(t *testing.T, second bool) recCase {
	t.Helper()
	c := recordstest.NewCase(t)
	rc := recCase{dir: c.Dir}
	rc.art = recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	rc.art2 = recordstest.AddArtifact(t, c, "b.db", make([]byte, recordstest.ArtifactSize))
	utc := time.Date(2026, 10, 4, 9, 12, 33, 500_000_000, time.UTC)
	recordstest.Ingest(t, c, recParser, []string{rc.art.ID}, []records.Record{
		{
			Type: "message", ArtifactID: rc.art.ID, Summary: "hello world", SourcePath: "/data/sms.db", Locator: "sqlite:table=sms;row=1",
			Time: &records.Time{T: utc}, Confidence: rptr(90), Payload: validPayload("message", map[string]any{"text": "hi", "n": int64(3)}),
		},
		{
			Type: "call", ArtifactID: rc.art.ID, Summary: "call to mum", SourcePath: "/data/calls.db",
			Time: &records.Time{T: time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC), Basis: records.BasisLocalUnknown}, Payload: validPayload("call", nil),
		},
		{
			Type: "note", ArtifactID: rc.art.ID, Summary: "a note",
			Time:    &records.Time{T: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC), Basis: records.BasisLocalOffset, OffsetMin: 120},
			TimeEnd: &records.Time{T: time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC), Basis: records.BasisLocalOffset, OffsetMin: 120},
			Body:    "first line\nsecond line", Payload: map[string]any{},
		},
		{Type: "file", ArtifactID: rc.art.ID, Summary: "removed file", Deleted: true, Payload: map[string]any{}},
		{
			Type: "contact", ArtifactID: rc.art.ID, Summary: "carved contact", Deleted: true, Recovery: "carve", Confidence: rptr(40),
			Time: &records.Time{T: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, Payload: validPayload("contact", nil),
		},
	})
	if second {
		recordstest.Ingest(t, c, records.Parser{Name: "calls", Version: "2.0"}, []string{rc.art2.ID}, []records.Record{
			{Type: "call", ArtifactID: rc.art2.ID, Summary: "second one", SourcePath: "/other/x", Time: &records.Time{T: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}, Payload: validPayload("call", nil)},
			{Type: "call", ArtifactID: rc.art2.ID, Summary: "second two", Payload: validPayload("call", nil)},
		})
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return rc
}

// cliJSON runs a command with --json and decodes its output into v.
func cliJSON(t *testing.T, v any, args ...string) {
	t.Helper()
	var errBuf bytes.Buffer
	code, out := run(t, Deps{Err: &errBuf}, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s%s", args, code, out, errBuf.String())
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("%v: output is not JSON: %v\n%s", args, err, out)
	}
}

type jsonRow struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	Summary    string `json:"summary"`
	ArtifactID string `json:"artifact_id"`
	SourcePath string `json:"source_path"`
	Locator    string `json:"locator"`
	Deleted    bool   `json:"deleted"`
	Recovered  bool   `json:"recovered"`
	Method     string `json:"recovery_method"`
	Confidence *int   `json:"confidence"`
	Superseded bool   `json:"superseded"`
	IngestID   string `json:"ingest_id"`
	TS         *struct {
		UnixMicro int64  `json:"unix_us"`
		Basis     string `json:"basis"`
		Display   string `json:"display"`
	} `json:"ts"`
	Parser struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"parser"`
}

type jsonList struct {
	Records    []jsonRow `json:"records"`
	NextCursor string    `json:"next_cursor"`
}

func listIDs(t *testing.T, args ...string) []int64 {
	t.Helper()
	var l jsonList
	cliJSON(t, &l, append([]string{"records", "list"}, args...)...)
	var ids []int64
	for _, r := range l.Records {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestRecordsListTextAndJSON(t *testing.T) {
	rc := recDataset(t, false)
	code, out := run(t, Deps{}, "records", "list", "--case", rc.dir)
	if code != 0 {
		t.Fatalf("list: %d %s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// ascending (untimed last): contact 2026-01-02, note 09:00Z, hello 09:12:33.5Z, call 10:00, then the untimed file
	if len(lines) != 5 {
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	want := []struct{ id, time, typ, marker, summary string }{
		{"5", "2026-01-02T03:04:05Z", "contact", "[deleted] [recovered:carve] [conf 40]", "carved contact"},
		{"3", "2026-10-04T11:00:00~local+02:00", "note", "", "a note"},
		{"1", "2026-10-04T09:12:33.5Z", "message", "[conf 90]", "hello world"},
		{"2", "2026-10-04T10:00:00~local", "call", "", "call to mum"},
		{"4", "-", "file", "[deleted]", "removed file"},
	}
	for i, w := range want {
		f := strings.Fields(lines[i])
		if len(f) < 4 || f[0] != w.id || f[1] != w.time || f[2] != w.typ {
			t.Errorf("line %d = %q, want id %s time %s type %s", i, lines[i], w.id, w.time, w.typ)
		}
		if !strings.Contains(lines[i], w.summary) || (w.marker != "" && !strings.Contains(lines[i], w.marker)) {
			t.Errorf("line %d = %q lacks %q / %q", i, lines[i], w.marker, w.summary)
		}
	}
	if strings.Contains(out, "[superseded]") || strings.Contains(out, "[artifact incomplete]") {
		t.Errorf("unexpected markers:\n%s", out)
	}

	var l jsonList
	cliJSON(t, &l, "records", "list", "--case", rc.dir)
	if len(l.Records) != 5 || l.NextCursor != "" {
		t.Fatalf("json: %d records, cursor %q", len(l.Records), l.NextCursor)
	}
	first := l.Records[0]
	if first.ID != 5 || first.Type != "contact" || !first.Recovered || first.Method != "carve" || first.Confidence == nil || *first.Confidence != 40 ||
		first.Parser.Name != "sms-parser" || first.Parser.Version != "1.0" || first.TS == nil || first.TS.Basis != "utc" || first.IngestID == "" {
		t.Fatalf("first json row = %+v", first)
	}
	if last := l.Records[4]; last.ID != 4 || last.TS != nil {
		t.Fatalf("last json row = %+v", last)
	}
	// descending
	if got := listIDs(t, "--case", rc.dir, "--sort", "-ts"); !slices.Equal(got, []int64{4, 2, 1, 3, 5}) {
		t.Fatalf("--sort -ts = %v", got)
	}
}

func TestRecordsListPagination(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	recordstest.Ingest(t, c, recParser, []string{art.ID}, recordstest.Records(art.ID, 53, 5))
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sort := range []string{"ts", "-ts"} {
		seen := map[int64]bool{}
		cursor, pages := "", 0
		for {
			args := []string{"--case", dir, "--limit", "7", "--sort", sort}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			var l jsonList
			cliJSON(t, &l, append([]string{"records", "list"}, args...)...)
			pages++
			if pages > 20 {
				t.Fatal("pagination does not end")
			}
			for _, r := range l.Records {
				if seen[r.ID] {
					t.Fatalf("record %d listed twice", r.ID)
				}
				seen[r.ID] = true
			}
			if l.NextCursor == "" {
				break
			}
			if len(l.Records) != 7 {
				t.Fatalf("a short page of %d with a cursor", len(l.Records))
			}
			cursor = l.NextCursor
		}
		var st struct {
			Overview struct {
				Records int `json:"records"`
			} `json:"overview"`
		}
		cliJSON(t, &st, "records", "stats", "--case", dir)
		if len(seen) != 53 || st.Overview.Records != 53 || pages != 8 {
			t.Fatalf("%s: union %d records over %d pages, stats total %d", sort, len(seen), pages, st.Overview.Records)
		}
	}
	// a cursor continues its own listing only
	var l jsonList
	cliJSON(t, &l, "records", "list", "--case", dir, "--limit", "7")
	code, out := run(t, Deps{}, "records", "list", "--case", dir, "--limit", "7", "--type", "message", "--cursor", l.NextCursor)
	if code != ExitUsage || !strings.Contains(out, "cursor") {
		t.Fatalf("a cursor of another filter: %d %s", code, out)
	}
}

func TestRecordsListFilterFlags(t *testing.T) {
	rc := recDataset(t, true)
	d := rc.dir
	for _, tc := range []struct {
		name string
		args []string
		want []int64 // ids in ascending order
	}{
		{"all current", nil, []int64{5, 6, 3, 1, 2, 4, 7}},
		{"type", []string{"--type", "call"}, []int64{6, 2, 7}},
		{"two types", []string{"--type", "call", "--type", "note"}, []int64{6, 3, 2, 7}},
		{"from", []string{"--from", "2026-10-04T09:30:00Z"}, []int64{2}},
		{"to", []string{"--to", "2026-10-04T09:30:00Z"}, []int64{5, 6, 3, 1}},
		{"from to", []string{"--from", "2026-10-04T00:00:00Z", "--to", "2026-10-04T09:30:00Z"}, []int64{3, 1}},
		{"offset time", []string{"--from", "2026-10-04T11:00:00+02:00", "--to", "2026-10-04T11:01:00+02:00"}, []int64{3}},
		{"include untimed", []string{"--from", "2026-10-04T09:30:00Z", "--include-untimed"}, []int64{2, 4, 7}},
		{"artifact", []string{"--artifact", rc.art2.ID}, []int64{6, 7}},
		{"two artifacts", []string{"--artifact", rc.art2.ID, "--artifact", rc.art.ID}, []int64{5, 6, 3, 1, 2, 4, 7}},
		{"path prefix", []string{"--path-prefix", "/data/"}, []int64{1, 2}},
		{"deleted only", []string{"--deleted", "only"}, []int64{5, 4}},
		{"deleted none", []string{"--deleted", "none"}, []int64{6, 3, 1, 2, 7}},
		{"recovered only", []string{"--recovered", "only"}, []int64{5}},
		{"recovered none", []string{"--recovered", "none"}, []int64{6, 3, 1, 2, 4, 7}},
		{"parser name", []string{"--parser", "calls"}, []int64{6, 7}},
		{"parser version", []string{"--parser", "sms-parser@1.0"}, []int64{5, 3, 1, 2, 4}},
		{"parser other version", []string{"--parser", "sms-parser@2.0"}, nil},
		{"min confidence", []string{"--min-confidence", "50"}, []int64{1}},
		{"newest first", []string{"--type", "call", "--sort", "-ts"}, []int64{7, 2, 6}},
		{"limit", []string{"--limit", "2"}, []int64{5, 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listIDs(t, append([]string{"--case", d}, tc.args...)...)
			if !slices.Equal(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	var l jsonList
	cliJSON(t, &l, "records", "list", "--case", d, "--parser", "calls")
	ing := l.Records[0].IngestID
	if got := listIDs(t, "--case", d, "--ingest", ing); !slices.Equal(got, []int64{6, 7}) {
		t.Fatalf("--ingest = %v", got)
	}
	// --all-runs: a newer complete run of the same parser name supersedes the older
	c := reopen(t, d)
	recordstest.Ingest(t, c, records.Parser{Name: "sms-parser", Version: "2.0"}, []string{rc.art.ID}, []records.Record{
		{Type: "message", ArtifactID: rc.art.ID, Summary: "newer", Time: &records.Time{T: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}, Payload: validPayload("message", nil)},
	})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, "--case", d, "--parser", "sms-parser"); !slices.Equal(got, []int64{8}) {
		t.Fatalf("after supersession = %v", got)
	}
	got := listIDs(t, "--case", d, "--parser", "sms-parser", "--all-runs")
	if len(got) != 6 {
		t.Fatalf("--all-runs = %v", got)
	}
	code, out := run(t, Deps{}, "records", "list", "--case", d, "--parser", "sms-parser", "--all-runs")
	if code != 0 || strings.Count(out, "[superseded]") != 5 {
		t.Fatalf("--all-runs text: %d\n%s", code, out)
	}
}

func TestRecordsListBadInputIsUsage(t *testing.T) {
	rc := recDataset(t, false)
	for _, args := range [][]string{
		{"--from", "yesterday"},
		{"--to", "2026-13-45"},
		{"--from", "2026-10-04"},
		{"--deleted", "x"},
		{"--recovered", "maybe"},
		{"--sort", "id"},
		{"--sort", "-"},
		{"--limit", "0"},
		{"--limit", "10001"},
		{"--limit", "-3"},
		{"--limit", "many"},
		{"--cursor", "garbage"},
		{"--cursor", "v1."},
		{"--cursor", "v1.e30"},
		{"--min-confidence", "101"},
		{"--min-confidence", "-1"},
		{"--min-confidence", "x"},
		{"--parser", "@1.0"},
		{"--parser", "name@"},
		{"--parser", ""},
		{"--path-prefix", "a\x00b"},
		{"--bogus"},
	} {
		code, out := run(t, Deps{}, append([]string{"records", "list", "--case", rc.dir}, args...)...)
		if code != ExitUsage {
			t.Errorf("%q: exit %d, want %d: %s", args, code, ExitUsage, out)
		}
	}
	for _, args := range [][]string{{"--by", "colour"}, {"--deleted", "x"}, {"--from", "x"}} {
		if code, out := run(t, Deps{}, append([]string{"records", "stats", "--case", rc.dir}, args...)...); code != ExitUsage {
			t.Errorf("stats %q: exit %d: %s", args, code, out)
		}
	}
	for _, args := range [][]string{{"abc"}, {"0"}, {"-4"}, {"1", "2"}, {}} {
		if code, out := run(t, Deps{}, append([]string{"records", "show", "--case", rc.dir}, args...)...); code != ExitUsage {
			t.Errorf("show %q: exit %d: %s", args, code, out)
		}
	}
	if code, _ := run(t, Deps{}, "records", "list"); code != ExitUsage {
		t.Errorf("missing --case: exit %d", code)
	}
	if code, _ := run(t, Deps{}, "records", "frobnicate"); code != ExitUsage {
		t.Errorf("unknown subcommand: exit %d", code)
	}
}

func TestRecordsShowAndPayload(t *testing.T) {
	rc := recDataset(t, false)
	code, out := run(t, Deps{}, "records", "show", "--case", rc.dir, "3")
	if code != 0 {
		t.Fatalf("show: %d %s", code, out)
	}
	for _, want := range []string{
		"record 3", "note", "2026-10-04T11:00:00~local+02:00", "2026-10-04T11:30:00~local+02:00", "a note",
		rc.art.ID, rc.art.Path, rc.art.SHA256, "kind file", "dev1",
		"sms-parser", "1.0", "abc123", "batch", "digest", "audit seq", "complete",
		"first line", "second line",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "payload") {
		t.Errorf("show without --payload prints the payload:\n%s", out)
	}
	// the batch's audit seq is a real audit entry
	es, err := evidence.ReadAuditEntries(filepath.Join(rc.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var batchSeq int
	for _, e := range es {
		if e.Action == "records.batch" {
			batchSeq = int(e.Seq)
		}
	}
	if batchSeq == 0 || !strings.Contains(out, "audit seq "+strconv.Itoa(batchSeq)) {
		t.Errorf("batch audit seq %d not shown:\n%s", batchSeq, out)
	}

	code, out = run(t, Deps{}, "records", "show", "--case", rc.dir, "1", "--payload")
	if code != 0 || !strings.Contains(out, "payload") || !strings.Contains(out, `"text": "hi"`) || !strings.Contains(out, `"n": 3`) {
		t.Fatalf("show --payload: %d\n%s", code, out)
	}

	var full struct {
		ID       int64           `json:"id"`
		Body     string          `json:"body"`
		Payload  json.RawMessage `json:"payload"`
		Artifact struct {
			ID     string `json:"id"`
			SHA256 string `json:"sha256"`
		} `json:"artifact"`
		Batch struct {
			Digest   string `json:"digest"`
			AuditSeq int64  `json:"audit_seq"`
		} `json:"batch"`
		Run struct {
			Outcome string `json:"outcome"`
		} `json:"run"`
	}
	cliJSON(t, &full, "records", "show", "--case", rc.dir, "1", "--payload")
	if full.ID != 1 || full.Artifact.ID != rc.art.ID || full.Artifact.SHA256 != rc.art.SHA256 || full.Batch.Digest == "" ||
		full.Batch.AuditSeq != int64(batchSeq) || full.Run.Outcome != "complete" || compactJSON(t, full.Payload) != `{"channel":"sms","direction":"unknown","kind":"unknown","n":3,"participants":[],"participants_unknown":true,"text":"hi"}` {
		t.Fatalf("show --json = %+v payload %s", full, full.Payload)
	}
}

func TestRecordsShowUnknownIDExit1(t *testing.T) {
	rc := recDataset(t, false)
	code, out := run(t, Deps{}, "records", "show", "--case", rc.dir, "999")
	if code != ExitError || !strings.Contains(out, "not found") {
		t.Fatalf("unknown id: %d %s", code, out)
	}
}

func TestRecordsStatsByAndOverview(t *testing.T) {
	rc := recDataset(t, true)
	// an incomplete run of another parser over art2
	c := reopen(t, rc.dir)
	w, err := records.NewWriter(c, records.Parser{Name: "cut-off", Version: "1"}, records.WriterOptions{BatchRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(t.Context(), records.StartOptions{Artifacts: []string{rc.art2.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(t.Context(), records.Record{Type: "event", ArtifactID: rc.art2.ID, Summary: "partial", Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Abort(t.Context(), errors.New("cut")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	var st struct {
		Overview struct {
			Records   int64            `json:"records"`
			Deleted   int64            `json:"deleted"`
			Recovered int64            `json:"recovered"`
			Untimed   int64            `json:"untimed"`
			Runs      map[string]int64 `json:"runs"`
			TSMin     *string          `json:"ts_min"`
			TSMax     *string          `json:"ts_max"`
		} `json:"overview"`
		By   string `json:"by"`
		Rows []struct {
			Key   string  `json:"key"`
			Count int64   `json:"count"`
			TSMin *string `json:"ts_min"`
		} `json:"rows"`
	}
	cliJSON(t, &st, "records", "stats", "--case", rc.dir)
	o := st.Overview
	if st.By != "type" || o.Records != 8 || o.Recovered != 1 || o.Untimed != 3 || o.Runs["complete"] != 2 || o.Runs["incomplete"] != 1 ||
		o.TSMin == nil || o.TSMax == nil || o.Deleted < 2 {
		t.Fatalf("overview %+v by %q", o, st.By)
	}
	counts := map[string]int64{}
	for _, r := range st.Rows {
		counts[r.Key] = r.Count
	}
	if counts["call"] != 3 || counts["message"] != 1 || counts["event"] != 1 || len(counts) != 6 {
		t.Fatalf("by type = %v", counts)
	}
	for by, wantKeys := range map[string][]string{
		"parser":   {"calls/2.0", "cut-off/1", "sms-parser/1.0"},
		"deleted":  {"deleted", "live", "recovered"},
		"artifact": {rc.art.ID, rc.art2.ID},
	} {
		var s2 struct {
			By   string `json:"by"`
			Rows []struct {
				Key string `json:"key"`
			} `json:"rows"`
		}
		cliJSON(t, &s2, "records", "stats", "--case", rc.dir, "--by", by)
		var keys []string
		for _, r := range s2.Rows {
			keys = append(keys, r.Key)
		}
		slices.Sort(keys)
		slices.Sort(wantKeys)
		if s2.By != by || !slices.Equal(keys, wantKeys) {
			t.Errorf("--by %s: %v, want %v", by, keys, wantKeys)
		}
	}
	var byRun struct {
		Rows []struct {
			Key   string `json:"key"`
			Count int64  `json:"count"`
		} `json:"rows"`
	}
	cliJSON(t, &byRun, "records", "stats", "--case", rc.dir, "--by", "run")
	if len(byRun.Rows) != 3 {
		t.Fatalf("--by run: %+v (the incomplete run is counted)", byRun.Rows)
	}
	// the stats take the list filters
	cliJSON(t, &st, "records", "stats", "--case", rc.dir, "--type", "call")
	if st.Overview.Records != 3 {
		t.Fatalf("filtered overview %+v", st.Overview)
	}

	code, out := run(t, Deps{}, "records", "stats", "--case", rc.dir, "--by", "type")
	if code != 0 || !strings.Contains(out, "records") || !strings.Contains(out, "incomplete") || !strings.Contains(out, "call") {
		t.Fatalf("stats text: %d\n%s", code, out)
	}
}

func TestV1CaseRefusesRecordsUntilUpgrade(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	for _, args := range [][]string{{"records", "list"}, {"records", "show", "1"}, {"records", "stats"}} {
		code, out := run(t, Deps{}, append(args, "--case", dir)...)
		if code != ExitUsage || !strings.Contains(out, "case upgrade") {
			t.Fatalf("%v on a v1 case: %d %s", args, code, out)
		}
	}
	if code, out := run(t, Deps{}, "case", "upgrade", "--case", dir); code != 0 {
		t.Fatalf("upgrade: %d %s", code, out)
	}
	code, out := run(t, Deps{}, "records", "list", "--case", dir)
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("list after upgrade: %d %q", code, out)
	}
	var l jsonList
	cliJSON(t, &l, "records", "list", "--case", dir)
	if len(l.Records) != 0 || l.NextCursor != "" {
		t.Fatalf("json after upgrade: %+v", l)
	}
}

func TestCaseVerifyAlteredRecordExits4(t *testing.T) {
	rc := recDataset(t, false)
	if code, out := run(t, Deps{}, "case", "verify", "--case", rc.dir); code != 0 {
		t.Fatalf("clean verify: %d %s", code, out)
	}
	recordstest.SetRecordSummary(t, rc.dir, 2, "altered afterwards")
	code, out := run(t, Deps{}, "case", "verify", "--case", rc.dir)
	if code != ExitIntegrity || !strings.Contains(out, "digest mismatch") {
		t.Fatalf("verify after altering a record: %d %s", code, out)
	}
}

// recEvil is text meant to attack a terminal: C0 and C1 controls, an ANSI
// sequence, bidi overrides and isolates, zero-width and format characters.
const recEvil = "a\x1b[31mRED\x1b]0;title\x07\u202eevil\u2066x\u2069\u200b\u200d\ufeff\u0085\u009b\x7f\x00\tb"

func noRawNonPrintable(t *testing.T, what, s string) {
	t.Helper()
	for i, r := range s {
		if r == '\n' || r == ' ' || unicode.IsPrint(r) {
			continue
		}
		t.Errorf("%s: raw non-printable rune %U at byte %d of %q", what, r, i, s)
		return
	}
}

func TestRecordsCLIEscapesRecordText(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	recEvilClean := strings.ReplaceAll(recEvil, "\x00", "")
	res := recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{{
		Type: "message", ArtifactID: art.ID, Summary: recEvilClean, SourcePath: "/p/" + recEvilClean, Locator: "sqlite:t=" + recEvilClean,
		Recovery: "carve", Deleted: true,
		Body:    "line one " + recEvilClean + "\nline two\u202e " + recEvilClean + "\n\x1b[2Jline three\r\n",
		Payload: validPayload("message", map[string]any{"k" + recEvilClean: "v" + recEvilClean, "list": []any{recEvilClean}}),
		Time:    &records.Time{T: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recordstest.SetParserColumn(t, dir, 1, "name", "parser"+recEvilClean)
	recordstest.SetParserColumn(t, dir, 1, "version", "v"+recEvilClean)
	// the strings of the record type, the recovery method, the ingest id and the batch and
	// run rows are database text too: hostile values in each of them
	recordstest.SetRecordColumn(t, dir, 1, "type", "ty"+recEvilClean)
	recordstest.SetRecordColumn(t, dir, 1, "recovery_method", "rm"+recEvilClean)
	evilIngest := "ing" + recEvilClean
	recordstest.SetBatchColumn(t, dir, res.IngestID, 1, "digest", "dg"+recEvilClean)
	recordstest.SetBatchColumn(t, dir, res.IngestID, 1, "created", "cr"+recEvilClean)
	recordstest.SetBatchColumn(t, dir, res.IngestID, 1, "ingest_id", evilIngest)
	recordstest.SetRunColumn(t, dir, res.IngestID, "analysis_id", "an"+recEvilClean)
	recordstest.SetRunColumn(t, dir, res.IngestID, "rollup", "ru"+recEvilClean)
	recordstest.SetRunColumn(t, dir, res.IngestID, "ended", "en"+recEvilClean)

	for _, args := range [][]string{
		{"records", "list", "--case", dir},
		{"records", "list", "--case", dir, "--limit", "1"},
		{"records", "show", "--case", dir, "1"},
		{"records", "show", "--case", dir, "1", "--payload"},
		{"records", "stats", "--case", dir, "--by", "parser"},
		{"records", "stats", "--case", dir, "--by", "run"},
		{"records", "stats", "--case", dir, "--by", "type"},
		{"records", "stats", "--case", dir, "--by", "artifact"},
		{"records", "stats", "--case", dir, "--by", "deleted"},
	} {
		code, out := run(t, Deps{}, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		noRawNonPrintable(t, strings.Join(args, " "), out)
	}
	// the escapes are visible, not silently dropped
	_, out := run(t, Deps{}, "records", "show", "--case", dir, "1", "--payload")
	for _, want := range []string{`\x1b`, `\u202e`} {
		if !strings.Contains(out, want) {
			t.Errorf("escaped output lacks %s:\n%s", want, out)
		}
	}

	// a cursor-shaped value echoed in an error
	code, out := run(t, Deps{}, "records", "list", "--case", dir, "--cursor", "v1."+recEvilClean)
	if code != ExitUsage {
		t.Fatalf("bad cursor: %d %s", code, out)
	}
	noRawNonPrintable(t, "bad cursor error", out)
	code, out = run(t, Deps{}, "records", "list", "--case", dir, "--ingest", recEvilClean, "--parser", "x"+recEvilClean)
	noRawNonPrintable(t, "recEvil filter", out)
	_ = code

	// --json keeps the strings as given
	var l jsonList
	cliJSON(t, &l, "records", "list", "--case", dir)
	r := l.Records[0]
	if r.Summary != recEvilClean || r.SourcePath != "/p/"+recEvilClean || r.Locator != "sqlite:t="+recEvilClean || r.Parser.Name != "parser"+recEvilClean {
		t.Fatalf("json row = %+v", r)
	}
	var full struct {
		Body string `json:"body"`
	}
	cliJSON(t, &full, "records", "show", "--case", dir, "1")
	if !strings.Contains(full.Body, recEvilClean) || !strings.Contains(full.Body, "\x1b[2Jline three\r\n") {
		t.Fatalf("json body = %q", full.Body)
	}
}

func TestRecordsCommandsDoNotWrite(t *testing.T) {
	rc := recDataset(t, true)
	snapshot := func() map[string]string {
		out := map[string]string{}
		err := filepath.WalkDir(rc.dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || filepath.Base(p) == "audit.jsonl" {
				return err
			}
			b, rerr := os.ReadFile(p) //nolint:gosec // a test reading the files of its own temp case
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(b)
			rel, _ := filepath.Rel(rc.dir, p)
			out[rel] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	audit := func() []string {
		es, err := evidence.ReadAuditEntries(filepath.Join(rc.dir, "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var a []string
		for _, e := range es {
			a = append(a, e.Action)
		}
		return a
	}
	before, auditBefore := snapshot(), audit()
	for i, args := range [][]string{
		{"records", "list"},
		{"records", "list", "--limit", "2", "--type", "call"},
		{"records", "list", "--json"},
		{"records", "show", "1", "--payload"},
		{"records", "show", "6"},
		{"records", "show", "4000"},
		{"records", "stats"},
		{"records", "stats", "--by", "run", "--json"},
		{"records", "list", "--cursor", "junk"},
		{"records", "search", "hello"},
		{"records", "search", "wor*", "--json", "--limit", "1"},
		{"records", "search", "--substring", "ell", "--no-snippets"},
		{"records", "search", "hello", "--rank"},
	} {
		run(t, Deps{}, append(args, "--case", rc.dir)...)
		now := audit()
		if len(now) != len(auditBefore)+i+1 || now[len(now)-1] != "case.open" {
			t.Fatalf("%v: audit has %d entries after %d commands (was %d), last %q: want one case.open per command",
				args, len(now), i+1, len(auditBefore), now[len(now)-1])
		}
		if after := snapshot(); !mapsEqual(before, after) {
			t.Fatalf("%v changed a case file other than audit.jsonl", args)
		}
	}
	for _, a := range audit()[len(auditBefore):] {
		if a != "case.open" {
			t.Fatalf("a read appended %q", a)
		}
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", rc.dir); code != 0 {
		t.Fatalf("verify after reads: %d %s", code, out)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestRecordsCommandsRefuseTamperedSchema: a redefined index (here the reviewer's
// attack: records_deleted over deleted = 0) or any other schema object that is not
// the expected one stops list, show and stats with exit 4, naming `case verify`.
func TestRecordsCommandsRefuseTamperedSchema(t *testing.T) {
	rc := recDataset(t, false)
	def := recordstest.SchemaSQL(t, rc.dir, "index", "records_deleted")
	recordstest.RedefineSchemaObject(t, rc.dir, "index", "records_deleted", strings.Replace(def, "deleted = 1", "deleted = 0", 1))
	for _, args := range [][]string{
		{"records", "list", "--case", rc.dir},
		{"records", "show", "--case", rc.dir, "1"},
		{"records", "stats", "--case", rc.dir},
	} {
		code, out := run(t, Deps{}, args...)
		if code != ExitIntegrity || !strings.Contains(out, "case verify") {
			t.Errorf("%v on a tampered schema: exit %d: %s", args[:2], code, out)
		}
	}
}

// TestRecordsShowArtifactMissingFromManifestExits4: a record whose artifact the
// manifest no longer holds is a verify-class failure (P1), exit 4, not a plain
// error.
func TestRecordsShowArtifactMissingFromManifestExits4(t *testing.T) {
	rc := recDataset(t, false)
	recordstest.RemoveArtifactEverywhere(t, rc.dir, rc.art.ID)
	code, out := run(t, Deps{}, "records", "show", "--case", rc.dir, "1")
	if code != ExitIntegrity || !strings.Contains(out, "unknown artifact") {
		t.Fatalf("show with the artifact gone: exit %d: %s", code, out)
	}
}

// validPayload is the minimal valid payload of a record type (recordstest.ValidPayload) plus the keys
// a test adds: the CLI links the payload validators, so a free-form payload of a validated type is refused.
func validPayload(typ string, extra map[string]any) map[string]any {
	p := recordstest.ValidPayload(typ)
	maps.Copy(p, extra)
	return p
}

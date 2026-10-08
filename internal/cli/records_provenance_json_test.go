package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Mirrors of the documented JSON, decoded strictly: an unknown or renamed key fails.
type provTestJSON struct {
	Status      string `json:"status"`
	ReachedRoot bool   `json:"reached_root"`
	Chain       []struct {
		Index    int                     `json:"index"`
		Artifact evidence.ManifestRecord `json:"artifact"`
		Link     string                  `json:"link"`
		Audit    struct {
			State  string `json:"state"`
			Seq    int64  `json:"seq"`
			Detail string `json:"detail"`
		} `json:"audit"`
		Segments struct {
			Total  int `json:"total"`
			Bad    int `json:"bad"`
			Listed []struct {
				ID     string `json:"id"`
				SHA256 string `json:"sha256"`
				State  string `json:"state"`
			} `json:"listed"`
		} `json:"segments"`
	} `json:"chain"`
	Recovery *struct {
		ArtifactID    string            `json:"artifact_id"`
		Hops          int               `json:"hops"`
		Recovery      evidence.Recovery `json:"recovery"`
		MinConfidence *int              `json:"min_confidence"`
		DeclaredRuns  *struct {
			ArtifactID string `json:"artifact_id"`
			State      string `json:"state"`
			Detail     string `json:"detail"`
		} `json:"declared_runs"`
	} `json:"recovery"`
	Offset struct {
		State       string `json:"state"`
		Reason      string `json:"reason"`
		Coordinates string `json:"coordinates"`
		Segments    int    `json:"segments"`
		Hops        []struct {
			From           string          `json:"from"`
			To             string          `json:"to"`
			Runs           string          `json:"runs"`
			Total          int             `json:"total"`
			Truncated      bool            `json:"truncated"`
			RunsExceedSize bool            `json:"runs_exceed_size"`
			Extents        []provExtentJSN `json:"extents"`
		} `json:"hops"`
		Image []provExtentJSN `json:"image"`
	} `json:"offset"`
	Notes    []string `json:"notes"`
	Problems []struct {
		Kind   string `json:"kind"`
		Hop    int    `json:"hop"`
		Detail string `json:"detail"`
	} `json:"problems"`
	ProblemsSuppressed int `json:"problems_suppressed"`
}

type provExtentJSN struct {
	ArtifactOffset int64 `json:"artifact_offset"`
	Length         int64 `json:"length"`
	ImageOffset    int64 `json:"image_offset"`
	Hole           bool  `json:"hole"`
}

// showProv runs `records show --json` and returns the strictly decoded provenance, the exit code,
// stdout and stderr.
func showProv(t *testing.T, dir, id string) (provTestJSON, int, string, string) {
	t.Helper()
	var errBuf bytes.Buffer
	code, out := run(t, Deps{Err: &errBuf}, "records", "show", "--case", dir, "--json", id)
	var top struct {
		Provenance json.RawMessage `json:"provenance"`
	}
	if err := json.Unmarshal([]byte(out), &top); err != nil || len(top.Provenance) == 0 {
		t.Fatalf("show --json: code %d, no provenance (%v):\n%s\n%s", code, err, out, errBuf.String())
	}
	var p provTestJSON
	dec := json.NewDecoder(bytes.NewReader(top.Provenance))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("provenance does not decode strictly: %v\n%s", err, top.Provenance)
	}
	return p, code, out, errBuf.String()
}

// liveDerivedCase: img <- f (extract, one inline run), record 1 on f with a range.
func liveDerivedCase(t *testing.T) (dir string, img, f evidence.ManifestRecord) {
	t.Helper()
	c := recordstest.NewCase(t)
	img = recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	f = recordstest.AddDerivedWith(t, c, "f.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/f", FSID: "nid:5",
		Runs: []evidence.Run{{Offset: 100, Length: 8}},
	}}, []byte("abcdefgh"))
	recordstest.Ingest(t, c, recParser, []string{f.ID}, []records.Record{{
		Type: "note", ArtifactID: f.ID, Summary: "live note", Payload: map[string]any{}, Range: &records.Range{Offset: 2, Length: 4},
	}})
	dir = c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, img, f
}

func TestShowJSONProvenanceShape(t *testing.T) {
	dir, img, f := liveDerivedCase(t)
	p, code, out, stderr := showProv(t, dir, "1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if p.Status != "live" || !p.ReachedRoot || p.Recovery != nil {
		t.Errorf("status %q reached_root %v recovery %v", p.Status, p.ReachedRoot, p.Recovery)
	}
	if len(p.Chain) != 2 || p.Chain[0].Index != 0 || p.Chain[0].Artifact.ID != f.ID || p.Chain[0].Link != "ok" ||
		p.Chain[1].Index != 1 || p.Chain[1].Artifact.ID != img.ID || p.Chain[1].Link != "root" {
		t.Fatalf("chain %+v", p.Chain)
	}
	if p.Chain[0].Artifact.SHA256 != f.SHA256 || p.Chain[0].Artifact.Source.Derived == nil {
		t.Errorf("the stored manifest record is not carried: %+v", p.Chain[0].Artifact)
	}
	if p.Chain[0].Audit.State != "bound" || p.Chain[0].Audit.Seq <= 0 {
		t.Errorf("audit %+v", p.Chain[0].Audit)
	}
	if p.Offset.State != "translated" || p.Offset.Coordinates != "media" || len(p.Offset.Image) != 1 ||
		p.Offset.Image[0] != (provExtentJSN{ArtifactOffset: 2, Length: 4, ImageOffset: 102}) {
		t.Errorf("offset %+v", p.Offset)
	}
	if len(p.Offset.Hops) != 1 || p.Offset.Hops[0].From != f.ID || p.Offset.Hops[0].To != img.ID || p.Offset.Hops[0].Runs != "inline" || p.Offset.Hops[0].Total != 1 {
		t.Errorf("hops %+v", p.Offset.Hops)
	}
	if p.Notes == nil || p.Problems == nil {
		t.Errorf("notes and problems must be arrays, not null: %s", out)
	}
	// existing keys unchanged
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "type", "summary", "artifact", "artifact_incomplete", "batch", "run", "times", "provenance"} {
		if _, ok := top[k]; !ok {
			t.Errorf("key %q missing from show --json", k)
		}
	}
}

func TestShowJSONKeepsRawStrings(t *testing.T) {
	evil := "dev\x1b[31m\nFAKE: line\u202e\u0085"
	c := recordstest.NewCase(t)
	root := recordstest.AddDerivedWith(t, c, "r.bin", evidence.Source{Kind: "file", DeviceID: "dev1", OriginalPath: evil}, []byte("abcdefgh"))
	recordstest.Ingest(t, c, recParser, []string{root.ID}, []records.Record{{
		Type: "note", ArtifactID: root.ID, Summary: "n", Payload: map[string]any{},
	}})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	p, code, out, _ := showProv(t, dir, "1")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if got := p.Chain[0].Artifact.Source.OriginalPath; got != evil {
		t.Errorf("original path = %q, want the raw string %q", got, evil)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("JSON must carry the ESC as an escape, not raw:\n%q", out)
	}
}

var problemCount = regexp.MustCompile(`found (\d+) problem\(s\)`)

// damagedCase: f derives from a parent that is not in the manifest; record 1 on f.
func damagedCase(t *testing.T) string {
	t.Helper()
	c := recordstest.NewCase(t)
	f := recordstest.AddDerivedWith(t, c, "f.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: "no-such-artifact", ParentSHA256: strings.Repeat("a", 64), Partition: 1, FSType: "mtfs", FSPath: "/f", FSID: "nid:5",
		Runs: []evidence.Run{{Offset: 100, Length: 8}},
	}}, []byte("abcdefgh"))
	recordstest.Ingest(t, c, recParser, []string{f.ID}, []records.Record{{
		Type: "note", ArtifactID: f.ID, Summary: "damaged", Payload: map[string]any{}, Range: &records.Range{Offset: 2, Length: 4},
	}})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRecordsShowDamagedChainExits4AfterPrinting(t *testing.T) {
	dir := damagedCase(t)

	var errBuf bytes.Buffer
	code, out := run(t, Deps{Err: &errBuf}, "records", "show", "--case", dir, "1")
	if code != 4 {
		t.Fatalf("text: exit %d, want 4\n%s\n%s", code, out, errBuf.String())
	}
	requireLines(t, out, "        link:      PARENT MISSING no-such-artifact")
	for _, want := range []string{"record 1\n", "  provenance:\n", "problems:      PROBLEM parent-missing", "    checked:       "} {
		if !strings.Contains(out, want) {
			t.Errorf("text: the full provenance was not printed; missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errBuf.String(), "case verify") || !problemCount.MatchString(errBuf.String()) {
		t.Errorf("text: stderr = %q, want it to count problems and name case verify", errBuf.String())
	}

	p, code, _, stderr := showProv(t, dir, "1")
	if code != 4 {
		t.Fatalf("json: exit %d, want 4", code)
	}
	if len(p.Problems) == 0 || p.Problems[0].Kind != "parent-missing" || len(p.Chain) != 1 || p.Chain[0].Link != "parent-missing" {
		t.Errorf("json: the full provenance was not printed: %+v", p)
	}
	if !strings.Contains(stderr, "case verify") || !problemCount.MatchString(stderr) {
		t.Errorf("json: stderr = %q", stderr)
	}
}

// The count in the message includes the problems the 50-per-kind cap suppressed.
func TestProvenanceFailureCountsSuppressedProblems(t *testing.T) {
	p := records.Provenance{
		Problems:           []records.Problem{{Kind: records.ProblemSegment, Detail: "a"}, {Kind: records.ProblemSegment, Detail: "b"}},
		ProblemsSuppressed: 7,
	}
	err := provenanceFailure(p, "/cases/a\x1b[31mb")
	if err == nil || !errors.Is(err, evidence.ErrIntegrity) || ExitCode(err) != ExitIntegrity {
		t.Fatalf("err = %v, exit %d", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "found 9 problem(s)") || !strings.Contains(err.Error(), "minutiae case verify --case /cases/a") {
		t.Errorf("message %q", err.Error())
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("the case path is not escaped: %q", err.Error())
	}
	if provenanceFailure(records.Provenance{Notes: []string{"n"}}, "d") != nil {
		t.Error("a chain with notes only must not fail")
	}
}

func TestRecordsShowNoteOnlyExits0(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	// a derived artifact that records no runs
	norun := recordstest.AddDerivedWith(t, c, "n.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/n", FSID: "nid:6",
	}}, []byte("abcdefgh"))
	// an incomplete derived artifact whose runs describe more than the bytes held
	inc, err := c.Capture("dev1", "acq-derived", "i.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/i", FSID: "nid:7",
		Runs: []evidence.Run{{Offset: 2000, Length: 1000}},
	}}, func(w io.Writer) error {
		_, _ = w.Write(make([]byte, 100))
		return errors.New("test: copy cut off")
	})
	if err == nil || !inc.Incomplete {
		t.Fatalf("incomplete artifact: %+v %v", inc, err)
	}
	recordstest.Ingest(t, c, recParser, []string{norun.ID, inc.ID}, []records.Record{
		{Type: "note", ArtifactID: norun.ID, Summary: "no runs", Payload: map[string]any{}, Range: &records.Range{Offset: 2, Length: 4}},
		{Type: "note", ArtifactID: inc.ID, Summary: "incomplete", Payload: map[string]any{}, Range: &records.Range{Offset: 10, Length: 20}},
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		var errBuf bytes.Buffer
		code, out := run(t, Deps{Err: &errBuf}, "records", "show", "--case", dir, id)
		if code != 0 || errBuf.Len() != 0 || !strings.Contains(out, "    notes:         ") || strings.Contains(out, "problems:") {
			t.Errorf("record %s text: exit %d stderr %q\n%s", id, code, errBuf.String(), out)
		}
		p, code, _, _ := showProv(t, dir, id)
		if code != 0 || len(p.Notes) == 0 || len(p.Problems) != 0 {
			t.Errorf("record %s json: exit %d, notes %q, problems %+v", id, code, p.Notes, p.Problems)
		}
	}
}

func TestRecordsShowCleanChainExits0(t *testing.T) {
	dir, _, _ := liveDerivedCase(t)
	var errBuf bytes.Buffer
	code, out := run(t, Deps{Err: &errBuf}, "records", "show", "--case", dir, "1")
	if code != 0 || errBuf.Len() != 0 || strings.Contains(out, "problems:") {
		t.Errorf("text: exit %d stderr %q\n%s", code, errBuf.String(), out)
	}
	if _, code, _, stderr := showProv(t, dir, "1"); code != 0 || stderr != "" {
		t.Errorf("json: exit %d stderr %q", code, stderr)
	}
}

// E46: status is "recovered" whenever the recovery view is non-null OR the row itself is recovered,
// and agrees with the text status line.
func TestShowRecoveredStatusInJSON(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	rec := recordstest.AddRecovered(t, c, img, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), rptr(60))
	recordstest.Ingest(t, c, recParser, []string{img.ID, rec.ID}, []records.Record{
		{Type: "note", ArtifactID: img.ID, Summary: "live", Payload: map[string]any{}},
		{Type: "note", ArtifactID: img.ID, Summary: "row only", Payload: map[string]any{}, Deleted: true, Recovery: "carve", Confidence: rptr(40)},
		{Type: "note", ArtifactID: rec.ID, Summary: "on recovered", Payload: map[string]any{}, Deleted: true, Recovery: "fat-contiguous", Confidence: rptr(50)},
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id       string
		status   string
		recovery bool
		text     string
	}{
		{"1", "live", false, "status:        live\n"},
		{"2", "recovered", false, "status:        RECOVERED DATA (class unknown, method carve, confidence 40; not live evidence)\n"},
		{"3", "recovered", true, "status:        RECOVERED DATA (class "},
	} {
		p, _, _, _ := showProv(t, dir, tc.id)
		if p.Status != tc.status || (p.Recovery != nil) != tc.recovery {
			t.Errorf("record %s: status %q, recovery %v; want %q, %v", tc.id, p.Status, p.Recovery != nil, tc.status, tc.recovery)
		}
		_, text := run(t, Deps{}, "records", "show", "--case", dir, tc.id)
		if !strings.Contains(text, tc.text) {
			t.Errorf("record %s: text status disagrees with JSON %q:\n%s", tc.id, tc.status, text)
		}
	}
}

func TestShowJSONTruncatedHopSaysSo(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	var runs []evidence.Run
	for i := range 300 {
		runs = append(runs, evidence.Run{Offset: int64(2 * i), Length: 1})
	}
	f := recordstest.AddDerivedWith(t, c, "f.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/f", FSID: "nid:5", Runs: runs,
	}}, make([]byte, 300))
	recordstest.Ingest(t, c, recParser, []string{f.ID}, []records.Record{{
		Type: "note", ArtifactID: f.ID, Summary: "wide", Payload: map[string]any{}, Range: &records.Range{Offset: 0, Length: 300},
	}})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	p, _, out, _ := showProv(t, dir, "1")
	if len(p.Offset.Hops) == 0 {
		t.Fatalf("no hops:\n%s", out)
	}
	h := p.Offset.Hops[len(p.Offset.Hops)-1]
	if !h.Truncated || h.Total != 300 || len(h.Extents) != records.MaxExtentsPerHop {
		t.Errorf("truncated %v total %d listed %d, want true 300 %d", h.Truncated, h.Total, len(h.Extents), records.MaxExtentsPerHop)
	}
}

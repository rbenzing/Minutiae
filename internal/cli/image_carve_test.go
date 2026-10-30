package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/carve/carvetest"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

// carveCLIPad rounds an object up to whole filesystem blocks.
func carveCLIPad(b []byte) []byte {
	out := make([]byte, (len(b)+recBS-1)/recBS*recBS)
	copy(out, b)
	return out
}

func carveCLIDeleted(path string, obj []byte) fstest.Node {
	return fstest.Node{Path: path, Data: carveCLIPad(obj), Deleted: true, Freed: true}
}

// carveCLIEnv is an imported MTFS image whose free space holds a JPEG and a PNG, and the image bytes.
type carveCLIEnv struct {
	*imgEnv
	media    []byte
	jpg, png []byte
}

func newCarveCLIEnv(t *testing.T, extra ...fstest.Node) *carveCLIEnv {
	t.Helper()
	jpg := carvetest.JPEG(carvetest.JPEGOpts{})
	png := carvetest.PNG(carvetest.PNGOpts{})
	nodes := append([]fstest.Node{
		{Path: "/a.bin", Data: recPat(1, 8192)},
		carveCLIDeleted("/del1.jpg", jpg),
		{Path: "/b.bin", Data: recPat(2, 4096)},
		carveCLIDeleted("/del2.png", png),
	}, extra...)
	media := recDisk(nodes...)
	e := &imgEnv{d: Deps{FSDrivers: mtfsDrivers}, c: newCLICase(t), nodes: nodes}
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", imgFile(t, media))
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); code != 0 || err != nil || len(recs) != 1 {
		t.Fatalf("image import: %d %s (%v)", code, out, err)
	}
	e.rec, e.ref = recs[0], recs[0].ID
	return &carveCLIEnv{imgEnv: e, media: media, jpg: jpg, png: png}
}

func (e *carveCLIEnv) carve(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return e.image(t, "carve", append([]string{e.ref}, args...)...)
}

func carveCLIRecords(t *testing.T, caseDir string) []evidence.ManifestRecord {
	t.Helper()
	recs, err := manifest(caseDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Kind == evidence.KindCarve {
			out = append(out, r)
		}
	}
	return out
}

func carveCLIAudit(t *testing.T, caseDir string) []evidence.AuditEntry {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(caseDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func carveCLIManifestLines(t *testing.T, caseDir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(caseDir, "manifest.jsonl")) //nolint:gosec // a test reading its own case
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

// carveCLISigCafe is a signature file with one header-only type, so its hits are bounded (confidence 25).
const carveCLISigCafe = `{"format":"` + carve.SigFileFormat + `","types":[{"name":"cafe","ext":"cfe","mime":"application/x-cafe",` +
	`"headers":[{"offset":0,"hex":"cafebabe","mask":""}],"footer":"","min_len":8,"max_len":512,"validate":"","align":"block"}]}`

func carveCLIWriteFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sigs.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// carveCLIBlankDisk is a GPT disk whose one partition holds only zeros: no filesystem is recognised.
func carveCLIBlankDisk() []byte {
	const first, sectors = 40, 64
	return volumetest.GPT(512, first+sectors+40, "11111111-2222-3333-4444-555555555555", []volumetest.Part{{
		StartLBA: first, Sectors: sectors, TypeGUID: imgLinuxType,
		GUID: "00000000-0000-0000-0000-000000000001", Name: "blank",
	}})
}

func TestImageCarveUsage(t *testing.T) {
	e := newCarveCLIEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"neither", []string{"image", "carve", "--case", e.c}, "image reference"},
		{"both", []string{"image", "carve", "--case", e.c, e.ref, "--artifact", "x"}, "mutually exclusive"},
		{"volume with partition", []string{"image", "carve", "--case", e.c, e.ref, "--volume", "-p", "1"}, "--volume"},
		{"volume with raw", []string{"image", "carve", "--case", e.c, e.ref, "--volume", "--raw"}, "--volume"},
		{"artifact with partition", []string{"image", "carve", "--case", e.c, "--artifact", "x", "-p", "1"}, "--artifact"},
		{"artifact with volume", []string{"image", "carve", "--case", e.c, "--artifact", "x", "--volume"}, "--artifact"},
		{"artifact with raw", []string{"image", "carve", "--case", e.c, "--artifact", "x", "--raw"}, "--artifact"},
		{"confidence high", []string{"image", "carve", "--case", e.c, e.ref, "--min-confidence", "101"}, "--min-confidence"},
		{"confidence negative", []string{"image", "carve", "--case", e.c, e.ref, "--min-confidence", "-1"}, "--min-confidence"},
		{"max files", []string{"image", "carve", "--case", e.c, e.ref, "--max-files", "-1"}, "--max-files"},
		{"max bytes", []string{"image", "carve", "--case", e.c, e.ref, "--max-bytes", "-1"}, "--max-bytes"},
		{"max file size", []string{"image", "carve", "--case", e.c, e.ref, "--max-file-size", "-1"}, "--max-file-size"},
		{"unknown type", []string{"image", "carve", "--case", e.c, e.ref, "--types", "jpeg,nosuch"}, "nosuch"},
		{"missing sig file", []string{"image", "carve", "--case", e.c, e.ref, "--sig-file", filepath.Join(t.TempDir(), "none.json")}, "--sig-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := run(t, e.d, tc.args...)
			if code != ExitUsage || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want 2 with %q:\n%s", code, tc.want, out)
			}
		})
	}
	// the unknown-type message lists the valid names
	_, out := run(t, e.d, "image", "carve", "--case", e.c, e.ref, "--types", "nosuch")
	for _, want := range []string{"jpeg", "png", "mp4", "docx", "bmp", "tiff", "dng", "avif", "jxl", "cur"} {
		if !strings.Contains(out, want) {
			t.Errorf("unknown-type message lacks valid name %q:\n%s", want, out)
		}
	}
	if n := len(carveCLIRecords(t, e.c)); n != 0 {
		t.Errorf("usage errors wrote %d carve artifacts", n)
	}
}

func TestImageCarveRunAndVerify(t *testing.T) {
	e := newCarveCLIEnv(t)
	code, out := e.carve(t)
	if code != 0 {
		t.Fatalf("carve: %d\n%s", code, out)
	}
	for _, want := range []string{"[recovered:carve-validated conf 70 high]", "carved/p1-mtfs/", "-jpeg.jpg", "-png.png", "carved 2 (", "recovered data is not live evidence", " at 0x"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	recs := carveCLIRecords(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d carve artifacts, want 2", len(recs))
	}
	if code, vout := run(t, e.d, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("verify: %d\n%s", code, vout)
	}
	p := filepath.Join(e.c, filepath.FromSlash(recs[0].Path))
	b, err := os.ReadFile(p) //nolint:gosec // a test reading its own case
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:gosec // a test tampering with its own case
		t.Fatal(err)
	}
	if code, vout := run(t, e.d, "case", "verify", "--case", e.c); code != ExitIntegrity {
		t.Fatalf("verify after a flipped byte: %d, want 4\n%s", code, vout)
	}
}

func TestImageCarveListWritesNothing(t *testing.T) {
	e := newCarveCLIEnv(t)
	lines, entries := carveCLIManifestLines(t, e.c), len(carveCLIAudit(t, e.c))
	code, out := e.carve(t, "--list")
	if code != 0 {
		t.Fatalf("carve --list: %d\n%s", code, out)
	}
	if strings.Count(out, "[would write]") != 2 || !strings.Contains(out, "carved/p1-mtfs/") || !strings.Contains(out, "would carve 2") {
		t.Errorf("list output:\n%s", out)
	}
	if n := carveCLIManifestLines(t, e.c); n != lines {
		t.Errorf("manifest has %d lines after --list, had %d", n, lines)
	}
	// only the case.open entry of this command may be new
	es := carveCLIAudit(t, e.c)
	for _, a := range es[entries:] {
		if a.Action != "case.open" {
			t.Errorf("--list wrote audit entry %q", a.Action)
		}
	}
	// a hit below the floor is listed with its reason
	code, out = e.carve(t, "--list", "--sig-file", carveCLIWriteFile(t, []byte(carveCLISigCafe)), "--types", "cafe")
	if code != 0 || strings.Contains(out, "[would write]") {
		t.Fatalf("list with no hit above the default floor: %d\n%s", code, out)
	}
}

func TestImageCarveMinConfidenceDefaultsTo40AndZeroIsAFloorOfZero(t *testing.T) {
	cafe := append([]byte{0xca, 0xfe, 0xba, 0xbe}, recPat(9, 600)...)
	e := newCarveCLIEnv(t, carveCLIDeleted("/c.bin", cafe))
	sig := carveCLIWriteFile(t, []byte(carveCLISigCafe))
	if code, out := e.carve(t, "--sig-file", sig, "--types", "cafe"); code != 0 || len(carveCLIRecords(t, e.c)) != 0 || !strings.Contains(out, "below-min-confidence") {
		t.Fatalf("default floor: %d, %d artifacts\n%s", code, len(carveCLIRecords(t, e.c)), out)
	}
	if code, out := e.carve(t, "--sig-file", sig, "--types", "cafe", "--min-confidence", "0"); code != 0 || len(carveCLIRecords(t, e.c)) != 1 {
		t.Fatalf("explicit floor 0: %d, %d artifacts\n%s", code, len(carveCLIRecords(t, e.c)), out)
	}
}

type carveCLIJSON struct {
	AnalysisID      string         `json:"analysis_id"`
	Carved          int            `json:"carved"`
	Partial         int            `json:"partial"`
	HitsByOutcome   map[string]int `json:"hits_by_outcome"`
	RejectedBy      map[string]int `json:"rejected_by"`
	SkippedBy       map[string]int `json:"skipped_by"`
	LimitReached    string         `json:"limit_reached"`
	NotScannedBytes int64          `json:"not_scanned_bytes"`
	Sigset          string         `json:"sigset"`
	SigsetSHA256    string         `json:"sigset_sha256"`
	Artifacts       []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"artifacts"`
}

func TestImageCarveJSONTerminalSafe(t *testing.T) {
	e := newCarveCLIEnv(t)
	code, out := e.carve(t, "--json")
	if code != 0 {
		t.Fatalf("carve --json: %d\n%s", code, out)
	}
	var j carveCLIJSON
	if err := json.Unmarshal(jsonPart(out), &j); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if j.Carved != 2 || j.HitsByOutcome["validated"] != 2 || len(j.Artifacts) != 2 || j.Sigset == "" || j.SigsetSHA256 == "" || j.AnalysisID == "" || j.RejectedBy == nil || j.SkippedBy == nil {
		t.Errorf("run json %+v", j)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(jsonPart(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"carved", "partial", "hits_by_outcome", "rejected_by", "skipped_by", "limit_reached", "not_scanned_bytes", "sigset", "sigset_sha256", "sig_file_sha256", "notes", "artifacts"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("run json lacks key %q", k)
		}
	}

	code, out = e.carve(t, "--json", "--list")
	if code != 0 {
		t.Fatalf("carve --json --list: %d\n%s", code, out)
	}
	var list struct {
		Hits []struct {
			Offset     int64  `json:"offset"`
			Length     int64  `json:"length"`
			Type       string `json:"type"`
			Outcome    string `json:"outcome"`
			Confidence int    `json:"confidence"`
			WouldWrite bool   `json:"would_write"`
			Name       string `json:"name"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(jsonPart(out), &list); err != nil || len(list.Hits) != 2 || !list.Hits[0].WouldWrite || list.Hits[0].Confidence != 70 {
		t.Fatalf("list json (%v): %+v\n%s", err, list, out)
	}
	var rawList map[string]json.RawMessage
	if err := json.Unmarshal(jsonPart(out), &rawList); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"scope", "hits", "hits_not_listed", "hits_by_outcome", "rejected_by", "skipped_by", "limit_reached", "not_scanned_bytes", "sigset", "sigset_sha256", "sig_file_sha256", "notes"} {
		if _, ok := rawList[k]; !ok {
			t.Errorf("list json lacks key %q", k)
		}
	}

	// a sink that refuses the output keeps the run's own error: the limit stop is exit 1 and is still named
	e2 := newCarveCLIEnv(t)
	var errBuf bytes.Buffer
	d := Deps{FSDrivers: mtfsDrivers, In: strings.NewReader(""), Out: carveCLIFailWriter{}, Err: &errBuf, Registry: device.NewRegistry()}
	code = Run([]string{"--json", "image", "carve", "--case", e2.c, e2.ref, "--max-files", "1"}, d)
	if code == 0 || !strings.Contains(errBuf.String(), "sink refused") || !strings.Contains(errBuf.String(), "max-files") {
		t.Errorf("refusing sink: exit %d, stderr %q", code, errBuf.String())
	}
}

type carveCLIFailWriter struct{}

func (carveCLIFailWriter) Write([]byte) (int, error) { return 0, errors.New("sink refused") }

// Output limits, unreadable bytes and capped types make the run partial: the work is written and printed, and
// the exit is 1.
func TestImageCarveLimitIsAPartialExit(t *testing.T) {
	e := newCarveCLIEnv(t)
	code, out := e.carve(t, "--max-files", "1")
	if code != ExitError || !strings.Contains(out, "limit reached: max-files") || !strings.Contains(out, "carved 1 (") {
		t.Fatalf("max-files: exit %d\n%s", code, out)
	}
	if n := len(carveCLIRecords(t, e.c)); n != 1 {
		t.Errorf("%d artifacts kept, want 1", n)
	}
	e2 := newCarveCLIEnv(t)
	if code, out := e2.carve(t, "--max-bytes", "1"); code != ExitError {
		t.Errorf("max-bytes: exit %d\n%s", code, out)
	}
}

func TestCarveCLIPartialError(t *testing.T) {
	cases := map[string]examine.CarveSummary{
		"unreadable": {Stats: carve.Stats{UnreadableBytes: 1}},
		"uncopied":   {UncopiedBytes: 1},
		"limit":      {LimitReached: "validate-budget"},
		"capped":     {Stats: carve.Stats{TypeCapped: []string{"jpeg"}}},
	}
	for name, s := range cases {
		if carveIncomplete(s) == nil {
			t.Errorf("%s: the run is clean", name)
		}
	}
	if err := carveIncomplete(examine.CarveSummary{}); err != nil {
		t.Errorf("a clean run is %v", err)
	}
	// a plain skip (below the floor) is not partial
	if err := carveIncomplete(examine.CarveSummary{SkippedBy: map[string]int{"below-min-confidence": 3}}); err != nil {
		t.Errorf("skips made the run partial: %v", err)
	}
}

func TestCarveExitMapping(t *testing.T) {
	if got := ExitCode(errors.Join(errors.New("x"), examine.ErrInvalidCarveOption)); got != ExitUsage {
		t.Errorf("ErrInvalidCarveOption: exit %d, want 2", got)
	}
	if got := ExitCode(examine.ErrNoRecognizedFilesystem); got != ExitError {
		t.Errorf("ErrNoRecognizedFilesystem: exit %d, want 1", got)
	}
	if got := ExitCode(errors.Join(evidence.ErrIntegrity, carveIncomplete(examine.CarveSummary{UncopiedBytes: 1}))); got != ExitIntegrity {
		t.Errorf("integrity joined with a partial run: exit %d, want 4", got)
	}
}

func TestImageCarveNoFilesystemSuggestsRaw(t *testing.T) {
	e := newCarveCLIEnv(t)
	// an image whose only partition holds no filesystem
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", imgFile(t, carveCLIBlankDisk()))
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); code != 0 || err != nil || len(recs) != 1 {
		t.Fatalf("import: %d %s", code, out)
	}
	code, out = e.image(t, "carve", recs[0].ID)
	if code != ExitError || !strings.Contains(out, "--raw") {
		t.Fatalf("exit %d, want 1 naming --raw:\n%s", code, out)
	}
}

func TestImageCarveSigFile(t *testing.T) {
	toy := recPat(7, 1024)
	copy(toy, "TOYHDR!\x00")
	copy(toy[1024-8:], "TOYFTR!\x00")
	sig := []byte(`{"format":"` + carve.SigFileFormat + `","types":[{"name":"toy","ext":"toy","mime":"application/x-toy",` +
		`"headers":[{"offset":0,"hex":"` + hex.EncodeToString([]byte("TOYHDR!\x00")) + `"}],"footer":"` + hex.EncodeToString([]byte("TOYFTR!\x00")) +
		`","min_len":1024,"max_len":4096,"validate":"","align":"block"}]}`)
	e := newCarveCLIEnv(t, carveCLIDeleted("/t.bin", toy))
	p := carveCLIWriteFile(t, sig)
	code, out := e.carve(t, "--sig-file", p, "--types", "toy", "--min-confidence", "50")
	if code != 0 || !strings.Contains(out, "[recovered:carve-footer conf 50") {
		t.Fatalf("at 50: %d\n%s", code, out)
	}
	sum := sha256.Sum256(sig)
	found := false
	for _, a := range carveCLIAudit(t, e.c) {
		if a.Action == "analysis.start" && a.Details["sig_file_sha256"] == hex.EncodeToString(sum[:]) {
			found = true
		}
	}
	if !found {
		t.Error("no analysis.start carries the signature file's SHA-256")
	}
	for name, data := range map[string][]byte{
		"too large":   bytes.Repeat([]byte(" "), 65*1024),
		"unknown key": []byte(`{"format":"` + carve.SigFileFormat + `","types":[],"bogus":1}`),
	} {
		code, out := e.carve(t, "--sig-file", carveCLIWriteFile(t, data))
		if code != ExitUsage {
			t.Errorf("%s: exit %d, want 2\n%s", name, code, out)
		}
	}
}

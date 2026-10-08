package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

const (
	recBS     = 4096
	recMethod = "fat-contiguous" // a deleted-file method the build has a rule for
)

// recPat is n bytes that are never uniform and never 0 or 255.
func recPat(seed byte, n int) []byte {
	rng := rand.New(rand.NewSource(int64(seed))) //nolint:gosec // seeded test data
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Intn(250) + 1)
	}
	return b
}

// recNode is a deleted node whose blocks are free, with the given maps (none: the map of its own blocks).
func recNode(path string, data []byte, maps ...fstest.RecoverMap) fstest.Node {
	if len(maps) == 0 {
		maps = []fstest.RecoverMap{{Method: recMethod}}
	}
	return fstest.Node{Path: path, Data: data, Deleted: true, Freed: true, Recover: maps}
}

func recDisk(nodes ...fstest.Node) []byte {
	fsys := fstest.Build(fstest.BuildSpec{Label: "L", BlockSize: recBS, FreeBlocks: 4, Nodes: nodes})
	const firstLBA = 40
	sectors := uint64(len(fsys)+511) / 512
	img := volumetest.GPT(512, firstLBA+sectors+8+40, "11111111-2222-3333-4444-555555555555", []volumetest.Part{{
		StartLBA: firstLBA, Sectors: sectors, TypeGUID: imgLinuxType,
		GUID: "00000000-0000-0000-0000-000000000001", Name: "data",
	}})
	copy(img[firstLBA*512:], fsys)
	return img
}

// newRecoverEnv imports a recovery-capable MTFS image of nodes into a fresh case.
func newRecoverEnv(t *testing.T, nodes ...fstest.Node) *imgEnv {
	t.Helper()
	e := &imgEnv{d: Deps{FSDrivers: mtfsDrivers}, c: newCLICase(t), nodes: nodes}
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", imgFile(t, recDisk(nodes...)))
	if code != 0 {
		t.Fatalf("image import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	e.rec, e.ref = recs[0], recs[0].ID
	return e
}

func (e *imgEnv) recover(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return e.image(t, "recover", append([]string{e.ref}, args...)...)
}

// caseFiles hashes every file of the case except the lock file.
func caseFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "case.lock" {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // a test reads its own case directory
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func recoveredRecords(t *testing.T, caseDir string) []evidence.ManifestRecord {
	t.Helper()
	recs, err := manifest(caseDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Kind == evidence.KindRecover {
			out = append(out, r)
		}
	}
	return out
}

type listJSON struct {
	Considered int `json:"considered"`
	Items      []struct {
		Path       string `json:"path"`
		ID         string `json:"id"`
		Type       string `json:"type"`
		Name       string `json:"name"`
		Candidates []struct {
			Method     string `json:"method"`
			Confidence int    `json:"confidence"`
			Band       string `json:"band"`
			Size       int64  `json:"size"`
			WouldWrite bool   `json:"would_write"`
			Skip       string `json:"skip"`
			Incomplete string `json:"incomplete"`
		} `json:"candidates"`
		Skips []struct {
			Reason string `json:"reason"`
		} `json:"skips"`
	} `json:"items"`
}

func (e *imgEnv) list(t *testing.T, args ...string) listJSON {
	t.Helper()
	code, out := e.recover(t, append([]string{"--list", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("recover --list: %d %s", code, out)
	}
	var l listJSON
	if err := json.Unmarshal(jsonPart(out), &l); err != nil {
		t.Fatalf("list json %q: %v", out, err)
	}
	return l
}

func twoDeleted() []fstest.Node {
	return []fstest.Node{
		recNode("/docs/a-del.bin", recPat(1, 3*recBS)),
		recNode("/b-del.bin", recPat(2, 2*recBS)),
		{Path: "/live.txt", Data: []byte("live")},
	}
}

func TestImageRecoverListIsReadOnly(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	before := caseFiles(t, e.c)
	nAudit := len(auditActions(t, e.c))
	code, out := e.recover(t, "--list")
	if code != 0 || !strings.Contains(out, "would write") {
		t.Fatalf("--list: %d %s", code, out)
	}
	after := caseFiles(t, e.c)
	for name, h := range before {
		if name != "audit.jsonl" && after[name] != h {
			t.Errorf("%s changed", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s was created", name)
		}
	}
	if acts := auditActions(t, e.c)[nAudit:]; !slices.Equal(acts, []string{"case.open"}) {
		t.Errorf("--list appended %v, want only case.open", acts)
	}
	if n := len(recoveredRecords(t, e.c)); n != 0 {
		t.Errorf("%d recovered artifacts after --list", n)
	}
}

func TestImageRecoverAllWritesAndVerifies(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	code, out := e.recover(t, "--all")
	if code != 0 || !strings.Contains(out, "recovered 2 (") {
		t.Fatalf("--all: %d %s", code, out)
	}
	if n := len(recoveredRecords(t, e.c)); n != 2 {
		t.Fatalf("%d recovered artifacts, want 2", n)
	}
	code, out = run(t, e.d, "case", "verify", "--case", e.c)
	if code != 0 {
		t.Fatalf("case verify: %d %s", code, out)
	}
	if !strings.Contains(out, "NOTICE:") || !strings.Contains(out, "recovered artifact(s)") || !strings.Contains(out, "2 recovered (2 reproduced)") {
		t.Errorf("verify output lacks the recovered notice or summary:\n%s", out)
	}
}

func TestImageRecoverRefsAndDirectory(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	l := e.list(t)
	ids := map[string]string{}
	for _, it := range l.Items {
		ids[it.Path] = it.ID
	}
	if len(ids) != 2 || ids["/b-del.bin"] == "" || ids["/docs/a-del.bin"] == "" {
		t.Fatalf("list items %+v, want both deleted entries", l.Items)
	}
	code, out := e.recover(t, "id:"+ids["/b-del.bin"])
	if code != 0 || !strings.Contains(out, "recovered 1 (") {
		t.Fatalf("by id: %d %s", code, out)
	}
	recs := recoveredRecords(t, e.c)
	if len(recs) != 1 || recs[0].Source.Derived.FSPath != "/b-del.bin" {
		t.Fatalf("recovered %+v, want only /b-del.bin", recs)
	}
	e2 := newRecoverEnv(t, twoDeleted()...)
	code, out = e2.recover(t, "/docs")
	if code != 0 || !strings.Contains(out, "recovered 1 (") {
		t.Fatalf("by directory: %d %s", code, out)
	}
	recs = recoveredRecords(t, e2.c)
	if len(recs) != 1 || recs[0].Source.Derived.FSPath != "/docs/a-del.bin" {
		t.Fatalf("recovered %+v, want only /docs/a-del.bin", recs)
	}
}

func TestImageRecoverTextAndJSON(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	code, out := e.recover(t, "--list")
	if code != 0 {
		t.Fatalf("list: %d %s", code, out)
	}
	for _, want := range []string{"[recovered:fat-contiguous conf ", "12288 bytes", "/docs/a-del.bin", "id:", "[would write]", "would recover 2 (20480 bytes)"} {
		if !strings.Contains(out, want) {
			t.Errorf("list text lacks %q:\n%s", want, out)
		}
	}
	l := e.list(t)
	if l.Considered != 2 || len(l.Items) != 2 {
		t.Fatalf("list json %+v", l)
	}
	for _, it := range l.Items {
		if len(it.Candidates) != 1 || it.Candidates[0].Method != recMethod || !it.Candidates[0].WouldWrite || it.Candidates[0].Band == "" || it.ID == "" || it.Type != "file" {
			t.Errorf("item %+v", it)
		}
	}
	code, out = e.recover(t, "--all", "--json")
	if code != 0 {
		t.Fatalf("run: %d %s", code, out)
	}
	var r struct {
		AnalysisID string                    `json:"analysis_id"`
		Considered int                       `json:"considered"`
		Candidates int                       `json:"candidates"`
		Recovered  int                       `json:"recovered"`
		Partial    int                       `json:"partial"`
		Uniform    int                       `json:"uniform"`
		Overlap    int                       `json:"overlap"`
		SkippedBy  map[string]int            `json:"skipped_by"`
		Limit      string                    `json:"limit_reached"`
		Artifacts  []evidence.ManifestRecord `json:"artifacts"`
	}
	if err := json.Unmarshal(jsonPart(out), &r); err != nil {
		t.Fatalf("run json %q: %v", out, err)
	}
	if r.AnalysisID == "" || r.Considered != 2 || r.Candidates != 2 || r.Recovered != 2 || r.Partial != 0 || r.Uniform != 0 || r.Overlap != 0 || r.SkippedBy == nil || r.Limit != "" || len(r.Artifacts) != 2 {
		t.Errorf("run json %+v", r)
	}
}

func TestImageRecoverPartialAndUniformMarkers(t *testing.T) {
	nodes := []fstest.Node{
		recNode("/part.bin", recPat(1, 4*recBS), fstest.RecoverMap{Method: recMethod, Size: 6 * recBS}), // claims more than its blocks
		recNode("/zero.bin", bytes.Repeat([]byte{0x41}, 2*recBS)),
		recNode("/ok.bin", recPat(3, recBS)),
	}
	e := newRecoverEnv(t, nodes...)
	code, out := e.recover(t, "--list")
	if code != 0 {
		t.Fatalf("list: %d %s", code, out)
	}
	lines := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		for _, p := range []string{"/part.bin", "/zero.bin", "/ok.bin"} {
			if strings.Contains(ln, p) {
				lines[p] = ln
			}
		}
	}
	if !strings.Contains(lines["/part.bin"], "[incomplete: ") || !strings.Contains(lines["/part.bin"], "[would write]") {
		t.Errorf("partial line %q lacks [incomplete: ...] and [would write]", lines["/part.bin"])
	}
	if !strings.Contains(lines["/zero.bin"], "[uniform skipped]") || strings.Contains(lines["/zero.bin"], "[would write]") {
		t.Errorf("uniform line %q", lines["/zero.bin"])
	}
	if strings.Contains(lines["/ok.bin"], "[incomplete") || strings.Contains(lines["/ok.bin"], "[uniform") || !strings.Contains(lines["/ok.bin"], "[would write]") {
		t.Errorf("plain line %q", lines["/ok.bin"])
	}
	code, out = e.recover(t, "--all")
	if code != 0 || !strings.Contains(out, "recovered 2 (") || !strings.Contains(out, "partial 1, uniform 1") || !strings.Contains(out, "[incomplete: ") {
		t.Fatalf("run: %d %s", code, out)
	}
	e2 := newRecoverEnv(t, nodes...)
	if code, out := e2.recover(t, "--all", "--keep-uniform"); code != 0 || !strings.Contains(out, "recovered 3 (") {
		t.Fatalf("keep-uniform: %d %s", code, out)
	}
}

func TestImageRecoverExitCodes(t *testing.T) {
	nodes := twoDeleted()
	probe := newRecoverEnv(t, nodes...)
	code, out := probe.image(t, "ls", probe.ref, "--json", "/")
	if code != 0 {
		t.Fatalf("ls: %d %s", code, out)
	}
	var entries []struct {
		Path  string `json:"path"`
		Entry struct {
			ID string `json:"id"`
		} `json:"entry"`
	}
	liveID := ""
	if err := json.Unmarshal(jsonPart(out), &entries); err == nil {
		for _, en := range entries {
			if en.Path == "/live.txt" {
				liveID = en.Entry.ID
			}
		}
	}
	if liveID == "" {
		t.Fatalf("no live id in ls output %s", out)
	}
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"all", []string{"--all"}, 0},
		{"list", []string{"--list"}, 0},
		{"neither all nor refs", nil, 2},
		{"refs with all", []string{"--all", "/docs"}, 2},
		{"confidence above 100", []string{"--all", "--min-confidence", "101"}, 2},
		{"confidence negative", []string{"--all", "--min-confidence", "-1"}, 2},
		{"negative max-files", []string{"--all", "--max-files", "-1"}, 2},
		{"negative max-bytes", []string{"--all", "--max-bytes", "-1"}, 2},
		{"empty id ref", []string{"id:"}, 2},
		{"unknown flag", []string{"--all", "--use-journal"}, 2},
		{"live ref", []string{"id:" + liveID}, 1},
		{"unknown id", []string{"id:nid:99999999"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecoverEnv(t, nodes...)
			code, out := env.recover(t, tc.args...)
			if code != tc.want {
				t.Errorf("exit %d, want %d:\n%s", code, tc.want, out)
			}
		})
	}
	// The failures the command cannot provoke without a hostile host: the error mapping itself.
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"insufficient space", fmt.Errorf("x: %w", examine.ErrInsufficientSpace), 1},
		{"cancelled", fmt.Errorf("x: %w", context.Canceled), 1},
		{"no recovery is not integrity", fmt.Errorf("mtfs: %w", filesys.ErrNoRecovery), 1},
		{"not deleted", fmt.Errorf("x: %w", filesys.ErrNotDeleted), 1},
		{"invalid limit", fmt.Errorf("x: %w", examine.ErrInvalidLimit), 2},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestImageRecoverTamperedParentExits4(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	p := filepath.Join(e.c, filepath.FromSlash(e.rec.Path))
	b, err := os.ReadFile(p) //nolint:gosec // inside the test case
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b[:len(b)-512], 0o600); err != nil { //nolint:gosec // inside the test case
		t.Fatal(err)
	}
	n := len(auditActions(t, e.c))
	code, out := e.recover(t, "--all")
	if code != 4 {
		t.Errorf("exit %d, want 4:\n%s", code, out)
	}
	if got := len(recoveredRecords(t, e.c)); got != 0 {
		t.Errorf("%d artifacts written", got)
	}
	for _, a := range auditActions(t, e.c)[n:] {
		if strings.HasPrefix(a, "analysis.") {
			t.Errorf("audit entry %s written for a tampered parent", a)
		}
	}
}

// noRecoveryDrivers are MTFS drivers that hide the Recoverer.
var noRecoveryDrivers = []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	fsys, err := fstest.Open(r, size)
	if err != nil {
		return nil, err
	}
	return struct{ filesys.FileSystem }{fsys}, nil
}}}

func TestImageRecoverUnsupportedFilesystemWritesNothing(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	e.d = Deps{FSDrivers: noRecoveryDrivers}
	n := len(auditActions(t, e.c))
	for _, args := range [][]string{{"--all"}, {"--list"}} {
		code, out := e.recover(t, args...)
		if code != 1 || !strings.Contains(out, "does not support recovery") || !strings.Contains(out, "mtfs") {
			t.Errorf("%v: exit %d, want 1 naming the filesystem and 'does not support recovery':\n%s", args, code, out)
		}
	}
	if acts := auditActions(t, e.c)[n:]; !slices.Equal(acts, []string{"case.open", "case.open"}) {
		t.Errorf("audit grew by %v, want only case.open per command", acts)
	}
	if got := len(recoveredRecords(t, e.c)); got != 0 { // (this opens the case, so it comes after the audit check)
		t.Errorf("%d artifacts written", got)
	}
}

func hasRawControl(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' { // the progress meter on stderr redraws with \r; it holds nothing from the case
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

func TestImageRecoverEscapesNames(t *testing.T) {
	hostile := "ev\x1b[31mil\u202edcba\u0085nl\nline\xff.bin"
	nodes := []fstest.Node{
		recNode("/"+hostile, recPat(1, recBS)),
		recNode("/bad\x1b[2Jmap.bin", recPat(2, recBS), fstest.RecoverMap{Method: recMethod, Runs: []filesys.Run{{Offset: 1 << 40, Length: recBS}}}),
	}
	e := newRecoverEnv(t, nodes...)
	for _, args := range [][]string{{"--list"}, {"--all"}} {
		var stdout, stderr bytes.Buffer
		d := Deps{FSDrivers: mtfsDrivers, Out: &stdout, Err: &stderr, In: strings.NewReader(""), Registry: device.NewRegistry()}
		code := Run(append([]string{"image", "recover", "--case", e.c, e.ref}, args...), d)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s%s", args, code, stdout.String(), stderr.String())
		}
		for name, s := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
			if hasRawControl(s) {
				t.Errorf("%v %s holds a raw control or format rune: %q", args, name, s)
			}
		}
	}
	code, out := e.recover(t, "--list", "--json")
	if code != 0 {
		t.Fatal(out)
	}
	var l listJSON
	if err := json.Unmarshal(jsonPart(out), &l); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range l.Items {
		found = found || strings.Contains(it.Path, "\x1b[31m") || strings.Contains(it.Path, "\u202e")
	}
	if !found {
		t.Errorf("json lost the original strings: %+v", l.Items)
	}
	b, err := os.ReadFile(filepath.Join(e.c, "audit.jsonl")) //nolint:gosec // inside the test case
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`\u001b[31m`)) {
		t.Errorf("the audit log does not hold the original escape sequence")
	}
}

func TestExtractDeletedSkipMessageNamesImageRecover(t *testing.T) {
	e := newImgEnv(t)
	code, out := e.image(t, "extract", e.ref, "-r", "/")
	if code != 0 || !strings.Contains(out, "image recover") {
		t.Errorf("extract: %d, output lacks 'image recover':\n%s", code, out)
	}
	if strings.Contains(out, "recovered:") {
		t.Errorf("extract printed a recovered line:\n%s", out)
	}
}

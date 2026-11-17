package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newState(t *testing.T) *state {
	t.Helper()
	scratch := t.TempDir()
	return &state{
		root: moduleRoot(t), scratch: scratch, caseDir: filepath.Join(scratch, "ACCEPT"),
		refs: map[string]string{}, oracle: map[string]json.RawMessage{}, out: &bytes.Buffer{},
	}
}

// The table names every recovery command, ends with the tamper check, and every fixture it names exists and
// matches its oracle (the import steps gunzip each into the scratch directory and compare hashes).
func TestStepsCoverEveryRecoveryCommand(t *testing.T) {
	st := newState(t)
	var all []string
	table := steps()
	for i, s := range table {
		if s.name == "verify-tamper" {
			if i != len(table)-1 || s.wantExit != 4 {
				t.Errorf("verify-tamper is step %d of %d with wantExit %d, want the last with 4", i+1, len(table), s.wantExit)
			}
			continue
		}
		if s.wantExit != 0 {
			t.Errorf("step %s pins exit %d, want 0 (the oracles have no undecided entry)", s.name, s.wantExit)
		}
		args, err := s.args(st)
		if err != nil {
			t.Fatalf("step %s: %v", s.name, err)
		}
		all = append(all, strings.Join(args, " "))
		if strings.HasPrefix(s.name, "import-") {
			st.refs[strings.TrimPrefix(s.name, "import-")] = "id-" + strings.TrimPrefix(s.name, "import-")
		}
	}
	if len(table) != 21 {
		t.Errorf("%d steps, want 21", len(table))
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{
		"image recover", "--use-journal", "--rollforward", "image carve", "image slack", "image journal report",
		"image journal blocks", "sqlite recover", "case verify",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no step runs %q", want)
		}
	}
	for _, f := range fixtures {
		if _, err := os.Stat(f.imagePath(st.root)); err != nil {
			t.Errorf("fixture %s: %v", f.name, err)
		}
		if _, err := os.Stat(f.oraclePath(st.root)); err != nil {
			t.Errorf("oracle of %s: %v", f.name, err)
		}
	}
	if !strings.Contains(joined, "--region live") {
		t.Error("the journal blocks step does not name a live region")
	}
}

func TestOracleReaders(t *testing.T) {
	st := newState(t)
	for _, f := range fixtures {
		raw, err := os.ReadFile(f.oraclePath(st.root))
		if err != nil {
			t.Fatal(err)
		}
		if sha, err := oracleImageSHA(raw, f.name); err != nil || len(sha) != 64 {
			t.Errorf("%s: image sha %q: %v", f.name, sha, err)
		}
		st.oracle[f.name] = raw
	}
	if sha, err := carvedSQLiteSHA(st.oracle["carve-fat12"]); err != nil || sha != "30cfa0a5768a590fcde1050540eebbf9252aa3813936106ada8de9490eb5971a" {
		t.Errorf("carved sqlite sha %q: %v", sha, err)
	}
	if seq, err := firstLiveSeq(st.oracle["ext4-journal-crash"]); err != nil || seq != 66 {
		t.Errorf("first live seq %d: %v, want 66", seq, err)
	}
	if _, err := firstLiveSeq([]byte(`{"txns":[{"seq":1,"region":"stale","committed":true},{"seq":2,"region":"live","committed":false}]}`)); err == nil {
		t.Error("a journal oracle with no committed live transaction is accepted")
	}
	if _, err := oracleImageSHA([]byte(`{}`), "x"); err == nil {
		t.Error("an oracle without image_sha256 is accepted")
	}
	if got, _ := oracleImageSHA([]byte(`{"images":[{"name":"a","image_sha256":"A"},{"name":"b","image_sha256":"B"}],"generator":{"image_sha256":"G"}}`), "b"); got != "B" {
		t.Errorf("images[b] gave %q", got)
	}
	if got, _ := oracleImageSHA([]byte(`{"generator":{"image_sha256":"G"}}`), "b"); got != "G" {
		t.Errorf("generator gave %q", got)
	}
}

// The checks read the case's manifest and the command's output.
func TestStepChecks(t *testing.T) {
	st := newState(t)
	write(t, filepath.Join(st.caseDir, "manifest.jsonl"),
		`{"id":"a","path":"p/a","sha256":"1","source":{"kind":"extract"}}`+"\n"+
			`{"id":"b","path":"p/b","sha256":"2","source":{"kind":"recover","derived":{"recovery":{"class":"deleted-file","method":"fat-contiguous"}}}}`+"\n")
	before := []manifestRec{{ID: "a"}}
	st.before = before
	rec := imageStep("recover-fat", "fat16-recover", []string{"recover"}, nil, "a recovery", isMethod("deleted-file", "fat-contiguous"))
	if err := rec.check(st, nil); err != nil {
		t.Errorf("a matching new artifact: %v", err)
	}
	wrong := imageStep("x", "fat16-recover", []string{"recover"}, nil, "an ext4 recovery", isMethod("deleted-file", "ext4-extent-leaf"))
	if err := wrong.check(st, nil); err == nil || !strings.Contains(err.Error(), "an ext4 recovery") {
		t.Errorf("a wrong method: %v", err)
	}
	st.before = []manifestRec{{ID: "a"}, {ID: "b"}}
	if err := rec.check(st, nil); err == nil {
		t.Error("an artifact that is not new satisfies the step")
	}

	report := `{"inputs":[{"role":"db","recovered":true,"confidence_cap":70},{"role":"wal","recovered":true,"confidence_cap":55}],"counts":{"written":7}}`
	if err := checkSQLiteReport(st, []byte("note\n"+report)); err != nil {
		t.Errorf("a good report: %v", err)
	}
	if !strings.Contains(st.out.(*bytes.Buffer).String(), "counts.written 7") {
		t.Errorf("counts.written is not printed: %q", st.out.(*bytes.Buffer).String())
	}
	for name, bad := range map[string]string{
		"live database": `{"inputs":[{"role":"db","recovered":false,"confidence_cap":70}],"counts":{}}`,
		"cap too high":  `{"inputs":[{"role":"db","recovered":true,"confidence_cap":71}],"counts":{}}`,
		"no cap":        `{"inputs":[{"role":"db","recovered":true}],"counts":{}}`,
		"no db":         `{"inputs":[{"role":"wal","recovered":true,"confidence_cap":1}],"counts":{}}`,
		"not json":      `nothing`,
	} {
		if err := checkSQLiteReport(st, []byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if err := noProblem(st, []byte("OK")); err != nil {
		t.Errorf("a clean verify: %v", err)
	}
	if err := noProblem(st, []byte("PROBLEM: x")); err == nil {
		t.Error("PROBLEM on stdout is accepted")
	}
	st.stderr = []byte("PROBLEM: y")
	if err := noProblem(st, nil); err == nil {
		t.Error("PROBLEM on stderr is accepted")
	}
}

func TestTamperFlipsByteZeroOfADeletedFile(t *testing.T) {
	st := newState(t)
	write(t, filepath.Join(st.caseDir, "manifest.jsonl"),
		`{"id":"a","path":"files/a","sha256":"1","source":{"kind":"extract"}}`+"\n"+
			`{"id":"b","path":"files/b","sha256":"2","source":{"kind":"recover","derived":{"recovery":{"class":"deleted-file","method":"m"}}}}`+"\n")
	write(t, filepath.Join(st.caseDir, "files", "a"), "AAA")
	write(t, filepath.Join(st.caseDir, "files", "b"), "BBB")
	if err := tamper(st); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(st.caseDir, "files", "a"))
	b, _ := os.ReadFile(filepath.Join(st.caseDir, "files", "b"))
	if string(a) != "AAA" || b[0] != 'B'^0xff || string(b[1:]) != "BB" {
		t.Errorf("a %q b %q: want only byte 0 of the deleted-file artifact flipped", a, b)
	}
	st2 := newState(t)
	write(t, filepath.Join(st2.caseDir, "manifest.jsonl"), `{"id":"a","path":"files/a","sha256":"1","source":{"kind":"extract"}}`+"\n")
	if err := tamper(st2); err == nil {
		t.Error("tampering with a case that holds no deleted-file artifact succeeds")
	}
}

func TestReadManifestIsStrict(t *testing.T) {
	dir := t.TempDir()
	good := `{"id":"a","path":"p","sha256":"1","source":{"kind":"recover","derived":{"recovery":{"class":"deleted-file","method":"m"}}}}` + "\n" +
		`{"id":"b","path":"q","sha256":"2","source":{"kind":"extract"}}` + "\n"
	write(t, filepath.Join(dir, "manifest.jsonl"), good)
	recs, err := readManifest(dir)
	if err != nil || len(recs) != 2 {
		t.Fatalf("a good manifest: %v %v", recs, err)
	}
	if r := recs[0]; r.ID != "a" || r.Kind != "recover" || r.Class != "deleted-file" || r.Method != "m" || r.SHA256 != "1" || r.Path != "p" {
		t.Errorf("record %+v", r)
	}
	for name, bad := range map[string]string{
		"malformed line": good + "{not json}\n",
		"no id":          good + `{"path":"x"}` + "\n",
		"blank line":     good + "\n",
		"empty":          "",
	} {
		write(t, filepath.Join(dir, "manifest.jsonl"), bad)
		if _, err := readManifest(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := readManifest(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Errorf("a missing manifest: %v, want not-exist", err)
	}
	got := newSince([]manifestRec{{ID: "a"}, {ID: "c"}}, []manifestRec{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}})
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "d" {
		t.Errorf("newSince %v, want b and d", got)
	}
}

func TestImportStepRefusesAFixtureThatDiffersFromItsOracle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "x")
	write(t, filepath.Join(dir, "oracle.json"), `{"generator":{"image_sha256":"`+strings.Repeat("0", 64)+`"}}`)
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	_, _ = zw.Write([]byte("image bytes"))
	_ = zw.Close()
	write(t, filepath.Join(dir, "x.img.gz"), zb.String())
	f := fixture{name: "x", dir: "internal/x", oracle: "oracle.json"}
	st := newState(t)
	st.root = root
	_, err := importStep(f).args(st)
	if err == nil || !strings.Contains(err.Error(), "fixture and oracle differ") {
		t.Errorf("error %v, want fixture and oracle differ", err)
	}
}

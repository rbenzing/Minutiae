package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

var mtfsDrivers = []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open}}

const imgLinuxType = "0fc63daf-8483-4772-8e79-3d69d8477de4"

// imgDisk lays one MTFS filesystem out as the only partition of a GPT disk.
func imgDisk(nodes ...fstest.Node) []byte {
	fsys := fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})
	const firstLBA = 40
	sectors := uint64(len(fsys)+511) / 512
	img := volumetest.GPT(512, firstLBA+sectors+8+40, "11111111-2222-3333-4444-555555555555", []volumetest.Part{{
		StartLBA: firstLBA, Sectors: sectors, TypeGUID: imgLinuxType,
		GUID: "00000000-0000-0000-0000-000000000001", Name: "data",
	}})
	copy(img[firstLBA*512:], fsys)
	return img
}

func defaultNodes() []fstest.Node {
	return []fstest.Node{
		{Path: "/docs/a.txt", Data: []byte("hello")},
		{Path: "/docs/b.txt", Data: []byte("second file")},
		{Path: "/top.txt", Data: []byte("top")},
		{Path: "/gone.txt", Data: []byte("deleted content"), Deleted: true},
	}
}

func imgFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type imgEnv struct {
	d     Deps
	c     string // case dir
	ref   string // parent artifact id
	rec   evidence.ManifestRecord
	nodes []fstest.Node
}

func newImgEnv(t *testing.T, nodes ...fstest.Node) *imgEnv {
	t.Helper()
	if len(nodes) == 0 {
		nodes = defaultNodes()
	}
	e := &imgEnv{d: Deps{FSDrivers: mtfsDrivers}, c: newCLICase(t), nodes: nodes}
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", imgFile(t, imgDisk(nodes...)))
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

func (e *imgEnv) image(t *testing.T, sub string, args ...string) (int, string) {
	t.Helper()
	full := append([]string{"image", sub, "--case", e.c}, args...)
	return run(t, e.d, full...)
}

func TestImageImportInfoLsStatExtractUnalloc(t *testing.T) {
	c := newCLICase(t)
	d := Deps{FSDrivers: mtfsDrivers}
	src := imgFile(t, imgDisk(defaultNodes()...))

	code, out := run(t, d, "image", "import", "--case", c, src)
	if code != 0 || !strings.Contains(out, filepath.Base(src)) || strings.Count(out, "\n") < 1 {
		t.Fatalf("import: %d %s", code, out)
	}
	recs, err := manifest(c)
	if err != nil || len(recs) != 1 {
		t.Fatalf("manifest: %v %v", recs, err)
	}
	rec := recs[0]
	if !strings.Contains(out, rec.SHA256) {
		t.Errorf("import output lacks the sha256 %s:\n%s", rec.SHA256, out)
	}
	if rec.Source.Kind != "import" || rec.Source.Segment != 1 {
		t.Errorf("import record source = %+v", rec.Source)
	}
	ref := rec.ID

	code, out = run(t, d, "image", "info", "--case", c, ref)
	if code != 0 {
		t.Fatalf("info: %d %s", code, out)
	}
	for _, want := range []string{"raw", "gpt", "512", "Linux filesystem", "data", "mtfs", "Label", "L"} {
		if !strings.Contains(out, want) {
			t.Errorf("info lacks %q:\n%s", want, out)
		}
	}
	code, out = run(t, d, "image", "info", "--case", c, ref, "--verify")
	if code != 0 || !strings.Contains(out, "container has no stored hashes") {
		t.Fatalf("info --verify: %d %s", code, out)
	}

	code, out = run(t, d, "image", "ls", "--case", c, ref)
	if code != 0 || !strings.Contains(out, "docs/") || !strings.Contains(out, "top.txt") || strings.Contains(out, "gone.txt") {
		t.Fatalf("ls: %d\n%s", code, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "d ") {
		t.Errorf("ls first line should start with the dir type char:\n%s", out)
	}
	code, out = run(t, d, "image", "ls", "--case", c, ref, "/docs")
	if code != 0 || !strings.Contains(out, "a.txt") || !strings.Contains(out, "b.txt") || strings.Contains(out, "top.txt") {
		t.Fatalf("ls /docs: %d\n%s", code, out)
	}
	code, out = run(t, d, "image", "ls", "--case", c, ref, "-r", "-p", "1")
	if code != 0 || !strings.Contains(out, "/docs/a.txt") || !strings.Contains(out, "/top.txt") {
		t.Fatalf("ls -r: %d\n%s", code, out)
	}

	code, out = run(t, d, "image", "stat", "--case", c, ref, "/docs/a.txt")
	if code != 0 || !strings.Contains(out, "/docs/a.txt") || !strings.Contains(out, "file") || !strings.Contains(out, "5") {
		t.Fatalf("stat: %d\n%s", code, out)
	}
	id := fieldAfter(out, "ID:")
	if id == "" {
		t.Fatalf("stat shows no ID:\n%s", out)
	}
	code, out = run(t, d, "image", "stat", "--case", c, ref, "id:"+id)
	if code != 0 || !strings.Contains(out, "/docs/a.txt") {
		t.Fatalf("stat id:%s: %d\n%s", id, code, out)
	}

	code, out = run(t, d, "image", "extract", "--case", c, ref, "-r", "/docs")
	if code != 0 || !strings.Contains(out, "extracted 2 files (") || !strings.Contains(out, "skipped 0") {
		t.Fatalf("extract: %d\n%s", code, out)
	}
	code, out = run(t, d, "image", "extract", "--case", c, ref, "/top.txt")
	if code != 0 || !strings.Contains(out, "extracted 1 files (") {
		t.Fatalf("extract file: %d\n%s", code, out)
	}
	code, out = run(t, d, "image", "unalloc", "--case", c, ref, "-p", "1")
	if code != 0 || !strings.Contains(out, "unallocated") {
		t.Fatalf("unalloc -p: %d\n%s", code, out)
	}
	code, out = run(t, d, "image", "unalloc", "--case", c, ref, "--volume")
	if code != 0 || !strings.Contains(out, "unallocated") {
		t.Fatalf("unalloc --volume: %d\n%s", code, out)
	}

	if code, out := run(t, d, "case", "verify", "--case", c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
}

func manifest(caseDir string) ([]evidence.ManifestRecord, error) {
	c, err := evidence.Open(caseDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return c.Manifest()
}

// fieldAfter returns the first whitespace-separated word following label on
// the line that contains it.
func fieldAfter(out, label string) string {
	for _, line := range strings.Split(out, "\n") {
		if _, rest, ok := strings.Cut(line, label); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
}

func TestImageImportSplitSegments(t *testing.T) {
	c := newCLICase(t)
	d := Deps{FSDrivers: mtfsDrivers}
	data := imgDisk(defaultNodes()...)
	dir := t.TempDir()
	half := len(data) / 2
	a, b := filepath.Join(dir, "disk.001"), filepath.Join(dir, "disk.002")
	for p, part := range map[string][]byte{a: data[:half], b: data[half:]} {
		if err := os.WriteFile(p, part, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out := run(t, d, "image", "import", "--case", c, "--device", "phone1", a, b)
	if code != 0 || !strings.Contains(out, "disk.001") || !strings.Contains(out, "disk.002") {
		t.Fatalf("import: %d %s", code, out)
	}
	recs, err := manifest(c)
	if err != nil || len(recs) != 2 || recs[0].Source.DeviceID != "phone1" || recs[1].Source.Segment != 2 {
		t.Fatalf("manifest = %+v %v", recs, err)
	}
	if code, out := run(t, d, "image", "ls", "--case", c, recs[0].ID); code != 0 || !strings.Contains(out, "top.txt") {
		t.Fatalf("ls of split image: %d %s", code, out)
	}
}

func TestImageLsDeletedFlag(t *testing.T) {
	e := newImgEnv(t)
	code, out := e.image(t, "ls", e.ref)
	if code != 0 || strings.Contains(out, "gone.txt") || strings.Contains(out, "[deleted]") {
		t.Fatalf("default ls shows deleted entries: %d\n%s", code, out)
	}
	code, out = e.image(t, "ls", e.ref, "--deleted")
	if code != 0 {
		t.Fatalf("ls --deleted: %d %s", code, out)
	}
	var found bool
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "gone.txt") {
			found = strings.Contains(line, "[deleted]")
		}
	}
	if !found {
		t.Fatalf("ls --deleted lacks the [deleted] marker:\n%s", out)
	}
}

func TestImageLsLineFormat(t *testing.T) {
	e := newImgEnv(t,
		fstest.Node{Path: "/f.txt", Data: []byte("hello"), Mode: 0o640, MTime: 1700000000},
		fstest.Node{Path: "/l", Link: "f.txt"},
		fstest.Node{Path: "/enc.bin", Data: []byte("zz"), Encrypted: true},
	)
	code, out := e.image(t, "ls", e.ref)
	if code != 0 {
		t.Fatalf("ls: %d %s", code, out)
	}
	lines := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		for _, name := range []string{"f.txt", "l", "enc.bin"} {
			if len(f) > 4 && f[4] == name {
				lines[name] = l
			}
		}
	}
	file := lines["f.txt"]
	want := fmt.Sprintf("- 0640 %12d %s f.txt", 5, time.Unix(1700000000, 0).UTC().Format(time.RFC3339))
	if file != want {
		t.Errorf("file line = %q, want %q", file, want)
	}
	if l := lines["l"]; !strings.HasPrefix(l, "l ") || !strings.HasSuffix(l, " l -> f.txt") {
		t.Errorf("symlink line = %q", l)
	}
	if l := lines["enc.bin"]; !strings.HasSuffix(l, "enc.bin [encrypted]") || !strings.Contains(l, " - enc.bin") {
		t.Errorf("encrypted line (absent mtime must print -) = %q", l)
	}
}

func TestImageNamesAreEscaped(t *testing.T) {
	e := newImgEnv(t, fstest.Node{Path: "/ev\x1b[31mil\nname", Data: []byte("x")})
	for _, args := range [][]string{{"ls", e.ref}, {"ls", e.ref, "-r"}} {
		code, out := e.image(t, args[0], args[1:]...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		if strings.ContainsAny(out, "\x1b\r") || strings.Count(out, "\n") != 1 {
			t.Errorf("%v printed raw control characters: %q", args, out)
		}
		if !strings.Contains(out, `\x1b`) {
			t.Errorf("%v does not show the escaped name: %q", args, out)
		}
	}
}

func TestImageJSONOutputs(t *testing.T) {
	e := newImgEnv(t)

	code, out := e.image(t, "info", e.ref, "--json")
	if code != 0 {
		t.Fatalf("info: %d %s", code, out)
	}
	var info examine.ImageInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil || info.Format != "raw" || info.Scheme != "gpt" || len(info.Partitions) != 1 {
		t.Fatalf("info json %q: %v (%+v)", out, err, info)
	}
	if info.ParentID != e.ref || info.Partitions[0].FSType != "mtfs" {
		t.Errorf("info = %+v", info)
	}

	code, out = e.image(t, "ls", e.ref, "--json", "--deleted")
	if code != 0 {
		t.Fatalf("ls: %d %s", code, out)
	}
	var items []struct {
		Path  string
		Entry struct {
			Name    string
			Type    string
			Deleted bool
		}
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil || len(items) != 3 {
		t.Fatalf("ls json %q: %v", out, err)
	}
	var sawDeleted bool
	for _, it := range items {
		if it.Path != "/"+it.Entry.Name {
			t.Errorf("item path %q vs name %q", it.Path, it.Entry.Name)
		}
		if it.Entry.Name == "gone.txt" {
			sawDeleted = it.Entry.Deleted && it.Entry.Type == "file"
		}
	}
	if !sawDeleted {
		t.Errorf("deleted entry not flagged in JSON: %s", out)
	}

	code, out = e.image(t, "stat", e.ref, "--json", "/docs/a.txt")
	var st struct {
		Path  string
		Entry struct {
			Name string
			Size int64
			ID   string
		}
	}
	if err := json.Unmarshal([]byte(out), &st); code != 0 || err != nil || st.Path != "/docs/a.txt" || st.Entry.Size != 5 || st.Entry.ID == "" {
		t.Fatalf("stat json: %d %q %v", code, out, err)
	}

	code, out = e.image(t, "extract", e.ref, "--json", "-r", "/docs")
	var sum examine.Summary
	if err := json.Unmarshal(jsonPart(out), &sum); code != 0 || err != nil || sum.Files != 2 || sum.AnalysisID == "" {
		t.Fatalf("extract json: %d %q %v", code, out, err)
	}

	code, out = e.image(t, "unalloc", e.ref, "--json", "--volume")
	if err := json.Unmarshal(jsonPart(out), &sum); code != 0 || err != nil || sum.Files != 1 {
		t.Fatalf("unalloc json: %d %q %v", code, out, err)
	}
}

func TestImageExtractEncryptedNote(t *testing.T) {
	e := newImgEnv(t,
		fstest.Node{Path: "/enc.bin", Data: []byte("cipher"), Encrypted: true},
		fstest.Node{Path: "/plain.txt", Data: []byte("plain")},
	)
	const note = "note: 1 encrypted files were extracted as ciphertext (decryption is roadmap sub-project 10)"
	code, out := e.image(t, "extract", e.ref, "/enc.bin", "/plain.txt")
	if code != 0 || !strings.Contains(out, "extracted 2 files (") || !strings.Contains(out, note) {
		t.Fatalf("extract: %d\n%s", code, out)
	}
	code, out = e.image(t, "extract", e.ref, "--include-encrypted", "/enc.bin")
	if code != 0 || strings.Contains(out, "note:") {
		t.Fatalf("extract --include-encrypted: %d\n%s", code, out)
	}
	code, out = e.image(t, "extract", e.ref, "/plain.txt")
	if code != 0 || strings.Contains(out, "note:") {
		t.Fatalf("no encrypted files, no note: %d\n%s", code, out)
	}
}

func TestImageExtractDeletedAndDirectoryErrors(t *testing.T) {
	e := newImgEnv(t)
	// A directory without -r is a refused request (exit 1), nothing is written.
	if code, out := e.image(t, "extract", e.ref, "/docs"); code != ExitError || !strings.Contains(out, "-r") {
		t.Fatalf("dir without -r: %d %s", code, out)
	}
	// A deleted entry (reached by id) is skipped with a warning, never extracted.
	_, out := e.image(t, "ls", e.ref, "--deleted", "--json")
	var items []struct {
		Entry struct {
			ID      string
			Name    string
			Deleted bool
		}
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, it := range items {
		if it.Entry.Deleted {
			id = it.Entry.ID
		}
	}
	code, out := e.image(t, "extract", e.ref, "id:"+id)
	if code != 0 || !strings.Contains(out, "extracted 0 files (") || !strings.Contains(out, "skipped 1") {
		t.Fatalf("extract deleted: %d\n%s", code, out)
	}
}

func TestImageUnallocNeedsTarget(t *testing.T) {
	e := newImgEnv(t)
	for _, args := range [][]string{{}, {"-p", "1", "--volume"}} {
		if code, out := e.image(t, "unalloc", append([]string{e.ref}, args...)...); code != ExitUsage {
			t.Errorf("unalloc %v: code %d, want 2: %s", args, code, out)
		}
	}
}

func TestImageBadPartitionFlagIsUsage(t *testing.T) {
	e := newImgEnv(t)
	for _, sub := range []string{"ls", "stat", "extract", "unalloc"} {
		args := []string{e.ref, "-p", "-1"}
		switch sub {
		case "stat", "extract":
			args = append(args, "/top.txt")
		}
		if code, out := e.image(t, sub, args...); code != ExitUsage {
			t.Errorf("%s -p -1: code %d, want 2: %s", sub, code, out)
		}
	}
	if code, out := e.image(t, "ls", e.ref, "-p", "7"); code != ExitError || !strings.Contains(out, "no partition 7") {
		t.Errorf("ls -p 7: %d %s", code, out)
	}
}

func TestImageArgumentCountsAreUsage(t *testing.T) {
	e := newImgEnv(t)
	for _, args := range [][]string{
		{"import"}, {"info"}, {"info", e.ref, "extra"}, {"ls"}, {"stat", e.ref}, {"extract", e.ref}, {"unalloc"},
	} {
		sub := args[0]
		if code, out := e.image(t, sub, args[1:]...); code != ExitUsage {
			t.Errorf("%v: code %d, want 2: %s", args, code, out)
		}
	}
	if code, _ := run(t, e.d, "image", "info", e.ref); code != ExitUsage {
		t.Errorf("missing --case: want usage error")
	}
}

func TestImageUnknownRefExits1(t *testing.T) {
	e := newImgEnv(t)
	for _, sub := range []string{"info", "ls", "unalloc"} {
		args := []string{"no-such-artifact"}
		if sub == "unalloc" {
			args = append(args, "--volume")
		}
		code, out := e.image(t, sub, args...)
		if code != ExitError || !strings.Contains(out, "unknown artifact") {
			t.Errorf("%s: %d %s", sub, code, out)
		}
	}
}

func TestImageUnknownPathExits1(t *testing.T) {
	e := newImgEnv(t)
	for _, sub := range []string{"ls", "stat", "extract"} {
		code, out := e.image(t, sub, e.ref, "/nope")
		if code != ExitError || !strings.Contains(out, "not found") {
			t.Errorf("%s /nope: %d %s", sub, code, out)
		}
	}
}

func TestImageTamperedParentExits4(t *testing.T) {
	e := newImgEnv(t)
	f, err := os.OpenFile(filepath.Join(e.c, filepath.FromSlash(e.rec.Path)), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"ls", "info"} {
		if code, out := e.image(t, sub, e.ref); code != ExitIntegrity {
			t.Errorf("%s on a tampered parent: %d %s", sub, code, out)
		}
	}
}

func TestImageIncompleteParentWarns(t *testing.T) {
	dir := t.TempDir()
	c, err := evidence.Create(dir, evidence.CreateOptions{ID: "C1", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	data := imgDisk(defaultNodes()...)
	rec, cerr := c.Capture("dev", evidence.NewAcquisitionID(time.Now()), "part.img",
		evidence.Source{Kind: "partition", DeviceID: "dev"}, func(w io.Writer) error {
			if _, err := w.Write(data); err != nil {
				return err
			}
			return errors.New("device vanished")
		})
	if cerr == nil || !rec.Incomplete {
		t.Fatalf("setup: want an incomplete artifact, got %+v, %v", rec, cerr)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{FSDrivers: mtfsDrivers}, "image", "ls", "--case", filepath.Join(dir, "C1"), rec.ID)
	if code != 0 || !strings.Contains(out, "warning: parent image is flagged incomplete; results may be partial") {
		t.Fatalf("ls: %d\n%s", code, out)
	}
}

func TestImageUnsupportedContainerAndUnrecognizedFS(t *testing.T) {
	c := newCLICase(t)
	d := Deps{FSDrivers: mtfsDrivers}
	// EWF2 signature: a recognised but unsupported container.
	ewf2 := append([]byte("EVF2\x0d\x0a\x81\x00"), make([]byte, 4096)...)
	code, out := run(t, d, "image", "import", "--case", c, "--json", imgFile(t, ewf2))
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil {
		t.Fatal(err)
	}
	code, out = run(t, d, "image", "info", "--case", c, recs[0].ID)
	if code != ExitError || !strings.Contains(out, "unsupported") {
		t.Fatalf("info on EWF2: %d %s", code, out)
	}

	// A blank image has no filesystem: info works, ls explains why it cannot.
	code, out = run(t, d, "image", "import", "--case", c, "--json", imgFile(t, make([]byte, 64*1024)))
	if code != 0 {
		t.Fatalf("import blank: %d %s", code, out)
	}
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil {
		t.Fatal(err)
	}
	if code, out = run(t, d, "image", "info", "--case", c, recs[0].ID); code != 0 || !strings.Contains(out, "no recognized filesystem") {
		t.Fatalf("info blank: %d %s", code, out)
	}
	if code, out = run(t, d, "image", "ls", "--case", c, recs[0].ID); code != ExitError || !strings.Contains(out, "no partition holds a recognized filesystem") {
		t.Fatalf("ls blank: %d %s", code, out)
	}
}

func TestImageErrorExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&filesys.CorruptError{Structure: "x", Offset: 1, Reason: "r"}, ExitError},
		{fmt.Errorf("w: %w", filesys.ErrUnsupported), ExitError},
		{fmt.Errorf("w: %w", filesys.ErrEncrypted), ExitError},
		{fmt.Errorf("w: %w", image.ErrUnsupportedContainer), ExitError},
		{fmt.Errorf("w: %w", evidence.ErrUnknownArtifact), ExitError},
		{fmt.Errorf("w: %w", evidence.ErrIntegrity), ExitIntegrity},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestImageUnknownSubcommandIsUsage(t *testing.T) {
	if code, _ := run(t, Deps{}, "image", "bogus"); code != ExitUsage {
		t.Fatalf("code = %d", code)
	}
}

// jsonPart drops the progress text that `run` merges in from stderr.
func jsonPart(out string) []byte {
	if i := strings.IndexAny(out, "[{"); i >= 0 {
		return []byte(out[i:])
	}
	return []byte(out)
}

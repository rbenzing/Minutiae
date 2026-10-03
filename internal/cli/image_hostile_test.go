package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/volume"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

// hostile carries a terminal escape sequence and a bidi override.
const hostile = "\x1b[31m\u202eevil"

// hostileDisk wraps a hand-written MTFS table as the only partition of a GPT disk.
func hostileDisk(t *testing.T, table any) []byte {
	t.Helper()
	tbl, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.Encode(tbl, nil)
	const firstLBA = 40
	sectors := uint64(len(fsys)+511) / 512
	img := volumetest.GPT(512, firstLBA+sectors+8+40, "11111111-2222-3333-4444-555555555555", []volumetest.Part{{
		StartLBA: firstLBA, Sectors: sectors, TypeGUID: imgLinuxType,
		GUID: "00000000-0000-0000-0000-000000000001", Name: "data",
	}})
	copy(img[firstLBA*512:], fsys)
	return img
}

func importRaw(t *testing.T, c string, data []byte) string {
	t.Helper()
	code, out := run(t, Deps{}, "image", "import", "--case", c, "--json", imgFile(t, data))
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil {
		t.Fatal(err)
	}
	return recs[0].ID
}

func noRawControl(t *testing.T, what, out string) {
	t.Helper()
	for _, bad := range []string{"\x1b", "\u202e", "\x00"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s printed raw %q: %q", what, bad, out)
		}
	}
}

// hostileCycleTable has a directory whose child directory repeats its id (a
// cycle) and names that carry an escape sequence and a bidi override.
func hostileCycleTable() map[string]any {
	return map[string]any{"block_size": 512, "label": hostile, "entries": []map[string]any{
		{"id": "1", "parent_id": "", "name": "", "type": "dir"},
		{"id": "2", "parent_id": "1", "name": "a" + hostile, "type": "dir"},
		{"id": "2", "parent_id": "2", "name": "loop" + hostile, "type": "dir"},
		{"id": "3", "parent_id": "1", "name": "z", "type": "file", "link": "t" + hostile},
	}}
}

func TestImageErrorsAndWarningsNeverPrintRawControlCharacters(t *testing.T) {
	c := newCLICase(t)
	d := Deps{FSDrivers: mtfsDrivers}
	ref := importRaw(t, c, hostileDisk(t, hostileCycleTable()))

	code, out := run(t, d, "image", "ls", "--case", c, ref, "-r")
	if code != 0 || !strings.Contains(out, "warning:") || !strings.Contains(out, "cycle") {
		t.Fatalf("ls -r: %d %q", code, out)
	}
	noRawControl(t, "ls -r", out)
	if !strings.Contains(out, `\x1b`) || !strings.Contains(out, `\u202e`) {
		t.Errorf("ls -r does not show the escapes: %q", out)
	}

	for name, args := range map[string][]string{
		"stat missing id":     {"stat", ref, "id:nope" + hostile},
		"stat missing path":   {"stat", ref, "/nope" + hostile},
		"extract dir by id":   {"extract", ref, "id:2"},
		"extract missing id":  {"extract", ref, "id:" + hostile},
		"ls missing ref":      {"ls", "no" + hostile},
		"info missing ref":    {"info", "no" + hostile},
		"extract dir by path": {"extract", ref, "/a" + hostile},
	} {
		code, out := run(t, d, append([]string{"image", args[0], "--case", c}, args[1:]...)...)
		if code != ExitError {
			t.Errorf("%s: code %d, want 1: %q", name, code, out)
		}
		noRawControl(t, name, out)
	}
	code, out = run(t, d, "image", "stat", "--case", c, ref, "id:nope"+hostile)
	if !strings.Contains(out, `id:nope\x1b[31m\u202eevil`) {
		t.Errorf("escaped id missing: %q (code %d)", out, code)
	}

	// info (label) and stat (link target) print the escaped forms.
	code, out = run(t, d, "image", "info", "--case", c, ref)
	noRawControl(t, "info", out)
	if code != 0 || !strings.Contains(out, `\x1b`) {
		t.Errorf("info: %d %q", code, out)
	}
	code, out = run(t, d, "image", "stat", "--case", c, ref, "/z")
	noRawControl(t, "stat", out)
	if code != 0 || !strings.Contains(out, `\x1b`) {
		t.Errorf("stat: %d %q", code, out)
	}
}

func TestEscapeText(t *testing.T) {
	for in, want := range map[string]string{
		"plain text é ✓":     "plain text é ✓",
		"two\nlines":         "two\nlines",
		"\x1b[31m":           `\x1b[31m`,
		"a\u202eb":           `a\u202eb`,
		"a\xffb":             `a\xffb`,
		"tab\there\r":        `tab\x09here\x0d`,
		"nul\x00":            `nul\x00`,
		"\U000e0001tag":      `\U000e0001tag`,
		"\u200bzero\u2066wi": `\u200bzero\u2066wi`,
	} {
		if got := escapeText(in); got != want {
			t.Errorf("escapeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintersEscapeInfoAndEntryText(t *testing.T) {
	var b strings.Builder
	printImageInfo(&b, examine.ImageInfo{
		Path: "p" + hostile, Format: "raw", Scheme: "gpt" + hostile, DiskGUID: hostile,
		Metadata: []image.KV{{Key: "k" + hostile, Value: "v" + hostile}},
		Warnings: []string{"warn" + hostile},
		Partitions: []examine.PartitionInfo{
			{
				Partition: volume.Partition{Index: 1, Name: hostile, TypeName: hostile, Type: hostile}, FSType: "x" + hostile,
				FSInfo: &filesys.Info{
					Type: "t" + hostile, Label: hostile, UUID: hostile, Features: []string{hostile}, Volumes: []string{hostile},
					Warnings: []string{"fw" + hostile},
				},
			},
			{Partition: volume.Partition{Index: 2}, Error: "err" + hostile},
		},
	})
	printEntry(&b, "/p"+hostile, filesys.Entry{
		Name: hostile, ID: hostile, LinkTarget: hostile, Attrs: []filesys.KV{{Key: "ak" + hostile, Value: "av" + hostile}},
	})
	out := b.String()
	noRawControl(t, "printers", out)
	for _, want := range []string{"warn", "fw", "err", "av", "ak", `k\x1b`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func auditLines(t *testing.T, c string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestImageUsageErrorsDoNotTouchTheCase(t *testing.T) {
	e := newImgEnv(t)
	before := auditLines(t, e.c)
	for name, args := range map[string][]string{
		"unalloc neither": {"unalloc", e.ref},
		"unalloc both":    {"unalloc", e.ref, "-p", "1", "--volume"},
		"unalloc -p -2":   {"unalloc", e.ref, "-p", "-2"},
		"import blank":    {"import", "--device", "", filepath.Join(t.TempDir(), "x.img")},
		"import spaces":   {"import", "--device", "  \t", filepath.Join(t.TempDir(), "x.img")},
	} {
		if code, out := e.image(t, args[0], args[1:]...); code != ExitUsage {
			t.Errorf("%s: code %d, want 2: %s", name, code, out)
		}
	}
	if after := auditLines(t, e.c); after != before {
		t.Errorf("usage errors changed the audit log: %d -> %d lines", before, after)
	}
	if recs, err := manifest(e.c); err != nil || len(recs) != 1 {
		t.Errorf("manifest changed: %d records, %v", len(recs), err)
	}
}

func TestImageJSONKeysAreSnakeCase(t *testing.T) {
	e := newImgEnv(t, fstest.Node{Path: "/f.txt", Data: []byte("x"), MTime: 1700000000})
	keys := func(args ...string) map[string]any {
		t.Helper()
		code, out := e.image(t, args[0], args[1:]...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		var v any
		if err := json.Unmarshal(jsonPart(out), &v); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		switch x := v.(type) {
		case map[string]any:
			return x
		case []any:
			return x[0].(map[string]any)
		}
		t.Fatalf("unexpected JSON %T", v)
		return nil
	}
	has := func(what string, m map[string]any, ks ...string) {
		t.Helper()
		for _, k := range ks {
			if _, ok := m[k]; !ok {
				t.Errorf("%s lacks key %q: %v", what, k, m)
			}
		}
	}
	info := keys("info", e.ref, "--json")
	has("info", info, "parent_id", "sha256", "sector_size", "metadata", "partitions", "unallocated", "warnings")
	has("partition", info["partitions"].([]any)[0].(map[string]any), "index", "start", "length", "type_name", "fs_type", "fs")
	has("ls item", keys("ls", e.ref, "--json"), "path", "entry")
	entry := keys("stat", e.ref, "--json", "/f.txt")["entry"].(map[string]any)
	has("entry", entry, "name", "id", "type", "size", "mode", "uid", "gid", "times", "deleted", "encrypted")
	mod := entry["times"].(map[string]any)["modified"].(map[string]any)
	if mod["time"] != "2023-11-14T22:13:20Z" || mod["zone_known"] != true {
		t.Errorf("modified = %v", mod)
	}
	has("summary", keys("extract", e.ref, "--json", "/f.txt"), "analysis_id", "files", "bytes", "skipped", "artifacts")
}

func TestImageJSONHostileTimestampDoesNotBreakOutput(t *testing.T) {
	const year10000 = 253402300800
	table := map[string]any{"block_size": 512, "entries": []map[string]any{
		{"id": "1", "parent_id": "", "name": "", "type": "dir"},
		{"id": "2", "parent_id": "1", "name": "far.txt", "type": "file", "mtime": year10000},
		{"id": "3", "parent_id": "1", "name": "old.txt", "type": "file", "mtime": -62135596800 - 86400*400},
	}}
	c := newCLICase(t)
	d := Deps{FSDrivers: mtfsDrivers}
	ref := importRaw(t, c, hostileDisk(t, table))
	for _, args := range [][]string{{"ls", ref, "--json"}, {"stat", ref, "--json", "/far.txt"}, {"ls", ref}, {"stat", ref, "/far.txt"}} {
		code, out := run(t, d, append([]string{"image", args[0], "--case", c}, args[1:]...)...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		if strings.Contains(strings.Join(args, " "), "--json") {
			var v any
			if err := json.Unmarshal(jsonPart(out), &v); err != nil {
				t.Fatalf("%v: invalid JSON: %v\n%s", args, err, out)
			}
			if !strings.Contains(out, "10000-01-01T00:00:00Z") {
				t.Errorf("%v: year-10000 time missing:\n%s", args, out)
			}
		}
	}
}

func TestImageLsJSONStreamsValidArray(t *testing.T) {
	e := newImgEnv(t)
	code, out := e.image(t, "ls", e.ref, "--json", "/docs")
	if code != 0 || !strings.HasPrefix(out, "[\n  {") || !strings.HasSuffix(out, "}\n]\n") || !strings.Contains(out, "},\n  {") {
		t.Fatalf("ls --json layout: %d %q", code, out)
	}
	if code, out = e.image(t, "ls", e.ref, "--json", "/top.txt"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	var one []pathEntryForTest
	if err := json.Unmarshal([]byte(out), &one); err != nil || len(one) != 1 {
		t.Fatalf("single-file listing: %v %q", err, out)
	}
	// An empty listing is a valid empty array.
	empty := newImgEnv(t, fstest.Node{Path: "/d", Dir: true})
	if code, out = empty.image(t, "ls", empty.ref, "--json", "/d"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty ls --json: %d %q", code, out)
	}
}

type pathEntryForTest struct {
	Path  string `json:"path"`
	Entry struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
	} `json:"entry"`
}

func TestImageLsOfDeletedEntryNotes(t *testing.T) {
	e := newImgEnv(t)
	_, out := e.image(t, "ls", e.ref, "--deleted", "--json")
	var items []pathEntryForTest
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, it := range items {
		if it.Entry.Deleted {
			id = it.Entry.ID
		}
	}
	code, out := e.image(t, "ls", e.ref, "id:"+id)
	if code != 0 || !strings.Contains(out, "note: entry is deleted; use --deleted") || strings.Contains(out, "gone.txt") {
		t.Fatalf("ls deleted without flag: %d %q", code, out)
	}
	code, out = e.image(t, "ls", e.ref, "id:"+id, "--deleted")
	if code != 0 || !strings.Contains(out, "gone.txt") || !strings.Contains(out, "[deleted]") || strings.Contains(out, "note:") {
		t.Fatalf("ls deleted with flag: %d %q", code, out)
	}
}

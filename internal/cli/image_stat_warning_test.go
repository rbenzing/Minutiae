package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// F92: stat prints one "warning:" prefix per warning line and the JSON warnings carry none.
func TestImageStatIncompleteWarningHasOnePrefix(t *testing.T) {
	e := newImgEnv(t)
	e.d.FSDrivers = driversWrapping(func(f filesys.FileSystem) filesys.FileSystem { return cutDocsFS{f} })
	_, lsOut := run(t, e.d, "image", "ls", "--case", e.c, "--deleted", "--json", e.ref)
	var ents []struct {
		Path  string
		Entry struct{ ID string }
	}
	if err := json.Unmarshal(jsonPart(lsOut), &ents); err != nil {
		t.Fatalf("ls json %q: %v", lsOut, err)
	}
	id := ""
	for _, en := range ents {
		if en.Path == "/gone.txt" {
			id = en.Entry.ID
		}
	}
	if id == "" {
		t.Fatalf("no /gone.txt in %q", lsOut)
	}
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "stat", "--case", e.c, e.ref, "id:"+id)
	if code != 0 {
		t.Fatalf("stat: exit %d, %q", code, errs)
	}
	if strings.Contains(errs+out, "warning: warning:") || strings.Count(errs, "warning: ") != 1 {
		t.Errorf("text output has the wrong warning prefixes:\n%s", errs)
	}
	code, out, _ = runSplit(context.Background(), t, e.d, "image", "stat", "--case", e.c, "--json", e.ref, "id:"+id)
	var js struct{ Warnings []string }
	if err := json.Unmarshal(jsonPart(out), &js); err != nil || code != 0 {
		t.Fatalf("stat json %q: %v (exit %d)", out, err, code)
	}
	if len(js.Warnings) != 1 || strings.HasPrefix(js.Warnings[0], "warning:") || !strings.Contains(js.Warnings[0], "live entry") {
		t.Errorf("json warnings = %q; want one item without a \"warning: \" prefix", js.Warnings)
	}
}

// F95: image extract of a deleted entry by id, after an incomplete search, says so in its summary (text and
// the JSON incomplete_ids) and still exits 0.
func TestImageExtractByIDAfterCutShowsTheIncompleteness(t *testing.T) {
	e := newImgEnv(t)
	e.d.FSDrivers = driversWrapping(func(f filesys.FileSystem) filesys.FileSystem { return cutDocsFS{f} })
	_, lsOut := run(t, e.d, "image", "ls", "--case", e.c, "--deleted", "--json", e.ref)
	var ents []struct {
		Path  string
		Entry struct{ ID string }
	}
	if err := json.Unmarshal(jsonPart(lsOut), &ents); err != nil {
		t.Fatalf("ls json %q: %v", lsOut, err)
	}
	id := ""
	for _, en := range ents {
		if en.Path == "/gone.txt" {
			id = en.Entry.ID
		}
	}
	if id == "" {
		t.Fatalf("no /gone.txt in %q", lsOut)
	}
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "extract", "--case", e.c, e.ref, "id:"+id)
	if code != 0 || !strings.Contains(out+errs, "a live entry with this id may exist") || !strings.Contains(out+errs, "/docs") {
		t.Errorf("text: exit %d\nstdout %q\nstderr %q", code, out, errs)
	}
	code, out, _ = runSplit(context.Background(), t, e.d, "image", "extract", "--case", e.c, "--json", e.ref, "id:"+id)
	var js struct {
		IncompleteIDs []string `json:"incomplete_ids"`
	}
	if err := json.Unmarshal(jsonPart(out), &js); err != nil || code != 0 {
		t.Fatalf("json %q: %v (exit %d)", out, err, code)
	}
	if len(js.IncompleteIDs) != 1 || !strings.Contains(js.IncompleteIDs[0], "a live entry with this id may exist") {
		t.Errorf("incomplete_ids = %q; want the one id's text", js.IncompleteIDs)
	}
}

// An extraction whose search read everything has an empty incomplete_ids list in its JSON.
func TestImageExtractJSONIncompleteIDsEmptyWhenComplete(t *testing.T) {
	e := newImgEnv(t)
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "extract", "--case", e.c, "--json", e.ref, "/docs/a.txt")
	var js map[string]json.RawMessage
	if err := json.Unmarshal(jsonPart(out), &js); err != nil || code != 0 {
		t.Fatalf("json %q: %v (exit %d, stderr %q)", out, err, code, errs)
	}
	if string(js["incomplete_ids"]) != "[]" {
		t.Errorf("incomplete_ids = %s, want []", js["incomplete_ids"])
	}
}

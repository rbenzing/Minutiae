package examine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// suppNote is the note a full collector holds when n occurrences were not recorded. The tests spell the
// text out so they do not depend on filesys.SuppressedNote.
func suppNote(n int) string {
	return fmt.Sprintf("further warnings suppressed: %d occurrences not recorded", n)
}

// distinctWarnings returns n distinct anomaly texts.
func distinctWarnings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("anomaly %d", i)
	}
	return out
}

// noteEntries returns the filesystem-sourced analysis.warning texts that are a suppression note.
func noteEntries(t *testing.T, c *evidence.Case) []string {
	t.Helper()
	var out []string
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if src, _ := e.Details["source"].(string); src == "" {
			continue
		}
		if w, _ := e.Details["warning"].(string); strings.HasPrefix(w, filesys.SuppressedWarning) {
			out = append(out, w)
		}
	}
	return out
}

// suppressedKey returns the fs_warnings_suppressed detail of the only entry of action, and whether it is
// present.
func suppressedKey(t *testing.T, c *evidence.Case, action string) (any, bool) {
	t.Helper()
	es := auditByAction(t, c, action)
	if len(es) != 1 {
		t.Fatalf("%d %s entries, want 1", len(es), action)
	}
	v, ok := es[0].Details["fs_warnings_suppressed"]
	return v, ok
}

// suppressedSession opens a filesystem whose warnings are open at the start and which rewrites or grows
// them as onOpen says while the files are opened.
func suppressedSession(t *testing.T, c *evidence.Case, open []string, onOpen func(w *[]string, name string)) *examine.Session {
	t.Helper()
	return warnSession(t, c, open, func(w *[]string) warnFS {
		return warnFS{warnings: w, onOpen: func(e filesys.Entry) []string {
			onOpen(w, e.Name)
			return nil
		}}
	},
		fstest.Node{Path: "/a.txt", Data: []byte("a")},
		fstest.Node{Path: "/b.txt", Data: []byte("b")},
		fstest.Node{Path: "/c.txt", Data: []byte("c")})
}

var allThree = []string{"/a.txt", "/b.txt", "/c.txt"}

func TestAnalysisAuditsSuppressionNoteOnce(t *testing.T) {
	c := newCase(t)
	// The list is full at open (1000 messages) and grows to the note, the note's count is rewritten
	// in place, and one warning follows the note (as a panic warning appended by safeFS would).
	s := suppressedSession(t, c, distinctWarnings(filesys.MaxWarnings), func(w *[]string, name string) {
		switch name {
		case "a.txt":
			*w = append(*w, suppNote(1))
		case "b.txt":
			(*w)[filesys.MaxWarnings] = suppNote(250)
		case "c.txt":
			*w = append(*w, "after the note")
		}
	})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: allThree})
	if got := noteEntries(t, c); len(got) != 1 || got[0] != suppNote(1) {
		t.Errorf("note entries = %q, want exactly the first sight %q", got, suppNote(1))
	}
	if sum.FSWarnings != 1 {
		t.Errorf("FSWarnings = %d, want 1 (the warning after the note; the note is not counted)", sum.FSWarnings)
	}
	if got := fsWarnings(t, c)["filesystem"]; len(got) != 2 || got[1] != "after the note" {
		t.Errorf("filesystem warnings = %q, want the note then the warning after it", got)
	}
}

func TestAnalysisAuditsSuppressionNoteOnceFromOpen(t *testing.T) {
	c := newCase(t)
	open := append(distinctWarnings(filesys.MaxWarnings), suppNote(1))
	s := suppressedSession(t, c, open, func(w *[]string, name string) {
		if name == "b.txt" {
			(*w)[filesys.MaxWarnings] = suppNote(250)
		}
		if name == "c.txt" {
			*w = append(*w, "after the note")
		}
	})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: allThree})
	if got := noteEntries(t, c); len(got) != 1 || got[0] != suppNote(1) {
		t.Errorf("note entries = %q, want exactly the open-time sight %q", got, suppNote(1))
	}
	if sum.FSWarnings != 1 {
		t.Errorf("FSWarnings = %d, want 1", sum.FSWarnings)
	}
}

func TestAnalysisEndCarriesSuppressedFilesystemWarnings(t *testing.T) {
	c := newCase(t)
	open := append(distinctWarnings(filesys.MaxWarnings), suppNote(1))
	s := suppressedSession(t, c, open, func(w *[]string, name string) {
		if name == "b.txt" {
			(*w)[filesys.MaxWarnings] = suppNote(250)
		}
	})
	extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: allThree})
	v, ok := suppressedKey(t, c, "analysis.end")
	if !ok || fmt.Sprint(v) != "250" {
		t.Errorf("fs_warnings_suppressed = %v (present %v), want 250", v, ok)
	}
	if _, isNum := v.(json.Number); !isNum { // the audit log decodes numbers as json.Number; a string would not be one
		t.Errorf("fs_warnings_suppressed is %T, want a JSON number", v)
	}
}

func TestAnalysisErrorCarriesSuppressedFilesystemWarnings(t *testing.T) {
	c := newCase(t)
	open := append(distinctWarnings(filesys.MaxWarnings), suppNote(1))
	s := warnSession(t, c, open, func(w *[]string) warnFS {
		return warnFS{warnings: w, onOpen: func(filesys.Entry) []string {
			(*w)[filesys.MaxWarnings] = suppNote(250)
			return nil
		}}
	}, fstest.Node{Path: "/a.txt", Data: pattern(4*blk, 3)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Extract(ctx, examine.ExtractOptions{
		Partition: -1, Paths: []string{"/a.txt"}, Progress: func(int64, int64) { cancel() },
	}); err == nil {
		t.Fatal("cancelled extract succeeded")
	}
	v, ok := suppressedKey(t, c, "analysis.error")
	if !ok || fmt.Sprint(v) != "250" {
		t.Errorf("fs_warnings_suppressed = %v (present %v), want 250", v, ok)
	}
}

func TestAnalysisEndOmitsKeyWhenNothingWasDropped(t *testing.T) {
	c := newCase(t)
	s := suppressedSession(t, c, distinctWarnings(filesys.MaxWarnings), func(w *[]string, name string) {
		if name == "b.txt" {
			*w = append(*w, "one more")
		}
	})
	extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: allThree})
	if v, ok := suppressedKey(t, c, "analysis.end"); ok {
		t.Errorf("fs_warnings_suppressed = %v present, want no key when nothing was dropped", v)
	}
}

func TestAnalysisIgnoresALookalikeNote(t *testing.T) {
	c := newCase(t)
	open := distinctWarnings(40)
	open[5] = suppNote(9)  // note-shaped, but the list never filled
	open[39] = suppNote(8) // also as the last element
	s := suppressedSession(t, c, open, func(*[]string, string) {})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: allThree})
	if got := fsWarnings(t, c)["filesystem-open"]; len(got) != 40 || got[5] != suppNote(9) || got[39] != suppNote(8) {
		t.Errorf("open warnings = %d entries, [5] = %q; want the lookalike audited as an ordinary warning", len(got), got[5])
	}
	if v, ok := suppressedKey(t, c, "analysis.end"); ok {
		t.Errorf("fs_warnings_suppressed = %v present for a lookalike note", v)
	}
	if sum.FSWarnings != 0 {
		t.Errorf("FSWarnings = %d, want 0", sum.FSWarnings)
	}
}

func TestAnalysisFinishFSNilFilesystem(t *testing.T) {
	c := newCase(t)
	s := suppressedSession(t, c, nil, func(*[]string, string) {})
	sum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Volume: true})
	if err != nil {
		t.Fatalf("volume unalloc export: %v", err)
	}
	if sum.AnalysisID == "" {
		t.Error("no analysis ran")
	}
	if v, ok := suppressedKey(t, c, "analysis.end"); ok {
		t.Errorf("fs_warnings_suppressed = %v present for an analysis without a filesystem", v)
	}
}

func TestAnalysisNoteSurvivesAPanicWarningAfterIt(t *testing.T) {
	c := newCase(t)
	open := append(distinctWarnings(filesys.MaxWarnings), suppNote(1))
	s := suppressedSession(t, c, open, func(w *[]string, name string) {
		switch name {
		case "a.txt":
			*w = append(*w, "panic in Info")
		case "b.txt":
			(*w)[filesys.MaxWarnings] = suppNote(77)
		}
	})
	extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: allThree})
	v, ok := suppressedKey(t, c, "analysis.end")
	if !ok || fmt.Sprint(v) != "77" {
		t.Errorf("fs_warnings_suppressed = %v (present %v), want 77 although an element follows the note", v, ok)
	}
}

package examine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func cutDirEnv(t *testing.T) *recEnv {
	t.Helper()
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.failReadDir = func(dir filesys.Entry) error {
			if dir.Name == "d" {
				return fmt.Errorf("directory cut: %w", filesys.ErrLimit)
			}
			return nil
		}
	}, delNode("/a.bin", pat(1, bs)), fstest.Node{Path: "/d", Dir: true})
	return e
}

// F81: an id search that met an unreadable directory and did not find the id
// is "search incomplete", never "not found".
func TestRecoverIDSearchWithCutDirectoryIsIncompleteNotNotFound(t *testing.T) {
	e := cutDirEnv(t)
	_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{"id:no-such-id"}})
	if err == nil {
		t.Fatal("Recover succeeded, want a search-incomplete error")
	}
	if errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("error %v wraps ErrNotFound while a directory could not be read", err)
	}
	var si *examine.SearchIncompleteError
	if !errors.As(err, &si) || !errors.Is(err, filesys.ErrLimit) || si.Unreadable != 1 || !strings.Contains(err.Error(), "/d") {
		t.Errorf("error %v, want *SearchIncompleteError wrapping the cut reason and naming /d", err)
	}
}

// F81: an id that was found is still found although another directory was cut.
func TestRecoverIDSearchFindsIDBesideCutDirectory(t *testing.T) {
	e := cutDirEnv(t)
	sum, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + idOf2(t, e, "/a.bin")}})
	if err != nil || sum.Recovered != 1 {
		t.Fatalf("Recover = %+v, %v; want the file recovered", sum, err)
	}
}

// F82: Session.Lookup by id never wraps ErrNotFound after an incomplete search.
func TestSessionLookupIDWithCutDirectoryIsIncompleteNotNotFound(t *testing.T) {
	e := cutDirEnv(t)
	fsys, _, err := e.s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.s.Lookup(fsys, "id:no-such-id")
	var si *examine.SearchIncompleteError
	if err == nil || errors.Is(err, filesys.ErrNotFound) || !errors.As(err, &si) || !errors.Is(err, filesys.ErrLimit) {
		t.Fatalf("Lookup error %v, want *SearchIncompleteError wrapping the cut reason and not ErrNotFound", err)
	}
	// A deleted match beside the cut directory is found, flagged incomplete (F89).
	if m, err := e.s.LookupMatch(fsys, "id:"+idOf2(t, e, "/a.bin")); err != nil || m.Incomplete == nil {
		t.Errorf("an id beside the cut directory: %+v, %v", m, err)
	}
}

func hasIncompleteWarning(t *testing.T, e *recEnv, id string) bool {
	t.Helper()
	for _, a := range auditByAction(t, e.c, "analysis.warning") {
		w, _ := a.Details["warning"].(string)
		if strings.Contains(w, "id:"+id) && strings.Contains(w, "/d") && strings.Contains(w, "live entry") && strings.Contains(w, "may exist") {
			return true
		}
	}
	return false
}

// F89: a deleted match found by an incomplete search is returned (partial work is kept) but carries the
// incompleteness, names the unread directories, is audited, and is never the only entry with that id.
func TestSessionLookupIDDeletedMatchAfterCutCarriesIncompleteness(t *testing.T) {
	e := cutDirEnv(t)
	fsys, _, err := e.s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	id := idOf2(t, e, "/a.bin")
	m, err := e.s.LookupMatch(fsys, "id:"+id)
	if err != nil || !m.Entry.Deleted || m.Path != "/a.bin" {
		t.Fatalf("LookupMatch = %+v, %v; want the deleted match", m, err)
	}
	if m.Incomplete == nil || m.Incomplete.Unreadable != 1 || m.Incomplete.Found != "/a.bin" || !slices.Contains(m.Incomplete.Dirs, "/d") || !errors.Is(m.Incomplete, filesys.ErrLimit) {
		t.Fatalf("Incomplete = %+v; want 1 unread directory /d, the match and the cut reason", m.Incomplete)
	}
	if !strings.Contains(m.Incomplete.Error(), "live entry") {
		t.Errorf("error text %q does not say a live entry may exist", m.Incomplete)
	}
	if !hasIncompleteWarning(t, e, id) {
		t.Errorf("no analysis.warning naming /d and a possible live entry for id %s", id)
	}
	// The three-value form refuses to hand the deleted match over as if it were the only entry.
	got, p, err := e.s.Lookup(fsys, "id:"+id)
	var si *examine.SearchIncompleteError
	if !errors.As(err, &si) || si.Found != "/a.bin" || !got.Deleted || p != "/a.bin" {
		t.Errorf("Lookup = %+v, %q, %v; want the match together with the *SearchIncompleteError", got, p, err)
	}
}

// A live match, or a search that read everything, carries no incompleteness.
func TestSessionLookupIDLiveMatchBesideCutCarriesNoFlag(t *testing.T) {
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.failReadDir = func(dir filesys.Entry) error {
			if dir.Name == "d" {
				return fmt.Errorf("directory cut: %w", filesys.ErrLimit)
			}
			return nil
		}
	}, fstest.Node{Path: "/live.txt", Data: pat(2, bs)}, fstest.Node{Path: "/d", Dir: true})
	fsys, _, err := e.s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	m, err := e.s.LookupMatch(fsys, "id:"+idOf2(t, e, "/live.txt"))
	if err != nil || m.Entry.Deleted || m.Incomplete != nil {
		t.Fatalf("LookupMatch = %+v, %v; want the live entry without a flag", m, err)
	}
}

// F89: the recover id search keeps the deleted match, writes it, and reports and audits the incompleteness.
func TestRecoverIDSearchDeletedMatchAfterCutIsFlaggedAndAudited(t *testing.T) {
	e := cutDirEnv(t)
	id := idOf2(t, e, "/a.bin")
	opts := examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + id}}
	plan, err := e.s.PlanRecovery(context.Background(), opts)
	if err != nil || len(plan.Items) != 1 || len(plan.IncompleteIDs) != 1 {
		t.Fatalf("PlanRecovery = %+v, %v; want one item and one incomplete id", plan, err)
	}
	sum, err := e.s.Recover(context.Background(), opts)
	if err != nil || sum.Recovered != 1 {
		t.Fatalf("Recover = %+v, %v; want the match recovered", sum, err)
	}
	if len(sum.IncompleteIDs) != 1 {
		t.Fatalf("IncompleteIDs = %+v", sum.IncompleteIDs)
	}
	si := sum.IncompleteIDs[0]
	if si.ID != id || si.Found != "/a.bin" || si.Unreadable != 1 || !slices.Contains(si.Dirs, "/d") {
		t.Errorf("IncompleteIDs[0] = %+v", si)
	}
	if !hasIncompleteWarning(t, e, id) {
		t.Errorf("no analysis.warning naming /d and a possible live entry for id %s", id)
	}
	verifyOK(t, e.c)
}

// An id search that read everything flags nothing.
func TestRecoverIDSearchCompleteHasNoIncompleteIDs(t *testing.T) {
	e, _ := newHookEnv(t, nil, delNode("/a.bin", pat(1, bs)))
	sum, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + idOf2(t, e, "/a.bin")}})
	if err != nil || sum.Recovered != 1 || len(sum.IncompleteIDs) != 0 {
		t.Fatalf("Recover = %+v, %v", sum, err)
	}
}

// incompleteWarnings returns the analysis.warning entries of the id-search-incomplete code for id.
func incompleteWarnings(t *testing.T, e *recEnv, id string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, a := range auditByAction(t, e.c, "analysis.warning") {
		if code, _ := a.Details["code"].(string); code == "id-search-incomplete" && a.Details["id"] == id {
			out = append(out, a.Details)
		}
	}
	return out
}

// F93: one incomplete id is reported once however often it is named: one IncompleteIDs item and ONE
// analysis.warning.
func TestRecoverIDSearchRepeatedRefsReportOnceAndAuditOnce(t *testing.T) {
	e := cutDirEnv(t)
	id := idOf2(t, e, "/a.bin")
	opts := examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + id, "id:" + id}}
	plan, err := e.s.PlanRecovery(context.Background(), opts)
	if err != nil || len(plan.IncompleteIDs) != 1 {
		t.Fatalf("PlanRecovery = %+v, %v; want ONE incomplete id", plan.IncompleteIDs, err)
	}
	sum, err := e.s.Recover(context.Background(), opts)
	if err != nil || len(sum.IncompleteIDs) != 1 {
		t.Fatalf("Recover = %+v, %v; want ONE incomplete id", sum.IncompleteIDs, err)
	}
	ws := incompleteWarnings(t, e, id)
	if len(ws) != 1 {
		t.Fatalf("%d id-search-incomplete warnings for %s, want exactly 1", len(ws), id)
	}
	if ws[0]["analysis_id"] != sum.AnalysisID {
		t.Errorf("warning analysis_id %v, want %s", ws[0]["analysis_id"], sum.AnalysisID)
	}
}

// F93: a single ref gives exactly one warning too (the count, not just the presence).
func TestRecoverIDSearchSingleRefAuditsExactlyOnce(t *testing.T) {
	e := cutDirEnv(t)
	id := idOf2(t, e, "/a.bin")
	if _, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, Refs: []string{"id:" + id}}); err != nil {
		t.Fatal(err)
	}
	if n := len(incompleteWarnings(t, e, id)); n != 1 {
		t.Errorf("%d id-search-incomplete warnings, want exactly 1", n)
	}
}

// F95: Extract of a deleted match found by an incomplete search keeps going, reports the incompleteness in its
// summary (once, however often the id is named), and audits ONE warning inside the analysis, carrying its id.
func TestExtractByIDAfterCutCarriesIncompleteIDsAndAnalysisID(t *testing.T) {
	e := cutDirEnv(t)
	id := idOf2(t, e, "/a.bin")
	sum, err := e.s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"id:" + id, "id:" + id}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(sum.IncompleteIDs) != 1 || sum.IncompleteIDs[0].ID != id || sum.IncompleteIDs[0].Found != "/a.bin" || !slices.Contains(sum.IncompleteIDs[0].Dirs, "/d") {
		t.Fatalf("IncompleteIDs = %+v; want the one id, its match and /d", sum.IncompleteIDs)
	}
	ws := incompleteWarnings(t, e, id)
	if len(ws) != 1 {
		t.Fatalf("%d id-search-incomplete warnings, want exactly 1", len(ws))
	}
	if ws[0]["analysis_id"] != sum.AnalysisID {
		t.Errorf("warning analysis_id %v, want %s", ws[0]["analysis_id"], sum.AnalysisID)
	}
	startAt, warnAt := -1, -1
	for i, a := range auditEntries(t, e.c) {
		switch {
		case a.Action == "analysis.start":
			startAt = i
		case a.Action == "analysis.warning" && a.Details["code"] == "id-search-incomplete":
			warnAt = i
		}
	}
	if startAt < 0 || warnAt < startAt {
		t.Errorf("warning at %d, analysis.start at %d; the warning belongs inside the analysis", warnAt, startAt)
	}
	verifyOK(t, e.c)
}

// A live match, or an id search that read everything, leaves Extract's IncompleteIDs empty.
func TestExtractByIDCompleteSearchHasNoIncompleteIDs(t *testing.T) {
	e, _ := newHookEnv(t, nil, fstest.Node{Path: "/live.txt", Data: pat(2, bs)})
	sum, err := e.s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"id:" + idOf2(t, e, "/live.txt")}})
	if err != nil || sum.Files != 1 || len(sum.IncompleteIDs) != 0 {
		t.Fatalf("Extract = %+v, %v", sum, err)
	}
}

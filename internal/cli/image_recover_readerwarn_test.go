package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func TestListShowsReaderWarnings(t *testing.T) {
	e := newRecoverEnv(t, recNode("/w-del.bin", recPat(1, recBS), fstest.RecoverMap{
		Method: recMethod, Warnings: []string{"stale name \x1b[31m", "second"},
	}))
	nAudit := len(auditActions(t, e.c))

	code, out := e.recover(t, "--list", "--json")
	if code != 0 {
		t.Fatalf("--list --json: %d %s", code, out)
	}
	var l struct {
		ReaderWarnings int `json:"reader_warnings"`
		Items          []struct {
			Candidates []struct {
				Warnings []string `json:"warnings"`
			} `json:"candidates"`
		} `json:"items"`
	}
	if err := json.Unmarshal(jsonPart(out), &l); err != nil {
		t.Fatalf("list json %q: %v", out, err)
	}
	if l.ReaderWarnings != 2 || len(l.Items) != 1 || len(l.Items[0].Candidates) != 1 || len(l.Items[0].Candidates[0].Warnings) != 2 {
		t.Fatalf("list json %+v, want reader_warnings 2 and the two warnings on the candidate", l)
	}

	code, out = e.recover(t, "--list")
	if code != 0 {
		t.Fatalf("--list: %d %s", code, out)
	}
	if !strings.Contains(out, "reader warning:") || strings.ContainsRune(out, 0x1b) {
		t.Errorf("text list lacks a 'reader warning:' line or leaks the control byte:\n%q", out)
	}
	if !strings.Contains(out, "reader warnings: 2") {
		t.Errorf("text list lacks the count:\n%s", out)
	}
	if acts := auditActions(t, e.c)[nAudit:]; !slices.Equal(acts, []string{"case.open", "case.open"}) {
		t.Errorf("--list appended %v, want only case.open entries", acts)
	}
}

func TestRunShowsReaderWarningCount(t *testing.T) {
	e := newRecoverEnv(t, recNode("/w-del.bin", recPat(1, recBS), fstest.RecoverMap{Method: recMethod, Warnings: []string{"one"}}))
	code, out := e.recover(t, "--all")
	if code != 0 || !strings.Contains(out, "reader warnings: 1") {
		t.Fatalf("--all: %d %s", code, out)
	}
	code, out = e.recover(t, "--all", "--json")
	if code != 0 {
		t.Fatalf("--all --json: %d %s", code, out)
	}
	var s struct {
		ReaderWarnings int `json:"reader_warnings"`
	}
	if err := json.Unmarshal(jsonPart(out), &s); err != nil || s.ReaderWarnings != 1 {
		t.Errorf("run json reader_warnings = %d (%v), want 1: %s", s.ReaderWarnings, err, out)
	}
}

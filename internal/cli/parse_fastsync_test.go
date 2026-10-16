package cli

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func ingestStartSync(t *testing.T, dir string) []any {
	t.Helper()
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	for _, e := range entries {
		if e.Action == evidence.ActionIngestStart {
			out = append(out, e.Details["sync"])
		}
	}
	return out
}

// TestParseRunFastSyncIsExplicitAndAudited: --fast-sync (ruling D89/D90) is off by default; with it the
// ingest runs synchronous=NORMAL during its batches and records.ingest.start says sync = normal.
func TestParseRunFastSyncIsExplicitAndAudited(t *testing.T) {
	reg := registry(t, "", good("a"))
	p := newPfx(t, "a")
	if code, out, e := runP(t, reg, "parse", "run", "--case", p.dir); code != 0 {
		t.Fatal(code, out, e)
	}
	if got := ingestStartSync(t, p.dir); len(got) != 1 || got[0] != nil {
		t.Fatalf("a default parse run recorded sync %v, want no sync key", got)
	}
	p2 := newPfx(t, "a")
	if code, out, e := runP(t, reg, "parse", "run", "--case", p2.dir, "--fast-sync"); code != 0 {
		t.Fatal(code, out, e)
	}
	if got := ingestStartSync(t, p2.dir); len(got) != 1 || got[0] != "normal" {
		t.Fatalf("a --fast-sync parse run recorded sync %v, want [normal]", got)
	}
	c, err := evidence.Open(p2.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify after a completed --fast-sync run: %v %q", err, rep.Problems)
	}
}

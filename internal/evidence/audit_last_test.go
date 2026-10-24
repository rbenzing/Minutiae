package evidence

import (
	"path/filepath"
	"testing"
)

func TestAuditLogLastAndHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := CreateAuditLog(path, "tester", "v0")
	if err != nil {
		t.Fatal(err)
	}
	if seq, h := a.Head(); seq != 0 || h != GenesisHash {
		t.Fatalf("empty Head = %d %s", seq, h)
	}
	if _, ok := a.Last("verify.run"); ok {
		t.Fatal("Last found an entry in an empty log")
	}
	e1, err := a.Append("verify.run", "", map[string]any{"ok": true})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := a.Append("verify.run", "", map[string]any{"ok": false})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = OpenAuditLog(path, "tester", "v0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	got, ok := a.Last("verify.run")
	if !ok || got.Seq != e2.Seq || got.Hash != e2.Hash || got.Seq == e1.Seq {
		t.Fatalf("Last after open = %+v %v, want seq %d", got, ok, e2.Seq)
	}
	e3, err := a.Append("verify.run", "", map[string]any{"ok": true})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := a.Last("verify.run"); !ok || got.Seq != e3.Seq {
		t.Fatalf("Last after append = %+v %v, want seq %d", got, ok, e3.Seq)
	}
	if _, err := a.Append("case.open", "", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := a.Last("verify.run"); !ok || got.Seq != e3.Seq {
		t.Fatalf("Last skipped back wrongly: %+v", got)
	}
	if seq, h := a.Head(); seq != e3.Seq+1 || h == "" || h == e3.Hash {
		t.Fatalf("Head = %d %s", seq, h)
	}
	if _, ok := a.Last(""); ok {
		t.Fatal("empty action query returned an entry")
	}
	if _, ok := a.Last("no.such"); ok {
		t.Fatal("unknown action found")
	}
}

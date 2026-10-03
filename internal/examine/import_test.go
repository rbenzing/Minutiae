package examine_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/examine"
)

func auditText(t *testing.T, caseDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(caseDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestImportRecordsOriginalAndSegments(t *testing.T) {
	c := newCase(t)
	data := []byte(strings.Repeat("0123456789abcdef", 100))
	paths := writeSegments(t, data, 2)
	var progressed, total int64
	recs, err := examine.Import(context.Background(), c, "img", paths, func(done, tot int64) { progressed, total = done, tot })
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if progressed != int64(len(data)) || total != int64(len(data)) {
		t.Errorf("progress = %d/%d, want %d/%d", progressed, total, len(data), len(data))
	}
	dir := path.Dir(recs[0].Path)
	for i, r := range recs {
		src, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		if r.Source.Kind != "import" || r.Source.Segment != i+1 || r.Source.DeviceID != "img" {
			t.Errorf("record %d source = %+v", i, r.Source)
		}
		if r.Source.OriginalPath != paths[i] || !filepath.IsAbs(r.Source.OriginalPath) {
			t.Errorf("OriginalPath = %q, want absolute %q", r.Source.OriginalPath, paths[i])
		}
		if r.Source.RemoteSize != int64(len(src)) || r.Source.RemoteMTime != st.ModTime().UTC().Format(time.RFC3339) {
			t.Errorf("remote size/mtime = %d / %q", r.Source.RemoteSize, r.Source.RemoteMTime)
		}
		if r.SHA256 != sha256hex(src) || r.Size != int64(len(src)) || r.Incomplete {
			t.Errorf("record %d hash/size/incomplete wrong: %+v", i, r)
		}
		if path.Dir(r.Path) != dir || path.Base(r.Path) != "disk.00"+string(rune('1'+i)) {
			t.Errorf("record %d path = %q (dir %q)", i, r.Path, dir)
		}
		if path.Base(path.Dir(r.Path)) != "image" {
			t.Errorf("record %d not under image/: %q", i, r.Path)
		}
	}
	log := auditText(t, c.Dir)
	for _, want := range []string{`"action":"acquire.start"`, `"type":"import"`, `"action":"acquire.end"`} {
		if !strings.Contains(log, want) {
			t.Errorf("audit lacks %s:\n%s", want, log)
		}
	}
}

func TestImportCancelledKeepsPartial(t *testing.T) {
	c := newCase(t)
	big := make([]byte, 4<<20)
	p := writeSegments(t, big, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recs, err := examine.Import(ctx, c, "img", p, func(done, _ int64) {
		if done > 0 {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("Import returned nil after cancellation")
	}
	if len(recs) != 1 || !recs[0].Incomplete || recs[0].Size <= 0 || recs[0].Size >= int64(len(big)) {
		t.Fatalf("records = %+v, want one partial incomplete artifact", recs)
	}
	if log := auditText(t, c.Dir); !strings.Contains(log, `"action":"acquire.error"`) {
		t.Errorf("audit lacks acquire.error:\n%s", log)
	}

	// An already-cancelled context still audits and keeps an (empty) incomplete artifact.
	recs, err = examine.Import(ctx, c, "img", writeSegments(t, big, 1), nil)
	if err == nil || len(recs) != 1 || !recs[0].Incomplete {
		t.Errorf("pre-cancelled: recs=%+v err=%v", recs, err)
	}
}

func TestImportRefusesInsideCase(t *testing.T) {
	c := newCase(t)
	inside := filepath.Join(c.Dir, "notes.bin")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := examine.Import(context.Background(), c, "img", []string{inside}, nil); err == nil || !strings.Contains(err.Error(), "cannot import from inside the case") {
		t.Fatalf("Import(inside case) = %v", err)
	}
	// A relative path that resolves inside the case is also refused.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(c.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := examine.Import(context.Background(), c, "img", []string{"notes.bin"}, nil); err == nil || !strings.Contains(err.Error(), "cannot import from inside the case") {
		t.Fatalf("Import(relative inside case) = %v", err)
	}
	if recs, _ := c.Manifest(); len(recs) != 0 {
		t.Errorf("manifest has %d records after refused imports", len(recs))
	}
}

func TestImportRefusesBadInput(t *testing.T) {
	c := newCase(t)
	dir := t.TempDir()
	good := writeSegments(t, []byte("data"), 1)[0]
	cases := map[string][]string{
		"directory": {dir},
		"missing":   {filepath.Join(dir, "nope")},
		"none":      {},
		"mixed":     {good, dir},
	}
	for name, paths := range cases {
		if _, err := examine.Import(context.Background(), c, "img", paths, nil); err == nil {
			t.Errorf("%s: Import succeeded", name)
		}
	}
	if _, err := examine.Import(context.Background(), c, "", []string{good}, nil); err == nil {
		t.Error("empty device id accepted")
	}
	if recs, _ := c.Manifest(); len(recs) != 0 {
		t.Errorf("manifest has %d records after refused imports (validation must precede any write)", len(recs))
	}
}

func TestImportRefusesCollidingNames(t *testing.T) {
	c := newCase(t)
	a, b := t.TempDir(), t.TempDir()
	write := func(dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pa, pb := write(a, "disk.img"), write(b, "disk.img")
	_, err := examine.Import(context.Background(), c, "img", []string{pa, pb}, nil)
	if err == nil || !strings.Contains(err.Error(), pa) || !strings.Contains(err.Error(), pb) {
		t.Fatalf("Import(same name) = %v, want an error naming both files", err)
	}
	// Names that differ only by case collide on case-insensitive filesystems.
	upper := write(b, "DISK.IMG")
	if _, err := examine.Import(context.Background(), c, "img", []string{pa, upper}, nil); err == nil {
		t.Error("Import accepted names differing only by case")
	}
	if recs, _ := c.Manifest(); len(recs) != 0 {
		t.Errorf("manifest has %d records; collisions must be refused before anything is written", len(recs))
	}
	if strings.Contains(auditText(t, c.Dir), "acquire.start") {
		t.Error("acquire.start was audited for a refused import")
	}
}

func TestImportRefusesExtendedLengthPathInsideCase(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("extended-length paths exist only on Windows")
	}
	c := newCase(t)
	inside := filepath.Join(c.Dir, "notes.bin")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{`\\?\` + inside, strings.ToUpper(`\\?\` + inside)} {
		if _, err := examine.Import(context.Background(), c, "img", []string{p}, nil); err == nil || !strings.Contains(err.Error(), "cannot import from inside the case") {
			t.Errorf("Import(%q) = %v", p, err)
		}
	}
}

func TestImportRefusesUnicodeCaseFoldCollision(t *testing.T) {
	c := newCase(t)
	a, b := t.TempDir(), t.TempDir()
	write := func(dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// strings.ToLower leaves the long s (U+017F) alone, but it case-folds to
	// "s". The other pairs are folded by lower-casing too and guard against a
	// regression to something weaker. A lower-casing check would
	// let both through while a case-insensitive filesystem merges them.
	for _, pair := range [][2]string{{"s.img", "ſ.img"}, {"k.img", "K.img"}, {"Ǆ.img", "ǆ.img"}} {
		pa, pb := write(a, pair[0]), write(b, pair[1])
		if _, err := examine.Import(context.Background(), c, "img", []string{pa, pb}, nil); err == nil {
			t.Errorf("Import accepted %q and %q, which differ only by Unicode case", pair[0], pair[1])
		}
	}
	if recs, _ := c.Manifest(); len(recs) != 0 {
		t.Errorf("manifest has %d records; collisions must be refused before anything is written", len(recs))
	}
}

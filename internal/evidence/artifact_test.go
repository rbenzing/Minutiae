package evidence

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

var testSrc = Source{Kind: "file", DeviceID: "dev1", RemotePath: "/sdcard/a.txt"}

func TestNewArtifactHashesAndRecords(t *testing.T) {
	c := newTestCase(t)
	w, err := c.NewArtifact("dev1", "acq1", "files/sdcard/a.txt", testSrc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "abc"); err != nil {
		t.Fatal(err)
	}
	rec, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Path != "artifacts/dev1/acq1/files/sdcard/a.txt" || rec.Size != 3 ||
		rec.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || rec.Incomplete {
		t.Fatalf("record = %+v", rec)
	}
	b, _ := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(rec.Path)))
	if string(b) != "abc" {
		t.Fatalf("file = %q", b)
	}
	m, _ := c.Manifest()
	if len(m) != 1 || m[0].ID != rec.ID {
		t.Fatalf("manifest = %+v", m)
	}
	h, _ := c.store.ArtifactHashes()
	if h[rec.ID] != rec.SHA256 {
		t.Fatalf("db hashes = %v", h)
	}
	es, _ := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if last := es[len(es)-1]; last.Action != "artifact.create" || last.DeviceID != "dev1" {
		t.Fatalf("last audit = %+v", last)
	}
}

func TestNewArtifactRefusesOverwrite(t *testing.T) {
	c := newTestCase(t)
	w, err := c.NewArtifact("dev1", "acq1", "x", testSrc)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Close()
	if _, err := c.NewArtifact("dev1", "acq1", "x", testSrc); !errors.Is(err, ErrArtifactExists) {
		t.Fatalf("err = %v, want ErrArtifactExists", err)
	}
}

func TestNewArtifactRejectsEscape(t *testing.T) {
	c := newTestCase(t)
	for _, p := range []string{"../x", "a/../../x", "/abs", "C:/x", "", ".", ".."} {
		if _, err := c.NewArtifact("dev1", "acq1", p, testSrc); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
}

func TestSanitizeRelPath(t *testing.T) {
	cases := map[string]string{
		"a/./b":           "a/b",
		"a:b/c?d":         "a_b/c_d",
		"dir/con.txt":     "dir/_con.txt",
		"dir./x ":         "dir_/x_",
		`back\slash`:      "back_slash",
		"tab\there":       "tab_here",
		"files/WhatsApp/": "files/WhatsApp",
	}
	for in, want := range cases {
		got, err := SanitizeRelPath(in)
		if err != nil || got != want {
			t.Errorf("SanitizeRelPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestAbortKeepsPartialFlagged(t *testing.T) {
	c := newTestCase(t)
	w, _ := c.NewArtifact("dev1", "acq1", "p.img", testSrc)
	_, _ = io.WriteString(w, "part")
	rec, err := w.Abort(errors.New("cable pulled"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Incomplete || rec.Error == "" || rec.Size != 4 {
		t.Fatalf("record = %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, filepath.FromSlash(rec.Path))); err != nil {
		t.Fatalf("partial file removed: %v", err)
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	c := newTestCase(t)
	w, _ := c.NewArtifact("dev1", "acq1", "x", testSrc)
	_, _ = w.Close()
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after close succeeded")
	}
	if _, err := w.Close(); err == nil {
		t.Fatal("double close succeeded")
	}
}

func TestCaptureAbortsOnFillError(t *testing.T) {
	c := newTestCase(t)
	boom := errors.New("boom")
	rec, err := c.Capture("dev1", "acq1", "x", testSrc, func(w io.Writer) error {
		_, _ = io.WriteString(w, "half")
		return boom
	})
	if !errors.Is(err, boom) || !rec.Incomplete {
		t.Fatalf("rec=%+v err=%v", rec, err)
	}
}

func TestConcurrentArtifacts(t *testing.T) {
	c := newTestCase(t)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Capture("dev1", "acq1", fmt.Sprintf("f%d", i), testSrc, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "data %d", i)
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m, _ := c.Manifest()
	if len(m) != 10 {
		t.Fatalf("manifest has %d records", len(m))
	}
	if _, problems, _ := VerifyAuditLog(filepath.Join(c.Dir, auditFile)); len(problems) != 0 {
		t.Fatalf("audit problems: %v", problems)
	}
}

func TestNewAcquisitionIDUnique(t *testing.T) {
	format := regexp.MustCompile(`^\d{8}T\d{6}\.\d{9}Z-[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for range 1000 {
		id := NewAcquisitionID(time.Now())
		if seen[id] {
			t.Fatalf("duplicate acquisition id %q", id)
		}
		seen[id] = true
	}
	for id := range seen {
		if !format.MatchString(id) || sanitizeComponent(id) != id {
			t.Fatalf("id %q is not of the form <timestamp>-<8 hex> or not filesystem-safe", id)
		}
	}
}

package evidence_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func openAndClose(c *evidence.Case, id string) error {
	f, _, err := c.OpenArtifact(id)
	if err == nil {
		_ = f.Close()
	}
	return err
}

func TestOpenArtifactReadsTheManifestOnceWhileItIsUnchanged(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
	b := recordstest.AddArtifact(t, c, "b.db", []byte("bbbb"))
	before := c.ManifestReads()
	for i := 0; i < 20; i++ {
		for _, id := range []string{a.ID, b.ID} {
			if err := openAndClose(c, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := c.ManifestReads() - before; got > 1 {
		t.Fatalf("40 opens read the manifest %d times, want at most 1", got)
	}
}

func TestOpenArtifactSeesWhatChangedInTheManifest(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
	if err := openAndClose(c, a.ID); err != nil { // fills the cache
		t.Fatal(err)
	}
	b := recordstest.AddArtifact(t, c, "b.db", []byte("bbbb")) // appended by this process
	if err := openAndClose(c, b.ID); err != nil {
		t.Fatalf("an artifact added after the cache was filled: %v", err)
	}
	recordstest.AppendManifestLine(t, c.Dir, a) // a duplicate id planted by someone else
	if err := openAndClose(c, a.ID); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("a duplicated id must be an integrity error, got %v", err)
	}
	recordstest.RemoveArtifactEverywhere(t, c.Dir, b.ID)
	if err := openAndClose(c, b.ID); !errors.Is(err, evidence.ErrUnknownArtifact) {
		t.Fatalf("a removed artifact must be unknown, got %v", err)
	}
}

func TestOpenArtifactSeesRenameOverAndInPlaceEdits(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
	if err := openAndClose(c, a.ID); err != nil { // fills the cache
		t.Fatal(err)
	}
	path := filepath.Join(c.Dir, "manifest.jsonl")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// a different file renamed over the manifest: the id appears twice
	dup := append(append([]byte{}, orig...), orig...)
	tmp := path + ".new"
	if err := os.WriteFile(tmp, dup, 0o600); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	if err := openAndClose(c, a.ID); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("after a rename-over: %v, want an integrity error", err)
	}

	// back to the original, then an in-place edit of the same size: the id changes by one character
	if err := os.WriteFile(path, orig, 0o600); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	if err := openAndClose(c, a.ID); err != nil {
		t.Fatalf("after restoring the manifest: %v", err)
	}
	old := []byte(`"id":"` + a.ID + `"`)
	edited := bytes.Replace(orig, old, []byte(`"id":"`+a.ID[:len(a.ID)-1]+`x"`), 1)
	if len(edited) != len(orig) || bytes.Equal(edited, orig) {
		t.Fatalf("the test edit must keep the size and change the bytes")
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	if err := openAndClose(c, a.ID); !errors.Is(err, evidence.ErrUnknownArtifact) {
		t.Fatalf("after an in-place edit: %v, want the artifact to be unknown", err)
	}
}

// The cache serves OpenArtifact only: Manifest (which case verify uses) reads the file every time, so
// a change the cache could not see is still seen by it.
func TestManifestIsNeverServedFromTheOpenCache(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
	if err := openAndClose(c, a.ID); err != nil {
		t.Fatal(err)
	}
	before := c.ManifestReads()
	for i := 0; i < 3; i++ {
		if _, err := c.Manifest(); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.ManifestReads() - before; got != 3 {
		t.Fatalf("3 Manifest calls read the file %d times, want 3", got)
	}
}

// Openers racing with a writer that appends: no race, and every artifact the writer finished is
// openable by the readers at once, while they also keep opening the first one.
func TestOpenArtifactCacheIsSafeUnderConcurrentAppends(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
	var (
		mu    sync.Mutex
		added []string // ids the writer has finished
		wg    sync.WaitGroup
		done  = make(chan struct{})
		errs  = make(chan error, 16)
	)
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{a.ID}, added...)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				for _, id := range snapshot() {
					if err := openAndClose(c, id); err != nil {
						errs <- err
						return
					}
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
	for i := range 8 {
		r := recordstest.AddArtifact(t, c, "n"+string(rune('a'+i))+".db", []byte("data"))
		mu.Lock()
		added = append(added, r.ID)
		mu.Unlock()
	}
	close(done)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, id := range snapshot() {
		if err := openAndClose(c, id); err != nil {
			t.Error(err)
		}
	}
}

// Each of the three stat tests stands alone: a replacement that matches the other two is still seen.
func TestOpenArtifactCacheChecksFileIdentitySizeAndTimeEach(t *testing.T) {
	setup := func(t *testing.T) (*evidence.Case, evidence.ManifestRecord, string, []byte, os.FileInfo) {
		c := recordstest.NewCase(t)
		a := recordstest.AddArtifact(t, c, "a.db", []byte("aaaa"))
		if err := openAndClose(c, a.ID); err != nil { // fills the cache
			t.Fatal(err)
		}
		path := filepath.Join(c.Dir, "manifest.jsonl")
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return c, a, path, orig, st
	}
	t.Run("a different file of the same size and time renamed over", func(t *testing.T) {
		c, a, path, orig, st := setup(t)
		edited := bytes.Replace(orig, []byte(`"id":"`+a.ID+`"`), []byte(`"id":"`+a.ID[:len(a.ID)-1]+`x"`), 1)
		tmp := path + ".new"
		if err := os.WriteFile(tmp, edited, 0o600); err != nil { //nolint:gosec // a test edits its own temp case
			t.Fatal(err)
		}
		if err := os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil { //nolint:gosec // a test edits its own temp case
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil { //nolint:gosec // a test edits its own temp case
			t.Fatal(err)
		}
		if err := openAndClose(c, a.ID); !errors.Is(err, evidence.ErrUnknownArtifact) {
			t.Fatalf("got %v, want the artifact to be unknown", err)
		}
	})
	t.Run("the same file and time but another size", func(t *testing.T) {
		c, a, path, orig, st := setup(t)
		grown := append(append([]byte{}, orig...), orig...)
		if err := os.WriteFile(path, grown, 0o600); err != nil { //nolint:gosec // a test edits its own temp case
			t.Fatal(err)
		}
		if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil { //nolint:gosec // a test edits its own temp case
			t.Fatal(err)
		}
		if err := openAndClose(c, a.ID); !errors.Is(err, evidence.ErrIntegrity) {
			t.Fatalf("got %v, want an integrity error", err)
		}
	})
}

// A caller that changes the record OpenArtifact returned cannot change what the next open returns.
func TestOpenArtifactResultCannotCorruptTheCache(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<12))
	snap := &evidence.SnapshotRef{Name: "S", Xid: 5}
	d := evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 4}, {Offset: 100, Length: 4}}, Times: map[string]string{"m": "t"}, Snapshot: snap, FSPath: "/f"}
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, d), patterned(8))
	open := func() evidence.ManifestRecord {
		fh, rec, err := c.OpenArtifact(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
		return rec
	}
	first := open()
	want := open()
	first.Source.Derived.Runs[0].Length = 999
	first.Source.Derived.Times["m"] = "changed"
	first.Source.Derived.Snapshot.Xid = 77
	first.Source.Derived.ParentSegments = append(first.Source.Derived.ParentSegments, evidence.SegmentRef{ID: "x"})
	got := open()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the cache was changed through a returned record:\n got %+v\nwant %+v", got.Source.Derived, want.Source.Derived)
	}
	if got.Source.Derived.Runs[0].Length != 4 || got.Source.Derived.Snapshot.Xid != 5 {
		t.Fatalf("runs %+v snapshot %+v", got.Source.Derived.Runs, got.Source.Derived.Snapshot)
	}
}

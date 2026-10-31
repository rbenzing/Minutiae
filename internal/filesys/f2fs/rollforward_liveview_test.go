package f2fs_test

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

// The live view of a volume with a roll-forward chain (3H Task 7, spec 16).

// liveSnap is everything the live view shows of one FS.
type liveSnap struct {
	Info        filesys.Info
	Walk        []filesys.Entry
	Paths       []string
	Content     map[string][]byte
	Runs        map[string][]filesys.Run
	Sizes       map[string]int64
	Lookups     map[string]string // path -> entry ID, or the error
	Unallocated []filesys.Run
}

// liveSnapshot reads the whole live view through the public interface; extra are paths that must
// resolve to ErrNotFound (the post-checkpoint names).
func liveSnapshot(t *testing.T, f *f2fs.FS, extra ...string) liveSnap {
	t.Helper()
	s := liveSnap{Info: f.Info(), Content: map[string][]byte{}, Runs: map[string][]filesys.Run{}, Sizes: map[string]int64{}, Lookups: map[string]string{}}
	type pe struct {
		p string
		e filesys.Entry
	}
	var files []pe
	err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("walk %s: %v", p, err)
			return nil
		}
		s.Walk = append(s.Walk, e)
		s.Paths = append(s.Paths, p)
		if e.Type == filesys.TypeFile && !e.Deleted {
			files = append(files, pe{p, e})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, x := range files {
		p := x.p
		fl, err := f.Open(x.e)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		b, err := io.ReadAll(io.NewSectionReader(fl, 0, fl.Size()))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		s.Content[p], s.Runs[p], s.Sizes[p] = b, fl.Runs(), fl.Size()
	}
	for _, p := range append(append([]string(nil), s.Paths...), extra...) {
		e, err := f.Lookup(p)
		if err != nil {
			s.Lookups[p] = err.Error()
			if !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("Lookup(%s): %v", p, err)
			}
			continue
		}
		s.Lookups[p] = e.ID
	}
	if s.Unallocated, err = f.Unallocated(); err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	return s
}

// useEverything calls every roll-forward and journal entry point of f.
func useEverything(t *testing.T, f *f2fs.FS) {
	t.Helper()
	if _, err := f.RollForward(func(filesys.RollForwardNode) bool { return true }); err != nil {
		t.Fatalf("RollForward: %v", err)
	}
	if _, err := f.PostCheckpointFiles(); err != nil {
		t.Fatalf("PostCheckpointFiles: %v", err)
	}
	if _, err := f.Journal(); err != nil {
		t.Fatalf("Journal: %v", err)
	}
	blocks := 0
	err := f.JournalTransactions(func(tx filesys.JournalTxn) bool {
		for i := range tx.Blocks {
			if _, _, err := f.JournalBlock(tx, i); err != nil {
				t.Errorf("JournalBlock %d: %v", i, err)
			}
			blocks++
		}
		return true
	})
	if err != nil || blocks == 0 {
		t.Fatalf("JournalTransactions: %d blocks, %v", blocks, err)
	}
}

func TestRollForwardNeverAppliedToLiveView(t *testing.T) {
	fx := newRFFix(t)
	extra := []string{"/rf/a.bin", "/rf/b.bin", "/ghost"}
	a, b := fx.open(t), fx.open(t)
	before := liveSnapshot(t, a, extra...)
	useEverything(t, a)
	after := liveSnapshot(t, a, extra...)
	fresh := liveSnapshot(t, b, extra...)
	if d := liveDiff(before, after); len(d) > 0 {
		t.Errorf("the live view changed after the roll-forward calls: %v differ", d)
	}
	if d := liveDiff(after, fresh); len(d) > 0 {
		t.Errorf("the used FS differs from a fresh one: %v differ", d)
	}
	// Entry by entry: names, IDs, sizes and attributes of the walk, and every lookup.
	if len(before.Walk) != len(after.Walk) {
		t.Fatalf("the walk lists %d entries after the calls, %d before", len(after.Walk), len(before.Walk))
	}
	for i, e := range before.Walk {
		g := after.Walk[i]
		if e.Name != g.Name || e.ID != g.ID || e.Size != g.Size || !reflect.DeepEqual(e.Attrs, g.Attrs) {
			t.Errorf("walk entry %d changed: %+v -> %+v", i, e, g)
		}
	}
	for p, id := range before.Lookups {
		if after.Lookups[p] != id {
			t.Errorf("Lookup(%s) changed: %q -> %q", p, id, after.Lookups[p])
		}
	}
	// The live-updated file reads as its checkpoint version, not the chain's 9000-byte inode.
	if !bytes.Equal(after.Content["/live.bin"], rfStamp(2)) || after.Sizes["/live.bin"] != 2*bs {
		t.Errorf("/live.bin: size %d, %d bytes read; want the checkpoint version of 2 blocks", after.Sizes["/live.bin"], len(after.Content["/live.bin"]))
	}
	for _, p := range extra {
		if _, err := a.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%s) = %v, want ErrNotFound", p, err)
		}
	}
	names := map[string]bool{}
	for _, p := range after.Paths {
		names[p] = true
	}
	for _, p := range []string{"/live.bin", "/rf"} {
		if !names[p] {
			t.Errorf("the walk lacks %s: %v", p, after.Paths)
		}
	}
	for _, p := range extra {
		if names[p] {
			t.Errorf("the walk lists %s, a post-checkpoint or unreplayed name", p)
		}
	}
}

// liveDiff names the fields of two snapshots that differ.
func liveDiff(a, b liveSnap) []string {
	var out []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, va.Type().Field(i).Name)
		}
	}
	return out
}

// liveDiff must see a change to the walk or to a lookup, or the live-view test above proves nothing.
func TestLiveDiffSeesWalkAndLookupChanges(t *testing.T) {
	base := liveSnapshot(t, newRFFix(t).open(t), "/rf/a.bin")
	mut := base
	mut.Walk = append([]filesys.Entry(nil), base.Walk...)
	mut.Walk[0].Size++
	if d := liveDiff(base, mut); !reflect.DeepEqual(d, []string{"Walk"}) {
		t.Errorf("a changed walk entry: diff %v, want [Walk]", d)
	}
	mut = base
	mut.Lookups = map[string]string{}
	for p, id := range base.Lookups {
		mut.Lookups[p] = id
	}
	mut.Lookups["/rf/a.bin"] = "nid:99"
	if d := liveDiff(base, mut); !reflect.DeepEqual(d, []string{"Lookups"}) {
		t.Errorf("a changed lookup: diff %v, want [Lookups]", d)
	}
}

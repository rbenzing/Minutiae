package artparse_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

func TestSealedReaderRefuses(t *testing.T) {
	f := newFixture(t, true, artparse.Options{})
	putFile(t, f.c, "D1", "A1", "/data/data/com.a/databases/other.db", "other")
	f.snap = snapshotOf(t, f.h)
	b := f.open(t)
	in := b.Input(false)
	others := in.Lookup.Find("android:/data/data/com.a/databases/other.db")
	if len(others) != 1 {
		t.Fatalf("found %v", ids(others))
	}
	opened, err := in.Lookup.Open(others[0])
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := in.Primary.R.ReadAt(buf, 0); err != nil {
		t.Fatalf("read before the seal: %v", err)
	}

	// a reader hammering from another goroutine when the seal comes
	started, stop := make(chan struct{}), make(chan error, 1)
	go func() {
		var once sync.Once
		for {
			_, err := in.Artifacts["wal"].R.ReadAt(make([]byte, 4), 0)
			once.Do(func() { close(started) })
			if err != nil {
				stop <- err
				return
			}
		}
	}()
	<-started
	b.Seal()
	if err := <-stop; !errors.Is(err, parse.ErrSealed) {
		t.Errorf("a read in flight at the seal ended with %v, want ErrSealed", err)
	}
	b.Seal() // idempotent

	for name, r := range map[string]io.ReaderAt{
		"primary": in.Primary.R, "wal": in.Artifacts["wal"].R, "lookup open": opened, "a later input": b.Input(true).Primary.R,
	} {
		if n, err := r.ReadAt(buf, 0); n != 0 || !errors.Is(err, parse.ErrSealed) {
			t.Errorf("%s after the seal: n=%d err=%v", name, n, err)
		}
	}
	if _, err := in.Lookup.Open(others[0]); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("Lookup.Open after the seal: %v", err)
	}
}

func TestReaderAtIsReadOnlyAndHoldsNoFile(t *testing.T) {
	for name, lim := range map[string]parse.Limits{
		"in memory": parse.DefaultLimits(),
		"streamed":  limitsWith(func(l *parse.Limits) { l.MemInputMax = 0 }),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, false, artparse.Options{Limits: lim})
			in := f.open(t).Input(false)
			for role, a := range map[string]parse.Artifact{"primary": in.Primary, "role": in.Artifacts["db"]} {
				if _, ok := a.R.(interface{ Seal() }); ok {
					t.Errorf("%s: dynamic type %T exposes Seal to the parser", role, a.R)
				}
				var v any = a.R
				if _, ok := v.(interface{ Fd() uintptr }); ok {
					t.Errorf("%s: has Fd", role)
				}
				if _, ok := v.(io.Writer); ok {
					t.Errorf("%s: is an io.Writer", role)
				}
				if _, ok := v.(io.WriterAt); ok {
					t.Errorf("%s: is an io.WriterAt", role)
				}
				if _, ok := v.(io.Seeker); ok {
					t.Errorf("%s: is an io.Seeker", role)
				}
				if _, ok := v.(io.Closer); ok {
					t.Errorf("%s: is an io.Closer", role)
				}
			}
		})
	}
}

func TestProbeLimitReader(t *testing.T) {
	big := strings.Repeat("x", 10000)
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", dbPath, big)
	opt := artparse.Options{Limits: limitsWith(func(l *parse.Limits) { l.ProbeBytes = 4096 })}
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	b, err := artparse.OpenBundle(context.Background(), h, snap, jobs[0], "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	r := b.Input(true).Primary.R
	buf := make([]byte, 3000)
	if n, err := r.ReadAt(buf, 0); n != 3000 || err != nil {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	if n, err := r.ReadAt(buf[:2000], 3000); n != 1096 || !errors.Is(err, parse.ErrProbeLimit) {
		t.Errorf("the read crossing the limit: n=%d err=%v, want 1096 and ErrProbeLimit", n, err)
	}
	if n, err := r.ReadAt(buf[:10], 0); n != 0 || !errors.Is(err, parse.ErrProbeLimit) {
		t.Errorf("a read past the limit: n=%d err=%v", n, err)
	}
	// the parse path is not limited, and every Probe input has its own count
	if n, err := b.Input(false).Primary.R.ReadAt(make([]byte, 10000), 0); n != 10000 || (err != nil && !errors.Is(err, io.EOF)) {
		t.Errorf("an unlimited read: n=%d err=%v", n, err)
	}
	if n, err := b.Input(true).Primary.R.ReadAt(buf, 0); n != 3000 || err != nil {
		t.Errorf("a new probe input must start its own count: n=%d err=%v", n, err)
	}
}

// pointers collects the addresses of every pointer, map and slice reachable
// from v. It stops at the invocation's own handles (readers, BudgetView), which
// are compared by address by the caller.
func pointers(v reflect.Value, into map[uintptr]string, path string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if v.IsNil() {
			return
		}
		if v.Kind() != reflect.Slice || v.Len() > 0 {
			into[v.Pointer()] = path
		}
		switch v.Kind() {
		case reflect.Pointer:
			switch v.Type() {
			case reflect.TypeFor[*parse.SealedReaderAt](), reflect.TypeFor[*parse.BudgetView]():
				return
			}
			pointers(v.Elem(), into, path+"*")
		case reflect.Map:
			for _, k := range v.MapKeys() {
				pointers(v.MapIndex(k), into, path+"["+k.String()+"]")
			}
		case reflect.Slice:
			for i := range v.Len() {
				pointers(v.Index(i), into, path+"[]")
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			pointers(v.Field(i), into, path+"."+v.Type().Field(i).Name)
		}
	case reflect.Interface:
		if strings.HasSuffix(path, ".Lookup") {
			return // the Lookuper is the invocation's own handle, compared by address by the caller
		}
		if !v.IsNil() {
			pointers(v.Elem(), into, path)
		}
	}
}

func TestInputIsADeepCopyPerInvocation(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putExtract(t, c, "D1", "A1", "x/db", "ext4", "/data/data/com.a/databases/app.db", &evidence.SnapshotRef{Name: "snap", Xid: 7})
	h := newHost(t, c, register(t, "p", "1.0.0"))
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{IncludeSnapshots: true})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	b, err := artparse.OpenBundle(context.Background(), h, snap, jobs[0], "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	a, a2 := b.Input(false), b.Input(false)
	if a.Primary.Source.Snapshot == nil || a.Primary.ID != rec.ID {
		t.Fatalf("the fixture needs a snapshot-derived primary: %+v", a.Primary)
	}
	pa, pb := map[uintptr]string{}, map[uintptr]string{}
	pointers(reflect.ValueOf(a), pa, "in")
	pointers(reflect.ValueOf(a2), pb, "in")
	for p, path := range pa {
		if other, shared := pb[p]; shared {
			t.Errorf("two inputs share %s / %s", path, other)
		}
	}
	if a.Primary.R == a2.Primary.R || a.Lookup == a2.Lookup || a.Budget == a2.Budget {
		t.Error("the readers, the Lookuper and the BudgetView must be fresh per invocation")
	}

	// mutate one copy: the other and a later one keep the bundle's facts
	a.Primary.SHA256, a.Primary.Source.Snapshot.Xid, a.Primary.Source.RemotePath = "x", 99, "x"
	pa2 := a.Artifacts["db"]
	pa2.Source.Snapshot.Name = "mutated"
	a.Artifacts["db"] = pa2
	delete(a.Artifacts, "db")
	for i, in := range []*parse.Input{a2, b.Input(false)} {
		if in.Primary.SHA256 != rec.SHA256 || in.Primary.Source.Snapshot.Xid != 7 || in.Primary.Source.Snapshot.Name != "snap" ||
			in.Primary.Source.RemotePath != "/data/data/com.a/databases/app.db" || in.Artifacts["db"].Source.Snapshot.Name != "snap" {
			t.Errorf("input %d saw the mutation: %+v", i, in.Primary)
		}
	}
}

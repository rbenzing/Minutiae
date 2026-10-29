package parse

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

type fakeLookup struct{}

func (fakeLookup) Find(string) []Artifact             { return nil }
func (fakeLookup) Open(Artifact) (io.ReaderAt, error) { return bytes.NewReader(nil), nil }

func intPtr(v int) *int { return &v }

func fullArtifact(id string) Artifact {
	return Artifact{
		ID: id, Acquisition: "acq-" + id, SHA256: "aa" + id, Platform: "android", Logical: "android:/data/" + id, Size: 123, Incomplete: true,
		Recovery: &RecoveryInfo{Class: "carved", Method: "sig", Confidence: intPtr(70), CapUnknown: true},
		Source: SourceInfo{
			Kind: "extract", DeviceID: "dev", RemotePath: "/r/" + id, OriginalPath: "/o", Partition: "p1", FSType: "ext4", FSPath: "/f",
			Encrypted: true, ParentIncomplete: true,
			Snapshot: &SnapshotInfo{Name: "snap-" + id, Xid: 77},
		},
		R: bytes.NewReader([]byte("content-" + id)),
	}
}

func fullInput() *Input {
	lim := DefaultLimits()
	return &Input{
		Job:     JobInfo{ParseID: "p-1", Job: 3, Parser: Identity{Name: "n", Version: "1.0.0", Hash: "h"}},
		Primary: fullArtifact("a1"),
		Artifacts: map[string]Artifact{
			"db":  fullArtifact("a1"),
			"wal": fullArtifact("a2"),
		},
		Lookup: fakeLookup{},
		Limits: lim,
		Budget: NewBudget(1 << 20).View(),
	}
}

func fullMeta() Meta {
	return Meta{
		Name: "n", Version: "1.0.0", Title: "T",
		Platforms: []string{"android", "ios"},
		Emits:     []Emit{{Type: "message", PayloadVersion: 1}, {Type: "contact", PayloadVersion: 2}},
		Inputs: []InputSpec{
			{Role: "db", Globs: []string{"android:/a", "ios:b/c"}, Companions: []string{"-wal"}, Required: true},
			{Role: "wal", Globs: []string{"./x"}, Companions: []string{"-shm", "-journal"}},
		},
		Claims: []TableClaim{{Role: "db", Table: "sms"}, {Role: "db", Table: "mms"}},
	}
}

// assertDisjoint fails for every pointer, slice or map that a and b share. Paths
// in shared are the intentional exceptions (the reader, the lookuper, the view).
func assertDisjoint(t *testing.T, a, b reflect.Value, path string, shared map[string]bool) {
	t.Helper()
	if shared[path] {
		return
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares a pointer with the original", path)
			return
		}
		assertDisjoint(t, a.Elem(), b.Elem(), path, shared)
	case reflect.Slice:
		if a.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares a slice with the original", path)
			return
		}
		for i := range a.Len() {
			assertDisjoint(t, a.Index(i), b.Index(i), path+"[]", shared)
		}
	case reflect.Map:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: the clone shares a map with the original", path)
			return
		}
		for _, k := range a.MapKeys() {
			assertDisjoint(t, a.MapIndex(k), b.MapIndex(k), path+"[]", shared)
		}
	case reflect.Struct:
		for i := range a.NumField() {
			assertDisjoint(t, a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name, shared)
		}
	case reflect.Interface:
		if a.IsNil() {
			return
		}
		assertDisjoint(t, a.Elem(), b.Elem(), path, shared)
	}
}

func TestInputAndMetaCloneAreDeep(t *testing.T) {
	t.Run("input equals its clone and shares no mutable memory", func(t *testing.T) {
		in := fullInput()
		c := in.Clone()
		if !reflect.DeepEqual(in, c) {
			t.Fatal("the clone differs from the original")
		}
		shared := map[string]bool{
			".Lookup": true, ".Budget.s": true, // one lookuper and one accounting state per invocation; the two BudgetView handles are distinct
			".Primary.R": true, ".Artifacts[].R": true, // the one sealed reader of the invocation
		}
		assertDisjoint(t, reflect.ValueOf(in).Elem(), reflect.ValueOf(c).Elem(), "", shared)
		if c == in {
			t.Fatal("Clone returned the receiver")
		}
	})

	t.Run("mutating the clone leaves the original unchanged", func(t *testing.T) {
		in := fullInput()
		c := in.Clone()
		mutateInput(c)
		if !reflect.DeepEqual(in, fullInput()) {
			t.Fatal("mutating the clone changed the original")
		}
	})

	t.Run("mutating the original leaves the clone unchanged", func(t *testing.T) {
		in := fullInput()
		c := in.Clone()
		mutateInput(in)
		if !reflect.DeepEqual(c, fullInput()) {
			t.Fatal("mutating the original changed the clone")
		}
	})

	t.Run("primary and the role entry are independent copies", func(t *testing.T) {
		in := fullInput()
		c := in.Clone()
		c.Primary.Recovery.Class = "x"
		c.Primary.Source.Snapshot.Name = "x"
		if c.Artifacts["db"].Recovery.Class == "x" || c.Artifacts["db"].Source.Snapshot.Name == "x" {
			t.Fatal("Primary and Artifacts[db] share Recovery or Snapshot")
		}
	})

	t.Run("the reader and lookuper are shared, the budget view is the parser's own handle", func(t *testing.T) {
		in := fullInput()
		c := in.Clone()
		if c.Primary.R != in.Primary.R || c.Artifacts["wal"].R != in.Artifacts["wal"].R {
			t.Fatal("Clone replaced the invocation's reader")
		}
		if c.Budget == in.Budget || c.Budget.s != in.Budget.s {
			t.Fatal("Clone must give the parser its own BudgetView handle onto the invocation's single accounting state")
		}
		if c.Lookup != in.Lookup {
			t.Fatal("Clone replaced the Lookuper")
		}
	})

	t.Run("nil and empty values", func(t *testing.T) {
		var nilIn *Input
		if nilIn.Clone() != nil {
			t.Fatal("Clone of a nil Input is not nil")
		}
		in := &Input{Primary: Artifact{ID: "x"}}
		c := in.Clone()
		if c.Artifacts != nil || c.Primary.Recovery != nil || c.Primary.Source.Snapshot != nil || c.Budget != nil {
			t.Fatalf("a nil became non-nil: %+v", c)
		}
		in2 := &Input{Artifacts: map[string]Artifact{}}
		if c2 := in2.Clone(); c2.Artifacts == nil {
			t.Fatal("an empty map became nil")
		}
		in3 := &Input{Primary: Artifact{Recovery: &RecoveryInfo{Class: "c"}}}
		if c3 := in3.Clone(); c3.Primary.Recovery.Confidence != nil {
			t.Fatal("a nil Confidence became non-nil")
		}
	})

	t.Run("meta equals its clone and shares nothing", func(t *testing.T) {
		m := fullMeta()
		c := m.Clone()
		if !reflect.DeepEqual(m, c) {
			t.Fatal("the clone differs from the original")
		}
		assertDisjoint(t, reflect.ValueOf(m), reflect.ValueOf(c), "", nil)
	})

	t.Run("mutating a Meta clone leaves the original unchanged and vice versa", func(t *testing.T) {
		for _, name := range []string{"clone", "original"} {
			m := fullMeta()
			c := m.Clone()
			target, other := &c, &m
			if name == "original" {
				target, other = &m, &c
			}
			target.Name = "x"
			target.Platforms[0] = "x"
			target.Platforms = append(target.Platforms, "y")
			target.Emits[0].Type = "x"
			target.Emits[1].PayloadVersion = 99
			target.Inputs[0].Role = "x"
			target.Inputs[0].Globs[0] = "x"
			target.Inputs[0].Companions[0] = "x"
			target.Inputs[1].Globs[0] = "x"
			target.Inputs[1].Companions[1] = "x"
			target.Inputs[0].Required = false
			target.Claims[0].Table = "x"
			target.Claims[1].Role = "x"
			if !reflect.DeepEqual(*other, fullMeta()) {
				t.Fatalf("mutating the %s changed the other copy", name)
			}
		}
	})
}

func mutateInput(in *Input) {
	in.Job.ParseID, in.Job.Job, in.Job.Parser.Hash = "x", 99, "x"
	mutateArtifact(&in.Primary)
	for k, a := range in.Artifacts {
		mutateArtifact(&a)
		in.Artifacts[k] = a
	}
	in.Artifacts["new"] = Artifact{ID: "new"}
	delete(in.Artifacts, "wal")
	in.Limits.Timeout = 1
	in.Limits.MaxRecords = 1
}

func mutateArtifact(a *Artifact) {
	a.ID, a.SHA256, a.Platform, a.Logical, a.Size, a.Incomplete = "x", "x", "x", "x", -1, false
	a.Recovery.Class, a.Recovery.Method, a.Recovery.CapUnknown = "x", "x", false
	*a.Recovery.Confidence = 1
	a.Recovery.Confidence = intPtr(2)
	a.Source.Kind, a.Source.DeviceID, a.Source.RemotePath, a.Source.OriginalPath = "x", "x", "x", "x"
	a.Source.Partition, a.Source.FSType, a.Source.FSPath = "x", "x", "x"
	a.Source.Encrypted, a.Source.ParentIncomplete = false, false
	a.Source.Snapshot.Name, a.Source.Snapshot.Xid = "x", 1
	a.Source.Snapshot = &SnapshotInfo{Name: "y"}
}

// The acquisition id is part of the Artifact contract (ruling T25): it is carried by Input.Clone
// for the primary and for every role entry, so a parser can require that two artifacts come from
// one acquisition of one device. The field is reached by name so a missing field is a failed
// assertion here.
func TestArtifactCarriesTheAcquisitionID(t *testing.T) {
	typ := reflect.TypeOf(Artifact{})
	if f, ok := typ.FieldByName("Acquisition"); !ok || f.Type.Kind() != reflect.String {
		t.Fatal("parse.Artifact has no string field Acquisition")
	}
	in := fullInput()
	reflect.ValueOf(&in.Primary).Elem().FieldByName("Acquisition").SetString("acq-1")
	for role, a := range in.Artifacts {
		a2 := a
		reflect.ValueOf(&a2).Elem().FieldByName("Acquisition").SetString("acq-" + role)
		in.Artifacts[role] = a2
	}
	c := in.Clone()
	get := func(a Artifact) string { return reflect.ValueOf(a).FieldByName("Acquisition").String() }
	if get(c.Primary) != "acq-1" || get(c.Artifacts["db"]) != "acq-db" || get(c.Artifacts["wal"]) != "acq-wal" {
		t.Fatalf("Clone dropped the acquisition id: %q %q %q", get(c.Primary), get(c.Artifacts["db"]), get(c.Artifacts["wal"]))
	}
	if get(fullInput().Primary) != "acq-a1" {
		t.Fatal("fullInput does not set an acquisition id (the DeepEqual clone tests would not cover the field)")
	}
}

// TestCloneGivesTheParserItsOwnBudgetHandle pins clone isolation of the
// pre-clone Input, which production code discards: the parser's BudgetView is
// a separate handle onto the one accounting state, so overwriting it detaches
// only that handle. It is not a protection of the host (the host keeps no
// handle) and not a boundary against a parser author.
func TestCloneGivesTheParserItsOwnBudgetHandle(t *testing.T) {
	in := fullInput()
	c := in.Clone()
	if c.Budget == in.Budget {
		t.Fatal("the clone shares the BudgetView pointer with the original")
	}
	if c.Budget.s != in.Budget.s {
		t.Fatal("the handles must reach the same accounting state")
	}
	limit := in.Budget.Limit()
	if err := c.Budget.Alloc(100); err != nil {
		t.Fatal(err)
	}
	*c.Budget = BudgetView{} // what a misbehaving parser could do to its own Input
	if in.Budget.Used() != 100 || in.Budget.Limit() != limit {
		t.Fatalf("the original handle saw used=%d limit=%d after the overwrite, want 100/%d", in.Budget.Used(), in.Budget.Limit(), limit)
	}
	if err := in.Budget.Alloc(1); err != nil {
		t.Fatalf("the original handle must keep allocating: %v", err)
	}
	if err := c.Budget.Alloc(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("the overwritten handle must fail closed, got %v", err)
	}
	if c.Budget.Used() != 0 || c.Budget.Limit() != 0 {
		t.Fatalf("the overwritten handle reports used=%d limit=%d, want 0/0", c.Budget.Used(), c.Budget.Limit())
	}
}

func TestCloneCopiesCapUnknown(t *testing.T) {
	a := fullArtifact("1")
	c := a.clone()
	if !c.Recovery.CapUnknown {
		t.Fatal("clone dropped CapUnknown")
	}
	c.Recovery.CapUnknown = false
	if !a.Recovery.CapUnknown {
		t.Fatal("clone shares Recovery with the original")
	}
}

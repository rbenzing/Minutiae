package records_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

// registerTest registers a test type and removes it when the test ends.
func registerTest(t *testing.T, ty records.Type) {
	t.Helper()
	if err := records.RegisterType(ty); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { records.UnregisterType(ty.Name) })
}

func TestSetValidatorInstallsOnce(t *testing.T) {
	registerTest(t, records.Type{Name: "zz_sv1", PayloadVersion: 7})
	if ty, _ := records.LookupType("zz_sv1"); ty.Validate != nil {
		t.Fatal("a type registered without a validator already has one")
	}
	records.SetValidator("zz_sv1", func(map[string]any) error { return nil })
	ty, ok := records.LookupType("zz_sv1")
	if !ok || ty.Validate == nil {
		t.Fatalf("LookupType after SetValidator = %+v, %v: want a validator", ty, ok)
	}
	if ty.PayloadVersion != 7 || ty.Name != "zz_sv1" {
		t.Errorf("SetValidator changed the type: %+v", ty)
	}
}

func TestSetValidatorPanicsOnMisuse(t *testing.T) {
	sentinel := errors.New("first validator")
	first := func(map[string]any) error { return sentinel }
	registerTest(t, records.Type{Name: "zz_sv_twice", PayloadVersion: 2})
	registerTest(t, records.Type{Name: "zz_sv_pre", PayloadVersion: 2, Validate: first})
	registerTest(t, records.Type{Name: "zz_sv_nil", PayloadVersion: 2})
	records.SetValidator("zz_sv_twice", first)

	other := func(map[string]any) error { return errors.New("other validator") }
	for name, fn := range map[string]func(){
		"unknown name":             func() { records.SetValidator("zz_sv_nonesuch", other) },
		"nil validator":            func() { records.SetValidator("zz_sv_nil", nil) },
		"second call":              func() { records.SetValidator("zz_sv_twice", other) },
		"registered with Validate": func() { records.SetValidator("zz_sv_pre", other) },
		"core type twice":          func() { records.SetValidator("zz_sv_pre", first) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: SetValidator did not panic", name)
				}
			}()
			fn()
		}()
	}
	for _, n := range []string{"zz_sv_twice", "zz_sv_pre"} {
		ty, _ := records.LookupType(n)
		if ty.Validate == nil || !errors.Is(ty.Validate(nil), sentinel) {
			t.Errorf("%s: a refused SetValidator altered the first validator", n)
		}
	}
	if ty, _ := records.LookupType("zz_sv_nil"); ty.Validate != nil {
		t.Error("a nil SetValidator installed a validator")
	}
	if _, ok := records.LookupType("zz_sv_nonesuch"); ok {
		t.Error("SetValidator registered an unknown type")
	}
}

func TestSetValidatorIsUsedByPrepare(t *testing.T) {
	c, a := setup(t)
	registerTest(t, records.Type{Name: "zz_sv_prep", PayloadVersion: 4})
	var got map[string]any
	boom := errors.New("n must be positive")
	records.SetValidator("zz_sv_prep", func(p map[string]any) error {
		got = p
		if n, _ := p["n"].(int); n <= 0 {
			return boom
		}
		p["mutated"] = true // a validator may scribble on its copy
		if inner, ok := p["inner"].(map[string]any); ok {
			inner["k"] = "changed"
		}
		return nil
	})

	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	bad := records.Record{Type: "zz_sv_prep", ArtifactID: a.ID, Payload: map[string]any{"n": 0}}
	err := w.Add(ctx, bad)
	if !errors.Is(err, records.ErrInvalidPayload) || !errors.Is(err, boom) {
		t.Fatalf("refused payload: err = %v, want ErrInvalidPayload wrapping the validator's error", err)
	}

	inner := map[string]any{"k": "orig"}
	good := records.Record{Type: "zz_sv_prep", ArtifactID: a.ID, Payload: map[string]any{"n": 1, "inner": inner}}
	if err := w.Add(ctx, good); err != nil {
		t.Fatalf("accepted payload: %v", err)
	}
	if _, ok := good.Payload["mutated"]; ok || inner["k"] != "orig" {
		t.Errorf("the validator changed the caller's payload: %v, inner %v", good.Payload, inner)
	}
	if got == nil || got["mutated"] != true {
		t.Error("the validator did not see a payload to work on")
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 1 {
		t.Fatalf("%d records stored, want 1 (the refused one is never buffered)", res.Records)
	}
	rows := loadRows(t, c)
	if len(rows) != 1 || rows[0].Type != "zz_sv_prep" || rows[0].PayloadV != 4 {
		t.Fatalf("stored rows = %+v, want one zz_sv_prep with payload_v 4", rows)
	}

	// Prepare reports the same refusal without a writer
	_, err = records.Prepare(bad, records.ArtifactInfo{ID: a.ID, Size: 10})
	if !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("Prepare of a refused payload = %v, want ErrInvalidPayload", err)
	}
}

func TestSetValidatorConcurrentWithLookup(t *testing.T) {
	const n = 8
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("zz_sv_conc%d", i)
		registerTest(t, records.Type{Name: names[i], PayloadVersion: 1})
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, name := range names {
					_, _ = records.LookupType(name)
				}
				for _, ti := range records.Types() {
					_ = ti.HasValidator
				}
			}
		}()
	}
	var setters sync.WaitGroup
	for _, name := range names {
		setters.Add(1)
		go func() {
			defer setters.Done()
			records.SetValidator(name, func(map[string]any) error { return nil })
		}()
	}
	setters.Wait()
	close(stop)
	readers.Wait()

	flags := map[string]bool{}
	for _, ti := range records.Types() {
		flags[ti.Name] = ti.HasValidator
	}
	for _, name := range names {
		ty, ok := records.LookupType(name)
		if !ok || ty.Validate == nil || !flags[name] {
			t.Errorf("%s: lookup validator set = %v, Types flag = %v: want both true", name, ty.Validate != nil, flags[name])
		}
	}
}

// TestCoreTypeValidatorsUnsetInRecordsPackage guards a cross-plan hazard: the
// per-type contracts are installed by init functions of the recordtypes package,
// which the records test binary never links. If a core type had a validator here,
// the record fixtures of the records tests (arbitrary payloads) would start to fail.
func TestCoreTypeValidatorsUnsetInRecordsPackage(t *testing.T) {
	core := []string{
		"message", "call", "contact", "calendar_event", "location", "web_visit",
		"web_search", "download", "file", "account", "app_event", "media", "note",
		"wifi_network", "event",
	}
	for _, n := range core {
		ty, ok := records.LookupType(n)
		if !ok {
			t.Errorf("core type %q is not registered", n)
			continue
		}
		if ty.Validate != nil {
			t.Errorf("core type %q has a validator in the records package: recordtypes leaked into this test binary", n)
		}
	}
}

package parse

import (
	"errors"
	"strings"
	"testing"
)

func validFacts() RecoveredFacts {
	return RecoveredFacts{
		Table: "sms", Method: "freelist-page", Basis: "fit", Relation: "absent-from-live", Origin: "db",
		Confidence: 60, Role: RoleDB, Locator: "db:page=3:cell=1", Page: 3, Cell: 1,
	}
}

func TestRecoveredFactsValidate(t *testing.T) {
	if err := validFacts().Validate(); err != nil {
		t.Fatalf("valid facts: %v", err)
	}
	wal := validFacts()
	wal.Role, wal.WAL = RoleWAL, &RecoveredWAL{Frame: 1}
	if err := wal.Validate(); err != nil {
		t.Fatalf("valid wal facts: %v", err)
	}
	jr := validFacts()
	jr.Role, jr.Journal = RoleJournal, &RecoveredJournal{Record: 1}
	if err := jr.Validate(); err != nil {
		t.Fatalf("valid journal facts: %v", err)
	}
	for _, rel := range []string{"superseded-version", "uncommitted", "unknown"} {
		f := validFacts()
		f.Relation = rel
		if err := f.Validate(); err != nil {
			t.Fatalf("relation %s: %v", rel, err)
		}
	}
	for _, b := range []string{"schema", "fit"} {
		f := validFacts()
		f.Basis = b
		if err := f.Validate(); err != nil {
			t.Fatalf("basis %s: %v", b, err)
		}
	}
	bad := []struct {
		name string
		mut  func(*RecoveredFacts)
	}{
		{"method empty", func(f *RecoveredFacts) { f.Method = "" }},
		{"method upper", func(f *RecoveredFacts) { f.Method = "Freelist" }},
		{"method one char", func(f *RecoveredFacts) { f.Method = "a" }},
		{"method digit first", func(f *RecoveredFacts) { f.Method = "1ab" }},
		{"method too long", func(f *RecoveredFacts) { f.Method = "a" + strings.Repeat("b", 32) }},
		{"confidence -1", func(f *RecoveredFacts) { f.Confidence = -1 }},
		{"confidence 101", func(f *RecoveredFacts) { f.Confidence = 101 }},
		{"basis guess", func(f *RecoveredFacts) { f.Basis = "guess" }},
		{"basis none", func(f *RecoveredFacts) { f.Basis = "none" }},
		{"basis empty", func(f *RecoveredFacts) { f.Basis = "" }},
		{"role empty", func(f *RecoveredFacts) { f.Role = "" }},
		{"role other", func(f *RecoveredFacts) { f.Role = "shm" }},
		{"locator empty", func(f *RecoveredFacts) { f.Locator = "" }},
		{"locator too long", func(f *RecoveredFacts) { f.Locator = strings.Repeat("x", 1025) }},
		{"relation other", func(f *RecoveredFacts) { f.Relation = "live" }},
		{"relation empty", func(f *RecoveredFacts) { f.Relation = "" }},
		{"wal on db role", func(f *RecoveredFacts) { f.WAL = &RecoveredWAL{} }},
		{"journal on db role", func(f *RecoveredFacts) { f.Journal = &RecoveredJournal{} }},
		{"journal on wal role", func(f *RecoveredFacts) { f.Role = RoleWAL; f.Journal = &RecoveredJournal{} }},
		{"wal on journal role", func(f *RecoveredFacts) { f.Role = RoleJournal; f.WAL = &RecoveredWAL{} }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			f := validFacts()
			tc.mut(&f)
			if err := f.Validate(); !errors.Is(err, ErrBadRecoveredFacts) {
				t.Fatalf("got %v, want ErrBadRecoveredFacts", err)
			}
		})
	}
	f := validFacts()
	f.Locator = strings.Repeat("x", 1024)
	if err := f.Validate(); err != nil {
		t.Fatalf("1024-byte locator: %v", err)
	}
	f = validFacts()
	if n := testing.AllocsPerRun(10, func() { _ = f.Validate() }); n != 0 {
		t.Fatalf("Validate allocated %v times on success", n)
	}
}

func TestRecoveryName(t *testing.T) {
	if RecoveryName("android.mmssms") != "android.mmssms.recover" || RecoverySuffix != ".recover" {
		t.Fatal("RecoveryName")
	}
}

func TestRecoverySentinelsAreDistinct(t *testing.T) {
	if errors.Is(ErrRecoveryFieldsFromParser, ErrLiveEmitInRecovery) || errors.Is(ErrBadRecoveredFacts, ErrLiveEmitInRecovery) {
		t.Fatal("sentinels must be distinct")
	}
}

func TestValidateMetaRefusesLongClaimingName(t *testing.T) {
	m := validMeta()
	m.Name = "a" + strings.Repeat("b", 56) // 57 bytes
	if err := ValidateMeta(m); err == nil {
		t.Fatal("57-byte claiming name accepted")
	}
	m.Name = "a" + strings.Repeat("b", 55) // 56 bytes
	if err := ValidateMeta(m); err != nil {
		t.Fatalf("56-byte claiming name: %v", err)
	}
	m = validMeta()
	m.Claims = nil
	m.Name = "a" + strings.Repeat("b", 59) // 60 bytes, no claims
	if err := ValidateMeta(m); err != nil {
		t.Fatalf("60-byte name without claims: %v", err)
	}
}

func TestValidateMetaRefusesRecoverSuffix(t *testing.T) {
	for _, claims := range []bool{true, false} {
		m := validMeta()
		if !claims {
			m.Claims = nil
		}
		m.Name = "x.recover"
		if err := ValidateMeta(m); err == nil {
			t.Fatalf("name x.recover accepted (claims=%v)", claims)
		}
	}
}

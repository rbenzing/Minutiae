package parse

import (
	"strings"
	"testing"
	"time"
)

const (
	kib = int64(1) << 10
	mib = int64(1) << 20
	gib = int64(1) << 30
)

func TestLimitsDefaultsAndValidate(t *testing.T) {
	d := DefaultLimits()
	want := Limits{
		ProbeBytes:     mib,
		MemInputMax:    256 * mib,
		MemInputTotal:  gib,
		MemBudget:      gib,
		MaxRecords:     10_000_000,
		MaxRejected:    1000,
		MaxNotes:       64,
		MaxLookupOpens: 1000,
		ProbeTimeout:   30 * time.Second,
		Timeout:        time.Hour,
		GracePeriod:    10 * time.Second,
	}
	if d != want {
		t.Fatalf("DefaultLimits() = %+v, want %+v", d, want)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}

	// Each bound is inclusive; one step outside it is an error naming the field.
	bounds := []struct {
		field    string
		set      func(l *Limits, v int64)
		min, max int64
	}{
		{"ProbeBytes", func(l *Limits, v int64) { l.ProbeBytes = v }, 4 * kib, 64 * mib},
		{"ProbeTimeout", func(l *Limits, v int64) { l.ProbeTimeout = time.Duration(v) }, int64(100 * time.Millisecond), int64(10 * time.Minute)},
		{"MemInputMax", func(l *Limits, v int64) { l.MemInputMax = v; l.MemInputTotal = 8 * gib }, 0, 4 * gib},
		{"MemBudget", func(l *Limits, v int64) { l.MemBudget = v }, 16 * mib, 64 * gib},
		{"MaxRecords", func(l *Limits, v int64) { l.MaxRecords = v }, 1, 1_000_000_000},
		{"MaxRejected", func(l *Limits, v int64) { l.MaxRejected = int(v) }, 1, 1_000_000},
		{"Timeout", func(l *Limits, v int64) { l.Timeout = time.Duration(v) }, int64(time.Second), int64(24 * time.Hour)},
		{"GracePeriod", func(l *Limits, v int64) { l.GracePeriod = time.Duration(v) }, int64(10 * time.Millisecond), int64(10 * time.Minute)},
		{"MaxNotes", func(l *Limits, v int64) { l.MaxNotes = int(v) }, 1, 1024},
		{"MaxLookupOpens", func(l *Limits, v int64) { l.MaxLookupOpens = int(v) }, 1, 100_000},
	}
	for _, b := range bounds {
		t.Run(b.field, func(t *testing.T) {
			for _, v := range []int64{b.min, b.max} {
				l := DefaultLimits()
				b.set(&l, v)
				if err := l.Validate(); err != nil {
					t.Errorf("%s = %d (a bound) refused: %v", b.field, v, err)
				}
			}
			for _, v := range []int64{b.min - 1, b.max + 1} {
				l := DefaultLimits()
				b.set(&l, v)
				err := l.Validate()
				if err == nil {
					t.Errorf("%s = %d accepted", b.field, v)
				} else if !strings.Contains(err.Error(), b.field) {
					t.Errorf("%s = %d: error %q does not name the field", b.field, v, err)
				}
			}
		})
	}

	t.Run("MemInputTotal is at least MemInputMax", func(t *testing.T) {
		l := DefaultLimits()
		l.MemInputTotal = l.MemInputMax
		if err := l.Validate(); err != nil {
			t.Fatalf("total == max refused: %v", err)
		}
		l.MemInputTotal = l.MemInputMax - 1
		err := l.Validate()
		if err == nil || !strings.Contains(err.Error(), "MemInputTotal") {
			t.Fatalf("total < max: %v", err)
		}
		l = DefaultLimits()
		l.MemInputMax, l.MemInputTotal = 0, 0 // always stream
		if err := l.Validate(); err != nil {
			t.Fatalf("stream-only limits refused: %v", err)
		}
		l.MemInputTotal = -1
		if err := l.Validate(); err == nil {
			t.Fatal("negative total accepted")
		}
	})

	t.Run("the production minimums stand", func(t *testing.T) {
		l := DefaultLimits()
		l.Timeout = 50 * time.Millisecond
		if err := l.Validate(); err == nil || !strings.Contains(err.Error(), "Timeout") {
			t.Fatalf("Timeout 50ms: %v", err)
		}
		l = DefaultLimits()
		l.GracePeriod = time.Millisecond
		if err := l.Validate(); err == nil {
			t.Fatal("GracePeriod 1ms accepted")
		}
		if err := (Limits{}).Validate(); err == nil {
			t.Fatal("the zero Limits was accepted")
		}
	})
}

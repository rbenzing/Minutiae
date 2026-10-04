package sqlitefile_test

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// ceilingTable is the hard ceiling of every Limits field, by field name (own
// copy: the test must not read the ceilings from the code under test). A
// caller value above it is clamped and the clamp is reported. MaxBTreeDepth
// is the one that guards the goroutine stack: a stack overflow is fatal in Go
// and no guard can recover it.
var ceilingTable = map[string]int64{
	"MaxBTreeDepth":           64,
	"MaxColumns":              32767,
	"MaxTextBytes":            1 << 30,
	"MaxBlobBytes":            1 << 30,
	"MaxRecoveredValueBytes":  1 << 30,
	"MaxRowBytes":             1 << 32,
	"MaxPayloadBytes":         1 << 30,
	"MaxSchemaObjects":        10000000,
	"MaxSchemaSQLBytes":       64 << 20,
	"MaxSchemaTotalBytes":     1 << 30,
	"MaxPages":                1<<32 - 2,
	"PageCacheBytes":          1 << 32,
	"DefaultBudgetBytes":      1 << 36,
	"MaxWALFrames":            1 << 32,
	"MaxJournalRecords":       1 << 32,
	"MaxJournalSegments":      1 << 20,
	"MaxHistoryPages":         1 << 32,
	"MaxHistoryRows":          1 << 32,
	"MaxHistoryOverflowPages": 1 << 32,
	"MaxDiffRows":             1 << 28,
	"MaxDiffRowsTotal":        1 << 29,
	"MaxFitSteps":             1 << 30,
	"MaxOrphans":              1 << 24,
	"MaxLocOverflow":          1 << 16,
	"MaxWarnings":             100000,
}

func TestLimitsCeilings(t *testing.T) {
	d := reflect.ValueOf(sqlitefile.DefaultLimits())
	typ := d.Type()
	if typ.NumField() != len(ceilingTable) {
		t.Fatalf("Limits has %d fields, the ceiling table %d: keep them in step", typ.NumField(), len(ceilingTable))
	}
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		ceil, ok := ceilingTable[name]
		if !ok {
			t.Errorf("Limits.%s has no ceiling in the table", name)
			continue
		}
		if def := d.Field(i).Int(); def > ceil {
			t.Errorf("default %s = %d is above its ceiling %d", name, def, ceil)
		}
		for _, tc := range []struct {
			in      int64
			want    int64
			clamped bool
		}{
			{ceil, ceil, false},
			{ceil + 1, ceil, true},
			{math.MaxInt64, ceil, true},
		} {
			var l sqlitefile.Limits
			reflect.ValueOf(&l).Elem().Field(i).SetInt(tc.in)
			got, notes := sqlitefile.ResolveLimitsReport(l)
			if v := reflect.ValueOf(got).Field(i).Int(); v != tc.want {
				t.Errorf("%s = %d resolved to %d, want %d", name, tc.in, v, tc.want)
			}
			if tc.clamped != (len(notes) == 1) || (len(notes) == 1 && !strings.Contains(notes[0], name)) {
				t.Errorf("%s = %d: clamp notes %q, want a note naming the field = %v", name, tc.in, notes, tc.clamped)
			}
			// the other fields are untouched by this one
			for j := range typ.NumField() {
				if j != i && reflect.ValueOf(got).Field(j).Int() != d.Field(j).Int() {
					t.Errorf("setting %s changed %s", name, typ.Field(j).Name)
				}
			}
		}
	}
	if ceilingTable["MaxBTreeDepth"] != 64 {
		t.Error("the stack-safety ceiling of MaxBTreeDepth is 64")
	}
	if _, notes := sqlitefile.ResolveLimitsReport(sqlitefile.Limits{}); len(notes) != 0 {
		t.Errorf("default limits report clamps: %q", notes)
	}
}

// TestOpenReportsClampedLimits: a ceiling hit is visible to the caller as a
// limit-reached warning, and the instance runs with the ceiling.
func TestOpenReportsClampedLimits(t *testing.T) {
	data := sqlitetest.New(sqlitetest.Options{}).Bytes()
	db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxBTreeDepth: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if got := db.EffectiveLimits().MaxBTreeDepth; got != 64 {
		t.Errorf("MaxBTreeDepth in effect = %d, want the ceiling 64", got)
	}
	var found bool
	for _, w := range db.Warnings() {
		if w.Code == sqlitefile.WarnLimitReached && strings.Contains(w.Msg, "MaxBTreeDepth") {
			found = true
		}
	}
	if !found {
		t.Errorf("no limit-reached warning names MaxBTreeDepth: %+v", db.Warnings())
	}
	// default limits: no such warning
	clean, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Warnings()) != 0 {
		t.Errorf("default Options warn: %+v", clean.Warnings())
	}
}

func TestWarningMessageIsClipped(t *testing.T) {
	w := sqlitefile.NewTestWarnings(0)
	long := strings.Repeat("a", 5000)
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, Msg: long})
	// a multi-byte rune that straddles the cut must not be split
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, Msg: strings.Repeat("é", 1000)})
	short := sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, Msg: "short"}
	w.Add(short)
	got := w.Snapshot()
	if len(got) != 3 {
		t.Fatalf("recorded %d warnings, want 3", len(got))
	}
	for _, x := range got[:2] {
		if len(x.Msg) > 256 || !utf8.ValidString(x.Msg) || !strings.HasSuffix(x.Msg, "[clipped]") {
			t.Errorf("long message not clipped and marked: %d bytes, valid=%v, tail %q", len(x.Msg), utf8.ValidString(x.Msg), x.Msg[max(0, len(x.Msg)-20):])
		}
	}
	if got[2].Msg != "short" {
		t.Errorf("a short message was changed: %q", got[2].Msg)
	}
	// exactly at the limit: untouched
	exact := strings.Repeat("b", 256)
	w2 := sqlitefile.NewTestWarnings(0)
	w2.Add(sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, Msg: exact})
	if g := w2.Snapshot()[0].Msg; g != exact {
		t.Errorf("a 256-byte message was clipped to %d bytes", len(g))
	}
}

func TestWarningDedupKeyIncludesOffset(t *testing.T) {
	w := sqlitefile.NewTestWarnings(0)
	base := sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, File: sqlitefile.FileDB, Page: 3, Msg: "m"}
	for _, off := range []int64{10, 20, 10} {
		x := base
		x.Offset = off
		w.Add(x)
	}
	got := w.Snapshot()
	if len(got) != 2 || got[0].Offset != 10 || got[1].Offset != 20 {
		t.Errorf("warnings %+v, want one per distinct offset (10, 20), the repeat collapsed", got)
	}
}

// countingBudget counts the bytes it has granted and not had returned.
type countingBudget struct {
	mu    sync.Mutex
	used  int64
	limit int64
}

func (c *countingBudget) Alloc(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 || n > c.limit-c.used {
		return sqlitefile.ErrBudget
	}
	c.used += n
	return nil
}

func (c *countingBudget) Free(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used -= n
}

func (c *countingBudget) inUse() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// TestLedgerReleasesChargesOnPanicAndError: what a call charged is given
// back when a panic unwinds it or it fails; a call that succeeds hands what
// it charged to its result.
func TestLedgerReleasesChargesOnPanicAndError(t *testing.T) {
	boom := errors.New("scan failed")
	for _, tc := range []struct {
		name     string
		fn       func(l *sqlitefile.TestLedger) error
		wantErr  func(error) bool
		wantUsed int64
	}{
		{"panic after charges", func(l *sqlitefile.TestLedger) error {
			if err := l.Alloc(100); err != nil {
				return err
			}
			if err := l.Alloc(50); err != nil {
				return err
			}
			panic("defect")
		}, func(err error) bool { var pe *sqlitefile.PanicError; return errors.As(err, &pe) }, 0},
		{"error after charges", func(l *sqlitefile.TestLedger) error {
			if err := l.Alloc(100); err != nil {
				return err
			}
			return boom
		}, func(err error) bool { return errors.Is(err, boom) }, 0},
		{"success keeps the charge for the result", func(l *sqlitefile.TestLedger) error {
			return l.Alloc(100)
		}, func(err error) bool { return err == nil }, 100},
		{"a refused charge grants nothing", func(l *sqlitefile.TestLedger) error {
			if err := l.Alloc(60); err != nil {
				return err
			}
			return l.Alloc(1 << 40)
		}, func(err error) bool { return errors.Is(err, sqlitefile.ErrBudget) }, 0},
		{"explicit free inside the call", func(l *sqlitefile.TestLedger) error {
			if err := l.Alloc(100); err != nil {
				return err
			}
			l.Free(40)
			panic("defect")
		}, func(err error) bool { return errors.Is(err, sqlitefile.ErrInternal) }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &countingBudget{limit: 1 << 20}
			err := sqlitefile.LedgerCall(b, tc.fn)
			if !tc.wantErr(err) {
				t.Errorf("err = %v", err)
			}
			if got := b.inUse(); got != tc.wantUsed {
				t.Errorf("budget in use = %d, want %d", got, tc.wantUsed)
			}
		})
	}
}

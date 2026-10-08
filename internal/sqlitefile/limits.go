package sqlitefile

import (
	"fmt"
	"sync"
)

// Budget accounts for the memory the library holds on behalf of a database
// (page cache, scan results, overlay maps, digest sets, layout arrays).
// Alloc returns an error wrapping ErrBudget when the request cannot be
// granted; a failed Alloc grants nothing.
type Budget interface {
	Alloc(n int64) error
	Free(n int64)
}

// Limits are the structural and memory caps. Every field is a limit; a zero
// (or negative) field means its default from DefaultLimits, and a value above
// the field's hard ceiling (see resolve) is clamped to it and reported. The
// ceiling of MaxBTreeDepth keeps recursion within the goroutine stack: a
// stack overflow is fatal in Go and no guard can recover it. Tree walks are
// iterative where they can be.
type Limits struct {
	MaxBTreeDepth                       int
	MaxColumns                          int
	MaxTextBytes, MaxBlobBytes          int64 // live values: longer ones are omitted
	MaxRecoveredValueBytes              int64 // history values: longer ones are clipped
	MaxRowBytes, MaxPayloadBytes        int64
	MaxSchemaObjects, MaxSchemaSQLBytes int
	MaxSchemaTotalBytes                 int64
	MaxPages                            int64
	PageCacheBytes, DefaultBudgetBytes  int64
	MaxWALFrames, MaxJournalRecords     int64
	MaxJournalSegments                  int
	MaxHistoryPages, MaxHistoryRows     int64
	MaxHistoryOverflowPages             int64
	MaxDiffRows, MaxDiffRowsTotal       int64
	MaxFitSteps                         int64
	MaxOrphans, MaxLocOverflow          int
	MaxWarnings                         int
}

// DefaultLimits returns the caps of the Format reference.
func DefaultLimits() Limits {
	return Limits{
		MaxBTreeDepth:           32,
		MaxColumns:              2000,
		MaxTextBytes:            4 << 20,
		MaxBlobBytes:            64 << 20,
		MaxRecoveredValueBytes:  1 << 20,
		MaxRowBytes:             128 << 20,
		MaxPayloadBytes:         1 << 30,
		MaxSchemaObjects:        100000,
		MaxSchemaSQLBytes:       1 << 20,
		MaxSchemaTotalBytes:     64 << 20,
		MaxPages:                1 << 25,
		PageCacheBytes:          32 << 20,
		DefaultBudgetBytes:      1 << 30,
		MaxWALFrames:            1 << 24,
		MaxJournalRecords:       1 << 24,
		MaxJournalSegments:      1 << 16,
		MaxHistoryPages:         1 << 26,
		MaxHistoryRows:          50000000,
		MaxHistoryOverflowPages: 1 << 24,
		MaxDiffRows:             2000000,
		MaxDiffRowsTotal:        4000000,
		MaxFitSteps:             1 << 20,
		MaxOrphans:              100000,
		MaxLocOverflow:          4096,
		MaxWarnings:             1000,
	}
}

// withDefaults returns l with every zero or negative field replaced by its
// default, so a partial Limits keeps what the caller set.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	i := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	n := func(v *int64, def int64) {
		if *v <= 0 {
			*v = def
		}
	}
	i(&l.MaxBTreeDepth, d.MaxBTreeDepth)
	i(&l.MaxColumns, d.MaxColumns)
	n(&l.MaxTextBytes, d.MaxTextBytes)
	n(&l.MaxBlobBytes, d.MaxBlobBytes)
	n(&l.MaxRecoveredValueBytes, d.MaxRecoveredValueBytes)
	n(&l.MaxRowBytes, d.MaxRowBytes)
	n(&l.MaxPayloadBytes, d.MaxPayloadBytes)
	i(&l.MaxSchemaObjects, d.MaxSchemaObjects)
	i(&l.MaxSchemaSQLBytes, d.MaxSchemaSQLBytes)
	n(&l.MaxSchemaTotalBytes, d.MaxSchemaTotalBytes)
	n(&l.MaxPages, d.MaxPages)
	n(&l.PageCacheBytes, d.PageCacheBytes)
	n(&l.DefaultBudgetBytes, d.DefaultBudgetBytes)
	n(&l.MaxWALFrames, d.MaxWALFrames)
	n(&l.MaxJournalRecords, d.MaxJournalRecords)
	i(&l.MaxJournalSegments, d.MaxJournalSegments)
	n(&l.MaxHistoryPages, d.MaxHistoryPages)
	n(&l.MaxHistoryRows, d.MaxHistoryRows)
	n(&l.MaxHistoryOverflowPages, d.MaxHistoryOverflowPages)
	n(&l.MaxDiffRows, d.MaxDiffRows)
	n(&l.MaxDiffRowsTotal, d.MaxDiffRowsTotal)
	n(&l.MaxFitSteps, d.MaxFitSteps)
	i(&l.MaxOrphans, d.MaxOrphans)
	i(&l.MaxLocOverflow, d.MaxLocOverflow)
	i(&l.MaxWarnings, d.MaxWarnings)
	return l
}

// ceilingLimits returns the hard ceiling of every limit: no caller value
// raises a limit above it. They are far above the defaults, so they bound a
// hostile or mistaken caller, not honest use. MaxBTreeDepth is the stack
// safety one.
func ceilingLimits() Limits {
	return Limits{
		MaxBTreeDepth:           64,
		MaxColumns:              32767,
		MaxTextBytes:            1 << 30,
		MaxBlobBytes:            1 << 30,
		MaxRecoveredValueBytes:  1 << 30,
		MaxRowBytes:             1 << 32,
		MaxPayloadBytes:         1 << 30,
		MaxSchemaObjects:        10000000,
		MaxSchemaSQLBytes:       64 << 20,
		MaxSchemaTotalBytes:     1 << 30,
		MaxPages:                1<<32 - 2,
		PageCacheBytes:          1 << 32,
		DefaultBudgetBytes:      1 << 36,
		MaxWALFrames:            1 << 32,
		MaxJournalRecords:       1 << 32,
		MaxJournalSegments:      1 << 20,
		MaxHistoryPages:         1 << 32,
		MaxHistoryRows:          1 << 32,
		MaxHistoryOverflowPages: 1 << 32,
		MaxDiffRows:             1 << 28,
		MaxDiffRowsTotal:        1 << 29,
		MaxFitSteps:             1 << 30,
		MaxOrphans:              1 << 24,
		MaxLocOverflow:          1 << 16,
		MaxWarnings:             100000,
	}
}

// resolve returns l with defaults applied (withDefaults) and every field
// above its hard ceiling clamped to it, with one note per clamped field.
func (l Limits) resolve() (Limits, []string) {
	l = l.withDefaults()
	c := ceilingLimits()
	var notes []string
	i := func(v *int, ceil int, name string) {
		if *v > ceil {
			notes = append(notes, fmt.Sprintf("limit %s clamped from %d to its ceiling %d", name, *v, ceil))
			*v = ceil
		}
	}
	n := func(v *int64, ceil int64, name string) {
		if *v > ceil {
			notes = append(notes, fmt.Sprintf("limit %s clamped from %d to its ceiling %d", name, *v, ceil))
			*v = ceil
		}
	}
	i(&l.MaxBTreeDepth, c.MaxBTreeDepth, "MaxBTreeDepth")
	i(&l.MaxColumns, c.MaxColumns, "MaxColumns")
	n(&l.MaxTextBytes, c.MaxTextBytes, "MaxTextBytes")
	n(&l.MaxBlobBytes, c.MaxBlobBytes, "MaxBlobBytes")
	n(&l.MaxRecoveredValueBytes, c.MaxRecoveredValueBytes, "MaxRecoveredValueBytes")
	n(&l.MaxRowBytes, c.MaxRowBytes, "MaxRowBytes")
	n(&l.MaxPayloadBytes, c.MaxPayloadBytes, "MaxPayloadBytes")
	i(&l.MaxSchemaObjects, c.MaxSchemaObjects, "MaxSchemaObjects")
	i(&l.MaxSchemaSQLBytes, c.MaxSchemaSQLBytes, "MaxSchemaSQLBytes")
	n(&l.MaxSchemaTotalBytes, c.MaxSchemaTotalBytes, "MaxSchemaTotalBytes")
	n(&l.MaxPages, c.MaxPages, "MaxPages")
	n(&l.PageCacheBytes, c.PageCacheBytes, "PageCacheBytes")
	n(&l.DefaultBudgetBytes, c.DefaultBudgetBytes, "DefaultBudgetBytes")
	n(&l.MaxWALFrames, c.MaxWALFrames, "MaxWALFrames")
	n(&l.MaxJournalRecords, c.MaxJournalRecords, "MaxJournalRecords")
	i(&l.MaxJournalSegments, c.MaxJournalSegments, "MaxJournalSegments")
	n(&l.MaxHistoryPages, c.MaxHistoryPages, "MaxHistoryPages")
	n(&l.MaxHistoryRows, c.MaxHistoryRows, "MaxHistoryRows")
	n(&l.MaxHistoryOverflowPages, c.MaxHistoryOverflowPages, "MaxHistoryOverflowPages")
	n(&l.MaxDiffRows, c.MaxDiffRows, "MaxDiffRows")
	n(&l.MaxDiffRowsTotal, c.MaxDiffRowsTotal, "MaxDiffRowsTotal")
	n(&l.MaxFitSteps, c.MaxFitSteps, "MaxFitSteps")
	i(&l.MaxOrphans, c.MaxOrphans, "MaxOrphans")
	i(&l.MaxLocOverflow, c.MaxLocOverflow, "MaxLocOverflow")
	i(&l.MaxWarnings, c.MaxWarnings, "MaxWarnings")
	return l, notes
}

// Options configure Open, ScanWAL and ScanJournal. The zero Options is valid.
type Options struct {
	Limits Limits
	// Budget is charged for everything the library keeps. Nil: an internal
	// budget of Limits.DefaultBudgetBytes (there is no unbudgeted mode).
	Budget Budget
	// SuperJournalPresent says the journal's named super-journal file exists
	// (a journal that names one is applied only then; Format reference,
	// journal rules).
	SuperJournalPresent bool
}

// memBudget is the internal Budget: a mutex-guarded counter against a fixed
// limit.
type memBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func (b *memBudget) Alloc(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 {
		return fmt.Errorf("%w: negative request of %d bytes", ErrBudget, n)
	}
	if n > b.limit-b.used { // no overflow: used <= limit
		return fmt.Errorf("%w: %d bytes requested, %d of %d in use", ErrBudget, n, b.used, b.limit)
	}
	b.used += n
	return nil
}

func (b *memBudget) Free(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= min(n, b.used)
}

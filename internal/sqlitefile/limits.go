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

// Limits are the structural and memory caps. Every field is a ceiling; a zero
// (or negative) field means its default from DefaultLimits.
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

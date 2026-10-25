package artparse

import (
	"context"
	"time"
)

// Test seams of the isolation guard and the emitter.
var (
	Guard     = guard
	PanicText = panicText
)

func GuardForTest(ctx context.Context, timeout, grace time.Duration, seal func(), pump <-chan func(), fn func(context.Context) error) Guarded {
	return guard(ctx, timeout, grace, seal, pump, fn)
}

var CleanAuditText = cleanAuditText

type Emitter = emitter

var NewEmitter = newEmitter

func (e *emitter) Seal()                                      { e.seal() }
func (e *emitter) Counts() (accepted, rejected, warnings int) { return e.counts() }
func (e *emitter) Notes() map[string]string                   { return e.notes() }

// SetAfterInflight installs the interleaving hook between the in-flight increment and the seal
// re-check; MarkSealed sets the seal flag without waiting (what a racing seal does first).
func (e *emitter) SetAfterInflight(f func()) { e.afterInflight = f }
func (e *emitter) MarkSealed()               { e.sealed.Store(true) }

// StuckInWriter reports whether seal gave up waiting for a writer call.
func (e *emitter) StuckInWriter() bool { return e.stuckInWriter() }

// Test seams of the recovery emitter (4G Task 7).
type RecoveryEmitter = recoveryEmitter

var NewRecoveryEmitter = newRecoveryEmitter

func (r *recoveryEmitter) Base() *emitter                       { return r.emitter }
func (r *recoveryEmitter) BelowMin() int64                      { return r.belowMin.Load() }
func (r *recoveryEmitter) UnmappedCount() int64                 { return r.unmapped.Load() }
func (r *recoveryEmitter) UnmappedLocators() ([][16]byte, bool) { return r.unmappedLocators() }

// SetMaxLocators changes the bound on the remembered unmapped locators of this emitter only.
func (r *recoveryEmitter) SetMaxLocators(n int) { r.maxLoc = n }

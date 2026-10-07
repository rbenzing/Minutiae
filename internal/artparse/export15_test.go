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

//go:build scale

package evidence

import "time"

// SetVerifyPhaseHook installs an observer of the time every phase of case verify takes (the scale
// harness records it in docs/performance.md). It exists only in builds with the scale tag.
// Contract: the hook is called with the verify's phase lock held, from any goroutine of the verify, one call at a time;
// it must not block and must not start or end a phase (it must not call back into the case).
func (c *Case) SetVerifyPhaseHook(f func(phase string, d time.Duration)) { c.verifyPhaseHook = f }

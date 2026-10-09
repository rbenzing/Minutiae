//go:build scale

package evidence

import "time"

// SetVerifyPhaseHook installs an observer of the time every phase of case verify takes (the scale
// harness records it in docs/performance.md). It exists only in builds with the scale tag.
func (c *Case) SetVerifyPhaseHook(f func(phase string, d time.Duration)) { c.verifyPhaseHook = f }

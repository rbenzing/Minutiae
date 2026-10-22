package evidence

import "time"

// phase starts timing a phase of case verify and returns the function that ends it. Without a hook
// (every build except the scale harness and the tests) it only costs a nil check; a hook observes,
// it cannot change what verify does.
//
// A name containing "/" is a sub-phase, "<parent>/<table>/<step>": it is nested inside its parent and
// ends before the parent ends, so a sum of phase times counts only the names without "/".
func (c *Case) phase(name string) func() {
	h := c.verifyPhaseHook
	if h == nil {
		return func() {}
	}
	start := time.Now()
	return func() { h(name, time.Since(start)) }
}

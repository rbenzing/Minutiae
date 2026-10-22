package evidence

import "time"

// phase starts timing a phase of case verify and returns the function that ends it. Without a hook
// and without a progress observer (every build except the scale harness, the tests and the command
// line status display) it only costs two nil checks; a hook or observer watches, it cannot change
// what verify does. The hook runs before the observer's phase ends, so a hook may take a Snapshot
// that still shows the finished phase's counter.
//
// A name containing "/" is a sub-phase, "<parent>/<table>/<step>": it is nested inside its parent and
// ends before the parent ends, so a sum of phase times counts only the names without "/".
func (c *Case) phase(name string) func() {
	h, p := c.verifyPhaseHook, c.verifyProgress
	if h == nil && p == nil {
		return func() {}
	}
	start := time.Now()
	var f *progressFrame
	if p != nil {
		f = p.begin(name, false, 0, "")
	}
	return func() {
		if h != nil {
			h(name, time.Since(start))
		}
		if p != nil {
			p.end(f)
		}
	}
}

package sqlitefile

import "io"

// Test-only access to internals. Panic injection reaches library code only
// through the instance-scoped env.hook set here (there is no package-level
// variable).

// ResolveLimits exposes Limits.withDefaults.
func ResolveLimits(l Limits) Limits { return l.withDefaults() }

// EnvBudget returns the budget an instance built from o would use.
func EnvBudget(o Options) Budget { return newEnv(o, nil).budget }

// EnvLimits returns the resolved limits of an instance built from o.
func EnvLimits(o Options) Limits { return newEnv(o, nil).opts.Limits }

// KnownWarningCodes returns the codes the collector accepts.
func KnownWarningCodes() []string {
	out := make([]string, 0, len(knownWarningCodes))
	for c := range knownWarningCodes {
		out = append(out, c)
	}
	return out
}

// TestWarnings wraps the unexported collector. Unknown codes panic, as the
// closed-set rule requires in tests (never in production).
type TestWarnings struct{ w *warnings }

// NewTestWarnings builds a strict collector with the given cap (0: default).
func NewTestWarnings(maxWarnings int) *TestWarnings {
	w := newWarnings(maxWarnings)
	w.unknown = func(code string) { panic("unknown warning code " + code) }
	return &TestWarnings{w}
}

// Add records a warning.
func (t *TestWarnings) Add(x Warning) { t.w.add(x) }

// Snapshot returns a copy of the recorded warnings.
func (t *TestWarnings) Snapshot() []Warning { return t.w.snapshot() }

// Probe is a stand-in for a library instance: it carries an env whose hook
// the test sets, and runs guarded.
type Probe struct{ env *env }

// NewProbeWithHook returns an instance whose hook runs at every site.
func NewProbeWithHook(hook func(site string)) *Probe {
	return &Probe{env: newEnv(Options{}, hook)}
}

// Run calls the hook for site inside the guard, as every exported method does.
func (p *Probe) Run(site string) (err error) {
	defer guard(&err)
	p.env.at(site)
	return nil
}

// GuardedCall runs fn under guard.
func GuardedCall(fn func() error) (err error) {
	defer guard(&err)
	return fn()
}

// RawPage reads page pgno of the database file as found.
func (d *DB) RawPage(pgno uint32) ([]byte, error) { return d.readRawPage(pgno) }

// OpenWithHook is Open with a panic-injection hook (called at every site).
func OpenWithHook(db io.ReaderAt, size int64, opts Options, hook func(site string)) (*DB, error) {
	return openWith(db, size, opts, hook)
}

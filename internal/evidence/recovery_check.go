package evidence

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits of a Recovery description (rule R2).
const (
	maxRecoveryExcluded = 256 // Excluded runs; the exact count is Alloc.ExcludedRuns
	maxRecoveryList     = 64  // Basis, Assumptions and Params entries
	maxRecoveryText     = 256 // bytes of one Basis, Assumptions or Params string
)

var (
	methodToken = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,63}$`)
	paramToken  = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

	recoveryScopes   = []string{"unallocated", "volume", "raw", "artifact"}
	recoveryContents = []string{"ok", "uniform", "type-match", "type-mismatch"}
)

// Check applies rule R1 (the class belongs to kind) and every R2 rule to r. It
// is pure and returns one string per problem, without an artifact prefix.
func (r *Recovery) Check(kind string) []string {
	if r == nil {
		return []string{"recovery: description is missing"}
	}
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf("recovery: "+format, a...)) }

	ci, known := LookupClass(r.Class)
	switch {
	case !known:
		add("class %q is not known", r.Class)
	case ci.Kind != kind:
		add("class %q belongs to kind %q, not %q", r.Class, ci.Kind, kind)
	}

	if !methodToken.MatchString(r.Method) {
		add("method %q is not a token", r.Method)
	} else if known && !slices.ContainsFunc(ci.MethodPrefixes, func(p string) bool { return strings.HasPrefix(r.Method, p) }) {
		add("method %q is not allowed for class %q", r.Method, r.Class)
	}

	switch c := r.Confidence; {
	case c == nil:
		if known && ci.ConfidenceRequired {
			add("confidence is required for class %q", r.Class)
		}
	case *c < 0 || *c > 100:
		add("confidence %d is outside 0..100", *c)
	case known && *c > ci.MaxConfidence:
		add("confidence %d is above the ceiling %d of class %q", *c, ci.MaxConfidence, r.Class)
	}

	if r.Algorithm == "" {
		add("algorithm is empty")
	}
	switch {
	case known && ci.Scope != "":
		// the scope of these classes comes from the class table, never from the stored field
		if r.Scope != ci.Scope {
			add("scope %q does not match %q, which class %q requires", r.Scope, ci.Scope, r.Class)
		}
	case r.Scope != "" && !slices.Contains(recoveryScopes, r.Scope):
		add("scope %q is not one of %s", r.Scope, strings.Join(recoveryScopes, ", "))
	case r.Scope != "" && known && !ci.ScopeRequired:
		// spec 5.2: the scope names what a carve searched; the classes that claim a file take none
		add("scope %q is not allowed for class %q (only a carved artifact takes a chosen scope)", r.Scope, r.Class)
	case r.Scope == "" && known && ci.ScopeRequired:
		add("scope is required for class %q", r.Class)
	}
	if r.Content != "" && !slices.Contains(recoveryContents, r.Content) {
		add("content %q is not one of %s", r.Content, strings.Join(recoveryContents, ", "))
	}

	out = append(out, r.checkJournal(ci, known)...)

	a := r.Alloc
	if a.Free < 0 || a.Allocated < 0 || a.Unknown < 0 || a.ExcludedRuns < 0 || a.ExcludedBytes < 0 {
		add("alloc values must not be negative")
	}
	if len(r.Excluded) > maxRecoveryExcluded {
		add("excluded has %d runs (at most %d)", len(r.Excluded), maxRecoveryExcluded)
	} else {
		for i, run := range r.Excluded {
			if run.Offset < 0 || run.Length <= 0 || run.Offset > math.MaxInt64-run.Length {
				add("excluded run %d is not valid (negative, empty, a hole or overflowing)", i)
			}
		}
	}
	out = append(out, checkRecoveryList("basis", r.Basis)...)
	out = append(out, checkRecoveryList("assumptions", r.Assumptions)...)
	out = append(out, checkRecoveryParams(r.Params)...)
	return out
}

// checkJournal: a journal reference is required for journal-block, optional
// for a deleted file found through the journal and for journal reports, and
// refused everywhere else.
func (r *Recovery) checkJournal(ci ClassInfo, known bool) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf("recovery: "+format, a...)) }
	if r.Journal == nil {
		if known && ci.JournalRequired {
			add("journal reference is required for class %q", r.Class)
		}
		return out
	}
	if known {
		switch r.Class {
		case ClassCarved, ClassSlack, ClassPostCheckpointFile:
			add("journal reference is not allowed for class %q", r.Class)
			return out
		}
	}
	if r.Journal.Region != "live" && r.Journal.Region != "stale" {
		add("journal reference region %q is not live or stale", r.Journal.Region)
	}
	return out
}

// recoveryTextProblem describes what is wrong with s, or "".
func recoveryTextProblem(s string) string {
	switch {
	case !utf8.ValidString(s):
		return "is not valid UTF-8"
	case strings.ContainsRune(s, 0):
		return "holds a NUL"
	case len(s) > maxRecoveryText:
		return fmt.Sprintf("is %d bytes (at most %d)", len(s), maxRecoveryText)
	}
	return ""
}

func checkRecoveryList(name string, list []string) []string {
	if len(list) > maxRecoveryList {
		return []string{fmt.Sprintf("recovery: %s has %d entries (at most %d)", name, len(list), maxRecoveryList)}
	}
	var out []string
	for i, s := range list {
		if p := recoveryTextProblem(s); p != "" {
			out = append(out, fmt.Sprintf("recovery: %s entry %d %s", name, i, p))
		}
	}
	return out
}

func checkRecoveryParams(params map[string]string) []string {
	if len(params) > maxRecoveryList {
		return []string{fmt.Sprintf("recovery: params has %d entries (at most %d)", len(params), maxRecoveryList)}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []string
	for _, k := range keys {
		if !paramToken.MatchString(k) {
			out = append(out, fmt.Sprintf("recovery: params key %q is not a token", k))
			continue
		}
		if p := recoveryTextProblem(params[k]); p != "" {
			out = append(out, fmt.Sprintf("recovery: params value of %q %s", k, p))
		}
	}
	return out
}

package parse

import "slices"

// Clone returns a deep copy of the Meta: no slice shares memory with the
// original, so neither side can change the other. nil and empty slices stay
// as they were.
func (m Meta) Clone() Meta {
	c := m
	c.Platforms = slices.Clone(m.Platforms)
	c.Emits = slices.Clone(m.Emits)
	c.Claims = slices.Clone(m.Claims)
	if m.Inputs != nil {
		c.Inputs = make([]InputSpec, len(m.Inputs))
		for i, in := range m.Inputs {
			in.Globs = slices.Clone(in.Globs)
			in.Companions = slices.Clone(in.Companions)
			c.Inputs[i] = in
		}
	}
	return c
}

// clone returns a deep copy of the artifact's own values (Recovery, Source and
// its Snapshot). R is the invocation's single sealed reader and is shared.
func (a Artifact) clone() Artifact {
	if a.Recovery != nil {
		r := *a.Recovery
		if r.Confidence != nil {
			v := *r.Confidence
			r.Confidence = &v
		}
		a.Recovery = &r
	}
	if a.Source.Snapshot != nil {
		s := *a.Source.Snapshot
		a.Source.Snapshot = &s
	}
	return a
}

// Clone returns the copy of the Input the host hands to a parser: deep over
// Artifacts, Recovery, Source and Snapshot, so a parser that mutates what it
// holds cannot change the host's tables or another invocation's. Primary and
// the entries of Artifacts are copied separately. The readers, the Lookuper and
// the BudgetView are the invocation's single handles and are shared (a second
// BudgetView would carry its own outstanding amount). A nil Input clones to nil.
func (in *Input) Clone() *Input {
	if in == nil {
		return nil
	}
	c := *in
	c.Primary = in.Primary.clone()
	if in.Artifacts != nil {
		c.Artifacts = make(map[string]Artifact, len(in.Artifacts))
		for role, a := range in.Artifacts {
			c.Artifacts[role] = a.clone()
		}
	}
	return &c
}

package artparse

import (
	"context"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Claimed is one table a registered parser claims, with the identity of that parser build.
type Claimed struct{ Table, Parser, Version, Hash string }

// CheckClaims requires a parser that claims tables to be a parse.RowMapper whose mappings handle every
// claimed table (a claim is a promise the host can check). A parser without claims passes.
func (r Registered) CheckClaims() (err error) {
	if len(r.meta.Claims) == 0 {
		return nil
	}
	rm, ok := r.p.(parse.RowMapper)
	if !ok {
		return fmt.Errorf("parser %s: declares table claims but is not a parse.RowMapper", r.meta.Name)
	}
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("parser %s: Mappings() panicked: %s", r.meta.Name, cleanAuditText(panicText(rec), maxReason))
		}
	}()
	out := parse.ClaimsOutsideMappings(r.meta, rm.Mappings())
	if len(out) == 0 {
		return nil
	}
	tables := make([]string, len(out))
	for i, c := range out {
		tables[i] = c.Table
	}
	return fmt.Errorf("parser %s: claims tables its mappings do not handle: %s", r.meta.Name, strings.Join(tables, ", "))
}

// ClaimedTables returns, for one artifact, the tables claimed by registered parsers whose primary glob
// matches its logical path and whose Probe says "applicable". It is empty in 4A for every real
// registry (no parser has claims); it is implemented so that 4G only enables claims. A parser whose
// claims are not backed by its mappings is an error, never a claim.
func (h *Host) ClaimedTables(ctx context.Context, artifactID string) ([]Claimed, error) {
	snap, err := h.TakeSnapshot()
	if err != nil {
		return nil, err
	}
	if _, ok := snap.Record(artifactID); !ok {
		return nil, fmt.Errorf("%w: %q", evidence.ErrUnknownArtifact, artifactID)
	}
	var refs []ParserRef
	for _, r := range h.registry {
		if len(r.meta.Claims) == 0 {
			continue
		}
		if err := r.CheckClaims(); err != nil {
			return nil, err
		}
		refs = append(refs, ParserRef{Name: r.meta.Name, Version: r.meta.Version})
	}
	if len(refs) == 0 {
		return nil, nil
	}
	jobs, _, err := h.Discover(ctx, snap, Selection{Parsers: refs, IncludeSnapshots: true})
	if err != nil {
		return nil, err
	}
	var out []Claimed
	for _, j := range jobs {
		if j.Primary.Artifact.ID != artifactID {
			continue
		}
		pr, err := h.prepareJob(ctx, snap, j, "claims")
		if err != nil {
			return nil, err
		}
		if pr.bundle == nil {
			continue // Probe did not say applicable (or the job was refused or unparsed)
		}
		pr.bundle.close()
		id := j.Parser.Identity()
		for _, c := range j.Parser.meta.Claims {
			out = append(out, Claimed{Table: c.Table, Parser: id.Name, Version: id.Version, Hash: id.Hash})
		}
	}
	return out, nil
}

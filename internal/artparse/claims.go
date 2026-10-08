package artparse

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Claimed is one table a registered parser claims, with the identity of that parser build.
type Claimed struct {
	Table, Role, Parser, Version, Hash string
	Unparsed                           bool   // the artifact is not parseable now (Reason says why); the claim is the parser's all the same
	Reason                             string // why, for an unparsed claim
}

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

// foldTable is the ASCII case fold SQLite compares table names with.
func foldTable(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// checkClaimUniqueness requires every table to be claimed once across the registry, compared
// case-insensitively (sms and SMS are one table): two claims on one table would make the owner of its
// rows ambiguous.
func checkClaimUniqueness(regs []Registered) error {
	owner := map[string]string{}
	for _, r := range regs {
		for _, c := range r.meta.Claims {
			k := foldTable(c.Table)
			if other, dup := owner[k]; dup {
				return fmt.Errorf("artparse: parsers %q and %q both claim table %q", other, r.meta.Name, c.Table)
			}
			owner[k] = r.meta.Name
		}
	}
	return nil
}

// rolesOf lists the roles artifact id fills in job j (the primary's, and every other role it matched).
func rolesOf(j Job, id string) []string {
	var roles []string
	if j.Primary.Artifact.ID == id {
		roles = append(roles, j.Parser.meta.Inputs[0].Role)
	}
	for role, m := range j.Others {
		if m.Artifact.ID == id {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}

// ClaimedTables returns, for one artifact, the tables registered parsers claim for the role the
// artifact fills in a job (its primary glob matches the artifact, or the artifact is one of the job's
// companions). It is empty in 4A for every real registry (no parser has claims); it is implemented so
// that 4G only enables claims. Nothing is dropped silently: a parser whose claims are not backed by its
// mappings or two parsers claiming one table are errors; an artifact that does not match the manifest is
// evidence.ErrIntegrity; a claim of a job that cannot be parsed now (encrypted, unsupported schema, ...)
// is returned marked Unparsed with its Reason, because the claim is the parser's whatever the state of
// the artifact. Only a parser whose Probe says "not applicable" makes no claim.
func (h *Host) ClaimedTables(ctx context.Context, artifactID string) ([]Claimed, error) {
	snap, err := h.TakeSnapshot()
	if err != nil {
		return nil, err
	}
	if _, ok := snap.Record(artifactID); !ok {
		return nil, fmt.Errorf("%w: %q", evidence.ErrUnknownArtifact, artifactID)
	}
	if err := checkClaimUniqueness(h.registry); err != nil {
		return nil, err
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
		roles := rolesOf(j, artifactID)
		if len(roles) == 0 {
			continue
		}
		pr, err := h.prepareJob(ctx, snap, j, "claims")
		if err != nil {
			return nil, err
		}
		if pr.integrity {
			return nil, fmt.Errorf("claims of artifact %s: %w", artifactID, integrityf("%s", strings.TrimPrefix(pr.reason, "integrity: ")))
		}
		unparsed, reason := false, ""
		switch {
		case pr.bundle != nil:
			pr.bundle.close()
		case pr.status == StatusSkipped:
			continue // the parser says this is not its format: it makes no claim
		default:
			unparsed, reason = true, pr.status+": "+pr.reason
			if pr.status == StatusUnparsed {
				reason = pr.reason
			}
		}
		id := j.Parser.Identity()
		for _, c := range j.Parser.meta.Claims {
			if slices.Contains(roles, c.Role) {
				out = append(out, Claimed{Table: c.Table, Role: c.Role, Parser: id.Name, Version: id.Version, Hash: id.Hash, Unparsed: unparsed, Reason: reason})
			}
		}
	}
	return out, nil
}

package artparse

import (
	"context"
	"fmt"
	"path"
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
	// RecoverParser and RecoverHash are the identity the typed recovery pass records this claim's rows
	// under ("<Parser>.recover", same version).
	RecoverParser, RecoverHash string
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

// checkClaimUniqueness requires every table to be claimed once per platform across the registry, compared
// case-insensitively (sms and SMS are one table): two claims on one table of one platform would make the owner of its
// rows ambiguous. The rule is per platform (4I-R23): an artifact's logical path has one scheme, ClaimedTables considers
// only the jobs whose primary glob matched it, and a primary glob lies inside Meta.Platforms, so parsers of different
// platforms never claim on one artifact. A parser declaring both platforms counts on both. As amended by 4I4-T10 the
// rule is also per possible artifact: two claims on one table of one platform do not collide when their files can never
// be one artifact, decided conservatively from the final segments of the globs of the claims' roles (claimsDisjoint);
// equal names, a pattern that can match the other's name, two patterns or a role without globs still collide. checkArtifactClaims backs this up per artifact, unchanged.
func checkClaimUniqueness(regs []Registered) error {
	type key struct{ platform, table string }
	owner := map[key][]Registered{}
	for _, r := range regs {
		for _, c := range r.meta.Claims {
			for _, platform := range r.meta.Platforms {
				k := key{platform, foldTable(c.Table)}
				for _, other := range owner[k] {
					if !claimsDisjoint(other, r, c.Role) {
						return fmt.Errorf("artparse: parsers %q and %q both claim table %q on platform %q", other.meta.Name, r.meta.Name, c.Table, platform)
					}
				}
				owner[k] = append(owner[k], r)
			}
		}
	}
	return nil
}

// claimsDisjoint says the files that parsers a and b read in the role of a claim (b's) can never be one artifact: for
// every pair of globs of that role (one from each parser) the final path segments are provably different under
// foldTable (segmentsDisjoint). A role either parser lacks, or one without globs, is not provably disjoint (4I4-T10).
func claimsDisjoint(a, b Registered, role string) bool {
	ga, gb := roleGlobs(a.meta, role), roleGlobs(b.meta, role)
	if len(ga) == 0 || len(gb) == 0 {
		return false
	}
	for _, x := range ga {
		for _, y := range gb {
			if !segmentsDisjoint(finalSegment(x), finalSegment(y)) {
				return false
			}
		}
	}
	return true
}

func roleGlobs(m parse.Meta, role string) []string {
	for _, in := range m.Inputs {
		if in.Role == role {
			return in.Globs
		}
	}
	return nil
}

// finalSegment is the part of glob after its last '/' (after the scheme's ':' when there is no '/').
func finalSegment(glob string) string {
	if i := strings.LastIndexByte(glob, '/'); i >= 0 {
		return glob[i+1:]
	}
	if i := strings.IndexByte(glob, ':'); i >= 0 {
		return glob[i+1:]
	}
	return glob
}

// segmentsDisjoint is true only when no file name can match both final segments. Two literals (no glob
// metacharacter) are disjoint when they differ under foldTable; a literal and a pattern when the pattern does not match
// the literal (folded). Two patterns, an empty segment and any brace or escape syntax are not decided: they collide.
func segmentsDisjoint(x, y string) bool {
	const meta = "*?[]{}\\"
	if x == "" || y == "" || strings.ContainsAny(x, "{}\\") || strings.ContainsAny(y, "{}\\") {
		return false
	}
	lx, ly := !strings.ContainsAny(x, meta), !strings.ContainsAny(y, meta)
	fx, fy := foldTable(x), foldTable(y)
	switch {
	case lx && ly:
		return fx != fy
	case lx:
		ok, err := path.Match(fy, fx)
		return err == nil && !ok
	case ly:
		ok, err := path.Match(fx, fy)
		return err == nil && !ok
	}
	return false
}

// checkArtifactClaims is the defence in depth of ClaimedTables: whatever the registry check let through, two parsers'
// claims on one folded table of one artifact are an error. Never one picked, never one dropped.
func checkArtifactClaims(artifactID string, out []Claimed) error {
	owner := map[string]string{}
	for _, c := range out {
		k := foldTable(c.Table)
		if other, dup := owner[k]; dup && other != c.Parser {
			return fmt.Errorf("artparse: artifact %s: parsers %q and %q both claim table %q", artifactID, other, c.Parser, c.Table)
		}
		owner[k] = c.Parser
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
	rec, ok := snap.Record(artifactID)
	if !ok {
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
	// Backup files are named by the Manifest.db of their backup (4E-R38): resolve before discovery. A
	// tampered Manifest.db of the artifact's own backup is an integrity failure, not a missing claim, and
	// a cancel never hides it.
	rs, rerr := h.ResolveBackups(ctx, snap)
	if acq, dir, ok := backupOf(rec); ok {
		for _, r := range rs {
			if r.Acquisition == acq && r.Dir == dir && r.Integrity {
				return nil, fmt.Errorf("claims of artifact %s: %w", artifactID, integrityf("%s", strings.TrimPrefix(r.Reason, "integrity: ")))
			}
		}
	}
	if rerr != nil {
		return nil, rerr
	}
	// A backup file no Manifest.db could name may still be a claimed table: say so (unparsed), never "no claim".
	if reason := unnamedBackupFileReason(snap, rec); reason != "" {
		var out []Claimed
		// A backup is an iOS acquisition: only the claims of parsers of that platform are its claims (4I-R23).
		for _, r := range h.registry {
			if !slices.Contains(r.meta.Platforms, parse.PlatformIOS) {
				continue
			}
			id := r.Identity()
			for _, c := range r.meta.Claims {
				out = append(out, Claimed{Table: c.Table, Role: c.Role, Parser: id.Name, Version: id.Version, Hash: id.Hash, Unparsed: true, Reason: reason})
			}
		}
		return out, nil
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
		rid, _ := j.Parser.RecoveryIdentity()
		for _, c := range j.Parser.meta.Claims {
			if slices.Contains(roles, c.Role) {
				out = append(out, Claimed{
					Table: c.Table, Role: c.Role, Parser: id.Name, Version: id.Version, Hash: id.Hash, Unparsed: unparsed, Reason: reason,
					RecoverParser: rid.Name, RecoverHash: rid.Hash,
				})
			}
		}
	}
	if err := checkArtifactClaims(artifactID, out); err != nil {
		return nil, err
	}
	return out, nil
}

package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// VerifyReport is the outcome of Case.Verify.
type VerifyReport struct {
	AuditEntries     int `json:"audit_entries"`
	ArtifactsChecked int `json:"artifacts_checked"`
	// RecordsChecked, RecordBatchesChecked and RecordRunsChecked count what the
	// record checks of a schema v2 case covered (0 for a v1 case).
	RecordsChecked       int `json:"records_checked"`
	RecordBatchesChecked int `json:"record_batches_checked"`
	RecordRunsChecked    int `json:"record_runs_checked"`
	// FTSDocsChecked is the number of documents of the word index for which the full-text check (P11) ran its comparison to the end (the expected side complete, every table read, no segment that fails to decode, every failing bucket localized). It does not mean they passed: the problems say what differs.
	FTSDocsChecked int `json:"fts_docs_checked"`
	// RecoveredArtifacts counts the artifacts of the five recovered kinds (set by the built-in
	// check); RecoveredReproduced those a composed check reproduced byte for byte from the image.
	RecoveredArtifacts  int      `json:"recovered_artifacts"`
	RecoveredReproduced int      `json:"recovered_reproduced"`
	Problems            []string `json:"problems"`
	// Notices are findings that are not integrity problems (for example an
	// announced upgrade that was never concluded); never silent.
	Notices []string `json:"notices"`

	reproduceRan    bool // R6 ran in this verify (MarkReproduceRan)
	cancelCut       bool // a phase was cut short by the end of the verify context
	recoveredByKind map[string]int
}

// OK reports whether no integrity problems were found.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

func (r *VerifyReport) problemf(format string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
}

// AddProblem appends an integrity problem; composed checks use it to report their findings.
func (r *VerifyReport) AddProblem(format string, a ...any) { r.problemf(format, a...) }

// AddNotice appends a notice (a finding that is not an integrity problem).
func (r *VerifyReport) AddNotice(format string, a ...any) { r.noticef(format, a...) }

func (r *VerifyReport) noticef(format string, a ...any) {
	r.Notices = append(r.Notices, fmt.Sprintf(format, a...))
}

// Verify re-hashes every artifact, checks the audit chain, and cross-checks
// the manifest (including each record's full source/provenance) against the audit log's artifact.create entries, artifacts.db
// and the artifacts directory, and flags leftover (unpromoted) staging
// directories. Every failure to read part of the case is a
// reported problem, so Verify always completes; the result is audited
// (verify.run). The returned error is only the failure to audit the result.
func (c *Case) Verify() (VerifyReport, error) { return c.VerifyWith(context.Background()) }

// VerifyWith is Verify plus checks composed by a layer above this package (for example the
// byte-level reproduction of recovered artifacts, which needs the parsers this package must not
// import). The built-in checks run first, then checks in order, each under a recover(); the
// verify.run audit entry is written last and covers their findings. A context that ends before
// or during a check makes the report carry "reproduce: verification cancelled" once.
func (c *Case) VerifyWith(ctx context.Context, checks ...VerifyCheck) (VerifyReport, error) {
	return c.verifyRun(ctx, nil, nil, checks)
}

// verify is the test seam: the built-in checks with a chunk observer.
func (c *Case) verify(observe verifyChunkObserver) (VerifyReport, error) {
	return c.verifyRun(context.Background(), observe, nil, nil)
}

func (c *Case) verifyRun(ctx context.Context, observe verifyChunkObserver, p *VerifyProgress, checks []VerifyCheck) (VerifyReport, error) {
	c.verifyProgress = p
	defer func() { c.verifyProgress = nil }()
	rep := VerifyReport{Problems: []string{}, Notices: []string{}}
	doneAudit := c.phase("audit")
	n, audited, auditProblems, err := verifyAudit(filepath.Join(c.Dir, auditFile))
	doneAudit()
	if err != nil {
		rep.problemf("audit log unreadable: %v", err)
	}
	auditReadable := err == nil
	rep.AuditEntries = n
	rep.Problems = append(rep.Problems, auditProblems...)

	recs, err := c.Manifest()
	if err != nil {
		rep.problemf("manifest unreadable: %v", err)
	}
	inManifest := map[string]bool{}
	seenID := map[string]bool{}
	doneArtifacts, addBytes := c.countedPhase("artifacts", artifactBytesTotal(recs), "bytes")
	c.hashArtifacts(ctx, &rep, recs, inManifest, seenID, addBytes)
	doneArtifacts()

	c.checkDerived(&rep, recs)
	c.crossCheckAudit(&rep, recs, audited)
	c.crossCheckDB(&rep, recs)
	c.checkTmp(&rep)
	c.verifyRecovered(&rep, recs)
	c.verifyRecords(ctx, &rep, recs, audited, auditReadable, observe)
	if auditReadable {
		c.checkSchema(&rep, audited)
	}
	c.checkUnmanifested(&rep, inManifest)
	c.checkStaging(&rep)
	if len(checks) > 0 {
		doneChecks := c.phase("checks") // so the display never shows a finished phase while a composed check works
		c.runComposedChecks(ctx, &rep, recs, checks)
		doneChecks()
	}
	if rep.cancelCut && !slices.Contains(rep.Problems, verifyCancelled) {
		rep.problemf("%s", verifyCancelled) // a verify cut short is never a pass, whichever phase it ended in
	}
	unreproducedNotices(&rep)

	_, err = c.Audit.Append("verify.run", "", map[string]any{
		"ok": rep.OK(), "artifacts_checked": rep.ArtifactsChecked,
		"audit_entries": rep.AuditEntries, "problems": len(rep.Problems),
		"records_checked": rep.RecordsChecked, "record_batches_checked": rep.RecordBatchesChecked,
		"record_runs_checked": rep.RecordRunsChecked, "fts_docs_checked": rep.FTSDocsChecked,
		"recovered_artifacts": rep.RecoveredArtifacts, "recovered_reproduced": rep.RecoveredReproduced,
	})
	return rep, err
}

// checkSchema (P13) requires the database's schema version to be the version
// the audit log says it has: case.create's schema_version (1 when absent), then
// every case.upgrade.done. A database migrated outside the audited upgrade is a
// problem. An announced upgrade without a conclusion is a notice: the audit
// entry was committed before the migration, so a database at either the old or
// the announced version is explained (case upgrade concludes it).
func (c *Case) checkSchema(rep *VerifyReport, entries []AuditEntry) {
	sa := auditedSchema(entries)
	for _, p := range sa.Problems {
		rep.problemf("%s", p)
	}
	dbv, err := c.store.SchemaVersion()
	if err != nil {
		rep.problemf("artifacts.db schema version unreadable: %v", err)
		return
	}
	d := sa.Dangling
	switch {
	case dbv == sa.Version:
		if d != nil {
			rep.noticef("audit seq %d: case.upgrade (schema v%d -> v%d) was announced but never concluded; the database is still at v%d (run: minutiae case upgrade)", d.Seq, d.From, d.To, dbv)
		}
	case d != nil && dbv == d.To && sa.Version == d.From:
		rep.noticef("audit seq %d: case.upgrade (schema v%d -> v%d) was announced and the database is at v%d, but case.upgrade.done was never audited (run: minutiae case upgrade)", d.Seq, d.From, d.To, dbv)
	default:
		rep.problemf("artifacts.db schema version %d does not match the audited schema version %d (the database was changed outside an audited upgrade, or the audit log was altered)", dbv, sa.Version)
	}
}

// auditedArtifact is the part of an artifact.create audit entry that must
// agree with the manifest record of the same id.
type auditedArtifact struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	MD5        string `json:"md5"`
	Incomplete bool   `json:"incomplete"`
}

// crossCheckAudit compares the manifest with the hash-chained audit log in
// both directions, so a forger who rewrites an artifact, its manifest line
// and its artifacts.db row consistently — or erases all three — is caught.
func (c *Case) crossCheckAudit(rep *VerifyReport, recs []ManifestRecord, entries []AuditEntry) {
	audited := map[string]auditedArtifact{}
	auditedSource := map[string]any{}
	var order []string
	for _, e := range entries {
		if e.Action != "artifact.create" {
			continue
		}
		id, a, err := decodeAuditedArtifact(e.Details)
		if err != nil {
			rep.problemf("audit seq %d: artifact.create details unreadable: %v", e.Seq, err)
			continue
		}
		if _, dup := audited[id]; dup {
			rep.problemf("artifact %s: recorded by more than one artifact.create audit entry", id)
			continue
		}
		audited[id] = a
		auditedSource[id] = e.Details["source"]
		order = append(order, id)
	}
	inManifest := map[string]bool{}
	for _, r := range recs {
		if inManifest[r.ID] {
			continue // duplicate id: already reported
		}
		inManifest[r.ID] = true
		a, ok := audited[r.ID]
		if !ok {
			rep.problemf("artifact %s: in manifest but has no artifact.create audit entry", r.ID)
			continue
		}
		switch auditDiff(r, a, auditedSource[r.ID]) {
		case "":
		case "source":
			rep.problemf("artifact %s (%s): source differs from audit (the manifest provenance does not match its artifact.create audit entry)", r.ID, r.Path)
		default:
			rep.problemf("artifact %s: manifest record (path=%s size=%d sha256=%s incomplete=%t) does not match its artifact.create audit entry (path=%s size=%d sha256=%s incomplete=%t)",
				r.ID, r.Path, r.Size, r.SHA256, r.Incomplete, a.Path, a.Size, a.SHA256, a.Incomplete)
		}
	}
	for _, id := range order {
		if !inManifest[id] {
			rep.problemf("artifact %s: in audit log (artifact.create) but not in manifest (path=%s)", id, audited[id].Path)
		}
	}
}

// sourceMatchesAudit compares a manifest record's full provenance with the
// source object of its artifact.create audit entry. Both sides are reduced to
// generic JSON values first, so key order and number formatting do not matter
// but every field (derivation, segments, original path, ...) does.
func sourceMatchesAudit(src Source, audited any) bool {
	b, err := json.Marshal(src)
	if err != nil {
		return false
	}
	// UseNumber, like the audit decoder, so integers compare as text, exactly.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var want any
	if err := dec.Decode(&want); err != nil {
		return false
	}
	return reflect.DeepEqual(want, audited)
}

func decodeAuditedArtifact(details map[string]any) (string, auditedArtifact, error) {
	var a auditedArtifact
	id, _ := details["id"].(string)
	if id == "" {
		return "", a, errors.New("missing id")
	}
	b, err := json.Marshal(details)
	if err != nil {
		return "", a, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return "", a, err
	}
	return id, a, nil
}

// crossCheckDB compares the manifest with artifacts.db in both directions.
func (c *Case) crossCheckDB(rep *VerifyReport, recs []ManifestRecord) {
	dbHashes, err := c.store.ArtifactHashes()
	if err != nil {
		rep.problemf("artifacts.db unreadable: %v", err)
		return
	}
	checked := map[string]bool{}
	for _, r := range recs {
		if checked[r.ID] {
			continue // duplicate id: already reported
		}
		checked[r.ID] = true
		h, ok := dbHashes[r.ID]
		switch {
		case !ok:
			rep.problemf("artifact %s: in manifest but not in artifacts.db", r.ID)
		case h != r.SHA256:
			rep.problemf("artifact %s: artifacts.db sha256 differs from manifest", r.ID)
		}
	}
	extra := make([]string, 0, len(dbHashes))
	for id := range dbHashes {
		if !checked[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		rep.problemf("artifact %s: in artifacts.db but not in manifest", id)
	}
}

// checkUnmanifested reports every file under artifacts/ that no manifest
// record names, and any part of the tree that cannot be read.
func (c *Case) checkUnmanifested(rep *VerifyReport, inManifest map[string]bool) {
	root := filepath.Join(c.Dir, artifactsDir)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			rep.problemf("artifacts directory unreadable at %s: %v", p, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(c.Dir, p)
		if err != nil {
			rep.problemf("artifacts directory: %v", err)
			return nil
		}
		if !inManifest[filepath.ToSlash(rel)] {
			rep.problemf("unmanifested file %s", filepath.ToSlash(rel))
		}
		return nil
	})
}

// checkStaging reports every entry left in <case>/staging/: an iOS backup
// working directory survives only when its files could not all be promoted
// into artifacts, so it holds acquisition data the manifest does not cover.
func (c *Case) checkStaging(rep *VerifyReport) {
	entries, err := os.ReadDir(filepath.Join(c.Dir, stagingDir))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		rep.problemf("staging directory unreadable: %v", err)
	}
	for _, e := range entries {
		rep.problemf("leftover staging directory %s (unpromoted acquisition data)", e.Name())
	}
}

// checkDerived requires, for every record with a Derivation, that its parent
// artifact exists in the manifest with the recorded SHA-256, that its runs
// sidecar artifact (when named) exists, and that its chain of derivations ends
// (no cycle, at most maxDerivedDepth hops).
func (c *Case) checkDerived(rep *VerifyReport, recs []ManifestRecord) {
	byID := make(map[string]ManifestRecord, len(recs))
	for _, r := range recs {
		if _, dup := byID[r.ID]; !dup {
			byID[r.ID] = r
		}
	}
	chainProblems := 0
	cycles := map[string]*cycleGroup{}
	for _, r := range recs {
		d := r.Source.Derived
		if d == nil {
			continue
		}
		parent, ok := byID[d.ParentID]
		switch {
		case !ok:
			rep.problemf("artifact %s (%s): derived from parent %q, which is not in the manifest", r.ID, r.Path, d.ParentID)
		case parent.SHA256 != d.ParentSHA256:
			rep.problemf("artifact %s (%s): parent %s sha256 %s differs from the derivation's recorded parent sha256 %s",
				r.ID, r.Path, d.ParentID, parent.SHA256, d.ParentSHA256)
		}
		for i, sg := range d.ParentSegments {
			seg, ok := byID[sg.ID]
			switch {
			case !ok:
				rep.problemf("artifact %s (%s): parent segment %d (%q) of %s is not in the manifest", r.ID, r.Path, i+1, sg.ID, d.ParentID)
			case seg.SHA256 != sg.SHA256:
				rep.problemf("artifact %s (%s): parent segment %d (%s) sha256 %s differs from the derivation's recorded sha256 %s",
					r.ID, r.Path, i+1, sg.ID, seg.SHA256, sg.SHA256)
			}
		}
		if d.RunsArtifact != "" {
			if _, ok := byID[d.RunsArtifact]; !ok {
				rep.problemf("artifact %s (%s): runs artifact %q (of parent %s) is not in the manifest", r.ID, r.Path, d.RunsArtifact, d.ParentID)
			}
		}
		switch w := derivedChainWalk(byID, r.ID); {
		case w.TooDeep:
			chainProblems++
			if chainProblems <= verifyMaxPerKind {
				rep.problemf("artifact %q (%q): derived chain is longer than %d hops", r.ID, r.Path, maxDerivedDepth)
			}
		case w.Cycle != nil:
			key := strings.Join(w.Cycle, "\x00")
			g := cycles[key]
			if g == nil {
				g = &cycleGroup{members: w.Cycle}
				cycles[key] = g
			}
			if !slices.Contains(w.Cycle, r.ID) {
				g.further++
			}
		}
	}
	groups := make([]*cycleGroup, 0, len(cycles))
	for _, g := range cycles {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b *cycleGroup) int { return strings.Compare(a.members[0], b.members[0]) })
	for _, g := range groups {
		rep.problemf("%s", cycleProblem(g))
	}
	if chainProblems > verifyMaxPerKind {
		rep.problemf("%d further derived chain problems are not listed", chainProblems-verifyMaxPerKind)
	}
}

// maxDerivedDepth is the longest chain of derivations (artifact, its parent, the
// parent's parent, ...) verify accepts: real chains are one or two hops, so a
// longer one is a damaged or hostile manifest.
const maxDerivedDepth = 16

// VerifyCheck is a check composed into VerifyWith by a layer above this package. It reports through
// rep.AddProblem and rep.AddNotice and may set rep.RecoveredReproduced; recs is the manifest.
type VerifyCheck func(ctx context.Context, c *Case, recs []ManifestRecord, rep *VerifyReport)

// verifyCancelled is the problem of a verify whose context ended: a cancelled verify is never OK.
const verifyCancelled = "reproduce: verification cancelled"

// runComposedChecks runs checks in order after the built-in ones. A panic in a check is a problem
// (the later checks still run); a context that ended before or during a check adds
// verifyCancelled once and skips the checks not yet started.
func (c *Case) runComposedChecks(ctx context.Context, rep *VerifyReport, recs []ManifestRecord, checks []VerifyCheck) {
	for _, check := range checks {
		if ctx.Err() != nil {
			break
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					rep.problemf("verify check panicked: %v", p)
				}
			}()
			check(ctx, c, recs, rep)
		}()
	}
	if len(checks) > 0 && ctx.Err() != nil {
		rep.problemf("%s", verifyCancelled)
	}
}

// RecoveredSummary is the part of the one-line verify summary that names the recovered
// artifacts: ", N recovered (M reproduced)" when there are any, else "".
func (r VerifyReport) RecoveredSummary() string {
	if r.RecoveredArtifacts == 0 {
		return ""
	}
	return fmt.Sprintf(", %d recovered (%d reproduced)", r.RecoveredArtifacts, r.RecoveredReproduced)
}

// MarkReproduceRan is called by the composed reproduce check (R6) when it ran, so the built-in
// check stops noticing that recover and carve artifacts are not reproduced by this build.
func (r *VerifyReport) MarkReproduceRan() { r.reproduceRan = true }

// CancelledOnly reports whether the only problem is that the verify was cut short by a cancel: no
// integrity finding was made, so the caller may treat it as a cancel and not as an integrity failure.
func (r VerifyReport) CancelledOnly() bool {
	return len(r.Problems) == 1 && r.Problems[0] == verifyCancelled
}

// hashArtifacts re-hashes every manifest artifact (phase "artifacts") and compares the result with the
// manifest. It stops hashing when ctx ends: the remaining artifacts stay in inManifest (they are in the
// manifest, only not checked) and the report is marked cut, so verifyRun adds the cancelled line.
func (c *Case) hashArtifacts(ctx context.Context, rep *VerifyReport, recs []ManifestRecord, inManifest, seenID map[string]bool, addBytes func(int64)) {
	for _, r := range recs {
		inManifest[r.Path] = true
		if seenID[r.ID] {
			rep.problemf("artifact %s: duplicate manifest id (%s)", r.ID, r.Path)
		}
		seenID[r.ID] = true
		if rep.cancelCut || ctx.Err() != nil {
			rep.cancelCut = true // the rest is not hashed: the artifact is still in the manifest, just not checked
			continue
		}
		d, err := hashFileContext(ctx, filepath.Join(c.Dir, filepath.FromSlash(r.Path)), addBytes)
		if ctxCaused(ctx, err) {
			rep.cancelCut = true
			continue
		}
		rep.ArtifactsChecked++
		if err != nil {
			rep.problemf("artifact %s (%s): %v", r.ID, r.Path, err)
			continue
		}
		if d.Size != r.Size || d.SHA256 != r.SHA256 || d.MD5 != r.MD5 {
			rep.problemf("artifact %s (%s): hash mismatch: manifest sha256=%s size=%d, file sha256=%s size=%d",
				r.ID, r.Path, r.SHA256, r.Size, d.SHA256, d.Size)
		}
	}
}

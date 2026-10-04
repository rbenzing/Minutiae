package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

// VerifyReport is the outcome of Case.Verify.
type VerifyReport struct {
	AuditEntries     int      `json:"audit_entries"`
	ArtifactsChecked int      `json:"artifacts_checked"`
	Problems         []string `json:"problems"`
	// Notices are findings that are not integrity problems (for example an
	// announced upgrade that was never concluded); never silent.
	Notices []string `json:"notices"`
}

// OK reports whether no integrity problems were found.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

func (r *VerifyReport) problemf(format string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
}

func (r *VerifyReport) noticef(format string, a ...any) {
	r.Notices = append(r.Notices, fmt.Sprintf(format, a...))
}

// Verify re-hashes every artifact, checks the audit chain, and cross-checks
// the manifest (including each record's full source/provenance) against the audit log's artifact.create entries, artifacts.db
// and the artifacts directory, and flags leftover (unpromoted) staging
// directories. Every failure to read part of the case is a
// reported problem, so Verify always completes; the result is audited
// (verify.run). The returned error is only the failure to audit the result.
func (c *Case) Verify() (VerifyReport, error) {
	rep := VerifyReport{Problems: []string{}, Notices: []string{}}
	n, audited, auditProblems, err := verifyAudit(filepath.Join(c.Dir, auditFile))
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
	for _, r := range recs {
		rep.ArtifactsChecked++
		inManifest[r.Path] = true
		if seenID[r.ID] {
			rep.problemf("artifact %s: duplicate manifest id (%s)", r.ID, r.Path)
		}
		seenID[r.ID] = true
		d, err := HashFile(filepath.Join(c.Dir, filepath.FromSlash(r.Path)))
		if err != nil {
			rep.problemf("artifact %s (%s): %v", r.ID, r.Path, err)
			continue
		}
		if d.Size != r.Size || d.SHA256 != r.SHA256 || d.MD5 != r.MD5 {
			rep.problemf("artifact %s (%s): hash mismatch: manifest sha256=%s size=%d, file sha256=%s size=%d",
				r.ID, r.Path, r.SHA256, r.Size, d.SHA256, d.Size)
		}
	}

	c.checkDerived(&rep, recs)
	c.crossCheckAudit(&rep, recs, audited)
	c.crossCheckDB(&rep, recs)
	if auditReadable {
		c.checkSchema(&rep, audited)
	}
	c.checkUnmanifested(&rep, inManifest)
	c.checkStaging(&rep)

	_, err = c.Audit.Append("verify.run", "", map[string]any{
		"ok": rep.OK(), "artifacts_checked": rep.ArtifactsChecked,
		"audit_entries": rep.AuditEntries, "problems": len(rep.Problems),
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
		switch {
		case !ok:
			rep.problemf("artifact %s: in manifest but has no artifact.create audit entry", r.ID)
		case a != (auditedArtifact{Path: r.Path, Size: r.Size, SHA256: r.SHA256, MD5: r.MD5, Incomplete: r.Incomplete}):
			rep.problemf("artifact %s: manifest record (path=%s size=%d sha256=%s incomplete=%t) does not match its artifact.create audit entry (path=%s size=%d sha256=%s incomplete=%t)",
				r.ID, r.Path, r.Size, r.SHA256, r.Incomplete, a.Path, a.Size, a.SHA256, a.Incomplete)
		case !sourceMatchesAudit(r.Source, auditedSource[r.ID]):
			rep.problemf("artifact %s (%s): source differs from audit (the manifest provenance does not match its artifact.create audit entry)", r.ID, r.Path)
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
// artifact exists in the manifest with the recorded SHA-256 and that its runs
// sidecar artifact (when named) exists.
func (c *Case) checkDerived(rep *VerifyReport, recs []ManifestRecord) {
	byID := make(map[string]ManifestRecord, len(recs))
	for _, r := range recs {
		if _, dup := byID[r.ID]; !dup {
			byID[r.ID] = r
		}
	}
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
	}
}

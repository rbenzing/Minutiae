package evidence

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
)

// VerifyReport is the outcome of Case.Verify.
type VerifyReport struct {
	AuditEntries     int      `json:"audit_entries"`
	ArtifactsChecked int      `json:"artifacts_checked"`
	Problems         []string `json:"problems"`
}

// OK reports whether no integrity problems were found.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

// Verify re-hashes every artifact, checks the audit chain, and cross-checks
// manifest, artifacts.db and the artifacts directory. The result is audited.
func (c *Case) Verify() (VerifyReport, error) {
	rep := VerifyReport{Problems: []string{}}
	n, auditProblems, err := VerifyAuditLog(filepath.Join(c.Dir, auditFile))
	if err != nil {
		return rep, err
	}
	rep.AuditEntries = n
	rep.Problems = append(rep.Problems, auditProblems...)

	recs, err := c.Manifest()
	if err != nil {
		rep.Problems = append(rep.Problems, fmt.Sprintf("manifest unreadable: %v", err))
	}
	inManifest := map[string]bool{}
	for _, r := range recs {
		rep.ArtifactsChecked++
		inManifest[r.Path] = true
		d, err := HashFile(filepath.Join(c.Dir, filepath.FromSlash(r.Path)))
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("artifact %s (%s): %v", r.ID, r.Path, err))
			continue
		}
		if d.Size != r.Size || d.SHA256 != r.SHA256 || d.MD5 != r.MD5 {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"artifact %s (%s): hash mismatch: manifest sha256=%s size=%d, file sha256=%s size=%d",
				r.ID, r.Path, r.SHA256, r.Size, d.SHA256, d.Size))
		}
	}

	dbHashes, err := c.store.ArtifactHashes()
	if err != nil {
		return rep, err
	}
	for _, r := range recs {
		h, ok := dbHashes[r.ID]
		switch {
		case !ok:
			rep.Problems = append(rep.Problems, fmt.Sprintf("artifact %s: in manifest but not in artifacts.db", r.ID))
		case h != r.SHA256:
			rep.Problems = append(rep.Problems, fmt.Sprintf("artifact %s: artifacts.db sha256 differs from manifest", r.ID))
		}
		delete(dbHashes, r.ID)
	}
	extra := make([]string, 0, len(dbHashes))
	for id := range dbHashes {
		extra = append(extra, id)
	}
	sort.Strings(extra)
	for _, id := range extra {
		rep.Problems = append(rep.Problems, fmt.Sprintf("artifact %s: in artifacts.db but not in manifest", id))
	}

	err = filepath.WalkDir(filepath.Join(c.Dir, artifactsDir), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(c.Dir, p)
		if err != nil {
			return err
		}
		if !inManifest[filepath.ToSlash(rel)] {
			rep.Problems = append(rep.Problems, fmt.Sprintf("unmanifested file %s", filepath.ToSlash(rel)))
		}
		return nil
	})
	if err != nil {
		return rep, err
	}

	_, err = c.Audit.Append("verify.run", "", map[string]any{
		"ok": rep.OK(), "artifacts_checked": rep.ArtifactsChecked,
		"audit_entries": rep.AuditEntries, "problems": len(rep.Problems),
	})
	return rep, err
}

package evidence

// MaxDerivedDepth is the longest chain of derivations (links from an artifact to its root) that
// case verify accepts; provenance resolution uses the same bound.
const MaxDerivedDepth = maxDerivedDepth

// AuditState says how a manifest record relates to its artifact.create audit entry.
type AuditState string

// The states of an AuditBinding.
const (
	AuditBound      AuditState = "bound"      // exactly one entry, and the record agrees with it
	AuditMissing    AuditState = "missing"    // no artifact.create entry names the record's id
	AuditDiffers    AuditState = "differs"    // one entry, but a field of the record differs
	AuditDuplicate  AuditState = "duplicate"  // more than one entry names the id
	AuditUnreadable AuditState = "unreadable" // the entry's details cannot be decoded
)

// AuditBinding is the result of binding one manifest record to the audit log. Seq is the audit
// sequence of the entry found (the first one for a duplicate; 0 when missing). Detail names the first
// differing field (path, size, sha256, md5, incomplete or source) or carries the decode error.
type AuditBinding struct {
	State  AuditState
	Seq    int64
	Detail string
}

// AuditIndex looks manifest records up in an audit log by artifact id.
type AuditIndex struct {
	byID map[string][]AuditEntry
}

// NewAuditIndex indexes the artifact.create entries by details["id"] without decoding them; an entry
// without a string id is not indexed (verify reports it as unreadable).
func NewAuditIndex(entries []AuditEntry) *AuditIndex {
	x := &AuditIndex{byID: map[string][]AuditEntry{}}
	for _, e := range entries {
		if e.Action != "artifact.create" {
			continue
		}
		if id, _ := e.Details["id"].(string); id != "" {
			x.byID[id] = append(x.byID[id], e)
		}
	}
	return x
}

// Bind compares rec with its artifact.create entry, decoding only the entries of rec's id, with the
// same comparison case verify makes.
func (x *AuditIndex) Bind(rec ManifestRecord) AuditBinding {
	es := x.byID[rec.ID]
	switch len(es) {
	case 0:
		return AuditBinding{State: AuditMissing}
	case 1:
	default:
		return AuditBinding{State: AuditDuplicate, Seq: es[0].Seq}
	}
	e := es[0]
	_, a, err := decodeAuditedArtifact(e.Details)
	if err != nil {
		return AuditBinding{State: AuditUnreadable, Seq: e.Seq, Detail: err.Error()}
	}
	if f := auditDiff(rec, a, e.Details["source"]); f != "" {
		return AuditBinding{State: AuditDiffers, Seq: e.Seq, Detail: f}
	}
	return AuditBinding{State: AuditBound, Seq: e.Seq}
}

// auditDiff names the first field of r that differs from its audit entry ("" when none): the one
// comparison shared by case verify and AuditIndex.Bind.
func auditDiff(r ManifestRecord, a auditedArtifact, auditedSource any) string {
	switch {
	case a.Path != r.Path:
		return "path"
	case a.Size != r.Size:
		return "size"
	case a.SHA256 != r.SHA256:
		return "sha256"
	case a.MD5 != r.MD5:
		return "md5"
	case a.Incomplete != r.Incomplete:
		return "incomplete"
	case !sourceMatchesAudit(r.Source, auditedSource):
		return "source"
	}
	return ""
}

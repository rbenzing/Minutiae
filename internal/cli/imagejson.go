package cli

import (
	"time"

	"github.com/rbenzing/minutiae/internal/carve"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/image"
)

// The JSON shapes of the image commands are defined here, with explicit
// snake_case names, so the output contract does not depend on Go field names
// of internal packages. Timestamps are strings: a hostile year must not be
// able to make time.Time marshalling (which rejects years outside 0..9999)
// fail the whole output.

type jsonKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type jsonRun struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type jsonTime struct {
	Time      string `json:"time"` // RFC 3339 (nano, UTC) when the zone is known, else the same layout without a zone
	ZoneKnown bool   `json:"zone_known"`
}

func newJSONTime(ts filesys.Timestamp) *jsonTime {
	switch {
	case ts.T.IsZero():
		return nil
	case ts.ZoneKnown:
		return &jsonTime{Time: ts.T.UTC().Format(time.RFC3339Nano), ZoneKnown: true}
	}
	return &jsonTime{Time: ts.T.Format("2006-01-02T15:04:05.999999999")}
}

type jsonTimes struct {
	Modified *jsonTime `json:"modified,omitempty"`
	Accessed *jsonTime `json:"accessed,omitempty"`
	Changed  *jsonTime `json:"changed,omitempty"`
	Created  *jsonTime `json:"created,omitempty"`
	Deleted  *jsonTime `json:"deleted,omitempty"`
}

type jsonEntry struct {
	Name       string    `json:"name"`
	RawName    []byte    `json:"raw_name,omitempty"` // base64 in JSON
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	UID        uint32    `json:"uid"`
	GID        uint32    `json:"gid"`
	Times      jsonTimes `json:"times"`
	Deleted    bool      `json:"deleted"`
	Encrypted  bool      `json:"encrypted"`
	LinkTarget string    `json:"link_target,omitempty"`
	Attrs      []jsonKV  `json:"attrs,omitempty"`
}

type pathEntry struct {
	Path  string    `json:"path"`
	Entry jsonEntry `json:"entry"`
	// Snapshot names the snapshot an entry of an opted-in recursive listing (--include-snapshots)
	// was read from: such an entry is historical data, never live. Absent for every other entry.
	Snapshot string `json:"snapshot,omitempty"`
}

func newPathEntry(p string, e filesys.Entry) pathEntry {
	je := jsonEntry{
		Name: e.Name, RawName: e.RawName, ID: e.ID, Type: e.Type.String(), Size: e.Size, Mode: e.Mode,
		UID: e.UID, GID: e.GID, Deleted: e.Deleted, Encrypted: e.Encrypted, LinkTarget: e.LinkTarget,
		Times: jsonTimes{
			Modified: newJSONTime(e.Times.Modified), Accessed: newJSONTime(e.Times.Accessed),
			Changed: newJSONTime(e.Times.Changed), Created: newJSONTime(e.Times.Created),
			Deleted: newJSONTime(e.Times.Deleted),
		},
	}
	for _, kv := range e.Attrs {
		je.Attrs = append(je.Attrs, jsonKV{Key: kv.Key, Value: kv.Value})
	}
	return pathEntry{Path: p, Entry: je}
}

// jsonWarningItem is an item of the `image ls -r --json` array that is not an entry: a finding of the walker (a
// cycle, nesting too deep, a path too long) at Path, so a consumer sees the listing is not whole.
type jsonWarningItem struct {
	Path     string   `json:"path"`
	Warnings []string `json:"warnings"`
	// Snapshot names the snapshot the finding lies in, when it lies in one: historical, never live.
	Snapshot string `json:"snapshot,omitempty"`
}

// newWalkWarningItem is the array item for a walker finding at p; snapshotOf gives the snapshot a path lies in.
func newWalkWarningItem(snapshotOf func(string) string, p, finding string) jsonWarningItem {
	return jsonWarningItem{Path: p, Warnings: []string{finding}, Snapshot: snapshotOf(p)}
}

// jsonStat is the object `image stat --json` prints: the entry plus the
// filesystem warnings its lookup raised.
type jsonStat struct {
	pathEntry
	Warnings []string `json:"warnings,omitempty"`
}

type jsonFSInfo struct {
	Type      string   `json:"type"`
	Label     string   `json:"label"`
	UUID      string   `json:"uuid"`
	BlockSize int      `json:"block_size"`
	Size      int64    `json:"size"`
	Features  []string `json:"features"`
	Encrypted bool     `json:"encrypted"`
	Volumes   []string `json:"volumes"`
	Warnings  []string `json:"warnings"`
}

type jsonPartition struct {
	Index      int         `json:"index"`
	Start      int64       `json:"start"`
	Length     int64       `json:"length"`
	Type       string      `json:"type"`
	TypeName   string      `json:"type_name"`
	Name       string      `json:"name"`
	GUID       string      `json:"guid,omitempty"`
	Attributes uint64      `json:"attributes"`
	FSType     string      `json:"fs_type,omitempty"`
	FS         *jsonFSInfo `json:"fs,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type jsonImageInfo struct {
	ParentID    string          `json:"parent_id"`
	Path        string          `json:"path"`
	SHA256      string          `json:"sha256"`
	Incomplete  bool            `json:"incomplete"`
	Format      string          `json:"format"`
	Size        int64           `json:"size"`
	SectorSize  int             `json:"sector_size"`
	Metadata    []jsonKV        `json:"metadata"`
	Scheme      string          `json:"scheme"`
	DiskGUID    string          `json:"disk_guid,omitempty"`
	Partitions  []jsonPartition `json:"partitions"`
	Unallocated []jsonRun       `json:"unallocated"`
	Warnings    []string        `json:"warnings"`
	// PartitionError is why the partition table could not be read (with --verify).
	PartitionError string `json:"partition_error,omitempty"`
	// Verify is present only with --verify on a container that stores hashes.
	Verify *jsonVerify `json:"verify,omitempty"`
	// AcquisitionGaps is present when the container declares unreadable acquisition ranges.
	AcquisitionGaps *jsonGaps `json:"acquisition_gaps,omitempty"`
	// ImplausibleSize is true when the container's claimed size is one its own bytes cannot hold.
	ImplausibleSize bool `json:"implausible_size,omitempty"`
}

type jsonHashCheck struct {
	Stored   string `json:"stored"`
	Computed string `json:"computed"`
	Status   string `json:"status"`
	// Reason is why the hash is unverified when its stored section is damaged.
	Reason string `json:"reason,omitempty"`
}

// jsonVerify is the outcome of `image info --verify`: the same values the
// image.verify audit entry records.
type jsonVerify struct {
	Result        string        `json:"result"`
	Size          int64         `json:"size"`
	BytesHashed   int64         `json:"bytes_hashed"`
	MD5           jsonHashCheck `json:"md5"`
	SHA1          jsonHashCheck `json:"sha1"`
	FirstBadChunk *int64        `json:"first_bad_chunk,omitempty"`
	// Error is the error that ended the run (cancelled, failed audit append),
	// else the error of the first unreadable chunk.
	Error string `json:"error,omitempty"`
}

func newJSONVerify(cv examine.ContainerVerification, verr error) *jsonVerify {
	v := &jsonVerify{
		Result: cv.Result, Size: cv.Size, BytesHashed: cv.BytesHashed,
		MD5:  jsonHashCheck{Stored: cv.MD5.Stored, Computed: cv.MD5.Computed, Status: string(cv.MD5.Status), Reason: cv.MD5.Damaged},
		SHA1: jsonHashCheck{Stored: cv.SHA1.Stored, Computed: cv.SHA1.Computed, Status: string(cv.SHA1.Status), Reason: cv.SHA1.Damaged},
	}
	if cv.BadChunk >= 0 {
		bad := cv.BadChunk
		v.FirstBadChunk = &bad
		v.Error = cv.BadChunkError
	}
	if verr != nil {
		v.Error = verr.Error()
	}
	return v
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func newJSONImageInfo(in examine.ImageInfo) jsonImageInfo {
	out := jsonImageInfo{
		ParentID: in.ParentID, Path: in.Path, SHA256: in.SHA256, Incomplete: in.Incomplete,
		Format: in.Format, Size: in.Size, SectorSize: in.SectorSize, Scheme: in.Scheme, DiskGUID: in.DiskGUID,
		Metadata: []jsonKV{}, Partitions: []jsonPartition{}, Unallocated: []jsonRun{}, Warnings: nonNil(in.Warnings),
		PartitionError:  in.PartitionError,
		AcquisitionGaps: newJSONGaps(in.AcquisitionGaps),
		ImplausibleSize: in.SizeClaim != nil,
	}
	for _, kv := range in.Metadata {
		out.Metadata = append(out.Metadata, jsonKV{Key: kv.Key, Value: kv.Value})
	}
	for _, r := range in.Unallocated {
		out.Unallocated = append(out.Unallocated, jsonRun{Offset: r.Offset, Length: r.Length})
	}
	for _, pi := range in.Partitions {
		p := pi.Partition
		jp := jsonPartition{
			Index: p.Index, Start: p.Start, Length: p.Length, Type: p.Type, TypeName: p.TypeName, Name: p.Name,
			GUID: p.GUID, Attributes: p.Attributes, FSType: pi.FSType, Error: pi.Error,
		}
		if fi := pi.FSInfo; fi != nil {
			jp.FS = &jsonFSInfo{
				Type: fi.Type, Label: fi.Label, UUID: fi.UUID, BlockSize: fi.BlockSize, Size: fi.Size,
				Features: nonNil(fi.Features), Encrypted: fi.Encrypted, Volumes: nonNil(fi.Volumes), Warnings: nonNil(fi.Warnings),
			}
		}
		out.Partitions = append(out.Partitions, jp)
	}
	return out
}

type jsonSummary struct {
	AnalysisID    string                    `json:"analysis_id"`
	Files         int                       `json:"files"`
	Bytes         int64                     `json:"bytes"`
	Skipped       int                       `json:"skipped"`
	FSWarnings    int                       `json:"fs_warnings"`
	IncompleteIDs []string                  `json:"incomplete_ids"`
	OutputLimit   string                    `json:"output_limit,omitempty"`
	Artifacts     []evidence.ManifestRecord `json:"artifacts"`
}

func newJSONSummary(s examine.Summary) jsonSummary {
	out := jsonSummary{AnalysisID: s.AnalysisID, Files: s.Files, Bytes: s.Bytes, Skipped: s.Skipped, FSWarnings: s.FSWarnings, IncompleteIDs: incompleteTexts(s.IncompleteIDs), OutputLimit: s.OutputLimit, Artifacts: s.Artifacts}
	if out.Artifacts == nil {
		out.Artifacts = []evidence.ManifestRecord{}
	}
	return out
}

// jsonUnalloc is the JSON of an unalloc run. It has no incomplete_ids: unalloc does no id search, and an empty
// list would claim a search that never ran read everything (F97).
type jsonUnalloc struct {
	AnalysisID  string                    `json:"analysis_id"`
	Files       int                       `json:"files"`
	Bytes       int64                     `json:"bytes"`
	Skipped     int                       `json:"skipped"`
	FSWarnings  int                       `json:"fs_warnings"`
	OutputLimit string                    `json:"output_limit,omitempty"`
	Artifacts   []evidence.ManifestRecord `json:"artifacts"`
}

func newJSONUnalloc(s examine.Summary) jsonUnalloc {
	j := newJSONSummary(s)
	return jsonUnalloc{AnalysisID: j.AnalysisID, Files: j.Files, Bytes: j.Bytes, Skipped: j.Skipped, FSWarnings: j.FSWarnings, OutputLimit: j.OutputLimit, Artifacts: j.Artifacts}
}

// jsonRecover is the JSON of a recover run (strings are the raw ones; artifacts are manifest records).
type jsonRecover struct {
	AnalysisID     string                    `json:"analysis_id"`
	Considered     int                       `json:"considered"`
	Candidates     int                       `json:"candidates"`
	Recovered      int                       `json:"recovered"`
	Partial        int                       `json:"partial"`
	Uniform        int                       `json:"uniform"`
	Overlap        int                       `json:"overlap"`
	SkippedBy      map[string]int            `json:"skipped_by"`
	LimitReached   string                    `json:"limit_reached"`
	NotProcessed   int                       `json:"not_processed"`
	ReaderWarnings int                       `json:"reader_warnings"`
	IncompleteIDs  []string                  `json:"incomplete_ids"`
	FSWarnings     int                       `json:"fs_warnings"`
	Artifacts      []evidence.ManifestRecord `json:"artifacts"`
}

func newJSONRecover(s examine.RecoverSummary) jsonRecover {
	out := jsonRecover{
		AnalysisID: s.AnalysisID, Considered: s.Considered, Candidates: s.Candidates, Recovered: s.Recovered, Partial: s.Partial,
		Uniform: s.Uniform, Overlap: s.Overlap, SkippedBy: s.SkippedBy, LimitReached: s.LimitReached, NotProcessed: s.NotProcessed, ReaderWarnings: s.ReaderWarnings, IncompleteIDs: incompleteTexts(s.IncompleteIDs), FSWarnings: s.FSWarnings, Artifacts: s.Artifacts,
	}
	if out.SkippedBy == nil {
		out.SkippedBy = map[string]int{}
	}
	if out.Artifacts == nil {
		out.Artifacts = []evidence.ManifestRecord{}
	}
	return out
}

type jsonRecoverCandidate struct {
	Method       string                `json:"method"`
	Confidence   int                   `json:"confidence"`
	Band         string                `json:"band"`
	Size         int64                 `json:"size"`
	DeclaredSize int64                 `json:"declared_size"`
	Runs         []jsonRun             `json:"runs"`
	Excluded     []jsonRun             `json:"excluded"`
	Alloc        evidence.AllocSummary `json:"alloc"`
	Basis        []string              `json:"basis"`
	Assumptions  []string              `json:"assumptions"`
	Content      string                `json:"content"`
	Encrypted    bool                  `json:"encrypted"`
	Incomplete   string                `json:"incomplete,omitempty"`
	WouldWrite   bool                  `json:"would_write"`
	Skip         string                `json:"skip,omitempty"`
	SkipDetail   string                `json:"skip_detail,omitempty"`
	Warnings     []string              `json:"warnings"`
}

type jsonRecoverSkip struct {
	Path   string `json:"path"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

type jsonRecoverItem struct {
	Path       string                 `json:"path"`
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Name       string                 `json:"name"`
	Candidates []jsonRecoverCandidate `json:"candidates"`
	Skips      []jsonRecoverSkip      `json:"skips"`
}

type jsonRecoverList struct {
	Considered     int               `json:"considered"`
	Items          []jsonRecoverItem `json:"items"`
	SkippedBy      map[string]int    `json:"skipped_by"`
	LimitReached   string            `json:"limit_reached"`
	NotProcessed   int               `json:"not_processed"`
	ReaderWarnings int               `json:"reader_warnings"`
	IncompleteIDs  []string          `json:"incomplete_ids"`
	FSWarnings     int               `json:"fs_warnings"`
	// FSWarningsSuppressed is how many warnings the filesystem dropped while planning (0 and omitted when none).
	FSWarningsSuppressed uint64 `json:"fs_warnings_suppressed,omitempty"`
}

func jsonRuns(rs []evidence.Run) []jsonRun {
	out := make([]jsonRun, len(rs))
	for i, r := range rs {
		out[i] = jsonRun{Offset: r.Offset, Length: r.Length}
	}
	return out
}

func newJSONRecoverList(p *examine.RecoverPlan) jsonRecoverList {
	out := jsonRecoverList{Considered: p.Considered, Items: []jsonRecoverItem{}, SkippedBy: p.SkippedBy, LimitReached: p.LimitReached, NotProcessed: p.NotProcessed, ReaderWarnings: p.ReaderWarnings, IncompleteIDs: incompleteTexts(p.IncompleteIDs), FSWarnings: len(p.FSWarnings), FSWarningsSuppressed: p.FSWarningsSuppressed}
	if out.SkippedBy == nil {
		out.SkippedBy = map[string]int{}
	}
	for _, it := range p.Items {
		ji := jsonRecoverItem{Path: it.Path, ID: it.ID, Type: it.Type.String(), Name: it.Name, Candidates: []jsonRecoverCandidate{}, Skips: []jsonRecoverSkip{}}
		for _, c := range it.Candidates {
			ji.Candidates = append(ji.Candidates, jsonRecoverCandidate{
				Method: c.Method, Confidence: c.Confidence, Band: c.Band, Size: c.Size, DeclaredSize: c.DeclaredSize,
				Runs: jsonRuns(c.Runs), Excluded: jsonRuns(c.Excluded), Alloc: c.Alloc, Basis: nonNil(c.Basis), Assumptions: nonNil(c.Assumptions),
				Content: c.Content, Encrypted: c.Encrypted, Incomplete: c.PartialWhy(), WouldWrite: c.Skip == "", Skip: c.Skip, SkipDetail: c.SkipDetail(), Warnings: nonNil(c.Warnings),
			})
		}
		for _, sk := range it.Skips {
			ji.Skips = append(ji.Skips, jsonRecoverSkip{Path: sk.Path, ID: sk.ID, Reason: sk.Reason, Detail: sk.Detail})
		}
		out.Items = append(out.Items, ji)
	}
	return out
}

// jsonGaps is the acquirer's record of unreadable ranges as media byte ranges,
// as the image states them. Offsets and lengths are integers.
type jsonGaps struct {
	State     string    `json:"state"` // absent, empty, declared or damaged (damaged: the gaps are unknown)
	Empty     int       `json:"empty"`
	Ranges    uint64    `json:"ranges"`
	Bytes     int64     `json:"bytes"`
	Outside   int       `json:"outside"`
	Truncated bool      `json:"truncated"`
	Kept      []jsonRun `json:"kept"`
}

func newJSONGaps(g *image.AcquisitionGaps) *jsonGaps {
	if g == nil {
		return nil
	}
	out := &jsonGaps{State: g.State.String(), Empty: g.Empty, Ranges: g.Ranges, Outside: g.Outside, Truncated: g.Truncated, Kept: []jsonRun{}}
	for _, k := range g.Kept {
		out.Kept = append(out.Kept, jsonRun{Offset: k.Offset, Length: k.Length})
		out.Bytes += k.Length // merged and clipped to the media, so the sum is at most the media size
	}
	return out
}

// jsonCarve is the JSON of a carve run (strings are the raw ones; artifacts are manifest records).
type jsonCarve struct {
	AnalysisID         string                    `json:"analysis_id"`
	Carved             int                       `json:"carved"`
	Partial            int                       `json:"partial"`
	HitsByOutcome      map[string]int            `json:"hits_by_outcome"`
	RejectedBy         map[string]int            `json:"rejected_by"`
	SkippedBy          map[string]int            `json:"skipped_by"`
	LimitReached       string                    `json:"limit_reached"`
	NotScannedBytes    int64                     `json:"not_scanned_bytes"`
	ScannedBytes       int64                     `json:"scanned_bytes"`
	UnreadableBytes    int64                     `json:"unreadable_bytes"`
	UncopiedBytes      int64                     `json:"uncopied_bytes"`
	TypesCapped        []string                  `json:"types_capped"`
	NestedDepthSkipped int                       `json:"nested_depth_skipped"`
	FSWarnings         int                       `json:"fs_warnings"`
	Sigset             string                    `json:"sigset"`
	SigsetSHA256       string                    `json:"sigset_sha256"`
	SigFileSHA256      string                    `json:"sig_file_sha256"`
	Artifacts          []evidence.ManifestRecord `json:"artifacts"`
}

func carveOutcomeCounts(m map[carve.Outcome]int) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}

func carveCounts(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

func carveStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func newJSONCarve(s examine.CarveSummary) jsonCarve {
	out := jsonCarve{
		AnalysisID: s.AnalysisID, Carved: s.Carved, Partial: s.Partial, HitsByOutcome: carveOutcomeCounts(s.Stats.Accepted),
		RejectedBy: carveCounts(s.Stats.Rejected), SkippedBy: carveCounts(s.SkippedBy), LimitReached: s.LimitReached,
		NotScannedBytes: s.NotScannedBytes, ScannedBytes: s.Stats.Scanned, UnreadableBytes: s.Stats.UnreadableBytes,
		UncopiedBytes: s.UncopiedBytes, TypesCapped: carveStrings(s.Stats.TypeCapped), NestedDepthSkipped: s.Stats.NestedDepthSkipped,
		FSWarnings: s.FSWarnings, Sigset: s.Sigset, SigsetSHA256: s.SigsetSHA256, SigFileSHA256: s.SigFileSHA256, Artifacts: s.Artifacts,
	}
	if out.Artifacts == nil {
		out.Artifacts = []evidence.ManifestRecord{}
	}
	return out
}

type jsonCarveHit struct {
	Offset      int64                 `json:"offset"`
	Length      int64                 `json:"length"`
	Family      string                `json:"family"`
	Type        string                `json:"type"`
	Outcome     string                `json:"outcome"`
	Method      string                `json:"method"`
	Confidence  int                   `json:"confidence"`
	Alloc       evidence.AllocSummary `json:"alloc"`
	NestedIn    *int64                `json:"nested_in"`
	WouldWrite  bool                  `json:"would_write"`
	Skip        string                `json:"skip"`
	Name        string                `json:"name"`
	Basis       []string              `json:"basis"`
	Assumptions []string              `json:"assumptions"`
}

// jsonCarveList is the JSON of image carve --list.
type jsonCarveList struct {
	Scope           string         `json:"scope"`
	ScopeRuns       int            `json:"scope_runs"`
	ScopeBytes      int64          `json:"scope_bytes"`
	Align           int64          `json:"align"`
	Hits            []jsonCarveHit `json:"hits"`
	HitsNotListed   int            `json:"hits_not_listed"`
	HitsByOutcome   map[string]int `json:"hits_by_outcome"`
	RejectedBy      map[string]int `json:"rejected_by"`
	SkippedBy       map[string]int `json:"skipped_by"`
	LimitReached    string         `json:"limit_reached"`
	NotScannedBytes int64          `json:"not_scanned_bytes"`
	UnreadableBytes int64          `json:"unreadable_bytes"`
	TypesCapped     []string       `json:"types_capped"`
	Sigset          string         `json:"sigset"`
	SigsetSHA256    string         `json:"sigset_sha256"`
	SigFileSHA256   string         `json:"sig_file_sha256"`
	Notes           []string       `json:"notes"`
}

func newJSONCarveList(p *examine.CarvePlan) jsonCarveList {
	out := jsonCarveList{
		Scope: p.Scope, ScopeRuns: p.ScopeRuns, ScopeBytes: p.ScopeBytes, Align: p.Align, Hits: []jsonCarveHit{},
		HitsNotListed: p.HitsNotListed, HitsByOutcome: carveOutcomeCounts(p.Stats.Accepted), RejectedBy: carveCounts(p.Stats.Rejected),
		SkippedBy: carveCounts(p.SkippedBy), LimitReached: p.LimitReached, NotScannedBytes: p.NotScannedBytes,
		UnreadableBytes: p.Stats.UnreadableBytes, TypesCapped: carveStrings(p.Stats.TypeCapped), Sigset: p.Sigset,
		SigsetSHA256: p.SigsetSHA256, SigFileSHA256: p.SigFileSHA256, Notes: carveStrings(p.Notes),
	}
	for _, h := range p.Hits {
		jh := jsonCarveHit{
			Offset: h.Offset, Length: h.Length, Family: h.Family, Type: h.Type, Outcome: string(h.Outcome), Method: h.Method,
			Confidence: h.Confidence, Alloc: h.Alloc, WouldWrite: h.Skip == "", Skip: h.Skip, Name: h.Name,
			Basis: carveStrings(h.Basis), Assumptions: carveStrings(h.Assumptions),
		}
		if h.NestedIn >= 0 {
			n := h.NestedIn
			jh.NestedIn = &n
		}
		out.Hits = append(out.Hits, jh)
	}
	return out
}

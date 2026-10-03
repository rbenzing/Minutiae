package cli

import (
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
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
	AnalysisID string                    `json:"analysis_id"`
	Files      int                       `json:"files"`
	Bytes      int64                     `json:"bytes"`
	Skipped    int                       `json:"skipped"`
	Artifacts  []evidence.ManifestRecord `json:"artifacts"`
}

func newJSONSummary(s examine.Summary) jsonSummary {
	out := jsonSummary{AnalysisID: s.AnalysisID, Files: s.Files, Bytes: s.Bytes, Skipped: s.Skipped, Artifacts: s.Artifacts}
	if out.Artifacts == nil {
		out.Artifacts = []evidence.ManifestRecord{}
	}
	return out
}

package evidence

import (
	"encoding/json"
	"fmt"
	"os"
)

// Source describes where an artifact's bytes came from.
type Source struct {
	Kind       string `json:"kind"` // "file", "partition", "serial", "info", "backup"
	DeviceID   string `json:"device_id"`
	RemotePath string `json:"remote_path,omitempty"`
	Partition  string `json:"partition,omitempty"`
	// Remote metadata as the device reported it (e.g. a sync LIST entry);
	// omitted when unknown. RemoteMTime is RFC 3339 UTC.
	RemoteMode  uint32 `json:"remote_mode,omitempty"`
	RemoteMTime string `json:"remote_mtime,omitempty"`
	RemoteSize  int64  `json:"remote_size,omitempty"`
	// Import: examiner-side source path and 1-based segment number.
	OriginalPath string `json:"original_path,omitempty"`
	Segment      int    `json:"segment,omitempty"`
	// Segments is the total number of segments of the import (set on every
	// segment record), so a partially imported image is detectable.
	Segments int `json:"segments,omitempty"`
	// Extract / unallocated: how the artifact was derived from another one.
	Derived *Derivation `json:"derived,omitempty"`
}

// ManifestRecord is one line of manifest.jsonl and one row of artifacts.
type ManifestRecord struct {
	ID         string `json:"id"`
	Path       string `json:"path"` // slash-separated, relative to the case dir
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	MD5        string `json:"md5"`
	Source     Source `json:"source"`
	Started    string `json:"started"`
	Finished   string `json:"finished"`
	Incomplete bool   `json:"incomplete"`
	Error      string `json:"error,omitempty"`
}

func appendManifest(path string, r ManifestRecord) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync manifest: %w", err)
	}
	return f.Close()
}

func readManifest(path string) ([]ManifestRecord, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	out := make([]ManifestRecord, 0, len(lines))
	for i, l := range lines {
		var r ManifestRecord
		if err := json.Unmarshal(l, &r); err != nil {
			return nil, fmt.Errorf("manifest line %d: %w: %w", i+1, ErrIntegrity, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// MaxInlineRuns is the most runs embedded in a Derivation; more go to a runs sidecar artifact.
const MaxInlineRuns = 4096

// Run is a byte range of a parent image. Offset -1 marks a sparse hole.
type Run struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// SegmentRef identifies one segment artifact of a multi-segment parent image.
type SegmentRef struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// Derivation records how an artifact was produced from another artifact.
type Derivation struct {
	ParentID         string `json:"parent_id"`
	ParentSHA256     string `json:"parent_sha256"`
	ParentIncomplete bool   `json:"parent_incomplete,omitempty"`
	// ParentSegments lists every segment (in order, including segment 1) of a
	// multi-segment parent; empty for a single-artifact parent.
	ParentSegments  []SegmentRef      `json:"parent_segments,omitempty"`
	Partition       int               `json:"partition"`        // 0 = whole image
	PartitionOffset int64             `json:"partition_offset"` // bytes into the image
	FSType          string            `json:"fs_type,omitempty"`
	FSPath          string            `json:"fs_path,omitempty"`
	FSID            string            `json:"fs_id,omitempty"`
	Mode            uint32            `json:"mode,omitempty"`
	UID             uint32            `json:"uid,omitempty"`
	GID             uint32            `json:"gid,omitempty"`
	Times           map[string]string `json:"times,omitempty"` // RFC 3339
	Encrypted       bool              `json:"encrypted,omitempty"`
	Runs            []Run             `json:"runs,omitempty"`          // image-relative byte runs (at most MaxInlineRuns)
	RunsArtifact    string            `json:"runs_artifact,omitempty"` // id of a runs sidecar artifact
	// DeclaredRunsArtifact names, on an incomplete extract, the runs sidecar that
	// holds the full run list the filesystem declared for the file, written before
	// the copy was cut short. It is provenance of what was declared, never of what
	// was captured (Runs/RunsArtifact are), and is not used to reproduce bytes.
	DeclaredRunsArtifact string `json:"declared_runs_artifact,omitempty"`

	// DeclaredSize is, on an extract, the full size of the source file the acquisition-gap note was computed
	// with (case verify recomputes the note with it). It differs from the artifact size when the copy was cut
	// short. Omitted for a 0-byte file (no note exists) and on extracts recorded before gap notes.
	DeclaredSize int64 `json:"declared_size,omitempty"`

	// Snapshot is set when the bytes were read from a snapshot view of the
	// filesystem (APFS) and not from the live tree.
	Snapshot *SnapshotRef `json:"snapshot,omitempty"`

	// AcquisitionGaps is set on extract and unallocated artifacts read from an
	// image that declares unreadable acquisition ranges (E01 error2) when the
	// artifact's bytes overlap them, or when that cannot be decided (a damaged
	// error2 section included). Unknown is never omitted when true.
	AcquisitionGaps *AcquisitionGapNote `json:"acquisition_gaps,omitempty"`

	// Recovery is set on artifacts of the recovered kinds (recover, carve, slack,
	// journal, report): the class, method, confidence and allocation evidence.
	Recovery *Recovery `json:"recovery,omitempty"`
}

// SnapshotRef names the filesystem snapshot a derived artifact was read from:
// the snapshot's display name and its transaction id.
type SnapshotRef struct {
	Name string `json:"name"`
	Xid  uint64 `json:"xid"`
}

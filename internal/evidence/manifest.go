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
			return nil, fmt.Errorf("manifest line %d: %w", i+1, err)
		}
		out = append(out, r)
	}
	return out, nil
}

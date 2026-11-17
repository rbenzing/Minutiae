package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// manifestRec is the part of a manifest.jsonl line that the acceptance steps read. The tool imports no Minutiae
// package, so the line is decoded as plain JSON.
type manifestRec struct {
	ID, Path, SHA256, Kind, Class, Method string
}

type rawManifestRec struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Source struct {
		Kind    string `json:"kind"`
		Derived *struct {
			Recovery *struct {
				Class  string `json:"class"`
				Method string `json:"method"`
			} `json:"recovery"`
		} `json:"derived"`
	} `json:"source"`
}

// maxManifestLine bounds one manifest line (inline run lists make some long).
const maxManifestLine = 64 << 20

// readManifest reads <caseDir>/manifest.jsonl. A line that is not a JSON object, or a record without an id, is an
// error: a damaged manifest never reads as a shorter one.
func readManifest(caseDir string) ([]manifestRec, error) {
	f, err := os.Open(filepath.Join(caseDir, "manifest.jsonl")) //nolint:gosec // the tool's own scratch case
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxManifestLine)
	var out []manifestRec
	for n := 1; sc.Scan(); n++ {
		var r rawManifestRec
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("manifest.jsonl line %d: %w", n, err)
		}
		if r.ID == "" {
			return nil, fmt.Errorf("manifest.jsonl line %d: a record without an id", n)
		}
		m := manifestRec{ID: r.ID, Path: r.Path, SHA256: r.SHA256, Kind: r.Source.Kind}
		if r.Source.Derived != nil && r.Source.Derived.Recovery != nil {
			m.Class, m.Method = r.Source.Derived.Recovery.Class, r.Source.Derived.Recovery.Method
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("manifest.jsonl holds no record")
	}
	return out, nil
}

// newSince is the records of now whose id is not in before.
func newSince(before, now []manifestRec) []manifestRec {
	seen := make(map[string]bool, len(before))
	for _, r := range before {
		seen[r.ID] = true
	}
	var out []manifestRec
	for _, r := range now {
		if !seen[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

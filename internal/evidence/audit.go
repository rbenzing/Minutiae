package evidence

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// GenesisHash is the prev_hash of the first audit entry.
var GenesisHash = strings.Repeat("0", 64)

// AuditEntry is one line of audit.jsonl.
type AuditEntry struct {
	Seq         int64          `json:"seq"`
	Time        string         `json:"time"`
	Actor       string         `json:"actor"`
	ToolVersion string         `json:"tool_version"`
	Action      string         `json:"action"`
	DeviceID    string         `json:"device_id,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
	PrevHash    string         `json:"prev_hash"`
	Hash        string         `json:"hash"`
}

// AuditLog is an append-only, hash-chained JSON-lines log. Safe for concurrent use.
type AuditLog struct {
	mu          sync.Mutex
	f           *os.File
	seq         int64
	last        string
	actor       string
	toolVersion string
	now         func() time.Time
}

// OpenAuditLog opens (or creates) the log at path and continues its chain.
func OpenAuditLog(path, actor, toolVersion string) (*AuditLog, error) {
	entries, err := ReadAuditEntries(path)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	a := &AuditLog{last: GenesisHash, actor: actor, toolVersion: toolVersion, now: time.Now}
	if n := len(entries); n > 0 {
		a.seq, a.last = entries[n-1].Seq, entries[n-1].Hash
	}
	a.f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return a, nil
}

// Append writes one entry, fsyncs, and returns it.
func (a *AuditLog) Append(action, deviceID string, details map[string]any) (AuditEntry, error) {
	norm, err := normalizeDetails(details)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("audit %s: %w", action, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return AuditEntry{}, errors.New("audit log is closed")
	}
	e := AuditEntry{
		Seq:         a.seq + 1,
		Time:        a.now().UTC().Format(time.RFC3339Nano),
		Actor:       a.actor,
		ToolVersion: a.toolVersion,
		Action:      action,
		DeviceID:    deviceID,
		Details:     norm,
		PrevHash:    a.last,
	}
	if e.Hash, err = entryHash(e); err != nil {
		return AuditEntry{}, err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return AuditEntry{}, err
	}
	if _, err := a.f.Write(append(line, '\n')); err != nil {
		return AuditEntry{}, fmt.Errorf("audit write: %w", err)
	}
	if err := a.f.Sync(); err != nil {
		return AuditEntry{}, fmt.Errorf("audit sync: %w", err)
	}
	a.seq, a.last = e.Seq, e.Hash
	return e, nil
}

// Close closes the log; later Appends fail.
func (a *AuditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}

// normalizeDetails round-trips details through JSON so that what is hashed
// at append time is byte-identical to what verification re-derives from the
// file (structs become maps with sorted keys, numbers become json.Number).
func normalizeDetails(d map[string]any) (map[string]any, error) {
	if len(d) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func entryHash(e AuditEntry) (string, error) {
	e.Hash = ""
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var lines [][]byte
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
			lines = append(lines, trimmed)
		}
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func decodeEntry(line []byte) (AuditEntry, error) {
	var e AuditEntry
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	err := dec.Decode(&e)
	return e, err
}

// ReadAuditEntries parses every entry; a missing file yields no entries.
func ReadAuditEntries(path string) ([]AuditEntry, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	entries := make([]AuditEntry, 0, len(lines))
	for i, l := range lines {
		e, err := decodeEntry(l)
		if err != nil {
			return nil, fmt.Errorf("audit line %d: %w", i+1, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// VerifyAuditLog checks JSON validity, seq continuity, the prev_hash chain and
// each entry's own hash. It returns the number of lines and every problem found.
func VerifyAuditLog(path string) (int, []string, error) {
	lines, err := readLines(path)
	if err != nil {
		return 0, nil, err
	}
	var problems []string
	prev := GenesisHash
	for i, l := range lines {
		n := i + 1
		e, err := decodeEntry(l)
		if err != nil {
			problems = append(problems, fmt.Sprintf("audit line %d: invalid JSON: %v", n, err))
			prev = ""
			continue
		}
		if e.Seq != int64(n) {
			problems = append(problems, fmt.Sprintf("audit line %d: seq %d, want %d (lines removed or reordered)", n, e.Seq, n))
		}
		if prev != "" && e.PrevHash != prev {
			problems = append(problems, fmt.Sprintf("audit line %d: prev_hash does not match previous entry (chain broken)", n))
		}
		h, err := entryHash(e)
		if err != nil {
			return 0, nil, err
		}
		if h != e.Hash {
			problems = append(problems, fmt.Sprintf("audit line %d: hash mismatch (entry altered)", n))
		}
		prev = e.Hash
	}
	return len(lines), problems, nil
}

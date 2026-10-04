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

// firstAuditAction is the action every case's audit log must begin with.
const firstAuditAction = "case.create"

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
	Hash        string         `json:"hash,omitempty"`
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

// CreateAuditLog creates a new, empty log at path. It refuses an existing file.
func CreateAuditLog(path, actor, toolVersion string) (*AuditLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create audit log: %w", err)
	}
	return &AuditLog{f: f, last: GenesisHash, actor: actor, toolVersion: toolVersion, now: time.Now}, nil
}

// OpenAuditLog opens an existing log and continues its chain. A missing or
// unparseable log is an integrity failure (ErrIntegrity): it is never
// silently recreated or repaired.
func OpenAuditLog(path, actor, toolVersion string) (*AuditLog, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: audit log %s is missing", ErrIntegrity, path)
	}
	lines, err := readLines(path)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	entries, bad, err := decodeEntries(lines)
	if err != nil {
		return nil, fmt.Errorf("%w: audit log unreadable at line %d (edited or torn write): %v", ErrIntegrity, bad, err)
	}
	a := &AuditLog{last: GenesisHash, actor: actor, toolVersion: toolVersion, now: time.Now}
	if n := len(entries); n > 0 {
		a.seq, a.last = entries[n-1].Seq, entries[n-1].Hash
	}
	a.f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return a, nil
}

// Append writes one entry, fsyncs, and returns it.
func (a *AuditLog) Append(action, deviceID string, details map[string]any) (AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return AuditEntry{}, errors.New("audit log is closed")
	}
	e, err := canonicalEntry(AuditEntry{
		Seq:         a.seq + 1,
		Time:        a.now().UTC().Format(time.RFC3339Nano),
		Actor:       a.actor,
		ToolVersion: a.toolVersion,
		Action:      action,
		DeviceID:    deviceID,
		Details:     details,
		PrevHash:    a.last,
	})
	if err != nil {
		return AuditEntry{}, fmt.Errorf("audit %s: %w", action, err)
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

// canonicalEntry round-trips e through JSON exactly as verification will
// decode it from the file, so the hash computed at append time is over the
// same bytes verification re-derives: structs become maps with sorted keys,
// numbers become json.Number and invalid UTF-8 becomes U+FFFD in every string.
func canonicalEntry(e AuditEntry) (AuditEntry, error) {
	e.Hash = ""
	b, err := json.Marshal(e)
	if err != nil {
		return AuditEntry{}, err
	}
	return decodeEntry(b)
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

// decodeEntries parses every line; on failure it returns the 1-based line number.
func decodeEntries(lines [][]byte) ([]AuditEntry, int, error) {
	entries := make([]AuditEntry, 0, len(lines))
	for i, l := range lines {
		e, err := decodeEntry(l)
		if err != nil {
			return nil, i + 1, err
		}
		entries = append(entries, e)
	}
	return entries, 0, nil
}

// errAuditCorrupt marks an audit log that cannot be parsed (as opposed to one
// that cannot be read: I/O errors stay plain errors).
var errAuditCorrupt = errors.New("audit log is corrupt")

// ReadAuditEntries parses every entry; a missing file yields no entries. An
// entry that does not parse wraps an unexported corruption marker; read errors
// are returned as they are.
func ReadAuditEntries(path string) ([]AuditEntry, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	entries, bad, err := decodeEntries(lines)
	if err != nil {
		return nil, fmt.Errorf("%w: audit line %d: %w", errAuditCorrupt, bad, err)
	}
	return entries, nil
}

// VerifyAuditLog checks JSON validity, that the log is non-empty and begins
// with case.create, seq continuity, the prev_hash chain and each entry's own
// hash. It returns the number of lines and every problem found.
func VerifyAuditLog(path string) (int, []string, error) {
	n, _, problems, err := verifyAudit(path)
	return n, problems, err
}

// verifyAudit is VerifyAuditLog that also returns the entries that decoded.
func verifyAudit(path string) (int, []AuditEntry, []string, error) {
	lines, err := readLines(path)
	if err != nil {
		return 0, nil, nil, err
	}
	problems := []string{}
	if len(lines) == 0 {
		problems = append(problems, "audit log is empty (it must begin with "+firstAuditAction+")")
	}
	var entries []AuditEntry
	prev := GenesisHash
	for i, l := range lines {
		n := i + 1
		e, err := decodeEntry(l)
		if err != nil {
			problems = append(problems, fmt.Sprintf("audit line %d: invalid JSON: %v", n, err))
			prev = ""
			continue
		}
		entries = append(entries, e)
		if n == 1 && e.Action != firstAuditAction {
			problems = append(problems, fmt.Sprintf("audit line 1: action %q, want %q (log truncated or replaced)", e.Action, firstAuditAction))
		}
		if e.Seq != int64(n) {
			problems = append(problems, fmt.Sprintf("audit line %d: seq %d, want %d (lines removed or reordered)", n, e.Seq, n))
		}
		if prev != "" && e.PrevHash != prev {
			problems = append(problems, fmt.Sprintf("audit line %d: prev_hash does not match previous entry (chain broken)", n))
		}
		h, err := entryHash(e)
		if err != nil {
			return 0, nil, nil, err
		}
		if h != e.Hash {
			problems = append(problems, fmt.Sprintf("audit line %d: hash mismatch (entry altered)", n))
		}
		prev = e.Hash
	}
	return len(lines), entries, problems, nil
}

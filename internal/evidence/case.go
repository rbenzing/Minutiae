package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rbenzing/minutiae/internal/version"
)

const (
	caseFile     = "case.json"
	auditFile    = "audit.jsonl"
	manifestFile = "manifest.jsonl"
	dbFile       = "artifacts.db"
	artifactsDir = "artifacts"
)

var (
	// ErrCaseExists is returned when creating a case over a non-empty directory.
	ErrCaseExists = errors.New("case already exists")
	// ErrIntegrity marks a failed integrity verification.
	ErrIntegrity = errors.New("integrity check failed")
)

// Meta is case.json.
type Meta struct {
	ID          string `json:"id"`
	Examiner    string `json:"examiner"`
	Description string `json:"description,omitempty"`
	Created     string `json:"created"`
	ToolVersion string `json:"tool_version"`
	HostOS      string `json:"host_os"`
	HostArch    string `json:"host_arch"`
}

// CreateOptions are the inputs to Create.
type CreateOptions struct {
	ID          string
	Examiner    string
	Description string
}

// Case is an open case directory.
type Case struct {
	Dir        string
	Meta       Meta
	Audit      *AuditLog
	store      *Store
	manifestMu sync.Mutex
}

var validCaseID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var currentActor = func() string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "unknown"
	}
	return u.Username
}

// Create makes <parentDir>/<ID> as a new case. It refuses a non-empty directory.
func Create(parentDir string, opts CreateOptions) (*Case, error) {
	if !validCaseID.MatchString(opts.ID) || strings.Contains(opts.ID, "..") {
		return nil, fmt.Errorf("invalid case id %q: use letters, digits, '.', '_' or '-' (max 64, must start with a letter or digit)", opts.ID)
	}
	if strings.TrimSpace(opts.Examiner) == "" {
		return nil, errors.New("examiner is required")
	}
	dir := filepath.Join(parentDir, opts.ID)
	entries, err := os.ReadDir(dir)
	switch {
	case err == nil && len(entries) > 0:
		return nil, fmt.Errorf("%w: %s", ErrCaseExists, dir)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, artifactsDir), 0o750); err != nil {
		return nil, err
	}
	meta := Meta{
		ID: opts.ID, Examiner: opts.Examiner, Description: opts.Description,
		Created:     time.Now().UTC().Format(time.RFC3339),
		ToolVersion: version.String(), HostOS: runtime.GOOS, HostArch: runtime.GOARCH,
	}
	if err := writeExclusiveJSON(filepath.Join(dir, caseFile), meta); err != nil {
		return nil, err
	}
	audit, err := CreateAuditLog(filepath.Join(dir, auditFile), currentActor(), version.String())
	if err != nil {
		return nil, err
	}
	c, err := withStore(dir, meta, audit)
	if err != nil {
		return nil, err
	}
	if _, err := c.Audit.Append("case.create", "", map[string]any{
		"id": meta.ID, "examiner": meta.Examiner, "description": meta.Description,
	}); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Open opens an existing case and records the access in the audit log.
func Open(dir string) (*Case, error) {
	b, err := os.ReadFile(filepath.Join(dir, caseFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s is not a Minutiae case (no %s)", dir, caseFile)
	}
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, fmt.Errorf("%s: %w", caseFile, err)
	}
	c, err := openParts(dir, meta)
	if err != nil {
		return nil, err
	}
	if _, err := c.Audit.Append("case.open", "", nil); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// openParts opens the audit log and artifacts.db of an existing case. Both are
// created only by Create; a missing one is an integrity failure, never recreated.
func openParts(dir string, meta Meta) (*Case, error) {
	if _, err := os.Stat(filepath.Join(dir, dbFile)); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s is missing from case %s", ErrIntegrity, dbFile, dir)
	}
	audit, err := OpenAuditLog(filepath.Join(dir, auditFile), currentActor(), version.String())
	if err != nil {
		return nil, err
	}
	return withStore(dir, meta, audit)
}

func withStore(dir string, meta Meta, audit *AuditLog) (*Case, error) {
	store, err := OpenStore(filepath.Join(dir, dbFile))
	if err != nil {
		_ = audit.Close()
		return nil, err
	}
	return &Case{Dir: dir, Meta: meta, Audit: audit, store: store}, nil
}

func writeExclusiveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Manifest returns every artifact record in order.
func (c *Case) Manifest() ([]ManifestRecord, error) {
	c.manifestMu.Lock()
	defer c.manifestMu.Unlock()
	return readManifest(filepath.Join(c.Dir, manifestFile))
}

// Close closes the store and audit log.
func (c *Case) Close() error {
	return errors.Join(c.store.Close(), c.Audit.Close())
}

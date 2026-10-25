package carve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Pattern is one header test: the bytes at Offset must match Bytes, under Mask
// when it is set (b&m == p&m).
type Pattern struct {
	Offset      int
	Bytes, Mask []byte
}

// Type is one signature: the headers that open an object, an optional footer,
// the length bounds and the validator that decides the object's end.
type Type struct {
	Name, Ext, MIME string
	Headers         []Pattern // ALL must match
	Footer          []byte    // optional
	MinLen, MaxLen  int64
	Validate        string // "" or a built-in validator id
	Align           string // "block" | "any"
}

// Set is a named collection of types.
type Set struct {
	ID    string
	Types []Type
}

// Limits of a signature and the built-in set's id. A change to the built-in
// table MUST change BuiltinID: audit records name the set by id and digest.
const (
	BuiltinID               = "minutiae-sigset/1"
	MaxPatternLen           = 32
	MaxPatternOffset        = 512
	MaxHeaders              = 4
	MaxTypeLen        int64 = 4 << 30
	DefaultBufSize          = 1 << 20
	MaxValidateBudget int64 = 64 << 30

	maxNameLen  = 32
	maxExtLen   = 16
	maxMIMELen  = 127
	maxSetIDLen = 64
	alignBlock  = "block"
	alignAny    = "any"
)

// Errors of the signature model.
var (
	ErrBadSignature = errors.New("carve: bad signature")
	ErrUnknownType  = errors.New("carve: unknown type")
)

func pat(off int, b ...byte) Pattern { return Pattern{Offset: off, Bytes: b} }

func builtinType(name, ext, mime, validate string, minLen, maxLen int64, headers ...Pattern) Type {
	return Type{Name: name, Ext: ext, MIME: mime, Headers: headers, MinLen: minLen, MaxLen: maxLen, Validate: validate, Align: alignBlock}
}

// Builtin returns a fresh deep copy of the built-in set (the twelve spec 7.2
// families). Nothing is shared between calls.
func Builtin() *Set {
	const mib, gib = 1 << 20, 1 << 30
	return &Set{ID: BuiltinID, Types: []Type{
		builtinType("jpeg", "jpg", "image/jpeg", "jpeg", 64, 128*mib, pat(0, 0xFF, 0xD8, 0xFF)),
		builtinType("png", "png", "image/png", "png", 57, 64*mib, pat(0, 0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A)),
		builtinType("gif", "gif", "image/gif", "gif", 26, 32*mib, pat(0, 'G', 'I', 'F', '8')),
		builtinType("webp", "webp", "image/webp", "riff-webp", 26, 64*mib, pat(0, 'R', 'I', 'F', 'F'), pat(8, 'W', 'E', 'B', 'P')),
		builtinType("isobmff", "bmff", "video/mp4", "isobmff", 24, 4*gib, pat(4, 'f', 't', 'y', 'p')),
		builtinType("sqlite", "sqlite", "application/vnd.sqlite3", "sqlite", 512, gib,
			pat(0, 'S', 'Q', 'L', 'i', 't', 'e', ' ', 'f', 'o', 'r', 'm', 'a', 't', ' ', '3', 0x00)),
		builtinType("bplist", "plist", "application/x-bplist", "bplist", 42, 16*mib, pat(0, 'b', 'p', 'l', 'i', 's', 't', '0', '0')),
		builtinType("zip", "zip", "application/zip", "zip", 64, gib, pat(0, 'P', 'K', 0x03, 0x04)),
		builtinType("pdf", "pdf", "application/pdf", "pdf", 32, 512*mib, pat(0, '%', 'P', 'D', 'F', '-')),
		builtinType("gzip", "gz", "application/gzip", "gzip", 18, 64*mib, pat(0, 0x1F, 0x8B, 0x08)),
		builtinType("wav", "wav", "audio/wav", "riff-wave", 44, gib, pat(0, 'R', 'I', 'F', 'F'), pat(8, 'W', 'A', 'V', 'E')),
		builtinType("ogg", "ogg", "audio/ogg", "ogg", 28, 256*mib, pat(0, 'O', 'g', 'g', 'S', 0x00)),
	}}
}

// RefinedTypes lists, per family that refines, the names the structure can
// assign (identity from structure, never from a guess). A fresh map each call.
func RefinedTypes() map[string][]string {
	return map[string][]string{
		"isobmff": {"mp4", "mov", "3gp", "heic", "heif", "isobmff"},
		"zip":     {"zip", "apk", "jar", "docx", "xlsx", "pptx"},
	}
}

type canonPattern struct {
	Offset int    `json:"offset"`
	Bytes  string `json:"bytes"`
	Mask   string `json:"mask,omitempty"`
}

type canonType struct {
	Name     string         `json:"name"`
	Ext      string         `json:"ext"`
	MIME     string         `json:"mime"`
	Headers  []canonPattern `json:"headers"`
	Footer   string         `json:"footer,omitempty"`
	MinLen   int64          `json:"min_len"`
	MaxLen   int64          `json:"max_len"`
	Validate string         `json:"validate"`
	Align    string         `json:"align"`
}

type canonSet struct {
	ID    string      `json:"id"`
	Types []canonType `json:"types"`
}

// Canonical is the canonical JSON form of the set: types sorted by name, a
// fixed field order, bytes as lowercase hex. Equal sets give equal bytes.
func (s *Set) Canonical() []byte {
	c := canonSet{ID: s.ID, Types: make([]canonType, 0, len(s.Types))}
	for _, t := range s.Types {
		ct := canonType{
			Name: t.Name, Ext: t.Ext, MIME: t.MIME, Footer: hex.EncodeToString(t.Footer),
			MinLen: t.MinLen, MaxLen: t.MaxLen, Validate: t.Validate, Align: t.Align,
			Headers: make([]canonPattern, 0, len(t.Headers)),
		}
		for _, p := range t.Headers {
			ct.Headers = append(ct.Headers, canonPattern{Offset: p.Offset, Bytes: hex.EncodeToString(p.Bytes), Mask: hex.EncodeToString(p.Mask)})
		}
		c.Types = append(c.Types, ct)
	}
	slices.SortStableFunc(c.Types, func(a, b canonType) int { return strings.Compare(a.Name, b.Name) })
	out, err := json.Marshal(c)
	if err != nil {
		return nil // unreachable: the structure holds strings and integers only
	}
	return out
}

// Digest is the lowercase hex SHA-256 of Canonical.
func (s *Set) Digest() string {
	sum := sha256.Sum256(s.Canonical())
	return hex.EncodeToString(sum[:])
}

func okIdent(s string, maxLen int, dash bool) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && dash && i > 0:
		default:
			return false
		}
	}
	return true
}

func printable(s string, maxLen int) bool {
	if len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

func (t *Type) check() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: type %q: %s", ErrBadSignature, t.Name, fmt.Sprintf(format, args...))
	}
	switch {
	case !okIdent(t.Name, maxNameLen, true):
		return bad("name must be 1-%d characters of a-z, 0-9 and a dash", maxNameLen)
	case !okIdent(t.Ext, maxExtLen, false):
		return bad("extension must be 1-%d characters of a-z and 0-9", maxExtLen)
	case !printable(t.MIME, maxMIMELen):
		return bad("MIME type must be printable ASCII of at most %d characters", maxMIMELen)
	case t.MaxLen < 1 || t.MaxLen > MaxTypeLen:
		return bad("MaxLen %d is outside 1..%d", t.MaxLen, MaxTypeLen)
	case t.MinLen < 0 || t.MinLen > t.MaxLen:
		return bad("MinLen %d is outside 0..MaxLen", t.MinLen)
	case len(t.Headers) < 1 || len(t.Headers) > MaxHeaders:
		return bad("%d headers, want 1..%d", len(t.Headers), MaxHeaders)
	case len(t.Footer) > MaxPatternLen:
		return bad("footer of %d bytes, at most %d", len(t.Footer), MaxPatternLen)
	case t.Align != alignBlock && t.Align != alignAny:
		return bad("align %q is neither %q nor %q", t.Align, alignBlock, alignAny)
	}
	if t.Validate != "" {
		if _, ok := builtinValidators()[t.Validate]; !ok {
			return bad("unknown validator %q", t.Validate)
		}
	}
	for i, p := range t.Headers {
		switch {
		case len(p.Bytes) < 1 || len(p.Bytes) > MaxPatternLen:
			return bad("header %d has %d bytes, want 1..%d", i, len(p.Bytes), MaxPatternLen)
		case p.Offset < 0 || p.Offset > MaxPatternOffset:
			return bad("header %d offset %d is outside 0..%d", i, p.Offset, MaxPatternOffset)
		case p.Mask != nil && len(p.Mask) != len(p.Bytes):
			return bad("header %d mask has %d bytes for %d pattern bytes", i, len(p.Mask), len(p.Bytes))
		}
	}
	return nil
}

// Check applies every rule of the model; the error wraps ErrBadSignature.
func (s *Set) Check() error {
	if !printable(s.ID, maxSetIDLen) || s.ID == "" {
		return fmt.Errorf("%w: set id must be 1-%d printable ASCII characters", ErrBadSignature, maxSetIDLen)
	}
	seen := map[string]bool{}
	for i := range s.Types {
		if err := s.Types[i].check(); err != nil {
			return err
		}
		if seen[s.Types[i].Name] {
			return fmt.Errorf("%w: duplicate type name %q", ErrBadSignature, s.Types[i].Name)
		}
		seen[s.Types[i].Name] = true
	}
	return nil
}

func (t Type) clone() Type {
	c := t
	c.Footer = bytes.Clone(t.Footer)
	c.Headers = make([]Pattern, len(t.Headers))
	for i, p := range t.Headers {
		c.Headers[i] = Pattern{Offset: p.Offset, Bytes: bytes.Clone(p.Bytes), Mask: bytes.Clone(p.Mask)}
	}
	return c
}

// Select returns a copy of the set restricted to the families the names
// enable, and the refined-name filter: family -> the refined names kept (no
// entry means the whole family). A name is a family name (the whole family) or
// a refined name of a family in the set; anything else wraps ErrUnknownType.
// No names selects everything. A family named whole is never narrowed by a
// refined name of the same family.
func (s *Set) Select(names []string) (*Set, map[string][]string, error) {
	out := &Set{ID: s.ID}
	filter := map[string][]string{}
	if len(names) == 0 {
		for _, t := range s.Types {
			out.Types = append(out.Types, t.clone())
		}
		return out, filter, nil
	}
	families := map[string]bool{}
	for _, t := range s.Types {
		families[t.Name] = true
	}
	refined := RefinedTypes()
	whole := map[string]bool{}
	part := map[string]map[string]bool{}
	for _, n := range names {
		if families[n] {
			whole[n] = true
			continue
		}
		found := false
		for fam, list := range refined {
			if families[fam] && slices.Contains(list, n) {
				if part[fam] == nil {
					part[fam] = map[string]bool{}
				}
				part[fam][n] = true
				found = true
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("%w: %q", ErrUnknownType, n)
		}
	}
	for _, t := range s.Types {
		if !whole[t.Name] && part[t.Name] == nil {
			continue
		}
		out.Types = append(out.Types, t.clone())
		if whole[t.Name] {
			continue
		}
		for _, r := range refined[t.Name] { // the table's order, so the filter is deterministic
			if part[t.Name][r] {
				filter[t.Name] = append(filter[t.Name], r)
			}
		}
	}
	return out, filter, nil
}

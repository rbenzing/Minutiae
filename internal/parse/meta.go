package parse

import (
	"errors"
	"fmt"
	"strings"
)

// Meta describes a parser: who it is, what it reads and what it may emit.
type Meta struct {
	Name, Version, Title string
	Platforms            []string     // "android", "ios"
	Emits                []Emit       // every record type the parser may emit, with the payload version it was written for
	Inputs               []InputSpec  // the first entry is the primary role
	Claims               []TableClaim // empty until a claim is a promise the host can check
}

// Emit declares one record type a parser may emit.
type Emit struct {
	Type           string
	PayloadVersion int
}

// InputSpec declares one input role of a parser.
type InputSpec struct {
	Role       string   // [a-z][a-z0-9_]{0,15}
	Globs      []string // primary: absolute; others: relative ("./...")
	Companions []string // suffixes appended to the primary's logical path: "-wal", "-journal", "-shm"
	Required   bool     // the primary role is always required
}

// TableClaim says a parser handles one table of a database role. Role is
// "db"; Table is matched the way SQLite does, ASCII case-insensitively.
type TableClaim struct{ Role, Table string }

// Platforms a parser can declare.
const (
	PlatformAndroid = "android"
	PlatformIOS     = "ios"
)

// Identity returns the identity of this parser build for the given source hash.
func (m Meta) Identity(hash string) Identity {
	return Identity{Name: m.Name, Version: m.Version, Hash: hash}
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool { return isLower(c) || isDigit(c) || (c >= 'A' && c <= 'Z') }

// validName: [A-Za-z0-9][A-Za-z0-9._-]{0,63}.
func validName(s string) bool {
	if s == "" || len(s) > 64 || !isAlnum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isAlnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// validRole: [a-z][a-z0-9_]{0,15}.
func validRole(s string) bool {
	if s == "" || len(s) > 16 || !isLower(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isLower(c) && !isDigit(c) && c != '_' {
			return false
		}
	}
	return true
}

// validVersion: MAJOR.MINOR.PATCH, decimal, no leading zeros, at most 9 digits each.
func validVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 9 || (len(p) > 1 && p[0] == '0') {
			return false
		}
		for i := 0; i < len(p); i++ {
			if !isDigit(p[i]) {
				return false
			}
		}
	}
	return true
}

// asciiFoldEqual compares two strings the way SQLite compares identifiers:
// ASCII letters fold, every other byte (including non-ASCII) is literal.
func asciiFoldEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func validTable(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func allowedCompanion(s string) bool { return s == "-wal" || s == "-journal" || s == "-shm" }

// ValidateMeta checks a Meta against the contract: name, version, title,
// platforms, emits, inputs (the primary first, required, with absolute globs
// of a declared platform; other roles with relative globs; companions from
// -wal, -journal, -shm; every glob compiling) and claims (role "db", the
// primary's role, a non-empty ASCII table, no duplicates).
func ValidateMeta(m Meta) error {
	if !validName(m.Name) {
		return errors.New("parse: meta: name must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}")
	}
	if !validVersion(m.Version) {
		return fmt.Errorf("parse: meta %s: version %q must be MAJOR.MINOR.PATCH", m.Name, m.Version)
	}
	if strings.TrimSpace(m.Title) == "" {
		return fmt.Errorf("parse: meta %s: title is empty", m.Name)
	}
	platforms, err := validatePlatforms(m)
	if err != nil {
		return err
	}
	if err := validateEmits(m); err != nil {
		return err
	}
	if err := validateInputs(m, platforms); err != nil {
		return err
	}
	return validateClaims(m)
}

func validatePlatforms(m Meta) (map[string]bool, error) {
	if len(m.Platforms) == 0 {
		return nil, fmt.Errorf("parse: meta %s: platforms is empty", m.Name)
	}
	set := map[string]bool{}
	for _, p := range m.Platforms {
		if p != PlatformAndroid && p != PlatformIOS {
			return nil, fmt.Errorf("parse: meta %s: unknown platform %q", m.Name, p)
		}
		if set[p] {
			return nil, fmt.Errorf("parse: meta %s: duplicate platform %q", m.Name, p)
		}
		set[p] = true
	}
	return set, nil
}

func validateEmits(m Meta) error {
	if len(m.Emits) == 0 {
		return fmt.Errorf("parse: meta %s: emits is empty", m.Name)
	}
	seen := map[string]bool{}
	for _, e := range m.Emits {
		if e.Type == "" {
			return fmt.Errorf("parse: meta %s: emit with an empty type", m.Name)
		}
		if seen[e.Type] {
			return fmt.Errorf("parse: meta %s: emit type %q is declared twice", m.Name, e.Type)
		}
		seen[e.Type] = true
		if e.PayloadVersion < 1 {
			return fmt.Errorf("parse: meta %s: emit %q has payload version %d, want >= 1", m.Name, e.Type, e.PayloadVersion)
		}
	}
	return nil
}

func validateInputs(m Meta, platforms map[string]bool) error {
	if len(m.Inputs) == 0 {
		return fmt.Errorf("parse: meta %s: inputs is empty", m.Name)
	}
	roles := map[string]bool{}
	for i, in := range m.Inputs {
		if !validRole(in.Role) {
			return fmt.Errorf("parse: meta %s: input %d: role %q must match [a-z][a-z0-9_]{0,15}", m.Name, i, in.Role)
		}
		if roles[in.Role] {
			return fmt.Errorf("parse: meta %s: role %q is declared twice", m.Name, in.Role)
		}
		roles[in.Role] = true
		for _, c := range in.Companions {
			if !allowedCompanion(c) {
				return fmt.Errorf("parse: meta %s: role %q: companion %q is not one of -wal, -journal, -shm", m.Name, in.Role, c)
			}
		}
		if i == 0 {
			if !in.Required {
				return fmt.Errorf("parse: meta %s: the primary role %q must be required", m.Name, in.Role)
			}
			if len(in.Globs) == 0 {
				return fmt.Errorf("parse: meta %s: the primary role %q has no globs", m.Name, in.Role)
			}
		}
		for _, pattern := range in.Globs {
			g, err := CompileGlob(pattern)
			if err != nil {
				return fmt.Errorf("parse: meta %s: role %q: glob: %w", m.Name, in.Role, err)
			}
			switch {
			case i == 0 && g.Relative():
				return fmt.Errorf("parse: meta %s: the primary role %q needs absolute globs, got %q", m.Name, in.Role, pattern)
			case i == 0 && !platforms[strings.TrimSuffix(g.scheme, ":")]:
				return fmt.Errorf("parse: meta %s: primary glob %q names a platform that is not in Platforms", m.Name, pattern)
			case i > 0 && !g.Relative():
				return fmt.Errorf("parse: meta %s: role %q needs relative globs (./...), got %q", m.Name, in.Role, pattern)
			}
		}
	}
	return nil
}

func validateClaims(m Meta) error {
	for i, c := range m.Claims {
		if c.Role != "db" || m.Inputs[0].Role != c.Role {
			return fmt.Errorf("parse: meta %s: claim %d: role %q must be \"db\" and the primary role (%q)", m.Name, i, c.Role, m.Inputs[0].Role)
		}
		if !validTable(c.Table) {
			return fmt.Errorf("parse: meta %s: claim %d: table must be non-empty printable ASCII", m.Name, i)
		}
		for _, prev := range m.Claims[:i] {
			if asciiFoldEqual(prev.Table, c.Table) {
				return fmt.Errorf("parse: meta %s: claim %d: table %q is claimed twice", m.Name, i, c.Table)
			}
		}
	}
	return nil
}

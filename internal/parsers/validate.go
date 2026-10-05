package parsers

import (
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// Validate runs the registry rules over a set and returns every violation:
// parse.ValidateMeta; unique names; the defining package is
// <module>/internal/parsers/<dir> (one directory that is an identifier, not
// parsertest); every emitted type is registered at its current payload version and
// has a validator; no claims while claimsEnabled is false, and when the parser is a
// parse.RowMapper every claim names a mapped table; a source hash is generated.
func Validate(all []parse.Parser) []error {
	return rules{base: parsersPath, pkgPath: PackagePath, hashOf: hashOfMeta, claimsEnabled: claimsEnabled}.validate(all)
}

// rules is Validate with its environment injected, so the tests can exercise
// every rule without a parser package or a generated table.
type rules struct {
	base          string
	pkgPath       func(parse.Parser) string
	hashOf        func(parse.Parser, parse.Meta) (string, bool)
	claimsEnabled bool
}

func (r rules) validate(all []parse.Parser) []error {
	var errs []error
	seen := map[string]bool{}
	for i, p := range all {
		if p == nil {
			errs = append(errs, fmt.Errorf("parsers: entry %d is nil", i))
			continue
		}
		m, err := safeMeta(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("parsers: package %q: Meta panicked: %w", r.pkgPath(p), err))
			continue
		}
		who := "parsers: " + m.Name + "@" + m.Version
		if err := parse.ValidateMeta(m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", who, err))
		}
		if seen[m.Name] {
			errs = append(errs, fmt.Errorf("parsers: duplicate parser name %q", m.Name))
		}
		seen[m.Name] = true
		if !r.validPackage(r.pkgPath(p)) {
			errs = append(errs, fmt.Errorf("%s: package %q is not %s/<name> (one directory named like an identifier, not parsertest)", who, r.pkgPath(p), r.base))
		}
		for _, e := range m.Emits {
			t, ok := records.LookupType(e.Type)
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("%s: emits type %q that is not registered", who, e.Type))
			case t.PayloadVersion != e.PayloadVersion:
				errs = append(errs, fmt.Errorf("%s: emits type %q at payload version %d, registered at %d", who, e.Type, e.PayloadVersion, t.PayloadVersion))
			case t.Validate == nil:
				errs = append(errs, fmt.Errorf("%s: emits type %q, which has no validator", who, e.Type))
			}
		}
		if len(m.Claims) != 0 {
			if !r.claimsEnabled {
				errs = append(errs, fmt.Errorf("%s: declares claims while claims are disabled", who))
			} else if rm, ok := p.(parse.RowMapper); ok {
				for _, c := range parse.ClaimsOutsideMappings(m, rm.Mappings()) {
					errs = append(errs, fmt.Errorf("%s: claims table %q that no mapping handles", who, c.Table))
				}
			}
		}
		if _, ok := r.hashOf(p, m); !ok {
			errs = append(errs, fmt.Errorf("%s: has no generated source hash", who))
		}
	}
	return errs
}

// validPackage reports whether path is <base>/<dir> with dir a Go identifier other
// than parsertest.
func (r rules) validPackage(path string) bool {
	dir, ok := strings.CutPrefix(path, r.base+"/")
	if !ok || dir == "parsertest" || dir == "" {
		return false
	}
	for i := 0; i < len(dir); i++ {
		c := dir[i]
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// safeMeta calls p.Meta once and turns a panic into an error.
func safeMeta(p parse.Parser) (m parse.Meta, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return p.Meta(), nil
}

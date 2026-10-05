package parsers

import (
	"reflect"

	"github.com/rbenzing/minutiae/internal/parse"
)

// anchor lets the package find its own import path without spelling it.
type anchor struct{}

// parsersPath is the import path of this package.
var parsersPath = reflect.TypeFor[anchor]().PkgPath()

// PackagePath is the import path of the package that defines p's dynamic type
// ("" for nil, a func type or an unnamed type). A pointer is followed to the type
// it points to.
func PackagePath(p parse.Parser) string {
	if p == nil {
		return ""
	}
	t := reflect.TypeOf(p)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Func || t.Name() == "" {
		return ""
	}
	return t.PkgPath()
}

// HashOf returns the generated source hash of p, looked up by name@version of
// p.Meta() and the package defining p: a different package cannot inherit
// another parser's identity.
func HashOf(p parse.Parser) (string, bool) { return hashOf(p, generated) }

func hashOf(p parse.Parser, table map[string]identity) (string, bool) {
	if p == nil {
		return "", false
	}
	return hashOfWith(p, p.Meta(), table)
}

// hashOfMeta is HashOf for a caller that already holds p.Meta().
func hashOfMeta(p parse.Parser, m parse.Meta) (string, bool) { return hashOfWith(p, m, generated) }

func hashOfWith(p parse.Parser, m parse.Meta, table map[string]identity) (string, bool) {
	id, ok := table[m.Name+"@"+m.Version]
	if !ok || id.Package == "" || id.Package != PackagePath(p) {
		return "", false
	}
	return id.Hash, true
}

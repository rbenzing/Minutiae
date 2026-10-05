package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// dumpFormat heads every normalised Go file. Changing the dump changes every
// parser hash, so it must be a deliberate bump together with the stream header
// in hash.go.
const dumpFormat = "goast-v1\n"

var (
	posType   = reflect.TypeOf(token.Pos(0))
	tokenType = reflect.TypeOf(token.ILLEGAL)

	// skipFields are never part of the dump: comments (the //go: and //line
	// directives are appended separately), object resolution, and the import
	// list (imports are rendered canonically).
	skipFields = map[string]bool{
		"Doc": true, "Comment": true, "Comments": true,
		"Obj": true, "Scope": true, "Unresolved": true, "Imports": true,
	}
)

// NormalizeGo returns the formatting-insensitive form of one Go file: a
// position-free, comment-free dump of its syntax tree with canonical imports,
// followed by its //go: and //line directive comments in source order.
func NormalizeGo(src []byte, name string) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%s: normalising panicked: %v", name, r)
		}
	}()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var b bytes.Buffer
	b.WriteString(dumpFormat)
	b.WriteString("package ")
	dumpValue(&b, reflect.ValueOf(f.Name))
	b.WriteByte('\n')
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			continue
		}
		dumpValue(&b, reflect.ValueOf(d))
		b.WriteByte('\n')
	}
	lines := map[string]bool{}
	for _, is := range f.Imports {
		pname := "-"
		if is.Name != nil {
			pname = is.Name.Name
		}
		p := is.Path.Value
		if u, uerr := strconv.Unquote(p); uerr == nil {
			p = u
		}
		lines["import "+pname+" "+p] = true
	}
	sorted := make([]string, 0, len(lines))
	for l := range lines {
		sorted = append(sorted, l)
	}
	sort.Strings(sorted)
	for _, l := range sorted {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:") || strings.HasPrefix(c.Text, "//line ") {
				b.WriteString("directive ")
				b.WriteString(c.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.Bytes(), nil
}

// dumpValue renders every non-zero field of v depth-first, in declaration
// order, skipping positions and the fields in skipFields.
func dumpValue(b *bytes.Buffer, v reflect.Value) {
	if v.Type() == posType {
		return
	}
	if v.Type() == tokenType {
		b.WriteString(token.Token(v.Int()).String())
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			b.WriteString("nil")
			return
		}
		dumpValue(b, v.Elem())
	case reflect.Struct:
		t := v.Type()
		b.WriteString(t.Name())
		b.WriteByte('(')
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() || skipFields[sf.Name] || sf.Type == posType {
				continue
			}
			fv := v.Field(i)
			if fv.IsZero() {
				continue
			}
			b.WriteString(sf.Name)
			b.WriteByte('=')
			dumpValue(b, fv)
			b.WriteByte(',')
		}
		b.WriteByte(')')
	case reflect.Slice:
		b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			dumpValue(b, v.Index(i))
			b.WriteByte(',')
		}
		b.WriteByte(']')
	case reflect.String:
		b.WriteString(strconv.Quote(v.String()))
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	default:
		fmt.Fprintf(b, "<%s>", v.Kind())
	}
}

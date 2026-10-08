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
const dumpFormat = "goast-v2\n"

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

	// layoutOnlyPos lists the token.Pos fields whose presence is not syntax of a dumped declaration,
	// with the reason. Every other position field is dumped as its presence (Name=set), never its value.
	layoutOnlyPos = map[string]string{
		"File.FileStart":    "a file is never dumped as a whole; only its declarations are",
		"File.FileEnd":      "a file is never dumped as a whole; only its declarations are",
		"File.Package":      "a file is never dumped as a whole; only its declarations are",
		"Comment.Slash":     "comments are never dumped; the directives are hashed as text",
		"ImportSpec.EndPos": "imports are rendered canonically, never dumped",
		"BasicLit.ValueEnd": "derived from the literal text, which is dumped",
		"Directive.Slash":   "a parsed directive comment is never part of the tree; directives are hashed as text",
		"Directive.ArgsPos": "a parsed directive comment is never part of the tree; directives are hashed as text",
		"DirectiveArg.Pos":  "a parsed directive comment is never part of the tree; directives are hashed as text",
	}
)

// NormalizeGo returns the formatting-insensitive form of one Go file: a
// position-free, comment-free dump of its syntax tree with canonical imports, in which
// every //go: and //line directive precedes the declaration it annotates.
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
	// Every directive comment belongs to the next declaration by source
	// position (blank lines and comments between them do not matter); one
	// after the last declaration belongs to the end of the file.
	var directives []*ast.Comment
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if isDirective(c.Text) {
				directives = append(directives, c)
			}
		}
	}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			continue
		}
		for len(directives) > 0 && directives[0].Pos() < d.Pos() {
			writeDirective(&b, "directive ", directives[0])
			directives = directives[1:]
		}
		// A directive inside the declaration (a //line in a function body) is
		// hashed with it, at the ordinal of the statement it precedes.
		dm := &dumper{b: &b}
		for len(directives) > 0 && directives[0].Pos() < d.End() {
			dm.pending = append(dm.pending, directives[0])
			directives = directives[1:]
		}
		dm.value(reflect.ValueOf(d))
		dm.flushBefore(d.End(), "directive-in-decl ")
		b.WriteByte('\n')
	}
	for _, c := range directives {
		writeDirective(&b, "directive-at-end ", c)
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
	return b.Bytes(), nil
}

// dumpValue renders every non-zero field of v depth-first, in declaration
// order, skipping positions and the fields in skipFields.
func writeDirective(b *bytes.Buffer, prefix string, c *ast.Comment) {
	b.WriteString(prefix)
	b.WriteString(c.Text)
	b.WriteByte('\n')
}

// dumper renders values; pending holds the directives found inside the
// declaration being dumped, written before the statement that follows them.
type dumper struct {
	b       *bytes.Buffer
	pending []*ast.Comment
}

func dumpValue(b *bytes.Buffer, v reflect.Value) { (&dumper{b: b}).value(v) }

// flushBefore writes the pending directives positioned before pos.
func (d *dumper) flushBefore(pos token.Pos, label string) {
	for len(d.pending) > 0 && d.pending[0].Pos() < pos {
		writeDirective(d.b, label, d.pending[0])
		d.pending = d.pending[1:]
	}
}

// hasPendingIn reports whether a pending directive lies inside blk.
func (d *dumper) hasPendingIn(blk *ast.BlockStmt) bool {
	for _, c := range d.pending {
		if c.Pos() > blk.Lbrace && c.Pos() < blk.Rbrace {
			return true
		}
	}
	return false
}

func (d *dumper) block(blk *ast.BlockStmt) {
	b := d.b
	b.WriteString("BlockStmt(List=[")
	for i, s := range blk.List {
		d.flushBefore(s.Pos(), fmt.Sprintf("directive@%d ", i))
		d.value(reflect.ValueOf(s))
		b.WriteByte(',')
	}
	d.flushBefore(blk.Rbrace, "directive ")
	b.WriteString("],)")
}

func (d *dumper) value(v reflect.Value) {
	b := d.b
	if v.Type() == posType {
		return
	}
	if v.Type() == tokenType {
		b.WriteString(token.Token(v.Int()).String())
		return
	}
	if blk, ok := v.Interface().(*ast.BlockStmt); ok && blk != nil && d.hasPendingIn(blk) {
		d.block(blk)
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			b.WriteString("nil")
			return
		}
		d.value(v.Elem())
	case reflect.Struct:
		t := v.Type()
		b.WriteString(t.Name())
		b.WriteByte('(')
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() || skipFields[sf.Name] {
				continue
			}
			if sf.Type == posType {
				if _, layout := layoutOnlyPos[t.Name()+"."+sf.Name]; !layout && v.Field(i).Int() != 0 {
					b.WriteString(sf.Name)
					b.WriteString("=set,")
				}
				continue
			}
			fv := v.Field(i)
			if fv.IsZero() {
				continue
			}
			b.WriteString(sf.Name)
			b.WriteByte('=')
			d.value(fv)
			b.WriteByte(',')
		}
		b.WriteByte(')')
	case reflect.Slice:
		b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			d.value(v.Index(i))
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

// isDirective reports whether a comment is a //go: directive or a line
// directive (//line or /*line); all of them change what the compiler or the
// runtime does, so they are hashed.
func isDirective(text string) bool {
	return strings.HasPrefix(text, "//go:") || strings.HasPrefix(text, "//line ") || strings.HasPrefix(text, "/*line ")
}

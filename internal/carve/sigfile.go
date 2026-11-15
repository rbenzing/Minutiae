package carve

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The examiner signature file: a strict JSON document that adds types to a
// base set. It is hostile input: the size is capped before decoding, keys are
// matched exactly and may not repeat, nothing follows the value, and nothing is
// allocated from a declared value (every count and length is bounded by the
// capped file itself). A refused file never yields a set.
const (
	SigFileFormat         = "minutiae-sig-file/1"
	MaxSigFileBytes       = 64 << 10
	MaxSigFileTypes       = 32
	MergedIDSuffix        = "+sig-file"
	maxSigFileObjectDepth = 3 // top, type, header
)

type sigHeader struct {
	Offset int    `json:"offset"`
	Hex    string `json:"hex"`
	Mask   string `json:"mask"`
}

type sigType struct {
	Name     string      `json:"name"`
	Ext      string      `json:"ext"`
	MIME     string      `json:"mime"`
	Headers  []sigHeader `json:"headers"`
	Footer   string      `json:"footer"`
	MinLen   int64       `json:"min_len"`
	MaxLen   int64       `json:"max_len"`
	Validate string      `json:"validate"`
	Align    string      `json:"align"`
}

type sigFileDoc struct {
	Format string    `json:"format"`
	Types  []sigType `json:"types"`
}

func badSig(format string, args ...any) error {
	return fmt.Errorf("%w: signature file: %s", ErrBadSignature, fmt.Sprintf(format, args...))
}

// sigKey says whether key is allowed in an object of the kind and what its value
// is: "" a scalar, "types" or "headers" an array. (A function, not a table: no
// package state.)
func sigKey(kind, key string) (child string, known bool) {
	switch kind + "." + key {
	case "top.format", "type.name", "type.ext", "type.mime", "type.footer", "type.min_len", "type.max_len",
		"type.validate", "type.align", "header.offset", "header.hex", "header.mask":
		return "", true
	case "top.types":
		return "types", true
	case "type.headers":
		return "headers", true
	}
	return "", false
}

// walkSigObject reads one object of the given kind token by token: every key
// must be an exact known key and may appear once. The shape is walked, so the
// recursion is bounded by the schema, not by the input.
func walkSigObject(dec *json.Decoder, kind string, depth int) error {
	if depth > maxSigFileObjectDepth {
		return badSig("nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return badSig("%v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return badSig("a %s must be an object", kind)
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return badSig("%v", err)
		}
		key, _ := tok.(string)
		child, known := sigKey(kind, key)
		if !known {
			return badSig("unknown key %q in a %s", key, kind)
		}
		if seen[key] {
			return badSig("duplicate key %q in a %s", key, kind)
		}
		seen[key] = true
		switch child {
		case "":
			v, err := dec.Token()
			if err != nil {
				return badSig("%v", err)
			}
			if _, isDelim := v.(json.Delim); isDelim {
				return badSig("key %q must be a scalar", key)
			}
		default:
			sub := "type"
			if child == "headers" {
				sub = "header"
			}
			if err := walkSigArray(dec, sub, depth+1, key); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return badSig("%v", err)
	}
	return nil
}

func walkSigArray(dec *json.Decoder, elem string, depth int, key string) error {
	tok, err := dec.Token()
	if err != nil {
		return badSig("%v", err)
	}
	if tok == nil { // null: the decode leaves the slice empty and the model check refuses it
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return badSig("%q must be an array", key)
	}
	for n := 0; dec.More(); n++ {
		if elem == "type" && n >= MaxSigFileTypes {
			return badSig("more than %d types", MaxSigFileTypes)
		}
		if elem == "header" && n >= MaxHeaders {
			return badSig("more than %d headers", MaxHeaders)
		}
		if err := walkSigObject(dec, elem, depth); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return badSig("%v", err)
	}
	return nil
}

// noTrailing reports whether the decoder has nothing but whitespace left.
func noTrailing(dec *json.Decoder) error {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badSig("data after the document")
	}
	return nil
}

func decodeHex(field, s string, allowEmpty bool) ([]byte, error) {
	if s == "" && allowEmpty {
		return nil, nil
	}
	if len(s) > 2*MaxPatternLen {
		return nil, badSig("%s is longer than %d bytes", field, MaxPatternLen)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, badSig("%s is not hex: %v", field, err)
	}
	return b, nil
}

func (st sigType) toType() (Type, error) {
	t := Type{
		Name: st.Name, Ext: st.Ext, MIME: st.MIME, MinLen: st.MinLen, MaxLen: st.MaxLen,
		Validate: st.Validate, Align: st.Align,
	}
	var err error
	if t.Footer, err = decodeHex("footer", st.Footer, true); err != nil {
		return Type{}, err
	}
	for i, h := range st.Headers {
		p := Pattern{Offset: h.Offset}
		if p.Bytes, err = decodeHex(fmt.Sprintf("header %d hex", i), h.Hex, false); err != nil {
			return Type{}, err
		}
		if p.Mask, err = decodeHex(fmt.Sprintf("header %d mask", i), h.Mask, true); err != nil {
			return Type{}, err
		}
		t.Headers = append(t.Headers, p)
	}
	return t, nil
}

// ParseSigFile parses an examiner signature file and returns base plus its
// types as a new set (id base.ID + MergedIDSuffix). Every error wraps
// ErrBadSignature and no partial set is returned. The base is not modified.
func ParseSigFile(data []byte, base *Set) (*Set, error) {
	if base == nil {
		return nil, badSig("no base set")
	}
	if len(data) > MaxSigFileBytes {
		return nil, badSig("%d bytes, at most %d", len(data), MaxSigFileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := walkSigObject(dec, "top", 1); err != nil {
		return nil, err
	}
	if err := noTrailing(dec); err != nil {
		return nil, err
	}
	var doc sigFileDoc
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, badSig("%v", err)
	}
	if err := noTrailing(dec); err != nil {
		return nil, err
	}
	if doc.Format != SigFileFormat {
		return nil, badSig("format %q, want %q", doc.Format, SigFileFormat)
	}
	if len(doc.Types) < 1 || len(doc.Types) > MaxSigFileTypes {
		return nil, badSig("%d types, want 1..%d", len(doc.Types), MaxSigFileTypes)
	}
	reserved := map[string]bool{}
	for _, t := range base.Types {
		reserved[t.Name] = true
	}
	for _, list := range RefinedTypes() {
		for _, n := range list {
			reserved[n] = true
		}
	}
	out := &Set{ID: base.ID + MergedIDSuffix}
	for _, t := range base.Types {
		out.Types = append(out.Types, t.clone())
	}
	inFile := map[string]bool{}
	for _, st := range doc.Types {
		if reserved[st.Name] {
			return nil, badSig("type name %q is a built-in or refined type name", st.Name)
		}
		if inFile[st.Name] { // a family of several rows is built-in only
			return nil, badSig("duplicate type name %q", st.Name)
		}
		inFile[st.Name] = true
		t, err := st.toType()
		if err != nil {
			return nil, err
		}
		out.Types = append(out.Types, t)
	}
	if err := out.Check(); err != nil {
		return nil, err
	}
	return out, nil
}

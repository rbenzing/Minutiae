package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// readManifestStrict reads manifest.jsonl once: recs is what readManifest returns (lenient decode, the
// same ErrIntegrity for a line that does not parse); strict lists, per line, why the line is not
// exactly what this build writes for the record it decodes to ("manifest line N: ...").
func readManifestStrict(path string) (recs []ManifestRecord, strict []string, err error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, nil, err
	}
	recs = make([]ManifestRecord, 0, len(lines))
	for i, l := range lines {
		var r ManifestRecord
		if err := json.Unmarshal(l, &r); err != nil {
			return nil, nil, fmt.Errorf("manifest line %d: %w: %w", i+1, ErrIntegrity, err)
		}
		recs = append(recs, r)
		if p := manifestLineProblem(l, r); p != "" {
			strict = append(strict, fmt.Sprintf("manifest line %d: %s", i+1, p))
		}
	}
	return recs, strict, nil
}

// manifestLineProblem is "" when line is a JSON object with no duplicate key at any depth, no key this
// build does not define, no key spelled in another case, and no key that json.Marshal(rec) omits; a key
// missing from the line whose zero value equals rec's is tolerated (K25). Else it names the first
// finding by key path (for example `source.derived.fs_tipe: not a field of this build`,
// `size: duplicate key`, `SHA256: key spelled differently from "sha256"`, `source.remote_path: written
// empty, this build omits it`).
func manifestLineProblem(line []byte, rec ManifestRecord) string {
	canonical, err := json.Marshal(rec)
	if err != nil {
		return "cannot be re-encoded: " + err.Error()
	}
	if bytes.Equal(line, canonical) {
		return "" // byte for byte what this build writes for the record: nothing to find
	}
	return manifestLineWalk(line, canonical)
}

// manifestLineWalk is the full key-by-key comparison of line with canonical (json.Marshal of rec);
// manifestLineProblem skips it for a line that is byte for byte canonical.
func manifestLineWalk(line, canonical []byte) string {
	if p := duplicateKey(line); p != "" {
		return p
	}
	var got any
	if err := decodeNumber(line, &got); err != nil {
		return "not valid JSON: " + err.Error()
	}
	if _, ok := got.(map[string]any); !ok {
		return "not a JSON object"
	}

	var want any
	if err := decodeNumber(canonical, &want); err != nil {
		return "cannot be re-decoded: " + err.Error()
	}
	return compareStrict("", reflect.TypeFor[ManifestRecord](), got, want)
}

func decodeNumber(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

// duplicateKey walks the tokens of one JSON value and names the first object key that occurs twice in
// the same object, at any depth. The walk is linear in the line and its stack is bounded by the nesting.
func duplicateKey(line []byte) string {
	type frame struct {
		obj    bool
		expect bool // an object frame: the next token is a key (or the closing brace)
		name   string
		curKey string
		idx    int
		seen   map[string]struct{}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	var stack []frame
	pathOf := func(key string) string {
		var sb strings.Builder
		add := func(seg string) {
			if seg == "" {
				return
			}
			if sb.Len() > 0 && seg[0] != '[' {
				sb.WriteByte('.')
			}
			sb.WriteString(seg)
		}
		for i := range stack {
			add(stack[i].name)
		}
		add(key)
		return sb.String()
	}
	valueDone := func() {
		if len(stack) == 0 {
			return
		}
		top := &stack[len(stack)-1]
		if top.obj {
			top.expect = true
		} else {
			top.idx++
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return "" // the lenient decode has already accepted the line (or this is the end of it)
		}
		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			if top.obj && top.expect {
				if d, ok := tok.(json.Delim); ok && d == '}' {
					stack = stack[:len(stack)-1]
					valueDone()
					continue
				}
				k, ok := tok.(string)
				if !ok {
					return ""
				}
				if _, dup := top.seen[k]; dup {
					return pathOf(k) + ": duplicate key"
				}
				top.seen[k] = struct{}{}
				top.curKey = k
				top.expect = false
				continue
			}
		}
		d, isDelim := tok.(json.Delim)
		switch {
		case isDelim && (d == '{' || d == '['):
			name := ""
			if len(stack) > 0 {
				top := &stack[len(stack)-1]
				if top.obj {
					name = top.curKey
				} else {
					name = fmt.Sprintf("[%d]", top.idx)
				}
			}
			stack = append(stack, frame{obj: d == '{', expect: d == '{', name: name, seen: map[string]struct{}{}})
		case isDelim: // a closing bracket
			if len(stack) == 0 {
				return ""
			}
			stack = stack[:len(stack)-1]
			valueDone()
		default:
			valueDone()
		}
	}
}

// strictFields maps the JSON key names of a struct type to their field types.
var strictFields sync.Map // reflect.Type -> map[string]reflect.Type

func fieldsOf(t reflect.Type) map[string]reflect.Type {
	if m, ok := strictFields.Load(t); ok {
		return m.(map[string]reflect.Type)
	}
	m := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		m[name] = f.Type
	}
	strictFields.Store(t, m)
	return m
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// compareStrict walks the line's generic value (got) against the generic form of json.Marshal of the
// decoded record (want), in parallel with the Go type, and names the first difference in the key set.
func compareStrict(path string, t reflect.Type, got, want any) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch g := got.(type) {
	case map[string]any:
		w, _ := want.(map[string]any)
		if t != nil && t.Kind() == reflect.Map {
			for _, k := range sortedKeys(g) {
				if _, ok := w[k]; ok {
					if p := compareStrict(joinPath(path, k), t.Elem(), g[k], w[k]); p != "" {
						return p
					}
				}
			}
			return ""
		}
		if t == nil || t.Kind() != reflect.Struct {
			return ""
		}
		fields := fieldsOf(t)
		for _, k := range sortedKeys(g) {
			wv, inWant := w[k]
			if inWant {
				if p := compareStrict(joinPath(path, k), fields[k], g[k], wv); p != "" {
					return p
				}
				continue
			}
			if _, isField := fields[k]; isField {
				return fmt.Sprintf("%s: written empty, this build omits it", joinPath(path, k))
			}
			for _, cand := range sortedKeys(w) {
				if strings.EqualFold(cand, k) {
					return fmt.Sprintf("%s: key spelled differently from %q", joinPath(path, k), cand)
				}
			}
			for cand := range fields {
				if strings.EqualFold(cand, k) {
					return fmt.Sprintf("%s: key spelled differently from %q", joinPath(path, k), cand)
				}
			}
			return fmt.Sprintf("%s: not a field of this build", joinPath(path, k))
		}
		for _, k := range sortedKeys(w) {
			if _, ok := g[k]; ok || isZeroJSON(w[k]) {
				continue
			}
			return fmt.Sprintf("%s: missing", joinPath(path, k))
		}
	case []any:
		w, _ := want.([]any)
		if t == nil || t.Kind() != reflect.Slice || len(w) != len(g) {
			return ""
		}
		for i := range g {
			if p := compareStrict(fmt.Sprintf("%s[%d]", path, i), t.Elem(), g[i], w[i]); p != "" {
				return p
			}
		}
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// isZeroJSON reports whether a generic JSON value is the zero value of its Go type (K25).
func isZeroJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 0
	case map[string]any:
		for _, e := range x {
			if !isZeroJSON(e) {
				return false
			}
		}
		return true
	case []any:
		for _, e := range x {
			if !isZeroJSON(e) {
				return false
			}
		}
		return true
	}
	return false
}

// manifestStrict is Manifest for case verify: the same single read under the manifest lock, plus the
// strictness findings of readManifestStrict. The open path (Manifest, OpenArtifact) stays lenient.
func (c *Case) manifestStrict() ([]ManifestRecord, []string, error) {
	c.manifestMu.Lock()
	defer c.manifestMu.Unlock()
	c.manifestReads.Add(1)
	return readManifestStrict(filepath.Join(c.Dir, manifestFile))
}

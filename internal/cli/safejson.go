package cli

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// This file is the one JSON writer of the CLI (TestCLIJSONGoesThroughTheSafeWriter). Its output is the
// encoding/json output with every non-graphic rune written as a \uXXXX escape, so a JSON line shown raw
// on a terminal cannot reorder or hide text, and a consumer that decodes it gets exactly the stored
// strings. A string that is not valid UTF-8 is refused: encoding/json would replace each bad byte by
// U+FFFD, which is a repair of evidence.

var (
	// errJSONNotUTF8 refuses JSON output that holds a string which is not valid UTF-8.
	errJSONNotUTF8 = errors.New("JSON output would not be valid UTF-8")
	// errJSONTooDeep refuses a value nested deeper than the pre-check follows.
	errJSONTooDeep = errors.New("JSON output is nested too deeply to check")
)

// maxJSONCheckDepth bounds the pre-check walk (above maxPayloadPrintDepth).
const maxJSONCheckDepth = 256

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// marshalSafe is the compact, terminal-safe form of v, without a trailing newline.
func marshalSafe(v any) ([]byte, error) {
	if err := checkJSONUTF8(v); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return terminalSafeJSON(b)
}

// writeJSON writes v indented with a trailing newline, terminal-safe, in one Write; on a refusal it
// writes nothing.
func writeJSON(w io.Writer, v any) error {
	if err := checkJSONUTF8(v); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	b, err := terminalSafeJSON(buf.Bytes())
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// writeNDJSON writes v as one compact terminal-safe line.
func writeNDJSON(w io.Writer, v any) error {
	b, err := marshalSafe(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// writeJSONArtifact is the plain encoder, for the device/info.json case file: it is a hashed artifact,
// not terminal output, and its bytes must not change between captures.
func writeJSONArtifact(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// joinJSON reports a JSON refusal after the command's own error, so an integrity failure still decides
// the exit code (ExitCode tests ErrIntegrity first).
func joinJSON(result, jsonErr error) error {
	switch {
	case jsonErr == nil:
		return result
	case result == nil:
		return jsonErr
	}
	return errors.Join(result, jsonErr)
}

// terminalSafeJSON rewrites the string tokens of encoded JSON: U+007F and every rune from U+0080 on that
// is not graphic becomes a \u escape (a surrogate pair above U+FFFF). Everything else is copied. Invalid
// UTF-8 inside a string is an error. The result decodes to what b decodes to.
func terminalSafeJSON(b []byte) ([]byte, error) {
	out := make([]byte, 0, len(b)+16)
	inString := false
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case !inString:
			inString = c == '"'
			out = append(out, c)
			i++
		case c == '\\':
			end := min(i+2, len(b))
			out = append(out, b[i:end]...)
			i = end
		case c == '"':
			inString = false
			out = append(out, c)
			i++
		case c == 0x7f:
			out = append(out, `\u007f`...)
			i++
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			r, n := utf8.DecodeRune(b[i:])
			switch {
			case r == utf8.RuneError && n <= 1:
				return nil, fmt.Errorf("%w: at output byte %d", errJSONNotUTF8, i)
			case unicode.IsGraphic(r):
				out = append(out, b[i:i+n]...)
			case r > 0xffff:
				hi, lo := utf16.EncodeRune(r)
				out = fmt.Appendf(out, `\u%04x\u%04x`, hi, lo)
			default:
				out = fmt.Appendf(out, `\u%04x`, r)
			}
			i += n
		}
	}
	return out, nil
}

// checkJSONUTF8 walks v the way encoding/json does and refuses the first string or map key that is not
// valid UTF-8, naming its path. It stops at json.Marshaler, encoding.TextMarshaler and []byte values
// (their output is checked after encoding) and skips fields the encoder would not write.
func checkJSONUTF8(v any) error {
	return checkJSONValue(reflect.ValueOf(v), "", 0)
}

func jsonPathOrTop(path string) string {
	if path == "" {
		return "(top level)"
	}
	return path
}

func jsonFieldPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func implementsMarshaler(v reflect.Value) bool {
	t := v.Type()
	for _, m := range []reflect.Type{jsonMarshalerType, textMarshalerType} {
		if t.Implements(m) || (v.CanAddr() && reflect.PointerTo(t).Implements(m)) {
			return true
		}
	}
	return false
}

func checkJSONValue(v reflect.Value, path string, depth int) error {
	if !v.IsValid() {
		return nil
	}
	if depth > maxJSONCheckDepth {
		return fmt.Errorf("%w: at %s", errJSONTooDeep, jsonPathOrTop(path))
	}
	if implementsMarshaler(v) {
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("%w: at %s", errJSONNotUTF8, jsonPathOrTop(path))
		}
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		// no depth step: an interface cannot hold itself, and a loop needs a pointer, map or slice
		return checkJSONValue(v.Elem(), path, depth)
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return checkJSONValue(v.Elem(), path, depth+1)
	case reflect.Struct:
		return checkJSONStruct(v, path, depth)
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			k := it.Key()
			if k.Kind() == reflect.String && !implementsMarshaler(k) && !utf8.ValidString(k.String()) {
				return fmt.Errorf("%w: at %s, a map key (%s)", errJSONNotUTF8, jsonPathOrTop(path), strconv.QuoteToASCII(k.String()))
			}
			if err := checkJSONValue(it.Value(), fmt.Sprintf("%s[%q]", path, k), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil // base64 text or numbers
		}
		for i := range v.Len() {
			if err := checkJSONValue(v.Index(i), path+"["+strconv.Itoa(i)+"]", depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkJSONStruct(v reflect.Value, path string, depth int) error {
	t := v.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := sf.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if sf.Anonymous {
			if !sf.IsExported() && ft.Kind() != reflect.Struct {
				continue
			}
		} else if !sf.IsExported() {
			continue
		}
		fv := v.Field(i)
		if hasJSONOption(opts, "omitempty") && jsonEmpty(fv) {
			continue
		}
		if hasJSONOption(opts, "omitzero") && fv.IsZero() {
			continue
		}
		if sf.Anonymous && name == "" && ft.Kind() == reflect.Struct && !implementsMarshaler(fv) {
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue
				}
				fv = fv.Elem()
			}
			if depth+1 > maxJSONCheckDepth {
				return fmt.Errorf("%w: at %s", errJSONTooDeep, jsonPathOrTop(path))
			}
			if err := checkJSONStruct(fv, path, depth+1); err != nil {
				return err
			}
			continue
		}
		if name == "" {
			name = sf.Name
		}
		if err := checkJSONValue(fv, jsonFieldPath(path, name), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func hasJSONOption(opts, want string) bool {
	for o := range strings.SplitSeq(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// jsonEmpty is encoding/json's omitempty test.
func jsonEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return v.IsZero()
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// jsonStream guards a JSON output that is written element by element (an array streamed by image ls,
// the events of parse run). Once one element is refused or fails to write, nothing more is written, and
// finish says how far the output got.
type jsonStream struct {
	w, errw io.Writer
	n       int
	err     error
}

// emit runs write for the next element unless an earlier one failed. write must produce nothing when
// it fails (marshalSafe checks an element before any byte is written).
func (s *jsonStream) emit(write func() error) {
	if s.err != nil {
		return
	}
	if err := write(); err != nil {
		s.err = err
		return
	}
	s.n++
}

// ndjson emits v as one line.
func (s *jsonStream) ndjson(v any) { s.emit(func() error { return writeNDJSON(s.w, v) }) }

// finish returns the first error and, when there is one, tells errw where the output stopped.
func (s *jsonStream) finish() error {
	if s.err != nil {
		fmt.Fprintf(s.errw, "output truncated after element %d: %s\n", s.n, escapeText(s.err.Error()))
	}
	return s.err
}

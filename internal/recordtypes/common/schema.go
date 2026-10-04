package common

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Kind is the type a schema field must have.
type Kind int

// The kinds of a Field. Integers accept int, int64, uint64 (up to MaxInt64) and a
// json.Number holding a canonical integer (no fraction, no exponent); numbers also
// accept a finite float64.
const (
	KString      Kind = iota + 1 // string (NonEmpty, MaxLen)
	KToken                       // string matching [a-z][a-z0-9_]{0,31}
	KEnum                        // string from Enum, or "unknown"
	KBool                        // bool
	KInt                         // integer (Min)
	KNumber                      // finite number (Min)
	KIDString                    // non-empty string (MaxLen, default 1024 bytes) or an integer
	KHex64                       // 64 lowercase hex digits
	KDate                        // YYYY-MM-DD or --MM-DD
	KObject                      // object; Obj is its schema (nil: any object within the depth limit)
	KArray                       // array of at most MaxLen (default 10,000) elements; Obj is the element schema (nil: any value)
	KStringArray                 // array of strings, at most MaxLen (default 10,000)
	KRaw                         // object of scalars and nested values within the depth limit
)

// Limits of the checker.
const (
	// MaxContainerDepth is the deepest container nesting a payload may have; the
	// payload object itself is level 1.
	MaxContainerDepth = 4

	defaultMaxArray = 10000
	defaultIDLen    = 1024
	// maxCheckedValues bounds the work of one Validate: with arrays of 10,000
	// entries nested 4 deep a payload could otherwise cost 10^12 steps.
	maxCheckedValues = 1 << 20
)

// Field describes one field of a payload object.
type Field struct {
	Name     string
	Kind     Kind
	Required bool
	NonEmpty bool     // KString
	Enum     []string // KEnum ("unknown" is always accepted)
	MaxLen   int      // KString bytes; KArray entries (default 10,000)
	Min      *int64   // KInt, KNumber
	Obj      *Schema  // KObject; element schema of KArray
}

// Schema is the contract of one payload object: its fields (anything else is
// allowed), a rule that at least one of several fields is present and non-empty,
// and a cross-field rule that runs after the field checks.
type Schema struct {
	Fields     []Field
	AtLeastOne []string                     // at least one of these present and non-empty
	Cross      func(m map[string]any) error // after the field checks
}

// SchemaError is the error Validate returns for a violation: the path of the
// field and what is wrong with it. It never contains a value of the payload.
type SchemaError struct {
	Path   string
	Reason string
}

func (e *SchemaError) Error() string { return e.Path + ": " + e.Reason }

// checker carries the work budget of one Validate.
type checker struct{ nodes int }

func (c *checker) tick(path string) error {
	c.nodes++
	if c.nodes > maxCheckedValues {
		return &SchemaError{path, "the payload is too large to check"}
	}
	return nil
}

// Validate checks payload against the schema and returns the first violation in
// field order, or nil. The errors are value-free ("payload.participants[3].role:
// not one of from,to,cc,bcc,member"): they name paths, kinds and schema constants
// and never a payload value, because a device controls the values. A nil payload
// is an empty one. Validate never panics and does bounded work. It has a value
// receiver so a package-level Schema can only ever be used, not modified.
func (s Schema) Validate(payload map[string]any) (err error) {
	defer func() {
		if recover() != nil {
			err = &SchemaError{"payload", "the payload could not be checked"}
		}
	}()
	return (&checker{}).object("payload", &s, payload, 1)
}

func (c *checker) object(path string, s *Schema, m map[string]any, depth int) error {
	if depth > MaxContainerDepth {
		return depthError(path)
	}
	if err := c.tick(path); err != nil {
		return err
	}
	for i := range s.Fields {
		f := &s.Fields[i]
		v, ok := m[f.Name]
		if !ok {
			if f.Required {
				return &SchemaError{path + "." + f.Name, "required field is missing"}
			}
			continue
		}
		if err := c.value(path+"."+f.Name, f, v, depth); err != nil {
			return err
		}
	}
	if len(s.AtLeastOne) > 0 {
		found := false
		for _, name := range s.AtLeastOne {
			if nonEmpty(m[name]) {
				found = true
				break
			}
		}
		if !found {
			return &SchemaError{path, "at least one of " + strings.Join(s.AtLeastOne, ",") + " must be present and not empty"}
		}
	}
	if s.Cross != nil {
		if err := s.Cross(m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func depthError(path string) error {
	return &SchemaError{path, "nested deeper than " + strconv.Itoa(MaxContainerDepth) + " levels"}
}

func nonEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// value checks one present value; depth is the depth of the object that holds it.
func (c *checker) value(path string, f *Field, v any, depth int) error {
	if err := c.tick(path); err != nil {
		return err
	}
	bad := func(want string) error { return &SchemaError{path, "must be " + want + ", not " + typeName(v)} }
	switch f.Kind {
	case KString:
		s, ok := v.(string)
		if !ok {
			return bad("a string")
		}
		if f.NonEmpty && s == "" {
			return &SchemaError{path, "must not be empty"}
		}
		if f.MaxLen > 0 && len(s) > f.MaxLen {
			return &SchemaError{path, "longer than " + strconv.Itoa(f.MaxLen) + " bytes"}
		}
	case KToken:
		s, ok := v.(string)
		if !ok {
			return bad("a string")
		}
		if !validToken(s) {
			return &SchemaError{path, "not a token (a lowercase letter, then up to 31 lowercase letters, digits or underscores)"}
		}
	case KEnum:
		s, ok := v.(string)
		if !ok {
			return bad("a string")
		}
		if s != "unknown" && !slices.Contains(f.Enum, s) {
			return &SchemaError{path, "not one of " + strings.Join(f.Enum, ",")}
		}
	case KBool:
		if _, ok := v.(bool); !ok {
			return bad("a boolean")
		}
	case KInt:
		n, ok := asInt(v)
		if !ok {
			return bad("an integer")
		}
		if f.Min != nil && n < *f.Min {
			return &SchemaError{path, "below the minimum " + strconv.FormatInt(*f.Min, 10)}
		}
	case KNumber:
		x, ok := asNumber(v)
		if !ok {
			return bad("a finite number")
		}
		if f.Min != nil && x < float64(*f.Min) {
			return &SchemaError{path, "below the minimum " + strconv.FormatInt(*f.Min, 10)}
		}
	case KIDString:
		if s, ok := v.(string); ok {
			limit := f.MaxLen
			if limit <= 0 {
				limit = defaultIDLen
			}
			if s == "" {
				return &SchemaError{path, "must not be empty"}
			}
			if len(s) > limit {
				return &SchemaError{path, "longer than " + strconv.Itoa(limit) + " bytes"}
			}
		} else if _, ok := asInt(v); !ok {
			return bad("a string or an integer")
		}
	case KHex64:
		s, ok := v.(string)
		if !ok {
			return bad("a string")
		}
		if !validHex64(s) {
			return &SchemaError{path, "not 64 lowercase hex digits"}
		}
	case KDate:
		s, ok := v.(string)
		if !ok {
			return bad("a string")
		}
		if !validDate(s) {
			return &SchemaError{path, "not a date (YYYY-MM-DD or --MM-DD)"}
		}
	case KObject:
		m, ok := v.(map[string]any)
		if !ok {
			return bad("an object")
		}
		if f.Obj != nil {
			return c.object(path, f.Obj, m, depth+1)
		}
		return c.free(path, m, depth+1)
	case KRaw:
		m, ok := v.(map[string]any)
		if !ok {
			return bad("an object")
		}
		return c.free(path, m, depth+1)
	case KArray, KStringArray:
		arr, ok := v.([]any)
		if !ok {
			return bad("an array")
		}
		if depth+1 > MaxContainerDepth {
			return depthError(path)
		}
		limit := f.MaxLen
		if limit <= 0 {
			limit = defaultMaxArray
		}
		if len(arr) > limit {
			return &SchemaError{path, "more than " + strconv.Itoa(limit) + " entries"}
		}
		for i, el := range arr {
			epath := path + "[" + strconv.Itoa(i) + "]"
			if err := c.tick(epath); err != nil {
				return err
			}
			switch {
			case f.Kind == KStringArray:
				if _, ok := el.(string); !ok {
					return &SchemaError{epath, "must be a string, not " + typeName(el)}
				}
			case f.Obj != nil:
				em, ok := el.(map[string]any)
				if !ok {
					return &SchemaError{epath, "must be an object, not " + typeName(el)}
				}
				if err := c.object(epath, f.Obj, em, depth+2); err != nil {
					return err
				}
			default:
				if err := c.free(epath, el, depth+2); err != nil {
					return err
				}
			}
		}
	default:
		return &SchemaError{path, "the schema gives this field no known kind"}
	}
	return nil
}

// free checks a value of no schema: a scalar of a type the records package
// canonicalizes, or a container within the depth limit and the array cap. Keys are
// not named in errors (they are parser-chosen but unbounded in number).
func (c *checker) free(path string, v any, depth int) error {
	if err := c.tick(path); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil, bool, string, int, int64, uint64:
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return &SchemaError{path, "holds a number that is not finite"}
		}
	case []any:
		if depth > MaxContainerDepth {
			return depthError(path)
		}
		if len(x) > defaultMaxArray {
			return &SchemaError{path, "holds an array of more than " + strconv.Itoa(defaultMaxArray) + " entries"}
		}
		for _, el := range x {
			if err := c.free(path, el, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		if depth > MaxContainerDepth {
			return depthError(path)
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if err := c.free(path, x[k], depth+1); err != nil {
				return err
			}
		}
	default:
		if n, ok := v.(numberLike); ok {
			if _, ok := asNumber(n); !ok {
				return &SchemaError{path, "holds a number that is not a finite JSON number"}
			}
			return nil
		}
		return &SchemaError{path, "holds a value of an unsupported type"}
	}
	return nil
}

// numberLike is what a json.Number offers; the package matches it by method set
// instead of importing encoding/json, which a shared helper has no other use for.
type numberLike interface {
	String() string
	Int64() (int64, error)
	Float64() (float64, error)
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case int, int64, uint64, float64:
		return "a number"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	case numberLike:
		return "a number"
	}
	return "a value of an unsupported type"
}

// asInt reads an integer: int, int64, uint64 up to MaxInt64, or a json.Number that
// is a canonical JSON integer (an optional minus and digits, no fraction and no
// exponent) within int64. A decimal or exponent form such as 1e3 or 2.0 is refused
// even when it is integral, so a typed reader that parses integers as integers
// (Decode) can never drop a value the validator accepted.
func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case numberLike:
		s := x.String()
		if !jsonNumber(s) || strings.ContainsAny(s, ".eE") {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// asNumber reads a finite number: an integer type, a finite float64, or a
// json.Number that is a JSON number and fits a float64.
func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case numberLike:
		s := x.String()
		if !jsonNumber(s) {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// jsonNumber reports whether s is a number in the JSON grammar (so "NaN", "Inf",
// hex floats and underscores, which strconv would accept, are not).
func jsonNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && isASCIIDigit(s[i]) {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && isASCIIDigit(s[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && isASCIIDigit(s[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}

func validToken(s string) bool {
	if len(s) < 1 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && !isASCIIDigit(c) && c != '_' {
			return false
		}
	}
	return true
}

func validHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isASCIIDigit(c) && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validDate accepts YYYY-MM-DD (a real calendar day) and --MM-DD (a birthday
// without a year: February 29 is allowed).
func validDate(s string) bool {
	digits := func(t string) (int, bool) {
		n := 0
		for i := 0; i < len(t); i++ {
			if !isASCIIDigit(t[i]) {
				return 0, false
			}
			n = n*10 + int(t[i]-'0')
		}
		return n, true
	}
	var year, month, day int
	var ok1, ok2, ok3 bool
	switch {
	case len(s) == 10 && s[4] == '-' && s[7] == '-':
		year, ok1 = digits(s[:4])
		month, ok2 = digits(s[5:7])
		day, ok3 = digits(s[8:])
	case len(s) == 7 && s[0] == '-' && s[1] == '-' && s[4] == '-':
		year, ok1 = 0, true // treated as a leap year: February 29 is possible
		month, ok2 = digits(s[2:4])
		day, ok3 = digits(s[5:])
	default:
		return false
	}
	return ok1 && ok2 && ok3 && month >= 1 && month <= 12 && day >= 1 && day <= daysIn(year, month)
}

func daysIn(year, month int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// CommonFields returns fresh definitions of the optional fields every type
// accepts: raw (KRaw); recovery (an object whose relation, when present, is one
// of the C10 vocabulary or "unknown", and whose other keys are free scalars or
// containers within the depth limit); snapshot (an object {name: non-empty
// string, xid: integer >= 0}); and deleted (an object with a non-empty string
// source). It is a function so that no exported package state exists, and every
// call returns values nobody else holds.
func CommonFields() []Field { //nolint:revive // the name is part of the plan's interface (used by every record type); Fields would hide what it returns
	zero := int64(0)
	return []Field{
		{Name: "raw", Kind: KRaw},
		{Name: "recovery", Kind: KObject, Obj: &Schema{
			Fields: []Field{{Name: "relation", Kind: KEnum, Enum: []string{"absent-from-live", "superseded-version", "uncommitted", "from-recovered-artifact"}}},
			Cross: func(m map[string]any) error {
				c := &checker{}
				for _, k := range slices.Sorted(maps.Keys(m)) {
					if k == "relation" {
						continue
					}
					if c.free("", m[k], 3) != nil { // the recovery object is level 2: its values are level 3
						return errors.New("another key holds an unsupported value or nests too deep")
					}
				}
				return nil
			},
		}},
		{Name: "snapshot", Kind: KObject, Obj: &Schema{Fields: []Field{
			{Name: "name", Kind: KString, Required: true, NonEmpty: true},
			{Name: "xid", Kind: KInt, Required: true, Min: &zero},
		}}},
		{Name: "deleted", Kind: KObject, Obj: &Schema{Fields: []Field{
			{Name: "source", Kind: KString, Required: true, NonEmpty: true},
		}}},
	}
}

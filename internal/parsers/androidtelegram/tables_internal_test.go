package androidtelegram

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// table is the generated table, built once for the tests (a parser itself holds no package state, P5).
var table = newTable()

const constructorsTSV = "../../../tools/fixtures/schemas/telegram-android/constructors.tsv"

var (
	notationGate  = regexp.MustCompile(`^\[(F2?)\.([0-9]+):$`)
	notationField = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.]*)(?:=(F2?)\.([0-9]+)|:(.+?))(\]?)$`)
	notationID    = regexp.MustCompile(`^id(32|64)@(user|chat|channel|unknown)$`)
	notationKinds = map[string]tl.Kind{
		"i32": tl.KInt32, "i64": tl.KInt64, "dbl": tl.KDouble, "bool": tl.KBool, "bytes": tl.KBytes,
		"str": tl.KString, "secret": tl.KSecret, "#": tl.KFlags, "vec<i32>": tl.KVecInt32, "vec<i64>": tl.KVecInt64,
	}
	notationTypes = map[string]string{
		"Message": typeMessage, "Peer": typePeer, "MessageFwdHeader": typeFwd, "MessageReplyHeader": typeReply,
		"MessageEntity": typeEntity, "MessageAction": typeAction, "PhoneCallDiscardReason": typeReason,
		"MessageMedia": typeMedia, "User": typeUser, "Chat": typeChat,
	}
)

func flagsWord(f string) string {
	if f == "F2" {
		return "flags2"
	}
	return "flags"
}

func typeName(t string) string {
	if c, ok := notationTypes[t]; ok {
		return c
	}
	return t
}

// parseNotation reads the fields column of constructors.tsv (the 4I4-P4 grammar) into the walker's fields,
// independently of the generator that wrote tables.go.
func parseNotation(s string) ([]tl.Field, error) {
	var out []tl.Field
	gate := ""
	bit := 0
	for tok := range strings.FieldsSeq(s) {
		if m := notationGate.FindStringSubmatch(tok); m != nil {
			if gate != "" {
				return nil, fmt.Errorf("gate inside a gate at %q", tok)
			}
			gate = flagsWord(m[1])
			bit, _ = strconv.Atoi(m[2])
			continue
		}
		m := notationField.FindStringSubmatch(tok)
		if m == nil {
			return nil, fmt.Errorf("token %q is outside the grammar", tok)
		}
		f := tl.Field{Name: m[1]}
		if m[2] != "" { // name=F.n: a flag-only true
			f.Kind, f.Flags = tl.KTrue, flagsWord(m[2])
			f.Bit, _ = strconv.Atoi(m[3])
		} else {
			typ := m[4]
			switch {
			case notationKinds[typ] != 0:
				f.Kind = notationKinds[typ]
			case notationID.MatchString(typ):
				id := notationID.FindStringSubmatch(typ)
				f.Kind, f.Class = tl.KID32, id[2]
				if id[1] == "64" {
					f.Kind = tl.KID64
				}
			case strings.HasPrefix(typ, "vec<") && strings.HasSuffix(typ, ">"):
				f.Kind, f.Of = tl.KVector, typeName(typ[4:len(typ)-1])
			default:
				f.Kind, f.Of = tl.KObject, typeName(typ)
			}
			if gate != "" {
				f.Flags, f.Bit = gate, bit
			}
		}
		out = append(out, f)
		if m[5] == "]" {
			if gate == "" {
				return nil, fmt.Errorf("%q closes a gate that is not open", tok)
			}
			gate = ""
		}
	}
	if gate != "" {
		return nil, errors.New("a gate is never closed")
	}
	return out, nil
}

type tsvRow struct {
	id                                  uint32
	typ, class, body, pin, source, flds string
}

func loadConstructors(t *testing.T) []tsvRow {
	t.Helper()
	raw, err := os.ReadFile(constructorsTSV)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	var rows []tsvRow
	for _, l := range lines[1:] {
		c := strings.Split(l, "\t")
		if len(c) != 7 {
			t.Fatalf("row %q has %d columns", l, len(c))
		}
		id, err := strconv.ParseUint(c[1], 0, 32)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, tsvRow{uint32(id), c[0], c[2], c[3], c[4], c[5], c[6]})
	}
	return rows
}

var bodyOf = map[string]tl.Body{"full": tl.BodyFull, "prefix": tl.BodyPrefix, "opaque": tl.BodyOpaque}

// tableDiffs compares a table with the facts, both ways, by (type, ID) (plan 4I4c W1, ruling C2: Go accepts a duplicate
// struct key in a map literal, so a duplicate (type, ID) row is reported here), and returns one line per difference.
func tableDiffs(tab tl.Table, rows []tsvRow) []string {
	var out []string
	have := map[tl.Key]bool{}
	for _, r := range rows {
		k := tl.Key{Type: typeName(r.typ), ID: r.id}
		if have[k] {
			out = append(out, fmt.Sprintf("%#08x %s %s: duplicate (type, id) row", r.id, r.typ, r.class))
			continue
		}
		have[k] = true
		c, ok := tab[k]
		if !ok {
			out = append(out, fmt.Sprintf("%#08x %s: no table entry", r.id, r.class))
			continue
		}
		want, err := parseNotation(r.flds)
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("%#08x %s: %v", r.id, r.class, err))
		case c.ID != r.id || c.Name != r.class || c.Type != typeName(r.typ) || c.Body != bodyOf[r.body] ||
			c.Source != r.source+"@"+r.pin:
			out = append(out, fmt.Sprintf("%#08x %s: header differs: %+v", r.id, r.class, c))
		case len(want) != len(c.Fields) || (len(want) > 0 && !reflect.DeepEqual(want, c.Fields)):
			out = append(out, fmt.Sprintf("%#08x %s: fields differ", r.id, r.class))
		}
	}
	for k := range tab {
		if !have[k] {
			out = append(out, fmt.Sprintf("%#08x %s: table entry without a row", k.ID, k.Type))
		}
	}
	return out
}

// TestTelegramConstructorTableMatchesFacts pins plan 4I4-P5 (spec 8.2): the generated table equals the
// committed facts, both ways, and the comparison itself catches a planted extra entry and a field swap.
func TestTelegramConstructorTableMatchesFacts(t *testing.T) {
	rows := loadConstructors(t)
	if len(rows) < 200 {
		t.Fatalf("only %d fact rows", len(rows))
	}
	if d := tableDiffs(table, rows); len(d) != 0 {
		t.Fatalf("%d differences, first: %s", len(d), d[0])
	}
	if len(table) != len(rows) {
		t.Fatalf("%d entries, %d rows", len(table), len(rows))
	}

	extra := maps.Clone(table)
	extra[tl.Key{Type: typeMessage, ID: 0xdeadbeef}] = tl.Constructor{ID: 0xdeadbeef, Name: "TL_planted", Type: typeMessage, Body: tl.BodyOpaque, Source: "X.java:1@A-HEAD"}
	if d := tableDiffs(extra, rows); len(d) != 1 || !strings.Contains(d[0], "without a row") {
		t.Errorf("a planted extra entry is not reported alone: %v", d)
	}

	// Ruling C2: a fact row repeated (the same type and ID twice) is reported once, as a duplicate.
	if d := tableDiffs(table, append(slices.Clone(rows), rows[0])); len(d) != 1 || !strings.Contains(d[0], "duplicate") {
		t.Errorf("a repeated (type, id) row is not reported alone as a duplicate: %v", d)
	}
	// A row whose (type, ID) the table lacks (the same ID under another type) is reported, and so is the entry left
	// without its row.
	moved := slices.Clone(rows)
	moved[0].typ = "Planted"
	if d := tableDiffs(table, moved); len(d) != 2 || !strings.Contains(d[0], "no table entry") || !strings.Contains(d[1], "without a row") {
		t.Errorf("a row of another type is not reported as a missing pair: %v", d)
	}

	swapped := maps.Clone(table)
	var victim tl.Key
	for _, r := range rows {
		k := tl.Key{Type: typeName(r.typ), ID: r.id}
		if f := table[k].Fields; len(f) >= 2 && !reflect.DeepEqual(f[0], f[1]) {
			victim = k
			break
		}
	}
	c := swapped[victim]
	c.Fields = slices.Clone(c.Fields)
	c.Fields[0], c.Fields[1] = c.Fields[1], c.Fields[0]
	swapped[victim] = c
	if d := tableDiffs(swapped, rows); len(d) != 1 || !strings.Contains(d[0], "fields differ") {
		t.Errorf("a planted field swap is not reported alone: %v", d)
	}
}

// TestTelegramTableHasNoSecretBytesField pins spec section 9: key, iv, file_reference and encrypted_key are
// KSecret (length only) and nothing else is.
func TestTelegramTableHasNoSecretBytesField(t *testing.T) {
	secrets := 0
	for _, c := range table {
		for _, f := range c.Fields {
			leaf := f.Name[strings.LastIndex(f.Name, ".")+1:]
			isKey := leaf == "key" || leaf == "iv" || leaf == "file_reference" || leaf == "encrypted_key"
			if isKey && f.Kind != tl.KSecret {
				t.Errorf("%s.%s is %d, want KSecret", c.Name, f.Name, f.Kind)
			}
			if f.Kind == tl.KSecret {
				secrets++
				if !isKey {
					t.Errorf("%s.%s is KSecret but is not key material", c.Name, f.Name)
				}
			}
		}
	}
	if secrets == 0 {
		t.Error("the table holds no secret field")
	}
}

// Task 7 review M1 (carried into Task 8, C5): the generated tables.go is gofmt-clean as written, so the formatter
// never rewrites a generated file and the generator's output is the committed text byte for byte.
func TestTelegramTablesGoIsGofmtClean(t *testing.T) {
	src, err := os.ReadFile("tables.go")
	if err != nil {
		t.Fatal(err)
	}
	got, err := format.Source(src)
	if err != nil {
		t.Fatalf("tables.go does not parse: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("tables.go is not gofmt-clean: regenerate it with tools/fixtures/telegram_tl_facts.py --go")
	}
}

// closureGaps are the object types a full or prefix row of tab names (a KObject or KVector field's Of) that no
// constructor of tab has, one line per naming field: "<type> named by <class>.<field>", sorted.
func closureGaps(tab tl.Table) []string {
	types := map[string]bool{}
	for _, c := range tab {
		types[c.Type] = true
	}
	var out []string
	for _, c := range tab {
		if c.Body == tl.BodyOpaque {
			continue
		}
		for _, f := range c.Fields {
			if (f.Kind == tl.KObject || f.Kind == tl.KVector) && f.Of != "" && !types[f.Of] {
				out = append(out, f.Of+" named by "+c.Name+"."+f.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// withoutType is a copy of tab without every constructor of type typ.
func withoutType(tab tl.Table, typ string) tl.Table {
	out := maps.Clone(tab)
	maps.DeleteFunc(out, func(k tl.Key, _ tl.Constructor) bool { return k.Type == typ })
	return out
}

// Plan 4I4b design D3, rulings R2, B2: the table is closed, so a decode never meets a type it has no row of: every
// object type a full or prefix row names has constructors in the table. A copy without the PhotoSize rows names the
// gap and a class that names it (TL_photo); one without the MessageAction rows names that gap.
func TestTelegramTableIsClosed(t *testing.T) {
	if gaps := closureGaps(newTable()); len(gaps) != 0 {
		t.Errorf("%d closure gaps, first: %s", len(gaps), gaps[0])
	}
	gaps := closureGaps(withoutType(newTable(), "PhotoSize"))
	if !slices.ContainsFunc(gaps, func(g string) bool { return strings.HasPrefix(g, "PhotoSize named by TL_photo.") }) {
		t.Errorf("without PhotoSize: no gap naming PhotoSize and TL_photo in %v", gaps)
	}
	gaps = closureGaps(withoutType(newTable(), typeAction))
	if !slices.ContainsFunc(gaps, func(g string) bool { return strings.HasPrefix(g, typeAction+" named by ") }) {
		t.Errorf("without MessageAction: no gap naming MessageAction in %v", gaps)
	}
}

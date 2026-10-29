package carve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

const cafeType = `{"name":"cafe","ext":"cfe","mime":"application/x-cafe","headers":[{"offset":0,"hex":"cafebabe","mask":""}],` +
	`"footer":"","min_len":8,"max_len":512,"validate":"","align":"block"}`

func sigFile(types ...string) []byte {
	return []byte(`{"format":"` + SigFileFormat + `","types":[` + strings.Join(types, ",") + `]}`)
}

func TestParseSigFileMerges(t *testing.T) {
	base := Builtin()
	set, err := ParseSigFile(sigFile(cafeType), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Types) != 13 || set.ID != BuiltinID+MergedIDSuffix {
		t.Fatalf("%d types, id %q", len(set.Types), set.ID)
	}
	if set.Types[12].Name != "cafe" || set.Types[12].Ext != "cfe" || !bytes.Equal(set.Types[12].Headers[0].Bytes, []byte{0xca, 0xfe, 0xba, 0xbe}) {
		t.Errorf("cafe = %+v", set.Types[12])
	}
	if len(base.Types) != 12 || base.ID != BuiltinID {
		t.Errorf("base mutated: %d types, id %q", len(base.Types), base.ID)
	}
	if set.Digest() == Builtin().Digest() {
		t.Error("merged digest equals the built-in digest")
	}
	again, err := ParseSigFile(sigFile(cafeType), Builtin())
	if err != nil || again.Digest() != set.Digest() {
		t.Errorf("digest not stable: %v", err)
	}
}

func TestParseSigFileRefuses(t *testing.T) {
	typ := func(repl ...string) string { return strings.NewReplacer(repl...).Replace(cafeType) }
	many := make([]string, MaxSigFileTypes+1)
	for i := range many {
		many[i] = typ(`"cafe"`, fmt.Sprintf(`"t%d"`, i))
	}
	atCap := make([]string, MaxSigFileTypes)
	for i := range atCap {
		atCap[i] = typ(`"cafe"`, fmt.Sprintf(`"t%d"`, i))
	}
	cases := map[string][]byte{
		"too big":           append(sigFile(cafeType), bytes.Repeat([]byte(" "), 65<<10)...),
		"wrong format":      []byte(`{"format":"x/1","types":[` + cafeType + `]}`),
		"no format":         []byte(`{"types":[` + cafeType + `]}`),
		"unknown key":       sigFile(typ(`"align":"block"`, `"align":"block","extra":1`)),
		"unknown top key":   []byte(`{"format":"` + SigFileFormat + `","types":[` + cafeType + `],"x":1}`),
		"dup name":          sigFile(typ(`"name":"cafe"`, `"name":"cafe","name":"cafe"`)),
		"dup top key":       []byte(`{"format":"` + SigFileFormat + `","format":"` + SigFileFormat + `","types":[` + cafeType + `]}`),
		"dup nested key":    sigFile(typ(`"offset":0`, `"offset":0,"offset":1`)),
		"trailing object":   append(sigFile(cafeType), []byte("{}")...),
		"trailing garbage":  append(sigFile(cafeType), []byte(" x")...),
		"not json":          []byte("hello"),
		"empty":             nil,
		"null":              []byte("null"),
		"deep":              []byte(strings.Repeat("[", 5000)),
		"no types":          sigFile(),
		"33 types":          sigFile(many...),
		"same name twice":   sigFile(cafeType, cafeType),
		"name jpeg":         sigFile(typ(`"cafe"`, `"jpeg"`)),
		"name mp4":          sigFile(typ(`"cafe"`, `"mp4"`)),
		"name heic":         sigFile(typ(`"cafe"`, `"heic"`)),
		"name apk":          sigFile(typ(`"cafe"`, `"apk"`)),
		"name docx":         sigFile(typ(`"cafe"`, `"docx"`)),
		"validator tar":     sigFile(typ(`"validate":""`, `"validate":"tar"`)),
		"max_len 0":         sigFile(typ(`"max_len":512`, `"max_len":0`)),
		"max_len 4GiB+1":    sigFile(typ(`"max_len":512`, `"max_len":4294967297`)),
		"max_len huge":      sigFile(typ(`"max_len":512`, `"max_len":1e30`)),
		"max_len fraction":  sigFile(typ(`"max_len":512`, `"max_len":5.5`)),
		"max_len string":    sigFile(typ(`"max_len":512`, `"max_len":"512"`)),
		"bad hex":           sigFile(typ(`"cafebabe"`, `"cafebabz"`)),
		"odd hex":           sigFile(typ(`"cafebabe"`, `"cafebab"`)),
		"bad footer hex":    sigFile(typ(`"footer":""`, `"footer":"zz"`)),
		"mask length":       sigFile(typ(`"mask":""`, `"mask":"ff"`)),
		"empty hex":         sigFile(typ(`"cafebabe"`, `""`)),
		"no headers":        sigFile(typ(`[{"offset":0,"hex":"cafebabe","mask":""}]`, `[]`)),
		"headers null":      sigFile(typ(`[{"offset":0,"hex":"cafebabe","mask":""}]`, `null`)),
		"offset huge":       sigFile(typ(`"offset":0`, `"offset":99999999999999999999`)),
		"offset past cap":   sigFile(typ(`"offset":0`, `"offset":513`)),
		"min over max":      sigFile(typ(`"min_len":8`, `"min_len":600`)),
		"bad align":         sigFile(typ(`"align":"block"`, `"align":"odd"`)),
		"type not object":   sigFile(`7`),
		"types not array":   []byte(`{"format":"` + SigFileFormat + `","types":{}}`),
		"case-folded key":   sigFile(typ(`"name"`, `"Name"`)),
		"case-fold dup key": sigFile(typ(`"name":"cafe"`, `"name":"cafe","NAME":"x"`)),
	}
	for name, data := range cases {
		base := Builtin()
		set, err := ParseSigFile(data, base)
		if !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
		if set != nil {
			t.Errorf("%s: a refused file yielded a set", name)
		}
		if len(base.Types) != 12 || base.ID != BuiltinID {
			t.Errorf("%s: base changed", name)
		}
	}
	if _, err := ParseSigFile(sigFile(atCap...), Builtin()); err != nil {
		t.Errorf("32 types: %v", err)
	}
	if _, err := ParseSigFile(sigFile(cafeType), nil); !errors.Is(err, ErrBadSignature) {
		t.Errorf("nil base: %v", err)
	}
	// Exactly at the size cap decodes (padding is JSON whitespace).
	pad := MaxSigFileBytes - len(sigFile(cafeType))
	if _, err := ParseSigFile(append(sigFile(cafeType), bytes.Repeat([]byte(" "), pad)...), Builtin()); err != nil {
		t.Errorf("file at the cap: %v", err)
	}
}

func TestSigFileTypeScansFooterAndBounded(t *testing.T) {
	foot := strings.NewReplacer(`"footer":""`, `"footer":"deadbeef"`, `"name":"cafe"`, `"name":"cafefoot"`,
		`"cafebabe"`, `"cafef007"`).Replace(cafeType)
	set, err := ParseSigFile(sigFile(foot, cafeType), Builtin())
	if err != nil {
		t.Fatal(err)
	}
	img := carvetest.Image(64*kib, fillPattern)
	obj := append([]byte{0xca, 0xfe, 0xf0, 0x07}, bytes.Repeat([]byte{'a'}, 96)...)
	obj = append(obj, 0xde, 0xad, 0xbe, 0xef)
	carvetest.Place(img, 4096, obj)
	capped := append([]byte{0xca, 0xfe, 0xba, 0xbe}, bytes.Repeat([]byte{'b'}, 2000)...)
	carvetest.Place(img, 16384, capped)

	var hits []Hit
	_, err = Scan(context.Background(), bytes.NewReader(img), whole(img), set, Options{}, func(h Hit) error {
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Hit{}
	for _, h := range hits {
		by[h.Type] = h
	}
	if h := by["cafefoot"]; h.Outcome != Footer || h.Method != "carve-footer" || h.Confidence != 50 || h.Offset != 4096 || h.Length != int64(len(obj)) {
		t.Errorf("footer hit = %+v", h)
	}
	// bounded with no footer: this assertion follows scan.go as of the base (the 7C merge may change how it fills assumptions)
	if h := by["cafe"]; h.Outcome != Bounded || h.Method != "carve-bounded" || h.Confidence != 25 || h.Length != 512 ||
		!slices.Contains(h.Assumptions, "length-capped") {
		t.Errorf("bounded hit = %+v", h)
	}
}

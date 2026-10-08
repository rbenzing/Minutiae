package main

import (
	"go/format"
	"strings"
	"testing"
)

func TestGenerateIdentityIsGofumptStable(t *testing.T) {
	for name, ids := range map[string]map[string]GenIdentity{
		"empty": nil,
		"two": {
			"b@1.0.0":  {Package: "m/internal/parsers/b", Hash: h64},
			"a@1.0.10": {Package: "m/internal/parsers/a", Hash: h64},
			"a@1.0.9":  {Package: "m/internal/parsers/a", Hash: h64},
		},
	} {
		out := GenerateIdentity(ids)
		again, err := format.Source(out)
		if err != nil || string(again) != string(out) {
			t.Errorf("%s: not format-stable (%v)", name, err)
		}
		if !strings.HasPrefix(string(out), "// Code generated") {
			t.Errorf("%s: missing generated header", name)
		}
	}
	out := string(GenerateIdentity(map[string]GenIdentity{"b@1.0.0": {}, "a@1.0.9": {}, "a@1.0.10": {}}))
	ia, ib, ic := strings.Index(out, `"a@1.0.10"`), strings.Index(out, `"a@1.0.9"`), strings.Index(out, `"b@1.0.0"`)
	if ia < 0 || ia >= ib || ib >= ic {
		t.Errorf("keys not sorted:\n%s", out)
	}
}

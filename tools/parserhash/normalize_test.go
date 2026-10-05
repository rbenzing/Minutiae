package main

import (
	"bytes"
	"testing"
)

func TestNormalizeGoBasics(t *testing.T) {
	out, err := NormalizeGo([]byte(baseSource), "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("goast-v1\n")) {
		t.Errorf("missing goast-v1 header: %.30q", out)
	}
	if !bytes.Contains(out, []byte("import - fmt")) || !bytes.Contains(out, []byte("//go:linkname h runtime.nanotime")) {
		t.Errorf("dump lacks canonical import or directive lines:\n%s", out)
	}
	if bytes.Contains(out, []byte("F doc")) || bytes.Contains(out, []byte("inside")) {
		t.Errorf("ordinary comments must not appear:\n%s", out)
	}
	if _, err := NormalizeGo([]byte("package a\nfunc ("), "bad.go"); err == nil {
		t.Error("syntax error must be an error")
	}
}

func FuzzNormalizeGo(f *testing.F) {
	f.Add([]byte(baseSource))
	f.Add([]byte("package a\n\nimport \"C\"\n"))
	f.Add([]byte("package a\n//go:embed x\nvar v string\n"))
	f.Add([]byte(""))
	f.Add([]byte("package a\nfunc f[T any](x T) { go func() {}() }\n"))
	f.Fuzz(func(t *testing.T, src []byte) {
		a, err := NormalizeGo(src, "f.go")
		if err != nil {
			return
		}
		b, err := NormalizeGo(src, "f.go")
		if err != nil || !bytes.Equal(a, b) {
			t.Fatal("NormalizeGo not deterministic")
		}
	})
}

package evidence

import (
	"strings"
	"testing"
)

// normalizeBenchInputs are the three texts the normalization cost is recorded for: plain ASCII (the
// common case, where the CJK step must be free), Latin with folding work, and CJK letters.
func normalizeBenchInputs() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"ascii", strings.Repeat("the quick brown fox ", 10)},
		{"latin", strings.Repeat("café Straße ", 17)},
		{"cjk", strings.Repeat("日本語のテキスト", 13)},
	}
}

func benchNormalize(b *testing.B, text string) {
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		_ = NormalizeText(text)
	}
}

func BenchmarkNormalizeText(b *testing.B) {
	for _, in := range normalizeBenchInputs() {
		b.Run(in.name, func(b *testing.B) { benchNormalize(b, in.text) })
	}
}

// maxASCIINormalizeNsPerOp is 500 microseconds for 200 bytes: about 50 times the expected cost, so load
// or -race cannot trip it, but a per-rune allocation or a quadratic pass does.
const maxASCIINormalizeNsPerOp = 500_000

// TestNormalizeTextASCIICostGuard (M24) is a loose guard on the ASCII path of NormalizeText.
func TestNormalizeTextASCIICostGuard(t *testing.T) {
	text := normalizeBenchInputs()[0].text
	if len(text) != 200 {
		t.Fatalf("ascii input is %d bytes, want 200", len(text))
	}
	res := testing.Benchmark(func(b *testing.B) { benchNormalize(b, text) })
	if res.N == 0 {
		t.Fatal("the benchmark did not run")
	}
	if ns := res.NsPerOp(); ns > maxASCIINormalizeNsPerOp {
		t.Fatalf("NormalizeText of 200 ASCII bytes costs %d ns/op, above %d", ns, maxASCIINormalizeNsPerOp)
	}
}

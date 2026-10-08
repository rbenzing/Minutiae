package parse

import (
	"strings"
	"testing"
)

func mustGlob(t *testing.T, pattern string) *Glob {
	t.Helper()
	g, err := CompileGlob(pattern)
	if err != nil {
		t.Fatalf("CompileGlob(%q): %v", pattern, err)
	}
	return g
}

func TestGlobMatch(t *testing.T) {
	const mms = "android:**/com.android.providers.telephony/databases/mmssms.db"
	tests := []struct {
		name, pattern, logical string
		want                   bool
	}{
		{"spec android data", mms, "android:/data/data/com.android.providers.telephony/databases/mmssms.db", true},
		{"spec android multi-user", mms, "android:/data/user/10/com.android.providers.telephony/databases/mmssms.db", true},
		{"spec android no prefix", mms, "android:/com.android.providers.telephony/databases/mmssms.db", true},
		{"android other file", mms, "android:/data/data/com.android.providers.telephony/databases/other.db", false},
		{"case sensitive file", mms, "android:/data/data/com.android.providers.telephony/databases/MMSSMS.db", false},
		{"case sensitive dir", mms, "android:/data/data/com.android.providers.Telephony/databases/mmssms.db", false},
		{"deeper than the file", mms, "android:/data/data/com.android.providers.telephony/databases/mmssms.db/x", false},

		{"spec ios domain", "ios:*/Library/SMS/sms.db", "ios:HomeDomain/Library/SMS/sms.db", true},
		{"spec ios app domain", "ios:*/Library/SMS/sms.db", "ios:AppDomain-x/Library/SMS/sms.db", true},
		{"ios deeper path", "ios:*/Library/SMS/sms.db", "ios:HomeDomain/x/Library/SMS/sms.db", false},
		{"ios shallower path", "ios:*/Library/SMS/sms.db", "ios:Library/SMS/sms.db", false},
		{"ios star needs an element", "ios:*/Library/SMS/sms.db", "ios:/Library/SMS/sms.db", false},

		{"star inside an element", "android:/data/*.db", "android:/data/a.db", true},
		{"star stays inside an element", "android:/data/*.db", "android:/data/a/b.db", false},
		{"star matches an empty run", "android:/data/*.db", "android:/data/.db", true},
		{"star in the middle", "android:/data/a*c", "android:/data/abbbc", true},
		{"star in the middle, mismatch", "android:/data/a*c", "android:/data/abbbd", false},
		{"two stars in one element", "android:/d/a*b*c", "android:/d/aXbYc", true},
		{"two stars in one element, order", "android:/d/a*b*c", "android:/d/aXcYb", false},
		{"prefix and suffix may not overlap", "android:/d/ab*ba", "android:/d/aba", false},
		{"prefix and suffix adjacent", "android:/d/ab*ba", "android:/d/abba", true},
		{"literal needs the whole element", "android:/d/abc", "android:/d/abcd", false},

		{"double star matches zero elements", "android:/data/**/x.db", "android:/data/x.db", true},
		{"double star matches many", "android:/data/**/x.db", "android:/data/a/b/c/x.db", true},
		{"double star in the middle must still anchor", "android:/data/**/x.db", "android:/other/a/x.db", false},
		{"trailing double star, zero elements", "android:/data/**", "android:/data", true},
		{"trailing double star, many", "android:/data/**", "android:/data/a/b", true},
		{"leading slash is optional in an android glob", "android:data/x", "android:/data/x", true},
		{"two double stars", "android:**/a/**/b", "android:/x/a/y/z/b", true},

		{"scheme mismatch android vs ios", "android:/data/x", "ios:data/x", false},
		{"scheme mismatch ios vs android", "ios:data/x", "android:/data/x", false},
		{"logical path without a scheme", "android:/data/x", "/data/x", false},
		{"android logical must be absolute", "android:/data/x", "android:data/x", false},
		{"logical with an empty element", "android:/data/x", "android:/data//x", false},
		{"logical with a trailing slash", "android:/data/x", "android:/data/x/", false},
		{"logical dot dot never matches", "android:**/x", "android:/a/../x", false},
		{"empty logical", "android:**", "", false},
		{"android root only", "android:**", "android:/", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := mustGlob(t, tc.pattern)
			if g.Relative() {
				t.Fatal("an absolute glob reports Relative")
			}
			if got := g.Match(tc.logical); got != tc.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", tc.pattern, tc.logical, got, tc.want)
			}
			if g.MatchRelative("android:/data/p", tc.logical) {
				t.Fatal("an absolute glob matched through MatchRelative")
			}
		})
	}
}

func TestGlobMatchRelative(t *testing.T) {
	const androidPrimary = "android:/data/data/p/databases/sms.db"
	const iosPrimary = "ios:HomeDomain/Library/SMS/sms.db"
	tests := []struct {
		name, pattern, primary, logical string
		want                            bool
	}{
		{"wal beside the primary", "./sms.db-wal", androidPrimary, "android:/data/data/p/databases/sms.db-wal", true},
		{"other directory", "./sms.db-wal", androidPrimary, "android:/data/data/q/databases/sms.db-wal", false},
		{"deeper directory", "./sms.db-wal", androidPrimary, "android:/data/data/p/databases/x/sms.db-wal", false},
		{"other file", "./sms.db-wal", androidPrimary, "android:/data/data/p/databases/sms.db-shm", false},
		{"wildcard in a relative glob", "./sms.db-*", androidPrimary, "android:/data/data/p/databases/sms.db-journal", true},
		{"wildcard stays in the element", "./sms.db-*", androidPrimary, "android:/data/data/p/databases/sms.db-a/b", false},
		{"double star below the primary directory", "./**/x.png", androidPrimary, "android:/data/data/p/databases/a/b/x.png", true},
		{"double star matches zero elements", "./**/x.png", androidPrimary, "android:/data/data/p/databases/x.png", true},
		{"double star never escapes upwards", "./**/x.png", androidPrimary, "android:/data/data/p/x.png", false},
		{"scheme mismatch", "./sms.db-wal", androidPrimary, "ios:data/data/p/databases/sms.db-wal", false},
		{"ios sibling", "./sms.db-wal", iosPrimary, "ios:HomeDomain/Library/SMS/sms.db-wal", true},
		{"ios other domain", "./sms.db-wal", iosPrimary, "ios:AppDomain-x/Library/SMS/sms.db-wal", false},
		{"primary that is not a logical path", "./sms.db-wal", "/data/sms.db", "android:/data/sms.db-wal", false},
		{"primary with an empty element", "./sms.db-wal", "android:/data//sms.db", "android:/data/sms.db-wal", false},
		{"directory characters in the primary are literal", "./a", "android:/d*/e/p.db", "android:/dX/e/a", false},
		{"directory characters in the primary match themselves", "./a", "android:/d*/e/p.db", "android:/d*/e/a", true},
		{"primary at the root", "./x", "android:/p.db", "android:/x", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := mustGlob(t, tc.pattern)
			if !g.Relative() {
				t.Fatal("a relative glob does not report Relative")
			}
			if got := g.MatchRelative(tc.primary, tc.logical); got != tc.want {
				t.Fatalf("MatchRelative(%q, %q, %q) = %v, want %v", tc.pattern, tc.primary, tc.logical, got, tc.want)
			}
			if g.Match(tc.logical) {
				t.Fatal("a relative glob matched through Match")
			}
		})
	}
}

func TestGlobRejectsMalformed(t *testing.T) {
	elems := func(n int) string { return strings.TrimSuffix(strings.Repeat("a/", n), "/") }
	tests := []struct{ name, pattern string }{
		{"empty", ""},
		{"scheme only", "android:"},
		{"scheme and slash only", "android:/"},
		{"ios scheme only", "ios:"},
		{"relative prefix only", "./"},
		{"unknown scheme", "windows:/x"},
		{"no scheme", "data/x"},
		{"double slash", "android:/data//x"},
		{"double slash after the optional slash", "android://x"},
		{"ios leading slash", "ios:/x"},
		{"relative double slash", "./a//b"},
		{"trailing slash", "android:/data/"},
		{"dot dot", "android:/data/../x"},
		{"relative dot dot", "./../x"},
		{"dot element", "android:/data/./x"},
		{"relative dot element", "./a/./b"},
		{"double star inside an element", "android:/a**b/c"},
		{"double star suffix", "android:/a**/c"},
		{"triple star", "android:/***/c"},
		{"NUL", "android:/a\x00b"},
		{"backslash", `android:/a\b`},
		{"65 elements", "android:/" + elems(65)},
		{"65 relative elements", "./" + elems(65)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if g, err := CompileGlob(tc.pattern); err == nil {
				t.Fatalf("CompileGlob(%q) = %+v, want an error", tc.pattern, g)
			}
		})
	}
	// The boundary: exactly 64 elements compile.
	mustGlob(t, "android:/"+elems(64))
	mustGlob(t, "./"+elems(64))
}

func TestGlobMatchIsLinear(t *testing.T) {
	const pattern = "android:**/**/**/**/**/x"
	g := mustGlob(t, pattern)
	const n = 4000
	elems := make([]string, n)
	for i := range elems {
		elems[i] = "a"
	}
	for _, tc := range []struct {
		name string
		last string
		want bool
	}{{"fails at the very end", "y", false}, {"matches", "x", true}} {
		t.Run(tc.name, func(t *testing.T) {
			elems[n-1] = tc.last
			logical := "android:/" + strings.Join(elems, "/")
			got, steps := g.matchSteps(logical)
			if got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
			if limit := g.elementCount() * n * 2; steps >= limit {
				t.Fatalf("%d steps for %d pattern elements x %d path elements (limit %d): matching is not linear", steps, g.elementCount(), n, limit)
			}
			if steps == 0 {
				t.Fatal("the step counter was never incremented: the seam is vacuous")
			}
		})
	}
	// Match itself agrees with the instrumented path.
	elems[n-1] = "x"
	if !g.Match("android:/" + strings.Join(elems, "/")) {
		t.Fatal("Match disagrees with matchSteps")
	}
}

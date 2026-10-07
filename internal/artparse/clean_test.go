package artparse_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/artparse"
)

type badStringer struct{}

func (badStringer) String() string { panic("String method panics") }

type badErr struct{}

func (*badErr) Error() string { panic("Error method panics") }

func TestPanicTextNeverCallsUnguardedError(t *testing.T) {
	if got := artparse.PanicText("plain"); got != "plain" {
		t.Errorf("string = %q", got)
	}
	if got := artparse.PanicText(errString("an error")); got != "an error" {
		t.Errorf("error = %q", got)
	}
	if got := artparse.PanicText(42); got != "42" {
		t.Errorf("int = %q", got)
	}
	// An Error or String method that panics must not escape: the type name is reported instead.
	for name, v := range map[string]any{"error": &badErr{}, "stringer": badStringer{}, "nil pointer error": (*badErr)(nil)} {
		got := artparse.PanicText(v)
		if !strings.Contains(got, "badErr") && !strings.Contains(got, "badStringer") {
			t.Errorf("%s: %q does not name the type", name, got)
		}
	}
	if got := artparse.PanicText(struct{ X chan int }{}); !strings.Contains(got, "struct") {
		t.Errorf("other value = %q, want its type", got)
	}
	if got := artparse.PanicText(nil); got == "" {
		t.Error("nil panic value rendered empty")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCleanAuditText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "hello"},
		{"a\x00b", "a?b"},
		{"bad\xffbyte", "bad?byte"},
		{"", ""},
	}
	for _, c := range cases {
		if got := artparse.CleanAuditText(c.in, 100); got != c.want {
			t.Errorf("clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("é", 100) // two bytes each
	for _, max := range []int{10, 11, 50, 199} {
		got := artparse.CleanAuditText(long, max)
		if len(got) > max || !utf8.ValidString(got) {
			t.Errorf("max %d: len %d valid %v", max, len(got), utf8.ValidString(got))
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("max %d: a clipped text ends with an ellipsis: %q", max, got)
		}
	}
	if got := artparse.CleanAuditText("abc", 0); got != "" {
		t.Errorf("max 0 = %q", got)
	}
	if got := artparse.CleanAuditText("abcdef", 3); got != "abc" {
		t.Errorf("max 3 = %q (no room for an ellipsis)", got)
	}
}

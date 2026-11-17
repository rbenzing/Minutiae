package wajid

import (
	"strings"
	"testing"
)

// FuzzWAJID: Parse never panics; on success User and Server are non-empty substrings of the
// input, Server holds no '@' and the class is one of the seven.
func FuzzWAJID(f *testing.F) {
	for _, c := range wajidCases {
		f.Add(c.in)
	}
	f.Fuzz(func(t *testing.T, s string) {
		j, err := Parse(s)
		if err != nil {
			return
		}
		if j.User == "" || j.Server == "" || !strings.Contains(s, j.User) || !strings.Contains(s, j.Server) || strings.Contains(j.Server, "@") {
			t.Fatalf("Parse(%q) = %+v", s, j)
		}
		switch j.Class {
		case ClassUser, ClassGroup, ClassBroadcast, ClassStatus, ClassLID, ClassNewsletter, ClassOther:
		default:
			t.Fatalf("Parse(%q): class %q", s, j.Class)
		}
	})
}

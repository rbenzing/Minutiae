package common_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

func TestAddressKind(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		// phone
		{"+1 (555) 123-4567", "phone"},
		{"+15551234567", "phone"},
		{"5551234", "phone"},
		{"555.123.4567", "phone"},
		{"(555) 123-4567", "phone"},
		{"+447911123456", "phone"},
		{"123456789012345", "phone"},    // 15 digits
		{"1234567890123456", "unknown"}, // 16 digits
		{"123456", "shortcode"},         // 6 digits: below the phone minimum
		{"+123456", "unknown"},          // a plus makes it a phone, and it is too short
		{"+1 555", "unknown"},
		{"555-1234x", "unknown"},
		{"+", "unknown"},
		{"++15551234567", "unknown"},
		{"5551234+", "unknown"},
		// shortcode
		{"12345", "shortcode"},
		{"123", "shortcode"},
		{"12", "unknown"},
		{"1234567", "phone"}, // 7 digits
		// alnum
		{"Bank", "alnum"},
		{"MyBank 24", "alnum"},
		{"A", "alnum"},
		{"ABCDEFGHIJK", "alnum"},    // 11
		{"ABCDEFGHIJKL", "unknown"}, // 12
		{"Bank-24", "unknown"},
		{"Bänk", "unknown"},
		{"   ", "unknown"},
		// email
		{"a@b.co", "email"},
		{"first.last+tag@example.com", "email"},
		{"a@b", "unknown"},
		{"@x.com", "unknown"},
		{"a@@b.com", "unknown"},
		{"a@b@c.com", "unknown"},
		{"a b@c.com", "unknown"},
		{"a@b .com", "unknown"},
		{"a@\u00A0b.com", "unknown"},
		{"a@.com", "unknown"},
		{"a@b.", "unknown"},
		{"a\n@b.com", "unknown"},
		// unknown
		{"", "unknown"},
		{"?", "unknown"},
		{"unknown sender", "unknown"},
		{"https://example.com", "unknown"},
		// Unicode digits are not digits
		{"\u0665\u0665\u0665\u0661\u0662\u0663\u0664", "unknown"}, // Arabic-Indic digits
		{"\uFF15\uFF15\uFF15\uFF11\uFF12\uFF13\uFF14", "unknown"}, // fullwidth digits
		{"\u0665\u0665\u0665", "unknown"},
	}
	for _, tc := range cases {
		if got := common.AddressKind(tc.addr); got != tc.want {
			t.Errorf("AddressKind(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
	// a hostile, very long input costs bounded work and is unknown
	for _, s := range []string{strings.Repeat("1", 1<<20), strings.Repeat("a", 1<<20) + "@b.com" + strings.Repeat("@", 100)} {
		if got := common.AddressKind(s); got != "unknown" && got != "email" {
			t.Errorf("long input classified as %q", got)
		}
	}
}

package parse

import (
	"reflect"
	"strings"
	"testing"
)

func validMeta() Meta {
	return Meta{
		Name:      "android.mmssms",
		Version:   "1.0.0",
		Title:     "Android SMS and MMS",
		Platforms: []string{"android"},
		Emits:     []Emit{{Type: "message", PayloadVersion: 1}},
		Inputs: []InputSpec{
			{Role: "db", Globs: []string{"android:**/com.android.providers.telephony/databases/mmssms.db"}, Required: true},
			{Role: "wal", Globs: []string{"./mmssms.db-wal"}, Companions: []string{"-wal"}},
		},
		Claims: []TableClaim{{Role: "db", Table: "sms"}, {Role: "db", Table: "threads"}},
	}
}

func TestValidateMeta(t *testing.T) {
	if err := ValidateMeta(validMeta()); err != nil {
		t.Fatalf("valid Meta refused: %v", err)
	}
	valid := []struct {
		name string
		mut  func(m *Meta)
	}{
		{"no claims", func(m *Meta) { m.Claims = nil }},
		{"both platforms with an iOS primary", func(m *Meta) {
			m.Platforms = []string{"android", "ios"}
			m.Inputs[0].Globs = []string{"ios:*/Library/SMS/sms.db"}
		}},
		{"both platforms with both globs", func(m *Meta) {
			m.Platforms = []string{"ios", "android"}
			m.Inputs[0].Globs = append(m.Inputs[0].Globs, "ios:*/Library/SMS/sms.db")
		}},
		{"a role that is only a companion", func(m *Meta) { m.Inputs[1] = InputSpec{Role: "journal", Companions: []string{"-journal", "-shm"}} }},
		{"a single input", func(m *Meta) { m.Inputs = m.Inputs[:1]; m.Claims = nil }},
		{"role of 16 characters", func(m *Meta) { m.Inputs[1].Role = "a123456789012345" }},
		{"name of 64 characters", func(m *Meta) { m.Name = "a" + strings.Repeat("b", 63) }},
		{"versions with several digits", func(m *Meta) { m.Version = "10.200.3000" }},
		{"a primary role that is not db and no claims", func(m *Meta) { m.Inputs[0].Role = "main"; m.Claims = nil }},
		{"table with spaces and punctuation", func(m *Meta) { m.Claims = []TableClaim{{Role: "db", Table: "my table$1"}} }},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			m := validMeta()
			tc.mut(&m)
			if err := ValidateMeta(m); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}

	bad := []struct {
		name string
		mut  func(m *Meta)
		want string // substring of the error
	}{
		{"empty name", func(m *Meta) { m.Name = "" }, "name"},
		{"name starts with a dash", func(m *Meta) { m.Name = "-x" }, "name"},
		{"name starts with a dot", func(m *Meta) { m.Name = ".x" }, "name"},
		{"name with a space", func(m *Meta) { m.Name = "a b" }, "name"},
		{"name with a slash", func(m *Meta) { m.Name = "a/b" }, "name"},
		{"name of 65 characters", func(m *Meta) { m.Name = "a" + strings.Repeat("b", 64) }, "name"},
		{"name with non-ASCII", func(m *Meta) { m.Name = "café" }, "name"},
		{"empty version", func(m *Meta) { m.Version = "" }, "version"},
		{"two-part version", func(m *Meta) { m.Version = "1.0" }, "version"},
		{"four-part version", func(m *Meta) { m.Version = "1.0.0.0" }, "version"},
		{"leading zero in version", func(m *Meta) { m.Version = "01.0.0" }, "version"},
		{"non-numeric version", func(m *Meta) { m.Version = "a.b.c" }, "version"},
		{"v-prefixed version", func(m *Meta) { m.Version = "v1.0.0" }, "version"},
		{"pre-release version", func(m *Meta) { m.Version = "1.0.0-rc1" }, "version"},
		{"negative version", func(m *Meta) { m.Version = "1.-1.0" }, "version"},
		{"empty title", func(m *Meta) { m.Title = "" }, "title"},
		{"blank title", func(m *Meta) { m.Title = " \t" }, "title"},
		{"no platforms", func(m *Meta) { m.Platforms = nil }, "platform"},
		{"unknown platform", func(m *Meta) { m.Platforms = []string{"android", "windows"} }, "platform"},
		{"duplicate platform", func(m *Meta) { m.Platforms = []string{"android", "android"} }, "platform"},
		{"no emits", func(m *Meta) { m.Emits = nil }, "emit"},
		{"empty emit type", func(m *Meta) { m.Emits = []Emit{{Type: "", PayloadVersion: 1}} }, "emit"},
		{"duplicate emit type", func(m *Meta) {
			m.Emits = []Emit{{Type: "message", PayloadVersion: 1}, {Type: "message", PayloadVersion: 2}}
		}, "emit"},
		{"payload version 0", func(m *Meta) { m.Emits[0].PayloadVersion = 0 }, "emit"},
		{"negative payload version", func(m *Meta) { m.Emits[0].PayloadVersion = -1 }, "emit"},
		{"no inputs", func(m *Meta) { m.Inputs = nil; m.Claims = nil }, "input"},
		{"primary not required", func(m *Meta) { m.Inputs[0].Required = false }, "required"},
		{"primary without globs", func(m *Meta) { m.Inputs[0].Globs = nil }, "glob"},
		{"relative primary glob", func(m *Meta) { m.Inputs[0].Globs = []string{"./mmssms.db"} }, "absolute"},
		{"primary glob scheme not in Platforms", func(m *Meta) { m.Inputs[0].Globs = []string{"ios:*/Library/SMS/sms.db"} }, "platform"},
		{"one primary glob outside Platforms", func(m *Meta) {
			m.Inputs[0].Globs = append(m.Inputs[0].Globs, "ios:*/Library/SMS/sms.db")
		}, "platform"},
		{"primary glob does not compile", func(m *Meta) { m.Inputs[0].Globs = []string{"android:/a**b"} }, "glob"},
		{"primary glob empty", func(m *Meta) { m.Inputs[0].Globs = []string{""} }, "glob"},
		{"non-primary absolute glob", func(m *Meta) { m.Inputs[1].Globs = []string{"android:/data/x-wal"} }, "relative"},
		{"non-primary glob does not compile", func(m *Meta) { m.Inputs[1].Globs = []string{"./a//b"} }, "glob"},
		{"role with a capital", func(m *Meta) { m.Inputs[1].Role = "Wal" }, "role"},
		{"role starting with a digit", func(m *Meta) { m.Inputs[1].Role = "1wal" }, "role"},
		{"role of 17 characters", func(m *Meta) { m.Inputs[1].Role = "a1234567890123456" }, "role"},
		{"empty role", func(m *Meta) { m.Inputs[1].Role = "" }, "role"},
		{"role with a dash", func(m *Meta) { m.Inputs[1].Role = "a-b" }, "role"},
		{"primary role invalid", func(m *Meta) { m.Inputs[0].Role = "DB"; m.Claims = nil }, "role"},
		{"duplicate roles", func(m *Meta) { m.Inputs[1].Role = "db" }, "role"},
		{"bad companion suffix", func(m *Meta) { m.Inputs[1].Companions = []string{"-bak"} }, "companion"},
		{"companion without a dash", func(m *Meta) { m.Inputs[1].Companions = []string{"wal"} }, "companion"},
		{"companion in capitals", func(m *Meta) { m.Inputs[1].Companions = []string{"-WAL"} }, "companion"},
		{"companion on the primary role", func(m *Meta) { m.Inputs[0].Companions = []string{".bak"} }, "companion"},
		{"claim for a role other than db", func(m *Meta) { m.Claims = []TableClaim{{Role: "wal", Table: "sms"}} }, "claim"},
		{"claim with db when the primary role is not db", func(m *Meta) { m.Inputs[0].Role = "main" }, "claim"},
		{"claim with an empty table", func(m *Meta) { m.Claims = []TableClaim{{Role: "db", Table: ""}} }, "claim"},
		{"claim with a non-ASCII table", func(m *Meta) { m.Claims = []TableClaim{{Role: "db", Table: "täble"}} }, "claim"},
		{"claim with a control character", func(m *Meta) { m.Claims = []TableClaim{{Role: "db", Table: "a\nb"}} }, "claim"},
		{"duplicate claim", func(m *Meta) { m.Claims = []TableClaim{{Role: "db", Table: "sms"}, {Role: "db", Table: "sms"}} }, "claim"},
		{"duplicate claim, other case", func(m *Meta) { m.Claims = []TableClaim{{Role: "db", Table: "sms"}, {Role: "db", Table: "SMS"}} }, "claim"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			m := validMeta()
			tc.mut(&m)
			err := ValidateMeta(m)
			if err == nil {
				t.Fatal("ValidateMeta accepted it")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestMetaIdentity(t *testing.T) {
	got := validMeta().Identity("src1:sha256:abc")
	want := Identity{Name: "android.mmssms", Version: "1.0.0", Hash: "src1:sha256:abc"}
	if got != want {
		t.Fatalf("Identity = %+v, want %+v", got, want)
	}
}

func TestMetaCloneKeepsNilAndEmpty(t *testing.T) {
	var zero Meta
	c := zero.Clone()
	if !reflect.DeepEqual(c, zero) {
		t.Fatalf("clone of the zero Meta = %#v", c)
	}
	m := Meta{Platforms: []string{}, Inputs: []InputSpec{{Role: "db", Globs: []string{}}}}
	c = m.Clone()
	if c.Platforms == nil || c.Inputs[0].Globs == nil || c.Emits != nil {
		t.Fatalf("clone changed nil/empty: %#v", c)
	}
}

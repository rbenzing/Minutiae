package artparse_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

func TestRegisterRefusesBadHashAndMeta(t *testing.T) {
	good := metaFor("p", "1.0.0")
	for name, hash := range map[string]string{
		"empty": "", "no prefix": strings.Repeat("a", 64), "short": "src1:sha256:abc",
		"upper": "src1:sha256:" + strings.Repeat("A", 64), "long": hashFor("p") + "0", "other algorithm": "src1:sha512:" + strings.Repeat("a", 64),
	} {
		if _, err := artparse.Register(&fake{meta: func() parse.Meta { return good }}, hash); err == nil {
			t.Errorf("hash %s accepted", name)
		}
	}
	bad := good
	bad.Name = ""
	if _, err := artparse.Register(&fake{meta: func() parse.Meta { return bad }}, hashFor("p")); err == nil {
		t.Error("a Meta that fails ValidateMeta was accepted")
	}
	if _, err := artparse.Register(&fake{meta: func() parse.Meta { return good }}, hashFor("p")); err != nil {
		t.Errorf("a good parser was refused: %v", err)
	}
}

func TestRegisterCachesMeta(t *testing.T) {
	n := 0
	f := &fake{meta: func() parse.Meta {
		n++
		return metaFor("p", "1.0."+string(rune('0'+n)))
	}}
	r, err := artparse.Register(f, hashFor("p"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := r.Meta().Version; got != "1.0.1" {
			t.Fatalf("Meta version = %s, the first call's 1.0.1 must stay", got)
		}
	}
	want := records.Parser{Name: "p", Version: "1.0.1", Hash: hashFor("p")}
	if got := r.Identity(); got != want {
		t.Errorf("Identity = %+v, want %+v", got, want)
	}
	if f.calls.Load() != 1 {
		t.Errorf("Meta() was called %d times, want exactly 1", f.calls.Load())
	}
	panicker := &fake{meta: func() parse.Meta { panic("boom") }}
	if _, err := artparse.Register(panicker, hashFor("p")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a panicking Meta() must be a Register error naming the panic: %v", err)
	}
}

func TestRegisterDeepCopiesMeta(t *testing.T) {
	m := metaFor("p", "1.0.0")
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("p"))
	if err != nil {
		t.Fatal(err)
	}
	// the parser mutates what it returned, in place and by append
	m.Emits[0].Type = "call"
	m.Emits = append(m.Emits, parse.Emit{Type: "contact", PayloadVersion: 1})
	m.Inputs[0].Globs[0] = "android:/evil"
	m.Platforms[0] = "ios"
	got := r.Meta()
	if len(got.Emits) != 1 || got.Emits[0].Type != message.Type || got.Inputs[0].Globs[0] == "android:/evil" || got.Platforms[0] != parse.PlatformAndroid {
		t.Errorf("the enforced Meta changed with the parser's own: %+v", got)
	}
	if len(got.Emits) == 0 || len(got.Inputs) == 0 {
		t.Fatal("Meta() returned no emits or inputs")
	}
	// and a caller mutating a returned Meta changes nothing for the next caller
	got.Emits[0].Type = "web_visit"
	got.Inputs[0].Globs[0] = "android:/also-evil"
	again := r.Meta()
	if again.Emits[0].Type != message.Type || again.Inputs[0].Globs[0] == "android:/also-evil" {
		t.Errorf("Meta() shares memory between calls: %+v", again)
	}
	if err := r.Check(); err != nil {
		t.Errorf("Check judges the host copy, not the parser's: %v", err)
	}
}

func TestRegisterBoundsMetaCall(t *testing.T) {
	defer artparse.SetMetaTimeout(50 * time.Millisecond)()
	release := make(chan struct{})
	defer close(release)
	f := &fake{meta: func() parse.Meta { <-release; return metaFor("p", "1.0.0") }}
	start := time.Now()
	_, err := artparse.Register(f, hashFor("p"))
	if err == nil {
		t.Fatal("a hanging Meta() was accepted")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Register took %v", d)
	}
}

func TestPayloadVersionMismatchRefusesJob(t *testing.T) {
	m := metaFor("p", "1.0.0")
	m.Emits[0].PayloadVersion = message.PayloadVersion + 1
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("p"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); !errors.Is(err, parse.ErrPayloadVersionMismatch) {
		t.Errorf("Check = %v, want ErrPayloadVersionMismatch", err)
	}
	if _, err := artparse.New(newCase(t), []artparse.Registered{r}, artparse.Options{Limits: parse.DefaultLimits()}); !errors.Is(err, parse.ErrPayloadVersionMismatch) {
		t.Errorf("New = %v, want ErrPayloadVersionMismatch", err)
	}
}

func TestParserEmittingTypeWithoutValidatorRefused(t *testing.T) {
	for _, typ := range []string{"location", "no_such_type"} {
		m := metaFor("p", "1.0.0")
		m.Emits = []parse.Emit{{Type: typ, PayloadVersion: 1}}
		r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("p"))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Check(); !errors.Is(err, parse.ErrNoPayloadContract) {
			t.Errorf("%s: Check = %v, want ErrNoPayloadContract", typ, err)
		}
	}
}

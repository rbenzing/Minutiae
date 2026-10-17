package utf8fold

import (
	"bytes"
	"runtime"
	"testing"
	"time"
)

// maxExpansionRunes returns the code points whose contribution to a key is maxKeyRunesPerRune runes, found
// by scanning the tables (not named in the test).
func maxExpansionRunes(t *testing.T) []rune {
	t.Helper()
	var out []rune
	for _, e := range append(append([]mapEntry(nil), decompIndex...), foldIndex...) {
		k, ok := Key(nil, []byte(string(e.r)))
		if !ok {
			continue
		}
		if n := len([]rune(string(k))); n == maxKeyRunesPerRune {
			out = append(out, e.r)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no code point reaches maxKeyRunesPerRune = %d", maxKeyRunesPerRune)
	}
	return out
}

func TestKeyNeverAllocatesFromContent(t *testing.T) {
	cps := maxExpansionRunes(t)
	// the longest 255-byte name made of them: as many whole characters as fit
	var name []byte
	for i := 0; ; i++ {
		c := []byte(string(cps[i%len(cps)]))
		if len(name)+len(c) > MaxName {
			break
		}
		name = append(name, c...)
	}
	// one base and 126 combining marks
	marks := []byte("a")
	for len(marks)+2 <= MaxName {
		marks = append(marks, "\u0301"...)
	}
	// the most one call may allocate: the rune buffer and the output, both bounded by MaxName*maxKeyRunesPerRune
	limit := uint64(MaxName*maxKeyRunesPerRune*4*5 + 8192)
	for _, n := range [][]byte{name, marks} {
		if len(n) > MaxName {
			t.Fatalf("test name has %d bytes", len(n))
		}
		var ms0, ms1 runtime.MemStats
		runtime.ReadMemStats(&ms0)
		k, ok := Key(nil, n)
		runtime.ReadMemStats(&ms1)
		if !ok {
			t.Fatalf("Key refused a valid %d-byte name", len(n))
		}
		if len([]rune(string(k))) > MaxName*maxKeyRunesPerRune {
			t.Errorf("key has %d runes, bound %d", len([]rune(string(k))), MaxName*maxKeyRunesPerRune)
		}
		if got := ms1.TotalAlloc - ms0.TotalAlloc; got > limit {
			t.Errorf("Key allocated %d bytes for a %d-byte name, bound %d", got, len(n), limit)
		}
		if a := testing.AllocsPerRun(20, func() { _, _ = Key(nil, n) }); a > 4 {
			t.Errorf("Key made %v allocations, want a small constant", a)
		}
	}
}

func TestKeyLongCombiningRunTerminates(t *testing.T) {
	n := []byte("a")
	for i := 0; len(n)+2 <= MaxName; i++ {
		if i%2 == 0 {
			n = append(n, "\u0323"...)
		} else {
			n = append(n, "\u0301"...)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		k, ok := Key(nil, n)
		if !ok || len(k) != len(n) {
			t.Errorf("Key = (%d bytes, %v) for a %d-byte run", len(k), ok, len(n))
		}
		// the reordered run sorts all U+0323 (class 220) before all U+0301 (class 230)
		if i := bytes.Index(k, []byte("\u0301")); i >= 0 && bytes.Contains(k[i:], []byte("\u0323")) {
			t.Error("marks not in canonical order")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Key did not finish")
	}
}

// A hostile name of a shape no kernel measurement covers is UNDECIDED: not folded, not "different", never a
// guess. Equal reports decided == false and Key refuses.
func TestHostileUnmeasuredShapeIsUndecided(t *testing.T) {
	hostile := [][]byte{
		[]byte("A\xffb"),                  // an invalid lead with text after it
		[]byte("\xff\xfe\xfd"),            // nothing before the invalid bytes
		[]byte("a\xe2\x82\u00e9"),         // a truncated sequence followed by a valid character
		bytes.Repeat([]byte("\xf8"), 200), // a long run of invalid leads
		[]byte("\x80\x80\xff"),            // leading stray bytes with another invalid form
	}
	for _, h := range hostile {
		if Classify(h) != FormUndecided {
			t.Errorf("Classify(%q) = %v, want FormUndecided", h, Classify(h))
		}
		if k, ok := Key(nil, h); ok || len(k) != 0 {
			t.Errorf("Key(%q) = (%q, %v)", h, k, ok)
		}
		for _, other := range [][]byte{[]byte("a"), []byte("a\xffB"), {}, h[:1]} {
			if eq, decided := eqd(h, other); decided || eq {
				if !bytes.Equal(h, other) {
					t.Errorf("Equal(%q, %q) = (%v, %v), want (false, false)", h, other, eq, decided)
				}
			}
		}
	}
}

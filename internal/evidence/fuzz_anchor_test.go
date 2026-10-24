package evidence

import (
	"reflect"
	"testing"
)

func FuzzParseAnchor(f *testing.F) {
	f.Add(testAnchor().Marshal())
	f.Add(SignAnchor(testAnchor(), testKey(f)).Marshal())
	for _, b := range anchorRejects(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		a, err := ParseAnchor(b)
		if err != nil {
			return
		}
		again, err := ParseAnchor(a.Marshal())
		if err != nil {
			t.Fatalf("an accepted anchor does not re-parse: %v\n%q", err, a.Marshal())
		}
		if !reflect.DeepEqual(a, again) {
			t.Fatalf("re-parse differs:\n%+v\n%+v", a, again)
		}
	})
}

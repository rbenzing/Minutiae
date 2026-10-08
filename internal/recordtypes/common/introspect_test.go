package common

import (
	"reflect"
	"testing"
)

func TestEmptyableStrings(t *testing.T) {
	s := &Schema{Fields: []Field{
		{Name: "a", Kind: KString},
		{Name: "b", Kind: KString, NonEmpty: true},
		{Name: "c", Kind: KToken},
		{Name: "o", Kind: KObject, Obj: &Schema{Fields: []Field{{Name: "x", Kind: KString}, {Name: "y", Kind: KString, NonEmpty: true}}}},
		{Name: "l", Kind: KArray, Obj: &Schema{Fields: []Field{{Name: "z", Kind: KString}}}},
	}}
	want := []string{"a", "o.x", "l.z"}
	if got := EmptyableStrings(s); !reflect.DeepEqual(got, want) {
		t.Errorf("EmptyableStrings = %v, want %v", got, want)
	}
	if got := EmptyableStrings(nil); len(got) != 0 {
		t.Errorf("nil schema: %v", got)
	}
}

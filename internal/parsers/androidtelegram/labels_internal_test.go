package androidtelegram

import (
	"slices"
	"testing"
)

// Ruling C4 (plan 4I4c P10, design section 9.4): the user_class of a mention's InputUser is its label, and every
// InputUser class of the table has one of the vocabulary: TL_inputUser and TL_inputUser_layer132 (whose class name has
// no label after the prefix, a layer suffix being no label) are "user", never "" and never the class name.
func TestTelegramInputUserLabelsArePinned(t *testing.T) {
	vocabulary := []string{"user", "from_message", "self", "empty"}
	n := 0
	for k, c := range newTable() {
		if k.Type != "InputUser" {
			continue
		}
		n++
		if got := inputUserLabel(c.Name); !slices.Contains(vocabulary, got) {
			t.Errorf("inputUserLabel(%s) = %q, want one of %v", c.Name, got, vocabulary)
		}
	}
	if n == 0 {
		t.Fatal("the table has no InputUser class")
	}
	for _, class := range []string{"TL_inputUser", "TL_inputUser_layer132"} {
		if got := inputUserLabel(class); got != "user" {
			t.Errorf("inputUserLabel(%s) = %q, want user (ruling C4)", class, got)
		}
	}
	if got := inputUserLabel("TL_inputPeerUser"); got != "" {
		t.Errorf("inputUserLabel of a class of another type = %q, want none", got)
	}
}

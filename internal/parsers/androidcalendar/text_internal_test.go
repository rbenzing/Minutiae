package androidcalendar

import (
	"strings"
	"testing"
)

// Review M2: a stored text over its limit is refused before it is copied (validText never shrinks its input), so a
// 1 MiB hostile identifier or duration costs no allocation; one within the limit whose invalid bytes grow past it as
// U+FFFD is refused after cleaning, and one that stays within it is the cleaned text.
func TestCleanWithinRefusesAnOverLongTextWithoutACopy(t *testing.T) {
	big := []byte(strings.Repeat("\xff", 1<<20))
	if n := testing.AllocsPerRun(5, func() {
		if _, ok := cleanWithin(big, maxRule); ok {
			t.Fatal("a 1 MiB text within the rule limit")
		}
	}); n != 0 {
		t.Errorf("cleanWithin allocated %v times for a text over its limit: it must refuse before copying", n)
	}
	if _, ok := cleanWithin([]byte(strings.Repeat("\xe2\x82", 15)), maxDurationLen); ok {
		t.Error("30 stored bytes that clean to 90 are within the 64-byte duration limit")
	}
	if s, ok := cleanWithin([]byte("PT1H\xff"), maxDurationLen); !ok || s != "PT1H\uFFFD" {
		t.Errorf("cleanWithin = %q, %v; want the cleaned text", s, ok)
	}
}

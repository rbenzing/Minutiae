package evidence

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFTSTrigramFoldMatchesTheTrigramReplica: the 3-rune windows of the fold are the terms the
// trigram tokenizer emits, in order.
func TestFTSTrigramFoldMatchesTheTrigramReplica(t *testing.T) {
	corpus := append([]string{}, normProbeCorpus()...)
	corpus = append(corpus, strings.Repeat("ab"+string(rune(0x301)), 50),
		string([]rune{0x1f600, ' ', 0x2764, 0xfe0f, ' ', 'x'}),
		string([]rune{'c', 'a', 'f', 0xe9, ' ', 'C', 'A', 'F', 0xc9, ' ', 0xdf, ' ', 0x130, ' ', 0xff21, 0xff22, 0xff23}))
	for _, raw := range corpus {
		s := NormalizeText(raw)
		var want []string
		tokenizeTrigram(s, func(term []byte, _ int) { want = append(want, string(term)) })
		key := FTSTrigramFold(s)
		var runes []string
		for i := 0; i < len(key); {
			_, size := utf8.DecodeRuneInString(key[i:])
			runes = append(runes, key[i:i+size])
			i += size
		}
		var got []string
		for i := 0; i+3 <= len(runes); i++ {
			got = append(got, runes[i]+runes[i+1]+runes[i+2])
		}
		if !slices.Equal(got, want) {
			t.Fatalf("fold windows of %q = %q, tokenizer terms %q", raw, got, want)
		}
	}
}

package evidence

import "strings"

// FTSTrigramFold is the text the trigram index sees of normalized text: exactly the runes the trigram
// tokenizer keeps (fold != 0), folded, written the way its terms are written. A trigram term is three
// consecutive runes of this text, so a needle of three or more kept characters matches a column
// exactly when the needle's key is a substring of the column's key. Scan search (L85) uses it to
// answer needles the index cannot hold with the index's own semantics.
func FTSTrigramFold(normalized string) string {
	tab := ftsTokTables()
	var b strings.Builder
	var arr [4]byte
	for i := 0; i < len(normalized); {
		c, size := ftsRuneAt(normalized, i)
		i += size
		if _, f := tab.get(c); f != 0 {
			b.Write(appendFTSRune(arr[:0], f))
		}
	}
	return b.String()
}

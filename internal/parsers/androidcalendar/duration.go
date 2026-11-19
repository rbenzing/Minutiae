package androidcalendar

// maxDurationLen is the longest Events.duration text the grammar reads (spec 5.3 and 9): a longer text is never cut
// and never parsed.
const maxDurationLen = 64

// The bounds of spec 5.3: a digit run of at most 9 digits, a total within the Java int the provider sums in.
const (
	maxDurationDigits = 9
	maxDurationSecs   = 2147483647
)

// The duration states (raw.duration_state takes "unparsed" for "too-long": the coordinator's ruling on the oracle).
const (
	durUnparsed   = "unparsed"
	durOutOfRange = "out-of-range"
	durTooLong    = "too-long"
)

// unitSecs returns the seconds of a unit as the provider counts them (a week is 604,800 seconds and a day 86,400,
// whatever the calendar does that week) and the unit's bit in the set of units seen; false for any other byte.
func unitSecs(c byte) (secs int64, bit uint8, ok bool) {
	switch c {
	case 'W':
		return 604800, 1, true
	case 'D':
		return 86400, 2, true
	case 'H':
		return 3600, 4, true
	case 'M':
		return 60, 8, true
	case 'S':
		return 1, 16, true
	}
	return 0, 0, false
}

// parseDuration reads the strict subset of the provider's duration grammar (spec 5.3): an optional sign, P, then one
// or more runs of digits each followed by one of W D H M S, with at most one T before any run. Each unit comes at most
// once (a repeated unit is one the provider would silently override); a digit run with no unit, any other character
// and an empty body are "unparsed". A grammatical text with a run of more than 9 digits, or whose total exceeds the
// Java int in absolute value, is "out-of-range"; a text over 64 bytes is "too-long" and is not read. secs is 0
// unless state is "".
func parseDuration(s []byte) (secs int64, state string) {
	if len(s) > maxDurationLen {
		return 0, durTooLong
	}
	i, sign := 0, int64(1)
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		if s[i] == '-' {
			sign = -1
		}
		i++
	}
	if i >= len(s) || s[i] != 'P' {
		return 0, durUnparsed
	}
	i++
	var seenT, over bool
	var seen uint8
	runs, total := 0, int64(0)
	for i < len(s) {
		if s[i] == 'T' {
			if seenT {
				return 0, durUnparsed
			}
			seenT = true
			i++
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i || j >= len(s) {
			return 0, durUnparsed // no digits (a T at the end, a stray character), or a digit run without a unit
		}
		unit, bit, ok := unitSecs(s[j])
		if !ok || seen&bit != 0 {
			return 0, durUnparsed
		}
		seen |= bit
		if j-i > maxDurationDigits {
			over = true
		} else {
			var n int64
			for _, d := range s[i:j] {
				n = n*10 + int64(d-'0')
			}
			total += n * unit // at most 5 runs of 999,999,999 weeks: far inside int64
		}
		runs++
		i = j + 1
	}
	if runs == 0 {
		return 0, durUnparsed
	}
	if over || total > maxDurationSecs {
		return 0, durOutOfRange
	}
	return sign * total, ""
}

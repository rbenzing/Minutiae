package chromium

// coreName names the core page transition (the low 8 bits), from the cross-checked table; any other core is
// "unknown", never guessed.
func coreName(core uint32) string {
	switch core {
	case 0:
		return "link"
	case 1:
		return "typed"
	case 2:
		return "auto_bookmark"
	case 3:
		return "auto_subframe"
	case 4:
		return "manual_subframe"
	case 5:
		return "generated"
	case 6:
		return "auto_toplevel"
	case 7:
		return "form_submit"
	case 8:
		return "reload"
	case 9:
		return "keyword"
	case 10:
		return "keyword_generated"
	}
	return "unknown"
}

type qualifier struct {
	mask uint32
	name string
}

// qualifiers are the known qualifier bits in ascending bit order (4F-R29).
func qualifiers() [9]qualifier {
	return [9]qualifier{
		{0x00800000, "blocked"},
		{0x01000000, "forward_back"},
		{0x02000000, "from_address_bar"},
		{0x04000000, "home_page"},
		{0x08000000, "from_api"},
		{0x10000000, "chain_start"},
		{0x20000000, "chain_end"},
		{0x40000000, "client_redirect"},
		{0x80000000, "server_redirect"},
	}
}

// decodeTransition reads visits.transition as a uint32 (Chromium stores the qualifier bits in a signed 32-bit
// column, so a server redirect is negative on disk, 4F-R7). ok is false for a value outside 32 bits. Bits that are
// neither core nor a known qualifier come back in unknownBits.
func decodeTransition(v int64) (core string, quals []string, unknownBits int64, ok bool) {
	if v < -(1<<31) || v > 1<<32-1 {
		return "", nil, 0, false
	}
	u := uint32(v) //nolint:gosec // v is within [-2^31, 2^32-1]: the conversion keeps the 32 stored bits
	core = coreName(u & 0xFF)
	rest := u &^ 0xFF
	for _, q := range qualifiers() {
		if rest&q.mask != 0 {
			quals = append(quals, q.name)
			rest &^= q.mask
		}
	}
	return core, quals, int64(rest), true
}

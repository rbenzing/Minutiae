package androidtelegram

import (
	"encoding/hex"

	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The caps of the poll lists (plan 4I4c "Raw shapes"): a list is cut at its cap and states its total and that it was
// cut (ruling T5r1), a text is cut at its cap and named in raw.text_flags, an identifier or an option is never cut.
const (
	maxAnswers      = 32 // poll_answers, and poll_results_options
	maxPollVoters   = 64 // poll_voters, the voters of every option flattened
	maxRecentVoters = 16 // poll_results.recent_voters
	maxCountries    = 64 // poll.countries_iso2
	maxOption       = 64 // bytes of a poll option stated as hex
)

const (
	keyPoll        = "media.poll"
	keyPollResults = "media.results"
)

// inputPeerLabel is the label of an InputPeer class (prefix TL_inputPeer): "user", "chat", "channel", "self", "empty",
// "user_from_message", "channel_from_message"; "" for a class of another prefix.
func inputPeerLabel(class string) string { return classLabel(class, "TL_inputPeer") }

// optionInto states a poll option b in m (ruling Q9, D9): lowercase hex up to maxOption bytes ("" for zero bytes, an
// exact value), else option_state too-long with option_len and option_sha256, never cut.
func optionInto(m map[string]any, b []byte) {
	if len(b) <= maxOption {
		m["option"] = hex.EncodeToString(b)
		return
	}
	m["option_state"], m["option_len"], m["option_sha256"] = stateTooLong, int64(len(b)), sha256Hex(b)
}

// pollRaw states the poll of a MessageMedia poll wrapper in raw (design 9.3; plan 4I4c "Raw shapes", Polls): raw.poll
// and raw.poll_results (level 3, scalar-only) and, at the top of raw (B1), raw.poll_answers, raw.poll_results_options and
// raw.poll_voters. Raw only: never a Body, a time or a participant (rulings Q12-Q14), the voters and the author of an
// answer are addresses. An object is stated only when read whole, its lists with it; a stop at or inside it is
// <key>_state "partial" and none of its lists (P3). No unit is invented for a date or a period (D10); the nested media is
// stated by its label and constructor only (D11).
func pollRaw(v view, tab tl.Table, raw map[string]any) {
	switch {
	case v.Whole(keyPoll):
		pollOf(v, tab, raw)
	case v.inside(keyPoll):
		raw["poll_state"] = statePartial
	}
	switch {
	case v.Whole(keyPollResults):
		pollResultsOf(v, tab, raw)
	case v.inside(keyPollResults):
		raw["poll_results_state"] = statePartial
	}
}

// nestedMedia states the label and constructor of the media object read at path, under keys <prefix>_type and
// <prefix>_constructor (nothing when no object of a known class was read there).
func nestedMedia(v view, tab tl.Table, m map[string]any, prefix, path string) {
	if id, c, ok := entryAt(v, tab, path); ok {
		m[prefix+"_type"], m[prefix+"_constructor"] = mediaLabel(c.Name), hexID(id)
	}
}

// pollOf is raw.poll of a poll read whole, and the poll_answers list of its answers.
func pollOf(v view, tab tl.Table, raw map[string]any) {
	id, _, _ := entryAt(v, tab, keyPoll)
	o := map[string]any{"constructor": hexID(id)}
	if x, ok := v.Int(keyPoll + ".id"); ok {
		o["id"] = x
	}
	for _, f := range []string{
		"closed", "public_voters", "multiple_choice", "quiz", "open_answers", "revoting_disabled",
		"shuffle_answers", "hide_results_until_close", "creator", "subscribers_only",
	} {
		if v.True(keyPoll + "." + f) {
			o[f] = true
		}
	}
	if b, ok := v.Str(keyPoll + ".question.text"); ok { // question.text in every class (a name difference)
		cleanInto(o, raw, "question", "poll.question", b, maxShort)
	}
	if n, ok := v.Len(keyPoll + ".question.entities"); ok {
		o["question_entities_total"] = int64(n)
	}
	for _, k := range []string{"close_period", "close_date"} {
		if x, ok := v.Int(keyPoll + "." + k); ok {
			o[k], o[k+"_unit"] = x, unitUnestablished
		}
	}
	countriesInto(v, o, raw)
	if x, ok := v.Int(keyPoll + ".hash"); ok {
		o["hash"] = x
	}
	if n, ok := v.Len(keyPoll + ".answers"); ok {
		o["answers_total"] = int64(n)
		if n > maxAnswers {
			o["answers_truncated"] = true
		}
		answers := []any{}
		for i := range min(n, maxAnswers) {
			answers = append(answers, pollAnswerOf(v, tab, raw, elem(keyPoll+".answers", i), i))
		}
		if len(answers) > 0 {
			raw["poll_answers"] = answers
		}
	}
	nestedMedia(v, tab, o, "attached_media", "media.attached_media")
	raw["poll"] = o
}

// countriesInto states the country list of a poll: its total, its cut when it was cut (T5r1) and the first maxCountries
// codes as identifiers; one code over maxTiny bytes makes it countries_iso2_state "too-long" and no list (an identifier is
// never cut), the total stays.
func countriesInto(v view, o, raw map[string]any) {
	const p = keyPoll + ".countries_iso2"
	n, ok := v.Len(p)
	if !ok {
		return
	}
	o["countries_iso2_total"] = int64(n)
	if n > maxCountries {
		o["countries_iso2_truncated"] = true
	}
	var codes []any
	flagged := false
	for i := range n {
		b, _ := v.Str(elem(p, i))
		cl := common.CleanText(b, maxTiny)
		if len(b) > maxTiny || cl.Truncated {
			o["countries_iso2_state"] = stateTooLong
			return
		}
		if i < maxCountries {
			codes = append(codes, cl.Text)
			flagged = flagged || len(cl.Flags) > 0
		}
	}
	if flagged {
		markText(raw, "poll.countries_iso2")
	}
	if len(codes) > 0 {
		o["countries_iso2"] = codes
	}
}

// pollAnswerOf is one scalar-only entry of raw.poll_answers: the PollAnswer at a, index i (0-based, wire order).
func pollAnswerOf(v view, tab tl.Table, raw map[string]any, a string, i int) map[string]any {
	id, _, _ := entryAt(v, tab, a)
	x := map[string]any{"index": int64(i), "constructor": hexID(id)}
	if b, ok := v.Str(a + ".text.text"); ok {
		cleanInto(x, raw, "text", "poll_answers.text", b, maxName)
	}
	if n, ok := v.Len(a + ".text.entities"); ok {
		x["entities_total"] = int64(n)
	}
	if b, ok := v.Str(a + ".option"); ok {
		optionInto(x, b)
	}
	nestedMedia(v, tab, x, "media", a+".media")
	if addr, ok := peerOf(v, a+".added_by"); ok { // an address, never a participant
		x["added_by"] = addr
	}
	if n, ok := v.Int(a + ".date"); ok {
		x["date"], x["date_unit"] = n, unitUnestablished
	}
	return x
}

// pollResultsOf is raw.poll_results of results read whole, and the lists of its options and their voters.
func pollResultsOf(v view, tab tl.Table, raw map[string]any) {
	id, _, _ := entryAt(v, tab, keyPollResults)
	o := map[string]any{"constructor": hexID(id)}
	for _, f := range []string{"min", "has_unread_votes", "can_view_stats"} {
		if v.True(keyPollResults + "." + f) {
			o[f] = true
		}
	}
	if x, ok := v.Int(keyPollResults + ".total_voters"); ok {
		o["total_voters"] = x
	}
	if b, ok := v.Str(keyPollResults + ".solution"); ok {
		cleanInto(o, raw, "solution", "poll_results.solution", b, maxShort)
	}
	if n, ok := v.Len(keyPollResults + ".solution_entities"); ok {
		o["solution_entities_total"] = int64(n)
	}
	nestedMedia(v, tab, o, "solution_media", keyPollResults+".solution_media")
	const rv = keyPollResults + ".recent_voters"
	if n, ok := v.Len(rv); ok { // a vec<idNN@user> voter is user:<id> as stored
		var addrs []any
		for i := range min(n, maxRecentVoters) {
			if a, ok := peerOf(v, elem(rv, i)); ok {
				addrs = append(addrs, a)
			}
		}
		if len(addrs) > 0 {
			o["recent_voters"] = addrs
		}
		o["recent_voters_total"] = int64(n)
		if n > maxRecentVoters {
			o["recent_voters_truncated"] = true
		}
	}
	optionsInto(v, o, raw)
	raw["poll_results"] = o
}

// optionsInto states the per-option results (raw.poll_results_options, at most maxAnswers) and the voters of every option
// flattened in wire order (raw.poll_voters, at most maxPollVoters: a list of lists would be too deep, B1), with the totals
// in o. An option and a voter's option follow the rule of an answer's (optionInto).
func optionsInto(v view, o, raw map[string]any) {
	const rp = keyPollResults + ".results"
	n, ok := v.Len(rp)
	if !ok {
		return
	}
	o["results_total"] = int64(n)
	if n > maxAnswers {
		o["results_truncated"] = true
	}
	var options, voters []any
	total := 0
	for i := range n {
		e := elem(rp, i)
		option, hasOption := v.Str(e + ".option")
		if i < maxAnswers {
			x := map[string]any{}
			if hasOption {
				optionInto(x, option)
			}
			for _, f := range []string{"chosen", "correct"} { // the stored flags of the device's account, never anyone's vote
				if v.True(e + "." + f) {
					x[f] = true
				}
			}
			if k, ok := v.Int(e + ".voters"); ok {
				x["voters"] = k
			}
			if k, ok := v.Len(e + ".recent_voters"); ok {
				x["recent_voters_total"] = int64(k)
			}
			options = append(options, x)
		}
		k, _ := v.Len(e + ".recent_voters")
		for j := range k {
			if len(voters) < maxPollVoters {
				x := map[string]any{}
				if hasOption {
					optionInto(x, option)
				}
				if a, ok := peerOf(v, elem(e+".recent_voters", j)); ok {
					x["peer"] = a
				}
				voters = append(voters, x)
			}
			total++
		}
	}
	o["option_voters_total"] = int64(total)
	if total > maxPollVoters {
		o["option_voters_truncated"] = true
	}
	if len(options) > 0 {
		raw["poll_results_options"] = options
	}
	if len(voters) > 0 {
		raw["poll_voters"] = voters
	}
}

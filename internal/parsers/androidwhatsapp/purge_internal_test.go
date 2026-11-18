package androidwhatsapp

import (
	"reflect"
	"testing"
)

func ptr(v int64) *int64 { return &v }

func key(chat, order, starred int64) rowKey {
	return rowKey{Chat: chat, ChatKnown: true, Starred: starred, StarredOK: true, Order: order, OrderOK: true}
}

// 4I-R4 (c): any job of the row's own chat that covers it clears it; a starred row is judged by the starred bound; a NULL
// bound covers nothing; the result lists every clearing job in rowid order.
func TestEvalClearsByAnyBoundOfItsOwnChat(t *testing.T) {
	js := &jobSet{Complete: true, ByChat: map[int64][]jobBound{
		7: {{ID: 1, Msg: ptr(10), BoundOK: true}, {ID: 2, Msg: ptr(20), Starred: ptr(30), BoundOK: true}, {ID: 3, BoundOK: true}},
		8: {{ID: 4, Msg: ptr(1000), BoundOK: true}},
	}}
	cases := []struct {
		name string
		k    rowKey
		want purgeResult
	}{
		{"covered by both", key(7, 5, 0), purgeResult{Verdict: purgeCleared, Jobs: []int64{1, 2}, Job: 1}},
		{"covered by the second only", key(7, 15, 0), purgeResult{Verdict: purgeCleared, Jobs: []int64{2}, Job: 2}},
		{"above every bound", key(7, 25, 0), purgeResult{}},
		{"starred, the starred bound reaches it", key(7, 25, 1), purgeResult{Verdict: purgeCleared, Jobs: []int64{2}, Job: 2}},
		{"starred above the starred bound", key(7, 31, 1), purgeResult{}},
		{"the bound is its own", key(7, 10, 0), purgeResult{Verdict: purgeCleared, Jobs: []int64{1, 2}, Job: 1}},
		{"another chat's job never applies", key(9, 5, 0), purgeResult{}},
		{"a chat with no job", key(7000, 1, 0), purgeResult{}},
		{"no chat known", rowKey{Order: 1, OrderOK: true, StarredOK: true}, purgeResult{}},
	}
	for _, c := range cases {
		if got := js.eval(c.k); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := (*jobSet)(nil).eval(key(7, 5, 0)); got.Verdict != purgeNone {
		t.Errorf("a nil set: %+v", got)
	}
}

// 4I-R4 (e): what cannot be decided is undecided with its reason and the smallest job involved; a job that clears the
// row wins over a job that cannot be evaluated.
func TestEvalUndecidedReasons(t *testing.T) {
	js := &jobSet{Complete: true, minID: 1, Read: 4, ByChat: map[int64][]jobBound{
		1: {{ID: 1, Msg: ptr(1), BoundOK: true, Lists: true}},
		2: {{ID: 2, Msg: nil, BoundOK: false}},
		3: {{ID: 3, Msg: ptr(5), BoundOK: true}},
		4: {{ID: 4, Msg: ptr(100), BoundOK: true, Lists: true}},
	}}
	und := func(job int64, reason string) purgeResult {
		return purgeResult{Verdict: purgeUndecided, Job: job, Reason: reason}
	}
	for name, c := range map[string]struct {
		k    rowKey
		want purgeResult
	}{
		"a list":                          {key(1, 50, 0), und(1, reasonJobLists)},
		"a bound that is not an integer":  {key(2, 50, 0), und(2, reasonJobBound)},
		"an order key that is no integer": {rowKey{Chat: 3, ChatKnown: true, StarredOK: true}, und(3, reasonRowKey)},
		"a starred flag outside 0 and 1":  {rowKey{Chat: 3, ChatKnown: true, Order: 1, OrderOK: true}, und(3, reasonRowKey)},
		"cleared beats a list":            {key(4, 50, 0), purgeResult{Verdict: purgeCleared, Jobs: []int64{4}, Job: 4}},
		"a chat that cannot be decided":   {rowKey{ChatUndecided: true}, und(1, reasonRowChat)},
		"no job at all in another chat":   {key(9, 1, 0), purgeResult{}},
		"a key problem with no job":       {rowKey{Chat: 9, ChatKnown: true}, purgeResult{}},
		"the row is cleared":              {key(3, 5, 0), purgeResult{Verdict: purgeCleared, Jobs: []int64{3}, Job: 3}},
	} {
		if got := js.eval(c.k); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}

	// an incomplete set: every row a read job does not clear is undecided, naming a job of its chat when one was read
	inc := &jobSet{Complete: false, Read: 1, minID: 1, ByChat: map[int64][]jobBound{1: {{ID: 1, Msg: ptr(10), BoundOK: true}}}}
	for name, c := range map[string]struct {
		k    rowKey
		want purgeResult
	}{
		"cleared by a read job":       {key(1, 5, 0), purgeResult{Verdict: purgeCleared, Jobs: []int64{1}, Job: 1}},
		"not cleared, a job was read": {key(1, 50, 0), und(1, reasonJobSet)},
		"no job of its chat was read": {key(2, 50, 0), und(0, reasonJobSet)},
		"a chat that cannot be known": {rowKey{ChatUndecided: true}, und(1, reasonRowChat)},
	} {
		if got := inc.eval(c.k); !reflect.DeepEqual(got, c.want) {
			t.Errorf("incomplete, %s: %+v, want %+v", name, got, c.want)
		}
	}
	// an unusable job makes the rows of chats with no clearing job undecided, and leaves the set complete
	unusable := &jobSet{Complete: true, Unusable: 1, Read: 1, minID: 1, ByChat: map[int64][]jobBound{}}
	if got := unusable.eval(key(5, 1, 0)); !reflect.DeepEqual(got, und(0, reasonJobSet)) {
		t.Errorf("unusable: %+v", got)
	}
	if !unusable.active() || !unusable.incomplete() || !unusable.Complete {
		t.Errorf("an unusable job must activate the evaluation and leave the set complete")
	}
}

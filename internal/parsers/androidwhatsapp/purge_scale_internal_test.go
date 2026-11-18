package androidwhatsapp

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

// The review of plan 4I (fix 5), I3: the evaluation of a row does not walk every job of its chat. oldEval is the evaluation
// as it was before the per-chat aggregates, kept here as the reference for an equivalence check.
func oldEval(js *jobSet, k rowKey) purgeResult {
	if js == nil {
		return purgeResult{}
	}
	if k.ChatUndecided {
		switch {
		case js.anyJob():
			return purgeResult{Verdict: purgeUndecided, Job: js.minID, Reason: reasonRowChat}
		case js.incomplete():
			return purgeResult{Verdict: purgeUndecided, Reason: reasonJobSet}
		}
		return purgeResult{}
	}
	if !k.ChatKnown {
		return purgeResult{}
	}
	jobs := js.ByChat[k.Chat]
	if k.StarredOK && k.OrderOK {
		var ids []int64
		for _, j := range jobs {
			if j.covers(k) {
				ids = append(ids, j.ID)
			}
		}
		if len(ids) > 0 {
			return purgeResult{Verdict: purgeCleared, Jobs: ids, Job: ids[0]}
		}
	}
	for _, j := range jobs {
		if j.Lists {
			return purgeResult{Verdict: purgeUndecided, Job: j.ID, Reason: reasonJobLists}
		}
	}
	for _, j := range jobs {
		if !j.BoundOK {
			return purgeResult{Verdict: purgeUndecided, Job: j.ID, Reason: reasonJobBound}
		}
	}
	if len(jobs) > 0 && (!k.StarredOK || !k.OrderOK) {
		return purgeResult{Verdict: purgeUndecided, Job: jobs[0].ID, Reason: reasonRowKey}
	}
	if js.incomplete() {
		r := purgeResult{Verdict: purgeUndecided, Reason: reasonJobSet}
		if len(jobs) > 0 {
			r.Job = jobs[0].ID
		}
		return r
	}
	return purgeResult{}
}

// asOld states a new result in the shape the old evaluation gave: the list capped and the full count apart.
func asOld(r purgeResult, all []int64) purgeResult { r.Jobs, r.Total = all, 0; return r }

func TestEvalAggregatesGiveTheVerdictsOfTheFullWalk(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	bound := func() *int64 {
		if rng.Intn(4) == 0 {
			return nil
		}
		return ptr(int64(rng.Intn(40)))
	}
	for round := 0; round < 300; round++ {
		js := &jobSet{Complete: rng.Intn(5) != 0, Read: 1, minID: 1, ByChat: map[int64][]jobBound{}}
		id := int64(0)
		for chat := int64(1); chat <= 3; chat++ {
			n := rng.Intn(6)
			if round%50 == 0 && chat == 1 {
				n = 1500 // past the listing cap
			}
			for i := 0; i < n; i++ {
				id++
				js.ByChat[chat] = append(js.ByChat[chat], jobBound{ID: id, Msg: bound(), Starred: bound(), BoundOK: rng.Intn(8) != 0, Lists: rng.Intn(12) == 0})
			}
		}
		for i := 0; i < 60; i++ {
			k := rowKey{
				Chat: int64(1 + rng.Intn(4)), ChatKnown: rng.Intn(10) != 0, ChatUndecided: rng.Intn(15) == 0,
				Starred: int64(rng.Intn(2)), StarredOK: rng.Intn(10) != 0, Order: int64(rng.Intn(45)), OrderOK: rng.Intn(10) != 0,
			}
			want := oldEval(js, k)
			got := js.eval(k)
			if got.Verdict == purgeCleared && got.Total > 0 {
				// 4I-R24: past the cap the list is maxPurgeJobs of the covering jobs, in id order, the total is the full count
				cover := map[int64]bool{}
				for _, id := range want.Jobs {
					cover[id] = true
				}
				if len(got.Jobs) != maxPurgeJobs || got.Total != len(want.Jobs) || !slices.IsSorted(got.Jobs) {
					t.Fatalf("round %d %+v: capped list %d total %d sorted %v, want %d of %d", round, k, len(got.Jobs), got.Total, slices.IsSorted(got.Jobs), maxPurgeJobs, len(want.Jobs))
				}
				for _, id := range got.Jobs {
					if !cover[id] {
						t.Fatalf("round %d %+v: listed job %d does not cover the row", round, k, id)
					}
				}
				got = asOld(got, want.Jobs)
			} else if got.Total != 0 {
				t.Fatalf("round %d %+v: total %d on a list that is whole", round, k, got.Total)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d %+v: %+v, want %+v", round, k, got, want)
			}
		}
	}
}

// With 100,000 jobs on one chat (the cap, still complete) a row costs at most the listing cap in job visits, never the
// number of jobs: the work counter of the set counts every job a row visits.
func TestEvalWorkIsBoundedByTheListingCapNotTheJobCount(t *testing.T) {
	const jobs, rows = maxJobs, 200
	js := &jobSet{Complete: true, Read: jobs, minID: 1, ByChat: map[int64][]jobBound{}}
	for i := int64(1); i <= jobs; i++ {
		js.ByChat[1] = append(js.ByChat[1], jobBound{ID: i, Msg: ptr(1000 + i%7), Starred: ptr(5), BoundOK: true})
	}
	for i := int64(0); i < rows; i++ {
		// even rows are cleared by every job, odd rows lie above every bound
		order := i
		if i%2 == 1 {
			order = 5000 + i
		}
		r := js.eval(key(1, order, 0))
		switch {
		case i%2 == 0 && (r.Verdict != purgeCleared || len(r.Jobs) != maxPurgeJobs || r.Total != jobs || r.Job != 1):
			t.Fatalf("row %d: %d ids, total %d, job %d, verdict %v", i, len(r.Jobs), r.Total, r.Job, r.Verdict)
		case i%2 == 1 && r.Verdict != purgeNone:
			t.Fatalf("row %d: %+v, want none", i, r)
		}
	}
	// the first call builds the aggregates of the chat (one pass over its jobs, counted once); each row then visits at most
	// the listing cap
	if limit := int64(jobs) + int64(rows)*maxPurgeJobs; js.work > limit {
		t.Fatalf("work %d over %d rows of %d jobs, want at most %d", js.work, rows, jobs, limit)
	}
	before := js.work
	js.eval(key(1, 1, 0))
	if d := js.work - before; d > maxPurgeJobs {
		t.Fatalf("one more row visited %d jobs, want at most %d", d, maxPurgeJobs)
	}
	// a row that no job covers, of a chat with 100,000 jobs, visits none
	before = js.work
	js.eval(key(1, 999_999, 0))
	if d := js.work - before; d != 0 {
		t.Fatalf("an uncovered row visited %d jobs, want 0", d)
	}
}

// 4I-R24: when more than maxPurgeJobs jobs cover a row, the listed ids are the maxPurgeJobs covering jobs with the nearest
// bounds (ties by id), in id order, and a row costs at most maxPurgeJobs job visits however the jobs lie in rowid order.
func TestEvalTruncatedListingTakesTheNearestBoundsWithBoundedWork(t *testing.T) {
	const low, high = 5000, maxPurgeJobs + 1
	js := &jobSet{Complete: true, Read: low + high, minID: 1, ByChat: map[int64][]jobBound{}}
	for i := int64(1); i <= low; i++ {
		js.ByChat[1] = append(js.ByChat[1], jobBound{ID: i, Msg: ptr(10), BoundOK: true})
	}
	for i := int64(1); i <= high; i++ {
		js.ByChat[1] = append(js.ByChat[1], jobBound{ID: low + i, Msg: ptr(1000 + i), BoundOK: true})
	}
	js.agg(1) // built once, not charged to a row
	ids := func(from, n int64) []int64 {
		var out []int64
		for i := from; i < from+n; i++ {
			out = append(out, i)
		}
		return out
	}
	for _, c := range []struct {
		name  string
		order int64
		total int
		want  []int64
	}{
		{"only the high jobs cover it", 500, high, ids(low+1, maxPurgeJobs)},
		{"every job covers it: the nearest bounds are the low jobs", 5, low + high, ids(1, maxPurgeJobs)},
		{"part of the high jobs (bounds 1500 and up: 502 jobs, listed whole)", 1500, 502, ids(low+500, 502)},
	} {
		before := js.work
		r := js.eval(key(1, c.order, 0))
		if d := js.work - before; d > maxPurgeJobs {
			t.Errorf("%s: %d job visits, want at most %d", c.name, d, maxPurgeJobs)
		}
		want := c.want
		wantTotal := c.total
		if c.total <= maxPurgeJobs {
			wantTotal = 0
		}
		if r.Verdict != purgeCleared || r.Total != wantTotal || !reflect.DeepEqual(r.Jobs, want) || r.Job != c.want[0] {
			t.Errorf("%s: verdict %v total %d jobs %d (first %v) job %d, want total %d and %d ids from %d", c.name, r.Verdict, r.Total, len(r.Jobs), r.Jobs[:min(3, len(r.Jobs))], r.Job, wantTotal, len(want), want[0])
		}
	}
}

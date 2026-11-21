package androidgooglemessages

import "github.com/rbenzing/minutiae/internal/parse"

// idCost is what one decided message id is charged to the job budget (4I5-P6, B6): the map entry and its overhead.
const idCost = 48

// idSet is the set of decided message _ids (B6): every messages row the live pass mapped. The parts pass reads it instead of
// a Get per part. It is complete only when every messages row was read and mapped; a refused budget charge, an unreadable
// _id, a row that was not mapped or a messages table the census could not read whole make it incomplete, and a part whose
// message is not in an incomplete set is undecided, never an orphan.
type idSet struct {
	ids      map[int64]struct{}
	complete bool
	cause    string // why the set is incomplete: the first cause met
	charged  int64
	stopped  bool // a charge was refused: no id is added any more
	b        *parse.BudgetView
}

func newIDSet(b *parse.BudgetView) *idSet {
	return &idSet{ids: map[int64]struct{}{}, complete: true, b: b}
}

// add records a decided message id, charged to the budget; a refusal makes the set incomplete (cause "budget") and stops
// the adding (an id that is not in the set is then undecided, never an orphan).
func (s *idSet) add(id int64) {
	if s.stopped {
		return
	}
	if _, dup := s.ids[id]; dup {
		return
	}
	if err := s.b.Alloc(idCost); err != nil {
		s.stopped = true
		s.markIncomplete("budget")
		return
	}
	s.charged += idCost
	s.ids[id] = struct{}{}
}

// markIncomplete says the set does not hold every message id; the first cause is kept.
func (s *idSet) markIncomplete(cause string) {
	if s.complete {
		s.complete, s.cause = false, cause
	}
}

func (s *idSet) has(id int64) bool {
	_, ok := s.ids[id]
	return ok
}

// release gives the charged budget back (Parse calls it after the run).
func (s *idSet) release() {
	if s == nil {
		return
	}
	s.b.Free(s.charged)
	s.charged = 0
	s.ids = map[int64]struct{}{}
}

package androidgooglemessages

import (
	"errors"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// memberCost is what one cached member is charged to the job budget (4I5-P6): this plus the length of its address.
const memberCost = 96

// setBaseCost is what every cached member set is charged before its members.
const setBaseCost = 256

// memberSet is the members of one conversation as the mappings state them (spec 6.3, B2, B3): the current non-self
// participants in conversation_participants rowid order, at most maxList of them, with the counts of all of them.
type memberSet struct {
	Parties        []party // the kept members that have an address
	Total          int     // the non-self members (before the cut)
	Cut            bool    // Total > maxList
	Dangling       int     // member rows naming no participants row (or an unreadable participant_id)
	Without        int     // kept members with no usable address
	Blocked        []int64 // ids of the kept members whose blocked is 1
	SelfUndecided  bool    // a member whose self-ness cannot be decided stays a member (B2)
	GroupUndecided bool    // a member row whose participant_id cannot be read: the member count is undecided, so is group
	Undecided      bool    // the members cannot be decided: the join is undecided, never "none"
	Joins          []string
	Unknown        map[string]any // the unreadable cells met, keyed <table>.<col>
}

func (s *memberSet) cost() int64 {
	n := int64(0)
	for _, p := range s.Parties {
		n += memberCost + int64(len(p.Address)+len(p.Name))
	}
	// a fixed base per set: conversations with no member at all still grow the charge (they cannot fill the cache uncharged)
	return n + int64(len(s.Blocked))*8 + setBaseCost + int64(len(s.Unknown))*64 + int64(len(s.Joins))*16
}

// memberCache holds the members of each conversation, read once per job (B3) and charged to the job budget (4I5-P6). A
// refused charge stops the caching: the members are then read per message (the same records, more work).
type memberCache struct {
	budget  *parse.BudgetView
	counts  *counts
	sets    map[int64]*memberSet
	charged int64
	stopped bool
	cutSeen map[int64]bool
}

func newMemberCache(b *parse.BudgetView, c *counts) *memberCache {
	return &memberCache{budget: b, counts: c, sets: map[int64]*memberSet{}, cutSeen: map[int64]bool{}}
}

// release gives the budget back (Parse calls it when it returns).
func (mc *memberCache) release() {
	if mc == nil {
		return
	}
	mc.budget.Free(mc.charged)
	mc.charged = 0
	mc.sets = map[int64]*memberSet{}
}

// noteCut remembers a conversation cut at maxList, once, for the warning after the run (B3).
func (mc *memberCache) noteCut(conv int64, total int) {
	if mc.cutSeen[conv] {
		return
	}
	mc.cutSeen[conv] = true
	if mc.counts == nil {
		return
	}
	mc.counts.CutConversations = append(mc.counts.CutConversations, conv)
	if mc.counts.CutTotals == nil {
		mc.counts.CutTotals = map[int64]int64{}
	}
	mc.counts.CutTotals[conv] = int64(total)
}

// membersOf is the member set of a conversation: from the cache when read before, else read (rowrun.Match on
// conversation_participants.conversation_id in rowid order, each participant by its rowid) and cached. The result is
// shared: callers never change it. mc may be nil (nothing is cached or counted).
func membersOf(m parse.MapContext, mc *memberCache, conv int64, pc rowrun.Cols) (*memberSet, error) {
	if mc != nil {
		if s, ok := mc.sets[conv]; ok {
			return s, nil
		}
	}
	s, err := readMembers(m, conv, pc)
	if err != nil {
		return nil, err
	}
	if mc == nil {
		return s, nil
	}
	if s.Cut {
		mc.noteCut(conv, s.Total)
	}
	if mc.stopped {
		return s, nil
	}
	cost := s.cost()
	switch err := mc.budget.Alloc(cost); {
	case err == nil:
		mc.charged += cost
		mc.sets[conv] = s
	case errors.Is(err, parse.ErrBudget):
		mc.stopped = true
	default:
		return nil, err
	}
	return s, nil
}

// readMembers reads the members of a conversation. Self rows are left out (B2); a member row naming no participant is
// counted (members_dangling); a join that cannot be decided (the index lost rows, a table the database lacks or cannot
// read, a guarded table) makes the whole set undecided and names the table.
func readMembers(m parse.MapContext, conv int64, pc rowrun.Cols) (*memberSet, error) {
	s := &memberSet{Unknown: map[string]any{}}
	rows, short, err := rowrun.Match(m, tableMembers, "conversation_id", conv)
	switch {
	case err != nil && joinUndecided(err):
		short = true
	case err != nil:
		return nil, err
	}
	if short {
		s.Undecided, s.Joins = true, []string{tableMembers}
		return s, nil
	}
	pidCol := m.Column(tableMembers, "participant_id")
	for _, mr := range rows {
		pid, k := intCellOf(mr, pidCol)
		if k != rowrun.CellValue {
			if k == rowrun.CellNull { // a NULL names no participant: dangling
				s.Dangling++
				continue
			}
			// an unreadable participant_id may name anyone, the device owner included: undecided, never dangling
			rowrun.Flag(s.Unknown, mr, tableMembers+".participant_id", pidCol, k)
			s.GroupUndecided = true
			continue
		}
		jraw := map[string]any{}
		prow, state, err := participantRow(m, pid, jraw)
		switch {
		case err != nil:
			return nil, err
		case prow == nil && state == stateUndecided:
			return &memberSet{Undecided: true, Joins: []string{tableParticipants}}, nil
		case prow == nil:
			s.Dangling++
			continue
		}
		sx := selfOf(prow, pc.At("sub_id"), s.Unknown)
		if sx == selfYes {
			continue
		}
		if sx == selfUndecided {
			s.SelfUndecided = true
		}
		s.Total++
		if s.Total > maxList {
			s.Cut = true
			continue
		}
		blocked := false
		if b, bk := intCellOf(prow, pc.At("blocked")); bk == rowrun.CellValue && b == 1 {
			blocked = true
			s.Blocked = append(s.Blocked, pid)
		}
		p, ok := partyFrom(pc, prow, nil, map[string]any{}, "")
		if !ok {
			s.Without++
			continue
		}
		p.ID, p.Self, p.Blocked = pid, sx, blocked
		s.Parties = append(s.Parties, p)
	}
	return s, nil
}

package ewf

import "fmt"

// tableGap records that chunk numbering is unknowable from chunk index from
// onward: a table that should have numbered the chunks after that point is
// missing, fails the structural rules or is disputed, so any later entry would
// be attached to a guessed index. Every chunk from that index on is unreadable
// (never served from a guess).
type tableGap struct {
	from int64
	why  string
}

// gapAt records the first gap (later ones add nothing, as everything after the
// first is already unreadable) and warns once. why is the failed condition.
func (o *opener) gapAt(n, group int, why string) {
	if o.r.gap != nil {
		return
	}
	from := int64(len(o.r.refs))
	o.r.gap = &tableGap{from: from, why: fmt.Sprintf("segment %d, chunk table group %d: %s; chunk numbering from index %d onward is unknown", n, group, why, from)}
	o.r.warn.add("segment %d, chunk table group %d: %s; chunks from index %d onward unreadable", n, group, why, from)
}

// tables resolves the chunk table groups of segment i. A group is a table
// section optionally followed immediately by its table2 copy, and must follow
// its sectors section directly. A group is TRUSTED only if
//
//	(a) it directly follows its sectors section,
//	(b) its first entry is that section's payload start (base + 76 in real
//	    output),
//	(c) its entries are strictly increasing (no duplicates), and
//	(d) every entry, and so the derived end of the last chunk, lies within
//	    that sectors section's payload.
//
// An untrusted table falls back to table2 when that satisfies (a)-(d); when
// neither does, the group is uncovered and, because the numbering after it is
// then unknowable, so is every later chunk (tableGap). A sectors section with
// no table group after it, or a table group that does not follow a sectors
// section, is the same failure. Both copies failing their own checksums stays a
// corrupt image (a CorruptError), as before.
func (o *opener) tables(i int, secs []section) error {
	n := i + 1
	s := o.r.segs[i]
	group := 0
	for j := 0; j < len(secs) && o.r.gap == nil; j++ {
		var sec section
		switch secs[j].kind {
		case kSectors:
			sec = secs[j]
			group++
			if j+1 >= len(secs) || (secs[j+1].kind != kTable && secs[j+1].kind != kTable2) {
				o.gapAt(n, group, fmt.Sprintf("the sectors section at offset %d has no chunk table after it", sec.off))
				continue
			}
			j++
		case kTable, kTable2:
			group++
			o.gapAt(n, group, fmt.Sprintf("the %s section at offset %d does not directly follow a sectors section", secs[j].label(), secs[j].off))
			continue
		default:
			continue
		}
		var tab, tab2 *section
		if secs[j].kind == kTable {
			tab = &secs[j]
			if j+1 < len(secs) && secs[j+1].kind == kTable2 {
				tab2 = &secs[j+1]
				j++
			}
		} else {
			tab2 = &secs[j]
		}
		if err := o.tableGroup(i, n, group, s, sec, tab, tab2); err != nil {
			return err
		}
	}
	return nil
}

// rules applies rules (b)-(d) of a group to its sectors section sec and
// returns the first one that fails ("" when all hold).
func (t *tbl) rules(sec section) string {
	start, end := sec.payloadOff(), sec.off+sec.psize
	if len(t.refs) == 0 {
		return "it has no entries"
	}
	prev := int64(-1)
	for k, r := range t.refs {
		off := r.off()
		switch {
		case r.outside():
			return fmt.Sprintf("rule (d): entry %d points outside the segment", k)
		case k == 0 && off != start:
			return fmt.Sprintf("rule (b): the first entry is at offset %d, not at the sectors payload start %d", off, start)
		case k > 0 && off <= prev:
			return fmt.Sprintf("rule (c): entry %d (offset %d) does not follow entry %d (offset %d)", k, off, k-1, prev)
		case off >= end:
			return fmt.Sprintf("rule (d): entry %d (offset %d) is outside the sectors payload [%d, %d)", k, off, start, end)
		}
		prev = off
	}
	return ""
}

// valid reports a table that passed its checksums and rules (b)-(d).
func (t *tbl) valid() bool { return t.trusted() && t.rule == "" }

// why describes why t is not usable, for warnings.
func (t *tbl) why() string {
	if !t.trusted() {
		return t.reason
	}
	return t.rule
}

func (o *opener) tableGroup(i, n, group int, s Segment, sec section, tab, tab2 *section) error {
	limit := int64(o.r.geo.chunks) - int64(len(o.r.refs))
	var a, b tbl
	var err error
	if tab != nil {
		if a, err = parseTable(i, s, *tab, limit); err != nil {
			return err
		}
		if a.trusted() {
			a.rule = a.rules(sec)
		}
	}
	if tab2 != nil {
		if b, err = parseTable(i, s, *tab2, limit); err != nil {
			return err
		}
		if b.trusted() {
			b.rule = b.rules(sec)
		}
	}
	// A present copy that fails its own checksums is unreadable; with no
	// readable copy at all the image is corrupt.
	aOK, bOK := tab != nil && a.trusted(), tab2 != nil && b.trusted()
	if !aOK && !bOK {
		switch {
		case tab != nil && tab2 != nil:
			return corrupt(n, "table", "table at offset %d unreadable (%s) and table2 at offset %d unreadable (%s)", a.off, a.reason, b.off, b.reason)
		case tab != nil:
			return corrupt(n, "table", "table at offset %d unreadable (%s) and has no table2", a.off, a.reason)
		default:
			return corrupt(n, "table2", "table2 at offset %d unreadable (%s) and has no table", b.off, b.reason)
		}
	}
	va, vb := tab != nil && a.valid(), tab2 != nil && b.valid()
	var use *tbl
	switch {
	case va && vb:
		use = &a
		switch d := firstDiff(a.refs, b.refs); {
		case d < 0:
		case len(a.refs) != len(b.refs):
			// The copies disagree on how many chunks the group holds, so the
			// numbering of everything after it is in doubt whichever is right.
			o.reconcile(n, &a, &b, d)
			return nil
		case a.footer != b.footer:
			// Both satisfy (a)-(d) but differ: the copy whose entries checksum
			// verified is the better evidence.
			if b.footer {
				use = &b
			}
			o.r.warn.add("segment %d: table at offset %d and its table2 differ (first at entry %d); using %s, whose entries checksum verified", n, a.off, d, use.label)
		default:
			o.reconcile(n, &a, &b, d)
			return nil
		}
	case va:
		use = &a
		if tab2 == nil {
			o.r.warn.add("segment %d: table at offset %d has no table2", n, a.off)
		} else {
			o.r.warn.add("segment %d: table2 at offset %d unusable (%s); using table", n, b.off, b.why())
		}
	case vb:
		use = &b
		if tab == nil {
			o.r.warn.add("segment %d: table2 at offset %d has no table", n, b.off)
		} else {
			o.r.warn.add("segment %d: table at offset %d unusable (%s); using table2", n, a.off, a.why())
		}
	default:
		// At least one copy passed its checksums but none satisfies (a)-(d).
		reason := ""
		if tab != nil {
			reason = "table: " + a.why()
		}
		if tab2 != nil {
			if reason != "" {
				reason += "; "
			}
			reason += "table2: " + b.why()
		}
		o.gapAt(n, group, "the chunk table is unusable ("+reason+")")
		return nil
	}
	o.addRefs(use.refs)
	return nil
}

// addRefs appends resolved entries, pre-sizing the slice on first use from the
// volume's chunk count, capped by what the segments' bytes could describe (a
// chunk needs a table entry and some data).
func (o *opener) addRefs(refs []chunkRef) {
	if o.r.refs == nil {
		var total int64
		for _, s := range o.r.segs {
			total += s.Size
		}
		o.r.refs = make([]chunkRef, 0, max(int64(len(refs)), min(int64(o.r.geo.chunks), total/8)))
	}
	o.r.refs = append(o.r.refs, refs...)
}

// reconcile appends the entries of a group whose table a and table2 b both
// satisfy (a)-(d) and pass their checksums (with the same footer status) but
// differ, first at entry d. Neither copy is believed: every differing entry is
// uncovered (its chunk is unreadable), the agreeing ones are used. When the two
// copies even disagree on the entry count, the numbering of every later chunk
// is in doubt as well: the entries both share are kept and every chunk after
// them is unreadable (see tableGap).
func (o *opener) reconcile(n int, a, b *tbl, d int) {
	r := o.r
	base := int64(len(r.refs))
	m := min(len(a.refs), len(b.refs))
	refs := a.refs[:m]
	diff := 0
	for k := range m {
		if a.refs[k] != b.refs[k] {
			refs[k] = makeRef(a.refs[k].seg(), disputedLocation, a.refs[k].compressed())
			diff++
		}
	}
	o.addRefs(refs)
	msg := fmt.Sprintf("segment %d: table at offset %d and its table2 differ (first at entry %d) and are equally trustworthy; %d differing chunk(s) cannot be read", n, a.off, d, diff)
	if len(a.refs) != len(b.refs) {
		from := base + int64(m)
		if r.gap == nil {
			r.gap = &tableGap{from: from, why: fmt.Sprintf("segment %d: table and table2 disagree on the entry count (%d and %d); chunk numbering from index %d onward is unknown", n, len(a.refs), len(b.refs), from)}
		}
		msg += fmt.Sprintf("; they disagree on the entry count (%d and %d), so chunks from index %d onward are unreadable", len(a.refs), len(b.refs), from)
	}
	r.warn.add("%s", msg)
}

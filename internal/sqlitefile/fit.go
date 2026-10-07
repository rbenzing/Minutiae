package sqlitefile

import (
	"slices"
	"sort"
)

// FitTier is how strictly values must fit a table.
type FitTier uint8

// The tiers.
const (
	FitStrict FitTier = iota
	FitLoose
)

// A fit to a schema is evidence, not proof: two candidates at a tier is
// ambiguous (none), never a pick.

// classOK reports whether a column of the given affinity may hold a value of
// kind k: TEXT holds NULL, text and blob; BLOB holds anything; INTEGER, NUMERIC
// and REAL hold any class (a value that would not convert stays text or blob).
func classOK(a Affinity, k Kind) bool {
	if a == AffText {
		return k == KindNull || k == KindText || k == KindBlob
	}
	return true
}

// fitCol is one position of an index entry.
type fitCol struct {
	aff Affinity
	any bool // an expression: any class
}

// fitItem is one table or index the fit can name.
type fitItem struct {
	name      string
	width     int       // the most values a record of it holds
	minValues int       // strict: the fewest values (the columns past it are addable)
	def       *TableDef // set for a table
	cols      []fitCol  // set for an index entry (key columns then the row key)
	rowidLast bool      // index of a rowid table: the last value is the integer rowid
}

// cost is the step charge of examining the item against one record.
func (it *fitItem) cost() int64 {
	if it.def != nil {
		return int64(len(it.def.Columns)) + 1
	}
	return int64(len(it.cols)) + 1
}

// matches reports whether values could be a record (an entry) of the item.
func (it *fitItem) matches(values []Value, tier FitTier) bool {
	n := len(values)
	if n < 1 || n > it.width {
		return false
	}
	if it.def == nil {
		if tier == FitStrict && n != it.width {
			return false
		}
		for i, v := range values {
			c := it.cols[i]
			switch {
			case c.any:
			case tier == FitStrict && it.rowidLast && i == it.width-1:
				if v.Kind != KindInt {
					return false
				}
			case !classOK(c.aff, v.Kind):
				return false
			}
		}
		return true
	}
	if tier == FitStrict && n < it.minValues {
		return false
	}
	for i := range it.def.Columns {
		c := &it.def.Columns[i]
		if c.RecordIndex < 0 || c.RecordIndex >= n {
			continue
		}
		v := values[c.RecordIndex]
		if !classOK(c.Affinity, v.Kind) {
			return false
		}
		if tier == FitStrict {
			if c.NotNull && v.Kind == KindNull {
				return false
			}
			if i == it.def.RowidAlias && v.Kind != KindNull {
				return false
			}
		}
	}
	return true
}

// fitSet indexes items by the number of values they hold, so a call examines
// only the items that can hold that many values.
type fitSet struct {
	items   []fitItem
	byWidth map[int][]int32 // width -> item indexes, by minValues then position
	widths  []int           // the distinct widths, ascending
}

func (fs *fitSet) add(it fitItem) {
	if it.width < 1 {
		return
	}
	fs.items = append(fs.items, it)
}

func (fs *fitSet) finish() {
	fs.byWidth = map[int][]int32{}
	for i := range fs.items {
		w := fs.items[i].width
		fs.byWidth[w] = append(fs.byWidth[w], int32(i))
	}
	for w, l := range fs.byWidth {
		fs.widths = append(fs.widths, w)
		sort.SliceStable(l, func(a, b int) bool { return fs.items[l[a]].minValues < fs.items[l[b]].minValues })
	}
	sort.Ints(fs.widths)
}

// fitRun carries the step budget of one call.
type fitRun struct {
	steps, max int64
	examined   int
	limited    bool
}

// examine charges the item and tests it; false once the budget is spent.
func (r *fitRun) examine(it *fitItem, values []Value, tier FitTier) (match, ok bool) {
	c := it.cost()
	if r.steps+c > r.max {
		r.limited = true
		return false, false
	}
	r.steps += c
	r.examined++
	return it.matches(values, tier), true
}

// find returns the indexes (ascending) of the items that fit values at the tier.
func (fs *fitSet) find(values []Value, tier FitTier, run *fitRun) []int32 {
	n := len(values)
	if n < 1 {
		return nil
	}
	var out []int32
	start := sort.SearchInts(fs.widths, n)
	for _, w := range fs.widths[start:] {
		for _, ix := range fs.byWidth[w] {
			it := &fs.items[ix]
			if tier == FitStrict && it.minValues > n {
				break // the rest of the bucket needs more values
			}
			m, ok := run.examine(it, values, tier)
			if !ok {
				return nil
			}
			if m {
				out = append(out, ix)
			}
		}
	}
	slices.Sort(out)
	return out
}

// fitIndex is the lookup structure built once per Schema.
type fitIndex struct {
	tables, indexes fitSet
}

// addable reports whether a column missing from a record is explained by ALTER
// TABLE ADD COLUMN: it is nullable or carries a literal default, and is neither
// a generated column nor the rowid alias.
func addable(def *TableDef, i int) bool {
	c := &def.Columns[i]
	return c.Generated == GenNone && i != def.RowidAlias && (!c.NotNull || c.Default.Kind == DefaultLiteral)
}

// tableMinValues is the fewest values a strict record of def holds: one past
// the last stored column that cannot be added. ok is false for a definition
// whose record positions are not a valid layout.
func tableMinValues(def *TableDef) (minValues int, ok bool) {
	for i := range def.Columns {
		c := &def.Columns[i]
		if c.RecordIndex < -1 || c.RecordIndex >= def.StoredColumns {
			return 0, false
		}
		if c.RecordIndex >= 0 && !addable(def, i) {
			minValues = max(minValues, c.RecordIndex+1)
		}
	}
	return minValues, true
}

// fitTable returns the fit item of a table object, or false when it fits nothing.
func fitTableDef(name string, def *TableDef, memo map[*TableDef]int) (fitItem, bool) {
	if def == nil || !def.ParseOK || def.StoredColumns < 1 {
		return fitItem{}, false
	}
	mv, seen := memo[def]
	if !seen {
		v, ok := tableMinValues(def)
		if !ok {
			v = -1
		}
		mv = v
		memo[def] = mv
	}
	if mv < 0 {
		return fitItem{}, false
	}
	return fitItem{name: name, width: def.StoredColumns, minValues: mv, def: def}, true
}

// fitIndexItem returns the fit item of an index object, or false.
func fitIndexItem(name string, ix *IndexDef, tdef *TableDef, lk colLookup) (fitItem, bool) {
	if ix == nil || !ix.ParseOK || ix.Auto || tdef == nil || !tdef.ParseOK || len(ix.Columns) == 0 {
		return fitItem{}, false
	}
	find := func(n string) *Column { return lk.find(tdef, n) }
	var cols []fitCol
	inIndex := map[string]bool{}
	for _, ic := range ix.Columns {
		if ic.Expr || ic.Name == "" {
			cols = append(cols, fitCol{any: true})
			continue
		}
		inIndex[asciiLower(ic.Name)] = true
		if c := find(ic.Name); c != nil {
			cols = append(cols, fitCol{aff: c.Affinity})
			continue
		}
		switch asciiLower(ic.Name) {
		case "rowid", "_rowid_", "oid":
			cols = append(cols, fitCol{aff: AffInteger})
		default:
			return fitItem{}, false // the index names a column the table lacks
		}
	}
	it := fitItem{name: name}
	if !tdef.WithoutRowid {
		cols = append(cols, fitCol{aff: AffInteger})
		it.rowidLast = true
	} else {
		var pks []*Column
		for i := range tdef.Columns {
			if tdef.Columns[i].PKOrdinal > 0 {
				pks = append(pks, &tdef.Columns[i])
			}
		}
		sort.SliceStable(pks, func(a, b int) bool { return pks[a].PKOrdinal < pks[b].PKOrdinal })
		for _, c := range pks {
			if !inIndex[asciiLower(c.Name)] {
				cols = append(cols, fitCol{aff: c.Affinity})
			}
		}
	}
	it.cols, it.width, it.minValues = cols, len(cols), len(cols)
	return it, true
}

// buildFit reads the schema once into the lookup sets.
func (s *Schema) buildFit() {
	fi := &fitIndex{}
	byName := map[string]*TableDef{}
	memo := map[*TableDef]int{}
	cols := colLookup{s: s, byTable: map[*TableDef]map[string]*Column{}}
	budget := s.fitBuildCap()
	for i := range s.Objects {
		o := &s.Objects[i]
		if o.Type != "table" || o.Virtual || o.Table == nil {
			continue
		}
		if _, dup := byName[asciiLower(o.Name)]; !dup {
			byName[asciiLower(o.Name)] = o.Table
		}
		if it, ok := fitTableDef(o.Name, o.Table, memo); ok {
			fi.tables.add(it)
		}
	}
	for i := range s.Objects {
		o := &s.Objects[i]
		if o.Type != "index" || o.Index == nil {
			continue
		}
		tn := o.Index.Table
		if tn == "" {
			tn = o.TblName
		}
		if s.fitSteps > budget {
			s.fitLimited = true // the rest of the indexes are not read
			break
		}
		s.fitSteps += int64(len(o.Index.Columns))
		if it, ok := fitIndexItem(o.Name, o.Index, byName[asciiLower(tn)], cols); ok {
			fi.indexes.add(it)
		}
	}
	fi.tables.finish()
	fi.indexes.finish()
	s.fit = fi
}

func (s *Schema) fitSets() *fitIndex {
	s.fitOnce.Do(s.buildFit)
	return s.fit
}

func (s *Schema) fitCap() int64 {
	if s.maxFit > 0 {
		return s.maxFit
	}
	return DefaultLimits().MaxFitSteps
}

// fitBuildCap is the step cap of resolving the index columns when the fit index
// is built: 64 fit calls (the build happens once per schema).
func (s *Schema) fitBuildCap() int64 { return 64 * s.fitCap() }

func (s *Schema) newFitRun() *fitRun {
	s.fitSets()
	return &fitRun{max: s.fitCap(), limited: s.fitLimited}
}

// colLookup resolves column names of a table through a map built once per table
// (the build charges its size to Schema.fitSteps).
type colLookup struct {
	s       *Schema
	byTable map[*TableDef]map[string]*Column
}

func (c colLookup) find(t *TableDef, name string) *Column {
	m, ok := c.byTable[t]
	if !ok {
		m = make(map[string]*Column, len(t.Columns))
		c.s.fitSteps += int64(len(t.Columns))
		for i := range t.Columns {
			k := asciiLower(t.Columns[i].Name)
			if _, dup := m[k]; !dup {
				m[k] = &t.Columns[i]
			}
		}
		c.byTable[t] = m
	}
	return m[asciiLower(name)]
}

func (fs *fitSet) names(ix []int32) []string {
	if len(ix) == 0 {
		return nil
	}
	out := make([]string, len(ix))
	for i, x := range ix {
		out[i] = fs.items[x].name
	}
	return out
}

// FitTables returns the names of the tables of s whose stored records the given
// values could be, at the tier, in schema order. Strict: 1 <= len(values) <=
// StoredColumns; every stored column the record lacks must be addable the way
// ALTER TABLE ADD COLUMN requires (nullable, or carrying a literal default);
// every present value's class is allowed by the column's affinity (record
// positions, so WITHOUT ROWID order is honoured); NOT NULL is respected and a
// rowid alias column is NULL. Loose: the length and the class check only.
// Candidates come from an index by column count built once per Schema, so a
// call never scans every schema object. A call that exhausts the step cap
// (Limits.MaxFitSteps) returns nothing.
func (s *Schema) FitTables(values []Value, tier FitTier) []string {
	names, _ := s.fitTablesCount(values, tier)
	return names
}

func (s *Schema) fitTablesCount(values []Value, tier FitTier) ([]string, int) {
	run := s.newFitRun()
	fs := &s.fitSets().tables
	ix := fs.find(values, tier, run)
	if run.limited {
		return nil, run.examined
	}
	return fs.names(ix), run.examined
}

// FitIndexes is FitTables for index entries: an entry is the indexed columns
// followed by the rowid (rowid tables) or by the primary-key columns not already
// indexed (WITHOUT ROWID tables). Strict requires the exact length and, for rowid
// tables, an integer last value; an expression column accepts any class; loose
// is the length bound and the class check only.
func (s *Schema) FitIndexes(values []Value, tier FitTier) []string {
	run := s.newFitRun()
	fs := &s.fitSets().indexes
	ix := fs.find(values, tier, run)
	if run.limited {
		return nil
	}
	return fs.names(ix)
}

// FitPage intersects FitTables over every cell of a page image (cells that are
// not records, empty ones, are ignored): the table when exactly one remains at
// the strict tier (BasisFit), else when exactly one remains at the loose tier
// (BasisGuess), else none. It never returns BasisSchema: only reachability from
// the schema may prove an owner. Work is capped by Limits.MaxFitSteps per page.
func (s *Schema) FitPage(cells [][]Value) (table string, basis TableBasis) {
	table, basis, _ = s.FitPageDetail(cells)
	return table, basis
}

// FitPageDetail is FitPage that also reports that the work cap was reached, in
// which case the answer is none (limit-reached).
func (s *Schema) FitPageDetail(cells [][]Value) (table string, basis TableBasis, limitReached bool) {
	return s.fitPage(&s.fitSets().tables, cells)
}

// FitIndexPage is FitPage for index entries.
func (s *Schema) FitIndexPage(cells [][]Value) (index string, basis TableBasis) {
	index, basis, _ = s.FitIndexPageDetail(cells)
	return index, basis
}

// FitIndexPageDetail is FitIndexPage that also reports the work cap.
func (s *Schema) FitIndexPageDetail(cells [][]Value) (index string, basis TableBasis, limitReached bool) {
	return s.fitPage(&s.fitSets().indexes, cells)
}

func (s *Schema) fitPage(fs *fitSet, cells [][]Value) (string, TableBasis, bool) {
	run := s.newFitRun()
	var strict, loose []int32
	first := true
	for _, cell := range cells {
		if len(cell) == 0 {
			continue
		}
		if first {
			first = false
			strict = fs.find(cell, FitStrict, run)
			if !run.limited {
				loose = fs.find(cell, FitLoose, run)
			}
		} else {
			strict = fs.filter(strict, cell, FitStrict, run)
			loose = fs.filter(loose, cell, FitLoose, run)
		}
		if run.limited {
			return "", BasisNone, true
		}
		if len(loose) == 0 {
			return "", BasisNone, false // the strict set is inside the loose one
		}
	}
	switch {
	case len(strict) == 1:
		return fs.items[strict[0]].name, BasisFit, false
	case len(loose) == 1:
		return fs.items[loose[0]].name, BasisGuess, false
	}
	return "", BasisNone, false
}

// filter keeps the items of cand that also fit values.
func (fs *fitSet) filter(cand []int32, values []Value, tier FitTier, run *fitRun) []int32 {
	out := cand[:0:0]
	for _, ix := range cand {
		m, ok := run.examine(&fs.items[ix], values, tier)
		if !ok {
			return nil
		}
		if m {
			out = append(out, ix)
		}
	}
	return out
}

package sqlitefile

// Table identity of the rows of a page image (ruling 3). The identity is
// evidence, never an assumption: BasisSchema needs the page to be a live b-tree
// page of the schema object AND every cell of the image to fit that object
// strictly; otherwise the identity comes from the fit alone, and an ambiguous
// fit names nothing.

// idKind says what the rows of an image are, for the comparison with the live
// state.
type idKind uint8

const (
	kindNone     idKind = iota // an index entry, or an image that no object is proven for: no comparison
	kindRowid                  // a row of a rowid table: looked up by rowid
	kindWithoutR               // a row of a WITHOUT ROWID table: compared through a digest set
)

// ident is the identity of the rows of one image.
type ident struct {
	table, index string
	basis        TableBasis
	notes        []string
	kind         idKind
}

// candidate is an object the fit may name.
type candidate struct {
	name   string
	index  bool
	item   *fitItem
	isWR   bool
	tbName string // the table of an index
}

// ownerOf returns the schema object the live state gives page pg to, when pg is
// a live b-tree page.
func (rp *rowPass) ownerOf(pg uint32) (uint32, bool) {
	if pg == 0 || pg > rp.lay.Addressable {
		return 0, false
	}
	if c := rp.lay.Class[pg]; c != ClassBTreeInterior && c != ClassBTreeLeaf {
		return 0, false
	}
	o := rp.lay.Owner[pg]
	return o, o != 0
}

// schemaItem is the fit item of the schema table itself.
var schemaItem = func() *fitItem {
	it, ok := fitTableDef("sqlite_schema", sqliteSchemaDef, map[*TableDef]int{})
	if !ok {
		return nil
	}
	return &it
}()

// ownerCand returns the candidate for the schema object that owns a page, or
// false when it is not something a strict fit can be tested against.
func (rp *rowPass) ownerCand(o uint32) (candidate, bool) {
	if o == 1 {
		return candidate{name: "sqlite_schema", item: schemaItem}, schemaItem != nil
	}
	k := int(o) - 2
	if k < 0 || k >= len(rp.sch.Objects) {
		return candidate{}, false
	}
	obj := &rp.sch.Objects[k]
	switch {
	case obj.Type == "table" && !obj.Virtual && obj.Table != nil && obj.Table.ParseOK:
		it := rp.tblItems[obj.Name]
		return candidate{name: obj.Name, item: it, isWR: obj.Table.WithoutRowid}, it != nil
	case obj.Type == "index" && obj.Index != nil:
		it := rp.idxItems[obj.Name]
		return candidate{name: obj.Name, index: true, item: it, tbName: rp.indexTable(obj)}, it != nil
	}
	return candidate{}, false
}

// indexTable is the table an index belongs to.
func (rp *rowPass) indexTable(o *SchemaObject) string {
	if o.Index != nil && o.Index.Table != "" {
		return o.Index.Table
	}
	return o.TblName
}

// leafKind is the page type a candidate's b-tree has at its leaves.
func (c candidate) leafType() PageType {
	if c.index || c.isWR {
		return PageIndexLeaf
	}
	return PageTableLeaf
}

func (c candidate) idKind() idKind {
	switch {
	case c.index:
		return kindNone
	case c.isWR:
		return kindWithoutR
	}
	return kindRowid
}

// identify says which table (or index) the cells of the image belong to.
func (rp *rowPass) identify(img PageImage, hd PageHeader, cells []imgCell) ident {
	var notes []string
	if o, ok := rp.ownerOf(img.Number); ok {
		if c, ok := rp.ownerCand(o); ok && c.leafType() == hd.Type && rp.allFit(c.item, cells) && rp.sameOwnerAtWrite(img, o) {
			id := ident{basis: BasisSchema, kind: c.idKind()}
			if c.index {
				id.index, id.table = c.name, c.tbName
			} else {
				id.table = c.name
			}
			return id
		}
		notes = append(notes, NoteOwnerChanged)
	}
	vals := make([][]Value, len(cells))
	for i := range cells {
		vals[i] = cells[i].kind
	}
	c, basis, limited := rp.fitPage(hd.Type, vals)
	if limited {
		rp.limitHit("MaxFitSteps", "the table fit of a page image reached its step cap and named no table")
		notes = append(notes, "fit-limit-reached")
	}
	id := ident{basis: basis, notes: notes}
	if basis == BasisNone {
		return id
	}
	if c.index {
		id.index, id.table = c.name, c.tbName
	} else {
		id.table = c.name
	}
	id.kind = c.idKind()
	return id
}

// allFit reports whether every cell fits the item strictly.
func (rp *rowPass) allFit(it *fitItem, cells []imgCell) bool {
	if it == nil {
		return false
	}
	for i := range cells {
		if !it.matches(cells[i].kind, FitStrict) {
			return false
		}
	}
	return true
}

// fitPage names the one table (or index) every cell of a page of leaf type lt
// fits: strict unique is BasisFit, else loose unique BasisGuess, else none. A
// table leaf can only belong to a rowid table; an index leaf to an index or a
// WITHOUT ROWID table. It never names BasisSchema.
func (rp *rowPass) fitPage(lt PageType, cells [][]Value) (candidate, TableBasis, bool) {
	run := rp.sch.newFitRun()
	fi := rp.sch.fitSets()
	var strict, loose []candidate
	tkeep := func(it *fitItem) bool { return it.def != nil && it.def.WithoutRowid == (lt == PageIndexLeaf) }
	ts, tl := fi.tables.fitCells(cells, tkeep, run)
	if run.limited {
		return candidate{}, BasisNone, true
	}
	for _, x := range ts {
		it := &fi.tables.items[x]
		strict = append(strict, candidate{name: it.name, item: it, isWR: it.def.WithoutRowid})
	}
	for _, x := range tl {
		it := &fi.tables.items[x]
		loose = append(loose, candidate{name: it.name, item: it, isWR: it.def.WithoutRowid})
	}
	if lt == PageIndexLeaf {
		is, il := fi.indexes.fitCells(cells, func(*fitItem) bool { return true }, run)
		if run.limited {
			return candidate{}, BasisNone, true
		}
		for _, x := range is {
			strict = append(strict, rp.indexCand(&fi.indexes.items[x]))
		}
		for _, x := range il {
			loose = append(loose, rp.indexCand(&fi.indexes.items[x]))
		}
	}
	switch {
	case len(strict) == 1:
		return strict[0], BasisFit, false
	case len(loose) == 1:
		return loose[0], BasisGuess, false
	}
	return candidate{}, BasisNone, false
}

func (rp *rowPass) indexCand(it *fitItem) candidate {
	return candidate{name: it.name, index: true, item: it, tbName: rp.idxTable[it.name]}
}

// fitCells intersects the items of fs that keep accepts and that fit every
// non-empty cell, at the strict and at the loose tier.
func (fs *fitSet) fitCells(cells [][]Value, keep func(*fitItem) bool, run *fitRun) (strict, loose []int32) {
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
			strict = fs.keepItems(strict, keep)
			loose = fs.keepItems(loose, keep)
		} else {
			strict = fs.filter(strict, cell, FitStrict, run)
			loose = fs.filter(loose, cell, FitLoose, run)
		}
		if run.limited || len(loose) == 0 {
			return nil, nil
		}
	}
	return strict, loose
}

func (fs *fitSet) keepItems(ix []int32, keep func(*fitItem) bool) []int32 {
	out := ix[:0:0]
	for _, x := range ix {
		if keep(&fs.items[x]) {
			out = append(out, x)
		}
	}
	return out
}

package sqlitetest

import (
	"bytes"
	"fmt"
	"slices"
)

// Tables, indexes and their b-trees. A Builder keeps the rows (and the cells
// that updates and deletes left behind) as data; Build lays them out. The
// layout is deterministic:
//   - page 1 is the schema table; the root page of object i (in creation
//     order, dropped ones included) is page i+2; every other page is
//     allocated in layout order (overflow chains as each cell is encoded,
//     leaves and interior pages as they are filled).
//   - leaves are filled greedily in key order; interior pages keep as many
//     children as fit. A single page that holds everything is the root.
//   - residue: a deleted row (or the old version of an updated row whose
//     record changed length) leaves its cell behind in the leaf where it
//     belonged, if there is room for it that still leaves room for the live
//     cell that follows; otherwise it is dropped. Cells sit in key order from
//     the end of the page downward. A residue cell at the lowest address is
//     swallowed by the gap (the content start moves above it, its bytes
//     stay); every other run of residue cells becomes one freeblock whose
//     first four bytes are the chain header, the rest of the bytes untouched.
//     Fragmented bytes stay 0.
//   - overflow pages of residue cells go on the freelist with their contents.
type Table struct {
	b       *Builder
	name    string
	sql     string
	index   bool
	parent  *Table
	cols    []int
	noRowid bool
	pk      int
	nullSQL bool // the schema row stores NULL, not text (an automatic index)
	root    uint32
	dropped bool

	rows []*trow // live rows in key order
	dead []*trow // cells left behind by deletes and updates

	// layout results (valid after Build)
	depth     int
	leaves    []uint32
	interiors []uint32
	pages     []uint32 // every page of the tree, overflow included
	where     map[int64]*cellInfo
}

type trow struct {
	handle  int64 // the rowid (the identity of a row of a WITHOUT ROWID table)
	vals    []any
	payload []byte
}

type cellInfo struct {
	page     uint32
	off      int
	cell     []byte
	overflow []uint32
}

// entry is a cell ready to place.
type entry struct {
	cell     []byte // as it sits in a leaf page: header, local payload, overflow pointer
	rowid    int64  // table trees: the key
	dead     bool
	handle   int64
	overflow []uint32
	placed   *cellInfo
}

func (e *entry) size() int { return max(len(e.cell), 4) } // cells are padded to 4 bytes

// CreateTable adds a rowid table; sql is stored verbatim in the schema.
func (b *Builder) CreateTable(name, sql string) *Table {
	return b.addObject(&Table{name: name, sql: sql})
}

// CreateTableWithoutRowid adds a WITHOUT ROWID table whose primary key is the
// first pkCols declared columns (the sql must say so). Rows are identified by
// the handle passed as rowid to Insert, Update and Delete; it is not stored.
func (b *Builder) CreateTableWithoutRowid(name, sql string, pkCols int) *Table {
	if pkCols < 1 {
		panic("sqlitetest: a WITHOUT ROWID table needs at least one primary key column")
	}
	return b.addObject(&Table{name: name, sql: sql, noRowid: true, pk: pkCols})
}

// CreateIndex adds an index over the given declared column numbers of table.
// Its entries are derived from the table's live rows at Build time.
func (b *Builder) CreateIndex(name, table, sql string, cols ...int) {
	t := b.Object(table)
	if t == nil || t.index {
		panic(fmt.Sprintf("sqlitetest: no table %q to index", table))
	}
	b.addObject(&Table{name: name, sql: sql, index: true, parent: t, cols: cols})
}

// Object returns the table or index created under name, or nil.
func (b *Builder) Object(name string) *Table {
	for _, t := range b.objs {
		if t.name == name && !t.dropped {
			return t
		}
	}
	return nil
}

func (b *Builder) addObject(t *Table) *Table {
	if b.o.AutoVacuum != 0 {
		panic("sqlitetest: tables in an auto-vacuum database need pointer-map pages, which the builder does not write yet")
	}
	if b.Object(t.name) != nil {
		panic(fmt.Sprintf("sqlitetest: object %q already exists", t.name))
	}
	t.b = b
	b.objs = append(b.objs, t)
	b.dirty = true
	return t
}

// DropTable removes a table and its indexes from the schema and frees every
// page of their trees onto the freelist with the contents intact.
func (b *Builder) DropTable(name string) {
	t := b.Object(name)
	if t == nil || t.index {
		panic(fmt.Sprintf("sqlitetest: no table %q to drop", name))
	}
	t.dropped = true
	for _, o := range b.objs {
		if o.parent == t {
			o.dropped = true
		}
	}
	b.dirty = true
}

func (t *Table) newRow(handle int64, vals []any) *trow {
	if t.index {
		panic("sqlitetest: rows of an index are derived from its table")
	}
	n := make([]any, len(vals))
	for i, v := range vals {
		n[i] = normalize(v)
	}
	if t.noRowid && len(n) < t.pk {
		panic("sqlitetest: fewer values than primary key columns")
	}
	return &trow{handle: handle, vals: n, payload: encodeRecord(n, t.b.o.Encoding)}
}

// compare orders two rows of the table: by rowid, or by primary key.
func (t *Table) compare(a, b *trow) int {
	if !t.noRowid {
		switch {
		case a.handle < b.handle:
			return -1
		case a.handle > b.handle:
			return 1
		}
		return 0
	}
	return t.compareKeys(a.vals[:t.pk], b.vals[:t.pk])
}

func (t *Table) compareKeys(a, b []any) int {
	for i := range min(len(a), len(b)) {
		if c := compareValues(a[i], b[i], t.b.o.Encoding); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

// compareValues orders values as the engine does with the default collation:
// NULL, then numbers, then text (bytes of the stored encoding), then blobs.
func compareValues(a, b any, enc int) int {
	class := func(v any) int {
		switch v.(type) {
		case nil:
			return 0
		case int64, float64:
			return 1
		case string:
			return 2
		}
		return 3
	}
	if ca, cb := class(a), class(b); ca != cb {
		return ca - cb
	}
	switch x := a.(type) {
	case int64:
		if y, ok := b.(int64); ok {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}
	case string:
		return bytes.Compare(encodeText(x, enc), encodeText(b.(string), enc))
	case []byte:
		return bytes.Compare(x, b.([]byte))
	case nil:
		return 0
	}
	fa, fb := toFloat(a), toFloat(b)
	switch {
	case fa < fb:
		return -1
	case fa > fb:
		return 1
	}
	return 0
}

func toFloat(v any) float64 {
	if i, ok := v.(int64); ok {
		return float64(i)
	}
	return v.(float64)
}

func (t *Table) find(handle int64) int {
	for i, r := range t.rows {
		if r.handle == handle {
			return i
		}
	}
	return -1
}

func (t *Table) insertSorted(r *trow) {
	i, found := slices.BinarySearchFunc(t.rows, r, t.compare)
	if found {
		panic(fmt.Sprintf("sqlitetest: table %q already has a row with the key of handle %d", t.name, r.handle))
	}
	t.rows = slices.Insert(t.rows, i, r)
	t.b.dirty = true
}

// Insert adds a row. vals are the declared columns: nil, int64, float64,
// string or []byte. For a WITHOUT ROWID table rowid is only a handle.
func (t *Table) Insert(rowid int64, vals ...any) {
	if t.find(rowid) >= 0 {
		panic(fmt.Sprintf("sqlitetest: table %q already has handle %d", t.name, rowid))
	}
	t.insertSorted(t.newRow(rowid, vals))
}

// InsertRaw adds a rowid-table row whose record is the given serial types
// followed by body, verbatim: hostile and reserved-type records.
func (t *Table) InsertRaw(rowid int64, serials []uint64, body []byte) {
	if t.noRowid || t.index {
		panic("sqlitetest: InsertRaw needs a rowid table")
	}
	var s []byte
	for _, v := range serials {
		s = append(s, putVarint(v)...)
	}
	t.insertSorted(&trow{handle: rowid, payload: joinRecord(s, body)})
}

// Update replaces the values of a row. A record of the same length is
// rewritten in place; otherwise the old cell stays behind as residue and the
// new one is placed like any other.
func (t *Table) Update(rowid int64, vals ...any) {
	i := t.find(rowid)
	if i < 0 {
		panic(fmt.Sprintf("sqlitetest: table %q has no handle %d to update", t.name, rowid))
	}
	old := t.rows[i]
	nu := t.newRow(rowid, vals)
	t.rows = slices.Delete(t.rows, i, i+1)
	if len(old.payload) != len(nu.payload) {
		t.dead = append(t.dead, old)
	}
	t.insertSorted(nu)
}

// Delete removes a row; its cell stays behind as residue.
func (t *Table) Delete(rowid int64) {
	i := t.find(rowid)
	if i < 0 {
		panic(fmt.Sprintf("sqlitetest: table %q has no handle %d to delete", t.name, rowid))
	}
	t.dead = append(t.dead, t.rows[i])
	t.rows = slices.Delete(t.rows, i, i+1)
	t.b.dirty = true
}

// Root is the root page of the tree (after Build).
func (t *Table) Root() uint32 { t.b.settle(); return t.root }

// Depth is the number of levels of the tree: 1 for a single leaf.
func (t *Table) Depth() int { t.b.settle(); return t.depth }

// Leaves are the leaf pages in key order.
func (t *Table) Leaves() []uint32 { t.b.settle(); return slices.Clone(t.leaves) }

// Interiors are the interior pages (the root included when it is one).
func (t *Table) Interiors() []uint32 { t.b.settle(); return slices.Clone(t.interiors) }

// Pages lists every page of the tree: the root, the other interior pages, the
// leaves and every overflow page (residue cells included), in layout order.
func (t *Table) Pages() []uint32 { t.b.settle(); return slices.Clone(t.pages) }

// Overflow lists the overflow pages of a live row's cell in chain order.
func (t *Table) Overflow(handle int64) []uint32 {
	t.b.settle()
	if c := t.where[handle]; c != nil {
		return slices.Clone(c.overflow)
	}
	return nil
}

// CellBytes returns the bytes of a live row's cell as laid out (header, local
// payload, overflow pointer; without the padding that brings a cell to four
// bytes) and the page and offset in the page where it lies.
func (t *Table) CellBytes(handle int64) (cell []byte, page uint32, off int) {
	t.b.settle()
	c := t.where[handle]
	if c == nil {
		return nil, 0, 0
	}
	return slices.Clone(c.cell), c.page, c.off
}

// entries returns the cells of the tree in key order, the residue interleaved
// (before a live cell of the same key). Overflow pages are written here.
func (t *Table) entries() []*entry {
	b := t.b
	if t.index {
		return t.indexEntries()
	}
	type item struct {
		r    *trow
		dead bool
	}
	var items []item
	for _, r := range t.rows {
		items = append(items, item{r, false})
	}
	for _, r := range t.dead {
		i := 0
		for i < len(items) && t.compare(items[i].r, r) < 0 {
			i++
		}
		items = slices.Insert(items, i, item{r, true})
	}
	var out []*entry
	for _, it := range items {
		e := b.encodeCell(!t.noRowid, it.r.handle, it.r.payload)
		e.dead, e.handle = it.dead, it.r.handle
		if it.dead {
			b.freed = append(b.freed, e.overflow...)
		}
		out = append(out, e)
	}
	return out
}

func (t *Table) indexEntries() []*entry {
	p := t.parent
	var list [][]any
	for _, r := range p.rows {
		if r.vals == nil {
			continue
		}
		var v []any
		for _, c := range t.cols {
			v = append(v, r.vals[c])
		}
		if p.noRowid {
			v = append(v, r.vals[:p.pk]...)
		} else {
			v = append(v, r.handle)
		}
		list = append(list, v)
	}
	slices.SortStableFunc(list, func(a, b []any) int { return p.compareKeys(a, b) })
	var out []*entry
	for _, v := range list {
		out = append(out, t.b.encodeCell(false, 0, encodeRecord(v, t.b.o.Encoding)))
	}
	return out
}

// encodeCell builds the leaf-format cell of a payload (the same bytes an
// index interior cell carries after its child pointer), writing its overflow
// chain.
func (b *Builder) encodeCell(tableLeaf bool, rowid int64, payload []byte) *entry {
	local, spills := localSize(b.usable(), tableLeaf, len(payload))
	cell := putVarint(uint64(len(payload)))
	if tableLeaf {
		cell = append(cell, putVarint(uint64(rowid))...)
	}
	cell = append(cell, payload[:local]...)
	e := &entry{rowid: rowid}
	if spills {
		e.overflow = b.spill(payload, local)
		var ptr [4]byte
		put32(ptr[:], e.overflow[0])
		cell = append(cell, ptr[:]...)
	}
	e.cell = cell
	return e
}

// node is a child of an interior page: its page number and the bytes that
// follow a child pointer in a cell naming it (a table's key varint, an index
// separator cell). The last node of a level has none.
type node struct {
	page uint32
	sep  []byte
}

// layout writes the tree and records where everything went.
func (t *Table) layout() {
	b := t.b
	t.where = map[int64]*cellInfo{}
	t.pages = []uint32{t.root}
	t.leaves, t.interiors = nil, nil
	ents := t.entries()
	for _, e := range ents {
		t.pages = append(t.pages, e.overflow...)
	}
	base := 0
	if t.root == 1 {
		base = 100
	}
	leafFlag, interiorFlag := byte(0x0d), byte(0x05)
	if t.index || t.noRowid {
		leafFlag, interiorFlag = 0x0a, 0x02
	}
	tableTree := leafFlag == 0x0d

	// One page holds everything: the root is a leaf.
	if kept, ok := b.packLeaf(ents, base); ok {
		t.depth = 1
		t.leaves = []uint32{t.root}
		t.writeLeaf(t.root, base, leafFlag, kept)
		t.setWhere(kept)
		return
	}

	// Leaves.
	var nodes []node
	if tableTree {
		groups := b.splitTableLeaves(ents)
		if len(groups) == 1 { // only page 1 (a smaller page) gets here: the root needs two children
			g := groups[0]
			if len(g) < 2 {
				panic("sqlitetest: cannot split the schema leaf")
			}
			groups = [][]*entry{g[:len(g)/2], g[len(g)/2:]}
		}
		for _, g := range groups {
			pg := b.alloc()
			t.leaves = append(t.leaves, pg)
			t.pages = append(t.pages, pg)
			t.writeLeaf(pg, 0, leafFlag, g)
			t.setWhere(g)
			nodes = append(nodes, node{page: pg, sep: putVarint(uint64(maxLiveRowid(g)))})
		}
	} else {
		groups, seps := b.splitIndexLeaves(ents)
		for i, g := range groups {
			pg := b.alloc()
			t.leaves = append(t.leaves, pg)
			t.pages = append(t.pages, pg)
			t.writeLeaf(pg, 0, leafFlag, g)
			n := node{page: pg}
			if i < len(seps) {
				n.sep = seps[i].cell
			}
			nodes = append(nodes, n)
		}
	}
	t.depth = 1

	// Interior levels, up to one page: the root.
	for {
		t.depth++
		if b.interiorFits(nodes, base) {
			t.interiors = append(t.interiors, t.root)
			t.writeInterior(t.root, base, interiorFlag, nodes)
			return
		}
		groups := b.splitInterior(nodes)
		if len(groups) == 1 { // fits a page of its own but not the root of page 1
			g := groups[0]
			if len(g) < 4 {
				panic("sqlitetest: cannot split the interior page")
			}
			groups = [][]node{g[:len(g)/2], g[len(g)/2:]}
		}
		var up []node
		for _, g := range groups {
			pg := b.alloc()
			t.interiors = append(t.interiors, pg)
			t.pages = append(t.pages, pg)
			t.writeInterior(pg, 0, interiorFlag, g)
			up = append(up, node{page: pg, sep: g[len(g)-1].sep})
		}
		nodes = up
	}
}

func maxLiveRowid(g []*entry) int64 {
	m := int64(0)
	first := true
	for _, e := range g {
		if !e.dead && (first || e.rowid > m) {
			m, first = e.rowid, false
		}
	}
	return m
}

// cellSpace is the room one cell takes in a page: its bytes (at least four)
// and, for a live cell, its two pointer bytes.
func cellSpace(e *entry) int {
	if e.dead {
		return e.size()
	}
	return e.size() + 2
}

// packLeaf reports whether every live cell fits one leaf page with the given
// header base, and returns the entries kept: the live cells and the residue
// that fits without crowding out a live cell.
func (b *Builder) packLeaf(ents []*entry, base int) ([]*entry, bool) {
	avail := b.usable() - base - 8
	liveAfter := make([]int, len(ents)+1) // room the live cells from i on need
	for i := len(ents) - 1; i >= 0; i-- {
		liveAfter[i] = liveAfter[i+1]
		if !ents[i].dead {
			liveAfter[i] += cellSpace(ents[i])
		}
	}
	if liveAfter[0] > avail {
		return nil, false
	}
	var kept []*entry
	for i, e := range ents {
		need := cellSpace(e)
		if e.dead && need > avail-liveAfter[i+1] {
			continue // residue that would not leave room for the live cells is not kept
		}
		avail -= need
		kept = append(kept, e)
	}
	return kept, true
}

// splitTableLeaves fills leaves greedily in key order; a leaf closes when the
// next live cell does not fit. Residue is kept only when it fits and, while
// the leaf holds no live cell yet, still leaves room for the next live cell,
// so no leaf is made of residue alone.
func (b *Builder) splitTableLeaves(ents []*entry) [][]*entry {
	var groups [][]*entry
	var cur []*entry
	hasLive := false
	avail := b.usable() - 8
	for i, e := range ents {
		need := cellSpace(e)
		if e.dead {
			reserve := 0
			if !hasLive {
				for _, f := range ents[i+1:] {
					if !f.dead {
						reserve = cellSpace(f)
						break
					}
				}
			}
			if need+reserve <= avail {
				cur = append(cur, e)
				avail -= need
			}
			continue
		}
		if need > avail {
			groups = append(groups, cur)
			cur, avail = nil, b.usable()-8
		}
		cur = append(cur, e)
		avail -= need
		hasLive = true
	}
	return append(groups, cur)
}

// splitIndexLeaves fills leaves greedily; the entry after each full leaf is
// promoted to the parent as a separator and is not stored in a leaf. The last
// leaf is never empty.
func (b *Builder) splitIndexLeaves(ents []*entry) (groups [][]*entry, seps []*entry) {
	i := 0
	for i < len(ents) {
		avail := b.usable() - 8
		var cur []*entry
		for i < len(ents) && cellSpace(ents[i]) <= avail {
			avail -= cellSpace(ents[i])
			cur = append(cur, ents[i])
			i++
		}
		if len(cur) == 0 {
			panic("sqlitetest: an index entry does not fit a page")
		}
		switch {
		case i >= len(ents):
			groups = append(groups, cur)
		case len(ents)-i == 1: // the separator would be the last entry: take it from this leaf
			if len(cur) < 2 {
				panic("sqlitetest: cannot balance the last index leaf")
			}
			groups = append(groups, cur[:len(cur)-1], ents[i:i+1])
			seps = append(seps, cur[len(cur)-1])
			i++
		default:
			groups = append(groups, cur)
			seps = append(seps, ents[i])
			i++
		}
	}
	return groups, seps
}

// interiorSpace is the room of the cell naming node n as a child.
func interiorSpace(n node) int { return 4 + len(n.sep) + 2 }

func (b *Builder) interiorFits(nodes []node, base int) bool {
	avail := b.usable() - base - 12
	for _, n := range nodes[:len(nodes)-1] {
		avail -= interiorSpace(n)
	}
	return avail >= 0
}

// splitInterior groups consecutive nodes into interior pages (each with at
// least two children). The separator after a group's last node is the
// group's own, used by the level above.
func (b *Builder) splitInterior(nodes []node) [][]node {
	var groups [][]node
	i := 0
	for i < len(nodes) {
		avail := b.usable() - 12
		cur := []node{nodes[i]}
		i++
		for i < len(nodes) {
			need := interiorSpace(cur[len(cur)-1])
			if need > avail {
				break
			}
			avail -= need
			cur = append(cur, nodes[i])
			i++
		}
		groups = append(groups, cur)
	}
	if n := len(groups); n >= 2 && len(groups[n-1]) == 1 {
		prev := groups[n-2]
		if len(prev) < 3 {
			panic("sqlitetest: cannot balance the last interior page")
		}
		groups[n-1] = []node{prev[len(prev)-1], groups[n-1][0]}
		groups[n-2] = prev[:len(prev)-1]
	}
	return groups
}

// placed is a cell with the bytes to write.
type placed struct {
	bytes []byte
	dead  bool
}

func (t *Table) writeLeaf(pg uint32, base int, flag byte, ents []*entry) {
	cells := make([]placed, len(ents))
	for i, e := range ents {
		cells[i] = placed{bytes: e.cell, dead: e.dead}
	}
	offs := t.b.writePage(pg, base, flag, cells, 0)
	for i, e := range ents {
		if !e.dead {
			e.placed = &cellInfo{page: pg, off: offs[i], cell: e.cell, overflow: e.overflow}
		}
	}
}

func (t *Table) writeInterior(pg uint32, base int, flag byte, nodes []node) {
	cells := make([]placed, len(nodes)-1)
	for i, n := range nodes[:len(nodes)-1] {
		var ptr [4]byte
		put32(ptr[:], n.page)
		cells[i] = placed{bytes: append(ptr[:], n.sep...)}
	}
	t.b.writePage(pg, base, flag, cells, nodes[len(nodes)-1].page)
}

// setWhere records the location of every live row's cell of a leaf.
func (t *Table) setWhere(ents []*entry) {
	if t.index {
		return
	}
	for _, e := range ents {
		if e.placed != nil {
			t.where[e.handle] = e.placed
		}
	}
}

// writePage writes a b-tree page: header at base, pointer array, cells from
// the end of the usable area downward in the order given (a dead cell gets no
// pointer; see the Table comment for what becomes of its bytes). It returns
// the offset of every cell.
func (b *Builder) writePage(pg uint32, base int, flag byte, cells []placed, rightChild uint32) []int {
	p := b.pages[pg-1]
	clear(p)
	u := b.usable()
	hdr := 8
	if flag == 0x02 || flag == 0x05 {
		hdr = 12
		put32(p[base+8:], rightChild)
	}
	type region struct {
		start, end int
		dead       bool
	}
	pos := u
	offs := make([]int, len(cells))
	regions := make([]region, 0, len(cells))
	var ptrs []int
	for i, c := range cells {
		size := max(len(c.bytes), 4)
		pos -= size
		copy(p[pos:], c.bytes)
		offs[i] = pos
		regions = append(regions, region{pos, pos + size, c.dead})
		if !c.dead {
			ptrs = append(ptrs, pos)
		}
	}
	if pos < base+hdr+2*len(ptrs) {
		panic(fmt.Sprintf("sqlitetest: page %d overflows (builder bug)", pg))
	}
	slices.Reverse(regions) // ascending addresses
	contentStart := pos
	i := 0
	for i < len(regions) && regions[i].dead { // residue at the lowest address joins the gap
		contentStart = regions[i].end
		i++
	}
	first, prev := 0, -1
	for i < len(regions) {
		if !regions[i].dead {
			i++
			continue
		}
		j := i
		for j < len(regions) && regions[j].dead {
			j++
		}
		start, size := regions[i].start, regions[j-1].end-regions[i].start
		if prev >= 0 {
			put16(p[prev:], uint16(start))
		} else {
			first = start
		}
		put16(p[start:], 0) // the last freeblock of the chain until the next one links to it
		put16(p[start+2:], uint16(size))
		prev = start
		i = j
	}
	p[base] = flag
	put16(p[base+1:], uint16(first))
	put16(p[base+3:], uint16(len(ptrs)))
	put16(p[base+5:], uint16(contentStart)) // 65536 wraps to 0, as the format says
	for k, o := range ptrs {
		put16(p[base+hdr+2*k:], uint16(o))
	}
	return offs
}

// CreateAutoIndex adds an index over the given declared column numbers of
// table whose schema row has the name given and a NULL sql, as the engine
// writes for the index behind a UNIQUE or PRIMARY KEY constraint.
func (b *Builder) CreateAutoIndex(name, table string, cols ...int) {
	b.CreateIndex(name, table, "", cols...)
	b.Object(name).nullSQL = true
}

// AddSchemaRow appends a row of five values (type, name, tbl_name, rootpage,
// sql) to the schema table after the rows of the objects, with no b-tree
// behind it: for views, triggers, virtual tables and for invalid rows. A value
// is nil, an int64 or a string, as for Insert.
func (b *Builder) AddSchemaRow(vals ...any) {
	b.raw = append(b.raw, vals)
	b.dirty = true
}

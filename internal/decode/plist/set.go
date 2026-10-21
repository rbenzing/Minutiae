package plist

// Set is an archived NSSet (Ordered false) or NSOrderedSet (Ordered true). Items are in archive
// order; their uniqueness is not checked.
type Set struct {
	Items   []any
	Ordered bool
}

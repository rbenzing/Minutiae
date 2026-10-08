package common

// EmptyableStrings lists the paths of the KString fields of s (and of the schemas nested in it)
// that accept an empty string. The record-type contracts say an optional text is absent, never
// empty (C3), so a package's test requires this list to hold only the fields it documents as
// deliberately empty-able.
func EmptyableStrings(s *Schema) []string { return emptyable(s, "") }

func emptyable(s *Schema, prefix string) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, f := range s.Fields {
		path := prefix + f.Name
		if f.Kind == KString && !f.NonEmpty {
			out = append(out, path)
		}
		if f.Obj != nil {
			out = append(out, emptyable(f.Obj, path+".")...)
		}
	}
	return out
}

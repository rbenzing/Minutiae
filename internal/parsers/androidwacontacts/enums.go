package androidwacontacts

// enumEntry is one code the parser interprets: the column it comes from, the stored value, the label and the sources
// that agree about that column (4I-R7, R4). Sources is a comma list of the research source IDs, or ref:<run id>. A code
// is mapped only when two independent sources agree about the SAME column and the value is in no research conflict
// row; TestWAContactsEnumTableCitesTwoSources enforces exactly this bar over the table.
type enumEntry struct {
	Column  string
	Value   int64
	Label   string
	Sources string
}

// colIsWhatsAppUser is wa_contacts.is_whatsapp_user.
const colIsWhatsAppUser = "is_whatsapp_user"

// enumTable is read-only. Only 1 is mapped: the value 0 is stored as it is and gets no label (spec 6.6).
var enumTable = []enumEntry{
	{colIsWhatsAppUser, 1, "whatsapp_user", "S9,S10"},
}

// lookupEnum returns the label of a stored value of a column, and false for a code the research does not support.
func lookupEnum(column string, v int64) (string, bool) {
	for _, e := range enumTable {
		if e.Column == column && e.Value == v {
			return e.Label, true
		}
	}
	return "", false
}

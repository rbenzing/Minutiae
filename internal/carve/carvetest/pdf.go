package carvetest

import (
	"fmt"
	"strings"
)

// PDFOpts shapes a synthetic PDF. Version is the number after "%PDF-"
// (default "1.7"); Increments is the number of incremental updates after the
// first revision (the file then holds 1+Increments revisions, each ending in
// startxref and %%EOF); BadStartxref points every startxref at a position
// that is no cross-reference section; NoEOF leaves out every %%EOF.
type PDFOpts struct {
	Version             string
	Increments          int
	BadStartxref, NoEOF bool
}

// PDF builds a minimal PDF with its revisions. Every %%EOF is followed by a
// line feed.
func PDF(o PDFOpts) []byte {
	if o.Version == "" {
		o.Version = "1.7"
	}
	var b strings.Builder
	b.WriteString("%PDF-" + o.Version + "\n%\xe2\xe3\xcf\xd3\n")
	off := func() int { return b.Len() }
	obj := func(n int, body string) int {
		at := off()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, body)
		return at
	}
	tail := func(xref int) {
		if o.BadStartxref {
			xref = 9 // inside the comment line
		}
		fmt.Fprintf(&b, "startxref\n%d\n", xref)
		if !o.NoEOF {
			b.WriteString("%%EOF\n")
		}
	}
	o1 := obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	o2 := obj(2, "<< /Type /Pages /Kids [] /Count 0 >>")
	x := off()
	fmt.Fprintf(&b, "xref\n0 3\n0000000000 65535 f \n%010d 00000 n \n%010d 00000 n \n", o1, o2)
	b.WriteString("trailer\n<< /Size 3 /Root 1 0 R >>\n")
	tail(x)
	prev := x
	for i := 0; i < o.Increments; i++ {
		n := 3 + i
		on := obj(n, "<< /Type /Annot >>")
		x = off()
		fmt.Fprintf(&b, "xref\n%d 1\n%010d 00000 n \n", n, on)
		fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Prev %d >>\n", n+1, prev)
		tail(x)
		prev = x
	}
	return []byte(b.String())
}

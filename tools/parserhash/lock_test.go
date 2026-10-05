package main

import (
	"strings"
	"testing"
)

const h64 = "src1:sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestLockParseRejectsUnsortedDuplicateAndMalformed(t *testing.T) {
	ok := "# c\na 1.0.0 " + h64 + "\na 1.0.10 " + h64 + "\nb 1.0.0 " + h64 + "\n"
	l, err := ParseLock([]byte(ok))
	if err != nil || len(l.Lines) != 3 || string(l.Format()) != ok {
		t.Fatalf("valid lock: %v %v", l, err)
	}
	for name, in := range map[string]string{
		"unsorted names":     "b 1.0.0 " + h64 + "\na 1.0.0 " + h64 + "\n",
		"unsorted numeric":   "a 1.0.10 " + h64 + "\na 1.0.9 " + h64 + "\n",
		"duplicate":          "a 1.0.0 " + h64 + "\na 1.0.0 " + h64 + "\n",
		"short hash":         "a 1.0.0 src1:sha256:00\n",
		"upper hash":         "a 1.0.0 src1:sha256:" + strings.Repeat("A", 64) + "\n",
		"bad version":        "a 1.0 " + h64 + "\n",
		"leading zero":       "a 1.00.0 " + h64 + "\n",
		"bad name":           "a/b 1.0.0 " + h64 + "\n",
		"two fields":         "a 1.0.0\n",
		"extra space":        "a  1.0.0 " + h64 + "\n",
		"comment after line": "a 1.0.0 " + h64 + "\n# late\n",
		"no final newline":   "a 1.0.0 " + h64,
		"CRLF":               "a 1.0.0 " + h64 + "\r\n",
		"blank line":         "a 1.0.0 " + h64 + "\n\n",
	} {
		if _, err := ParseLock([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if l, err := ParseLock(nil); err != nil || len(l.Lines) != 0 {
		t.Errorf("empty lock: %v %v", l, err)
	}
}

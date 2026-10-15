package evidence

import (
	"strings"
	"testing"
)

func TestReadRunLinesRefusesDuplicateAndCaseVariantKeys(t *testing.T) {
	bad := map[string]string{
		"duplicate offset":   `{"offset":0,"offset":5,"length":10}` + "\n",
		"duplicate length":   `{"offset":0,"length":10,"length":20}` + "\n",
		"capitalised offset": `{"Offset":0,"length":10}` + "\n",
		"upper-case length":  `{"offset":0,"LENGTH":10}` + "\n",
		"duplicate line 2":   `{"offset":0,"length":10}` + "\n" + `{"offset":1,"offset":1,"length":1}` + "\n",
	}
	for name, in := range bad {
		if _, err := readRunLines(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got, err := readRunLines(strings.NewReader(`{"offset":0,"length":10}` + "\n")); err != nil || len(got) != 1 {
		t.Errorf("good line: %v, %v", got, err)
	}
}

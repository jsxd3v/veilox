package detect

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestWorstCaseInputsAreFast guards against detectors that slow down badly on long inputs.
// Timing is meaningless with the race detector or coverage, so it only runs without them.
func TestWorstCaseInputsAreFast(t *testing.T) {
	if raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test: run without -race and -cover")
	}
	names := make([]string, 5000)
	for i := range names {
		names[i] = fmt.Sprintf("Person%d Surname%d", i, i)
	}
	e, err := New(Options{Names: names})
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{
		"digit groups": strings.Repeat("1 ", 50000),
		"digits":       strings.Repeat("1", 100000),
		"dashes":       strings.Repeat("12-", 30000),
		"dots":         strings.Repeat("1.", 50000),
		"at signs":     strings.Repeat("a@", 50000),
		"letters":      strings.Repeat("ab12 ", 20000),
		"prose":        strings.Repeat("Person12 Surname13 wrote to a@b.co about 4111 1111 1111 1111. ", 2000),
		"parens":       strings.Repeat("(1", 50000),
	}
	for name, in := range inputs {
		start := time.Now()
		e.Find(in)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s (%d bytes) took %v", name, len(in), d)
		} else {
			t.Logf("%s (%d bytes): %v", name, len(in), d)
		}
	}
}

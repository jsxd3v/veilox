package pricing

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `
currency: USD
models:
  gpt-test:
    input: 1.00
    cached_input: 0.25
    output: 4.00
  "claude-test*":
    input: 3
    cache_write: 3.75
    cache_read: 0.30
    output: 15
  "claude-test-long*":
    input: 1
    output: 1
`

func TestLookupAndCost(t *testing.T) {
	tb, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Len() != 3 {
		t.Fatal(tb.Len())
	}
	p, ok := tb.Lookup("gpt-test")
	if !ok {
		t.Fatal("gpt-test")
	}
	// 1,000 input + 500 cached + 2,000 output = 1000*1 + 500*0.25 + 2000*4 = 9,125 micro-dollars
	if c := p.Cost(Usage{Input: 1000, CacheRead: 500, Output: 2000}); c != 9125 {
		t.Fatal(c)
	}
	if _, ok := tb.Lookup("gpt-test-2026"); ok {
		t.Fatal("exact entries must not prefix-match")
	}
	p, ok = tb.Lookup("claude-test-2026-01-01")
	if !ok || p.Input != 3 {
		t.Fatal(p)
	}
	if c := p.Cost(Usage{Input: 100, CacheWrite: 1000, CacheRead: 1000, Output: 10}); c != 300+3750+300+150 {
		t.Fatal(c)
	}
	if p, _ := tb.Lookup("claude-test-long-x"); p.Input != 1 {
		t.Fatal("longest prefix should win")
	}
	if _, ok := tb.Lookup("unknown"); ok {
		t.Fatal("unknown")
	}
	if _, ok := tb.Lookup(""); ok {
		t.Fatal("empty")
	}
}

func TestCacheFallsBackToInput(t *testing.T) {
	p := Price{Input: 2, Output: 8}
	if c := p.Cost(Usage{CacheRead: 10, CacheWrite: 10}); c != 40 {
		t.Fatal(c)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{
		"currency: EUR\n",
		"models:\n  x:\n    input: -1\n",
		"models:\n  'a*b':\n    input: 1\n",
		"models:\n  'a**':\n    input: 1\n",
		"models:\n  x:\n    inptu: 1\n",
		"models: [",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	if tb, err := Parse(nil); err != nil || tb.Len() != 0 {
		t.Fatal("empty file should be fine")
	}
}

func TestLoad(t *testing.T) {
	tb, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil || tb.Len() != 0 {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(p, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	if tb, err := Load(p); err != nil || tb.Len() != 3 {
		t.Fatal(err)
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("a directory should fail")
	}
}

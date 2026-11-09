package mask

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/jsxd3v/veilox/internal/detect"
)

func engine(t testing.TB) *detect.Engine {
	t.Helper()
	e, err := detect.New(detect.Options{Names: []string{"Maria Santos", `Ann "Q" O'Neill`}, Rules: []detect.Rule{{Name: "CUSTOMER_ID", Pattern: `CUS-[0-9]{6}`}}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const sample = "Customer Maria Santos (maria.santos@example.com, +351 912 345 678) says card 4111 1111 1111 1111 was charged twice."

func TestMaskBasic(t *testing.T) {
	m := NewMapper(engine(t))
	got := m.Mask(sample)
	want := "Customer <NAME_1> (<EMAIL_1>, <PHONE_1>) says card <CREDIT_CARD_1> was charged twice."
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if m.Restore(got) != sample {
		t.Fatalf("restore: %s", m.Restore(got))
	}
	if !reflect.DeepEqual(m.Counts(), map[string]int{"NAME": 1, "EMAIL": 1, "PHONE": 1, "CREDIT_CARD": 1}) || m.Total() != 4 {
		t.Fatal(m.Counts())
	}
	if !m.Issued() {
		t.Fatal("issued")
	}
}

func TestSameValueSamePlaceholderAcrossTexts(t *testing.T) {
	m := NewMapper(engine(t))
	a := m.Mask("Maria Santos <maria@x.com>")
	b := m.Mask("Reply to MARIA@X.COM for maria santos, cc ana@x.com")
	if a != "<NAME_1> <<EMAIL_1>>" || b != "Reply to <EMAIL_1> for <NAME_1>, cc <EMAIL_2>" {
		t.Fatalf("%q %q", a, b)
	}
	// First spelling seen is restored.
	if got := m.Restore("<EMAIL_1> <NAME_1>"); got != "maria@x.com Maria Santos" {
		t.Fatal(got)
	}
	if m.Counts()["EMAIL"] != 3 {
		t.Fatal(m.Counts())
	}
}

func TestReservedPlaceholdersAreSkipped(t *testing.T) {
	m := NewMapper(engine(t))
	in := "The literal <EMAIL_1> and <EMAIL_2> are docs. Real: a@b.co"
	m.Reserve(in)
	m.Reserve("also <NAME_1>")
	out := m.Mask(in)
	if out != "The literal <EMAIL_1> and <EMAIL_2> are docs. Real: <EMAIL_3>" {
		t.Fatal(out)
	}
	if m.Mask("Maria Santos") != "<NAME_2>" {
		t.Fatal("name numbering should skip reserved 1")
	}
	// Literal placeholders that weren't issued stay as they are.
	if got := m.Restore("<EMAIL_1> <EMAIL_3> <NAME_1> <NAME_2> <PHONE_9>"); got != "<EMAIL_1> a@b.co <NAME_1> Maria Santos <PHONE_9>" {
		t.Fatal(got)
	}
}

func TestRestoreNothingIssued(t *testing.T) {
	m := NewMapper(engine(t))
	if m.Mask("nothing here") != "nothing here" || m.Restore("<EMAIL_1>") != "<EMAIL_1>" {
		t.Fatal("unexpected change")
	}
	s := m.NewStream(false)
	if s.Write("<EMA") != "<EMA" || s.Pending() {
		t.Fatal("must not hold when no placeholders were issued")
	}
}

func TestRestoreJSON(t *testing.T) {
	m := NewMapper(engine(t))
	masked := m.Mask(`Ann "Q" O'Neill <ann@x.com>`)
	args := `{"to":"` + strings.Split(masked, " ")[0] + `","note":"<b>hi</b>"}`
	got := m.RestoreJSON(args)
	var v map[string]string
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", got, err)
	}
	if v["to"] != `Ann "Q" O'Neill` || v["note"] != "<b>hi</b>" {
		t.Fatal(v)
	}
}

func TestRestoreJSONEscapedBrackets(t *testing.T) {
	m := NewMapper(engine(t))
	m.Mask("a@b.co Maria Santos")
	got := m.RestoreJSON(`{"to":"\u003cEMAIL_1\u003e","n":"\u003CNAME_1\u003E","x":"\u003cNAME_9\u003e","y":"<b>"}`)
	if got != `{"to":"a@b.co","n":"Maria Santos","x":"\u003cNAME_9\u003e","y":"<b>"}` {
		t.Fatal(got)
	}
	// Plain text mode leaves escapes alone: they are only meaningful inside JSON.
	if got := m.Restore(`\u003cEMAIL_1\u003e`); got != `\u003cEMAIL_1\u003e` {
		t.Fatal(got)
	}
}

func TestMaskTextFindings(t *testing.T) {
	m := NewMapper(engine(t))
	_, f := m.MaskText("x CUS-123456 y a@b.co")
	if len(f) != 2 || f[0].Type != "CUSTOMER_ID" || f[0].Start != 2 || f[0].Placeholder != "<CUSTOMER_ID_1>" || f[1].Value != "a@b.co" {
		t.Fatalf("%+v", f)
	}
}

func TestValues(t *testing.T) {
	m := NewMapper(engine(t))
	v := map[string]any{"to": "a@b.co", "list": []any{"Maria Santos", 3.0, true, nil}, "nested": map[string]any{"k": "a@b.co"}}
	MapStrings(v, m.Mask)
	if v["to"] != "<EMAIL_1>" || v["list"].([]any)[0] != "<NAME_1>" || v["nested"].(map[string]any)["k"] != "<EMAIL_1>" {
		t.Fatal(v)
	}
	r := m.RestoreValue(map[string]any{"<EMAIL_1>": []any{"<NAME_1>", 1.0}}).(map[string]any)
	if r["a@b.co"].([]any)[0] != "Maria Santos" {
		t.Fatal(r)
	}
}

// streamAll feeds pieces to a Stream and returns everything it emitted.
func streamAll(s *Stream, pieces []string) string {
	var b strings.Builder
	for _, p := range pieces {
		b.WriteString(s.Write(p))
	}
	b.WriteString(s.Flush())
	return b.String()
}

func TestStreamSplitPlaceholder(t *testing.T) {
	m := NewMapper(engine(t))
	m.Mask("Maria Santos a@b.co")
	s := m.NewStream(false)
	if out := s.Write("Hi <NA"); out != "Hi " || !s.Pending() {
		t.Fatalf("%q", out)
	}
	if out := s.Write("ME_1>, mail <EMA"); out != "Maria Santos, mail " {
		t.Fatalf("%q", out)
	}
	if out := s.Write("IL_1> <b>bold</b> <"); out != "a@b.co <b>bold</b> " {
		t.Fatalf("%q", out)
	}
	if out := s.Write("i>"); out != "<i>" {
		t.Fatalf("%q", out)
	}
	if out := s.Write("end <NAME_"); out != "end " {
		t.Fatalf("%q", out)
	}
	if out := s.Flush(); out != "<NAME_" {
		t.Fatalf("flush %q", out)
	}
}

func TestStreamCompletePlaceholderNotHeld(t *testing.T) {
	m := NewMapper(engine(t))
	m.Mask("a@b.co CUS-123456")
	s := m.NewStream(false)
	if out := s.Write("x <EMAIL_1>"); out != "x a@b.co" || s.Pending() {
		t.Fatalf("%q", out)
	}
}

// Every way of cutting a text into two or three pieces gives the same result as a one-shot restore.
func TestStreamEverySplit(t *testing.T) {
	m := NewMapper(engine(t))
	m.Mask(sample + " CUS-000001 Ann \"Q\" O'Neill")
	texts := []string{
		"Hi <NAME_1>, about <CREDIT_CARD_1> and <EMAIL_1>/<PHONE_1>.",
		"<NAME_1><NAME_1><<NAME_1>><CUSTOMER_ID_1>",
		"tags <b> <NAME_9> <NAME_ <EMAIL_1 < <> <NAME_2>",
		`{"to":"<NAME_2>","card":"<CREDIT_CARD_1>"}`,
		`{"to":"\u003cNAME_2\u003e","card":"\u003cCREDIT_CARD_1\u003e","x":"\u003cb\u003e \\u003"}`,
	}
	for _, text := range texts {
		for _, escape := range []bool{false, true} {
			want := m.restore(text, escape)
			for i := 0; i <= len(text); i++ {
				for j := i; j <= len(text); j++ {
					got := streamAll(m.NewStream(escape), []string{text[:i], text[i:j], text[j:]})
					if got != want {
						t.Fatalf("split %d,%d of %q (json=%v):\n got %q\nwant %q", i, j, text, escape, got, want)
					}
				}
			}
		}
	}
}

func TestStreamRandomChunks(t *testing.T) {
	m := NewMapper(engine(t))
	m.Mask(sample)
	r := rand.New(rand.NewPCG(1, 2))
	parts := []string{"<NAME_1>", "<EMAIL_1>", "<PHONE_1>", "<CREDIT_CARD_1>", "<", ">", "<NAME_", " text ", "<b>", "é", "<EMAIL_2>"}
	for range 2000 {
		var b strings.Builder
		for range r.IntN(30) {
			b.WriteString(parts[r.IntN(len(parts))])
		}
		text := b.String()
		var pieces []string
		for rest := text; rest != ""; {
			n := 1 + r.IntN(6)
			if n > len(rest) {
				n = len(rest)
			}
			pieces = append(pieces, rest[:n])
			rest = rest[n:]
		}
		if got, want := streamAll(m.NewStream(false), pieces), m.Restore(text); got != want {
			t.Fatalf("pieces %q:\n got %q\nwant %q", pieces, got, want)
		}
	}
}

func FuzzStreamEqualsRestore(f *testing.F) {
	e, _ := detect.New(detect.Options{Names: []string{"Maria Santos"}})
	m := NewMapper(e)
	m.Mask(sample)
	f.Add("Hi <NAME_1>, card <CREDIT_CARD_1>", uint8(3), uint8(7))
	f.Add("<<EMAIL_1>>", uint8(1), uint8(2))
	f.Fuzz(func(t *testing.T, text string, a, b uint8) {
		i, j := int(a)%(len(text)+1), int(b)%(len(text)+1)
		if i > j {
			i, j = j, i
		}
		got := streamAll(m.NewStream(false), []string{text[:i], text[i:j], text[j:]})
		if want := m.Restore(text); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
}

// Round trip: restore(mask(x)) == x for generated texts.
func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	words := []string{"Maria Santos", "maria santos", "a@b.co", "A@B.CO", "+351 912 345 678", "4111 1111 1111 1111",
		"DE89 3704 0044 0532 0130 00", "203.0.113.7", "CUS-123456", "hello", ",", "\n", "<EMAIL_1>", "<b>", "order 123456789"}
	for range 500 {
		var parts []string
		for range r.IntN(12) {
			parts = append(parts, words[r.IntN(len(words))])
		}
		text := strings.Join(parts, " ")
		m := NewMapper(engine(t))
		m.Reserve(text)
		if got := m.Restore(m.Mask(text)); got != restoreExpect(text) {
			t.Fatalf("round trip of %q gave %q", text, got)
		}
	}
}

// Different spellings of the same value come back as the first spelling seen.
func restoreExpect(text string) string {
	m := NewMapper(func() *detect.Engine {
		e, _ := detect.New(detect.Options{Names: []string{"Maria Santos", `Ann "Q" O'Neill`}, Rules: []detect.Rule{{Name: "CUSTOMER_ID", Pattern: `CUS-[0-9]{6}`}}})
		return e
	}())
	m.Reserve(text)
	_, findings := m.MaskText(text)
	first := map[string]string{}
	var b strings.Builder
	last := 0
	for _, f := range findings {
		if _, ok := first[f.Placeholder]; !ok {
			first[f.Placeholder] = f.Value
		}
		b.WriteString(text[last:f.Start])
		b.WriteString(first[f.Placeholder])
		last = f.End
	}
	b.WriteString(text[last:])
	return b.String()
}

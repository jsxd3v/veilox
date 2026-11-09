package detect

import (
	"reflect"
	"strings"
	"testing"
)

func mustEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// found returns "TYPE:value" for each match.
func found(e *Engine, text string) []string {
	var out []string
	for _, m := range e.Find(text) {
		if text[m.Start:m.End] != m.Value {
			panic("offsets don't match value")
		}
		out = append(out, m.Type+":"+m.Value)
	}
	return out
}

func check(t *testing.T, e *Engine, cases map[string][]string) {
	t.Helper()
	for text, want := range cases {
		got := found(e, text)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q\n got: %v\nwant: %v", text, got, want)
		}
	}
}

func only(t *testing.T, name string) *Engine {
	return mustEngine(t, Options{Builtins: []string{name}})
}

func TestEmail(t *testing.T) {
	check(t, only(t, "email"), map[string][]string{
		"mail maria.santos@example.com today":     {"EMAIL:maria.santos@example.com"},
		"<maria+tag@mail.example.co.uk>":          {"EMAIL:maria+tag@mail.example.co.uk"},
		"Write to A_B-c%d@x-y.io.":                {"EMAIL:A_B-c%d@x-y.io"},
		"two: a@b.cd, e@f.gh":                     {"EMAIL:a@b.cd", "EMAIL:e@f.gh"},
		"start.@x.com":                            {"EMAIL:start.@x.com"},
		"...dots@x.com":                           {"EMAIL:dots@x.com"},
		"no tld a@b":                              nil,
		"single letter tld a@b.c":                 nil,
		"handle @maria and maria@":                nil,
		"version pkg@1.2.3":                       nil,
		"glued a@b.com_x":                         nil,
		"José <jose@exemplo.pt> escreveu":         {"EMAIL:jose@exemplo.pt"},
		"MARIA@EXAMPLE.COM and maria@example.com": {"EMAIL:MARIA@EXAMPLE.COM", "EMAIL:maria@example.com"},
	})
}

func TestEmailKeyIsCaseInsensitive(t *testing.T) {
	ms := only(t, "email").Find("MARIA@EXAMPLE.COM maria@example.com")
	if len(ms) != 2 || ms[0].Key != ms[1].Key {
		t.Fatalf("keys differ: %+v", ms)
	}
}

func TestCreditCard(t *testing.T) {
	check(t, only(t, "credit_card"), map[string][]string{
		"card 4111 1111 1111 1111 please":            {"CREDIT_CARD:4111 1111 1111 1111"},
		"card 4111-1111-1111-1111":                   {"CREDIT_CARD:4111-1111-1111-1111"},
		"4111111111111111":                           {"CREDIT_CARD:4111111111111111"},
		"amex 3782 822463 10005.":                    {"CREDIT_CARD:3782 822463 10005"},
		"mc 5555555555554444, visa 4012888888881881": {"CREDIT_CARD:5555555555554444", "CREDIT_CARD:4012888888881881"},
		"bad luhn 4111 1111 1111 1112":               nil,
		"too short 4111 1111 111":                    nil,
		"too long 41111111111111111111111":           nil,
		"order 12 4111 1111 1111 1111":               {"CREDIT_CARD:4111 1111 1111 1111"},
		"glued x4111111111111111":                    nil,
		"double space 4111  1111 1111 1111":          nil,
		"zeros 0000 0000 0000 0000":                  nil,
		"4111111111111111 5555555555554444":          {"CREDIT_CARD:4111111111111111", "CREDIT_CARD:5555555555554444"},
	})
}

func TestLuhn(t *testing.T) {
	for d, want := range map[string]bool{"79927398713": true, "79927398710": false, "4111111111111111": true, "0": true} {
		if luhn(d) != want {
			t.Errorf("luhn(%s) != %v", d, want)
		}
	}
}

func TestIBAN(t *testing.T) {
	check(t, only(t, "iban"), map[string][]string{
		"pay to DE89 3704 0044 0532 0130 00 today":                        {"IBAN:DE89 3704 0044 0532 0130 00"},
		"DE89370400440532013000":                                          {"IBAN:DE89370400440532013000"},
		"gb82 west 1234 5698 7654 32":                                     {"IBAN:gb82 west 1234 5698 7654 32"},
		"PT50000201231234567890154.":                                      {"IBAN:PT50000201231234567890154"},
		"two DE89 3704 0044 0532 0130 00 and GB82 WEST 1234 5698 7654 32": {"IBAN:DE89 3704 0044 0532 0130 00", "IBAN:GB82 WEST 1234 5698 7654 32"},
		"bad checksum DE88 3704 0044 0532 0130 00":                        nil,
		"wrong length DE89 3704 0044 0532 0130 0":                         nil,
		"glued XDE89370400440532013000":                                   nil,
		"plain words AB12 is not an iban at all ok":                       nil,
	})
}

func TestIPv4(t *testing.T) {
	check(t, only(t, "ipv4"), map[string][]string{
		"from 203.0.113.7 at noon": {"IPV4:203.0.113.7"},
		"10.0.0.1, 192.168.1.20":   {"IPV4:10.0.0.1", "IPV4:192.168.1.20"},
		"ip:255.255.255.255.":      {"IPV4:255.255.255.255"},
		"version 1.2.3.4.5":        nil,
		"v1.2.3.4":                 nil,
		"bad octet 256.1.1.1":      nil,
		"leading zero 01.2.3.4":    nil,
		"0.0.0.0":                  {"IPV4:0.0.0.0"},
		"glued 1.2.3.4x":           nil,
	})
	skip := mustEngine(t, Options{Builtins: []string{"ipv4"}, SkipPrivateIPv4: true})
	check(t, skip, map[string][]string{
		"10.1.2.3 172.16.0.1 172.32.0.1 192.168.0.1 127.0.0.1 8.8.8.8": {"IPV4:172.32.0.1", "IPV4:8.8.8.8"},
	})
}

func TestPhone(t *testing.T) {
	check(t, only(t, "phone"), map[string][]string{
		"call +351 912 345 678 now":                   {"PHONE:+351 912 345 678"},
		"call +351912345678":                          {"PHONE:+351912345678"},
		"(555) 010-4477":                              {"PHONE:(555) 010-4477"},
		"+1 (555) 010-4477":                           {"PHONE:+1 (555) 010-4477"},
		"tel 06 12 34 56 78.":                         {"PHONE:06 12 34 56 78"},
		"tel 912 345 678":                             {"PHONE:912 345 678"},
		"tel 030.1234.5678":                           {"PHONE:030.1234.5678"},
		"tel 06 12 34 56 78 2 times":                  {"PHONE:06 12 34 56 78"},
		"order 123456789":                             nil,
		"order 2026-1105-77":                          nil,
		"date 2026-11-09 14:30":                       nil,
		"date 09.11.2026 10 00":                       nil,
		"ip 192.168.100.200":                          nil,
		"short 555-0101":                              nil,
		"too long +1234567890123456":                  nil,
		"glued 912 345 678x":                          nil,
		"ref-912 345 678":                             nil,
		"price 1,912 345 678":                         nil,
		"0000 0000 0000 0000":                         nil,
		"acct 3704 0044 0532 0130 00":                 nil,
		"card 4111 1111 1111 1112":                    nil,
		"ids 1234 5678 9012 3456 then 06 12 34 56 78": {"PHONE:06 12 34 56 78"},
	})
}

func TestPhoneKey(t *testing.T) {
	ms := only(t, "phone").Find("+351 912 345 678 or +351-912-345-678")
	if len(ms) != 2 || ms[0].Key != ms[1].Key || ms[0].Key != "+351912345678" {
		t.Fatalf("%+v", ms)
	}
}

func TestNames(t *testing.T) {
	e := mustEngine(t, Options{Builtins: []string{}, Names: []string{"Maria Santos", "Maria", "John O'Neill", "José Álvarez", "  ", "maria"}})
	check(t, e, map[string][]string{
		"Maria Santos called":           {"NAME:Maria Santos"},
		"MARIA SANTOS and maria":        {"NAME:MARIA SANTOS", "NAME:maria"},
		"Maria  Santos (two spaces)":    {"NAME:Maria  Santos"},
		"Maria Santosa is someone else": {"NAME:Maria"},
		"Mariana is not Maria":          {"NAME:Maria"},
		"John O'Neill's order":          {"NAME:John O'Neill"},
		"Olá José Álvarez!":             {"NAME:José Álvarez"},
		"JoséÁlvarez":                   nil,
		"xMaria":                        nil,
	})
	ms := e.Find("Maria  Santos / maria santos")
	if ms[0].Key != ms[1].Key {
		t.Fatalf("name keys differ: %q %q", ms[0].Key, ms[1].Key)
	}
}

func TestNoNamesNoDetector(t *testing.T) {
	e := mustEngine(t, Options{Builtins: []string{}, Names: []string{" ", ""}})
	if len(e.Types()) != 0 {
		t.Fatal(e.Types())
	}
}

func TestRules(t *testing.T) {
	e := mustEngine(t, Options{Builtins: []string{}, Rules: []Rule{
		{Name: "CUSTOMER_ID", Pattern: `CUS-[0-9]{6}`},
		{Name: "POLICY_NO", Pattern: `policy no\. ([A-Z]{2}[0-9]{8})`, Group: 1},
	}})
	check(t, e, map[string][]string{
		"customer CUS-004211 and CUS-1":      {"CUSTOMER_ID:CUS-004211"},
		"see policy no. AB12345678 for info": {"POLICY_NO:AB12345678"},
	})
}

func TestRuleErrors(t *testing.T) {
	for _, r := range []Rule{
		{Name: "lower", Pattern: "x"},
		{Name: "EMAIL", Pattern: "x"},
		{Name: "NAME", Pattern: "x"},
		{Name: "BAD", Pattern: "(["},
		{Name: "GRP", Pattern: "a(b)", Group: 2},
	} {
		if _, err := New(Options{Rules: []Rule{r}}); err == nil {
			t.Errorf("rule %+v: expected error", r)
		}
	}
	if _, err := New(Options{Rules: []Rule{{Name: "A", Pattern: "a"}, {Name: "A", Pattern: "b"}}}); err == nil {
		t.Error("duplicate rule: expected error")
	}
	if _, err := New(Options{Builtins: []string{"ssn"}}); err == nil {
		t.Error("unknown detector: expected error")
	}
}

func TestOverlaps(t *testing.T) {
	e := mustEngine(t, Options{
		Names: []string{"Maria Santos", "Santos"},
		Rules: []Rule{{Name: "DIGITS16", Pattern: `[0-9]{16}`}, {Name: "LONGER", Pattern: `ref 203\.0\.113\.7`}},
	})
	check(t, e, map[string][]string{
		// Longer match wins: the email contains a listed name.
		"maria.santos@example.com": {"EMAIL:maria.santos@example.com"},
		"santos@example.com":       {"EMAIL:santos@example.com"},
		// Same span: card beats a custom rule.
		"4111111111111111": {"CREDIT_CARD:4111111111111111"},
		// A custom rule that is longer than a built-in match wins.
		"ref 203.0.113.7": {"LONGER:ref 203.0.113.7"},
		// Card with separators that also looks like a phone.
		"4111 1111 1111 1111": {"CREDIT_CARD:4111 1111 1111 1111"},
	})
}

func TestEverything(t *testing.T) {
	e := mustEngine(t, Options{Names: []string{"Maria Santos"}})
	text := "Customer Maria Santos (maria.santos@example.com, +351 912 345 678) says card 4111 1111 1111 1111 " +
		"was charged twice. IBAN PT50000201231234567890154, login from 203.0.113.7."
	want := []string{
		"NAME:Maria Santos", "EMAIL:maria.santos@example.com", "PHONE:+351 912 345 678",
		"CREDIT_CARD:4111 1111 1111 1111", "IBAN:PT50000201231234567890154", "IPV4:203.0.113.7",
	}
	if got := found(e, text); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestDeterministic(t *testing.T) {
	e := mustEngine(t, Options{Names: []string{"Maria Santos", "Maria", "Ana"}, Rules: []Rule{{Name: "X", Pattern: `[0-9]{9}`}}})
	text := strings.Repeat("Maria Santos 912 345 678 a@b.co 4111111111111111 Ana 123456789 ", 20)
	first := e.Find(text)
	for range 100 {
		if !reflect.DeepEqual(first, e.Find(text)) {
			t.Fatal("results differ between runs")
		}
	}
}

func TestTypes(t *testing.T) {
	e := mustEngine(t, Options{Names: []string{"A"}, Rules: []Rule{{Name: "R", Pattern: "r"}}})
	want := []string{CreditCard, IBAN, Email, IPv4, Phone, "R", Name}
	if !reflect.DeepEqual(e.Types(), want) {
		t.Fatal(e.Types())
	}
}

func FuzzFindDoesNotPanic(f *testing.F) {
	e, _ := New(Options{Names: []string{"Maria Santos"}, Rules: []Rule{{Name: "R", Pattern: `x+`}}})
	for _, s := range []string{"a@b.co", "+351 912 345 678", "(555) 010-4477", "DE89 3704 0044 0532 0130 00", "1.2.3.4", "4111 1111 1111 1111", "Maria Santos"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		prev := -1
		for _, m := range e.Find(s) {
			if m.Start < prev || m.End <= m.Start || s[m.Start:m.End] != m.Value {
				t.Fatalf("bad match %+v in %q", m, s)
			}
			prev = m.End
		}
	})
}

func TestNameMustStartWithWordRune(t *testing.T) {
	if _, err := New(Options{Names: []string{"(Maria"}}); err == nil {
		t.Fatal("expected error")
	}
}

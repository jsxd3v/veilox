package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewKeyAndLookup(t *testing.T) {
	a, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewKey()
	if !strings.HasPrefix(a, Prefix) || len(a) != 43 || a == b {
		t.Fatalf("%q %q", a, b)
	}
	k, err := NewKeyring(map[string]string{"one": Hash(a), "two": Hash(b)})
	if err != nil {
		t.Fatal(err)
	}
	if app, ok := k.Lookup(a); !ok || app != "one" {
		t.Fatal(app)
	}
	if app, ok := k.Lookup(b); !ok || app != "two" {
		t.Fatal(app)
	}
	for _, bad := range []string{"", "vx_nope", a + "x", Hash(a)} {
		if _, ok := k.Lookup(bad); ok {
			t.Fatalf("%q should not match", bad)
		}
	}
}

func TestHashIsSHA256(t *testing.T) {
	if Hash("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(Hash("abc"))
	}
}

func TestBadHash(t *testing.T) {
	if _, err := NewKeyring(map[string]string{"x": "zz"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestFromRequest(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	if FromRequest(r) != "" {
		t.Fatal("empty")
	}
	r.Header.Set("Authorization", "Bearer  vx_1 ")
	if FromRequest(r) != "vx_1" {
		t.Fatal(FromRequest(r))
	}
	r.Header.Set("Authorization", "bearer vx_2")
	if FromRequest(r) != "vx_2" {
		t.Fatal(FromRequest(r))
	}
	r.Header.Set("Authorization", "Basic abc")
	if FromRequest(r) != "" {
		t.Fatal("basic auth is not a key")
	}
	r.Header.Set("x-api-key", "vx_3")
	if FromRequest(r) != "vx_3" {
		t.Fatal(FromRequest(r))
	}
}

// Package auth handles the keys apps use to call veilox.
//
// The config stores only the SHA-256 of each key. Lookups hash the presented key and compare it
// against every app in constant time.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strings"
)

// Prefix starts every app key.
const Prefix = "vx_"

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewKey returns a random app key: vx_ + 40 base62 characters (~238 bits).
func NewKey() (string, error) {
	var b strings.Builder
	b.WriteString(Prefix)
	max := big.NewInt(int64(len(alphabet)))
	for range 40 {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String(), nil
}

// Hash returns the hex SHA-256 of a key, as stored in key_sha256.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type entry struct {
	app  string
	hash []byte
}

// Keyring maps keys to app names.
type Keyring struct{ entries []entry }

// NewKeyring builds a keyring from app name → hex hash.
func NewKeyring(hashes map[string]string) (*Keyring, error) {
	k := &Keyring{}
	for app, h := range hashes {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("app %q: invalid key_sha256", app)
		}
		k.entries = append(k.entries, entry{app, b})
	}
	return k, nil
}

// Lookup returns the app that owns key. It compares against every entry without stopping early,
// so timing doesn't reveal which app matched or how close a guess was.
func (k *Keyring) Lookup(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(key))
	found := ""
	for _, e := range k.entries {
		if subtle.ConstantTimeCompare(sum[:], e.hash) == 1 {
			found = e.app
		}
	}
	return found, found != ""
}

// FromRequest reads the app key from Authorization: Bearer (OpenAI SDKs) or x-api-key (Anthropic SDKs).
func FromRequest(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("Authorization"); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

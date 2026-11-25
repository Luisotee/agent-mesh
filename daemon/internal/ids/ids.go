// Package ids makes the random identifiers and secrets the daemon hands out.
package ids

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"strings"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// New returns prefix followed by n random lowercase base32 characters (5 bits each).
func New(prefix string, n int) string {
	buf := make([]byte, (n*5+7)/8)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return prefix + strings.ToLower(b32.EncodeToString(buf))[:n]
}

// Secret returns a 256-bit random secret, URL-safe, with a prefix that says what it is.
func Secret(prefix string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf)
}

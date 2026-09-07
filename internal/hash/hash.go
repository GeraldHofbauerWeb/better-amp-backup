// Package hash wraps the content hash used to address objects in the repository.
//
// The hash is BLAKE3-256 over the *plaintext* of an object. It is deliberately
// a distinct type from a plain string so that a hex digest can never be mixed
// up with a file path or an AMP backup GUID.
package hash

import (
	"encoding/hex"
	"fmt"
	"io"

	"github.com/zeebo/blake3"
)

// Size is the length of a hash in bytes.
const Size = 32

// HexSize is the length of a hash in its hex representation.
const HexSize = Size * 2

// Hash is a BLAKE3-256 digest.
type Hash [Size]byte

// Zero is the hash value of an unset Hash. It is never a valid object address.
var Zero Hash

// IsZero reports whether h is unset.
func (h Hash) IsZero() bool { return h == Zero }

// String returns the lowercase hex representation.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// MarshalText implements encoding.TextMarshaler so hashes serialise as hex in JSON.
func (h Hash) MarshalText() ([]byte, error) {
	buf := make([]byte, HexSize)
	hex.Encode(buf, h[:])
	return buf, nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// Parse decodes a hex digest.
func Parse(s string) (Hash, error) {
	var h Hash
	if len(s) != HexSize {
		return h, fmt.Errorf("hash: want %d hex chars, got %d", HexSize, len(s))
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("hash: %w", err)
	}
	return h, nil
}

// Sum hashes a byte slice.
func Sum(b []byte) Hash {
	return Hash(blake3.Sum256(b))
}

// Hasher incrementally computes a Hash.
type Hasher struct{ h *blake3.Hasher }

// New returns a Hasher ready for use.
func New() *Hasher { return &Hasher{h: blake3.New()} }

func (w *Hasher) Write(p []byte) (int, error) { return w.h.Write(p) }

// Sum returns the digest of everything written so far.
func (w *Hasher) Sum() Hash {
	var out Hash
	w.h.Digest().Read(out[:])
	return out
}

// Reset prepares the Hasher for reuse.
func (w *Hasher) Reset() { w.h.Reset() }

// OfReader consumes r and returns its digest along with the number of bytes read.
func OfReader(r io.Reader) (Hash, int64, error) {
	w := New()
	n, err := io.Copy(w, r)
	if err != nil {
		return Zero, n, err
	}
	return w.Sum(), n, nil
}

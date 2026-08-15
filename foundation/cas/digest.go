package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ErrInvalidDigest is returned by ParseDigest for anything that is not exactly
// 64 lowercase hex characters.
var ErrInvalidDigest = errors.New("cas: digest must be 64 lowercase hex characters")

// shortLen is how many hex characters Short returns. Forty-eight bits is far
// more than enough to keep the release directories of one site distinct, and
// short enough to read off a terminal and type back.
const shortLen = 12

// Digest names a chunk by the SHA-256 of its plaintext bytes.
//
// Deliberately over the plaintext and not over the stored, compressed form. The
// entire value of this package is that identical content is written once, and
// two compressors -- or two versions of one compressor, or one compressor at two
// levels -- may encode the same bytes differently. Addressing the compressed
// form would make deduplication depend on the compressor staying bit-identical
// for the life of the store, which is not a promise anyone can make.
//
// An array rather than a slice, so Digest is comparable and usable as a map key.
// The mark phase of the collector depends on that.
//
// The zero value means "no digest". It is a legitimate SHA-256 output in
// principle, but finding content that hashes to sixty-four zeros is infeasible,
// so nothing will ever hold it by accident.
type Digest struct {
	b [sha256.Size]byte
}

// SumDigest returns the digest of b.
func SumDigest(b []byte) Digest {
	return Digest{b: sha256.Sum256(b)}
}

// ParseDigest validates s as a hex-encoded SHA-256.
//
// Uppercase is rejected rather than folded. A digest is a filesystem path here,
// and accepting both cases would give one chunk two names on a case-sensitive
// filesystem and a silent collision on a case-insensitive one.
func ParseDigest(s string) (Digest, error) {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return Digest{}, ErrInvalidDigest
	}

	var d Digest
	if _, err := hex.Decode(d.b[:], []byte(s)); err != nil {
		return Digest{}, ErrInvalidDigest
	}

	// hex.Decode accepts uppercase; the round trip catches it.
	if d.String() != s {
		return Digest{}, ErrInvalidDigest
	}

	return d, nil
}

// MustParseDigest parses s and panics on failure; for tests and known-good
// constants.
func MustParseDigest(s string) Digest {
	d, err := ParseDigest(s)
	if err != nil {
		panic(err)
	}

	return d
}

// String returns the lowercase hex encoding.
func (d Digest) String() string { return hex.EncodeToString(d.b[:]) }

// Short returns the first shortLen hex characters, for release directory names
// and anywhere a human has to read one.
func (d Digest) Short() string { return d.String()[:shortLen] }

// IsZero reports whether d is the unset zero value.
func (d Digest) IsZero() bool { return d == Digest{} }

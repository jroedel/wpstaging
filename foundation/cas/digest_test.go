package cas

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestDigestRoundTrip(t *testing.T) {
	want := SumDigest([]byte("wp-config.php"))

	got, err := ParseDigest(want.String())
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}

	if got != want {
		t.Errorf("round trip changed the digest: %s -> %s", want, got)
	}
}

func TestParseDigestRejects(t *testing.T) {
	valid := SumDigest(nil).String()

	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"too short", valid[:63]},
		{"too long", valid + "a"},
		{"not hex", strings.Repeat("z", 64)},
		// Uppercase is rejected rather than folded: a digest is a path, and
		// accepting both cases gives one chunk two names on a case-sensitive
		// filesystem and a collision on a case-insensitive one.
		{"uppercase", strings.ToUpper(valid)},
		{"mixed case", strings.ToUpper(valid[:2]) + valid[2:]},
		{"leading space", " " + valid[1:]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseDigest(tt.input); err != ErrInvalidDigest {
				t.Errorf("ParseDigest(%q) = %v, want ErrInvalidDigest", tt.input, err)
			}
		})
	}
}

func TestDigestZeroValue(t *testing.T) {
	var zero Digest

	if !zero.IsZero() {
		t.Error("the zero Digest does not report IsZero")
	}

	if d := SumDigest(nil); d.IsZero() {
		t.Error("the digest of empty input reports IsZero; it is a real digest")
	}
}

func TestDigestShort(t *testing.T) {
	d := SumDigest([]byte("uploads/2016/04"))

	if len(d.Short()) != shortLen {
		t.Errorf("Short() is %d characters, want %d", len(d.Short()), shortLen)
	}

	if !strings.HasPrefix(d.String(), d.Short()) {
		t.Error("Short() is not a prefix of String()")
	}
}

func TestSumDigestMatchesSHA256(t *testing.T) {
	// The store's addressing is a documented format, not an internal detail:
	// anyone auditing a store should be able to run sha256sum and get the
	// filename back.
	data := []byte("the quick brown fox")
	want := sha256.Sum256(data)

	if got := SumDigest(data); got.b != want {
		t.Errorf("SumDigest does not agree with crypto/sha256")
	}
}

func TestMustParseDigestPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustParseDigest did not panic on invalid input")
		}
	}()

	MustParseDigest("nonsense")
}

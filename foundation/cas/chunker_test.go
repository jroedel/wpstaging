package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math/bits"
	"math/rand/v2"
	"testing"
)

// frozenGearChecksum pins the gear table forever.
//
// If this test fails, the table changed, and every store written by the old
// table stops deduplicating against anything written by the new one -- the next
// snapshot of an unchanged site silently costs a full copy. There is no
// migration for that short of rewriting every store. The only correct response
// to a failure here is to put the generator back.
const frozenGearChecksum = "3aa6d5acf7ca8bc0df52e546b57a5952b88b51e802bfbdefa1ba48f881f24b6e"

func gearChecksum(g [256]uint64) string {
	h := sha256.New()

	var b [8]byte
	for _, v := range g {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}

	return hex.EncodeToString(h.Sum(nil))
}

func TestGearTableIsFrozen(t *testing.T) {
	if got := gearChecksum(gear); got != frozenGearChecksum {
		t.Fatalf("gear table changed, which breaks deduplication against every existing store\n got: %s\nwant: %s", got, frozenGearChecksum)
	}
}

func TestGearTableIsWellSpread(t *testing.T) {
	// A degenerate table -- repeated values, or values clustered in a few bits --
	// would still produce boundaries, just terrible ones. Two cheap sanity checks
	// that would catch a broken generator.
	seen := make(map[uint64]bool, len(gear))
	var total int

	for _, v := range gear {
		if seen[v] {
			t.Fatalf("duplicate gear value %#x", v)
		}
		seen[v] = true
		total += bits.OnesCount64(v)
	}

	// 256 values of 64 bits each, expected half set.
	if mean := float64(total) / float64(len(gear)); mean < 28 || mean > 36 {
		t.Errorf("mean population count %.1f, want near 32", mean)
	}
}

func TestSpreadMask(t *testing.T) {
	for _, want := range []int{1, 8, 16, 20, 48} {
		mask := spreadMask(want)

		if got := bits.OnesCount64(mask); got != want {
			t.Errorf("spreadMask(%d) set %d bits, want %d (mask %#016x)", want, got, want, mask)
		}

		// Every masked bit must sit high enough that it depends on a real window
		// of bytes; see the comment on spreadMask.
		if mask&((1<<16)-1) != 0 {
			t.Errorf("spreadMask(%d) = %#016x has bits below 16", want, mask)
		}
	}
}

// deterministicBytes returns n pseudo-random bytes, the same ones every run.
func deterministicBytes(t *testing.T, n int, seed uint64) []byte {
	t.Helper()

	r := rand.New(rand.NewPCG(seed, 0x5DEECE66D))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}

	return b
}

// chunkAll splits data and returns the pieces' digests and lengths.
func chunkAll(t *testing.T, data []byte, cfg Config) ([]Digest, []int) {
	t.Helper()

	c := newChunker(bytes.NewReader(data), cfg)

	var digests []Digest
	var sizes []int

	for {
		chunk, err := c.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}

		digests = append(digests, SumDigest(chunk))
		sizes = append(sizes, len(chunk))
	}

	return digests, sizes
}

// TestChunkerAverageSize is the empirical check on the mask choice.
//
// The claim in spreadMask -- that sampling high, spread-out bits gives the
// boundary test roughly its nominal probability -- is not something a reader can
// verify by inspection. This measures it. FastCDC's normalized chunking puts the
// true mean somewhat above the configured average, since the boundary test only
// begins at MinSize, so the band is deliberately wide; what it is really
// catching is a mask that is off by an order of magnitude.
func TestChunkerAverageSize(t *testing.T) {
	cfg := DefaultConfig()
	data := deterministicBytes(t, 64<<20, 7)

	_, sizes := chunkAll(t, data, cfg)

	if len(sizes) < 100 {
		t.Fatalf("only %d chunks, too few to say anything", len(sizes))
	}

	var total int
	for _, s := range sizes {
		total += s
	}
	mean := total / len(sizes)

	lo, hi := cfg.AvgSize*3/4, cfg.AvgSize*2
	if mean < lo || mean > hi {
		t.Errorf("mean chunk size %d bytes, want within [%d, %d] of the %d target", mean, lo, hi, cfg.AvgSize)
	}

	t.Logf("%d chunks, mean %d bytes (target %d)", len(sizes), mean, cfg.AvgSize)
}

func TestChunkerRespectsBounds(t *testing.T) {
	cfg := DefaultConfig()
	data := deterministicBytes(t, 16<<20, 11)

	_, sizes := chunkAll(t, data, cfg)

	// Every chunk but the last is bounded on both sides. The last is whatever
	// the stream had left, which may be anything at all.
	for i, size := range sizes[:len(sizes)-1] {
		if size < cfg.MinSize || size > cfg.MaxSize {
			t.Fatalf("chunk %d is %d bytes, outside [%d, %d]", i, size, cfg.MinSize, cfg.MaxSize)
		}
	}
}

func TestChunkerIsDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	data := deterministicBytes(t, 8<<20, 13)

	first, _ := chunkAll(t, data, cfg)
	second, _ := chunkAll(t, data, cfg)

	if len(first) != len(second) {
		t.Fatalf("same input produced %d then %d chunks", len(first), len(second))
	}

	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("chunk %d differs between runs over identical input", i)
		}
	}
}

// TestChunkerResistsInsertion is the property the whole storage model rests on.
//
// Inserting bytes near the front of a file must not shift every boundary after
// it. If it did, editing one line of a database dump would re-store the entire
// dump, and keeping fifty states would cost fifty full copies -- the thing this
// package exists to avoid. A fixed-size splitter fails this test completely.
func TestChunkerResistsInsertion(t *testing.T) {
	cfg := DefaultConfig()
	original := deterministicBytes(t, 16<<20, 17)

	// Splice 500 bytes in near the start, well inside the first chunk.
	const at = 5000
	modified := make([]byte, 0, len(original)+500)
	modified = append(modified, original[:at]...)
	modified = append(modified, deterministicBytes(t, 500, 19)...)
	modified = append(modified, original[at:]...)

	before, _ := chunkAll(t, original, cfg)
	after, _ := chunkAll(t, modified, cfg)

	kept := make(map[Digest]bool, len(before))
	for _, d := range before {
		kept[d] = true
	}

	var shared int
	for _, d := range after {
		if kept[d] {
			shared++
		}
	}

	reuse := float64(shared) / float64(len(before))
	if reuse < 0.9 {
		t.Errorf("only %.0f%% of chunks survived a 500-byte insertion (%d of %d); content-defined chunking is not working", reuse*100, shared, len(before))
	}

	t.Logf("%d of %d chunks reused (%.1f%%) after inserting 500 bytes at offset %d", shared, len(before), reuse*100, at)
}

func TestChunkerShortInput(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name string
		size int
		want int // chunks expected
	}{
		{"empty", 0, 0},
		{"one byte", 1, 1},
		{"below the minimum", cfg.MinSize - 1, 1},
		{"exactly the minimum", cfg.MinSize, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := deterministicBytes(t, tt.size, 23)

			digests, sizes := chunkAll(t, data, cfg)

			if len(digests) != tt.want {
				t.Fatalf("got %d chunks, want %d", len(digests), tt.want)
			}

			var total int
			for _, s := range sizes {
				total += s
			}
			if total != tt.size {
				t.Errorf("chunks total %d bytes, want %d", total, tt.size)
			}
		})
	}
}

// TestChunkerCoversInputExactly guards the buffer-shifting in next: a chunker
// that drops or repeats bytes across the buffer boundary would still produce
// plausible-looking chunks.
func TestChunkerCoversInputExactly(t *testing.T) {
	cfg := DefaultConfig()
	data := deterministicBytes(t, 8<<20, 29)

	c := newChunker(bytes.NewReader(data), cfg)

	var got []byte
	for {
		chunk, err := c.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}

		got = append(got, chunk...)
	}

	if !bytes.Equal(got, data) {
		t.Fatalf("concatenated chunks (%d bytes) do not reproduce the input (%d bytes)", len(got), len(data))
	}
}

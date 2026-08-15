package cas

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	cfg := DefaultConfig()
	cfg.SkipSync = true // tests are not restoring anything

	s, err := Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s
}

func mustPut(t *testing.T, s *Store, data []byte) Blob {
	t.Helper()

	blob, err := s.Put(t.Context(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	return blob
}

func mustStat(t *testing.T, s *Store) Stats {
	t.Helper()

	st, err := s.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	return st
}

func TestStoreRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"single byte", 1},
		{"below the chunk minimum", 1000},
		{"a few chunks", 2 << 20},
		{"many chunks", 24 << 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			data := deterministicBytes(t, tt.size, 31)

			blob := mustPut(t, s, data)

			if blob.Size != int64(tt.size) {
				t.Errorf("blob reports %d bytes, wrote %d", blob.Size, tt.size)
			}

			got, err := s.ReadAll(t.Context(), blob)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}

			if !bytes.Equal(got, data) {
				t.Fatalf("read back %d bytes, not equal to the %d written", len(got), len(data))
			}
		})
	}
}

// TestStoreGetStreams checks the reader path specifically, with a tiny
// destination buffer so it is exercised across chunk boundaries rather than
// swallowing each chunk whole.
func TestStoreGetStreams(t *testing.T) {
	s := newTestStore(t)
	data := deterministicBytes(t, 4<<20, 37)

	blob := mustPut(t, s, data)

	rc := s.Get(t.Context(), blob)
	defer rc.Close()

	var got bytes.Buffer
	if _, err := io.CopyBuffer(&got, rc, make([]byte, 7)); err != nil {
		t.Fatalf("copy: %v", err)
	}

	if !bytes.Equal(got.Bytes(), data) {
		t.Fatal("streamed bytes differ from what was stored")
	}
}

func TestStoreDeduplicates(t *testing.T) {
	s := newTestStore(t)
	data := deterministicBytes(t, 8<<20, 41)

	first := mustPut(t, s, data)
	afterFirst := mustStat(t, s)

	second := mustPut(t, s, data)
	afterSecond := mustStat(t, s)

	if afterSecond.Chunks != afterFirst.Chunks {
		t.Errorf("storing identical content again added %d chunks", afterSecond.Chunks-afterFirst.Chunks)
	}

	if afterSecond.Bytes != afterFirst.Bytes {
		t.Errorf("storing identical content again cost %d bytes", afterSecond.Bytes-afterFirst.Bytes)
	}

	if len(first.Chunks) != len(second.Chunks) {
		t.Fatalf("same content gave %d then %d chunks", len(first.Chunks), len(second.Chunks))
	}

	for i := range first.Chunks {
		if first.Chunks[i] != second.Chunks[i] {
			t.Fatalf("chunk %d differs between two puts of identical content", i)
		}
	}
}

// TestStoreIncrementalCost is the economic claim from the README, measured: a
// small edit to a large file must cost roughly the size of the edit, not the
// size of the file. This is what makes snapshotting before every change
// affordable enough to actually do.
func TestStoreIncrementalCost(t *testing.T) {
	s := newTestStore(t)
	original := deterministicBytes(t, 32<<20, 43)

	mustPut(t, s, original)
	before := mustStat(t, s)

	// A 500-byte edit in the middle, of the kind a database dump sees between
	// two snapshots of a site nobody has touched much.
	modified := bytes.Clone(original)
	copy(modified[len(modified)/2:], deterministicBytes(t, 500, 47))

	mustPut(t, s, modified)
	after := mustStat(t, s)

	growth := after.Bytes - before.Bytes
	if limit := before.Bytes / 20; growth > limit {
		t.Errorf("a 500-byte edit to a %d-byte blob grew the store by %d bytes, more than the %d (5%%) allowed", len(original), growth, limit)
	}

	t.Logf("editing 500 bytes of %d added %d bytes and %d chunks to the store", len(original), growth, after.Chunks-before.Chunks)
}

// TestStoreVerifiesOnRead is the bit-rot guarantee: a restore must fail loudly
// rather than hand back bytes that are not what was stored.
func TestStoreVerifiesOnRead(t *testing.T) {
	s := newTestStore(t)
	data := deterministicBytes(t, 2<<20, 53)

	blob := mustPut(t, s, data)

	// Corrupt the first chunk on disk, as a failing drive would.
	path := s.path(blob.Chunks[0].Digest)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	body[len(body)-1] ^= 0xFF
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write chunk: %v", err)
	}

	if _, err := s.ReadAll(t.Context(), blob); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("reading a corrupted chunk gave %v, want ErrCorrupt", err)
	}
}

func TestStoreReportsMissingChunk(t *testing.T) {
	s := newTestStore(t)
	blob := mustPut(t, s, deterministicBytes(t, 2<<20, 59))

	if err := os.Remove(s.path(blob.Chunks[0].Digest)); err != nil {
		t.Fatalf("remove chunk: %v", err)
	}

	if _, err := s.ReadAll(t.Context(), blob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading a missing chunk gave %v, want ErrNotFound", err)
	}
}

// TestStoreCompressionChoice checks both sides of the heuristic: text-like data
// is worth compressing, already-compressed media is not.
func TestStoreCompressionChoice(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want byte
	}{
		{"compressible", bytes.Repeat([]byte("wp_options wp_postmeta "), 40000), encodingZstd},
		{"incompressible", nil, encodingRaw}, // filled below; stands in for a JPEG
	}
	tests[1].data = deterministicBytes(t, 900000, 61)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)

			blob := mustPut(t, s, tt.data)

			body, err := os.ReadFile(s.path(blob.Chunks[0].Digest))
			if err != nil {
				t.Fatalf("read chunk: %v", err)
			}

			if body[0] != tt.want {
				t.Errorf("chunk stored with encoding %d, want %d", body[0], tt.want)
			}
		})
	}
}

// TestStoreConcurrentPut covers two goroutines publishing the same chunk at the
// same time. Both rename onto one path; the content is identical by
// construction, so both must succeed and the store must hold one copy.
func TestStoreConcurrentPut(t *testing.T) {
	s := newTestStore(t)
	data := deterministicBytes(t, 8<<20, 67)

	const writers = 8
	blobs := make([]Blob, writers)
	errs := make([]error, writers)

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			blobs[i], errs[i] = s.Put(t.Context(), bytes.NewReader(data))
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	for i := 1; i < writers; i++ {
		if len(blobs[i].Chunks) != len(blobs[0].Chunks) {
			t.Fatalf("writer %d produced %d chunks, writer 0 produced %d", i, len(blobs[i].Chunks), len(blobs[0].Chunks))
		}
	}

	// One copy of each chunk, and nothing left behind in tmp.
	unique := make(map[Digest]bool)
	for _, c := range blobs[0].Chunks {
		unique[c.Digest] = true
	}

	if got := mustStat(t, s); got.Chunks != len(unique) {
		t.Errorf("store holds %d chunks, want %d", got.Chunks, len(unique))
	}

	leftovers, err := os.ReadDir(filepath.Join(s.root, tempDir))
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("%d temp files left behind after successful puts", len(leftovers))
	}

	got, err := s.ReadAll(t.Context(), blobs[0])
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("content stored concurrently does not read back intact")
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"default", DefaultConfig(), true},
		{"zero value", Config{}, false},
		{"negative minimum", Config{MinSize: -1, AvgSize: 2, MaxSize: 3}, false},
		{"average below minimum", Config{MinSize: 100, AvgSize: 50, MaxSize: 200}, false},
		{"average equal to minimum", Config{MinSize: 100, AvgSize: 100, MaxSize: 200}, false},
		{"maximum below average", Config{MinSize: 10, AvgSize: 100, MaxSize: 50}, false},
		{"maximum equal to average", Config{MinSize: 10, AvgSize: 100, MaxSize: 100}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()

			if tt.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tt.ok && err == nil {
				t.Error("Validate() = nil, want an error")
			}
		})
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	if _, err := Open(t.TempDir(), Config{}); err == nil {
		t.Fatal("Open accepted the zero Config")
	}
}

// backdate ages everything under the store so the collector's grace period does
// not spare it. Faking the clock is the only way to test a duration-based guard
// without sleeping for a day.
func backdate(t *testing.T, s *Store, by time.Duration) {
	t.Helper()

	when := time.Now().Add(-by)

	err := filepath.WalkDir(s.root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}

		return os.Chtimes(path, when, when)
	})
	if err != nil {
		t.Fatalf("backdate store: %v", err)
	}
}

func TestGCDeletesUnreachable(t *testing.T) {
	s := newTestStore(t)

	keep := mustPut(t, s, deterministicBytes(t, 4<<20, 71))
	drop := mustPut(t, s, deterministicBytes(t, 4<<20, 73))

	backdate(t, s, 48*time.Hour)

	reachable := make(map[Digest]bool, len(keep.Chunks))
	for _, c := range keep.Chunks {
		reachable[c.Digest] = true
	}

	stats, err := s.GC(t.Context(), func(d Digest) bool { return reachable[d] }, DefaultGCMinAge)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	if stats.Kept != len(reachable) {
		t.Errorf("kept %d chunks, want %d", stats.Kept, len(reachable))
	}
	if stats.Deleted != len(drop.Chunks) {
		t.Errorf("deleted %d chunks, want %d", stats.Deleted, len(drop.Chunks))
	}
	if stats.Freed <= 0 {
		t.Error("freed no bytes despite deleting chunks")
	}

	// The surviving state must still restore.
	if _, err := s.ReadAll(t.Context(), keep); err != nil {
		t.Fatalf("reachable blob no longer readable after GC: %v", err)
	}

	// The collected one must be gone rather than merely unreferenced.
	if _, err := s.ReadAll(t.Context(), drop); !errors.Is(err, ErrNotFound) {
		t.Errorf("collected blob still readable, got %v", err)
	}
}

// TestGCSparesYoungChunks covers the race the grace period exists for: a
// snapshot has written its chunks but has not yet recorded the state that refers
// to them, so they look like garbage and must not be taken.
func TestGCSparesYoungChunks(t *testing.T) {
	s := newTestStore(t)

	blob := mustPut(t, s, deterministicBytes(t, 4<<20, 79))

	// Nothing is reachable -- exactly what an in-flight snapshot looks like.
	stats, err := s.GC(t.Context(), func(Digest) bool { return false }, DefaultGCMinAge)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	if stats.Deleted != 0 {
		t.Errorf("deleted %d chunks that were younger than the grace period", stats.Deleted)
	}
	if stats.Spared != len(blob.Chunks) {
		t.Errorf("spared %d chunks, want %d", stats.Spared, len(blob.Chunks))
	}

	if _, err := s.ReadAll(t.Context(), blob); err != nil {
		t.Fatalf("in-flight blob was damaged by GC: %v", err)
	}
}

func TestGCRejectsNonPositiveMinAge(t *testing.T) {
	s := newTestStore(t)

	for _, minAge := range []time.Duration{0, -time.Hour} {
		if _, err := s.GC(t.Context(), func(Digest) bool { return false }, minAge); err == nil {
			t.Errorf("GC accepted minAge %v", minAge)
		}
	}
}

func TestGCRemovesAbandonedTempFiles(t *testing.T) {
	s := newTestStore(t)

	debris := filepath.Join(s.root, tempDir, "chunk-interrupted")
	if err := os.WriteFile(debris, []byte("half a chunk"), 0o600); err != nil {
		t.Fatalf("write debris: %v", err)
	}

	backdate(t, s, 48*time.Hour)

	stats, err := s.GC(t.Context(), func(Digest) bool { return true }, DefaultGCMinAge)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}

	if stats.TempFiles != 1 {
		t.Errorf("removed %d temp files, want 1", stats.TempFiles)
	}

	if _, err := os.Stat(debris); !errors.Is(err, fs.ErrNotExist) {
		t.Error("abandoned temp file survived collection")
	}
}

// TestGCLeavesForeignFilesAlone: a store root is someone's disk, and files this
// package did not write are not its business.
func TestGCLeavesForeignFilesAlone(t *testing.T) {
	s := newTestStore(t)

	mustPut(t, s, deterministicBytes(t, 1<<20, 83))
	backdate(t, s, 48*time.Hour)

	foreign := filepath.Join(s.root, chunkDir, "README-from-the-sysadmin")
	if err := os.WriteFile(foreign, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	if _, err := s.GC(t.Context(), func(Digest) bool { return false }, DefaultGCMinAge); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("collector removed a file it did not write: %v", err)
	}
}

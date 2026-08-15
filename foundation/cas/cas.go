// Package cas is the content-addressed store: the reason fifty snapshots of a
// twenty-gigabyte site cost twenty gigabytes and the differences between them,
// rather than a terabyte.
//
// A stream written here is split at content-defined boundaries, and each piece
// is stored once under the SHA-256 of its bytes. Writing the same content again
// finds the chunk already present and writes nothing. That is the whole idea; the
// rest of this package is the care needed to make it safe -- atomic publication,
// verification on read, and a collector that cannot race a concurrent write.
//
// The package knows nothing about WordPress, or about what a state is. It moves
// opaque bytes in and out and reclaims what nothing points at, and the caller
// supplies the meaning. That separation is deliberate: it is what makes this
// testable without a database, a web server or a WordPress installation
// anywhere in sight.
package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// ErrNotFound is returned when a digest names a chunk the store does not hold.
var ErrNotFound = errors.New("cas: chunk not found")

// ErrCorrupt is returned when a chunk's bytes do not hash to the digest they
// were stored under -- bit rot, a truncated write, or a tampered store.
var ErrCorrupt = errors.New("cas: chunk failed verification")

// Directory names under the store root.
const (
	chunkDir = "chunks"
	tempDir  = "tmp"
)

// Chunk framing. Every stored chunk begins with one byte saying how the rest is
// encoded, so the store can hold compressed and uncompressed chunks side by side
// without a separate index to say which is which.
const (
	encodingRaw  byte = 0
	encodingZstd byte = 1
)

// compressionThreshold is how much smaller zstd must make a chunk for the
// compressed form to be kept: 31/32, so anything saving less than about three
// percent is stored raw.
//
// A WordPress uploads directory is mostly JPEG, PNG, WebP and PDF -- formats
// that are already compressed and will not shrink again. Storing those raw costs
// a few percent of disk and removes a decompression pass from every read of
// them, which is the trade worth making when reads are how a site gets restored.
const compressionThreshold = 31

// Config sets the chunker's bounds. The zero value is not usable; use
// DefaultConfig and adjust.
//
// These sizes are burned into every store they write. Changing them does not
// corrupt anything -- old chunks stay readable, they are addressed by content --
// but content chunked under one configuration will not deduplicate against the
// same content chunked under another, so the first snapshot after a change costs
// a full copy.
type Config struct {
	MinSize int
	AvgSize int
	MaxSize int

	// SkipSync trades durability for speed by not flushing each chunk to disk
	// before publishing it. A crash can then lose chunks that a state claims to
	// contain, which is a corrupt backup -- so this is for tests, not for a
	// store anyone intends to restore from.
	SkipSync bool
}

// DefaultConfig returns the chunk sizes described on the Default* constants.
func DefaultConfig() Config {
	return Config{
		MinSize: DefaultMinSize,
		AvgSize: DefaultAvgSize,
		MaxSize: DefaultMaxSize,
	}
}

// Validate reports whether the sizes are usable.
func (c Config) Validate() error {
	switch {
	case c.MinSize <= 0:
		return errors.New("cas: MinSize must be positive")
	case c.AvgSize <= c.MinSize:
		return errors.New("cas: AvgSize must exceed MinSize")
	case c.MaxSize < c.AvgSize:
		return errors.New("cas: MaxSize must be at least AvgSize")
	}

	return nil
}

// Chunk is one piece of a stream: where to find it, and how long it is in
// plaintext.
type Chunk struct {
	Digest Digest
	Size   int
}

// Blob is what a stream was split into -- the recipe for reassembling it. It
// holds no data, only digests, so it is small enough to keep in a manifest and
// cheap enough to compare against another.
type Blob struct {
	Size   int64
	Chunks []Chunk
}

// Store is a content-addressed chunk store rooted at a directory.
//
// Safe for concurrent use. Two goroutines storing identical content race to
// publish the same path, and both win: the content is identical by construction,
// and the publish is a rename.
type Store struct {
	root string
	cfg  Config
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// Open prepares a store at root, creating the directory layout if absent.
func Open(root string, cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	for _, dir := range []string{chunkDir, tempDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create store layout: %w", err)
		}
	}

	// EncodeAll and DecodeAll are safe for concurrent use, and chunks are small
	// enough to hold whole, so one encoder and one decoder serve every caller.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("zstd encoder: %w", err)
	}

	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		enc.Close()
		return nil, fmt.Errorf("zstd decoder: %w", err)
	}

	return &Store{root: root, cfg: cfg, enc: enc, dec: dec}, nil
}

// Close releases the compressor resources. The store is unusable afterwards.
func (s *Store) Close() error {
	s.dec.Close()

	return s.enc.Close()
}

// path is where the chunk named by d lives.
//
// One level of fan-out on the first byte, so a store of a few hundred thousand
// chunks is a few hundred directories of a few hundred entries rather than one
// directory that makes `ls` unusable and directory lookup linear.
func (s *Store) path(d Digest) string {
	h := d.String()

	return filepath.Join(s.root, chunkDir, h[:2], h)
}

// Has reports whether the store already holds the chunk named by d.
func (s *Store) Has(d Digest) bool {
	_, err := os.Stat(s.path(d))

	return err == nil
}

// Put splits r into chunks, stores the ones not already present, and returns the
// recipe for reading the stream back.
func (s *Store) Put(ctx context.Context, r io.Reader) (Blob, error) {
	c := newChunker(r, s.cfg)

	var blob Blob
	for {
		if err := ctx.Err(); err != nil {
			return Blob{}, err
		}

		chunk, err := c.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Blob{}, fmt.Errorf("read stream: %w", err)
		}

		d := SumDigest(chunk)
		if err := s.putChunk(d, chunk); err != nil {
			return Blob{}, err
		}

		blob.Chunks = append(blob.Chunks, Chunk{Digest: d, Size: len(chunk)})
		blob.Size += int64(len(chunk))
	}

	return blob, nil
}

// putChunk writes one chunk, unless the store already holds it.
func (s *Store) putChunk(d Digest, plain []byte) error {
	// The dedup fast path, and the reason a re-snapshot of an unchanged site
	// costs almost nothing: the content is already here, so there is no work.
	if s.Has(d) {
		return nil
	}

	body := append([]byte{encodingRaw}, plain...)
	if packed := s.enc.EncodeAll(plain, nil); len(packed)*32 < len(plain)*compressionThreshold {
		body = append([]byte{encodingZstd}, packed...)
	}

	return s.publish(s.path(d), body)
}

// publish writes body to dst atomically: a reader either sees the whole chunk or
// no chunk at all, never a partial one.
//
// The temp file lives inside the store root rather than in the system temp
// directory, because rename is only atomic within a filesystem and /tmp is
// routinely a different one.
func (s *Store) publish(dst string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create chunk directory: %w", err)
	}

	f, err := os.CreateTemp(filepath.Join(s.root, tempDir), "chunk-*")
	if err != nil {
		return fmt.Errorf("create temp chunk: %w", err)
	}
	defer os.Remove(f.Name()) // no-op once the rename below succeeds

	if _, err := f.Write(body); err != nil {
		f.Close()
		return fmt.Errorf("write temp chunk: %w", err)
	}

	// Flush before publishing. Without this the rename can reach disk while the
	// contents have not, and a crash leaves a chunk that exists, is named after
	// its content, and does not contain it.
	if !s.cfg.SkipSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("sync temp chunk: %w", err)
		}
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp chunk: %w", err)
	}

	if err := os.Rename(f.Name(), dst); err != nil {
		return fmt.Errorf("publish chunk: %w", err)
	}

	return nil
}

// getChunk reads and verifies one chunk.
func (s *Store) getChunk(d Digest) ([]byte, error) {
	body, err := os.ReadFile(s.path(d))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", ErrNotFound, d)
	case err != nil:
		return nil, fmt.Errorf("read chunk %s: %w", d, err)
	case len(body) == 0:
		return nil, fmt.Errorf("%w: %s is empty", ErrCorrupt, d)
	}

	plain := body[1:]
	if body[0] == encodingZstd {
		plain, err = s.dec.DecodeAll(body[1:], nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrCorrupt, d, err)
		}
	}

	// Verify on every read rather than on demand. This is a backup store: the
	// moment a corrupt chunk matters is a restore, and a restore that quietly
	// produces the wrong bytes is worse than one that fails. The cost is a
	// SHA-256 pass, which runs on hardware instructions and is not the
	// bottleneck next to reading the file.
	if sha256.Sum256(plain) != d.b {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, d)
	}

	return plain, nil
}

// Get returns a reader over the reassembled stream described by b.
//
// Chunks are fetched as the reader is drained, so restoring a twenty-gigabyte
// state does not need twenty gigabytes of memory.
func (s *Store) Get(ctx context.Context, b Blob) io.ReadCloser {
	return &blobReader{ctx: ctx, store: s, chunks: b.Chunks}
}

// blobReader streams a blob one chunk at a time.
type blobReader struct {
	ctx    context.Context
	store  *Store
	chunks []Chunk
	cur    []byte
	err    error
}

func (r *blobReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	for len(r.cur) == 0 {
		if err := r.ctx.Err(); err != nil {
			r.err = err
			return 0, err
		}

		if len(r.chunks) == 0 {
			r.err = io.EOF
			return 0, io.EOF
		}

		plain, err := r.store.getChunk(r.chunks[0].Digest)
		if err != nil {
			r.err = err
			return 0, err
		}

		r.cur, r.chunks = plain, r.chunks[1:]
	}

	n := copy(p, r.cur)
	r.cur = r.cur[n:]

	return n, nil
}

func (r *blobReader) Close() error {
	r.cur = nil
	r.chunks = nil

	if r.err == nil {
		r.err = fs.ErrClosed
	}

	return nil
}

// ReadAll is Get drained into memory, for callers that know the blob is small --
// a manifest, a configuration file. Do not use it for an uploads tree.
func (s *Store) ReadAll(ctx context.Context, b Blob) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(int(b.Size))

	rc := s.Get(ctx, b)
	defer rc.Close()

	if _, err := io.Copy(&buf, rc); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Stats describes what a store currently holds on disk.
type Stats struct {
	Chunks int
	Bytes  int64 // as stored: compressed, plus framing
}

// Stat walks the store and totals it.
func (s *Store) Stat() (Stats, error) {
	var st Stats

	err := filepath.WalkDir(filepath.Join(s.root, chunkDir), func(_ string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}

		info, err := e.Info()
		if err != nil {
			return err
		}

		st.Chunks++
		st.Bytes += info.Size()

		return nil
	})
	if err != nil {
		return Stats{}, fmt.Errorf("walk store: %w", err)
	}

	return st, nil
}

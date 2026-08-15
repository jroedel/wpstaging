package cas

import (
	"io"
	"math/bits"
)

// Default chunk sizes.
//
// Smaller than a general-purpose backup tool would choose -- restic averages a
// megabyte -- because the workload here is not general purpose. The database
// dump is one large file that changes a little on every snapshot, and small
// chunks are what let an unchanged table's bytes be recognised on either side of
// an edit. A quarter-megabyte average puts a twenty-gigabyte site at roughly
// eighty thousand chunks, which is a directory tree an ordinary filesystem is
// happy with.
//
// The minimum matters for the opposite reason: WordPress core and its plugins
// are thousands of files of a few kilobytes. Every one of those lands in a
// single chunk regardless, so the floor costs nothing there, and it keeps the
// chunker from cutting a large file into uselessly small pieces.
const (
	DefaultMinSize = 64 << 10  // 64 KiB
	DefaultAvgSize = 256 << 10 // 256 KiB
	DefaultMaxSize = 1 << 20   // 1 MiB
)

// normalization is FastCDC's normalized chunking level: the boundary test is
// made 2^2 times harder before the average size is reached and 2^2 times easier
// after it. Without it, content-defined chunking produces an exponential size
// distribution -- a great many tiny chunks and a long tail of maximum-size ones.
// Pulling the distribution in towards the average is what keeps the chunk count
// predictable.
const normalization = 2

// chunker splits a stream at content-defined boundaries using FastCDC.
//
// Content-defined is the whole point: a fixed-size split would mean inserting
// one byte at the top of a file shifts every subsequent boundary and nothing
// downstream deduplicates. Here the boundary is decided by the bytes around it,
// so an edit disturbs only the chunks it actually touches.
type chunker struct {
	r   io.Reader
	cfg Config

	maskS uint64 // before the average size: harder to satisfy
	maskL uint64 // after it: easier

	buf     []byte
	n       int  // valid bytes in buf
	pending int  // bytes of buf already handed out, dropped on the next call
	eof     bool // r has returned io.EOF
}

func newChunker(r io.Reader, cfg Config) *chunker {
	avgBits := bits.Len(uint(cfg.AvgSize)) - 1

	return &chunker{
		r:   r,
		cfg: cfg,
		// spreadMask draws from a 48-bit span, so neither level can exceed it.
		maskS: spreadMask(min(avgBits+normalization, 48)),
		maskL: spreadMask(max(avgBits-normalization, 1)),
		buf:   make([]byte, cfg.MaxSize),
	}
}

// next returns the next chunk, or io.EOF when the stream is exhausted.
//
// The returned slice aliases the chunker's buffer and is valid only until the
// following call to next. Every caller in this package hashes and compresses it
// immediately, which saves an allocation and a copy per chunk -- at eighty
// thousand chunks a snapshot, that is worth the constraint.
func (c *chunker) next() ([]byte, error) {
	if c.pending > 0 {
		c.n = copy(c.buf, c.buf[c.pending:c.n])
		c.pending = 0
	}

	if err := c.fill(); err != nil {
		return nil, err
	}

	if c.n == 0 {
		return nil, io.EOF
	}

	cut := c.cutpoint(c.buf[:c.n])
	c.pending = cut

	return c.buf[:cut], nil
}

// fill tops the buffer up to capacity, or to whatever the reader has left.
func (c *chunker) fill() error {
	for !c.eof && c.n < len(c.buf) {
		read, err := c.r.Read(c.buf[c.n:])
		c.n += read

		switch {
		case err == io.EOF:
			c.eof = true
		case err != nil:
			return err
		}
	}

	return nil
}

// cutpoint returns the length of the first chunk in data.
func (c *chunker) cutpoint(data []byte) int {
	n := len(data)
	if n <= c.cfg.MinSize {
		return n
	}

	n = min(n, c.cfg.MaxSize)
	normal := min(c.cfg.AvgSize, n)

	// Below MinSize no boundary is even considered, which is what enforces the
	// floor -- the hash is not rolled over those bytes at all.
	var h uint64
	i := c.cfg.MinSize

	for ; i < normal; i++ {
		h = (h << 1) + gear[data[i]]
		if h&c.maskS == 0 {
			return i
		}
	}

	for ; i < n; i++ {
		h = (h << 1) + gear[data[i]]
		if h&c.maskL == 0 {
			return i
		}
	}

	return n
}

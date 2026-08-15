// Package wpdb writes and reads a logical dump of a WordPress database, with
// one property that ordinary dump tools do not offer: **byte-for-byte
// determinism**. Dump an unchanged database twice and the two files are
// identical.
//
// That is not tidiness. The dump is stored in a content-addressed chunk store
// that deduplicates by content, and a file that differs everywhere on every
// snapshot deduplicates against nothing. `mysqldump` writes a header containing
// the time of the dump and does not guarantee the order of the rows it emits,
// so two dumps of a database nobody touched differ near the top and then differ
// again at every table -- and the storage economics that make snapshotting
// cheap disappear. Determinism here is what makes the layer above work.
//
// What that costs, and what it buys, are set out on the constants and options
// below. The short version:
//
//   - Tables are emitted in name order, rows in primary-key order.
//   - No timestamp, server version or host appears anywhere in the output.
//   - AUTO_INCREMENT is stripped from CREATE TABLE, because it moves without
//     the data moving.
//   - Every statement occupies exactly one line, so restoring is a matter of
//     reading lines, and a dump is still valid input to `mysql`.
//
// That last point is deliberate insurance. A backup you can only restore with
// the tool that wrote it is a backup with a single point of failure, so the
// output stays pipe-to-mysql compatible even though nothing here needs it to
// be.
package wpdb

import (
	"errors"
	"fmt"
)

// FormatVersion identifies the dump layout. It appears in the first line of
// every dump so a reader can refuse a file it does not understand rather than
// half-applying it.
const FormatVersion = 1

// DefaultBatchBytes is the size at which a multi-row INSERT is flushed and a
// new one begun.
//
// The trade is between restore speed and how much of a dump a single changed
// row disturbs. Larger statements restore faster; they also mean one edited
// comment rewrites the whole batch it sits in, and the chunk store then has to
// re-store all of it. Half a megabyte is comfortably under the 64 MiB default
// max_allowed_packet, large enough that per-statement overhead vanishes, and
// small enough that an edit dirties about two chunks rather than twenty.
const DefaultBatchBytes = 512 << 10

// ErrUnsupportedFormat is returned by Restore for a dump written by a newer
// version of this package.
var ErrUnsupportedFormat = errors.New("wpdb: unsupported dump format")

// Options controls a dump.
type Options struct {
	// Include, when non-empty, limits the dump to these tables. Exclude drops
	// tables from whatever Include selected. Both match exact table names.
	Include []string
	Exclude []string

	// BatchBytes is the multi-row INSERT flush threshold; zero means
	// DefaultBatchBytes.
	BatchBytes int

	// SkipConsistentSnapshot abandons the point-in-time guarantee. The dump
	// otherwise runs inside a consistent-snapshot transaction, so tables read
	// late show the same instant as tables read early -- without which a
	// snapshot of a live site can capture a comment in one table and miss its
	// metadata in another. Turn this off only where the server will not permit
	// the transaction.
	SkipConsistentSnapshot bool
}

func (o Options) batchBytes() int {
	if o.BatchBytes <= 0 {
		return DefaultBatchBytes
	}

	return o.BatchBytes
}

// TableReport records what happened to one table, including the caveats. The
// caveats are the point: a dump that quietly loses its determinism guarantee on
// three plugin tables is worse than one that says so.
type TableReport struct {
	Name      string
	Engine    string
	Rows      int64
	OrderedBy []string
	Warnings  []string
}

// Report summarises a dump or a restore.
type Report struct {
	Tables       []TableReport
	Rows         int64
	Bytes        int64
	SkippedViews []string
}

// Warnings collects every table-level caveat, prefixed with its table.
func (r Report) Warnings() []string {
	var out []string
	for _, t := range r.Tables {
		for _, w := range t.Warnings {
			out = append(out, fmt.Sprintf("%s: %s", t.Name, w))
		}
	}

	if len(r.SkippedViews) > 0 {
		out = append(out, fmt.Sprintf("skipped %d view(s): %v", len(r.SkippedViews), r.SkippedViews))
	}

	return out
}

// Deterministic reports whether every table in the dump could be ordered
// totally, which is what makes the output reproducible. A false here means the
// storage layer above will see spurious differences between snapshots.
func (r Report) Deterministic() bool {
	for _, t := range r.Tables {
		if len(t.OrderedBy) == 0 {
			return false
		}
	}

	return true
}

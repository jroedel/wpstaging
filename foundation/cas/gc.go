package cas

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DefaultGCMinAge is how long a chunk must have existed before the collector
// will consider deleting it.
//
// This is the fix for the one race that can destroy a store. A snapshot writes
// its chunks and only afterwards records the state that refers to them, so
// between those two moments the chunks are on disk and nothing points at them --
// which is indistinguishable, to a collector, from garbage. Sweeping only what
// is older than the longest plausible snapshot closes the window without any
// locking between the two operations.
//
// A day is far longer than a snapshot of a large site takes, and the cost of
// being wrong in this direction is deferred disk reclamation, while the cost of
// being wrong in the other is a backup that cannot be restored.
const DefaultGCMinAge = 24 * time.Hour

// GCStats reports what a collection did.
type GCStats struct {
	Kept      int
	Deleted   int
	Freed     int64
	Spared    int // unreferenced, but too young to sweep
	TempFiles int // abandoned partial writes removed
}

// GC deletes chunks that nothing points at.
//
// Reachability is the caller's to define: this package has no idea what a state
// is or where the index lives, so it asks. reachable must report true for every
// chunk any live state depends on -- a false negative here deletes data that
// something still needs.
//
// minAge spares chunks newer than the given duration; pass DefaultGCMinAge
// unless you have a specific reason and have read why it exists. A zero or
// negative minAge is rejected rather than treated as "sweep everything", because
// the only way to reach that value by accident is an uninitialised field, and
// obeying it would delete a snapshot that is still being written.
func (s *Store) GC(ctx context.Context, reachable func(Digest) bool, minAge time.Duration) (GCStats, error) {
	if minAge <= 0 {
		return GCStats{}, errors.New("cas: GC minAge must be positive")
	}

	cutoff := time.Now().Add(-minAge)

	stats, err := s.sweepChunks(ctx, reachable, cutoff)
	if err != nil {
		return GCStats{}, err
	}

	// Temp files are the debris of writes interrupted by a crash. They are never
	// reachable -- publication is a rename, so anything still bearing a temp name
	// was never published -- but the age check applies to them too, since a
	// concurrent Put has one open right now.
	stats.TempFiles, err = s.sweepTemp(ctx, cutoff)
	if err != nil {
		return GCStats{}, err
	}

	return stats, nil
}

// sweepChunks walks the chunk tree and removes the unreferenced and unprotected.
func (s *Store) sweepChunks(ctx context.Context, reachable func(Digest) bool, cutoff time.Time) (GCStats, error) {
	var stats GCStats

	root := filepath.Join(s.root, chunkDir)

	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		// A file whose name is not a digest was not written by this package.
		// Leave it alone and say nothing: a store directory is someone's disk,
		// and deleting unrecognised files there is not this collector's business.
		d, err := ParseDigest(e.Name())
		if err != nil {
			return nil
		}

		if reachable(d) {
			stats.Kept++
			return nil
		}

		info, err := e.Info()
		if err != nil {
			// Vanished under us -- a concurrent collector, most likely. Not an
			// error: the outcome we wanted either way is that it is gone.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		if info.ModTime().After(cutoff) {
			stats.Spared++
			return nil
		}

		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove chunk %s: %w", d, err)
		}

		stats.Deleted++
		stats.Freed += info.Size()

		return nil
	})
	if err != nil {
		return GCStats{}, fmt.Errorf("sweep chunks: %w", err)
	}

	return stats, nil
}

// sweepTemp removes abandoned partial writes.
func (s *Store) sweepTemp(ctx context.Context, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, tempDir))
	if err != nil {
		return 0, fmt.Errorf("read temp directory: %w", err)
	}

	var removed int
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}

		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return removed, err
		}

		if info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}

		if err := os.Remove(filepath.Join(s.root, tempDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("remove temp file: %w", err)
		}

		removed++
	}

	return removed, nil
}

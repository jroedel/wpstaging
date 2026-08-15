package wpdb

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// RestoreOptions controls a restore.
type RestoreOptions struct {
	// FromPrefix and ToPrefix rename tables on the way in, which is how one
	// state is deployed into a release that owns its own set of tables:
	// wp_posts becomes wpstg_a1b2c3_posts, the release's wp-config.php names
	// the same prefix, and the two never collide with the live site's.
	//
	// Only table identifiers are rewritten. WordPress also embeds its prefix in
	// *data* -- wp_user_roles in the options table, wp_capabilities and
	// wp_user_level in user metadata -- and those rows decide whether anyone
	// can still log in. They are deliberately not touched here: this package
	// moves bytes and does not know what WordPress means by any of them. The
	// rewrite layer above handles them, and a restore with a prefix change is
	// not complete until it has.
	FromPrefix string
	ToPrefix   string
}

func (o RestoreOptions) rewriting() bool {
	return o.FromPrefix != "" && o.FromPrefix != o.ToPrefix
}

// RestoreReport summarises what a restore applied.
type RestoreReport struct {
	Statements int
	RowsMoved  int64
	Bytes      int64
	Tables     []string
}

// Restore applies a dump to db.
//
// The format puts one statement on each line, so this reads lines. That is the
// whole parser: no quote tracking, no delimiter state machine, and no way for a
// string containing a semicolon to be split in the middle. The dump writer
// guarantees the invariant by escaping every newline inside a value.
func Restore(ctx context.Context, db *sql.DB, r io.Reader, opts RestoreOptions) (RestoreReport, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return RestoreReport{}, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	br := bufio.NewReaderSize(r, 1<<20)

	header, err := br.ReadString('\n')
	if err != nil {
		return RestoreReport{}, fmt.Errorf("read dump header: %w", err)
	}
	if err := checkHeader(header); err != nil {
		return RestoreReport{}, err
	}

	var report RestoreReport
	report.Bytes = int64(len(header))

	for {
		line, err := br.ReadString('\n')
		report.Bytes += int64(len(line))

		stmt := strings.TrimSpace(line)
		switch {
		case stmt == "", strings.HasPrefix(stmt, "--"):
			// Blank or comment; nothing to run.
		default:
			stmt = strings.TrimSuffix(stmt, ";")

			if opts.rewriting() {
				var renamed string
				stmt, renamed = rewriteTablePrefix(stmt, opts.FromPrefix, opts.ToPrefix)
				if renamed != "" && !slices.Contains(report.Tables, renamed) {
					report.Tables = append(report.Tables, renamed)
				}
			}

			res, execErr := conn.ExecContext(ctx, stmt)
			if execErr != nil {
				return report, fmt.Errorf("statement %d (%s): %w", report.Statements+1, truncate(stmt, 120), execErr)
			}

			report.Statements++
			if n, err := res.RowsAffected(); err == nil {
				report.RowsMoved += n
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return report, fmt.Errorf("read dump: %w", err)
		}
	}

	return report, nil
}

// checkHeader refuses a dump this package does not understand, rather than
// applying the part it recognises and stopping somewhere in the middle.
func checkHeader(line string) error {
	const marker = "-- wpstaging dump format "

	rest, ok := strings.CutPrefix(strings.TrimSpace(line), marker)
	if !ok {
		return fmt.Errorf("%w: not a wpstaging dump", ErrUnsupportedFormat)
	}

	v, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return fmt.Errorf("%w: unreadable version %q", ErrUnsupportedFormat, rest)
	}

	if v > FormatVersion {
		return fmt.Errorf("%w: dump is format %d, this build understands %d", ErrUnsupportedFormat, v, FormatVersion)
	}

	return nil
}

// rewriteTablePrefix renames the table a statement operates on, returning the
// new statement and the new table name (empty if nothing was renamed).
//
// It rewrites the first backtick-quoted identifier, which for every statement
// this package emits -- DROP TABLE, CREATE TABLE, INSERT INTO -- is the table
// being operated on. Identifiers appearing later, in column definitions and key
// lists, are left alone, which is what we want: a column called `wp_id` must
// not be renamed because it happens to share the prefix.
func rewriteTablePrefix(stmt, from, to string) (string, string) {
	start := strings.IndexByte(stmt, '`')
	if start < 0 {
		return stmt, ""
	}

	end := closingBacktick(stmt, start+1)
	if end < 0 {
		return stmt, ""
	}

	ident := strings.ReplaceAll(stmt[start+1:end], "``", "`")

	suffix, ok := strings.CutPrefix(ident, from)
	if !ok {
		return stmt, ""
	}

	renamed := to + suffix

	return stmt[:start] + quoteIdent(renamed) + stmt[end+1:], renamed
}

// closingBacktick finds the backtick that ends an identifier started at i,
// stepping over the doubled backticks that escape one inside a name.
func closingBacktick(s string, i int) int {
	for i < len(s) {
		if s[i] != '`' {
			i++
			continue
		}

		if i+1 < len(s) && s[i+1] == '`' {
			i += 2
			continue
		}

		return i
	}

	return -1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}

package wpdb

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// autoIncrement matches the table option, not the column attribute -- the
// column form has no `=`, so this cannot touch a column definition.
//
// It is stripped because it moves without the data moving. InnoDB advances the
// counter on an insert that is later rolled back, and older versions recomputed
// it on restart, so leaving it in makes the schema of an untouched table differ
// between two snapshots. The cost is that a restored table's counter resumes
// from the highest surviving key rather than from wherever it had reached, so
// ids freed by deletion can be handed out again. For WordPress that is
// harmless: a deleted post's id is not referenced by anything that survives it.
var autoIncrement = regexp.MustCompile(` AUTO_INCREMENT=\d+`)

// preamble is emitted before any table.
//
// SET NAMES binary is what makes the round trip exact. It stops the server
// converting between the connection's character set and the column's in either
// direction, so the bytes written to the dump are the bytes that were stored,
// and the bytes restored are the bytes that were dumped. On a site that has
// been running for ten years -- likely carrying latin1 tables, utf8 tables and
// utf8mb4 tables at once, some of them mislabelled -- any conversion is a
// chance to mangle text that has been fine for a decade.
var preamble = []string{
	"SET NAMES binary;",
	"SET FOREIGN_KEY_CHECKS=0;",
	"SET UNIQUE_CHECKS=0;",
	"SET SQL_MODE='NO_AUTO_VALUE_ON_ZERO';",
}

// countingWriter tracks how much was written, for the report.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)

	return n, err
}

// Dump writes a deterministic logical dump of db to w.
func Dump(ctx context.Context, db *sql.DB, w io.Writer, opts Options) (Report, error) {
	// Everything runs on one connection: the consistent-snapshot transaction
	// and the session settings below are connection state, and the pool would
	// otherwise hand different queries to different connections that never saw
	// them.
	conn, err := db.Conn(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SET SESSION character_set_results = binary"); err != nil {
		return Report{}, fmt.Errorf("set result charset: %w", err)
	}

	if !opts.SkipConsistentSnapshot {
		if _, err := conn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
			return Report{}, fmt.Errorf("start consistent snapshot: %w", err)
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	}

	counter := &countingWriter{w: w}
	out := bufio.NewWriterSize(counter, 1<<20)

	fmt.Fprintf(out, "-- wpstaging dump format %d\n", FormatVersion)
	for _, line := range preamble {
		fmt.Fprintln(out, line)
	}

	tables, views, err := listTables(ctx, conn, opts)
	if err != nil {
		return Report{}, err
	}

	report := Report{SkippedViews: views}

	for _, t := range tables {
		tr, err := dumpTable(ctx, conn, out, t, opts)
		if err != nil {
			return Report{}, fmt.Errorf("dump %s: %w", t.name, err)
		}

		report.Tables = append(report.Tables, tr)
		report.Rows += tr.Rows
	}

	if err := out.Flush(); err != nil {
		return Report{}, fmt.Errorf("flush dump: %w", err)
	}

	report.Bytes = counter.n

	return report, nil
}

type tableInfo struct {
	name   string
	engine string
}

// listTables returns the base tables to dump, in name order, and the views that
// were skipped.
//
// Name order rather than the server's order because information_schema makes no
// promise about the latter, and a dump whose tables move between runs differs
// from top to bottom for no reason.
func listTables(ctx context.Context, conn *sql.Conn, opts Options) ([]tableInfo, []string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT TABLE_NAME, COALESCE(ENGINE, ''), TABLE_TYPE
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE()
		ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()

	var tables []tableInfo
	var views []string

	for rows.Next() {
		var name, engine, kind string
		if err := rows.Scan(&name, &engine, &kind); err != nil {
			return nil, nil, fmt.Errorf("scan table row: %w", err)
		}

		if kind != "BASE TABLE" {
			views = append(views, name)
			continue
		}

		if len(opts.Include) > 0 && !slices.Contains(opts.Include, name) {
			continue
		}
		if slices.Contains(opts.Exclude, name) {
			continue
		}

		tables = append(tables, tableInfo{name: name, engine: engine})
	}

	return tables, views, rows.Err()
}

// orderingColumns returns the columns to sort a table's rows by, and any caveat
// about the choice.
//
// A primary key is the answer whenever there is one. Where there is not -- some
// plugin tables have no key at all -- ordering by every column is still totally
// deterministic for the bytes that come out, because two rows that tie are
// identical and so produce the same text whichever way round they land.
func orderingColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, []string, error) {
	pk, err := indexColumns(ctx, conn, table, "PRIMARY")
	if err != nil {
		return nil, nil, err
	}
	if len(pk) > 0 {
		return pk, nil, nil
	}

	cols, err := allColumns(ctx, conn, table)
	if err != nil {
		return nil, nil, err
	}
	if len(cols) == 0 {
		return nil, []string{"no columns found; rows emitted unordered"}, nil
	}

	// The residual risk, stated because it is real: MySQL truncates sort keys
	// at max_sort_length (1 KiB by default), so two rows differing only beyond
	// that point may order either way between runs.
	return cols, []string{"no primary key; ordered by all columns, which is not guaranteed total for long text"}, nil
}

func indexColumns(ctx context.Context, conn *sql.Conn, table, index string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT COLUMN_NAME
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?
		ORDER BY SEQ_IN_INDEX`, table, index)
	if err != nil {
		return nil, fmt.Errorf("read index %s: %w", index, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}

	return cols, rows.Err()
}

func allColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT COLUMN_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}

	return cols, rows.Err()
}

// createStatement returns a normalised CREATE TABLE, on a single line.
//
// Collapsed to one line because the format's whole restore story is "each
// statement is a line". SHOW CREATE TABLE renders its string literals escaped,
// so the only newlines in its output are structural and folding them is safe.
func createStatement(ctx context.Context, conn *sql.Conn, table string) (string, error) {
	var name, create string
	err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+quoteIdent(table)).Scan(&name, &create)
	if err != nil {
		return "", fmt.Errorf("show create table: %w", err)
	}

	create = autoIncrement.ReplaceAllString(create, "")

	var b strings.Builder
	b.Grow(len(create))
	for line := range strings.SplitSeq(create, "\n") {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strings.TrimSpace(line))
	}

	return b.String(), nil
}

// dumpTable writes one table's schema and rows.
func dumpTable(ctx context.Context, conn *sql.Conn, out *bufio.Writer, t tableInfo, opts Options) (TableReport, error) {
	tr := TableReport{Name: t.name, Engine: t.engine}

	// MyISAM predates transactions, so a consistent-snapshot dump does not
	// actually cover it. Saying so is the only honest option short of locking
	// the table, which would stall the live site.
	if !opts.SkipConsistentSnapshot && !strings.EqualFold(t.engine, "InnoDB") && t.engine != "" {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf("engine %s is not transactional; this table is not covered by the consistent snapshot", t.engine))
	}

	create, err := createStatement(ctx, conn, t.name)
	if err != nil {
		return tr, err
	}

	order, warnings, err := orderingColumns(ctx, conn, t.name)
	if err != nil {
		return tr, err
	}
	tr.OrderedBy = order
	tr.Warnings = append(tr.Warnings, warnings...)

	fmt.Fprintf(out, "\n-- table %s\n", quoteIdent(t.name))
	fmt.Fprintf(out, "DROP TABLE IF EXISTS %s;\n", quoteIdent(t.name))
	fmt.Fprintf(out, "%s;\n", create)

	query := "SELECT * FROM " + quoteIdent(t.name)
	if len(order) > 0 {
		quoted := make([]string, len(order))
		for i, c := range order {
			quoted[i] = quoteIdent(c)
		}
		query += " ORDER BY " + strings.Join(quoted, ", ")
	}

	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return tr, fmt.Errorf("select rows: %w", err)
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		return tr, fmt.Errorf("column types: %w", err)
	}

	typeNames := make([]string, len(types))
	for i, ct := range types {
		typeNames[i] = ct.DatabaseTypeName()
	}

	insertPrefix := "INSERT INTO " + quoteIdent(t.name) + " VALUES "

	raw := make([]sql.RawBytes, len(types))
	scan := make([]any, len(types))
	for i := range raw {
		scan[i] = &raw[i]
	}

	var batch strings.Builder
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}

		if _, err := out.WriteString(insertPrefix); err != nil {
			return err
		}
		if _, err := out.WriteString(batch.String()); err != nil {
			return err
		}
		if _, err := out.WriteString(";\n"); err != nil {
			return err
		}

		batch.Reset()

		return nil
	}

	limit := opts.batchBytes()

	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			return tr, fmt.Errorf("scan row: %w", err)
		}

		if batch.Len() > 0 {
			batch.WriteByte(',')
		}

		batch.WriteByte('(')
		for i, v := range raw {
			if i > 0 {
				batch.WriteByte(',')
			}
			batch.WriteString(encodeValue(v, v == nil, typeNames[i]))
		}
		batch.WriteByte(')')

		tr.Rows++

		if batch.Len() >= limit {
			if err := flush(); err != nil {
				return tr, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return tr, fmt.Errorf("iterate rows: %w", err)
	}

	if err := flush(); err != nil {
		return tr, err
	}

	return tr, nil
}

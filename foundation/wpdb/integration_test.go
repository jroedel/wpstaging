//go:build integration

package wpdb

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/jroedel/wpstaging/foundation/cas"
)

// The DSN names a throwaway server. Nothing here should ever be pointed at a
// real site: every test drops and recreates schemas.
const dsnEnv = "WPSTAGING_TEST_DSN"

func testDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s not set; start a throwaway server and export it", dsnEnv)
	}

	return dsn
}

// freshDB creates an empty database and returns a handle to it.
func freshDB(t *testing.T, name string) *sql.DB {
	t.Helper()

	base := testDSN(t)

	admin, err := sql.Open("mysql", base)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()

	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + name,
		"CREATE DATABASE " + name + " DEFAULT CHARACTER SET utf8mb4",
	} {
		if _, err := admin.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// Swap the database out of the DSN path.
	target := base[:strings.LastIndex(base, "/")+1] + name

	db, err := sql.Open("mysql", target)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("ping %s: %v", name, err)
	}

	return db
}

// wordpressish builds a schema with the shapes a real WordPress database has,
// and fills it with the values that break naive dump tools.
func wordpressish(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := t.Context()

	schema := []string{
		`CREATE TABLE wp_posts (
			ID bigint(20) unsigned NOT NULL AUTO_INCREMENT,
			post_title text NOT NULL,
			post_content longtext NOT NULL,
			post_date datetime NOT NULL DEFAULT '0000-00-00 00:00:00',
			comment_count bigint(20) NOT NULL DEFAULT 0,
			PRIMARY KEY (ID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE wp_options (
			option_id bigint(20) unsigned NOT NULL AUTO_INCREMENT,
			option_name varchar(191) NOT NULL DEFAULT '',
			option_value longtext NOT NULL,
			autoload varchar(20) NOT NULL DEFAULT 'yes',
			PRIMARY KEY (option_id),
			UNIQUE KEY option_name (option_name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE wp_comments (
			comment_ID bigint(20) unsigned NOT NULL AUTO_INCREMENT,
			comment_post_ID bigint(20) unsigned NOT NULL DEFAULT 0,
			comment_author tinytext NOT NULL,
			comment_content text NOT NULL,
			comment_approved varchar(20) NOT NULL DEFAULT '1',
			PRIMARY KEY (comment_ID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		// A binary column, and a table with no primary key at all -- both turn
		// up in plugin schemas and both are where dump tools go wrong.
		`CREATE TABLE wp_binary_blobs (
			id int NOT NULL AUTO_INCREMENT,
			payload longblob,
			PRIMARY KEY (id)
		) ENGINE=InnoDB`,

		`CREATE TABLE wp_keyless_log (
			event varchar(64),
			detail text
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}

	for _, s := range schema {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	posts := []struct {
		title, content string
	}{
		{"Hello world", "A first post."},
		{"Schönstatt", "Umlauts: äöüß, and a dove: 🕊"},
		{"Quotes & slashes", `He said "it's fine" and typed C:\Users\x`},
		{"Multi\nline title", "Body with\nembedded\nnewlines\r\nand CRLF"},
		{"Serialized", `a:2:{s:5:"width";i:640;s:6:"height";i:480;}`},
		{"Empty content", ""},
	}

	for i, p := range posts {
		_, err := db.ExecContext(ctx,
			"INSERT INTO wp_posts (ID, post_title, post_content, post_date, comment_count) VALUES (?,?,?,?,?)",
			i+1, p.title, p.content, "2016-04-01 12:00:00", 0)
		if err != nil {
			t.Fatalf("insert post %d: %v", i, err)
		}
	}

	options := [][2]string{
		{"siteurl", "https://example.com"},
		{"home", "https://example.com"},
		{"blogname", "Schönstatt Fathers"},
		{"wp_user_roles", `a:1:{s:13:"administrator";a:2:{s:4:"name";s:13:"Administrator";}}`},
	}
	for i, o := range options {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO wp_options (option_id, option_name, option_value) VALUES (?,?,?)",
			i+1, o[0], o[1]); err != nil {
			t.Fatalf("insert option: %v", err)
		}
	}

	for i := range 40 {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO wp_comments (comment_ID, comment_post_ID, comment_author, comment_content) VALUES (?,?,?,?)",
			i+1, (i%6)+1, fmt.Sprintf("Visitor %d", i), fmt.Sprintf("Comment number %d — with an em dash", i)); err != nil {
			t.Fatalf("insert comment: %v", err)
		}
	}

	// Bytes that are not valid UTF-8 and contain NUL: the hex path.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO wp_binary_blobs (id, payload) VALUES (?,?), (?,?), (?,?)",
		1, []byte{0x00, 0xFF, 0xFE, 0x01}, 2, []byte{}, 3, nil); err != nil {
		t.Fatalf("insert blob: %v", err)
	}

	for i := range 5 {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO wp_keyless_log (event, detail) VALUES (?,?)",
			fmt.Sprintf("event-%d", i), "no primary key here"); err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}
}

func dumpToBytes(t *testing.T, db *sql.DB, opts Options) ([]byte, Report) {
	t.Helper()

	var buf bytes.Buffer
	report, err := Dump(t.Context(), db, &buf, opts)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	return buf.Bytes(), report
}

// TestDumpIsDeterministic is the claim the storage layer is built on.
func TestDumpIsDeterministic(t *testing.T) {
	db := freshDB(t, "wpstg_det")
	wordpressish(t, db)

	first, report := dumpToBytes(t, db, Options{})

	for i := range 5 {
		again, _ := dumpToBytes(t, db, Options{})

		if !bytes.Equal(first, again) {
			t.Fatalf("dump %d differs from the first over an unchanged database at byte %d", i+2, firstDifference(first, again))
		}
	}

	if !report.Deterministic() {
		t.Errorf("report says the dump is not fully deterministic: %v", report.Warnings())
	}

	t.Logf("%d bytes, %d rows, %d tables", report.Bytes, report.Rows, len(report.Tables))
}

// TestDumpIgnoresAutoIncrementDrift covers the specific reason AUTO_INCREMENT is
// stripped: inserting a row and deleting it again leaves the data identical but
// moves the counter, and a dump that recorded it would differ for no reason.
func TestDumpIgnoresAutoIncrementDrift(t *testing.T) {
	db := freshDB(t, "wpstg_autoinc")
	wordpressish(t, db)

	before, _ := dumpToBytes(t, db, Options{})

	for range 20 {
		if _, err := db.ExecContext(t.Context(),
			"INSERT INTO wp_posts (post_title, post_content, post_date) VALUES ('temp','temp','2016-04-01 12:00:00')"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := db.ExecContext(t.Context(), "DELETE FROM wp_posts WHERE post_title = 'temp'"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, _ := dumpToBytes(t, db, Options{})

	if !bytes.Equal(before, after) {
		t.Fatalf("dump changed after inserting and deleting rows, at byte %d:\n%s",
			firstDifference(before, after), contextAround(before, after))
	}
}

// TestDumpRestoreRoundTrip proves fidelity and determinism together: restore a
// dump into an empty database, dump that, and the two files must be identical.
// Any value the encoder mangles shows up here as a difference.
func TestDumpRestoreRoundTrip(t *testing.T) {
	source := freshDB(t, "wpstg_src")
	wordpressish(t, source)

	original, srcReport := dumpToBytes(t, source, Options{})

	target := freshDB(t, "wpstg_dst")

	rr, err := Restore(t.Context(), target, bytes.NewReader(original), RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	roundTripped, dstReport := dumpToBytes(t, target, Options{})

	if !bytes.Equal(original, roundTripped) {
		t.Fatalf("round trip changed the dump at byte %d:\n%s",
			firstDifference(original, roundTripped), contextAround(original, roundTripped))
	}

	if srcReport.Rows != dstReport.Rows {
		t.Errorf("row count changed: %d then %d", srcReport.Rows, dstReport.Rows)
	}

	t.Logf("%d statements applied, %d rows, %d bytes round-tripped exactly", rr.Statements, srcReport.Rows, len(original))
}

// TestRestoreWithPrefixRewrite covers deploying a state into a release that owns
// its own tables.
func TestRestoreWithPrefixRewrite(t *testing.T) {
	source := freshDB(t, "wpstg_pfx_src")
	wordpressish(t, source)

	dump, _ := dumpToBytes(t, source, Options{})

	target := freshDB(t, "wpstg_pfx_dst")

	const newPrefix = "wpstg_a1b2c3_"
	if _, err := Restore(t.Context(), target, bytes.NewReader(dump), RestoreOptions{
		FromPrefix: "wp_",
		ToPrefix:   newPrefix,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rows, err := target.QueryContext(t.Context(),
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}

	if len(names) == 0 {
		t.Fatal("no tables were created")
	}
	for _, n := range names {
		if !strings.HasPrefix(n, newPrefix) {
			t.Errorf("table %q was not renamed to the release prefix", n)
		}
	}

	// The data has to survive the rename, not merely the schema.
	var title string
	if err := target.QueryRowContext(t.Context(),
		"SELECT post_title FROM `"+newPrefix+"posts` WHERE ID = 2").Scan(&title); err != nil {
		t.Fatalf("read renamed table: %v", err)
	}
	if title != "Schönstatt" {
		t.Errorf("post title came back as %q; utf8 did not survive the rename", title)
	}
}

// TestDumpReportsMissingPrimaryKey checks that the caveat is surfaced rather
// than silently accepted -- a table that cannot be ordered totally is a table
// whose dump may differ between runs.
func TestDumpReportsMissingPrimaryKey(t *testing.T) {
	db := freshDB(t, "wpstg_nopk")
	wordpressish(t, db)

	_, report := dumpToBytes(t, db, Options{})

	var found bool
	for _, tr := range report.Tables {
		if tr.Name != "wp_keyless_log" {
			continue
		}
		found = true

		if len(tr.Warnings) == 0 {
			t.Error("keyless table produced no warning")
		}
		if len(tr.OrderedBy) == 0 {
			t.Error("keyless table was not ordered at all")
		}
	}

	if !found {
		t.Fatal("wp_keyless_log missing from the report")
	}
}

func TestRestoreRejectsForeignDump(t *testing.T) {
	db := freshDB(t, "wpstg_reject")

	_, err := Restore(t.Context(), db, strings.NewReader("-- MySQL dump 10.13\nDROP TABLE x;\n"), RestoreOptions{})
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Restore accepted a foreign dump: %v", err)
	}
}

func TestIncludeExclude(t *testing.T) {
	db := freshDB(t, "wpstg_filter")
	wordpressish(t, db)

	_, only := dumpToBytes(t, db, Options{Include: []string{"wp_posts", "wp_options"}})
	if len(only.Tables) != 2 {
		t.Errorf("Include selected %d tables, want 2", len(only.Tables))
	}

	_, without := dumpToBytes(t, db, Options{Exclude: []string{"wp_comments"}})
	for _, tr := range without.Tables {
		if tr.Name == "wp_comments" {
			t.Error("Exclude did not drop wp_comments")
		}
	}
}

// TestDumpDedupesInTheStore is the end-to-end economic claim: a dump of a
// database that gained one comment must cost the store almost nothing. This is
// the whole reason the dump is deterministic, measured against the real store
// rather than argued for.
func TestDumpDedupesInTheStore(t *testing.T) {
	db := freshDB(t, "wpstg_dedup")
	wordpressish(t, db)

	// Bulk the database out so the dump spans many chunks; a dump smaller than
	// one chunk would prove nothing. The bodies are varied rather than repeated
	// text, because prose that compresses eighty to one is not what a real site
	// looks like and would make the ratios below meaningless.
	padPosts(t, db, 6000)

	cfg := cas.DefaultConfig()
	cfg.SkipSync = true

	store, err := cas.Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	first, _ := dumpToBytes(t, db, Options{})
	if _, err := store.Put(t.Context(), bytes.NewReader(first)); err != nil {
		t.Fatalf("store first dump: %v", err)
	}

	before, err := store.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// One new comment, exactly as production would gain between snapshots.
	if _, err := db.ExecContext(t.Context(),
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content) VALUES (1,'Late arrival','One more comment')"); err != nil {
		t.Fatalf("insert comment: %v", err)
	}

	second, _ := dumpToBytes(t, db, Options{})
	if _, err := store.Put(t.Context(), bytes.NewReader(second)); err != nil {
		t.Fatalf("store second dump: %v", err)
	}

	after, err := store.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// The property being measured is that a localised edit dirties a constant
	// number of chunks rather than a number that grows with the dump. Adding a
	// comment shifts every following byte along by the length of one row, and
	// content-defined chunking is what stops that shift propagating: the chunk
	// holding the edit changes, the next one resynchronises, and the remaining
	// tens of megabytes are recognised unchanged.
	newChunks := after.Chunks - before.Chunks
	if newChunks > 4 {
		t.Errorf("one extra comment added %d chunks to a %d-chunk dump; chunk boundaries are not resynchronising",
			newChunks, before.Chunks)
	}

	growth := after.Bytes - before.Bytes
	if limit := before.Bytes * 15 / 100; growth > limit {
		t.Errorf("one extra comment grew the store by %d bytes over the %d allowed (15%% of %d)",
			growth, limit, before.Bytes)
	}

	t.Logf("dump %d bytes in %d chunks, stored in %d bytes; one new comment cost %d bytes and %d chunks (%.1f%%)",
		len(first), before.Chunks, before.Bytes, growth, newChunks,
		float64(growth)/float64(before.Bytes)*100)
}

// padPosts inserts n posts with varied bodies, in batches, so the fixture is
// large without the test taking a minute to build it.
func padPosts(t *testing.T, db *sql.DB, n int) {
	t.Helper()

	words := strings.Fields(`parish chapel shrine covenant pilgrimage founder rosary
		vocation seminary retreat community mission apostolate formation liturgy
		vespers novena procession blessing anniversary jubilee dedication`)

	r := rand.New(rand.NewPCG(97, 31))

	const batch = 500
	for start := 0; start < n; start += batch {
		var (
			sb   strings.Builder
			args []any
		)
		sb.WriteString("INSERT INTO wp_posts (post_title, post_content, post_date) VALUES ")

		for i := start; i < min(start+batch, n); i++ {
			if i > start {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?)")

			var body strings.Builder
			for range 220 {
				body.WriteString(words[r.IntN(len(words))])
				body.WriteByte(' ')
			}

			args = append(args, fmt.Sprintf("Padding post %d", i), body.String(), "2016-04-01 12:00:00")
		}

		if _, err := db.ExecContext(t.Context(), sb.String(), args...); err != nil {
			t.Fatalf("pad posts: %v", err)
		}
	}
}

// firstDifference returns the index of the first differing byte, for error
// messages that point at the problem instead of printing two megabytes.
func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	if len(a) != len(b) {
		return min(len(a), len(b))
	}

	return -1
}

func contextAround(a, b []byte) string {
	i := firstDifference(a, b)
	if i < 0 {
		return "(identical)"
	}

	window := func(s []byte) string {
		lo, hi := max(i-80, 0), min(i+80, len(s))

		return string(s[lo:hi])
	}

	return fmt.Sprintf("  first: ...%s...\n second: ...%s...", window(a), window(b))
}

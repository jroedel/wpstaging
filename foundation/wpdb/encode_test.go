package wpdb

import (
	"strings"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ordinary", "wp_posts", "`wp_posts`"},
		{"empty", "", "``"},
		// Table names come from information_schema rather than from a request,
		// but a ten-year-old site accumulates plugin tables named by people who
		// were not thinking about quoting.
		{"embedded backtick", "we`ird", "`we``ird`"},
		// Two backticks in, four out, wrapped: six in total.
		{"only backticks", "``", strings.Repeat("`", 6)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteIdent(tt.input); got != tt.want {
				t.Errorf("quoteIdent(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestEncodeValueNeverEmitsRawNewline is the invariant the whole restore path
// depends on. The format puts one statement per line and the reader splits on
// newlines, so a value that emitted a literal newline would silently cut a
// statement in half and produce a dump that fails partway through a restore --
// the worst possible time to find out.
func TestEncodeValueNeverEmitsRawNewline(t *testing.T) {
	inputs := [][]byte{
		[]byte("line one\nline two"),
		[]byte("carriage\r\nreturn"),
		[]byte("trailing newline\n"),
		[]byte("\n\n\n"),
		{0x00, 0x0A, 0xFF}, // NUL forces the hex path
		[]byte("post_content with\nlots\nof\nlines"),
		[]byte(`a:1:{s:5:"hello";s:6:"wor\nld";}`), // PHP serialized, as WordPress stores
	}

	for _, in := range inputs {
		got := encodeValue(in, false, "TEXT")

		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("encodeValue(%q) = %q, which contains a raw newline", in, got)
		}
	}
}

func TestEncodeValue(t *testing.T) {
	tests := []struct {
		name   string
		in     []byte
		isNull bool
		typ    string
		want   string
	}{
		{"null", nil, true, "VARCHAR", "NULL"},
		{"null beats everything", []byte("ignored"), true, "INT", "NULL"},
		{"integer unquoted", []byte("42"), false, "BIGINT", "42"},
		{"negative integer", []byte("-1"), false, "INT", "-1"},
		{"decimal keeps its text", []byte("3.140"), false, "DECIMAL", "3.140"},
		{"empty numeric falls through to a string", []byte(""), false, "INT", "''"},
		{"plain text", []byte("hello"), false, "VARCHAR", "'hello'"},
		{"empty text", []byte(""), false, "VARCHAR", "''"},
		{"apostrophe", []byte("it's"), false, "TEXT", `'it\'s'`},
		{"backslash", []byte(`C:\path`), false, "TEXT", `'C:\\path'`},
		{"utf8 is left alone", []byte("Schönstatt"), false, "VARCHAR", "'Schönstatt'"},
		{"emoji survives", []byte("🕊"), false, "VARCHAR", "'🕊'"},
		// Not valid UTF-8, so it becomes hex rather than a quoted string that
		// could not be read back.
		{"invalid utf8 goes hex", []byte{0xFF, 0xFE}, false, "BLOB", "0xfffe"},
		{"nul goes hex", []byte{'a', 0x00, 'b'}, false, "TEXT", "0x610062"},
		// An empty BLOB cannot be 0x'' -- that is not valid MySQL.
		{"empty blob stays quoted", []byte{}, false, "BLOB", "''"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encodeValue(tt.in, tt.isNull, tt.typ); got != tt.want {
				t.Errorf("encodeValue(%q, %v, %s) = %s, want %s", tt.in, tt.isNull, tt.typ, got, tt.want)
			}
		})
	}
}

func TestEncodeValueIsStable(t *testing.T) {
	// Determinism at the smallest scale: the same input must always give the
	// same bytes, or nothing above this can be reproducible.
	in := []byte("a value with 'quotes' and \\ and \n")

	first := encodeValue(in, false, "TEXT")
	for range 100 {
		if got := encodeValue(in, false, "TEXT"); got != first {
			t.Fatalf("encodeValue is not stable: %q then %q", first, got)
		}
	}
}

func TestRewriteTablePrefix(t *testing.T) {
	tests := []struct {
		name        string
		stmt        string
		from, to    string
		want        string
		wantRenamed string
	}{
		{
			"insert",
			"INSERT INTO `wp_posts` VALUES (1,'x')",
			"wp_", "wpstg_a1b2c3_",
			"INSERT INTO `wpstg_a1b2c3_posts` VALUES (1,'x')", "wpstg_a1b2c3_posts",
		},
		{
			"drop",
			"DROP TABLE IF EXISTS `wp_options`",
			"wp_", "wpstg_a1b2c3_",
			"DROP TABLE IF EXISTS `wpstg_a1b2c3_options`", "wpstg_a1b2c3_options",
		},
		{
			// Only the leading identifier moves. A column that happens to share
			// the prefix must survive untouched, or the restored schema stops
			// matching what WordPress queries.
			"create leaves columns alone",
			"CREATE TABLE `wp_posts` ( `wp_id` bigint NOT NULL, `post_title` text )",
			"wp_", "wpstg_a1b2c3_",
			"CREATE TABLE `wpstg_a1b2c3_posts` ( `wp_id` bigint NOT NULL, `post_title` text )",
			"wpstg_a1b2c3_posts",
		},
		{
			"table outside the prefix is untouched",
			"INSERT INTO `other_table` VALUES (1)",
			"wp_", "wpstg_",
			"INSERT INTO `other_table` VALUES (1)", "",
		},
		{
			"statement with no identifier",
			"SET FOREIGN_KEY_CHECKS=0",
			"wp_", "wpstg_",
			"SET FOREIGN_KEY_CHECKS=0", "",
		},
		{
			// Data containing the old prefix must not be rewritten -- only the
			// identifier is in scope, and a post body mentioning wp_posts is
			// just text.
			"prefix inside data is left alone",
			"INSERT INTO `wp_posts` VALUES (1,'see wp_posts for details')",
			"wp_", "wpstg_",
			"INSERT INTO `wpstg_posts` VALUES (1,'see wp_posts for details')", "wpstg_posts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, renamed := rewriteTablePrefix(tt.stmt, tt.from, tt.to)

			if got != tt.want {
				t.Errorf("statement:\n got %s\nwant %s", got, tt.want)
			}
			if renamed != tt.wantRenamed {
				t.Errorf("renamed = %q, want %q", renamed, tt.wantRenamed)
			}
		})
	}
}

func TestCheckHeader(t *testing.T) {
	tests := []struct {
		name string
		line string
		ok   bool
	}{
		{"current", "-- wpstaging dump format 1\n", true},
		{"no trailing newline", "-- wpstaging dump format 1", true},
		{"a future format is refused", "-- wpstaging dump format 99\n", false},
		{"mysqldump output", "-- MySQL dump 10.13  Distrib 8.0.35\n", false},
		{"empty", "", false},
		{"garbage version", "-- wpstaging dump format abc\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkHeader(tt.line)

			if tt.ok && err != nil {
				t.Errorf("checkHeader(%q) = %v, want nil", tt.line, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("checkHeader(%q) = nil, want an error", tt.line)
			}
		})
	}
}

func TestStripAutoIncrement(t *testing.T) {
	// The table option goes; the column attribute, which has no '=', stays.
	in := "CREATE TABLE `wp_posts` (\n  `ID` bigint NOT NULL AUTO_INCREMENT,\n  PRIMARY KEY (`ID`)\n) ENGINE=InnoDB AUTO_INCREMENT=48213 DEFAULT CHARSET=utf8mb4"

	got := autoIncrement.ReplaceAllString(in, "")

	if strings.Contains(got, "AUTO_INCREMENT=") {
		t.Errorf("table option survived: %s", got)
	}
	if !strings.Contains(got, "`ID` bigint NOT NULL AUTO_INCREMENT,") {
		t.Errorf("column attribute was damaged: %s", got)
	}
}

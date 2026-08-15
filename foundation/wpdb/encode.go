package wpdb

import (
	"bytes"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// quoteIdent wraps an identifier in backticks, doubling any it contains.
//
// Table names reach this from information_schema rather than from a user, but
// "the input is trusted" is exactly the reasoning behind most injection, and a
// ten-year-old site accumulates plugin tables named by people who were not
// thinking about quoting.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// isNumericType reports whether a column's values can be written to the dump
// bare, without quotes.
//
// MySQL renders these canonically on the wire -- a DECIMAL comes back as its
// exact decimal text, an INT as digits -- so passing the bytes through
// unchanged is both faithful and stable. Anything not listed here is quoted or
// hex-encoded, which is always safe; the list is an optimisation for
// readability and size, not a correctness boundary.
func isNumericType(t string) bool {
	switch strings.ToUpper(t) {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT",
		"UNSIGNED TINYINT", "UNSIGNED SMALLINT", "UNSIGNED MEDIUMINT",
		"UNSIGNED INT", "UNSIGNED BIGINT",
		"FLOAT", "DOUBLE", "DECIMAL", "YEAR":
		return true
	}

	return false
}

// encodeValue renders one column value as a SQL literal.
//
// The choice between a quoted string and a hex literal is made from the bytes
// themselves rather than from the column's declared type. That is deliberate:
// the MySQL wire protocol does not cleanly distinguish TEXT from BLOB -- both
// are MYSQL_TYPE_BLOB, told apart only by a charset flag -- so type-driven
// encoding depends on a driver detail that has been got wrong by most tools
// that tried it. Bytes are never ambiguous.
//
// Both forms are accepted by MySQL for either kind of column, so this decides
// only how the dump reads, never whether it restores correctly. Text stays
// legible for anyone grepping a dump; anything that is not valid UTF-8, or that
// carries a NUL, becomes hex and cannot corrupt the file it lives in.
func encodeValue(v []byte, isNull bool, columnType string) string {
	if isNull {
		return "NULL"
	}

	if isNumericType(columnType) && len(v) > 0 {
		return string(v)
	}

	if !utf8.Valid(v) || bytes.IndexByte(v, 0) >= 0 {
		// 0x'' is not valid MySQL; the empty case has to stay a quoted string.
		if len(v) == 0 {
			return "''"
		}

		return "0x" + hex.EncodeToString(v)
	}

	return quoteString(v)
}

// quoteString renders bytes as a single-quoted MySQL literal.
//
// Newline and carriage return are escaped, and that is load-bearing rather than
// cosmetic: the dump format puts one statement on each line so that restoring
// is a matter of reading lines. A literal newline inside a string would break
// that, so no value is ever allowed to emit one.
//
// Backslash and quote are escaped because they must be. The rest are left
// alone: MySQL only needs \0, \', \", \b, \n, \r, \t, \Z and \\ handled, and
// escaping beyond that would make two dumps of identical data differ if the
// escaping rules were ever adjusted.
func quoteString(v []byte) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('\'')

	for _, c := range v {
		switch c {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1A:
			// Control-Z ends input on some Windows clients reading a dump from
			// a pipe, and costs nothing to escape.
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}

	b.WriteByte('\'')

	return b.String()
}

package sqlitestore

import "strings"

// occurrenceOrderSQL uses the canonical index expression so SQLite can stream
// candidates in the same order without an application-defined SQL function.
func occurrenceOrderSQL(alias string) string {
	_, after, _ := strings.Cut(schemaSQL, "/* occurrence-key-begin */")
	expression, _, _ := strings.Cut(after, "/* occurrence-key-end */")
	return strings.ReplaceAll(strings.TrimSpace(expression), "metadata", alias+".metadata")
}

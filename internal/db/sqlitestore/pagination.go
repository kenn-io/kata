package sqlitestore

import "fmt"

const creationCursorTimeFormat = "2006-01-02T15:04:05.000000000"

// creationKeySQL compares stored creation instants without losing fractional
// precision. SQLite's date functions round fractions to milliseconds, so only
// the whole-second part goes through strftime. Imports use Go's time.Time text
// form, which can include a numeric offset and zone abbreviation; ordinary
// writes use RFC3339. Neither representation needs a persisted change.
// alias is a query-local identifier supplied by this package, never user input.
func creationKeySQL(alias string) string {
	stored := "replace(" + alias + ".created_at, ' +0000 UTC', 'Z')"
	goOffset := fmt.Sprintf("max(instr(%[1]s, ' +'), instr(%[1]s, ' -'))", stored)
	stamp := fmt.Sprintf(`CASE WHEN %[2]s > 0 THEN replace(substr(%[1]s, 1, %[2]s-1), ' ', 'T') || substr(%[1]s, %[2]s+1, 3) || ':' || substr(%[1]s, %[2]s+4, 2) ELSE %[1]s END`, stored, goOffset)
	zone := fmt.Sprintf("CASE WHEN substr(%[1]s,-1)='Z' THEN 'Z' WHEN substr(%[1]s,-6,1) IN ('+','-') THEN substr(%[1]s,-6) ELSE '' END", stamp)
	return fmt.Sprintf(`(strftime('%%Y-%%m-%%dT%%H:%%M:%%S', substr((%[1]s),1,19) || (%[2]s)) || '.' || CASE WHEN substr((%[1]s),20,1)='.' THEN substr(substr((%[1]s),21,length((%[1]s))-20-length(%[2]s)) || '000000000',1,9) ELSE '000000000' END)`, stamp, zone)
}

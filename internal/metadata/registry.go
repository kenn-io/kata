// Package metadata defines the server-reserved keys inside issues.metadata
// and projects.metadata JSON blobs, plus their value types. Reserved keys
// have semantic load on the daemon side (e.g. scheduled_on participates in
// view filters) so their values are validated. All other keys are accepted
// opaquely — the daemon stores and roundtrips them without inspection so
// consumers can carry their own UI hints without coordinating a release.
//
// Adding a new reserved key:
//  1. Add an entry below.
//  2. (Optional) Add an SQLite expression index in internal/db/schema.sql.
package metadata

// Type describes the expected value type for a reserved metadata key.
type Type int

// Valid Type constants. TypeUnknown is the zero value and must not be used
// for registered keys; all others correspond to the value kinds the
// per-type validators in validate.go know how to check.
const (
	TypeUnknown      Type = iota // zero value — no key should carry this
	TypeDate                     // "YYYY-MM-DD"
	TypeBool                     // true / false
	TypeString                   // free-form string
	TypeChecklist                // array of {id: ULID, text: string, done: bool}
	TypeTimezoneIANA             // IANA timezone string
	TypeSchedule                 // date, local date-time, or UTC instant
)

// Entry describes one server-reserved metadata key.
type Entry struct {
	Type Type
}

// IssueRegistry is the set of server-reserved keys for issues.metadata.
// Keys outside this set are accepted opaquely by Validate.
var IssueRegistry = map[string]Entry{
	"scheduled_on": {Type: TypeSchedule},
	"deadline_on":  {Type: TypeDate},
	"someday":      {Type: TypeBool},
	"checklist":    {Type: TypeChecklist},
	"timezone":     {Type: TypeTimezoneIANA},
}

// ProjectRegistry is the set of server-reserved keys for projects.metadata.
// Keys outside this set are accepted opaquely by Validate.
var ProjectRegistry = map[string]Entry{
	"area": {Type: TypeString},
}

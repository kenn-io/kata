// Package commentref resolves stable comment handles and projects authorized
// comment graphs. Callers must filter inaccessible records before using it.
package commentref

import (
	"errors"
	"strings"

	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/uid"
)

// MinLength is the minimum suffix length for a comment handle.
const MinLength = 6

// ErrInvalidRef reports malformed or incomplete comment reference syntax.
var ErrInvalidRef = errors.New("invalid comment reference")

// Ref is a parsed full UID or optionally qualified comment suffix.
type Ref struct {
	Project  string
	IssueRef string
	Suffix   string
	UID      string
}

// Parse accepts full UIDs, c:<suffix>, <issue>:<suffix> and
// <project>#<issue>:<suffix>. Only UID and suffix/issue case is normalized.
func Parse(input string) (Ref, error) {
	if uid.Valid(strings.ToUpper(input)) {
		return Ref{UID: strings.ToUpper(input)}, nil
	}
	left, suffix, ok := strings.Cut(input, ":")
	suffix = strings.ToLower(suffix)
	if !ok || len(suffix) < MinLength || !shortid.Valid(suffix) {
		return Ref{}, ErrInvalidRef
	}
	if strings.EqualFold(left, "c") {
		return Ref{Suffix: suffix}, nil
	}
	issue, err := shortid.Parse(strings.ToLower(left))
	if err != nil {
		return Ref{}, ErrInvalidRef
	}
	// Project names retain their case, unlike the issue short ID.
	project := ""
	if pos := strings.LastIndex(left, "#"); pos >= 0 {
		project = left[:pos]
	}
	if issue.ShortID == "c" || issue.ULID != "" {
		return Ref{}, ErrInvalidRef
	}
	return Ref{Project: project, IssueRef: issue.ShortID, Suffix: suffix}, nil
}

// Handles finds each UID's shortest unique project suffix. Repeated copies of
// the same UID do not create an artificial collision.
func Handles(uids []string) map[string]string {
	counts := make(map[string]int)
	unique := make(map[string]struct{}, len(uids))
	for _, id := range uids {
		unique[strings.ToUpper(id)] = struct{}{}
	}
	for id := range unique {
		for n := MinLength; n <= shortid.MaxLength; n++ {
			if suffix, err := shortid.Derive(id, n); err == nil {
				counts[suffix]++
			}
		}
	}
	result := make(map[string]string, len(uids))
	for _, id := range uids {
		for n := MinLength; n <= shortid.MaxLength; n++ {
			suffix, err := shortid.Derive(strings.ToUpper(id), n)
			if err == nil && counts[suffix] == 1 {
				result[id] = suffix
				break
			}
		}
	}
	return result
}

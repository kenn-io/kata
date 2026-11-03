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
	normalizedUID, isUID := canonicalUID(input)
	if isUID {
		return Ref{UID: normalizedUID}, nil
	}
	colon := strings.LastIndexByte(input, ':')
	if colon < 0 {
		return Ref{}, ErrInvalidRef
	}
	left, suffix := input[:colon], strings.ToLower(input[colon+1:])
	if len(suffix) < MinLength || !shortid.Valid(suffix) {
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

func canonicalUID(value string) (string, bool) {
	canonical := uid.Canonical(value)
	if !uid.Valid(canonical) {
		return "", false
	}
	return canonical, true
}

func equalUID(left, right string) bool {
	canonicalLeft, leftValid := canonicalUID(left)
	canonicalRight, rightValid := canonicalUID(right)
	return leftValid && rightValid && canonicalLeft == canonicalRight
}

func identityUID(value string) string {
	if canonical, valid := canonicalUID(value); valid {
		return canonical
	}
	return value
}

// Handles finds each UID's shortest unique project suffix. Repeated copies of
// the same UID do not create an artificial collision.
func Handles(uids []string) map[string]string {
	counts := make(map[string]int)
	unique := make(map[string]struct{}, len(uids))
	for _, id := range uids {
		canonical, ok := canonicalUID(id)
		if ok {
			unique[canonical] = struct{}{}
		}
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
		canonical, ok := canonicalUID(id)
		if !ok {
			continue
		}
		for n := MinLength; n <= shortid.MaxLength; n++ {
			suffix, err := shortid.Derive(canonical, n)
			if err == nil && counts[suffix] == 1 {
				result[id] = suffix
				break
			}
		}
	}
	return result
}

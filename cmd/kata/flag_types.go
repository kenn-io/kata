package main

import (
	"strings"
)

const issueRefHelp = "ref: short_id (abc4), qualified short_id (kata#abc4), or full ULID"

// refSliceValue is a pflag.Value implementation backing a []string of
// issue refs. It mirrors pflag's StringSlice behavior (comma-split,
// append on each --flag call) but reports its Type() as "ref" instead
// of "strings", so `--help` reads `--blocks ref` not `--blocks strings`.
//
// Issue refs are short_id values, qualified short_id values, or full ULIDs.
// None of those forms can contain a comma, so the comma-split behavior is safe.
type refSliceValue struct {
	target *[]string
	// changed records whether Set has been called at least once.
	// pflag uses this to distinguish "user passed --flag once" (replace
	// the default) from "user passed --flag again" (append). We
	// replicate the semantics so a user passing `--blocks 5` doesn't
	// accidentally inherit a stale default.
	changed bool
}

func newRefSliceValue(target *[]string) *refSliceValue {
	return &refSliceValue{target: target}
}

func (v *refSliceValue) String() string {
	if v == nil || v.target == nil {
		return ""
	}
	return strings.Join(*v.target, ",")
}

func (v *refSliceValue) Set(s string) error {
	parts := splitRefList(s)
	if !v.changed {
		*v.target = append((*v.target)[:0], parts...)
		v.changed = true
	} else {
		*v.target = append(*v.target, parts...)
	}
	return nil
}

func (v *refSliceValue) Type() string { return "ref" }

// splitRefList splits a comma-separated ref list, trimming surrounding
// whitespace on each entry. Empty entries are PRESERVED so the
// downstream resolver can reject them with the standard
// "<flag> must not be empty" error — silently dropping them would
// let `--blocks ,` succeed with no link applied, and a mixed
// `kata edit 1 --title T --blocks ,` would land the title change
// while quietly discarding the malformed relationship operand.
func splitRefList(s string) []string {
	if s == "" {
		// Preserve the empty literal so the resolver fires its
		// "must not be empty" validation instead of silently
		// accepting nothing.
		return []string{""}
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

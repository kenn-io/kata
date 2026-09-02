package api //nolint:revive // package name "api" is fixed by Plan 1 §4 wire-types layout.

import (
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// ifMatchPresentButEmpty reports whether the request carried an If-Match header
// with no value. Huma's typed binding reads headers via Header.Get, which
// collapses an absent header and a present-but-empty one to "", so the raw
// headers are consulted here to tell them apart. A present-but-empty If-Match
// is a malformed conditional write, not the documented unconditional default.
func ifMatchPresentButEmpty(ctx huma.Context) bool {
	empty := false
	ctx.EachHeader(func(name, value string) {
		if strings.EqualFold(name, "If-Match") && value == "" {
			empty = true
		}
	})
	return empty
}

// Resolve rejects a present-but-empty If-Match header on the issue metadata
// patch so a malformed conditional write is not silently downgraded to an
// unconditional last-write-wins patch. An absent header stays the documented
// unconditional default (see parseOptionalIfMatchRevision).
func (*PatchIssueMetadataRequest) Resolve(ctx huma.Context) []error {
	if ifMatchPresentButEmpty(ctx) {
		return []error{NewError(400, "validation", "If-Match header required", "", nil)}
	}
	return nil
}

// Resolve applies the same present-but-empty If-Match rejection to the project
// metadata patch endpoint.
func (*PatchProjectMetadataRequest) Resolve(ctx huma.Context) []error {
	if ifMatchPresentButEmpty(ctx) {
		return []error{NewError(400, "validation", "If-Match header required", "", nil)}
	}
	return nil
}

// Resolve rejects an empty close revision guard before the action can be
// mistaken for an unconditional close.
func (*CloseActionRequest) Resolve(ctx huma.Context) []error {
	if ifMatchPresentButEmpty(ctx) {
		return []error{NewError(400, "validation", "If-Match header required", "", nil)}
	}
	return nil
}

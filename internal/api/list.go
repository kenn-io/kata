package api //nolint:revive // wire-types package

import (
	"strconv"

	"github.com/danielgtaylor/huma/v2"
)

// Resolve preserves the complete explicit project set and rejects empty sets
// before they can accidentally become an unrestricted global query.
func (in *ListAllIssuesRequest) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	q := u.Query()
	if !q.Has("project_ids") {
		return nil
	}
	if q.Has("project_id") {
		return []error{NewError(400, "validation", "project_id and project_ids are mutually exclusive", "", nil)}
	}
	in.ProjectIDs = make([]int64, 0, len(q["project_ids"]))
	for _, raw := range q["project_ids"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return []error{NewError(400, "validation", "project_ids must contain positive integers", "", nil)}
		}
		in.ProjectIDs = append(in.ProjectIDs, id)
	}
	return nil
}

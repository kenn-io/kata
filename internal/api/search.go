package api //nolint:revive // package name "api" is fixed by the wire-types layout.

import "github.com/danielgtaylor/huma/v2"

// Resolve distinguishes an omitted status from a present empty parameter,
// which Huma otherwise treats as absent during query binding.
func (*SearchRequest) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	if u.Query().Has("status") && u.Query().Get("status") == "" {
		return []error{NewError(400, "validation", "status must be open or closed", "", nil)}
	}
	return nil
}

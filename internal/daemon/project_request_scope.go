package daemon

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/projectscope"
)

func requestProjectScope(ctx context.Context, store db.Storage, header http.Header) ([]string, *api.APIError) {
	values, present := header[http.CanonicalHeaderKey(projectscope.Header)]
	if !present {
		return nil, nil
	}
	if len(values) == 0 {
		return nil, api.NewError(http.StatusBadRequest, "invalid_project_scope", "project scope must not be empty", "", nil)
	}
	var limits []string
	for _, value := range values {
		refs, err := projectscope.ParseHeader(value)
		if err != nil {
			return nil, api.NewError(http.StatusBadRequest, "invalid_project_scope", err.Error(), "", nil)
		}
		layer := make([]string, 0, len(refs))
		for _, ref := range refs {
			projectUID := ref
			if rawID, numeric := strings.CutPrefix(ref, "id:"); numeric {
				id, _ := strconv.ParseInt(rawID, 10, 64)
				if store == nil {
					return nil, api.NewError(http.StatusServiceUnavailable, "unavailable", "project storage unavailable", "", nil)
				}
				project, err := store.ProjectByID(ctx, id)
				if errors.Is(err, db.ErrNotFound) {
					return nil, api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
				}
				if err != nil {
					return nil, api.NewError(http.StatusInternalServerError, "internal", "internal error", "", nil)
				}
				projectUID = project.UID
			}
			if !slices.Contains(layer, projectUID) {
				layer = append(layer, projectUID)
			}
		}
		limits = narrowProjectUIDs(limits, layer)
	}
	return limits, nil
}

// A nil limit means no additional boundary. An empty intersection must remain
// nonnil because nil authorized projects represents unrestricted authority.
func narrowProjectUIDs(projects, limit []string) []string {
	if limit == nil {
		return projects
	}
	if projects == nil {
		return slices.Clone(limit)
	}
	narrowed := make([]string, 0, len(projects))
	for _, project := range projects {
		if slices.Contains(limit, project) {
			narrowed = append(narrowed, project)
		}
	}
	return narrowed
}

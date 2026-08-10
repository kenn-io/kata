package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func registerUILaunchHandler(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "readUILaunchTarget",
		Method:      http.MethodGet,
		Path:        "/api/v1/ui/launch-target",
		Summary:     "Read the safe browser launch target for an active issue",
	}, func(ctx context.Context, in *api.UILaunchTargetRequest) (*api.UILaunchTargetResponse, error) {
		issueUID := strings.ToUpper(strings.TrimSpace(in.IssueUID))
		if !uid.Valid(issueUID) {
			return nil, api.NewError(
				http.StatusBadRequest, "validation",
				"issue_uid must be a full 26-character UID", "", nil,
			)
		}
		issue, err := cfg.DB.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
		if errors.Is(err, db.ErrNotFound) {
			err = api.NewError(http.StatusNotFound, "issue_not_found", "issue not found", "", nil)
		} else if err != nil {
			err = internalAPIError(err)
		}
		if err != nil {
			return nil, concealHostUILaunchNotFound(ctx, err)
		}
		ctx, err = authorizeHostProjectScope(ctx, []int64{issue.ProjectID}, nil, false)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, issue.ProjectID); err != nil {
			return nil, concealHostUILaunchNotFound(ctx, err)
		}

		out := &api.UILaunchTargetResponse{}
		launchURL, ok := safeUILaunchURL(cfg.WebSessions, issue.UID)
		if !ok {
			out.Body.Reason = api.UILaunchTargetBrowserUnavailable
			return out, nil
		}
		out.Body.Available = true
		out.Body.URL = launchURL
		return out, nil
	})
}

func concealHostUILaunchNotFound(ctx context.Context, err error) error {
	if _, mounted := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); !mounted {
		return err
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
	}
	return err
}

func safeUILaunchURL(manager *WebSessionManager, issueUID string) (string, bool) {
	if manager == nil {
		return "", false
	}
	origin, err := url.Parse(manager.Origin())
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" ||
		origin.User != nil || (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" ||
		origin.Fragment != "" || origin.Opaque != "" {
		return "", false
	}
	origin.Path = "/kata"
	origin.RawPath = ""
	query := url.Values{}
	query.Set("issue", issueUID)
	origin.RawQuery = query.Encode()
	origin.Fragment = "direct=1"
	return origin.String(), true
}

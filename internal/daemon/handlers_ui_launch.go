package daemon

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerUILaunchHandler(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "readUILaunchTarget",
		Method:      http.MethodGet,
		Path:        "/api/v1/ui/launch-target",
		Summary:     "Read the safe browser launch target for an active issue",
	}, func(ctx context.Context, in *api.UILaunchTargetRequest) (*api.UILaunchTargetResponse, error) {
		issue, err := resolveIssueByUIDOrPrefix(ctx, cfg.DB, strings.TrimSpace(in.IssueUID), db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		ctx, err = authorizeHostProjectScope(ctx, []int64{issue.ProjectID}, nil, false)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, issue.ProjectID); err != nil {
			return nil, err
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
	return origin.String(), true
}

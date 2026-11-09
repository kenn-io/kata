package daemon

import (
	"net/url"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/twentysync"
)

var trackerBindOptions = map[string]string{
	"github":   "host owner repo title_prefix since",
	"notion":   "data_source_id database status_property assignee_property since complete_group todo_group closed_status open_status title_prefix done_statuses",
	"linear":   "workspace_id team_id project_id since closed_state_id open_state_id title_prefix",
	"plane":    "workspace project_id since closed_state_id open_state_id title_prefix",
	"todoist":  "project_id history_since title_prefix",
	"ticktick": "project_id title_prefix",
	"twenty":   "since closed_status open_status title_prefix open_statuses",
}

func validateTrackerBindOptions(provider string, options map[string]any) error {
	allowed, ok := trackerBindOptions[provider]
	if !ok {
		return tracker.ErrInvalid
	}
	for key, value := range options {
		if !slices.Contains(strings.Fields(allowed), key) {
			return tracker.ErrInvalid
		}
		switch key {
		case "title_prefix":
			if _, ok := value.(bool); !ok {
				return tracker.ErrInvalid
			}
		case "done_statuses", "open_statuses":
			switch list := value.(type) {
			case []string:
				if len(list) > 1024 {
					return tracker.ErrInvalid
				}
			case []any:
				if len(list) > 1024 {
					return tracker.ErrInvalid
				}
				for _, item := range list {
					if _, ok := item.(string); !ok {
						return tracker.ErrInvalid
					}
				}
			default:
				return tracker.ErrInvalid
			}
		default:
			if _, ok := value.(string); !ok {
				return tracker.ErrInvalid
			}
		}
	}
	return nil
}

// trackerBindResource validates a caller's resource against the selected owner's
// immutable authority and provider selectors before any credential is resolved.
// Named Notion binds use an explicit data source; database auto-selection is not
// allowed to guess a source across accounts. Twenty discovers only the selected
// credential's own workspace and must match the requested pinned workspace.
func trackerBindResource(c config.TrackerConnectionConfig, requested string, options map[string]any) (string, error) {
	if err := validateTrackerBindOptions(c.Provider, options); err != nil {
		return "", err
	}
	text := func(key string) string { value, _ := options[key].(string); return strings.TrimSpace(value) }
	resource := ""
	switch c.Provider {
	case "github":
		host := strings.ToLower(text("host"))
		if host == "" {
			host = "github.com"
		}
		owner, repo := strings.ToLower(text("owner")), strings.ToLower(text("repo"))
		parsed, err := url.Parse("https://" + host)
		if err != nil || parsed.Host != host || parsed.User != nil || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || owner == "" || repo == "" || owner == "." || owner == ".." || repo == "." || repo == ".." || strings.ContainsAny(owner+repo, "/\\?#@: \t\r\n") {
			return "", tracker.ErrInvalid
		}
		resource = "github:repository:" + host + "/" + owner + "/" + repo
	case "notion":
		if text("database") != "" || strings.ContainsAny(text("data_source_id"), "/:") {
			return "", tracker.ErrInvalid
		}
		id, err := notionsync.ParseDatabaseLocator(text("data_source_id"))
		if err != nil {
			return "", tracker.ErrInvalid
		}
		resource = "notion:" + id
	case "linear":
		workspace, err := linearsync.CanonicalID(text("workspace_id"))
		if err != nil {
			return "", tracker.ErrInvalid
		}
		team, err := linearsync.CanonicalID(text("team_id"))
		if err != nil {
			return "", tracker.ErrInvalid
		}
		source := linearsync.Config{WorkspaceID: workspace, TeamID: team}
		if text("project_id") != "" {
			source.ProjectID, err = linearsync.CanonicalID(text("project_id"))
			if err != nil {
				return "", tracker.ErrInvalid
			}
		}
		resource = source.SourceKey()
	case "plane":
		if c.Plane == nil || c.Plane.APIOrigin == "" || planesync.ValidateWorkspace(text("workspace")) != nil {
			return "", tracker.ErrInvalid
		}
		id, err := planesync.CanonicalID(text("project_id"))
		if err != nil {
			return "", tracker.ErrInvalid
		}
		resource = (planesync.Config{APIOrigin: c.Plane.APIOrigin, Workspace: text("workspace"), ProjectID: id}).SourceKey()
	case "todoist":
		if c.Todoist == nil || c.Todoist.APIOrigin == "" || todoistsync.ValidateID(text("project_id")) != nil {
			return "", tracker.ErrInvalid
		}
		resource = "todoist:" + c.Todoist.APIOrigin + "/project:" + text("project_id")
	case "ticktick":
		if tickticksync.ValidateID(text("project_id")) != nil {
			return "", tracker.ErrInvalid
		}
		resource = "ticktick:" + text("project_id")
	case "twenty":
		if c.Twenty == nil || c.Twenty.APIOrigin == "" {
			return "", tracker.ErrInvalid
		}
		prefix := "twenty:" + c.Twenty.APIOrigin + "/"
		if !strings.HasPrefix(requested, prefix) {
			return "", tracker.ErrInvalid
		}
		id, err := twentysync.CanonicalID(strings.TrimPrefix(requested, prefix))
		if err != nil {
			return "", tracker.ErrInvalid
		}
		resource = prefix + id
	default:
		return "", tracker.ErrInvalid
	}
	if requested != resource {
		return "", tracker.ErrInvalid
	}
	return resource, nil
}

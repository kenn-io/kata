package tui

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"go.kenn.io/kata/pkg/client/generated"
)

// relatedCronTranscriptURL returns the recorded AgentsView locator of the
// latest close of the run's linked issue. It is related issue context, never
// proof that the session executed the run. Only issue.closed events for the
// subject issue recorded in one of projectUIDs qualify (an issue keeps its
// history when it moves); the greatest event ID with a safe URL wins, and the
// URL is returned byte for byte.
func relatedCronTranscriptURL(enabled bool, projectUIDs []string, issueUID string, events []EventLogEntry) string {
	projectUIDs = slices.DeleteFunc(slices.Clone(projectUIDs), func(uid string) bool { return uid == "" })
	if !enabled || len(projectUIDs) == 0 || issueUID == "" {
		return ""
	}
	best, found := "", int64(0)
	for _, e := range events {
		if e.Type != "issue.closed" || !slices.Contains(projectUIDs, e.ProjectUID) || e.IssueUID != issueUID || best != "" && e.ID <= found {
			continue
		}
		transcript, _ := e.Payload["transcript"].(map[string]any)
		locator, _ := transcript["url"].(string)
		if safeTranscriptURL(locator) {
			best, found = locator, e.ID
		}
	}
	return best
}

// safeTranscriptURL accepts an absolute HTTP(S) URL with a host and no
// credentials, query, fragment, whitespace or control characters.
func safeTranscriptURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "?#\\") || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return false
	}
	return true
}

// cronRelatedEventsLimit is the one optional history page the TUI reads.
const cronRelatedEventsLimit = 1000

// cronRelatedProject names where the related read happens. id and uid are
// the issue's current project; uid may be empty for a moved issue whose new
// project the catalog does not list, and then the daemon's capability answer
// for id supplies it. runUID is the run's own canonical project UID.
type cronRelatedProject struct {
	id          int64
	uid, runUID string
}

// CronRelatedAPI is optional so older and test clients keep the core peek.
type CronRelatedAPI interface {
	CronRelatedTranscript(ctx context.Context, project cronRelatedProject, issueUID string) (string, error)
}

// CronRelatedTranscript reads the daemon's agentsview_enabled capability and,
// only when it is true for the same project, one issue-filtered event page.
// It never follows the page cursor: no safe locator on that page means none.
func (c *Client) CronRelatedTranscript(ctx context.Context, project cronRelatedProject, issueUID string) (string, error) {
	api, err := c.generatedClient()
	if err != nil {
		return "", err
	}
	wire, callErr := api.GetCronCapabilitiesWithResponse(ctx, &generated.GetCronCapabilitiesRequestOptions{PathParams: &generated.GetCronCapabilitiesPath{ProjectID: project.id}})
	if wire == nil {
		return "", callErr
	}
	// Daemons that predate the field omit it, which reads as disabled.
	var capabilities generated.CronCapabilitiesResponseBody
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &capabilities); err != nil {
		return "", err
	}
	if !capabilities.AgentsviewEnabled || capabilities.ProjectUID == "" || project.uid != "" && capabilities.ProjectUID != project.uid {
		return "", nil
	}
	page, err := c.listEventsPage(ctx, project.id, 0, issueUID)
	if err != nil {
		return "", err
	}
	// A daemon that did not filter by this issue cannot vouch for the page.
	if page.IssueUID != issueUID || page.ResetRequired || len(page.Events) > cronRelatedEventsLimit {
		return "", nil
	}
	return relatedCronTranscriptURL(true, []string{capabilities.ProjectUID, project.runUID}, issueUID, page.Events), nil
}

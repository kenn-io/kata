package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

// retainedMoveDiscussion reads the watermark before comment rows, so the move
// transaction can reject changes made while the hub history is checked.
func retainedMoveDiscussion(ctx context.Context, cfg ServerConfig, binding db.FederationBinding, issue db.Issue) (int64, error) {
	former, err := cfg.DB.FormerInstanceUIDs(ctx)
	if err != nil {
		return 0, err
	}
	ctx = db.WithAuthorizedProjects(ctx, nil) // The handler already admitted this issue's current project.
	var after, watermark int64
	risk := map[string]bool{}
	for {
		events, err := cfg.DB.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Types: []string{"issue.created", "issue.snapshot", "issue.moved", "issue.commented", "issue.comment_edited"}, AfterID: after, Limit: 1000})
		if err != nil {
			return 0, err
		}
		for _, event := range events {
			watermark = max(watermark, event.ID)
			if event.ID > binding.RetainedThroughEventID || (event.OriginInstanceUID != cfg.DB.InstanceUID() && !slices.Contains(former, event.OriginInstanceUID)) {
				continue
			}
			payload := db.PayloadMap([]byte(event.Payload))
			if event.Type == "issue.comment_edited" {
				uid, _ := db.StringValue(payload["comment_uid"])
				risk[uid] = true
				continue
			}
			typ := event.Type
			if typ == "issue.moved" {
				typ, event.Payload = "issue.snapshot", string(payload["snapshot"])
			}
			projection := db.FoldEvents([]db.FoldEvent{{UID: event.UID, IssueUID: issue.UID, Type: typ, Actor: event.Actor, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339Nano), Payload: []byte(event.Payload)}})
			for uid := range projection.Comments {
				risk[uid] = true
			}
		}
		if len(events) < 1000 {
			break
		}
		after = events[len(events)-1].ID
	}
	comments, err := cfg.DB.CommentsByIssue(ctx, issue.ID)
	if err != nil {
		return 0, err
	}
	var needed []db.Comment
	for _, comment := range comments {
		if risk[comment.UID] && comment.Author != binding.Actor {
			needed = append(needed, comment)
		}
	}
	if len(needed) == 0 {
		return watermark, nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg, binding)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(strings.Split(cred.Capabilities, ","), "pull") {
		return 0, retainedMoveUnsupported()
	}
	history, err := remote.issueHistory(ctx, cred.HubProjectID, issue.UID)
	if err != nil {
		var status *claimHubStatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
			return 0, retainedMoveUnsupported()
		}
		return 0, api.NewError(http.StatusServiceUnavailable, "federation_offline", "the hub is unreachable or its history is unavailable", "retry the move", nil)
	}
	if !db.FederationMoveHistoryProvesComments(issue.UID, binding.HubProjectUID, history, needed) {
		return 0, retainedMoveUnsupported()
	}
	return watermark, nil
}

func retainedMoveUnsupported() error {
	return api.NewError(http.StatusConflict, "federated_move_unsupported", "this retained task cannot be moved", "create an issue in the target project and link it to this task", nil)
}

func (c *claimHubClient) issueHistory(ctx context.Context, projectID int64, issueUID string) ([]db.FoldEvent, error) {
	if c.transportErr != nil {
		return nil, c.transportErr
	}
	client, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(claimHubDoer{client: c.client}))
	if err != nil {
		return nil, err
	}
	var history []db.FoldEvent
	var cursor int64
	limit := int64(1000)
	for {
		query := &generated.PollFederationProjectEventsQuery{AfterID: &cursor, IssueUID: &issueUID, Limit: &limit}
		response, err := client.PollFederationProjectEventsWithResponse(ctx, &generated.PollFederationProjectEventsRequestOptions{PathParams: &generated.PollFederationProjectEventsPath{ProjectID: projectID}, Query: query}, func(_ context.Context, req *http.Request) error {
			req.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := db.RequireEventFeatures(db.SupportedEventFeatures, response.HTTPResponse.Header.Get(db.RequiredEventFeaturesHeader)); err != nil {
			return nil, err
		}
		var page api.PollEventsBody
		if err := json.Unmarshal(response.Body, &page); err != nil {
			return nil, err
		}
		if page.ResetRequired {
			return nil, errors.New("hub issue history requires reset")
		}
		for _, event := range page.Events {
			if db.EventIssueUID(event.IssueUID) != issueUID {
				return nil, errors.New("hub returned another issue's history")
			}
			history = append(history, db.FoldEvent{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: issueUID, RelatedIssueUID: db.EventIssueUID(event.RelatedIssueUID), Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339Nano), Payload: event.Payload})
		}
		if len(page.Events) == 0 {
			return history, nil
		}
		if page.NextAfterID <= cursor {
			return nil, errors.New("hub issue history cursor did not advance")
		}
		cursor = page.NextAfterID
	}
}

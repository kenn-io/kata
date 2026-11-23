package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

type retainedMoveSnapshotProof struct {
	sourceProjectUID string
	actor            string
	history          []db.FoldEvent
	canonicalize     bool
}

func (p retainedMoveSnapshotProof) prepare(issue db.Issue, payload string) (string, error) {
	if p.canonicalize {
		canonical, err := db.CanonicalizeFederationSnapshotAuthors(db.RemoteEvent{
			Type: "issue.snapshot", Payload: jsontext.Value(payload),
		}, p.actor)
		if err != nil {
			return "", err
		}
		payload = string(canonical.Payload)
	}
	if !db.FederationMoveHistoryProvesSnapshot(issue.UID, p.sourceProjectUID, p.history, payload, p.actor) {
		return payload, db.ErrFederatedMoveUnsupported
	}
	return payload, nil
}

// retainedMoveDiscussion reads the hub's accepted issue history and verifies
// the current task and comments before the move transaction starts.
func retainedMoveDiscussion(ctx context.Context, cfg ServerConfig, binding db.FederationBinding, issue db.Issue) (retainedMoveSnapshotProof, error) {
	payload, err := retainedMoveLocalSnapshot(ctx, cfg, issue)
	if err != nil {
		return retainedMoveSnapshotProof{}, err
	}
	finish, err := beginClaimFederationTransport(ctx, cfg, binding.ProjectID)
	if err != nil {
		return retainedMoveSnapshotProof{}, err
	}
	defer finish()
	current, err := cfg.DB.FederationBindingByProject(ctx, binding.ProjectID)
	if err != nil || !sameRetainedMoveBinding(binding, current) {
		return retainedMoveSnapshotProof{}, retainedMoveUnavailable("the federation binding changed; retry the move")
	}
	remote, cred, err := claimForwardClient(ctx, cfg, current)
	if err != nil {
		return retainedMoveSnapshotProof{}, err
	}
	if !federationCapabilitiesContain(cred.Capabilities, "pull") {
		return retainedMoveSnapshotProof{}, retainedMoveUnavailable("hub history access is unavailable; retry the move")
	}
	history, err := remote.issueHistory(ctx, cred.HubProjectID, issue.UID)
	if err != nil {
		return retainedMoveSnapshotProof{}, retainedMoveUnavailable("hub history is unavailable; retry the move")
	}
	proof := retainedMoveSnapshotProof{
		sourceProjectUID: binding.HubProjectUID, actor: binding.Actor, history: history,
		canonicalize: retainedMoveHistoryHasRecoverySnapshot(history, binding.HubProjectUID, cfg.DB.InstanceUID(), binding.Actor),
	}
	if _, err := proof.prepare(issue, payload); err != nil {
		return retainedMoveSnapshotProof{}, retainedMoveUnsupported()
	}
	return proof, nil
}

func retainedMoveHistoryHasRecoverySnapshot(history []db.FoldEvent, projectUID, originUID, actor string) bool {
	for _, event := range history {
		if event.Type != "issue.snapshot" || event.ProjectUID != projectUID || event.OriginInstanceUID != originUID || event.Actor != actor {
			continue
		}
		var snapshot struct {
			Author string `json:"author"`
		}
		if json.Unmarshal(event.Payload, &snapshot) == nil && strings.TrimSpace(snapshot.Author) == strings.TrimSpace(actor) {
			return true
		}
	}
	return false
}

func retainedMoveLocalSnapshot(ctx context.Context, cfg ServerConfig, issue db.Issue) (string, error) {
	comments, err := cfg.DB.CommentsByIssue(ctx, issue.ID)
	if err != nil {
		return "", err
	}
	document := db.IssueDocumentPayload{UID: issue.UID, Author: issue.Author, Comments: make([]db.IssueDocumentComment, 0, len(comments))}
	for _, comment := range comments {
		value := db.IssueDocumentComment{
			CommentUID: comment.UID, Author: comment.Author, Teammate: comment.Teammate,
			Body: comment.Body, CreatedAt: comment.CreatedAt.UTC().Format(time.RFC3339Nano),
			ReplyToUID: comment.ReplyToUID, ReplyKind: comment.ReplyKind,
		}
		if comment.EditedAt != nil {
			value.EditedAt = comment.EditedAt.UTC().Format(time.RFC3339Nano)
		}
		document.Comments = append(document.Comments, value)
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func sameRetainedMoveBinding(expected, current db.FederationBinding) bool {
	return current.Enabled && current.PushEnabled && current.Role == expected.Role && current.RelayConfig == nil &&
		current.HubURL == expected.HubURL && current.HubProjectID == expected.HubProjectID &&
		current.HubProjectUID == expected.HubProjectUID && current.Actor == expected.Actor
}

func retainedMoveUnavailable(message string) error {
	return api.NewError(http.StatusServiceUnavailable, "federation_offline", message, "retry the move", nil)
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
	limit := int64(1000)
	poll := func(cursor int64, filter *string) (*generated.PollFederationProjectEventsResponse, error) {
		query := &generated.PollFederationProjectEventsQuery{AfterID: &cursor, IssueUID: filter, Limit: &limit}
		response, err := client.PollFederationProjectEventsWithResponse(ctx, &generated.PollFederationProjectEventsRequestOptions{PathParams: &generated.PollFederationProjectEventsPath{ProjectID: projectID}, Query: query}, func(_ context.Context, req *http.Request) error {
			req.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if response == nil || response.StatusCode != http.StatusOK || response.JSON200 == nil {
			if response == nil {
				return nil, errors.New("hub issue history response is missing")
			}
			return nil, fmt.Errorf("hub issue history returned HTTP %d", response.StatusCode)
		}
		if err := db.RequireEventFeatures(db.SupportedEventFeatures, response.HTTPResponse.Header.Get(db.RequiredEventFeaturesHeader)); err != nil {
			return nil, err
		}
		return response.JSON200, nil
	}
	first, err := poll(0, nil)
	if err != nil {
		return nil, err
	}
	if first.ResetRequired {
		return nil, errors.New("hub issue history requires reset")
	}
	through := first.NextAfterID
	if through <= 0 {
		return nil, errors.New("hub issue history has no high-water cursor")
	}
	var history []db.FoldEvent
	var cursor int64
	for cursor < through {
		page, err := poll(cursor, &issueUID)
		if err != nil {
			return nil, err
		}
		if page.ResetRequired {
			return nil, errors.New("hub issue history requires reset")
		}
		for _, event := range page.Events {
			if db.EventIssueUID(event.IssueUID) != issueUID {
				return nil, errors.New("hub returned another issue's history")
			}
			if event.EventID > through {
				continue
			}
			history = append(history, db.FoldEvent{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: issueUID, RelatedIssueUID: db.EventIssueUID(event.RelatedIssueUID), Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HlcPhysicalMs, HLCCounter: event.HlcCounter, CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339Nano), Payload: event.Payload})
		}
		if len(page.Events) == 0 || page.NextAfterID >= through {
			break
		}
		if page.NextAfterID <= cursor {
			return nil, errors.New("hub issue history cursor did not advance")
		}
		cursor = page.NextAfterID
	}
	if len(history) == 0 {
		return nil, errors.New("hub returned no issue history")
	}
	return history, nil
}

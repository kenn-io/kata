package db

import (
	"encoding/json/v2"
	"errors"
	"math"
	"time"
)

// FederationMoveSnapshotAuthors uses the source's effective snapshot when a
// retained task moves, while leaving its local history and rows unchanged.
func FederationMoveSnapshotAuthors(payload string, events []FoldEvent, issueUID, sourceUID string) (string, error) {
	departure, err := json.Marshal(map[string]string{"from_project_uid": sourceUID})
	if err != nil {
		return "", err
	}
	events = append(events, FoldEvent{IssueUID: issueUID, Type: "issue.moved", HLCPhysicalMS: math.MaxInt64, Payload: departure})
	events = FederationMoveBaselineEvents(events)
	if len(events) == 0 {
		return "", errors.New("federation move baseline omitted the departure")
	}
	events = events[:len(events)-1]
	projection := FoldEvents(events)
	var doc IssueDocumentPayload
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return "", err
	}
	if issue := projection.Issues[issueUID]; issue.Author != "" {
		doc.Author = issue.Author
	}
	for i := range doc.Comments {
		if comment, ok := projection.Comments[doc.Comments[i].CommentUID]; ok && comment.Author != "" {
			doc.Comments[i].Author = comment.Author
		}
	}
	for i := range doc.Links {
		link := &doc.Links[i]
		from, to := issueUID, link.ToIssueUID
		if link.Incoming {
			from, to = to, from
		}
		if link.Type == "related" && from > to {
			from, to = to, from
		}
		if state, ok := projection.Links[FoldLinkKey{FromUID: from, ToUID: to, Type: link.Type}]; ok && state.Present && state.Author != "" {
			link.Author = state.Author
		}
	}
	encoded, err := json.Marshal(doc)
	return string(encoded), err
}

// FederationMoveBaselineEvents assembles moved-issue replay from the effective
// source enrollment baseline. Earlier initializers cannot restore an author
// rewritten before enrollment. Only comment records covered by that baseline
// are omitted; moves and other deltas remain. Retained events and feeds do not change.
func FederationMoveBaselineEvents(events []FoldEvent) []FoldEvent {
	type sourceKey struct{ issue, project string }
	departures := map[sourceKey]FoldClock{}
	for _, event := range events {
		if event.Type != "issue.moved" {
			continue
		}
		from, _ := StringValue(PayloadMap(event.Payload)["from_project_uid"])
		key := sourceKey{event.IssueUID, from}
		if clock := clockOf(event); compareClock(clock, departures[key]) > 0 {
			departures[key] = clock
		}
	}
	baselines := map[string]FoldEvent{}
	for _, event := range events {
		if event.Type != "issue.snapshot" {
			continue
		}
		departure, exists := departures[sourceKey{event.IssueUID, event.ProjectUID}]
		clock := clockOf(event)
		if exists && compareClock(clock, departure) < 0 && compareClock(clock, clockOf(baselines[event.IssueUID])) > 0 {
			baselines[event.IssueUID] = event
		}
	}
	if len(baselines) == 0 {
		return events
	}
	comments := federationBaselineComments(events, baselines)
	out := make([]FoldEvent, 0, len(events))
	for _, event := range events {
		baseline, exists := baselines[event.IssueUID]
		if exists && compareClock(clockOf(event), clockOf(baseline)) < 0 {
			switch event.Type {
			case "issue.created", "issue.snapshot", RepeatedImportEventType:
				// The baseline is the issue's whole state, so it covers an
				// earlier creation, snapshot, or whole copy.
				continue
			case "issue.commented", "issue.comment_edited":
				uid, body, version := federationCommentEditVersion(event)
				comment, covered := comments[federationBaselineCommentKey{event.IssueUID, uid}]
				if covered && (event.Type == "issue.commented" || comment.coversEdit(event, body, version)) {
					continue
				}
			}
		}
		out = append(out, event)
	}
	return out
}

type federationBaselineCommentKey struct{ issue, comment string }

type federationBaselineComment struct {
	body         string
	version      time.Time
	coveredClock FoldClock
	hasClock     bool
}

func (c federationBaselineComment) coversEdit(event FoldEvent, body string, version time.Time) bool {
	if c.hasClock {
		return compareClock(clockOf(event), c.coveredClock) <= 0
	}
	return !c.version.IsZero() && !version.IsZero() &&
		(c.version.After(version) || (c.version.Equal(version) && c.body == body))
}

func federationCommentEditVersion(event FoldEvent) (uid, body string, version time.Time) {
	payload := PayloadMap(event.Payload)
	uid, _ = StringValue(payload["comment_uid"])
	body, _ = StringValue(payload["body"])
	edited, _ := StringValue(payload["edited_at"])
	if edited == "" {
		edited = event.CreatedAt
	}
	version, _ = time.Parse(time.RFC3339Nano, edited)
	return uid, body, version
}

func federationBaselineComments(events []FoldEvent, baselines map[string]FoldEvent) map[federationBaselineCommentKey]federationBaselineComment {
	comments := map[federationBaselineCommentKey]federationBaselineComment{}
	for issueUID, baseline := range baselines {
		var payload struct {
			Comments []struct {
				UID      string `json:"comment_uid"`
				Body     string `json:"body"`
				EditedAt string `json:"edited_at"`
			} `json:"comments"`
		}
		_ = json.Unmarshal(baseline.Payload, &payload)
		for _, comment := range payload.Comments {
			version, _ := time.Parse(time.RFC3339Nano, comment.EditedAt)
			comments[federationBaselineCommentKey{issueUID, comment.UID}] = federationBaselineComment{body: comment.Body, version: version}
		}
	}
	for _, event := range events {
		if event.Type != "issue.comment_edited" {
			continue
		}
		uid, body, version := federationCommentEditVersion(event)
		key := federationBaselineCommentKey{event.IssueUID, uid}
		comment, exists := comments[key]
		if exists && !version.IsZero() && version.Equal(comment.version) && body == comment.body &&
			compareClock(clockOf(event), clockOf(baselines[event.IssueUID])) < 0 &&
			(!comment.hasClock || compareClock(clockOf(event), comment.coveredClock) > 0) {
			comment.coveredClock, comment.hasClock = clockOf(event), true
			comments[key] = comment
		}
	}
	return comments
}

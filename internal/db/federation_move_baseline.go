package db

import (
	"encoding/json/v2"
	"time"
)

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
	relocations := federationRelocationBaselines(events)
	if len(baselines) == 0 && len(relocations) == 0 {
		return events
	}
	comments := federationBaselineComments(events, baselines)
	relocated := federationBaselineComments(events, relocations)
	out := make([]FoldEvent, 0, len(events))
	for _, event := range events {
		if baseline, exists := relocations[event.IssueUID]; exists && event.ProjectUID != baseline.ProjectUID &&
			compareClock(clockOf(event), clockOf(baseline)) < 0 {
			switch event.Type {
			case "issue.created", "issue.snapshot", RepeatedImportEventType, "issue.labeled", "issue.unlabeled":
				// The relocating snapshot carries the issue's fields and labels.
				continue
			case "issue.commented", "issue.comment_edited":
				uid, body, version := federationCommentEditVersion(event)
				comment, covered := relocated[federationBaselineCommentKey{event.IssueUID, uid}]
				if covered && (event.Type == "issue.commented" || comment.coversEdit(event, body, version)) {
					continue
				}
			}
		}
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

// federationRelocationBaselines returns, for each issue without retained move
// history, its latest snapshot when other projects also hold events for it.
// Those events reach a fold only through a snapshot relocation, and that
// snapshot is the issue's state at its clock, so it covers the older ones.
func federationRelocationBaselines(events []FoldEvent) map[string]FoldEvent {
	moved := map[string]bool{}
	latest := map[string]FoldEvent{}
	for _, event := range events {
		switch event.Type {
		case "issue.moved":
			moved[event.IssueUID] = true
		case "issue.snapshot":
			if previous, ok := latest[event.IssueUID]; !ok || compareClock(clockOf(event), clockOf(previous)) > 0 {
				latest[event.IssueUID] = event
			}
		}
	}
	relocations := map[string]FoldEvent{}
	for _, event := range events {
		snapshot, ok := latest[event.IssueUID]
		if ok && !moved[event.IssueUID] && event.ProjectUID != snapshot.ProjectUID {
			relocations[event.IssueUID] = snapshot
		}
	}
	return relocations
}

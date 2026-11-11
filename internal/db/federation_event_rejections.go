package db

import (
	"encoding/json/v2"
	"fmt"
	"strings"
)

// FederationProjectNotEnrolledCode identifies a missing current-project enrollment.
const FederationProjectNotEnrolledCode = "federation_project_not_enrolled"

// FederationEventRejectionsHeader opts a sender into per-event rejection responses.
const FederationEventRejectionsHeader = "X-Kata-Federation-Event-Rejections"

// FederationEventRejection identifies retained source work that needs another
// project enrollment. It never acknowledges a change to that issue.
type FederationEventRejection struct {
	SourceEventID      int64  `json:"source_event_id"`
	EventUID           string `json:"event_uid"`
	IssueUID           string `json:"issue_uid"`
	CurrentProjectUID  string `json:"current_project_uid"`
	CurrentProjectName string `json:"current_project_name"`
	Code               string `json:"code"`
	Message            string `json:"message"`
}

// FederationProjectNotEnrolledError is narrower than an invalid credential:
// the source credential is valid but no supplemental proof was submitted.
type FederationProjectNotEnrolledError struct {
	IssueUID    string
	ProjectUID  string
	ProjectName string
}

func (e *FederationProjectNotEnrolledError) Error() string {
	return fmt.Sprintf("issue %s currently belongs to project %s (%s); matching project enrollment or push proof is missing; configure matching push access and actor for that project to retry", e.IssueUID, e.ProjectName, e.ProjectUID)
}
func (e *FederationProjectNotEnrolledError) Unwrap() error {
	return ErrFederationMoveSourceUnauthorized
}

// Rejection describes the source event retained until its project is enrolled.
func (e *FederationProjectNotEnrolledError) Rejection(sourceID int64, eventUID string) FederationEventRejection {
	return FederationEventRejection{SourceEventID: sourceID, EventUID: eventUID, IssueUID: e.IssueUID, CurrentProjectUID: e.ProjectUID, CurrentProjectName: e.ProjectName, Code: FederationProjectNotEnrolledCode, Message: e.Error()}
}

// FederationRejectedCreations tracks unavailable issues during ingestion or a push drain.
type FederationRejectedCreations map[string]FederationEventRejection

// ValidationUIDs lets validation recognize retained creations without treating
// them as accepted by subsequent events.
func (r FederationRejectedCreations) ValidationUIDs(known map[string]struct{}) map[string]struct{} {
	if len(r) == 0 {
		return known
	}
	all := make(map[string]struct{}, len(known)+len(r))
	for uid := range known {
		all[uid] = struct{}{}
	}
	for uid := range r {
		all[uid] = struct{}{}
	}
	return all
}

// Dependent returns the original rejection for an event needing a retained creation.
func (r FederationRejectedCreations) Dependent(event RemoteEvent) (FederationEventRejection, bool, error) {
	if len(r) == 0 {
		return FederationEventRejection{}, false, nil
	}
	uid, refs, err := federationEventIssueReferences(event, event)
	if err != nil {
		return FederationEventRejection{}, false, err
	}
	refs = append(refs, uid)
	for _, ref := range refs {
		if rejection, ok := r[ref]; ok {
			return rejection, true, nil
		}
	}
	return FederationEventRejection{}, false, nil
}

func federationEventIssueReferences(event, referenceEvent RemoteEvent) (string, []string, error) {
	if strings.HasPrefix(event.Type, "issue.") {
		primary, err := FederationPayloadIssueUID(event, PayloadMap(event.Payload))
		if err != nil {
			return "", nil, err
		}
		refs, err := FederationPayloadReferencedIssueUIDs(referenceEvent, PayloadMap(referenceEvent.Payload))
		return primary, refs, err
	}
	if event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot" {
		var run CronRunObservation
		if err := json.Unmarshal(event.Payload, &run); err != nil {
			return "", nil, err
		}
		if run.IssueUID != nil {
			return *run.IssueUID, nil, nil
		}
	}
	return "", nil, nil
}

// Remember records a rejected creation so dependent events remain pending too.
func (r FederationRejectedCreations) Remember(event RemoteEvent, rejection FederationEventRejection) {
	if event.Type != "issue.created" && event.Type != "issue.snapshot" {
		return
	}
	if uid, err := FederationPayloadIssueUID(event, PayloadMap(event.Payload)); err == nil && uid != "" {
		r[uid] = rejection
	}
}

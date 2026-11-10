package db

import (
	"encoding/json/jsontext"
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
	return fmt.Sprintf("issue %s currently belongs to project %s (%s); the spoke has no enrollment there; join that project to retry", e.IssueUID, e.ProjectName, e.ProjectUID)
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
func (r FederationRejectedCreations) Dependent(event RemoteEvent, primary func(RemoteEvent, map[string]jsontext.Value) (string, error), references func(RemoteEvent, map[string]jsontext.Value) ([]string, error)) (FederationEventRejection, bool, error) {
	if len(r) == 0 || !strings.HasPrefix(event.Type, "issue.") {
		return FederationEventRejection{}, false, nil
	}
	payload := PayloadMap(event.Payload)
	uid, err := primary(event, payload)
	if err != nil {
		return FederationEventRejection{}, false, err
	}
	refs, err := references(event, payload)
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

// Remember records a rejected creation so dependent events remain pending too.
func (r FederationRejectedCreations) Remember(event RemoteEvent, rejection FederationEventRejection, primary func(RemoteEvent, map[string]jsontext.Value) (string, error)) {
	if event.Type != "issue.created" && event.Type != "issue.snapshot" {
		return
	}
	if uid, err := primary(event, PayloadMap(event.Payload)); err == nil && uid != "" {
		r[uid] = rejection
	}
}

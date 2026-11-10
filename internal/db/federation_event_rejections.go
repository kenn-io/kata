package db

import (
	"fmt"
)

const FederationProjectNotEnrolledCode = "federation_project_not_enrolled"
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
func (e *FederationProjectNotEnrolledError) Rejection(sourceID int64, eventUID string) FederationEventRejection {
	return FederationEventRejection{SourceEventID: sourceID, EventUID: eventUID, IssueUID: e.IssueUID, CurrentProjectUID: e.ProjectUID, CurrentProjectName: e.ProjectName, Code: FederationProjectNotEnrolledCode, Message: e.Error()}
}

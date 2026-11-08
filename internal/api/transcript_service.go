package api

import "go.kenn.io/kata/internal/transcriptservice"

const TranscriptServicePath = "/api/v1/projects/{project_id}/transcript-service"
const FederationTranscriptServicePath = "/api/v1/projects/{project_id}/federation/transcript-service"

type TranscriptServiceRequest struct {
	ProjectID int64 `path:"project_id" required:"true" minimum:"1"`
}

type FederationTranscriptServiceRequest struct {
	ProjectID     int64  `path:"project_id" required:"true" minimum:"1"`
	Authorization string `header:"Authorization"`
	ProjectUID    string `query:"project_uid" required:"true" maxLength:"26"`
	AuthorityUID  string `query:"authority_uid" required:"true" maxLength:"26"`
}

type TranscriptServiceResponse struct {
	CacheControl string `header:"Cache-Control"`
	Body         transcriptservice.Discovery
}

package api

import "github.com/danielgtaylor/huma/v2"

// FederationPublicationEnvelope describes the closed version-1 portable object.
// Each record's data is validated by its kind by the publication codec, including
// immutable event payload strings. It is never an opaque owner backup.
type FederationPublicationEnvelope struct {
	Version           int                           `json:"version" enum:"1"`
	OperationUID      string                        `json:"operation_uid"`
	SourceInstanceUID string                        `json:"source_instance_uid"`
	ProjectUID        string                        `json:"project_uid"`
	TargetInstanceUID string                        `json:"target_instance_uid"`
	ProjectName       string                        `json:"project_name"`
	Digest            string                        `json:"digest"`
	Records           []FederationPublicationRecord `json:"records"`
}

// FederationPublicationRecord contains one typed record in a publication envelope.
type FederationPublicationRecord struct {
	Kind string        `json:"kind" enum:"project,issue,comment,issue_label,link,event"`
	Data JSONRawObject `json:"data"`
}

// PublishFederationProjectRequest retains the original JSON object for exact validation.
type PublishFederationProjectRequest struct {
	// Retain the original object for duplicate/unknown-member and digest checks.
	RawBody []byte
}

// PublishFederationProjectBody is the generated request shape for publication.
type PublishFederationProjectBody struct {
	Preflight bool                          `json:"preflight"`
	Envelope  FederationPublicationEnvelope `json:"envelope"`
}

// FederationPublicationCounts reports records included in the publication.
type FederationPublicationCounts struct {
	Issues   int64 `json:"issues"`
	Comments int64 `json:"comments"`
	Events   int64 `json:"events"`
}

// FederationPublicationBody reports the publication result and destination project.
type FederationPublicationBody struct {
	Status      string                      `json:"status" enum:"ready,published"`
	ProjectUID  string                      `json:"project_uid"`
	ProjectID   int64                       `json:"project_id,omitempty,omitzero"`
	ProjectName string                      `json:"project_name"`
	Account     string                      `json:"account"`
	Visibility  string                      `json:"visibility" enum:"all,teams"`
	Counts      FederationPublicationCounts `json:"counts"`
	Replayed    bool                        `json:"replayed"`
}

// PublishFederationProjectResponse wraps the publication result body.
type PublishFederationProjectResponse struct {
	Body FederationPublicationBody
}

// TransformSchema gives the new enum its own generated Go constants. Without
// explicit names the generator retypes the existing project-policy All/Teams
// constants when it encounters this earlier schema.
func (FederationPublicationBody) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	if schema == nil || schema.Properties == nil {
		return schema
	}
	visibility, ok := schema.Properties["visibility"]
	if !ok || visibility == nil {
		return schema
	}
	visibility.Extensions = map[string]any{
		"x-enum-names": []string{"FederationPublicationVisibilityAll", "FederationPublicationVisibilityTeams"},
	}
	return schema
}

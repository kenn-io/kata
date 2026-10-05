package db

import "go.kenn.io/kata/internal/embedding"

// CreateRelayEnrollmentParams narrows a live ordinary user credential to one
// project and peer. Actor, when supplied, must match the credential's account.
// Token is the retry identity; an explicit token returns the retained grant.
// RebindParent deliberately replaces only its same-account issuing credential.
type CreateRelayEnrollmentParams struct {
	ProjectID        int64
	ParentTokenID    int64
	SpokeInstanceUID string
	ProtocolVersion  int
	Actor            string
	Token            string
	ServeDownstream  bool
	RebindParent     bool
}

// RelayBatch starts at the sender's last acknowledged prefix. A lost response
// can replay that prefix; it can never advance past receiver acceptance.
type RelayBatch struct {
	Stream    string                        `json:"stream"`
	After     int64                         `json:"after"`
	Envelopes []RelayEnvelope               `json:"envelopes"`
	Artifacts []embedding.EmbeddingArtifact `json:"artifacts,omitempty"`
}

// RelayAcceptance reports the accepted prefix and exact artifact digests still missing.
type RelayAcceptance struct {
	Through           int64    `json:"through"`
	Digest            string   `json:"digest"`
	MissingDigests    []string `json:"missing_digests,omitempty"`
	InsertedEventUIDs []string `json:"-"`
}

package kata_test

import (
	"context"
	"encoding/json/jsontext"
	"io"
	"testing"

	kata "go.kenn.io/kata"
)

// External embedders must be able to name every callback type without
// importing Kata's internal packages.
func TestAgentsViewAdapterTypesArePublic(_ *testing.T) {
	_ = kata.Config{AgentsViewRelay: &kata.AgentsViewRelayConfig{
		LiveClaimStatus: func(context.Context, kata.AgentsViewFederationBinding, string) (kata.AgentsViewClaimStatus, error) {
			return kata.AgentsViewClaimStatus{}, nil
		},
		Selection: func(context.Context, kata.AgentsViewIssue, kata.AgentsViewAssociation, jsontext.Value) error {
			return nil
		},
		Spool: func(context.Context, io.Reader, kata.AgentsViewControl) (kata.AgentsViewSnapshot, error) {
			return nil, nil
		},
		Forward: func(context.Context, kata.AgentsViewIssue, kata.AgentsViewControl, io.ReadSeeker) (kata.AgentsViewReceipt, error) {
			return kata.AgentsViewReceipt{}, nil
		},
		Ingest: func(context.Context, kata.AgentsViewIssue, kata.AgentsViewAssociation, io.ReadSeeker) (kata.AgentsViewIngestAck, error) {
			return kata.AgentsViewIngestAck{}, nil
		},
		VerifyFederationUpload: func(context.Context, kata.AgentsViewControl) error { return nil },
	}}
}

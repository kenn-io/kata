package db_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestDocbankOriginPayloadRequiresFeature(t *testing.T) {
	const ref = `{"origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV","docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","name":"file.pdf","size":12,"media_type":"application/pdf"}`
	for _, tc := range []struct{ kind, payload, required string }{
		{"issue.created", `{"metadata":{"attachment.id":` + ref + `}}`, "docbank_origin_v1"},
		{"issue.created", `{"metadata":{"cron.occurrence":{},"attachment.id":` + ref + `}}`, "cron_v1,docbank_origin_v1"},
		{"issue.metadata_updated", `{"diff":{"attachment.id":{"from":null,"to":` + ref + `}}}`, "docbank_origin_v1"},
		{"issue.metadata_updated", `{"diff":{"attachment.id":{"from":` + ref + `,"to":null}}}`, "docbank_origin_v1"},
		{"issue.snapshot", `{"metadata":{"attachment.id":` + ref + `}}`, "docbank_origin_v1"},
		{"issue.snapshot", `{"\u006detadata":{"attachment.id":` + ref + `}}`, "docbank_origin_v1"},
		{"projection.issue", `{"metadata":{"attachment.id":` + ref + `}}`, "docbank_origin_v1"},
		{"issue.moved", `{"snapshot":{"metadata":{"attachment.id":` + ref + `}}}`, "docbank_origin_v1,issue_move_v1"},
		{"issue.closed", `{"evidence":[{"type":"attachment","attachment":` + ref + `}]}`, "docbank_origin_v1"},
		{"issue.closed", `{"evidence":[{"type":"test","attachment":` + ref + `}]}`, ""},
		{"issue.created", `{"body":"attachment.id origin","metadata":{"attachment.id":{"origin":"local"}}}`, "docbank_origin_v1"},
		{"issue.created", `{"metadata":{"attachment.id":{"origin":""}}}`, ""},
		{"issue.created", `{"metadata":{"attachment.id":{"origin":"\u006cocal"}}}`, "docbank_origin_v1"},
		{"issue.metadata_updated", `{"diff":{"attachment.id":{"from":{"origin":"local"},"to":null}}}`, "docbank_origin_v1"},
		{"issue.snapshot", `{"metadata":{"note":"\\\"attachment.id\\\":{\\\"origin\\\":\\\"nonlocal\\\"}"}}`, ""},
		{"project.metadata_updated", `{"evidence":[{"type":"attachment","attachment":` + ref + `}]}`, ""},
		{"issue.created", `{"metadata":{"other":` + ref + `}}`, ""},
		{"project.metadata_updated", `{"metadata":{"attachment.id":` + ref + `}}`, ""},
		{"issue.created", `{"metadata":{"attachment.id":` + strings.ReplaceAll(ref, `"origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV",`, "") + `}}`, ""},
	} {
		got, err := db.PayloadRequiredFeatures(tc.kind, tc.payload)
		require.NoError(t, err)
		require.Equal(t, tc.required, got, tc.payload)
	}
}

package db_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

func TestDocbankOriginPayloadRequiresFeature(t *testing.T) {
	const ref = `{"origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV","docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","name":"file.pdf","size":12,"media_type":"application/pdf"}`
	for _, tc := range []struct{ kind, payload, required string }{
		{"issue.created", `{"metadata":{"attachment.id":` + ref + `}}`, "docbank_origin_v1"},
		{"issue.metadata_updated", `{"diff":{"attachment.id":{"from":null,"to":` + ref + `}}}`, "docbank_origin_v1"},
		{"issue.metadata_updated", `{"diff":{"attachment.id":{"from":` + ref + `,"to":null}}}`, "docbank_origin_v1"},
		{"issue.snapshot", `{"metadata":{"attachment.id":` + ref + `}}`, "docbank_origin_v1"},
		{"projection.issue", `{"metadata":{"attachment.id":` + ref + `}}`, "docbank_origin_v1"},
		{"issue.moved", `{"snapshot":{"metadata":{"attachment.id":` + ref + `}}}`, "docbank_origin_v1,issue_move_v1"},
		{"issue.closed", `{"evidence":[{"type":"attachment","attachment":` + ref + `}]}`, "docbank_origin_v1"},
		{"issue.closed", `{"evidence":[{"type":"test","attachment":` + ref + `}]}`, ""},
		{"issue.created", `{"body":"attachment.id origin","metadata":{"attachment.id":{"origin":"local"}}}`, "docbank_origin_v1"},
		{"issue.created", `{"metadata":{"attachment.id":{"origin":""}}}`, ""},
		{"issue.created", `{"metadata":{"attachment.id":{"origin":"\u006cocal"}}}`, "docbank_origin_v1"},
		{"issue.snapshot", `{"metadata":{"attachment.id":{"or\u0069gin":"\u00301ARZ3NDEKTSV4RRFFQ69G5FAV"}}}`, "docbank_origin_v1"},
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
	hegel.Test(t, func(ht *hegel.T) {
		mask := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
		escape := func(value string) string {
			var out strings.Builder
			for i, c := range value {
				if mask&(1<<uint(i)) != 0 {
					fmt.Fprintf(&out, `\u%04x`, c)
				} else {
					out.WriteRune(c)
				}
			}
			return out.String()
		}
		payload := `{"` + escape("metadata") + `":{"` + escape("attachment.id") + `":` + strings.Replace(ref, `"origin"`, `"`+escape("origin")+`"`, 1) + `}}`
		got, err := db.PayloadRequiredFeatures("issue.snapshot", payload)
		if err != nil || got != "docbank_origin_v1" {
			ht.Fatalf("escaped metadata key lost feature: %s: %v %s", payload, err, got)
		}
	}, hegel.WithTestCases(32))
}

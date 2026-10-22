package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/shortid"
)

func publicationFixture(t *testing.T) FederationPublication {
	t.Helper()
	const project = "01K00000000000000000000001"
	const issue = "01K0000000000000000000AB34"
	const stamp = "2026-10-20T12:00:00.000Z"
	event := &EventExport{ID: 9, UID: "01K00000000000000000000003", OriginInstanceUID: "01K00000000000000000000004", ProjectID: 7, ProjectUID: project, ProjectName: "former-project", IssueID: new(int64(8)), IssueUID: new(issue), Type: "issue.created", Actor: "writer", HLCPhysicalMS: 1792497600000, CreatedAt: stamp,
		Payload: jsontext.Value(`{"uid":"` + issue + `","short_id":"ab34","title":"Portable issue","body":"","author":"writer","status":"open","created_at":"` + stamp + `","updated_at":"` + stamp + `","metadata":{}}`)}
	require.NoError(t, PrepareReplayEvent(event, project, event.ProjectName, true))
	return FederationPublication{OperationUID: "01K00000000000000000000005", SourceInstanceUID: event.OriginInstanceUID, ProjectUID: project, TargetInstanceUID: "01K00000000000000000000006", ProjectName: "example-project", Records: []ImportRecord{
		&ProjectExport{ID: 7, UID: project, Name: "example-project", CreatedAt: stamp, Metadata: jsontext.Value(`{}`), Revision: 1},
		&IssueExport{ID: 8, UID: issue, ProjectID: 7, ShortID: "ab34", Title: "Portable issue", Author: "writer", Status: "open", CreatedAt: stamp, UpdatedAt: stamp, Metadata: jsontext.Value(`{}`), Revision: 1, ContentRevision: 1}, event,
	}}
}

// The publication contract rejects unsupported content rather than accepting
// owner-restore records or silently changing history to fit current rows.
func TestFederationPublicationClosedEnvelope(t *testing.T) {
	p := publicationFixture(t)
	before, err := json.Marshal(p.Records)
	require.NoError(t, err)
	counts, err := ValidateFederationPublication(p)
	require.NoError(t, err)
	require.Equal(t, FederationPublicationCounts{Issues: 1, Events: 1}, counts)
	after, err := json.Marshal(p.Records)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for _, tc := range []struct {
		name   string
		mutate func(*FederationPublication)
	}{
		{"invalid identity", func(p *FederationPublication) { p.OperationUID = "bad" }},
		{"host metadata", func(p *FederationPublication) {
			p.Records = append(p.Records, &MetaKV{Key: "secret", Value: "host-only"})
		}},
		{"alias", func(p *FederationPublication) { p.Records = append(p.Records, &AliasExport{}) }},
		{"nil", func(p *FederationPublication) { p.Records = append(p.Records, (*IssueExport)(nil)) }},
		{"duplicate row", func(p *FederationPublication) { p.Records = append(p.Records, p.Records[1]) }},
		{"duplicate event", func(p *FederationPublication) { p.Records = append(p.Records, p.Records[2]) }},
		{"bad hash", func(p *FederationPublication) { p.Records[2].(*EventExport).ContentHash = string(make([]byte, 64)) }},
		{"projection disagreement", func(p *FederationPublication) { p.Records[1].(*IssueExport).Title = "invented" }},
		{"missing history", func(p *FederationPublication) { p.Records = p.Records[:2] }},
		{"recurring issue", func(p *FederationPublication) {
			p.Records[1].(*IssueExport).RecurrenceUID = new("01K00000000000000000000007")
		}},
		{"archived project", func(p *FederationPublication) {
			p.Records[0].(*ProjectExport).DeletedAt = new("2026-10-20T12:00:00.000Z")
		}},
		{"foreign project", func(p *FederationPublication) { p.Records[1].(*IssueExport).ProjectID++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := publicationFixture(t)
			tc.mutate(&p)
			_, err := ValidateFederationPublication(p)
			require.ErrorIs(t, err, ErrFederationPublicationInvalid)
		})
	}
}

func TestFederationPublicationRejectsInvalidIssueProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*IssueExport, map[string]jsontext.Value)
	}{
		{"blank title", func(issue *IssueExport, payload map[string]jsontext.Value) {
			issue.Title = " \t "
			payload["title"], _ = json.Marshal(issue.Title)
		}},
		{"priority outside store range", func(issue *IssueExport, payload map[string]jsontext.Value) {
			issue.Priority = new(int64(5))
			payload["priority"] = jsontext.Value(`5`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := publicationFixture(t)
			issue := p.Records[1].(*IssueExport)
			event := p.Records[2].(*EventExport)
			payload := PayloadMap(event.Payload)
			tc.mutate(issue, payload)
			event.Payload, _ = json.Marshal(payload)
			require.NoError(t, PrepareReplayEvent(event, p.ProjectUID, event.ProjectName, true))

			_, err := ValidateFederationPublication(p)
			require.ErrorIs(t, err, ErrFederationPublicationInvalid)
		})
	}
}

func TestFederationPublicationHistoryOrder(t *testing.T) {
	for _, tc := range []struct{ name, kind, payload string }{
		{"dependent first", "issue.assigned", `{"owner":"worker"}`},
		{"unknown event", "issue.future", `{}`},
		{"purge", "issue.purged", `{}`},
		{"external peer", "issue.links_changed", `{"related_removed_uids":["01K00000000000000000000007"]}`},
		{"purged close parent", "issue.closed", `{"reason":"done","parent_uid":"01K00000000000000000000007"}`},
		{"purged close evidence", "issue.closed", `{"reason":"duplicate","evidence":[{"type":"duplicate-of","issue_ref":"other-project#zz99"}]}`},
		{"mismatching identity", "issue.updated", `{"uid":"01K00000000000000000000007"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := publicationFixture(t)
			e := *p.Records[2].(*EventExport)
			e.ID, e.UID, e.Type, e.Payload = 10, "01K00000000000000000000008", tc.kind, jsontext.Value(tc.payload)
			e.HLCCounter = 1
			require.NoError(t, PrepareReplayEvent(&e, p.ProjectUID, e.ProjectName, true))
			if tc.name == "dependent first" {
				e.ID, e.HLCCounter = 1, 0
				p.Records = append(p.Records[:2], &e, p.Records[2])
			} else {
				p.Records = append(p.Records, &e)
			}
			_, err := ValidateFederationPublication(p)
			require.ErrorIs(t, err, ErrFederationPublicationInvalid)
		})
	}
	t.Run("catalog cannot introduce issue", func(t *testing.T) {
		p := publicationFixture(t)
		original := *p.Records[2].(*EventExport)
		catalog := original
		catalog.ID, catalog.UID, catalog.Type, catalog.Payload = 9, "01K00000000000000000000008", "project.created", jsontext.Value(`{"name":"example-project"}`)
		catalog.IssueID, catalog.IssueUID, catalog.RelatedIssueID, catalog.RelatedIssueUID = nil, nil, nil, nil
		require.NoError(t, PrepareReplayEvent(&catalog, p.ProjectUID, catalog.ProjectName, true))
		dependent := original
		dependent.ID, dependent.UID, dependent.Type, dependent.Payload, dependent.HLCCounter = 10, "01K00000000000000000000009", "issue.assigned", jsontext.Value(`{"owner":"worker"}`), 1
		require.NoError(t, PrepareReplayEvent(&dependent, p.ProjectUID, dependent.ProjectName, true))
		p.Records[2] = &catalog
		p.Records = append(p.Records, &dependent)
		_, err := ValidateFederationPublication(p)
		require.ErrorIs(t, err, ErrFederationPublicationInvalid)
	})
}

func TestFederationPublicationTypedPayloads(t *testing.T) {
	for _, tc := range []struct{ name, kind, payload string }{
		{"wrong scalar type", "issue.updated", `{"title":true}`},
		{"null owner", "issue.assigned", `{"owner":null}`},
		{"missing label", "issue.labeled", `{}`},
		{"unknown payload reference", "issue.updated", `{"future_issue_uid":"01K00000000000000000000007"}`},
		{"historical recurrence", "issue.snapshot", `{"uid":"01K0000000000000000000AB34","short_id":"ab34","title":"Portable issue","body":"","author":"writer","status":"open","created_at":"2026-10-20T12:00:00.000Z","updated_at":"2026-10-20T12:00:00.000Z","metadata":{},"recurrence_uid":"01K00000000000000000000007"}`},
		{"null embedded collection", "issue.snapshot", `{"uid":"01K0000000000000000000AB34","short_id":"ab34","title":"Portable issue","body":"","author":"writer","status":"open","created_at":"2026-10-20T12:00:00.000Z","updated_at":"2026-10-20T12:00:00.000Z","metadata":{},"comments":null}`},
		{"unknown unlink endpoint", "issue.unlinked", `{"type":"related","from_uid":"01K00000000000000000000002","to_uid":"01K00000000000000000000007"}`},
		{"malformed array", "issue.links_changed", `{"related_removed_uids":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := publicationFixture(t)
			e := *p.Records[2].(*EventExport)
			e.ID, e.UID, e.Type, e.Payload, e.HLCCounter = 10, "01K00000000000000000000008", tc.kind, jsontext.Value(tc.payload), 1
			require.NoError(t, PrepareReplayEvent(&e, p.ProjectUID, e.ProjectName, true))
			p.Records = append(p.Records, &e)
			_, err := ValidateFederationPublication(p)
			require.ErrorIs(t, err, ErrFederationPublicationInvalid)
		})
	}
}

func TestFederationPublicationEmbeddedCommentShape(t *testing.T) {
	for _, variant := range []string{"unknown field", "duplicate identity"} {
		t.Run(variant, func(t *testing.T) {
			p := publicationFixture(t)
			e := p.Records[2].(*EventExport)
			c := &CommentExport{ID: 10, UID: "01K00000000000000000000007", IssueID: 8, Author: "writer", Teammate: "builder", Body: "Comment", CreatedAt: e.CreatedAt}
			p.Records = append(p.Records, c)
			entry := map[string]any{"comment_uid": c.UID, "author": c.Author, "teammate": c.Teammate, "body": c.Body, "created_at": c.CreatedAt}
			payload := PayloadMap(e.Payload)
			payload["comments"], _ = json.Marshal([]any{entry})
			e.Payload, _ = json.Marshal(payload)
			require.NoError(t, PrepareReplayEvent(e, p.ProjectUID, e.ProjectName, true))
			_, err := ValidateFederationPublication(p)
			require.NoError(t, err, "base fixture is replayable")
			if variant == "unknown field" {
				entry["future_issue_uid"] = "01K00000000000000000000008"
				payload["comments"], _ = json.Marshal([]any{entry})
			} else {
				payload["comments"], _ = json.Marshal([]any{entry, entry})
			}
			e.Payload, _ = json.Marshal(payload)
			require.NoError(t, PrepareReplayEvent(e, p.ProjectUID, e.ProjectName, true))
			_, err = ValidateFederationPublication(p)
			require.ErrorIs(t, err, ErrFederationPublicationInvalid)
		})
	}
}

// The native schema stores lowercase UID suffixes, including extended lengths.
// Reject a syntactically valid but unrelated ID before native insertion.
func TestFederationPublicationShortIDSuffix(t *testing.T) {
	for _, tc := range []struct {
		id    string
		valid bool
	}{{"ab34", true}, {"00ab34", true}, {"zz12", false}} {
		t.Run(tc.id, func(t *testing.T) {
			require.True(t, shortid.Valid(tc.id))
			p := publicationFixture(t)
			p.Records[1].(*IssueExport).ShortID = tc.id
			e := p.Records[2].(*EventExport)
			e.Payload = jsontext.Value(strings.ReplaceAll(string(e.Payload), "ab34", tc.id))
			require.NoError(t, PrepareReplayEvent(e, p.ProjectUID, e.ProjectName, true))
			_, err := ValidateFederationPublication(p)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrFederationPublicationInvalid)
			}
		})
	}
}

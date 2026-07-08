package api_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/metadata"
)

// TestShowIssueResponseHasShortIDAndNoNumber pins the wire-side rename:
// every issue endpoint's response body now exposes short_id (+ qualified_id)
// instead of the legacy integer number. The plan's example referenced an
// IssueResponse alias; this codebase uses ShowIssueResponse for the per-issue
// read payload, whose Body.Issue embeds db.Issue (so ShortID/UID live on the
// embedded struct).
func TestShowIssueResponseHasShortIDAndNoNumber(t *testing.T) {
	body := reflect.TypeOf(api.ShowIssueResponse{}.Body)
	issueField, ok := body.FieldByName("Issue")
	if !ok {
		t.Fatalf("ShowIssueResponse.Body.Issue field missing")
	}
	requireFieldHasJSONTag(t, issueField.Type, "ShortID", "short_id")
	requireFieldHasJSONTag(t, issueField.Type, "UID", "uid")
	_, hasNumber := issueField.Type.FieldByName("Number")
	assert.False(t, hasNumber, "db.Issue must not have a Number field")
}

// TestIssueRefHasShortIDAndQualifiedID covers the compact parent-context type
// used inside ShowIssueResponse.Parent.
func TestIssueRefHasShortIDAndQualifiedID(t *testing.T) {
	typ := reflect.TypeOf(api.IssueRef{})
	requireFieldHasJSONTag(t, typ, "ShortID", "short_id")
	requireFieldHasJSONTag(t, typ, "QualifiedID", "qualified_id")
	_, hasNumber := typ.FieldByName("Number")
	assert.False(t, hasNumber, "IssueRef must not have a Number field")
}

// TestIssueOutHasShortIDFamily covers the list/search row projection that the
// daemon hydrates: short_id, qualified_id, plus structured peer arrays for
// blocks/blocked_by/related (UID + short_id).
//
// ShortID/QualifiedID and the three peer arrays are required: a regression
// that deletes any of them is a wire-shape break, not an optional change.
// Number/ParentNumber stay as explicit absence checks — those are the v7
// fields whose disappearance the cutover is meant to enforce.
func TestIssueOutHasShortIDFamily(t *testing.T) {
	typ := reflect.TypeOf(api.IssueOut{})
	// ShortID lives on the embedded db.Issue but reflect's FieldByName
	// traverses anonymous fields, so the lookup still resolves.
	requireFieldHasJSONTag(t, typ, "ShortID", "short_id")
	requireFieldHasJSONTag(t, typ, "QualifiedID", "qualified_id")
	for _, f := range []string{"Number", "ParentNumber"} {
		_, has := typ.FieldByName(f)
		assert.Falsef(t, has, "IssueOut.%s must disappear", f)
	}
	peerSlice := reflect.SliceOf(reflect.TypeOf(api.LinkPeer{}))
	for _, f := range []string{"Blocks", "BlockedBy", "Related"} {
		// The int64 link arrays are replaced by LinkPeer slices so
		// consumers get UID + short_id together; a missing field would
		// quietly drop the peer projection from the wire and must fail.
		field, ok := typ.FieldByName(f)
		if !ok {
			t.Errorf("IssueOut.%s field missing", f)
			continue
		}
		assert.Equalf(t, peerSlice, field.Type, "IssueOut.%s must be []LinkPeer", f)
	}
}

// TestProjectOutHasNoNextIssueNumber checks the project projection lost its
// counter field. The plan calls this ProjectResponse but the codebase exposes
// the projection as ProjectOut (embedded in ListProjectsResponse, ShowProject
// Response, etc.).
func TestProjectOutHasNoNextIssueNumber(t *testing.T) {
	typ := reflect.TypeOf(api.ProjectOut{})
	_, has := typ.FieldByName("NextIssueNumber")
	assert.False(t, has, "ProjectOut.NextIssueNumber must be gone")
}

// TestLinkPeerShape pins the structured replacement for from_number/to_number
// on link records. Project and QualifiedID are always populated (0.2.0).
func TestLinkPeerShape(t *testing.T) {
	typ := reflect.TypeOf(api.LinkPeer{})
	requireFieldHasJSONTag(t, typ, "UID", "uid")
	requireFieldHasJSONTag(t, typ, "ShortID", "short_id")
	requireFieldHasJSONTag(t, typ, "Project", "project")
	requireFieldHasJSONTag(t, typ, "QualifiedID", "qualified_id")
}

// TestIssueOutUsesLinkPeerForParent pins that IssueOut carries Parent *LinkPeer
// (0.2.0) and no longer exposes the bare ParentShortID string.
func TestIssueOutUsesLinkPeerForParent(t *testing.T) {
	typ := reflect.TypeOf(api.IssueOut{})
	_, hasOld := typ.FieldByName("ParentShortID")
	assert.False(t, hasOld, "IssueOut.ParentShortID must be removed (replaced by Parent *LinkPeer)")
	f, ok := typ.FieldByName("Parent")
	if !ok {
		t.Fatal("IssueOut.Parent missing")
	}
	peerPtrType := reflect.TypeOf((*api.LinkPeer)(nil))
	assert.Equal(t, peerPtrType, f.Type, "IssueOut.Parent must be *LinkPeer")
}

// TestLinkPeerHasStatus pins that LinkPeer carries the peer issue's status
// (0.9.0), so clients (e.g. `kata list` human output) can render a blocked
// glyph without an extra lookup per blocked_by peer.
func TestLinkPeerHasStatus(t *testing.T) {
	typ := reflect.TypeOf(api.LinkPeer{})
	f, ok := typ.FieldByName("Status")
	if !ok {
		t.Fatal("LinkPeer.Status missing")
	}
	assert.Equal(t, reflect.TypeOf(""), f.Type, "LinkPeer.Status must be string")
	assert.Equal(t, "status", f.Tag.Get("json"), `LinkPeer.Status must be tagged json:"status"`)
}

// TestLinkOutUsesLinkPeer covers the wire projection of one link: from/to are
// now structured peer objects rather than flat integer numbers, and the
// project_id field is gone now that links are project-independent edges
// (storage v16 / API 0.2.0).
func TestLinkOutUsesLinkPeer(t *testing.T) {
	typ := reflect.TypeOf(api.LinkOut{})
	for _, field := range []string{"FromNumber", "FromIssueUID", "ToNumber", "ToIssueUID", "ProjectID"} {
		_, has := typ.FieldByName(field)
		assert.Falsef(t, has, "LinkOut.%s must not be on the wire", field)
	}
	for _, field := range []string{"From", "To"} {
		f, ok := typ.FieldByName(field)
		if !ok {
			t.Fatalf("LinkOut.%s missing", field)
		}
		assert.Equalf(t, reflect.TypeOf(api.LinkPeer{}), f.Type,
			"LinkOut.%s must be LinkPeer", field)
	}
}

// TestPathRequestsUseRef pins the request-struct switch from {number} path
// params to {ref} (a string that the daemon resolves to short_id or UID).
// Iterates every issue-scoped request type listed in spec §9.3.
func TestPathRequestsUseRef(t *testing.T) {
	cases := []any{
		api.ShowIssueRequest{},
		api.EditIssueRequest{},
		api.CommentRequest{},
		api.ActionRequest{},
		api.CreateLinkRequest{},
		api.DeleteLinkRequest{},
		api.AddLabelRequest{},
		api.RemoveLabelRequest{},
		api.AssignRequest{},
		api.PriorityRequest{},
		api.UnassignRequest{},
		api.DestructiveActionRequest{},
		api.RestoreRequest{},
	}
	for _, c := range cases {
		typ := reflect.TypeOf(c)
		_, hasNumber := typ.FieldByName("Number")
		assert.Falsef(t, hasNumber, "%s.Number must be gone", typ.Name())
		f, ok := typ.FieldByName("Ref")
		if !ok {
			t.Errorf("%s.Ref missing", typ.Name())
			continue
		}
		got, _, _ := strings.Cut(f.Tag.Get("path"), ",")
		assert.Equalf(t, "ref", got, "%s.Ref must carry path:\"ref\"", typ.Name())
		assert.Equalf(t, "true", f.Tag.Get("required"), "%s.Ref must be required", typ.Name())
		assert.Equalf(t, reflect.String, f.Type.Kind(), "%s.Ref must be a string", typ.Name())
	}
}

// TestCreateInitialLinkBodyUsesRef pins the JSON input switch on the create
// link body: to_number (int) becomes to_ref (string).
func TestCreateInitialLinkBodyUsesRef(t *testing.T) {
	typ := reflect.TypeOf(api.CreateInitialLinkBody{})
	_, hasOld := typ.FieldByName("ToNumber")
	assert.False(t, hasOld, "CreateInitialLinkBody.ToNumber must be replaced by ToRef")
	requireFieldHasJSONTag(t, typ, "ToRef", "to_ref")
}

// TestCreateLinkRequestBodyUsesRef covers the same rename for the standalone
// POST /links endpoint.
func TestCreateLinkRequestBodyUsesRef(t *testing.T) {
	body := reflect.TypeOf(api.CreateLinkRequest{}.Body)
	_, hasOld := body.FieldByName("ToNumber")
	assert.False(t, hasOld, "CreateLinkRequest.Body.ToNumber must be replaced by ToRef")
	requireFieldHasJSONTag(t, body, "ToRef", "to_ref")
}

// TestLinksDeltaUsesRefStrings checks the bulk relationship payload swapped
// every int64 link list for a []string of refs.
func TestLinksDeltaUsesRefStrings(t *testing.T) {
	typ := reflect.TypeOf(api.LinksDelta{})
	stringPtrFields := []string{"SetParent", "RemoveParent"}
	for _, name := range stringPtrFields {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("LinksDelta.%s missing", name)
			continue
		}
		assert.Equalf(t, "ptr", f.Type.Kind().String(), "LinksDelta.%s must be *string", name)
		assert.Equalf(t, reflect.String, f.Type.Elem().Kind(), "LinksDelta.%s must be *string", name)
	}
	sliceFields := []string{"AddBlocks", "AddBlockedBy", "AddRelated", "RemoveBlocks", "RemoveBlockedBy", "RemoveRelated"}
	for _, name := range sliceFields {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("LinksDelta.%s missing", name)
			continue
		}
		assert.Equalf(t, reflect.Slice, f.Type.Kind(), "LinksDelta.%s must be a slice", name)
		assert.Equalf(t, reflect.String, f.Type.Elem().Kind(), "LinksDelta.%s elements must be strings", name)
	}
}

// TestLinkChangesUsesLinkPeer pins the post-mutation reporting type: every
// applied link change is reported as a LinkPeer (UID + short_id) so callers
// know both forms without joining.
func TestLinkChangesUsesLinkPeer(t *testing.T) {
	typ := reflect.TypeOf(api.LinkChanges{})
	peerType := reflect.TypeOf(api.LinkPeer{})
	peerPtrType := reflect.PointerTo(peerType)
	peerSliceType := reflect.SliceOf(peerType)

	ptrFields := []string{"ParentSet", "ParentRemoved"}
	for _, name := range ptrFields {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("LinkChanges.%s missing", name)
			continue
		}
		assert.Equalf(t, peerPtrType, f.Type, "LinkChanges.%s must be *LinkPeer", name)
	}
	sliceFields := []string{
		"BlocksAdded", "BlocksRemoved",
		"BlockedByAdded", "BlockedByRemoved",
		"RelatedAdded", "RelatedRemoved",
	}
	for _, name := range sliceFields {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("LinkChanges.%s missing", name)
			continue
		}
		assert.Equalf(t, peerSliceType, f.Type, "LinkChanges.%s must be []LinkPeer", name)
	}
}

// TestEventEnvelopeUsesShortID pins the SSE/poll event payload: issue_number
// disappears in favor of issue_short_id (UID stays as the stable reference).
func TestEventEnvelopeUsesShortID(t *testing.T) {
	typ := reflect.TypeOf(api.EventEnvelope{})
	_, hasOld := typ.FieldByName("IssueNumber")
	assert.False(t, hasOld, "EventEnvelope.IssueNumber must be replaced by IssueShortID")
	requireFieldHasJSONTag(t, typ, "IssueShortID", "issue_short_id")
}

// TestDigestIssueActionsUsesShortID pins the digest payload: issue_number is
// replaced by issue_short_id (issue_uid is added as the stable reference).
func TestDigestIssueActionsUsesShortID(t *testing.T) {
	typ := reflect.TypeOf(api.DigestIssueActions{})
	_, hasOld := typ.FieldByName("IssueNumber")
	assert.False(t, hasOld, "DigestIssueActions.IssueNumber must disappear")
	requireFieldHasJSONTag(t, typ, "IssueShortID", "issue_short_id")
	requireFieldHasJSONTag(t, typ, "IssueUID", "issue_uid")
}

// TestMergeProjectResponseSurfacesExtensions checks that the merge response
// plumbs the db.ShortIDExtensions field through the API projection so callers
// see which source-side issues had their short_id shifted to break collisions.
func TestMergeProjectResponseSurfacesExtensions(t *testing.T) {
	typ := reflect.TypeOf(api.MergeProjectResponse{}.Body)
	f, ok := typ.FieldByName("ShortIDExtensions")
	if !ok {
		t.Fatalf("MergeProjectResponse.Body.ShortIDExtensions missing")
	}
	got, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	assert.Equal(t, "short_id_extensions", got)
	assert.Equal(t, reflect.Slice, f.Type.Kind())
}

func requireFieldHasJSONTag(t *testing.T, typ reflect.Type, name, jsonTag string) {
	t.Helper()
	f, ok := typ.FieldByName(name)
	if !ok {
		t.Fatalf("%s.%s field missing", typ.Name(), name)
	}
	got, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if got != jsonTag {
		t.Fatalf("%s.%s json tag = %q; want %q", typ.Name(), name, got, jsonTag)
	}
}

// TestActionRequest_RoundTripWithEvidence pins the close-action wire shape
// (anti-agent-justification): message, evidence, and dry_run fields land on
// Body, and Evidence decodes via its UnmarshalJSON union check.
func TestActionRequest_RoundTripWithEvidence(t *testing.T) {
	in := `{
      "actor": "wesm",
      "reason": "done",
      "message": "Fixed Safari callback double-submit.",
      "evidence": [{"type":"commit","sha":"abc1234"}],
      "dry_run": false
    }`
	var req api.ActionRequest
	require.NoError(t, json.Unmarshal([]byte(in), &req.Body))
	assert.Equal(t, "done", req.Body.Reason)
	assert.Equal(t, "Fixed Safari callback double-submit.", req.Body.Message)
	require.Len(t, req.Body.Evidence, 1)
	assert.Equal(t, api.EvidenceCommit, req.Body.Evidence[0].Type)
}

// TestMetadataKeyConstantsMirrorRegistry verifies that every api-package
// metadata key constant resolves to a server-reserved registry entry,
// catching any typos or registry/constant skew at test time.
func TestMetadataKeyConstantsMirrorRegistry(t *testing.T) {
	for _, k := range []string{
		api.MetadataKeyScheduledOn, api.MetadataKeyDeadlineOn, api.MetadataKeySomeday,
		api.MetadataKeyChecklist, api.MetadataKeyTimezone,
	} {
		_, ok := metadata.IssueRegistry[k]
		assert.True(t, ok, "api constant %q absent from issue registry", k)
	}
	for _, k := range []string{
		api.ProjectMetadataKeyArea,
	} {
		_, ok := metadata.ProjectRegistry[k]
		assert.True(t, ok, "api constant %q absent from project registry", k)
	}
}

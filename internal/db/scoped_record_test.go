package db

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/embedding"
)

func recordEnvelope(t *testing.T, e ScopedEffect, subject, peerProject, peer *string) ScopedRecordExport {
	t.Helper()
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	project, err := ScopedEffectProjectUID(e)
	require.NoError(t, err)
	return ScopedRecordExport{ViewUID: verificationID(20), Generation: 1, Collection: e.Collection, RecordKey: e.Key, ProjectUID: project, SubjectUID: subject, PeerProjectUID: peerProject, PeerUID: peer, VisibleRevision: e.Revision, Present: e.Action == "replace", RecordBytes: raw, Digest: ScopedOfferedDigest(raw)}
}
func recordPointer(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func TestScopedRecordIndexesAndEnvelope(t *testing.T) {
	p, q, i, j, entity := verificationID(1), verificationID(2), verificationID(40), verificationID(41), verificationID(42)
	for _, tc := range []struct {
		effect                     ScopedEffect
		subject, peerProject, peer string
	}{
		{ScopedEffect{Collection: "projects", Key: p, Project: &ScopedProjectState{UID: p}}, "", "", ""},
		{ScopedEffect{Collection: "issues", Key: p + ":" + i, Issue: &ScopedIssueState{ProjectUID: p, UID: i}}, i, "", ""},
		{ScopedEffect{Collection: "comments", Key: p + ":" + entity, Comment: &ScopedCommentState{ProjectUID: p, UID: entity, IssueUID: i}}, i, "", ""},
		{ScopedEffect{Collection: "labels", Key: p + ":" + i + ":label:with:colon", Label: &ScopedLabelState{ProjectUID: p, IssueUID: i, Label: "label:with:colon"}}, i, "", ""},
		{ScopedEffect{Collection: "definitions", Key: p + ":job:" + entity, Definition: &ScopedDefinitionState{ProjectUID: p, UID: entity, Kind: "job", Job: &cron.JobDefinition{Version: 1, Kind: "job"}}}, entity, "", ""},
		{ScopedEffect{Collection: "references", Key: p + ":" + i + "::close", Reference: &ScopedReference{Slot: "close", Kind: "close", Subject: ScopedEndpoint{p, i}, Present: true, Close: &ScopedCloseValue{Kind: "duplicate", PeerProjectUID: q, PeerIssueUID: j}, Revision: 1}}, i, q, j},
		{ScopedEffect{Collection: "references", Key: p + ":" + i + "::parent", Reference: &ScopedReference{Slot: "parent", Kind: "parent", Subject: ScopedEndpoint{p, i}, Revision: 1}}, i, "", ""},
		{ScopedEffect{Collection: "references", Key: p + ":" + i + ":" + entity + ":reply", Reference: &ScopedReference{Slot: "reply", Kind: "reply", Subject: ScopedEndpoint{p, i}, SubjectEntityUID: entity, Present: true, Peer: &ScopedEndpoint{q, j}, PeerEntityUID: verificationID(43), Relation: "reply", Revision: 1}}, i, q, j},
		{ScopedEffect{Collection: "references", Key: p + "::" + entity + ":workflow", Reference: &ScopedReference{Slot: "workflow", Kind: "definition", Subject: ScopedEndpoint{ProjectUID: p}, SubjectEntityUID: entity, Present: true, Peer: &ScopedEndpoint{ProjectUID: q}, PeerEntityUID: j, Relation: "workflow", Revision: 1}}, entity, q, j},
		{ScopedEffect{Collection: "provenance", Key: p + ":issue:" + i, Provenance: &ScopedProvenance{ProjectUID: p, Kind: "issue", EntityUID: i}}, i, "", ""},
		{ScopedEffect{Collection: "artifacts", Key: p + ":" + ScopedOfferedDigest([]byte("artifact")), Artifact: &embedding.ArtifactManifest{ArtifactIdentity: embedding.ArtifactIdentity{ProjectUID: p, IssueUID: i}, Digest: ScopedOfferedDigest([]byte("artifact"))}}, i, "", ""},
	} {
		t.Run(tc.effect.Key, func(t *testing.T) {
			effect := tc.effect
			effect.Revision = 1
			effect.Action = "replace"
			subject, peerProject, peer, err := ScopedRecordIndexes(effect, nil)
			require.NoError(t, err)
			require.Equal(t, recordPointer(tc.subject), subject)
			require.Equal(t, recordPointer(tc.peerProject), peerProject)
			require.Equal(t, recordPointer(tc.peer), peer)
			row := recordEnvelope(t, effect, subject, peerProject, peer)
			require.NoError(t, ValidateScopedRecordEnvelope(row))
			require.NoError(t, ValidateScopedImportRecord(&row))
			if effect.Reference != nil && !effect.Reference.Present {
				require.True(t, row.Present, "nullable absent replacement remains a retained slot")
			}
			row.Present = false
			require.Error(t, ValidateScopedRecordEnvelope(row))
			row.Present = true
			row.RecordBytes = append(row.RecordBytes, ' ')
			require.Error(t, ValidateScopedRecordEnvelope(row), "exact bytes digest, no normalization")
		})
	}
}

func TestScopedRecordRemovalRetainsHistoricalHints(t *testing.T) {
	p, q, i, j, entity := verificationID(1), verificationID(2), verificationID(40), verificationID(41), verificationID(42)
	for _, tc := range []struct{ collection, key, subject string }{
		{"projects", p, ""}, {"issues", p + ":" + i, i}, {"comments", p + ":" + entity, i}, {"labels", p + ":" + i + ":example", i}, {"definitions", p + ":workflow:" + entity, entity}, {"references", p + ":" + i + "::parent", i}, {"references", p + "::" + entity + ":workflow", entity}, {"provenance", p + ":issue:" + i, i}, {"artifacts", p + ":" + ScopedOfferedDigest(nil), i},
	} {
		t.Run(tc.collection+tc.key, func(t *testing.T) {
			effect := ScopedEffect{Collection: tc.collection, Key: tc.key, Revision: 2, Action: "remove"}
			prior := ScopedRecordExport{Collection: tc.collection, RecordKey: tc.key, ProjectUID: p, SubjectUID: recordPointer(tc.subject)}
			if tc.collection == "references" {
				prior.PeerProjectUID = &q
				prior.PeerUID = &j
			}
			subject, peerProject, peer, err := ScopedRecordIndexes(effect, &prior)
			require.NoError(t, err)
			require.Equal(t, prior.SubjectUID, subject)
			require.Equal(t, prior.PeerProjectUID, peerProject)
			require.Equal(t, prior.PeerUID, peer)
			row := recordEnvelope(t, effect, subject, peerProject, peer)
			require.NoError(t, ValidateScopedRecordEnvelope(row))
			require.NoError(t, ValidateScopedImportRecord(&row))
			noPriorSubject, noPriorProject, noPriorPeer, err := ScopedRecordIndexes(effect, nil)
			require.NoError(t, err)
			require.Nil(t, noPriorProject)
			require.Nil(t, noPriorPeer)
			if tc.collection == "comments" || tc.collection == "artifacts" {
				require.Nil(t, noPriorSubject)
			} else {
				require.Equal(t, prior.SubjectUID, noPriorSubject)
			}
			prior.ProjectUID = q
			_, _, _, err = ScopedRecordIndexes(effect, &prior)
			require.Error(t, err, "foreign prior cannot donate index authority")
			row.SubjectUID = recordPointer("alias")
			require.Error(t, ValidateScopedRecordEnvelope(row))
		})
	}
}

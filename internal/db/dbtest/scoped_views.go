package dbtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	"slices"
	"strings"
	"testing"
	"time"
)

type ScopedViewsFixture struct {
	Root       ScopedCaptureFixture
	Checkpoint db.ScopedCheckpoint
	Records    []db.ImportRecord
	Signers    db.RootSigners
}

func NewScopedViewsFixture(t *testing.T, root db.Storage, seed ...func(ScopedCaptureFixture)) ScopedViewsFixture {
	t.Helper()
	f := NewScopedCaptureFixture(t, root)
	for _, prepare := range seed {
		prepare(f)
	}
	signers := db.RootSigners{}
	for _, p := range f.Params.View.Projects {
		signers[p.ProjectUID] = ScopedCaptureSigner(t, f, p.ProjectUID)
	}
	return NewScopedViewsFixtureFromCapture(t, root, f, signers)
}

func NewScopedViewsFixtureFromCapture(t *testing.T, root db.Storage, f ScopedCaptureFixture, signers db.RootSigners) ScopedViewsFixture {
	t.Helper()
	_, err := root.RenameProject(t.Context(), f.Projects[0].ID, "alpha")
	require.NoError(t, err)
	_, err = root.PatchProjectMetadata(t.Context(), db.PatchProjectMetadataIn{ProjectID: f.Projects[0].ID, Actor: "member", Patch: map[string]jsontext.Value{"example.project": jsontext.Value(`"signed"`)}})
	require.NoError(t, err)
	capture, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err)
	collections, err := db.ProjectScopedState(capture, f.Params.View)
	require.NoError(t, err)
	history := []db.RootKeyTransition{}
	for _, selected := range f.Params.View.Projects {
		transitions, err := root.RootKeyTransitions(t.Context(), selected.ProjectUID)
		require.NoError(t, err)
		history = append(history, transitions...)
	}
	checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: f.Params.View, Cut: db.ScopedCut{Sequence: 1, CaptureUID: capture.CaptureUID}, Collections: collections, KeyHistory: history}, signers)
	require.NoError(t, err)
	result := ScopedViewsFixture{Root: f, Checkpoint: checkpoint, Signers: signers}
	// Ordinary normalized rows and enrolled roots come from the actual native
	// snapshot. Deliberately do not copy root source history or its grant/token.
	result.Records = append(result.Records, &db.MetaKV{Key: "instance_uid", Value: checkpoint.View.DestinationUID})
	for _, record := range scopedAuthoritySnapshot(t, root) {
		switch r := record.(type) {
		case *db.MetaKV:
			for _, selected := range f.Params.View.Projects {
				if strings.HasPrefix(r.Key, db.RootKeyTransitionMetadataPrefix+selected.ProjectUID+".") {
					result.Records = append(result.Records, record)
				}
			}
		case *db.ProjectExport:
			copy := *r
			if r.UID == f.Projects[0].UID {
				copy.Name = "local-alpha"
			}
			result.Records = append(result.Records, &copy)
		case *db.IssueExport, *db.CommentExport, *db.IssueLabelExport, *db.LinkExport, *db.RootKeyPin, *db.ScopedProtocolCutoverExport:
			result.Records = append(result.Records, record)
		}
	}
	key := checkpoint.View.Key
	hop := checkpoint.View.Route[0]
	result.Records = append(result.Records,
		&db.ScopedViewExport{ViewUID: key.ViewUID, CatalogEntryUID: f.Params.CatalogEntryUID, AuthorityUID: checkpoint.View.AuthorityUID, BindingUID: hop.BindingUID, Epoch: hop.Epoch, AccountableActor: f.Parent.Actor, RouteDigest: checkpoint.View.RouteDigest, DestinationUID: checkpoint.View.DestinationUID, ActiveGeneration: &key.Generation, State: "active", RequestDigest: checkpoint.View.RequestDigest},
		&db.ScopedGenerationExport{ViewUID: key.ViewUID, Generation: key.Generation, DescriptorBytes: scopedOwnerBytes(t, checkpoint.View), DescriptorDigest: db.ScopedOfferedDigest(scopedOwnerBytes(t, checkpoint.View)), CheckpointBytes: scopedOwnerBytes(t, checkpoint), CheckpointDigest: &checkpoint.Cut.Digest, CutSequence: checkpoint.Cut.Sequence, CutDigest: checkpoint.Cut.Digest, State: "active", AssembledBytes: int64(len(scopedOwnerBytes(t, checkpoint))), RootCutoverUID: checkpoint.View.RootCutoverUID},
		&db.ScopedWorkExport{ViewUID: key.ViewUID, DirtyRevision: 1, State: "idle"})
	for _, p := range checkpoint.View.Projects {
		result.Records = append(result.Records, &db.ScopedProjectExport{ProjectUID: p.ProjectUID, OwnerViewUID: &key.ViewUID, OwnerGeneration: &key.Generation, ProtocolFloor: 2, RootCutoverUID: &checkpoint.View.RootCutoverUID})
	}
	effects, err := db.ScopedCheckpointRecordEffects(checkpoint)
	require.NoError(t, err)
	for _, e := range effects {
		project, err := db.ScopedEffectProjectUID(e)
		require.NoError(t, err)
		subject, peerProject, peer, err := db.ScopedRecordIndexes(e, nil)
		require.NoError(t, err)
		raw := scopedOwnerBytes(t, e)
		result.Records = append(result.Records, &db.ScopedRecordExport{ViewUID: key.ViewUID, Generation: key.Generation, Collection: e.Collection, RecordKey: e.Key, ProjectUID: project, SubjectUID: subject, PeerProjectUID: peerProject, PeerUID: peer, VisibleRevision: e.Revision, Present: true, RecordBytes: raw, Digest: db.ScopedOfferedDigest(raw)})
	}
	require.NoError(t, db.ValidateImportRecords(result.Records))
	return result
}

func RunScopedOwnerRestoreReadModelAtomic(t *testing.T, open func(*testing.T) db.Storage) {
	root := open(t)
	f := NewScopedViewsFixture(t, root, func(capture ScopedCaptureFixture) {
		_, _, err := root.AddLabelAndEvent(t.Context(), capture.Issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "current-label", Actor: "member"})
		require.NoError(t, err)
	})
	t.Run("local_alias_positive", func(t *testing.T) {
		target := open(t)
		require.NoError(t, target.ImportReplay(t.Context(), f.Records, db.ImportOptions{}))
		p, err := target.ProjectByUID(t.Context(), f.Root.Projects[0].UID)
		require.NoError(t, err)
		require.Equal(t, "local-alpha", p.Name)
		require.Equal(t, "alpha", f.Checkpoint.Collections.Projects[0].Name)
		require.JSONEq(t, string(f.Checkpoint.Collections.Projects[0].Metadata), string(p.Metadata), "routing alias does not replace authenticated project metadata")
		issue, err := target.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, f.Root.Issue.Title, issue.Title)
		labels, err := target.LabelsByIssue(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Len(t, labels, 1)
		require.Equal(t, "current-label", labels[0].Label)
		links, err := target.LinksByIssue(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Len(t, links, 1)
		require.Equal(t, "blocks", links[0].Type)
	})
	// Every active native collection must corroborate its retained signed view.
	// Change only normalized rows; the authenticated baseline remains exact.
	for _, kind := range []string{"issue_title_tamper", "project_metadata_tamper", "label_tamper", "label_omission", "link_type_tamper", "link_omission"} {
		t.Run(kind, func(t *testing.T) {
			target := open(t)
			require.NoError(t, target.ImportReplay(t.Context(), f.Records, db.ImportOptions{}))
			before := scopedAuthoritySnapshot(t, target)
			instance := target.InstanceUID()
			changed := make([]db.ImportRecord, 0, len(f.Records))
			mutations := 0
			for _, r := range f.Records {
				omit := false
				switch v := r.(type) {
				case *db.IssueExport:
					if kind == "issue_title_tamper" && v.UID == f.Root.Issue.UID {
						copy := *v
						copy.Title = "tampered-title"
						r = &copy
						mutations++
					}
				case *db.ProjectExport:
					if kind == "project_metadata_tamper" && v.UID == f.Root.Projects[0].UID {
						copy := *v
						copy.Metadata = jsontext.Value(`{"example":{"project":"tampered"}}`)
						r = &copy
						mutations++
					}
				case *db.IssueLabelExport:
					if kind == "label_tamper" || kind == "label_omission" {
						copy := *v
						copy.Label = "other-label"
						r = &copy
						omit = kind == "label_omission"
						mutations++
					}
				case *db.LinkExport:
					if kind == "link_type_tamper" || kind == "link_omission" {
						copy := *v
						copy.Type = "related"
						r = &copy
						omit = kind == "link_omission"
						mutations++
					}
				}
				if !omit {
					changed = append(changed, r)
				}
			}
			require.Equal(t, 1, mutations, "exactly one native row changes; signed projection bytes remain untouched")
			require.NoError(t, db.ValidateImportRecords(changed), "A1 remains valid: signed records/checkpoint/pins and exact bytes unchanged")
			err := target.ImportReplay(t.Context(), changed, db.ImportOptions{})
			require.Error(t, err, "native A2 mismatch must reject inside replacement transaction")
			require.Equal(t, instance, target.InstanceUID())
			require.Equal(t, before, scopedAuthoritySnapshot(t, target), "failed native join leaves full populated target unchanged")
		})
	}
	t.Run("root_unowned_canonical", func(t *testing.T) {
		root := open(t)
		native := NewScopedCaptureFixture(t, root)
		records := scopedAuthoritySnapshot(t, root)
		require.NoError(t, root.ImportReplay(t.Context(), records, db.ImportOptions{}))
		issue, err := root.IssueByUID(t.Context(), native.Issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, native.Issue.Title, issue.Title, "outgoing prepared projection does not replace canonical root content")
	})
}

func InstallScopedViewsFixture(t *testing.T, target db.Storage, f ScopedViewsFixture) [2]db.Project {
	t.Helper()
	return InstallScopedViewsFixtureAt(t, target, f, "https://hub.example")
}

func InstallScopedViewsFixtureAt(t *testing.T, target db.Storage, f ScopedViewsFixture, origin string, downstream ...bool) [2]db.Project {
	t.Helper()
	require.NoError(t, target.ImportReplay(t.Context(), f.Records, db.ImportOptions{}))
	include := db.IncludeDeletedNo
	for _, current := range f.Checkpoint.Collections.Issues {
		if current.UID == f.Root.Issue.UID && current.Deleted {
			include = db.IncludeDeletedYes
		}
	}
	issue, err := target.IssueByUID(t.Context(), f.Root.Issue.UID, include)
	require.NoError(t, err)
	importedLinks, err := target.LinksByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotEmpty(t, importedLinks, "signed fixture's normalized edges survive raw owner restore")
	team, _, err := target.CreateTeam(t.Context(), "view-local-team", "admin")
	require.NoError(t, err)
	_, _, err = target.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "view-local-token", Actor: "local-member", AdminActor: "admin", TeamUIDs: []string{team.UID}})
	require.NoError(t, err)
	var projects [2]db.Project
	for n, p := range f.Checkpoint.View.Projects {
		projects[n], err = target.ProjectByUID(t.Context(), p.ProjectUID)
		require.NoError(t, err)
		_, _, err = target.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: p.ProjectUID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
		require.NoError(t, err)
		_, err = target.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: projects[n].ID, Role: db.FederationRoleSpoke, HubURL: origin, HubProjectID: f.Root.Projects[n].ID, HubProjectUID: p.ProjectUID, PushEnabled: true, Actor: f.Root.Parent.Actor, Enabled: true})
		require.NoError(t, err)
	}
	InstallScopedIncomingAuthority(t, target, f.Checkpoint.View, projects, downstream...)
	// The legacy binding installer reconciles links from root event history.
	// This owner fixture intentionally transfers no such history. Finish the
	// normalized restore after installing actual local authority, preserving
	// the resulting native credentials, policy and binding/config rows.
	records := scopedAuthoritySnapshot(t, target)
	linkIDs := map[int64]bool{}
	for _, record := range records {
		if link, ok := record.(*db.LinkExport); ok {
			linkIDs[link.ID] = true
		}
	}
	for _, record := range f.Records {
		if link, ok := record.(*db.LinkExport); ok && !linkIDs[link.ID] {
			records = append(records, link)
		}
	}
	require.NoError(t, target.ImportReplay(t.Context(), records, db.ImportOptions{}))
	issue, err = target.IssueByUID(t.Context(), f.Root.Issue.UID, include)
	require.NoError(t, err)
	for n, pin := range f.Checkpoint.View.Projects {
		projects[n], err = target.ProjectByUID(t.Context(), pin.ProjectUID)
		require.NoError(t, err)
	}
	restoredLinks, err := target.LinksByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, importedLinks, restoredLinks, "fixture setup preserves the exact restored native edges")
	return projects
}

type ScopedViewsOperations interface {
	PrepareScopedCheckpoint(context.Context, db.ScopedCheckpoint) error
	ActivateScopedCheckpoint(context.Context, db.ScopedViewKey, string) error
}

func scopedViewsOperations(t *testing.T, s db.Storage) ScopedViewsOperations {
	t.Helper()
	operations, ok := s.(ScopedViewsOperations)
	require.True(t, ok, "native T06 prepare/activate methods must be concrete")
	return operations
}

func ScopedViewsPreparedGeneration(t *testing.T, s db.Storage, view db.ScopedViewDescriptor) {
	t.Helper()
	records := scopedAuthoritySnapshot(t, s)
	found := false
	for _, r := range records {
		if row, ok := r.(*db.ScopedViewExport); ok && row.ViewUID == view.Key.ViewUID {
			row.PreparedGeneration = &view.Key.Generation
			row.RequestDigest = view.RequestDigest
			found = true
		}
	}
	require.True(t, found)
	raw := scopedOwnerBytes(t, view)
	records = append(records, &db.ScopedGenerationExport{ViewUID: view.Key.ViewUID, Generation: view.Key.Generation, DescriptorBytes: raw, DescriptorDigest: db.ScopedOfferedDigest(raw), State: "prepared", RootCutoverUID: view.RootCutoverUID})
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
}

func RunScopedAtomicBaselineAndReferenceCoverage(t *testing.T, open func(*testing.T) db.Storage) {
	root, receiver := open(t), open(t)
	f := NewScopedViewsFixture(t, root)
	issuer := scopedViewsOperations(t, root)
	target := scopedViewsOperations(t, receiver)
	before, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	invalid := f.Checkpoint
	invalid.Collections.References = nil
	require.Error(t, issuer.PrepareScopedCheckpoint(t.Context(), invalid), "missing collection must not mean empty")
	require.NoError(t, issuer.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
	require.NoError(t, issuer.PrepareScopedCheckpoint(t.Context(), f.Checkpoint), "exact preparation retry")
	require.Error(t, issuer.ActivateScopedCheckpoint(t.Context(), f.Checkpoint.View.Key, strings.Repeat("0", 64)))
	require.NoError(t, issuer.ActivateScopedCheckpoint(t.Context(), f.Checkpoint.View.Key, f.Checkpoint.Cut.Digest))
	after, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, before, after, "outgoing root publication leaves canonical rows canonical")
	require.Nil(t, ScopedCaptureWork(t, root, f.Checkpoint.View.Key.ViewUID).CaptureUID, "published signed baseline independently retains its capture result")
	projects := InstallScopedViewsFixture(t, receiver, f)
	receiverIssue, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := receiver.LinksByIssue(t.Context(), receiverIssue.ID)
	require.NoError(t, err)
	require.NotEmpty(t, links)
	// Ordinary root writers arrange the next literal current issue and remove
	// the old edge before another real immutable native capture.
	title := "Updated signed title"
	_, err = root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
	require.NoError(t, err)
	rootLinks, err := root.LinksByIssue(t.Context(), f.Root.Issue.ID)
	require.NoError(t, err)
	for _, link := range rootLinks {
		require.NoError(t, root.DeleteLinkByID(t.Context(), link.ID))
	}
	capture, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Checkpoint.View.Key)
	require.NoError(t, err)
	current, err := db.ProjectScopedState(capture, f.Checkpoint.View)
	require.NoError(t, err)
	view := f.Checkpoint.View
	view.Key.Generation++
	view.RequestDigest = strings.Repeat("4", 64)
	// The wire contract allows an explicitly empty complete reference collection.
	// This is distinct from missing/null, and must clear all prior native edges.
	current.References = []db.ScopedReference{}
	checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: view, Cut: db.ScopedCut{Sequence: 1, CaptureUID: capture.CaptureUID}, Collections: current, KeyHistory: []db.RootKeyTransition{}}, f.Signers)
	require.NoError(t, err)
	ScopedViewsPreparedGeneration(t, receiver, view)
	cursor := scopedViewsCursor(t, receiver)
	events, err := receiver.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	require.NoError(t, target.PrepareScopedCheckpoint(t.Context(), checkpoint))
	require.Equal(t, cursor, scopedViewsCursor(t, receiver), "preparation alone is not a visible current change")
	old, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Issue.Title, old.Title, "preparation never writes serving native content")
	require.NoError(t, target.ActivateScopedCheckpoint(t.Context(), view.Key, checkpoint.Cut.Digest))
	require.Greater(t, scopedViewsCursor(t, receiver), cursor, "native scoped current changes invalidate private cache context atomically")
	afterEvents, err := receiver.EventsAfter(t.Context(), db.EventsAfterParams{Limit: 1000})
	require.NoError(t, err)
	require.Equal(t, events, afterEvents, "existing attribution reset reserves cursor identity without a fabricated event row")
	installed, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, title, installed.Title)
	links, err = receiver.LinksByIssue(t.Context(), installed.ID)
	require.NoError(t, err)
	require.Empty(t, links)
	project, err := receiver.ProjectByUID(t.Context(), projects[0].UID)
	require.NoError(t, err)
	require.Equal(t, "local-alpha", project.Name, "baseline install must preserve the local catalog alias")
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)), "entity/reference/provenance and owner graph install together")
}

func RunScopedCheckpointBytes(t *testing.T, root db.Storage) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	encoded, ok := root.(interface {
		PrepareScopedCheckpointBytes(context.Context, []byte) error
	})
	require.True(t, ok, "native exact-body checkpoint preparation must be concrete")
	producer := scopedViewsOperations(t, root)
	compact := scopedOwnerBytes(t, f.Checkpoint)
	body := append([]byte(" \n\t"), compact...)
	body = append(body, '\n', ' ')
	require.NotEqual(t, compact, body)
	require.Error(t, db.ValidateScopedSignedBytes(body), "literal outer body is deliberately noncanonical")
	var decoded db.ScopedCheckpoint
	require.NoError(t, db.DecodeScopedRecord(body, &decoded))
	pins := []db.RootKeyPin{}
	for _, project := range f.Checkpoint.View.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedCheckpoint(decoded, f.Checkpoint.View, pins), "unchanged signed content authenticates independently of outer whitespace")

	before := scopedAuthoritySnapshot(t, root)
	unknown := append([]byte(`{"unknown":true,`), compact[1:]...)
	require.Error(t, encoded.PrepareScopedCheckpointBytes(t.Context(), unknown), "encoded entrypoint shares closed-shape validation")
	require.Equal(t, before, scopedAuthoritySnapshot(t, root))
	require.NoError(t, encoded.PrepareScopedCheckpointBytes(t.Context(), body))
	require.NoError(t, encoded.PrepareScopedCheckpointBytes(t.Context(), body), "exact body retry")
	prepared := scopedAuthoritySnapshot(t, root)
	generations := 0
	for _, record := range prepared {
		if generation, ok := record.(*db.ScopedGenerationExport); ok && generation.ViewUID == f.Checkpoint.View.Key.ViewUID && generation.Generation == f.Checkpoint.View.Key.Generation {
			generations++
			require.Equal(t, body, []byte(generation.CheckpointBytes), "retain the actual input, not a typed rendering")
			require.Equal(t, int64(len(body)), generation.AssembledBytes)
		}
	}
	require.Equal(t, 1, generations)
	changed := append([]byte("\n"), body...)
	require.NoError(t, db.DecodeScopedRecord(changed, &decoded))
	require.Equal(t, f.Checkpoint.Cut.Digest, decoded.Cut.Digest, "changed outer bytes leave signed content identity unchanged")
	require.ErrorIs(t, encoded.PrepareScopedCheckpointBytes(t.Context(), changed), db.ErrRemoteEventConflict, "same key cannot replace retained body bytes")
	require.ErrorIs(t, producer.PrepareScopedCheckpoint(t.Context(), f.Checkpoint), db.ErrRemoteEventConflict, "typed convenience uses the same keyed preparation core")
	require.Equal(t, prepared, scopedAuthoritySnapshot(t, root), "both rejected replacements preserve all retained rows")

	require.NoError(t, producer.ActivateScopedCheckpoint(t.Context(), f.Checkpoint.View.Key, f.Checkpoint.Cut.Digest))
	active := scopedAuthoritySnapshot(t, root)
	offers := 0
	for _, record := range active {
		switch row := record.(type) {
		case *db.ScopedGenerationExport:
			if row.ViewUID == f.Checkpoint.View.Key.ViewUID && row.Generation == f.Checkpoint.View.Key.Generation {
				require.Equal(t, body, []byte(row.CheckpointBytes))
			}
		case *db.ScopedDeliveryRecord:
			if row.ObjectUID != f.Checkpoint.View.Key.ViewUID || row.ObjectDigest != f.Checkpoint.Cut.Digest {
				continue
			}
			offers++
			header, forwarded, err := db.DecodeScopedDelivery(row.EnvelopeBytes)
			require.NoError(t, err)
			require.Equal(t, body, forwarded, "initial offer forwards original authenticated body bytes")
			require.Equal(t, db.ScopedOfferedDigest(body), header.BodyDigest)
			require.Equal(t, db.ScopedOfferedDigest(row.EnvelopeBytes), row.OfferedDigest)
		}
	}
	require.Equal(t, 1, offers)
	require.NoError(t, producer.ActivateScopedCheckpoint(t.Context(), f.Checkpoint.View.Key, f.Checkpoint.Cut.Digest))
	require.Equal(t, active, scopedAuthoritySnapshot(t, root), "activation retry cannot allocate another offer")

	_, _, err := root.RevokeAPIToken(t.Context(), f.Root.Parent.ID, "admin")
	require.NoError(t, err)
	revoked := scopedAuthoritySnapshot(t, root)
	require.Error(t, encoded.PrepareScopedCheckpointBytes(t.Context(), body), "retained bytes never replace independent current native authority")
	require.Equal(t, revoked, scopedAuthoritySnapshot(t, root))
}

func ScopedViewsSignedBatch(t *testing.T, fixture ScopedViewsFixture, current db.ScopedCollections, sequence int64, previousDigest string) db.ScopedBatch {
	t.Helper()
	effects, err := db.DiffScopedState(fixture.Checkpoint.Collections, current, sequence)
	require.NoError(t, err)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: fixture.Checkpoint.View, Cut: db.ScopedCut{Sequence: sequence, CaptureUID: scopedOwnerUID(9800)}, PreviousDigest: previousDigest, Effects: effects}, fixture.Signers)
	require.NoError(t, err)
	return batch
}

func RunScopedApplyBatch(t *testing.T, root, receiver db.Storage) {
	t.Helper()
	fixture := NewScopedViewsFixture(t, root)
	apply, ok := receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	})
	require.True(t, ok, "native atomic signed batch apply must be concrete")
	projects := InstallScopedViewsFixture(t, receiver, fixture)
	current, err := db.RestrictScopedState(fixture.Checkpoint.Collections, nil, nil)
	require.NoError(t, err)
	changed := false
	for n := range current.Issues {
		if current.Issues[n].UID == fixture.Root.Issue.UID {
			current.Issues[n].Title = "Signed batch title"
			changed = true
		}
	}
	require.True(t, changed)
	batch := ScopedViewsSignedBatch(t, fixture, current, fixture.Checkpoint.Cut.Sequence+1, fixture.Checkpoint.Cut.Digest)
	cursor := scopedViewsCursor(t, receiver)
	rootBefore, err := root.IssueByUID(t.Context(), fixture.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NoError(t, apply.ApplyScopedBatch(t.Context(), batch))
	issue, err := receiver.IssueByUID(t.Context(), fixture.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Signed batch title", issue.Title)
	require.Equal(t, projects[0].UID, issue.ProjectUID)
	appliedCursor := scopedViewsCursor(t, receiver)
	require.Greater(t, appliedCursor, cursor, "actual current change reserves the existing private cache cursor")
	after := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(after))
	require.NoError(t, apply.ApplyScopedBatch(t.Context(), batch), "exact signed batch retry")
	require.Equal(t, appliedCursor, scopedViewsCursor(t, receiver))
	require.Equal(t, after, scopedAuthoritySnapshot(t, receiver), "retry changes no native rows, retained bytes or cursor")
	for n := range current.Issues {
		if current.Issues[n].UID == fixture.Root.Issue.UID {
			current.Issues[n].Title = "Conflicting signed batch title"
		}
	}
	conflict := ScopedViewsSignedBatch(t, fixture, current, batch.Cut.Sequence, batch.PreviousDigest)
	require.NotEqual(t, batch.Cut.Digest, conflict.Cut.Digest)
	require.ErrorIs(t, apply.ApplyScopedBatch(t.Context(), conflict), db.ErrRemoteEventConflict)
	require.Equal(t, after, scopedAuthoritySnapshot(t, receiver), "keyed conflict rolls back the complete target")
	rootAfter, err := root.IssueByUID(t.Context(), fixture.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, rootBefore, rootAfter, "incoming normalized apply never mutates canonical root content")
}

// Scoped apply must preserve the existing native content-feed identity and
// notify project-filtered readers at both ends of a changed relationship.
func RunScopedApplyCurrentObservers(t *testing.T, open func(*testing.T) db.Storage) {
	for _, mode := range []string{"label", "title", "edge_withdrawal"} {
		t.Run(mode, func(t *testing.T) {
			root, receiver := open(t), open(t)
			f := NewScopedViewsFixture(t, root)
			projects := InstallScopedViewsFixture(t, receiver, f)
			operations := scopedViewsOperations(t, root)
			require.NoError(t, operations.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
			require.NoError(t, operations.ActivateScopedCheckpoint(t.Context(), f.Checkpoint.View.Key, f.Checkpoint.Cut.Digest))
			before, err := receiver.ListIssueContent(t.Context(), 0, 100)
			require.NoError(t, err)
			require.Len(t, before, 2)
			cursor := scopedViewsCursor(t, receiver)
			switch mode {
			case "label":
				_, _, err = root.AddLabelAndEvent(t.Context(), f.Root.Issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "observer-label", Actor: "member"})
			case "title":
				title := "Changed native observer text"
				_, err = root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
			case "edge_withdrawal":
				links, lookupErr := root.LinksByIssue(t.Context(), f.Root.Issue.ID)
				require.NoError(t, lookupErr)
				require.Len(t, links, 1)
				peer, lookupErr := root.IssueByUID(t.Context(), links[0].ToIssueUID, db.IncludeDeletedNo)
				require.NoError(t, lookupErr)
				_, err = root.DeleteLinkAndEvent(t.Context(), links[0], db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: f.Root.Issue.ID, FromShortID: f.Root.Issue.ShortID, FromUID: f.Root.Issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "member"})
			}
			require.NoError(t, err)
			capture, err := root.(interface {
				CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
			}).CaptureScopedView(t.Context(), f.Checkpoint.View.Key)
			require.NoError(t, err)
			current, err := db.ProjectScopedState(capture, f.Checkpoint.View)
			require.NoError(t, err)
			effects, err := db.DiffScopedState(f.Checkpoint.Collections, current, f.Checkpoint.Cut.Sequence+1)
			require.NoError(t, err)
			require.NotEmpty(t, effects)
			batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: f.Checkpoint.Cut.Sequence + 1, CaptureUID: capture.CaptureUID}, PreviousDigest: f.Checkpoint.Cut.Digest, Effects: effects}, f.Signers)
			require.NoError(t, err)
			require.NoError(t, root.(interface {
				CommitScopedBatch(context.Context, db.ScopedBatch, string) error
			}).CommitScopedBatch(t.Context(), batch, capture.CaptureUID))
			require.NoError(t, receiver.(interface {
				ApplyScopedBatch(context.Context, db.ScopedBatch) error
			}).ApplyScopedBatch(t.Context(), batch))
			if mode == "edge_withdrawal" {
				for _, project := range projects {
					reset, err := receiver.PurgeResetCheck(t.Context(), cursor, project.ID)
					require.NoError(t, err)
					require.Greater(t, reset, cursor, "both endpoint projects must observe the withdrawn edge")
				}
				return
			}
			after, err := receiver.ListIssueContent(t.Context(), 0, 100)
			require.NoError(t, err)
			require.Len(t, after, len(before))
			for n, issue := range after {
				require.Equal(t, before[n].UID, issue.UID)
				want := before[n].ContentRevision
				if issue.Title != before[n].Title || issue.Body != before[n].Body {
					want++
				}
				require.Equal(t, want, issue.ContentRevision, "native vector-feed identity changes only with that issue's text")
			}
		})
	}
}

func RunScopedApplyBatchBytes(t *testing.T, root, receiver db.Storage) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	encoded, ok := receiver.(interface {
		ApplyScopedBatchBytes(context.Context, []byte) error
	})
	require.True(t, ok, "native exact-body batch apply must be concrete")
	InstallScopedViewsFixture(t, receiver, f)
	current, err := db.RestrictScopedState(f.Checkpoint.Collections, nil, nil)
	require.NoError(t, err)
	changed := false
	for n := range current.Issues {
		if current.Issues[n].UID == f.Root.Issue.UID {
			current.Issues[n].Title = "Encoded signed batch title"
			changed = true
		}
	}
	require.True(t, changed)
	batch := ScopedViewsSignedBatch(t, f, current, f.Checkpoint.Cut.Sequence+1, f.Checkpoint.Cut.Digest)
	compact := scopedOwnerBytes(t, batch)
	body := append([]byte(" \n\t"), compact...)
	body = append(body, '\n', ' ')
	require.Error(t, db.ValidateScopedSignedBytes(body), "literal incoming body is deliberately noncanonical")
	var decoded db.ScopedBatch
	require.NoError(t, db.DecodeScopedRecord(body, &decoded))
	pins := []db.RootKeyPin{}
	for _, project := range f.Checkpoint.View.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedBatch(decoded, f.Checkpoint.View, pins), "outer whitespace does not change authenticated content")
	rootBefore := scopedAuthoritySnapshot(t, root)
	before := scopedAuthoritySnapshot(t, receiver)
	cursor := scopedViewsCursor(t, receiver)
	unknown := append([]byte(`{"unknown":true,`), compact[1:]...)
	require.Error(t, encoded.ApplyScopedBatchBytes(t.Context(), unknown), "same closed-shape validation")
	require.Equal(t, before, scopedAuthoritySnapshot(t, receiver))
	require.Equal(t, cursor, scopedViewsCursor(t, receiver))
	require.NoError(t, encoded.ApplyScopedBatchBytes(t.Context(), body))
	issue, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Encoded signed batch title", issue.Title)
	require.Equal(t, f.Root.Projects[0].UID, issue.ProjectUID)
	appliedCursor := scopedViewsCursor(t, receiver)
	require.Greater(t, appliedCursor, cursor)
	applied := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(applied))
	assertBody := func(records []db.ImportRecord) {
		matches := 0
		for _, record := range records {
			if row, ok := record.(*db.ScopedBatchExport); ok && row.ViewUID == batch.View.Key.ViewUID && row.Generation == batch.View.Key.Generation && row.Sequence == batch.Cut.Sequence {
				matches++
				require.Equal(t, body, row.BatchBytes, "retain the literal authenticated incoming body")
				require.Equal(t, batch.Cut.Digest, row.Digest)
			}
		}
		require.Equal(t, 1, matches)
	}
	assertBody(applied)
	require.NoError(t, encoded.ApplyScopedBatchBytes(t.Context(), body), "exact encoded retry")
	require.Equal(t, applied, scopedAuthoritySnapshot(t, receiver))
	require.Equal(t, appliedCursor, scopedViewsCursor(t, receiver))
	changedBody := append([]byte("\n"), body...)
	require.NoError(t, db.DecodeScopedRecord(changedBody, &decoded))
	require.Equal(t, batch.Cut.Digest, decoded.Cut.Digest)
	require.ErrorIs(t, encoded.ApplyScopedBatchBytes(t.Context(), changedBody), db.ErrRemoteEventConflict, "same signed digest cannot replace exact retained bytes")
	require.ErrorIs(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(t.Context(), batch), db.ErrRemoteEventConflict, "typed producer reaches the same keyed core")
	require.Equal(t, applied, scopedAuthoritySnapshot(t, receiver))
	require.Equal(t, appliedCursor, scopedViewsCursor(t, receiver))
	require.NoError(t, receiver.ImportReplay(t.Context(), applied, db.ImportOptions{}))
	recovered := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(recovered))
	assertBody(recovered)
	require.NoError(t, encoded.ApplyScopedBatchBytes(t.Context(), body), "exact body survives real native recovery")
	require.Equal(t, recovered, scopedAuthoritySnapshot(t, receiver))
	project, err := receiver.ProjectByUID(t.Context(), f.Root.Projects[1].UID)
	require.NoError(t, err, "resolve native identity again after replay")
	_, _, err = receiver.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{}}, "admin")
	require.NoError(t, err)
	denied := scopedAuthoritySnapshot(t, receiver)
	require.Error(t, encoded.ApplyScopedBatchBytes(t.Context(), body), "retained body retry never grants missing live incoming authority")
	require.Equal(t, denied, scopedAuthoritySnapshot(t, receiver))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func scopedViewsApplyFixture(t *testing.T, root, receiver db.Storage) (ScopedViewsFixture, db.ScopedCollections, db.ScopedCut) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	InstallScopedViewsFixture(t, receiver, f)
	current, err := db.RestrictScopedState(f.Checkpoint.Collections, nil, nil)
	require.NoError(t, err)
	for n := range current.Issues {
		if current.Issues[n].UID == f.Root.Issue.UID {
			current.Issues[n].Title = "Lifecycle signed title"
		}
	}
	batch := ScopedViewsSignedBatch(t, f, current, f.Checkpoint.Cut.Sequence+1, f.Checkpoint.Cut.Digest)
	apply, ok := receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	})
	require.True(t, ok)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, apply.ApplyScopedBatch(t.Context(), batch), "actual clean Apply positive before lifecycle assertion")
	issue, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Lifecycle signed title", issue.Title)
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	return f, current, batch.Cut
}

func RunScopedApplyCommentRemoval(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut, comment := scopedViewsCommentFixture(t, root, receiver, checkNative)
	after := scopedViewsWithoutComment(t, current, comment.UID)
	batch := scopedViewsCommentBatch(t, f, current, after, cut)
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(t.Context(), batch))
	issue, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := receiver.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments, "native comments must converge to the signed explicit empty collection")
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	require.NoError(t, checkNative(t.Context(), f.Checkpoint.View.Key))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedApplyCommentPurge(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut, comment := scopedViewsCommentFixture(t, root, receiver, checkNative)
	_, err := root.PurgeIssue(t.Context(), f.Root.Issue.ID, "member", nil)
	require.NoError(t, err, "supported native root issue/comment deletion premise")
	_, err = root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	rootComments, err := root.CommentIssueIDsByUIDs(t.Context(), []string{comment.UID})
	require.NoError(t, err)
	require.Empty(t, rootComments, "root purge really removed the native comment")
	after := scopedViewsWithoutComment(t, current, comment.UID)
	after.Issues = slices.DeleteFunc(after.Issues, func(issue db.ScopedIssueState) bool { return issue.UID == f.Root.Issue.UID })
	after.References = slices.DeleteFunc(after.References, func(r db.ScopedReference) bool {
		return r.Subject.IssueUID == f.Root.Issue.UID || r.Peer != nil && r.Peer.IssueUID == f.Root.Issue.UID || r.Close != nil && r.Close.PeerIssueUID == f.Root.Issue.UID
	})
	after.Provenance = slices.DeleteFunc(after.Provenance, func(prov db.ScopedProvenance) bool { return prov.Kind == "issue" && prov.EntityUID == f.Root.Issue.UID })
	batch := scopedViewsCommentBatch(t, f, current, after, cut)
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(t.Context(), batch), "clean signed issue/comment omission has no owed retention obligation")
	_, err = receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	comments, err := receiver.CommentIssueIDsByUIDs(t.Context(), []string{comment.UID})
	require.NoError(t, err)
	require.Empty(t, comments)
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedApplyCommentOwedRemoval(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut, comment := scopedViewsCommentFixture(t, root, receiver, checkNative)
	ctx, subject, want := scopedViewsRetainOwedSubjectForComment(t, receiver, f, checkNative, comment.UID)
	require.Equal(t, f.Root.Issue.UID, subject.IssueUID)
	issue, err := receiver.IssueByUID(ctx, subject.IssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := receiver.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, comment.UID, comments[0].UID)
	require.Equal(t, "Current signed comment", comments[0].Body)
	batch := scopedViewsCommentBatch(t, f, current, scopedViewsWithoutComment(t, current, comment.UID), cut)
	before := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(before))
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	require.ErrorIs(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(ctx, batch), db.ErrScopedCapturePending, "pending exact-comment original must not be discarded or silently diverge")
	retained := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(retained))
	require.Equal(t, before, retained, "strict whole-owner snapshot including every allocator")
	require.Equal(t, want, scopedViewsOwedRows(t, retained))
	require.Equal(t, cursor, scopedViewsCursor(t, receiver))
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func scopedViewsCommentFixture(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) (ScopedViewsFixture, db.ScopedCollections, db.ScopedCut, db.Comment) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	InstallScopedViewsFixture(t, receiver, f)
	producer := scopedViewsOperations(t, root)
	require.NoError(t, producer.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
	require.NoError(t, producer.ActivateScopedCheckpoint(t.Context(), f.Checkpoint.View.Key, f.Checkpoint.Cut.Digest))
	ctx := db.WithRootAttribution(t.Context(), f.Signers[f.Root.Projects[0].UID], "member")
	comment, _, err := root.CreateComment(ctx, db.CreateCommentParams{IssueID: f.Root.Issue.ID, Author: "member", Teammate: "helper", Body: "Original signed comment"})
	require.NoError(t, err)
	capture, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Checkpoint.View.Key)
	require.NoError(t, err)
	current, err := db.ProjectScopedState(capture, f.Checkpoint.View)
	require.NoError(t, err)
	require.Len(t, current.Comments, 1)
	require.Equal(t, comment.UID, current.Comments[0].UID)
	batch := scopedViewsCommentBatch(t, f, f.Checkpoint.Collections, current, f.Checkpoint.Cut)
	apply := receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	})
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, apply.ApplyScopedBatch(t.Context(), batch), "actual native captured comment introduction positive")
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	issue, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := receiver.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, comment.UID, comments[0].UID)
	require.Equal(t, "Original signed comment", comments[0].Body)
	comment, _, changed, err := root.EditComment(ctx, db.EditCommentParams{IssueID: f.Root.Issue.ID, CommentUID: comment.UID, Actor: "member", Body: "Current signed comment"})
	require.NoError(t, err)
	require.True(t, changed)
	updated, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	updated.Comments[0].Body = comment.Body
	edit := scopedViewsCommentBatch(t, f, current, updated, batch.Cut)
	cursor = scopedViewsCursor(t, receiver)
	require.NoError(t, apply.ApplyScopedBatch(t.Context(), edit), "actual root edit, literal signed same-comment upsert positive")
	comments, err = receiver.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, comment.UID, comments[0].UID)
	require.Equal(t, "Current signed comment", comments[0].Body)
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	require.NoError(t, checkNative(t.Context(), f.Checkpoint.View.Key))
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	return f, updated, edit.Cut, comment
}

func scopedViewsWithoutComment(t *testing.T, current db.ScopedCollections, commentUID string) db.ScopedCollections {
	t.Helper()
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	after.Comments = slices.DeleteFunc(after.Comments, func(comment db.ScopedCommentState) bool { return comment.UID == commentUID })
	after.References = slices.DeleteFunc(after.References, func(r db.ScopedReference) bool {
		return r.Kind == "reply" && (r.SubjectEntityUID == commentUID || r.PeerEntityUID == commentUID)
	})
	after.Provenance = slices.DeleteFunc(after.Provenance, func(prov db.ScopedProvenance) bool { return prov.Kind == "comment" && prov.EntityUID == commentUID })
	return after
}

func scopedViewsCommentBatch(t *testing.T, f ScopedViewsFixture, before, after db.ScopedCollections, cut db.ScopedCut) db.ScopedBatch {
	t.Helper()
	effects, err := db.DiffScopedState(before, after, cut.Sequence+1)
	require.NoError(t, err)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: cut.Sequence + 1, CaptureUID: scopedOwnerUID(9820 + int(cut.Sequence))}, PreviousDigest: cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, project := range f.Checkpoint.View.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedBatch(batch, f.Checkpoint.View, pins))
	return batch
}

func RunScopedCommentGenerationOmission(t *testing.T, root, receiver db.Storage, owed bool, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, _, comment := scopedViewsCommentFixture(t, root, receiver, checkNative)
	ctx := t.Context()
	var want map[string][]byte
	if owed {
		var subject db.ScopedEndpoint
		ctx, subject, want = scopedViewsRetainOwedSubjectForComment(t, receiver, f, checkNative, comment.UID)
		require.Equal(t, f.Root.Issue.UID, subject.IssueUID)
	}
	view := f.Checkpoint.View
	view.Key.Generation++
	view.RequestDigest = strings.Repeat("5", 64)
	checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: view, Cut: db.ScopedCut{Sequence: 1, CaptureUID: scopedOwnerUID(9860)}, Collections: scopedViewsWithoutComment(t, current, comment.UID), KeyHistory: []db.RootKeyTransition{}}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, project := range view.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedCheckpoint(checkpoint, view, pins))
	ScopedViewsPreparedGeneration(t, receiver, view)
	operations := scopedViewsOperations(t, receiver)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, operations.PrepareScopedCheckpoint(ctx, checkpoint))
	require.Equal(t, cursor, scopedViewsCursor(t, receiver), "preparation never exposes new current content")
	issue, err := receiver.IssueByUID(ctx, f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := receiver.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, comment.UID, comments[0].UID)
	require.Equal(t, "Current signed comment", comments[0].Body)
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key), "old active generation stays authorized while prepared differs")
	for _, record := range scopedAuthoritySnapshot(t, receiver) {
		if row, ok := record.(*db.ScopedViewExport); ok && row.ViewUID == view.Key.ViewUID {
			require.Equal(t, &f.Checkpoint.View.Key.Generation, row.ActiveGeneration)
			require.Equal(t, &view.Key.Generation, row.PreparedGeneration)
			require.Equal(t, view.RequestDigest, row.RequestDigest)
		}
	}
	rootBefore := scopedAuthoritySnapshot(t, root)
	before := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(before))
	if owed {
		require.ErrorIs(t, operations.ActivateScopedCheckpoint(ctx, view.Key, checkpoint.Cut.Digest), db.ErrScopedCapturePending)
		retained := scopedAuthoritySnapshot(t, receiver)
		require.NoError(t, db.ValidateImportRecords(retained))
		require.Equal(t, before, retained, "failed activation preserves whole owner graph and ALL allocators")
		require.Equal(t, want, scopedViewsOwedRows(t, retained))
		require.Equal(t, cursor, scopedViewsCursor(t, receiver))
		require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key))
	} else {
		require.NoError(t, operations.ActivateScopedCheckpoint(ctx, view.Key, checkpoint.Cut.Digest))
		issue, err = receiver.IssueByUID(ctx, f.Root.Issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		comments, err = receiver.CommentsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Empty(t, comments, "activated signed omission reconciles the native comment read model")
		require.Greater(t, scopedViewsCursor(t, receiver), cursor)
		require.NoError(t, checkNative(ctx, view.Key))
		project, err := receiver.ProjectByUID(ctx, f.Root.Projects[0].UID)
		require.NoError(t, err)
		require.Equal(t, "local-alpha", project.Name)
		require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	}
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedOwnerRestoreCommentsAtomic(t *testing.T, open func(*testing.T) db.Storage, checkNative func(context.Context, db.Storage, db.ScopedViewKey) error) {
	t.Helper()
	root, receiver := open(t), open(t)
	f, _, _, comment := scopedViewsCommentFixture(t, root, receiver, func(ctx context.Context, key db.ScopedViewKey) error {
		return checkNative(ctx, receiver, key)
	})
	records := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(records))
	var peerID int64
	for _, record := range records {
		if issue, ok := record.(*db.IssueExport); ok && issue.UID != f.Root.Issue.UID {
			peerID = issue.ID
		}
	}
	require.NotZero(t, peerID, "membership tamper uses a real other native issue")
	for _, kind := range []string{"body", "author", "teammate", "created_at", "issue_membership"} {
		t.Run(kind, func(t *testing.T) {
			target := open(t)
			require.NoError(t, target.ImportReplay(t.Context(), records, db.ImportOptions{}), "legitimate complete comment owner restore positive")
			issue, err := target.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err, "resolve local native ID after restore by stable UID")
			comments, err := target.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, comment.UID, comments[0].UID)
			require.Equal(t, "Current signed comment", comments[0].Body)
			project, err := target.ProjectByUID(t.Context(), f.Root.Projects[0].UID)
			require.NoError(t, err)
			require.Equal(t, "local-alpha", project.Name, "legitimate local catalog alias survives comment corroboration")
			before := scopedAuthoritySnapshot(t, target)
			instance := target.InstanceUID()
			changed := make([]db.ImportRecord, 0, len(records))
			mutations := 0
			for _, record := range records {
				if row, ok := record.(*db.CommentExport); ok && row.UID == comment.UID {
					copy := *row
					switch kind {
					case "body":
						copy.Body = "Tampered native comment"
					case "author":
						copy.Author = "local-member" // actual existing native caller identity
					case "teammate":
						copy.Teammate = "other-helper"
					case "created_at":
						copy.CreatedAt = comment.CreatedAt.Add(time.Second).UTC().Format(time.RFC3339Nano)
					case "issue_membership":
						copy.IssueID = peerID
					}
					require.NotEqual(t, row, &copy)
					changed = append(changed, &copy)
					mutations++
				} else {
					changed = append(changed, record)
				}
			}
			require.Equal(t, 1, mutations)
			require.NoError(t, db.ValidateImportRecords(changed), "valid retained signed owner graph before native semantic corroboration")
			require.Error(t, target.ImportReplay(t.Context(), changed, db.ImportOptions{}), "native comment tamper must refuse before restore commit")
			after := scopedAuthoritySnapshot(t, target)
			require.NoError(t, db.ValidateImportRecords(after))
			require.Equal(t, before, after, "strict populated target rollback including ALL allocators and signed bytes")
			require.Equal(t, instance, target.InstanceUID())
			issue, err = target.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err = target.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, comment.UID, comments[0].UID)
			require.Equal(t, "Current signed comment", comments[0].Body)
		})
	}
}

func RunScopedOwnerRestoreRaisedSequenceAtomic(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	root, target := open(t), open(t)
	f := NewScopedViewsFixture(t, root)
	require.NoError(t, target.ImportReplay(t.Context(), f.Records, db.ImportOptions{}), "legitimate complete signed owner restore positive")
	issue, err := target.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Issue.Title, issue.Title)
	project, err := target.ProjectByUID(t.Context(), f.Root.Projects[0].UID)
	require.NoError(t, err)
	require.Equal(t, "local-alpha", project.Name)
	before := scopedAuthoritySnapshot(t, target)
	require.NoError(t, db.ValidateImportRecords(before))
	sequences := func(records []db.ImportRecord) map[string]int64 {
		result := map[string]int64{}
		for _, record := range records {
			if row, ok := record.(*db.SequenceExport); ok {
				result[row.Name] = row.Seq
			}
		}
		return result
	}
	beforeSequences := sequences(before)
	floor, present := beforeSequences["issues"]
	require.True(t, present, "actual populated target exports its portable issues allocator")
	require.Positive(t, floor)
	raisedFloor := floor + 17
	require.Greater(t, raisedFloor, floor)
	raised := make([]db.ImportRecord, 0, len(before))
	sequenceChanges := 0
	for _, record := range before {
		if row, ok := record.(*db.SequenceExport); ok && row.Name == "issues" {
			copy := *row
			copy.Seq = raisedFloor
			raised = append(raised, &copy)
			sequenceChanges++
		} else {
			raised = append(raised, record)
		}
	}
	require.Equal(t, 1, sequenceChanges)
	require.NoError(t, db.ValidateImportRecords(raised), "raised sequence alone preserves the complete signed owner graph")
	positive := open(t)
	require.NoError(t, positive.ImportReplay(t.Context(), raised, db.ImportOptions{}), "actual positive import proves the raised floor is supported")
	positiveRecords := scopedAuthoritySnapshot(t, positive)
	require.NoError(t, db.ValidateImportRecords(positiveRecords))
	require.Equal(t, raisedFloor, sequences(positiveRecords)["issues"])
	positiveIssue, err := positive.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Issue.Title, positiveIssue.Title)
	require.Equal(t, before, scopedAuthoritySnapshot(t, target), "separate positive store must not pre-raise the rejection target")
	changed := make([]db.ImportRecord, 0, len(raised))
	titleChanges := 0
	for _, record := range raised {
		if row, ok := record.(*db.IssueExport); ok && row.UID == f.Root.Issue.UID {
			copy := *row
			copy.Title = "Tampered native issue title"
			require.NotEqual(t, row.Title, copy.Title)
			changed = append(changed, &copy)
			titleChanges++
		} else {
			changed = append(changed, record)
		}
	}
	require.Equal(t, 1, titleChanges)
	require.NoError(t, db.ValidateImportRecords(changed), "A1 positive: signed bytes untouched, native title and supported floor changed")
	instance := target.InstanceUID()
	rootBefore := scopedAuthoritySnapshot(t, root)
	require.Error(t, target.ImportReplay(t.Context(), changed, db.ImportOptions{}), "native title corroboration refuses before replacement commit")
	after := scopedAuthoritySnapshot(t, target)
	require.NoError(t, db.ValidateImportRecords(after))
	require.Equal(t, beforeSequences, sequences(after), "rejected owner restore preserves EVERY allocator, including the raised issues floor")
	require.Equal(t, before, after, "strict complete populated target rollback with exact signed and native rows")
	require.Equal(t, instance, target.InstanceUID())
	issue, err = target.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Issue.Title, issue.Title)
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedApplyRemoval(t *testing.T, root, receiver db.Storage) {
	t.Helper()
	f, current, cut := scopedViewsApplyFixture(t, root, receiver)
	require.Empty(t, current.Comments)
	require.Empty(t, current.Labels)
	require.Empty(t, current.Artifacts)
	_, err := root.PurgeIssue(t.Context(), f.Root.Issue.ID, "member", nil)
	require.NoError(t, err, "supported native root deletion premise")
	_, err = root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	after.Issues = slices.DeleteFunc(after.Issues, func(i db.ScopedIssueState) bool { return i.UID == f.Root.Issue.UID })
	after.References = slices.DeleteFunc(after.References, func(r db.ScopedReference) bool {
		return r.Subject.IssueUID == f.Root.Issue.UID || r.Peer != nil && r.Peer.IssueUID == f.Root.Issue.UID || r.Close != nil && r.Close.PeerIssueUID == f.Root.Issue.UID
	})
	after.Provenance = slices.DeleteFunc(after.Provenance, func(p db.ScopedProvenance) bool { return p.Kind == "issue" && p.EntityUID == f.Root.Issue.UID })
	effects, err := db.DiffScopedState(current, after, cut.Sequence+1)
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(effects, func(e db.ScopedEffect) bool {
		return e.Collection == "issues" && e.Action == "remove" && e.Key == f.Root.Projects[0].UID+":"+f.Root.Issue.UID
	}))
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: cut.Sequence + 1, CaptureUID: scopedOwnerUID(9801)}, PreviousDigest: cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(t.Context(), batch), "clean authoritative omission must not be mistaken for an owed obligation")
	_, err = receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedApplyClosePeerLifetime(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut := scopedViewsApplyFixture(t, root, receiver)
	subject, local := newScopedLocalRootPeer(t, receiver, f.Root.Issue.UID, f.Root.Projects[0].UID)
	// The accepted T03 helper performs a complete owner replay before creating
	// the local canonical subject. Resolve all incoming local IDs afterward.
	token, err := receiver.ResolveAPIToken(t.Context(), "view-local-token")
	require.NoError(t, err)
	require.Equal(t, "local-member", token.Actor)
	allowed, err := receiver.AccessibleProjectUIDs(t.Context(), token.Actor)
	require.NoError(t, err)
	for _, project := range f.Checkpoint.View.Projects {
		require.Contains(t, allowed, project.ProjectUID)
		_, err := receiver.ProjectByUID(t.Context(), project.ProjectUID)
		require.NoError(t, err)
	}
	ctx := db.WithAuthorizedProjects(t.Context(), allowed)
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key), "actual incoming authority/owner/A2 survives the legal local-root setup")
	peer, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Projects[0].UID, peer.ProjectUID)
	localProject, err := receiver.ProjectByUID(t.Context(), local.View.Projects[0].ProjectUID)
	require.NoError(t, err)
	capture, err := receiver.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), local.View.Key)
	require.NoError(t, err, "actual independent local canonical-root capture, not an incoming-owned capture")
	workBefore := ScopedCaptureWork(t, receiver, local.View.Key.ViewUID)
	require.NotNil(t, workBefore.CaptureUID)
	require.NotNil(t, workBefore.CapturedRevision)
	require.NotNil(t, workBefore.CaptureDigest)
	require.NotEmpty(t, workBefore.CaptureBytes)
	require.Equal(t, capture.CaptureUID, *workBefore.CaptureUID)
	incomingBefore := ScopedCaptureWork(t, receiver, f.Checkpoint.View.Key.ViewUID)
	require.Nil(t, incomingBefore.CaptureUID)
	proofBefore := ScopedCurrentClose(t, receiver, subject.UID)
	require.Equal(t, &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: peer.ProjectUID, PeerIssueUID: peer.UID}, proofBefore.Value)
	require.NotEmpty(t, proofBefore.StatusIntentUID)
	require.NotNil(t, proofBefore.CloseBoundaryClock)
	rawClose := func(records []db.ImportRecord) db.ScopedCanonicalCloseExport {
		for _, record := range records {
			if row, ok := record.(*db.ScopedCanonicalCloseExport); ok && row.IssueUID == subject.UID {
				return *row
			}
		}
		t.Fatal("local canonical subject current proof missing")
		return db.ScopedCanonicalCloseExport{}
	}
	before := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(before), "complete two-authority owner graph is valid before signed peer omission")
	rawBefore := rawClose(before)
	clockBytes := bytes.Clone(rawBefore.StatusClockBytes)
	originals := func() map[string][]byte {
		events, err := receiver.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: localProject.ID, Limit: 1000})
		require.NoError(t, err)
		out := map[string][]byte{}
		for _, event := range events {
			if event.IssueUID == nil || *event.IssueUID != subject.UID {
				continue
			}
			raw, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(event))
			require.NoError(t, err)
			out[event.UID] = raw
		}
		return out
	}
	originalBefore := originals()
	require.Contains(t, originalBefore, proofBefore.StatusClock.EventUID, "writer is an actual retained native event")
	require.Contains(t, originalBefore, proofBefore.StatusIntentUID)
	require.Contains(t, originalBefore, proofBefore.CloseBoundaryClock.EventUID)
	_, err = root.PurgeIssue(t.Context(), f.Root.Issue.ID, "member", nil)
	require.NoError(t, err, "supported native source-root peer purge")
	_, err = root.IssueByUID(t.Context(), peer.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	after.Issues = slices.DeleteFunc(after.Issues, func(issue db.ScopedIssueState) bool { return issue.UID == peer.UID })
	after.References = slices.DeleteFunc(after.References, func(reference db.ScopedReference) bool {
		return reference.Subject.IssueUID == peer.UID || reference.Peer != nil && reference.Peer.IssueUID == peer.UID || reference.Close != nil && reference.Close.PeerIssueUID == peer.UID
	})
	after.Provenance = slices.DeleteFunc(after.Provenance, func(provenance db.ScopedProvenance) bool {
		return provenance.Kind == "issue" && provenance.EntityUID == peer.UID
	})
	batch := scopedViewsSignMove(t, f, current, after, cut)
	require.True(t, slices.ContainsFunc(batch.Effects, func(effect db.ScopedEffect) bool {
		return effect.Collection == "issues" && effect.Action == "remove" && effect.Key == peer.ProjectUID+":"+peer.UID
	}))
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(ctx, batch), "signed incoming omission must preserve the surviving local canonical root")
	_, err = receiver.IssueByUID(t.Context(), peer.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound, "the incoming native peer is actually removed")
	proofAfter := ScopedCurrentClose(t, receiver, subject.UID)
	workAfter := ScopedCaptureWork(t, receiver, local.View.Key.ViewUID)
	if proofAfter.Value != nil {
		t.Errorf("signed peer removal leaves stale derived current close value: %+v", proofAfter.Value)
	}
	if workAfter.DirtyRevision <= workBefore.DirtyRevision {
		t.Errorf("signed peer invalidation did not wake independent local-root work: before=%d after=%d", workBefore.DirtyRevision, workAfter.DirtyRevision)
	}
	require.Equal(t, proofBefore.StatusIntentUID, proofAfter.StatusIntentUID)
	require.Equal(t, proofBefore.StatusClock, proofAfter.StatusClock)
	require.Equal(t, proofBefore.CloseBoundaryClock, proofAfter.CloseBoundaryClock)
	rawAfter := rawClose(scopedAuthoritySnapshot(t, receiver))
	require.Equal(t, rawBefore.ProjectUID, rawAfter.ProjectUID)
	require.Equal(t, rawBefore.StatusIntentUID, rawAfter.StatusIntentUID)
	require.Equal(t, clockBytes, rawAfter.StatusClockBytes, "retained writer/intent/boundary bytes never rewritten by derived peer invalidation")
	require.Equal(t, originalBefore, originals(), "native original bodies/hash/UID/clock remain exact")
	require.Equal(t, workBefore.CaptureUID, workAfter.CaptureUID)
	require.Equal(t, workBefore.CapturedRevision, workAfter.CapturedRevision)
	require.Equal(t, workBefore.CaptureDigest, workAfter.CaptureDigest)
	require.Equal(t, workBefore.CaptureBytes, workAfter.CaptureBytes, "new dirty work cannot overwrite the immutable owed local-root cut")
	require.Equal(t, incomingBefore, ScopedCaptureWork(t, receiver, f.Checkpoint.View.Key.ViewUID), "incoming-owned replicas never become canonical capture work")
	got, err := receiver.IssueByUID(t.Context(), subject.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, localProject.UID, got.ProjectUID)
	require.Equal(t, "closed", got.Status)
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
}

func RunScopedApplyClosePeerMoveLifetime(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut := scopedViewsApplyFixture(t, root, receiver)
	subject, local := newScopedLocalRootPeer(t, receiver, f.Root.Issue.UID, f.Root.Projects[0].UID)
	// The accepted T03 helper performs a complete owner replay before creating
	// the local canonical subject. Resolve all incoming local IDs afterward.
	token, err := receiver.ResolveAPIToken(t.Context(), "view-local-token")
	require.NoError(t, err)
	require.Equal(t, "local-member", token.Actor)
	allowed, err := receiver.AccessibleProjectUIDs(t.Context(), token.Actor)
	require.NoError(t, err)
	for _, project := range f.Checkpoint.View.Projects {
		require.Contains(t, allowed, project.ProjectUID)
		_, err := receiver.ProjectByUID(t.Context(), project.ProjectUID)
		require.NoError(t, err)
	}
	ctx := db.WithAuthorizedProjects(t.Context(), allowed)
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key), "actual incoming authority/owner/A2 survives the legal local-root setup")
	peer, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Projects[0].UID, peer.ProjectUID)
	localProject, err := receiver.ProjectByUID(t.Context(), local.View.Projects[0].ProjectUID)
	require.NoError(t, err)
	capture, err := receiver.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), local.View.Key)
	require.NoError(t, err, "actual independent local canonical-root capture, not an incoming-owned capture")
	workBefore := ScopedCaptureWork(t, receiver, local.View.Key.ViewUID)
	require.NotNil(t, workBefore.CaptureUID)
	require.NotNil(t, workBefore.CapturedRevision)
	require.NotNil(t, workBefore.CaptureDigest)
	require.NotEmpty(t, workBefore.CaptureBytes)
	require.Equal(t, capture.CaptureUID, *workBefore.CaptureUID)
	incomingBefore := ScopedCaptureWork(t, receiver, f.Checkpoint.View.Key.ViewUID)
	require.Nil(t, incomingBefore.CaptureUID)
	proofBefore := ScopedCurrentClose(t, receiver, subject.UID)
	require.Equal(t, &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: peer.ProjectUID, PeerIssueUID: peer.UID}, proofBefore.Value)
	require.NotEmpty(t, proofBefore.StatusIntentUID)
	require.NotNil(t, proofBefore.CloseBoundaryClock)
	rawClose := func(records []db.ImportRecord) db.ScopedCanonicalCloseExport {
		for _, record := range records {
			if row, ok := record.(*db.ScopedCanonicalCloseExport); ok && row.IssueUID == subject.UID {
				return *row
			}
		}
		t.Fatal("local canonical subject current proof missing")
		return db.ScopedCanonicalCloseExport{}
	}
	before := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(before), "complete two-authority owner graph is valid before signed peer relocation")
	rawBefore := rawClose(before)
	clockBytes := bytes.Clone(rawBefore.StatusClockBytes)
	originals := func() map[string][]byte {
		events, err := receiver.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: localProject.ID, Limit: 1000})
		require.NoError(t, err)
		out := map[string][]byte{}
		for _, event := range events {
			if event.IssueUID == nil || *event.IssueUID != subject.UID {
				continue
			}
			raw, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(event))
			require.NoError(t, err)
			out[event.UID] = raw
		}
		return out
	}
	originalBefore := originals()
	require.Contains(t, originalBefore, proofBefore.StatusClock.EventUID, "writer is an actual retained native event")
	require.Contains(t, originalBefore, proofBefore.StatusIntentUID)
	require.Contains(t, originalBefore, proofBefore.CloseBoundaryClock.EventUID)
	rootBefore := scopedAuthoritySnapshot(t, root)
	for step, destination := range []string{f.Root.Projects[1].UID, f.Root.Projects[0].UID} {
		require.NotEqual(t, peer.ProjectUID, destination)
		require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key), "both selected projects still have actual incoming authority and exact owner")
		after, err := db.RestrictScopedState(current, nil, nil)
		require.NoError(t, err)
		shortID := scopedViewsMoveShortID(t, current, peer.UID, destination)
		scopedViewsRetargetIssue(t, &after, peer.UID, destination, shortID)
		batch := scopedViewsCommentBatch(t, f, current, after, cut)
		require.True(t, slices.ContainsFunc(batch.Effects, func(effect db.ScopedEffect) bool {
			return effect.Collection == "issues" && effect.Action == "remove" && effect.Key == peer.ProjectUID+":"+peer.UID
		}))
		require.True(t, slices.ContainsFunc(batch.Effects, func(effect db.ScopedEffect) bool {
			return effect.Collection == "issues" && effect.Action == "replace" && effect.Key == destination+":"+peer.UID
		}))
		cursor := scopedViewsCursor(t, receiver)
		require.NoError(t, receiver.(interface {
			ApplyScopedBatch(context.Context, db.ScopedBatch) error
		}).ApplyScopedBatch(ctx, batch), "valid signed same-view peer relocation must preserve the independently owned local root")
		gotPeer, err := receiver.IssueByUID(t.Context(), peer.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, peer.ID, gotPeer.ID, "relocation preserves the actual native peer row identity")
		require.Equal(t, peer.UID, gotPeer.UID)
		require.Equal(t, destination, gotPeer.ProjectUID)
		require.Equal(t, shortID, gotPeer.ShortID)
		proofAfter := ScopedCurrentClose(t, receiver, subject.UID)
		workAfter := ScopedCaptureWork(t, receiver, local.View.Key.ViewUID)
		if proofAfter.Value != nil {
			t.Errorf("signed peer relocation step %d leaves or resurrects derived close value: %+v", step, proofAfter.Value)
		}
		if step == 0 && workAfter.DirtyRevision <= workBefore.DirtyRevision {
			t.Errorf("signed peer relocation did not wake independent local-root work: before=%d after=%d", workBefore.DirtyRevision, workAfter.DirtyRevision)
		}
		require.Equal(t, proofBefore.StatusIntentUID, proofAfter.StatusIntentUID)
		require.Equal(t, proofBefore.StatusClock, proofAfter.StatusClock)
		require.Equal(t, proofBefore.CloseBoundaryClock, proofAfter.CloseBoundaryClock)
		rawAfter := rawClose(scopedAuthoritySnapshot(t, receiver))
		require.Equal(t, rawBefore.ProjectUID, rawAfter.ProjectUID)
		require.Equal(t, rawBefore.StatusIntentUID, rawAfter.StatusIntentUID)
		require.Equal(t, clockBytes, rawAfter.StatusClockBytes)
		require.Equal(t, originalBefore, originals(), "relocation never rewrites retained local-root original bytes")
		require.Equal(t, workBefore.CaptureUID, workAfter.CaptureUID)
		require.Equal(t, workBefore.CapturedRevision, workAfter.CapturedRevision)
		require.Equal(t, workBefore.CaptureDigest, workAfter.CaptureDigest)
		require.Equal(t, workBefore.CaptureBytes, workAfter.CaptureBytes, "owed old local-root cut remains immutable")
		require.Equal(t, incomingBefore, ScopedCaptureWork(t, receiver, f.Checkpoint.View.Key.ViewUID))
		gotSubject, err := receiver.IssueByUID(t.Context(), subject.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, localProject.UID, gotSubject.ProjectUID)
		require.Equal(t, "closed", gotSubject.Status)
		require.Greater(t, scopedViewsCursor(t, receiver), cursor)
		require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key))
		require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root), "literal signed input does not mutate the canonical source root")
		require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
		current, cut, peer = after, batch.Cut, gotPeer
	}
}

func RunScopedApplyMove(t *testing.T, root, receiver db.Storage) {
	t.Helper()
	f, current, cut := scopedViewsApplyFixture(t, root, receiver)
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	shortID := scopedViewsMoveShortID(t, current, f.Root.Issue.UID, f.Root.Projects[1].UID)
	for n := range after.Issues {
		if after.Issues[n].UID == f.Root.Issue.UID {
			after.Issues[n].ProjectUID = f.Root.Projects[1].UID
			after.Issues[n].ShortID = shortID
		}
	}
	for n := range after.References {
		r := &after.References[n]
		if r.Subject.IssueUID == f.Root.Issue.UID {
			r.Subject.ProjectUID = f.Root.Projects[1].UID
		}
		if r.Peer != nil && r.Peer.IssueUID == f.Root.Issue.UID {
			r.Peer.ProjectUID = f.Root.Projects[1].UID
		}
		if r.Close != nil && r.Close.PeerIssueUID == f.Root.Issue.UID {
			r.Close.PeerProjectUID = f.Root.Projects[1].UID
		}
	}
	for n := range after.Provenance {
		if after.Provenance[n].Kind == "issue" && after.Provenance[n].EntityUID == f.Root.Issue.UID {
			after.Provenance[n].ProjectUID = f.Root.Projects[1].UID
		}
	}
	effects, err := db.DiffScopedState(current, after, cut.Sequence+1)
	require.NoError(t, err)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: cut.Sequence + 1, CaptureUID: scopedOwnerUID(9802)}, PreviousDigest: cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	rootBefore := scopedAuthoritySnapshot(t, root)
	require.NoError(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(t.Context(), batch), "same-view selected-project UID move is not a foreign owner collision")
	issue, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Projects[1].UID, issue.ProjectUID)
	require.Equal(t, shortID, issue.ShortID)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedApplyCommentMove(t *testing.T, root, receiver db.Storage, owed bool, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut, comment := scopedViewsCommentFixture(t, root, receiver, checkNative)
	ctx := t.Context()
	var want map[string][]byte
	if owed {
		ctx, _, want = scopedViewsRetainOwedSubjectForComment(t, receiver, f, checkNative, comment.UID)
	}
	// Every receiver-local ID is resolved after the optional full owner replay.
	beforeIssue, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Projects[0].UID, beforeIssue.ProjectUID)
	beforeComments, err := receiver.CommentsByIssue(t.Context(), beforeIssue.ID)
	require.NoError(t, err)
	require.Len(t, beforeComments, 1)
	require.Equal(t, comment.UID, beforeComments[0].UID)
	require.Equal(t, beforeIssue.ID, beforeComments[0].IssueID)
	require.Equal(t, "Current signed comment", beforeComments[0].Body)
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key), "actual selected source/destination owner and signed current-content positive")
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	destination := f.Root.Projects[1].UID
	shortID := scopedViewsMoveShortID(t, current, beforeIssue.UID, destination)
	scopedViewsRetargetIssue(t, &after, beforeIssue.UID, destination, shortID)
	commentsMoved, provenanceMoved := 0, 0
	for n := range after.Comments {
		if after.Comments[n].IssueUID == beforeIssue.UID {
			require.Equal(t, comment.UID, after.Comments[n].UID)
			after.Comments[n].ProjectUID = destination
			commentsMoved++
		}
	}
	for n := range after.Provenance {
		if after.Provenance[n].Kind == "comment" && after.Provenance[n].EntityUID == comment.UID {
			after.Provenance[n].ProjectUID = destination
			provenanceMoved++
		}
	}
	require.Equal(t, 1, commentsMoved)
	require.Equal(t, 1, provenanceMoved, "the same comment's current project and provenance move together")
	batch := scopedViewsSignMove(t, f, current, after, cut)
	for _, effect := range []struct{ action, key string }{
		{"remove", beforeIssue.ProjectUID + ":" + comment.UID},
		{"replace", destination + ":" + comment.UID},
	} {
		require.True(t, slices.ContainsFunc(batch.Effects, func(actual db.ScopedEffect) bool {
			return actual.Collection == "comments" && actual.Action == effect.action && actual.Key == effect.key
		}), "signed input includes exact project-qualified comment transition %s %s", effect.action, effect.key)
	}
	if owed {
		refusal := scopedViewsRejectMove(t, root, receiver, ctx, f.Checkpoint.View.Key, batch, checkNative)
		require.ErrorIs(t, refusal, db.ErrScopedCapturePending, "same-comment pending original stays owed across a selected-project move")
		require.Equal(t, want, scopedViewsOwedRows(t, scopedAuthoritySnapshot(t, receiver)))
		got, err := receiver.IssueByUID(t.Context(), beforeIssue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, beforeIssue, got)
		comments, err := receiver.CommentsByIssue(t.Context(), got.ID)
		require.NoError(t, err)
		require.Equal(t, beforeComments, comments)
		return
	}
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(ctx, batch), "corroborated same-issue comments are current content, not an independent move obligation")
	got, err := receiver.IssueByUID(t.Context(), beforeIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, beforeIssue.ID, got.ID)
	require.Equal(t, beforeIssue.UID, got.UID)
	require.Equal(t, destination, got.ProjectUID)
	require.Equal(t, shortID, got.ShortID)
	comments, err := receiver.CommentsByIssue(t.Context(), got.ID)
	require.NoError(t, err)
	require.Equal(t, beforeComments, comments, "same native comment UID/issue row/scalars survive the signed project move")
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key), "signed issue/comment/provenance project membership corroborates after move")
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedApplyMoveUnownedCollision(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut, source, ctx := scopedViewsMoveCollisionFixture(t, root, receiver, false, checkNative)
	project, err := receiver.CreateProject(t.Context(), "local-collision-project")
	require.NoError(t, err)
	local, _, err := receiver.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: f.Root.Issue.UID, Title: "Local canonical collision", Author: "local-member"})
	require.NoError(t, err)
	require.Equal(t, project.UID, local.ProjectUID)
	for _, record := range scopedAuthoritySnapshot(t, receiver) {
		if owner, ok := record.(*db.ScopedProjectExport); ok && owner.ProjectUID == project.UID {
			require.Nil(t, owner.OwnerViewUID)
			require.Nil(t, owner.OwnerGeneration)
		}
	}
	batch := scopedViewsCollisionBatch(t, f, current, cut, source)
	err = scopedViewsRejectMove(t, root, receiver, ctx, f.Checkpoint.View.Key, batch, checkNative)
	require.ErrorIs(t, err, db.ErrRemoteEventConflict)
	got, err := receiver.IssueByUID(t.Context(), local.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, local, got, "unowned canonical issue stays canonical")
}

func RunScopedApplyMoveForeignCollision(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut, source, ctx := scopedViewsMoveCollisionFixture(t, root, receiver, true, checkNative)
	foreign, err := receiver.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, f.Root.Projects[0].UID, foreign.ProjectUID)
	var foreignKey db.ScopedViewKey
	for _, record := range scopedAuthoritySnapshot(t, receiver) {
		if owner, ok := record.(*db.ScopedProjectExport); ok && owner.ProjectUID == foreign.ProjectUID {
			require.NotNil(t, owner.OwnerViewUID)
			require.NotNil(t, owner.OwnerGeneration)
			require.False(t, owner.ServingFenced)
			foreignKey = db.ScopedViewKey{ViewUID: *owner.OwnerViewUID, Generation: *owner.OwnerGeneration}
		}
	}
	require.NotEmpty(t, foreignKey.ViewUID)
	require.NotEqual(t, f.Checkpoint.View.Key.ViewUID, foreignKey.ViewUID)
	require.NoError(t, checkNative(ctx, foreignKey), "actual retained foreign owner and live native incoming context")
	batch := scopedViewsCollisionBatch(t, f, current, cut, source)
	err = scopedViewsRejectMove(t, root, receiver, ctx, f.Checkpoint.View.Key, batch, checkNative)
	require.ErrorIs(t, err, db.ErrRemoteEventConflict)
	require.NoError(t, checkNative(ctx, foreignKey))
	got, err := receiver.IssueByUID(t.Context(), foreign.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, foreign, got)
}

func RunScopedApplyMoveOwed(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut := scopedViewsApplyFixture(t, root, receiver)
	ctx, subject, want := scopedViewsRetainOwedSubject(t, receiver, f, checkNative)
	before, err := receiver.IssueByUID(t.Context(), subject.IssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	shortID := scopedViewsMoveShortID(t, current, subject.IssueUID, f.Root.Projects[1].UID)
	scopedViewsRetargetIssue(t, &after, subject.IssueUID, f.Root.Projects[1].UID, shortID)
	batch := scopedViewsSignMove(t, f, current, after, cut)
	refusal := scopedViewsRejectMove(t, root, receiver, ctx, f.Checkpoint.View.Key, batch, checkNative)
	require.True(t, errors.Is(refusal, db.ErrRemoteEventConflict) || errors.Is(refusal, db.ErrScopedCapturePending), "current mismatch guard or qualified owed refusal, not an unrelated admission failure: %v", refusal)
	require.Equal(t, want, scopedViewsOwedRows(t, scopedAuthoritySnapshot(t, receiver)))
	got, err := receiver.IssueByUID(t.Context(), subject.IssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, before, got)
}

func scopedViewsRejectMove(t *testing.T, root, receiver db.Storage, ctx context.Context, key db.ScopedViewKey, batch db.ScopedBatch, checkNative func(context.Context, db.ScopedViewKey) error) error {
	t.Helper()
	require.NoError(t, checkNative(ctx, key), "live native authority/owner positive immediately before collision attempt")
	before := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(before))
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	err := receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(ctx, batch)
	require.Error(t, err)
	after := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(after))
	require.Equal(t, before, after, "strict whole-owner snapshot including every allocator/custody row")
	require.Equal(t, cursor, scopedViewsCursor(t, receiver))
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
	return err
}

func scopedViewsSignMove(t *testing.T, f ScopedViewsFixture, current, after db.ScopedCollections, cut db.ScopedCut) db.ScopedBatch {
	t.Helper()
	effects, err := db.DiffScopedState(current, after, cut.Sequence+1)
	require.NoError(t, err)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: cut.Sequence + 1, CaptureUID: scopedOwnerUID(9804)}, PreviousDigest: cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, project := range f.Checkpoint.View.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedBatch(batch, f.Checkpoint.View, pins), "literal signed input is valid before native collision assertion")
	return batch
}

func scopedViewsMoveShortID(t *testing.T, current db.ScopedCollections, uid, project string) string {
	t.Helper()
	for length := shortid.MinLength; length <= shortid.MaxLength; length++ {
		candidate, err := shortid.Derive(uid, length)
		require.NoError(t, err)
		occupied := slices.ContainsFunc(current.Issues, func(issue db.ScopedIssueState) bool {
			return issue.ProjectUID == project && issue.UID != uid && issue.ShortID == candidate
		})
		if !occupied {
			return candidate
		}
	}
	t.Fatal("no UID-derived short ID available in destination fixture")
	return ""
}

func scopedViewsRetargetIssue(t *testing.T, after *db.ScopedCollections, uid, project, shortID string) {
	t.Helper()
	found := false
	for n := range after.Issues {
		if after.Issues[n].UID == uid {
			after.Issues[n].ProjectUID, after.Issues[n].ShortID = project, shortID
			found = true
		}
	}
	require.True(t, found)
	for n := range after.References {
		r := &after.References[n]
		if r.Subject.IssueUID == uid {
			r.Subject.ProjectUID = project
		}
		if r.Peer != nil && r.Peer.IssueUID == uid {
			r.Peer.ProjectUID = project
		}
		if r.Close != nil && r.Close.PeerIssueUID == uid {
			r.Close.PeerProjectUID = project
		}
	}
	for n := range after.Provenance {
		if after.Provenance[n].Kind == "issue" && after.Provenance[n].EntityUID == uid {
			after.Provenance[n].ProjectUID = project
		}
	}
}

func scopedViewsCollisionBatch(t *testing.T, f ScopedViewsFixture, current db.ScopedCollections, cut db.ScopedCut, source db.ScopedCollections) db.ScopedBatch {
	t.Helper()
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	for _, issue := range source.Issues {
		if issue.UID == f.Root.Issue.UID {
			after.Issues = append(after.Issues, issue)
		}
	}
	for _, reference := range source.References {
		if reference.Subject.IssueUID == f.Root.Issue.UID {
			after.References = append(after.References, reference)
		}
	}
	for _, provenance := range source.Provenance {
		if provenance.Kind == "issue" && provenance.EntityUID == f.Root.Issue.UID {
			after.Provenance = append(after.Provenance, provenance)
		}
	}
	// The collision is first in stable UID order, before any surviving peer
	// upsert. These baseline controls do not claim every later-collision order.
	for _, issue := range current.Issues {
		require.Less(t, f.Root.Issue.UID, issue.UID, "native fixture creates the root anchor before its peer")
	}
	shortID := scopedViewsMoveShortID(t, current, f.Root.Issue.UID, f.Checkpoint.View.Projects[0].ProjectUID)
	scopedViewsRetargetIssue(t, &after, f.Root.Issue.UID, f.Checkpoint.View.Projects[0].ProjectUID, shortID)
	return scopedViewsSignMove(t, f, current, after, cut)
}

func scopedViewsMoveCollisionFixture(t *testing.T, root, receiver db.Storage, foreign bool, checkNative func(context.Context, db.ScopedViewKey) error) (ScopedViewsFixture, db.ScopedCollections, db.ScopedCut, db.ScopedCollections, context.Context) {
	t.Helper()
	base := NewScopedViewsFixture(t, root)
	indices := []int{1}
	if foreign {
		indices = append(indices, 0)
	}
	owners := []ScopedViewsFixture{}
	for n, index := range indices {
		view := base.Checkpoint.View
		view.Key.ViewUID = scopedOwnerUID(9700 + n)
		view.Projects = []db.ScopedProjectPin{base.Checkpoint.View.Projects[index]}
		view.Route = slices.Clone(view.Route)
		require.Len(t, view.Route, 1)
		view.Route[0].BindingUID = scopedOwnerUID(9710 + n)
		view.RouteDigest = strings.Repeat([]string{"6", "7"}[n], 64)
		view.RequestDigest = strings.Repeat([]string{"8", "9"}[n], 64)
		catalog := scopedOwnerUID(9720 + n)
		grant, err := root.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), db.CreateScopedRelayEnrollmentParams{View: view, CatalogEntryUID: catalog, AnchorProjectID: base.Root.Projects[index].ID, ParentTokenID: base.Root.Parent.ID, Token: []string{"move-target-token", "move-foreign-token"}[n], ServeDownstream: true})
		require.NoError(t, err, "actual native root grant for each disjoint selection")
		require.Equal(t, &base.Root.Parent.ID, grant.Enrollment.ParentTokenID)
		require.Equal(t, view.Route[0].BindingUID, grant.Enrollment.RelayBindingUID)
		capture, err := root.(interface {
			CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
		}).CaptureScopedView(t.Context(), view.Key)
		require.NoError(t, err)
		collections, err := db.ProjectScopedState(capture, view)
		require.NoError(t, err)
		checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: view, Cut: db.ScopedCut{Sequence: 1, CaptureUID: capture.CaptureUID}, Collections: collections, KeyHistory: []db.RootKeyTransition{}}, base.Signers)
		require.NoError(t, err)
		producer := scopedViewsOperations(t, root)
		require.NoError(t, producer.PrepareScopedCheckpoint(t.Context(), checkpoint))
		require.NoError(t, producer.ActivateScopedCheckpoint(t.Context(), view.Key, checkpoint.Cut.Digest))
		owners = append(owners, ScopedViewsFixture{Root: base.Root, Checkpoint: checkpoint, Signers: base.Signers})
	}
	keepIssueIDs := map[int64]bool{}
	for _, record := range base.Records {
		if issue, ok := record.(*db.IssueExport); ok && (foreign || issue.ProjectID == base.Root.Projects[1].ID) {
			keepIssueIDs[issue.ID] = true
		}
	}
	records := []db.ImportRecord{}
	for _, record := range base.Records {
		switch row := record.(type) {
		case *db.MetaKV, *db.ProjectExport, *db.RootKeyPin, *db.ScopedProtocolCutoverExport:
			records = append(records, record)
		case *db.IssueExport:
			if keepIssueIDs[row.ID] {
				records = append(records, record)
			}
		case *db.CommentExport:
			if keepIssueIDs[row.IssueID] {
				records = append(records, record)
			}
		case *db.IssueLabelExport:
			if keepIssueIDs[row.IssueID] {
				records = append(records, record)
			}
		}
	}
	selectedViews := map[string]bool{}
	for _, owner := range owners {
		view := owner.Checkpoint.View
		selectedViews[view.Key.ViewUID] = true
		key := view.Key
		records = append(records, &db.ScopedProjectExport{ProjectUID: view.Projects[0].ProjectUID, OwnerViewUID: &key.ViewUID, OwnerGeneration: &key.Generation, ProtocolFloor: 2, RootCutoverUID: &view.RootCutoverUID})
	}
	// Retained descriptors, signed baselines, current envelopes and work rows
	// come from the actual native producer, including its exact stored bytes.
	for _, record := range scopedAuthoritySnapshot(t, root) {
		var viewUID string
		switch row := record.(type) {
		case *db.ScopedViewExport:
			viewUID = row.ViewUID
		case *db.ScopedGenerationExport:
			viewUID = row.ViewUID
		case *db.ScopedRecordExport:
			viewUID = row.ViewUID
		case *db.ScopedWorkExport:
			viewUID = row.ViewUID
		}
		if selectedViews[viewUID] {
			records = append(records, record)
		}
	}
	require.NoError(t, db.ValidateImportRecords(records), "complete disjoint signed owner graph before native restore")
	require.NoError(t, receiver.ImportReplay(t.Context(), records, db.ImportOptions{}))
	team, _, err := receiver.CreateTeam(t.Context(), "move-collision-team", "admin")
	require.NoError(t, err)
	_, _, err = receiver.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "move-collision-token", Actor: "local-member", AdminActor: "admin", TeamUIDs: []string{team.UID}})
	require.NoError(t, err)
	for n, owner := range owners {
		view := owner.Checkpoint.View
		project, err := receiver.ProjectByUID(t.Context(), view.Projects[0].ProjectUID)
		require.NoError(t, err)
		_, _, err = receiver.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
		require.NoError(t, err)
		_, err = receiver.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: base.Root.Projects[indices[n]].ID, HubProjectUID: project.UID, PushEnabled: true, Actor: base.Root.Parent.Actor, Enabled: true})
		require.NoError(t, err)
		hop := view.Route[0]
		_, err = receiver.SetRelayBindingConfig(t.Context(), project.ID, db.RelayBindingConfig{ScopedView: &view.Key, RootCutoverUID: view.RootCutoverUID, ProtocolVersion: 2, BindingUID: hop.BindingUID, UpstreamInstanceUID: view.AuthorityUID, AuthorityUID: view.AuthorityUID, HubPath: []string{view.AuthorityUID, view.DestinationUID}, LocalActor: "local-member", ResetEpoch: hop.Epoch})
		require.NoError(t, err, "actual installed incoming config for each exact owner")
	}
	token, err := receiver.ResolveAPIToken(t.Context(), "move-collision-token")
	require.NoError(t, err)
	require.Equal(t, "local-member", token.Actor)
	allowed, err := receiver.AccessibleProjectUIDs(t.Context(), token.Actor)
	require.NoError(t, err)
	ctx := db.WithAuthorizedProjects(t.Context(), allowed)
	for _, owner := range owners {
		require.Contains(t, allowed, owner.Checkpoint.View.Projects[0].ProjectUID)
		_, err := receiver.ProjectByUID(t.Context(), owner.Checkpoint.View.Projects[0].ProjectUID)
		require.NoError(t, err, "resolve imported native IDs by stable UID")
		require.NoError(t, checkNative(ctx, owner.Checkpoint.View.Key))
	}
	target := owners[0]
	current, err := db.RestrictScopedState(target.Checkpoint.Collections, nil, nil)
	require.NoError(t, err)
	require.Len(t, current.Issues, 1)
	current.Issues[0].Title = "Collision baseline signed title"
	batch := ScopedViewsSignedBatch(t, target, current, target.Checkpoint.Cut.Sequence+1, target.Checkpoint.Cut.Digest)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(ctx, batch), "actual signed incoming Apply positive before collision")
	issue, err := receiver.IssueByUID(t.Context(), current.Issues[0].UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, current.Issues[0].Title, issue.Title)
	require.Greater(t, scopedViewsCursor(t, receiver), cursor)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, receiver)))
	return target, current, batch.Cut, base.Checkpoint.Collections, ctx
}

func RunScopedApplyOwedRemoval(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f, current, cut := scopedViewsApplyFixture(t, root, receiver)
	ctx, subject, want := scopedViewsRetainOwedSubject(t, receiver, f, checkNative)
	_, err := root.PurgeIssue(t.Context(), f.Root.Issue.ID, "member", nil)
	require.NoError(t, err)
	_, err = root.IssueByUID(t.Context(), subject.IssueUID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	after, err := db.RestrictScopedState(current, nil, nil)
	require.NoError(t, err)
	after.Issues = slices.DeleteFunc(after.Issues, func(i db.ScopedIssueState) bool { return i.UID == subject.IssueUID })
	after.References = slices.DeleteFunc(after.References, func(r db.ScopedReference) bool {
		return r.Subject.IssueUID == subject.IssueUID || r.Peer != nil && r.Peer.IssueUID == subject.IssueUID || r.Close != nil && r.Close.PeerIssueUID == subject.IssueUID
	})
	after.Provenance = slices.DeleteFunc(after.Provenance, func(p db.ScopedProvenance) bool { return p.Kind == "issue" && p.EntityUID == subject.IssueUID })
	effects, err := db.DiffScopedState(current, after, cut.Sequence+1)
	require.NoError(t, err)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: cut.Sequence + 1, CaptureUID: scopedOwnerUID(9803)}, PreviousDigest: cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	before := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(before))
	cursor := scopedViewsCursor(t, receiver)
	require.ErrorIs(t, receiver.(interface {
		ApplyScopedBatch(context.Context, db.ScopedBatch) error
	}).ApplyScopedBatch(ctx, batch), db.ErrScopedCapturePending, "genuinely owed source remains retained instead of clean removal")
	retainedAfter := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(retainedAfter))
	require.Equal(t, before, retainedAfter, "bounded refusal preserves complete native/owner/custody state")
	require.Equal(t, cursor, scopedViewsCursor(t, receiver))
	require.Equal(t, want, scopedViewsOwedRows(t, scopedAuthoritySnapshot(t, receiver)))
}

func scopedViewsRetainOwedSubject(t *testing.T, receiver db.Storage, f ScopedViewsFixture, checkNative func(context.Context, db.ScopedViewKey) error) (context.Context, db.ScopedEndpoint, map[string][]byte) {
	t.Helper()
	return scopedViewsRetainOwedSubjectForComment(t, receiver, f, checkNative, "")
}

func scopedViewsRetainOwedSubjectForComment(t *testing.T, receiver db.Storage, f ScopedViewsFixture, checkNative func(context.Context, db.ScopedViewKey) error, commentUID string) (context.Context, db.ScopedEndpoint, map[string][]byte) {
	t.Helper()
	records := scopedAuthoritySnapshot(t, receiver)
	var binding *db.FederationBindingExport
	for _, pin := range f.Checkpoint.View.Projects {
		project, err := receiver.ProjectByUID(t.Context(), pin.ProjectUID)
		require.NoError(t, err)
		found := false
		for _, record := range records {
			row, ok := record.(*db.FederationBindingExport)
			if !ok || row.ProjectID != project.ID {
				continue
			}
			found = true
			require.Equal(t, pin.ProjectUID, row.HubProjectUID)
			require.NotNil(t, row.RelayConfig)
			if binding != nil {
				require.Equal(t, binding.Actor, row.Actor)
				require.Equal(t, binding.RelayConfig, row.RelayConfig)
			}
			if pin.ProjectUID == f.Root.Projects[0].UID {
				binding = row
			}
		}
		require.True(t, found)
	}
	require.NotNil(t, binding)
	for _, record := range records {
		switch row := record.(type) {
		case *db.ScopedSourceOperationExport:
			require.NotEqual(t, scopedOwnerUID(1900), row.OperationUID)
		case *db.ScopedPrivateSourceExport:
			require.NotEqual(t, scopedOwnerUID(1901), row.EventUID)
		case *db.EventExport:
			require.NotEqual(t, scopedOwnerUID(1901), row.UID)
		}
	}
	subject := db.ScopedEndpoint{ProjectUID: f.Root.Projects[0].UID, IssueUID: f.Root.Issue.UID}
	owed := typedOwedSourceFixtureForComment(t, f.Checkpoint.View, *binding, subject, f.Signers[subject.ProjectUID], commentUID)
	require.Len(t, owed, 4)
	complete := append(records, owed...)
	require.NoError(t, db.ValidateImportRecords(complete), "complete typed custody graph before native import")
	require.NoError(t, receiver.ImportReplay(t.Context(), complete, db.ImportOptions{}))
	want := scopedViewsOwedRows(t, owed)
	reload := func() context.Context {
		token, err := receiver.ResolveAPIToken(t.Context(), "view-local-token")
		require.NoError(t, err, "reload actual native caller after import")
		require.Equal(t, "local-member", token.Actor)
		allowed, err := receiver.AccessibleProjectUIDs(t.Context(), token.Actor)
		require.NoError(t, err)
		for _, pin := range f.Checkpoint.View.Projects {
			require.Contains(t, allowed, pin.ProjectUID)
			_, err := receiver.ProjectByUID(t.Context(), pin.ProjectUID)
			require.NoError(t, err, "resolve local project IDs again after import")
		}
		issue, err := receiver.IssueByUID(t.Context(), subject.IssueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, subject.ProjectUID, issue.ProjectUID)
		ctx := db.WithAuthorizedProjects(t.Context(), allowed)
		require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key), "real native route/pin/epoch/owner authority after import")
		return ctx
	}
	reload()
	retained := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(retained))
	require.Equal(t, want, scopedViewsOwedRows(t, retained), "exact four pending row representations reexport")
	require.NoError(t, receiver.ImportReplay(t.Context(), retained, db.ImportOptions{}), "real owner recovery")
	ctx := reload()
	recovered := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(recovered))
	require.Equal(t, want, scopedViewsOwedRows(t, recovered), "owner recovery preserves typed custody bytes")
	for _, record := range recovered {
		operation, ok := record.(*db.ScopedSourceOperationExport)
		if !ok || operation.OperationUID != scopedOwnerUID(1900) {
			continue
		}
		require.Equal(t, "staged", operation.State)
		require.Equal(t, "pending", operation.OverlayState)
		submission, err := db.DecodeScopedSourceSubmission(operation.ManifestBytes)
		require.NoError(t, err, "pending original body remains decodable after native recovery")
		require.Len(t, submission.Originals, 1)
		require.NotNil(t, submission.Originals[0].IssueUID)
		require.Equal(t, subject.IssueUID, *submission.Originals[0].IssueUID)
		require.Equal(t, subject.ProjectUID, submission.Originals[0].ProjectUID)
		if commentUID != "" {
			require.Equal(t, "editComment", submission.Operation.CommandKind)
			require.Equal(t, "issue.comment_edited", submission.Originals[0].Type)
			var payload struct {
				CommentUID string `json:"comment_uid"`
				Body       string `json:"body"`
			}
			require.NoError(t, json.Unmarshal(submission.Originals[0].Payload, &payload))
			require.Equal(t, commentUID, payload.CommentUID)
			require.Equal(t, "Pending local comment edit", payload.Body)
		}
	}
	return ctx, subject, want
}

func scopedViewsOwedRows(t *testing.T, records []db.ImportRecord) map[string][]byte {
	t.Helper()
	rows := map[string][]byte{}
	for _, record := range records {
		match := false
		switch row := record.(type) {
		case *db.ScopedSourceOperationExport:
			match = row.OperationUID == scopedOwnerUID(1900)
		case *db.ScopedPrivateSourceExport:
			match = row.EventUID == scopedOwnerUID(1901)
		case *db.ScopedSourceCustodyExport:
			match = row.OperationUID == scopedOwnerUID(1900)
		case *db.ScopedSourceDependencyExport:
			match = row.OperationUID == scopedOwnerUID(1900)
		}
		if match {
			rows[record.ImportKind()] = scopedOwnerBytes(t, record)
		}
	}
	require.Len(t, rows, 4)
	return rows
}

func RunScopedFenceRecovery(t *testing.T, root, receiver db.Storage, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	InstallScopedViewsFixture(t, receiver, f)
	ctx, subject, owed := scopedViewsRetainOwedSubject(t, receiver, f, checkNative)
	require.NoError(t, checkNative(ctx, f.Checkpoint.View.Key))
	localProject, err := receiver.CreateProject(t.Context(), "independent-canonical")
	require.NoError(t, err)
	localIssue, _, err := receiver.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: localProject.ID, Title: "Canonical content survives fencing", Author: "local-member"})
	require.NoError(t, err)
	incoming, err := receiver.IssueByUID(t.Context(), subject.IssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	worker, ok := receiver.(interface {
		FenceScopedView(context.Context, db.ScopedViewKey, string) error
	})
	require.True(t, ok, "native scoped view fencing and recovery boundary must be concrete")
	before := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(before))
	require.Equal(t, owed, scopedViewsOwedRows(t, before))
	retained := func(records []db.ImportRecord) map[string][]byte {
		rows := map[string][]byte{}
		for _, record := range records {
			var body []byte
			switch row := record.(type) {
			case *db.ScopedGenerationExport:
				copy := *row
				copy.State = "" // Lifecycle state may fence; its retained bytes may not change.
				body = scopedOwnerBytes(t, &copy)
			case *db.ScopedBatchExport, *db.ScopedRecordExport, *db.ScopedWorkExport, *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
				body = scopedOwnerBytes(t, record)
			}
			if body != nil {
				rows[record.ImportKind()+":"+db.ScopedOfferedDigest(body)] = body
			}
		}
		return rows
	}
	retainedBefore := retained(before)
	require.NotEmpty(t, retainedBefore)
	owners := map[string]db.ScopedProjectExport{}
	for _, record := range before {
		if row, ok := record.(*db.ScopedProjectExport); ok && row.OwnerViewUID != nil && *row.OwnerViewUID == f.Checkpoint.View.Key.ViewUID {
			require.False(t, row.ServingFenced)
			owners[row.ProjectUID] = *row
		}
	}
	require.Len(t, owners, len(f.Checkpoint.View.Projects))
	assertFenced := func(records []db.ImportRecord) {
		views, selected := 0, 0
		for _, record := range records {
			switch row := record.(type) {
			case *db.ScopedViewExport:
				if row.ViewUID == f.Checkpoint.View.Key.ViewUID {
					views++
					require.Equal(t, "recovery_required", row.ReasonCode)
				}
			case *db.ScopedProjectExport:
				if want, present := owners[row.ProjectUID]; present {
					selected++
					want.ServingFenced = true
					require.Equal(t, want, *row, "fencing retains exact owner/generation/floor/cutover context")
				}
				if row.ProjectUID == localProject.UID {
					require.Nil(t, row.OwnerViewUID)
					require.False(t, row.ServingFenced)
				}
			}
		}
		require.Equal(t, 1, views)
		require.Len(t, owners, selected)
		require.Equal(t, retainedBefore, retained(records))
		require.Equal(t, owed, scopedViewsOwedRows(t, records))
	}
	rootBefore := scopedAuthoritySnapshot(t, root)
	cursor := scopedViewsCursor(t, receiver)
	require.NoError(t, worker.FenceScopedView(ctx, f.Checkpoint.View.Key, "recovery_required"))
	require.ErrorIs(t, checkNative(ctx, f.Checkpoint.View.Key), db.ErrNotFound, "native owned-read helper refuses before any restore")
	fenced := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(fenced))
	assertFenced(fenced)
	fencedCursor := scopedViewsCursor(t, receiver)
	require.Greater(t, fencedCursor, cursor, "existing private read/cache cursor observes durable fencing")
	got, err := receiver.IssueByUID(t.Context(), incoming.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, incoming, got, "native row retention does not confer serving authority")
	got, err = receiver.IssueByUID(t.Context(), localIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, localIssue, got, "unowned canonical content remains canonical")
	require.NoError(t, receiver.ImportReplay(t.Context(), fenced, db.ImportOptions{}), "complete non-serving owner restore preserves recovery custody")
	recovered := scopedAuthoritySnapshot(t, receiver)
	require.NoError(t, db.ValidateImportRecords(recovered))
	assertFenced(recovered)
	require.ErrorIs(t, checkNative(ctx, f.Checkpoint.View.Key), db.ErrNotFound, "restore cannot reactivate retained fenced rows")
	require.Equal(t, fencedCursor, scopedViewsCursor(t, receiver))
	got, err = receiver.IssueByUID(t.Context(), localIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err) // Resolve by stable UID again after owner replay.
	require.Equal(t, localIssue.UID, got.UID)
	require.Equal(t, localProject.UID, got.ProjectUID)
	require.Equal(t, localIssue.Title, got.Title)
	require.Equal(t, rootBefore, scopedAuthoritySnapshot(t, root))
}

func RunScopedFenceRetry(t *testing.T, root, target db.Storage, mode string, checkNative func(context.Context, db.ScopedViewKey) error) {
	t.Helper()
	require.Contains(t, []string{"root_same_reason", "root_parent_revoked", "root_policy_loss", "incoming_same_reason", "incoming_policy_loss"}, mode)
	f := NewScopedViewsFixture(t, root)
	key, view := f.Checkpoint.View.Key, f.Checkpoint.View
	incoming := strings.HasPrefix(mode, "incoming_")
	ctx := t.Context()
	credential := "parent-test-token"
	var owed map[string][]byte
	if incoming {
		InstallScopedViewsFixture(t, target, f)
		var subject db.ScopedEndpoint
		ctx, subject, owed = scopedViewsRetainOwedSubject(t, target, f, checkNative)
		require.Equal(t, f.Root.Issue.UID, subject.IssueUID)
		require.NoError(t, checkNative(ctx, key), "real incoming local policy/config/owner/A2 precedes first Fence")
		credential = "view-local-token"
	} else {
		require.Equal(t, root.InstanceUID(), target.InstanceUID())
		operations := scopedViewsOperations(t, root)
		require.NoError(t, operations.PrepareScopedCheckpoint(ctx, f.Checkpoint))
		require.NoError(t, operations.ActivateScopedCheckpoint(ctx, key, f.Checkpoint.Cut.Digest))
	}
	worker := target.(interface {
		FenceScopedView(context.Context, db.ScopedViewKey, string) error
	})
	reload := func() db.APIToken {
		parent, err := target.ResolveAPIToken(t.Context(), credential)
		require.NoError(t, err)
		allowed, err := target.AccessibleProjectUIDs(t.Context(), parent.Actor)
		require.NoError(t, err)
		for _, selected := range view.Projects {
			require.Contains(t, allowed, selected.ProjectUID)
			project, err := target.ProjectByUID(t.Context(), selected.ProjectUID)
			require.NoError(t, err)
			binding, err := target.FederationBindingByProject(t.Context(), project.ID)
			require.NoError(t, err)
			require.True(t, binding.Enabled)
			pin, err := target.RootAuthority(t.Context(), selected.ProjectUID)
			require.NoError(t, err)
			require.Equal(t, view.AuthorityUID, pin.AuthorityUID)
			require.Equal(t, selected.Pin.KeyID, pin.KeyID)
			require.Equal(t, selected.Pin.PublicKey, pin.PublicKey)
			if incoming {
				require.Equal(t, db.FederationRoleSpoke, binding.Role)
				require.True(t, binding.PushEnabled)
				require.NotNil(t, binding.RelayConfig)
				require.NotNil(t, binding.RelayConfig.ScopedView)
				require.Equal(t, key, *binding.RelayConfig.ScopedView)
				require.Equal(t, view.Route[0].BindingUID, binding.RelayConfig.BindingUID)
				require.Equal(t, view.Route[0].Epoch, binding.RelayConfig.ResetEpoch)
			} else {
				require.Equal(t, db.FederationRoleHub, binding.Role)
			}
		}
		if !incoming {
			anchor, err := target.ProjectByUID(t.Context(), f.Root.Projects[0].UID)
			require.NoError(t, err)
			grants, err := target.ListProjectFederationEnrollments(t.Context(), anchor.ID)
			require.NoError(t, err)
			found := 0
			for _, grant := range grants {
				if grant.RelayBindingUID == view.Route[0].BindingUID {
					found++
					require.Equal(t, db.ScopedRelayProtocolVersion, grant.RelayProtocolVersion)
					require.Equal(t, view.Route[0].Epoch, grant.RelayResetEpoch)
					require.Equal(t, view.Route[0].InstanceUID, grant.SpokeInstanceUID)
					require.Equal(t, &parent.ID, grant.ParentTokenID)
					require.Equal(t, parent.Actor, grant.Actor)
					require.Contains(t, strings.Split(grant.Capabilities, ","), "pull")
					require.Nil(t, grant.RevokedAt)
				}
			}
			require.Equal(t, 1, found)
		}
		return parent
	}
	reload()
	var pending []db.ScopedDeliveryRecord
	if !incoming {
		var err error
		pending, err = target.(interface {
			PendingScopedViewDeliveries(context.Context, db.ScopedViewKey, int) ([]db.ScopedDeliveryRecord, error)
		}).PendingScopedViewDeliveries(ctx, key, 1)
		require.NoError(t, err, "real current native root grant permits exact initial emission")
		require.Len(t, pending, 1)
		require.True(t, pending[0].Emitted)
		_, body, err := db.DecodeScopedDelivery(pending[0].EnvelopeBytes)
		require.NoError(t, err)
		require.Equal(t, scopedOwnerBytes(t, f.Checkpoint), body)
	}
	const reason = "retry_recovery"
	require.NoError(t, worker.FenceScopedView(ctx, key, reason), "first native Fence must commit before retry attribution")
	parent := reload()
	fenced := scopedAuthoritySnapshot(t, target)
	require.NoError(t, db.ValidateImportRecords(fenced))
	rows := 0
	for _, record := range fenced {
		if row, ok := record.(*db.ScopedViewExport); ok && row.ViewUID == key.ViewUID {
			rows++
			require.Equal(t, "fenced", row.State)
			require.Equal(t, reason, row.ReasonCode)
			require.Equal(t, &key.Generation, row.ActiveGeneration)
		}
	}
	require.Equal(t, 1, rows)
	if incoming {
		require.Equal(t, owed, scopedViewsOwedRows(t, fenced))
	} else {
		again, err := target.(interface {
			PendingScopedViewDeliveries(context.Context, db.ScopedViewKey, int) ([]db.ScopedDeliveryRecord, error)
		}).PendingScopedViewDeliveries(ctx, key, 1)
		require.NoError(t, err, "independent live native-hop auth still succeeds after serving fence")
		require.Equal(t, pending, again)
		require.Equal(t, fenced, scopedAuthoritySnapshot(t, target))
	}
	veto := false
	switch mode {
	case "root_parent_revoked":
		_, _, err := target.RevokeAPIToken(t.Context(), parent.ID, "admin")
		require.NoError(t, err)
		current, err := target.APITokenByID(t.Context(), parent.ID)
		require.NoError(t, err)
		require.NotNil(t, current.RevokedAt)
		veto = true
	case "root_policy_loss", "incoming_policy_loss":
		_, _, err := target.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: view.Projects[1].ProjectUID, Visibility: "teams"}, "admin")
		require.NoError(t, err)
		allowed, err := target.AccessibleProjectUIDs(t.Context(), parent.Actor)
		require.NoError(t, err)
		require.NotContains(t, allowed, view.Projects[1].ProjectUID, "real current policy loss, not a retained caller hint")
		veto = true
	}
	before := scopedAuthoritySnapshot(t, target)
	require.NoError(t, db.ValidateImportRecords(before))
	cursor := scopedViewsCursor(t, target)
	native, err := target.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	retryErr := worker.FenceScopedView(ctx, key, reason)
	after := scopedAuthoritySnapshot(t, target)
	require.NoError(t, db.ValidateImportRecords(after))
	require.Equal(t, before, after, "exact retry preserves EVERY owner/custody/canonical/allocator row before success assertion")
	require.Equal(t, cursor, scopedViewsCursor(t, target), "retry cannot reserve another private reset cursor")
	got, err := target.IssueByUID(t.Context(), native.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, native, got)
	if incoming {
		require.Equal(t, owed, scopedViewsOwedRows(t, after))
	}
	if veto {
		require.ErrorIs(t, retryErr, db.ErrNotFound, "retained fenced tuple cannot override live native authority loss")
		return
	}
	require.NoError(t, retryErr, "same key and safe reason retry confirms the committed fence under current native authority")
}

func RunScopedViewDeliveryFencedRoot(t *testing.T, root db.Storage, mode string) {
	t.Helper()
	require.Contains(t, []string{"fresh_fenced", "emitted_fenced", "emitted_revoked"}, mode)
	f := NewScopedViewsFixture(t, root)
	key, view := f.Checkpoint.View.Key, f.Checkpoint.View
	require.Equal(t, root.InstanceUID(), view.AuthorityUID)
	require.NotEqual(t, root.InstanceUID(), view.DestinationUID)
	operations := scopedViewsOperations(t, root)
	require.NoError(t, operations.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
	require.NoError(t, operations.ActivateScopedCheckpoint(t.Context(), key, f.Checkpoint.Cut.Digest))
	title := "Fenced outgoing delivery"
	_, err := root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
	require.NoError(t, err)
	capture, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), key)
	require.NoError(t, err)
	projected, err := db.ProjectScopedState(capture, view)
	require.NoError(t, err)
	effects, err := db.DiffScopedState(f.Checkpoint.Collections, projected, f.Checkpoint.Cut.Sequence+1)
	require.NoError(t, err)
	require.NotEmpty(t, effects)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: view, Cut: db.ScopedCut{Sequence: f.Checkpoint.Cut.Sequence + 1, CaptureUID: capture.CaptureUID}, PreviousDigest: f.Checkpoint.Cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, p := range view.Projects {
		pins = append(pins, p.Pin)
	}
	require.NoError(t, db.VerifyScopedBatch(batch, view, pins))
	require.NoError(t, root.(interface {
		CommitScopedBatch(context.Context, db.ScopedBatch, string) error
	}).CommitScopedBatch(t.Context(), batch, capture.CaptureUID))
	worker := root.(interface {
		PendingScopedViewDeliveries(context.Context, db.ScopedViewKey, int) ([]db.ScopedDeliveryRecord, error)
		AckScopedViewDeliveries(context.Context, db.ScopedViewKey, int64, string) error
		FenceScopedView(context.Context, db.ScopedViewKey, string) error
	})
	hop := view.Route[0]
	stream := db.ScopedDeliveryStream{BindingUID: hop.BindingUID, Epoch: hop.Epoch, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}
	offersFrom := func(records []db.ImportRecord) []db.ScopedDeliveryRecord {
		rows := []db.ScopedDeliveryRecord{}
		for _, record := range records {
			if row, ok := record.(*db.ScopedDeliveryRecord); ok && row.ScopedDeliveryStream == stream {
				require.NoError(t, row.Validate())
				rows = append(rows, *row)
			}
		}
		slices.SortFunc(rows, func(a, b db.ScopedDeliveryRecord) int {
			if a.Sequence < b.Sequence {
				return -1
			}
			if a.Sequence > b.Sequence {
				return 1
			}
			return 0
		})
		return rows
	}
	reload := func() db.APIToken {
		parent, err := root.ResolveAPIToken(t.Context(), "parent-test-token")
		require.NoError(t, err)
		anchor, err := root.ProjectByUID(t.Context(), f.Root.Projects[0].UID)
		require.NoError(t, err)
		grants, err := root.ListProjectFederationEnrollments(t.Context(), anchor.ID)
		require.NoError(t, err)
		found := 0
		for _, grant := range grants {
			if grant.RelayBindingUID == hop.BindingUID {
				found++
				require.Equal(t, db.ScopedRelayProtocolVersion, grant.RelayProtocolVersion)
				require.Equal(t, hop.Epoch, grant.RelayResetEpoch)
				require.Equal(t, hop.InstanceUID, grant.SpokeInstanceUID)
				require.Equal(t, &parent.ID, grant.ParentTokenID)
				require.Equal(t, parent.Actor, grant.Actor)
				require.Nil(t, grant.RevokedAt)
			}
		}
		require.Equal(t, 1, found)
		return parent
	}
	reload()
	offers := offersFrom(scopedAuthoritySnapshot(t, root))
	require.Len(t, offers, 2)
	for n, row := range offers {
		require.Equal(t, int64(n+1), row.Sequence)
		require.False(t, row.Emitted)
		require.False(t, row.Acknowledged)
		header, body, err := db.DecodeScopedDelivery(row.EnvelopeBytes)
		require.NoError(t, err)
		require.Equal(t, root.InstanceUID(), header.SenderInstanceUID)
		require.Equal(t, hop.InstanceUID, header.ReceiverInstanceUID)
		want := scopedOwnerBytes(t, f.Checkpoint)
		if n == 1 {
			want = scopedOwnerBytes(t, batch)
		}
		require.Equal(t, want, body)
	}
	if mode != "fresh_fenced" {
		pending, err := worker.PendingScopedViewDeliveries(t.Context(), key, 2)
		require.NoError(t, err, "actual live authority emits both exact offers BEFORE fencing")
		for n := range offers {
			offers[n].Emitted = true
		}
		require.Equal(t, offers, pending)
	}
	before := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(before))
	native, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	cursor := scopedViewsCursor(t, root)
	require.NoError(t, worker.FenceScopedView(t.Context(), key, "delivery_recovery"), "legal native outgoing root Fence admission is a prerequisite; never bypass refusal")
	parent := reload()
	fenced := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(fenced))
	want := make([]db.ImportRecord, 0, len(before))
	changed := 0
	for _, record := range before {
		if row, ok := record.(*db.ScopedViewExport); ok && row.ViewUID == key.ViewUID {
			copy := *row
			copy.State, copy.ReasonCode = "fenced", "delivery_recovery"
			want = append(want, &copy)
			changed++
			continue
		}
		want = append(want, record)
	}
	require.Equal(t, 1, changed)
	require.Equal(t, want, fenced, "durable outgoing fence preserves all canonical/retained/custody/work/delivery/allocator bytes")
	require.Equal(t, offers, offersFrom(fenced))
	require.Equal(t, cursor, scopedViewsCursor(t, root), "outgoing unowned root projects stay canonical")
	if mode == "fresh_fenced" {
		pending, err := worker.PendingScopedViewDeliveries(t.Context(), key, 2)
		require.Error(t, err, "durable serving fence vetoes first disclosure")
		require.Empty(t, pending)
		require.Equal(t, fenced, scopedAuthoritySnapshot(t, root))
		require.Error(t, worker.AckScopedViewDeliveries(t.Context(), key, offers[1].Sequence, offers[1].OfferedDigest))
		require.Equal(t, fenced, scopedAuthoritySnapshot(t, root), "no skip, partial flag or lost prefix/body")
		return
	}
	pending, err := worker.PendingScopedViewDeliveries(t.Context(), key, 2)
	require.NoError(t, err, "already emitted exact offers retry under live same-hop auth despite durable serving fence")
	require.Equal(t, offers, pending)
	require.Equal(t, fenced, scopedAuthoritySnapshot(t, root))
	if mode == "emitted_revoked" {
		_, _, err := root.RevokeAPIToken(t.Context(), parent.ID, "admin")
		require.NoError(t, err)
		revoked := scopedAuthoritySnapshot(t, root)
		require.NoError(t, db.ValidateImportRecords(revoked))
		require.Equal(t, offers, offersFrom(revoked))
		retained := func(records []db.ImportRecord) []db.ImportRecord {
			rows := []db.ImportRecord{}
			for _, record := range records {
				switch record.(type) {
				case *db.ScopedGenerationExport, *db.ScopedRecordExport, *db.ScopedBatchExport, *db.ScopedWorkExport, *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
					rows = append(rows, record)
				}
			}
			return rows
		}
		require.Equal(t, retained(fenced), retained(revoked))
		vetoCursor := scopedViewsCursor(t, root)
		pending, err = worker.PendingScopedViewDeliveries(t.Context(), key, 2)
		require.Error(t, err, "fenced retained tuple is not authority after real parent revocation")
		require.Empty(t, pending)
		require.Error(t, worker.AckScopedViewDeliveries(t.Context(), key, offers[1].Sequence, offers[1].OfferedDigest))
		require.Equal(t, revoked, scopedAuthoritySnapshot(t, root))
		require.Equal(t, vetoCursor, scopedViewsCursor(t, root))
		return
	}
	require.NoError(t, worker.AckScopedViewDeliveries(t.Context(), key, offers[1].Sequence, offers[1].OfferedDigest))
	settled := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(settled))
	withoutDelivery := func(records []db.ImportRecord) []db.ImportRecord {
		rows := []db.ImportRecord{}
		for _, record := range records {
			switch record.(type) {
			case *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
				continue
			}
			rows = append(rows, record)
		}
		return rows
	}
	require.Equal(t, withoutDelivery(fenced), withoutDelivery(settled))
	streamCursors := 0
	for _, record := range settled {
		if row, ok := record.(*db.ScopedDeliveryCursor); ok && row.ScopedDeliveryStream == stream {
			streamCursors++
			require.Equal(t, offers[1].Sequence, row.EmittedSequence)
			require.Equal(t, offers[1].OfferedDigest, row.EmittedDigest)
			require.Equal(t, offers[1].Sequence, row.AcknowledgedSequence)
			require.Equal(t, offers[1].OfferedDigest, row.AcknowledgedDigest)
		}
	}
	require.Equal(t, 1, streamCursors)
	finalOffers := offersFrom(settled)
	require.Len(t, finalOffers, len(offers))
	for n, row := range finalOffers {
		require.True(t, db.SameScopedDeliveryIdentity(offers[n], row))
		require.True(t, row.Emitted)
		require.True(t, row.Acknowledged)
		require.NoError(t, worker.AckScopedViewDeliveries(t.Context(), key, row.Sequence, row.OfferedDigest))
	}
	pending, err = worker.PendingScopedViewDeliveries(t.Context(), key, 2)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Equal(t, settled, scopedAuthoritySnapshot(t, root))
	require.Equal(t, cursor, scopedViewsCursor(t, root))
	got, err := root.IssueByUID(t.Context(), native.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, native, got)
}

func RunScopedViewDeliverySharedStream(t *testing.T, root db.Storage) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	second := f.Root.Params
	second.View.Key.ViewUID = scopedOwnerUID(10050)
	second.CatalogEntryUID = scopedOwnerUID(10051)
	require.NotEqual(t, f.Checkpoint.View.Key.ViewUID, second.View.Key.ViewUID)
	require.NoError(t, second.View.Validate())
	grant, err := root.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), second)
	require.NoError(t, err, "second view uses real native admission on the existing hop")
	parent, err := root.ResolveAPIToken(t.Context(), "parent-test-token")
	require.NoError(t, err)
	require.Equal(t, f.Root.Grant.Enrollment.ID, grant.Enrollment.ID)
	require.Equal(t, &parent.ID, grant.Enrollment.ParentTokenID)
	require.Equal(t, parent.Actor, grant.Enrollment.Actor)
	require.Equal(t, second.View.Route[0].BindingUID, grant.Enrollment.RelayBindingUID)
	require.Equal(t, second.View.Route[0].Epoch, grant.Enrollment.RelayResetEpoch)
	require.Equal(t, second.View.Route[0].InstanceUID, grant.Enrollment.SpokeInstanceUID)
	capturer := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	committer := root.(interface {
		CommitScopedBatch(context.Context, db.ScopedBatch, string) error
	})
	capture, err := capturer.CaptureScopedView(t.Context(), second.View.Key)
	require.NoError(t, err)
	projected, err := db.ProjectScopedState(capture, second.View)
	require.NoError(t, err)
	checkpoint, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: second.View, Cut: db.ScopedCut{Sequence: 1, CaptureUID: capture.CaptureUID}, Collections: projected, KeyHistory: []db.RootKeyTransition{}}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, project := range second.View.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedCheckpoint(checkpoint, second.View, pins))
	checkpoints := []db.ScopedCheckpoint{f.Checkpoint, checkpoint}
	keys := []db.ScopedViewKey{f.Checkpoint.View.Key, checkpoint.View.Key}
	operations := scopedViewsOperations(t, root)
	for _, current := range checkpoints {
		require.NoError(t, operations.PrepareScopedCheckpoint(t.Context(), current))
		require.NoError(t, operations.ActivateScopedCheckpoint(t.Context(), current.View.Key, current.Cut.Digest))
	}
	title := "Shared stream current work"
	_, err = root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
	require.NoError(t, err)
	batches := []db.ScopedBatch{}
	for _, current := range checkpoints {
		captured, err := capturer.CaptureScopedView(t.Context(), current.View.Key)
		require.NoError(t, err)
		content, err := db.ProjectScopedState(captured, current.View)
		require.NoError(t, err)
		effects, err := db.DiffScopedState(current.Collections, content, current.Cut.Sequence+1)
		require.NoError(t, err)
		require.NotEmpty(t, effects)
		batch, err := db.SignScopedBatch(db.ScopedBatch{View: current.View, Cut: db.ScopedCut{Sequence: current.Cut.Sequence + 1, CaptureUID: captured.CaptureUID}, PreviousDigest: current.Cut.Digest, Effects: effects}, f.Signers)
		require.NoError(t, err)
		require.NoError(t, db.VerifyScopedBatch(batch, current.View, pins))
		require.NoError(t, committer.CommitScopedBatch(t.Context(), batch, captured.CaptureUID))
		batches = append(batches, batch)
	}
	before := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(before))
	for _, batch := range batches {
		require.NoError(t, committer.CommitScopedBatch(t.Context(), batch, batch.Cut.CaptureUID), "each exact retry rechecks real native authority before delivery targets")
	}
	require.Equal(t, before, scopedAuthoritySnapshot(t, root))
	stream := db.ScopedDeliveryStream{BindingUID: grant.Enrollment.RelayBindingUID, Epoch: grant.Enrollment.RelayResetEpoch, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}
	offers := []db.ScopedDeliveryRecord{}
	for _, record := range before {
		if row, ok := record.(*db.ScopedDeliveryRecord); ok && row.ScopedDeliveryStream == stream {
			require.NoError(t, row.Validate())
			require.False(t, row.Emitted)
			require.False(t, row.Acknowledged)
			offers = append(offers, *row)
		}
	}
	slices.SortFunc(offers, func(a, b db.ScopedDeliveryRecord) int {
		if a.Sequence < b.Sequence {
			return -1
		}
		if a.Sequence > b.Sequence {
			return 1
		}
		return 0
	})
	require.Len(t, offers, 4)
	bodies := [][]byte{scopedOwnerBytes(t, checkpoints[0]), scopedOwnerBytes(t, checkpoints[1]), scopedOwnerBytes(t, batches[0]), scopedOwnerBytes(t, batches[1])}
	cuts := []db.ScopedCut{checkpoints[0].Cut, checkpoints[1].Cut, batches[0].Cut, batches[1].Cut}
	for n, row := range offers {
		require.Equal(t, int64(n+1), row.Sequence, "one actual contiguous sequence namespace spans both view UIDs")
		require.Equal(t, keys[n%2].ViewUID, row.ObjectUID)
		require.Equal(t, cuts[n].Digest, row.ObjectDigest)
		header, body, err := db.DecodeScopedDelivery(row.EnvelopeBytes)
		require.NoError(t, err)
		require.Equal(t, root.InstanceUID(), header.SenderInstanceUID)
		require.Equal(t, grant.Enrollment.SpokeInstanceUID, header.ReceiverInstanceUID)
		require.Equal(t, bodies[n], body)
		require.Equal(t, db.ScopedOfferedDigest(body), header.BodyDigest)
		require.Equal(t, db.ScopedOfferedDigest(row.EnvelopeBytes), row.OfferedDigest)
	}
	require.Equal(t, int64(2), batches[0].Cut.Sequence)
	require.Equal(t, int64(2), batches[1].Cut.Sequence)
	require.NotEqual(t, offers[2].Sequence, batches[0].Cut.Sequence)
	require.NotEqual(t, offers[3].Sequence, batches[1].Cut.Sequence)
	worker, ok := root.(interface {
		PendingScopedViewDeliveries(context.Context, db.ScopedViewKey, int) ([]db.ScopedDeliveryRecord, error)
		AckScopedViewDeliveries(context.Context, db.ScopedViewKey, int64, string) error
	})
	require.True(t, ok)
	native, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, title, native.Title)
	cursor := scopedViewsCursor(t, root)
	withoutDelivery := func(records []db.ImportRecord) []db.ImportRecord {
		out := []db.ImportRecord{}
		for _, record := range records {
			switch record.(type) {
			case *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
				continue
			}
			out = append(out, record)
		}
		return out
	}
	unchanged := withoutDelivery(before)
	// A mixed page cannot partially emit its first valid row before refusing B.
	_, err = worker.PendingScopedViewDeliveries(t.Context(), keys[0], 2)
	require.Error(t, err, "key A cannot filter B out of its complete prefix page")
	require.Equal(t, before, scopedAuthoritySnapshot(t, root))
	_, err = worker.PendingScopedViewDeliveries(t.Context(), keys[1], 1)
	require.Error(t, err, "key B cannot skip the earlier un-emitted A offer")
	require.Equal(t, before, scopedAuthoritySnapshot(t, root))
	for n, row := range offers {
		key, other := keys[n%2], keys[(n+1)%2]
		start := scopedAuthoritySnapshot(t, root)
		_, err := worker.PendingScopedViewDeliveries(t.Context(), other, 1)
		require.Error(t, err, "other key cannot skip the complete stream head")
		require.Equal(t, start, scopedAuthoritySnapshot(t, root))
		pending, err := worker.PendingScopedViewDeliveries(t.Context(), key, 1)
		require.NoError(t, err, "the head's independently enrolled key can emit this exact prefix")
		expected := row
		expected.Emitted = true
		require.Equal(t, []db.ScopedDeliveryRecord{expected}, pending)
		emitted := scopedAuthoritySnapshot(t, root)
		require.NoError(t, db.ValidateImportRecords(emitted))
		require.Error(t, worker.AckScopedViewDeliveries(t.Context(), other, row.Sequence, row.OfferedDigest), "another valid key cannot acknowledge this emitted object's endpoint")
		require.Equal(t, emitted, scopedAuthoritySnapshot(t, root))
		_, err = worker.PendingScopedViewDeliveries(t.Context(), other, 1)
		require.Error(t, err, "an emitted but unacknowledged other-view predecessor remains owed")
		require.Equal(t, emitted, scopedAuthoritySnapshot(t, root))
		require.NoError(t, worker.AckScopedViewDeliveries(t.Context(), key, row.Sequence, row.OfferedDigest))
		settled := scopedAuthoritySnapshot(t, root)
		require.NoError(t, db.ValidateImportRecords(settled))
		require.Equal(t, unchanged, withoutDelivery(settled), "all non-delivery owner records and native sequences stay exact")
	}
	settled := scopedAuthoritySnapshot(t, root)
	require.NotEqual(t, offers[0].OfferedDigest, offers[3].OfferedDigest)
	require.Error(t, worker.AckScopedViewDeliveries(t.Context(), keys[0], offers[0].Sequence, offers[3].OfferedDigest), "the global final cursor cannot authenticate a different older row")
	require.Equal(t, settled, scopedAuthoritySnapshot(t, root))
	for n, key := range keys {
		require.NoError(t, worker.AckScopedViewDeliveries(t.Context(), key, offers[n].Sequence, offers[n].OfferedDigest))
		pending, err := worker.PendingScopedViewDeliveries(t.Context(), key, 1)
		require.NoError(t, err)
		require.Empty(t, pending)
		require.Equal(t, settled, scopedAuthoritySnapshot(t, root), "both keys see the same fully settled stream")
	}
	deliveries, cursors := 0, 0
	for _, record := range settled {
		switch row := record.(type) {
		case *db.ScopedDeliveryRecord:
			if row.ScopedDeliveryStream != stream {
				continue
			}
			deliveries++
			require.NoError(t, row.Validate())
			require.True(t, row.Emitted)
			require.True(t, row.Acknowledged)
			require.GreaterOrEqual(t, row.Sequence, int64(1))
			require.LessOrEqual(t, row.Sequence, int64(4))
			require.True(t, db.SameScopedDeliveryIdentity(offers[row.Sequence-1], *row))
		case *db.ScopedDeliveryCursor:
			if row.ScopedDeliveryStream != stream {
				continue
			}
			cursors++
			require.Equal(t, offers[3].Sequence, row.EmittedSequence)
			require.Equal(t, offers[3].OfferedDigest, row.EmittedDigest)
			require.Equal(t, offers[3].Sequence, row.AcknowledgedSequence)
			require.Equal(t, offers[3].OfferedDigest, row.AcknowledgedDigest)
		}
	}
	require.Equal(t, 4, deliveries)
	require.Equal(t, 1, cursors)
	require.Equal(t, cursor, scopedViewsCursor(t, root))
	got, err := root.IssueByUID(t.Context(), native.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, native, got)
}

func RunScopedViewDelivery(t *testing.T, root db.Storage) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	key := f.Checkpoint.View.Key
	operations := scopedViewsOperations(t, root)
	require.NoError(t, operations.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
	require.NoError(t, operations.ActivateScopedCheckpoint(t.Context(), key, f.Checkpoint.Cut.Digest))
	title := "Published delivery content"
	_, err := root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
	require.NoError(t, err)
	capture, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), key)
	require.NoError(t, err)
	projected, err := db.ProjectScopedState(capture, f.Checkpoint.View)
	require.NoError(t, err)
	effects, err := db.DiffScopedState(f.Checkpoint.Collections, projected, f.Checkpoint.Cut.Sequence+1)
	require.NoError(t, err)
	require.NotEmpty(t, effects)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: f.Checkpoint.Cut.Sequence + 1, CaptureUID: capture.CaptureUID}, PreviousDigest: f.Checkpoint.Cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, project := range f.Checkpoint.View.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedBatch(batch, f.Checkpoint.View, pins))
	require.NoError(t, root.(interface {
		CommitScopedBatch(context.Context, db.ScopedBatch, string) error
	}).CommitScopedBatch(t.Context(), batch, capture.CaptureUID))
	before := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(before))
	offers := []db.ScopedDeliveryRecord{}
	for _, record := range before {
		if row, ok := record.(*db.ScopedDeliveryRecord); ok && row.Direction == db.ScopedDeliveryOutbound && row.Channel == db.ScopedDeliveryView {
			require.NoError(t, row.Validate())
			require.Equal(t, key.ViewUID, row.ObjectUID)
			require.False(t, row.Emitted)
			require.False(t, row.Acknowledged)
			offers = append(offers, *row)
		}
	}
	slices.SortFunc(offers, func(a, b db.ScopedDeliveryRecord) int {
		if a.Sequence < b.Sequence {
			return -1
		}
		if a.Sequence > b.Sequence {
			return 1
		}
		return 0
	})
	require.Len(t, offers, 2, "native checkpoint and capture publication establish two actual offers")
	require.Equal(t, int64(1), offers[0].Sequence)
	require.Equal(t, int64(2), offers[1].Sequence)
	require.Equal(t, offers[0].ScopedDeliveryStream, offers[1].ScopedDeliveryStream)
	require.Equal(t, f.Checkpoint.View.Route[0].BindingUID, offers[0].BindingUID)
	require.Equal(t, f.Checkpoint.View.Route[0].Epoch, offers[0].Epoch)
	for n, offer := range offers {
		header, body, err := db.DecodeScopedDelivery(offer.EnvelopeBytes)
		require.NoError(t, err)
		require.Equal(t, root.InstanceUID(), header.SenderInstanceUID)
		require.Equal(t, f.Checkpoint.View.Route[0].InstanceUID, header.ReceiverInstanceUID)
		want := scopedOwnerBytes(t, f.Checkpoint)
		if n == 1 {
			want = scopedOwnerBytes(t, batch)
		}
		require.Equal(t, want, body)
		require.Equal(t, db.ScopedOfferedDigest(offer.EnvelopeBytes), offer.OfferedDigest)
	}
	worker, ok := root.(interface {
		PendingScopedViewDeliveries(context.Context, db.ScopedViewKey, int) ([]db.ScopedDeliveryRecord, error)
		AckScopedViewDeliveries(context.Context, db.ScopedViewKey, int64, string) error
	})
	require.True(t, ok, "native complete-stream pending and exact endpoint acknowledgement helpers must be concrete")
	cursor := scopedViewsCursor(t, root)
	native, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = worker.PendingScopedViewDeliveries(t.Context(), key, 0)
	require.Error(t, err)
	require.Error(t, worker.AckScopedViewDeliveries(t.Context(), key, offers[1].Sequence, offers[1].OfferedDigest), "cannot acknowledge an un-emitted endpoint")
	require.Equal(t, before, scopedAuthoritySnapshot(t, root))
	pending, err := worker.PendingScopedViewDeliveries(t.Context(), key, 1)
	require.NoError(t, err)
	first := offers[0]
	first.Emitted = true
	require.Equal(t, []db.ScopedDeliveryRecord{first}, pending)
	emitted := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(emitted))
	again, err := worker.PendingScopedViewDeliveries(t.Context(), key, 1)
	require.NoError(t, err)
	require.Equal(t, pending, again, "lost responses retry the same immutable offer")
	require.Equal(t, emitted, scopedAuthoritySnapshot(t, root))
	require.Error(t, worker.AckScopedViewDeliveries(t.Context(), key, offers[1].Sequence, offers[1].OfferedDigest), "first emission cannot skip the second un-emitted offer")
	require.Error(t, worker.AckScopedViewDeliveries(t.Context(), key, first.Sequence, first.ObjectDigest), "object digest is not the emitted offered digest")
	require.Equal(t, emitted, scopedAuthoritySnapshot(t, root))
	require.NoError(t, worker.AckScopedViewDeliveries(t.Context(), key, first.Sequence, first.OfferedDigest))
	ackedFirst := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(ackedFirst))
	require.NoError(t, worker.AckScopedViewDeliveries(t.Context(), key, first.Sequence, first.OfferedDigest))
	require.Equal(t, ackedFirst, scopedAuthoritySnapshot(t, root), "exact endpoint acknowledgement retry is idempotent")
	pending, err = worker.PendingScopedViewDeliveries(t.Context(), key, 1)
	require.NoError(t, err)
	second := offers[1]
	second.Emitted = true
	require.Equal(t, []db.ScopedDeliveryRecord{second}, pending)
	require.NoError(t, worker.AckScopedViewDeliveries(t.Context(), key, second.Sequence, second.OfferedDigest))
	settled := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(settled))
	pending, err = worker.PendingScopedViewDeliveries(t.Context(), key, 1)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Equal(t, settled, scopedAuthoritySnapshot(t, root))
	withoutDelivery := func(records []db.ImportRecord) []db.ImportRecord {
		out := []db.ImportRecord{}
		for _, record := range records {
			switch record.(type) {
			case *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
				continue
			}
			out = append(out, record)
		}
		return out
	}
	require.Equal(t, withoutDelivery(before), withoutDelivery(settled), "settling delivery never rewrites signed inventory, canonical rows or capture work")
	deliveries, cursors := 0, 0
	for _, record := range settled {
		switch row := record.(type) {
		case *db.ScopedDeliveryRecord:
			deliveries++
			require.NoError(t, row.Validate())
			require.True(t, row.Emitted)
			require.True(t, row.Acknowledged)
			require.GreaterOrEqual(t, row.Sequence, int64(1))
			require.LessOrEqual(t, row.Sequence, int64(2))
			require.True(t, db.SameScopedDeliveryIdentity(offers[row.Sequence-1], *row), "exact hop transcript and offered identity survive settlement")
		case *db.ScopedDeliveryCursor:
			cursors++
			require.Equal(t, offers[0].ScopedDeliveryStream, row.ScopedDeliveryStream)
			require.Equal(t, second.Sequence, row.EmittedSequence)
			require.Equal(t, second.OfferedDigest, row.EmittedDigest)
			require.Equal(t, second.Sequence, row.AcknowledgedSequence)
			require.Equal(t, second.OfferedDigest, row.AcknowledgedDigest)
		}
	}
	require.Equal(t, 2, deliveries)
	require.Equal(t, 1, cursors)
	require.Equal(t, cursor, scopedViewsCursor(t, root))
	got, err := root.IssueByUID(t.Context(), native.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, native, got)
}

func RunScopedCommitBatch(t *testing.T, root db.Storage) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	worker, ok := root.(interface {
		CommitScopedBatch(context.Context, db.ScopedBatch, string) error
	})
	require.True(t, ok, "native immutable-capture batch publication must be concrete")
	key := f.Checkpoint.View.Key
	operations := scopedViewsOperations(t, root)
	require.NoError(t, operations.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
	require.NoError(t, operations.ActivateScopedCheckpoint(t.Context(), key, f.Checkpoint.Cut.Digest))
	require.Nil(t, ScopedCaptureWork(t, root, key.ViewUID).CaptureUID)
	baseline := scopedAuthoritySnapshot(t, root)
	var baselineDelivery *db.ScopedDeliveryRecord
	for _, record := range baseline {
		if row, ok := record.(*db.ScopedDeliveryRecord); ok && row.ObjectUID == key.ViewUID && row.ObjectDigest == f.Checkpoint.Cut.Digest {
			require.Nil(t, baselineDelivery)
			baselineDelivery = row
		}
	}
	require.NotNil(t, baselineDelivery, "actual root checkpoint publication established the native outbound stream")
	title := "Captured publication title"
	_, err := root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
	require.NoError(t, err)
	capture, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), key)
	require.NoError(t, err)
	frozen := ScopedCaptureWork(t, root, key.ViewUID)
	require.NotNil(t, frozen.CapturedRevision)
	require.NotNil(t, frozen.CaptureUID)
	require.Equal(t, capture.CaptureUID, *frozen.CaptureUID)
	projected, err := db.ProjectScopedState(capture, f.Checkpoint.View)
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(projected.Issues, func(issue db.ScopedIssueState) bool {
		return issue.UID == f.Root.Issue.UID && issue.Title == title
	}))
	effects, err := db.DiffScopedState(f.Checkpoint.Collections, projected, f.Checkpoint.Cut.Sequence+1)
	require.NoError(t, err)
	require.NotEmpty(t, effects)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: f.Checkpoint.View, Cut: db.ScopedCut{Sequence: f.Checkpoint.Cut.Sequence + 1, CaptureUID: capture.CaptureUID}, PreviousDigest: f.Checkpoint.Cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, project := range f.Checkpoint.View.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedBatch(batch, f.Checkpoint.View, pins))
	body := scopedOwnerBytes(t, batch)
	laterTitle := "Newer canonical title remains dirty"
	_, err = root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &laterTitle})
	require.NoError(t, err)
	newer := ScopedCaptureWork(t, root, key.ViewUID)
	require.Greater(t, newer.DirtyRevision, *frozen.CapturedRevision)
	require.Equal(t, frozen.CaptureBytes, newer.CaptureBytes)
	nativeBefore, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, laterTitle, nativeBefore.Title)
	before := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(before))
	cursor := scopedViewsCursor(t, root)
	require.Error(t, worker.CommitScopedBatch(t.Context(), batch, scopedOwnerUID(9950)), "wrong private capture UID cannot publish or complete")
	require.Equal(t, before, scopedAuthoritySnapshot(t, root))
	require.Equal(t, cursor, scopedViewsCursor(t, root))
	require.NoError(t, worker.CommitScopedBatch(t.Context(), batch, capture.CaptureUID), "publish the frozen cut without rereading newer canonical content")
	done := ScopedCaptureWork(t, root, key.ViewUID)
	require.Nil(t, done.CaptureUID)
	require.Nil(t, done.CapturedRevision)
	require.Nil(t, done.CaptureDigest)
	require.Nil(t, done.CaptureBytes)
	require.Equal(t, newer.DirtyRevision, done.DirtyRevision)
	require.Equal(t, "dirty", done.State, "exact completion preserves newer canonical work")
	got, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, nativeBefore, got, "root publication never replaces canonical rows with captured projection rows")
	require.Equal(t, cursor, scopedViewsCursor(t, root), "publication does not synthesize another canonical UI event")
	published := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(published))
	batches, deliveries, currentRows := 0, 0, 0
	for _, record := range published {
		switch row := record.(type) {
		case *db.ScopedBatchExport:
			if row.ViewUID == key.ViewUID && row.Generation == key.Generation && row.Sequence == batch.Cut.Sequence {
				batches++
				require.Equal(t, body, row.BatchBytes)
				require.Equal(t, capture.CaptureUID, row.CaptureUID)
				require.Equal(t, batch.Cut.Digest, row.Digest)
			}
		case *db.ScopedDeliveryRecord:
			if row.ObjectUID == key.ViewUID && row.ObjectDigest == batch.Cut.Digest {
				deliveries++
				require.Equal(t, baselineDelivery.ScopedDeliveryStream, row.ScopedDeliveryStream)
				require.Equal(t, baselineDelivery.Sequence+1, row.Sequence)
				header, deliveredBody, err := db.DecodeScopedDelivery(row.EnvelopeBytes)
				require.NoError(t, err)
				require.Equal(t, batch.Cut.Digest, header.ObjectDigest)
				require.Equal(t, body, deliveredBody)
				require.Equal(t, db.ScopedOfferedDigest(row.EnvelopeBytes), row.OfferedDigest)
			}
		case *db.ScopedRecordExport:
			if row.ViewUID == key.ViewUID && row.Generation == key.Generation && row.Collection == "issues" && row.RecordKey == f.Root.Projects[0].UID+":"+f.Root.Issue.UID {
				currentRows++
				var effect db.ScopedEffect
				require.NoError(t, db.DecodeScopedRecord(row.RecordBytes, &effect))
				require.NotNil(t, effect.Issue)
				require.Equal(t, title, effect.Issue.Title, "published read model comes from the immutable old cut")
			}
		}
	}
	require.Equal(t, 1, batches)
	require.Equal(t, 1, deliveries)
	require.Equal(t, 1, currentRows)
	require.NoError(t, worker.CommitScopedBatch(t.Context(), batch, capture.CaptureUID), "exact retry remains idempotent after its capture tuple completed")
	require.Equal(t, published, scopedAuthoritySnapshot(t, root))
	require.Equal(t, cursor, scopedViewsCursor(t, root))
}

func RunScopedCaptureObligationWorker(t *testing.T, root db.Storage, mode string) {
	t.Helper()
	require.Contains(t, []string{"no_effect_completion", "publication_ack"}, mode)
	f := NewScopedViewsFixture(t, root)
	key, view := f.Checkpoint.View.Key, f.Checkpoint.View
	operations := scopedViewsOperations(t, root)
	require.NoError(t, operations.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
	require.NoError(t, operations.ActivateScopedCheckpoint(t.Context(), key, f.Checkpoint.Cut.Digest))
	if mode == "publication_ack" {
		title := "Frozen publication title"
		_, err := root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
		require.NoError(t, err)
	}
	capturer := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	})
	capture, err := capturer.CaptureScopedView(t.Context(), key)
	require.NoError(t, err)
	projected, err := db.ProjectScopedState(capture, view)
	require.NoError(t, err)
	beforeCapture := ScopedCaptureWork(t, root, key.ViewUID)
	require.NotNil(t, beforeCapture.CaptureUID)
	require.NotNil(t, beforeCapture.CapturedRevision)
	require.NotNil(t, beforeCapture.CaptureDigest)
	require.Equal(t, capture.CaptureUID, *beforeCapture.CaptureUID)
	laterTitle := "Newer canonical title remains dirty"
	_, err = root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &laterTitle})
	require.NoError(t, err)
	newer := ScopedCaptureWork(t, root, key.ViewUID)
	require.Greater(t, newer.DirtyRevision, *beforeCapture.CapturedRevision)
	require.Equal(t, beforeCapture.CaptureBytes, newer.CaptureBytes)
	parent, err := root.ResolveAPIToken(t.Context(), "parent-test-token")
	require.NoError(t, err)
	principal := db.CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: view.Route[0].Epoch, AccountableActor: parent.Actor, RouteDigest: view.RouteDigest}
	principalKey, err := db.ScopedPrincipalKey(principal)
	require.NoError(t, err)
	request := db.ScopedConversionRequest{UID: scopedOwnerUID(10100), Digest: strings.Repeat("e", 64), CatalogEntryUID: f.Root.Params.CatalogEntryUID, RouteDigest: view.RouteDigest, Target: view, SuspendBindingUIDs: []string{}}
	for _, project := range view.Projects {
		request.ProjectUIDs = append(request.ProjectUIDs, project.ProjectUID)
	}
	conversion := &db.ScopedConversionExport{ConversionUID: request.UID, RequestDigest: request.Digest, RequestBytes: scopedOwnerBytes(t, request), OldBindingsBytes: []byte(`[]`), Phase: "prepare"}
	obligation := &db.ScopedConversionObligationExport{ConversionUID: request.UID, ObligationUID: scopedOwnerUID(10101), Kind: "capture", BindingUID: principal.BindingUID, ObjectUID: capture.CaptureUID, Digest: *newer.CaptureDigest, Epoch: principal.Epoch, PrincipalKey: principalKey, State: "pending"}
	pinBytes := map[string][]byte{"conversion": scopedOwnerBytes(t, conversion), "capture": scopedOwnerBytes(t, obligation)}
	records := append(scopedAuthoritySnapshot(t, root), conversion, obligation)
	require.NoError(t, db.ValidateImportRecords(records), "accepted T03 two-row capture-pin shape has a complete owner graph")
	require.NoError(t, root.ImportReplay(t.Context(), records, db.ImportOptions{}))
	// Replay can change local IDs; reload actual credentials/projects/grant.
	parent, err = root.ResolveAPIToken(t.Context(), "parent-test-token")
	require.NoError(t, err)
	anchor, err := root.ProjectByUID(t.Context(), f.Root.Projects[0].UID)
	require.NoError(t, err)
	grants, err := root.ListProjectFederationEnrollments(t.Context(), anchor.ID)
	require.NoError(t, err)
	matched := 0
	for _, grant := range grants {
		if grant.RelayBindingUID == principal.BindingUID {
			matched++
			require.Equal(t, db.ScopedRelayProtocolVersion, grant.RelayProtocolVersion)
			require.Equal(t, &parent.ID, grant.ParentTokenID)
			require.Equal(t, parent.Actor, grant.Actor)
			require.Equal(t, principal.Epoch, grant.RelayResetEpoch)
			require.Equal(t, view.Route[0].InstanceUID, grant.SpokeInstanceUID)
			require.Nil(t, grant.RevokedAt)
		}
	}
	require.Equal(t, 1, matched)
	pinRows := func(records []db.ImportRecord) map[string][]byte {
		rows := map[string][]byte{}
		for _, record := range records {
			switch row := record.(type) {
			case *db.ScopedConversionExport:
				if row.ConversionUID == request.UID {
					rows["conversion"] = scopedOwnerBytes(t, row)
				}
			case *db.ScopedConversionObligationExport:
				if row.ConversionUID == request.UID {
					rows["capture"] = scopedOwnerBytes(t, row)
				}
			}
		}
		return rows
	}
	before := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(before))
	require.Equal(t, pinBytes, pinRows(before), "exact pending conversion/capture obligations survive owner recovery")
	require.Equal(t, newer, ScopedCaptureWork(t, root, key.ViewUID))
	replayed, err := capturer.CaptureScopedView(t.Context(), key)
	require.NoError(t, err, "real native authority is independently rechecked, never borrowed from owner import")
	require.Equal(t, capture, replayed)
	require.Equal(t, before, scopedAuthoritySnapshot(t, root))
	native, err := root.IssueByUID(t.Context(), f.Root.Issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, laterTitle, native.Title)
	cursor := scopedViewsCursor(t, root)
	if mode == "no_effect_completion" {
		priorDigest, err := db.ScopedStateDigest(f.Checkpoint.Collections)
		require.NoError(t, err)
		captureDigest, err := db.ScopedStateDigest(projected)
		require.NoError(t, err)
		require.Equal(t, priorDigest, captureDigest, "immutable tuple is genuinely no-effect despite newer native current")
		require.ErrorIs(t, root.(interface {
			CompleteScopedViewCapture(context.Context, db.ScopedViewKey, string) error
		}).CompleteScopedViewCapture(t.Context(), key, capture.CaptureUID), db.ErrScopedCapturePending)
		require.Equal(t, before, scopedAuthoritySnapshot(t, root), "pin refuses completion without any whole-owner/allocator/cursor mutation")
		require.Equal(t, newer, ScopedCaptureWork(t, root, key.ViewUID))
		require.Equal(t, cursor, scopedViewsCursor(t, root))
		return
	}
	effects, err := db.DiffScopedState(f.Checkpoint.Collections, projected, f.Checkpoint.Cut.Sequence+1)
	require.NoError(t, err)
	require.NotEmpty(t, effects)
	batch, err := db.SignScopedBatch(db.ScopedBatch{View: view, Cut: db.ScopedCut{Sequence: f.Checkpoint.Cut.Sequence + 1, CaptureUID: capture.CaptureUID}, PreviousDigest: f.Checkpoint.Cut.Digest, Effects: effects}, f.Signers)
	require.NoError(t, err)
	pins := []db.RootKeyPin{}
	for _, project := range view.Projects {
		pins = append(pins, project.Pin)
	}
	require.NoError(t, db.VerifyScopedBatch(batch, view, pins))
	publisher := root.(interface {
		CommitScopedBatch(context.Context, db.ScopedBatch, string) error
		PendingScopedViewDeliveries(context.Context, db.ScopedViewKey, int) ([]db.ScopedDeliveryRecord, error)
		AckScopedViewDeliveries(context.Context, db.ScopedViewKey, int64, string) error
	})
	require.NoError(t, publisher.CommitScopedBatch(t.Context(), batch, capture.CaptureUID))
	published := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(published))
	require.Equal(t, pinBytes, pinRows(published))
	require.Equal(t, newer, ScopedCaptureWork(t, root, key.ViewUID), "successful publication cannot clear independently pinned capture or lose newer dirty work")
	require.NoError(t, publisher.CommitScopedBatch(t.Context(), batch, capture.CaptureUID))
	require.Equal(t, published, scopedAuthoritySnapshot(t, root), "exact retry allocates no extra delivery or work revision")
	pending, err := publisher.PendingScopedViewDeliveries(t.Context(), key, 2)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.Equal(t, int64(1), pending[0].Sequence)
	require.Equal(t, int64(2), pending[1].Sequence)
	_, body, err := db.DecodeScopedDelivery(pending[1].EnvelopeBytes)
	require.NoError(t, err)
	var retained db.ScopedBatch
	require.NoError(t, db.DecodeScopedRecord(body, &retained))
	require.Equal(t, batch, retained)
	require.NoError(t, publisher.AckScopedViewDeliveries(t.Context(), key, pending[1].Sequence, pending[1].OfferedDigest))
	settled := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(settled))
	withoutDelivery := func(records []db.ImportRecord) []db.ImportRecord {
		rows := []db.ImportRecord{}
		for _, record := range records {
			switch record.(type) {
			case *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
				continue
			}
			rows = append(rows, record)
		}
		return rows
	}
	require.Equal(t, withoutDelivery(published), withoutDelivery(settled), "ACK preserves every non-delivery row and native allocator")
	require.Equal(t, pinBytes, pinRows(settled), "delivery ACK never settles an independent capture obligation")
	require.Equal(t, newer, ScopedCaptureWork(t, root, key.ViewUID))
	require.Equal(t, cursor, scopedViewsCursor(t, root))
	got, err := root.IssueByUID(t.Context(), native.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, native, got)
	replayed, err = capturer.CaptureScopedView(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, capture, replayed, "owed capture stays immutable after publication and ACK")
	require.Equal(t, settled, scopedAuthoritySnapshot(t, root))
}

func RunScopedNoEffectCaptureCompletion(t *testing.T, root db.Storage) {
	t.Helper()
	f := NewScopedViewsFixture(t, root)
	worker, ok := root.(interface {
		CompleteScopedViewCapture(context.Context, db.ScopedViewKey, string) error
	})
	require.True(t, ok, "native no-visible-effect capture completion must be concrete")
	key := f.Checkpoint.View.Key
	captured := ScopedCaptureWork(t, root, key.ViewUID)
	require.Error(t, worker.CompleteScopedViewCapture(t.Context(), key, f.Checkpoint.Cut.CaptureUID), "initial capture has no authenticated prior view")
	require.Equal(t, captured, ScopedCaptureWork(t, root, key.ViewUID))
	operations := scopedViewsOperations(t, root)
	require.NoError(t, operations.PrepareScopedCheckpoint(t.Context(), f.Checkpoint))
	require.NoError(t, operations.ActivateScopedCheckpoint(t.Context(), key, f.Checkpoint.Cut.Digest))
	capture, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), key)
	require.NoError(t, err)
	frozen := ScopedCaptureWork(t, root, key.ViewUID)
	require.NotNil(t, frozen.CapturedRevision)
	require.Error(t, worker.CompleteScopedViewCapture(t.Context(), key, scopedOwnerUID(9900)))
	require.Equal(t, frozen, ScopedCaptureWork(t, root, key.ViewUID))

	// A prepared descriptor with a different selected set cannot consume this
	// active-old tuple. Its presence never changes the old capture's binding.
	prepared := f.Checkpoint.View
	prepared.Key.Generation++
	prepared.RequestDigest = strings.Repeat("5", 64)
	prepared.Projects = append([]db.ScopedProjectPin{}, prepared.Projects[:1]...)
	ScopedViewsPreparedGeneration(t, root, prepared)
	require.ErrorIs(t, worker.CompleteScopedViewCapture(t.Context(), prepared.Key, capture.CaptureUID), db.ErrScopedCapturePending)
	require.Equal(t, frozen, ScopedCaptureWork(t, root, key.ViewUID))

	// A later real mutation advances dirty work but cannot alter frozen bytes.
	// Completion must compare the tuple, not reread this newer canonical title.
	title := "Later visible title"
	_, err = root.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{IssueID: f.Root.Issue.ID, Actor: "member", Title: &title})
	require.NoError(t, err)
	newer := ScopedCaptureWork(t, root, key.ViewUID)
	require.Greater(t, newer.DirtyRevision, *frozen.CapturedRevision)
	require.Equal(t, frozen.CaptureBytes, newer.CaptureBytes)
	published := scopedViewsPublishedRows(t, root)
	cursor := scopedViewsCursor(t, root)
	require.NoError(t, worker.CompleteScopedViewCapture(t.Context(), key, capture.CaptureUID))
	require.Equal(t, cursor, scopedViewsCursor(t, root), "no-effect completion does not reserve another visible reset")
	done := ScopedCaptureWork(t, root, key.ViewUID)
	require.Nil(t, done.CaptureUID)
	require.Equal(t, newer.DirtyRevision, done.DirtyRevision)
	require.Equal(t, "dirty", done.State)
	require.Equal(t, published, scopedViewsPublishedRows(t, root), "no empty batch, sequence, delivery or rewritten current projection")

	visible, err := root.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), key)
	require.NoError(t, err)
	dirty := ScopedCaptureWork(t, root, key.ViewUID)
	require.Error(t, worker.CompleteScopedViewCapture(t.Context(), key, visible.CaptureUID), "visible content change requires signed publication")
	require.Equal(t, dirty, ScopedCaptureWork(t, root, key.ViewUID))
	require.Equal(t, published, scopedViewsPublishedRows(t, root))
}

func scopedViewsPublishedRows(t *testing.T, store db.Storage) []db.ImportRecord {
	t.Helper()
	result := []db.ImportRecord{}
	for _, record := range scopedAuthoritySnapshot(t, store) {
		switch record.(type) {
		case *db.ScopedGenerationExport, *db.ScopedBatchExport, *db.ScopedRecordExport, *db.ScopedDeliveryRecord, *db.ScopedDeliveryCursor:
			result = append(result, record)
		}
	}
	return result
}

func scopedViewsCursor(t *testing.T, store db.Storage) int64 {
	t.Helper()
	reader, ok := store.(interface {
		UIEventCursor(context.Context) (int64, error)
	})
	require.True(t, ok)
	cursor, err := reader.UIEventCursor(t.Context())
	require.NoError(t, err)
	return cursor
}

func typedOwedSourceFixture(t *testing.T, view db.ScopedViewDescriptor, binding db.FederationBindingExport, subject db.ScopedEndpoint, signer db.RootAttributionSigner) []db.ImportRecord {
	t.Helper()
	return typedOwedSourceFixtureForComment(t, view, binding, subject, signer, "")
}

func typedOwedSourceFixtureForComment(t *testing.T, view db.ScopedViewDescriptor, binding db.FederationBindingExport, subject db.ScopedEndpoint, signer db.RootAttributionSigner, commentUID string) []db.ImportRecord {
	t.Helper()
	require.Len(t, view.Route, 1, "this minimal fixture is the accepted one-hop shape")
	require.NoError(t, subject.Validate())
	config := binding.RelayConfig
	require.NotNil(t, config)
	require.True(t, binding.Enabled)
	require.Equal(t, string(db.FederationRoleSpoke), binding.Role)
	require.Equal(t, subject.ProjectUID, binding.HubProjectUID)
	require.NoError(t, config.Validate(view.DestinationUID))
	require.Equal(t, db.ScopedRelayProtocolVersion, config.ProtocolVersion)
	require.NotNil(t, config.ScopedView)
	require.Equal(t, view.Key, *config.ScopedView)
	require.Equal(t, view.RootCutoverUID, config.RootCutoverUID)
	require.Equal(t, view.AuthorityUID, config.AuthorityUID)
	require.Equal(t, view.Route[0].BindingUID, config.BindingUID)
	require.Equal(t, view.Route[0].Epoch, config.ResetEpoch)
	require.Equal(t, view.DestinationUID, view.Route[0].InstanceUID)
	principal := db.CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: config.BindingUID, Epoch: config.ResetEpoch, AccountableActor: binding.Actor, RouteDigest: view.RouteDigest}
	require.NoError(t, principal.Validate())
	principalBytes, err := json.Marshal(principal)
	require.NoError(t, err)
	principalKey, err := db.ScopedPrincipalKey(principal)
	require.NoError(t, err)

	var selected db.ScopedProjectPin
	found := false
	for _, project := range view.Projects {
		if project.ProjectUID == subject.ProjectUID {
			selected, found = project, true
		}
	}
	require.True(t, found)
	require.Equal(t, view.AuthorityUID, signer.AuthorityUID)
	require.Equal(t, ed25519.PrivateKeySize, len(signer.PrivateKey))
	require.Equal(t, selected.Pin.PublicKey, []byte(signer.PrivateKey.Public().(ed25519.PublicKey)))
	proof := db.ScopedMembershipProof{Kind: "root_membership", Endpoint: subject}
	transcript, err := db.ScopedMembershipSigningBytes(proof, principal)
	require.NoError(t, err)
	proof.Signatures = []db.ScopedSignature{{ProjectUID: subject.ProjectUID, KeyID: selected.Pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signer.PrivateKey, transcript))}}
	require.NoError(t, db.VerifyScopedMembership(proof, principal, []db.RootKeyPin{selected.Pin}))
	proofBytes, err := json.Marshal(proof)
	require.NoError(t, err)

	// Same native metadata body shape/clock/whitespace as the accepted T05
	// whole-manifest fixture; only identities select this genuine native issue.
	operationUID, eventUID := "00000000000000000000001900", "00000000000000000000001901"
	event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: view.DestinationUID, ProjectUID: subject.ProjectUID, IssueUID: &subject.IssueUID, Type: "issue.metadata_updated", Actor: "source-author", HLCPhysicalMS: 100, HLCCounter: 0, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Payload: []byte("{ \"diff\" : { \"key0\" : {\"from\":null,\"to\":\"value\"} } } ")}
	commandKind := "patchIssueMetadata"
	if commentUID != "" {
		commandKind = "editComment"
		event.Type = "issue.comment_edited"
		event.Payload, err = json.Marshal(struct {
			CommentUID string `json:"comment_uid"`
			Body       string `json:"body"`
			EditedAt   string `json:"edited_at"`
		}{CommentUID: commentUID, Body: "Pending local comment edit", EditedAt: "2026-10-01T00:00:00Z"})
		require.NoError(t, err)
	}
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	native, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	native = append(append([]byte(" \n"), native...), []byte("\n ")...)
	operation := db.SourceOperation{Version: 1, UID: operationUID, SourceInstanceUID: view.DestinationUID, CommandKind: commandKind, Principal: principal, Members: []db.SourceMember{{ProjectUID: subject.ProjectUID, EventUID: eventUID, ContentHash: event.ContentHash, Ordinal: 0}}, Membership: []db.ScopedMembershipProof{proof}}
	operation.Digest, err = db.SourceOperationDigest(operation)
	require.NoError(t, err)
	wire, err := db.EncodeScopedSourceSubmission(db.SourceSubmission{Operation: operation, Originals: []db.RemoteEvent{event}, OriginalBytes: [][]byte{native}})
	require.NoError(t, err)
	wire = append(append([]byte(" \n"), wire...), []byte("\n ")...)
	decoded, err := db.DecodeScopedSourceSubmission(wire)
	require.NoError(t, err)
	require.True(t, bytes.Equal(native, decoded.OriginalBytes[0]))
	require.True(t, bytes.Equal(wire, decoded.WireBytes))
	evidence, err := json.Marshal(db.ScopedAdmissionEvidence{Version: 1, OperationUID: operationUID, OperationDigest: operation.Digest, Principal: principal})
	require.NoError(t, err)
	ordinal := int64(0)
	records := []db.ImportRecord{
		&db.ScopedSourceOperationExport{OperationUID: operationUID, Digest: operation.Digest, SourceInstanceUID: view.DestinationUID, CommandKind: operation.CommandKind, ManifestBytes: wire, PrincipalBytes: principalBytes, PrincipalKey: principalKey, State: "staged", OverlayState: "pending", AdmissionBytes: evidence, RequestFingerprint: db.ScopedOfferedDigest(wire)},
		&db.ScopedPrivateSourceExport{EventUID: eventUID, ContentHash: event.ContentHash, ProjectUID: subject.ProjectUID, OperationUID: &operationUID, Ordinal: &ordinal, EventBytes: native},
		&db.ScopedSourceCustodyExport{OperationUID: operationUID, BindingUID: principal.BindingUID, Epoch: principal.Epoch, Direction: "outbound", PrincipalBytes: principalBytes, ManifestDigest: operation.Digest, OwnsComplete: true, ResultAcknowledged: false, InvalidationState: "none"},
		&db.ScopedSourceDependencyExport{OperationUID: operationUID, ProjectUID: subject.ProjectUID, IssueUID: subject.IssueUID, ProofBytes: proofBytes, State: "satisfied"},
	}
	for _, record := range records {
		require.NoError(t, db.ValidateScopedImportRecord(record))
	}
	return records
}

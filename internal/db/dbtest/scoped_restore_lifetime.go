package dbtest

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunScopedOwnerBackupRequiresRootWork starts with a real native grant and a
// complete owner export. Omitting its root work row must fail before replacement
// can leave an otherwise authorized view unable to capture current state.
func RunScopedOwnerBackupRequiresRootWork(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	s := open(t)
	f := NewScopedCaptureFixture(t, s)
	records := scopedAuthoritySnapshot(t, s)
	require.NoError(t, db.ValidateImportRecords(records), "unmodified native owner backup is valid")
	restored := open(t)
	require.NoError(t, restored.ImportReplay(t.Context(), records, db.ImportOptions{}))
	_, err := restored.(interface {
		CaptureScopedView(context.Context, db.ScopedViewKey) (db.CanonicalViewState, error)
	}).CaptureScopedView(t.Context(), f.Params.View.Key)
	require.NoError(t, err, "unmodified owner restore retains usable native capture work")

	withoutWork := make([]db.ImportRecord, 0, len(records))
	withoutInstance := make([]db.ImportRecord, 0, len(records))
	withoutInstanceAndWork := make([]db.ImportRecord, 0, len(records))
	removed, removedInstance := 0, 0
	for _, record := range records {
		work, isWork := record.(*db.ScopedWorkExport)
		omitWork := isWork && work.ViewUID == f.Params.View.Key.ViewUID
		meta, isMeta := record.(*db.MetaKV)
		omitInstance := isMeta && meta.Key == "instance_uid"
		if omitWork {
			removed++
		} else {
			withoutWork = append(withoutWork, record)
		}
		if omitInstance {
			removedInstance++
		} else {
			withoutInstance = append(withoutInstance, record)
		}
		if !omitWork && !omitInstance {
			withoutInstanceAndWork = append(withoutInstanceAndWork, record)
		}
	}
	require.Equal(t, 1, removed, "remove only the actual grant's root work row")
	require.Equal(t, 1, removedInstance, "remove only the actual owner's instance identity")
	for _, tc := range []struct {
		name    string
		records []db.ImportRecord
	}{
		{"missing_root_work", withoutWork},
		{"missing_owner", withoutInstance},
		{"missing_owner_and_work", withoutInstanceAndWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("validation", func(t *testing.T) {
				require.Error(t, db.ValidateImportRecords(tc.records), "root backup requires its actual owner identity and native work row")
			})
			t.Run("restore_atomic", func(t *testing.T) {
				target := open(t)
				_, err := target.CreateProject(t.Context(), "restore-sentinel")
				require.NoError(t, err)
				before := scopedAuthoritySnapshot(t, target)
				err = target.ImportReplay(t.Context(), tc.records, db.ImportOptions{})
				require.Error(t, err, "reject incomplete root ownership before replacing the target")
				require.Equal(t, before, scopedAuthoritySnapshot(t, target), "rejected owner restore has no partial effects")
			})
		})
	}
}

// RunScopedRetainedProjectLifetime starts with the existing complete signed
// owner-backup fixture through the actual native replacement API. It establishes
// no native enrollment/activation; it checks the supported post-restore lifetime
// contract: an unbound project still referenced by retained state cannot vanish.
func RunScopedRetainedProjectLifetime(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, context := range []string{"generation", "local_source", "root_floor", "cutover_only", "control_only", "predicate_only", "import_only"} {
		t.Run(context, func(t *testing.T) {
			for _, operation := range []string{"merge", "purge"} {
				t.Run(operation, func(t *testing.T) {
					s := open(t)
					f := ScopedOwnerBackupFixture(t)
					if context != "generation" {
						var records []db.ImportRecord
						for _, record := range f.Records {
							keep := !db.IsScopedImportRecord(record)
							if context == "local_source" {
								switch record.(type) {
								case *db.ScopedSourceOperationExport:
									pending := *record.(*db.ScopedSourceOperationExport)
									// Accepted local work can precede coverage and any view generation.
									pending.OverlayState = "pending"
									record = &pending
									keep = true
								case *db.ScopedPrivateSourceExport, *db.ScopedSourceDependencyExport:
									keep = true
								}
							} else {
								switch row := record.(type) {
								case *db.ScopedProtocolCutoverExport:
									keep = context == "root_floor" || context == "cutover_only"
								case *db.ScopedProjectExport:
									root := *row
									root.OwnerViewUID, root.OwnerGeneration = nil, nil
									record = &root
									keep = context == "root_floor"
								case *db.ScopedControlCustodyExport:
									keep = context == "control_only"
								case *db.ScopedPredicateIncarnationExport:
									keep = context == "predicate_only"
								}
							}
							if keep {
								records = append(records, record)
							}
						}
						if context == "import_only" {
							stamp := time.Unix(1, 0).UTC()
							op := db.SourceOperation{Version: 3, UID: f.Operation.UID, SourceInstanceUID: f.Operation.SourceInstanceUID, CommandKind: "importIssues", Principal: f.Principal, ImportIntent: &db.ScopedImportIntent{ProjectUID: f.Active.Projects[0].ProjectUID, Source: "example-import", Items: []db.ScopedImportItem{{ExternalID: "external-key", Title: "Imported work", Author: "member", Status: "open", CreatedAt: stamp, UpdatedAt: stamp}}}}
							var err error
							op.Digest, err = db.SourceOperationDigest(op)
							require.NoError(t, err)
							manifest, err := db.EncodeScopedSourceSubmission(db.SourceSubmission{Operation: op})
							require.NoError(t, err)
							key, err := db.ScopedPrincipalKey(op.Principal)
							require.NoError(t, err)
							records = append(records, &db.ScopedSourceOperationExport{OperationUID: op.UID, Digest: op.Digest, SourceInstanceUID: op.SourceInstanceUID, CommandKind: op.CommandKind, ManifestBytes: manifest, PrincipalBytes: scopedOwnerBytes(t, op.Principal), PrincipalKey: key, State: "staged", OverlayState: "pending", RequestFingerprint: db.ScopedOfferedDigest(manifest)})
						}
						f.Records = records
					}
					require.NoError(t, db.ValidateImportRecords(f.Records))
					require.NoError(t, s.ImportReplay(t.Context(), f.Records, db.ImportOptions{}))
					require.Equal(t, f.Active.DestinationUID, s.InstanceUID())
					project, err := s.ProjectByUID(t.Context(), f.Active.Projects[0].ProjectUID)
					require.NoError(t, err)
					_, err = s.FederationBindingByProject(t.Context(), project.ID)
					require.ErrorIs(t, err, db.ErrNotFound, "the accepted owner snapshot is genuinely unbound; no binding was removed")
					target, err := s.CreateProject(t.Context(), "retirement-target")
					require.NoError(t, err)
					if operation == "purge" {
						_, event, err := s.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: project.ID, Actor: "member", Force: true})
						require.NoError(t, err, "reach the actual archived-project purge precondition")
						require.NotNil(t, event)
					}
					before := scopedAuthoritySnapshot(t, s)
					require.NoError(t, db.ValidateImportRecords(before), "native post-restore state must remain complete before UID retirement")
					switch operation {
					case "merge":
						_, err = s.MergeProjects(t.Context(), db.MergeProjectsParams{SourceProjectID: project.ID, TargetProjectID: target.ID, Actor: "member"})
					case "purge":
						_, err = s.PurgeProject(t.Context(), db.PurgeProjectParams{ProjectID: project.ID, Actor: "member"})
					}
					after := scopedAuthoritySnapshot(t, s)
					if err != nil {
						require.Equal(t, before, after, "retained-context refusal preserves the entire owner snapshot")
					} else {
						require.NoError(t, db.ValidateImportRecords(after), "successful UID retirement must not strand immutable generation/cutover/custody dependencies")
						require.NoError(t, open(t).ImportReplay(t.Context(), after, db.ImportOptions{}))
					}
				})
			}
		})
	}
}

// RunScopedCloseSubjectFirstOwnerBackup delivers only native root-produced
// originals through the same floor2 incoming fixture as the replica lifetime
// tests. A missing cross-project peer is an ordinary arrival-order state.
func RunScopedCloseSubjectFirstOwnerBackup(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	root, replica := open(t), open(t)
	f := NewScopedCaptureFixture(t, root)
	peer, _, err := root.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Later close peer"})
	require.NoError(t, err)
	value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: peer.ProjectUID, PeerIssueUID: peer.UID}
	_, closeEvents, changed, err := root.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: f.Issue.ID, Actor: "member", Reason: "duplicate", CloseValue: value})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, closeEvents)
	require.Equal(t, value, ScopedCurrentClose(t, root, f.Issue.UID).Value)
	rootRecords := scopedAuthoritySnapshot(t, root)
	require.NoError(t, db.ValidateImportRecords(rootRecords))
	var rootWithoutPeer []db.ImportRecord
	removed := 0
	for _, record := range rootRecords {
		if issue, ok := record.(*db.IssueExport); ok && issue.UID == peer.UID {
			removed++
			continue
		}
		rootWithoutPeer = append(rootWithoutPeer, record)
	}
	require.Equal(t, 1, removed)
	require.ErrorContains(t, db.ValidateImportRecords(rootWithoutPeer), "canonical close peer missing", "a canonical root cannot use the incoming-replica exception")
	view, projects := NewScopedIncomingAuthorityFixture(t, replica)
	InstallScopedIncomingAuthority(t, replica, view, projects)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, replica)), "complete empty incoming fixture is valid before source arrival")
	for n, project := range f.Projects {
		require.Equal(t, project.UID, projects[n].UID)
		events, err := root.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 1000})
		require.NoError(t, err)
		var originals []db.RemoteEvent
		for _, event := range events {
			if event.IssueUID != nil {
				originals = append(originals, db.RemoteEventFromStored(event))
			}
		}
		require.NotEmpty(t, originals)
		inserted, err := replica.InsertRemoteEvents(t.Context(), projects[n].ID, originals)
		require.NoError(t, err, "native pull admission accepts genuine root originals")
		require.Len(t, inserted, len(originals))
		for _, fresh := range inserted {
			require.True(t, fresh)
		}
		require.NoError(t, replica.MaterializeFederatedProject(t.Context(), projects[n].ID))
		phase := "peer_arrived"
		if n == 0 {
			phase = "subject_only"
			_, err := replica.IssueByUID(t.Context(), peer.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound, "no peer original has arrived")
		}
		current, err := replica.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, "closed", current.Status)
		scopedRestoreAssertOriginal(t, replica, closeEvents[0])
		t.Run(phase, func(t *testing.T) {
			records := scopedAuthoritySnapshot(t, replica)
			require.NoError(t, db.ValidateImportRecords(records), "native materialization must remain full-owner restorable at every arrival boundary")
			if n == 1 {
				changed := append([]db.ImportRecord(nil), records...)
				for i, record := range changed {
					if close, ok := record.(*db.ScopedCanonicalCloseExport); ok && close.IssueUID == f.Issue.UID {
						bad := *close
						wrongProject := *value
						wrongProject.PeerProjectUID = f.Projects[0].UID
						bad.ValueBytes, err = json.Marshal(wrongProject)
						require.NoError(t, err)
						changed[i] = &bad
						break
					}
				}
				require.ErrorContains(t, db.ValidateImportRecords(changed), "canonical close peer missing or belongs to another project", "an arrived peer must still match its actual project")
			}
			restored := open(t)
			require.NoError(t, restored.ImportReplay(t.Context(), records, db.ImportOptions{}))
			restoredSubject, err := restored.IssueByUID(t.Context(), f.Issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, current.Status, restoredSubject.Status)
			scopedRestoreAssertOriginal(t, restored, closeEvents[0])
		})
	}
}

// RunScopedCloseUnboundProjectLifetime never removes a binding or installs a
// synthetic project floor. The floor2 case creates ordinary unbound peer/target
// projects after establishing a real capture fixture on the subject's root.
func RunScopedCloseUnboundProjectLifetime(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, floor := range []string{"floor1", "floor2"} {
		t.Run(floor, func(t *testing.T) {
			for _, operation := range []string{"merge_peer", "purge_peer"} {
				t.Run(operation, func(t *testing.T) {
					s := open(t)
					var subject db.Issue
					if floor == "floor2" {
						f := NewScopedCaptureFixture(t, s)
						subject = f.Issue
					} else {
						project, err := s.CreateProject(t.Context(), "owner-project")
						require.NoError(t, err)
						var errCreate error
						subject, _, errCreate = s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Ordinary close subject"})
						require.NoError(t, errCreate)
					}
					source, err := s.CreateProject(t.Context(), "lifetime-source")
					require.NoError(t, err)
					target, err := s.CreateProject(t.Context(), "lifetime-target")
					require.NoError(t, err)
					for _, project := range []db.Project{source, target} {
						_, err := s.FederationBindingByProject(t.Context(), project.ID)
						require.ErrorIs(t, err, db.ErrNotFound, "new projects are genuinely unbound")
					}
					peer, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Author: "member", Title: "Unbound close peer"})
					require.NoError(t, err)
					value := &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: source.UID, PeerIssueUID: peer.UID}
					_, closeEvents, changed, err := s.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: subject.ID, Actor: "member", Reason: "duplicate", CloseValue: value})
					require.NoError(t, err)
					require.True(t, changed)
					require.NotEmpty(t, closeEvents)
					if floor == "floor2" {
						require.Equal(t, value, ScopedCurrentClose(t, s, subject.UID).Value, "native root actually retains a proof naming the unbound peer")
					}
					if operation == "purge_peer" {
						_, _, err = s.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: source.ID, Actor: "member", Force: true})
						require.NoError(t, err, "archive the actual unbound peer project through native lifecycle")
					}
					before := scopedAuthoritySnapshot(t, s)
					require.NoError(t, db.ValidateImportRecords(before), "complete native owner graph is valid before the lifetime mutation")
					require.NoError(t, open(t).ImportReplay(t.Context(), before, db.ImportOptions{}), "unmodified native owner backup restores first")
					switch operation {
					case "merge_peer":
						_, err = s.MergeProjects(t.Context(), db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "member"})
					case "purge_peer":
						_, err = s.PurgeProject(t.Context(), db.PurgeProjectParams{ProjectID: source.ID, Actor: "member"})
					}
					require.NoError(t, err, "legal unbound project lifecycle remains supported at both protocol floors")
					movedPeer, peerErr := s.IssueByUID(t.Context(), peer.UID, db.IncludeDeletedYes)
					if operation == "merge_peer" {
						require.NoError(t, peerErr)
						require.Equal(t, target.UID, movedPeer.ProjectUID, "native merge actually relocates the peer")
					} else {
						require.ErrorIs(t, peerErr, db.ErrNotFound, "native purge actually removes the peer")
					}
					if floor == "floor2" {
						require.Nil(t, ScopedCurrentClose(t, s, subject.UID).Value, "lifetime mutation invalidates the old typed peer value")
					}
					after := scopedAuthoritySnapshot(t, s)
					require.NoError(t, db.ValidateImportRecords(after), "a committed merge/purge must not strand a retained close peer")
					require.NoError(t, open(t).ImportReplay(t.Context(), after, db.ImportOptions{}), "post-mutation owner graph remains restorable")
					scopedRestoreAssertOriginal(t, s, closeEvents[0])
				})
			}
		})
	}
}

func scopedRestoreAssertOriginal(t *testing.T, s db.Storage, original db.Event) {
	t.Helper()
	project, err := s.ProjectByUID(t.Context(), original.ProjectUID)
	require.NoError(t, err)
	events, err := s.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 1000})
	require.NoError(t, err)
	for _, event := range events {
		if event.UID == original.UID {
			require.Equal(t, original.Payload, event.Payload, "current-proof repair must preserve original close bytes")
			require.Equal(t, original.ContentHash, event.ContentHash)
			return
		}
	}
	t.Fatal("native original close is missing")
}

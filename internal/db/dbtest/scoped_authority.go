package dbtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type ScopedAuthorityFixture struct {
	Params   db.CreateScopedRelayEnrollmentParams
	Parent   db.APIToken
	Projects [2]db.Project
	Teams    [2]db.Team
	Denied   db.Project
	Issue    db.Issue
}

// NewScopedAuthorityFixture imports retained root context only. Native teams,
// credentials, project policy, bindings, links and the grant use real APIs.
func NewScopedAuthorityFixture(t *testing.T, store db.Storage, withCrossProjectLink ...bool) ScopedAuthorityFixture {
	t.Helper()
	require.LessOrEqual(t, len(withCrossProjectLink), 1, "fixture accepts at most one link-construction option")
	ctx := t.Context()
	root := ScopedOwnerRootIdleFixture(t)
	var records []db.ImportRecord
	for _, r := range root.Records {
		switch r.(type) {
		case *db.MetaKV, *db.ProjectExport, *db.RootKeyPin, *db.ScopedProjectExport, *db.ScopedProtocolCutoverExport:
			records = append(records, r)
		}
	}
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))
	require.Equal(t, root.View.AuthorityUID, store.InstanceUID())
	f := ScopedAuthorityFixture{}
	view := root.View
	view.Key = db.ScopedViewKey{ViewUID: fmt.Sprintf("%026d", 210), Generation: 1}
	for n, pin := range view.Projects {
		var err error
		f.Projects[n], err = store.ProjectByUID(ctx, pin.ProjectUID)
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, f.Projects[n].ID, "admin")
		require.NoError(t, err)
		f.Teams[n], _, err = store.CreateTeam(ctx, []string{"anchor-team", "peer-team"}[n], "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: pin.ProjectUID, Visibility: "teams", TeamUIDs: []string{f.Teams[n].UID}}, "admin")
		require.NoError(t, err)
	}
	var err error
	f.Parent, _, err = store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "parent-test-token", Actor: "member", AdminActor: "admin", TeamUIDs: []string{f.Teams[0].UID, f.Teams[1].UID}})
	require.NoError(t, err)
	f.Denied, err = store.CreateProject(ctx, "denied-project")
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.Denied.UID, Visibility: "teams"}, "admin")
	require.NoError(t, err)
	f.Issue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Author: "member", Title: "Anchor work"})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Peer work"})
	require.NoError(t, err)
	if len(withCrossProjectLink) == 0 || withCrossProjectLink[0] {
		_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: f.Issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "member"})
		require.NoError(t, err)
	}
	f.Params = db.CreateScopedRelayEnrollmentParams{View: view, CatalogEntryUID: fmt.Sprintf("%026d", 211), AnchorProjectID: f.Projects[0].ID, ParentTokenID: f.Parent.ID, Token: "scoped-transport-test-token", ServeDownstream: true}
	return f
}

func scopedAuthoritySnapshot(t *testing.T, s db.Storage) []db.ImportRecord {
	t.Helper()
	snapshot, release, err := s.(interface {
		BeginExportSnapshot(context.Context) (db.Storage, func() error, error)
	}).BeginExportSnapshot(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, release()) }()
	s = snapshot
	records, err := CollectImportRecords(t.Context(), s, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	for record, err := range s.ExportAttribution(t.Context(), db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, record)
	}
	for record, err := range s.ExportProjectAccess(t.Context()) {
		require.NoError(t, err)
		records = append(records, record)
	}
	exporter := s.(interface {
		ExportScopedState(context.Context) iter.Seq2[db.ImportRecord, error]
	})
	for record, err := range exporter.ExportScopedState(t.Context()) {
		require.NoError(t, err)
		records = append(records, record)
	}
	for record, err := range s.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		records = append(records, record)
	}
	return records
}

func RunScopedAuthority(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	t.Run("real_parent_atomic_prepared_replay", func(t *testing.T) {
		s := open(t)
		f := NewScopedAuthorityFixture(t, s)
		ctx := t.Context()
		native, ok := s.(db.ScopedRelayEnrollmentStore)
		require.True(t, ok, "real optional native constructor required")
		events, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
		require.NoError(t, err)
		got, err := native.CreateScopedRelayEnrollment(ctx, f.Params)
		require.NoError(t, err)
		parent, err := s.ResolveAPIToken(ctx, "parent-test-token")
		require.NoError(t, err)
		require.Equal(t, &parent.ID, got.Enrollment.ParentTokenID)
		require.Equal(t, parent.Actor, got.Enrollment.Actor)
		require.Equal(t, &f.Projects[0].ID, got.Enrollment.ProjectID)
		require.Equal(t, db.ScopedRelayProtocolVersion, got.Enrollment.RelayProtocolVersion)
		require.Equal(t, f.Params.View.Route[0].BindingUID, got.Enrollment.RelayBindingUID)
		require.Equal(t, f.Params.View.Route[0].InstanceUID, got.Enrollment.SpokeInstanceUID)
		require.Equal(t, f.Params.View.Route[0].Epoch, got.Enrollment.RelayResetEpoch)
		afterEvents, err := s.EventsAfter(ctx, db.EventsAfterParams{Limit: 1000})
		require.NoError(t, err)
		require.Equal(t, events, afterEvents, "no v1 snapshot or source seed")
		records := scopedAuthoritySnapshot(t, s)
		require.NoError(t, db.ValidateImportRecords(records), "complete owner graph remains restorable")
		counts := map[string]int{}
		for _, r := range records {
			switch row := r.(type) {
			case *db.ScopedViewExport:
				counts["view"]++
				require.Nil(t, row.ActiveGeneration)
				require.Equal(t, &f.Params.View.Key.Generation, row.PreparedGeneration)
				require.Equal(t, "prepared", row.State)
				require.Equal(t, f.Params.CatalogEntryUID, row.CatalogEntryUID)
			case *db.ScopedGenerationExport:
				counts["generation"]++
				require.Nil(t, row.CheckpointBytes)
				require.Nil(t, row.CheckpointDigest)
				require.Zero(t, row.CutSequence)
				require.Empty(t, row.CutDigest)
				require.Zero(t, row.AssembledBytes)
				require.Equal(t, "prepared", row.State)
				var descriptor db.ScopedViewDescriptor
				require.NoError(t, json.Unmarshal(row.DescriptorBytes, &descriptor))
				require.Equal(t, f.Params.View, descriptor)
			case *db.ScopedWorkExport:
				counts["work"]++
				require.EqualValues(t, 1, row.DirtyRevision)
				require.Equal(t, "dirty", row.State)
				require.Nil(t, row.CaptureUID)
				require.Nil(t, row.CaptureBytes)
				require.Nil(t, row.CaptureDigest)
				require.Nil(t, row.CapturedRevision)
			case *db.ScopedProjectExport:
				require.Nil(t, row.OwnerViewUID)
				require.Nil(t, row.OwnerGeneration)
			case *db.ScopedRecordExport, *db.ScopedBatchExport, *db.ScopedDeliveryRecord:
				t.Fatal("initial admission created publication/materialization/transport state")
			}
		}
		require.Equal(t, map[string]int{"view": 1, "generation": 1, "work": 1}, counts)
		replay, err := native.CreateScopedRelayEnrollment(ctx, f.Params)
		require.NoError(t, err)
		require.Equal(t, got, replay)
		require.Equal(t, records, scopedAuthoritySnapshot(t, s), "retry preserves all retained bytes")
		wrong := f.Params
		wrong.AnchorProjectID = f.Projects[1].ID
		_, err = native.CreateScopedRelayEnrollment(ctx, wrong)
		require.Error(t, err)
		require.Equal(t, records, scopedAuthoritySnapshot(t, s))
		grants, err := s.ListFederationEnrollments(ctx)
		require.NoError(t, err)
		require.Len(t, grants, 1)
	})

	t.Run("max_full_route_early_adjacent_forwarder", func(t *testing.T) {
		s := open(t)
		f := NewScopedAuthorityFixture(t, s)
		route := []db.ScopedHopBinding{}
		for n := 0; n < db.MaxRelayHubs; n++ {
			route = append(route, db.ScopedHopBinding{BindingUID: fmt.Sprintf("%026d", 300+n), InstanceUID: fmt.Sprintf("%026d", 400+n), Epoch: 1})
		}
		f.Params.View.Route = route
		f.Params.View.DestinationUID = route[len(route)-1].InstanceUID
		got, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), f.Params)
		require.NoError(t, err, "full root-to-leaf route does not consume the first hop's hub budget")
		require.Equal(t, route[0].InstanceUID, got.Enrollment.SpokeInstanceUID)
		require.Equal(t, route[0].BindingUID, got.Enrollment.RelayBindingUID)
	})
	t.Run("leaf_grant_cannot_promise_later_hop", func(t *testing.T) {
		s := open(t)
		f := NewScopedAuthorityFixture(t, s)
		f.Params.View.Route = append(f.Params.View.Route, db.ScopedHopBinding{BindingUID: fmt.Sprintf("%026d", 301), InstanceUID: fmt.Sprintf("%026d", 401), Epoch: 1})
		f.Params.View.DestinationUID = f.Params.View.Route[1].InstanceUID
		f.Params.ServeDownstream = false
		_, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), f.Params)
		require.Error(t, err)
	})
	t.Run("over_full_route", func(t *testing.T) {
		s := open(t)
		f := NewScopedAuthorityFixture(t, s)
		route := []db.ScopedHopBinding{}
		for n := 0; n <= db.MaxRelayHubs; n++ {
			route = append(route, db.ScopedHopBinding{BindingUID: fmt.Sprintf("%026d", 300+n), InstanceUID: fmt.Sprintf("%026d", 400+n), Epoch: 1})
		}
		f.Params.View.Route = route
		f.Params.View.DestinationUID = route[len(route)-1].InstanceUID
		require.NoError(t, f.Params.View.Validate(), "descriptor bound is broader than native admission")
		_, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), f.Params)
		require.Error(t, err)
	})
	for _, name := range []string{"revoked_parent", "second_project_loss", "expired_parent", "scoped_parent", "attribution_ceiling", "pin_change", "binding_disabled", "cross_account_rebind", "revoked_grant", "unselected_anchor"} {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			f := NewScopedAuthorityFixture(t, s)
			ctx := t.Context()
			native := s.(db.ScopedRelayEnrollmentStore)
			grant, err := native.CreateScopedRelayEnrollment(ctx, f.Params)
			require.NoError(t, err)
			params := f.Params
			switch name {
			case "revoked_parent":
				_, _, err = s.RevokeAPIToken(ctx, f.Parent.ID, "admin")
			case "second_project_loss":
				_, err = s.SetTeamMembership(ctx, f.Teams[1].UID, "member", false, "admin")
			case "expired_parent":
				expiry := time.Now().UTC().Add(-time.Hour)
				var parent db.APIToken
				parent, _, err = s.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "expired-parent-test-token", Actor: "member", AdminActor: "admin", ExpiresAt: &expiry})
				params.ParentTokenID = parent.ID
				params.RebindParent = true
			case "scoped_parent":
				expiry := time.Now().UTC().Add(time.Hour)
				var parent db.APIToken
				parent, _, err = s.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "subtree-parent-test-token", Actor: "member", AdminActor: "admin", ExpiresAt: &expiry, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.Projects[0].UID, RootIssueUID: f.Issue.UID}})
				params.ParentTokenID = parent.ID
				params.RebindParent = true
			case "attribution_ceiling":
				ctx = db.WithAuthorizedProjects(ctx, []string{f.Projects[0].UID})
			case "pin_change":
				key := ed25519.NewKeyFromSeed(make([]byte, 32))
				public := key.Public().(ed25519.PublicKey)
				err = s.(db.RootRepinStorage).RepinRootAuthority(ctx, f.Params.View.Projects[1].Pin.KeyID, db.RootKeyPin{ProjectUID: f.Projects[1].UID, AuthorityUID: s.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}, true)
			case "binding_disabled":
				var binding db.FederationBinding
				binding, err = s.FederationBindingByProject(ctx, f.Projects[1].ID)
				if err == nil {
					binding.Enabled = false
					_, err = s.UpsertFederationBinding(ctx, binding)
				}
			case "cross_account_rebind":
				var parent db.APIToken
				parent, _, err = s.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "other-parent-test-token", Actor: "other-member", AdminActor: "admin", TeamUIDs: []string{f.Teams[0].UID, f.Teams[1].UID}})
				params.ParentTokenID = parent.ID
				params.RebindParent = true
			case "revoked_grant":
				err = s.RevokeFederationEnrollment(ctx, grant.Enrollment.ID)
			case "unselected_anchor":
				params.AnchorProjectID = f.Denied.ID
			}
			require.NoError(t, err)
			before := scopedAuthoritySnapshot(t, s)
			_, err = native.CreateScopedRelayEnrollment(ctx, params)
			require.Error(t, err)
			require.Equal(t, before, scopedAuthoritySnapshot(t, s), "refusal rolls back prepared/grant/work writes")
		})
	}
	t.Run("new_view_inherits_route_private_bytes_plus_dirty_work", func(t *testing.T) {
		s := open(t)
		f := NewScopedAuthorityFixture(t, s)
		native := s.(db.ScopedRelayEnrollmentStore)
		_, err := native.CreateScopedRelayEnrollment(t.Context(), f.Params)
		require.NoError(t, err)
		records := scopedAuthoritySnapshot(t, s)
		var oldWork *db.ScopedWorkExport
		for _, r := range records {
			if w, ok := r.(*db.ScopedWorkExport); ok {
				oldWork = w
				w.State = "idle"
			}
		}
		require.NotNil(t, oldWork)
		principal := db.CustodyPrincipal{AuthorityUID: f.Params.View.AuthorityUID, BindingUID: f.Params.View.Route[0].BindingUID, Epoch: 1, AccountableActor: f.Parent.Actor, RouteDigest: f.Params.View.RouteDigest}
		request := db.ScopedControlRequest{RequestUID: scopedOwnerUID(600), Digest: strings.Repeat("d", 64), Kind: "status", Subject: db.ScopedEndpoint{ProjectUID: f.Projects[0].UID, IssueUID: f.Issue.UID}, Principal: db.ScopedControlPrincipal{HolderInstanceUID: s.InstanceUID(), Holder: "member", ClientKind: "cli"}}
		control := &db.ScopedControlCustodyExport{RequestUID: request.RequestUID, RequestDigest: request.Digest, BindingUID: principal.BindingUID, PrincipalBytes: scopedOwnerBytes(t, principal), RequestBytes: scopedOwnerBytes(t, request), State: "pending"}
		padding := db.MaxScopedRecoveryBytes - db.ScopedRetainedBytes(oldWork) - db.ScopedRetainedBytes(control)
		require.Positive(t, padding)
		control.RequestBytes = append(control.RequestBytes, bytes.Repeat([]byte{' '}, int(padding))...)
		require.EqualValues(t, db.MaxScopedRecoveryBytes, db.ScopedRetainedBytes(control)+db.ScopedRetainedBytes(oldWork))
		records = append(records, control)
		require.NoError(t, db.ValidateImportRecords(records), "old idle view is exactly within private budget")
		require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
		before := scopedAuthoritySnapshot(t, s)
		params := f.Params
		params.View.Key.ViewUID = scopedOwnerUID(601)
		params.CatalogEntryUID = scopedOwnerUID(602)
		_, err = native.CreateScopedRelayEnrollment(t.Context(), params)
		require.Error(t, err, "new dirty work costs one more byte than old idle work and must refuse atomically")
		require.Equal(t, before, scopedAuthoritySnapshot(t, s))
	})
	t.Run("same_account_rebind", func(t *testing.T) {
		s := open(t)
		f := NewScopedAuthorityFixture(t, s)
		native := s.(db.ScopedRelayEnrollmentStore)
		ctx := t.Context()
		first, err := native.CreateScopedRelayEnrollment(ctx, f.Params)
		require.NoError(t, err)
		parent, _, err := s.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "replacement-parent-test-token", Actor: "member", AdminActor: "admin"})
		require.NoError(t, err)
		p := f.Params
		p.ParentTokenID = parent.ID
		p.RebindParent = true
		got, err := native.CreateScopedRelayEnrollment(ctx, p)
		require.NoError(t, err)
		want := first
		want.Enrollment.ParentTokenID = &parent.ID
		want.Enrollment.UpdatedAt = got.Enrollment.UpdatedAt
		require.Equal(t, want, got)
	})
}

// NewScopedIncomingAuthorityFixture stages signed retained owner context only;
// native teams, local membership and actual spoke/config APIs establish authority.
// Empty signed baseline is an owner-restore fixture, not activation proof.
func NewScopedIncomingAuthorityFixture(t *testing.T, s db.Storage, prefixHops ...int) (db.ScopedViewDescriptor, [2]db.Project) {
	t.Helper()
	f := ScopedOwnerBackupFixture(t)
	v := f.Active
	v.Projects = f.View.Projects
	if len(prefixHops) > 0 && prefixHops[0] > 0 {
		prefix := []db.ScopedHopBinding{}
		for n := 0; n < prefixHops[0]; n++ {
			prefix = append(prefix, db.ScopedHopBinding{BindingUID: scopedOwnerUID(610 + n), InstanceUID: scopedOwnerUID(630 + n), Epoch: 1})
		}
		v.Route = append(prefix, v.Route...)
	}
	var records []db.ImportRecord
	for _, record := range f.Records {
		switch r := record.(type) {
		case *db.MetaKV, *db.ProjectExport, *db.RootKeyPin, *db.ScopedProtocolCutoverExport:
			records = append(records, r)
		case *db.ScopedViewExport:
			r.PreparedGeneration = nil
			if len(prefixHops) > 0 && prefixHops[0] > 0 {
				r.BindingUID = v.Route[0].BindingUID
				r.AccountableActor = "other-hop-member"
			}
			r.RequestDigest = v.RequestDigest
			records = append(records, r)
		case *db.ScopedProjectExport:
			r.OwnerViewUID = &v.Key.ViewUID
			r.OwnerGeneration = &v.Key.Generation
			r.ServingFenced = false
			records = append(records, r)
		case *db.ScopedGenerationExport:
			if r.Generation != v.Key.Generation {
				continue
			}
			r.DescriptorBytes = scopedOwnerBytes(t, v)
			r.DescriptorDigest = db.ScopedOfferedDigest(r.DescriptorBytes)
			var checkpoint db.ScopedCheckpoint
			require.NoError(t, json.Unmarshal(r.CheckpointBytes, &checkpoint))
			checkpoint.View = v
			checkpoint.Signatures = nil
			var err error
			checkpoint.Cut.Digest, err = db.ScopedCheckpointDigest(checkpoint)
			require.NoError(t, err)
			raw, err := db.ScopedCheckpointSigningBytes(checkpoint)
			require.NoError(t, err)
			for n, p := range v.Projects {
				key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(n + 1)}, 32))
				checkpoint.Signatures = append(checkpoint.Signatures, db.ScopedSignature{ProjectUID: p.ProjectUID, KeyID: p.Pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))})
			}
			r.CheckpointBytes = scopedOwnerBytes(t, checkpoint)
			r.CheckpointDigest = &checkpoint.Cut.Digest
			r.CutDigest = checkpoint.Cut.Digest
			r.AssembledBytes = int64(len(r.CheckpointBytes))
			records = append(records, r)
		}
	}
	records = append(records, &db.ScopedWorkExport{ViewUID: v.Key.ViewUID, DirtyRevision: 1, State: "idle"})
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	team, _, err := s.CreateTeam(t.Context(), "local-team", "admin")
	require.NoError(t, err)
	_, _, err = s.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "incoming-local-token", Actor: "local-member", AdminActor: "admin", TeamUIDs: []string{team.UID}})
	require.NoError(t, err)
	var projects [2]db.Project
	for n, p := range v.Projects {
		projects[n], err = s.ProjectByUID(t.Context(), p.ProjectUID)
		require.NoError(t, err)
		_, _, err = s.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: p.ProjectUID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
		require.NoError(t, err)
		_, err = s.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: projects[n].ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: projects[n].ID, HubProjectUID: p.ProjectUID, PushEnabled: true, Actor: "member", Enabled: true})
		require.NoError(t, err)
	}
	return v, projects
}

func InstallScopedIncomingAuthority(t *testing.T, s db.Storage, v db.ScopedViewDescriptor, projects [2]db.Project, downstream ...bool) {
	t.Helper()
	path := []string{v.AuthorityUID}
	for _, h := range v.Route {
		path = append(path, h.InstanceUID)
	}
	terminal := v.Route[len(v.Route)-1]
	config := db.RelayBindingConfig{ScopedView: &v.Key, RootCutoverUID: v.RootCutoverUID, ProtocolVersion: 2, BindingUID: terminal.BindingUID, UpstreamInstanceUID: path[len(path)-2], AuthorityUID: v.AuthorityUID, HubPath: path, LocalActor: "local-member", ResetEpoch: 1}
	if len(downstream) > 0 {
		config.ServeDownstream = downstream[0]
	}
	for _, project := range projects {
		_, err := s.SetRelayBindingConfig(t.Context(), project.ID, config)
		require.NoError(t, err)
	}
}

// RestoreScopedIncomingMixedAccounts retains actual installed bindings through
// the typed owner restore API, preserving distinct-hop tuple legality. Runtime
// authority must still reject a union of different upstream accounts on one hop.
func RestoreScopedIncomingMixedAccounts(t *testing.T, s db.Storage, projectID int64) {
	t.Helper()
	records := scopedAuthoritySnapshot(t, s)
	for _, r := range records {
		if b, ok := r.(*db.FederationBindingExport); ok && b.ProjectID == projectID {
			b.Actor = "different-upstream-member"
		}
	}
	require.NoError(t, db.ValidateImportRecords(records))
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
}

func RunScopedAuthorityLocalForwardingPrefix(t *testing.T, s db.Storage) {
	t.Helper()
	// Root + six preceding peers + this issuer is the final allowed hub prefix.
	v, projects := NewScopedIncomingAuthorityFixture(t, s, db.MaxRelayHubs-2)
	InstallScopedIncomingAuthority(t, s, v, projects, true)
	parent, err := s.ResolveAPIToken(t.Context(), "incoming-local-token")
	require.NoError(t, err)
	child := v
	child.Key = db.ScopedViewKey{ViewUID: scopedOwnerUID(650), Generation: 1}
	child.Route = append(append([]db.ScopedHopBinding{}, v.Route...), db.ScopedHopBinding{BindingUID: scopedOwnerUID(651), InstanceUID: scopedOwnerUID(652), Epoch: 1})
	child.DestinationUID = child.Route[len(child.Route)-1].InstanceUID
	require.Equal(t, db.MaxRelayHubs+1, len(child.Route)+1)
	require.NoError(t, child.Validate())
	params := db.CreateScopedRelayEnrollmentParams{View: child, CatalogEntryUID: scopedOwnerUID(653), AnchorProjectID: projects[0].ID, ParentTokenID: parent.ID, Token: "last-leaf-token", ServeDownstream: true}
	native := s.(db.ScopedRelayEnrollmentStore)
	_, err = native.CreateScopedRelayEnrollment(t.Context(), params)
	require.Error(t, err, "adjacent forwarding peer would exceed local hub prefix")
	params.ServeDownstream = false
	grant, err := native.CreateScopedRelayEnrollment(t.Context(), params)
	require.NoError(t, err, "the same adjacent peer may be the final leaf")
	require.Equal(t, parent.Actor, grant.Enrollment.Actor)
	require.NotEqual(t, "member", grant.Enrollment.Actor, "incoming upstream account is distinct")
	require.Equal(t, child.Route[len(child.Route)-1].BindingUID, grant.Enrollment.RelayBindingUID)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

// NewScopedOutgoingGrantFixture uses the real native factory before testing a
// shape-valid owner restore whose current grant flags need runtime validation.
func NewScopedOutgoingGrantFixture(t *testing.T, s db.Storage, kind string) db.ScopedViewDescriptor {
	t.Helper()
	var params db.CreateScopedRelayEnrollmentParams
	if kind == "last_leaf" {
		owner, projects := NewScopedIncomingAuthorityFixture(t, s, db.MaxRelayHubs-2)
		InstallScopedIncomingAuthority(t, s, owner, projects, true)
		parent, err := s.ResolveAPIToken(t.Context(), "incoming-local-token")
		require.NoError(t, err)
		child := owner
		child.Key = db.ScopedViewKey{ViewUID: scopedOwnerUID(660), Generation: 1}
		child.Route = append(append([]db.ScopedHopBinding{}, owner.Route...), db.ScopedHopBinding{BindingUID: scopedOwnerUID(661), InstanceUID: scopedOwnerUID(662), Epoch: 1})
		child.DestinationUID = child.Route[len(child.Route)-1].InstanceUID
		params = db.CreateScopedRelayEnrollmentParams{View: child, CatalogEntryUID: scopedOwnerUID(663), AnchorProjectID: projects[0].ID, ParentTokenID: parent.ID, Token: "restored-last-leaf-token", ServeDownstream: false}
		require.Equal(t, db.MaxRelayHubs+1, len(child.Route)+1)
	} else {
		f := NewScopedAuthorityFixture(t, s)
		params = f.Params
		routeLength := 2
		if kind == "early_forwarder" {
			routeLength = db.MaxRelayHubs
		}
		params.View.Route = []db.ScopedHopBinding{}
		for n := 0; n < routeLength; n++ {
			params.View.Route = append(params.View.Route, db.ScopedHopBinding{BindingUID: scopedOwnerUID(670 + n), InstanceUID: scopedOwnerUID(690 + n), Epoch: 1})
		}
		params.View.DestinationUID = params.View.Route[len(params.View.Route)-1].InstanceUID
		params.ServeDownstream = true
	}
	_, err := s.(db.ScopedRelayEnrollmentStore).CreateScopedRelayEnrollment(t.Context(), params)
	require.NoError(t, err)
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
	return params.View
}

func RestoreScopedOutgoingGrantFlags(t *testing.T, s db.Storage, view db.ScopedViewDescriptor, serveDownstream bool) {
	t.Helper()
	records := scopedAuthoritySnapshot(t, s)
	var found bool
	for _, record := range records {
		if grant, ok := record.(*db.FederationEnrollmentExport); ok && grant.RelayProtocolVersion == db.ScopedRelayProtocolVersion {
			require.False(t, found)
			found = true
			require.Equal(t, view.Route[len(view.Route)-1].Epoch, grant.RelayResetEpoch)
			grant.RelayServeDownstream = serveDownstream
		}
	}
	require.True(t, found, "real factory grant must be present in complete owner export")
	require.NoError(t, db.ValidateImportRecords(records), "shape-valid grant restore is distinct from current runtime path permission")
	require.NoError(t, s.ImportReplay(t.Context(), records, db.ImportOptions{}))
	require.NoError(t, db.ValidateImportRecords(scopedAuthoritySnapshot(t, s)))
}

func ScopedAuthorityWithoutCutover(t *testing.T, s db.Storage) ScopedAuthorityFixture {
	t.Helper()
	f := NewScopedAuthorityFixture(t, s)
	records := scopedAuthoritySnapshot(t, s)
	native := []db.ImportRecord{}
	for _, record := range records {
		if !db.IsScopedImportRecord(record) {
			native = append(native, record)
		}
	}
	require.NoError(t, db.ValidateImportRecords(native), "ordinary root owner backup legitimately has no scoped state")
	require.NoError(t, s.ImportReplay(t.Context(), native, db.ImportOptions{}))
	parent, err := s.ResolveAPIToken(t.Context(), "parent-test-token")
	require.NoError(t, err)
	anchor, err := s.ProjectByUID(t.Context(), f.Projects[0].UID)
	require.NoError(t, err)
	f.Params.ParentTokenID = parent.ID
	f.Params.AnchorProjectID = anchor.ID
	return f
}

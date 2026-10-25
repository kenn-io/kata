package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type scopedPreparationStore interface {
	PrepareMutation(context.Context, db.ScopedCommand) (db.MutationExpectation, error)
}

type scopedStablePreparationStore interface {
	PrepareScopedMutation(context.Context, db.ScopedMutationPreparation) (db.MutationExpectation, error)
}

// This is an actual in-process DTO entrypoint for the native source adapter,
// not a testing-only admission path. Missing trusted context must still refuse.
func RunScopedExpectationStablePreparation(t *testing.T, s db.Storage) {
	t.Helper()
	native, ok := s.(scopedStablePreparationStore)
	require.True(t, ok, "native stable-DTO preparation API must be implemented")
	f := NewScopedCaptureFixture(t, s)
	preparation := db.ScopedMutationPreparation{Kind: "patchProjectMetadata", Subject: db.ScopedMutationSubject{Kind: "project", ProjectUID: f.Projects[0].UID}, Peers: []db.ScopedEndpoint{}, Predicate: "metadata", MetadataGuard: &db.ScopedMetadataPredicate{Key: "guard", IfValue: db.JSONBlob(`"B"`)}}
	token, err := native.PrepareScopedMutation(t.Context(), preparation)
	require.Error(t, err, "a stable DTO cannot supply native authority")
	require.Empty(t, token.Token)
	for _, record := range scopedAuthoritySnapshot(t, s) {
		_, isToken := record.(*db.ScopedMutationExpectationExport)
		require.False(t, isToken)
	}
	write := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[0].UID), "member")
	_, err = s.PatchProjectMetadata(write, db.PatchProjectMetadataIn{ProjectID: f.Projects[0].ID, Actor: "member", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"B"`)}})
	require.NoError(t, err)
	access := db.ScopedSourceAccess{View: f.Params.View.Key, Principal: db.CustodyPrincipal{AuthorityUID: f.Params.View.AuthorityUID, BindingUID: f.Params.View.Route[0].BindingUID, Epoch: f.Params.View.Route[0].Epoch, AccountableActor: f.Parent.Actor, RouteDigest: f.Params.View.RouteDigest}}
	ctx := db.WithScopedSourceAccess(t.Context(), access)
	token, err = native.PrepareScopedMutation(ctx, preparation)
	require.NoError(t, err, "explicit stable project GuardB enters real native core")
	predicate := scopedPreparedPredicate(t, s, token)
	require.Equal(t, preparation, predicate.Preparation)
	t.Run("narrow_project", func(t *testing.T) {
		narrow := db.WithAuthorizedProjects(ctx, []string{f.Projects[0].UID})
		prepared, err := native.PrepareScopedMutation(narrow, preparation)
		require.NoError(t, err, "caller needs only the real subject project while the native parent still independently sees the whole selected view")
		require.NotEmpty(t, prepared.Token)
	})
	peerWrite := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[1].UID), "member")
	outsideProjectPeer, _, err := s.CreateIssue(peerWrite, db.CreateIssueParams{ProjectID: f.Projects[1].ID, Author: "member", Title: "Outside caller project ceiling"})
	require.NoError(t, err)
	before := scopedAuthoritySnapshot(t, s)
	for _, name := range []string{"malformed", "actor", "view", "binding", "route", "empty_selection", "narrow_subject", "narrow_peer", "native_fence", "guard"} {
		t.Run(name, func(t *testing.T) {
			carrier := access
			request := preparation
			requestContext := ctx
			switch name {
			case "malformed":
				carrier = db.ScopedSourceAccess{}
			case "actor":
				carrier.Principal.AccountableActor = "forged-member"
			case "view":
				carrier.View.ViewUID = scopedOwnerUID(1290)
			case "binding":
				carrier.Principal.BindingUID = scopedOwnerUID(1291)
			case "route":
				carrier.Principal.RouteDigest = strings.Repeat("b", 64)
			case "empty_selection":
				requestContext = db.WithAuthorizedProjects(ctx, []string{})
			case "narrow_subject":
				requestContext = db.WithAuthorizedProjects(ctx, []string{f.Projects[0].UID})
				request = db.ScopedMutationPreparation{Kind: "patchProjectMetadata", Subject: db.ScopedMutationSubject{Kind: "project", ProjectUID: f.Projects[1].UID}, Predicate: "metadata", MetadataGuard: &db.ScopedMetadataPredicate{Key: "guard", IfAbsent: true}}
			case "narrow_peer":
				requestContext = db.WithAuthorizedProjects(ctx, []string{f.Projects[0].UID})
				request = db.ScopedMutationPreparation{Kind: "editIssue", Subject: db.ScopedMutationSubject{Kind: "issue", ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, Predicate: "parent", Peers: []db.ScopedEndpoint{{ProjectUID: outsideProjectPeer.ProjectUID, IssueUID: outsideProjectPeer.UID}}}
			case "native_fence":
				requestContext = db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return db.ErrNotFound })
			case "guard":
				request.MetadataGuard = &db.ScopedMetadataPredicate{Key: "guard", IfValue: db.JSONBlob(`"C"`)}
			}
			requestContext = db.WithScopedSourceAccess(requestContext, carrier)
			refused, err := native.PrepareScopedMutation(requestContext, request)
			require.Error(t, err)
			require.Empty(t, refused.Token)
			require.Equal(t, before, scopedAuthoritySnapshot(t, s), "refused hints/guard cannot alter private predicate state")
		})
	}
	normal, ok := s.(scopedPreparationStore)
	require.True(t, ok)
	_, err = s.PatchIssueMetadata(write, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"B"`)}})
	require.NoError(t, err)
	command := db.ScopedCommand{Kind: "patchIssueMetadata", Subject: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, Actor: "source-label", Metadata: &db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "source-label", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"next"`)}, Guard: &db.MetadataPatchGuard{Key: "guard", IfValue: jsontext.Value(`"B"`)}}}
	issueToken, err := normal.PrepareMutation(ctx, command)
	require.NoError(t, err, "normal native command adapter shares actual guarded core")
	issuePredicate := scopedPreparedPredicate(t, s, issueToken)
	require.Equal(t, f.Issue.UID, issuePredicate.Preparation.Subject.IssueUID)
	stableToken, err := native.PrepareScopedMutation(ctx, issuePredicate.Preparation)
	require.NoError(t, err)
	stablePredicate := scopedPreparedPredicate(t, s, stableToken)
	require.Equal(t, issuePredicate, stablePredicate)
	require.NotEqual(t, issueToken.Token, stableToken.Token)
	expires := time.Now().UTC().Add(time.Hour)
	local, _, err := s.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "scoped-preparation-local-token", Actor: "member", AdminActor: "admin", Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.Issue.ProjectUID, RootIssueUID: f.Issue.UID}, ExpiresAt: &expires})
	require.NoError(t, err)
	localContext := db.WithIssueScopeTargets(ctx)
	localContext = db.WithTransactionFence(localContext, s.IssueScopedTokenTransactionFence(local))
	_, err = native.PrepareScopedMutation(localContext, issuePredicate.Preparation)
	require.NoError(t, err, "local issue-scoped credential admits its actual subject")
	outside, _, err := s.CreateIssue(write, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Author: "member", Title: "Outside local token subtree"})
	require.NoError(t, err)
	outsidePreparation := db.ScopedMutationPreparation{Kind: "patchIssueMetadata", Subject: db.ScopedMutationSubject{Kind: "issue", ProjectUID: outside.ProjectUID, IssueUID: outside.UID}, Predicate: "metadata", MetadataGuard: &db.ScopedMetadataPredicate{Key: "guard", IfAbsent: true}}
	outsideContext := db.WithIssueScopeTargets(ctx)
	outsideContext = db.WithTransactionFence(outsideContext, s.IssueScopedTokenTransactionFence(local))
	beforeScopeRefusal := scopedAuthoritySnapshot(t, s)
	_, err = native.PrepareScopedMutation(outsideContext, outsidePreparation)
	require.Error(t, err, "a valid hop hint cannot broaden an independent native issue token")
	require.Equal(t, beforeScopeRefusal, scopedAuthoritySnapshot(t, s))
	_, _, err = s.RevokeAPIToken(t.Context(), local.ID, "admin")
	require.NoError(t, err)
	beforeLocalRevocation := scopedAuthoritySnapshot(t, s)
	_, err = native.PrepareScopedMutation(localContext, issuePredicate.Preparation)
	require.Error(t, err, "local token revocation is independent of the live hop parent")
	require.Equal(t, beforeLocalRevocation, scopedAuthoritySnapshot(t, s))
	_, _, err = s.RevokeAPIToken(t.Context(), f.Parent.ID, "admin")
	require.NoError(t, err)
	afterRevocation := scopedAuthoritySnapshot(t, s)
	refused, err := native.PrepareScopedMutation(ctx, preparation)
	require.Error(t, err, "actual native parent revocation overrides a previously valid copied hint")
	require.Empty(t, refused.Token)
	require.Equal(t, afterRevocation, scopedAuthoritySnapshot(t, s))
}

func scopedPreparedPredicate(t *testing.T, s db.Storage, token db.MutationExpectation) db.ScopedMutationPredicate {
	t.Helper()
	require.NotEmpty(t, token.Token)
	require.False(t, strings.HasPrefix(token.Token, "rev-"))
	var found *db.ScopedMutationExpectationExport
	for _, record := range scopedAuthoritySnapshot(t, s) {
		if r, ok := record.(*db.ScopedMutationExpectationExport); ok && r.TokenDigest == db.ScopedOfferedDigest([]byte(token.Token)) {
			found = r
		}
	}
	require.NotNil(t, found)
	var predicate db.ScopedMutationPredicate
	require.NoError(t, db.DecodeScopedRecord(found.PredicateBytes, &predicate))
	return predicate
}

func scopedAssertPredicateCurrent(t *testing.T, s db.Storage, prior db.ScopedMutationPredicate, stale bool) {
	t.Helper()
	kind, err := db.ScopedPredicateStorageKind(prior.Preparation)
	require.NoError(t, err)
	for _, record := range scopedAuthoritySnapshot(t, s) {
		if r, ok := record.(*db.ScopedPredicateIncarnationExport); ok && r.ProjectUID == prior.Preparation.Subject.ProjectUID && r.PredicateKind == kind {
			subject := prior.Preparation.Subject
			id := subject.IssueUID
			if subject.Kind == "project" {
				id = subject.ProjectUID
			} else if subject.DefinitionUID != "" {
				id = subject.DefinitionUID
			}
			if r.SubjectUID != id {
				continue
			}
			if stale {
				require.Greater(t, r.Incarnation, prior.Incarnation, "ABA cannot make an old opaque predicate current")
			} else {
				require.Equal(t, prior.Incarnation, r.Incarnation, "no-op must not invent a predicate transition")
				require.Equal(t, prior.CanonicalDigest, r.CanonicalDigest)
			}
			return
		}
	}
	t.Fatal("current predicate row missing")
}

// Callers install only copied trusted-native context. Each guarded family
// gets its own native fixture so one RED cannot hide another reserved hook.
func RunScopedOpaqueGuardABA(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, family := range []string{"issue_metadata", "project_metadata", "parent", "direct_parent"} {
		t.Run(family, func(t *testing.T) {
			s := open(t)
			f := NewScopedCaptureFixture(t, s)
			ctx := scopedPreparationContext(t, f)
			native, ok := s.(scopedPreparationStore)
			require.True(t, ok, "native opaque preparation must be implemented")
			stable, ok := s.(scopedStablePreparationStore)
			require.True(t, ok, "native stable DTO preparation must be implemented")
			write := db.WithRootAttribution(t.Context(), ScopedCaptureSigner(t, f, f.Projects[0].UID), "member")
			var prepare func() (db.MutationExpectation, error)
			var noop, change func()
			switch family {
			case "issue_metadata":
				patch := func(value string, changed bool) {
					out, err := s.PatchIssueMetadata(write, db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"guard": jsontext.Value(value)}})
					require.NoError(t, err)
					require.Equal(t, changed, out.Changed)
					issue, err := s.IssueByUID(write, f.Issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					var current map[string]jsontext.Value
					require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &current))
					require.Equal(t, value, string(current["guard"]))
				}
				patch(`"B"`, true)
				command := db.ScopedCommand{Kind: "patchIssueMetadata", Subject: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, Metadata: &db.PatchIssueMetadataIn{IssueID: f.Issue.ID, Actor: "source-label", Patch: map[string]jsontext.Value{"guard": jsontext.Value(`"next"`)}, Guard: &db.MetadataPatchGuard{Key: "guard", IfValue: jsontext.Value(`"B"`)}}}
				prepare = func() (db.MutationExpectation, error) { return native.PrepareMutation(ctx, command) }
				noop = func() { patch(`"B"`, false) }
				change = func() { patch(`"C"`, true); patch(`"B"`, true) }
			case "project_metadata":
				patch := func(value string, changed bool) {
					out, err := s.PatchProjectMetadata(write, db.PatchProjectMetadataIn{ProjectID: f.Projects[0].ID, Actor: "member", Patch: map[string]jsontext.Value{"guard": jsontext.Value(value)}})
					require.NoError(t, err)
					require.Equal(t, changed, out.Changed)
					project, err := s.ProjectByUID(write, f.Projects[0].UID)
					require.NoError(t, err)
					var current map[string]jsontext.Value
					require.NoError(t, json.Unmarshal([]byte(project.Metadata), &current))
					require.Equal(t, value, string(current["guard"]))
				}
				patch(`"B"`, true)
				preparation := db.ScopedMutationPreparation{Kind: "patchProjectMetadata", Subject: db.ScopedMutationSubject{Kind: "project", ProjectUID: f.Projects[0].UID}, Predicate: "metadata", MetadataGuard: &db.ScopedMetadataPredicate{Key: "guard", IfValue: db.JSONBlob(`"B"`)}}
				prepare = func() (db.MutationExpectation, error) { return stable.PrepareScopedMutation(ctx, preparation) }
				noop = func() { patch(`"B"`, false) }
				change = func() { patch(`"C"`, true); patch(`"B"`, true) }
			case "parent":
				b, _, err := s.CreateIssue(write, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Title: "Parent B", Author: "member"})
				require.NoError(t, err)
				c, _, err := s.CreateIssue(write, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Title: "Parent C", Author: "member"})
				require.NoError(t, err)
				set := func(parent db.Issue, changed bool) {
					out, err := s.EditIssueAtomic(write, db.EditIssueAtomicParams{IssueID: f.Issue.ID, Actor: "member", SetParent: &parent.ID})
					require.NoError(t, err)
					require.Equal(t, changed, out.AnyChange)
					if changed {
						require.NotNil(t, out.Changes.ParentSet)
						require.Equal(t, parent.UID, out.Changes.ParentSet.UID)
						require.Len(t, out.Events, 1)
						require.Equal(t, "issue.links_changed", out.Events[0].Type)
						require.NotNil(t, out.Events[0].IssueUID)
						require.Equal(t, f.Issue.UID, *out.Events[0].IssueUID)
					} else {
						require.Nil(t, out.Changes.ParentSet)
						require.Nil(t, out.Changes.ParentRemoved)
						require.Empty(t, out.Events)
					}
					current, err := s.ParentOf(write, f.Issue.ID)
					require.NoError(t, err)
					require.Equal(t, "parent", current.Type)
					require.Equal(t, f.Issue.UID, current.FromIssueUID)
					require.Equal(t, parent.UID, current.ToIssueUID)
					peer, err := s.IssueByUID(write, current.ToIssueUID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, parent.UID, peer.UID)
				}
				set(b, true)
				command := db.ScopedCommand{Kind: "editIssue", Subject: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, Edit: &db.EditIssueAtomicParams{IssueID: f.Issue.ID, Actor: "source-label", SetParent: &c.ID}}
				prepare = func() (db.MutationExpectation, error) { return native.PrepareMutation(ctx, command) }
				noop = func() { set(b, false) }
				change = func() { set(c, true); set(b, true) }
			case "direct_parent":
				// Bare native row writers are intentionally eventless. Their ABA
				// must still invalidate an existing root parent predicate.
				b, _, err := s.CreateIssue(write, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Title: "Direct parent B", Author: "member"})
				require.NoError(t, err)
				c, _, err := s.CreateIssue(write, db.CreateIssueParams{ProjectID: f.Projects[0].ID, Title: "Direct parent C", Author: "member"})
				require.NoError(t, err)
				history, err := s.EventsAfter(write, db.EventsAfterParams{Limit: 1000})
				require.NoError(t, err)
				require.Less(t, len(history), 1000, "fixture history must fit the complete native read")
				quietHistory := func() {
					current, err := s.EventsAfter(write, db.EventsAfterParams{Limit: 1000})
					require.NoError(t, err)
					require.Equal(t, history, current, "bare link writers must remain eventless")
				}
				read := func(parent db.Issue) db.Link {
					current, err := s.ParentOf(write, f.Issue.ID)
					require.NoError(t, err)
					require.Equal(t, "parent", current.Type)
					require.Equal(t, f.Issue.UID, current.FromIssueUID)
					require.Equal(t, parent.UID, current.ToIssueUID)
					peer, err := s.IssueByUID(write, current.ToIssueUID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, parent.UID, peer.UID)
					stored, err := s.LinkByEndpoints(write, f.Issue.ID, parent.ID, "parent")
					require.NoError(t, err)
					require.Equal(t, current, stored)
					return current
				}
				add := func(parent db.Issue) db.Link {
					link, err := s.CreateLink(write, db.CreateLinkParams{FromIssueID: f.Issue.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
					require.NoError(t, err)
					require.Equal(t, link, read(parent))
					quietHistory()
					return link
				}
				remove := func(link db.Link) {
					require.NoError(t, s.DeleteLinkByID(write, link.ID))
					_, err := s.ParentOf(write, f.Issue.ID)
					require.ErrorIs(t, err, db.ErrNotFound)
					_, err = s.LinkByID(write, link.ID)
					require.ErrorIs(t, err, db.ErrNotFound)
					require.ErrorIs(t, s.DeleteLinkByID(write, link.ID), db.ErrNotFound)
					_, err = s.ParentOf(write, f.Issue.ID)
					require.ErrorIs(t, err, db.ErrNotFound)
					quietHistory()
				}
				initial := add(b)
				command := db.ScopedCommand{Kind: "editIssue", Subject: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, Edit: &db.EditIssueAtomicParams{IssueID: f.Issue.ID, Actor: "source-label", SetParent: &c.ID}}
				prepare = func() (db.MutationExpectation, error) { return native.PrepareMutation(ctx, command) }
				noop = func() {
					_, err := s.CreateLink(write, db.CreateLinkParams{FromIssueID: f.Issue.ID, ToIssueID: b.ID, Type: "parent", Author: "member"})
					require.ErrorIs(t, err, db.ErrLinkExists)
					require.Equal(t, initial, read(b))
					quietHistory()
				}
				change = func() {
					remove(initial)
					middle := add(c)
					remove(middle)
					add(b)
					quietHistory()
				}
			}
			token, err := prepare()
			require.NoError(t, err)
			prior := scopedPreparedPredicate(t, s, token)
			if noop != nil {
				noop()
				scopedAssertPredicateCurrent(t, s, prior, false)
			}
			change()
			scopedAssertPredicateCurrent(t, s, prior, true)
			fresh, err := prepare()
			require.NoError(t, err)
			require.NotEqual(t, token.Token, fresh.Token)
		})
	}
}

func scopedPreparationContext(t *testing.T, f ScopedCaptureFixture) context.Context {
	t.Helper()
	view := f.Params.View
	return db.WithScopedSourceAccess(t.Context(), db.ScopedSourceAccess{View: view.Key, Principal: db.CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: view.Route[0].Epoch, AccountableActor: f.Parent.Actor, RouteDigest: view.RouteDigest}})
}

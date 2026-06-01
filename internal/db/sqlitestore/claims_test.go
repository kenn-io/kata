package sqlitestore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestCanonicalFederationCapabilitiesIncludesClaim(t *testing.T) {
	got, err := db.CanonicalFederationCapabilities("pull,push,claim")

	require.NoError(t, err)
	assert.Equal(t, "claim,pull,push", got)
}

func TestAuthorizeFederationTokenRequiresClaimCapability(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	token := "claim-capability-token"
	spokeUID := newTestUID(t)

	_, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: spokeUID,
		ProjectID:        &p.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)

	_, err = d.AuthorizeFederationToken(ctx, token, p.ID, "claim")
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrNotFound)

	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token + "-allowed",
		SpokeInstanceUID: spokeUID,
		ProjectID:        &p.ID,
		Capabilities:     "claim,pull",
	})
	require.NoError(t, err)

	got, err := d.AuthorizeFederationToken(ctx, token+"-allowed", p.ID, "claim")
	require.NoError(t, err)
	assert.Equal(t, created.Enrollment.ID, got.ID)
	assert.Equal(t, "claim,pull", got.Capabilities)
}

func TestAcquireClaimFirstHolderGrantsAndEmitsEvent(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	got, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: claimPrincipal(t, "alice"),
		ClaimKind: "hard",
		Purpose:   "edit",
		Now:       now,
	})

	require.NoError(t, err)
	assert.True(t, got.Granted)
	assert.Equal(t, "alice", got.Holder.Holder)
	assert.NotNil(t, got.Claim)
	assert.Equal(t, issue.UID, got.Claim.IssueUID)
	assert.Equal(t, "hard", got.Claim.ClaimKind)
	require.NotNil(t, got.Event)
	assert.Equal(t, "claim.acquired", got.Event.Type)
	assert.NotEmpty(t, got.Event.ContentHash)
	assertEventCount(t, d, "claim.acquired", 1)
}

func TestAcquireClaimDifferentLiveHolderDeniesWithCurrentHolder(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	bob := claimPrincipal(t, "bob")
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)

	got, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: bob, ClaimKind: "hard", Now: now,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimDenied)
	assert.False(t, got.Granted)
	assert.Equal(t, alice.Holder, got.Holder.Holder)
	assert.Equal(t, alice.HolderInstanceUID, got.Holder.HolderInstanceUID)
	assertEventCount(t, d, "claim.acquired", 1)
}

func TestTimedClaimRequiresBoundedTTL(t *testing.T) {
	for name, ttl := range map[string]time.Duration{
		"too-short": time.Minute - time.Nanosecond,
		"too-long":  24*time.Hour + time.Nanosecond,
	} {
		t.Run(name, func(t *testing.T) {
			d, ctx, p, issue := setupTestIssue(t)

			_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: p.ID,
				IssueRef:  issue.ShortID,
				Principal: claimPrincipal(t, "alice"),
				ClaimKind: "timed",
				TTL:       ttl,
				Now:       time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
			})

			require.Error(t, err)
			assert.ErrorIs(t, err, db.ErrClaimValidation)
		})
	}

	for name, ttl := range map[string]time.Duration{
		"min": time.Minute,
		"max": 24 * time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			d, ctx, p, issue := setupTestIssue(t)

			got, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: p.ID,
				IssueRef:  issue.ShortID,
				Principal: claimPrincipal(t, "alice"),
				ClaimKind: "timed",
				TTL:       ttl,
				Now:       time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
			})

			require.NoError(t, err)
			assert.True(t, got.Granted)
			require.NotNil(t, got.Claim)
			assert.Equal(t, "timed", got.Claim.ClaimKind)
		})
	}
}

func TestAcquireClaimSameHardHolderRetryIsIdempotent(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	first, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)

	retry, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now.Add(time.Second),
	})

	require.NoError(t, err)
	assert.True(t, retry.Granted)
	assert.Equal(t, first.Claim.ID, retry.Claim.ID)
	assert.Equal(t, alice.Holder, retry.Holder.Holder)
	assert.Nil(t, retry.Event)
	assertEventCount(t, d, "claim.acquired", 1)
}

func TestRenewClaimTimedExtendsExpiryAndIncrementsRevision(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now,
	})
	require.NoError(t, err)
	require.NotNil(t, acquired.Claim.ExpiresAt)
	originalRevision := acquired.Claim.Revision

	renewed, err := d.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		TTL:       2 * time.Minute,
		Now:       now.Add(30 * time.Second),
	})

	require.NoError(t, err)
	require.NotNil(t, renewed.Claim)
	require.NotNil(t, renewed.Claim.ExpiresAt)
	assert.Equal(t, originalRevision+1, renewed.Claim.Revision)
	assert.True(t, renewed.Claim.ExpiresAt.Equal(now.Add(150*time.Second)), renewed.Claim.ExpiresAt)
	assert.Equal(t, acquired.Claim.ID, renewed.Claim.ID)
}

func TestRenewClaimDifferentTupleDoesNotExtendTimedClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now,
	})
	require.NoError(t, err)
	require.NotNil(t, acquired.Claim.ExpiresAt)

	got, err := d.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: claimPrincipal(t, "bob"),
		TTL:       10 * time.Minute,
		Now:       now.Add(30 * time.Second),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimNotHeld)
	assert.False(t, got.Granted)
	require.NotNil(t, got.Claim)
	require.NotNil(t, got.Claim.ExpiresAt)
	assert.True(t, got.Claim.ExpiresAt.Equal(*acquired.Claim.ExpiresAt), got.Claim.ExpiresAt)
}

func TestRenewClaimHardClaimReturnsValidationError(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "hard",
		Now:       now,
	})
	require.NoError(t, err)

	got, err := d.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		TTL:       time.Minute,
		Now:       now.Add(30 * time.Second),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	require.NotNil(t, got.Claim)
	assert.Equal(t, acquired.Claim.ID, got.Claim.ID)
	assert.Equal(t, acquired.Claim.Revision, got.Claim.Revision)
	assert.Nil(t, got.Claim.ExpiresAt)
}

func TestReleaseClaimSameTupleSucceedsAndEmitsEvent(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)

	got, err := d.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, Reason: "done", Now: now.Add(time.Minute),
	})

	require.NoError(t, err)
	assert.True(t, got.Granted)
	assert.Equal(t, alice.Holder, got.Holder.Holder)
	require.NotNil(t, got.Claim)
	assert.NotNil(t, got.Claim.ReleasedAt)
	require.NotNil(t, got.Event)
	assert.Equal(t, "claim.released", got.Event.Type)
	assertEventCount(t, d, "claim.released", 1)
}

func TestReleaseClaimDifferentHolderDenies(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)

	got, err := d.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: claimPrincipal(t, "bob"), Reason: "done", Now: now,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimNotHeld)
	assert.False(t, got.Granted)
	assert.Equal(t, alice.Holder, got.Holder.Holder)
	assertEventCount(t, d, "claim.released", 0)
}

func TestClaimCloseByHolderReleasesClaimAndEmitsEvent(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: localClaimPrincipal(d, "alice"),
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, changed, err := d.CloseIssue(ctx, issue.ID, "done", "alice", "", nil)

	require.NoError(t, err)
	assert.True(t, changed)
	assertLiveClaimCount(t, d, issue.UID, 0)
	assertEventCount(t, d, "issue.closed", 1)
	assertEventCount(t, d, "claim.released", 1)
	assertEventCount(t, d, "claim.violated", 0)
}

func TestClaimCloseByNonHolderEmitsViolationAndReleasesClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: localClaimPrincipal(d, "alice"),
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, changed, err := d.CloseIssue(ctx, issue.ID, "done", "bob", "", nil)

	require.NoError(t, err)
	assert.True(t, changed)
	assertLiveClaimCount(t, d, issue.UID, 0)
	assertEventCount(t, d, "claim.violated", 1)
	assertEventCount(t, d, "claim.released", 1)
}

func TestIngestClaimViolationPayloadIncludesCanonicalOffenderFields(t *testing.T) {
	d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
	claim, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "holder"},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	offending := remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.updated", "remote-agent")

	_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, offending))
	require.NoError(t, err)

	payload := latestClaimViolationPayload(t, d)
	assert.Equal(t, issue.UID, payloadString(t, payload, "issue_uid"))
	assert.Equal(t, offending.EventUID, payloadString(t, payload, "offending_event_uid"))
	assert.Equal(t, "issue.updated", payloadString(t, payload, "offending_event_type"))
	assert.Equal(t, spokeUID, payloadString(t, payload, "offending_origin_instance_uid"))
	assert.Equal(t, "remote-agent", payloadString(t, payload, "actor"))
	assert.Equal(t, "uncovered_work", payloadString(t, payload, "reason"))
	require.NotNil(t, claim.Claim)
	assert.Equal(t, claim.Claim.ClaimUID, payloadString(t, payload, "claim_uid"))
	assert.Equal(t, "holder", payloadString(t, payload, "holder"))
	assert.Equal(t, spokeUID, payloadString(t, payload, "holder_instance_uid"))
}

func TestIngestWithoutLiveClaimDoesNotEmitViolation(t *testing.T) {
	d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
	offending := remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.updated", "remote-agent")

	_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, offending))
	require.NoError(t, err)

	assertEventCount(t, d, "claim.violated", 0)
}

func TestClaimViolationQueriesUseLegacyPayloadOriginFallback(t *testing.T) {
	d, ctx, p, _, issue, _ := setupIngestClaimIssue(t)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: newTestUID(t), Holder: "holder"},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	legacyOrigin := newTestUID(t)
	insertLegacyClaimViolationEvent(ctx, t, d, p, issue,
		`{"issue_uid":"`+issue.UID+`","event_uid":"legacy-event","event_type":"issue.updated","origin_instance_uid":"`+legacyOrigin+`","actor":"remote-agent","reason":"uncovered_work"}`)

	violations, count, err := d.UnresolvedClaimViolationsForIssue(ctx, p.ID, issue.UID, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	require.Len(t, violations, 1)
	assert.Equal(t, "legacy-event", violations[0].OffendingEventUID)
	assert.Equal(t, "issue.updated", violations[0].OffendingEventType)
	assert.Equal(t, legacyOrigin, violations[0].OffendingOriginInstanceUID)
}

func TestClaimViolationQueriesDoNotFallbackToAuditEventOrigin(t *testing.T) {
	d, ctx, p, _, issue, _ := setupIngestClaimIssue(t)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: newTestUID(t), Holder: "holder"},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	insertLegacyClaimViolationEvent(ctx, t, d, p, issue,
		`{"issue_uid":"`+issue.UID+`","event_uid":"legacy-event","event_type":"issue.updated","actor":"remote-agent","reason":"uncovered_work"}`)

	violations, count, err := d.UnresolvedClaimViolationsForIssue(ctx, p.ID, issue.UID, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	require.Len(t, violations, 1)
	assert.Empty(t, violations[0].OffendingOriginInstanceUID)
}

func TestClaimViolationQueriesIgnoreNeverClaimedIssue(t *testing.T) {
	d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
	offending := remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.updated", "remote-agent")
	_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, offending))
	require.NoError(t, err)

	issueViolations, issueCount, err := d.UnresolvedClaimViolationsForIssue(ctx, p.ID, issue.UID, 3)
	require.NoError(t, err)
	assert.Zero(t, issueCount)
	assert.Empty(t, issueViolations)

	projectViolations, projectCount, err := d.UnresolvedClaimViolationsForProject(ctx, p.ID, 5)
	require.NoError(t, err)
	assert.Zero(t, projectCount)
	assert.Empty(t, projectViolations)
}

func TestClaimViolationQueriesTreatReleaseEventsAsResolutionBoundary(t *testing.T) {
	tests := []struct {
		name    string
		release func(t *testing.T, d *sqlitestore.Store, ctx context.Context, p db.Project, issue db.Issue, principal db.ClaimPrincipal)
	}{
		{
			name: "claim.released",
			release: func(t *testing.T, d *sqlitestore.Store, ctx context.Context, p db.Project, issue db.Issue, principal db.ClaimPrincipal) {
				t.Helper()
				_, err := d.ReleaseClaim(ctx, db.ReleaseClaimParams{
					ProjectID: p.ID,
					IssueRef:  issue.ShortID,
					Principal: principal,
					Reason:    "done",
					Now:       time.Now().UTC(),
				})
				require.NoError(t, err)
			},
		},
		{
			name: "claim.force_released",
			release: func(t *testing.T, d *sqlitestore.Store, ctx context.Context, p db.Project, issue db.Issue, _ db.ClaimPrincipal) {
				t.Helper()
				_, err := d.ForceReleaseClaim(ctx, db.ForceReleaseClaimParams{
					ProjectID: p.ID,
					IssueRef:  issue.ShortID,
					Actor:     "admin",
					Reason:    "operator",
					Now:       time.Now().UTC(),
				})
				require.NoError(t, err)
			},
		},
		{
			name: "claim.expired",
			release: func(t *testing.T, d *sqlitestore.Store, ctx context.Context, p db.Project, issue db.Issue, _ db.ClaimPrincipal) {
				t.Helper()
				_, err := d.ForceReleaseClaim(ctx, db.ForceReleaseClaimParams{
					ProjectID: p.ID,
					IssueRef:  issue.ShortID,
					Actor:     "admin",
					Reason:    "sweep",
					Now:       time.Now().UTC().Add(25 * time.Hour),
				})
				require.ErrorIs(t, err, db.ErrClaimExpired)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
			principal := db.ClaimPrincipal{
				HolderInstanceUID: spokeUID,
				Holder:            "holder",
				ClientKind:        "cli",
			}
			claimKind := "hard"
			ttl := time.Duration(0)
			if tc.name == "claim.expired" {
				claimKind = "timed"
				ttl = time.Hour
			}
			_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: p.ID,
				IssueRef:  issue.ShortID,
				Principal: principal,
				ClaimKind: claimKind,
				TTL:       ttl,
				Now:       time.Now().UTC(),
			})
			require.NoError(t, err)
			offending := remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.updated", "remote-agent")
			_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, offending))
			require.NoError(t, err)

			tc.release(t, d, ctx, p, issue, principal)

			issueViolations, issueCount, err := d.UnresolvedClaimViolationsForIssue(ctx, p.ID, issue.UID, 3)
			require.NoError(t, err)
			assert.Zero(t, issueCount)
			assert.Empty(t, issueViolations)

			projectViolations, projectCount, err := d.UnresolvedClaimViolationsForProject(ctx, p.ID, 5)
			require.NoError(t, err)
			assert.Zero(t, projectCount)
			assert.Empty(t, projectViolations)
		})
	}
}

func TestClaimCloseIdempotentRetryDoesNotDuplicateReleaseEvent(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: localClaimPrincipal(d, "alice"),
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, changed, err := d.CloseIssue(ctx, issue.ID, "done", "alice", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	_, _, changed, err = d.CloseIssue(ctx, issue.ID, "done", "alice", "", nil)

	require.NoError(t, err)
	assert.False(t, changed)
	assertEventCount(t, d, "claim.released", 1)
}

func TestClaimCloseNonFederatedDoesNotEmitViolation(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: localClaimPrincipal(d, "alice"),
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	_, _, changed, err := d.CloseIssue(ctx, issue.ID, "done", "bob", "", nil)

	require.NoError(t, err)
	assert.True(t, changed)
	assertEventCount(t, d, "claim.violated", 0)
}

func TestRenewClaimExpiredTimedClaimPersistsExpiry(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now,
	})
	require.NoError(t, err)

	_, err = d.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		TTL:       time.Minute,
		Now:       now.Add(2 * time.Minute),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimExpired)
	assertClaimReleasedAndNotLive(t, d, acquired.Claim.ID, issue.UID)
	assertEventCount(t, d, "claim.expired", 1)

	_, err = d.RenewClaim(ctx, db.RenewClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		TTL:       time.Minute,
		Now:       now.Add(3 * time.Minute),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimNotHeld)
}

func TestReleaseClaimExpiredTimedClaimPersistsExpiry(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now,
	})
	require.NoError(t, err)

	_, err = d.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		Reason:    "done",
		Now:       now.Add(2 * time.Minute),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimExpired)
	assertClaimReleasedAndNotLive(t, d, acquired.Claim.ID, issue.UID)
	assertEventCount(t, d, "claim.released", 0)
	assertEventCount(t, d, "claim.expired", 1)

	_, err = d.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		Reason:    "done",
		Now:       now.Add(3 * time.Minute),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimNotHeld)
}

func TestTimedClaimExpiredBeforeNewAcquireReleasesAndEmitsExpired(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now,
	})
	require.NoError(t, err)

	got, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: claimPrincipal(t, "bob"),
		ClaimKind: "hard",
		Now:       now.Add(2 * time.Minute),
	})

	require.NoError(t, err)
	assert.True(t, got.Granted)
	require.NotNil(t, got.Claim)
	assert.Equal(t, "bob", got.Claim.Holder)
	assertClaimReleased(t, d, acquired.Claim.ID)
	assertReleaseReason(t, d, acquired.Claim.ID, "expired")
	assertEventCount(t, d, "claim.expired", 1)
}

func TestExpireClaimEmitsExpiredOnceAcrossSweeperAndOpportunisticExpiry(t *testing.T) {
	t.Run("sweeper then opportunistic", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: claimPrincipal(t, "alice"),
			ClaimKind: "timed",
			TTL:       time.Minute,
			Now:       now,
		})
		require.NoError(t, err)

		events, err := d.ExpireTimedClaims(ctx, now.Add(2*time.Minute), 100)
		require.NoError(t, err)
		require.Len(t, events, 1)
		assert.Equal(t, "claim.expired", events[0].Type)

		_, err = d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: claimPrincipal(t, "bob"),
			ClaimKind: "hard",
			Now:       now.Add(3 * time.Minute),
		})
		require.NoError(t, err)
		assertClaimReleased(t, d, acquired.Claim.ID)
		assertEventCount(t, d, "claim.expired", 1)
	})

	t.Run("opportunistic then sweeper", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: claimPrincipal(t, "alice"),
			ClaimKind: "timed",
			TTL:       time.Minute,
			Now:       now,
		})
		require.NoError(t, err)

		_, err = d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: claimPrincipal(t, "bob"),
			ClaimKind: "hard",
			Now:       now.Add(2 * time.Minute),
		})
		require.NoError(t, err)

		events, err := d.ExpireTimedClaims(ctx, now.Add(3*time.Minute), 100)
		require.NoError(t, err)
		assert.Empty(t, events)
		assertClaimReleased(t, d, acquired.Claim.ID)
		assertEventCount(t, d, "claim.expired", 1)
	})
}

func TestForceReleaseClaimExpiredTimedClaimPersistsExpiry(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	acquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: claimPrincipal(t, "alice"),
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now,
	})
	require.NoError(t, err)

	_, err = d.ForceReleaseClaim(ctx, db.ForceReleaseClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Actor:     "admin",
		Reason:    "override",
		Now:       now.Add(2 * time.Minute),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimExpired)
	assertClaimReleasedAndNotLive(t, d, acquired.Claim.ID, issue.UID)
}

func TestForceReleaseClaimReleasesAnyHolderAndEmitsEvent(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)

	got, err := d.ForceReleaseClaim(ctx, db.ForceReleaseClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Actor: "admin", Reason: "override", Now: now.Add(time.Minute),
	})

	require.NoError(t, err)
	assert.True(t, got.Granted)
	assert.Equal(t, alice.Holder, got.Holder.Holder)
	require.NotNil(t, got.Claim)
	assert.NotNil(t, got.Claim.ReleasedAt)
	require.NotNil(t, got.Event)
	assert.Equal(t, "claim.force_released", got.Event.Type)
	assert.Equal(t, "admin", got.Event.Actor)
	assertEventCount(t, d, "claim.force_released", 1)
}

func TestClaimStatusReturnsLiveHolderAndHubNow(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "hard",
		Now:       now,
	})
	require.NoError(t, err)
	hubNow := now.Add(time.Minute)

	status, err := d.ClaimStatus(ctx, p.ID, issue.ShortID, hubNow)

	require.NoError(t, err)
	assert.True(t, status.Held)
	assert.Equal(t, alice, status.Holder)
	assert.True(t, status.HubNow.Equal(hubNow), status.HubNow)
	require.NotNil(t, status.Claim)
	assert.Equal(t, issue.UID, status.Claim.IssueUID)
}

func TestClaimsCanBeReacquiredForSoftDeletedIssue(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "hard",
		Now:       now,
	})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, issue.ID, "alice")
	require.NoError(t, err)

	released, err := d.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		Reason:    "restore handoff",
		Now:       now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.True(t, released.Granted)

	reacquired, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "hard",
		Now:       now.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	assert.True(t, reacquired.Granted)
	require.NotNil(t, reacquired.Claim)
	assert.Equal(t, issue.UID, reacquired.Claim.IssueUID)

	status, err := d.ClaimStatus(ctx, p.ID, issue.ShortID, now.Add(3*time.Minute))
	require.NoError(t, err)
	assert.True(t, status.Held)
	assert.Equal(t, issue.UID, status.Claim.IssueUID)
}

func TestClaimStatusExpiresTimedClaimForSoftDeletedIssue(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: claimPrincipal(t, "alice"),
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now.Add(-2 * time.Minute),
	})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, issue.ID, "alice")
	require.NoError(t, err)

	status, err := d.ClaimStatus(ctx, p.ID, issue.ShortID, now)

	require.NoError(t, err)
	assert.False(t, status.Held)
	require.Len(t, status.Events, 1)
	assert.Equal(t, "claim.expired", status.Events[0].Type)
}

func TestPendingClaimRequestInsertIsDurable(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	p := createProject(ctx, t, d, "p")
	issue := makeIssue(t, ctx, d, p.ID, "x", "tester")
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	pending, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: claimPrincipal(t, "alice"),
		ClaimKind: "timed",
		TTL:       5 * time.Minute,
		Purpose:   "edit",
		Now:       now,
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	reopened, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	var got db.PendingClaimRequest
	require.NoError(t, reopened.QueryRow(`
		SELECT request_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid, client_kind, claim_kind,
		       ttl_seconds, purpose, requested_at
		  FROM pending_claim_requests
		 WHERE request_uid = ?`, pending.RequestUID).
		Scan(&got.RequestUID, &got.ProjectID, &got.IssueID, &got.IssueUID,
			&got.Holder, &got.HolderInstanceUID, &got.ClientKind, &got.ClaimKind,
			&got.TTLSeconds, &got.Purpose, &got.RequestedAt))
	assert.Equal(t, pending.RequestUID, got.RequestUID)
	assert.Equal(t, p.ID, got.ProjectID)
	assert.Equal(t, issue.ID, got.IssueID)
	assert.Equal(t, issue.UID, got.IssueUID)
	assert.Equal(t, "alice", got.Holder)
	assert.Equal(t, pending.HolderInstanceUID, got.HolderInstanceUID)
	assert.Equal(t, "cli", got.ClientKind)
	assert.Equal(t, "timed", got.ClaimKind)
	require.NotNil(t, got.TTLSeconds)
	assert.Equal(t, int64(300), *got.TTLSeconds)
	assert.Equal(t, "edit", got.Purpose)
}

func TestPendingClaimRequestDuplicateReturnsExisting(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")

	first, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Purpose: "edit", Now: now,
	})
	require.NoError(t, err)
	second, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Purpose: "edit", Now: now.Add(time.Minute),
	})
	require.NoError(t, err)

	assert.Equal(t, first.RequestUID, second.RequestUID)
	assert.Equal(t, first.RequestedAt, second.RequestedAt)
	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pending_claim_requests WHERE issue_uid = ? AND rejected_at IS NULL AND resolved_at IS NULL`,
		issue.UID,
	).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestPendingClaimRequestActiveUniquenessUsesFullPrincipal(t *testing.T) {
	t.Run("different client kind", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		cli := claimPrincipal(t, "alice")
		tui := cli
		tui.ClientKind = "tui"

		first, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
			ProjectID: p.ID, IssueRef: issue.ShortID, Principal: cli, ClaimKind: "hard", Now: now,
		})
		require.NoError(t, err)
		second, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
			ProjectID: p.ID, IssueRef: issue.ShortID, Principal: tui, ClaimKind: "hard", Now: now,
		})
		require.NoError(t, err)

		assert.NotEqual(t, first.RequestUID, second.RequestUID)
	})

	t.Run("different holder instance", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		local := claimPrincipal(t, "alice")
		remote := local
		remote.HolderInstanceUID = newTestUID(t)

		first, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
			ProjectID: p.ID, IssueRef: issue.ShortID, Principal: local, ClaimKind: "hard", Now: now,
		})
		require.NoError(t, err)
		second, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
			ProjectID: p.ID, IssueRef: issue.ShortID, Principal: remote, ClaimKind: "hard", Now: now,
		})
		require.NoError(t, err)

		assert.NotEqual(t, first.RequestUID, second.RequestUID)
	})

	t.Run("same principal different claim kind returns existing", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		alice := claimPrincipal(t, "alice")

		hard, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
			ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
		})
		require.NoError(t, err)
		timed, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
			ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "timed", TTL: 5 * time.Minute, Now: now,
		})
		require.NoError(t, err)

		assert.Equal(t, hard.RequestUID, timed.RequestUID)
		assert.Equal(t, "hard", timed.ClaimKind)
	})
}

func TestPendingClaimRequestLegacyEmptyHolderInstanceStillDedupesByHolder(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	legacyRequestUID := newTestUID(t)
	_, err := d.ExecContext(ctx, `
		INSERT INTO pending_claim_requests(
		  request_uid, project_id, issue_id, issue_uid, holder, holder_instance_uid,
		  client_kind, claim_kind, purpose, requested_at
		)
		VALUES(?, ?, ?, ?, ?, '', ?, 'hard', '', ?)`,
		legacyRequestUID, p.ID, issue.ID, issue.UID, alice.Holder, alice.ClientKind,
		now.UTC().Format("2006-01-02T15:04:05.000Z"))
	require.NoError(t, err)

	got, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now.Add(time.Minute),
	})
	require.NoError(t, err)

	assert.Equal(t, legacyRequestUID, got.RequestUID)
	assert.Empty(t, got.HolderInstanceUID)
}

func TestClaimGateAllowsUnclaimedIssue(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	err := d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: claimPrincipal(t, "alice"), Now: now,
	})

	require.NoError(t, err)
}

func TestPendingClaimDoesNotBlockUnclaimedClaimGate(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	_, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)

	err = d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, Now: now,
	})

	require.NoError(t, err)
}

func TestPendingClaimResolveStoresCachedClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	pending, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Purpose: "edit", Now: now,
	})
	require.NoError(t, err)
	claim := cachedClaim(t, issue, alice, "hard", now, nil)

	require.NoError(t, d.ResolvePendingClaim(ctx, pending.RequestUID, claim))

	assertLiveClaimHolder(t, d, issue.UID, alice.Holder)
	require.NoError(t, d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, Now: now,
	}))
	assertPendingResolved(t, d, pending.RequestUID)
}

func TestPendingClaimResolveRejectsDifferentHolderClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	pending, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Purpose: "edit", Now: now,
	})
	require.NoError(t, err)
	bobClaim := cachedClaim(t, issue, claimPrincipal(t, "bob"), "hard", now, nil)

	err = d.ResolvePendingClaim(ctx, pending.RequestUID, bobClaim)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	assertPendingUnresolved(t, d, pending.RequestUID)
	assertLiveClaimCount(t, d, issue.UID, 0)
}

func TestPendingClaimResolveRejectsDifferentClaimKind(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	pending, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: alice,
		ClaimKind: "timed",
		TTL:       5 * time.Minute,
		Purpose:   "edit",
		Now:       now,
	})
	require.NoError(t, err)
	hardClaim := cachedClaim(t, issue, alice, "hard", now, nil)

	err = d.ResolvePendingClaim(ctx, pending.RequestUID, hardClaim)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	assertPendingUnresolved(t, d, pending.RequestUID)
	assertLiveClaimCount(t, d, issue.UID, 0)
}

func TestPendingClaimResolveRejectsDifferentClientKind(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	pending, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Purpose: "edit", Now: now,
	})
	require.NoError(t, err)
	claim := cachedClaim(t, issue, alice, "hard", now, nil)
	claim.ClientKind = "tui"

	err = d.ResolvePendingClaim(ctx, pending.RequestUID, claim)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	assertPendingUnresolved(t, d, pending.RequestUID)
	assertLiveClaimCount(t, d, issue.UID, 0)
}

func TestPendingClaimResolveRejectsDifferentHolderInstanceUID(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	pending, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, ClaimKind: "hard", Purpose: "edit", Now: now,
	})
	require.NoError(t, err)
	claim := cachedClaim(t, issue, alice, "hard", now, nil)
	claim.HolderInstanceUID = newTestUID(t)

	err = d.ResolvePendingClaim(ctx, pending.RequestUID, claim)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	assertPendingUnresolved(t, d, pending.RequestUID)
	assertLiveClaimCount(t, d, issue.UID, 0)
}

func TestCachedClaimHardSatisfiesClaimGateOffline(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	require.NoError(t, d.UpsertClaimCache(ctx, cachedClaim(t, issue, alice, "hard", now, nil)))

	err := d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, Now: now.Add(time.Hour),
	})

	require.NoError(t, err)
}

func TestClaimGateIgnoresClientKindForLiveHolderMatch(t *testing.T) {
	t.Run("same holder instance with non-empty claim client kind satisfies gate", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		acquired := db.ClaimPrincipal{
			HolderInstanceUID: newTestUID(t),
			Holder:            "alice",
			ClientKind:        "cli",
		}
		_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: acquired,
			ClaimKind: "hard",
			Now:       now,
		})
		require.NoError(t, err)

		err = d.CheckClaimGate(ctx, db.ClaimGateParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{
				HolderInstanceUID: acquired.HolderInstanceUID,
				Holder:            acquired.Holder,
			},
			Now: now,
		})

		require.NoError(t, err)
	})

	t.Run("same holder instance expired timed claim with non-empty client kind is treated as absent", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		acquired := db.ClaimPrincipal{
			HolderInstanceUID: newTestUID(t),
			Holder:            "alice",
			ClientKind:        "cli",
		}
		_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: acquired,
			ClaimKind: "timed",
			TTL:       time.Minute,
			Now:       now,
		})
		require.NoError(t, err)

		err = d.CheckClaimGate(ctx, db.ClaimGateParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{
				HolderInstanceUID: acquired.HolderInstanceUID,
				Holder:            acquired.Holder,
			},
			Now: now.Add(time.Minute),
		})

		require.NoError(t, err)
	})

	t.Run("different holder instance still returns denied", func(t *testing.T) {
		d, ctx, p, issue := setupTestIssue(t)
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
		_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{
				HolderInstanceUID: newTestUID(t),
				Holder:            "alice",
				ClientKind:        "cli",
			},
			ClaimKind: "hard",
			Now:       now,
		})
		require.NoError(t, err)

		err = d.CheckClaimGate(ctx, db.ClaimGateParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{
				HolderInstanceUID: newTestUID(t),
				Holder:            "bob",
			},
			Now: now,
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, db.ErrClaimDenied)
	})
}

func TestCachedClaimTimedGateAllowsAfterExpiry(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Minute)
	alice := claimPrincipal(t, "alice")
	require.NoError(t, d.UpsertClaimCache(ctx, cachedClaim(t, issue, alice, "timed", now, &expires)))

	require.NoError(t, d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, Now: now.Add(30 * time.Second),
	}))

	err := d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, Now: expires,
	})
	require.NoError(t, err)
}

func TestCachedClaimTimedGateDeniesDifferentTupleBeforeExpiry(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Minute)
	alice := claimPrincipal(t, "alice")
	bob := claimPrincipal(t, "bob")
	require.NoError(t, d.UpsertClaimCache(ctx, cachedClaim(t, issue, alice, "timed", now, &expires)))

	err := d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: bob, Now: now.Add(30 * time.Second),
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimDenied)

	err = d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: bob, Now: expires,
	})

	require.NoError(t, err)
}

func TestCachedClaimDeniedForDifferentHolder(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	require.NoError(t, d.UpsertClaimCache(ctx, cachedClaim(t, issue, claimPrincipal(t, "alice"), "hard", now, nil)))

	err := d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: claimPrincipal(t, "bob"), Now: now,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimDenied)
}

func TestApplyClaimStatusStoresLiveCachedClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	claim := cachedClaim(t, issue, alice, "hard", now, nil)
	claim.ProjectID = 0
	claim.IssueID = 0

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: now,
	}))

	assertLiveClaimHolder(t, d, issue.UID, "alice")
	require.NoError(t, d.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: p.ID, IssueRef: issue.ShortID, Principal: alice, Now: now,
	}))
}

func TestApplyClaimStatusSameClaimUIDUpdatesCachedTimedClaimInPlace(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	expires := now.Add(time.Minute)
	claim := cachedClaim(t, issue, alice, "timed", now, &expires)
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: now,
	}))
	originalID := liveClaimID(t, d, issue.UID)

	refreshedExpires := now.Add(10 * time.Minute)
	refreshed := claim
	refreshed.ExpiresAt = &refreshedExpires
	refreshed.Revision = 7
	refreshed.UpdatedAt = now.Add(30 * time.Second)
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &refreshed, HubNow: now.Add(30 * time.Second),
	}))

	assert.Equal(t, originalID, liveClaimID(t, d, issue.UID))
	assertLiveClaimCount(t, d, issue.UID, 1)
	assertLiveClaimRevisionAndExpiry(t, d, issue.UID, 7, refreshedExpires)
	assertLiveReleaseReasonNil(t, d, issue.UID)
	assertEventCount(t, d, "claim.released", 0)
}

func TestApplyClaimStatusReplacesCachedHolderWithSingleLiveClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: ptrClaim(cachedClaim(t, issue, alice, "hard", now, nil)), HubNow: now,
	}))
	aliceClaimID := liveClaimID(t, d, issue.UID)
	bob := claimPrincipal(t, "bob")

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: bob, Claim: ptrClaim(cachedClaim(t, issue, bob, "hard", now.Add(time.Minute), nil)), HubNow: now.Add(time.Minute),
	}))

	assertLiveClaimCount(t, d, issue.UID, 1)
	assertLiveClaimHolder(t, d, issue.UID, "bob")
	assertReleaseReason(t, d, aliceClaimID, "status_refresh_replaced")
}

func TestApplyClaimStatusNoLiveClaimClearsCachedClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: ptrClaim(cachedClaim(t, issue, alice, "hard", now, nil)), HubNow: now,
	}))
	claimID := liveClaimID(t, d, issue.UID)

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: false, HubNow: now.Add(time.Minute),
	}))

	assertLiveClaimCount(t, d, issue.UID, 0)
	assertReleaseReason(t, d, claimID, "status_refresh")
}

func TestApplyClaimStatusStaleLiveStatusDoesNotResurrectAfterNewerNoLiveStatus(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	t1 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)
	alice := claimPrincipal(t, "alice")
	claim := cachedClaim(t, issue, alice, "hard", t1, nil)
	claim.UpdatedAt = t2
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: t2,
	}))

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: false, HubNow: t3,
	}))
	assertLiveClaimCount(t, d, issue.UID, 0)

	stale := claim
	stale.ClaimUID = newTestUID(t)
	stale.UpdatedAt = t1
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &stale, HubNow: t1,
	}))

	assertLiveClaimCount(t, d, issue.UID, 0)
}

func TestApplyClaimStatusStaleSameUIDLiveStatusDoesNotResurrectAfterNewerNoLiveStatus(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	t1 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)
	alice := claimPrincipal(t, "alice")
	claim := cachedClaim(t, issue, alice, "hard", t1, nil)
	claim.UpdatedAt = t2
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: t2,
	}))

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: false, HubNow: t3,
	}))
	assertLiveClaimCount(t, d, issue.UID, 0)

	stale := claim
	stale.UpdatedAt = t1
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &stale, HubNow: t1,
	}))

	assertLiveClaimCount(t, d, issue.UID, 0)
}

func TestApplyClaimStatusStaleNoLiveStatusDoesNotReleaseNewerCachedClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	t1 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	alice := claimPrincipal(t, "alice")
	claim := cachedClaim(t, issue, alice, "hard", t1, nil)
	claim.UpdatedAt = t2
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: t2,
	}))
	claimID := liveClaimID(t, d, issue.UID)

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: false, HubNow: t1,
	}))

	assertLiveClaimCount(t, d, issue.UID, 1)
	assert.Equal(t, claimID, liveClaimID(t, d, issue.UID))
	assertLiveClaimHolder(t, d, issue.UID, "alice")
	assertLiveClaimUpdatedAt(t, d, issue.UID, t2)
	assertLiveReleaseReasonNil(t, d, issue.UID)
}

func TestApplyClaimStatusStaleLiveStatusDoesNotReplaceNewerCachedClaim(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	t1 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	bob := claimPrincipal(t, "bob")
	bobClaim := cachedClaim(t, issue, bob, "hard", t1, nil)
	bobClaim.UpdatedAt = t2
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: bob, Claim: &bobClaim, HubNow: t2,
	}))
	bobClaimID := liveClaimID(t, d, issue.UID)
	alice := claimPrincipal(t, "alice")
	aliceClaim := cachedClaim(t, issue, alice, "hard", t1, nil)

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &aliceClaim, HubNow: t1,
	}))

	assertLiveClaimCount(t, d, issue.UID, 1)
	assert.Equal(t, bobClaimID, liveClaimID(t, d, issue.UID))
	assertLiveClaimHolder(t, d, issue.UID, "bob")
	assertLiveClaimUpdatedAt(t, d, issue.UID, t2)
}

func TestApplyClaimStatusStaleWrongIssueUIDStillReturnsValidation(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issueA := makeIssue(t, ctx, d, p.ID, "a", "tester")
	issueB := makeIssue(t, ctx, d, p.ID, "b", "tester")
	t1 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	alice := claimPrincipal(t, "alice")
	claim := cachedClaim(t, issueA, alice, "hard", t1, nil)
	claim.UpdatedAt = t2
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issueA.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: t2,
	}))
	claimID := liveClaimID(t, d, issueA.UID)
	staleWrongIssue := cachedClaim(t, issueB, alice, "hard", t1, nil)

	err := d.ApplyClaimStatus(ctx, p.ID, issueA.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &staleWrongIssue, HubNow: t1,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	assertLiveClaimCount(t, d, issueA.UID, 1)
	assert.Equal(t, claimID, liveClaimID(t, d, issueA.UID))
	assertLiveClaimUpdatedAt(t, d, issueA.UID, t2)
}

func TestApplyClaimStatusRejectsClaimForDifferentIssueUID(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issueA := makeIssue(t, ctx, d, p.ID, "a", "tester")
	issueB := makeIssue(t, ctx, d, p.ID, "b", "tester")
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	claim := cachedClaim(t, issueB, alice, "hard", now, nil)

	err := d.ApplyClaimStatus(ctx, p.ID, issueA.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: now,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrClaimValidation)
	assertLiveClaimCount(t, d, issueA.UID, 0)
}

func TestApplyClaimStatusSameClaimUIDDoesNotMoveMutableFieldsBackward(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	t1 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	alice := claimPrincipal(t, "alice")
	expires2 := t2.Add(10 * time.Minute)
	claim := cachedClaim(t, issue, alice, "timed", t1, &expires2)
	claim.Revision = 7
	claim.UpdatedAt = t2
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: t2,
	}))
	claimID := liveClaimID(t, d, issue.UID)

	expires1 := t1.Add(5 * time.Minute)
	stale := claim
	stale.ExpiresAt = &expires1
	stale.Revision = 3
	stale.UpdatedAt = t1
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &stale, HubNow: t2.Add(time.Minute),
	}))

	assert.Equal(t, claimID, liveClaimID(t, d, issue.UID))
	assertLiveClaimRevisionAndExpiry(t, d, issue.UID, 7, expires2)
	assertLiveClaimUpdatedAt(t, d, issue.UID, t2)
}

func TestApplyClaimStatusSameClaimUIDEqualTimestampLowerRevisionDoesNotOverwrite(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	expiresLater := now.Add(10 * time.Minute)
	claim := cachedClaim(t, issue, alice, "timed", now, &expiresLater)
	claim.Revision = 3
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: now,
	}))
	claimID := liveClaimID(t, d, issue.UID)

	expiresEarlier := now.Add(5 * time.Minute)
	stale := claim
	stale.ExpiresAt = &expiresEarlier
	stale.Revision = 2
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &stale, HubNow: now,
	}))

	assert.Equal(t, claimID, liveClaimID(t, d, issue.UID))
	assertLiveClaimRevisionAndExpiry(t, d, issue.UID, 3, expiresLater)
	assertLiveClaimUpdatedAt(t, d, issue.UID, now)
}

func TestApplyClaimStatusSameClaimUIDEqualTimestampHigherRevisionUpdates(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	alice := claimPrincipal(t, "alice")
	expiresEarlier := now.Add(5 * time.Minute)
	claim := cachedClaim(t, issue, alice, "timed", now, &expiresEarlier)
	claim.Revision = 3
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &claim, HubNow: now,
	}))
	claimID := liveClaimID(t, d, issue.UID)

	expiresLater := now.Add(10 * time.Minute)
	fresh := claim
	fresh.ExpiresAt = &expiresLater
	fresh.Revision = 4
	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: true, Holder: alice, Claim: &fresh, HubNow: now,
	}))

	assert.Equal(t, claimID, liveClaimID(t, d, issue.UID))
	assertLiveClaimRevisionAndExpiry(t, d, issue.UID, 4, expiresLater)
	assertLiveClaimUpdatedAt(t, d, issue.UID, now)
}

func TestApplyClaimStatusNoLiveClaimNoopSucceeds(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	require.NoError(t, d.ApplyClaimStatus(ctx, p.ID, issue.UID, db.ClaimStatus{
		Held: false, HubNow: now,
	}))

	assertLiveClaimCount(t, d, issue.UID, 0)
}

func TestAcquireClaimConcurrentAttemptsGrantExactlyOne(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	const attempts = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	principals := make([]db.ClaimPrincipal, attempts)
	for i := 0; i < attempts; i++ {
		principals[i] = claimPrincipal(t, "holder-"+strconv.Itoa(i))
	}

	for i := 0; i < attempts; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: p.ID,
				IssueRef:  issue.ShortID,
				Principal: principals[i],
				ClaimKind: "hard",
				Now:       now,
			})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var granted, denied int
	for err := range results {
		switch {
		case err == nil:
			granted++
		case errors.Is(err, db.ErrClaimDenied):
			denied++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, granted)
	assert.Equal(t, attempts-1, denied)
	assertEventCount(t, d, "claim.acquired", 1)
}

func assertClaimReleasedAndNotLive(t *testing.T, d *sqlitestore.Store, claimID int64, issueUID string) {
	t.Helper()
	assertClaimReleased(t, d, claimID)

	var liveCount int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&liveCount))
	assert.Equal(t, 0, liveCount)
}

func assertClaimReleased(t *testing.T, d *sqlitestore.Store, claimID int64) {
	t.Helper()
	var released int
	require.NoError(t, d.QueryRow(
		`SELECT released_at IS NOT NULL FROM issue_claims WHERE id = ?`, claimID,
	).Scan(&released))
	assert.Equal(t, 1, released)
}

func assertReleaseReason(t *testing.T, d *sqlitestore.Store, claimID int64, want string) {
	t.Helper()
	var got string
	require.NoError(t, d.QueryRow(
		`SELECT release_reason FROM issue_claims WHERE id = ?`, claimID,
	).Scan(&got))
	assert.Equal(t, want, got)
}

func assertPendingResolved(t *testing.T, d *sqlitestore.Store, requestUID string) {
	t.Helper()
	var resolved int
	require.NoError(t, d.QueryRow(
		`SELECT resolved_at IS NOT NULL FROM pending_claim_requests WHERE request_uid = ?`, requestUID,
	).Scan(&resolved))
	assert.Equal(t, 1, resolved)
}

func assertPendingUnresolved(t *testing.T, d *sqlitestore.Store, requestUID string) {
	t.Helper()
	var resolved, rejected int
	require.NoError(t, d.QueryRow(
		`SELECT resolved_at IS NOT NULL, rejected_at IS NOT NULL
		   FROM pending_claim_requests WHERE request_uid = ?`, requestUID,
	).Scan(&resolved, &rejected))
	assert.Equal(t, 0, resolved)
	assert.Equal(t, 0, rejected)
}

func assertLiveClaimCount(t *testing.T, d *sqlitestore.Store, issueUID string, want int) {
	t.Helper()
	var got int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&got))
	assert.Equal(t, want, got)
}

func assertLiveClaimHolder(t *testing.T, d *sqlitestore.Store, issueUID, want string) {
	t.Helper()
	var got string
	require.NoError(t, d.QueryRow(
		`SELECT holder FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&got))
	assert.Equal(t, want, got)
}

func assertLiveClaimRevisionAndExpiry(t *testing.T, d *sqlitestore.Store, issueUID string, wantRevision int64, wantExpiry time.Time) {
	t.Helper()
	var (
		revision int64
		expires  time.Time
	)
	require.NoError(t, d.QueryRow(
		`SELECT revision, expires_at FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&revision, &expires))
	assert.Equal(t, wantRevision, revision)
	assert.True(t, expires.Equal(wantExpiry), expires)
}

func assertLiveClaimUpdatedAt(t *testing.T, d *sqlitestore.Store, issueUID string, want time.Time) {
	t.Helper()
	var got time.Time
	require.NoError(t, d.QueryRow(
		`SELECT updated_at FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&got))
	assert.True(t, got.Equal(want), got)
}

func assertLiveReleaseReasonNil(t *testing.T, d *sqlitestore.Store, issueUID string) {
	t.Helper()
	var reason sql.NullString
	require.NoError(t, d.QueryRow(
		`SELECT release_reason FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&reason))
	assert.False(t, reason.Valid)
}

func liveClaimID(t *testing.T, d *sqlitestore.Store, issueUID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, d.QueryRow(
		`SELECT id FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&id))
	return id
}

func cachedClaim(
	t *testing.T,
	issue db.Issue,
	principal db.ClaimPrincipal,
	kind string,
	acquiredAt time.Time,
	expiresAt *time.Time,
) db.IssueClaim {
	t.Helper()
	claim := db.IssueClaim{
		ClaimUID:          newTestUID(t),
		ProjectID:         issue.ProjectID,
		IssueID:           issue.ID,
		IssueUID:          issue.UID,
		Holder:            principal.Holder,
		HolderInstanceUID: principal.HolderInstanceUID,
		ClientKind:        principal.ClientKind,
		Purpose:           "edit",
		ClaimKind:         kind,
		AcquiredAt:        acquiredAt,
		ExpiresAt:         expiresAt,
		Revision:          1,
		UpdatedAt:         acquiredAt,
	}
	return claim
}

func ptrClaim(claim db.IssueClaim) *db.IssueClaim {
	return &claim
}

func latestClaimViolationPayload(t *testing.T, d *sqlitestore.Store) map[string]any {
	t.Helper()
	var raw string
	require.NoError(t, d.QueryRow(`
		SELECT payload
		  FROM events
		 WHERE type = 'claim.violated'
		 ORDER BY id DESC
		 LIMIT 1`).Scan(&raw))
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	return payload
}

func insertLegacyClaimViolationEvent(
	ctx context.Context,
	t *testing.T,
	d *sqlitestore.Store,
	p db.Project,
	issue db.Issue,
	payload string,
) {
	t.Helper()
	eventUID := newTestUID(t)
	_, err := d.ExecContext(ctx, `
		INSERT INTO events (
			uid, origin_instance_uid, project_id, project_name, issue_id, issue_uid,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		)
		VALUES (?, (SELECT value FROM meta WHERE key='instance_uid'), ?, ?, ?, ?,
			'claim.violated', 'tester', ?, 1, 0,
			'0000000000000000000000000000000000000000000000000000000000000000',
			'2026-05-23T12:00:00.000Z')`,
		eventUID, p.ID, p.Name, issue.ID, issue.UID, payload)
	require.NoError(t, err)
}

func payloadString(t *testing.T, payload map[string]any, key string) string {
	t.Helper()
	raw, ok := payload[key]
	require.True(t, ok, "missing payload key %s", key)
	got, ok := raw.(string)
	require.True(t, ok, "payload key %s is %T, not string", key, raw)
	return got
}

func claimPrincipal(t *testing.T, holder string) db.ClaimPrincipal {
	t.Helper()
	return db.ClaimPrincipal{
		HolderInstanceUID: newTestUID(t),
		Holder:            holder,
		ClientKind:        "cli",
	}
}

func localClaimPrincipal(d *sqlitestore.Store, holder string) db.ClaimPrincipal {
	return db.ClaimPrincipal{
		HolderInstanceUID: d.InstanceUID(),
		Holder:            holder,
		ClientKind:        "",
	}
}

func TestCountLiveClaims(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	got, err := d.CountLiveClaims(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "no claims yet")

	// CountLiveClaims evaluates expires_at against SQLite's strftime('%Y-%m-%dT%H:%M:%fZ','now'),
	// i.e. wall-clock now, so the timed claim must be created with a TTL that lands in the
	// real future or the test races a moving wall clock.
	now := time.Now().UTC()
	hardIssue := makeIssue(t, ctx, d, p.ID, "hard", "tester")
	timedIssue := makeIssue(t, ctx, d, p.ID, "timed", "tester")
	releasedIssue := makeIssue(t, ctx, d, p.ID, "released", "tester")

	alice := claimPrincipal(t, "alice")
	bob := claimPrincipal(t, "bob")
	carol := claimPrincipal(t, "carol")

	_, err = d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: hardIssue.ShortID, Principal: alice, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)
	_, err = d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: timedIssue.ShortID, Principal: bob,
		ClaimKind: "timed", TTL: time.Hour, Now: now,
	})
	require.NoError(t, err)
	_, err = d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID, IssueRef: releasedIssue.ShortID, Principal: carol, ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)
	_, err = d.ReleaseClaim(ctx, db.ReleaseClaimParams{
		ProjectID: p.ID, IssueRef: releasedIssue.ShortID, Principal: carol, Reason: "done", Now: now.Add(time.Second),
	})
	require.NoError(t, err)

	got, err = d.CountLiveClaims(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got, "hard + timed (not yet expired); released excluded")
}

func TestCountPendingClaims(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	got, err := d.CountPendingClaims(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "no pending requests yet")

	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	openIssue := makeIssue(t, ctx, d, p.ID, "open", "tester")
	resolvedIssue := makeIssue(t, ctx, d, p.ID, "resolved", "tester")
	rejectedIssue := makeIssue(t, ctx, d, p.ID, "rejected", "tester")

	_, err = d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: openIssue.ShortID, Principal: claimPrincipal(t, "alice"),
		ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)
	resolved, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: resolvedIssue.ShortID, Principal: claimPrincipal(t, "bob"),
		ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)
	rejected, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID, IssueRef: rejectedIssue.ShortID, Principal: claimPrincipal(t, "carol"),
		ClaimKind: "hard", Now: now,
	})
	require.NoError(t, err)

	require.NoError(t, d.ResolvePendingClaim(ctx, resolved.RequestUID, db.IssueClaim{
		ClaimUID:          newTestUID(t),
		ProjectID:         resolved.ProjectID,
		IssueID:           resolved.IssueID,
		IssueUID:          resolved.IssueUID,
		Holder:            resolved.Holder,
		HolderInstanceUID: resolved.HolderInstanceUID,
		ClientKind:        resolved.ClientKind,
		ClaimKind:         "hard",
		AcquiredAt:        now,
		UpdatedAt:         now,
	}))
	require.NoError(t, d.RejectPendingClaim(ctx, rejected.RequestUID, "denied by hub", now))

	got, err = d.CountPendingClaims(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got, "only the open request remains; resolved + rejected excluded")
}

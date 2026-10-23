package mcpevents_test

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/mcpevents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func requireWire(t *testing.T, err error, code int, reason string) *mcpevents.Error {
	t.Helper()
	var wire *mcpevents.Error
	require.ErrorAs(t, err, &wire)
	assert.Equal(t, code, wire.Code)
	assert.Equal(t, reason, wire.Reason)
	return wire
}

func TestSubscribeTTL(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		now := h.clock.Now()
		for i, tc := range []struct {
			name string
			ttl  mcpevents.TTLMS
			want *time.Duration
		}{
			{"absent defaults to 24h", mcpevents.TTLMS{}, new(24 * time.Hour)},
			{"one second clamps up to 60s", mcpevents.TTLValue(1000), new(60 * time.Second)},
			{"zero clamps up to 60s", mcpevents.TTLValue(0), new(60 * time.Second)},
			{"negative clamps up to 60s", mcpevents.TTLValue(-5), new(60 * time.Second)},
			{"ninety days clamps to the 30d maximum", mcpevents.TTLValue((90 * 24 * time.Hour).Milliseconds()), new(30 * 24 * time.Hour)},
			{"in range is honored", mcpevents.TTLValue((2 * time.Hour).Milliseconds()), new(2 * time.Hour)},
			{"null grants no expiry", mcpevents.TTLNull(), nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				req := h.request(fmt.Sprintf("example-user/%d", i))
				req.TTLMS = tc.ttl
				result := h.mustSubscribe(req)
				if tc.want == nil {
					assert.Nil(t, result.RefreshBefore)
					assert.Nil(t, h.row(result.ID).ExpiresAt)
					return
				}
				require.NotNil(t, result.RefreshBefore)
				assert.Equal(t, now.Add(*tc.want), *result.RefreshBefore)
			})
		}
	})
}

func TestTTLFieldDecodesAbsentNullAndValue(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		present    bool
		null       bool
		value      int64
	}{
		{"absent", `{}`, false, false, 0},
		{"null", `{"ttlMs":null}`, true, true, 0},
		{"value", `{"ttlMs":1500}`, true, false, 1500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req mcpevents.SubscribeRequest
			require.NoError(t, json.Unmarshal([]byte(tc.body), &req))
			present, null, ms := mcpevents.TTLParts(req.TTLMS)
			assert.Equal(t, tc.present, req.TTLMS.Present())
			assert.Equal(t, tc.present, present)
			assert.Equal(t, tc.null, req.TTLMS.IsNull())
			assert.Equal(t, tc.null, null)
			assert.Equal(t, tc.value, ms)
		})
	}
}

func TestSubscribeValidation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		for _, tc := range []struct {
			name   string
			mutate func(*mcpevents.SubscribeRequest)
			code   int
			reason string
		}{
			{"unknown event", func(r *mcpevents.SubscribeRequest) { r.Name = "kata.other" }, mcpevents.CodeNotFound, "event"},
			{"unsupported mode", func(r *mcpevents.SubscribeRequest) { r.Delivery.Mode = "poll" }, mcpevents.CodeUnsupported, "deliveryMode"},
			{"bad secret", func(r *mcpevents.SubscribeRequest) { r.Delivery.Secret = "whsec_short" }, mcpevents.CodeInvalidParams, "invalid_secret"},
			{"missing secret", func(r *mcpevents.SubscribeRequest) { r.Delivery.Secret = "" }, mcpevents.CodeInvalidParams, "invalid_secret"},
			{"http callback", func(r *mcpevents.SubscribeRequest) { r.Delivery.URL = "http://example.com/x" }, mcpevents.CodeInvalidParams, "invalid_callback"},
			{"private callback", func(r *mcpevents.SubscribeRequest) { r.Delivery.URL = "https://10.0.0.1/x" }, mcpevents.CodeInvalidParams, "invalid_callback"},
			{"blank recipient", func(r *mcpevents.SubscribeRequest) { r.Arguments.For = " " }, mcpevents.CodeInvalidParams, "invalid_arguments"},
			{"empty project list", func(r *mcpevents.SubscribeRequest) { r.Arguments.ProjectUIDs = []string{} }, mcpevents.CodeInvalidParams, "invalid_project_uids"},
			{"unsorted project list", func(r *mcpevents.SubscribeRequest) { r.Arguments.ProjectUIDs = []string{testProjectB, testProjectA} }, mcpevents.CodeInvalidParams, "project_uids_not_sorted_unique"},
			{"duplicate project list", func(r *mcpevents.SubscribeRequest) { r.Arguments.ProjectUIDs = []string{testProjectA, testProjectA} }, mcpevents.CodeInvalidParams, "project_uids_not_sorted_unique"},
			{"lowercase project uid", func(r *mcpevents.SubscribeRequest) { r.Arguments.ProjectUIDs = []string{strings.ToLower(testProjectA)} }, mcpevents.CodeInvalidParams, "invalid_project_uids"},
			{"bad issue uid", func(r *mcpevents.SubscribeRequest) { r.Arguments.IssueUID = "nope" }, mcpevents.CodeInvalidParams, "invalid_issue_uid"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				req := h.request("example-user")
				tc.mutate(&req)
				_, err := h.subscribe(principalA, req)
				_ = requireWire(t, err, tc.code, tc.reason)
				assert.Zero(t, h.recv.challenges.Load(), "validation fails before any network activity")
			})
		}
		t.Run("bad cursor MAC", func(t *testing.T) {
			req := h.request("example-user")
			cursor := "c1.0123456789abcdef0123456789abcdef.0.5.AAAAAAAAAAAAAAAAAAAAAA"
			req.Cursor = &cursor
			_, err := h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeInvalidParams, "invalid_cursor")
		})
		t.Run("invalid principal", func(t *testing.T) {
			_, err := h.subscribe(mcpevents.Principal{Authority: "bootstrap", Subject: "x"}, h.request("example-user"))
			assert.ErrorIs(t, err, mcpevents.ErrForbidden)
		})
	})
}

func TestForwarderScopeNeverRewritesSelection(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		h.fake.setProjects(mcpevents.Project{UID: testProjectA, ID: 1}, mcpevents.Project{UID: testProjectB, ID: 2})
		bound := mcpevents.ForwarderScope{Projects: []string{testProjectA}}
		req := h.request("example-user")

		_, err := h.svc.Subscribe(t.Context(), principalA, bound, req)
		_ = requireWire(t, err, mcpevents.CodeForbidden, "project_scope") // absent selection

		req.Arguments.ProjectUIDs = []string{testProjectA, testProjectB}
		_, err = h.svc.Subscribe(t.Context(), principalA, bound, req)
		_ = requireWire(t, err, mcpevents.CodeForbidden, "project_scope") // not a subset

		req.Arguments.ProjectUIDs = []string{testProjectA}
		result, err := h.svc.Subscribe(t.Context(), principalA, bound, req)
		require.NoError(t, err)
		assert.JSONEq(t, `{"for":"example-user","project_uids":["`+testProjectA+`"]}`, string(h.row(result.ID).Arguments))

		// Unsubscribe and status need only ownership, so a narrowed forwarder
		// can still clean up.
		require.NoError(t, h.svc.Unsubscribe(t.Context(), principalA, mcpevents.UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: mcpevents.Delivery{URL: req.Delivery.URL}}))
	})
}

func TestSelectionVisibility(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		t.Run("an empty catalog keeps a dynamic subscription active", func(t *testing.T) {
			h := newHarness(t, open(t))
			h.fake.setProjects()
			result := h.mustSubscribe(h.request("example-user/dynamic"))
			assert.Equal(t, db.MCPEventStateActive, h.row(result.ID).State)
			result = h.mustSubscribe(h.request("example-user/dynamic")) // refresh
			assert.Equal(t, db.MCPEventStateActive, h.row(result.ID).State)
		})
		t.Run("an explicit selection with nothing visible ends scope_removed", func(t *testing.T) {
			h := newHarness(t, open(t))
			req := h.request("example-user/explicit")
			req.Arguments.ProjectUIDs = []string{testProjectA}
			result := h.mustSubscribe(req)
			h.fake.setProjects()

			_, err := h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeForbidden, "scope_removed")
			row := h.row(result.ID)
			assert.Equal(t, db.MCPEventStateEnded, row.State)
			assert.Equal(t, "scope_removed", row.EndReason)
			assert.Equal(t, db.MCPEventPendingTerminated, row.PendingKind)
			assert.JSONEq(t, `{"type":"terminated","error":{"code":-32024,"message":"Forbidden","data":{"reason":"scope_removed"}}}`, string(row.PendingBody))
			assert.True(t, strings.HasPrefix(row.PendingMessageID, "msg_terminated_"))

			// The cause still holds, so a later subscribe is refused.
			_, err = h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeForbidden, "scope_removed")
			assert.Equal(t, db.MCPEventStateEnded, h.row(result.ID).State)

			// Once the project is visible again the subscription reactivates.
			h.fake.setProjects(mcpevents.Project{UID: testProjectA, ID: 1})
			_, err = h.subscribe(principalA, req)
			require.NoError(t, err)
			reactivated := h.row(result.ID)
			assert.Equal(t, db.MCPEventStateActive, reactivated.State)
			assert.Empty(t, reactivated.PendingKind)
		})
		t.Run("a new explicit selection with nothing visible is refused", func(t *testing.T) {
			h := newHarness(t, open(t))
			h.fake.setProjects()
			req := h.request("example-user/new")
			req.Arguments.ProjectUIDs = []string{testProjectA}
			_, err := h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeForbidden, "scope_removed")
			assert.Zero(t, h.recv.challenges.Load())
		})
	})
}

func TestPrincipalRevokedStaysRefused(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		result := h.mustSubscribe(req)

		h.auth.fail(mcpevents.ErrPrincipalRevoked)
		_, err := h.subscribe(principalA, req)
		_ = requireWire(t, err, mcpevents.CodeForbidden, "principal_revoked")
		row := h.row(result.ID)
		assert.Equal(t, db.MCPEventStateEnded, row.State)
		assert.Equal(t, "principal_revoked", row.EndReason)
		assert.Equal(t, db.MCPEventPendingTerminated, row.PendingKind)

		_, err = h.subscribe(principalA, req)
		_ = requireWire(t, err, mcpevents.CodeForbidden, "principal_revoked")

		h.auth.fail(mcpevents.ErrAuthorityUnavailable)
		_, err = h.subscribe(principalA, req)
		_ = requireWire(t, err, mcpevents.CodeUnavailable, "authority_unavailable")
		assert.Equal(t, db.MCPEventStateEnded, h.row(result.ID).State, "an unavailable decision neither allows nor ends")
	})
}

func TestIssueFilterMustBeVisible(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		h.fake.hiddenIssue = map[string]bool{testIssue: true}
		req := h.request("example-user")
		req.Arguments.IssueUID = testIssue
		_, err := h.subscribe(principalA, req)
		_ = requireWire(t, err, mcpevents.CodeForbidden, "issue_not_visible")
	})
}

func TestUnsubscribe(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")

		err := h.unsubscribe(req)
		_ = requireWire(t, err, mcpevents.CodeNotFound, "subscription")
		assert.Equal(t, "subscription", err.(*mcpevents.Error).Data["kind"])

		result := h.mustSubscribe(req)
		before := h.row(result.ID)
		require.NoError(t, h.unsubscribe(req))
		after := h.row(result.ID)
		assert.Equal(t, db.MCPEventStateEnded, after.State)
		assert.Equal(t, "unsubscribed", after.EndReason)
		assert.Equal(t, before.Generation+1, after.Generation, "ending fences outstanding claims")
		assert.Empty(t, after.PendingKind, "unsubscribe sends no control body")

		_ = requireWire(t, h.unsubscribe(req), mcpevents.CodeNotFound, "subscription")

		// Ownership is part of the key: another principal finds nothing.
		other := h.mustSubscribe(req)
		err = h.svc.Unsubscribe(t.Context(), mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "8", Actor: "example-user"},
			mcpevents.UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: mcpevents.Delivery{URL: req.Delivery.URL}})
		_ = requireWire(t, err, mcpevents.CodeNotFound, "subscription")
		assert.Equal(t, db.MCPEventStateActive, h.row(other.ID).State)
	})
}

func TestSubscriptionIDIsOwnedByThePrincipalKey(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		a, err := h.subscribe(principalA, req)
		require.NoError(t, err)
		b, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "8", Actor: "example-user"}, req)
		require.NoError(t, err)
		assert.NotEqual(t, a.ID, b.ID)

		host1, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1", Actor: "shared-actor"}, req)
		require.NoError(t, err)
		host2, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-2", Actor: "shared-actor"}, req)
		require.NoError(t, err)
		assert.NotEqual(t, host1.ID, host2.ID, "two host subjects with one actor are two principals")
		assert.True(t, strings.HasPrefix(a.ID, "sub_"))
		assert.Equal(t, a.ID, mcpevents.SubscriptionID(principalA.Key(), mcpevents.EventName, mcpevents.CanonicalArguments(mcpevents.Arguments{For: "example-user"}), testCallback))
	})
}

func TestStatusListsOnlyTheCallersSubscriptionsWithoutDestinations(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		req.Arguments.ProjectUIDs = []string{testProjectA}
		result := h.mustSubscribe(req)
		_, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "8"}, h.request("someone-else"))
		require.NoError(t, err)

		statuses, err := h.svc.Status(t.Context(), principalA)
		require.NoError(t, err)
		require.Len(t, statuses, 1)
		st := statuses[0]
		assert.Equal(t, result.ID, st.ID)
		assert.Equal(t, "example-user", st.Recipient)
		assert.Equal(t, []string{testProjectA}, st.ProjectUIDs)
		assert.Equal(t, "active", st.State)
		assert.Equal(t, result.Cursor, st.Cursor)
		encoded, err := json.Marshal(statuses)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "example.com")
		assert.NotContains(t, string(encoded), "whsec_")

		list, err := h.svc.List(t.Context(), principalA)
		require.NoError(t, err)
		require.Len(t, list.Events, 1)
		assert.Equal(t, mcpevents.EventName, list.Events[0].Name)
		assert.Equal(t, []string{"webhook"}, list.Events[0].Delivery)
	})
}

func TestTerminatedBodyMessageIsTheWireName(t *testing.T) {
	for reason, want := range map[string]struct {
		code    int
		message string
	}{
		"principal_revoked": {mcpevents.CodeForbidden, "Forbidden"},
		"event_removed":     {mcpevents.CodeNotFound, "NotFound"},
	} {
		body, _, err := mcpevents.TerminatedBody(reason)
		require.NoError(t, err)
		var decoded struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &decoded), reason)
		assert.Equal(t, want.code, decoded.Error.Code, reason)
		assert.Equal(t, want.message, decoded.Error.Message, reason)
	}
}

func TestOmittedTTLHonorsConfiguredMaximumOnCreateAndRefresh(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarnessWith(t, store, func(cfg *mcpevents.ServiceConfig) { cfg.MaxTTL = 2 * time.Hour })
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		assert.Equal(t, h.clock.Now().Add(2*time.Hour), *h.row(first.ID).ExpiresAt)
		h.clock.Advance(time.Hour)
		refreshed := h.mustSubscribe(req)
		assert.Equal(t, first.ID, refreshed.ID)
		assert.Equal(t, h.clock.Now().Add(2*time.Hour), *h.row(first.ID).ExpiresAt)
	})
}

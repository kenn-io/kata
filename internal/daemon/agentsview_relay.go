package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// AgentsViewSnapshot is a bounded private immutable spool supplied by the
// existing binary transport adapter. Close removes it on every return path.
type AgentsViewSnapshot interface {
	io.ReadSeeker
	io.Closer
}

// AgentsViewIngestAck identifies the snapshot accepted by the receiver.
type AgentsViewIngestAck struct {
	SessionID string
	Agent     string
	Status    string
}

// AgentsViewRelayConfig contains narrow dependency seams, not another receiver
// or transport. Nil dependencies refuse before body admission. Selection must
// verify pinned discovery at close and re-read live policy/route on dispatch.
// Spool is implemented by the existing binary signing/spool primitive; it must
// authenticate headers before reading, enforce bounds/digest and remove files.
// Forward must use only the selected immediate-upstream enrollment credential,
// remap/sign control and use the same bytes; Ingest resolves only root policy.
// No default implementation reads process HOME or receiver credentials.
type AgentsViewRelayConfig struct {
	// LiveClaimStatus uses only the selected enrollment-pinned existing claim transport.
	// It must fail on missing/offline proof; it never reads CLI-home auth.
	LiveClaimStatus func(context.Context, db.FederationBinding, string) (api.ClaimStatusBody, error)
	Selection       func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error
	Spool           func(context.Context, io.Reader, agentsviewrelay.Control) (AgentsViewSnapshot, error)
	Forward         func(context.Context, db.Issue, agentsviewrelay.Control, io.ReadSeeker) (agentsviewrelay.Receipt, error)
	Ingest          func(context.Context, db.Issue, agentsviewrelay.Association, io.ReadSeeker) (AgentsViewIngestAck, error)
	// VerifyFederationUpload authenticates the exact signed control header,
	// generation and raw-body profile using the existing binary verifier.
	// Until PR983's profile is integrated, federation upload remains unavailable.
	VerifyFederationUpload func(context.Context, agentsviewrelay.Control) error
}

func agentsViewUnavailable() error {
	return api.NewError(503, "agentsview_unavailable", "AgentsView relay dependency is unavailable", "", nil)
}

// requireAgentsViewClaimWhileCoordinated reuses the existing claim client,
// remapping and store gate under the caller's shared project lock. Reacquiring
// that lock can deadlock behind a waiting rebind. Unlike ordinary offline local
// edits, sharing requires a successful live upstream claim status response.
func requireAgentsViewClaimWhileCoordinated(ctx context.Context, cfg ServerConfig, issue db.Issue, actor string, incoming *federationPrincipal, clientKind string, mapped *db.ClaimPrincipal) error {
	principal := db.ClaimPrincipal{HolderInstanceUID: cfg.DB.InstanceUID(), Holder: strings.TrimSpace(actor), ClientKind: clientKind}
	if incoming != nil {
		principal = db.ClaimPrincipal{HolderInstanceUID: incoming.SpokeInstanceUID, Holder: incoming.Actor, ClientKind: clientKind}
	} else if cfg.HostAccess != nil {
		p, ok := PrincipalFromContext(ctx)
		if !ok || !validHostPrincipal(p) {
			return api.NewError(401, "auth_required", "valid host principal required", "", nil)
		}
		principal = hostClaimPrincipal(cfg, api.ClaimActionBody{}, p.Subject).ClaimPrincipal
	}
	binding, err := cfg.DB.FederationBindingByProject(ctx, issue.ProjectID)
	if err != nil || !binding.Enabled {
		return api.NewError(409, "agentsview_route_changed", "selected project route is unavailable", "", nil)
	}
	if binding.Role == db.FederationRoleSpoke {
		if !binding.PushEnabled || binding.RelayConfig == nil || binding.RelayConfig.UpstreamRevoked {
			return api.NewError(409, "agentsview_route_changed", "selected upstream is unavailable", "", nil)
		}
		if cfg.AgentsViewRelay == nil || cfg.AgentsViewRelay.LiveClaimStatus == nil {
			return agentsViewUnavailable()
		}
		status, err := cfg.AgentsViewRelay.LiveClaimStatus(ctx, binding, issue.UID)
		if err != nil {
			return agentsViewUnavailable()
		}
		status, err = remapCachedClaimStatus(ctx, cfg.DB, binding.ProjectID, issue.UID, status)
		if err != nil {
			return claimAPIError(err)
		}
		if err = cfg.DB.ApplyClaimStatus(ctx, binding.ProjectID, issue.UID, claimStatusFromAPI(status)); err != nil {
			return claimAPIError(err)
		}
	}
	principal = boundSpokeClaimPrincipal(binding, principal, cfg.DB.InstanceUID())
	if err := cfg.DB.CheckClaimGate(ctx, db.ClaimGateParams{ProjectID: issue.ProjectID, IssueRef: issue.UID, Principal: principal, Now: time.Now().UTC()}); err != nil {
		return claimGateAPIError(err)
	}
	if mapped != nil {
		*mapped = principal
	}
	return nil
}

func agentsViewForwardControl(c agentsviewrelay.Control, principal db.ClaimPrincipal) agentsviewrelay.Control {
	c.ClientKind = principal.ClientKind
	return c
}

func decodeAgentsViewAssociation(raw jsontext.Value) (*agentsviewrelay.Association, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > agentsviewrelay.MaxProofBytes {
		return nil, errors.New("AgentsView association too large")
	}
	var a agentsviewrelay.Association
	if err := json.Unmarshal(raw, &a, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	a = a.Clone()
	return &a, nil
}

func validateAgentsViewClose(ctx context.Context, cfg ServerConfig, issue db.Issue, a *agentsviewrelay.Association, proof jsontext.Value) (func(), error) {
	if cfg.AgentsViewRelay == nil || cfg.AgentsViewRelay.Selection == nil {
		return nil, agentsViewUnavailable()
	}
	if len(proof) == 0 || len(proof) > agentsviewrelay.MaxProofBytes || issue.UID != a.IssueUID {
		return nil, api.NewError(400, "agentsview_invalid_association", "matching bounded discovery proof required", "", nil)
	}
	finish, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(cfg.DB.InstanceUID(), issue.ProjectID), cfg.DB, issue.ProjectID)
	if err != nil {
		return nil, api.NewError(503, "agentsview_unavailable", "project route is busy", "", nil)
	}
	selectionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = cfg.AgentsViewRelay.Selection(selectionCtx, issue, a.Clone(), proof); err != nil {
		finish()
		return nil, err
	}
	return finish, nil
}

// loadAgentsViewClose uses the real event producer and current history. A
// missing origin event is pending; it never synthesizes/injects that event.
func loadAgentsViewClose(ctx context.Context, cfg ServerConfig, issue db.Issue, c agentsviewrelay.Control) (agentsviewrelay.Association, error) {
	if issue.UID != c.IssueUID {
		return agentsviewrelay.Association{}, api.NewError(409, "agentsview_close_changed", "original close is no longer current", "", nil)
	}
	events, err := cfg.DB.EventsByUIDs(ctx, issue.ProjectID, []string{c.CloseEventUID})
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return agentsviewrelay.Association{}, internalAPIError(err)
	}
	if events == nil || len(events) != 1 || errors.Is(err, db.ErrNotFound) {
		return agentsviewrelay.Association{}, api.NewError(409, "close_not_at_origin", "original close is not available here", "retry explicitly after normal replication", nil)
	}
	if issue.Status != "closed" {
		return agentsviewrelay.Association{}, api.NewError(409, "agentsview_close_changed", "original close is no longer current", "", nil)
	}
	e := events[0]
	if e.Type != "issue.closed" || e.IssueUID == nil || *e.IssueUID != issue.UID || e.ProjectUID != c.ProjectUID {
		return agentsviewrelay.Association{}, api.NewError(409, "agentsview_association_changed", "original event differs from selection", "", nil)
	}
	var payload struct {
		Association *agentsviewrelay.Association `json:"agentsview_association"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil || payload.Association == nil {
		return agentsviewrelay.Association{}, api.NewError(409, "agentsview_association_changed", "original close has no matching association", "", nil)
	}
	a := payload.Association.Clone()
	selected := c.Association(a.LocatorBase)
	selected.RootKeyID = a.RootKeyID
	if a.Validate() != nil || !a.Equal(selected) {
		return agentsviewrelay.Association{}, api.NewError(409, "agentsview_association_changed", "snapshot differs from original close", "", nil)
	}
	current, err := effectiveIssueCloseEventUID(ctx, cfg.DB, issue)
	if err != nil {
		return agentsviewrelay.Association{}, internalAPIError(err)
	}
	if current != c.CloseEventUID {
		return agentsviewrelay.Association{}, api.NewError(409, "agentsview_close_changed", "a different close is effective", "", nil)
	}
	// Include hidden intervening transitions for the decision only. Never return
	// their data; moving away and back must not resurrect old consent.
	historyCtx := db.WithAuthorizedProjects(ctx, nil)
	var cursor int64
	for {
		history, err := cfg.DB.EventsAfter(historyCtx, db.EventsAfterParams{AfterID: cursor, IssueUID: issue.UID, Types: []string{"issue.moved", "issue.reopened", "issue.closed"}, Limit: pollLimitMax})
		if err != nil {
			return agentsviewrelay.Association{}, internalAPIError(err)
		}
		for _, transition := range history {
			if agentsViewEventAfter(transition, e) {
				return agentsviewrelay.Association{}, api.NewError(409, "agentsview_close_changed", "issue history changed after consented close", "", nil)
			}
			if transition.ID <= cursor {
				return agentsviewrelay.Association{}, internalAPIError(errors.New("issue history cursor did not advance"))
			}
			cursor = transition.ID
		}
		if len(history) < pollLimitMax {
			break
		}
	}

	return a, nil
}

// agentsViewEventAfter uses the same HLC ordering as the federation fold.
func agentsViewEventAfter(a, b db.Event) bool {
	return db.CompareEventClocks(a, b) > 0
}

func revalidateAgentsViewCaller(ctx context.Context) error {
	if decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision); decision != nil {
		if err := decision.revalidateRelease(ctx); err != nil {
			return err
		}
	}
	if state, _ := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); state != nil {
		// Re-evaluate even when a host supplied no long-lived lease callback.
		if _, err := state.controller.Authorize(ctx, state.request); err != nil {
			return api.NewError(403, "auth_invalid", "host authority changed", "", nil)
		}
	}
	return revalidateHostAccess(ctx)
}

// withAgentsViewRelayBudget includes project, host and signed preauthorization
// in the transfer budget; it preserves any earlier caller deadline.
func withAgentsViewRelayBudget(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && (strings.HasSuffix(r.URL.Path, "/transcript-share") || strings.Contains(r.URL.Path, "/federation/transcript-service/issues/")) {
			ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			_ = http.NewResponseController(w).SetReadDeadline(deadline)
			_ = http.NewResponseController(w).SetWriteDeadline(deadline)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

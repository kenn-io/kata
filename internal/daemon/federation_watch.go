package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
)

// federationWatchMaxProjects bounds the projects one watch names, counting the
// route project. At about 120 bytes per project the request stays well under
// the 64 KiB federation control body limit.
const federationWatchMaxProjects = 256

// federationWatchAdvertised is the watch size a hub advertises: none when it
// requires signed requests, because a watch cannot prove the signing key of
// any project but the route's.
func federationWatchAdvertised(cfg ServerConfig) int {
	if cfg.FederationSigningRequired {
		return 0
	}
	return federationWatchMaxProjects
}

// federationWatchCarriesTokens reports whether a watch may authorize projects
// with the tokens in its body. Those tokens prove bearer possession only, so
// they stand in for a project's own poll only where that poll needs nothing
// more: an unsigned request on a hub that does not require signatures, with no
// daemon principal composed into it.
func federationWatchCarriesTokens(ctx context.Context, cfg ServerConfig) bool {
	if cfg.FederationSigningRequired {
		return false
	}
	if _, signed := ctx.Value(federationSigningAdmissionContextKey{}).(federationsigning.Admission); signed {
		return false
	}
	_, authenticated := PrincipalFromContext(ctx)
	return !authenticated
}

// federationWatchEntry is one project a watch names, with the bearer that
// authorizes pulls from it.
type federationWatchEntry struct {
	projectID  int64
	afterID    int64
	authHeader string
	project    db.Project
	// base is the context each authorization of this entry starts from.
	base context.Context
	// ctx carries the entry's latest authorization for its reads.
	ctx context.Context
}

// supplementalFederationWatchBase isolates a supplemental project from the
// route's authorization: the request's project scope and access decision
// belong to the route credential's project, and authorizing another project
// must neither borrow nor rewrite them.
func supplementalFederationWatchBase(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, projectAccessContextKey{}, (*ProjectAccessDecision)(nil))
	return db.WithAuthorizedProjects(ctx, nil)
}

// outsideRequestScope reports whether a supplemental project lies outside the
// fixed project scope the request was admitted with. The watch names such a
// project as unwatched before any host admission for it, as other
// supplemental credentials are refused. A missing project answers the same, so
// the answer reveals nothing about which projects exist.
func outsideRequestScope(ctx context.Context, store db.Storage, projectID int64) (bool, error) {
	decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision)
	if decision == nil || decision.fixedUIDs == nil {
		return false, nil
	}
	project, err := store.ProjectByID(supplementalFederationWatchBase(ctx), projectID)
	if errors.Is(err, db.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, internalAPIError(err)
	}
	return !slices.Contains(decision.fixedUIDs, project.UID), nil
}

// authorize checks the entry's bearer against its project from scratch, as a
// project poll by that bearer would be checked.
func (e *federationWatchEntry) authorize(cfg ServerConfig, operation HostFederationOperation) error {
	ctx, project, err := reauthorizeFederationPoll(e.base, cfg, e.authHeader, e.projectID, operation)
	if err != nil {
		return err
	}
	e.ctx, e.project = ctx, project
	return nil
}

// ready reports whether the entry's project has events or a reset after its
// cursor. A read the spoke's own poll would refuse, such as one needing event
// features the spoke lacks, also counts: the poll reports why.
func (e *federationWatchEntry) ready(cfg ServerConfig, features string) (bool, error) {
	result, err := cfg.DB.ReadFederation(e.ctx, db.FederationReadParams{ProjectID: e.projectID, Features: features, AfterID: e.afterID, Limit: 1})
	if err != nil {
		apiErr := federationFeatureError(err)
		if status := apiStatus(apiErr); status >= 400 && status < 500 {
			return true, nil
		}
		return false, apiErr
	}
	return !federationPollCaughtUp(result), nil
}

func apiStatus(err error) int {
	if apiErr, ok := errors.AsType[*api.APIError](err); ok {
		return apiErr.Status
	}
	return 0
}

// federationWatchDenied reports an authorization failure that removes one
// project from the watch instead of failing the request.
func federationWatchDenied(err error) bool {
	switch apiStatus(err) {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func validateFederationWatch(in *api.FederationWatchEventsRequest) error {
	invalid := func(message string) error {
		return api.NewError(http.StatusBadRequest, "validation", message, "", nil)
	}
	if in.ProjectID <= 0 {
		return invalid("project_id must be a positive integer")
	}
	if in.Body.AfterID < 0 {
		return invalid("after_id must be non-negative")
	}
	if len(in.Body.Projects) >= federationWatchMaxProjects {
		return invalid(fmt.Sprintf("a watch names at most %d projects, counting the route project", federationWatchMaxProjects))
	}
	seen := map[int64]bool{in.ProjectID: true}
	for _, project := range in.Body.Projects {
		switch {
		case project.ProjectID <= 0:
			return invalid("projects[].project_id must be a positive integer")
		case seen[project.ProjectID]:
			return invalid("projects must be distinct and must not repeat the route project")
		case project.AfterID < 0:
			return invalid("projects[].after_id must be non-negative")
		case project.Token == "":
			return invalid("projects[].token is required")
		}
		seen[project.ProjectID] = true
	}
	return nil
}

func registerFederationWatch(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID:  "watchFederationProjectEvents",
		Method:       http.MethodPost,
		Path:         "/api/v1/projects/{project_id}/federation/events:watch",
		MaxBodyBytes: 64 << 10,
	}, func(ctx context.Context, in *api.FederationWatchEventsRequest) (*api.FederationWatchEventsResponse, error) {
		requestCtx := ctx
		operation := federationTransportOperation("watchFederationProjectEvents")
		ctx, principal, err := authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull", operation)
		if err != nil {
			return nil, err
		}
		if err := rejectRelayCredentialOnLegacyEvents(principal); err != nil {
			return nil, err
		}
		if err := validateFederationWatch(in); err != nil {
			return nil, err
		}
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		wait, err := federationPollWait(requestCtx, in.Wait)
		if err != nil {
			return nil, err
		}
		holdUntil := time.Now().Add(wait)

		// The route project keeps the request's own authorization; every
		// other project is authorized by its own token alone.
		entries := []*federationWatchEntry{{
			projectID: in.ProjectID, afterID: in.Body.AfterID, authHeader: in.Authorization,
			project: project, base: requestCtx, ctx: db.WithFederationEventStream(ctx),
		}}
		unwatched := []int64{}
		// keep authorizes an entry again. A denial names its project
		// unwatched and drops it; any other failure ends the request.
		keep := func(entry *federationWatchEntry) (bool, error) {
			err := entry.authorize(cfg, operation)
			if err != nil && federationWatchDenied(err) {
				unwatched = append(unwatched, entry.projectID)
				return false, nil
			}
			return err == nil, err
		}
		ready := map[int64]bool{}
		markReady := func(entry *federationWatchEntry) error {
			found, err := entry.ready(cfg, in.EventFeatures)
			if found {
				ready[entry.projectID] = true
			}
			return err
		}
		carriesTokens := federationWatchCarriesTokens(requestCtx, cfg)
		for _, other := range in.Body.Projects {
			if !carriesTokens {
				unwatched = append(unwatched, other.ProjectID)
				continue
			}
			outside, err := outsideRequestScope(requestCtx, cfg.DB, other.ProjectID)
			if err != nil {
				return nil, err
			}
			if outside {
				unwatched = append(unwatched, other.ProjectID)
				continue
			}
			entry := &federationWatchEntry{
				projectID: other.ProjectID, afterID: other.AfterID, authHeader: authBearerPrefix + other.Token,
				base: supplementalFederationWatchBase(requestCtx),
			}
			if kept, err := keep(entry); err != nil {
				return nil, err
			} else if kept {
				entries = append(entries, entry)
			}
		}

		var waiter *federationPollWaiter
		if wait > 0 && heldFederationPollAllowed(requestCtx) {
			projects := make([]db.Project, len(entries))
			for i, entry := range entries {
				projects[i] = entry.project
			}
			waiter = cfg.federationPollWaits.admit(cfg.Broadcaster, projects...)
			defer waiter.release()
		}
		for _, entry := range entries {
			if err := markReady(entry); err != nil {
				return nil, err
			}
		}
		for waiter != nil && len(ready) == 0 && len(entries) > 0 && time.Now().Before(holdUntil) {
			woken, err := waiter.wait(requestCtx, holdUntil)
			if err != nil {
				return nil, fmt.Errorf("held federation watch ended: %w", err)
			}
			if len(woken) == 0 {
				break
			}
			// Authorize each woken project again before reading it, so
			// revocation during the wait applies.
			kept := entries[:0]
			for _, entry := range entries {
				if !woken[entry.projectID] {
					kept = append(kept, entry)
					continue
				}
				if ok, err := keep(entry); err != nil {
					return nil, err
				} else if !ok {
					continue
				}
				kept = append(kept, entry)
				if err := markReady(entry); err != nil {
					return nil, err
				}
			}
			entries = kept
		}
		// Name a project as ready only while its own credential still
		// authorizes pulls from it.
		body := api.FederationWatchEventsBody{Ready: []int64{}}
		for _, entry := range entries {
			if !ready[entry.projectID] {
				continue
			}
			if ok, err := keep(entry); err != nil {
				return nil, err
			} else if ok {
				body.Ready = append(body.Ready, entry.projectID)
			}
		}
		body.Unwatched = unwatched
		slices.Sort(body.Ready)
		slices.Sort(body.Unwatched)
		// The body names only projects each credential authorized just
		// before the answer, and carries none of their data.
		releaseProjectAccess(requestCtx)
		return &api.FederationWatchEventsResponse{
			PollWaitMax: int(federationPollWaitMax / time.Second),
			WatchMax:    federationWatchAdvertised(cfg),
			Body:        body,
		}, nil
	})
}

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/pkg/client/generated"
)

// batchPollRefusalHold is how long a spoke keeps polling one project at a
// time after a hub that advertised batches refused the batch route, as an
// embedding host that does not allow the route does.
const batchPollRefusalHold = time.Hour

// batchPollResult is a project's answer from the pass's batched poll: an empty
// page for an idle project, or the refusal its single-project poll returned.
// Body and Err are what PollProjectEvents would have returned.
type batchPollResult struct {
	Err  error
	Body api.PollEventsBody
}

// pollProjectEventsBatch asks the hub which of several projects that share this
// client's credential are idle, in one request. Results are in cursor order; a
// nil result is a project with something to read, which its turn polls.
func (c *Client) pollProjectEventsBatch(ctx context.Context, cursors []api.FederationBatchPollCursor) ([]*batchPollResult, error) {
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(replicationDoer{c.client}))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(api.FederationBatchPollRequestBody{Projects: cursors})
	if err != nil {
		return nil, err
	}
	var payload generated.PollFederationEventsBatchBody
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	response, callErr := apiClient.PollFederationEventsBatchWithResponse(ctx, &generated.PollFederationEventsBatchRequestOptions{Body: &payload})
	if response == nil {
		return nil, callErr
	}
	var body api.FederationBatchPollBody
	if err := decodeReplicationResponse(response.HTTPResponse, response.Body, &body); err != nil {
		return nil, err
	}
	if len(body.Projects) != len(cursors) {
		return nil, fmt.Errorf("hub batched poll returned %d projects for %d requested", len(body.Projects), len(cursors))
	}
	results := make([]*batchPollResult, len(cursors))
	for i, entry := range body.Projects {
		if entry.ProjectID != cursors[i].ProjectID {
			return nil, fmt.Errorf("hub batched poll returned project %d in place of %d", entry.ProjectID, cursors[i].ProjectID)
		}
		results[i] = batchPollEntryResult(entry, cursors[i].AfterID)
	}
	return results, nil
}

// batchPollEntryResult rebuilds the outcome the single-project poll route
// would have given the replication client for an idle or refused project.
func batchPollEntryResult(entry api.FederationBatchPollPage, afterID int64) *batchPollResult {
	switch {
	case entry.Status >= 200 && entry.Status < 300:
		if !entry.Idle {
			return nil
		}
		return &batchPollResult{Body: api.PollEventsBody{Events: []api.EventEnvelope{}, NextAfterID: afterID}}
	default:
		message := ""
		if entry.Error != nil {
			raw, _ := json.Marshal(entry.Error)
			message = string(raw)
		}
		return &batchPollResult{Err: &HubStatusError{
			Path:       fmt.Sprintf("/api/v1/projects/%d/federation/events", entry.ProjectID),
			StatusCode: entry.Status, Body: message,
		}}
	}
}

// federationTurnObservation records what one spoke turn learned from and did
// to its hub, so the runner can decide whether a page polled earlier in the
// pass still answers this turn's pull.
type federationTurnObservation struct {
	pollFeatures string
	polled       bool
	hubWrite     bool
}

type federationTurnObservationKey struct{}

func withFederationTurnObservation(ctx context.Context) (context.Context, *federationTurnObservation) {
	observation := &federationTurnObservation{}
	return context.WithValue(ctx, federationTurnObservationKey{}, observation), observation
}

func observeFederationPoll(ctx context.Context, features string) {
	if observation, ok := ctx.Value(federationTurnObservationKey{}).(*federationTurnObservation); ok {
		observation.polled, observation.pollFeatures = true, features
	}
}

func observeFederationHubWrite(ctx context.Context) {
	if observation, ok := ctx.Value(federationTurnObservationKey{}).(*federationTurnObservation); ok {
		observation.hubWrite = true
	}
}

// prefetchedFederationPoll is a project's answer from the pass's batched poll,
// with the hub, credential and cursor it was checked at.
type prefetchedFederationPoll struct {
	result       batchPollResult
	key          batchPollKey
	hubProjectID int64
	afterID      int64
}

type prefetchedFederationPollKey struct{}

// firstFederationPoll returns this turn's first page. The pass's batched poll
// answers it only when it checked the cursor the turn holds and the turn has
// not written to the hub, so a push or claim retry is still followed by a poll
// that can see it, as before batching.
func firstFederationPoll(ctx context.Context, client *Client, hubProjectID, afterID int64) (api.PollEventsBody, error) {
	prefetched, _ := ctx.Value(prefetchedFederationPollKey{}).(*prefetchedFederationPoll)
	observation, _ := ctx.Value(federationTurnObservationKey{}).(*federationTurnObservation)
	if prefetched != nil && prefetched.afterID == afterID && (observation == nil || !observation.hubWrite) {
		return prefetched.result.Body, prefetched.result.Err
	}
	return client.PollProjectEvents(ctx, hubProjectID, afterID, federationPollLimit)
}

// batchPollKey groups the bindings whose polls one request may carry: the
// same hub origin, credential and transport policy.
type batchPollKey struct {
	hubURL        string
	token         [sha256.Size]byte
	allowInsecure bool
}

type batchPollHubState struct {
	refusedUntil time.Time
	advertised   bool
}

// batchPollStates is the runner's in-memory view of which credential groups
// a hub serves batched polls to.
type batchPollStates struct {
	groups map[batchPollKey]batchPollHubState
	// hubRefusedUntil pauses every credential group on a hub whose batch route
	// is missing or too slow, by canonical hub address.
	hubRefusedUntil map[string]time.Time
	mu              sync.Mutex
}

// batchPollGroupKey reports the batch group of a binding, or false when its
// polls must stay single: relay bindings, pending enrollments and signed
// credentials, whose signature covers one project's request.
func batchPollGroupKey(binding db.FederationBinding, cred config.FederationCredential) (batchPollKey, bool) {
	if !binding.Enabled || binding.Role != db.FederationRoleSpoke || binding.RelayConfig != nil ||
		cred.RelayEnrollmentPending || cred.Signing != nil || strings.TrimSpace(cred.Token) == "" {
		return batchPollKey{}, false
	}
	return batchPollKey{hubURL: cred.HubURL, token: sha256.Sum256([]byte(cred.Token)), allowInsecure: cred.AllowInsecure}, true
}

// anyAdvertised reports whether any hub has advertised batches to this runner.
func (s *batchPollStates) anyAdvertised() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.groups {
		if state.advertised {
			return true
		}
	}
	return false
}

func (s *batchPollStates) usable(key batchPollKey, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.groups[key]
	return state.advertised && !now.Before(state.refusedUntil) && !now.Before(s.hubRefusedUntil[batchPollHub(key)])
}

// batchPollHub is the canonical address of a group's hub.
func batchPollHub(key batchPollKey) string {
	if hub, err := httpurl.CanonicalHTTPBaseURL(key.hubURL); err == nil {
		return hub
	}
	return key.hubURL
}

// learn records what a single-project poll response advertised.
func (s *batchPollStates) learn(key batchPollKey, features string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups == nil {
		s.groups = map[batchPollKey]batchPollHubState{}
	}
	state := s.groups[key]
	state.advertised = slices.Contains(strings.Split(features, ","), db.FederationBatchPollFeature)
	s.groups[key] = state
}

// refuse pauses batches for the group, or for every group on its hub when
// the refusal does not depend on the credential.
func (s *batchPollStates) refuse(key batchPollKey, wholeHub bool, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wholeHub {
		if s.hubRefusedUntil == nil {
			s.hubRefusedUntil = map[string]time.Time{}
		}
		s.hubRefusedUntil[batchPollHub(key)] = until
		return
	}
	state := s.groups[key]
	state.refusedUntil = until
	s.groups[key] = state
}

type batchPollMember struct {
	cred         config.FederationCredential
	projectUID   string
	projectID    int64
	hubProjectID int64
	afterID      int64
	// retryIdentity is the fingerprint the project's own turn would record a
	// hub's request to wait under.
	retryIdentity [32]byte
}

// federationCredentialSnapshotter reads every project's credential at once.
type federationCredentialSnapshotter interface {
	FederationCredentialSnapshot(context.Context) (map[string]config.FederationCredential, error)
}

// groupingCredentials returns a lookup for sorting bindings into batch groups.
// A store that can snapshot is read once per pass; each batch member's
// credential is read again under its sync lock before anything is sent.
func groupingCredentials(ctx context.Context, credentials config.FederationCredentialStore) func(string) (config.FederationCredential, bool) {
	if snapshotter, ok := credentials.(federationCredentialSnapshotter); ok {
		snapshot, err := snapshotter.FederationCredentialSnapshot(ctx)
		return func(projectUID string) (config.FederationCredential, bool) {
			cred, found := snapshot[projectUID]
			return cred, err == nil && found
		}
	}
	return func(projectUID string) (config.FederationCredential, bool) {
		cred, found, err := credentials.FederationCredential(ctx, projectUID)
		return cred, err == nil && found
	}
}

// prefetchPolls sends one batched poll per credential group the hub serves
// batches to and returns each idle or refused project's answer by local
// project ID. Other projects take their single-project poll in their turn.
func (r *Runner) prefetchPolls(ctx context.Context, spokes []activeSpokeBinding, credentials config.FederationCredentialStore, include func(activeSpokeBinding) bool, retries *SyncRetries) map[int64]*prefetchedFederationPoll {
	now := r.refusalNow()
	lookup := groupingCredentials(ctx, credentials)
	groups := map[batchPollKey][]batchPollMember{}
	var order []batchPollKey
	for _, spoke := range spokes {
		if !include(spoke) {
			continue
		}
		cred, ok := lookup(spoke.project.UID)
		if !ok {
			continue
		}
		cred = config.FederationTransportCredential(spoke.binding.HubURL, spoke.binding.HubProjectID, spoke.binding.AllowInsecure, cred)
		key, ok := batchPollGroupKey(spoke.binding, cred)
		if !ok || !r.batchPolls.usable(key, now) {
			continue
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], batchPollMember{projectUID: spoke.project.UID, projectID: spoke.binding.ProjectID})
	}
	pages := map[int64]*prefetchedFederationPoll{}
	for _, key := range order {
		for members := range slices.Chunk(groups[key], api.FederationBatchPollMaxProjects) {
			r.prefetchGroup(ctx, key, members, credentials, pages, now, retries)
		}
	}
	return pages
}

// lockBatchMembers takes each member's shared sync lock without waiting and
// rereads its binding and credential under that lock, as a turn does. A member
// that is mid-rebind or no longer in the group is left out and polls in its own
// turn. Candidates whose lock was not free come back as unlocked. The caller
// releases the returned locks after the request.
func (r *Runner) lockBatchMembers(ctx context.Context, key batchPollKey, candidates []batchPollMember, credentials config.FederationCredentialStore) (members, unlocked []batchPollMember, release func()) {
	var releases []func()
	release = func() {
		for _, finish := range releases {
			finish()
		}
	}
	members = make([]batchPollMember, 0, len(candidates))
	for _, candidate := range candidates {
		finish, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(r.DB.InstanceUID(), candidate.projectID), r.DB, candidate.projectID)
		if err != nil {
			unlocked = append(unlocked, candidate)
			continue
		}
		releases = append(releases, finish)
		binding, err := r.DB.FederationBindingByProject(ctx, candidate.projectID)
		if err != nil {
			continue
		}
		stored, found, err := credentials.FederationCredential(ctx, candidate.projectUID)
		if err != nil || !found {
			continue
		}
		cred := config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, stored)
		if current, ok := batchPollGroupKey(binding, cred); !ok || current != key {
			continue
		}
		identity, err := syncRetryIdentity(binding, stored)
		if err != nil {
			continue
		}
		candidate.cred, candidate.hubProjectID, candidate.afterID = cred, cred.HubProjectID, binding.PullCursorEventID
		candidate.retryIdentity = identity
		members = append(members, candidate)
	}
	return members, unlocked, release
}

// prefetchGroup batches one chunk of a credential group. A PostgreSQL spoke
// holds one connection per project lock and has fewer of them than a chunk
// can hold, so each round releases its locks before it locks the projects it
// could not. A group then takes one batch per pool's worth of projects. It
// stops once the hub refuses or throttles the group or a round locks nothing
// new.
func (r *Runner) prefetchGroup(ctx context.Context, key batchPollKey, candidates []batchPollMember, credentials config.FederationCredentialStore, pages map[int64]*prefetchedFederationPoll, now time.Time, retries *SyncRetries) {
	// One project gains nothing from a batch; its turn polls it.
	for len(candidates) >= 2 && r.batchPolls.usable(key, now) {
		members, unlocked, release := r.lockBatchMembers(ctx, key, candidates, credentials)
		throttled := r.sendBatch(ctx, key, members, pages, retries)
		release()
		if throttled || len(unlocked) == len(candidates) {
			return
		}
		candidates = unlocked
	}
}

// sendBatch checks locked members in one request and records each idle or
// refused project's answer. It reports whether the hub asked the group to
// wait; the members are then held on the retry schedule their turns use.
func (r *Runner) sendBatch(ctx context.Context, key batchPollKey, members []batchPollMember, pages map[int64]*prefetchedFederationPoll, retries *SyncRetries) bool {
	if len(members) < 2 {
		return false
	}
	cred := members[0].cred
	client, err := NewClient(ctx, cred.HubURL, cred.Token, clientOptsForCredential(r.clientOpts(), cred))
	if err != nil {
		return false
	}
	cursors := make([]api.FederationBatchPollCursor, len(members))
	for i, member := range members {
		cursors[i] = api.FederationBatchPollCursor{ProjectID: member.hubProjectID, AfterID: member.afterID}
	}
	results, err := client.pollProjectEventsBatch(ctx, cursors)
	if hubAskedToWait(err) {
		for _, member := range members {
			retries.record(member.projectID, member.retryIdentity, err, r.effectiveInterval(), r.refusalNow())
		}
		if r.OnError != nil {
			r.OnError(fmt.Errorf("hub %s asked the batched federation poll to wait; holding its %d projects on their retry schedule: %w",
				cred.HubURL, len(members), err))
		}
		return true
	}
	if reason, wholeHub, refused := batchPollRefusal(err); refused {
		until := r.refusalNow().Add(batchPollRefusalHold)
		r.batchPolls.refuse(key, wholeHub, until)
		if r.OnError != nil {
			paused := "its projects"
			if !wholeHub {
				paused = "this credential's projects"
			}
			r.OnError(fmt.Errorf("hub %s answered the batched federation poll with %s; polling %s one at a time until %s",
				cred.HubURL, reason, paused, until.Format(time.RFC3339)))
		}
		return false
	}
	if err != nil {
		// Each project's own poll reports whatever is wrong with the hub.
		return false
	}
	for i, result := range results {
		if result != nil {
			pages[members[i].projectID] = &prefetchedFederationPoll{
				result: *result, key: key, hubProjectID: members[i].hubProjectID, afterID: members[i].afterID,
			}
		}
	}
	return false
}

// batchPollRefusal reports a batch failure that will repeat on every pass:
// a hub or a host in front of it that refuses the route, or a batch that
// takes longer than the request timeout although single polls may not.
// wholeHub is false only for 403, which can depend on the credential; a
// missing route or a timeout affects every credential on that hub.
func batchPollRefusal(err error) (reason string, wholeHub, refused bool) {
	if status, ok := errors.AsType[*HubStatusError](err); ok {
		switch status.StatusCode {
		case http.StatusForbidden:
			return fmt.Sprintf("status %d", status.StatusCode), false, true
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			return fmt.Sprintf("status %d", status.StatusCode), true, true
		}
		return "", false, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "a timeout", true, true
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return "a timeout", true, true
	}
	return "", false, false
}

// prefetchPass runs prefetchPolls under its own drain admission. A denied
// admission ends the pass, as it does for any spoke turn.
func (r *Runner) prefetchPass(
	ctx context.Context,
	spokes []activeSpokeBinding,
	credentials config.FederationCredentialStore,
	validateLease func(context.Context) error,
	include func(activeSpokeBinding) bool,
	retries *SyncRetries,
) (map[int64]*prefetchedFederationPoll, <-chan struct{}, bool) {
	if !r.batchPolls.anyAdvertised() || validateFederationRunnerLease(ctx, validateLease) != nil {
		// Nothing to batch, or the spoke turns report the lost lease.
		return nil, nil, false
	}
	if r.DrainAdmission != nil {
		lease, admitted, retry := r.DrainAdmission()
		if !admitted {
			return nil, retry, true
		}
		defer lease.Release()
	}
	return r.prefetchPolls(ctx, spokes, credentials, include, retries), nil, false
}

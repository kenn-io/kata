package federation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/vector"
)

// maxDirectArtifactBatch is the most issue UIDs one hub request may carry.
const maxDirectArtifactBatch = 32

// directArtifactRequestTimeout bounds one hub request, so an unresponsive hub
// delays the reconcile pass by at most this long per origin.
const directArtifactRequestTimeout = 15 * time.Second

// DirectArtifactFetcher asks a direct spoke's hub for the embedding artifacts
// the local index is missing and retains them. It adds no loop of its own: the
// reconciler calls Fetch each pass, and a per-project throttle keeps that to
// one hub request per project per interval.
type DirectArtifactFetcher struct {
	store       db.Storage
	credentials config.FederationCredentialStore
	opts        clientpkg.Opts
	interval    time.Duration
	now         func() time.Time

	requestTimeout time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

// NewDirectArtifactFetcher builds a fetcher that sends at most one request per
// project every interval.
func NewDirectArtifactFetcher(store db.Storage, credentials config.FederationCredentialStore, opts clientpkg.Opts, interval time.Duration) *DirectArtifactFetcher {
	return &DirectArtifactFetcher{store: store, credentials: credentials, opts: opts, interval: interval, now: time.Now, requestTimeout: directArtifactRequestTimeout, last: map[string]time.Time{}}
}

// Fetch requests the wanted artifacts from each project's hub and returns the
// outcome of every project it attempted, keyed by project UID. A nil error
// means success, including a hub that returned nothing. Projects that are
// throttled, not served by a remote producer with this recipe, or without a
// credential to send are absent. A credential that does not match its binding
// is reported and never sent. After a transport failure (anything but a hub
// status reply) the remaining projects on that hub origin are skipped for this
// call, with no outcome and no throttle.
func (f *DirectArtifactFetcher) Fetch(ctx context.Context, recipe embedding.ArtifactIdentity, wants []vector.ArtifactWant) map[string]error {
	outcomes := map[string]error{}
	if len(wants) == 0 {
		return outcomes
	}
	bindings, err := f.store.ListFederationBindings(ctx)
	if err != nil {
		slog.Warn("direct artifact fetch: list federation bindings failed", "err", err)
		return outcomes
	}
	direct := map[int64]db.FederationBinding{}
	for _, binding := range bindings {
		if binding.Role == db.FederationRoleSpoke && binding.Enabled && binding.RelayConfig == nil {
			direct[binding.ProjectID] = binding
		}
	}
	if len(direct) == 0 {
		return outcomes
	}
	byProject := map[string][]vector.ArtifactWant{}
	var order []string
	for _, want := range wants {
		if _, seen := byProject[want.ProjectUID]; !seen {
			order = append(order, want.ProjectUID)
		}
		byProject[want.ProjectUID] = append(byProject[want.ProjectUID], want)
	}
	recipe.ProjectUID, recipe.IssueUID, recipe.InputHash, recipe.ProducerInstanceUID = "", "", "", ""
	unreachable := map[string]bool{}
	for _, projectUID := range order {
		if ctx.Err() != nil {
			break
		}
		project, err := f.store.ProjectByUID(ctx, projectUID)
		if err != nil {
			slog.Warn("direct artifact fetch: project lookup failed", "project_uid", projectUID, "err", err)
			continue
		}
		if project.DeletedAt != nil {
			continue
		}
		binding, ok := direct[project.ID]
		if !ok || !f.remoteProducerMatches(project, recipe) || f.throttled(projectUID) {
			continue
		}
		origin, originErr := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
		if originErr == nil && unreachable[origin] {
			continue
		}
		credential, ok, err := bindingTransportCredential(ctx, f.store, f.credentials, projectUID, binding)
		var failure *credentialCheckError
		if errors.As(err, &failure) && failure.missing {
			continue
		}
		if err != nil {
			outcomes[projectUID] = err
			continue
		}
		if !ok {
			continue
		}
		down, err := f.fetchProject(ctx, projectUID, recipe.RecipeFingerprint, credential, byProject[projectUID])
		outcomes[projectUID] = err
		if originErr == nil && down {
			unreachable[origin] = true
		}
	}
	return outcomes
}

// remoteProducerMatches reports whether the project's readable producer is
// another instance generating with this daemon's recipe, the exact condition
// under which a direct spoke defers generation and its artifacts are usable.
func (f *DirectArtifactFetcher) remoteProducerMatches(project db.Project, recipe embedding.ArtifactIdentity) bool {
	producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
	if err != nil || producer == nil || producer.ProducerInstanceUID == f.store.InstanceUID() {
		return false
	}
	selected := producer.Recipe
	selected.ProjectUID, selected.IssueUID, selected.InputHash, selected.ProducerInstanceUID = "", "", "", ""
	return selected == recipe
}

// throttled reports whether the project was attempted within the interval.
func (f *DirectArtifactFetcher) throttled(projectUID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, ok := f.last[projectUID]
	return ok && f.now().Sub(last) < f.interval
}

func (f *DirectArtifactFetcher) markAttempt(projectUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[projectUID] = f.now()
}

func (f *DirectArtifactFetcher) fetchProject(ctx context.Context, projectUID, recipeFingerprint string, credential config.FederationCredential, wants []vector.ArtifactWant) (bool, error) {
	artifacts, ok := f.store.(db.EmbeddingArtifactStorage)
	if !ok {
		return false, errors.New("local store cannot retain embedding artifacts")
	}
	requested := map[string]string{}
	sorted := make([]string, 0, len(wants))
	for _, want := range wants {
		if _, dup := requested[want.IssueUID]; dup {
			continue
		}
		requested[want.IssueUID] = want.InputHash
		sorted = append(sorted, want.IssueUID)
	}
	// The reconciler already pages wants per project; this bound only keeps a
	// direct caller within what one request may carry.
	sort.Strings(sorted)
	issueUIDs := sorted[:min(len(sorted), maxDirectArtifactBatch)]
	for _, issueUID := range sorted[len(issueUIDs):] {
		delete(requested, issueUID)
	}
	f.markAttempt(projectUID)
	hub, err := NewClient(ctx, credential.HubURL, credential.Token, clientOptsForCredential(f.opts, credential))
	if err != nil {
		return false, errors.New("connect to hub failed")
	}
	requestCtx, cancel := context.WithTimeout(ctx, f.requestTimeout)
	defer cancel()
	fetched, err := hub.EmbeddingArtifacts(requestCtx, credential.HubProjectID, recipeFingerprint, issueUIDs)
	if err != nil {
		_, replied := errors.AsType[*HubStatusError](err)
		return !replied && ctx.Err() == nil, describeFetchError(ctx, err)
	}
	failed := 0
	var firstErr error
	for _, artifact := range fetched {
		// The hub may have edited the issue after the want was computed. Keep
		// only artifacts for the exact content requested.
		inputHash, wanted := requested[artifact.IssueUID]
		if artifact.ProjectUID != projectUID || !wanted || artifact.InputHash != inputHash {
			continue
		}
		if _, err := artifacts.RetainEmbeddingArtifact(ctx, artifact); err != nil {
			if errors.Is(err, db.ErrNotFound) || errors.Is(err, db.ErrEmbeddingArtifactMiss) {
				continue
			}
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if failed > 0 {
		return false, fmt.Errorf("retain %d fetched artifacts failed: %w", failed, firstErr)
	}
	return false, nil
}

// describeFetchError reduces a hub failure to its status and error code. The
// request path, URL and token never appear in the message.
func describeFetchError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var status *HubStatusError
	if !errors.As(err, &status) {
		return errors.New("fetch embedding artifacts from hub failed")
	}
	var envelope struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	code := ""
	if json.Unmarshal([]byte(status.Body), &envelope) == nil {
		code = envelope.Error.Code
		if code == "" {
			code = envelope.Code
		}
	}
	if code != "" {
		return fmt.Errorf("hub returned status %d (%s) for embedding artifacts", status.StatusCode, code)
	}
	return fmt.Errorf("hub returned status %d for embedding artifacts", status.StatusCode)
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// VectorUpstream fetches hub-computed vectors for one federated replica
// project. internal/federation implements it over the vectors:lookup route;
// the interface keeps this package free of that import (federation already
// imports daemon).
type VectorUpstream interface {
	LookupVectors(
		ctx context.Context,
		binding db.FederationBinding,
		projectUID string,
		request api.FederationVectorLookupRequestBody,
	) (api.FederationVectorLookupBody, error)
}

// ErrVectorUpstreamUnsupported marks a hub that predates vectors:lookup (it
// answered 404). The spoke embeds that project locally, as before.
var ErrVectorUpstreamUnsupported = errors.New("hub does not serve federated vectors")

// Replica source statuses, per project and aggregated in health.
const (
	ReplicaStatusDisabled           = "disabled"
	ReplicaStatusPending            = "pending"
	ReplicaStatusOK                 = "ok"
	ReplicaStatusUnsupported        = "unsupported"
	ReplicaStatusNoPublisher        = "no_publisher"
	ReplicaStatusGenerationMismatch = "generation_mismatch"
	ReplicaStatusUnreachable        = "unreachable"
)

const (
	replicaLookupBatch   = 64
	replicaMinBackoff    = 30 * time.Second
	replicaMaxBackoff    = 30 * time.Minute
	replicaReprobeEvery  = 30 * time.Minute
	replicaNormTolerance = 0.01
)

// ReplicaProjectHealth is one federated replica project's import state.
type ReplicaProjectHealth struct {
	ProjectUID          string `json:"project_uid"`
	Status              string `json:"status"`
	UpstreamFingerprint string `json:"upstream_fingerprint,omitempty"`
}

type replicaProject struct {
	uid                 string
	binding             db.FederationBinding
	status              string
	upstreamFingerprint string
	failures            int
	nextProbeAt         time.Time
}

type replicaDoc struct {
	projectUID string
	hash       string
	failures   int
	nextAt     time.Time
}

// replicaState is owned by the reconciler goroutine; Health reads only the
// summary publishReplicaHealth copies into r.health under r.mu.
type replicaState struct {
	projects      map[string]*replicaProject
	docs          map[string]*replicaDoc
	replicated    int64
	rejected      int64
	lastSuccessAt *time.Time
	deferred      bool
}

// hubBound reports whether a project's rows wait for hub vectors (and so are
// excluded from the provider fill). Unsupported, no-publisher, and mismatched
// hubs fall back to local embedding.
func hubBound(status string) bool {
	switch status {
	case ReplicaStatusPending, ReplicaStatusOK, ReplicaStatusUnreachable:
		return true
	}
	return false
}

func replicaBackoff(failures int) time.Duration {
	d := replicaMinBackoff
	for i := 1; i < failures && d < replicaMaxBackoff; i++ {
		d *= 2
	}
	return min(d, replicaMaxBackoff)
}

// importReplicaVectors imports hub-computed vectors for pending rows of
// federated replica projects and returns the project UIDs whose rows the
// provider fill must skip this turn. It never calls the embedding provider.
func (r *Reconciler) importReplicaVectors(ctx context.Context, key string, dims int) ([]string, error) {
	now := r.now()
	defer r.publishReplicaHealth(now)
	r.replica.deferred = false
	if err := r.refreshReplicaProjects(ctx); err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(r.replica.projects))
	for uid := range r.replica.projects {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	var excluded []string
	for _, uid := range uids {
		p := r.replica.projects[uid]
		if p == nil {
			continue
		}
		if err := r.importReplicaProject(ctx, key, dims, p, now); err != nil {
			return nil, err
		}
		if hubBound(p.status) {
			excluded = append(excluded, uid)
		}
	}
	return excluded, nil
}

// refreshReplicaProjects tracks every enabled spoke binding whose local
// project is live. Without an upstream there is nothing to import from, so
// every project embeds locally exactly as before this feature.
func (r *Reconciler) refreshReplicaProjects(ctx context.Context) error {
	if r.replica.projects == nil {
		r.replica.projects = make(map[string]*replicaProject)
		r.replica.docs = make(map[string]*replicaDoc)
	}
	seen := make(map[string]bool)
	if r.cfg.Upstream != nil {
		bindings, err := r.store.ListFederationBindings(ctx)
		if err != nil {
			return fmt.Errorf("replica import: list federation bindings: %w", err)
		}
		for _, b := range bindings {
			if !b.Enabled || b.Role != db.FederationRoleSpoke {
				continue
			}
			project, err := r.store.ProjectByID(ctx, b.ProjectID)
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("replica import: load project %d: %w", b.ProjectID, err)
			}
			if project.DeletedAt != nil {
				continue
			}
			seen[project.UID] = true
			p, ok := r.replica.projects[project.UID]
			if !ok || p == nil {
				p = &replicaProject{uid: project.UID, status: ReplicaStatusPending}
				r.replica.projects[project.UID] = p
			} else if p.binding.HubURL != b.HubURL || p.binding.HubProjectID != b.HubProjectID {
				// Rebound to another hub endpoint: forget what the old one said.
				p.status, p.upstreamFingerprint, p.failures, p.nextProbeAt = ReplicaStatusPending, "", 0, time.Time{}
			}
			p.binding = b
		}
	}
	for uid := range r.replica.projects {
		if !seen[uid] {
			delete(r.replica.projects, uid)
		}
	}
	for uid, d := range r.replica.docs {
		if !seen[d.projectUID] {
			delete(r.replica.docs, uid)
		}
	}
	return nil
}

// importReplicaProject pages through the project's pending rows, skipping
// rows still backing off at the same content hash, and looks the rest up in
// batches. A lookup that moves the project out of the ok state stops the
// pass for that project.
func (r *Reconciler) importReplicaProject(ctx context.Context, key string, dims int, p *replicaProject, now time.Time) error {
	if now.Before(p.nextProbeAt) {
		return nil
	}
	seen := make(map[string]bool)
	contacted := false
	after := ""
	for {
		pending, err := r.idx.PendingScoped(ctx, key,
			vector.PendingScope{Projects: []string{p.uid}, Include: true, AfterDoc: after}, replicaLookupBatch)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			break
		}
		after = pending[len(pending)-1].Doc
		batch := make([]vector.PendingDoc, 0, len(pending))
		for _, doc := range pending {
			seen[doc.Doc] = true
			st := r.replica.docs[doc.Doc]
			if st != nil && st.hash == vector.ContentSHA256(doc.Content) && now.Before(st.nextAt) {
				continue
			}
			batch = append(batch, doc)
		}
		if len(batch) == 0 {
			continue
		}
		contacted = true
		stillOK, err := r.lookupReplicaBatch(ctx, key, dims, p, batch, now)
		if err != nil {
			return err
		}
		if !stillOK {
			return nil
		}
	}
	if !contacted && p.status != ReplicaStatusOK {
		// Nothing pending to ask about: probe once so health reflects the hub.
		if _, err := r.lookupReplicaBatch(ctx, key, dims, p, nil, now); err != nil {
			return err
		}
	}
	for uid, d := range r.replica.docs {
		if d.projectUID == p.uid && !seen[uid] {
			delete(r.replica.docs, uid)
		}
	}
	return nil
}

// lookupReplicaBatch asks the hub for batch and saves every valid record.
// It reports whether the project is still importing (status ok).
func (r *Reconciler) lookupReplicaBatch(ctx context.Context, key string, dims int, p *replicaProject, batch []vector.PendingDoc, now time.Time) (bool, error) {
	hashes := make([]string, len(batch))
	request := api.FederationVectorLookupRequestBody{Fingerprint: key}
	for i, doc := range batch {
		hashes[i] = vector.ContentSHA256(doc.Content)
		request.Docs = append(request.Docs, api.FederationVectorLookupDoc{IssueUID: doc.Doc, ContentSHA256: hashes[i]})
	}
	resp, err := r.cfg.Upstream.LookupVectors(ctx, p.binding, p.uid, request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if errors.Is(err, ErrVectorUpstreamUnsupported) {
			p.fallBackToLocal(ReplicaStatusUnsupported, "", now)
			r.clearReplicaDocState(p.uid)
			return false, nil
		}
		p.status = ReplicaStatusUnreachable
		p.failures++
		p.nextProbeAt = now.Add(replicaBackoff(p.failures))
		return false, nil
	}
	if resp.Generation == nil {
		p.fallBackToLocal(ReplicaStatusNoPublisher, "", now)
		r.clearReplicaDocState(p.uid)
		return false, nil
	}
	if resp.Generation.Fingerprint != key {
		p.fallBackToLocal(ReplicaStatusGenerationMismatch, resp.Generation.Fingerprint, now)
		r.clearReplicaDocState(p.uid)
		return false, nil
	}
	p.status, p.upstreamFingerprint, p.failures, p.nextProbeAt = ReplicaStatusOK, resp.Generation.Fingerprint, 0, time.Time{}
	records := make(map[string]api.FederationVectorRecord, len(resp.Records))
	for _, rec := range resp.Records {
		records[rec.IssueUID] = rec
	}
	for i, doc := range batch {
		rec, found := records[doc.Doc]
		switch {
		case found && rec.Status == api.FederationVectorStatusDeferred:
			r.replica.deferred = true
		case found && (rec.Status == api.FederationVectorStatusOK || rec.Status == api.FederationVectorStatusSkipped):
			vectors, err := validateReplicaRecord(rec, doc.Content, hashes[i], dims)
			if err != nil {
				r.replica.rejected++
				r.backOffReplicaDoc(doc.Doc, p.uid, hashes[i], now)
				continue
			}
			if err := r.idx.SaveImported(ctx, key, doc.Doc, doc.Revision, vectors); err != nil {
				if errors.Is(err, kitvec.ErrStale) {
					continue // edited mid-turn; the next turn looks up the new hash
				}
				return false, err
			}
			delete(r.replica.docs, doc.Doc)
			r.replica.replicated++
			at := now
			r.replica.lastSuccessAt = &at
		default:
			r.backOffReplicaDoc(doc.Doc, p.uid, hashes[i], now)
		}
	}
	return true, nil
}

func (p *replicaProject) fallBackToLocal(status, upstreamFingerprint string, now time.Time) {
	p.status, p.upstreamFingerprint, p.failures = status, upstreamFingerprint, 0
	p.nextProbeAt = now.Add(replicaReprobeEvery)
}

func (r *Reconciler) clearReplicaDocState(projectUID string) {
	for uid, doc := range r.replica.docs {
		if doc.projectUID == projectUID {
			delete(r.replica.docs, uid)
		}
	}
}

// backOffReplicaDoc schedules the next lookup for a document the hub could
// not serve. A new content hash restarts the schedule.
func (r *Reconciler) backOffReplicaDoc(uid, projectUID, hash string, now time.Time) {
	st := r.replica.docs[uid]
	if st == nil || st.hash != hash {
		st = &replicaDoc{projectUID: projectUID, hash: hash}
		r.replica.docs[uid] = st
	}
	st.failures++
	st.nextAt = now.Add(replicaBackoff(st.failures))
}

// validateReplicaRecord turns a hub record into vectors to save, or rejects
// it. Identical text split with identical chunking (the fingerprint covers
// both) must produce exactly the local chunk indices, so the chunk layout is
// checked against the local split rather than a fixed cap.
func validateReplicaRecord(rec api.FederationVectorRecord, content, hash string, dims int) ([]kitvec.ChunkVector, error) {
	if rec.ContentSHA256 != hash {
		return nil, fmt.Errorf("record content hash %q does not match local %q", rec.ContentSHA256, hash)
	}
	if rec.Status == api.FederationVectorStatusSkipped {
		if len(rec.Chunks) != 0 {
			return nil, errors.New("skipped record carries vectors")
		}
		return nil, nil
	}
	want := kitvec.Split(content, embedding.SplitOptions())
	if len(rec.Chunks) == 0 || len(rec.Chunks) != len(want) {
		return nil, fmt.Errorf("record has %d chunks, local text splits into %d", len(rec.Chunks), len(want))
	}
	out := make([]kitvec.ChunkVector, len(rec.Chunks))
	for i, c := range rec.Chunks {
		if c.Index != want[i].Index {
			return nil, fmt.Errorf("chunk %d has index %d, want %d", i, c.Index, want[i].Index)
		}
		v, err := vector.DecodeVector(c.Vector, dims)
		if err != nil {
			return nil, err
		}
		var sum float64
		for j, x := range v {
			f := float64(x)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, fmt.Errorf("chunk %d component %d is not finite", c.Index, j)
			}
			sum += f * f
		}
		if norm := math.Sqrt(sum); math.Abs(norm-1) > replicaNormTolerance {
			return nil, fmt.Errorf("chunk %d has L2 norm %.4f, want 1", c.Index, norm)
		}
		out[i] = kitvec.ChunkVector{ChunkIndex: c.Index, Vector: v}
	}
	return out, nil
}

var replicaStatusRank = map[string]int{
	ReplicaStatusOK:                 1,
	ReplicaStatusPending:            2,
	ReplicaStatusNoPublisher:        3,
	ReplicaStatusUnsupported:        4,
	ReplicaStatusGenerationMismatch: 5,
	ReplicaStatusUnreachable:        6,
}

// publishReplicaHealth copies the replica summary into r.health. The
// aggregate source_status is the most severe project status.
func (r *Reconciler) publishReplicaHealth(now time.Time) {
	projects := make([]ReplicaProjectHealth, 0, len(r.replica.projects))
	status := ReplicaStatusDisabled
	for _, p := range r.replica.projects {
		projects = append(projects, ReplicaProjectHealth{
			ProjectUID: p.uid, Status: p.status, UpstreamFingerprint: p.upstreamFingerprint,
		})
		if replicaStatusRank[p.status] > replicaStatusRank[status] {
			status = p.status
		}
	}
	slices.SortFunc(projects, func(a, b ReplicaProjectHealth) int {
		switch {
		case a.ProjectUID < b.ProjectUID:
			return -1
		case a.ProjectUID > b.ProjectUID:
			return 1
		}
		return 0
	})
	var awaiting int64
	for _, d := range r.replica.docs {
		if now.Before(d.nextAt) {
			awaiting++
		}
	}
	source := "provider"
	if len(projects) > 0 {
		source = "replica"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.Source = source
	r.health.SourceStatus = status
	r.health.ReplicaProjects = projects
	r.health.Replicated = r.replica.replicated
	r.health.Rejected = r.replica.rejected
	r.health.AwaitingUpstream = awaiting
	if r.replica.lastSuccessAt != nil {
		at := *r.replica.lastSuccessAt
		r.health.LastReplicaSuccessAt = &at
	}
}

// successDelay is how long runLeader sleeps after a successful turn: the
// periodic sweep, or sooner when a replica retry or deferred page is due.
func (r *Reconciler) successDelay() time.Duration {
	delay := r.cfg.SweepEvery
	if r.replica.deferred {
		return min(delay, r.cfg.MinBackoff)
	}
	now := r.now()
	consider := func(at time.Time) {
		if at.IsZero() {
			return
		}
		if d := max(at.Sub(now), 0); d < delay {
			delay = d
		}
	}
	for _, p := range r.replica.projects {
		consider(p.nextProbeAt)
	}
	for _, d := range r.replica.docs {
		p := r.replica.projects[d.projectUID]
		if p == nil || !hubBound(p.status) {
			continue
		}
		at := d.nextAt
		if p.nextProbeAt.After(at) {
			at = p.nextProbeAt
		}
		consider(at)
	}
	return delay
}

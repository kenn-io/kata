package dbtest

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Contract: only one immutable successful request reserves a job occurrence.
func checkCronOccurrenceClaims(t *testing.T, s db.Storage, b Backend) error {
	ctx := t.Context()
	p, err := s.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "worker", Definition: def})
	require.NoError(t, err)
	run, err := uid.New()
	require.NoError(t, err)
	in := db.CronOccurrenceClaim{ProjectID: p.ID, JobUID: job.UID, RunUID: run, DefinitionEventUID: job.DefinitionEventUID, OccurrenceKey: "2030-01-02T03:04:05Z", HolderInstanceUID: s.InstanceUID(), Actor: "worker", ExecutorLabel: new("executor-a")}
	authority, err := s.CronClaimAuthority(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, s.InstanceUID(), authority)
	got, err := s.ClaimCronOccurrence(ctx, in)
	require.NoError(t, err)
	require.False(t, got.Replayed)
	require.Equal(t, in.RunUID, got.Claim.RunUID)
	require.Equal(t, s.InstanceUID(), got.Claim.AuthorityInstanceUID)
	require.False(t, got.Claim.CreatedAt.IsZero())
	replay, err := s.ClaimCronOccurrence(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, got.Claim, replay.Claim)
	other := in
	other.RunUID, err = uid.New()
	require.NoError(t, err)
	conflict, err := s.ClaimCronOccurrence(ctx, other)
	require.ErrorIs(t, err, db.ErrCronOccurrenceClaimed)
	require.Equal(t, got.Claim, conflict.Claim)
	for name, change := range map[string]func(*db.CronOccurrenceClaim){
		"key":    func(c *db.CronOccurrenceClaim) { c.OccurrenceKey = "another" },
		"holder": func(c *db.CronOccurrenceClaim) { c.HolderInstanceUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV" },
		"actor":  func(c *db.CronOccurrenceClaim) { c.Actor = "other" },
		"label":  func(c *db.CronOccurrenceClaim) { c.ExecutorLabel = new("executor-b") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := in
			change(&changed)
			_, e := s.ClaimCronOccurrence(t.Context(), changed)
			require.ErrorIs(t, e, db.ErrCronClaimIdentity)
		})
	}
	edited, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: def, ExpectedEventUID: job.DefinitionEventUID, Actor: "worker"})
	require.NoError(t, err)
	other.DefinitionEventUID = edited.DefinitionEventUID
	_, err = s.ClaimCronOccurrence(ctx, other)
	require.ErrorIs(t, err, db.ErrCronOccurrenceClaimed, "editing a job must not reopen its occurrence")
	_, _, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: def, ExpectedEventUID: edited.DefinitionEventUID, Actor: "worker", Deleted: true})
	require.NoError(t, err)
	replay, err = s.ClaimCronOccurrence(ctx, in)
	require.NoError(t, err, "retained grant survives job tombstone")
	require.True(t, replay.Replayed)
	var exported []db.CronClaimExport
	for claim, e := range s.ExportCronClaims(ctx, db.ExportFilter{ProjectID: &p.ID, IncludeDeleted: true}) {
		require.NoError(t, e)
		exported = append(exported, claim)
	}
	require.Len(t, exported, 1)
	require.Equal(t, got.Claim, db.CronOccurrenceClaim(exported[0]))
	read, err := s.CronOccurrenceClaim(ctx, p.ID, in.RunUID)
	require.NoError(t, err)
	require.Equal(t, got.Claim, read)
	second, err := s.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	changed := in
	changed.ProjectID = second.ID
	hidden, err := s.ClaimCronOccurrence(ctx, changed)
	require.ErrorIs(t, err, db.ErrCronClaimIdentity)
	require.Empty(t, hidden.Claim.RunUID, "cross-project conflict must not disclose a claim")
	_, err = s.CronOccurrenceClaim(ctx, second.ID, in.RunUID)
	require.ErrorIs(t, err, db.ErrNotFound)
	for _, key := range []string{"", strings.Repeat("é", 513), string([]byte{0xff})} {
		changed := in
		changed.OccurrenceKey = key
		_, err = s.ClaimCronOccurrence(ctx, changed)
		require.ErrorIs(t, err, cron.ErrInvalid)
	}
	unknown := in
	unknown.RunUID, err = uid.New()
	require.NoError(t, err)
	unknown.JobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	unknown.OccurrenceKey = "unseen"
	_, err = s.ClaimCronOccurrence(ctx, unknown)
	require.ErrorIs(t, err, db.ErrCronClaimPending)
	_, err = s.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: p.ID, TargetProjectID: second.ID, Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported, "merging cannot erase a retained exclusion")
	_, err = s.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: p.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported, "adoption cannot discard root claims")
	// An authority transition must not discard retained root exclusions.
	_, err = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
	require.Error(t, err, "a root with retained claims cannot become a replica")
	replica, err := s.CreateProject(ctx, "replica-project")
	require.NoError(t, err)
	replicaJob, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: replica.ID, Name: "Review", Actor: "worker", Definition: def})
	require.NoError(t, err)
	_, err = s.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: replica.ID, HubProjectUID: replica.UID, Enabled: true})
	require.NoError(t, err)
	candidate := in
	candidate.ProjectID = replica.ID
	candidate.JobUID = replicaJob.UID
	candidate.DefinitionEventUID = replicaJob.DefinitionEventUID
	candidate.RunUID, err = uid.New()
	require.NoError(t, err)
	_, err = s.ClaimCronOccurrence(ctx, candidate)
	require.ErrorIs(t, err, db.ErrCronClaimPending)
	_, err = s.LeaveFederationReplica(ctx, replica.ID)
	require.NoError(t, err)
	_, err = s.EnableProjectFederation(ctx, replica.ID, "worker")
	require.NoError(t, err)
	_, err = s.ClaimCronOccurrence(ctx, candidate)
	require.ErrorIs(t, err, db.ErrCronClaimPending, "detached replica must not become a new arbitration root")
	// Two independent store handles cannot both win the same key.
	fresh, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: second.ID, Name: "Concurrent", Actor: "worker", Definition: def})
	require.NoError(t, err)
	stores := []db.Storage{s, b.OpenSame(t, s)}
	outcomes := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, store := range stores {
		request := in
		request.ProjectID = second.ID
		request.JobUID = fresh.UID
		request.DefinitionEventUID = fresh.DefinitionEventUID
		request.RunUID, err = uid.New()
		require.NoError(t, err)
		wg.Go(func() { <-start; _, e := store.ClaimCronOccurrence(ctx, request); outcomes <- e })
	}
	close(start)
	wg.Wait()
	close(outcomes)
	wins, losses := 0, 0
	for e := range outcomes {
		if e == nil {
			wins++
		} else {
			require.True(t, errors.Is(e, db.ErrCronOccurrenceClaimed), "%v", e)
			losses++
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 1, losses)

	_, _, err = s.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "worker", Force: true})
	require.NoError(t, err)
	_, err = s.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "worker"})
	require.NoError(t, err, "explicit project purge removes root reservations")
	_, err = s.CronOccurrenceClaim(ctx, p.ID, in.RunUID)
	require.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

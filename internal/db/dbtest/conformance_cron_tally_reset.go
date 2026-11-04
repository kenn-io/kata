package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// oldPeerMetadata reuses the ordinary metadata fold, which retains the
// pre-tally project's recursive from/to semantics. Map project events to issue
// metadata events so reserved tally keys receive no absolute-value override.
// Values and clocks remain intact, including integer counts above 2^53.
func oldPeerMetadata(t *testing.T, events []db.RemoteEvent) map[string]jsontext.Value {
	t.Helper()
	legacy := []db.FoldEvent{}
	for _, event := range remoteEventsToFold(events) {
		event.IssueUID = "legacy-project"
		switch event.Type {
		case "project.federation_enabled":
			// Only metadata belongs in the synthetic issue baseline.
			payload, err := json.Marshal(map[string]jsontext.Value{"metadata": db.PayloadMap(event.Payload)["metadata"]})
			require.NoError(t, err)
			event.Type, event.Payload = "issue.snapshot", payload
		case "project.metadata_updated":
			event.Type = "issue.metadata_updated"
		default:
			continue
		}
		legacy = append(legacy, event)
	}
	folded := db.FoldEvents(legacy)
	current := map[string]jsontext.Value{}
	if raw := folded.IssueMetadata["legacy-project"]; len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &current))
	}
	return current
}

// Contract: a root reset replaces the complete reserved namespace even on a
// peer whose ordinary metadata fold still recursively merges object values.
// SQL deliberately supplies divergent retained history and current inventory:
// a matching update event could mask a missing authoritative reset overlay.
func checkCronTallyResetInventory(t *testing.T, store db.Storage) error {
	// The legacy fold applies from/to changes rather than overwriting unchanged
	// nested fields. Compare the oracle with the ordinary metadata fold first.
	ordinary := []db.RemoteEvent{
		{ProjectUID: "example-project", Type: "project.federation_enabled", HLCPhysicalMS: 1, Payload: []byte(`{"metadata":{"profile":{"label":"root","count":1,"removed":true}}}`)},
		{ProjectUID: "example-project", Type: "project.metadata_updated", HLCPhysicalMS: 2, Payload: []byte(`{"diff":{"profile":{"from":{"label":"old","count":1,"removed":true},"to":{"label":"old","count":2}}}}`)},
	}
	folded := db.FoldEvents(remoteEventsToFold(ordinary))
	legacy, err := json.Marshal(oldPeerMetadata(t, ordinary))
	require.NoError(t, err)
	require.JSONEq(t, `{"profile":{"label":"root","count":2}}`, string(folded.ProjectMetadata["example-project"]))
	require.JSONEq(t, string(folded.ProjectMetadata["example-project"]), string(legacy))
	ctx := t.Context()
	for i, mode := range []string{"removed kind", "removed key", "empty inventory"} {
		fixture := newRelayLinkBoundaryFixture(t, store, fmt.Sprintf("tally-reset-%d", i))
		a, err := uid.New()
		require.NoError(t, err)
		b, err := uid.New()
		require.NoError(t, err)
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		entry := db.CronTallyEntry{Kind: "job", UID: a, CoverageStartedAt: at, LastPrunedAt: at, OldestCreatedAt: at, NewestCreatedAt: at, Counts: db.CronTallyCounts{Succeeded: 9007199254740993}}
		workflow := entry
		workflow.Kind = "workflow"
		removed := entry
		removed.UID = b
		prior := map[string]any{db.CronTallyPrefix + a: db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": entry, "workflow": workflow}}, db.CronTallyPrefix + b: db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": removed}}}
		fields := map[string]any{"ordinary": "current"}
		if mode != "empty inventory" {
			groups := map[string]db.CronTallyEntry{"job": entry}
			if mode == "removed key" {
				groups["workflow"] = workflow
			}
			fields[db.CronTallyPrefix+a] = db.CronTallyValue{Version: 1, Groups: groups}
		}
		diff := map[string]any{}
		for k, v := range prior {
			diff[k] = map[string]any{"from": nil, "to": v}
		}
		payload, err := json.Marshal(map[string]any{"diff": diff})
		require.NoError(t, err)
		original := newRemoteEvent(t, fixture.project, nil, "project.metadata_updated", "operator", store.InstanceUID(), at.UnixMilli(), payload)
		q := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, err = q.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, original.EventUID, original.OriginInstanceUID, fixture.project.ID, fixture.project.Name, original.Type, original.Actor, string(original.Payload), original.HLCPhysicalMS, original.HLCCounter, original.ContentHash, original.CreatedAt.Format(db.EventTimestampFormat))
		require.NoError(t, err)
		current, err := json.Marshal(fields)
		require.NoError(t, err)
		_, err = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(current), fixture.project.ID)
		require.NoError(t, err)
		peer, err := uid.New()
		require.NoError(t, err)
		grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion})
		require.NoError(t, err)
		drainRelayGrant(t, store, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch)
		measured, err := store.(db.RelayCheckpointMeasurer).RelayCheckpointSize(ctx, fixture.project.ID)
		require.NoError(t, err)
		checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, fixture.signer)
		require.NoError(t, err)
		require.GreaterOrEqual(t, measured.Bytes, db.RootResetSnapshotBytes(checkpoint.Snapshot), "status estimate must bound the extra clear event and its receipt")
		events, _, err := db.DecodeRootResetPayload(mustRootPin(t, store, fixture.project.UID), checkpoint.Snapshot)
		require.NoError(t, err)
		found := false
		for _, event := range events {
			if event.EventUID == original.EventUID {
				require.Equal(t, original, event)
				found = true
			}
		}
		require.True(t, found, "source history remains byte-identical")
		upgraded := db.FoldEvents(remoteEventsToFold(events))
		require.JSONEq(t, string(current), string(upgraded.ProjectMetadata[fixture.project.UID]))
		old, err := json.Marshal(oldPeerMetadata(t, events))
		require.NoError(t, err)
		require.JSONEq(t, string(current), string(old), mode)
	}
	return nil
}

func remoteEventsToFold(events []db.RemoteEvent) []db.FoldEvent {
	out := make([]db.FoldEvent, 0, len(events))
	for _, e := range events {
		out = append(out, db.FoldEvent{UID: e.EventUID, ProjectUID: e.ProjectUID, OriginInstanceUID: e.OriginInstanceUID, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, Type: e.Type, Actor: e.Actor, Payload: e.Payload})
	}
	return out
}

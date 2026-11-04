package dbtest

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// oldPeerMetadata deliberately models the pre-tally recursive object fold.
// It does not call Kata's current fold/merge helpers: nested omitted members
// survive unless an ordinary whole-key deletion precedes the replacement.
func oldPeerMetadata(t *testing.T, events []db.RemoteEvent) map[string]jsontext.Value {
	t.Helper()
	slices.SortFunc(events, func(a, b db.RemoteEvent) int {
		for _, v := range []int{cmp.Compare(a.HLCPhysicalMS, b.HLCPhysicalMS), cmp.Compare(a.HLCCounter, b.HLCCounter), cmp.Compare(a.OriginInstanceUID, b.OriginInstanceUID), cmp.Compare(a.EventUID, b.EventUID)} {
			if v != 0 {
				return v
			}
		}
		return 0
	})
	current := map[string]jsontext.Value{}
	var merge func(jsontext.Value, jsontext.Value) jsontext.Value
	merge = func(previous, next jsontext.Value) jsontext.Value {
		a, b := map[string]jsontext.Value{}, map[string]jsontext.Value{}
		if json.Unmarshal(previous, &a) != nil || json.Unmarshal(next, &b) != nil || a == nil || b == nil {
			return next
		}
		for k, v := range b {
			if string(v) == "null" {
				delete(a, k)
			} else {
				a[k] = merge(a[k], v)
			}
		}
		raw, err := json.Marshal(a)
		require.NoError(t, err)
		return raw
	}
	for _, event := range events {
		if event.Type != "project.metadata_updated" {
			continue
		}
		var payload struct {
			Diff map[string]struct {
				To jsontext.Value `json:"to"`
			} `json:"diff"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		for key, change := range payload.Diff {
			if string(change.To) == "null" {
				delete(current, key)
			} else {
				current[key] = merge(current[key], change.To)
			}
		}
	}
	return current
}

// Contract: a root reset replaces the complete reserved namespace even on a
// peer whose ordinary metadata fold still recursively merges object values.
// SQL deliberately supplies divergent retained history and current inventory:
// a matching update event could mask a missing authoritative reset overlay.
func checkCronTallyResetInventory(t *testing.T, store db.Storage) error {
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

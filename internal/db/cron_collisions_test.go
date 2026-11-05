package db

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
	_ "modernc.org/sqlite"
)

// The collision read's lookahead prefix is bounded in bytes before Scan,
// including metadata containing multibyte Unicode. This is test-local DDL.
func TestCronCollisionParentMetadataByteReadBound(t *testing.T) {
	pool, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	_, err = pool.ExecContext(t.Context(), `CREATE TABLE issues(id INTEGER, uid TEXT, project_id INTEGER, status TEXT, owner TEXT, author TEXT, metadata TEXT, revision INTEGER, deleted_at TEXT)`)
	require.NoError(t, err)
	metadata := `{"blob":"` + strings.Repeat("😀", 128) + `"}`
	parentUID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	_, err = pool.ExecContext(t.Context(), `INSERT INTO issues VALUES(1,$1,1,'open','','operator',$2,1,NULL)`, parentUID, metadata)
	require.NoError(t, err)
	tx, err := pool.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	parent, _, err := (CronSQL{}).recurringParentRead(t.Context(), tx, 1, parentUID, false, 128)
	require.NoError(t, err)
	require.LessOrEqual(t, len(parent.Metadata), 128)
}

// Admission owns binary name/UID order and self-exclusion. Changing physical
// insertion order must leave the exact admitted peer sequence unchanged.
func TestCronCollisionsOrderProperty(t *testing.T) {
	pool, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	_, err = pool.ExecContext(t.Context(), `CREATE TABLE cron_jobs(uid TEXT, project_id INTEGER, name TEXT, definition_json TEXT, deleted_at TEXT)`)
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "cron", Cron: "0 * * * *"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "latest"}
	require.NoError(t, definition.Validate())
	raw, err := json.Marshal(definition)
	require.NoError(t, err)
	candidate := cron.CollisionJob{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "Candidate"}
	hegel.Test(t, func(ht *hegel.T) {
		tx, err := pool.BeginTx(t.Context(), nil)
		require.NoError(ht, err)
		defer func() { require.NoError(ht, tx.Rollback()) }()
		size := hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1)))
		peers := make([]cron.CollisionJob, min(size, 12))
		for i := range peers {
			var name string
			if i%2 == 0 {
				drawn := hegel.Draw(ht, hegel.Text())
				name = strings.TrimSpace(strings.ToValidUTF8(drawn[:min(len(drawn), 256)], ""))
				if name == "" {
					name = "Peer"
				}
			} else {
				name = peers[i-1].Name
			}
			peers[i] = cron.CollisionJob{UID: fmt.Sprintf("%026d", len(peers)-i), Name: name, Def: definition}
		}
		want := slices.Clone(peers)
		sort.Slice(want, func(i, j int) bool {
			if want[i].Name != want[j].Name {
				return want[i].Name < want[j].Name
			}
			return want[i].UID < want[j].UID
		})
		for range 2 {
			_, err := tx.ExecContext(t.Context(), `DELETE FROM cron_jobs`)
			require.NoError(ht, err)
			for _, peer := range append(slices.Clone(peers), candidate) {
				_, err = tx.ExecContext(t.Context(), `INSERT INTO cron_jobs VALUES($1,1,$2,$3,NULL)`, peer.UID, peer.Name, string(raw))
				require.NoError(ht, err)
			}
			admitted, skipped, limited, err := (CronSQL{}).collisionPeers(t.Context(), tx, CronValidationInput{ProjectID: 1, UID: candidate.UID}, definition)
			require.NoError(ht, err)
			require.Zero(ht, skipped)
			require.False(ht, limited)
			require.Equal(ht, want, admitted)
			slices.Reverse(peers)
		}
	}, hegel.WithTestCases(60), hegel.WithDatabase(t.TempDir()))
}

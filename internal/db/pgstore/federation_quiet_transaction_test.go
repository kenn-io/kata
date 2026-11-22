package pgstore_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv/sqlcount"
)

func TestQuietFederationPollTransactions(t *testing.T) {
	t.Cleanup(pgstore.CountStatementsForTest())
	dsn, cleanup := conformanceTemplate.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.OpenWithConfig(t.Context(), dsn, conformanceConfig())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	quiet, err := s.CreateProject(t.Context(), "quiet")
	require.NoError(t, err)
	_, err = s.EnableProjectFederation(t.Context(), quiet.ID, "tester")
	require.NoError(t, err)
	busy, err := s.CreateProject(t.Context(), "busy")
	require.NoError(t, err)
	cursor, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	_, _, err = s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: busy.ID, Title: "busy", Author: "tester"})
	require.NoError(t, err)
	newest, err := s.MaxEventID(t.Context())
	require.NoError(t, err)
	var count sqlcount.Counter
	ctx := count.Context(t.Context())
	in := db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: cursor, Limit: 100}
	page, err := s.ReadFederation(ctx, in)
	require.NoError(t, err)
	require.Empty(t, page.Events)
	require.Equal(t, newest, page.NextAfterID)
	require.Zero(t, count.Begins())
	_, _, err = s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: quiet.ID, Title: "visible", Author: "tester"})
	require.NoError(t, err)
	count.Reset()
	in.AfterID = newest
	page, err = s.ReadFederation(ctx, in)
	require.NoError(t, err)
	require.NotEmpty(t, page.Events)
	require.Positive(t, count.Begins())
}

func TestQuietFederationPollMoveDuringProof(t *testing.T) {
	t.Cleanup(pgstore.CountStatementsForTest())
	dsn, cleanup := conformanceTemplate.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	reader, err := pgstore.OpenWithConfig(t.Context(), dsn, conformanceConfig())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	writer, err := pgstore.OpenWithConfig(t.Context(), dsn, conformanceConfig())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	ctx := t.Context()
	quiet, err := writer.CreateProject(ctx, "quiet")
	require.NoError(t, err)
	busy, err := writer.CreateProject(ctx, "busy")
	require.NoError(t, err)
	_, err = writer.EnableProjectFederation(ctx, quiet.ID, "tester")
	require.NoError(t, err)
	_, err = writer.EnableProjectFederation(ctx, busy.ID, "tester")
	require.NoError(t, err)
	issue, _, err := writer.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "owned", Author: "tester"})
	require.NoError(t, err)
	cursor, err := writer.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = writer.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES($1,$2,$3,$4,$5,'issue.updated','tester','{}',1,0,$6)`, "01M80000000000000000000001", writer.InstanceUID(), busy.ID, busy.Name, issue.UID, strings.Repeat("a", 64))
	require.NoError(t, err)
	foreign, err := writer.MaxEventID(ctx)
	require.NoError(t, err)
	var count sqlcount.Counter
	var move db.MoveIssueProjectOut
	var moveErr error
	count.AfterNext = func() {
		move, moveErr = writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	}
	page, err := reader.ReadFederation(count.Context(ctx), db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: cursor, Limit: 100})
	require.NoError(t, moveErr)
	require.NoError(t, err)
	require.NotEmpty(t, page.Events, "a proof that saw the candidate must not advance past it with an empty page")
	require.Greater(t, move.EventID, foreign)
}

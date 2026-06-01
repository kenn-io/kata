package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestAPITokensTableExists(t *testing.T) {
	d := openTestDB(t)
	var n int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('api_tokens')
		WHERE name IN ('id','token_hash','actor','name','created_at','last_used_at','revoked_at')
	`).Scan(&n))
	assert.Equal(t, 7, n)
}

func TestHashTokenSHA256Hex(t *testing.T) {
	got := sqlitestore.HashTokenForTest("secret")
	assert.Equal(t, "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b", got)
	assert.Len(t, got, 64)
}

func TestSystemProjectInitializedAndHidden(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	sys, err := d.SystemProject(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.SystemProjectName, sys.Name)
	assert.Equal(t, db.SystemProjectUID, sys.UID)

	projects, err := d.ListProjects(ctx)
	require.NoError(t, err)
	for _, p := range projects {
		assert.NotEqual(t, db.SystemProjectName, p.Name)
	}
}

func TestEnsureSystemProjectRejectsConflictingSentinelUID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	_, err := d.ExecContext(ctx, `DROP TRIGGER trg_projects_uid_immutable`)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx,
		`UPDATE projects SET uid = ? WHERE name = ?`,
		"01HZNQ7VFPK1XGD8R5MABCD4EX", db.SystemProjectName)
	require.NoError(t, err)

	err = d.EnsureSystemProject(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), db.SystemProjectName)
}

func TestBatchProjectStatsHidesSystemProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	sys, err := d.SystemProject(ctx)
	require.NoError(t, err)

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)
	assert.NotContains(t, stats, sys.ID)
}

func TestCreateAPITokenStoresHashAndEmitsEvent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	name := "laptop"

	tok, evt, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		Name:           &name,
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	assert.Equal(t, "wesm", tok.Actor)
	require.NotNil(t, tok.Name)
	assert.Equal(t, "laptop", *tok.Name)
	assert.Nil(t, tok.LastUsedAt)
	assert.Nil(t, tok.RevokedAt)

	var storedHash string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT token_hash FROM api_tokens WHERE id = ?`, tok.ID).Scan(&storedHash))
	assert.Equal(t, sqlitestore.HashTokenForTest("secret-token"), storedHash)
	assert.NotContains(t, storedHash, "secret-token")

	assert.Equal(t, "token.created", evt.Type)
	assert.Equal(t, db.BootstrapActor, evt.Actor)
	assert.Equal(t, db.SystemProjectName, evt.ProjectName)
	payload := unmarshalPayload[map[string]any](t, evt.Payload)
	assert.Equal(t, storedHash, payload["token_hash"])
	assert.Equal(t, "wesm", payload["target_actor"])
	assert.Equal(t, "laptop", payload["name"])
	assert.NotContains(t, evt.Payload, "secret-token")
}

func TestRevokeAPITokenSetsRevokedAtAndEmitsEvent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	name := "laptop"
	tok, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		Name:           &name,
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	revoked, evt, err := d.RevokeAPIToken(ctx, tok.ID, db.BootstrapActor)
	require.NoError(t, err)
	require.NotNil(t, revoked.RevokedAt)
	assert.Equal(t, "token.revoked", evt.Type)
	assert.Equal(t, db.BootstrapActor, evt.Actor)
	payload := unmarshalPayload[map[string]any](t, evt.Payload)
	assert.InDelta(t, float64(tok.ID), payload["token_id"].(float64), 0)
	assert.Equal(t, "wesm", payload["target_actor"])
	assert.Equal(t, "laptop", payload["name"])
	assert.NotContains(t, evt.Payload, "secret-token")
	assert.NotContains(t, evt.Payload, sqlitestore.HashTokenForTest("secret-token"))
}

func TestRevokeAPITokenAlreadyRevokedDoesNotEmitDuplicateEvent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tok, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = d.RevokeAPIToken(ctx, tok.ID, db.BootstrapActor)
	require.NoError(t, err)

	_, _, err = d.RevokeAPIToken(ctx, tok.ID, db.BootstrapActor)
	assert.ErrorIs(t, err, db.ErrNotFound)

	var n int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE type = 'token.revoked'`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestResolveAPITokenReturnsActiveToken(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tok, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	got, err := d.ResolveAPIToken(ctx, "secret-token")
	require.NoError(t, err)
	assert.Equal(t, tok.ID, got.ID)
	assert.Equal(t, "wesm", got.Actor)
}

func TestResolveAPITokenLazilyUpdatesLastUsedAt(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tok, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	got, err := d.ResolveAPIToken(ctx, "secret-token")
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt)

	tokens, err := d.ListAPITokens(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	require.NotNil(t, tokens[0].LastUsedAt)
	assert.Equal(t, tok.ID, tokens[0].ID)

	firstUsed := *tokens[0].LastUsedAt
	got, err = d.ResolveAPIToken(ctx, "secret-token")
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt)
	assert.Equal(t, firstUsed, *got.LastUsedAt)
}

func TestResolveAPITokenReturnsTokenWhenLastUsedUpdateFails(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tok, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		CREATE TRIGGER fail_api_token_last_used
		BEFORE UPDATE OF last_used_at ON api_tokens
		BEGIN
			SELECT RAISE(FAIL, 'last_used_at unavailable');
		END`)
	require.NoError(t, err)

	got, err := d.ResolveAPIToken(ctx, "secret-token")
	require.NoError(t, err)
	assert.Equal(t, tok.ID, got.ID)
	assert.Nil(t, got.LastUsedAt)
}

func TestResolveAPITokenRejectsRevokedToken(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tok, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = d.RevokeAPIToken(ctx, tok.ID, db.BootstrapActor)
	require.NoError(t, err)

	_, err = d.ResolveAPIToken(ctx, "secret-token")
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestListAPITokensIncludesRevokedAndHidesHash(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tok, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = d.RevokeAPIToken(ctx, tok.ID, db.BootstrapActor)
	require.NoError(t, err)

	tokens, err := d.ListAPITokens(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	assert.Empty(t, tokens[0].TokenHash)
	assert.NotNil(t, tokens[0].RevokedAt)
}

func TestCreateAPITokenRejectsReservedBootstrapActorCaseInsensitive(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	for _, actor := range []string{"bootstrap", "Bootstrap", " BOOTSTRAP "} {
		_, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
			PlaintextToken: "secret-token",
			Actor:          actor,
			AdminActor:     db.BootstrapActor,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reserved")
	}
}

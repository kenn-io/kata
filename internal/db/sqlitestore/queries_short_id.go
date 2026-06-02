package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/shortid"
)

// assignShortID returns the smallest-length short_id (>= shortid.MinLength)
// derived from ulid that does not collide with any existing issue in the
// project, including soft-deleted rows or purge_log tombstones. Soft-deleted
// issues retain their short_ids so `kata restore` is stable; purged issues
// leave a purge_log tombstone so external refs (`kata#abc4` in a commit
// message) can't be silently re-targeted at a later-created issue whose ULID
// happens to suffix-match.
func assignShortID(ctx context.Context, tx *sql.Tx, projectID int64, ulid string) (string, error) {
	return assignShortIDIn(ctx, tx, []int64{projectID}, ulid, shortid.MinLength)
}

// resolveShortID picks the short_id for a new issue. When override is empty,
// it auto-extends via assignShortID. When override is non-empty (JSONL import
// path; spec §8.1), it validates that override is a syntactically valid
// short_id and equals the lowercased suffix of ulid at its own length — the
// same invariant the schema CHECK enforces. The override is used verbatim
// without a collision check: the UNIQUE(project_id, short_id) index will
// surface any duplicate at INSERT time.
func resolveShortID(ctx context.Context, tx *sql.Tx, projectID int64, ulid, override string) (string, error) {
	if override == "" {
		s, err := assignShortID(ctx, tx, projectID, ulid)
		if err != nil {
			return "", fmt.Errorf("assign short_id: %w", err)
		}
		return s, nil
	}
	if !shortid.Valid(override) {
		return "", fmt.Errorf("invalid short_id override %q", override)
	}
	derived, err := shortid.Derive(ulid, len(override))
	if err != nil {
		return "", fmt.Errorf("validate short_id override %q against uid %q: %w", override, ulid, err)
	}
	if override != derived {
		return "", fmt.Errorf("short_id override %q does not match uid %q suffix at length %d (expected %q)",
			override, ulid, len(override), derived)
	}
	return override, nil
}

// assignShortIDIn is the generalized form of assignShortID that returns the
// smallest-length short_id (>= minLength) derived from ulid that doesn't
// collide with any issue in the given project set. Rows whose uid matches
// ulid are excluded from the collision count, so re-keying an issue in place
// doesn't count its own row as a self-collision. minLength must be in
// [shortid.MinLength, shortid.MaxLength]; the merge rekey path passes
// len(currentShortID)+1 so a rekey can only extend, never shorten (spec
// §5.2). Single-project creates pass shortid.MinLength via assignShortID.
func assignShortIDIn(ctx context.Context, tx *sql.Tx, projectIDs []int64, ulid string, minLength int) (string, error) {
	if len(projectIDs) == 0 {
		return "", fmt.Errorf("assignShortIDIn: empty projectIDs")
	}
	if minLength < shortid.MinLength || minLength > shortid.MaxLength {
		return "", fmt.Errorf("assignShortIDIn: minLength %d out of range [%d, %d]",
			minLength, shortid.MinLength, shortid.MaxLength)
	}
	placeholders, args := projectIDPlaceholders(projectIDs)
	// The collision check runs across both live issues and purge_log
	// tombstones. v7→v8 cutover entries have short_id IS NULL and so cannot
	// match candidate (which is non-NULL); no special-case needed.
	query := `SELECT (
		(SELECT COUNT(*) FROM issues
		   WHERE project_id IN (` + placeholders + `)
		     AND short_id = ?
		     AND uid <> ?)
		+
		(SELECT COUNT(*) FROM purge_log
		   WHERE project_id IN (` + placeholders + `)
		     AND short_id = ?)
	)`
	for length := minLength; length <= shortid.MaxLength; length++ {
		candidate, err := shortid.Derive(ulid, length)
		if err != nil {
			return "", fmt.Errorf("derive short_id at length %d: %w", length, err)
		}
		queryArgs := make([]any, 0, 2*len(args)+3)
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, candidate, ulid)
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, candidate)
		var n int
		if err := tx.QueryRowContext(ctx, query, queryArgs...).Scan(&n); err != nil {
			return "", fmt.Errorf("collision check at length %d: %w", length, err)
		}
		if n == 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("short_id auto-extend exhausted for ulid %s", ulid)
}

// projectIDPlaceholders returns a comma-separated "?"-list and the matching
// args slice for use in a SQL IN-clause.
func projectIDPlaceholders(ids []int64) (string, []any) {
	out := make([]byte, 0, 2*len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
		args = append(args, id)
	}
	return string(out), args
}

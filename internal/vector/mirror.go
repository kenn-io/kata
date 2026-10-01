package vector

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

const mirrorPageSize = 500

// RefreshMirror synchronizes issue_mirror with the canonical store: it
// upserts new/edited issues (rendering the embed recipe) and removes rows —
// plus their vectors in every generation — for issues that left the feed
// (soft-deleted, purged, or their project deleted). Dropping soft-deleted
// issues is a privacy contract: mirror content is sent to the embedding
// endpoint, and deleting an issue must stop that outbound flow. A restore
// puts the issue back in the feed, re-mirroring and re-embedding it. It
// returns the number of rows written or removed.
func (ix *Index) RefreshMirror(ctx context.Context, store db.Storage) (int, error) {
	if ix.pg != nil {
		return ix.pg.refreshMirror(ctx, store)
	}
	changed := 0
	seen := make(map[string]struct{})
	afterID := int64(0)
	for {
		page, err := store.ListIssueContent(ctx, afterID, mirrorPageSize)
		if err != nil {
			return changed, fmt.Errorf("vector: list issue content: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ic := range page {
			seen[ic.UID] = struct{}{}
			afterID = ic.ID
			res, err := ix.db.ExecContext(ctx, `
				INSERT INTO issue_mirror (issue_uid, project_uid, content, content_revision)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(issue_uid) DO UPDATE SET
				  project_uid = excluded.project_uid,
				  content = excluded.content,
				  content_revision = excluded.content_revision
				WHERE issue_mirror.content_revision != excluded.content_revision
				   OR issue_mirror.project_uid != excluded.project_uid`,
				ic.UID, ic.ProjectUID, embedding.EmbedText(ic.Title, ic.Body), ic.ContentRevision)
			if err != nil {
				return changed, fmt.Errorf("vector: upsert mirror row %s: %w", ic.UID, err)
			}
			if n, err := res.RowsAffected(); err == nil {
				changed += int(n)
			}
		}
	}

	stale, err := ix.mirrorUIDsNotIn(ctx, seen)
	if err != nil {
		return changed, err
	}
	for _, uid := range stale {
		if err := ix.store.DeleteVectors(ctx, uid); err != nil {
			return changed, fmt.Errorf("vector: delete vectors for %s: %w", uid, err)
		}
		if _, err := ix.db.ExecContext(ctx,
			`DELETE FROM issue_mirror WHERE issue_uid = ?`, uid); err != nil {
			return changed, fmt.Errorf("vector: delete mirror row %s: %w", uid, err)
		}
		changed++
	}
	return changed, nil
}

func (ix *Index) mirrorUIDsNotIn(ctx context.Context, seen map[string]struct{}) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT issue_uid FROM issue_mirror`)
	if err != nil {
		return nil, fmt.Errorf("vector: scan mirror uids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("vector: scan mirror uid: %w", err)
		}
		if _, ok := seen[uid]; !ok {
			out = append(out, uid)
		}
	}
	return out, rows.Err()
}

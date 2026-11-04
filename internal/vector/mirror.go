package vector

import (
	"context"
	"database/sql"
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
	known, err := loadMirrorKeys(ctx, ix.db, `SELECT issue_uid, content_revision, project_uid FROM issue_mirror`)
	if err != nil {
		return 0, fmt.Errorf("vector: scan mirror keys: %w", err)
	}
	changed, stale, err := syncMirror(ctx, store, known, func(ic db.IssueContent) (int, error) {
		res, err := ix.db.ExecContext(ctx, `
			INSERT INTO issue_mirror (issue_uid, project_uid, content, content_revision)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(issue_uid) DO UPDATE SET
			  project_uid = excluded.project_uid,
			  content = excluded.content,
			  content_revision = excluded.content_revision`,
			ic.UID, ic.ProjectUID, embedding.EmbedText(ic.Title, ic.Body), ic.ContentRevision)
		if err != nil {
			return 0, fmt.Errorf("vector: upsert mirror row %s: %w", ic.UID, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			return int(n), nil
		}
		return 0, nil
	})
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

type mirrorKey struct {
	revision   int64
	projectUID string
}

type mirrorQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// loadMirrorKeys closes the read before either backend starts writing.
func loadMirrorKeys(ctx context.Context, queryer mirrorQuerier, query string) (map[string]mirrorKey, error) {
	rows, err := queryer.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	known := make(map[string]mirrorKey)
	for rows.Next() {
		var uid string
		var key mirrorKey
		if err := rows.Scan(&uid, &key.revision, &key.projectUID); err != nil {
			return nil, fmt.Errorf("scan mirror key: %w", err)
		}
		known[uid] = key
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close mirror keys: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mirror keys: %w", err)
	}
	return known, nil
}

// syncMirror consumes known as it pages the live feed. Leftovers are safe to
// remove only after every page has been read successfully.
func syncMirror(ctx context.Context, store db.Storage, known map[string]mirrorKey, upsert func(db.IssueContent) (int, error)) (changed int, leftovers []string, err error) {
	var afterID int64
	for {
		page, err := store.ListIssueContent(ctx, afterID, mirrorPageSize)
		if err != nil {
			return changed, nil, fmt.Errorf("vector: list issue content: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, issue := range page {
			afterID = issue.ID
			key, exists := known[issue.UID]
			delete(known, issue.UID)
			if exists && key.revision == issue.ContentRevision && key.projectUID == issue.ProjectUID {
				continue
			}
			n, err := upsert(issue)
			if err != nil {
				return changed, nil, err
			}
			changed += n
		}
	}
	for uid := range known {
		leftovers = append(leftovers, uid)
	}
	return changed, leftovers, nil
}

package vector

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
)

// ArtifactWant names one document whose embedding the index still lacks for a
// generation, with the hash of the exact text it would embed.
type ArtifactWant struct{ IssueUID, ProjectUID, InputHash string }

// PendingArtifactWants lists the first rotated page of documents pending for
// generation key. See PendingArtifactWantsAfterProject for the paging contract.
func (ix *Index) PendingArtifactWants(ctx context.Context, key string, perProject, scanLimit int, after map[string]string) ([]ArtifactWant, error) {
	return ix.PendingArtifactWantsAfterProject(ctx, key, perProject, scanLimit, after, "")
}

// PendingArtifactWantsAfterProject lists documents pending for generation key,
// grouped by project. It returns at most perProject wants per project, at most
// scanLimit wants total, and rotates project selection bytewise after
// afterProject before wrapping to the first project. Within each selected
// project, it returns the next page of pending issue UIDs after after[project];
// when that cursor has no later pending issues, the project wraps to its first
// pending issue. Project pages are never truncated by the global bound, so
// every selected project can advance independently. A document is pending
// under the same predicate the fill worker uses: its newest stamp for key is
// missing or does not match its current content revision. perProject <= 0 or
// scanLimit <= 0 returns an empty slice without querying. On PostgreSQL the
// read runs on the reconciler lease session, like ImportArtifact. key must
// name an existing generation.
func (ix *Index) PendingArtifactWantsAfterProject(ctx context.Context, key string, perProject, scanLimit int, after map[string]string, afterProject string) ([]ArtifactWant, error) {
	wants := []ArtifactWant{}
	if perProject <= 0 || scanLimit <= 0 {
		return wants, nil
	}
	pageSize := min(perProject, scanLimit)
	projectLimit := max(1, scanLimit/pageSize)
	var rows *sql.Rows
	var err error
	if ix.pg != nil {
		cursorProjects, cursorAfter := make([]string, 0, len(after)), make([]string, 0, len(after))
		for project, uid := range after {
			cursorProjects, cursorAfter = append(cursorProjects, project), append(cursorAfter, uid)
		}
		executor, leaseErr := ix.pg.reconcilerExecutor()
		if leaseErr != nil {
			return nil, leaseErr
		}
		rows, err = executor.QueryContext(ctx, `
			WITH pending AS MATERIALIZED (
			  SELECT m.project_uid, m.issue_uid, m.content
			  FROM issue_vector_mirror m
			  LEFT JOIN issue_vector_stamps s
			    ON s.gen_key = $1 AND s.issue_uid = m.issue_uid AND s.revision = m.content_revision
			  WHERE s.issue_uid IS NULL
			), project_candidates AS (
			  SELECT DISTINCT project_uid FROM pending
			), selected_projects AS (
			  SELECT project_uid,
			         ROW_NUMBER() OVER (
			           ORDER BY CASE WHEN project_uid COLLATE "C" > $6 COLLATE "C" THEN 0 ELSE 1 END,
			                    project_uid COLLATE "C"
			         ) AS project_order
			  FROM project_candidates
			  ORDER BY CASE WHEN project_uid COLLATE "C" > $6 COLLATE "C" THEN 0 ELSE 1 END,
			           project_uid COLLATE "C"
			  LIMIT $3
			), ranked AS (
			  SELECT p.project_uid, p.issue_uid, p.content, selected.project_order,
			         ROW_NUMBER() OVER (PARTITION BY p.project_uid COLLATE "C" ORDER BY p.issue_uid COLLATE "C") AS page_rank
			  FROM pending p
			  JOIN selected_projects selected ON selected.project_uid = p.project_uid COLLATE "C"
			  LEFT JOIN unnest($4::text[], $5::text[]) AS c(project_uid, after_uid)
			    ON c.project_uid = p.project_uid COLLATE "C"
			  WHERE c.project_uid IS NULL
			     OR NOT EXISTS (
			       SELECT 1 FROM pending later
			       WHERE later.project_uid = p.project_uid COLLATE "C"
			         AND later.issue_uid COLLATE "C" > c.after_uid COLLATE "C"
			     )
			     OR p.issue_uid COLLATE "C" > c.after_uid COLLATE "C"
			)
			SELECT project_uid, issue_uid, content
			FROM ranked
			WHERE page_rank <= $2
			ORDER BY project_order, issue_uid COLLATE "C"`, key, pageSize, projectLimit, cursorProjects, cursorAfter, afterProject)
	} else {
		cursors, marshalErr := json.Marshal(after)
		if marshalErr != nil {
			return nil, fmt.Errorf("vector: encode artifact want cursors: %w", marshalErr)
		}
		rows, err = ix.db.QueryContext(ctx, `
			WITH pending AS MATERIALIZED (
			  SELECT d.project_uid, d.issue_uid, d.content
			  FROM issue_mirror d
			  LEFT JOIN `+vectorsPrefix+`_stamps stamp
			    ON stamp.ordinal = (SELECT ordinal FROM `+vectorsPrefix+`_generations WHERE gen_key = ?)
			   AND stamp.doc_key = d.issue_uid
			  WHERE NOT (d.embed_gen IS NOT NULL AND stamp.doc_key IS NOT NULL
			             AND (d.content_revision IS stamp.revision))
			), project_candidates AS (
			  SELECT DISTINCT project_uid FROM pending
			), selected_projects AS (
			  SELECT project_uid,
			         ROW_NUMBER() OVER (
			           ORDER BY CASE WHEN project_uid > ? THEN 0 ELSE 1 END, project_uid
			         ) AS project_order
			  FROM project_candidates
			  ORDER BY CASE WHEN project_uid > ? THEN 0 ELSE 1 END, project_uid
			  LIMIT ?
			), ranked AS (
			  SELECT p.project_uid, p.issue_uid, p.content, selected.project_order,
			         ROW_NUMBER() OVER (PARTITION BY p.project_uid ORDER BY p.issue_uid) AS page_rank
			  FROM pending p
			  JOIN selected_projects selected ON selected.project_uid = p.project_uid
			  LEFT JOIN json_each(?) c ON c.key = p.project_uid
			  WHERE c.key IS NULL
			     OR NOT EXISTS (
			       SELECT 1 FROM pending later
			       WHERE later.project_uid = p.project_uid AND later.issue_uid > c.value
			     )
			     OR p.issue_uid > c.value
			)
			SELECT project_uid, issue_uid, content
			FROM ranked
			WHERE page_rank <= ?
			ORDER BY project_order, issue_uid`, key, afterProject, afterProject, projectLimit, string(cursors), pageSize)
	}
	if err != nil {
		return nil, fmt.Errorf("vector: pending artifact wants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var want ArtifactWant
		var content string
		if err := rows.Scan(&want.ProjectUID, &want.IssueUID, &content); err != nil {
			return nil, fmt.Errorf("vector: scan pending artifact want: %w", err)
		}
		want.InputHash = embedding.ArtifactInputHash(content)
		wants = append(wants, want)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector: pending artifact wants: %w", err)
	}
	return wants, nil
}

// pendingDocsFor reads the documents among issueUIDs that are pending for
// generation key, under the same predicate as PendingArtifactWants and the
// fill worker, with the content and revision the stamp is checked against.
// Unknown and already covered UIDs are absent. The order is unspecified.
func (ix *Index) pendingDocsFor(ctx context.Context, key string, issueUIDs []string) ([]kitvec.Pending[string], error) {
	var rows *sql.Rows
	var err error
	if ix.pg != nil {
		executor, leaseErr := ix.pg.reconcilerExecutor()
		if leaseErr != nil {
			return nil, leaseErr
		}
		rows, err = executor.QueryContext(ctx, `
			SELECT m.issue_uid, m.content, m.content_revision
			FROM issue_vector_mirror m
			LEFT JOIN issue_vector_stamps s
			  ON s.gen_key = $1 AND s.issue_uid = m.issue_uid AND s.revision = m.content_revision
			WHERE s.issue_uid IS NULL AND m.issue_uid = ANY($2::text[])`, key, issueUIDs)
	} else {
		encoded, marshalErr := json.Marshal(issueUIDs)
		if marshalErr != nil {
			return nil, fmt.Errorf("vector: encode pending document uids: %w", marshalErr)
		}
		rows, err = ix.db.QueryContext(ctx, `
			SELECT d.issue_uid, d.content, d.content_revision
			FROM issue_mirror d
			LEFT JOIN `+vectorsPrefix+`_stamps stamp
			  ON stamp.ordinal = (SELECT ordinal FROM `+vectorsPrefix+`_generations WHERE gen_key = ?)
			 AND stamp.doc_key = d.issue_uid
			WHERE NOT (d.embed_gen IS NOT NULL AND stamp.doc_key IS NOT NULL
			           AND (d.content_revision IS stamp.revision))
			  AND d.issue_uid IN (SELECT value FROM json_each(?))`, key, string(encoded))
	}
	if err != nil {
		return nil, fmt.Errorf("vector: pending documents for uids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	docs := []kitvec.Pending[string]{}
	for rows.Next() {
		var doc kitvec.Pending[string]
		var content sql.NullString
		var revision any
		if err := rows.Scan(&doc.Doc, &content, &revision); err != nil {
			return nil, fmt.Errorf("vector: scan pending document: %w", err)
		}
		doc.Content, doc.Revision = content.String, revision
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector: pending documents for uids: %w", err)
	}
	return docs, nil
}

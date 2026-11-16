package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/fileevidence"
)

func captureFileEvidence(ctx context.Context, cfg ServerConfig, evidence []db.Evidence) error {
	count := 0
	for _, e := range evidence {
		if err := db.ValidateFileEvidenceFields(e); err != nil {
			return err
		}
		if e.Type == "file" {
			count++
		}
	}
	if count > 16 {
		return fmt.Errorf("at most 16 file evidence entries per close")
	}
	for i, e := range evidence {
		if e.Type != "file" {
			continue
		}
		if e.File == nil {
			return fmt.Errorf("file reference is required")
		}
		ref, err := cfg.FileEvidence.Capture(ctx, *e.File, cfg.DB.InstanceUID())
		if err != nil {
			return err
		}
		evidence[i].File = &ref
	}
	return nil
}

// FileEvidenceRequest selects a zero-based file-only index within a visible close event.
type FileEvidenceRequest struct {
	ProjectID  int64  `path:"project_id"`
	Ref        string `path:"ref"`
	EventUID   string `query:"event_uid,omitempty"`
	Index      int    `query:"index,omitempty" minimum:"0"`
	Historical bool   `query:"historical,omitempty"`
	Content    bool   `query:"content,omitempty"`
}

// FileEvidenceResponse wraps a retained reference and its resolution result.
type FileEvidenceResponse struct{ Body FileEvidenceBody }

// FileEvidenceBody identifies the selected receipt and file-only index.
type FileEvidenceBody struct {
	EventUID string `json:"event_uid"`
	Index    int    `json:"index"`
	fileevidence.Result
}

func registerFileEvidenceHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "readFileEvidence", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/issues/{ref}/file-evidence"}, func(ctx context.Context, in *FileEvidenceRequest) (*FileEvidenceResponse, error) {
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		var event *db.Event
		if in.EventUID == "" {
			event, err = effectiveCloseEventForShow(ctx, cfg.DB, issue)
		} else {
			query := db.EventsAfterParams{IssueUID: issue.UID, Types: []string{"issue.closed"}, Limit: 100}
			for {
				events, readErr := cfg.DB.EventsAfter(ctx, query)
				if readErr != nil {
					err = readErr
					break
				}
				for _, e := range events {
					if e.UID == in.EventUID && e.Type == "issue.closed" && e.IssueUID != nil && *e.IssueUID == issue.UID {
						selected := e
						event = &selected
					}
				}
				if event != nil || len(events) < query.Limit {
					break
				}
				query.AfterID = events[len(events)-1].ID
			}
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		canonicalOrigin := ""
		if event != nil {
			// Scoped projection redacts envelope identity; validate provenance
			// against the canonical header, never its response projection.
			canonicalOrigin = event.OriginInstanceUID
			resolved := attachResolvedIssueID(issue, []db.Event{*event})
			event = &resolved[0]
			event, err = scopedMutationEvent(ctx, cfg.DB, event)
			if err != nil {
				return nil, err
			}
		}
		missing := func() (*FileEvidenceResponse, error) {
			return nil, api.NewError(404, "file_evidence_not_found", "file evidence not found on a visible close event", "", nil)
		}
		if event == nil || in.Index < 0 {
			return missing()
		}
		var payload struct {
			Evidence []db.Evidence `json:"evidence"`
		}
		if json.Unmarshal([]byte(event.Payload), &payload) != nil {
			return nil, internalAPIError(fmt.Errorf("invalid close payload"))
		}
		files := []fileevidence.Reference{}
		for _, e := range payload.Evidence {
			if e.Type == "file" && e.File != nil {
				files = append(files, *e.File)
			}
		}
		if in.Index >= len(files) {
			return missing()
		}
		// activeIssueByRef and scopedMutationEvent register scope targets. Middleware
		// revalidates them after resolver I/O before releasing this response.
		ref := files[in.Index]
		result := fileevidence.Result{Status: "invalid", Reference: ref}
		// Capture stamps the closing daemon's identity. A replayed descriptor
		// cannot claim this daemon's local aliases on a foreign event's behalf.
		if ref.Origin == canonicalOrigin {
			result = cfg.FileEvidence.Read(ctx, ref, cfg.DB.InstanceUID(), in.Historical)
		}
		if !in.Content {
			result.Content = nil
		}
		return &FileEvidenceResponse{Body: FileEvidenceBody{EventUID: event.UID, Index: in.Index, Result: result}}, nil
	})
}

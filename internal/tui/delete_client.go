package tui

import (
	"context"
	"errors"

	"go.kenn.io/kata/pkg/client/generated"
)

// deleteAPI and restoreAPI soft-delete an issue and restore one, through the
// same daemon calls as `kata delete` and `kata restore`.
type deleteAPI interface {
	DeleteIssue(ctx context.Context, projectID int64, ref, qualifiedID, actor string) (*MutationResp, error)
}

type restoreAPI interface {
	RestoreIssue(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error)
}

// DeleteIssue soft-deletes the issue. The daemon only accepts the delete when
// X-Kata-Confirm reads "DELETE <project>#<short_id>"; qualifiedID is that
// "<project>#<short_id>".
func (c *Client) DeleteIssue(ctx context.Context, projectID int64, ref, qualifiedID, actor string) (*MutationResp, error) {
	apiClient, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	confirm := "DELETE " + qualifiedID
	wire, callErr := apiClient.DeleteIssueWithResponse(ctx, &generated.DeleteIssueRequestOptions{
		PathParams: &generated.DeleteIssuePath{ProjectID: projectID, Ref: ref},
		Body:       &generated.DeleteIssueBody{Actor: actor},
		Header:     &generated.DeleteIssueHeaders{XKataConfirm: &confirm},
	})
	if wire == nil {
		return nil, callErr
	}
	var resp MutationResp
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RestoreIssue clears a soft delete. The daemon answers changed=false when the
// issue is not deleted.
func (c *Client) RestoreIssue(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error) {
	apiClient, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := apiClient.RestoreIssueWithResponse(ctx, &generated.RestoreIssueRequestOptions{
		PathParams: &generated.RestoreIssuePath{ProjectID: projectID, Ref: ref},
		Body:       &generated.RestoreIssueBody{Actor: actor},
	})
	if wire == nil {
		return nil, callErr
	}
	var resp MutationResp
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *undoClient) DeleteIssue(ctx context.Context, projectID int64, ref, qualifiedID, actor string) (*MutationResp, error) {
	writer, ok := c.KataAPI.(deleteAPI)
	if !ok {
		return nil, errors.New("connected client cannot delete issues")
	}
	return c.edit(ctx, projectID, ref, actor, "delete", func(uid string) (*MutationResp, error) {
		return writer.DeleteIssue(ctx, projectID, uid, qualifiedID, actor)
	})
}

// undoDelete restores a deleted issue by UID. A deleted issue cannot be read
// back, so the restore is the check: the daemon answers changed=false when the
// issue is no longer deleted, which undo reports as already restored.
func (c *undoClient) undoDelete(ctx context.Context, entry undoEntry, out undoOutcome) undoOutcome {
	restorer, ok := c.KataAPI.(restoreAPI)
	if !ok {
		out.err = errors.New("connected client cannot restore issues")
		return out
	}
	out.resp, out.err = restorer.RestoreIssue(ctx, entry.projectID, entry.uid, entry.actor)
	if out.err != nil {
		out.attempt.unknown = ambiguousWriteError(out.err)
		return out
	}
	if out.resp == nil {
		out.err = errors.New("undo returned no response")
		out.attempt.unknown = true
		return out
	}
	out.changed = out.resp.Changed
	out.already = !out.changed
	return out
}

package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
)

type undoOutcome struct {
	attempt       *undoAttempt
	resp          *MutationResp
	ref           string
	changed       bool
	already       bool
	needsEvidence bool
	conflict      string
	err           error
}

func (c *undoClient) undo(ctx context.Context, entry undoEntry, evidenceRequired bool, evidence *CloseInput) undoOutcome {
	attempt, err := c.begin(ctx, false)
	if err != nil {
		return undoOutcome{err: err}
	}
	out := undoOutcome{attempt: attempt}
	instance, err := c.GetInstance(ctx)
	if err != nil {
		out.err = err
		return out
	}
	if instance.InstanceUID == "" || instance.InstanceUID != entry.instanceUID {
		out.conflict = "daemon instance changed"
		return out
	}
	if !reflect.DeepEqual(instance.Auth, entry.auth) {
		out.conflict = "daemon principal changed"
		return out
	}
	detail, err := c.GetIssueDetail(ctx, entry.projectID, entry.uid)
	if err != nil {
		out.err = err
		return out
	}
	if detail == nil || detail.Issue == nil {
		out.err = errors.New("undo preflight returned no issue")
		return out
	}
	current := detail.Issue
	out.ref = current.ShortID
	if current.QualifiedID != "" {
		out.ref = current.QualifiedID
	}
	if current.UID != entry.uid || current.ProjectID != entry.projectID ||
		(entry.projectUID != "" && current.ProjectUID != entry.projectUID) {
		out.conflict = "issue identity changed"
		return out
	}
	switch entry.kind {
	case "close", "reopen":
		if sameStatus(current, &entry.before) {
			out.already = true
			return out
		}
		if !sameStatus(current, &entry.after) || current.Revision != entry.revision {
			out.conflict = "issue status changed since the action"
			return out
		}
		if current.RecurrenceID != nil || detail.Lease != nil {
			out.conflict = "issue now has recurrence or a live lease"
			return out
		}
		if entry.kind == "reopen" && evidenceRequired && evidence == nil {
			out.needsEvidence = true
			return out
		}
		if entry.kind == "close" {
			out.resp, out.err = c.KataAPI.Reopen(ctx, entry.projectID, entry.uid, entry.actor)
		} else if evidenceRequired {
			closer, ok := c.KataAPI.(evidenceCloseAPI)
			if !ok || evidence == nil || evidence.Reason != "done" {
				out.err = errors.New("evidence-bearing done close is required")
				return out
			}
			out.resp, out.err = closer.CloseWithEvidence(ctx, entry.projectID, entry.uid, *evidence)
		} else {
			out.resp, out.err = c.KataAPI.Close(ctx, entry.projectID, entry.uid, entry.actor)
		}
	case "owner.assign":
		if equalStringPtr(current.Owner, entry.before.Owner) {
			out.already = true
			return out
		}
		if !equalStringPtr(current.Owner, entry.after.Owner) || current.Revision != entry.revision {
			out.conflict = "issue owner changed since the action"
			return out
		}
		owner := ""
		if entry.before.Owner != nil {
			owner = *entry.before.Owner
		}
		out.resp, out.err = c.KataAPI.Assign(ctx, entry.projectID, entry.uid, owner, entry.actor)
	case "priority.set":
		if equalInt64Ptr(current.Priority, entry.before.Priority) {
			out.already = true
			return out
		}
		if !equalInt64Ptr(current.Priority, entry.after.Priority) || current.Revision != entry.revision {
			out.conflict = "issue priority changed since the action"
			return out
		}
		out.resp, out.err = c.KataAPI.SetPriority(ctx, entry.projectID, entry.uid, entry.before.Priority, entry.actor)
	case "body.edit":
		if current.Body == entry.before.Body {
			out.already = true
			return out
		}
		if current.Body != entry.after.Body || current.Revision != entry.revision {
			out.conflict = "issue body changed since the action"
			return out
		}
		out.resp, out.err = c.KataAPI.EditBody(ctx, entry.projectID, entry.uid, entry.before.Body, entry.actor)
	case "label.add", "label.remove":
		present := slices.Contains(current.Labels, entry.label)
		if (entry.kind == "label.add" && !present) || (entry.kind == "label.remove" && present) {
			out.already = true
			return out
		}
		if current.Revision != entry.revision {
			out.conflict = "issue changed since the label edit"
			return out
		}
		if entry.kind == "label.add" {
			out.resp, out.err = c.KataAPI.RemoveLabel(ctx, entry.projectID, entry.uid, entry.label, entry.actor)
		} else {
			out.resp, out.err = c.KataAPI.AddLabel(ctx, entry.projectID, entry.uid, entry.label, entry.actor)
		}
	case "link.add":
		if entry.link == nil {
			out.err = errors.New("undo record has no created link")
			return out
		}
		links, err := c.ListLinks(ctx, entry.projectID, entry.uid)
		if err != nil {
			out.err = err
			return out
		}
		found, replaced := false, false
		for _, link := range links {
			if !sameLinkEndpoints(&link, entry.link) {
				continue
			}
			if link.ID == entry.link.ID {
				found = true
			} else {
				replaced = true
			}
		}
		if !found && !replaced {
			if entry.link.Type == "parent" && detail.Parent != nil {
				out.conflict = "issue parent changed since the action"
			} else {
				out.already = true
			}
			return out
		}
		if !found || replaced || current.Revision != entry.revision {
			out.conflict = "created link changed since the action"
			return out
		}
		remover, ok := c.KataAPI.(interface {
			RemoveLink(context.Context, int64, string, int64, string) (*MutationResp, error)
		})
		if !ok {
			out.err = errors.New("connected client cannot remove links")
			return out
		}
		out.resp, out.err = remover.RemoveLink(ctx, entry.projectID, entry.uid, entry.link.ID, entry.actor)
	default:
		out.err = fmt.Errorf("unsupported undo action %q", entry.kind)
		return out
	}
	if out.err != nil {
		attempt.unknown = ambiguousWriteError(out.err)
		return out
	}
	if out.resp == nil {
		out.err = errors.New("undo returned no response")
		attempt.unknown = true
		return out
	}
	out.changed = out.resp.Changed
	if !out.changed {
		out.already, out.err = c.restoredAfterNoop(ctx, entry)
		if out.err == nil && !out.already {
			out.err = errors.New("undo returned no change; issue still needs correction")
		}
	}
	return out
}

func (c *undoClient) restoredAfterNoop(ctx context.Context, entry undoEntry) (bool, error) {
	detail, err := c.GetIssueDetail(ctx, entry.projectID, entry.uid)
	if err != nil {
		return false, err
	}
	if detail == nil || detail.Issue == nil || detail.Issue.UID != entry.uid {
		return false, errors.New("undo readback returned a different issue")
	}
	current := detail.Issue
	switch entry.kind {
	case "close", "reopen":
		return sameStatus(current, &entry.before), nil
	case "owner.assign":
		return equalStringPtr(current.Owner, entry.before.Owner), nil
	case "priority.set":
		return equalInt64Ptr(current.Priority, entry.before.Priority), nil
	case "body.edit":
		return current.Body == entry.before.Body, nil
	case "label.add":
		return !slices.Contains(current.Labels, entry.label), nil
	case "label.remove":
		return slices.Contains(current.Labels, entry.label), nil
	case "link.add":
		links, err := c.ListLinks(ctx, entry.projectID, entry.uid)
		if err != nil {
			return false, err
		}
		for _, link := range links {
			if sameLinkEndpoints(&link, entry.link) {
				return false, nil
			}
		}
		return entry.link != nil && (entry.link.Type != "parent" || detail.Parent == nil), nil
	}
	return false, fmt.Errorf("unsupported undo action %q", entry.kind)
}

func ambiguousWriteError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
		return false
	}
	return err != nil
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameLinkEndpoints(a, b *LinkEntry) bool {
	return a != nil && b != nil && a.Type == b.Type && a.From.UID == b.From.UID && a.To.UID == b.To.UID
}

func sameStatus(a, b *Issue) bool {
	if a == nil || b == nil || a.Status != b.Status {
		return false
	}
	if a.ClosedReason == nil || b.ClosedReason == nil {
		return a.ClosedReason == nil && b.ClosedReason == nil
	}
	return *a.ClosedReason == *b.ClosedReason
}

var _ KataAPI = (*undoClient)(nil)

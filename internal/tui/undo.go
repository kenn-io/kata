package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const undoLimit = 20

// undoHistory belongs to one connected TUI session. Entries are ordered by
// completed writes, oldest first.
type undoHistory struct {
	entries  []undoEntry
	boundary string
	nextID   uint64
}

func (h *undoHistory) push(entry undoEntry) {
	h.nextID++
	entry.id = h.nextID
	h.entries = append(h.entries, entry)
	if len(h.entries) > undoLimit {
		h.entries = h.entries[len(h.entries)-undoLimit:]
	}
}

func (h *undoHistory) clear(reason string) {
	h.entries = nil
	h.boundary = reason
}

func (m Model) recordUndoAttempt(mut mutationDoneMsg) Model {
	if mut.resp == nil || mut.resp.undo == nil {
		return m
	}
	attempt := mut.resp.undo
	attempt.complete()
	if m.api != attempt.client || m.staleConnMsg(mut.connGen) {
		return m
	}
	switch {
	case attempt.unknown:
		m.advanceMutationEpoch()
		m.undoHistory.clear("previous action's outcome is unknown")
	case attempt.boundary != "":
		m.advanceMutationEpoch()
		m.undoHistory.clear(attempt.boundary)
	case mut.err == nil && attempt.entry != nil:
		m.advanceMutationEpoch()
		m.undoHistory.push(*attempt.entry)
	}
	return m
}

type undoEntry struct {
	id          uint64
	kind        string
	uid         string
	projectID   int64
	projectUID  string
	actor       string
	instanceUID string
	auth        AuthInfo
	before      Issue
	after       Issue
	revision    int64
	link        *LinkEntry
	label       string
}

type undoDoneMsg struct {
	connGen uint64
	entryID uint64
	formGen int64
	outcome undoOutcome
}

func (m Model) startUndo() (Model, tea.Cmd) {
	if m.undoInFlight {
		return m.undoNotice("undo already in progress", toastInfo)
	}
	if len(m.undoHistory.entries) == 0 {
		message := "nothing to undo"
		if m.undoHistory.boundary != "" {
			message = "nothing to undo: " + m.undoHistory.boundary
		}
		return m.undoNotice(message, toastInfo)
	}
	client, ok := m.api.(*undoClient)
	if !ok {
		return m.undoNotice("undo unavailable for this connection", toastError)
	}
	if !m.mutationAllowed(true) {
		return m.undoNotice("undo unavailable with current permissions", toastError)
	}
	entry := m.undoHistory.entries[len(m.undoHistory.entries)-1]
	m.undoInFlight = true
	connGen := m.connGen
	evidenceRequired := m.closeRequiresEvidence
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return undoDoneMsg{connGen: connGen, entryID: entry.id,
			outcome: client.undo(ctx, entry, evidenceRequired, nil)}
	}
}

func (m Model) handleUndoDone(msg undoDoneMsg) (Model, tea.Cmd) {
	if msg.outcome.attempt != nil {
		msg.outcome.attempt.complete()
	}
	if m.staleConnMsg(msg.connGen) {
		return m, nil
	}
	m.undoInFlight = false
	formMatches := msg.formGen != 0 && m.input.kind == inputCloseForm && m.input.formGen == msg.formGen && m.undoCloseEntryID == msg.entryID
	if len(m.undoHistory.entries) == 0 || m.undoHistory.entries[len(m.undoHistory.entries)-1].id != msg.entryID {
		return m, nil
	}
	out := msg.outcome
	entry := m.undoHistory.entries[len(m.undoHistory.entries)-1]
	target := m.undoTarget(entry, out)
	if out.conflict == "daemon principal changed" || out.conflict == "daemon instance changed" {
		m.undoHistory.clear(out.conflict)
		if formMatches {
			m.input = inputState{}
			m.undoCloseEntryID = 0
		}
		return m.undoNotice("cannot undo "+target+": "+out.conflict, toastError)
	}
	if formMatches {
		m.input.saving = false
		if out.conflict != "" {
			m.input.err = "cannot undo: " + out.conflict
			return m, nil
		}
		if out.err != nil && (out.attempt == nil || !out.attempt.unknown) {
			m.input.err = "undo failed: " + out.err.Error()
			return m, nil
		}
		if out.changed || out.already || (out.attempt != nil && out.attempt.unknown) {
			m.input = inputState{}
			m.undoCloseEntryID = 0
		}
	}
	switch {
	case out.needsEvidence:
		return m.openUndoCloseForm(), nil
	case out.conflict != "":
		return m.undoNotice("cannot undo "+target+": "+out.conflict, toastError)
	case out.attempt != nil && out.attempt.unknown:
		m.advanceMutationEpoch()
		m.undoHistory.clear("undo outcome is unknown")
		m, refresh := m.refreshAfterUndo(entry, nil)
		m, notice := m.undoNotice("undo outcome unknown; refreshed issue state", toastError)
		return m, combineCmds(notice, refresh)
	case out.err != nil:
		return m.undoNotice("undo failed for "+target+": "+out.err.Error(), toastError)
	case out.already:
		m.advanceMutationEpoch()
		m.undoHistory.entries = m.undoHistory.entries[:len(m.undoHistory.entries)-1]
		m, refresh := m.refreshAfterUndo(entry, nil)
		m, notice := m.undoNotice(target+" already restored; no change made", toastInfo)
		return m, combineCmds(notice, refresh)
	case out.changed:
		m.advanceMutationEpoch()
		m.undoHistory.entries = m.undoHistory.entries[:len(m.undoHistory.entries)-1]
		m.rebaseUndoRevisions(entry, out.resp)
		m, refresh := m.refreshAfterUndo(entry, out.resp)
		m, notice := m.undoNotice("undid "+entry.kind+" on "+target, toastInfo)
		return m, combineCmds(notice, refresh)
	default:
		return m.undoNotice("undo made no change", toastInfo)
	}
}

func (m Model) undoTarget(entry undoEntry, out undoOutcome) string {
	ref := out.ref
	if ref == "" {
		ref = entry.before.ShortID
	}
	if ref == "" {
		ref = entry.uid
	}
	if entry.before.QualifiedID != "" && out.ref == "" {
		return entry.before.QualifiedID
	}
	if project := m.projectsByID[entry.projectID]; project != "" && ref != entry.uid && !strings.Contains(ref, "#") {
		return project + "#" + ref
	}
	if ref != entry.uid && !strings.Contains(ref, "#") {
		return "#" + ref
	}
	return ref
}

func (m Model) refreshAfterUndo(entry undoEntry, resp *MutationResp) (Model, tea.Cmd) {
	if m.cache != nil {
		m.cache.markStale()
	}
	cmds := []tea.Cmd{m.fetchInitial()}
	if m.detail.issue != nil && (m.detail.issue.UID == entry.uid ||
		(entry.link != nil && (m.detail.issue.UID == entry.link.From.UID || m.detail.issue.UID == entry.link.To.UID))) {
		if m.detail.issue.UID == entry.uid && resp != nil && resp.Issue != nil {
			m.detail.issue = mergeUndoIssue(m.detail.issue, resp.Issue, entry)
		}
		floor := nextDetailFetchRequestSeq.Load() + 1
		m.detail.fetchSeq.issue = max(m.detail.fetchSeq.issue, floor)
		m.detail.fetchSeq.comments = max(m.detail.fetchSeq.comments, floor)
		m.detail.fetchSeq.events = max(m.detail.fetchSeq.events, floor)
		m.detail.fetchSeq.links = max(m.detail.fetchSeq.links, floor)
		cmds = append(cmds, m.detail.refetch(m.api))
	}
	if (entry.kind == "label.add" || entry.kind == "label.remove") && m.projectLabels != nil {
		if _, exists := m.projectLabels.byProject[entry.projectID]; exists {
			var labelCmd tea.Cmd
			m, labelCmd = m.dispatchLabelFetch(entry.projectID)
			cmds = append(cmds, labelCmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func mergeUndoIssue(current, updated *Issue, entry undoEntry) *Issue {
	merged := *current
	merged.Revision = updated.Revision
	merged.UpdatedAt = updated.UpdatedAt
	switch entry.kind {
	case "close", "reopen":
		merged.Status, merged.ClosedReason, merged.ClosedAt = updated.Status, updated.ClosedReason, updated.ClosedAt
	case "owner.assign":
		merged.Owner = updated.Owner
	case "priority.set":
		merged.Priority = updated.Priority
	case "body.edit":
		merged.Body = updated.Body
	case "label.add":
		merged.Labels = slices.DeleteFunc(slices.Clone(merged.Labels), func(label string) bool { return label == entry.label })
	case "label.remove":
		if !slices.Contains(merged.Labels, entry.label) {
			merged.Labels = append(slices.Clone(merged.Labels), entry.label)
			slices.Sort(merged.Labels)
		}
	}
	return &merged
}

func (m Model) undoNotice(message string, level toastLevel) (Model, tea.Cmd) {
	m.toast = &toast{text: message, level: level, expiresAt: m.toastNow().Add(toastNoBindingTTL)}
	return m, toastExpireCmd(toastNoBindingTTL)
}

func (m Model) openUndoCloseForm() Model {
	entry := m.undoHistory.entries[len(m.undoHistory.entries)-1]
	m = m.openCloseForm(formTarget{projectID: entry.projectID, issueShortID: entry.uid, origin: "undo"})
	m.undoCloseEntryID = entry.id
	m.input.title = "undo reopen — complete as done"
	for i := range m.input.fields {
		if m.input.fields[i].id == fieldCloseReason {
			m.input.fields[i].radio.choices = []string{"done"}
			break
		}
	}
	return m
}

func (m Model) dispatchUndoEvidenceClose(in CloseInput) (Model, tea.Cmd) {
	if len(m.undoHistory.entries) == 0 || m.undoHistory.entries[len(m.undoHistory.entries)-1].id != m.undoCloseEntryID {
		m.input.err = "undo action is no longer available"
		return m, nil
	}
	client, ok := m.api.(*undoClient)
	if !ok {
		m.input.err = "undo unavailable for this connection"
		return m, nil
	}
	entry := m.undoHistory.entries[len(m.undoHistory.entries)-1]
	in.Actor = entry.actor
	in.Reason = "done"
	m.input.saving = true
	m.input.err = ""
	m.undoInFlight = true
	connGen, formGen := m.connGen, m.input.formGen
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return undoDoneMsg{connGen: connGen, entryID: entry.id, formGen: formGen,
			outcome: client.undo(ctx, entry, true, &in)}
	}
}

func (m *Model) rebaseUndoRevisions(entry undoEntry, resp *MutationResp) {
	if resp == nil || resp.Issue == nil {
		return
	}
	wantRevision := entry.revision
	if entry.kind == "close" || entry.kind == "reopen" || entry.kind == "owner.assign" {
		wantRevision++
	}
	if resp.Issue.Revision != wantRevision {
		return
	}
	for i := range m.undoHistory.entries {
		older := &m.undoHistory.entries[i]
		if older.instanceUID == entry.instanceUID && older.projectID == entry.projectID && older.uid == entry.uid && older.revision == entry.before.Revision {
			older.revision = resp.Issue.Revision
		}
	}
}

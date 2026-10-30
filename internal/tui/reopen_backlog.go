package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronProgressPrefix = "cron.progress."

// reopenBacklogAPI reports how many recurring children reopening an issue
// would deliver. Clients that cannot answer simply reopen without a prompt.
type reopenBacklogAPI interface {
	ReopenBacklog(ctx context.Context, projectID int64, ref string) (int, error)
}

// ReopenBacklog sums, over every native recurring job whose progress is stored
// on the issue, the children the daemon would create once the issue is open
// again. Jobs that cannot be read or planned count as zero: the figure is a
// courtesy and must never block a reopen.
func (c *Client) ReopenBacklog(ctx context.Context, projectID int64, ref string) (int, error) {
	apiClient, err := c.generatedClient()
	if err != nil {
		return 0, err
	}
	wire, callErr := apiClient.GetIssueMetadataWithResponse(ctx, &generated.GetIssueMetadataRequestOptions{
		PathParams: &generated.GetIssueMetadataPath{ProjectID: projectID, Ref: ref},
	})
	if wire == nil {
		return 0, callErr
	}
	var body struct {
		Issue struct {
			Metadata map[string]jsontext.Value `json:"metadata"`
		} `json:"issue"`
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &body); err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(body.Issue.Metadata))
	for key := range body.Issue.Metadata {
		if strings.HasPrefix(key, cronProgressPrefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	now := time.Now().UTC()
	total := 0
	for _, key := range keys {
		var progress cron.RecurringProgress
		if json.Unmarshal(body.Issue.Metadata[key], &progress) != nil {
			continue
		}
		job, err := c.recurringJob(ctx, apiClient, projectID, strings.TrimPrefix(key, cronProgressPrefix))
		if err != nil {
			continue
		}
		count, err := cron.RecurringBacklog(job.Definition.Native(), job.DefinitionEventUID, progress, now)
		if err != nil {
			continue
		}
		total += count
	}
	return total, nil
}

func (*Client) recurringJob(ctx context.Context, apiClient *generated.Client, projectID int64, uid string) (api.CronJob, error) {
	wire, callErr := apiClient.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{
		PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: uid},
	})
	if wire == nil {
		return api.CronJob{}, callErr
	}
	var body struct {
		Job api.CronJob `json:"job"`
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &body); err != nil {
		return api.CronJob{}, err
	}
	if body.Job.DeletedAt != nil {
		return api.CronJob{}, errors.New("cron job is deleted")
	}
	return body.Job, nil
}

// backlogChecker finds the optional backlog reader behind the undo wrapper.
func backlogChecker(apiClient any) (reopenBacklogAPI, bool) {
	if undo, ok := apiClient.(*undoClient); ok {
		apiClient = undo.KataAPI
	}
	checker, ok := apiClient.(reopenBacklogAPI)
	return checker, ok
}

// reopenPrompt is the pending confirmation for a reopen that would deliver a
// backlog. undoEntryID is non-zero when the reopen is the undo of a close.
type reopenPrompt struct {
	target      formTarget
	ref         string
	count       int
	undoEntryID uint64
}

// reopenBacklogMsg carries the backlog count for a reopen the user asked for.
type reopenBacklogMsg struct {
	connGen uint64
	target  formTarget
	count   int
	err     error
}

// routeReopenKey checks the backlog before an `r` reopen. It leaves the key to
// the views when the connected client cannot report a backlog.
func (m Model) routeReopenKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.Reopen.matches(msg) {
		return m, nil, false
	}
	checker, ok := backlogChecker(m.api)
	if !ok {
		return m, nil, false
	}
	target, ok := m.activeCloseTarget()
	if !ok {
		return m, nil, true
	}
	connGen := m.connGen
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		count, err := checker.ReopenBacklog(ctx, target.projectID, target.issueShortID)
		return reopenBacklogMsg{connGen: connGen, target: target, count: count, err: err}
	}, true
}

func (m Model) handleReopenBacklog(msg reopenBacklogMsg) (Model, tea.Cmd) {
	if m.staleConnMsg(msg.connGen) {
		return m, nil
	}
	if current, ok := m.activeCloseTarget(); !ok || current != msg.target {
		return m, nil
	}
	if msg.err == nil && msg.count > 1 {
		m.reopenPrompt = &reopenPrompt{target: msg.target, ref: "#" + msg.target.issueShortID, count: msg.count}
		m.modal = modalReopenBacklog
		return m, nil
	}
	return m, m.dispatchReopen(msg.target)
}

func (m Model) dispatchReopen(target formTarget) tea.Cmd {
	apiClient, actor := m.api, m.list.actor
	if target.origin == "detail" {
		actor = m.detail.actor
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := apiClient.Reopen(ctx, target.projectID, target.issueShortID, actor)
		return mutationDoneMsg{origin: target.origin, gen: target.detailGen, kind: "reopen", resp: resp, err: err}
	}
}

// confirmReopenBacklog proceeds with the reopen the user accepted.
func (m Model) confirmReopenBacklog() (Model, tea.Cmd) {
	prompt := m.reopenPrompt
	m.modal, m.reopenPrompt = modalNone, nil
	if prompt == nil {
		return m, nil
	}
	if prompt.undoEntryID == 0 {
		return m, m.dispatchReopen(prompt.target)
	}
	return m.dispatchConfirmedUndo(prompt.undoEntryID)
}

func (m Model) cancelReopenBacklog() Model {
	m.modal, m.reopenPrompt = modalNone, nil
	return m
}

func (m Model) confirmModalView() string {
	if m.modal == modalReopenBacklog {
		return renderReopenBacklogModal(m.reopenPrompt)
	}
	return renderConfirmModal(m.modal)
}

func renderReopenBacklogModal(prompt *reopenPrompt) string {
	if prompt == nil {
		return ""
	}
	body := strings.Join([]string{
		fmt.Sprintf("Reopen %s?", sanitizeForLine(prompt.ref)),
		"",
		fmt.Sprintf("Reopening delivers %d recurring child", prompt.count),
		"issues that came due while it was closed.",
		"",
		"To change how missed runs are handled,",
		"use `kata cron job update`.",
		"",
		"[Y] Reopen    [N] Cancel",
	}, "\n")
	return modalBoxStyle.Render(body)
}

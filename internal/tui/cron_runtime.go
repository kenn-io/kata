package tui

import (
	"context"
	"fmt"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronRuntimeRefreshInterval = time.Minute

type cronRuntimePoll struct {
	active, out, scheduled bool
	epoch, seq             uint64
}
type cronRuntimeTickMsg struct{ conn, scope, epoch uint64 }
type cronRuntimeLoadedMsg struct {
	conn, scope, epoch, seq uint64
	inventory               *cron.RuntimeInventory
	err                     error
}

// cronRuntimeSync owns one polling chain per visible cron-view lifetime.
func (m Model) cronRuntimeSync(refresh bool) (Model, tea.Cmd) {
	active := m.view == viewCron && !m.scope.allProjects && m.scope.projectID != 0
	p := &m.cron.runtimePoll
	if !active {
		if p.active {
			p.epoch++
			p.active = false
			p.out = false
			p.scheduled = false
			m.cron.runtimes = nil
		}
		return m, nil
	}
	if !p.active {
		p.active = true
		p.epoch++
		p.out = false
		p.scheduled = false
		m.cron.runtimes = nil
		refresh = true
	}
	if !refresh || p.out {
		return m, nil
	}
	p.out = true
	p.seq++
	return m, m.fetchCronRuntimes()
}
func (m Model) fetchCronRuntimes() tea.Cmd {
	reader, id := m.api, m.scope.projectID
	msg := cronRuntimeLoadedMsg{conn: m.connGen, scope: m.scopeGen, epoch: m.cron.runtimePoll.epoch, seq: m.cron.runtimePoll.seq}
	return func() tea.Msg {
		// Bubble Tea supplies no context to commands; this finite read owns its budget.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		msg.inventory, msg.err = reader.ListCronRuntimes(ctx, id)
		return msg
	}
}
func (m Model) scheduleCronRuntimeRefresh() tea.Cmd {
	msg := cronRuntimeTickMsg{conn: m.connGen, scope: m.scopeGen, epoch: m.cron.runtimePoll.epoch}
	return tea.Tick(cronRuntimeRefreshInterval, func(time.Time) tea.Msg { return msg })
}
func (m Model) cronRuntimeCurrent(conn, scope, epoch uint64) bool {
	return m.view == viewCron && m.cron.runtimePoll.active && conn == m.connGen && scope == m.scopeGen && epoch == m.cron.runtimePoll.epoch
}
func (m Model) cronRuntimeLoaded(msg cronRuntimeLoadedMsg) (Model, tea.Cmd) {
	if !m.cronRuntimeCurrent(msg.conn, msg.scope, msg.epoch) || msg.seq != m.cron.runtimePoll.seq {
		return m, nil
	}
	m.cron.runtimePoll.out = false
	m.cron.runtimes = msg.inventory
	if msg.err != nil {
		m.cron.runtimes = nil
	}
	if m.cron.runtimePoll.scheduled {
		return m, nil
	}
	m.cron.runtimePoll.scheduled = true
	return m, m.scheduleCronRuntimeRefresh()
}
func (m Model) cronRuntimeTick(msg cronRuntimeTickMsg) (Model, tea.Cmd) {
	if !m.cronRuntimeCurrent(msg.conn, msg.scope, msg.epoch) {
		return m, nil
	}
	m.cron.runtimePoll.scheduled = false
	return m.cronRuntimeSync(true)
}

// cronRuntimeBanner describes this daemon's evidence, never execution eligibility.
func cronRuntimeBanner(inventory *cron.RuntimeInventory, jobs []generated.CronJob, jobsAvailable bool) (string, bool) {
	if inventory == nil || inventory.Inventory.State != "ready" {
		return "", false
	}
	noLive := false
	unmatched := map[string]bool{}
	for _, w := range inventory.Warnings {
		if w.Code == "no_live_runtime" {
			noLive = true
		}
		if (w.Code == "pin_unmatched" || w.Code == "adapter_unmatched") && w.JobUID != "" {
			unmatched[w.JobUID] = true
		}
	}
	if noLive {
		text := "No live runtime advertised to this daemon"
		if jobsAvailable {
			n := 0
			for _, j := range jobs {
				if j.DeletedAt == nil && j.Definition.Enabled && j.Definition.Action.Kind == "execute" {
					n++
				}
			}
			text += fmt.Sprintf("; %d enabled execute jobs", n)
		}
		return text + ".", true
	}
	if len(unmatched) > 0 {
		if !jobsAvailable {
			return "Enabled execute jobs have no matching live advertisement here. See kata cron runtime list.", true
		}
		return fmt.Sprintf("%d enabled execute jobs have no matching live advertisement here. See kata cron runtime list.", len(unmatched)), true
	}
	return "", false
}
func cronRuntimeJobMessages(inventory *cron.RuntimeInventory, id string, enabledExecute bool) []string {
	if inventory == nil || inventory.Inventory.State != "ready" {
		return nil
	}
	messages := []string{}
	for _, w := range inventory.Warnings {
		if w.JobUID == id || w.Code == "no_live_runtime" && enabledExecute {
			messages = append(messages, sanitizeForLine(w.Message))
		}
	}
	slices.Sort(messages)
	return slices.Compact(messages)
}

// ListCronRuntimes is optional evidence; callers decide how to handle older peers.
func (c *Client) ListCronRuntimes(ctx context.Context, projectID int64) (*cron.RuntimeInventory, error) {
	client, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	wire, callErr := client.ListCronRuntimesWithResponse(ctx, &generated.ListCronRuntimesRequestOptions{PathParams: &generated.ListCronRuntimesPath{ProjectID: projectID}})
	if wire == nil {
		return nil, callErr
	}
	var inventory cron.RuntimeInventory
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, &inventory); err != nil {
		return nil, err
	}
	return &inventory, nil
}

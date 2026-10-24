package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Read ownership is separate from mutation and navigation identity. Snapshots
// keep presentation state; only a live detail pane retains a batch.
type detailReadBatch struct {
	cancel    context.CancelFunc
	floor     uint64
	remaining uint8
	canceled  bool
	pending   bool
	dirty     bool
}

func (b *detailReadBatch) stop() {
	if b != nil {
		b.canceled = true
		if b.cancel != nil {
			b.cancel()
		}
	}
}

func (dm *detailModel) cancelReads() { dm.reads.stop() }

func (dm *detailModel) fetchDetail(api detailAPI, ref string) tea.Cmd {
	if api == nil {
		return nil
	}
	dm.cancelReads()
	ctx, cancel := context.WithCancel(context.Background())
	if dm.reads == nil {
		dm.reads = &detailReadBatch{}
	}
	*dm.reads = detailReadBatch{cancel: cancel, floor: nextDetailFetchRequestSeq.Add(1), remaining: 15}
	dm.readRef = ref
	return tea.Batch(
		fetchIssue(api, dm.scopePID, ref, dm.gen, ctx),
		fetchComments(api, dm.scopePID, ref, dm.gen, ctx),
		fetchEvents(api, dm.scopePID, ref, dm.gen, ctx),
		fetchLinks(api, dm.scopePID, ref, dm.gen, ctx),
	)
}

func detailReadResult(msg tea.Msg) (gen int64, seq uint64, component uint8) {
	switch msg := msg.(type) {
	case detailFetchedMsg:
		return msg.gen, msg.requestSeq, 1
	case commentsFetchedMsg:
		return msg.gen, msg.requestSeq, 2
	case eventsFetchedMsg:
		return msg.gen, msg.requestSeq, 4
	case linksFetchedMsg:
		return msg.gen, msg.requestSeq, 8
	default:
		return 0, 0, 0
	}
}

func (dm detailModel) readResultCurrent(msg tea.Msg) bool {
	gen, seq, component := detailReadResult(msg)
	if component == 0 {
		return true
	}
	if gen != dm.gen {
		return false
	}
	return seq == 0 || dm.reads == nil || (!dm.reads.canceled && seq >= dm.reads.floor)
}

func (dm detailModel) acceptReadResult(msg tea.Msg) bool {
	if !dm.readResultCurrent(msg) {
		return false
	}
	_, seq, component := detailReadResult(msg)
	if dm.reads != nil && seq != 0 {
		dm.reads.remaining &^= component
	}
	return true
}

// A live refresh tick belongs to one dispatch era, never a restored snapshot.
type detailRefreshTickMsg struct {
	owner   *detailReadBatch
	floor   uint64
	gen     int64
	connGen uint64
}

func (m *Model) scheduleDetailRefresh() tea.Cmd {
	if m.detail.reads == nil {
		m.detail.reads = &detailReadBatch{}
	}
	b := m.detail.reads
	// A hidden pane keeps the invalidation until it is visible again.
	if !m.detailRefreshEligible() || (!b.canceled && b.remaining != 0) {
		b.dirty = true
		return nil
	}
	if b.pending {
		return nil
	}
	b.pending = true
	tick := detailRefreshTickMsg{owner: b, floor: b.floor, gen: m.detail.gen, connGen: m.connGen}
	return tea.Tick(detailFollowDebounce, func(time.Time) tea.Msg { return tick })
}

func (m Model) handleDetailRefreshTick(msg detailRefreshTickMsg) (Model, tea.Cmd) {
	b := m.detail.reads
	if b == nil || b.canceled || b != msg.owner || b.floor != msg.floor || m.detail.gen != msg.gen || m.connGen != msg.connGen || !b.pending {
		return m, nil
	}
	b.pending = false
	b.dirty = true
	cmd := m.finishDetailRefresh()
	return m, cmd
}

// finishDetailRefresh consumes a retained invalidation once no reads are in
// flight and the pane is visible. Update runs it after every message, so
// returning from a full-screen view such as the daemon selector refreshes a
// detail that changed behind it.
func (m *Model) finishDetailRefresh() tea.Cmd {
	b := m.detail.reads
	if b == nil || b.canceled || b.remaining != 0 || !b.dirty || !m.detailRefreshEligible() {
		return nil
	}
	return m.refetchOpenDetail()
}

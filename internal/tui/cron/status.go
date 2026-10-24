package cron

import "time"

const statusLifetime = 5 * time.Second

func (m *model) notify(kind statusKind, text string) {
	m.notice = statusNotice{Text: text, Kind: kind, Until: m.now().Add(statusLifetime)}
}

func (m *model) clearStatus() {
	m.notice = statusNotice{}
}

// Reuse the existing refresh tick so a transient notice adds no idle timer.
func (m *model) expireStatus() {
	if !m.notice.Until.IsZero() && !m.now().Before(m.notice.Until) {
		m.clearStatus()
	}
}

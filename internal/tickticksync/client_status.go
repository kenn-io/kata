package tickticksync

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/issuesync"
)

// StatusSession delivers only completion, with admission after shared pacing.
type StatusSession interface {
	ReadStatus(context.Context, string) (issuesync.StatusObservation, error)
	WriteStatus(context.Context, string, string, func() error) (issuesync.StatusObservation, error)
}

func statusObservation(t Task, at time.Time) (issuesync.StatusObservation, error) {
	if t.Status == nil || (*t.Status != 0 && *t.Status != 2) || t.Kind == "NOTE" {
		return issuesync.StatusObservation{}, blocked("TickTick task is abandoned or unavailable")
	}
	raw := taskStatusRaw(t)
	obs := issuesync.StatusObservation{RawStatus: &raw, Status: "open", Version: at.UTC().Truncate(time.Millisecond)}
	if *t.Status == 2 {
		obs.Status = "closed"
		obs.ClosedReason = "done"
		obs.ClosedAt = new(obs.Version)
	}
	return obs, nil
}

// ReadStatus reads one task. The adapter checks project policy once when the
// status pass opens; WriteStatus rechecks it before every completion.
func (s *clientSession) ReadStatus(ctx context.Context, id string) (issuesync.StatusObservation, error) {
	t, err := s.readTask(ctx, id)
	if err != nil {
		return issuesync.StatusObservation{}, statusReadError(err)
	}
	return statusObservation(t, s.client.cfg.Now())
}
func statusReadError(err error) error {
	if _, ok := err.(*issuesync.StatusError); ok {
		return err
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	return blocked("TickTick status source is unavailable or outside the selected project")
}
func (s *clientSession) WriteStatus(ctx context.Context, id, desired string, admission func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	if s.config.StatusSync != "two-way" || admission == nil || (desired != "open" && desired != "closed") {
		return zero, blocked("TickTick writes require two-way mode, valid status and admission")
	}
	if _, err := s.Project(ctx); err != nil {
		return zero, statusReadError(err)
	}
	task, err := s.readTask(ctx, id)
	if err != nil {
		return zero, statusReadError(err)
	}
	obs, err := statusObservation(task, s.client.cfg.Now())
	if err != nil || obs.Status == desired {
		return obs, err
	}
	if desired == "open" {
		return zero, blocked("TickTick reopening is unsupported by the public status API; reopen the task in TickTick")
	}
	if task.RepeatFlag != "" {
		return zero, blocked("TickTick recurring task completion requires manual action in TickTick")
	}
	if err = s.client.admit(ctx); err != nil {
		return zero, err
	}
	if err = admission(); err != nil {
		return zero, err
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	traced, sent := issuesync.TrackRequestWrite(ctx)
	req, err := http.NewRequestWithContext(traced, http.MethodPost, apiOrigin+s.path()+"/task/"+id+"/complete", nil)
	if err != nil {
		return zero, blocked("invalid TickTick completion request")
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.http.Do(req)
	if err != nil {
		if !sent() {
			return zero, &issuesync.StatusError{Message: "TickTick completion could not connect"}
		}
		return zero, &issuesync.StatusError{Message: "TickTick completion response was lost", Ambiguous: true}
	}
	_ = res.Body.Close()
	var delay time.Duration
	if res.StatusCode == 429 || res.StatusCode >= 500 {
		delay = s.client.deferRequests(res.Header.Get("Retry-After"))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return zero, &issuesync.StatusError{Message: fmt.Sprintf("TickTick completion failed (HTTP %d)", res.StatusCode), HTTPStatus: res.StatusCode, RetryAfter: delay, Ambiguous: res.StatusCode >= 500, Blocked: res.StatusCode >= 300 && res.StatusCode < 500 && res.StatusCode != 429 && res.StatusCode != 409}
	}
	obs, err = s.ReadStatus(ctx, id)
	if err != nil || obs.Status != desired {
		return zero, &issuesync.StatusError{Message: "TickTick completion could not be verified", Ambiguous: true}
	}
	return obs, nil
}

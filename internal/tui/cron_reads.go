package tui

import (
	"context"
	"errors"
	"maps"
	"sync"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

// cronReadScope owns and cancels asynchronous work for a TUI lifetime or an
// active Cron visit. Sharing its pointer across Model values preserves ownership.
type cronReadScope struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *cronReadScope) stop() {
	if s != nil {
		s.cancel()
	}
}

func cronRead[T any](ctx context.Context, read func(context.Context, int64) (T, error), id int64) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	return read(ctx, id)
}

// cronFetch keeps the first three independent reads within one timeout window,
// leaving a separate bounded budget for per-job enrichment.
func cronFetch(ctx context.Context, reader cronAPI, id int64, data *cronData) {
	var wg sync.WaitGroup
	wg.Go(func() { data.jobs, data.jobsErr = cronRead(ctx, reader.ListCronJobs, id) })
	wg.Go(func() { data.workflows, data.workflowsErr = cronRead(ctx, reader.ListCronWorkflows, id) })
	wg.Go(func() {
		readCtx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
		defer cancel()
		data.runs, data.nextBefore, data.runsErr = reader.ListCronJobRuns(readCtx, id, "", "", 100)
	})
	if optional, ok := reader.(cronReplicaAPI); ok {
		wg.Go(func() {
			if replica, err := cronRead(ctx, optional.readCronReplica, id); err == nil {
				data.replica = replica
			}
		})
	}
	wg.Wait()
	cronEnrich(ctx, reader, id, data)
}

type cronEnrichment struct {
	uid    string
	parent bool
	runs   []generated.CronRun
	value  cronParent
	err    error
}

// cronEnrich establishes fresh last-run provenance independently of the display
// cache. All extra reads share four workers and one deadline, including parents.
func cronEnrich(ctx context.Context, reader cronAPI, id int64, p *cronData) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	p.lastRuns = map[string]generated.CronRun{}
	p.lastErr = map[string]error{}
	p.progress = map[string]cron.RecurringProgress{}
	p.parentOpen = map[string]bool{}
	if p.jobsErr != nil {
		return
	}
	if p.runsErr == nil {
		for _, r := range p.runs {
			if uid := deref(r.JobUID); uid != "" {
				if _, seen := p.lastRuns[uid]; !seen {
					p.lastRuns[uid] = r
				}
			}
		}
	}
	tasks := []cronEnrichment{}
	parents := map[string]bool{}
	fills := 0
	for _, j := range p.jobs {
		if parent := cronNativeParent(j); parent != "" {
			if !j.Definition.Enabled || parents[parent] {
				continue
			}
			parents[parent] = true
			if len(parents) <= 200 {
				tasks = append(tasks, cronEnrichment{uid: parent, parent: true})
			}
			continue
		}
		if _, known := p.lastRuns[j.UID]; known {
			continue
		}
		fills++
		if fills > 200 {
			p.lastErr[j.UID] = errors.New("too many jobs (200 fill reads per refresh)")
			continue
		}
		tasks = append(tasks, cronEnrichment{uid: j.UID})
	}
	queue := make(chan cronEnrichment, len(tasks))
	results := make(chan cronEnrichment, len(tasks))
	for _, task := range tasks {
		queue <- task
	}
	close(queue)
	var wg sync.WaitGroup
	for range min(4, len(tasks)) {
		wg.Go(func() {
			for task := range queue {
				if task.err = ctx.Err(); task.err == nil {
					if task.parent {
						task.value, task.err = reader.readCronParent(ctx, id, task.uid)
					} else {
						task.runs, _, task.err = reader.ListCronJobRuns(ctx, id, task.uid, "", 1)
						if task.err == nil {
							task.err = cronValidateJobPage(task.runs, task.uid)
						}
					}
				}
				results <- task
			}
		})
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.parent {
			if result.err == nil {
				p.parentOpen[result.uid] = result.value.open
				maps.Copy(p.progress, result.value.progress)
			}
		} else {
			p.lastErr[result.uid] = result.err
			if result.err == nil && len(result.runs) > 0 {
				p.lastRuns[result.uid] = result.runs[0]
			}
		}
	}
}

func cronValidateJobPage(runs []generated.CronRun, job string) error {
	for _, r := range runs {
		if deref(r.JobUID) != job {
			return errors.New("run page does not match requested job")
		}
	}
	return nil
}

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
	uid               string
	parent, wait, run bool
	runs              []generated.CronRun
	value             cronParent
	attention         cronRunWait
	err               error
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
	p.runErr = map[string]error{}
	p.lookup = map[string]generated.CronRun{}
	p.waits = map[string]cronRunWait{}
	p.waitErr = map[string]error{}
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
			if j.Definition.Enabled && !parents[parent] {
				parents[parent] = true
				if len(parents) <= 200 {
					tasks = append(tasks, cronEnrichment{uid: parent, parent: true})
				}
			}
		}
		if _, known := p.lastRuns[j.UID]; known {
			continue
		}
		fills++
		if fills > 200 {
			p.lastErr[j.UID] = errors.New("last-run lookup limit reached (200 jobs per refresh)")
			continue
		}
		tasks = append(tasks, cronEnrichment{uid: j.UID})
	}

	runReader, supportsRun := reader.(cronRunAPI)
	for _, r := range p.inspectedRuns {
		p.lookup[r.UID] = r
		fresh, known := p.lastRuns[deref(r.JobUID)]
		if known && fresh.UID == r.UID {
			p.lookup[r.UID] = fresh
			continue
		}
		if !supportsRun {
			p.runErr[r.UID] = errors.New("run refresh unavailable")
			continue
		}
		tasks = append(tasks, cronEnrichment{uid: r.UID, run: true})
	}
	waitReader, supportsWait := reader.(cronWaitAPI)
	issues := map[string]bool{}
	waitTasks := func(runs []generated.CronRun) []cronEnrichment {
		var added []cronEnrichment
		if !supportsWait {
			return added
		}
		for _, r := range runs {
			uid := deref(r.IssueUID)
			if r.Status != "running" || uid == "" || issues[uid] {
				continue
			}
			if j := p.job(deref(r.JobUID)); j != nil && cronNativeParent(*j) != "" {
				continue
			}
			issues[uid] = true
			if len(issues) > 20 {
				p.waitErr[uid] = errors.New("too many running issues (20 attention reads per refresh)")
				continue
			}
			added = append(added, cronEnrichment{uid: uid, wait: true})
		}
		return added
	}
	tasks = append(tasks, waitTasks(cronMergeRuns(p.historyRuns, p.runs))...)
	// Fill reads may discover running observations. The coordinator admits those
	// reads into this same pool before closing it; no new deadline is created.
	queue := make(chan cronEnrichment, len(tasks)+20)
	results := make(chan cronEnrichment, len(tasks)+20)
	for _, task := range tasks {
		queue <- task
	}
	var wg sync.WaitGroup
	for range min(4, len(tasks)) {
		wg.Go(func() {
			for task := range queue {
				if task.err = ctx.Err(); task.err == nil {
					switch {
					case task.run:
						var r generated.CronRun
						r, task.err = runReader.readCronRun(ctx, id, task.uid)
						if task.err == nil && r.UID != task.uid {
							task.err = errors.New("received a different run")
						}
						task.runs = []generated.CronRun{r}
					case task.wait:
						task.attention, task.err = waitReader.readCronRunWait(ctx, id, task.uid)
					case task.parent:
						task.value, task.err = reader.readCronParent(ctx, id, task.uid)
					default:
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
	for pending := len(tasks); pending > 0; {
		result := <-results
		pending--
		switch {
		case result.run:
			p.runErr[result.uid] = result.err
			if result.err == nil {
				p.lookup[result.uid] = result.runs[0]
			}
		case result.wait:
			p.waitErr[result.uid] = result.err
			if result.err == nil {
				p.waits[result.uid] = result.attention
			}
		case result.parent:
			if result.err == nil {
				p.parentOpen[result.uid] = result.value.open
				maps.Copy(p.progress, result.value.progress)
			}
		default:
			p.lastErr[result.uid] = result.err
			if result.err == nil && len(result.runs) > 0 {
				p.lastRuns[result.uid] = result.runs[0]
				for _, task := range waitTasks(result.runs) {
					queue <- task
					pending++
				}
			}
		}
	}
	close(queue)
	wg.Wait()
}

func cronValidateJobPage(runs []generated.CronRun, job string) error {
	for _, r := range runs {
		if deref(r.JobUID) != job {
			return errors.New("received runs for a different job")
		}
	}
	return nil
}

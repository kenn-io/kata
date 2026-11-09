package daemon

import (
	"context"
	"time"

	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/twentysync"
)

func newTrackerRemoteClients(run *trackerRemoteRun) trackerClients {
	return trackerClients{github: &trackerRemoteGitHub{run: run}, notion: &trackerRemoteNotion{run: run}, linear: &trackerRemoteLinear{run: run}, plane: &trackerRemotePlane{run: run}, ticktick: &trackerRemoteTickTick{run: run}, todoist: &trackerRemoteTodoist{run: run}, twenty: &trackerRemoteTwenty{run: run}}
}
func remoteTrackerRead[T any](ctx context.Context, r *trackerRemoteRun, method string, args trackerWorkerArguments) (T, error) {
	var out T
	action := r.action
	if action == "" {
		action = tracker.ActionRead
	}
	err := r.invoke(ctx, method, action, args, &out, false)
	return out, err
}
func remoteTrackerStatus(ctx context.Context, r *trackerRemoteRun, args trackerWorkerArguments) (issuesync.StatusObservation, error) {
	out, err := remoteTrackerRead[issuesync.StatusObservation](ctx, r, "read-status", args)
	out.TrackerWriteUID = ""
	return out, err
}

type trackerRemoteNotion struct{ run *trackerRemoteRun }

func (f *trackerRemoteNotion) ForRun(ctx context.Context) (notionsync.Session, error) {
	return f, ctx.Err()
}
func (f *trackerRemoteNotion) Database(ctx context.Context, id string) (notionsync.Database, error) {
	return remoteTrackerRead[notionsync.Database](ctx, f.run, "database", trackerWorkerArguments{ID: id})
}
func (f *trackerRemoteNotion) DataSource(ctx context.Context, id string) (notionsync.DataSource, error) {
	return remoteTrackerRead[notionsync.DataSource](ctx, f.run, "data-source", trackerWorkerArguments{ID: id})
}
func (f *trackerRemoteNotion) Pages(ctx context.Context, c notionsync.Config, since *time.Time) ([]notionsync.Page, error) {
	return remoteTrackerRead[[]notionsync.Page](ctx, f.run, "pages", trackerWorkerArguments{Notion: &c, Since: since})
}
func (f *trackerRemoteNotion) Content(ctx context.Context, c notionsync.Config, p notionsync.Page) (notionsync.PageContent, error) {
	return remoteTrackerRead[notionsync.PageContent](ctx, f.run, "content", trackerWorkerArguments{Notion: &c, ID: p.ID})
}
func (f *trackerRemoteNotion) ReadStatus(ctx context.Context, c notionsync.Config, id string) (issuesync.StatusObservation, error) {
	return remoteTrackerStatus(ctx, f.run, trackerWorkerArguments{Notion: &c, ID: id})
}
func (f *trackerRemoteNotion) WriteStatus(ctx context.Context, c notionsync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	return f.run.writeStatus(ctx, trackerWorkerArguments{Notion: &c, ID: id, Target: target}, admit)
}

type trackerRemoteLinear struct{ run *trackerRemoteRun }

func (f *trackerRemoteLinear) ForRun(ctx context.Context, c linearsync.Config) (linearsync.Session, error) {
	return f, ctx.Err()
}
func (f *trackerRemoteLinear) Scope(ctx context.Context, c linearsync.Config) (linearsync.Scope, error) {
	return remoteTrackerRead[linearsync.Scope](ctx, f.run, "scope", trackerWorkerArguments{Linear: &c})
}
func (f *trackerRemoteLinear) States(ctx context.Context, c linearsync.Config) ([]linearsync.State, error) {
	return remoteTrackerRead[[]linearsync.State](ctx, f.run, "states", trackerWorkerArguments{Linear: &c})
}
func (f *trackerRemoteLinear) Issues(ctx context.Context, c linearsync.Config) ([]linearsync.Issue, error) {
	return remoteTrackerRead[[]linearsync.Issue](ctx, f.run, "issues", trackerWorkerArguments{Linear: &c})
}
func (f *trackerRemoteLinear) ReadStatus(ctx context.Context, c linearsync.Config, id string) (issuesync.StatusObservation, error) {
	return remoteTrackerStatus(ctx, f.run, trackerWorkerArguments{Linear: &c, ID: id})
}
func (f *trackerRemoteLinear) WriteStatus(ctx context.Context, c linearsync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	return f.run.writeStatus(ctx, trackerWorkerArguments{Linear: &c, ID: id, Target: target}, admit)
}

type trackerRemotePlane struct{ run *trackerRemoteRun }

func (f *trackerRemotePlane) ForRun(ctx context.Context, c planesync.Config) (planesync.Session, error) {
	return f, ctx.Err()
}
func (f *trackerRemotePlane) Project(ctx context.Context, c planesync.Config) (planesync.Project, error) {
	return remoteTrackerRead[planesync.Project](ctx, f.run, "project", trackerWorkerArguments{Plane: &c})
}
func (f *trackerRemotePlane) States(ctx context.Context, c planesync.Config) ([]planesync.State, error) {
	return remoteTrackerRead[[]planesync.State](ctx, f.run, "states", trackerWorkerArguments{Plane: &c})
}
func (f *trackerRemotePlane) WorkItems(ctx context.Context, c planesync.Config) ([]planesync.WorkItem, error) {
	return remoteTrackerRead[[]planesync.WorkItem](ctx, f.run, "work-items", trackerWorkerArguments{Plane: &c})
}
func (f *trackerRemotePlane) ReadStatus(ctx context.Context, c planesync.Config, id string) (issuesync.StatusObservation, error) {
	return remoteTrackerStatus(ctx, f.run, trackerWorkerArguments{Plane: &c, ID: id})
}
func (f *trackerRemotePlane) WriteStatus(ctx context.Context, c planesync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	return f.run.writeStatus(ctx, trackerWorkerArguments{Plane: &c, ID: id, Target: target}, admit)
}

type trackerRemoteTickTick struct {
	run    *trackerRemoteRun
	config tickticksync.Config
}

func (f *trackerRemoteTickTick) ForRun(ctx context.Context, c tickticksync.Config) (tickticksync.Session, error) {
	return &trackerRemoteTickTick{run: f.run, config: c}, ctx.Err()
}
func (f *trackerRemoteTickTick) Project(ctx context.Context) (tickticksync.Project, error) {
	return remoteTrackerRead[tickticksync.Project](ctx, f.run, "project", trackerWorkerArguments{TickTick: &f.config})
}
func (f *trackerRemoteTickTick) Data(ctx context.Context) (tickticksync.ProjectData, error) {
	return remoteTrackerRead[tickticksync.ProjectData](ctx, f.run, "data", trackerWorkerArguments{TickTick: &f.config})
}
func (f *trackerRemoteTickTick) Task(ctx context.Context, id string) (tickticksync.Task, error) {
	return remoteTrackerRead[tickticksync.Task](ctx, f.run, "task", trackerWorkerArguments{TickTick: &f.config, ID: id})
}
func (f *trackerRemoteTickTick) ReadStatus(ctx context.Context, id string) (issuesync.StatusObservation, error) {
	return remoteTrackerStatus(ctx, f.run, trackerWorkerArguments{TickTick: &f.config, ID: id})
}
func (f *trackerRemoteTickTick) WriteStatus(ctx context.Context, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	return f.run.writeStatus(ctx, trackerWorkerArguments{TickTick: &f.config, ID: id, Target: target}, admit)
}

type trackerRemoteTodoist struct{ run *trackerRemoteRun }

func (f *trackerRemoteTodoist) Account(ctx context.Context) (string, error) {
	return remoteTrackerRead[string](ctx, f.run, "account", trackerWorkerArguments{})
}
func (f *trackerRemoteTodoist) ForRun(ctx context.Context, c todoistsync.Config) (todoistsync.Session, error) {
	return f, ctx.Err()
}
func (f *trackerRemoteTodoist) Project(ctx context.Context, c todoistsync.Config) (todoistsync.Project, error) {
	return remoteTrackerRead[todoistsync.Project](ctx, f.run, "project", trackerWorkerArguments{Todoist: &c})
}
func (f *trackerRemoteTodoist) Tasks(ctx context.Context, c todoistsync.Config, since, until time.Time) ([]todoistsync.Task, error) {
	return remoteTrackerRead[[]todoistsync.Task](ctx, f.run, "tasks", trackerWorkerArguments{Todoist: &c, Since: &since, Until: &until})
}
func (f *trackerRemoteTodoist) ReadStatus(ctx context.Context, c todoistsync.Config, t todoistsync.StatusTarget) (issuesync.StatusObservation, error) {
	return remoteTrackerStatus(ctx, f.run, trackerWorkerArguments{Todoist: &c, TodoistTarget: &t})
}
func (f *trackerRemoteTodoist) WriteStatus(ctx context.Context, c todoistsync.Config, t todoistsync.StatusTarget, target string, admit func() error) (issuesync.StatusObservation, error) {
	return f.run.writeStatus(ctx, trackerWorkerArguments{Todoist: &c, TodoistTarget: &t, Target: target}, admit)
}

type trackerRemoteTwenty struct{ run *trackerRemoteRun }

func (f *trackerRemoteTwenty) ForRun(ctx context.Context, c twentysync.Config) (twentysync.Session, error) {
	return f, ctx.Err()
}
func (f *trackerRemoteTwenty) Workspace(ctx context.Context, c twentysync.Config) (twentysync.Workspace, error) {
	return remoteTrackerRead[twentysync.Workspace](ctx, f.run, "workspace", trackerWorkerArguments{Twenty: &c})
}
func (f *trackerRemoteTwenty) Schema(ctx context.Context, c twentysync.Config) (twentysync.Schema, error) {
	return remoteTrackerRead[twentysync.Schema](ctx, f.run, "schema", trackerWorkerArguments{Twenty: &c})
}
func (f *trackerRemoteTwenty) Tasks(ctx context.Context, c twentysync.Config) ([]twentysync.Task, error) {
	return remoteTrackerRead[[]twentysync.Task](ctx, f.run, "tasks", trackerWorkerArguments{Twenty: &c})
}
func (f *trackerRemoteTwenty) ReadStatus(ctx context.Context, c twentysync.Config, id string) (issuesync.StatusObservation, error) {
	return remoteTrackerStatus(ctx, f.run, trackerWorkerArguments{Twenty: &c, ID: id})
}
func (f *trackerRemoteTwenty) WriteStatus(ctx context.Context, c twentysync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	return f.run.writeStatus(ctx, trackerWorkerArguments{Twenty: &c, ID: id, Target: target}, admit)
}

type trackerRemoteGitHub struct{ run *trackerRemoteRun }

func (f *trackerRemoteGitHub) ForBinding(ctx context.Context, b githubsync.Binding) (githubsync.Fetcher, error) {
	return f, ctx.Err()
}
func remoteGitHubConfig(b githubsync.Binding) githubsync.Config {
	return githubsync.Config{Host: b.Host, Owner: b.Owner, Repo: b.Repo}
}
func (f *trackerRemoteGitHub) Repository(ctx context.Context, host, owner, repo string) (githubsync.Repository, error) {
	c := githubsync.Config{Host: host, Owner: owner, Repo: repo}
	return remoteTrackerRead[githubsync.Repository](ctx, f.run, "repository", trackerWorkerArguments{GitHub: &c})
}
func (f *trackerRemoteGitHub) Issues(ctx context.Context, b githubsync.Binding, since *time.Time) ([]githubsync.Issue, error) {
	c := remoteGitHubConfig(b)
	return remoteTrackerRead[[]githubsync.Issue](ctx, f.run, "issues", trackerWorkerArguments{GitHub: &c, Since: since})
}
func (f *trackerRemoteGitHub) Comments(ctx context.Context, b githubsync.Binding, number int) ([]githubsync.Comment, error) {
	c := remoteGitHubConfig(b)
	return remoteTrackerRead[[]githubsync.Comment](ctx, f.run, "comments", trackerWorkerArguments{GitHub: &c, Number: number})
}
func (f *trackerRemoteGitHub) RepositoryComments(ctx context.Context, b githubsync.Binding, since *time.Time) (map[int][]githubsync.Comment, bool, error) {
	c := remoteGitHubConfig(b)
	out, err := remoteTrackerRead[trackerWorkerRepositoryComments](ctx, f.run, "repository-comments", trackerWorkerArguments{GitHub: &c, Since: since})
	return out.Comments, out.Complete, err
}
func (f *trackerRemoteGitHub) ParentData(ctx context.Context, b githubsync.Binding, request githubsync.ParentRequest) (githubsync.ParentData, error) {
	c := remoteGitHubConfig(b)
	return remoteTrackerRead[githubsync.ParentData](ctx, f.run, "parents", trackerWorkerArguments{GitHub: &c, ParentRequest: &request})
}
func (f *trackerRemoteGitHub) ReadStatus(ctx context.Context, c githubsync.Config, id string, number int) (issuesync.StatusObservation, error) {
	return remoteTrackerStatus(ctx, f.run, trackerWorkerArguments{GitHub: &c, ID: id, Number: number})
}
func (f *trackerRemoteGitHub) WriteStatus(ctx context.Context, c githubsync.Config, id string, number int, target string, admit func() error) (issuesync.StatusObservation, error) {
	return f.run.writeStatus(ctx, trackerWorkerArguments{GitHub: &c, ID: id, Number: number, Target: target}, admit)
}
func (f *trackerRemoteGitHub) StatusLocators(ctx context.Context, c githubsync.Config, page int) ([]githubsync.StatusLocator, int, error) {
	out, err := remoteTrackerRead[trackerWorkerStatusLocators](ctx, f.run, "status-locators", trackerWorkerArguments{GitHub: &c, Page: page})
	return out.Locators, out.Next, err
}
func (f *trackerRemoteGitHub) ReadStatuses(ctx context.Context, c githubsync.Config, targets []githubsync.StatusLocator) map[string]githubsync.StatusResult {
	out := make(map[string]githubsync.StatusResult, len(targets))
	for _, target := range targets {
		obs, err := f.ReadStatus(ctx, c, target.ExternalID, target.Number)
		out[target.ExternalID] = githubsync.StatusResult{Observation: obs, Err: err}
	}
	return out
}

var _ notionsync.Fetcher = (*trackerRemoteNotion)(nil)
var _ notionsync.StatusSession = (*trackerRemoteNotion)(nil)
var _ linearsync.Fetcher = (*trackerRemoteLinear)(nil)
var _ linearsync.StatusSession = (*trackerRemoteLinear)(nil)
var _ planesync.Fetcher = (*trackerRemotePlane)(nil)
var _ planesync.StatusSession = (*trackerRemotePlane)(nil)
var _ tickticksync.Fetcher = (*trackerRemoteTickTick)(nil)
var _ tickticksync.StatusSession = (*trackerRemoteTickTick)(nil)
var _ todoistsync.Fetcher = (*trackerRemoteTodoist)(nil)
var _ todoistsync.StatusSession = (*trackerRemoteTodoist)(nil)
var _ twentysync.Fetcher = (*trackerRemoteTwenty)(nil)
var _ twentysync.StatusSession = (*trackerRemoteTwenty)(nil)
var _ githubsync.Fetcher = (*trackerRemoteGitHub)(nil)
var _ githubsync.StatusSession = (*trackerRemoteGitHub)(nil)
var _ githubsync.StatusLocatorSession = (*trackerRemoteGitHub)(nil)
var _ githubsync.StatusBatchSession = (*trackerRemoteGitHub)(nil)

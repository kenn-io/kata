package daemon

import (
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
)

// trackerClients are owner-local and never marshaled. One client is retained
// per connection/generation so requests share provider pacing and cooldowns.
// Constructors do not resolve tokens or make provider requests.
type trackerClients struct {
	github   githubsync.Fetcher
	notion   notionsync.Fetcher
	linear   linearsync.Fetcher
	plane    planesync.Fetcher
	ticktick tickticksync.Fetcher
	todoist  todoistsync.Fetcher
	twenty   twentysync.Fetcher
}

func newTrackerClients(c config.TrackerConnectionConfig) trackerClients {
	switch c.Provider {
	case "github":
		return trackerClients{github: githubsync.NewHTTPFetcher(githubsync.HTTPFetcherConfig{CredentialResolver: githubsync.NewNamedCredentialResolver(*c.GitHub, nil)})}
	case "notion":
		return trackerClients{notion: notionsync.NewClient(notionsync.ClientConfig{TokenEnv: c.Notion.TokenEnv})}
	case "linear":
		return trackerClients{linear: linearsync.NewClient(linearsync.ClientConfig{Daemon: *c.Linear})}
	case "plane":
		return trackerClients{plane: planesync.NewClient(planesync.ClientConfig{Daemon: *c.Plane})}
	case "ticktick":
		return trackerClients{ticktick: tickticksync.NewClient(tickticksync.ClientConfig{TokenEnv: c.TickTick.TokenEnv})}
	case "todoist":
		return trackerClients{todoist: todoistsync.NewClient(todoistsync.ClientConfig{Daemon: *c.Todoist})}
	case "twenty":
		return trackerClients{twenty: twentysync.NewClient(twentysync.ClientConfig{Daemon: *c.Twenty})}
	}
	return trackerClients{}
}

func (r *TrackerRuntime) GitHubFetcher(legacy githubsync.Fetcher) githubsync.Fetcher {
	return trackerGitHubFetcher{runtime: r, legacy: legacy}
}
func (r *TrackerRuntime) NotionFetcher(legacy notionsync.Fetcher) notionsync.Fetcher {
	return trackerNotionFetcher{runtime: r, legacy: legacy}
}
func (r *TrackerRuntime) LinearFetcher(legacy linearsync.Fetcher) linearsync.Fetcher {
	return trackerLinearFetcher{runtime: r, legacy: legacy}
}
func (r *TrackerRuntime) PlaneFetcher(legacy planesync.Fetcher) planesync.Fetcher {
	return trackerPlaneFetcher{runtime: r, legacy: legacy}
}
func (r *TrackerRuntime) TickTickFetcher(legacy tickticksync.Fetcher) tickticksync.Fetcher {
	return trackerTickTickFetcher{runtime: r, legacy: legacy}
}
func (r *TrackerRuntime) TodoistFetcher(legacy todoistsync.Fetcher) todoistsync.Fetcher {
	return trackerTodoistFetcher{runtime: r, legacy: legacy}
}
func (r *TrackerRuntime) TwentyFetcher(legacy twentysync.Fetcher) twentysync.Fetcher {
	return trackerTwentyFetcher{runtime: r, legacy: legacy}
}

package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/splitlayout"
)

// searchModelAPI makes fakeListAPI's search answers reachable through the
// KataAPI a Model holds; everything else stays unimplemented on purpose.
type searchModelAPI struct {
	KataAPI
	fake *fakeListAPI
}

func (a searchModelAPI) SearchIssues(ctx context.Context, pid int64, q SearchQuery) (*SearchAnswer, error) {
	return a.fake.SearchIssues(ctx, pid, q)
}

var ctrlT = tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}

func searchTestIssues() (alpha, beta, gamma Issue) {
	return Issue{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "fix alpha", Status: "open"},
		Issue{ProjectID: 7, UID: "01TEST-bbb2", ShortID: "bbb2", Title: "billing rounding", Status: "open"},
		Issue{ProjectID: 7, UID: "01TEST-ccc3", ShortID: "ccc3", Title: "login hangs", Status: "open"}
}

func daemonSearchFixture(t *testing.T) (Model, *fakeListAPI) {
	t.Helper()
	useNoColor(t)
	useInstantTicks(t)
	api := &fakeListAPI{}
	m := newTestModel()
	m.api = searchModelAPI{fake: api}
	m.view = viewList
	alpha, beta, gamma := searchTestIssues()
	m.list.issues = []Issue{alpha, beta, gamma}
	m = resizeModel(m, 80, 24)
	return m, api
}

func visibleUIDs(m Model) []string {
	var uids []string
	for _, row := range m.list.visibleRows() {
		uids = append(uids, row.issue.UID)
	}
	return uids
}

func typeText(m Model, text string) (Model, []tea.Cmd) {
	var cmds []tea.Cmd
	for _, r := range text {
		var cmd tea.Cmd
		m, cmd = stepModel(m, runeKey(r))
		cmds = append(cmds, cmd)
	}
	return m, cmds
}

// settleSearch plays out the typing-pause timers and daemon answers the given
// commands lead to, and nothing else.
func settleSearch(m Model, cmds ...tea.Cmd) Model {
	queue := cmds
	for len(queue) > 0 {
		cmd := queue[0]
		queue = queue[1:]
		for _, msg := range runCmdTree(cmd) {
			switch msg.(type) {
			case daemonSearchTickMsg, daemonSearchDoneMsg:
				var next tea.Cmd
				m, next = stepModel(m, msg)
				queue = append(queue, next)
			}
		}
	}
	return m
}

// onlyMsg runs cmd and returns the one message it produces.
func onlyMsg(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msgs := runCmdTree(cmd)
	require.Len(t, msgs, 1)
	return msgs[0]
}

func answerWith(mode string, issues ...Issue) func(SearchQuery) (*SearchAnswer, error) {
	return func(SearchQuery) (*SearchAnswer, error) {
		return &SearchAnswer{Mode: mode, Issues: issues}, nil
	}
}

func TestDaemonSearch_AsksOncePerPauseAndListsHitsInDaemonOrder(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	// Neither title contains the query, so only the daemon can find them.
	api.searchFn = answerWith("hybrid", gamma, alpha)
	m.list.filter = ListFilter{Status: "open", Labels: []string{"bug", "ui"}}

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	require.Empty(t, api.searchCalls, "typing alone must not ask the daemon")
	m = settleSearch(m, cmds...)

	require.Equal(t, []SearchQuery{{
		Query: "payments", Mode: "auto", Limit: 50, Status: "open", Labels: []string{"bug", "ui"},
	}}, api.searchCalls)
	require.Equal(t, []int64{7}, api.searchPIDs)
	require.Equal(t, []string{"01TEST-ccc3", "01TEST-aaa1"}, visibleUIDs(m))
	for _, row := range m.list.visibleRows() {
		require.Zero(t, row.depth, "ranked hits show flat")
	}
}

func TestDaemonSearch_OwnerFilterStillNarrowsHits(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	owner := "agent-a"
	gamma.Owner = &owner
	m.list.issues = []Issue{alpha, gamma}
	api.searchFn = answerWith("hybrid", alpha, gamma)
	m.list.filter = ListFilter{Owner: "agent-a"}

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)

	require.Equal(t, []string{"01TEST-ccc3"}, visibleUIDs(m))
}

func TestDaemonSearch_StaleAnswerDoesNotOverwriteNewerOne(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	api.searchFn = func(q SearchQuery) (*SearchAnswer, error) {
		if q.Query == "pay" {
			return &SearchAnswer{Mode: "hybrid", Issues: []Issue{alpha}}, nil
		}
		return &SearchAnswer{Mode: "hybrid", Issues: []Issue{gamma}}, nil
	}
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)

	m, cmds := typeText(m, "pay")
	m, ask := stepModel(m, onlyMsg(t, cmds[len(cmds)-1]))
	slowAnswer := onlyMsg(t, ask)

	m, cmds = typeText(m, "ments")
	m, ask = stepModel(m, onlyMsg(t, cmds[len(cmds)-1]))
	newerAnswer := onlyMsg(t, ask)

	m, _ = stepModel(m, newerAnswer)
	m, _ = stepModel(m, slowAnswer)
	require.Equal(t, []string{"01TEST-ccc3"}, visibleUIDs(m))
}

func TestDaemonSearch_ToggleOffReturnsToLocalFiltering(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, _ := searchTestIssues()
	api.searchFn = answerWith("hybrid", alpha)

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "login")
	m = settleSearch(m, cmds...)
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))

	m, _ = stepModel(m, ctrlT)
	require.Equal(t, []string{"01TEST-ccc3"}, visibleUIDs(m), "the text filters loaded rows again")
	m, cmds = typeText(m, "x")
	m = settleSearch(m, cmds...)
	require.Empty(t, visibleUIDs(m))
	require.Len(t, api.searchCalls, 1, "local filtering asks the daemon nothing")
}

func TestDaemonSearch_AllProjectsSendsNothingAndSaysWhy(t *testing.T) {
	m, api := daemonSearchFixture(t)
	m.scope = scope{allProjects: true}
	m = openBarFromCmd(t, m, '/')

	m, cmd := stepModel(m, ctrlT)
	require.Nil(t, cmd)
	require.False(t, m.daemonSearch)
	require.Equal(t, "daemon search needs one project", m.list.rankedNote())

	m, cmds := typeText(m, "login")
	m = settleSearch(m, cmds...)
	require.Empty(t, api.searchCalls)
	require.Equal(t, []string{"01TEST-ccc3"}, visibleUIDs(m), "the text still filters loaded rows")
	require.Empty(t, m.list.rankedNote())
}

func TestDaemonSearch_ErrorKeepsRowsAndShowsInStatusLine(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, _ := searchTestIssues()
	api.searchFn = func(q SearchQuery) (*SearchAnswer, error) {
		if q.Query == "pay" {
			return &SearchAnswer{Mode: "hybrid", Issues: []Issue{alpha}}, nil
		}
		return nil, errors.New("embedding service refused")
	}
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "pay")
	m = settleSearch(m, cmds...)
	m, cmds = typeText(m, "ments")
	m = settleSearch(m, cmds...)

	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))
	require.Contains(t, stripANSI(m.list.status), "embedding service refused")
}

func TestDaemonSearch_OldDaemonErrorShowsUpgradeMessage(t *testing.T) {
	m, api := daemonSearchFixture(t)
	api.searchFn = func(SearchQuery) (*SearchAnswer, error) {
		return nil, errors.New("status-filtered search requires daemon API 0.20.0 or newer; upgrade the daemon")
	}
	m.list.filter.Status = "open"
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "pay")
	m = settleSearch(m, cmds...)

	require.Contains(t, stripANSI(m.list.status), "upgrade the daemon")
	require.Len(t, visibleUIDs(m), 3, "an old daemon never turns into an empty result")
}

func TestDaemonSearch_NoteSaysHowTheDaemonRanked(t *testing.T) {
	_, _, gamma := searchTestIssues()
	for _, tc := range []struct {
		name   string
		answer SearchAnswer
		want   []string
		deny   []string
	}{
		{"words and meaning", SearchAnswer{Mode: "hybrid"}, []string{"ranked by words and meaning"}, []string{"lexical"}},
		{"words only", SearchAnswer{Mode: "lexical"}, []string{"ranked by words only (lexical)"}, []string{"meaning"}},
		{
			"degraded says why",
			SearchAnswer{Mode: "lexical", Degraded: true, DegradedReason: "embedding service unreachable"},
			[]string{"words only", "embedding service unreachable"}, nil,
		},
		{"more than fifty match", SearchAnswer{Mode: "hybrid", Truncated: true}, []string{"showing the best 50"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := daemonSearchFixture(t)
			answer := tc.answer
			answer.Issues = []Issue{gamma}
			api.searchFn = func(SearchQuery) (*SearchAnswer, error) { return &answer, nil }
			m = openBarFromCmd(t, m, '/')
			m, _ = stepModel(m, ctrlT)
			m, cmds := typeText(m, "pay")
			m = settleSearch(m, cmds...)
			for _, want := range tc.want {
				require.Contains(t, m.list.rankedNote(), want)
			}
			for _, deny := range tc.deny {
				require.NotContains(t, m.list.rankedNote(), deny)
			}
		})
	}
}

func TestDaemonSearch_EscRestoresPreviousListAndDropsPendingAsk(t *testing.T) {
	m, api := daemonSearchFixture(t)
	_, beta, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma, beta)
	m.list.filter.Search = "fix"
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))

	// Toggling on with text already in the bar asks at once.
	m = openBarFromCmd(t, m, '/')
	m, cmd := stepModel(m, ctrlT)
	m = settleSearch(m, cmd)
	require.Equal(t, []string{"01TEST-ccc3", "01TEST-bbb2"}, visibleUIDs(m))

	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, inputNone, m.input.kind)
	require.Equal(t, "fix", m.list.filter.Search)
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))

	// A typing-pause timer that outlives the cancelled bar asks nothing.
	calls := len(api.searchCalls)
	m = openBarFromCmd(t, m, '/')
	m, cmds := typeText(m, "z")
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = settleSearch(m, cmds...)
	require.Len(t, api.searchCalls, calls, "the cancelled typing asked nothing")
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))
}

func TestDaemonSearch_CommitKeepsRankedResultsUntilFiltersClear(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma, alpha)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)

	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, inputNone, m.input.kind)
	require.Equal(t, []string{"01TEST-ccc3", "01TEST-aaa1"}, visibleUIDs(m))
	require.Contains(t, m.list.rankedNote(), "ranked by words and meaning")

	m, _ = stepModel(m, runeKey('c'))
	require.ElementsMatch(t, []string{"01TEST-aaa1", "01TEST-bbb2", "01TEST-ccc3"}, visibleUIDs(m))
	require.Empty(t, m.list.rankedNote())
}

func TestDaemonSearch_RefreshKeepsRankedOrderWithFreshRows(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, beta, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma, alpha)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})

	alpha.Title = "fix alpha, renamed"
	m.list = m.list.applyFetched(refetchedMsg{issues: []Issue{beta, alpha}})

	rows := m.list.visibleRows()
	require.Len(t, rows, 2)
	require.Equal(t, "01TEST-ccc3", rows[0].issue.UID, "an issue the refresh dropped keeps its place")
	require.Equal(t, "fix alpha, renamed", rows[1].issue.Title)
}

func TestDaemonSearch_BlankQueryAsksNothingAndShowsTheNormalList(t *testing.T) {
	m, api := daemonSearchFixture(t)
	_, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	require.Equal(t, []string{"01TEST-ccc3"}, visibleUIDs(m))

	var erase []tea.Cmd
	for range "payments" {
		var cmd tea.Cmd
		m, cmd = stepModel(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
		erase = append(erase, cmd)
	}
	m = settleSearch(m, erase...)

	require.Len(t, api.searchCalls, 1)
	require.Len(t, visibleUIDs(m), 3)
}

func TestDaemonSearch_FooterHintFollowsTheSamePredicateAsTheKey(t *testing.T) {
	askHint := helplayout.HelpItem{Key: "ctrl+t", Description: "ask daemon"}
	rowsHint := helplayout.HelpItem{Key: "ctrl+t", Description: "filter rows"}
	for _, tc := range []struct {
		name  string
		setup func(m Model) Model
		want  *helplayout.HelpItem
	}{
		{"bar open over one project", func(m Model) Model { return openBarFromCmdNoT(m) }, &askHint},
		{"results focus", func(m Model) Model {
			m = openBarFromCmdNoT(m)
			m.input.searchFocus = searchFocusResults
			return m
		}, &askHint},
		{"toggled on offers the way back", func(m Model) Model {
			m = openBarFromCmdNoT(m)
			m.daemonSearch = true
			return m
		}, &rowsHint},
		{"all projects", func(m Model) Model {
			m.scope = scope{allProjects: true}
			return openBarFromCmdNoT(m)
		}, nil},
		{"bar closed", func(m Model) Model { return m }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := daemonSearchFixture(t)
			m = tc.setup(m)
			rows := flattenHelpRows(m.helpRows())
			hasKey := false
			for _, item := range rows {
				hasKey = hasKey || item.Key == "ctrl+t"
			}
			if tc.want == nil {
				require.False(t, hasKey, "no hint when the toggle cannot work: %+v", rows)
				require.Empty(t, m.helpKeymap().DaemonSearch.Keys)
				return
			}
			assertHelpItemPresent(t, rows, *tc.want)
			require.Equal(t, []string{"ctrl+t"}, m.helpKeymap().DaemonSearch.Keys)
			require.True(t, m.daemonSearchAvailable())
		})
	}
}

func openBarFromCmdNoT(m Model) Model {
	out, cmd := m.Update(runeKey('/'))
	m = out.(Model)
	out, _ = m.Update(cmd())
	return out.(Model)
}

func TestDaemonSearch_RendersCompletelyAtEverySize(t *testing.T) {
	longName := strings.Repeat("example-project-", 5)
	for _, tc := range []struct {
		name          string
		width, height int
		split         bool
		allProjects   bool
		wantFooter    bool
	}{
		{"80x24", 80, 24, false, false, true},
		{"80x12", 80, 12, false, false, true},
		{"100x8", 100, 8, false, false, false},
		{"split 160x40", 160, 40, true, false, true},
		{"all projects 80x24", 80, 24, false, true, false},
		{"all projects 100x8", 100, 8, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := daemonSearchFixture(t)
			_, _, gamma := searchTestIssues()
			api.searchFn = func(SearchQuery) (*SearchAnswer, error) {
				return &SearchAnswer{
					Mode: "lexical", Degraded: true, DegradedReason: "embedding service unreachable",
					Truncated: true, Issues: []Issue{gamma},
				}, nil
			}
			m.scope = scope{projectID: 7, projectName: longName}
			if tc.allProjects {
				m.scope = scope{allProjects: true}
			}
			m = resizeModel(m, tc.width, tc.height)
			if tc.split {
				require.Equal(t, splitlayout.Split, m.layout)
			}
			m = openBarFromCmd(t, m, '/')
			m, _ = stepModel(m, ctrlT)
			wantNote := "ranked by words only"
			if tc.allProjects {
				wantNote = "daemon search needs one project"
			} else {
				var cmds []tea.Cmd
				m, cmds = typeText(m, "pay")
				m = settleSearch(m, cmds...)
			}

			view := stripANSI(m.View().Content)
			require.Contains(t, view, wantNote)
			if tc.wantFooter {
				require.Contains(t, view, "ctrl+t")
			} else {
				require.NotContains(t, view, "ctrl+t")
			}
			lines := strings.Split(view, "\n")
			require.LessOrEqual(t, len(lines), tc.height)
			for _, line := range lines {
				require.LessOrEqual(t, ansi.StringWidth(line), tc.width, line)
			}
		})
	}
}

func TestClient_SearchIssues_SendsRouteParametersAndDecodesHitsInOrder(t *testing.T) {
	var got struct {
		path, q, mode, limit, status string
		labels                       []string
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			respondJSON(t, w, map[string]any{"api_schema_version": "0.61.0"})
			return
		}
		query := r.URL.Query()
		got.path, got.q, got.mode = r.URL.Path, query.Get("q"), query.Get("mode")
		got.limit, got.status, got.labels = query.Get("limit"), query.Get("status"), query["label"]
		respondJSON(t, w, map[string]any{
			"query": "payments", "mode": "lexical", "degraded": true,
			"degraded_reason": "embedding service unreachable", "complete": false, "truncated": true,
			"results": []map[string]any{
				{"issue": map[string]any{"uid": "u-2", "project_id": 7, "short_id": "bbb2", "title": "second"}, "matched_in": []string{"title"}},
				{"issue": map[string]any{"uid": "u-1", "project_id": 7, "short_id": "aaa1", "title": "first"}, "matched_in": []string{"title"}},
			},
		})
	})

	answer, err := c.SearchIssues(t.Context(), 7, SearchQuery{
		Query: "payments", Mode: "auto", Limit: 50, Status: "open", Labels: []string{"bug", "ui"},
	})
	require.NoError(t, err)

	require.Equal(t, "/api/v1/projects/7/search", got.path)
	require.Equal(t, "payments", got.q)
	require.Equal(t, "auto", got.mode)
	require.Equal(t, "50", got.limit)
	require.Equal(t, "open", got.status)
	require.Equal(t, []string{"bug", "ui"}, got.labels)
	require.Equal(t, []string{"u-2", "u-1"}, []string{answer.Issues[0].UID, answer.Issues[1].UID})
	require.Equal(t, "lexical", answer.Mode)
	require.True(t, answer.Degraded)
	require.Equal(t, "embedding service unreachable", answer.DegradedReason)
	require.True(t, answer.Truncated)
}

func TestClient_SearchIssues_OldDaemonGetsUpgradeErrorNotAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		query   SearchQuery
		wantErr string
	}{
		{"status needs 0.20.0", "0.19.0", SearchQuery{Query: "q", Status: "open"}, "0.20.0"},
		{"labels need 0.8.0", "0.7.0", SearchQuery{Query: "q", Labels: []string{"bug"}}, "0.8.0"},
		{"status on 0.20.0 passes", "0.20.0", SearchQuery{Query: "q", Status: "closed"}, ""},
		{"plain query needs no version", "0.1.0", SearchQuery{Query: "q"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searches := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					respondJSON(t, w, map[string]any{"api_schema_version": tc.version})
					return
				}
				searches++
				respondJSON(t, w, map[string]any{
					"query": "q", "mode": "lexical", "complete": true, "truncated": false, "results": []any{},
				})
			})
			_, err := c.SearchIssues(t.Context(), 7, tc.query)
			if tc.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, 1, searches)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
			require.ErrorContains(t, err, "upgrade the daemon")
			require.Zero(t, searches, fmt.Sprintf("%s: the route must not be asked", tc.name))
		})
	}
}

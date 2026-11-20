package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

func (a searchModelAPI) ReadIssue(ctx context.Context, pid int64, ref string) (*Issue, error) {
	return a.fake.ReadIssue(ctx, pid, ref)
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

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	require.Empty(t, api.searchCalls, "typing alone must not ask the daemon")
	m = settleSearch(m, cmds...)

	require.Equal(t, []SearchQuery{{Query: "payments", Limit: 50}}, api.searchCalls)
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

func TestDaemonSearch_StatusAndLabelFiltersNarrowHitsAnyOf(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, beta, gamma := searchTestIssues()
	alpha.Labels = []string{"bug"}
	beta.Labels = []string{"ui"}
	gamma.Labels = []string{"bug"}
	gamma.Status = "closed"
	m.list.issues = []Issue{alpha, beta, gamma}
	api.searchFn = answerWith("hybrid", gamma, alpha, beta)
	m.list.filter = ListFilter{Status: "open", Labels: []string{"bug", "ui"}}

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)

	require.Equal(t, []SearchQuery{{Query: "payments", Limit: 50}}, api.searchCalls, "filters stay out of the ask")
	require.Equal(t, []string{"01TEST-aaa1", "01TEST-bbb2"}, visibleUIDs(m), "either label keeps a hit; closed drops")
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

func TestDaemonSearch_AllProjectsKeyDoesNothing(t *testing.T) {
	m, api := daemonSearchFixture(t)
	m.scope = scope{allProjects: true}
	m = openBarFromCmd(t, m, '/')

	m, cmd := stepModel(m, ctrlT)
	require.Nil(t, cmd)
	require.Empty(t, m.list.rankedNote())

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

func TestDaemonSearch_EscOnPendingAskLetsReopeningAskAgain(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, _ := searchTestIssues()
	api.searchFn = answerWith("hybrid", alpha)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "foo")
	m, ask := stepModel(m, onlyMsg(t, cmds[len(cmds)-1]))
	inFlight := onlyMsg(t, ask)

	// Commit while the answer is still out, then open the bar and cancel it.
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m, _ = stepModel(m, inFlight)
	require.Empty(t, visibleUIDs(m), "the dropped answer leaves the committed text filter")

	out, open := m.Update(runeKey('/'))
	m, reask := stepModel(out.(Model), open())
	m = settleSearch(m, reask)
	require.Len(t, api.searchCalls, 2, "reopening asks again")
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))
	require.Contains(t, m.list.rankedNote(), "ranked by words and meaning")
}

func TestDaemonSearch_LateAnswerDoesNotMoveSelectionUnderAnotherView(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", alpha, gamma)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "login")
	m, ask := stepModel(m, onlyMsg(t, cmds[len(cmds)-1]))
	inFlight := onlyMsg(t, ask)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	_, uid, _ := highlightedIdentity(m.list)
	require.Equal(t, "01TEST-ccc3", uid)

	m, _ = stepModel(m, runeKey('?'))
	require.Equal(t, viewHelp, m.view)
	m, _ = stepModel(m, inFlight)

	_, uid, _ = highlightedIdentity(m.list)
	require.Equal(t, "01TEST-ccc3", uid, "the answer must not re-rank rows behind the help overlay")
}

func TestDaemonSearch_AnswerFollowsSelectionInSplitDetail(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma, alpha)
	m = resizeModel(m, 160, 40)
	require.Equal(t, splitlayout.Split, m.layout)

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m, ask := stepModel(m, onlyMsg(t, cmds[len(cmds)-1]))
	inFlight := onlyMsg(t, ask)
	// Moving into the results points the detail pane at the unfiltered list's row.
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyDown})
	_, before, _ := highlightedIdentity(m.list)
	require.NotEqual(t, "01TEST-ccc3", before)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, before, m.detail.issue.UID)

	m, _ = stepModel(m, inFlight)

	require.Equal(t, "01TEST-ccc3", m.detail.issue.UID, "the detail pane follows the first ranked hit")
}

func TestDaemonSearch_StatusKeyNarrowsCommittedHitsAndKeepsThem(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, beta, gamma := searchTestIssues()
	alpha.Status = "closed"
	m.list.issues = []Issue{alpha, beta, gamma}
	api.searchFn = answerWith("hybrid", gamma, alpha, beta)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, []string{"01TEST-ccc3", "01TEST-aaa1", "01TEST-bbb2"}, visibleUIDs(m))

	m, _ = stepModel(m, runeKey('s'))

	require.Equal(t, "open", m.list.filter.Status)
	require.Equal(t, []string{"01TEST-ccc3", "01TEST-bbb2"}, visibleUIDs(m), "ranked hits, narrowed by status")
	require.Contains(t, m.list.rankedNote(), "ranked by words and meaning")
}

func TestDaemonSearch_ToggleOffInResultsFocusFollowsSelectionInSplitDetail(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma, alpha)
	m = resizeModel(m, 160, 40)
	require.Equal(t, splitlayout.Split, m.layout)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "login")
	m = settleSearch(m, cmds...)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyDown})
	require.NotNil(t, m.detail.issue)
	require.Equal(t, "01TEST-aaa1", m.detail.issue.UID, "the detail pane follows the second ranked hit")

	m, _ = stepModel(m, ctrlT)

	require.Equal(t, []string{"01TEST-ccc3"}, visibleUIDs(m), "the text filters loaded rows again")
	require.NotNil(t, m.detail.issue)
	require.Equal(t, "01TEST-ccc3", m.detail.issue.UID)
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

func TestDaemonSearch_RefreshUpdatesHitsAndDropsMissingOnes(t *testing.T) {
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
	require.Len(t, rows, 1, "a hit the complete refresh no longer returns is gone")
	require.Equal(t, "01TEST-aaa1", rows[0].issue.UID)
	require.Equal(t, "fix alpha, renamed", rows[0].issue.Title)
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

func TestDaemonSearch_FooterHintFollowsTheKey(t *testing.T) {
	askHint := helplayout.HelpItem{Key: "ctrl+t", Description: "ask daemon"}
	rowsHint := helplayout.HelpItem{Key: "ctrl+t", Description: "filter rows"}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, m Model) Model
		want  *helplayout.HelpItem
	}{
		{"bar open over one project", func(t *testing.T, m Model) Model { return openBarFromCmd(t, m, '/') }, &askHint},
		{"results focus", func(t *testing.T, m Model) Model {
			m = openBarFromCmd(t, m, '/')
			m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyDown})
			return m
		}, &askHint},
		{"toggled on offers the way back", func(t *testing.T, m Model) Model {
			m = openBarFromCmd(t, m, '/')
			m, _ = stepModel(m, ctrlT)
			return m
		}, &rowsHint},
		{"all projects", func(t *testing.T, m Model) Model {
			m.scope = scope{allProjects: true}
			return openBarFromCmd(t, m, '/')
		}, nil},
		{"bar closed", func(_ *testing.T, m Model) Model { return m }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := daemonSearchFixture(t)
			m = tc.setup(t, m)
			rows := flattenHelpRows(m.helpRows())
			if tc.want == nil {
				for _, item := range rows {
					require.NotEqual(t, "ctrl+t", item.Key, "no hint when the toggle cannot work: %+v", rows)
				}
				return
			}
			assertHelpItemPresent(t, rows, *tc.want)
		})
	}
}

func TestDaemonSearch_HitsBeyondTheLoadedListAreReadBeforeFiltering(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, _ := searchTestIssues()
	alpha.Labels = []string{"bug"}
	m.list.issues = []Issue{alpha}
	far := Issue{ProjectID: 7, UID: "01TEST-far1", ShortID: "far1", Title: "beyond the limit", Status: "open"}
	deleted := Issue{ProjectID: 7, UID: "01TEST-del1", ShortID: "del1", Title: "deleted", Status: "open"}
	moved := Issue{ProjectID: 7, UID: "01TEST-mov1", ShortID: "mov1", Title: "moved", Status: "open"}
	api.searchFn = answerWith("hybrid", far, alpha, deleted, moved)
	api.readFn = func(ref string) (*Issue, error) {
		switch ref {
		case far.UID:
			labelled := far
			labelled.Labels = []string{"bug"}
			return &labelled, nil
		case moved.UID:
			elsewhere := moved
			elsewhere.ProjectID = 8
			return &elsewhere, nil
		}
		return nil, &APIError{Status: http.StatusNotFound}
	}
	m.list.filter.Labels = []string{"bug"}

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)

	require.Equal(t, []string{"01TEST-far1", "01TEST-aaa1"}, visibleUIDs(m), "daemon order; gone and moved hits dropped")
	require.ElementsMatch(t, []string{far.UID, deleted.UID, moved.UID}, api.readRefs, "loaded hits are not read")
}

func TestDaemonSearch_RefreshRechecksHitsMissingFromATruncatedList(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, _ := searchTestIssues()
	m.list.issues = []Issue{alpha}
	far := Issue{ProjectID: 7, UID: "01TEST-far1", ShortID: "far1", Title: "beyond the limit", Status: "open"}
	api.searchFn = answerWith("hybrid", far, alpha)
	api.readFn = func(string) (*Issue, error) { return &far, nil }
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, []string{"01TEST-far1", "01TEST-aaa1"}, visibleUIDs(m))

	// Someone else deletes the far issue; the refresh holds only the first 2,000.
	api.readFn = func(string) (*Issue, error) { return nil, &APIError{Status: http.StatusNotFound} }
	refetched := []Issue{alpha}
	for i := range queueWorkingSetLimit {
		refetched = append(refetched, Issue{ProjectID: 7, UID: fmt.Sprintf("01FILL-%04d", i), ShortID: fmt.Sprintf("f%04d", i), Status: "open"})
	}
	m, cmd := stepModel(m, refetchedMsg{dispatchKey: m.currentCacheKey(), issues: refetched})
	require.True(t, m.list.truncated)
	require.Equal(t, []string{"01TEST-far1", "01TEST-aaa1"}, visibleUIDs(m), "an incomplete list cannot say it is gone")
	for _, msg := range runCmdTree(cmd) {
		if recheck, ok := msg.(daemonSearchRecheckMsg); ok {
			m, _ = stepModel(m, recheck)
		}
	}

	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))
}

func TestDaemonSearch_EscRestoresRankedHitsBroughtUpToDate(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma, alpha)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, refetchedMsg{dispatchKey: m.currentCacheKey(), issues: []Issue{alpha}})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m), "the issue deleted meanwhile does not return")
}

func TestDaemonSearch_CommitWhileAskPendingFollowsSelectionInSplitDetail(t *testing.T) {
	m, api := daemonSearchFixture(t)
	_, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma)
	m = resizeModel(m, 160, 40)
	require.Equal(t, splitlayout.Split, m.layout)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "login")
	m, _ = stepModel(m, onlyMsg(t, cmds[len(cmds)-1])) // the ask goes out; its answer never arrives
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, m.detail.issue)
	require.NotEqual(t, "01TEST-ccc3", m.detail.issue.UID, "the pane follows the unfiltered row while asking")

	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Equal(t, []string{"01TEST-ccc3"}, visibleUIDs(m), "the committed text filters loaded rows")
	require.Equal(t, "01TEST-ccc3", m.detail.issue.UID)
}

func TestDaemonSearch_AnswerForEarlierTextIsDroppedAndAskedAgain(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, _ := searchTestIssues()
	api.searchFn = answerWith("hybrid", alpha)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "pay")
	m, ask := stepModel(m, onlyMsg(t, cmds[len(cmds)-1]))
	slow := onlyMsg(t, ask)
	m, _ = typeText(m, "ments") // the next ask has not gone out yet

	m, _ = stepModel(m, slow)

	require.False(t, m.list.ranked.hasAnswer, "an answer for \"pay\" must not replace rows for \"payments\"")
	require.Len(t, visibleUIDs(m), 3)
	require.Empty(t, m.list.ranked.asked, "typing \"pay\" again would ask again")
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
			api.searchFn = answerWith("lexical", gamma)
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
			if !tc.allProjects {
				var cmds []tea.Cmd
				m, cmds = typeText(m, "pay")
				m = settleSearch(m, cmds...)
			}

			view := stripANSI(m.View().Content)
			if !tc.allProjects {
				require.Contains(t, view, "ranked by words only")
			}
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

func TestClient_SearchIssues_SendsOnlyQueryAndLimitAndDecodesHitsInOrder(t *testing.T) {
	var path string
	var query url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.Query()
		respondJSON(t, w, map[string]any{
			"query": "payments", "mode": "hybrid", "complete": true, "truncated": false,
			"results": []map[string]any{
				{"issue": map[string]any{"uid": "u-2", "project_id": 7, "short_id": "bbb2", "title": "second"}, "matched_in": []string{"title"}},
				{"issue": map[string]any{"uid": "u-1", "project_id": 7, "short_id": "aaa1", "title": "first"}, "matched_in": []string{"title"}},
			},
		})
	})

	answer, err := c.SearchIssues(t.Context(), 7, SearchQuery{Query: "payments", Limit: 50})
	require.NoError(t, err)

	require.Equal(t, "/api/v1/projects/7/search", path)
	require.Equal(t, url.Values{"q": {"payments"}, "limit": {"50"}}, query)
	require.Equal(t, []string{"u-2", "u-1"}, []string{answer.Issues[0].UID, answer.Issues[1].UID})
	require.Equal(t, "hybrid", answer.Mode)
}

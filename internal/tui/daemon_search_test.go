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
	m.list.loading = false
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

func TestDaemonSearch_FailureBeforeAnyAnswerShowsOnceAtEverySize(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
	}{
		{"full height", 80, 24},
		{"compact", 100, 8},
		{"split", 160, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := daemonSearchFixture(t)
			api.searchFn = func(SearchQuery) (*SearchAnswer, error) { return nil, errors.New("embedding service refused") }
			m = resizeModel(m, tc.width, tc.height)
			m = openBarFromCmd(t, m, '/')
			m, _ = stepModel(m, ctrlT)
			m, cmds := typeText(m, "pay")
			m = settleSearch(m, cmds...)
			require.Equal(t, 1, strings.Count(stripANSI(m.View().Content), "search failed"))

			// A later reply that takes the status line must not hide the failure.
			m.list.status = "closed #42"
			require.Equal(t, 1, strings.Count(stripANSI(m.View().Content), "search failed"))
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

	m, _ = stepModel(m, runeKey('c'))
	require.ElementsMatch(t, []string{"01TEST-aaa1", "01TEST-bbb2", "01TEST-ccc3"}, visibleUIDs(m))
	require.Empty(t, m.list.rankedNote(), "clearing the filters clears the results")
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

func TestDaemonSearch_AnswerBeforeFirstFetchOnATruncatedListCountsHitsBeyondAndFollowsRefetches(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, _ := searchTestIssues()
	far1 := Issue{ProjectID: 7, UID: "01TEST-far1", ShortID: "far1", Status: "open"}
	far2 := Issue{ProjectID: 7, UID: "01TEST-far2", ShortID: "far2", Status: "open"}
	api.searchFn = answerWith("hybrid", far1, alpha, far2)
	m.list = newListModel()
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})

	// The first fetch holds the first 2,000 issues only: alpha and fillers.
	loaded := []Issue{alpha}
	for i := len(loaded); i <= queueWorkingSetLimit; i++ {
		loaded = append(loaded, Issue{ProjectID: 7, UID: fmt.Sprintf("01FILL-%04d", i), ShortID: fmt.Sprintf("f%04d", i), Status: "open"})
	}
	m.list = m.list.applyFetched(initialFetchMsg{issues: loaded})
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))
	require.Contains(t, m.list.rankedNote(), "2 hits beyond the loaded issues")

	// far1 is edited and now sorts into the loaded rows.
	m.list = m.list.applyFetched(refetchedMsg{issues: append([]Issue{far1}, loaded[:queueWorkingSetLimit]...)})
	require.Equal(t, []string{"01TEST-far1", "01TEST-aaa1"}, visibleUIDs(m), "the hit enters in the daemon's order")
	require.Contains(t, m.list.rankedNote(), "1 hit beyond the loaded issues")
}

func TestDaemonSearch_HitDroppedLikeADeleteReturnsInDaemonOrderWhenRefetchedAgain(t *testing.T) {
	m, api := daemonSearchFixture(t)
	alpha, _, gamma := searchTestIssues()
	api.searchFn = answerWith("hybrid", gamma, alpha)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT)
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m.list = m.list.droppingIssues(gamma.UID, []string{gamma.UID})
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))

	// An undo brings the issue back with the next fetch.
	m.list = m.list.applyFetched(refetchedMsg{issues: append([]Issue{alpha}, gamma)})
	require.Equal(t, []string{"01TEST-ccc3", "01TEST-aaa1"}, visibleUIDs(m))
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
	require.Len(t, visibleUIDs(m), 3, "an answer for \"pay\" must not replace rows for \"payments\"")

	var erase []tea.Cmd
	for range "ments" {
		var cmd tea.Cmd
		m, cmd = stepModel(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
		erase = append(erase, cmd)
	}
	m = settleSearch(m, erase...)
	require.Len(t, api.searchCalls, 2, "typing \"pay\" again asks again")
	require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))
}

func TestDaemonSearch_AnswerBeforeTheFirstFetchOfASwitchedProjectKeepsHits(t *testing.T) {
	m, api := daemonSearchFixture(t)
	injectProjects(&m, mockProject{ID: 5, Name: "spoke-project", Ident: "..."})
	hit := Issue{ProjectID: 5, UID: "01TEST-spk1", ShortID: "spk1", Title: "in the spoke", Status: "open"}
	api.searchFn = answerWith("hybrid", hit)
	m = openBarFromCmd(t, m, '/')
	m, _ = stepModel(m, ctrlT) // daemon search lasts for the session
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	m, _ = updateModel(m, keyRune('P'))
	m.projectsCursor = cursorForTestProject(t, m, 5)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, int64(5), m.scope.projectID)
	m = openBarFromCmd(t, m, '/')
	m, cmds := typeText(m, "payments")
	m = settleSearch(m, cmds...)
	require.Equal(t, []string{"01TEST-spk1"}, visibleUIDs(m), "the list has not loaded yet, so the hit is not outside it")

	m, _ = stepModel(m, initialFetchMsg{dispatchKey: m.currentCacheKey(), issues: []Issue{hit}})
	require.Equal(t, []string{"01TEST-spk1"}, visibleUIDs(m))
}

func TestDaemonSearch_HitsBeyondTheLoadedIssuesAreNotShownAndTheNoteCountsThem(t *testing.T) {
	for _, tc := range []struct {
		name      string
		truncated bool
		wantNote  bool
	}{
		{"truncated list says so", true, true},
		{"complete list stays quiet", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := daemonSearchFixture(t)
			alpha, _, _ := searchTestIssues()
			m.list.issues = []Issue{alpha}
			m.list.truncated = tc.truncated
			far1 := Issue{ProjectID: 7, UID: "01TEST-far1", ShortID: "far1", Status: "open"}
			far2 := Issue{ProjectID: 7, UID: "01TEST-far2", ShortID: "far2", Status: "open"}
			api.searchFn = answerWith("hybrid", far1, alpha, far2)

			m = openBarFromCmd(t, m, '/')
			m, _ = stepModel(m, ctrlT)
			m, cmds := typeText(m, "payments")
			m = settleSearch(m, cmds...)

			require.Equal(t, []string{"01TEST-aaa1"}, visibleUIDs(m))
			if tc.wantNote {
				require.Contains(t, m.list.rankedNote(), "2 hits beyond the loaded issues")
			} else {
				require.NotContains(t, m.list.rankedNote(), "beyond")
			}
		})
	}
}

func TestDaemonSearch_RendersCompletelyAtEverySize(t *testing.T) {
	longName := strings.Repeat("example-project-", 5)
	for _, tc := range []struct {
		name          string
		width, height int
		split         bool
		wantFooter    bool
	}{
		{"80x24", 80, 24, false, true},
		{"80x12", 80, 12, false, true},
		{"100x8", 100, 8, false, false},
		{"split 160x40", 160, 40, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := daemonSearchFixture(t)
			_, _, gamma := searchTestIssues()
			api.searchFn = answerWith("lexical", gamma)
			m.scope = scope{projectID: 7, projectName: longName}
			m = resizeModel(m, tc.width, tc.height)
			if tc.split {
				require.Equal(t, splitlayout.Split, m.layout)
			}
			m = openBarFromCmd(t, m, '/')
			m, _ = stepModel(m, ctrlT)
			m, cmds := typeText(m, "pay")
			m = settleSearch(m, cmds...)

			view := stripANSI(m.View().Content)
			require.Contains(t, view, "ranked by words only")
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

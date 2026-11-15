package tui

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/splitlayout"
)

// moveTestAPI answers the project list and move requests of the Move form
// and records every move request it receives.
type moveTestAPI struct {
	*Client
	projects []ProjectSummary
	calls    []MoveInput
	// preview and move answer the dry run and the real move; nil means a
	// preview of the issue alone at revision 5 and a move to "new1".
	preview func(call int, in MoveInput) (*MutationResp, error)
	move    func(call int, in MoveInput) (*MutationResp, error)
}

func (a *moveTestAPI) ListProjects(context.Context) ([]ProjectSummary, error) {
	return a.projects, nil
}

func (a *moveTestAPI) MoveIssue(_ context.Context, _ int64, _ string, in MoveInput) (*MutationResp, error) {
	a.calls = append(a.calls, in)
	if in.DryRun {
		if a.preview != nil {
			return a.preview(len(a.calls), in)
		}
		return &MutationResp{Issue: &Issue{UID: "issue-b", ShortID: "bbbb"}, Revision: 5}, nil
	}
	if a.move != nil {
		return a.move(len(a.calls), in)
	}
	return &MutationResp{Issue: &Issue{UID: "issue-b", ShortID: "new1"}, NewShortID: "new1", Changed: true}, nil
}

func (a *moveTestAPI) dryRuns() int {
	n := 0
	for _, call := range a.calls {
		if call.DryRun {
			n++
		}
	}
	return n
}

func moveProject(id int64, name, role, area string) ProjectSummary {
	p := ProjectSummary{ID: id, Name: name, UID: fmt.Sprintf("uid-%d", id)}
	if role != "" {
		p.Metadata.Role = jsontext.Value(`"` + role + `"`)
	}
	if area != "" {
		p.Metadata.Area = jsontext.Value(`"` + area + `"`)
	}
	return p
}

func moveModel(t *testing.T, api *moveTestAPI) Model {
	t.Helper()
	m := newTestModel()
	m.api = newUndoClient(api)
	m.view = viewList
	m.list.actor = "alice"
	m.list.issues = []Issue{
		{UID: "issue-a", ProjectID: 7, ProjectUID: "uid-7", ShortID: "aaaa", QualifiedID: "source#aaaa", Title: "First", Revision: 3},
		{UID: "issue-b", ProjectID: 7, ProjectUID: "uid-7", ShortID: "bbbb", QualifiedID: "source#bbbb", Title: "Second", Revision: 4},
		{UID: "issue-c", ProjectID: 7, ProjectUID: "uid-7", ShortID: "cccc", QualifiedID: "source#cccc", Title: "Third", Revision: 5},
	}
	m.list = m.list.syncSelection(m.list.visibleRows())
	m = selectListUID(t, m, "issue-b")
	return m
}

func selectListUID(t *testing.T, m Model, uid string) Model {
	t.Helper()
	rows := m.list.visibleRows()
	for i, row := range rows {
		if row.issue.UID == uid {
			m.list = m.list.selectVisibleRow(rows, i)
			return m
		}
	}
	require.Failf(t, "row missing", "no list row %s", uid)
	return m
}

// deliver runs cmd and hands its message back to the model.
func deliver(t *testing.T, m Model, cmd tea.Cmd) (Model, tea.Cmd) {
	t.Helper()
	require.NotNil(t, cmd)
	return updateModel(m, cmd())
}

// openMove presses M and delivers the project list.
func openMove(t *testing.T, m Model) Model {
	t.Helper()
	m, cmd := updateModel(m, runeKey('M'))
	require.Equal(t, modalMoveIssue, m.modal, "M opens the move picker")
	m, _ = deliver(t, m, cmd)
	return m
}

func enter(m Model) (Model, tea.Cmd) {
	return updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
}

// flattenModal joins a rendered box's wrapped text back into one line.
func flattenModal(view string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(view)), " ")
}

func moveLabels(m Model) []string {
	var out []string
	for _, c := range m.move.choices() {
		out = append(out, c.label)
	}
	return out
}

func TestMovePickerOffersOnlyProjectsAnIssueCanMoveTo(t *testing.T) {
	api := &moveTestAPI{projects: []ProjectSummary{
		moveProject(7, "own-project", "", ""),
		moveProject(1, "inbox", "inbox", ""),
		moveProject(2, "hub-project", "", ""),
	}}
	m := openMove(t, moveModel(t, api))
	require.Equal(t, []string{"hub-project"}, moveLabels(m))
}

func TestMovePickerShowsAreaToTellSameNamedProjectsApart(t *testing.T) {
	api := &moveTestAPI{projects: []ProjectSummary{
		moveProject(2, "docs", "", "work"),
		moveProject(3, "Docs", "", "home"),
		moveProject(4, "DOCS", "", "home"),
		moveProject(6, "solo", "", "work"),
		moveProject(11, "notes", "", "work "),
		moveProject(12, "Notes", "", "work"),
		moveProject(13, "wiki", "", " home "),
		moveProject(14, "WIKI", "", "play"),
	}}
	m := openMove(t, moveModel(t, api))
	require.Equal(t, []string{
		"docs · work",
		"Docs · uid-3",
		"DOCS · uid-4",
		"notes · uid-11",
		"Notes · uid-12",
		"solo",
		"wiki · home",
		"WIKI · play",
	}, moveLabels(m), "same-named projects (any case) get an area only when it is theirs alone, ignoring surrounding spaces; otherwise the UID")

	m = typeString(m, "WORK")
	require.Equal(t, []string{"docs · work"}, moveLabels(m), "the filter matches the area shown beside a name, case-insensitively")

	m, _ = updateModel(m, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m = typeString(m, "so")
	require.Equal(t, []string{"solo"}, moveLabels(m), "the filter matches names")
}

func TestMovePreviewNamesTheMoveAndListsDescendants(t *testing.T) {
	family := func(kids ...Issue) func(int, MoveInput) (*MutationResp, error) {
		return func(int, MoveInput) (*MutationResp, error) {
			return &MutationResp{
				Issue:    &Issue{UID: "issue-b", ShortID: "bbbb", ProjectID: 7},
				Issues:   append([]Issue{{UID: "issue-b", ShortID: "bbbb", Title: "Second", ProjectID: 7}}, kids...),
				Revision: 4,
			}, nil
		}
	}
	for _, tc := range []struct {
		name    string
		preview func(int, MoveInput) (*MutationResp, error)
		want    []string
		notWant string
	}{
		{"lone issue", nil, []string{"has no descendants, so it moves alone"}, "This issue and its"},
		{"one descendant", family(Issue{UID: "kid-1", ShortID: "k001", Title: "Child one", ProjectID: 7}),
			[]string{"This issue and its 1 descendant move", "#k001 Child one"}, "1 descendants"},
		{"several descendants", family(
			Issue{UID: "kid-1", ShortID: "k001", Title: "Child one", ProjectID: 7},
			Issue{UID: "kid-2", ShortID: "k002", Title: "Child two", ProjectID: 9}),
			[]string{"This issue and its 2 descendants move", "#k001 Child one", "#k002 Child two"}, "moves alone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}, preview: tc.preview}
			m := openMove(t, moveModel(t, api))
			m, cmd := enter(m)
			m, _ = deliver(t, m, cmd)

			view := flattenModal(stripANSI(renderMoveModal(m.move, 100, 40)))
			for _, want := range tc.want {
				require.Contains(t, view, want)
			}
			require.NotContains(t, view, tc.notWant)
			require.Len(t, api.calls, 1)
			require.True(t, api.calls[0].DryRun, "the first enter only previews")
			require.Zero(t, api.calls[0].IfMatchRevision)
			require.Equal(t, "uid-2", api.calls[0].ToProjectUID)
		})
	}
}

func TestMoveRefusedPreviewShowsDaemonWordsAndWritesNothing(t *testing.T) {
	api := &moveTestAPI{
		projects: []ProjectSummary{moveProject(2, "hub-project", "", "")},
		preview: func(int, MoveInput) (*MutationResp, error) {
			return nil, &APIError{Status: 409, Code: "recurrence_pinned",
				Message: "issue belongs to a recurrence", Hint: "unpin the issue from its recurrence before moving"}
		},
	}
	m := openMove(t, moveModel(t, api))
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)

	require.Equal(t, modalMoveIssue, m.modal)
	view := stripANSI(renderMoveModal(m.move, 120, 40))
	require.Contains(t, flattenModal(view),
		"recurrence_pinned: issue belongs to a recurrence: hint: unpin the issue from its recurrence before moving")

	_, cmd = enter(m)
	m, _ = deliver(t, m, cmd)
	for _, call := range api.calls {
		require.True(t, call.DryRun, "a refused preview never reaches a real move")
	}
	require.Equal(t, 2, api.dryRuns())
}

func TestMoveConflictPreviewsAgainAtCurrentRevision(t *testing.T) {
	api := &moveTestAPI{
		projects: []ProjectSummary{moveProject(2, "hub-project", "", "")},
		preview: func(call int, _ MoveInput) (*MutationResp, error) {
			revision := int64(5)
			if call > 2 {
				revision = 9
			}
			return &MutationResp{Issue: &Issue{UID: "issue-b", ShortID: "bbbb"}, Revision: revision}, nil
		},
		move: func(_ int, in MoveInput) (*MutationResp, error) {
			if in.IfMatchRevision != 9 {
				return nil, &APIError{Status: http.StatusPreconditionFailed, Code: "revision_conflict", Message: "issue revision is 9"}
			}
			return &MutationResp{Issue: &Issue{UID: "issue-b"}, NewShortID: "new1", Changed: true}, nil
		},
	}
	m := openMove(t, moveModel(t, api))
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	m, cmd = enter(m)
	m, cmd = deliver(t, m, cmd)

	require.Equal(t, modalMoveIssue, m.modal, "a conflict keeps the form open")
	require.NotNil(t, cmd, "a conflict asks for a fresh preview")
	m, _ = deliver(t, m, cmd)
	require.Contains(t, flattenModal(stripANSI(renderMoveModal(m.move, 100, 40))),
		"The issue's revision changed since the preview; nothing was moved.")

	m, cmd = enter(m)
	m, _ = deliver(t, m, cmd)
	require.Equal(t, modalNone, m.modal)

	require.Len(t, api.calls, 4)
	require.True(t, api.calls[0].DryRun)
	require.Equal(t, int64(5), api.calls[1].IfMatchRevision, "the move is guarded on the previewed revision")
	require.True(t, api.calls[2].DryRun)
	require.Zero(t, api.calls[2].IfMatchRevision, "the re-preview reads the current revision")
	require.Equal(t, int64(9), api.calls[3].IfMatchRevision)
}

func TestMoveSelectsNeighbourRowAndNamesNewRef(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected string
		moved    []string
		wantNext string
	}{
		{"next row", "issue-b", []string{"issue-b"}, "issue-c"},
		{"last row takes the one before", "issue-c", []string{"issue-c"}, "issue-b"},
		{"descendants moving with it are skipped", "issue-b", []string{"issue-b", "issue-c"}, "issue-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &moveTestAPI{
				projects: []ProjectSummary{moveProject(2, "hub-project", "", "")},
				preview: func(int, MoveInput) (*MutationResp, error) {
					resp := &MutationResp{Issue: &Issue{UID: tc.moved[0]}, Revision: 5}
					for _, uid := range tc.moved {
						resp.Issues = append(resp.Issues, Issue{UID: uid, ShortID: "x"})
					}
					return resp, nil
				},
			}
			m := selectListUID(t, moveModel(t, api), tc.selected)
			m = openMove(t, m)
			m, cmd := enter(m)
			m, _ = deliver(t, m, cmd)
			m, cmd = enter(m)
			m, follow := deliver(t, m, cmd)

			require.Equal(t, modalNone, m.modal)
			require.Equal(t, tc.wantNext, m.list.selectedUID)
			for _, row := range m.list.visibleRows() {
				require.NotContains(t, tc.moved, row.issue.UID, "a moved issue leaves the list until the refetch lists it again")
			}
			require.NotNil(t, follow, "the list is fetched again")
			require.NotNil(t, m.toast)
			require.Contains(t, m.toast.text, "hub-project#new1")
		})
	}
}

// moveThrough previews and performs a move of the selected issue to
// hub-project, answering with a preview that lists the given uids.
func moveThrough(t *testing.T, m Model, previewed ...string) (Model, tea.Cmd) {
	t.Helper()
	api := &moveTestAPI{
		projects: []ProjectSummary{moveProject(2, "hub-project", "", "")},
		preview: func(int, MoveInput) (*MutationResp, error) {
			resp := &MutationResp{Issue: &Issue{UID: "issue-b"}, Revision: 5}
			for _, uid := range previewed {
				resp.Issues = append(resp.Issues, Issue{UID: uid, ShortID: "x"})
			}
			return resp, nil
		},
	}
	m.api = newUndoClient(api)
	m = openMove(t, m)
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	m, cmd = enter(m)
	return deliver(t, m, cmd)
}

func TestMoveLeavesASplitDetailOnTheSameTerms(t *testing.T) {
	splitModel := func(t *testing.T, shown string, back ...string) Model {
		t.Helper()
		m := moveModel(t, &moveTestAPI{})
		m.layout, m.focus = splitlayout.Split, focusList
		m = selectListUID(t, m, "issue-b")
		for _, row := range m.list.visibleRows() {
			if row.issue.UID == shown {
				issue := row.issue
				m.detail = detailModel{scopePID: 7, issue: &issue}
			}
		}
		for _, uid := range back {
			m.detail.navStack = append(m.detail.navStack, detailModel{issue: &Issue{UID: uid}})
		}
		return m
	}
	// A reset pane follows the neighbour the selection moved to, without Back
	// history. (The moved issue's own pane is covered by the selection test.)
	t.Run("a pane showing a descendant that moved with it resets", func(t *testing.T) {
		m, _ := moveThrough(t, splitModel(t, "issue-c", "issue-a"), "issue-b", "issue-c")
		require.NotNil(t, m.detail.issue)
		require.Equal(t, "issue-a", m.detail.issue.UID)
		require.Empty(t, m.detail.navStack)
	})
	t.Run("a pane with the moved issue in its Back history resets", func(t *testing.T) {
		m, _ := moveThrough(t, splitModel(t, "issue-a", "issue-b"))
		require.NotNil(t, m.detail.issue)
		require.Equal(t, "issue-c", m.detail.issue.UID)
		require.Empty(t, m.detail.navStack)
	})
	t.Run("a pane on an unrelated issue keeps it and its Back history", func(t *testing.T) {
		m, _ := moveThrough(t, splitModel(t, "issue-a", "issue-x"))
		require.NotNil(t, m.detail.issue)
		require.Equal(t, "issue-a", m.detail.issue.UID)
		require.Len(t, m.detail.navStack, 1)
	})
}

func TestMoveClearsUndoHistory(t *testing.T) {
	api := &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}}
	m := moveModel(t, api)
	m.undoHistory.push(undoEntry{kind: "priority.set", uid: "issue-a"})
	m = openMove(t, m)
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	require.Len(t, m.undoHistory.entries, 1, "a preview is not a boundary")
	m, cmd = enter(m)
	m, _ = deliver(t, m, cmd)
	require.Empty(t, m.undoHistory.entries, "a move changes the issue's qualified reference, so earlier edits can no longer be undone")
}

func TestMoveIssueNeedsDaemonWithSubtreeMoves(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.34.0"}`)
			return
		}
		posts.Add(1)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, srv.Client()).MoveIssue(t.Context(), 7, "bbbb",
		MoveInput{Actor: "alice", ToProjectUID: "uid-2", DryRun: true})
	require.ErrorContains(t, err, "0.35.0")
	require.ErrorContains(t, err, "upgrade the daemon")
	require.Zero(t, posts.Load(), "an old daemon is never sent a move it would run as a single-issue move")
}

func TestMoveIsRefusedWhenTheIssueChangedAfterThePreviewAgainstDaemon(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	base := NewClient(env.URL, env.HTTP)
	source, err := env.DB.CreateProject(ctx, "example-workspace")
	require.NoError(t, err)
	_, err = env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Guarded", Author: "alice"})
	require.NoError(t, err)

	client := newConnectedUndoClient(t, base)
	m := initialModel(Options{})
	m.api = client
	m.view = viewList
	m.list.actor = "alice"
	m.scope = scope{projectID: source.ID}
	detail, err := client.GetIssueDetail(ctx, source.ID, issue.UID)
	require.NoError(t, err)
	m.list.issues = []Issue{*detail.Issue}
	m = selectListUID(t, m, issue.UID)

	m = openMove(t, m)
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	// A dates edit advances the issue revision the move is guarded on.
	_, err = base.SetDates(ctx, source.ID, issue.UID, DatesInput{
		Actor: "alice", IfMatchRevision: issue.Revision, Set: map[string]*string{"scheduled": new("2030-01-01")},
	})
	require.NoError(t, err)
	m, cmd = enter(m)
	m, cmd = deliver(t, m, cmd)
	m, _ = deliver(t, m, cmd)

	stored, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, source.ID, stored.ProjectID, "a stale preview moves nothing")
	require.Equal(t, modalMoveIssue, m.modal, "the form stays open on a fresh preview")
	require.NotNil(t, m.move.preview, "and shows the preview at the current revision")
	require.Contains(t, m.move.note, "revision changed since the preview")
}

func TestMoveFooterNamesWhatEnterDoesAtEachStep(t *testing.T) {
	api := &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}}
	enterAction := func(m Model) string {
		for _, item := range flattenHelpRows(m.helpRows()) {
			if item.Key == "enter" {
				return item.Description
			}
		}
		return ""
	}
	m := openMove(t, moveModel(t, api))
	require.Equal(t, "preview", enterAction(m), "in the picker enter sends a dry run")
	m, cmd := enter(m)
	require.Empty(t, flattenHelpRows(m.helpRows()), "no keys are offered while the preview request is in flight")
	m, _ = deliver(t, m, cmd)
	require.Equal(t, "move", enterAction(m), "in the preview enter moves")
	m, cmd = enter(m)
	require.Empty(t, flattenHelpRows(m.helpRows()), "no keys are offered while the move request is in flight")
	_, _ = deliver(t, m, cmd)
}

// TestMoveFormRendersCompletelyAtEveryTerminalSize follows the Delete form's
// rule: the modal fits the terminal, with a long destination name, and the
// outcome and the enter/esc prompt stay visible.
func TestMoveFormRendersCompletelyAtEveryTerminalSize(t *testing.T) {
	defer snapshotInit(t)()
	longName := strings.Repeat("example-destination-", 5) + "project"
	kids := []Issue{
		{UID: "kid-1", ShortID: "k001", Title: "Child one", ProjectID: 7},
		{UID: "kid-2", ShortID: "k002", Title: "Child two", ProjectID: 7},
		{UID: "kid-3", ShortID: "k003", Title: "Child three", ProjectID: 7},
		{UID: "kid-4", ShortID: "k004", Title: "Child four", ProjectID: 7},
	}
	api := &moveTestAPI{
		projects: []ProjectSummary{moveProject(2, longName, "", "")},
		preview: func(int, MoveInput) (*MutationResp, error) {
			return &MutationResp{
				Issues:   append([]Issue{{UID: "issue-b", ShortID: "bbbb", Title: "Second", ProjectID: 7}}, kids...),
				Revision: 4,
			}, nil
		},
	}
	picker := openMove(t, moveModel(t, api))
	previewing, cmd := enter(picker)
	previewing, _ = deliver(t, previewing, cmd)
	refused := previewing
	refused.move.err = "move failed: project_not_found: project not found"
	// The mandatory text of a failed re-preview after a conflict: the note and a
	// daemon error of several lines, with no preview, under a long name.
	crowdedAPI := &moveTestAPI{
		projects: api.projects,
		preview: func(call int, in MoveInput) (*MutationResp, error) {
			if call > 2 {
				return nil, errors.New("descendant example-destination#abcd: issue_move_descendant_archived\nrestore its project or remove its parent link\nbefore moving the subtree")
			}
			return api.preview(call, in)
		},
		move: func(int, MoveInput) (*MutationResp, error) {
			return nil, &APIError{Status: http.StatusPreconditionFailed, Code: "revision_conflict", Message: "issue revision is 9"}
		},
	}
	crowded := openMove(t, moveModel(t, crowdedAPI))
	crowded, cmd = enter(crowded)
	crowded, _ = deliver(t, crowded, cmd)
	crowded, cmd = enter(crowded)
	crowded, cmd = deliver(t, crowded, cmd) // the 412 asks for a fresh preview
	crowded, _ = deliver(t, crowded, cmd)   // which fails
	require.Nil(t, crowded.move.preview)
	require.NotEmpty(t, crowded.move.note)
	conflicted := openMove(t, moveModel(t, &moveTestAPI{
		projects: api.projects, preview: api.preview,
		move: func(call int, _ MoveInput) (*MutationResp, error) {
			if call == 2 {
				return nil, &APIError{Status: http.StatusPreconditionFailed, Code: "revision_conflict", Message: "issue revision is 9"}
			}
			return &MutationResp{}, nil
		},
	}))
	conflicted, cmd = enter(conflicted)
	conflicted, _ = deliver(t, conflicted, cmd)
	conflicted, cmd = enter(conflicted)
	conflicted, cmd = deliver(t, conflicted, cmd)
	conflicted, _ = deliver(t, conflicted, cmd) // a fresh preview after the conflict
	require.NotNil(t, conflicted.move.preview)
	require.Contains(t, flattenModal(stripANSI(renderMoveModal(previewing.move, 100, 40))), "Nothing has moved yet.", "an ordinary preview says so")
	conflictView := flattenModal(stripANSI(renderMoveModal(conflicted.move, 100, 40)))
	require.Contains(t, conflictView, "nothing was moved", "the note says it")
	require.NotContains(t, conflictView, "Nothing has moved yet.", "so the preview does not say it again")

	for _, size := range []struct {
		name          string
		width, height int
	}{
		{"80x24", 80, 24}, {"80x12", 80, 12}, {"70x24", 70, 24}, {"100x8", 100, 8}, {"split 140x40", 140, 40}, {"split 140x10", 140, 10},
	} {
		sized := picker
		sized.width, sized.height = size.width, size.height
		pasted, _ := stepModel(sized, tea.PasteMsg{Content: strings.Repeat("pasted-filter-", 20)})
		for _, step := range []struct {
			name  string
			state Model
			want  []string
			// prompt is the in-modal hint, shown only where the footer is not.
			prompt string
		}{
			{"picker", picker, []string{"Project:", "▸ " + longName[:20]}, "enter preview · esc cancel"},
			{"picker with a long pasted filter", pasted, []string{"Project:", "no project matches"}, "enter preview · esc cancel"},
			{"preview", previewing, []string{"This issue and its 4 descendants move together."}, "enter move · esc back"},
			{"refusal", refused, []string{"project_not_found"}, "enter move · esc back"},
			{"conflict note and a multiline error after a failed re-preview", crowded, []string{"revision changed since the preview", "issue_move_descendant_archived", "remove its parent link", "before moving the subtree"}, "enter preview · esc cancel"},
		} {
			t.Run(size.name+" "+step.name, func(t *testing.T) {
				rendered := stripANSI(renderMoveModal(step.state.move, size.width, size.height))
				var content strings.Builder
				for line := range strings.SplitSeq(rendered, "\n") {
					require.LessOrEqual(t, ansi.StringWidth(line), size.width, "each line fits the terminal:\n%s", rendered)
					content.WriteString(strings.Trim(line, " │╭╮╰╯─"))
					content.WriteString(" ")
				}
				// Below 80 columns the too-narrow screen draws no footer at all.
				footerShown := size.width >= 80 && size.height >= 10
				limit := size.height
				if footerShown {
					limit-- // the footer keeps its row
				}
				require.LessOrEqual(t, strings.Count(rendered, "\n")+1, limit, "the whole modal fits:\n%s", rendered)
				flat := strings.Join(strings.Fields(content.String()), " ")
				for _, want := range step.want {
					require.Contains(t, flat, want, "%s\n%s", size.name, rendered)
				}
				if footerShown {
					require.NotContains(t, flat, step.prompt, "the footer already says it:\n%s", rendered)
				} else {
					require.Contains(t, flat, step.prompt, "no footer, so the modal says it:\n%s", rendered)
				}
				if step.state.move.preview != nil && step.name != "conflict note and a multiline error after a failed re-preview" {
					require.Equal(t, 1, strings.Count(strings.Join(strings.Fields(content.String()), ""), "to"+longName),
						"the full destination name shows once, wrapped if need be:\n%s", rendered)
				}
			})
		}
	}
}

func TestMoveDropsEveryIssueTheDaemonReportsMoved(t *testing.T) {
	// issue-c became a child of the moved issue after the preview was read.
	api := &moveTestAPI{
		projects: []ProjectSummary{moveProject(2, "hub-project", "", "")},
		move: func(int, MoveInput) (*MutationResp, error) {
			return &MutationResp{
				Issues:     []Issue{{UID: "issue-b"}, {UID: "issue-c"}},
				NewShortID: "new1", Changed: true,
			}, nil
		},
	}
	m := moveModel(t, api)
	m.layout, m.focus = splitlayout.Split, focusList
	child := m.list.issues[2]
	m.detail = detailModel{scopePID: 7, issue: &child, navStack: []detailModel{{issue: &Issue{UID: "issue-x"}}}}

	m = openMove(t, m)
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	m, cmd = enter(m)
	m, _ = deliver(t, m, cmd)

	for _, row := range m.list.visibleRows() {
		require.NotEqual(t, "issue-c", row.issue.UID, "the late child left the list with its parent")
	}
	require.Equal(t, "issue-a", m.list.selectedUID)
	require.NotNil(t, m.detail.issue)
	require.NotEqual(t, "issue-c", m.detail.issue.UID, "a pane showing the late child resets")
	require.Empty(t, m.detail.navStack)
}

func TestMoveFormClosesWhenCredentialsNoLongerAllowAWholeProjectWrite(t *testing.T) {
	m := openMove(t, moveModel(t, &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}}))
	// Issue-scoped credentials may edit within their subtree but cannot move.
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{
		Kind: "token", Actor: "member", AllowedActions: []string{"issue.read", "issue.edit"},
		Scope: &api.TokenScopeOut{},
	}})
	require.Equal(t, modalNone, m.modal)
}

func TestMoveRefreshesEveryCachedProjectsLabelCounts(t *testing.T) {
	// A labeled child from project 9 joined the subtree after the preview, so the
	// preview never named project 9.
	api := &moveTestAPI{
		projects: []ProjectSummary{moveProject(2, "hub-project", "", "")},
		move: func(int, MoveInput) (*MutationResp, error) {
			return &MutationResp{
				Issues:     []Issue{{UID: "issue-b", ProjectID: 2}, {UID: "late-kid", ProjectID: 2}},
				NewShortID: "new1", Changed: true,
			}, nil
		},
	}
	m := moveModel(t, api)
	m.projectLabels = newLabelCache()
	for _, pid := range []int64{7, 9, 2} {
		m.projectLabels.byProject[pid] = labelCacheEntry{pid: pid}
	}
	m = openMove(t, m)
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	m, cmd = enter(m)
	m, _ = deliver(t, m, cmd)
	for _, pid := range []int64{7, 9, 2} {
		require.True(t, m.projectLabels.byProject[pid].fetching, "project %d has a label cache, so its counts are fetched again", pid)
	}
	_, fetched := m.projectLabels.byProject[11]
	require.False(t, fetched, "a project without a cache is not fetched")
}

func TestMoveCancelsAnArmedPriorityPrompt(t *testing.T) {
	m := moveModel(t, &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}})
	m, _ = updateModel(m, runeKey('!'))
	require.True(t, m.list.pendingPriority)
	m = openMove(t, m)
	require.False(t, m.list.pendingPriority, "a digit after the move must not set a priority on the neighbour")
	require.Empty(t, m.list.status, "the hint naming the issue is gone")
}

func TestMoveKeepsAnUnrelatedListStatusWhenNoPriorityPromptIsArmed(t *testing.T) {
	m := moveModel(t, &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}})
	m.list.status = "filter: 3 of 9"
	m = openMove(t, m)
	require.Equal(t, "filter: 3 of 9", m.list.status)
}

// quietMoveAPI swallows screen-visit telemetry, which the move fake's absent
// client cannot send.
type quietMoveAPI struct{ *moveTestAPI }

func (quietMoveAPI) ReportScreenViewed(context.Context, string) error { return nil }

// A launch on `kata tui <ref>` opens the pane on that issue before the list
// has a say. Esc returns focus to the list, whose highlight is another row, and
// the pane keeps the opened issue with an empty Back history. A move of the
// highlighted row must leave that pane, its scroll and its tab alone.
func TestMoveKeepsAnUnrelatedSplitPaneWithNoBackHistory(t *testing.T) {
	api := &moveTestAPI{
		projects: []ProjectSummary{moveProject(2, "hub-project", "", "")},
		preview: func(int, MoveInput) (*MutationResp, error) {
			return &MutationResp{Issue: &Issue{UID: "issue-b", ShortID: "bbbb"}, Revision: 5}, nil
		},
	}
	m := moveModel(t, api)
	m.api = newUndoClient(quietMoveAPI{api})
	opened := m.list.issues[0]
	opened.Body = strings.Repeat("A long paragraph of the opened issue.\n\n", 80)
	m = m.seedOpenDetail(opened)
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 140, Height: 40})
	require.Equal(t, splitlayout.Split, m.layout)
	require.Equal(t, focusDetail, m.focus)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyTab})
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	tab, scroll := m.detail.activeTab, m.detail.scroll
	require.NotEqual(t, tabComments, tab, "Tab moved the pane off its first tab")
	require.Positive(t, scroll, "PageDown scrolled the pane")
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, focusList, m.focus)
	require.Equal(t, "issue-b", m.list.selectedUID, "the list highlights a different row than the pane")
	require.Empty(t, m.detail.navStack)

	m = openMove(t, m)
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	m, cmd = enter(m)
	m, _ = deliver(t, m, cmd)

	require.Equal(t, "issue-c", m.list.selectedUID, "the list selects the row after the one removed")
	require.NotNil(t, m.detail.issue)
	require.Equal(t, "issue-a", m.detail.issue.UID, "the pane keeps the issue it was opened on")
	require.Equal(t, tab, m.detail.activeTab)
	require.Equal(t, scroll, m.detail.scroll)
}

// TestMoveKeyIsOfferedOnlyOnThePlainListRow holds M to one rule: the footer,
// the help overlay and the dispatch all offer the key on the selected row of
// the plain issue list, and nowhere else. An undo in flight also takes it away,
// because the Move form would be replaced when the undo answers.
func TestMoveKeyIsOfferedOnlyOnThePlainListRow(t *testing.T) {
	hint := helplayout.HelpItem{Key: "M", Description: "move"}
	for _, tc := range []struct {
		name    string
		view    viewID
		scoped  bool
		context string
		extra   string
	}{
		{name: "unscoped list", view: viewList},
		{name: "unscoped detail", view: viewDetail},
		{name: "scoped list", view: viewList, scoped: true},
		{name: "scoped detail", view: viewDetail, scoped: true},
		{name: "Inbox", view: viewList, context: "Inbox"},
		{name: "Attention", view: viewList, context: "Attention"},
		{name: "Help", view: viewList, extra: "help"},
		{name: "empty list", view: viewList, extra: "empty"},
		{name: "split with the detail focused", view: viewList, extra: "split detail"},
		{name: "undo in flight", view: viewList, extra: "undo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, tc.view)
			target := h.issues[0]
			switch tc.context {
			case "Inbox":
				_, err := h.env.DB.DesignateInboxProject(t.Context(), db.DesignateInboxProjectIn{ProjectID: h.project.ID, Actor: "alice"})
				require.NoError(t, err)
				h.m = h.drain(h.finish(h.key(runeKey('I'))))
				require.True(t, h.m.scope.inbox)
				require.NotNil(t, h.m.inboxReturn)
			case "Attention":
				h.m.attention.returnContext = &attentionReturnContext{detail: h.m.detail}
			}
			h.selectIssue(target.UID)
			if tc.view == viewDetail {
				h.m.focus = focusDetail
				require.NotNil(t, h.m.detail.issue)
			}
			h.m.authCapabilitiesRequired, h.m.authCapabilitiesReady = true, true
			h.m.issueScoped, h.m.scopedWritable = tc.scoped, tc.scoped
			switch tc.extra {
			case "help":
				h.m.view = viewHelp
			case "empty":
				h.m.list.issues, h.m.list.selectedUID, h.m.list.selectedProjectID = nil, "", 0
			case "split detail":
				issue := h.m.list.issues[0]
				h.m.layout, h.m.focus, h.m.detail.issue = splitlayout.Split, focusDetail, &issue
			case "undo":
				h.m.undoInFlight = true
			}

			offered := tc.view == viewList && !tc.scoped && tc.context == "" && tc.extra == ""
			helpListed := !tc.scoped && tc.context == "" && tc.extra != "undo"
			if tc.extra != "help" {
				rows := flattenHelpRows(h.m.helpRows())
				if offered {
					assertHelpItemPresent(t, rows, hint)
				} else {
					assertHelpItemAbsent(t, rows, hint)
				}
			}
			if tc.scoped || tc.context != "" {
				assertHelpItemAbsent(t, flattenHelpRows(h.m.splitHelpRows()), hint)
				assertHelpItemAbsent(t, flattenHelpRows(listHelpRows(h.m.list, h.m.chrome())), hint)
			}
			for _, section := range helpSections(h.m.helpKeymap()) {
				for _, item := range section.rows {
					require.NotEmpty(t, item.Key, "help omits unbound rows")
				}
			}
			helpModel := h.m
			helpModel.view, helpModel.width = viewHelp, 120
			require.Equal(t, helpListed, strings.Contains(stripANSI(helpModel.viewBody()), "move to another project"))

			cmd := h.key(runeKey('M'))
			require.Equal(t, offered, h.m.modal == modalMoveIssue, "dispatch follows the footer")
			if !offered {
				require.Nil(t, cmd)
			}
		})
	}
}

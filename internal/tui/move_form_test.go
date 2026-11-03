package tui

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
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
		{UID: "issue-a", ProjectID: 7, ProjectUID: "uid-7", ShortID: "aaaa", Title: "First", Revision: 3},
		{UID: "issue-b", ProjectID: 7, ProjectUID: "uid-7", ShortID: "bbbb", Title: "Second", Revision: 4},
		{UID: "issue-c", ProjectID: 7, ProjectUID: "uid-7", ShortID: "cccc", Title: "Third", Revision: 5},
	}
	m.list = m.list.syncSelection(m.list.visibleRows())
	m = selectListUID(t, m, "issue-b")
	return m
}

func selectListUID(t *testing.T, m Model, uid string) Model {
	t.Helper()
	for i, row := range m.list.visibleRows() {
		if row.issue.UID == uid {
			m.list.cursor, m.list.selectedUID, m.list.selectedProjectID = i, uid, row.issue.ProjectID
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

func typeText(m Model, text string) Model {
	for _, r := range text {
		m, _ = updateModel(m, runeKey(r))
	}
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
		moveProject(3, "docs", "", "home"),
		moveProject(4, "docs", "", "home"),
		moveProject(6, "solo", "", "work"),
	}}
	m := openMove(t, moveModel(t, api))
	require.Equal(t, []string{
		"docs · work",
		"docs · uid-3",
		"docs · uid-4",
		"solo",
	}, moveLabels(m), "an area names a project only when no same-named project shares it; otherwise the UID does")

	m = typeText(m, "WORK")
	require.Equal(t, []string{"docs · work"}, moveLabels(m), "the filter matches the area shown beside a name, case-insensitively")

	m, _ = updateModel(m, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m = typeText(m, "so")
	require.Equal(t, []string{"solo"}, moveLabels(m), "the filter matches names")
}

func TestMovePickerShowsNoPlaceholderFragmentBeforeTyping(t *testing.T) {
	api := &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}}
	m := openMove(t, moveModel(t, api))
	view := flattenModal(stripANSI(renderMoveModal(m.move, 100, 40)))
	require.NotContains(t, view, "Project: t", "an empty filter must not show a clipped placeholder")
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
		{"lone issue", nil, []string{"Single move"}, "Subtree move"},
		{"one descendant", family(Issue{UID: "kid-1", ShortID: "k001", Title: "Child one", ProjectID: 7}),
			[]string{"Subtree move", "its 1 descendant move", "#k001 Child one"}, "1 descendants"},
		{"several descendants", family(
			Issue{UID: "kid-1", ShortID: "k001", Title: "Child one", ProjectID: 7},
			Issue{UID: "kid-2", ShortID: "k002", Title: "Child two", ProjectID: 9}),
			[]string{"Subtree move", "its 2 descendants move", "#k001 Child one", "#k002 Child two"}, "Single move"},
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
	require.Contains(t, stripANSI(renderMoveModal(m.move, 100, 40)), "changed")

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
			require.NotNil(t, follow, "the list is fetched again")
			require.NotNil(t, m.toast)
			require.Contains(t, m.toast.text, "hub-project#new1")
		})
	}
}

func TestMoveFromDetailLeavesTheMovedIssue(t *testing.T) {
	api := &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}}
	m := moveModel(t, api)
	m.view = viewDetail
	m.detail.scopePID = 7
	m.detail.gen = 3
	issue := m.list.issues[1]
	m.detail.issue = &issue
	m = openMove(t, m)
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	m, cmd = enter(m)
	m, _ = deliver(t, m, cmd)
	require.Equal(t, viewList, m.view, "the detail of a moved issue closes")
	require.Equal(t, "issue-c", m.list.selectedUID)
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
	require.Empty(t, m.undoHistory.entries, "a move changes the short id, so earlier edits can no longer be undone")
	require.Equal(t, "moving an issue cannot be undone", m.undoHistory.boundary)
}

func TestMoveFollowsTheSamePermissionGateAsOtherEdits(t *testing.T) {
	api := &moveTestAPI{projects: []ProjectSummary{moveProject(2, "hub-project", "", "")}}
	m := moveModel(t, api)
	m.authCapabilitiesRequired, m.authCapabilitiesReady = true, true
	m.issueScoped, m.scopedWritable = true, false
	m, _ = updateModel(m, runeKey('M'))
	require.Equal(t, modalNone, m.modal, "a read-only token cannot open the move form")
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

func TestMoveIssueAgainstDaemonKeepsCommentsAndLinks(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	base := NewClient(env.URL, env.HTTP)
	source, err := env.DB.CreateProject(ctx, "example-workspace")
	require.NoError(t, err)
	dest, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	create := func(title string) db.Issue {
		issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: source.ID, Title: title, Body: "body", Author: "alice",
		})
		require.NoError(t, err)
		return issue
	}
	parent, child, other := create("Parent to move"), create("Child to move"), create("Related stays")
	_, err = base.AddLink(ctx, source.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
	require.NoError(t, err)
	_, err = base.AddLink(ctx, source.ID, parent.UID, LinkBody{Type: "related", ToRef: other.UID}, "alice")
	require.NoError(t, err)
	_, err = base.AddComment(ctx, source.ID, parent.UID, "Original finding", "alice")
	require.NoError(t, err)

	client := newConnectedUndoClient(t, base)
	m := initialModel(Options{})
	m.api = client
	m.view = viewList
	m.list.actor = "alice"
	m.scope = scope{projectID: source.ID}
	for _, issue := range []db.Issue{parent, child, other} {
		detail, err := client.GetIssueDetail(ctx, source.ID, issue.UID)
		require.NoError(t, err)
		m.list.issues = append(m.list.issues, *detail.Issue)
	}
	m = selectListUID(t, m, parent.UID)
	m.undoHistory.push(undoEntry{kind: "priority.set", uid: other.UID})

	m = openMove(t, m)
	m = typeText(m, "spoke")
	m, cmd := enter(m)
	m, _ = deliver(t, m, cmd)
	view := stripANSI(renderMoveModal(m.move, 100, 40))
	require.Contains(t, view, "Child to move", "the preview lists the descendant that moves with it")
	stored, err := env.DB.IssueByUID(ctx, parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, source.ID, stored.ProjectID, "a preview writes nothing")

	m, cmd = enter(m)
	m, _ = deliver(t, m, cmd)

	moved, err := env.DB.IssueByUID(ctx, parent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, dest.ID, moved.ProjectID)
	movedChild, err := env.DB.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, dest.ID, movedChild.ProjectID, "the subtree moves together")

	comments, err := base.ListComments(ctx, dest.ID, moved.UID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "Original finding", comments[0].Body)
	links, err := env.DB.LinksByIssue(ctx, moved.ID)
	require.NoError(t, err)
	require.Len(t, links, 2, "the related link and the child's parent link survive the move")
	detail, err := base.GetIssueDetail(ctx, dest.ID, moved.UID)
	require.NoError(t, err)
	require.Len(t, detail.Children, 1)
	events, err := env.DB.EventsAfter(ctx, db.EventsAfterParams{IssueUID: moved.UID, Limit: 100})
	require.NoError(t, err)
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	require.Contains(t, types, "issue.created", "the issue's history survives the move")
	require.Contains(t, types, "issue.moved")

	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "spoke-project#"+moved.ShortID, "the notice names the new short id")
	require.Empty(t, m.undoHistory.entries)
	require.Equal(t, modalNone, m.modal)
}

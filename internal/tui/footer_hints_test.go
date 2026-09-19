package tui

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/splitlayout"
)

func TestQueueHelpRows_ConditionalItems(t *testing.T) {
	withChildren := Model{list: listModel{issues: hierarchyIssues()}}
	assertHelpItemsPresent(t, withChildren.queueHelpRows(),
		helplayout.HelpItem{Key: "space", Description: "expand"},
		helplayout.HelpItem{Key: "E", Description: "all"},
		helplayout.HelpItem{Key: "N", Description: "child"},
		helplayout.HelpItem{Key: "o", Description: "order"})

	leaf := Model{list: listModel{issues: []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "leaf", Status: "open"},
	}}}
	assertHelpItemAbsent(t, flattenHelpRows(leaf.queueHelpRows()),
		helplayout.HelpItem{Key: "space", Description: "expand"})
	assertHelpItemAbsent(t, flattenHelpRows(leaf.queueHelpRows()),
		helplayout.HelpItem{Key: "E", Description: "all"})
	assertHelpItemPresent(t, flattenHelpRows(leaf.queueHelpRows()),
		helplayout.HelpItem{Key: "N", Description: "child"})

	flat := withChildren
	flat.list.viewMode = issueListViewFlat
	assertHelpItemAbsent(t, flattenHelpRows(flat.queueHelpRows()),
		helplayout.HelpItem{Key: "E", Description: "all"})

	empty := Model{}
	assertHelpItemAbsent(t, flattenHelpRows(empty.queueHelpRows()),
		helplayout.HelpItem{Key: "N", Description: "child"})
}

// TestDetailHelpRows_Contexts: the persistent detail footer is
// comprehensive — every detail-mode binding handled by the Update
// loop appears so the user is not stranded looking for an action.
// The activity-focus row carries navigation primitives plus the
// full mutation surface (edit/comment/label/owner/parent/blocker/
// link/close/reopen/quit). Children focus swaps the navigation
// header (↑↓ child / ↵ open child) but keeps the action surface.
func TestDetailHelpRows_Contexts(t *testing.T) {
	activity := Model{detail: detailModel{
		issue:       &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "issue", Status: "open"},
		detailFocus: focusActivity,
		activeTab:   tabComments,
	}}
	assertHelpItemsPresent(t, activity.detailHelpRows(),
		helplayout.HelpItem{Key: "↑↓", Description: "scroll"},
		helplayout.HelpItem{Key: "j/k", Description: "row"},
		helplayout.HelpItem{Key: "↹", Description: "section"},
		helplayout.HelpItem{Key: "↵", Description: "open"},
		helplayout.HelpItem{Key: "pgup/pgdn", Description: "page"},
		helplayout.HelpItem{Key: "e", Description: "edit"},
		helplayout.HelpItem{Key: "c", Description: "comment"},
		helplayout.HelpItem{Key: "+", Description: "label"},
		helplayout.HelpItem{Key: "a", Description: "owner"},
		helplayout.HelpItem{Key: "x", Description: "close"},
		helplayout.HelpItem{Key: "r", Description: "reopen"},
		helplayout.HelpItem{Key: "p", Description: "parent"},
		helplayout.HelpItem{Key: "b", Description: "block"},
		helplayout.HelpItem{Key: "l", Description: "related"},
		helplayout.HelpItem{Key: "L", Description: "layout"},
		helplayout.HelpItem{Key: "esc", Description: "back"},
		helplayout.HelpItem{Key: "?", Description: "help"},
		helplayout.HelpItem{Key: "q", Description: "quit"})

	children := Model{detail: hierarchyDetailModel(focusChildren)}
	assertHelpItemsPresent(t, children.detailHelpRows(),
		helplayout.HelpItem{Key: "↑↓", Description: "scroll"},
		helplayout.HelpItem{Key: "j/k", Description: "child"},
		helplayout.HelpItem{Key: "↵", Description: "open child"},
		helplayout.HelpItem{Key: "N", Description: "child"},
		helplayout.HelpItem{Key: "e", Description: "edit"},
		helplayout.HelpItem{Key: "x", Description: "close"},
		helplayout.HelpItem{Key: "?", Description: "help"},
		helplayout.HelpItem{Key: "q", Description: "quit"})
}

func TestHelpRows_InputAndModalContexts(t *testing.T) {
	tests := []struct {
		name string
		m    Model
		want [][]helplayout.HelpItem
	}{
		{
			name: "search query focus",
			m:    Model{input: inputState{kind: inputSearchBar}},
			want: [][]helplayout.HelpItem{{
				{Key: "↑↓/enter", Description: "results"},
				{Key: "esc", Description: "cancel"},
				{Key: "ctrl+u", Description: "clear"},
			}},
		},
		{
			name: "search results focus",
			m: Model{input: inputState{
				kind:        inputSearchBar,
				searchFocus: searchFocusResults,
			}},
			want: [][]helplayout.HelpItem{{
				{Key: "↑↓", Description: "move"},
				{Key: "enter", Description: "apply"},
				{Key: "esc", Description: "query"},
				{Key: "/", Description: "query"},
			}},
		},
		{
			name: "filter form",
			m:    Model{input: inputState{kind: inputFilterForm}},
			want: [][]helplayout.HelpItem{{
				{Key: "ctrl+o", Description: "apply"},
				{Key: "esc", Description: "cancel"},
				{Key: "ctrl+r", Description: "reset"},
			}},
		},
		{
			name: "quit modal",
			m:    Model{modal: modalQuitConfirm},
			want: [][]helplayout.HelpItem{{
				{Key: "y", Description: "confirm"},
				{Key: "n/esc", Description: "cancel"},
			}},
		},
		{
			name: "discard modal in split layout",
			m: Model{
				layout: splitlayout.Split,
				input:  inputState{kind: inputCommentForm},
				modal:  modalDiscardComment,
			},
			want: [][]helplayout.HelpItem{{
				{Key: "y", Description: "discard"},
				{Key: "n/esc", Description: "keep editing"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.helpRows(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("help rows = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestViewChromeHelpRows_ModalPrecedesInput(t *testing.T) {
	tests := []struct {
		name  string
		modal modalKind
		want  [][]helplayout.HelpItem
	}{
		{
			name:  "discard comment",
			modal: modalDiscardComment,
			want: [][]helplayout.HelpItem{{
				{Key: "y", Description: "discard"},
				{Key: "n/esc", Description: "keep editing"},
			}},
		},
		{
			name:  "discard new issue",
			modal: modalDiscardNewIssue,
			want: [][]helplayout.HelpItem{{
				{Key: "y", Description: "discard"},
				{Key: "n/esc", Description: "keep editing"},
			}},
		},
		{
			name:  "quit",
			modal: modalQuitConfirm,
			want: [][]helplayout.HelpItem{{
				{Key: "y", Description: "confirm"},
				{Key: "n/esc", Description: "cancel"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				input: inputState{kind: inputCommentForm},
				modal: tt.modal,
			}
			chrome := m.chrome()
			if got := listHelpRows(listModel{}, chrome); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("list help rows = %+v, want %+v", got, tt.want)
			}
			if got := detailHelpRows(detailModel{}, chrome); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("detail help rows = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAuxiliaryViewFooters_ModalPrecedesActions(t *testing.T) {
	projects := setupProjectsView()
	daemons := setupDaemonView()
	federation := setupFederationView()
	federation.width, federation.height = 120, 24

	tests := []struct {
		name       string
		model      Model
		render     func(Model) string
		normalWant string
	}{
		{
			name:       "projects",
			model:      projects,
			render:     renderProjects,
			normalWant: "q quit",
		},
		{
			name:       "daemons",
			model:      daemons,
			render:     renderDaemons,
			normalWant: "[q] quit",
		},
		{
			name:       "federation",
			model:      federation,
			render:     renderFederation,
			normalWant: "[?] help",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalFooter := lastRenderedLine(tt.render(tt.model))
			if !strings.Contains(normalFooter, tt.normalWant) {
				t.Fatalf("ordinary footer = %q, want %q action", normalFooter, tt.normalWant)
			}

			modalModel := tt.model
			modalModel.modal = modalQuitConfirm
			if got := lastRenderedLine(tt.render(modalModel)); got != "y confirm▕ n/esc cancel" {
				t.Fatalf("quit modal footer = %q", got)
			}
		})
	}
}

func TestFederationDetailEmpty_ModalFooterPrecedesEarlyReturn(t *testing.T) {
	m := setupFederationView()
	m.width, m.height = 120, 24
	m.federation.mode = federationModeDetail
	m.federation.statuses = nil

	if got := lastRenderedLine(renderFederation(m)); got != "no federation selected" {
		t.Fatalf("ordinary empty-detail footer = %q", got)
	}

	m.modal = modalQuitConfirm
	if got := lastRenderedLine(renderFederation(m)); got != "y confirm▕ n/esc cancel" {
		t.Fatalf("quit modal empty-detail footer = %q", got)
	}
}

func lastRenderedLine(rendered string) string {
	lines := strings.Split(stripANSI(rendered), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// TestPersistentHelpRowsPreferArrowNotation guards the queue (list)
// footer's arrow-first convention. The detail footer additionally
// surfaces j/k as the section-cursor binding, since the unified
// document refactor split arrows (viewport scroll) from j/k (row
// cursor) — both must be discoverable from the persistent help row.
func TestPersistentHelpRowsPreferArrowNotation(t *testing.T) {
	m := Model{
		list:   listModel{issues: hierarchyIssues()},
		detail: hierarchyDetailModel(focusActivity),
	}
	for _, item := range flattenHelpRows(m.queueHelpRows()) {
		if strings.Contains(item.Key, "j/k") {
			t.Fatalf("queue footer keys should use arrows, got %+v", item)
		}
	}
}

func TestFooterHelpTableParity(t *testing.T) {
	oldMode, oldDark := activeColorMode, activeHasDarkBackground
	defer func() { applyColorMode(oldMode, oldDark) }()

	listRows := (Model{list: listModel{issues: hierarchyIssues()}}).queueHelpRows()
	detailRows := hierarchyDetailModel(focusChildren).detailHelpRows()
	cases := []struct {
		name       string
		mode       colorMode
		dark       bool
		width      int
		innerWidth int
		rows       [][]helplayout.HelpItem
		want       string
		wantLines  int
	}{
		{
			name:       "modal light",
			mode:       colorLight,
			width:      80,
			innerWidth: 78,
			rows:       modalHelpRows(modalQuitConfirm),
			want:       "\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242my\x1b[m \x1b[38;5;248mconfirm\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mn/esc\x1b[m \x1b[38;5;248mcancel\x1b[m                                                       \x1b[m\x1b[48;5;253m \x1b[m",
			wantLines:  1,
		},
		{
			name:       "list dark",
			mode:       colorDark,
			dark:       true,
			width:      80,
			innerWidth: 78,
			rows:       listRows,
			want:       "\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246m↑↓\x1b[m \x1b[38;5;240mmove\x1b[m  \x1b[38;5;242m▕\x1b[m \x1b[38;5;246m↵\x1b[m \x1b[38;5;240mopen\x1b[m      \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mspace\x1b[m \x1b[38;5;240mexpand\x1b[m \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mE\x1b[m \x1b[38;5;240mall\x1b[m   \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mn\x1b[m \x1b[38;5;240mnew\x1b[m  \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mN\x1b[m \x1b[38;5;240mchild\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246m/\x1b[m \x1b[38;5;240msearch\x1b[m  \x1b[m\x1b[48;5;234m \x1b[m\n\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246mf\x1b[m \x1b[38;5;240mfilter\x1b[m \x1b[38;5;242m▕\x1b[m \x1b[38;5;246ms\x1b[m \x1b[38;5;240mstatus\x1b[m    \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mv\x1b[m \x1b[38;5;240mview\x1b[m       \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mo\x1b[m \x1b[38;5;240morder\x1b[m \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mc\x1b[m \x1b[38;5;240mclear\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mx\x1b[m \x1b[38;5;240mclose\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246m!\x1b[m \x1b[38;5;240mpriority\x1b[m\x1b[m\x1b[48;5;234m \x1b[m\n\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246mD\x1b[m \x1b[38;5;240mdaemons\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mF\x1b[m \x1b[38;5;240mfederation\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mC\x1b[m \x1b[38;5;240mcredentials\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mL\x1b[m \x1b[38;5;240mlayout\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246m?\x1b[m \x1b[38;5;240mhelp\x1b[m \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mq\x1b[m \x1b[38;5;240mquit\x1b[m             \x1b[m\x1b[48;5;234m \x1b[m",
			wantLines:  3,
		},
		{
			name:       "detail light",
			mode:       colorLight,
			width:      80,
			innerWidth: 78,
			rows:       detailRows,
			want:       "\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242m↑↓\x1b[m \x1b[38;5;248mscroll\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mj/k\x1b[m \x1b[38;5;248mchild\x1b[m   \x1b[38;5;248m▕\x1b[m \x1b[38;5;242m↵\x1b[m \x1b[38;5;248mopen child\x1b[m \x1b[38;5;248m▕\x1b[m \x1b[38;5;242m↹\x1b[m \x1b[38;5;248msection\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mpgup/pgdn\x1b[m \x1b[38;5;248mpage\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242me\x1b[m \x1b[38;5;248medit\x1b[m     \x1b[m\x1b[48;5;253m \x1b[m\n\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242mc\x1b[m \x1b[38;5;248mcomment\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242m+\x1b[m \x1b[38;5;248mlabel\x1b[m     \x1b[38;5;248m▕\x1b[m \x1b[38;5;242m-\x1b[m \x1b[38;5;248munlabel\x1b[m    \x1b[38;5;248m▕\x1b[m \x1b[38;5;242ma\x1b[m \x1b[38;5;248mowner\x1b[m  \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mA\x1b[m \x1b[38;5;248munassign\x1b[m    \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mt\x1b[m \x1b[38;5;248mtimed\x1b[m    \x1b[m\x1b[48;5;253m \x1b[m\n\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242mx\x1b[m \x1b[38;5;248mclose\x1b[m  \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mr\x1b[m \x1b[38;5;248mreopen\x1b[m    \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mp\x1b[m \x1b[38;5;248mparent\x1b[m     \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mb\x1b[m \x1b[38;5;248mblock\x1b[m  \x1b[38;5;248m▕\x1b[m \x1b[38;5;242ml\x1b[m \x1b[38;5;248mrelated\x1b[m     \x1b[38;5;248m▕\x1b[m \x1b[38;5;242m!\x1b[m \x1b[38;5;248mpriority\x1b[m \x1b[m\x1b[48;5;253m \x1b[m\n\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242mD\x1b[m \x1b[38;5;248mdaemons\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mF\x1b[m \x1b[38;5;248mfederation\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mC\x1b[m \x1b[38;5;248mcredentials\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mN\x1b[m \x1b[38;5;248mchild\x1b[m  \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mL\x1b[m \x1b[38;5;248mlayout\x1b[m      \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mesc\x1b[m \x1b[38;5;248mback\x1b[m   \x1b[m\x1b[48;5;253m \x1b[m\n\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242m?\x1b[m \x1b[38;5;248mhelp\x1b[m   \x1b[38;5;248m▕\x1b[m \x1b[38;5;242mq\x1b[m \x1b[38;5;248mquit\x1b[m                                                             \x1b[m\x1b[48;5;253m \x1b[m",
			wantLines:  5,
		},
		{
			name:       "project dark",
			mode:       colorDark,
			dark:       true,
			width:      80,
			innerWidth: 78,
			rows:       projectsHelpRows(),
			want:       "\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246m↑↓\x1b[m \x1b[38;5;240mmove\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246m↵\x1b[m \x1b[38;5;240mopen\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mesc\x1b[m \x1b[38;5;240mback\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mr\x1b[m \x1b[38;5;240mrefresh\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mD\x1b[m \x1b[38;5;240mdaemons\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mF\x1b[m \x1b[38;5;240mfederation\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246mC\x1b[m \x1b[38;5;240mcredentials\x1b[m  \x1b[m\x1b[48;5;234m \x1b[m\n\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246m?\x1b[m \x1b[38;5;240mhelp\x1b[m \x1b[38;5;242m▕\x1b[m \x1b[38;5;246mq\x1b[m \x1b[38;5;240mquit\x1b[m                                                               \x1b[m\x1b[48;5;234m \x1b[m",
			wantLines:  2,
		},
		{
			name:       "ragged light",
			mode:       colorLight,
			width:      14,
			innerWidth: 12,
			rows: [][]helplayout.HelpItem{{
				{Key: "a", Description: "one"},
				{Key: "b", Description: "two"},
			}, {
				{Key: "solo"},
			}},
			want:      "\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242ma\x1b[m \x1b[38;5;248mone\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mb\x1b[m \x1b[38;5;248mtwo\x1b[m\x1b[m\x1b[48;5;253m \x1b[m\n\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242msolo\x1b[m        \x1b[m\x1b[48;5;253m \x1b[m",
			wantLines: 2,
		},
		{
			name:       "unicode dark",
			mode:       colorDark,
			dark:       true,
			width:      18,
			innerWidth: 16,
			rows: [][]helplayout.HelpItem{{
				{Key: "界", Description: "東"},
				{Key: "🙂", Description: "ok"},
			}},
			want:      "\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246m界\x1b[m \x1b[38;5;240m東\x1b[m\x1b[38;5;242m▕\x1b[m \x1b[38;5;246m🙂\x1b[m \x1b[38;5;240mok\x1b[m    \x1b[m\x1b[48;5;234m \x1b[m",
			wantLines: 1,
		},
		{
			name:       "exact outer 14",
			mode:       colorLight,
			width:      14,
			innerWidth: 12,
			rows: [][]helplayout.HelpItem{{
				{Key: "a", Description: "one"},
				{Key: "b", Description: "two"},
			}},
			want:      "\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242ma\x1b[m \x1b[38;5;248mone\x1b[m\x1b[38;5;248m▕\x1b[m \x1b[38;5;242mb\x1b[m \x1b[38;5;248mtwo\x1b[m\x1b[m\x1b[48;5;253m \x1b[m",
			wantLines: 1,
		},
		{
			name:       "reflow outer 13",
			mode:       colorLight,
			width:      13,
			innerWidth: 11,
			rows: [][]helplayout.HelpItem{{
				{Key: "a", Description: "one"},
				{Key: "b", Description: "two"},
			}},
			want:      "\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242ma\x1b[m \x1b[38;5;248mone\x1b[m      \x1b[m\x1b[48;5;253m \x1b[m\n\x1b[48;5;253m \x1b[m\x1b[38;5;242;48;5;253m\x1b[38;5;242mb\x1b[m \x1b[38;5;248mtwo\x1b[m      \x1b[m\x1b[48;5;253m \x1b[m",
			wantLines: 2,
		},
		{
			name:       "narrow 8",
			mode:       colorDark,
			dark:       true,
			width:      8,
			innerWidth: 6,
			rows: [][]helplayout.HelpItem{{
				{Key: "↑↓", Description: "move"},
				{Key: "↵", Description: "open"},
				{Key: "space", Description: "expand"},
			}},
			want:      "\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246m↑↓\x1b[m \x1b[38;5;240mmo…\x1b[m\x1b[m\x1b[48;5;234m \x1b[m\n\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246m↵\x1b[m \x1b[38;5;240mope…\x1b[m\x1b[m\x1b[48;5;234m \x1b[m\n\x1b[48;5;234m \x1b[m\x1b[38;5;246;48;5;234m\x1b[38;5;246mspace\x1b[m…\x1b[38;5;240m\x1b[m\x1b[m\x1b[48;5;234m \x1b[m",
			wantLines: 3,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			applyColorMode(tt.mode, tt.dark)
			if got := titleBarInnerWidth(tt.width); got != tt.innerWidth {
				t.Fatalf("inner width = %d, want %d", got, tt.innerWidth)
			}
			got := renderFooterHelpTable(tt.rows, tt.width)
			if got != tt.want {
				t.Fatalf("footer output = %q, want %q", got, tt.want)
			}
			if got := strings.Count(got, "\n") + 1; got != tt.wantLines {
				t.Fatalf("rendered lines = %d, want %d", got, tt.wantLines)
			}
			if got := helpLines(tt.rows, tt.width); got != tt.wantLines {
				t.Fatalf("helpLines = %d, want %d", got, tt.wantLines)
			}
		})
	}
}

func TestRenderHelpTable_ReflowsToFitWidth80(t *testing.T) {
	rows := [][]helplayout.HelpItem{{
		{Key: "↑↓", Description: "move"},
		{Key: "↵", Description: "open"},
		{Key: "space", Description: "expand"},
		{Key: "N", Description: "child"},
		{Key: "/", Description: "search"},
		{Key: "f", Description: "filter"},
		{Key: "s", Description: "status"},
		{Key: "c", Description: "clear"},
		{Key: "x", Description: "close"},
		{Key: "?", Description: "help"},
		{Key: "q", Description: "quit"},
	}}
	got := stripANSI(renderFooterHelpTable(rows, 80))
	assertLinesFitWidth(t, got, 80)
	assertStringContains(t, got, "▕")
	assertStringContains(t, got, "space expand")
	assertStringContains(t, got, "q quit")
}

func TestReflowHelpRows_ExtremeNarrowFallsBackToOneItemPerRow(t *testing.T) {
	rows := [][]helplayout.HelpItem{{
		{Key: "↑↓", Description: "move"},
		{Key: "↵", Description: "open"},
		{Key: "space", Description: "expand"},
	}}
	got := convertAndReflowHelpRows(rows, 8)
	want := [][]helplayout.HelpItem{
		{{Key: "↑↓", Description: "move"}},
		{{Key: "↵", Description: "open"}},
		{{Key: "space", Description: "expand"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("narrow reflow = %+v, want %+v", got, want)
	}
}

func TestListViewFooterUsesAdaptiveHelpTable(t *testing.T) {
	lm := listModel{issues: hierarchyIssues()}
	got := stripANSI(lm.View(80, 14, viewChrome{}))
	assertLineCount(t, got, 14)
	assertLinesFitWidth(t, got, 80)
	assertStringContains(t, got, "space expand")
	assertStringContains(t, got, "N child")
	assertStringContains(t, got, "▕")
}

func TestDetailViewFooterUsesAdaptiveChildrenFocusHints(t *testing.T) {
	dm := hierarchyDetailModel(focusChildren)
	got := stripANSI(dm.View(80, 18, viewChrome{}))
	assertLineCount(t, got, 18)
	assertLinesFitWidth(t, got, 80)
	assertStringContains(t, got, "open child")
	assertStringContains(t, got, "N child")
}

// hierarchyIssues returns a parent (ShortID=p001) and one child (ShortID=c002)
// under ProjectID 7, the standard fixture for tests that need a queue
// or detail view with a parent/child relationship.
func hierarchyIssues() []Issue {
	parentSID := "p001"
	return []Issue{
		{ProjectID: 7, UID: "01TEST-p001", ShortID: parentSID, Title: "parent", Status: "open"},
		{ProjectID: 7, UID: "01TEST-c002", ShortID: "c002", Parent: &LinkPeer{UID: "01TEST-" + parentSID, ShortID: parentSID}, Title: "child", Status: "open"},
	}
}

func hierarchyDetailModel(focus detailFocus) detailModel {
	return detailModel{
		issue:       &Issue{UID: "01TEST-p001", ShortID: "p001", Title: "parent", Status: "open"},
		children:    []Issue{{UID: "01TEST-c002", ShortID: "c002", Title: "child", Status: "open"}},
		detailFocus: focus,
	}
}

func flattenHelpRows(rows [][]helplayout.HelpItem) []helplayout.HelpItem {
	out := []helplayout.HelpItem{}
	for _, row := range rows {
		out = append(out, row...)
	}
	return out
}

func assertHelpItemPresent(t *testing.T, rows []helplayout.HelpItem, want helplayout.HelpItem) {
	t.Helper()
	if slices.Contains(rows, want) {
		return
	}
	t.Fatalf("help rows missing %+v in %+v", want, rows)
}

func assertHelpItemsPresent(t *testing.T, rows [][]helplayout.HelpItem, wants ...helplayout.HelpItem) {
	t.Helper()
	flat := flattenHelpRows(rows)
	for _, want := range wants {
		assertHelpItemPresent(t, flat, want)
	}
}

func assertHelpItemAbsent(t *testing.T, rows []helplayout.HelpItem, deny helplayout.HelpItem) {
	t.Helper()
	for _, row := range rows {
		if row == deny {
			t.Fatalf("help rows unexpectedly contain %+v in %+v", deny, rows)
		}
	}
}

func assertStringContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("output missing %q:\n%s", want, got)
	}
}

func assertLineCount(t *testing.T, got string, want int) {
	t.Helper()
	if lines := strings.Split(got, "\n"); len(lines) != want {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), want, got)
	}
}

func assertLinesFitWidth(t *testing.T, got string, width int) {
	t.Helper()
	for i, line := range strings.Split(got, "\n") {
		if w := runewidth.StringWidth(line); w > width {
			t.Fatalf("line %d width=%d exceeds %d:\n%s", i+1, w, width, got)
		}
	}
}

func assertStringsLack(t *testing.T, got string, denials ...string) {
	t.Helper()
	for _, deny := range denials {
		if strings.Contains(got, deny) {
			t.Fatalf("output unexpectedly contains %q:\n%s", deny, got)
		}
	}
}

func assertContainsAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

// assertMaxGap finds the first lines containing topMarker and bottomMarker
// and fails if the bottom line trails the top by more than maxGap rows.
func assertMaxGap(t *testing.T, got, topMarker, bottomMarker string, maxGap int) {
	t.Helper()
	lines := strings.Split(got, "\n")
	top := indexOf(lines, topMarker)
	bottom := indexOf(lines, bottomMarker)
	if top < 0 || bottom < 0 {
		t.Fatalf("missing markers (%q=%d, %q=%d):\n%s", topMarker, top, bottomMarker, bottom, got)
	}
	if gap := bottom - top; gap > maxGap {
		t.Fatalf("%q→%q gap=%d exceeds %d:\n%s", topMarker, bottomMarker, gap, maxGap, got)
	}
}

package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// testIssueProjectID is the ProjectID used by the testIssue builder. It
// matches the projectID on the scope returned by newTestModel/homedScope so
// fixtures and model state agree by default.
const testIssueProjectID int64 = 7

// issueOpt mutates an Issue built by testIssue. Use the with* helpers
// below rather than constructing issueOpt values directly.
type issueOpt func(*Issue)

// testIssue builds an Issue with ProjectID=7, a synthetic UID derived
// from shortID, and a default title of "issue <shortID>". Tests that
// care about the title set it explicitly via withTitle.
func testIssue(shortID string, opts ...issueOpt) Issue {
	i := Issue{
		ProjectID: testIssueProjectID,
		UID:       "01TEST-" + shortID,
		ShortID:   shortID,
		Title:     fmt.Sprintf("issue %s", shortID),
	}
	for _, opt := range opts {
		opt(&i)
	}
	return i
}

func withTitle(title string) issueOpt {
	return func(i *Issue) { i.Title = title }
}

func withParent(parentShortID string) issueOpt {
	return func(i *Issue) {
		i.Parent = &LinkPeer{UID: "01TEST-" + parentShortID, ShortID: parentShortID}
	}
}

// withParentPeer points the issue's parent at a specific issue, carrying the
// peer's real UID — the form the daemon emits for cross-project parents.
func withParentPeer(parent Issue) issueOpt {
	return func(i *Issue) {
		i.Parent = &LinkPeer{UID: parent.UID, ShortID: parent.ShortID}
	}
}

// withBlocksPeer appends a blocks edge to a specific issue, carrying the
// peer's real UID — the form the daemon emits for cross-project blockers.
func withBlocksPeer(blocked Issue) issueOpt {
	return func(i *Issue) {
		i.Blocks = append(i.Blocks, LinkPeer{UID: blocked.UID, ShortID: blocked.ShortID})
	}
}

func inProject(projectID int64) issueOpt {
	return func(i *Issue) { i.ProjectID = projectID }
}

// withUID overrides the builder's shortID-derived UID. Needed when a test
// reuses one short_id across projects: short_ids are project-scoped, UIDs
// are global.
func withUID(uid string) issueOpt {
	return func(i *Issue) { i.UID = uid }
}

func withCounts(open, total int) issueOpt {
	return func(i *Issue) { i.ChildCounts = &ChildCounts{Open: open, Total: total} }
}

func withBlocks(blocks ...string) issueOpt {
	return func(i *Issue) {
		peers := make([]LinkPeer, len(blocks))
		for j, b := range blocks {
			peers[j] = LinkPeer{UID: "01TEST-" + b, ShortID: b}
		}
		i.Blocks = peers
	}
}

func withStatus(status string) issueOpt {
	return func(i *Issue) { i.Status = status }
}

func withLabels(labels ...string) issueOpt {
	return func(i *Issue) { i.Labels = labels }
}

// mockDaemon builds an httptest.Server that dispatches by URL path. The
// server sets Content-Type: application/json before invoking each handler,
// so handlers only need to write status + body. Unmapped paths return 404.
// The server is closed via t.Cleanup.
func mockDaemon(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h, ok := routes[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// projectNotInitializedHandler responds to /api/v1/projects/resolve with
// the 404 + project_not_initialized envelope used to drive the boot path
// into the empty-state / projects-view branches.
func projectNotInitializedHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": 404,
		"error":  map[string]any{"code": "project_not_initialized"},
	})
}

// useNoColor switches the package-level styles to colorNone for the
// duration of the test process. Tests that need ANSI-free output call
// this so assertions can use plain string matching.
func useNoColor(_ *testing.T) {
	applyColorMode(colorNone, false)
}

// renderBodyNoColor renders lm's body and strips ANSI escapes, the
// shape every list-render test wants when asserting on plain text.
func renderBodyNoColor(lm listModel, width, height int, chrome viewChrome) string {
	return stripANSI(lm.renderBody(width, height, chrome))
}

// newTestModel returns a Model wired with a stub Client, single-project
// scope (projectID=7), and list.loading=false — the baseline shared by
// most state-machine tests in this package.
func newTestModel() Model {
	m := initialModel(Options{})
	m.api = &Client{}
	m.scope = scope{projectID: 7}
	m.list.loading = false
	return m
}

// updateModel pipes msg through m.Update and unwraps the returned
// tea.Model interface back to the concrete Model struct.
func updateModel(m Model, msg tea.Msg) (Model, tea.Cmd) {
	out, cmd := m.Update(msg)
	return out.(Model), cmd
}

// sendRune dispatches a single-rune KeyMsg through m.Update and returns
// the resulting Model, collapsing the verbose tea.KeyPressMsg + type-assert
// boilerplate to one call.
func sendRune(m Model, r rune) Model {
	nm, _ := updateModel(m, keyRune(r))
	return nm
}

// keyRune builds a single-rune tea.KeyPressMsg for tests that route keys
// directly (e.g. routeProjectsViewKey) rather than through m.Update.
func keyRune(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// sendKey dispatches a special-key KeyPressMsg (e.g. tea.KeyDown,
// tea.KeyTab, tea.KeyEnter — rune constants in Bubble Tea v2) through
// m.Update and returns the resulting Model.
func sendKey(m Model, code rune) Model {
	nm, _ := updateModel(m, tea.KeyPressMsg{Code: code})
	return nm
}

// resizeModel sends a WindowSizeMsg through m.Update and returns the
// resulting Model, collapsing the WindowSizeMsg + type-assert
// boilerplate that every layout/chrome test repeats.
func resizeModel(m Model, width, height int) Model {
	nm, _ := updateModel(m, tea.WindowSizeMsg{Width: width, Height: height})
	return nm
}

// makeTestIssues returns count Issues with stable "row" titles and
// deterministic UID/ShortID fields, suitable for pagination and viewport
// boundary tests.
func makeTestIssues(count int) []Issue {
	issues := make([]Issue, count)
	for i := range issues {
		sid := fmt.Sprintf("r%03d", i+1)
		issues[i] = Issue{
			ProjectID: testIssueProjectID,
			UID:       "01TEST-" + sid,
			ShortID:   sid,
			Title:     "row",
		}
	}
	return issues
}

// mockProject bundles the three parallel maps the projects view consults
// (projectsByID, projectIdentByID, projectStats) into one record so test
// fixtures can list projects without juggling three coordinated literals.
type mockProject struct {
	ID    int64
	Name  string
	Ident string
	Stats ProjectStatsSummary
}

// injectProjects populates the model's three parallel project maps from
// the given mockProject records.
func injectProjects(m *Model, projects ...mockProject) {
	for _, p := range projects {
		m.projectsByID[p.ID] = p.Name
		m.projectIdentByID[p.ID] = p.Ident
		m.projectStats[p.ID] = p.Stats
	}
}

// setupProjectsView returns a Model in viewProjects with standard 120×24
// dimensions, optionally pre-populated with the given mockProject rows.
func setupProjectsView(projects ...mockProject) Model {
	m := initialModel(Options{})
	m.view = viewProjects
	m.width, m.height = 120, 24
	injectProjects(&m, projects...)
	return m
}

// setupEmptyView returns a Model in viewEmpty with standard 80×24
// dimensions — the baseline for empty-state rendering and key-handling
// tests.
func setupEmptyView() Model {
	m := initialModel(Options{})
	m.view = viewEmpty
	m.width, m.height = 80, 24
	return m
}

// assertCmdQuit fails t if cmd is nil or its result is not tea.QuitMsg.
// The shape every quit-path assertion repeats: a nil cmd means the
// keystroke didn't trigger Bubble Tea's quit at all.
func assertCmdQuit(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("cmd = nil, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("cmd = %T, want tea.QuitMsg", cmd())
	}
}

// homedScope builds a scope where the active project equals the home
// project — the common shape for tests that need a single-project scope
// already settled (i.e. came from a list, not a fresh boot).
func homedScope(id int64, name string) scope {
	return scope{projectID: id, projectName: name, homeProjectID: id, homeProjectName: name}
}

// assertContains fails t if subject does not contain substr. failMsg
// describes the assertion (the helper appends the missing substring and
// the full subject to keep diagnostics consistent across tests).
func assertContains(t *testing.T, subject, substr, failMsg string) {
	t.Helper()
	if !strings.Contains(subject, substr) {
		t.Fatalf("%s. Expected to find %q in:\n%s", failMsg, substr, subject)
	}
}

// assertNotContains is the inverse of assertContains.
func assertNotContains(t *testing.T, subject, substr, failMsg string) {
	t.Helper()
	if strings.Contains(subject, substr) {
		t.Fatalf("%s. Did not expect to find %q in:\n%s", failMsg, substr, subject)
	}
}

// assertStyleForeground fails t if style's foreground differs from the
// color lipgloss.Color(want) resolves to. Collapses the recurring
// compare block in style tests.
func assertStyleForeground(t *testing.T, style lipgloss.Style, label, want string) {
	t.Helper()
	if got, wantC := style.GetForeground(), lipgloss.Color(want); got != wantC {
		t.Fatalf("%s foreground = %v, want %v (%q)", label, got, wantC, want)
	}
}

// assertTerminalColor fails t if tc differs from the color
// lipgloss.Color(want) resolves to. Used for raw color.Color vars
// (e.g. panel border colors) that aren't wrapped in a Style.
func assertTerminalColor(t *testing.T, tc color.Color, label, want string) {
	t.Helper()
	if wantC := lipgloss.Color(want); tc != wantC {
		t.Fatalf("%s = %v, want %v (%q)", label, tc, wantC, want)
	}
}

// assertSelection fails t if either the cursor row or the recorded
// selectedUID identity differs from the wants.
func assertSelection(t *testing.T, m Model, wantCursor int, wantUID string) {
	t.Helper()
	if m.list.cursor != wantCursor {
		t.Fatalf("cursor = %d, want %d", m.list.cursor, wantCursor)
	}
	if m.list.selectedUID != wantUID {
		t.Fatalf("selectedUID = %q, want %q", m.list.selectedUID, wantUID)
	}
}

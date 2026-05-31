// Package tui implements the kata terminal UI built on Bubble Tea.
package tui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

const defaultHTTPTimeout = 5 * time.Second
const remoteProbeTimeout = time.Second

type sseStarter func(context.Context, sseClient, string, *int64, chan tea.Msg, uint64)

type sseRestartState struct {
	root   context.Context
	start  sseStarter
	mu     sync.Mutex
	latest uint64
	cancel context.CancelFunc
}

func newSSERestartState(root context.Context, cancel context.CancelFunc, start sseStarter) *sseRestartState {
	return &sseRestartState{
		root:   root,
		start:  start,
		cancel: cancel,
	}
}

func (s *sseRestartState) restart(conn daemonConnection, gen uint64, ch chan tea.Msg) tea.Cmd {
	s.mu.Lock()
	if gen > s.latest {
		s.latest = gen
	}
	s.mu.Unlock()
	return func() tea.Msg {
		s.mu.Lock()
		if gen != s.latest {
			s.mu.Unlock()
			return nil
		}
		s.cancel()
		sseCtx, cancel := context.WithCancel(s.root)
		s.cancel = cancel
		shouldStart := !conn.init.scope.empty && conn.sseHC != nil
		s.mu.Unlock()

		if shouldStart {
			s.start(sseCtx, conn.sseHC, conn.endpoint, sseProjectScope(conn.init.scope), ch, gen)
		}
		return nil
	}
}

func (s *sseRestartState) cancelCurrent() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	cancel()
}

// Options controls TUI behavior. Stable across versions; new fields
// must be optional.
//
// IncludeDeleted is intentionally absent: the daemon's ListIssuesRequest
// (internal/api/types.go) does not accept include_deleted, and
// db.ListIssues hard-codes deleted_at IS NULL, so there is no way for
// the TUI to surface soft-deleted rows today. Re-introducing the flag
// is deferred to a follow-up that adds wire + handler support.
//
// AllProjects is intentionally absent from Options: the boot flow
// always starts in single-project mode (resolved from the cwd) or empty
// state, and users toggle to all-projects via the R binding at runtime.
// Adding a CLI flag is reasonable as a future ergonomic but isn't
// required for the navigation surface.
type Options struct {
	Stdout           io.Writer // typically os.Stdout
	Stderr           io.Writer // typically os.Stderr
	DisplayUIDFormat string    // none, short, or full
	Mouse            bool      // opt-in mouse capture and mouse-driven navigation
}

// Run starts the TUI. Blocks until the user quits or ctx is cancelled.
// Returns nil on clean exit. Returns errNotATTY when stdin or the
// active output stream is not a terminal so callers can print a
// friendly message.
func Run(ctx context.Context, opts Options) error {
	if !isTerminal(os.Stdin) || !outputIsTerminal(opts.Stdout) {
		return errNotATTY
	}
	c, sseHC, bi, endpoint, conn, err := bootClient(ctx, opts)
	if err != nil {
		return err
	}
	m := buildRunModel(opts, c, bi, conn)
	sseCtx, cancelSSE := context.WithCancel(ctx)
	startSSE := func(ctx context.Context, hc sseClient, endpoint string, projectID *int64, ch chan tea.Msg, gen uint64) {
		go startSSEForConnection(ctx, hc, endpoint, projectID, ch, gen)
	}
	sseRestart := newSSERestartState(ctx, cancelSSE, startSSE)
	defer sseRestart.cancelCurrent()
	m.connGen = 1
	m.sseRestart = sseRestart.restart
	if !bi.scope.empty && sseHC != nil {
		startSSE(sseCtx, sseHC, endpoint, sseProjectScope(bi.scope), m.sseCh, m.connGen)
	}
	if _, err := tea.NewProgram(m, programOpts(ctx, opts)...).Run(); err != nil {
		return err
	}
	return nil
}

// buildRunModel seeds the initial model with the resolved client,
// scope, and view. When the boot path landed on viewProjects, the
// pre-fetched project rows are seeded into the cache maps so the first
// frame renders with stats.
func buildRunModel(opts Options, c *Client, bi bootInit, conns ...daemonConnection) Model {
	m := initialModel(opts)
	// Guard against a typed-nil *Client becoming a non-nil KataAPI:
	// only assign when c carries a value, so m.api stays a true nil
	// interface otherwise and m.api != nil checks remain correct.
	if c != nil {
		m.api = c
	}
	m.scope = bi.scope
	m.view = bi.view
	if len(conns) > 0 {
		m.activeDaemon = conns[0].target
		m.daemonTargets = conns[0].catalog
	}
	if len(bi.projects) > 0 {
		m.projectsByID = make(map[int64]string, len(bi.projects))
		m.projectIdentByID = make(map[int64]string, len(bi.projects))
		m.projectStats = make(map[int64]ProjectStatsSummary, len(bi.projects))
		for _, r := range bi.projects {
			m.projectsByID[r.ID] = r.Name
			m.projectIdentByID[r.ID] = r.Name
			if r.Stats != nil {
				m.projectStats[r.ID] = *r.Stats
			}
		}
		m.projectsCursor = cursorForScope(projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats), bi.scope)
	}
	return m
}

// programOpts returns the tea.ProgramOption slice for tea.NewProgram.
// Splitting this off Run keeps Run's cyclomatic complexity within the
// project's ≤8 limit.
func programOpts(ctx context.Context, opts Options) []tea.ProgramOption {
	out := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithAltScreen(),
	}
	if opts.Mouse {
		// Opt-in only: mouse tracking blocks native text selection in many
		// terminals. CellMotion captures clicks/releases/wheel without idle
		// all-motion churn; users can hold Option (macOS) or Shift (Linux)
		// to bypass tracking for native selection.
		out = append(out, tea.WithMouseCellMotion())
	}
	if opts.Stdout != nil {
		out = append(out, tea.WithOutput(opts.Stdout))
	}
	return out
}

// sseProjectScope picks the project_id pointer to thread into startSSE.
// Always returns nil so the SSE stream carries every project's events
// regardless of the current scope. The TUI filters per-message via
// Model.eventAffectsView, so a user who toggles into all-projects mode
// (R binding) sees events from projects that weren't in scope at boot
// without restarting the SSE goroutine.
func sseProjectScope(_ scope) *int64 {
	return nil
}

// bootClient discovers the daemon, constructs the typed HTTP client, the
// streaming-only client used for SSE, and resolves the initial scope.
// Splitting this off Run keeps Run's cyclomatic complexity inside the
// project's ≤8 hard limit and isolates the network preflight from the
// Bubble Tea wiring.
//
// The SSE client is built with no overall Client.Timeout (only a 10s
// response-header ceiling) so a long-lived stream isn't reaped after 5s.
// We re-use NewHTTPClient with ResponseHeaderTimeout instead of building
// a bespoke transport so unix-socket dialing stays in one place.
func bootClient(ctx context.Context, opts Options) (*Client, *http.Client, bootInit, string, daemonConnection, error) {
	conn, err := bootDaemonConnection(ctx, opts)
	if err != nil {
		return nil, nil, bootInit{}, "", daemonConnection{}, err
	}
	return conn.api, conn.sseHC, conn.init, conn.endpoint, conn, nil
}

// scope describes the issue-set the TUI is browsing. Exactly one of
// projectID, allProjects, empty is set. The boot path drives the initial
// values; runtime transitions in viewProjects mutate scope before
// transitioning to viewList.
//
// homeProjectID/homeProjectName capture the project bootResolveScope
// picked from the cwd. They're zero when boot landed in viewProjects
// or viewEmpty.
type scope struct {
	projectID       int64
	allProjects     bool
	empty           bool
	projectName     string
	workspace       string
	homeProjectID   int64
	homeProjectName string
}

// bootInit packages the resolved scope, the initial view, and any
// projects fetched during boot. When the boot path resolves into
// viewProjects, projects holds the rows from ListProjectsWithStats so
// the first frame can render with stats — no second roundtrip. For
// viewList and viewEmpty, projects is nil.
type bootInit struct {
	scope    scope
	view     viewID
	projects []ProjectSummaryWithStats
}

// bootResolveScope picks the initial scope + view from cwd. Spec §4.2:
//
//  1. POST /projects/resolve(cwd) success → single-project scope, viewList.
//  2. project_not_initialized + ≥1 registered project → empty scope,
//     viewProjects (the user browses the workspace). The fetched rows
//     are returned alongside so the model can render with stats on
//     the first frame.
//  3. project_not_initialized + 0 projects → empty scope (sc.empty=true),
//     viewEmpty.
//  4. Any other resolve error → propagate so Run fails loudly. Once we
//     cross the resolve gate (case 2 or 3), the projects-list call is
//     non-optional and a failure there is also treated as boot failure.
//
// On error, the bootInit is the zero value; callers must check err first.
func bootResolveScope(ctx context.Context, c *Client, cwd string) (bootInit, error) {
	rr, err := c.ResolveProject(ctx, cwd)
	if err == nil {
		return bootInit{
			scope: scope{
				projectID:       rr.Project.ID,
				projectName:     rr.Project.Name,
				workspace:       rr.WorkspaceRoot,
				homeProjectID:   rr.Project.ID,
				homeProjectName: rr.Project.Name,
			},
			view: viewList,
		}, nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "project_not_initialized" {
		return bootInit{}, err
	}
	rows, err := c.ListProjectsWithStats(ctx)
	if err != nil {
		return bootInit{}, err
	}
	if len(rows) == 0 {
		return bootInit{scope: scope{empty: true}, view: viewEmpty}, nil
	}
	return bootInit{view: viewProjects, projects: rows}, nil
}

// errNotATTY indicates the TUI was launched outside a terminal.
var errNotATTY = errors.New("kata tui requires a terminal (stdin/stdout must be a tty)")

// isTerminal reports whether f is connected to a real terminal. We use
// golang.org/x/term so /dev/null and other character devices do not
// pass (an os.ModeCharDevice check would let those through).
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd())) //nolint:gosec // G115: fd fits int on every supported OS.
}

// outputIsTerminal validates the writer the TUI will actually render to.
// A nil opts.Stdout means "use os.Stdout". Only *os.File values can be
// terminals — bytes.Buffer and other in-memory writers always fail this
// check so Run refuses to emit alt-screen control sequences into a sink
// that cannot honor them.
func outputIsTerminal(w io.Writer) bool {
	if w == nil {
		return isTerminal(os.Stdout)
	}
	if f, ok := w.(*os.File); ok {
		return isTerminal(f)
	}
	return false
}

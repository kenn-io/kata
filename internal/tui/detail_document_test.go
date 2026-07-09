package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

func TestDetailDocumentPage80x50LayoutSignals(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailHierarchyFixture()
	dm.issue.Owner = ptrString("alice")
	dm.issue.Labels = []string{"prio-1", "bug", "needs-design"}
	dm.issue.CreatedAt = time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	dm.issue.UpdatedAt = snapshotFixedNow.Add(-3 * time.Hour)

	got := stripANSI(dm.View(80, 50, viewChrome{
		scope:   scope{projectID: 7, projectName: "kata"},
		version: "dev",
	}))

	assertLineCount(t, got, 50)
	assertLinesFitWidth(t, got, 80)
	for _, want := range []string{
		"#42aa",
		"fix login bug on Safari",
		"[open]",
		"authored by wesm",
		"created Apr 30 10:00",
		"updated 3h ago",
		"owner: alice",
		"labels: [bug] [needs-design] [prio-1]",
		"parent: #c012 workspace polish parent",
		"priority: none",
		"children: 1 open / 2 total",
		"Body",
		"Children",
		"Activity",
		"[ Comments (2) ]",
	} {
		assertStringContains(t, got, want)
	}
	assertStringsLack(t, got, "Owner:", "Parent:")
}

func TestDetailCompactSheet_UsesDenseRhythmAndNoDecorativeRules(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.issue.Owner = ptrString("alice")
	dm.issue.Labels = []string{"improvement"}

	got := stripANSI(dm.View(80, 40, viewChrome{}))
	lines := strings.Split(got, "\n")
	title := indexOf(lines, "fix login bug on Safari")
	if title < 0 {
		t.Fatalf("missing title:\n%s", got)
	}
	if !strings.Contains(lines[title], "#42") {
		t.Fatalf("issue number should share the title row:\n%s", got)
	}
	if strings.Contains(got, "issue #42") {
		t.Fatalf("compact sheet should not render issue number as a separate lead-in:\n%s", got)
	}
	assertStringsLack(t, got, "Body ─", "Activity ─")
	// The redesign separates metadata and Body with one blank breather
	// row so the page reads as a quiet document. Body should land
	// within two rows of the last metadata line — close enough to feel
	// connected, not far enough to leak dead air. Same rhythm between
	// body content and Activity.
	assertMaxGap(t, got, "labels:", "Body", 2)
	assertMaxGap(t, got, "Click the login button twice.", "Activity", 3)
}

// TestDetailCompactSheet_AdaptiveSurfaces locks down the redesigned
// surface choices: only the markdown code block keeps a subtle
// background in color modes; the metadata band and section header
// styles render flat. Earlier iterations painted full-width slabs
// behind those rows and looked like heavy chrome competing with the
// issue body — the redesign drops them so the page reads as a quiet
// document.
func TestDetailCompactSheet_AdaptiveSurfaces(t *testing.T) {
	applyColorMode(colorDark, true)
	if styleHasBackground(detailMetaStyle) {
		t.Fatal("detailMetaStyle should not paint a background slab in color modes")
	}
	if styleHasBackground(detailSectionHeaderStyle) {
		t.Fatal("detailSectionHeaderStyle should not paint a background slab in color modes")
	}
	if markdownCodeBlockBackground() == nil {
		t.Fatal("markdown code blocks need a subtle background in color modes")
	}

	applyColorMode(colorNone, false)
	if styleHasBackground(detailMetaStyle) {
		t.Fatal("detailMetaStyle must not paint a background in colorNone")
	}
	if styleHasBackground(detailSectionHeaderStyle) {
		t.Fatal("detailSectionHeaderStyle must not paint a background in colorNone")
	}
	if markdownCodeBlockBackground() != nil {
		t.Fatal("markdown code block background must be disabled in colorNone")
	}
}

func TestMarkdownCodeBlockBackground_RespectsAutoDetectedBackground(t *testing.T) {
	oldMode := activeColorMode
	oldDark := activeHasDarkBackground
	defer func() { applyColorMode(oldMode, oldDark) }()

	// In v2 the light/dark pick happens inside applyColorMode (driven
	// by tea.BackgroundColorMsg in colorAuto), not at read time.
	applyColorMode(colorAuto, false)
	if bg := markdownCodeBlockBackground(); bg == nil || *bg != "252" {
		t.Fatalf("light terminal background = %v, want 252", bg)
	}
	applyColorMode(colorAuto, true)
	if bg := markdownCodeBlockBackground(); bg == nil || *bg != "236" {
		t.Fatalf("dark terminal background = %v, want 236", bg)
	}
}

func styleHasBackground(s lipgloss.Style) bool {
	switch s.GetBackground().(type) {
	case nil:
		return false
	case lipgloss.NoColor:
		return false
	default:
		return true
	}
}

func TestDetailDocument_DoesNotPadBodyBeforeChildren(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailHierarchyFixture()

	got := stripANSI(dm.View(80, 50, viewChrome{}))
	assertMaxGap(t, got, "Click the login button twice.", "Children", 4)
}

// TestDetailDocument_NarrowStacksMetadata verifies that on a narrow
// terminal where owner+parent cannot fit on one row, the metadata
// stacks vertically rather than overflowing the sheet. Empty
// labels/children are still omitted entirely — the stacking is a
// fallback for the present rows, not an excuse to render placeholders.
func TestDetailDocument_NarrowStacksMetadata(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.issue.Owner = ptrString("alice")
	dm.issue.Labels = []string{"bug", "prio-1"}
	dm.parent = &IssueRef{ShortID: "12pp", Title: "workspace polish", Status: "open"}

	got := stripANSI(dm.View(72, 40, viewChrome{}))
	assertLinesFitWidth(t, got, 72)
	assertStringContains(t, got, "owner: alice")
	assertStringContains(t, got, "labels: [bug] [prio-1]")
	assertStringContains(t, got, "parent: #12pp workspace polish")
	if strings.Contains(got, "children: none") {
		t.Fatalf("empty children should be omitted, not labeled `children: none`:\n%s", got)
	}
}

func TestDetailDocument_EmptyBodyAndActivityOmitted(t *testing.T) {
	defer snapshotInit(t)()
	iss := Issue{ProjectID: 7, UID: "01TEST-99", ShortID: "99zz", Title: "empty issue", Status: "open"}
	dm := detailModel{issue: &iss}

	got := stripANSI(dm.View(80, 32, viewChrome{}))
	assertStringContains(t, got, "(no description)")
	if strings.Contains(got, "Activity") {
		t.Fatalf("detail document should omit all-empty activity section:\n%s", got)
	}
}

func TestDetailDocument_LongTitleKeepsStatusVisible(t *testing.T) {
	defer snapshotInit(t)()
	title := "this is a very long issue title that should truncate before it can collide with the status pill"
	iss := Issue{ProjectID: 7, UID: "01TEST-77", ShortID: "77yy", Title: title, Status: "closed"}
	dm := detailModel{issue: &iss}

	got := stripANSI(dm.View(80, 32, viewChrome{}))
	assertLinesFitWidth(t, got, 80)
	assertStringContains(t, got, "[closed]")
	if !strings.Contains(got, "…") {
		t.Fatalf("expected long title truncation:\n%s", got)
	}
}

func TestDetailDocument_MarkdownRenderingDropsSourceFences(t *testing.T) {
	defer snapshotInit(t)()
	iss := Issue{
		ProjectID: 7,
		UID:       "01TEST-55",
		ShortID:   "55ww",
		Title:     "markdown body",
		Status:    "open",
		Body: strings.Join([]string{
			"## Steps",
			"",
			"- Click `Login` twice.",
			"",
			"```go",
			`fmt.Println("ok")`,
			"```",
		}, "\n"),
	}
	dm := detailModel{issue: &iss}

	got := stripANSI(dm.View(80, 40, viewChrome{}))
	for _, want := range []string{"Steps", "`Login`", `fmt.Println("ok")`} {
		assertStringContains(t, got, want)
	}
	assertStringsLack(t, got, "## Steps", "```")
}

func TestDetailDocument_CommentAuthorsAlignTimestamps(t *testing.T) {
	defer snapshotInit(t)()
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	comments := []CommentEntry{
		{Author: "alice", Body: "first", CreatedAt: when},
		{Author: "bob", Body: "second", CreatedAt: when.Add(time.Hour)},
	}

	got := stripANSI(renderCommentsTab(comments, 80, 10, 0, tabState{}))
	assertStringContains(t, got, "alice  Apr 30 10:00")
	assertStringContains(t, got, "bob    Apr 30 11:00")
}

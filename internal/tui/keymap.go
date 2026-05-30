package tui

import tea "github.com/charmbracelet/bubbletea"

// keymap is the single source of truth for keybindings. The help view
// reads from this same value so rendered help stays in lockstep with
// what Update actually handles.
type keymap struct {
	Help, Quit                                     key
	Projects, Daemons                              key
	ToggleLayout                                   key
	Up, Down, PageUp, PageDown, Home, End          key
	ScrollUp, ScrollDown                           key
	Open, NewIssue, NewChild, Search               key
	ExpandCollapse, Expand, Collapse               key
	SortChildren                                   key
	FilterStatus, FilterForm, ClearFilters         key
	Close, Reopen                                  key
	NextTab, PrevTab, JumpRef, Back                key
	EditBody, NewComment                           key
	SetParent, AddBlocker, AddLink                 key
	AddLabel, RemoveLabel, AssignOwner, ClearOwner key
	SetPriority                                    key
}

// key is a binding plus its human label. matches() compares against the
// canonical string Bubble Tea reports for a KeyMsg.
type key struct {
	Keys []string
	Help string
}

// newKeymap returns the spec §7.3 bindings.
func newKeymap() keymap {
	return keymap{
		Help:         key{Keys: []string{"?"}, Help: "help"},
		Quit:         key{Keys: []string{"q", "ctrl+c"}, Help: "quit"},
		Projects:     key{Keys: []string{"P"}, Help: "projects"},
		Daemons:      key{Keys: []string{"D"}, Help: "daemons"},
		ToggleLayout: key{Keys: []string{"L"}, Help: "toggle layout"},
		// Up/Down are the section-cursor bindings: in the detail view
		// they move the activity tab cursor or the children cursor.
		// ScrollUp/ScrollDown are the document-viewport scroll bindings:
		// in the detail view they slide the whole detail document one
		// line at a time. The list view binds both pairs to the same
		// cursor handler so existing j/k + arrow ergonomics survive.
		Up:             key{Keys: []string{"k"}, Help: "up"},
		Down:           key{Keys: []string{"j"}, Help: "down"},
		ScrollUp:       key{Keys: []string{"up"}, Help: "scroll up"},
		ScrollDown:     key{Keys: []string{"down"}, Help: "scroll down"},
		PageUp:         key{Keys: []string{"pgup"}, Help: "page up"},
		PageDown:       key{Keys: []string{"pgdown"}, Help: "page down"},
		Home:           key{Keys: []string{"g"}, Help: "first"},
		End:            key{Keys: []string{"G"}, Help: "last"},
		Open:           key{Keys: []string{"enter"}, Help: "open detail"},
		NewIssue:       key{Keys: []string{"n"}, Help: "new issue (form)"},
		NewChild:       key{Keys: []string{"N"}, Help: "new child"},
		ExpandCollapse: key{Keys: []string{" "}, Help: "expand/collapse"},
		Expand:         key{Keys: []string{"right"}, Help: "expand"},
		Collapse:       key{Keys: []string{"left"}, Help: "collapse"},
		SortChildren:   key{Keys: []string{"o"}, Help: "toggle graph order"},
		Search:         key{Keys: []string{"/"}, Help: "search"},
		FilterStatus:   key{Keys: []string{"s"}, Help: "cycle status filter"},
		FilterForm:     key{Keys: []string{"f"}, Help: "filter (form)"},
		// Plan 8 commit 5a: the o key (filter by owner) was subsumed
		// by the f filter modal. The s/ /c keys stay as cheap paths
		// for common single-axis gestures (Q6 hybrid).
		// Label filter is intentionally absent from v1 of the modal:
		// the Issue projection drops Labels (Task 3 wire-vs-spec
		// adaptation #1), so a TUI label filter could not actually
		// narrow the displayed list. Plan 8 commit 5b will add the
		// daemon LabelsByIssues hook + the Labels axis.
		ClearFilters: key{Keys: []string{"c"}, Help: "clear filters"},
		Close:        key{Keys: []string{"x"}, Help: "close"},
		Reopen:       key{Keys: []string{"r"}, Help: "reopen"},
		NextTab:      key{Keys: []string{"tab"}, Help: "next tab"},
		PrevTab:      key{Keys: []string{"shift+tab"}, Help: "prev tab"},
		JumpRef:      key{Keys: []string{"enter"}, Help: "jump to referenced issue"},
		Back:         key{Keys: []string{"esc", "backspace"}, Help: "back"},
		EditBody:     key{Keys: []string{"e"}, Help: "edit body"},
		NewComment:   key{Keys: []string{"c"}, Help: "new comment"},
		SetParent:    key{Keys: []string{"p"}, Help: "set parent"},
		AddBlocker:   key{Keys: []string{"b"}, Help: "add blocker"},
		AddLink:      key{Keys: []string{"l"}, Help: "add related"},
		AddLabel:     key{Keys: []string{"+"}, Help: "add label"},
		RemoveLabel:  key{Keys: []string{"-"}, Help: "remove label"},
		AssignOwner:  key{Keys: []string{"a"}, Help: "assign owner"},
		ClearOwner:   key{Keys: []string{"A"}, Help: "clear owner"},
		SetPriority:  key{Keys: []string{"!"}, Help: "set priority"},
	}
}

// matches reports whether msg is one of k's bound keys.
func (k key) matches(msg tea.KeyMsg) bool {
	s := msg.String()
	for _, b := range k.Keys {
		if s == b {
			return true
		}
	}
	return false
}

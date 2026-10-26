package cron

import (
	"slices"
)

// keymap is shared by input routing and the footer hints.
type keymap struct {
	Quit         key
	Interrupt    key
	Kata         key
	KataTab      key
	JobsTab      key
	RunsTab      key
	WorkflowsTab key
	NextTab      key
	PrevTab      key
	Layout       key
	Down         key
	Up           key
	ScrollUp     key
	ScrollDown   key
	Left         key
	Right        key
	Home         key
	End          key
	DocHome      key
	DocEnd       key
	LineHome     key
	LineEnd      key
	PrevPage     key
	NextPage     key
	PrevPageAlt  key
	NextPageAlt  key
	PageUp       key
	PageDown     key
	HalfDown     key
	HalfUp       key
	Back         key
	Open         key
	Accept       key
	Cancel       key
	Backspace    key
	Toggle       key
	Search       key
	Reload       key
	Pause        key
	New          key
	Edit         key
	Delete       key
	Issue        key
	Status       key
	Related      key
	Save         key
	SaveAlt      key
	Yes          key
	DeleteChar   key
}

type key struct {
	Keys          []string
	Help, Display string
}

func newKeymap() keymap {
	return keymap{
		Quit:         key{Keys: []string{"q"}, Help: "quit", Display: "q"},
		Interrupt:    key{Keys: []string{"ctrl+c"}, Help: "quit", Display: "ctrl+c"},
		Kata:         key{Keys: []string{"K"}, Help: "kata", Display: "K"},
		KataTab:      key{Keys: []string{"1"}, Help: "katatab", Display: "1"},
		JobsTab:      key{Keys: []string{"2"}, Help: "jobstab", Display: "2"},
		RunsTab:      key{Keys: []string{"3"}, Help: "runstab", Display: "3"},
		WorkflowsTab: key{Keys: []string{"4"}, Help: "workflowstab", Display: "4"},
		NextTab:      key{Keys: []string{"tab"}, Help: "nexttab", Display: "tab"},
		PrevTab:      key{Keys: []string{"shift+tab"}, Help: "prevtab", Display: "shift+tab"},
		Layout:       key{Keys: []string{"L"}, Help: "layout", Display: "L"},
		Down:         key{Keys: []string{"j"}, Help: "down", Display: "j"},
		Up:           key{Keys: []string{"k"}, Help: "up", Display: "k"},
		ScrollUp:     key{Keys: []string{"up"}, Help: "scrollup", Display: "up"},
		ScrollDown:   key{Keys: []string{"down"}, Help: "scrolldown", Display: "down"},
		Left:         key{Keys: []string{"left"}, Help: "left", Display: "left"},
		Right:        key{Keys: []string{"right"}, Help: "right", Display: "right"},
		Home:         key{Keys: []string{"home"}, Help: "home", Display: "home"},
		End:          key{Keys: []string{"end"}, Help: "end", Display: "end"},
		DocHome:      key{Keys: []string{"ctrl+home"}, Help: "dochome", Display: "ctrl+home"},
		DocEnd:       key{Keys: []string{"ctrl+end"}, Help: "docend", Display: "ctrl+end"},
		LineHome:     key{Keys: []string{"ctrl+a"}, Help: "linehome", Display: "ctrl+a"},
		LineEnd:      key{Keys: []string{"ctrl+e"}, Help: "lineend", Display: "ctrl+e"},
		PrevPage:     key{Keys: []string{"["}, Help: "prevpage", Display: "["},
		NextPage:     key{Keys: []string{"]"}, Help: "nextpage", Display: "]"},
		PrevPageAlt:  key{Keys: []string{"ctrl+b"}, Help: "prevpagealt", Display: "ctrl+b"},
		NextPageAlt:  key{Keys: []string{"ctrl+f"}, Help: "nextpagealt", Display: "ctrl+f"},
		PageUp:       key{Keys: []string{"pgup"}, Help: "pageup", Display: "pgup"},
		PageDown:     key{Keys: []string{"pgdown"}, Help: "pagedown", Display: "pgdown"},
		HalfDown:     key{Keys: []string{"ctrl+d"}, Help: "halfdown", Display: "ctrl+d"},
		HalfUp:       key{Keys: []string{"ctrl+u"}, Help: "halfup", Display: "ctrl+u"},
		Back:         key{Keys: []string{"h"}, Help: "back", Display: "h"},
		Open:         key{Keys: []string{"l"}, Help: "open", Display: "l"},
		Accept:       key{Keys: []string{"enter"}, Help: "choose", Display: "enter"},
		Cancel:       key{Keys: []string{"esc"}, Help: "cancel", Display: "esc"},
		Backspace:    key{Keys: []string{"backspace"}, Help: "backspace", Display: "backspace"},
		Toggle:       key{Keys: []string{"space"}, Help: "toggle", Display: "space"},
		Search:       key{Keys: []string{"/"}, Help: "search", Display: "/"},
		Reload:       key{Keys: []string{"r"}, Help: "reload", Display: "r"},
		Pause:        key{Keys: []string{"p"}, Help: "pause", Display: "p"},
		New:          key{Keys: []string{"n"}, Help: "new", Display: "n"},
		Edit:         key{Keys: []string{"e"}, Help: "edit", Display: "e"},
		Delete:       key{Keys: []string{"D"}, Help: "delete", Display: "D"},
		Issue:        key{Keys: []string{"i"}, Help: "issue", Display: "i"},
		Status:       key{Keys: []string{"f"}, Help: "status", Display: "f"},
		Related:      key{Keys: []string{"o"}, Help: "Open in AgentsView", Display: "o"},
		Save:         key{Keys: []string{"ctrl+s"}, Help: "save", Display: "ctrl+s"},
		SaveAlt:      key{Keys: []string{"S"}, Help: "save", Display: "S"},
		Yes:          key{Keys: []string{"y"}, Help: "yes", Display: "y"},
		DeleteChar:   key{Keys: []string{"delete"}, Help: "deletechar", Display: "delete"},
	}
}

var keys = newKeymap()

func (k key) matchesString(name string) bool {
	return slices.Contains(k.Keys, name)
}
func matchesKey(name string, bindings ...key) bool {
	for _, binding := range bindings {
		if binding.matchesString(name) {
			return true
		}
	}
	return false
}

func (k key) hint(priority int) helpItem {
	return helpItem{Key: k.Display, Desc: k.Help, Prio: priority, bindings: append([]string(nil), k.Keys...)}
}

package notionsync

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

// StatusSchema is a validated live view; never persist its option order as a
// second completion authority. Fingerprint excludes names, colors and order.
type StatusSchema struct {
	CompleteGroup, TodoGroup Group
	Fingerprint              string
	closedTarget, openTarget string
	options, completed       map[string]bool
}

func selectStatusGroup(groups []Group, selector, defaultName string, required bool) (Group, error) {
	if selector == "" {
		selector = defaultName
	}
	options := make([]Option, 0, len(groups))
	choices := make([]string, 0, len(groups))
	for _, group := range groups {
		options = append(options, Option{ID: group.ID, Name: group.Name})
		choices = append(choices, fmt.Sprintf("%s (%s)", group.Name, group.ID))
	}
	selected, err := selectOption(options, selector)
	if err != nil {
		if !required {
			// An omitted optional To-do group is allowed in one-way mode, but
			// ambiguous matches still require an explicit selection.
			matches := 0
			for _, group := range groups {
				if group.ID == selector || group.Name == selector {
					matches++
				}
			}
			if matches == 0 {
				return Group{}, nil
			}
		}
		return Group{}, fmt.Errorf("select exactly one Notion %s group; choices: %s", defaultName, strings.Join(choices, ", "))
	}
	for _, group := range groups {
		if group.ID == selected.ID {
			return group, nil
		}
	}
	return Group{}, fmt.Errorf("selected Notion workflow group is missing")
}

// ResolveStatusSchema validates the current selected property and its workflow
// memberships, then resolves defaults from each group's live option order.
func ResolveStatusSchema(c Config, ds DataSource) (StatusSchema, error) {
	c, err := normalizeConfig(c)
	if err != nil {
		return StatusSchema{}, err
	}
	id, err := canonicalID(ds.ID)
	if err != nil || id != c.DataSourceID {
		return StatusSchema{}, fmt.Errorf("notion status schema belongs to a different data source")
	}
	var property Property
	count := 0
	for _, p := range ds.Properties {
		if p.ID == c.StatusPropertyID {
			property = p
			count++
		}
	}
	if count != 1 || property.Type != "status" {
		return StatusSchema{}, fmt.Errorf("selected Notion status property is missing or has changed type")
	}
	r := StatusSchema{options: map[string]bool{}, completed: map[string]bool{}}
	for _, option := range property.Options {
		if strings.TrimSpace(option.ID) == "" || r.options[option.ID] {
			return StatusSchema{}, fmt.Errorf("notion status option IDs must be unique and nonempty")
		}
		r.options[option.ID] = true
	}
	if c.CompleteGroupID == "" {
		for _, id := range c.DoneStatusIDs {
			if !r.options[id] {
				return StatusSchema{}, fmt.Errorf("selected Notion completion option is missing")
			}
			r.completed[id] = true
		}
		return r, nil
	}
	groups := map[string]bool{}
	membership := map[string]string{}
	for _, group := range property.Groups {
		if strings.TrimSpace(group.ID) == "" || groups[group.ID] {
			return StatusSchema{}, fmt.Errorf("notion workflow group IDs must be unique and nonempty")
		}
		groups[group.ID] = true
		for _, member := range group.OptionIDs {
			if !r.options[member] {
				return StatusSchema{}, fmt.Errorf("notion workflow group references an unknown option")
			}
			if _, present := membership[member]; present {
				return StatusSchema{}, fmt.Errorf("notion status option belongs to more than one workflow membership")
			}
			membership[member] = group.ID
		}
		if group.ID == c.CompleteGroupID {
			r.CompleteGroup = Group{ID: group.ID, Name: group.Name, OptionIDs: slices.Clone(group.OptionIDs)}
		}
		if group.ID == c.TodoGroupID {
			r.TodoGroup = Group{ID: group.ID, Name: group.Name, OptionIDs: slices.Clone(group.OptionIDs)}
		}
	}
	if r.CompleteGroup.ID == "" || (c.TodoGroupID != "" && r.TodoGroup.ID == "") {
		return StatusSchema{}, fmt.Errorf("selected Notion workflow group is missing")
	}
	if c.StatusSync == "two-way" && (len(r.CompleteGroup.OptionIDs) == 0 || len(r.TodoGroup.OptionIDs) == 0) {
		return StatusSchema{}, fmt.Errorf("notion two-way status requires nonempty To-do and Complete groups")
	}
	for _, id := range r.CompleteGroup.OptionIDs {
		r.completed[id] = true
	}
	for _, target := range []struct {
		override    string
		group       Group
		destination *string
	}{{c.ClosedStatusID, r.CompleteGroup, &r.closedTarget}, {c.OpenStatusID, r.TodoGroup, &r.openTarget}} {
		if target.override != "" {
			if !slices.Contains(target.group.OptionIDs, target.override) {
				return StatusSchema{}, fmt.Errorf("notion status write target is missing or outside its selected group")
			}
			*target.destination = target.override
		} else if len(target.group.OptionIDs) > 0 {
			*target.destination = target.group.OptionIDs[0]
		}
	}
	type entry struct{ OptionID, GroupID string }
	entries := make([]entry, 0, len(r.options))
	for option := range r.options {
		entries = append(entries, entry{option, membership[option]})
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.OptionID, b.OptionID) })
	raw, err := json.Marshal(struct {
		CompleteGroupID string
		Membership      []entry
	}{c.CompleteGroupID, entries})
	if err != nil {
		return StatusSchema{}, fmt.Errorf("cannot encode Notion workflow membership")
	}
	r.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(raw))
	return r, nil
}

// Classify preserves every non-completed substate, including a null value.
func (s StatusSchema) Classify(optionID *string) (string, error) {
	if optionID == nil {
		return "open", nil
	}
	if !s.options[*optionID] {
		return "", fmt.Errorf("notion status option is absent from the live schema")
	}
	if s.completed[*optionID] {
		return "closed", nil
	}
	return "open", nil
}

// Target resolves a binary transition; callers first preserve matching states.
func (s StatusSchema) Target(state string) (string, error) {
	var target string
	switch state {
	case "closed":
		target = s.closedTarget
	case "open":
		target = s.openTarget
	default:
		return "", fmt.Errorf("notion status transition must be open or closed")
	}
	if target == "" {
		return "", fmt.Errorf("notion selected workflow group has no write target")
	}
	return target, nil
}

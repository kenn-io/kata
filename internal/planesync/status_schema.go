package planesync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/issuesync"
)

type statusSchema struct {
	states      []State
	groups      map[string]string
	fingerprint string
}

func blockedStatus(message string) error {
	return &issuesync.StatusError{Message: message, Blocked: true}
}
func statusReadError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, ok := errors.AsType[*issuesync.StatusError](err); ok {
		return err
	}
	return blockedStatus(err.Error())
}

func resolveStatusSchema(states []State) (statusSchema, error) {
	groups, err := stateGroups(states)
	if err != nil {
		return statusSchema{}, statusReadError(err)
	}
	ordered := append([]State(nil), states...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Sequence == ordered[j].Sequence {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Sequence < ordered[j].Sequence
	})

	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var membership strings.Builder
	for _, id := range ids {
		membership.WriteString(id + ":" + groups[id] + "\n")
	}
	return statusSchema{states: ordered, groups: groups, fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(membership.String())))}, nil
}
func (schema statusSchema) target(c Config, desired string) (string, error) {
	group, override := "completed", c.ClosedStateID
	if desired == "open" {
		group, override = "unstarted", c.OpenStateID
	}
	if override != "" {
		if schema.groups[override] != group {
			return "", blockedStatus("Plane status target is missing or outside the selected workflow group")
		}
		return override, nil
	}
	for _, state := range schema.states {
		if state.Group == group {
			return state.ID, nil
		}
	}
	return "", blockedStatus("Plane status sync requires a state in the " + group + " group")
}

// ValidateStatusTargets validates UUID syntax in all modes and live target
// membership for two-way sync without writing a work item or persisting workflow metadata.
func ValidateStatusTargets(c Config, states []State) error {
	c, err := normalizeConfig(c)
	if err != nil {
		return err
	}
	if c.StatusSync != "two-way" {
		return nil
	}
	schema, err := resolveStatusSchema(states)
	if err != nil {
		return err
	}
	for _, desired := range []string{"closed", "open"} {
		if _, err := schema.target(c, desired); err != nil {
			return err
		}
	}
	return nil
}

package linearsync

import (
	"context"
	"errors"
	"sort"

	"go.kenn.io/kata/internal/issuesync"
)

type statusSchema struct {
	states []State
	types  map[string]string
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
	types, err := stateTypes(states)
	if err != nil {
		return statusSchema{}, statusReadError(err)
	}
	ordered := append([]State(nil), states...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Position == ordered[j].Position {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Position < ordered[j].Position
	})
	return statusSchema{ordered, types}, nil
}
func (s statusSchema) target(c Config, desired string) (string, error) {
	typ, override := "completed", c.ClosedStateID
	if desired == "open" {
		typ, override = "unstarted", c.OpenStateID
	}
	if override != "" {
		if s.types[override] != typ {
			return "", blockedStatus("Linear status target is missing or outside selected workflow type")
		}
		return override, nil
	}
	for _, state := range s.states {
		if state.Type == typ {
			return state.ID, nil
		}
	}
	return "", blockedStatus("Linear status sync requires a " + typ + " state")
}

// ValidateStatusTargets checks live completed and unstarted targets in two-way mode.
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
	for _, desired := range []string{"open", "closed"} {
		if _, err := schema.target(c, desired); err != nil {
			return err
		}
	}
	return nil
}

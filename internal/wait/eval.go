// Package wait evaluates issue wait conditions shared by clients.
package wait

import (
	"bytes"
	"encoding/json/jsontext"
	"time"

	"go.kenn.io/kata/internal/metadata"
)

// MaxConsecutiveFailures bounds transient retries for each issue.
const MaxConsecutiveFailures = 3

// DefaultPollInterval is the CLI's default cadence between retries.
const DefaultPollInterval = 2 * time.Second

// Evaluator retains the initial reply slot for one issue and recipient.
type Evaluator struct {
	baseline jsontext.Value
	set      bool
}

// Eval reports whether an issue satisfies a wait condition.
func (e *Evaluator) Eval(mode, status, attention string, reply jsontext.Value) (bool, string) {
	if status == "closed" {
		return true, "closed"
	}
	switch mode {
	case "attention":
		if attention != "" && attention != "ok" {
			return true, "attention"
		}
	case "needs-human", "stuck":
		if attention == mode {
			return true, "attention"
		}
	case "reply":
		slot := metadata.NormalizeJSON(reply)
		if len(slot) == 0 || string(slot) == "null" {
			slot = nil
		}
		if !e.set {
			e.baseline = append(jsontext.Value(nil), slot...)
			e.set = true
			return false, ""
		}
		if !bytes.Equal(e.baseline, slot) {
			return true, "reply"
		}
	}
	return false, ""
}

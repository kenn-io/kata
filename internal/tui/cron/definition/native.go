package definition

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	native "go.kenn.io/kata/internal/cron"
)

// ValidateWorkflow applies the native workflow validation contract.
func ValidateWorkflow(raw []byte) error { _, err := native.ParseWorkflow(raw); return err }

// RunParent reads the opaque run_parent option for display.
func RunParent(d JobDef) string { return OptionString(d.Options, "run_parent") }

// OptionString reads an optional string without rewriting other options.
func OptionString(raw jsontext.Value, key string) string {
	var m map[string]jsontext.Value
	_ = json.Unmarshal(raw, &m)
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}

// MaxSeconds is the largest whole-second Go duration.
const MaxSeconds = int64(9223372036854775807 / 1000000000)

// PortableKey checks the native portable key syntax.
func PortableKey(key string) bool { return portableKey(key) }

// CronExpression uses the native cron parser.
func CronExpression(t Trigger) (any, error) { return native.ParseCron(t.Cron) }

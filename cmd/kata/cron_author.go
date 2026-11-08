package main

import (
	"encoding/json/v2"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
)

// cronAuthorOptions builds only the common per-run job forms. Other fields
// remain available through --file, without an overlay or a second validator.
type cronAuthorOptions struct {
	name, title, body                   string
	expression, every, once, timezone   string
	catchup, overlap, grace, timeout    string
	prompt, adapter, agent, checkoutKey string
	recipient, message                  string
	enabled, notify                     bool
}

var cronAuthorFlags = []string{
	"name", "title", "body", "enabled", "cron", "every", "once", "timezone",
	"catchup", "overlap", "grace", "timeout", "notify", "recipient", "message",
	"prompt", "adapter", "adapter-agent", "checkout-key",
}

func (o *cronAuthorOptions) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&o.name, "name", "", "job name (required for typed creation)")
	f.StringVar(&o.title, "title", "", "per-run issue title (required for typed creation)")
	f.StringVar(&o.body, "body", "", "literal per-run issue body")
	f.BoolVar(&o.enabled, "enabled", false, "explicit enabled state; use --enabled=false to store a disabled job")
	f.StringVar(&o.expression, "cron", "", "five-field cron expression or descriptor; requires --timezone")
	f.StringVar(&o.every, "every", "", "positive whole-second interval, such as 1h or 30m")
	f.StringVar(&o.once, "once", "", "RFC3339 instant for an adapter job")
	f.StringVar(&o.timezone, "timezone", "", "IANA timezone (required for --cron; Local is refused)")
	f.StringVar(&o.catchup, "catchup", "", "explicit catch-up policy: skip, latest, or all")
	f.StringVar(&o.overlap, "overlap", "", "explicit overlap policy: allow or forbid (native requires allow)")
	f.StringVar(&o.grace, "grace", "", "nonnegative whole-second catch-up grace, such as 5m")
	f.StringVar(&o.timeout, "timeout", "", "nonnegative whole-second adapter timeout")
	f.BoolVar(&o.notify, "notify", false, "create native recurring child issues; requires --parent and --recipient")
	f.StringVar(&o.recipient, "recipient", "", "exact native inbox recipient or current-owner-or-author")
	f.StringVar(&o.message, "message", "", "literal native inbox message")
	f.StringVar(&o.prompt, "prompt", "", "literal adapter prompt; requires --adapter")
	f.StringVar(&o.adapter, "adapter", "", "adapter name for a prompt job")
	f.StringVar(&o.agent, "adapter-agent", "", "optional agent name for the adapter")
	f.StringVar(&o.checkoutKey, "checkout-key", "", "optional portable checkout key for the adapter")
}

func (o *cronAuthorOptions) used(cmd *cobra.Command) bool {
	if o == nil {
		return false
	}
	for _, name := range cronAuthorFlags {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

// cronAuthorSeconds rejects truncation and time.Duration overflow before any
// network request. Duration syntax is shared by interval, grace, and timeout.
func cronAuthorSeconds(value, flag string, allowZero bool) (int64, error) {
	_, err := time.ParseDuration(value)
	if err != nil {
		return 0, cronCLIValidation(fmt.Sprintf("--%s requires a supported duration: %v", flag, err))
	}
	// ParseDuration validates syntax and bounds, but discards fractions of a
	// nanosecond. Sum the original decimals exactly before converting seconds.
	negative := strings.HasPrefix(value, "-")
	remaining := strings.TrimPrefix(strings.TrimPrefix(value, "-"), "+")
	total := new(big.Rat)
	units := map[string]int64{"ns": 1, "us": 1000, "µs": 1000, "μs": 1000, "ms": 1000000, "s": 1000000000, "m": 60000000000, "h": 3600000000000}
	for remaining != "" && remaining != "0" {
		i := 0
		for i < len(remaining) && (remaining[i] >= '0' && remaining[i] <= '9' || remaining[i] == '.') {
			i++
		}
		part, ok := new(big.Rat).SetString(remaining[:i])
		if !ok {
			return 0, cronCLIValidation("--" + flag + " requires a supported duration")
		}
		remaining = remaining[i:]
		i = 0
		for i < len(remaining) && (remaining[i] < '0' || remaining[i] > '9') && remaining[i] != '.' {
			i++
		}
		part.Mul(part, new(big.Rat).SetFrac64(units[remaining[:i]], int64(time.Second)))
		total.Add(total, part)
		remaining = remaining[i:]
	}
	if negative {
		total.Neg(total)
	}
	if total.Sign() < 0 || !allowZero && total.Sign() == 0 {
		rule := "positive"
		if allowZero {
			rule = "nonnegative"
		}
		return 0, cronCLIValidation("--" + flag + " must be " + rule)
	}
	if !total.IsInt() {
		return 0, cronCLIValidation("--" + flag + " must use whole seconds")
	}
	return total.Num().Int64(), nil
}

func (o *cronAuthorOptions) build(cmd *cobra.Command, parent string) (api.PutCronJobBody, error) {
	var out api.PutCronJobBody
	for _, flag := range []string{"name", "title", "enabled", "catchup", "overlap"} {
		if !cmd.Flags().Changed(flag) {
			return out, cronCLIValidation("typed creation requires explicit --" + flag)
		}
	}
	out.Name = strings.TrimSpace(o.name)
	if out.Name == "" || len(out.Name) > 256 {
		return out, cronCLIValidation("name must contain 1–256 UTF-8 bytes after trimming")
	}
	if strings.TrimSpace(o.title) == "" {
		return out, cronCLIValidation("--title must not be empty")
	}
	if cmd.Flags().Changed("notify") && !o.notify {
		return out, cronCLIValidation("--notify must be true when supplied")
	}
	if o.notify == cmd.Flags().Changed("prompt") {
		return out, cronCLIValidation("choose exactly one of --notify or --prompt")
	}
	trigger, err := o.trigger(cmd)
	if err != nil {
		return out, err
	}
	out.Definition = api.CronJobDefinition{
		Version: 1, Kind: "job", Enabled: o.enabled, Trigger: trigger,
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: o.title, Body: o.body},
		Overlap: o.overlap, Catchup: o.catchup,
	}
	for _, setting := range []struct {
		flag, value string
		target      *int64
	}{{"grace", o.grace, &out.Definition.GraceSeconds}, {"timeout", o.timeout, &out.Definition.TimeoutSeconds}} {
		if cmd.Flags().Changed(setting.flag) {
			*setting.target, err = cronAuthorSeconds(setting.value, setting.flag, true)
			if err != nil {
				return out, err
			}
		}
	}
	if o.notify {
		if strings.TrimSpace(parent) == "" || strings.TrimSpace(o.recipient) == "" {
			return out, cronCLIValidation("--notify requires --parent and --recipient")
		}
		for _, flag := range []string{"adapter", "adapter-agent", "checkout-key"} {
			if cmd.Flags().Changed(flag) {
				return out, cronCLIValidation("--" + flag + " requires --prompt")
			}
		}
		out.Definition.Action = cron.Action{Kind: "notify", Recipient: o.recipient, Message: o.message}
		if err := out.Definition.Native().ValidateBeforeParentResolution(); err != nil {
			return out, cronCLIValidation(err.Error())
		}
		// The existing --parent resolver fills parent_uid before full validation.
		return out, nil
	}
	if parent != "" || cmd.Flags().Changed("recipient") || cmd.Flags().Changed("message") {
		return out, cronCLIValidation("--parent, --recipient and --message require --notify")
	}
	if strings.TrimSpace(o.prompt) == "" || strings.TrimSpace(o.adapter) == "" {
		return out, cronCLIValidation("--prompt requires a nonempty prompt and explicit --adapter")
	}
	out.Definition.Action = cron.Action{Kind: "execute", Prompt: o.prompt}
	out.Definition.CheckoutKey = o.checkoutKey
	options := map[string]string{"adapter": o.adapter}
	if cmd.Flags().Changed("adapter-agent") {
		if strings.TrimSpace(o.agent) == "" {
			return out, cronCLIValidation("--adapter-agent must not be empty")
		}
		options["agent"] = o.agent
	}
	out.Definition.Options, err = json.Marshal(options)
	if err == nil {
		err = out.Definition.Native().Validate()
	}
	if err != nil {
		return out, cronCLIValidation(err.Error())
	}
	return out, nil
}

func (o *cronAuthorOptions) trigger(cmd *cobra.Command) (cron.Trigger, error) {
	var trigger cron.Trigger
	count := 0
	for _, flag := range []string{"cron", "every", "once"} {
		if cmd.Flags().Changed(flag) {
			count++
		}
	}
	if count != 1 {
		return trigger, cronCLIValidation("choose exactly one of --cron, --every or --once")
	}
	if cmd.Flags().Changed("timezone") || cmd.Flags().Changed("cron") {
		if o.timezone == "" || o.timezone == "Local" {
			return trigger, cronCLIValidation("--timezone requires an explicit IANA timezone; Local is not portable")
		}
	}
	trigger.Timezone = o.timezone
	switch {
	case cmd.Flags().Changed("cron"):
		expression := strings.TrimSpace(o.expression)
		if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
			return trigger, cronCLIValidation("--cron uses --timezone; timezone directives remain available through --file")
		}
		if strings.HasPrefix(expression, "@every") {
			return trigger, cronCLIValidation("use --every for an interval instead of --cron @every")
		}
		trigger.Kind, trigger.Cron = "cron", o.expression
	case cmd.Flags().Changed("every"):
		trigger.Kind = "interval"
		seconds, err := cronAuthorSeconds(o.every, "every", false)
		if err != nil {
			return trigger, err
		}
		trigger.IntervalSeconds = seconds
	case cmd.Flags().Changed("once"):
		trigger.Kind, trigger.At = "once", o.once
	}
	if err := trigger.Validate(); err != nil {
		return trigger, cronCLIValidation(err.Error())
	}
	return trigger, nil
}

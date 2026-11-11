package main

import (
	"encoding/json/v2"
	"fmt"
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

// cronAuthorSeconds converts durations to whole seconds.
func cronAuthorSeconds(value, flag string) (int64, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, cronCLIValidation(fmt.Sprintf("--%s requires a supported duration: %v", flag, err))
	}
	if d%time.Second != 0 {
		return 0, cronCLIValidation("--" + flag + " must use whole seconds")
	}
	return int64(d / time.Second), nil
}

func (o *cronAuthorOptions) build(cmd *cobra.Command, parent string) (api.PutCronJobBody, error) {
	var out api.PutCronJobBody
	for _, flag := range []string{"name", "title", "enabled", "catchup", "overlap"} {
		if !cmd.Flags().Changed(flag) {
			return out, cronCLIValidation("typed creation requires explicit --" + flag)
		}
	}
	out.Name = o.name
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
			*setting.target, err = cronAuthorSeconds(setting.value, setting.flag)
			if err != nil {
				return out, err
			}
		}
	}
	if o.notify {
		for _, flag := range []string{"adapter", "adapter-agent", "checkout-key"} {
			if cmd.Flags().Changed(flag) {
				return out, cronCLIValidation("--" + flag + " requires --prompt")
			}
		}
		out.Definition.Action = cron.Action{Kind: "notify", Recipient: o.recipient, Message: o.message}
		// The existing --parent resolver fills parent_uid before full validation.
		return out, nil
	}
	if parent != "" || cmd.Flags().Changed("recipient") || cmd.Flags().Changed("message") {
		return out, cronCLIValidation("--parent, --recipient and --message require --notify")
	}
	if o.adapter == "" || strings.TrimSpace(o.adapter) != o.adapter {
		return out, cronCLIValidation("--adapter must be a non-empty selector without surrounding spaces")
	}
	out.Definition.Action = cron.Action{Kind: "execute", Prompt: o.prompt}
	out.Definition.CheckoutKey = o.checkoutKey
	options := map[string]string{"adapter": o.adapter}
	if cmd.Flags().Changed("adapter-agent") {
		if o.agent == "" || strings.TrimSpace(o.agent) != o.agent {
			return out, cronCLIValidation("--adapter-agent must be a non-empty selector without surrounding spaces")
		}
		options["agent"] = o.agent
	}
	out.Definition.Options, _ = json.Marshal(options)
	return out, nil
}

func (o *cronAuthorOptions) trigger(cmd *cobra.Command) (cron.Trigger, error) {
	trigger := cron.Trigger{Timezone: o.timezone}
	count := 0
	for _, flag := range []string{"cron", "every", "once"} {
		if cmd.Flags().Changed(flag) {
			count++
		}
	}
	if count != 1 {
		return trigger, cronCLIValidation("choose exactly one of --cron, --every or --once")
	}
	if (cmd.Flags().Changed("timezone") || cmd.Flags().Changed("cron")) && (o.timezone == "" || o.timezone == "Local") {
		return trigger, cronCLIValidation("--timezone requires an explicit IANA timezone; Local is not portable")
	}
	switch {
	case cmd.Flags().Changed("cron"):
		expression := strings.TrimSpace(o.expression)
		if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
			return trigger, cronCLIValidation("--cron uses --timezone; timezone directives remain available through --file")
		}
		trigger.Kind, trigger.Cron = "cron", o.expression
	case cmd.Flags().Changed("every"):
		trigger.Kind = "interval"
		seconds, err := cronAuthorSeconds(o.every, "every")
		if err != nil {
			return trigger, err
		}
		trigger.IntervalSeconds = seconds
	case cmd.Flags().Changed("once"):
		trigger.Kind, trigger.At = "once", o.once
	}
	return trigger, nil
}

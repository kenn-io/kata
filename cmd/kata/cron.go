package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// Native cron commands are finite API clients. Scheduling, process
// execution and durable executor journals belong to the external adapter.
func newCronCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "cron",
		Short: "manage shared jobs, workflows and run evidence",
		Long: `Store a project's shared job and workflow definitions and record run evidence.
Kata does not schedule or run anything: an external adapter reads the
definitions, decides when to run, and reports each run with kata cron run
observe. A recorded run status never controls whether anything runs.
Results are JSON.
A job is configured work: its trigger, action, and execution settings.
A workflow is reusable command or prompt steps with dependencies.
A run is one execution and its reported status and results.
The request-file formats are in the Native cron reference (docs/reference/cron.md).`,
		Example: `  kata cron capabilities --json
  kata cron job create --file job.json --json
  kata cron job list --json
  kata cron run observe <run-uid> --json-input observation.json --json`,
	}
	for _, resource := range []string{"job", "workflow"} {
		group := &cobra.Command{Use: resource, Short: "manage native " + resource + " definitions"}
		for _, action := range []string{"list", "show", "create", "update", "delete", "restore"} {
			group.AddCommand(newCronOperationCmd(resource, action))
		}
		command.AddCommand(group)
	}
	runs := &cobra.Command{Use: "run", Short: "list, show and record independent run observations"}
	for _, action := range []string{"list", "show", "observe"} {
		runs.AddCommand(newCronOperationCmd("run", action))
	}
	capabilities := newCronOperationCmd("capabilities", "show")
	capabilities.Use = "capabilities"
	capabilities.Short = "discover local protocol support without creating cron state"
	command.AddCommand(runs, capabilities)
	return command
}

type cronCLIOptions struct {
	resource, action                 string
	file, identity, expectedEventUID string
	includeDeleted                   bool
	limit                            int64
	beforeUID, jobUID                string
}

func (o cronCLIOptions) mutation() bool   { return o.action != "list" && o.action != "show" }
func (o cronCLIOptions) definition() bool { return o.resource == "job" || o.resource == "workflow" }
func newCronOperationCmd(resource, action string) *cobra.Command {
	options := cronCLIOptions{resource: resource, action: action, limit: 100}
	takesUID := (resource == "job" || resource == "workflow" || resource == "run") && action != "list" && action != "create"
	command := &cobra.Command{Use: action, Short: action + " native " + resource, Args: cobra.NoArgs}
	command.Long, command.Example = cronOperationHelp(resource, action)
	if takesUID {
		command.Use += " <uid>"
		command.Args = cobra.ExactArgs(1)
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if takesUID {
			options.identity = args[0]
		}
		return options.execute(cmd)
	}
	if options.mutation() {
		if resource == "run" {
			command.Flags().StringVar(&options.file, "json-input", "", "observation JSON file, or - for stdin")
		} else {
			command.Flags().StringVar(&options.file, "file", "", "JSON request body file, or - for stdin")
		}
		if options.definition() {
			if action == "create" {
				command.Flags().StringVar(&options.identity, "uid", "", "retained definition ULID (generated once and printed if absent)")
			} else {
				command.Flags().StringVar(&options.expectedEventUID, "expected-event-uid", "", "current definition event UID; must match the request body if present")
			}
		}
	}
	if action == "list" && options.definition() {
		command.Flags().BoolVar(&options.includeDeleted, "include-deleted", false, "include tombstoned definitions")
	}
	if resource == "run" && action == "list" {
		command.Flags().Int64Var(&options.limit, "limit", 100, "maximum results in one page (1-100)")
		command.Flags().StringVar(&options.beforeUID, "before-uid", "", "run UID cursor from next_before_uid")
		command.Flags().StringVar(&options.jobUID, "job-uid", "", "list only runs of this job")
	}
	return command
}

// cronOperationHelp returns Long and Example text for one generated command.
func cronOperationHelp(resource, action string) (string, string) {
	cmd := "kata cron " + resource + " " + action
	switch resource + " " + action {
	case "capabilities show":
		return `Show whether the daemon supports native cron for this project (the cron_v1
event feature). Reads only; creates no cron state.`, "  kata cron capabilities --json"
	case "run list":
		return `List the project's recorded runs, newest first. --job-uid lists one job's runs.
For the next page, pass the response's next_before_uid as --before-uid.`,
			"  " + cmd + " --limit 20 --json\n  " + cmd + " --job-uid <job-uid> --before-uid <run-uid> --json"
	case "run show":
		return "Show one recorded run.", "  " + cmd + " <run-uid> --json"
	case "run observe":
		return `Record or update the observation for one run from a JSON file (--json-input,
or - for stdin). The adapter creates the run UID once and reuses it across
retries; an identical retry returns replayed: true and changes nothing. A new
run uses expected_revision 0; a changed one needs its current revision.
An omitted JSON teammate inherits --teammate or KATA_TEAMMATE. Recording a run
never changes an issue's owner, dates, lease, or status.`,
			"  " + cmd + " <run-uid> --json-input observation.json --json"
	}
	switch action {
	case "list":
		return "List the project's " + resource + ` definitions. --include-deleted also lists
deleted (tombstoned) ones.`, "  " + cmd + " --json\n  " + cmd + " --include-deleted --json"
	case "show":
		return "Show one " + resource + ` definition. Its definition_event_uid is the value
update, delete, and restore require as --expected-event-uid.`, "  " + cmd + " <" + resource + "-uid> --json"
	case "create":
		long := "Create a " + resource + ` definition from a JSON request file (--file, or -
for stdin) holding a name and a definition. Pass --uid to choose its ULID;
otherwise one is generated and printed to stderr. Reuse that --uid when
retrying: a repeated create never becomes an update.`
		if resource == "job" {
			long += "\nCreating or enabling a job starts no timer or process."
		}
		return long, "  " + cmd + " --uid <" + resource + "-uid> --file " + resource + ".json --json"
	case "update":
		return "Replace a " + resource + ` definition from a JSON request file. Pass the current
definition_event_uid from kata cron ` + resource + ` show as --expected-event-uid
(or as expected_event_uid in the file); a stale value fails with a conflict.`,
			"  " + cmd + " <" + resource + "-uid> --expected-event-uid <event-uid> --file " + resource + ".json --json"
	case "delete":
		long := "Delete a " + resource + ` definition. Requires the current --expected-event-uid.
The definition stays readable and kata cron ` + resource + ` restore brings it back.`
		if resource == "workflow" {
			long += "\nRefused while a live job in the project uses the workflow."
		}
		return long, "  " + cmd + " <" + resource + "-uid> --expected-event-uid <event-uid> --json"
	case "restore":
		return "Restore a deleted " + resource + ` definition. Requires the current
--expected-event-uid.`, "  " + cmd + " <" + resource + "-uid> --expected-event-uid <event-uid> --json"
	}
	return "", ""
}

func cronCLIValidation(message string) error {
	return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
}
func normalizedCronCLIUID(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !uid.Valid(value) {
		return "", cronCLIValidation("cron identity must be a 26-character ULID")
	}
	return value, nil
}
func (o *cronCLIOptions) execute(cmd *cobra.Command) error {
	for _, value := range []*string{&o.identity, &o.beforeUID, &o.jobUID} {
		if *value != "" {
			normalized, err := normalizedCronCLIUID(*value)
			if err != nil {
				return err
			}
			*value = normalized
		}
	}
	if o.limit < 1 || o.limit > 100 {
		return cronCLIValidation("limit must be between 1 and 100")
	}
	var body []byte
	if o.mutation() {
		var err error
		body, err = o.requestBody(cmd)
		if err != nil {
			return err
		}
		if o.definition() && o.action == "create" {
			if o.identity == "" {
				o.identity, err = uid.New()
				if err != nil {
					return err
				}
			}
			if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "Definition UID: %s (reuse --uid on retry)\n", o.identity); err != nil {
				return err
			}
		}
	}
	daemon, err := dialDaemon(cmd.Context())
	if err != nil {
		return err
	}
	var project projectRef
	if strings.TrimSpace(flags.Project) != "" {
		project, err = resolveProjectSelectorIncludingArchived(daemon, flags.Project)
	} else {
		project, err = resolveProjectArgFlagOrWorkspace(daemon, nil)
	}
	if err != nil {
		return err
	}
	client, err := kataclient.NewWithHTTPClient(daemon.baseURL, daemon.client)
	if err != nil {
		return err
	}
	raw, err := o.call(daemon, client, project.ID, body)
	if err != nil {
		return err
	}
	if flags.Quiet && currentOutputMode() != outputJSON {
		return nil
	}
	return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
}

// cronDefinitionRequestLimit bounds a job or workflow request read by the CLI.
const cronDefinitionRequestLimit = 1 << 20

func (o *cronCLIOptions) requestBody(cmd *cobra.Command) ([]byte, error) {
	raw, err := o.readRequest(cmd)
	if err != nil {
		return nil, err
	}
	fields, err := o.decodeRequest(raw)
	if err != nil {
		return nil, err
	}
	if err := applyCronActor(cmd, fields); err != nil {
		return nil, err
	}
	if o.resource == "run" && o.action == "observe" {
		if err := applyCronTeammate(cmd, fields); err != nil {
			return nil, err
		}
	}
	if err := o.applyExpectedEventUID(fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// readRequest returns the bounded request file. Definition delete and restore
// may omit it and send an empty object; every other mutation requires it.
func (o *cronCLIOptions) readRequest(cmd *cobra.Command) ([]byte, error) {
	if o.file == "" {
		if o.definition() && o.action != "create" && o.action != "update" {
			return []byte(`{}`), nil
		}
		if o.resource == "run" {
			return nil, cronCLIValidation("--json-input is required; use --json-input - for stdin")
		}
		return nil, cronCLIValidation("--file is required; use --file - for stdin")
	}
	reader := cmd.InOrStdin()
	if o.file != "-" {
		file, err := os.Open(o.file)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	limit := int64(cronDefinitionRequestLimit)
	if o.resource == "run" {
		limit = db.CronObservationLimit
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, cronCLIValidation("cron request exceeds its byte limit")
	}
	return raw, nil
}

// decodeRequest validates the public shape before defaults are added. Raw
// option and input objects stay intact, so unknown permission booleans or
// misspelled fields are refused rather than silently dropped.
func (o *cronCLIOptions) decodeRequest(raw []byte) (map[string]jsontext.Value, error) {
	fields := map[string]jsontext.Value{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, cronCLIValidation("invalid cron JSON: " + err.Error())
	}
	if fields == nil {
		return nil, cronCLIValidation("cron request must be a JSON object")
	}
	shape, err := o.requestShape()
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, shape, json.RejectUnknownMembers(true)); err != nil {
		return nil, cronCLIValidation("invalid cron request: " + err.Error())
	}
	return fields, nil
}

func (o *cronCLIOptions) requestShape() (any, error) {
	replace := o.action == "create" || o.action == "update"
	switch {
	case o.resource == "job" && replace:
		return &api.PutCronJobBody{}, nil
	case o.resource == "workflow" && replace:
		return &api.PutCronWorkflowBody{}, nil
	case o.resource == "run" && o.action == "observe":
		return &api.ObserveCronRunBody{}, nil
	case o.definition():
		return &api.CronDefinitionActionBody{}, nil
	default:
		return nil, cronCLIValidation("unsupported cron operation")
	}
}

func cronStringField(fields map[string]jsontext.Value, key string) (string, error) {
	var value string
	if raw, exists := fields[key]; exists {
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", cronCLIValidation(key + " must be a string")
		}
	}
	return value, nil
}

func setCronStringField(fields map[string]jsontext.Value, key, value string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode cron request %s: %w", key, err)
	}
	fields[key] = encoded
	return nil
}

// applyCronActor fills an absent body actor from the resolved CLI actor and
// refuses a body actor that contradicts --as.
func applyCronActor(cmd *cobra.Command, fields map[string]jsontext.Value) error {
	actor, err := cronStringField(fields, "actor")
	if err != nil {
		return err
	}
	if actor == "" {
		actor, _ = resolveActor(cmd.Context(), flags.As, nil)
		return setCronStringField(fields, "actor", actor)
	}
	if flags.As != "" && actor != flags.As {
		return cronCLIValidation("body actor and --as must match")
	}
	return nil
}

// applyCronTeammate keeps observation attribution stable across retries.
// Explicit JSON, including null or an invalid empty identity, never inherits
// the environment; --teammate may only confirm it.
func applyCronTeammate(cmd *cobra.Command, fields map[string]jsontext.Value) error {
	raw, frozen := fields["teammate"]
	if !frozen {
		selected, err := resolveTeammate(cmd)
		if err != nil {
			return err
		}
		if selected == "" {
			return nil
		}
		return setCronStringField(fields, "teammate", selected)
	}
	if !cmd.Flags().Changed("teammate") {
		return nil
	}
	selected, err := resolveTeammate(cmd)
	if err != nil {
		return err
	}
	var body *string
	if err := json.Unmarshal(raw, &body); err != nil {
		return cronCLIValidation("teammate must be a string or null")
	}
	value := ""
	if body != nil {
		value = *body
	}
	if selected != value {
		return cronCLIValidation("body teammate and --teammate must match")
	}
	return nil
}

// applyExpectedEventUID reconciles the body precondition with
// --expected-event-uid and writes the normalized value back to the body.
func (o *cronCLIOptions) applyExpectedEventUID(fields map[string]jsontext.Value) error {
	const key = "expected_event_uid"
	bodyUID, err := cronStringField(fields, key)
	if err != nil {
		return err
	}
	if bodyUID != "" {
		if bodyUID, err = normalizedCronCLIUID(bodyUID); err != nil {
			return err
		}
	}
	if o.expectedEventUID == "" {
		o.expectedEventUID = bodyUID
	} else {
		if o.expectedEventUID, err = normalizedCronCLIUID(o.expectedEventUID); err != nil {
			return err
		}
		if bodyUID != "" && bodyUID != o.expectedEventUID {
			return cronCLIValidation(key + " in body and flag must match")
		}
	}
	if err := o.checkExpectedEventUID(); err != nil {
		return err
	}
	if o.expectedEventUID == "" {
		return nil
	}
	return setCronStringField(fields, key, o.expectedEventUID)
}

// checkExpectedEventUID requires a definition create to be unconditional and
// every other definition mutation to name the event it replaces.
func (o *cronCLIOptions) checkExpectedEventUID() error {
	if !o.definition() {
		return nil
	}
	if o.action == "create" && o.expectedEventUID != "" {
		return cronCLIValidation("create requires absent expected_event_uid; use update for replacement")
	}
	if o.action != "create" && o.expectedEventUID == "" {
		return cronCLIValidation("expected_event_uid is required for definition mutations")
	}
	return nil
}

// Validate the native public shape first; generated option maps preserve their
// opaque JSON values through JSONv2 and the SDK's precise-number decoders.
func decodeCronCLIRequest(raw []byte, value any) error {
	return json.Unmarshal(raw, value, json.RejectUnknownMembers(true))
}
func cronCLIResponse(status int, raw []byte, err error) ([]byte, error) {
	if responseErr := externalCLIResponseError(status, raw, err); responseErr != nil {
		return nil, responseErr
	}
	return raw, nil
}

func (o cronCLIOptions) call(a daemonAPI, c *kataclient.Client, projectID int64, raw []byte) ([]byte, error) {
	switch {
	case o.resource == "capabilities":
		return o.callCapabilities(a, c, projectID)
	case o.action == "list" || o.action == "show":
		return o.callRead(a, c, projectID)
	case o.action == "delete" || o.action == "restore":
		return o.callLifecycle(a, c, projectID, raw)
	default:
		return o.callWrite(a, c, projectID, raw)
	}
}

func (o cronCLIOptions) callRead(a daemonAPI, c *kataclient.Client, projectID int64) ([]byte, error) {
	switch {
	case o.resource == "job" && o.action == "list":
		response, err := c.ListCronJobsWithResponse(a.ctx, &generated.ListCronJobsRequestOptions{PathParams: &generated.ListCronJobsPath{ProjectID: projectID}, Query: &generated.ListCronJobsQuery{IncludeDeleted: &o.includeDeleted}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "job" && o.action == "show":
		response, err := c.ShowCronJobWithResponse(a.ctx, &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: o.identity}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "workflow" && o.action == "list":
		response, err := c.ListCronWorkflowsWithResponse(a.ctx, &generated.ListCronWorkflowsRequestOptions{PathParams: &generated.ListCronWorkflowsPath{ProjectID: projectID}, Query: &generated.ListCronWorkflowsQuery{IncludeDeleted: &o.includeDeleted}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "workflow" && o.action == "show":
		response, err := c.ShowCronWorkflowWithResponse(a.ctx, &generated.ShowCronWorkflowRequestOptions{PathParams: &generated.ShowCronWorkflowPath{ProjectID: projectID, CronUID: o.identity}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "run" && o.action == "list":
		response, err := c.ListCronRunsWithResponse(a.ctx, &generated.ListCronRunsRequestOptions{PathParams: &generated.ListCronRunsPath{ProjectID: projectID}, Query: &generated.ListCronRunsQuery{Limit: &o.limit, BeforeUID: &o.beforeUID, JobUID: &o.jobUID}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	case o.resource == "run" && o.action == "show":
		response, err := c.ShowCronRunWithResponse(a.ctx, &generated.ShowCronRunRequestOptions{PathParams: &generated.ShowCronRunPath{ProjectID: projectID, RunUID: o.identity}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	return nil, cronCLIValidation("unsupported cron operation")
}

func (o cronCLIOptions) callCapabilities(a daemonAPI, c *kataclient.Client, projectID int64) ([]byte, error) {
	response, err := c.GetCronCapabilitiesWithResponse(a.ctx, &generated.GetCronCapabilitiesRequestOptions{PathParams: &generated.GetCronCapabilitiesPath{ProjectID: projectID}})
	if response == nil {
		return nil, externalCLITransportError(response, err)
	}
	raw, err := cronCLIResponse(response.StatusCode, response.Body, err)
	if err != nil {
		return nil, err
	}
	header := ""
	if response.Headers200 != nil {
		header = response.Headers200.XKataEventFeatures
	}
	return cronCapabilitiesCLIResponse(raw, header)
}

func (o cronCLIOptions) callWrite(a daemonAPI, c *kataclient.Client, projectID int64, raw []byte) ([]byte, error) {
	if o.resource == "run" && o.action == "observe" {
		var body generated.ObserveCronRunBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		response, err := c.ObserveCronRunWithResponse(a.ctx, &generated.ObserveCronRunRequestOptions{PathParams: &generated.ObserveCronRunPath{ProjectID: projectID, RunUID: o.identity}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	if o.resource == "job" && (o.action == "create" || o.action == "update") {
		var body generated.PutCronJobBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		response, err := c.ReplaceCronJobWithResponse(a.ctx, &generated.ReplaceCronJobRequestOptions{PathParams: &generated.ReplaceCronJobPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	if o.resource == "workflow" && (o.action == "create" || o.action == "update") {
		var body generated.PutCronWorkflowBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		response, err := c.ReplaceCronWorkflowWithResponse(a.ctx, &generated.ReplaceCronWorkflowRequestOptions{PathParams: &generated.ReplaceCronWorkflowPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	return nil, cronCLIValidation("unsupported cron operation")
}

func (o cronCLIOptions) callLifecycle(a daemonAPI, c *kataclient.Client, projectID int64, raw []byte) ([]byte, error) {
	if o.definition() {
		var body generated.CronDefinitionActionBody
		if err := decodeCronCLIRequest(raw, &body); err != nil {
			return nil, err
		}
		switch {
		case o.resource == "job" && o.action == "delete":
			response, err := c.ArchiveCronJobWithResponse(a.ctx, &generated.ArchiveCronJobRequestOptions{PathParams: &generated.ArchiveCronJobPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		case o.resource == "job" && o.action == "restore":
			response, err := c.RestoreCronJobWithResponse(a.ctx, &generated.RestoreCronJobRequestOptions{PathParams: &generated.RestoreCronJobPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		case o.resource == "workflow" && o.action == "delete":
			response, err := c.ArchiveCronWorkflowWithResponse(a.ctx, &generated.ArchiveCronWorkflowRequestOptions{PathParams: &generated.ArchiveCronWorkflowPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		case o.resource == "workflow" && o.action == "restore":
			response, err := c.RestoreCronWorkflowWithResponse(a.ctx, &generated.RestoreCronWorkflowRequestOptions{PathParams: &generated.RestoreCronWorkflowPath{ProjectID: projectID, CronUID: o.identity}, Body: &body})
			if response == nil {
				return nil, externalCLITransportError(response, err)
			}
			return cronCLIResponse(response.StatusCode, response.Body, err)
		}
	}
	return nil, cronCLIValidation("unsupported cron operation")
}

func cronCapabilitiesCLIResponse(raw []byte, header string) ([]byte, error) {
	var body map[string]jsontext.Value
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, cronCLIValidation("cron discovery response must be a JSON object")
	}
	features := []string{}
	seen := map[string]bool{}
	for part := range strings.SplitSeq(header, ",") {
		feature := strings.TrimSpace(part)
		if feature != "" && !seen[feature] {
			features = append(features, feature)
			seen[feature] = true
		}
	}
	encoded, err := json.Marshal(features)
	if err != nil {
		return nil, err
	}
	body["event_features"] = encoded
	return json.Marshal(body)
}

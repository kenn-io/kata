package main

// These bodies are displayed unchanged for create and update. Integration tests
// extract them from rendered help and submit their accompanying CLI commands.
const cronRecurringRequestExample = `  # Recurring child issue: save as recurring.json; keep its parent open.
  {
    "name": "Daily review",
    "definition": {
      "version": 1,
      "kind": "job",
      "enabled": true,
      "trigger": {"kind": "cron", "cron": "0 9 * * *", "timezone": "UTC"},
      "action": {"kind": "notify", "recipient": "current-owner-or-author"},
      "issue": {"kind": "per-run", "title": "Review open work"},
      "overlap": "allow",
      "catchup": "latest"
    }
  }
`

const cronPromptRequestExample = `  # Adapter prompt job: save as prompt.json.
  {
    "name": "Daily summary",
    "definition": {
      "version": 1,
      "kind": "job",
      "enabled": true,
      "trigger": {"kind": "cron", "cron": "0 9 * * *", "timezone": "UTC"},
      "action": {"kind": "execute", "prompt": "Review open issues and summarize next actions."},
      "issue": {"kind": "per-run", "title": "Daily summary"},
      "overlap": "forbid",
      "catchup": "latest",
      "checkout_key": "example-project",
      "options": {"agent": "codex"}
    }
  }
`

const cronWorkflowRequestExample = `  # Command workflow with a dependency: save as workflow.json.
  {
    "name": "nightly-tests",
    "definition": {
      "version": 1,
      "steps": [
        {"key": "check", "kind": "command", "command": "git diff --check"},
        {"key": "test", "kind": "command", "command": "go test ./...", "after": ["check"]}
      ]
    }
  }
`

const cronCommandRequestExample = `  # Adapter command job: save as command.json after creating the workflow above.
  {
    "name": "nightly-tests",
    "definition": {
      "version": 1,
      "kind": "job",
      "enabled": true,
      "trigger": {"kind": "cron", "cron": "0 2 * * *", "timezone": "UTC"},
      "action": {"kind": "execute", "workflow_uid": "01J00000000000000000000010"},
      "issue": {"kind": "per-run", "title": "Nightly tests"},
      "overlap": "forbid",
      "catchup": "latest",
      "checkout_key": "example-project"
    }
  }
`

func cronRequestHelp(resource string) string {
	if resource == "workflow" {
		return `Both name and definition are required. The definition needs version: 1 and
a nonempty steps array. Each step needs a unique key, kind, and the matching
command or prompt. after names earlier step keys.`
	}
	return `Both name and definition are required. A job definition needs version: 1,
kind: "job", trigger, action, overlap, and catchup. enabled defaults to false.
An execute action requires an issue policy and exactly one of prompt or
workflow_uid. Commands belong in workflow steps, referenced by workflow_uid.
A recurring notify job needs issue.kind: "per-run", title, and parent_uid
(or --parent); its overlap must be "allow". The examples use catchup: "latest"
to deliver only the newest occurrence after downtime. The adapter resolves
checkout_key locally; options.agent selects its prompt agent (adapter-specific).`
}

func cronRequestExamples(resource, action string) string {
	cmd := "  kata cron " + resource + " " + action
	if action == "create" {
		cmd += " --uid <" + resource + "-uid>"
	} else {
		cmd += " <" + resource + "-uid> --expected-event-uid <event-uid>"
	}
	if resource == "workflow" {
		return cronWorkflowRequestExample + cmd + " --file workflow.json --json"
	}
	return cronRecurringRequestExample + cmd + " --parent <parent-ref> --file recurring.json --json\n\n" +
		cronPromptRequestExample + cmd + " --file prompt.json --json\n\n" +
		"  # Choose a workflow ULID; replace the sample UID in both the command and job.\n" +
		"  # If the workflow already exists, use its UID and skip workflow create.\n" +
		cronWorkflowRequestExample + "  kata cron workflow create --uid 01J00000000000000000000010 --file workflow.json --json\n\n" +
		cronCommandRequestExample + cmd + " --file command.json --json"
}

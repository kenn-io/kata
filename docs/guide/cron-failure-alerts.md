---
title: Alert on cron run failures
description: Notify an inbox recipient when a cron run observation reports a failure.
last_edited: 2026-11-21
---

# Alert on cron run failures

You can configure a daemon hook to send an inbox request whenever an adapter
records a cron run with status `failed` or `unknown`. Choose an open issue and
an exact recipient address for the alerts. That issue is the target of the inbox
request, and it does not have to be the issue associated with the run.

## Configure the hook

Before you write the script, check these requirements on the daemon host:

- **Python 3 and a named daemon catalog entry.** The example script uses Python
  3 and a named daemon catalog entry that selects the daemon running the hook.
  For a local home, configure a
  [local daemon profile](../operations/local-daemon-profiles.md) and confirm that
  the hook's CLI environment can resolve the entry.
- **Actor and environment variables.** The hook inherits the daemon's
  environment, plus any configured hook variables and Kata event variables. To
  choose the local actor, set `KATA_AUTHOR` in the daemon environment;
  otherwise, Kata falls back to `USER`, Git `user.name`, and then `anonymous`.
  In identity mode, Kata derives the actor from the token instead; see
  [Actor](concepts.md#actor). Set `KATA_*` variables in the daemon environment,
  not in `hook.env`.
- **Notification write credentials.** Give the CLI a credential with
  notification write authority. A named daemon can use catalog `token`,
  `token_file`, or `token_env`, `KATA_AUTH_TOKEN` can supply a token, and local
  selection can use the home's `[auth].token`. Pinned profiles use their own
  auth configuration. See
  [daemon selection and credentials](../reference/go-client.md#connect-to-a-catalog-daemon-by-name)
  for precedence and profile details.

Save this script as an executable file on the daemon host, for example
`/opt/kata/bin/cron-alert`. Replace `PROJECT`, `ISSUE_UID`, `RECIPIENT`, the
absolute `kata` path, and the named daemon with values for that daemon. Pick an
open issue in the configured project. Closing that issue hides existing requests
and prevents new notifications. Monitor the issue, and reopen it or point the
script at another open target if it closes.

```python
#!/usr/bin/env python3
import json
import subprocess
import sys

PROJECT = "example-project"
ISSUE_UID = "01J00000000000000000000010"
RECIPIENT = "example-agent/worker"
KATA = "/opt/kata/bin/kata"
DAEMON = "example-daemon"

event = json.load(sys.stdin)
if type(event.get("kata_hook_version")) is not int or event["kata_hook_version"] != 1:
    raise SystemExit("unsupported hook envelope version")
if event.get("project", {}).get("name") != PROJECT:
    sys.exit(0)
status = event.get("payload", {}).get("status")
if status in ("failed", "unknown"):
    uid = event["payload"]["uid"]
    subprocess.run(
        [KATA, "--daemon", DAEMON, "--project", PROJECT, "notify", ISSUE_UID,
         "--to", RECIPIENT, "--message",
         f"Cron observation {status}: run {uid}", "--agent"],
        check=True,
    )
```

The project, issue, and recipient come from operator configuration, not from an
observation. The run UID lets the recipient inspect the observation with
`kata --daemon example-daemon --project example-project cron run show <run-uid>`.
Because the script passes an argument list to `subprocess.run`, a shell never
interprets event data. Protect the script and daemon configuration from
modification by untrusted users.
[Hooks run with the daemon's authority](../design/architecture.md#hooks-local-automation-with-a-hard-boundary).

The filter checks the project name resolved when the hook runs. After you rename
a project, update `PROJECT` in the script; otherwise, the hook silently skips
observations and the hook log can still show success.

Next, add this entry to `<KATA_HOME>/hooks.toml` using the script's absolute
path:

```toml
[[hook]]
event = "cron.run.observed"
command = "/opt/kata/bin/cron-alert"
```

Restart the daemon to load the new hook configuration. Then verify the exact
project and open target issue on the selected daemon, and record a synthetic
failed observation with a test job, issue, and recipient. Use the same daemon,
project, and exact recipient as in your hook configuration, and replace the
example run UID below with a fresh valid run UID on every probe.

Run the `daemon locate` command in the hook's environment and working directory,
and confirm that its address matches the daemon running the hook. Use that same
catalog configuration for the remaining commands:

```sh
kata --daemon example-daemon daemon locate --json
kata --daemon example-daemon projects show example-project
kata --daemon example-daemon --project example-project show 01J00000000000000000000010
kata --daemon example-daemon --project example-project cron run observe 01J00000000000000000000020 --json-input observation.json --json
kata --daemon example-daemon --project example-project inbox --for example-agent/worker
```

Prepare `observation.json` with `status: "failed"` and valid test-job
references; the [cron reference](../reference/cron.md#independent-run-evidence)
describes the fields. Replaying identical evidence emits no event and cannot
test a changed hook.

The hook runs asynchronously. Wait and recheck both the inbox for your run UID
and the hook log before treating an empty inbox as a failure. To check the hook
result on the daemon host, select its `KATA_HOME` and run
`kata daemon logs --hooks`. Daemon administration uses the current home, not
`--daemon`. Compare the configured project and recipient values exactly. A
different project is filtered out, and a different valid recipient has a
separate inbox.

## Delivery and federation

Hooks run asynchronously after an observation commits and cannot roll it back.
The hook queue is bounded and can drop work, and a hook itself can fail or time
out. Inspect hook logs and daemon logs to monitor delivery. If missing an alert
is unacceptable, use an external durable delivery system.

Each matching new or changed observation can trigger a notification, while an
identical observation retry emits no new event. Because `kata notify` keeps one
request per issue and recipient, a later request replaces the previous one.
Hooks can also run concurrently and finish out of order. The inbox message is
not necessarily the newest observation.

An alert stays in the inbox until an operator clears it; a later successful
observation does not clear it automatically. After you handle an alert, clear it
with `kata --daemon example-daemon --project example-project notify
01J00000000000000000000010 --to example-agent/worker --clear`, and inspect run
history for the full picture.

Hooks can fire for local writes, events accepted from a spoke push, events
accepted by a relay, and events delivered by federation or relay pulls. A spoke
that pulls the project can therefore alert on another spoke's run. Configuring
this hook on multiple daemons can produce duplicate alerts. Choose the daemon
and route that fit your deployment. Federation and hook delivery are
asynchronous, which means a spoke observation does not imply immediate delivery.
The transport-only `cron.run.snapshot` bootstrap does not trigger the
observation hook, so bootstrapped history does not produce alerts.

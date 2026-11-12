---
title: Alert on cron run failures
description: Notify an inbox recipient when a cron run observation reports a failure.
last_edited: 2026-11-12
---

# Alert on cron run failures

A daemon hook can send an inbox request when an adapter records a run with
status `failed` or `unknown`. Choose an open issue and an exact recipient
address for the alerts. The issue is the inbox request's target; it need not be
the issue associated with the run.

## Configure the hook

On the daemon host, save this example as an executable script, such as
`/opt/kata/bin/cron-alert`. Replace the project, issue UID, recipient, and
absolute `kata` path and named daemon with values for that daemon. Use an open
issue in the configured project. Closing it hides existing requests and prevents
new notifications; monitor the issue and reopen it or configure another open
target. The example requires Python 3. Ensure the hook's CLI environment can
resolve the named daemon; for a local home without a catalog entry, omit
`--daemon` and select the same Kata home in the hook environment and
verification commands.

The hook inherits the daemon's environment, plus configured hook variables and
Kata event variables. Set `KATA_AUTHOR` in the daemon environment for the
desired local actor; otherwise `USER`, Git `user.name`, then `anonymous` are
used. Identity mode derives the actor from the token instead; see [Actor](concepts.md#actor).
Give the CLI a credential with notification write authority: a named daemon
can use catalog `token`, `token_file`, or `token_env`, while `KATA_AUTH_TOKEN`
can supply a token and local selection can use the home's `[auth].token`.
Pinned profiles use their own auth configuration. See [daemon selection and
credentials](../reference/go-client.md#connect-to-a-catalog-daemon-by-name) for precedence
and profile details. Set `KATA_*` variables in the daemon environment, not
`hook.env`.

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
if event.get("payload_truncated"):
    raise SystemExit("hook payload was truncated")
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

The project, issue, and recipient are operator configuration, not values taken
from an observation. The run UID lets the recipient inspect the observation
with `kata --daemon example-daemon --project example-project cron run show <run-uid>`.
The command uses an argument list, so event
data is not interpreted by a shell. Protect the script and daemon configuration
from modification by untrusted users: [hooks run with the daemon's authority](../design/architecture.md#hooks-local-automation-with-a-hard-boundary).
The filter uses the project name resolved when the hook runs. After a project
rename, update `PROJECT` in the script; otherwise the hook silently
skips observations and the hook log can still show success.

Add this entry to `<KATA_HOME>/hooks.toml`, using the script's absolute path:

```toml
[[hook]]
event = "cron.run.observed"
command = "/opt/kata/bin/cron-alert"
```

Restart the daemon to load the configuration. Verify the exact project and open
target on the selected daemon, then record a synthetic failed observation using
a test job, issue, and recipient. Use the same daemon, project, and exact
recipient as the hook configuration. Use a fresh valid run UID for every probe;
the UID below is an example to replace:

```sh
kata --daemon example-daemon projects show example-project
kata --daemon example-daemon --project example-project show 01J00000000000000000000010
kata --daemon example-daemon --project example-project cron run observe 01J00000000000000000000020 --json-input observation.json --json
kata --daemon example-daemon --project example-project inbox --for example-agent/worker
```

Prepare `observation.json` with `status: "failed"` and valid test-job references;
the [cron reference](../reference/cron.md#independent-run-evidence) describes the
fields. The hook runs asynchronously: wait and recheck the inbox for this run
UID and the hook log for its result before treating an empty inbox as a failure.
On the daemon host, select its `KATA_HOME` and run `kata daemon logs --hooks` to check the
hook result. A replay of identical evidence emits no event and cannot test a
changed hook. Daemon administration uses
the current home, not `--daemon`. Compare the configured project and recipient
exactly: a different project is filtered out, and a different valid recipient
has a separate inbox.

## Delivery and federation

Hooks run asynchronously after a committed observation; they cannot roll it
back. The queue is bounded and can drop work, and a hook can fail or time out.
Inspect hook logs and daemon logs; use an external durable delivery system if
missing an alert is unacceptable. A truncated payload fails this example
instead of treating an absent status as success.

Each matching new or changed observation can trigger a notification; an
identical observation retry emits no new event. `kata notify` keeps one request
per issue and recipient, so a later request replaces the previous one. Hooks
can run concurrently and finish out of order: the inbox message is not
necessarily the newest observation. An alert remains until an operator clears
it; a later successful observation does not clear it automatically. After
handling it, use `kata --daemon example-daemon --project example-project notify
01J00000000000000000000010 --to example-agent/worker --clear` and inspect run
history for the full picture.

Hooks can fire for local writes, events accepted from a spoke push, events
accepted by a relay, and events delivered by federation or relay pulls. A spoke
that pulls the project can therefore alert on another spoke's run. Configuring
the recipe on multiple daemons can produce duplicate alerts; choose the daemon
and route that fit your deployment. Federation and hook delivery are
asynchronous, so a spoke observation does not imply immediate delivery.

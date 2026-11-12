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
target. The daemon must have authority to write the notification. The example
requires Python 3. Ensure the hook's CLI environment can resolve the named
daemon; for a local home without a catalog entry, omit `--daemon` and select
the same Kata home in the hook environment and verification commands.

```python
#!/usr/bin/env python3
import json
import re
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
    uid = event["payload"].get("uid")
    if not isinstance(uid, str) or not re.fullmatch(r"[0-7][0-9A-HJKMNP-TV-Z]{25}", uid):
        raise SystemExit("invalid run UID")
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

Add this entry to `<KATA_HOME>/hooks.toml`, using the script's absolute path:

```toml
[[hook]]
event = "cron.run.observed"
command = "/opt/kata/bin/cron-alert"
```

Restart the daemon to load the configuration. Verify the exact project and open
target on the selected daemon, then record a synthetic failed observation using
a test job, issue, and recipient. Use the same daemon, project, and exact
recipient as the hook configuration:

```sh
kata --daemon example-daemon projects show example-project
kata --daemon example-daemon --project example-project show 01J00000000000000000000010
kata --daemon example-daemon --project example-project cron run observe 01J00000000000000000000020 --json-input observation.json --json
kata --daemon example-daemon --project example-project inbox --for example-agent/worker
```

Prepare `observation.json` with `status: "failed"` and valid test-job references;
the [cron reference](../reference/cron.md#independent-run-evidence) describes the
fields. On the daemon host, select its `KATA_HOME` and run
`kata daemon logs --hooks` to check the hook result. Daemon administration uses
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

A spoke can run its own hook for a locally recorded observation. When its event
is accepted by the hub, the hub also publishes it to its hooks. Configuring the
recipe on both can therefore notify twice; choose the daemon and route that fit
your deployment. Federation and hook delivery are asynchronous, so neither a
spoke observation nor its hub alert implies immediate delivery.

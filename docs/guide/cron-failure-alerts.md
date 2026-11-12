---
title: Alert on cron run failures
description: Notify an inbox recipient when a cron run observation reports a failure.
last_edited: 2026-11-11
---

# Alert on cron run failures

A daemon hook can send an inbox request when an adapter records a run with
status `failed` or `unknown`. Choose an existing issue and an exact recipient
address for the alerts. The issue is the inbox request's target; it need not be
the issue associated with the run.

## Configure the hook

On the daemon host, save this example as an executable script, such as
`/opt/kata/bin/cron-alert`. Replace the project, issue UID, recipient, and
absolute `kata` path with values for that daemon. Use an existing issue in the
configured project. The daemon must have authority to write the notification.
The example requires Python 3. Ensure the CLI invoked by the hook selects
this daemon and its Kata home; configure its environment or use an explicit
`--daemon` argument if your installation requires one.

```python
#!/usr/bin/env python3
import json
import subprocess
import sys

PROJECT = "example-project"
ISSUE_UID = "01J00000000000000000000010"
RECIPIENT = "example-agent/worker"
KATA = "/opt/kata/bin/kata"

event = json.load(sys.stdin)
if event.get("payload_truncated"):
    raise SystemExit("hook payload was truncated")
if event.get("type") != "cron.run.observed":
    raise SystemExit("unexpected event type")
if event.get("project", {}).get("name") != PROJECT:
    sys.exit(0)
status = event.get("payload", {}).get("status")
if status in ("failed", "unknown"):
    event_id = event.get("event_id")
    if type(event_id) is not int or event_id <= 0:
        raise SystemExit("invalid event ID")
    subprocess.run(
        [KATA, "--project", PROJECT, "notify", ISSUE_UID,
         "--to", RECIPIENT, "--message",
         f"Cron observation {status} (daemon event {event_id})", "--agent"],
        check=True,
    )
```

The project, issue, and recipient are operator configuration, not values taken
from an observation. The event ID is local to the daemon and helps find the
observation in its event history. The command uses an argument list, so event
data is not interpreted by a shell. Protect the script and daemon configuration
from modification by untrusted users: hooks run with the daemon's authority.

Add this entry to `<KATA_HOME>/hooks.toml`, using the script's absolute path:

```toml
[[hook]]
event = "cron.run.observed"
command = "/opt/kata/bin/cron-alert"
```

Restart the daemon to load the configuration. Record a synthetic observation in
a test project, then check `kata inbox --for example-agent/worker` and
`kata daemon logs --hooks`. Use a test issue and recipient when checking the
recipe. The [cron reference](../reference/cron.md#independent-run-evidence)
shows how to record an observation.

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
necessarily the newest observation. Clear the request after handling it and
inspect run history for the full picture.

A spoke can run its own hook for a locally recorded observation. When its event
is accepted by the hub, the hub also publishes it to its hooks. Configuring the
recipe on both can therefore notify twice; choose the daemon and route that fit
your deployment. Federation and hook delivery are asynchronous, so neither a
spoke observation nor its hub alert implies immediate delivery.

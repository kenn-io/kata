#!/usr/bin/env python3
"""Compare root/serving-relay behavior on disposable loopback daemons.

Usage: python3 e2e/testdata/relay_withheld_creation.py /absolute/path/to/kata [historical]

The optional historical case creates and removes a private link before relay
admission. The default case checks live-link refusal and the full adapter cron
lifecycle followed by ordinary issue delivery. Native-store regressions cover
retained-checkpoint refresh and explicit quarantine recovery automatically.
"""

import json
import os
import pathlib
import shutil
import socket
import subprocess
import sys
import tempfile
import time

binary = sys.argv[1]
scratch = pathlib.Path(tempfile.mkdtemp(prefix="kata-relay-repro-"))
print("SCRATCH", scratch, flush=True)
base = {
    k: os.environ[k]
    for k in ("PATH", "HOME", "USER", "TMPDIR", "LANG")
    if k in os.environ
}
base["GOMAXPROCS"] = "2"
procs = []
ports = {}
envs = {}


def call(side, *args, token=None, check=True):
    e = envs[side].copy()
    if token:
        e["KATA_AUTH_TOKEN"] = token
    workspace = (
        scratch
        / side
        / "workspace"
        / str(args[args.index("--project") + 1] if "--project" in args else "admin")
    )
    workspace.mkdir(exist_ok=True)
    p = subprocess.run(
        [binary, *args],
        env=e,
        cwd=workspace,
        capture_output=True,
        check=False,
        text=True,
    )
    print(side, " ".join(args), "=>", p.returncode, p.stdout, p.stderr, flush=True)
    if check and p.returncode:
        raise RuntimeError(args)
    if p.returncode:
        return {"command_exit_code": p.returncode, "stderr": p.stderr}
    try:
        return json.loads(p.stdout)
    except json.JSONDecodeError:
        return p.stdout


def start(side, extra=""):
    home = scratch / side
    home.mkdir(exist_ok=True)
    (home / "workspace").mkdir(exist_ok=True)
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        ports[side] = s.getsockname()[1]
    envs[side] = dict(
        base,
        KATA_HOME=str(home),
        KATA_SERVER=f"http://127.0.0.1:{ports[side]}",
        KATA_AUTH_TOKEN="scratch-bootstrap-token",
    )
    (home / "config.toml").write_text(
        '[auth]\ntoken = "scratch-bootstrap-token"\nrequire_token_identity = true\ntrust_private_network = true\n'
        + extra
    )
    e = envs[side].copy()
    e.pop("KATA_SERVER")
    e.pop("KATA_AUTH_TOKEN")
    with open(home / "daemon.log", "w") as log:
        p = subprocess.Popen(
            [
                binary,
                "daemon",
                "start",
                "--foreground",
                "--listen",
                f"127.0.0.1:{ports[side]}",
            ],
            env=e,
            cwd=home / "workspace",
            stdout=log,
            stderr=log,
        )
    procs.append(p)
    for i in range(100):
        try:
            socket.create_connection(("127.0.0.1", ports[side]), timeout=0.3).close()
            break
        except OSError:
            time.sleep(0.1)
    else:
        raise RuntimeError((home / "daemon.log").read_text())


try:
    start("root")
    token = call(
        "root", "tokens", "create", "--actor", "member", "--name", "scratch", "--json"
    )["plaintext"]
    envs["root"]["KATA_AUTH_TOKEN"] = token
    call("root", "init", "--project", "shared-project", "--json")
    call("root", "init", "--project", "private-project", "--json")
    if len(sys.argv) > 2:
        private = call(
            "root",
            "create",
            "Private historical target",
            "--project",
            "private-project",
            "--json",
        )["issue"]
        historical = call(
            "root",
            "create",
            "Historical linked creation",
            "--project",
            "shared-project",
            "--related",
            "private-project#" + private["short_id"],
            "--json",
        )["issue"]
        call(
            "root",
            "edit",
            historical["short_id"],
            "--remove-related",
            "private-project#" + private["short_id"],
            "--project",
            "shared-project",
            "--json",
        )
    call("root", "federation", "enable", "--project", "shared-project", "--json")
    start(
        "relay",
        f'\n[[daemon]]\nname = "root-hub"\nurl = "http://127.0.0.1:{ports["root"]}"\ntoken_env = "SCRATCH_ROOT_TOKEN"\n',
    )
    # catalog tokens must be present in daemon's allowlisted environment: restart only relay
    procs[-1].terminate()
    procs[-1].wait()
    e = envs["relay"].copy()
    e.pop("KATA_SERVER")
    e.pop("KATA_AUTH_TOKEN")
    e["SCRATCH_ROOT_TOKEN"] = token
    with open(scratch / "relay" / "daemon.log", "a") as log:
        procs.append(
            subprocess.Popen(
                [
                    binary,
                    "daemon",
                    "start",
                    "--foreground",
                    "--listen",
                    f"127.0.0.1:{ports['relay']}",
                ],
                env=e,
                cwd=scratch / "relay" / "workspace",
                stdout=log,
                stderr=log,
            )
        )
    time.sleep(1)
    call(
        "relay",
        "federation",
        "bridge",
        "connect",
        "--project",
        "shared-replica",
        "--hub-daemon",
        "root-hub",
        "--hub-project",
        "shared-project",
        "--serve-downstream",
        "--as",
        "member",
        "--json",
    )
    private = call(
        "root", "create", "Private target", "--project", "private-project", "--json"
    )
    issue = call(
        "root",
        "create",
        "Linked creation",
        "--project",
        "shared-project",
        "--related",
        "private-project#" + private["issue"]["short_id"],
        "--json",
        check=False,
    )
    ordinary = call(
        "root",
        "create",
        "Later ordinary issue",
        "--project",
        "shared-project",
        "--json",
    )["issue"]
    call(
        "root",
        "meta",
        "set",
        ordinary["short_id"],
        "adapter.state",
        "running",
        "--project",
        "shared-project",
        "--json",
    )
    call(
        "root",
        "assign",
        ordinary["short_id"],
        "member",
        "--project",
        "shared-project",
        "--json",
    )
    definition = {
        "name": "Relay canary",
        "definition": {
            "version": 1,
            "kind": "job",
            "enabled": True,
            "trigger": {"kind": "manual"},
            "action": {"kind": "execute", "prompt": "Check relay progress."},
            "issue": {"kind": "per-run", "title": "Relay canary run"},
            "overlap": "forbid",
            "catchup": "latest",
        },
    }
    jobfile = scratch / "job.json"
    jobfile.write_text(json.dumps(definition))
    job = call(
        "root",
        "cron",
        "job",
        "create",
        "--uid",
        "01ARZ3NDEKTSV4RRFFQ69G5FAA",
        "--file",
        str(jobfile),
        "--project",
        "shared-project",
        "--json",
    )
    runissue = call(
        "root", "create", "Relay canary run", "--project", "shared-project", "--json"
    )["issue"]
    print("JOB_RESPONSE", job, flush=True)
    eventuid = job.get(
        "definition_event_uid", job.get("job", {}).get("definition_event_uid")
    )
    observation = {
        "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAA",
        "definition_event_uid": eventuid,
        "occurrence_key": "manual:repro",
        "issue_uid": runissue["uid"],
        "actor": "member",
        "executor_label": "scratch-adapter",
        "status": "succeeded",
        "summary": {"version": 1, "message": "Canary completed"},
        "expected_revision": 0,
    }
    obsfile = scratch / "observation.json"
    obsfile.write_text(json.dumps(observation))
    call(
        "root",
        "cron",
        "run",
        "observe",
        "01ARZ3NDEKTSV4RRFFQ69G5FAB",
        "--json-input",
        str(obsfile),
        "--project",
        "shared-project",
        "--json",
    )
    call(
        "root",
        "cron",
        "job",
        "delete",
        "01ARZ3NDEKTSV4RRFFQ69G5FAA",
        "--expected-event-uid",
        eventuid,
        "--project",
        "shared-project",
        "--json",
    )
    call(
        "root",
        "close",
        runissue["short_id"],
        "--done",
        "--message",
        "Completed the scratch relay adapter canary with a retained run observation.",
        "--evidence",
        "test:scratch-adapter",
        "--project",
        "shared-project",
        "--json",
    )
    call(
        "root",
        "create",
        "After cron ordinary issue",
        "--project",
        "shared-project",
        "--json",
    )
    time.sleep(35)
    received = call("relay", "list", "--project", "shared-replica", "--json")["issues"]
    assert {x["title"] for x in received} == (
        {
            "Later ordinary issue",
            "After cron ordinary issue",
            "Historical linked creation",
        }
        if len(sys.argv) > 2
        else {"Later ordinary issue", "After cron ordinary issue"}
    )
    mirrored = next(x for x in received if x["uid"] == ordinary["uid"])
    assert (
        mirrored["owner"] == "member"
        and mirrored["metadata"]["adapter.state"] == "running"
    )
    closed = call(
        "relay", "show", runissue["short_id"], "--project", "shared-replica", "--json"
    )
    assert closed["issue"]["status"] == "closed"
    observed = call(
        "relay",
        "cron",
        "run",
        "show",
        "01ARZ3NDEKTSV4RRFFQ69G5FAB",
        "--project",
        "shared-replica",
        "--json",
    )
    assert observed["run"]["status"] == "succeeded"
    deleted = call(
        "relay",
        "cron",
        "job",
        "show",
        "01ARZ3NDEKTSV4RRFFQ69G5FAA",
        "--project",
        "shared-replica",
        "--json",
    )
    assert deleted["job"]["deleted_at"]
    assert issue["command_exit_code"] == 5
    status = call("relay", "federation", "status", "--json")
    assert status["statuses"][0]["active_quarantine_count"] == 0
    print("PASS behavioral assertions", flush=True)
finally:
    for p in reversed(procs):
        if p.poll() is None:
            p.terminate()
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait(timeout=10)
    shutil.rmtree(scratch)
    print("STOPPED AND REMOVED", scratch, flush=True)

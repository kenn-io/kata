#!/usr/bin/env python3
"""Measure the tracker latencies quoted on katatracker.com.

Benchmarks an isolated kata daemon (Unix-socket round trips and end-to-end
CLI invocations) and prints millisecond percentiles as JSON. Run it against a
throwaway KATA_HOME, never a live one:

    export KATA_HOME=$(mktemp -d)/home
    mkdir -p "$KATA_HOME" /tmp/bench-ws && cd /tmp/bench-ws
    kata init --project bench
    export BENCH_WS=$PWD
    export BENCH_SOCK=$(kata daemon status | sed -n 's/.*unix:\\/\\///p')
    python3 scripts/bench-tracker-latency.py

The hosted-tracker comparison quoted next to these numbers is a GitHub Issues
REST list (per_page=50) timed with `curl -w '%{time_total}'` from the same
machine, cold (fresh TLS) and warm (URLs repeated in one curl invocation).
"""

from __future__ import annotations

import http.client
import json
import os
import socket
import statistics
import subprocess
import sys
import time

WS = os.environ["BENCH_WS"]
SOCK = os.environ["BENCH_SOCK"]
SEED = int(os.environ.get("BENCH_SEED", "300"))
N_HTTP = 200
N_CLI = 30


def run(args: list[str]) -> str:
    result = subprocess.run(
        ["kata", *args], cwd=WS, capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        sys.exit(f"kata {' '.join(args)} failed: {result.stderr.strip()}")
    return result.stdout


def seed() -> list[str]:
    refs = []
    for i in range(SEED):
        out = run(
            [
                "create",
                f"bench issue {i}: fix synthetic race in module {i % 17}",
                "--force-new",
                "--body",
                f"Synthetic benchmark issue {i} with a body long enough to be realistic.",
                "--idempotency-key",
                f"bench-{i}",
                "--agent",
            ]
        )
        refs.append(out.split()[-1].strip())
    return refs


def existing_refs() -> list[str]:
    data = json.loads(run(["list", "--status", "open", "--limit", "0", "--json"]))
    return [issue["short_id"] for issue in data["issues"]]


class UDSConnection(http.client.HTTPConnection):
    def __init__(self, path: str) -> None:
        super().__init__("kata")
        self._uds_path = path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX)
        sock.connect(self._uds_path)
        self.sock = sock


def bench_http(path: str, warm: bool) -> list[float]:
    times: list[float] = []
    conn = None
    for _ in range(N_HTTP):
        start = time.perf_counter()
        if conn is None:
            conn = UDSConnection(SOCK)
        conn.request("GET", path)
        response = conn.getresponse()
        body = response.read()
        if response.status != 200:
            raise RuntimeError(
                f"unexpected status for {path}: {response.status} {body[:100]!r}"
            )
        times.append((time.perf_counter() - start) * 1000)
        if not warm:
            conn.close()
            conn = None
    if conn is not None:
        conn.close()
    return times


def bench_cli(label: str, make_args, results: dict) -> None:
    times: list[float] = []
    for i in range(N_CLI):
        args = make_args(i)
        start = time.perf_counter()
        run(args)
        times.append((time.perf_counter() - start) * 1000)
    results[label] = summarize(times)


def summarize(times: list[float]) -> dict:
    ordered = sorted(times)
    return {
        "n": len(ordered),
        "p50_ms": round(statistics.median(ordered), 3),
        "p95_ms": round(ordered[int(len(ordered) * 0.95) - 1], 3),
        "min_ms": round(ordered[0], 3),
        "max_ms": round(ordered[-1], 3),
    }


def main() -> None:
    refs = existing_refs() if os.environ.get("BENCH_SKIP_SEED") else seed()
    results: dict = {"seeded_issues": len(refs)}

    issues_path = "/api/v1/issues?project=bench&status=open"
    results["daemon_health_warm"] = summarize(bench_http("/api/v1/health", warm=True))
    results["daemon_issues_warm"] = summarize(bench_http(issues_path, warm=True))
    results["daemon_issues_cold_conn"] = summarize(bench_http(issues_path, warm=False))

    bench_cli("cli_list", lambda i: ["list", "--agent"], results)
    bench_cli("cli_show", lambda i: ["show", refs[i % len(refs)], "--agent"], results)
    bench_cli("cli_search", lambda i: ["search", "synthetic race", "--agent"], results)
    bench_cli("cli_next", lambda i: ["next", "--unowned", "--agent"], results)
    bench_cli(
        "cli_create",
        lambda i: [
            "create",
            f"bench extra {i}",
            "--force-new",
            "--idempotency-key",
            f"bench-extra-{i}",
            "--agent",
        ],
        results,
    )
    bench_cli(
        "cli_comment",
        lambda i: ["comment", refs[i], "--body", f"bench comment {i}", "--agent"],
        results,
    )
    bench_cli(
        "cli_close",
        lambda i: [
            "close",
            refs[i],
            "--done",
            "--message",
            "Benchmark issue complete; synthetic work verified.",
            "--test",
            "python3 scripts/bench-tracker-latency.py",
            "--agent",
        ],
        results,
    )

    print(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()

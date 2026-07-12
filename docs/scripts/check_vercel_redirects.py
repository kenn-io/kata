#!/usr/bin/env python3
from __future__ import annotations

import fnmatch
import json
import math
import pathlib
import sys
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[1]
VERCEL = ROOT / "vercel.json"
VERCELIGNORE = ROOT.parent / ".vercelignore"

TEMPORARY = {
    "/install.sh": "https://raw.githubusercontent.com/kenn-io/kata/main/scripts/install.sh",
    "/install.ps1": "https://raw.githubusercontent.com/kenn-io/kata/main/scripts/install.ps1",
}


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def reject_json_constant(constant: str) -> None:
    raise ValueError(f"non-finite numeric constant {constant}")


def validate_finite_json_numbers(path: str, value: object) -> None:
    if isinstance(value, bool):
        return
    if isinstance(value, float):
        if not math.isfinite(value):
            fail(f"{path} contains non-finite number")
        return
    if isinstance(value, dict):
        for key, item in value.items():
            validate_finite_json_numbers(f"{path}.{key}", item)
    elif isinstance(value, list):
        for index, item in enumerate(value):
            validate_finite_json_numbers(f"{path}[{index}]", item)


def load_vercel() -> dict[str, Any]:
    try:
        data = json.loads(
            VERCEL.read_text(encoding="utf-8"),
            parse_constant=reject_json_constant,
        )
    except FileNotFoundError:
        fail("missing vercel.json")
    except json.JSONDecodeError as error:
        fail(f"invalid vercel.json: {error}")
    except ValueError as error:
        fail(f"invalid vercel.json: {error}")

    if not isinstance(data, dict):
        fail("vercel.json must contain an object")
    validate_finite_json_numbers("vercel.json", data)
    return data


def load_vercelignore() -> list[str]:
    try:
        lines = VERCELIGNORE.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        fail("missing repository-root .vercelignore")
    return [line.strip() for line in lines if line.strip() and not line.lstrip().startswith("#")]


def ignore_rule_matches(rule: str, path: str) -> bool:
    pattern = rule.lstrip("/").rstrip("/")
    if pattern == "*":
        return True
    if any(character in pattern for character in "*?["):
        return fnmatch.fnmatchcase(path, pattern)
    return path == pattern or path.startswith(f"{pattern}/")


def deployment_includes(rules: list[str], path: str) -> bool:
    ignored = False
    for raw_rule in rules:
        negated = raw_rule.startswith("!")
        rule = raw_rule[1:] if negated else raw_rule
        if ignore_rule_matches(rule, path):
            ignored = not negated
    return not ignored


def collect_redirects(data: dict[str, object]) -> dict[str, dict[str, object]]:
    raw_redirects = data.get("redirects", [])
    if not isinstance(raw_redirects, list):
        fail("vercel redirects must be a list")

    redirects: dict[str, dict[str, object]] = {}
    for index, item in enumerate(raw_redirects):
        if not isinstance(item, dict):
            fail(f"redirect entry {index} must be an object")
        if set(item) != {"source", "destination", "permanent"}:
            fail(f"redirect entry {index} must contain source, destination, and permanent only")
        source = item.get("source")
        destination = item.get("destination")
        permanent = item.get("permanent")
        if not isinstance(source, str) or not source:
            fail(f"redirect entry {index} missing source")
        if not isinstance(destination, str) or not destination:
            fail(f"redirect entry {index} missing destination")
        if not isinstance(permanent, bool):
            fail(f"redirect entry {index} permanent must be boolean")
        if source in redirects:
            fail(f"duplicate redirect source {source}")
        redirects[source] = item
    return redirects


def main() -> None:
    rules = load_vercelignore()
    required_paths = (
        "docs/index.md",
        "docs/vercel.json",
        "docs/assets/screenshots/tui/hero.svg",
    )
    excluded_paths = (
        "cmd/kata/main.go",
        ".superpowers/sdd/progress.md",
        "docs/site/index.html",
        "docs/superpowers/plans/example.md",
        "docs/.venv/bin/zensical",
        "docs/.cache/build-entry",
        "docs/.env.local",
        "docs/.env.preview.local",
        "docs/.vercel/project.json",
        "docs/__pycache__/module.pyc",
        "docs/scripts/__pycache__/checker.pyc",
        "docs/.zensical-build.example.toml",
        "docs/zensical-public-docs.example/index.md",
        "docs/.idea/workspace.xml",
        "docs/guide/.idea/workspace.xml",
        "docs/.vscode/settings.json",
        "docs/guide/.vscode/settings.json",
        "docs/notes.swp",
        "docs/guide/notes.swp",
        "docs/draft~",
        "docs/guide/draft~",
        "docs/.DS_Store",
        "docs/guide/.DS_Store",
        "docs/.kata.local.toml",
        "docs/guide/.kata.local.toml",
        "docs/check.test",
        "docs/guide/check.test",
        "docs/check.out",
        "docs/guide/check.out",
        "docs/coverage.out",
        "docs/guide/coverage.out",
    )
    for path in required_paths:
        if not deployment_includes(rules, path):
            fail(f"Vercel deployment must include {path}")
    for path in excluded_paths:
        if deployment_includes(rules, path):
            fail(f"Vercel deployment must exclude {path}")

    data = load_vercel()
    if "framework" not in data or data["framework"] is not None:
        fail("vercel framework must be null")
    if data.get("installCommand") != "uv sync --frozen --no-dev":
        fail("unexpected Vercel installCommand")
    if data.get("buildCommand") != "uv run --frozen bash ./vercel-build.sh":
        fail("unexpected Vercel buildCommand")
    if data.get("outputDirectory") != "site":
        fail("unexpected Vercel outputDirectory")

    redirects = collect_redirects(data)
    for source in redirects:
        if source.endswith(".md"):
            fail(f"Markdown source URL must be served as a static file, not redirected: {source}")

    for source, destination in TEMPORARY.items():
        item = redirects.get(source)
        if not item:
            fail(f"missing temporary redirect {source}")
        if item.get("destination") != destination or item.get("permanent") is not False:
            fail(f"incorrect temporary redirect {source}")

    print("vercel redirect checks passed")


if __name__ == "__main__":
    main()

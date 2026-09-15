#!/usr/bin/env python3
from __future__ import annotations

import argparse
import datetime as dt
import pathlib
import sys
from typing import Any

import yaml


ROOT = pathlib.Path(__file__).resolve().parents[1]
TARGET_DIRS = ("guide", "operations", "reference")
REQUIRED_TEXT_FIELDS = ("title", "description")


def parse_frontmatter(path: pathlib.Path) -> tuple[dict[str, Any] | None, str | None]:
    lines = path.read_text(encoding="utf-8").splitlines()
    if not lines or lines[0] != "---":
        return None, "missing YAML frontmatter"
    try:
        closing_index = lines[1:].index("---") + 1
    except ValueError:
        return None, "unterminated YAML frontmatter"
    try:
        metadata = yaml.safe_load("\n".join(lines[1:closing_index]))
    except (ValueError, yaml.YAMLError):
        return None, "invalid YAML frontmatter"
    if not isinstance(metadata, dict):
        return None, "YAML frontmatter must be a mapping"
    return metadata, None


def valid_last_edited(value: Any) -> bool:
    if isinstance(value, dt.datetime):
        return False
    if isinstance(value, dt.date):
        return True
    if not isinstance(value, str):
        return False
    try:
        parsed = dt.date.fromisoformat(value)
    except ValueError:
        return False
    return value == parsed.isoformat()


def check_page(path: pathlib.Path, docs_root: pathlib.Path) -> list[str]:
    relative = path.relative_to(docs_root).as_posix()
    metadata, parse_error = parse_frontmatter(path)
    if parse_error is not None:
        return [f"{relative}: {parse_error}"]
    assert metadata is not None

    errors: list[str] = []
    for field in REQUIRED_TEXT_FIELDS:
        value = metadata.get(field)
        if not isinstance(value, str) or not value.strip():
            errors.append(f"{relative}: {field} must be a non-empty string")
    if not valid_last_edited(metadata.get("last_edited")):
        errors.append(f"{relative}: last_edited must be a YYYY-MM-DD date")
    return errors


def target_pages(docs_root: pathlib.Path) -> tuple[list[pathlib.Path], list[str]]:
    pages: list[pathlib.Path] = []
    errors: list[str] = []
    for directory in TARGET_DIRS:
        target = docs_root / directory
        if not target.is_dir():
            errors.append(f"{directory}: missing docs directory")
            continue
        pages.extend(
            path
            for path in target.rglob("*.md")
            if path.name != "README.md"
        )
    return sorted(pages), errors


def run(docs_root: pathlib.Path) -> int:
    pages, errors = target_pages(docs_root)
    for path in pages:
        errors.extend(check_page(path, docs_root))
    if errors:
        for error in errors:
            print(f"FAIL: {error}", file=sys.stderr)
        return 1
    print(f"frontmatter checks passed ({len(pages)} files)")
    return 0


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Check required frontmatter in guide, operations, and reference docs.",
    )
    parser.add_argument(
        "--docs-root",
        type=pathlib.Path,
        default=ROOT,
        help="docs root containing guide, operations, and reference directories",
    )
    args = parser.parse_args()
    raise SystemExit(run(args.docs_root.resolve()))


if __name__ == "__main__":
    main()

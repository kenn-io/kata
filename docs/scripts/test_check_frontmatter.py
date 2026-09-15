from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("check_frontmatter.py")
TARGET_DIRS = ("guide", "operations", "reference")


class CheckFrontmatterTest(unittest.TestCase):
    def run_check(self, docs_root: Path) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--docs-root", str(docs_root)],
            check=False,
            capture_output=True,
            text=True,
        )

    def make_docs_root(self, root: Path) -> None:
        for directory in TARGET_DIRS:
            (root / directory).mkdir(parents=True)

    def write_valid_page(self, path: Path, title: str = "Example page") -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            "---\n"
            f"title: {title}\n"
            "description: Explains the example behavior and its operator contract.\n"
            "last_edited: 2026-09-15\n"
            "---\n\n"
            f"# {title}\n",
            encoding="utf-8",
        )

    def test_accepts_valid_nested_pages_and_exempts_readmes(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            docs_root = Path(temp_dir)
            self.make_docs_root(docs_root)
            self.write_valid_page(docs_root / "guide" / "nested" / "concepts.md")
            self.write_valid_page(docs_root / "operations" / "backup.md")
            self.write_valid_page(docs_root / "reference" / "cli.md")
            (docs_root / "guide" / "README.md").write_text(
                "# Guide index\n",
                encoding="utf-8",
            )

            result = self.run_check(docs_root)

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, "frontmatter checks passed (3 files)\n")

    def test_reports_every_page_without_frontmatter(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            docs_root = Path(temp_dir)
            self.make_docs_root(docs_root)
            for relative_path in (
                "guide/missing.md",
                "operations/nested/also-missing.md",
            ):
                path = docs_root / relative_path
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("# Missing metadata\n", encoding="utf-8")

            result = self.run_check(docs_root)

            self.assertEqual(result.returncode, 1)
            self.assertIn("guide/missing.md: missing YAML frontmatter", result.stderr)
            self.assertIn(
                "operations/nested/also-missing.md: missing YAML frontmatter",
                result.stderr,
            )

    def test_reports_malformed_yaml_and_non_mapping_frontmatter(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            docs_root = Path(temp_dir)
            self.make_docs_root(docs_root)
            (docs_root / "guide" / "malformed.md").write_text(
                "---\ntitle: [unterminated\n---\n\n# Broken\n",
                encoding="utf-8",
            )
            (docs_root / "reference" / "list.md").write_text(
                "---\n- title\n- description\n---\n\n# List\n",
                encoding="utf-8",
            )

            result = self.run_check(docs_root)

            self.assertEqual(result.returncode, 1)
            self.assertIn("guide/malformed.md: invalid YAML frontmatter", result.stderr)
            self.assertIn(
                "reference/list.md: YAML frontmatter must be a mapping",
                result.stderr,
            )

    def test_requires_nonempty_strings_and_iso_last_edited_date(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            docs_root = Path(temp_dir)
            self.make_docs_root(docs_root)
            (docs_root / "operations" / "invalid.md").write_text(
                "---\n"
                "title: '   '\n"
                "description: 42\n"
                "last_edited: 15/09/2026\n"
                "---\n\n"
                "# Invalid\n",
                encoding="utf-8",
            )

            result = self.run_check(docs_root)

            self.assertEqual(result.returncode, 1)
            self.assertIn(
                "operations/invalid.md: title must be a non-empty string",
                result.stderr,
            )
            self.assertIn(
                "operations/invalid.md: description must be a non-empty string",
                result.stderr,
            )
            self.assertIn(
                "operations/invalid.md: last_edited must be a YYYY-MM-DD date",
                result.stderr,
            )

    def test_aggregates_impossible_yaml_date_with_other_page_errors(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            docs_root = Path(temp_dir)
            self.make_docs_root(docs_root)
            (docs_root / "guide" / "impossible-date.md").write_text(
                "---\n"
                "title: Impossible date\n"
                "description: Exercises an invalid calendar date.\n"
                "last_edited: 2026-02-29\n"
                "---\n\n"
                "# Impossible date\n",
                encoding="utf-8",
            )
            (docs_root / "reference" / "missing.md").write_text(
                "# Missing metadata\n",
                encoding="utf-8",
            )

            result = self.run_check(docs_root)

            self.assertEqual(result.returncode, 1)
            self.assertIn(
                "guide/impossible-date.md: invalid YAML frontmatter",
                result.stderr,
            )
            self.assertIn(
                "reference/missing.md: missing YAML frontmatter",
                result.stderr,
            )
            self.assertNotIn("Traceback", result.stderr)


if __name__ == "__main__":
    unittest.main()

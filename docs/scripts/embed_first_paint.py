#!/usr/bin/env python3
"""Make the hand-written pages in the assembled site paint in their own fonts.

The browser finds a font in a separate file only after it parses the
stylesheet, so it can paint text in a fallback face first. This embeds every
font that styles/site.css references as a data URL, and inlines
website/hold-first-paint.js right after each hand-written page's charset
declaration so Chromium waits for the embedded fonts to decode.
"""

from __future__ import annotations

import argparse
import base64
import pathlib
import re

from public_markdown_sources import fail

ROOT = pathlib.Path(__file__).resolve().parents[1]
WEBSITE = ROOT / "website"
HANDWRITTEN_PAGES = ("index.html", "guide/index.html", "404.html")
CHARSET = '<meta charset="utf-8">'
FONT_URL = re.compile(r'url\("/fonts/([A-Za-z0-9-]+\.woff2)"\)')


def font_data_url(match: re.Match[str]) -> str:
    font = WEBSITE / "fonts" / match[1]
    if not font.is_file():
        fail(f"styles/site.css references a missing font: {font.relative_to(ROOT)}")
    encoded = base64.b64encode(font.read_bytes()).decode("ascii")
    return f'url("data:font/woff2;base64,{encoded}")'


def embed_fonts(stylesheet: pathlib.Path) -> None:
    css, count = FONT_URL.subn(font_data_url, stylesheet.read_text(encoding="utf-8"))
    if count == 0:
        fail(f"{stylesheet} references no /fonts/ files to embed")
    if "/fonts/" in css:
        fail(f"{stylesheet} still references /fonts/ after embedding")
    stylesheet.write_text(css, encoding="utf-8")


def hold_first_paint(page: pathlib.Path, script: str) -> None:
    html = page.read_text(encoding="utf-8")
    if html.count(CHARSET) != 1:
        fail(f"{page} must declare {CHARSET} exactly once")
    held = html.replace(CHARSET, f"{CHARSET}\n    <script>\n{script}</script>")
    page.write_text(held, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Embed website fonts and hold first paint in the assembled site.",
    )
    parser.add_argument(
        "--site-dir",
        required=True,
        help="assembled site directory relative to docs root",
    )
    args = parser.parse_args()
    site_dir = ROOT / args.site_dir
    if not site_dir.is_dir():
        fail(f"missing assembled site directory: {site_dir.relative_to(ROOT)}")

    embed_fonts(site_dir / "styles" / "site.css")
    script = (WEBSITE / "hold-first-paint.js").read_text(encoding="utf-8")
    for name in HANDWRITTEN_PAGES:
        hold_first_paint(site_dir / name, script)
    print("website fonts embedded and first paint held")


if __name__ == "__main__":
    main()

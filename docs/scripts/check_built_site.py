#!/usr/bin/env python3
"""Validate the assembled three-tier site under docs/site.

Tiers: the hand-written marketing page at /, the guide at /guide/, and the
Zensical docs under /docs/. Every nav docs page must ship a Markdown twin and
advertise it; every local link and asset referenced by the hand-written tiers
must resolve inside the built site; llms.txt must index only real routes; and
no dotfile or credential-pattern file may enter the published output.
"""

from __future__ import annotations

import fnmatch
import pathlib
import re
import sys
from html.parser import HTMLParser

ROOT = pathlib.Path(__file__).resolve().parents[1]
SITE = ROOT / "site"
ORIGIN = "https://katatracker.com"

REQUIRED_ROOT_ENTRIES = [
    "index.html",
    "index.md",
    "guide/index.html",
    "guide.md",
    "404.html",
    "favicon.svg",
    "llms.txt",
    "sitemap.xml",
    "styles/site.css",
    "scripts/site.js",
    "fonts/JetBrainsMono-Regular.woff2",
    "fonts/licenses/Inter-OFL-1.1.txt",
    "fonts/licenses/JetBrains-Mono-OFL-1.1.txt",
    "docs/index.html",
    "docs/index.md",
    "docs/sitemap.xml",
]

REQUIRED_SITEMAP_URLS = [
    f"{ORIGIN}/",
    f"{ORIGIN}/guide/",
    f"{ORIGIN}/docs/",
    f"{ORIGIN}/docs/workflows/agents/",
]

# Keep in sync with the prune in docs/zensical-docs.sh assemble_site_root.
FORBIDDEN_SITE_PATTERNS = [
    ".*",
    "*~",
    "*.swp",
    "client_secret*.json",
    "credentials*.json",
    "service_account*.json",
    "service-account*.json",
    "token.json",
    "tokens.json",
    "*.pem",
    "*.key",
    "*.p12",
    "*.pfx",
    "id_rsa*",
    "id_ed25519*",
    "*.tfstate",
    "*.tfvars",
]


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


class LinkParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.references: list[str] = []
        self.ids: set[str] = set()

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        attributes = {name: value for name, value in attrs if value is not None}
        identifier = attributes.get("id")
        if identifier:
            self.ids.add(identifier)
        for attribute in ("href", "src"):
            value = attributes.get(attribute)
            if value:
                self.references.append(value)


def parse_html(path: pathlib.Path) -> LinkParser:
    parser = LinkParser()
    parser.feed(path.read_text(encoding="utf-8"))
    return parser


def route_to_file(route: str) -> pathlib.Path | None:
    """Map a root-relative route to the built file that serves it."""
    path = route.lstrip("/")
    if route.endswith("/") or path == "":
        return SITE / path / "index.html"
    candidate = SITE / path
    if candidate.suffix:
        return candidate
    # Extensionless routes are directories served via index.html.
    return SITE / path / "index.html"


REDIRECTED_ROUTE = re.compile(r"^/(install\.sh|install\.ps1|assets/screenshots/)")


def check_local_reference(page: pathlib.Path, reference: str) -> None:
    if reference.startswith(("http://", "https://", "mailto:", "data:", "#")):
        return
    if not reference.startswith("/"):
        fail(f"{page.relative_to(SITE)}: non-root-relative reference {reference}")
    route, _, fragment = reference.partition("#")
    if route == "":
        return
    if REDIRECTED_ROUTE.match(route):
        # Served through vercel.json redirects, not the filesystem.
        return
    target = route_to_file(route)
    if target is None or not target.is_file():
        fail(f"{page.relative_to(SITE)}: broken local reference {reference}")
    if fragment and target.suffix == ".html":
        anchors = parse_html(target).ids
        if fragment not in anchors:
            fail(
                f"{page.relative_to(SITE)}: reference {reference} targets a "
                f"missing anchor in {target.relative_to(SITE)}"
            )


def check_handwritten_pages() -> None:
    for name in ("index.html", "guide/index.html", "404.html"):
        page = SITE / name
        parser = parse_html(page)
        for reference in parser.references:
            check_local_reference(page, reference)


def nav_sources() -> list[str]:
    sys.path.insert(0, str(ROOT / "scripts"))
    from public_markdown_sources import public_markdown_sources

    return public_markdown_sources(ROOT / "zensical.toml")


def docs_route(source: str) -> str:
    stem = source.removesuffix(".md")
    if stem == "index":
        return "/docs/"
    return f"/docs/{stem.removesuffix('/index')}/"


def check_docs_tier() -> None:
    for source in nav_sources():
        route = docs_route(source)
        page = route_to_file(route)
        if page is None or not page.is_file():
            fail(f"docs page missing for nav source {source}: {route}")
        twin = SITE / "docs" / source
        if not twin.is_file():
            fail(f"Markdown twin missing for nav source {source}")
        twin_url = f"{ORIGIN}{route.rstrip('/')}"
        twin_url = f"{ORIGIN}/docs/index.md" if route == "/docs/" else f"{twin_url}.md"
        alternate = f'<link rel="alternate" type="text/markdown" href="{twin_url}">'
        if alternate not in page.read_text(encoding="utf-8"):
            fail(f"{page.relative_to(SITE)} does not advertise its Markdown twin")


def check_markdown_twin_links() -> None:
    for name in ("index.md", "guide.md"):
        text = (SITE / name).read_text(encoding="utf-8")
        for url in re.findall(r"\((https://katatracker\.com[^)#]*)[^)]*\)", text):
            route = url[len(ORIGIN) :] or "/"
            if REDIRECTED_ROUTE.match(route):
                continue
            target = route_to_file(route)
            if target is None or not target.is_file():
                fail(f"{name}: broken site link {url}")


def check_llms_txt() -> None:
    text = (SITE / "llms.txt").read_text(encoding="utf-8")
    urls = re.findall(r"\((https://katatracker\.com[^)]+)\)", text)
    if not urls:
        fail("llms.txt lists no katatracker.com routes")
    for url in urls:
        route = url[len(ORIGIN) :]
        target = route_to_file(route)
        if target is None or not target.is_file():
            fail(f"llms.txt links a missing route: {url}")
    for twin in (f"{ORIGIN}/index.md", f"{ORIGIN}/guide.md", f"{ORIGIN}/docs/index.md"):
        if twin not in urls:
            fail(f"llms.txt must index {twin}")


def check_sitemaps() -> None:
    root_sitemap = (SITE / "sitemap.xml").read_text(encoding="utf-8")
    for url in REQUIRED_SITEMAP_URLS:
        if f"<loc>{url}</loc>" not in root_sitemap:
            fail(f"root sitemap.xml is missing {url}")
    docs_sitemap = (SITE / "docs" / "sitemap.xml").read_text(encoding="utf-8")
    if f"<loc>{ORIGIN}/docs/</loc>" not in docs_sitemap:
        fail("docs sitemap.xml is missing the docs index")


def check_public_site_file_inventory(site: pathlib.Path = SITE) -> None:
    if not site.is_dir():
        fail(f"missing built site directory: {site}")
    for path in site.rglob("*"):
        if not path.is_file():
            continue
        for pattern in FORBIDDEN_SITE_PATTERNS:
            if fnmatch.fnmatchcase(path.name, pattern):
                fail(f"forbidden file in built site: {path.relative_to(site)}")


def main() -> None:
    for entry in REQUIRED_ROOT_ENTRIES:
        if not (SITE / entry).is_file():
            fail(f"built site is missing {entry}")
    check_handwritten_pages()
    check_docs_tier()
    check_markdown_twin_links()
    check_llms_txt()
    check_sitemaps()
    check_public_site_file_inventory()
    print("built site checks passed")


if __name__ == "__main__":
    main()

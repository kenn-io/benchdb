#!/usr/bin/env python3
from __future__ import annotations

import importlib
import re
import shutil
import sys
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit, urlunsplit


class DocsSiteError(Exception):
    pass


MARKDOWN_LINK_RE = re.compile(r"(?P<prefix>!?\[[^\]\n]*\]\()(?P<target>[^)\n]+)(?P<suffix>\))")


def load_toml(path: Path) -> dict[str, Any]:
    try:
        toml = importlib.import_module("tomllib")
    except ModuleNotFoundError:
        toml = importlib.import_module("tomli")
    return toml.loads(path.read_text(encoding="utf-8"))


def published_markdown_pages(config: dict[str, Any]) -> list[Path]:
    pages: list[Path] = []

    def collect(value: Any) -> None:
        if isinstance(value, str):
            if value.endswith(".md"):
                pages.append(Path(value))
            return
        if isinstance(value, list):
            for item in value:
                collect(item)
            return
        if isinstance(value, dict):
            for item in value.values():
                collect(item)

    collect(config.get("project", {}).get("nav", []))
    return sorted(set(pages))


def markdown_twin_path(site_root: Path, page: Path) -> Path:
    if page == Path("index.md"):
        return site_root / "docs.md"
    return site_root / "docs" / page


def rendered_page_path(site_root: Path, page: Path) -> Path:
    if page == Path("index.md"):
        return site_root / "docs" / "index.html"
    return site_root / "docs" / page.with_suffix("") / "index.html"


def markdown_twin_url(config: dict[str, Any], page: Path) -> str:
    docs_url = str(config["project"]["site_url"]).rstrip("/")
    if page == Path("index.md"):
        return docs_url.removesuffix("/docs") + "/docs.md"
    return f"{docs_url}/{page.as_posix()}"


def root_markdown_twin(source: str) -> str:
    def rewrite(match: re.Match[str]) -> str:
        target, separator, title = match.group("target").partition(" ")
        parsed = urlsplit(target)
        if parsed.scheme or parsed.netloc or target.startswith(("/", "#")) or not parsed.path.endswith(".md"):
            return match.group(0)

        path = parsed.path.removeprefix("./")
        destination = urlunsplit(("", "", f"/docs/{path}", parsed.query, parsed.fragment))
        rewritten_target = destination if not separator else f"{destination}{separator}{title}"
        return f'{match.group("prefix")}{rewritten_target}{match.group("suffix")}'

    return MARKDOWN_LINK_RE.sub(rewrite, source)


def stage_site_root(
    docs_source: Path,
    website_source: Path,
    llms_source: Path,
    site_root: Path,
    config: dict[str, Any],
) -> None:
    shutil.copytree(website_source, site_root, dirs_exist_ok=True)
    for page in published_markdown_pages(config):
        source = docs_source / page
        if not source.is_file():
            raise DocsSiteError(f"published docs source is missing: {page.as_posix()}")
        twin = markdown_twin_path(site_root, page)
        twin.parent.mkdir(parents=True, exist_ok=True)
        contents = source.read_text(encoding="utf-8")
        if page == Path("index.md"):
            contents = root_markdown_twin(contents)
        twin.write_text(contents, encoding="utf-8")
    shutil.copy2(llms_source, site_root / "llms.txt")


def verify_site_root(site_root: Path, config: dict[str, Any]) -> None:
    missing: list[str] = []
    for static_page in (Path("index.html"), Path("guide/index.html")):
        if not non_empty_file(site_root / static_page):
            missing.append(static_page.as_posix())

    for page in published_markdown_pages(config):
        if not non_empty_file(rendered_page_path(site_root, page)):
            missing.append(f"rendered page for {page.as_posix()}")
        if not non_empty_file(markdown_twin_path(site_root, page)):
            missing.append(f"markdown twin for {page.as_posix()}")

    llms_path = site_root / "llms.txt"
    if not non_empty_file(llms_path):
        missing.append("llms.txt")
    else:
        llms = llms_path.read_text(encoding="utf-8")
        for page in published_markdown_pages(config):
            if markdown_twin_url(config, page) not in llms:
                missing.append(f"llms.txt entry for {page.as_posix()}")

    if missing:
        raise DocsSiteError("site output is missing required entries:\n  " + "\n  ".join(missing))


def non_empty_file(path: Path) -> bool:
    return path.is_file() and path.stat().st_size > 0


def build(repo_root: Path) -> None:
    config = load_toml(repo_root / "zensical.toml")
    site_root = repo_root / "site"
    stage_site_root(
        repo_root / "docs" / "site",
        repo_root / "website",
        repo_root / "docs" / "llms.txt",
        site_root,
        config,
    )
    verify_site_root(site_root, config)
    print(f"tiered documentation site OK ({len(published_markdown_pages(config))} reference pages)")


def main(argv: list[str]) -> int:
    if len(argv) > 2:
        print("usage: build_docs_site.py [repo-root]", file=sys.stderr)
        return 2
    repo_root = Path(argv[1]) if len(argv) == 2 else Path(__file__).resolve().parent.parent
    try:
        build(repo_root)
    except (DocsSiteError, KeyError) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))

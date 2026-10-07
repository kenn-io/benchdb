#!/usr/bin/env python3
from __future__ import annotations

import json
import re
import sys
from pathlib import Path


class PinError(Exception):
    pass


def validate_docs_screenshot_pins(root: Path) -> None:
    read_package_playwright_version(root / "web" / "package.json")


def read_package_playwright_version(package_json: Path) -> str:
    package = json.loads(package_json.read_text(encoding="utf-8"))
    version = package.get("devDependencies", {}).get("@playwright/test")
    if not isinstance(version, str) or not re.fullmatch(r"\d+\.\d+\.\d+", version):
        raise PinError("web/package.json must pin an exact @playwright/test version")
    return version


def main(argv: list[str]) -> int:
    root = Path(argv[1]) if len(argv) == 2 else Path(".")
    try:
        validate_docs_screenshot_pins(root)
    except (PinError, FileNotFoundError, json.JSONDecodeError) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    print("docs screenshot pins OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))

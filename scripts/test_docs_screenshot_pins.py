import json
import tempfile
import unittest
from pathlib import Path

from scripts.docs_screenshot_pins import PinError, validate_docs_screenshot_pins


class DocsScreenshotPinsTest(unittest.TestCase):
    def make_root(
        self,
        package_playwright: str = "1.60.0",
    ) -> Path:
        tmp_handle = tempfile.TemporaryDirectory(prefix="benchdb-docs-screenshot-pins-")
        self.addCleanup(tmp_handle.cleanup)
        root = Path(tmp_handle.name)
        (root / "web").mkdir()
        (root / "web" / "package.json").write_text(
            json.dumps({"devDependencies": {"@playwright/test": package_playwright}}),
            encoding="utf-8",
        )
        return root

    def test_accepts_exact_playwright_pin(self) -> None:
        validate_docs_screenshot_pins(self.make_root())

    def test_rejects_non_exact_package_pin(self) -> None:
        with self.assertRaisesRegex(PinError, "exact @playwright/test version"):
            validate_docs_screenshot_pins(self.make_root(package_playwright="^1.60.0"))

if __name__ == "__main__":
    unittest.main()

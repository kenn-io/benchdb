import struct
import tempfile
import unittest
import zlib
from pathlib import Path

from scripts.build_docs_site import (
    DocsSiteError,
    markdown_twin_path,
    markdown_twin_url,
    published_markdown_pages,
    rendered_page_path,
    stage_site_root,
    verify_site_root,
)


def write_png(path: Path, width: int, height: int) -> None:
    def chunk(kind: bytes, data: bytes) -> bytes:
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))

    rows = b"".join(b"\x00" + b"\x00\x00\x00" * width for _ in range(height))
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(rows))
        + chunk(b"IEND", b"")
    )


class BuildDocsSiteTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory(prefix="benchdb-docs-site-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.docs = self.root / "source"
        self.website = self.root / "website"
        self.site = self.root / "site"
        self.llms = self.root / "llms.txt"
        self.config = {
            "project": {
                "site_url": "https://benchdb.example/docs/",
                "nav": [
                    {"Overview": "index.md"},
                    {"Start": ["quickstart.md", {"Automation": "agents.md"}]},
                ],
            }
        }

        self.docs.mkdir()
        self.website.joinpath("guide").mkdir(parents=True)
        self.website.joinpath("index.html").write_text("<h1>Home</h1>\n", encoding="utf-8")
        self.website.joinpath("guide/index.html").write_text("<h1>Guide</h1>\n", encoding="utf-8")
        self.docs.joinpath("index.md").write_text(
            "# Docs\n\n"
            "[Quickstart](quickstart.md)\n\n"
            "[Automation](agents.md#agent-workflow)\n\n"
            "[Website](https://benchdb.example/)\n",
            encoding="utf-8",
        )
        self.docs.joinpath("quickstart.md").write_text("# Quickstart\n", encoding="utf-8")
        self.docs.joinpath("agents.md").write_text("# Agents\n", encoding="utf-8")
        self.docs.joinpath("private-notes.md").write_text("# Private\n", encoding="utf-8")

        entries = "\n".join(
            f"- [{page}]({markdown_twin_url(self.config, page)})"
            for page in published_markdown_pages(self.config)
        )
        self.llms.write_text(f"# BenchDB\n\n{entries}\n", encoding="utf-8")

        for page in published_markdown_pages(self.config):
            rendered = rendered_page_path(self.site, page)
            rendered.parent.mkdir(parents=True, exist_ok=True)
            rendered.write_text("<html>rendered</html>\n", encoding="utf-8")

    def test_stages_public_nav_pages_and_verifies_complete_site(self) -> None:
        stage_site_root(self.docs, self.website, self.llms, self.site, self.config)
        verify_site_root(self.site, self.config)

        self.assertEqual(markdown_twin_path(self.site, Path("index.md")), self.site / "docs.md")
        root_twin = self.site.joinpath("docs.md").read_text(encoding="utf-8")
        self.assertIn("[Quickstart](/docs/quickstart.md)", root_twin)
        self.assertIn("[Automation](/docs/agents.md#agent-workflow)", root_twin)
        self.assertIn("[Website](https://benchdb.example/)", root_twin)
        self.assertEqual(
            self.site.joinpath("docs/agents.md").read_text(encoding="utf-8"),
            "# Agents\n",
        )
        self.assertFalse(self.site.joinpath("docs/private-notes.md").exists())

    def test_rejects_missing_markdown_twin(self) -> None:
        stage_site_root(self.docs, self.website, self.llms, self.site, self.config)
        self.site.joinpath("docs/quickstart.md").unlink()

        with self.assertRaisesRegex(DocsSiteError, r"markdown twin for quickstart\.md"):
            verify_site_root(self.site, self.config)

    def test_rejects_llms_index_missing_a_published_page(self) -> None:
        self.llms.write_text("# BenchDB\n", encoding="utf-8")
        stage_site_root(self.docs, self.website, self.llms, self.site, self.config)

        with self.assertRaisesRegex(DocsSiteError, r"llms\.txt entry for agents\.md"):
            verify_site_root(self.site, self.config)

    def test_sizes_images_from_the_staged_png(self) -> None:
        write_png(self.website / "assets/shot.png", 7, 3)
        self.website.joinpath("index.html").write_text(
            '<img\n  src="/assets/shot.png"\n  alt="Shot"\n  width="1378"\n  height="565"\n/>\n'
            '<img src="https://example.com/remote.png" alt="Remote" />\n',
            encoding="utf-8",
        )

        stage_site_root(self.docs, self.website, self.llms, self.site, self.config)

        self.assertEqual(
            self.site.joinpath("index.html").read_text(encoding="utf-8"),
            '<img\n  src="/assets/shot.png" width="7" height="3"\n  alt="Shot"\n/>\n'
            '<img src="https://example.com/remote.png" alt="Remote" />\n',
        )

    def test_rejects_image_missing_from_the_site(self) -> None:
        self.website.joinpath("guide/index.html").write_text('<img src="/assets/gone.png" alt="" />\n', encoding="utf-8")

        with self.assertRaisesRegex(DocsSiteError, r"image is missing from the site: /assets/gone\.png"):
            stage_site_root(self.docs, self.website, self.llms, self.site, self.config)


if __name__ == "__main__":
    unittest.main()

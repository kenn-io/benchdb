"""Exercise release metadata against isolated synthetic Git histories."""

import os
import pathlib
import subprocess
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name("release_image.sh").resolve()


class ReleaseImageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = pathlib.Path(self.temp.name)
        self.env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        self.env.update(GIT_CONFIG_GLOBAL=str(self.repo / "global.gitconfig"), GIT_CONFIG_NOSYSTEM="1")
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Release Tests")
        self.git("config", "user.email", "release-tests@example.com")
        self.git("commit", "--allow-empty", "-m", "initial")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo, env=self.env, text=True, stderr=subprocess.DEVNULL).strip()

    def metadata(self, tag):
        return subprocess.run(["bash", str(SCRIPT), tag], cwd=self.repo, env=self.env, text=True, capture_output=True)

    def test_stable_annotated_tag_uses_actual_commit(self):
        initial = self.git("rev-parse", "HEAD")
        self.git("tag", "-a", "v1.2.3", "-m", "release")
        self.git("commit", "--allow-empty", "-m", "next")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        result = self.metadata("v1.2.3")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(dict(line.split("=", 1) for line in result.stdout.splitlines()), {
            "version": "v1.2.3", "revision": initial, "image": "ghcr.io/kenn-io/benchdb",
        })

    def test_unmerged_release_tag_is_rejected(self):
        self.git("checkout", "-b", "feature")
        self.git("commit", "--allow-empty", "-m", "unmerged")
        self.git("tag", "v9.0.0")
        result = self.metadata("v9.0.0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("main", result.stderr)
        self.assertEqual(result.stdout, "")

    def test_missing_tag_is_rejected(self):
        result = self.metadata("v1.2.3")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")

    def test_nonstable_or_malformed_versions_are_rejected(self):
        for tag in ["v01.2.3", "v1.2", "1.2.3", "v1.2.3-rc.1", "v1.2.3+meta", "--help", "v1.2.3\nversion=evil"]:
            with self.subTest(tag=tag):
                result = self.metadata(tag)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("vMAJOR.MINOR.PATCH", result.stderr)
                self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()

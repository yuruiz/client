import importlib.util
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("release", Path(__file__).with_name("release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class SourceReleaseTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.repo = Path(self.temporary.name) / "repo"
        self.repo.mkdir()
        subprocess.run(["git", "init", "--quiet", str(self.repo)], check=True)
        for name, contents in {
            "LICENSE": "test license\n",
            "go/go.mod": "module example.test/helper\n\ngo 1.26.0\n",
            "go/go.sum": "",
            "go/kbfs/kbfsgit/git-remote-keybase-standalone/main.go": "package main\n",
            ".gitignore": "go/ignored-secret\n",
        }.items():
            target = self.repo / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(contents)
        self.git("add", ".")
        self.git("-c", "user.name=Release Test", "-c", "user.email=release-test@localhost",
                 "commit", "--quiet", "-m", "fixture")

    def git(self, *args):
        return release.git(self.repo, *args)

    def test_snapshot_contains_changes_and_is_reproducible(self):
        (self.repo / "go/new.go").write_text("package main\n// newly added\n")
        (self.repo / "go/go.mod").write_text("module example.test/modified\n\ngo 1.26.0\n")
        (self.repo / "go/ignored-secret").write_text("not a real secret")
        first = Path(self.temporary.name) / "first.tar.gz"
        second = Path(self.temporary.name) / "second.tar.gz"
        digest = release.source_archive(self.repo, first, "0.1.0")
        self.assertEqual(digest, release.source_archive(self.repo, second, "0.1.0"))
        with tarfile.open(first) as archive:
            prefix = "git-remote-keybase-standalone-0.1.0/"
            self.assertIn(prefix + "go/new.go", archive.getnames())
            self.assertNotIn(prefix + "go/ignored-secret", archive.getnames())
            self.assertIn(b"modified", archive.extractfile(prefix + "go/go.mod").read())
            self.assertIn(b'"working_tree_changes": true', archive.extractfile(prefix + "SOURCE.json").read())
        # Re-running the same release is harmless, but changing its source is not.
        self.assertEqual(digest, release.source_archive(self.repo, first, "0.1.0"))
        (self.repo / "go/new.go").write_text("package other\n")
        with self.assertRaisesRegex(ValueError, "different source snapshot"):
            release.source_archive(self.repo, first, "0.1.0")

    def test_link_outside_source_is_rejected(self):
        outside = Path(self.temporary.name) / "outside"
        outside.write_text("outside")
        (self.repo / "go/link").symlink_to("../../outside")
        with self.assertRaisesRegex(ValueError, "leaves the repository"):
            release.source_files(self.repo)

    def test_formula_values_do_not_allow_ruby_interpolation(self):
        rendered = release.render_formula("url @SOURCE_URL@\nversion @VERSION@\nsha256 @SHA256@",
                                          version="0.1.0", url="https://example.test/#{unexpected}.tar.gz",
                                          digest="a" * 64, homepage="https://example.test")
        self.assertIn(r"\#{unexpected}", rendered)
        with self.assertRaises(ValueError):
            release.render_formula("", version="0.1.0", url="http://example.test/source.tar.gz",
                                   digest="a" * 64, homepage="https://example.test")


if __name__ == "__main__":
    unittest.main()

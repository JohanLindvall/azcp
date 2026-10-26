# SPDX-License-Identifier: MIT

import contextlib
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from release_version import main, next_tag, version


class ReleaseVersionTests(unittest.TestCase):
    def test_patch_increments_and_reruns_reuse_the_tag(self):
        history = ["new", "base"]
        tags = {"v0.6.7": "base"}
        self.assertEqual(next_tag(history, tags), "v0.6.8")
        tags["v0.6.8"] = "new"
        self.assertEqual(next_tag(history, tags), "v0.6.8")
        tags["v0.7.0"] = "base"
        self.assertEqual(next_tag(history, tags), "v0.6.8")

    def test_out_of_order_workflows_keep_commit_order(self):
        tags = {"v0.6.7": "base"}
        self.assertEqual(next_tag(["newer", "older", "base"], tags), "v0.6.9")
        tags["v0.6.9"] = "newer"
        self.assertEqual(next_tag(["older", "base"], tags), "v0.6.8")

    def test_new_minor_release_and_non_release_tags(self):
        tags = {"v0.6.7": "base", "v0.7.0": "minor", "v99.0.0-rc.1": "new"}
        self.assertEqual(next_tag(["new", "minor", "base"], tags), "v0.7.1")
        self.assertEqual(next_tag(["new", "base"], {}), "v0.0.2")

    def test_collision_never_moves_a_tag(self):
        with self.assertRaisesRegex(ValueError, "already names another commit"):
            next_tag(["new", "base"], {"v0.6.7": "base", "v0.6.8": "unrelated"})

    def test_latest_does_not_regress_when_an_older_build_finishes_last(self):
        for candidate, previous, expected in [
            ("v0.6.9", "v0.6.8", "true"), ("v0.6.8", "v0.6.9", "false"),
            ("v0.6.9", "v0.6.9", "true"), ("v0.6.9", "", "true"),
            ("v0.10.0", "v0.9.99", "true"),
        ]:
            with contextlib.redirect_stdout(io.StringIO()) as output:
                main(["latest", candidate, previous])
            self.assertEqual(output.getvalue().strip(), expected)
        for tag in ["main", "v1.2.3-rc.1", "v01.2.3", "v1.2.3\ninjected=value"]:
            self.assertIsNone(version(tag))

    def test_real_git_history_includes_annotated_tags_and_counts_merges_once(self):
        script = Path(__file__).with_name("release_version.py").resolve()
        with tempfile.TemporaryDirectory() as repo:
            env = {**os.environ, "GIT_AUTHOR_NAME": "test", "GIT_COMMITTER_NAME": "test",
                   "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_EMAIL": "test@example.invalid"}

            def git(*args):
                return subprocess.check_output(["git", *args], cwd=repo, env=env, text=True, stderr=subprocess.DEVNULL).strip()

            git("init", "-b", "main")
            git("commit", "--allow-empty", "-m", "base")
            git("tag", "-a", "v0.6.7", "-m", "release")
            git("checkout", "-b", "feature")
            for number in range(3):
                git("commit", "--allow-empty", "-m", f"feature {number}")
            git("checkout", "main")
            git("merge", "--no-ff", "feature", "-m", "merge")
            result = subprocess.check_output(["python3", str(script), "next", "HEAD"], cwd=repo, env=env, text=True)
            self.assertEqual(result.strip(), "v0.6.8")


if __name__ == "__main__":
    unittest.main()

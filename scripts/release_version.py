# SPDX-License-Identifier: MIT

"""Assign stable release versions in main's commit order, independent of CI timing."""

import re
import subprocess
import sys


def version(tag):
    match = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag)
    return tuple(map(int, match.groups())) if match else None


def next_tag(history, tags):
    # First-parent distance counts a merged PR once. The same commit gets the
    # same version even if a newer commit's CI has already tagged its release.
    existing = [tag for tag, commit in tags.items() if commit == history[0] and version(tag) is not None]
    if existing:
        return max(existing, key=version)
    positions = {commit: distance for distance, commit in enumerate(history)}
    candidates = [(version(tag), positions[commit]) for tag, commit in tags.items()
                  if version(tag) is not None and commit in positions]
    if candidates:
        (major, minor, patch), distance = max(candidates)
    else:
        major, minor, patch, distance = 0, 0, 0, len(history)
    tag = f"v{major}.{minor}.{patch + distance}"
    if tag in tags and tags[tag] != history[0]:
        raise ValueError(f"{tag} already names another commit; choose a new minor release")
    return tag


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def main(args):
    if len(args) == 2 and args[0] == "next":
        history = git("rev-list", "--first-parent", args[1]).splitlines()
        tags = {}
        for tag in git("tag", "--list", "v*").splitlines():
            if version(tag) is not None:
                tags[tag] = git("rev-parse", f"{tag}^{{commit}}")
        print(next_tag(history, tags))
    elif len(args) == 3 and args[0] == "latest":
        candidate, previous = version(args[1]), version(args[2])
        if candidate is None:
            raise ValueError("release tags must be stable vMAJOR.MINOR.PATCH versions")
        print("true" if previous is None or candidate >= previous else "false")
    else:
        raise ValueError("usage: release_version.py next COMMIT | latest VERSION PREVIOUS")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (ValueError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))

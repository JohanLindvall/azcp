#!/usr/bin/env python3
"""Compare local copy results with the compatibility reference, GNU cp 9.4.

Run after make build, or set AZCP to a binary. All operations use temporary
trees. Compares exit status, bytes, modes, preserved mtimes, symlink targets
and hard-link relationships. Stderr wording and access times are not compared.
Requires Python 3 and Unix symlinks; no third-party modules are needed.
"""

import hashlib
import itertools
import os
from pathlib import Path
import stat
import subprocess
import tempfile


FLAGS = [
    [],
    ["-f"],
    ["-n"],
    ["-i"],
    ["-p"],
    ["--preserve=links"],
    ["-a"],
    ["-P"],
    ["-L"],
    ["-l"],
    ["-lf"],
    ["-li"],
    ["-lP"],
    ["--attributes-only"],
    ["--attributes-only", "-P"],
    ["--attributes-only", "-fP"],
    ["-b"],
    ["--backup=numbered"],
    ["--no-preserve=mode"],
    ["-p", "--no-preserve=mode"],
    ["--remove-destination"],
    ["--sparse=always"],
    ["--reflink=never"],
    ["-u"],
    ["-uP"],
    ["-ub"],
]

CASES = [
    "new",
    "sourcealias",
    "samename",
    "existing",
    "destlink",
    "danglingdest",
    "sourcelink",
    "danglingsource",
    "newtree",
    "existingtree",
    "destaliases",
    "dirsymlink",
    "read_after_write",
    "write_after_read",
    "update_link_old",
    "update_link_new",
]

OLD, ORIGINAL, NEW = 1600000000, 1700000000, 1900000000
FIXED_TIMES = {value * 10**9 for value in (OLD, ORIGINAL, NEW)}


def setup(root, case):
    (root / "src").mkdir()
    (root / "src/a").write_text("alpha\n")
    (root / "src/b").write_text("beta longer\n")
    (root / "src/sub").mkdir()
    (root / "src/sub/c").write_text("gamma")
    (root / "src/link").symlink_to("a")
    (root / "src/dangling").symlink_to("missing")
    os.link(root / "src/a", root / "src/alias")
    (root / "src/a").chmod(0o640)
    (root / "src/sub").chmod(0o750)
    args = ["src/a", "dst"]

    if case == "sourcealias":
        os.link(root / "src/a", root / "dst")
    elif case == "samename":
        args = ["src/a", "src/a"]
    elif case == "existing":
        (root / "dst").write_text("old")
        (root / "dst").chmod(0o600)
    elif case == "destlink" or case.startswith("update_link_"):
        (root / "other").write_text("old")
        (root / "dst").symlink_to("other")
    elif case == "danglingdest":
        (root / "dst").symlink_to("missing")
    elif case == "sourcelink":
        args = ["src/link", "dst"]
        (root / "dst").write_text("old")
    elif case == "danglingsource":
        args = ["src/dangling", "dst"]
    elif case == "newtree":
        args = ["-r", "src", "dst"]
    elif case == "existingtree":
        args = ["-rT", "src", "dst"]
        (root / "dst").mkdir()
        (root / "dst/a").write_text("old")
        (root / "dst").chmod(0o700)
    elif case == "destaliases":
        (root / "dst").mkdir()
        (root / "dst/a").write_text("old")
        os.link(root / "dst/a", root / "dst/b")
        args = ["src/a", "src/b", "dst"]
    elif case == "dirsymlink":
        (root / "source-dir").symlink_to("src")
        args = ["-r", "source-dir", "dst"]
    elif case in ("read_after_write", "write_after_read"):
        (root / "src/a").write_bytes(b"a" * 1048576)
        (root / "dst").mkdir()
        source, dest = ("b", "a") if case == "read_after_write" else ("a", "b")
        os.link(root / "src" / source, root / "dst" / dest)
        args = ["src/a", "src/b", "dst"]

    for path in root.rglob("*"):
        os.utime(path, (ORIGINAL, ORIGINAL), follow_symlinks=False)
    if case.startswith("update_link_"):
        when = OLD if case.endswith("old") else NEW
        os.utime(root / "other", (when, when))
        os.utime(root / "dst", (NEW, NEW), follow_symlinks=False)
    return args


def snapshot(root):
    rows, inodes = [], {}
    for path in sorted(root.rglob("*")):
        info = path.lstat()
        name = str(path.relative_to(root))
        mode = stat.S_IMODE(info.st_mode)
        # Independent invocations create files at different instants. Keep
        # fixed fixture times exact, but normalize newly assigned timestamps.
        mtime = info.st_mtime_ns if info.st_mtime_ns in FIXED_TIMES else "changed"
        if path.is_symlink():
            detail = ("link", os.readlink(path))
        elif path.is_dir():
            detail = ("dir",)
        else:
            detail = ("file", hashlib.sha256(path.read_bytes()).hexdigest())
        if not stat.S_ISDIR(info.st_mode):
            inodes.setdefault((info.st_dev, info.st_ino), []).append(name)
        rows.append((name, mode, mtime, detail))
    links = sorted(tuple(names) for names in inodes.values() if len(names) > 1)
    return rows, links


def outcome(command, flags, case, posix):
    with tempfile.TemporaryDirectory(prefix="azcp-cp-compat-") as directory:
        root = Path(directory)
        args = setup(root, case)
        env = dict(os.environ, LC_ALL="C", AZCP_NO_BROWSER="1")
        env.pop("POSIXLY_CORRECT", None)
        if posix:
            env["POSIXLY_CORRECT"] = "1"
        result = subprocess.run(
            [command, *flags, *args],
            cwd=root,
            input=b"y\n" * 20,
            capture_output=True,
            env=env,
            timeout=30,
        )
        return (
            result.returncode,
            snapshot(root),
            result.stderr.decode(errors="replace").replace(directory, "ROOT"),
        )


def main():
    version = subprocess.run(["cp", "--version"], capture_output=True, text=True)
    if "(GNU coreutils) 9.4\n" not in version.stdout:
        raise SystemExit("This check requires GNU cp 9.4, the compatibility reference.")
    azcp = str(Path(os.environ.get("AZCP", "bin/azcp")).resolve())
    cases = [(case, flags, False) for case, flags in itertools.product(CASES, FLAGS)]
    cases += [
        ("danglingdest", flags, True)
        for flags in (["-n"], ["--update=none"], ["-u"], ["-i"])
    ]
    mismatches = []
    for case, flags, posix in cases:
        expected = outcome("cp", flags, case, posix)
        actual = outcome(azcp, flags, case, posix)
        if expected[:2] != actual[:2]:
            mismatches.append((case, flags, posix, expected, actual))

    print(f"{len(cases)} cases, {len(mismatches)} mismatches")
    for case, flags, posix, expected, actual in mismatches:
        print(case, flags, f"POSIXLY_CORRECT={posix}")
        print("statuses:", expected[0], actual[0])
        print("cp stderr:", expected[2].strip())
        print("azcp stderr:", actual[2].strip())
        cp_rows, azcp_rows = expected[1][0], actual[1][0]
        print("only cp:", [row for row in cp_rows if row not in azcp_rows])
        print("only azcp:", [row for row in azcp_rows if row not in cp_rows])
        print("links:", expected[1][1], actual[1][1])
    return bool(mismatches)


if __name__ == "__main__":
    raise SystemExit(main())

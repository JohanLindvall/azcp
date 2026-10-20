# Contributing to azcp

Thanks for helping make copying files easier. Bug reports, clearer examples,
platform testing and small fixes are all useful contributions. You do not need
an Azure account to build the project or run the regular tests.

## Start here

- Found a problem? [Report a bug](https://github.com/JohanLindvall/azcp/issues/new?template=bug_report.yml)
  with a small example and the output of `azcp --version`.
- Have an idea? [Describe the task you want to make easier](https://github.com/JohanLindvall/azcp/issues/new?template=feature_request.yml).
- New to the code? Try a documentation improvement, a regression test for a
  reproducible bug, or a platform check. [Open issues](https://github.com/JohanLindvall/azcp/issues)
  show work already being discussed.
- Found a security issue? Use the private reporting route in
  [SECURITY.md](SECURITY.md).

Keep discussion welcoming and specific. Explain the behavior you observed,
ask when something is unclear, and review the work with respect for the person
who submitted it.

## Development setup

Install Go 1.26.0 or newer and Git. The Makefile commands also need Make and a
POSIX shell. Docker is needed only for the Azurite end-to-end suite.

Fork the repository for your pull request, then clone your fork. To explore
the project without a fork:

```sh
git clone https://github.com/JohanLindvall/azcp.git
cd azcp
make build
./bin/azcp --help
make test
```

On Windows without Make, `go build -o bin/azcp.exe ./cmd/azcp` and
`go test ./...` work directly. Run native tests when changing Windows-specific
behavior.

## Choose the checks that fit the change

| Change | Checks |
| --- | --- |
| Documentation | Check relative links and anchors; run changed examples with temporary files or Azurite. |
| Go code | `make lint` and `make test`; add a regression test for behavior you change. |
| Progress, concurrency or shared state | `make check`, including the race detector. |
| Blob transfer behavior | `make e2e` as well as focused tests. |
| Platform-specific behavior | `make cross` and native tests on that platform. |
| GNU `cp` behavior | Check GNU coreutils 9.4 and run `python3 scripts/cp-compat.py` after building. |

Once dependencies are downloaded, the regular tests need no credentials or
external services. HTTP fixtures use loopback sockets. `make e2e` starts and
stops a local Azurite container. To avoid colliding with another emulator:

```sh
make e2e AZURITE_NAME=azcp-contributor-test AZURITE_PORT=10073
```

The compatibility script requires Python 3, Unix symlinks and GNU `cp` 9.4.
It compares exit status, bytes, modes, timestamps and link relationships;
it does not compare stderr wording or access times.

CI tests Linux and Windows natively on x86-64 and ARM64. It cross-compiles
macOS too, but that is not a substitute for native macOS tests. macOS release
binaries are built on macOS so that the token cache can use the keychain.
If you cannot run a relevant platform or service check, say so in the PR.

## What a useful pull request contains

Keep the change focused on one problem. Describe what the user can do
afterwards and include the commands you used to validate it. For a bug fix,
a small reproduction is more useful than a long description. Documentation
fixes do not need a new test suite.

`azcp` follows GNU `cp` 9.4. Options, operands, exit statuses and diagnostics
are part of that contract. Check the real `cp` before changing its behavior,
and document intentional differences in the
[user guide](docs/guide.md#differences-from-cp).

For larger features, open an issue first so the scope can be discussed before
you invest in an implementation. The project focuses on local files and Azure
Blob Storage; additional backends, a separate sync command and durable job
plans need a clear reason to expand that scope.

Use `gofmt`, keep new dependencies justified, and include an
`SPDX-License-Identifier: MIT` comment in new source files. Read
[AGENTS.md](https://github.com/JohanLindvall/azcp/blob/main/AGENTS.md)
for the copy, authentication, concurrency and terminal invariants.

## Finding your way around

| Directory | Responsibility |
| --- | --- |
| `cmd/azcp` | Entry point, signals and exit status |
| `internal/cli`, `internal/cpflags` | Options, help and argument parsing |
| `internal/engine` | Planning, overwrite decisions and copy semantics |
| `internal/store/local`, `internal/store/azure` | Filesystem and blob operations |
| `internal/glob` | Brace expansion and wildcard matching |
| `internal/codec`, `internal/parallel` | Compression and bounded workers |
| `internal/progress`, `internal/logx` | Terminal display, logging and redaction |
| `internal/retryx`, `internal/uri`, `internal/humanize` | Retries, locations and formatting |
| `scripts` | Installer, compatibility checks and emulator tests |
| `docs` | User guides, comparisons and audit records |

The main flow is `cli.Parse` → `engine.Run` → scanner → transfer workers.
The scanner makes overwrite decisions in source order; workers move the data.

## Reporting problems and performance results

Include your OS and architecture, the transfer direction, a minimal command,
and expected versus actual behavior. Remove SAS signatures, account keys,
bearer tokens and private data from anything you share.

For performance results, also include both tool versions, file counts and
sizes, transfer options, endpoint type, elapsed times, and repeated runs.
Separate emulator request counts from cloud throughput. The
[comparison guide](docs/comparison.md#measure-your-own-workload) explains how
to make measurements useful.

# azcp documentation

Copy files between your computer and Azure Blob Storage with familiar `cp`
commands.

| I want to… | Start here |
| --- | --- |
| Install on Linux, macOS or Windows | [Installation](installation.md) |
| Make my first copy | [Quick start](quickstart.md) |
| Browse, copy, move or delete in two panes | [File manager and Azure directory trees](tui.md) |
| Fix a first-run problem | [Troubleshooting](quickstart.md#if-something-goes-wrong) |
| Try Azure transfers without an account | [Local emulator walkthrough](quickstart.md#try-blob-storage-without-an-azure-account) |
| Find a command for a common task | [Everyday examples](../README.md#everyday-examples) |
| Sign in or use a SAS token | [Authentication](guide.md#signing-in) |
| Choose files with wildcards and filters | [Wildcards](guide.md#wildcards) · [Filters](guide.md#choosing-what-to-copy) |
| Resume an interrupted transfer | [Resume](guide.md#interrupted-transfers) |
| Preserve attributes or compress files | [Archiving](guide.md#archiving-a-tree) · [Compression](guide.md#compressing-on-the-way) |
| Use azcp in a script | [JSON output](guide.md#machine-readable-output) · [Logging](guide.md#logging) |
| Tune or measure transfers | [Concurrency](guide.md#transfers) · [Benchmark](guide.md#measuring-the-link) |
| Compare azcp with AzCopy | [Tradeoffs and measurements](comparison.md) · [Migration guide](azcopy.md) |
| Check compatibility or find an option | [Differences from cp](guide.md#differences-from-cp) · [Options](guide.md#options) |
| Report a problem or contribute | [Contributing](../CONTRIBUTING.md) · [Security](../SECURITY.md) |

The [user guide](guide.md) contains the full reference. `azcp --help` lists
the options supported by your installed version.

## Project records

- [September 2026 service audit](audit-2026-09-21.md)
- [October 2026 repository audit](audit-2026-10-03.md)
- [Release history](https://github.com/JohanLindvall/azcp/releases)
- [Citation metadata](../CITATION.cff)

Audit results describe the code and checks at the time of each review. Their
validation sections distinguish local tests, emulator tests and native
platform coverage.

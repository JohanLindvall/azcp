# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

`azcp` is a drop-in replacement for GNU `cp` that can also copy to and from
Azure Blob Storage. Compatibility with `cp` is a hard requirement, not an
aspiration: the option table, the operand shapes, the exit statuses and the
wording of messages on stderr are all part of the contract, because scripts
depend on them.

Reference behaviour is GNU coreutils 9.4. When changing anything `cp` also
does, check the real thing first (`cp --help`, or run it on a scratch tree)
rather than reasoning from memory.

## Commands

```
make build     # ./bin/azcp
make test      # go test ./...
make race      # go test -race ./...
make lint      # gofmt -w, go vet, go test
```

For manual end-to-end work against blob storage, the Azurite emulator is
enough and needs no account:

```
docker run -d --rm --name azurite -p 10000:10000 \
  mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0

export AZURE_STORAGE_ACCOUNT=devstoreaccount1
export AZURE_STORAGE_KEY='Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=='

./bin/azcp --create-container -r ./somedir \
  http://127.0.0.1:10000/devstoreaccount1/data/somedir
```

Azurite does not implement `Put Blob From URL` or `Put Block From URL`, so
blob-to-blob copies there take the asynchronous `Copy Blob` route. That is a
gap in the emulator, not in this tool — do not "fix" it by removing the
preferred routes.

## Layout

```
cmd/azcp             entry point, signal handling, exit status
internal/cli         option table, help text, resolved configuration
internal/cpflags     getopt_long-compatible parser
internal/engine      planning, the worker pool, cp's per-file semantics
internal/glob        brace expansion and the pattern matcher
internal/store       namespace interface and the pattern-driven walker
internal/store/local filesystem, reflink, sparse copies, attributes
internal/store/azure blob storage, credentials, transfers
internal/progress    the live terminal display
internal/logx        logging, terminal arbitration, secret redaction
internal/retryx      transient-failure classification and backoff
internal/humanize    sizes, rates, durations, eliding
internal/uri         location parsing
```

Flow: `cli.Parse` → `engine.Run` → one scanner goroutine walks the sources and
feeds a channel → `--jobs` workers transfer files → `progress` draws.

## Invariants worth knowing before editing

**The terminal has one owner.** While the progress display is running it owns
stderr. Every write goes through `logx.WithTerminal`, which the display hooks
via `logx.SetGuard` so it can erase its live region first. Writing straight to
`os.Stdout`/`os.Stderr` from anywhere else tears the bars. The one deliberate
exception is the device-code sign-in prompt in `store/azure/auth.go`, which
must appear whatever the log level.

**Secrets are redacted at the output chokepoint,** in `logx`. SAS signatures,
account keys and bearer tokens are stripped from everything this package
writes, so an SDK error quoting a signed URL cannot leak. Do not add a code
path that bypasses `logx` to print an error, and do not echo a raw command-line
argument — use `uri.URL.Display()`, which renders a SAS as `?<sas>`.

**Problems are reported once.** `engine.fail` and `engine.note` print one
`cp`-style line and drop their structured record to debug level when logs share
stderr with that line (`logx.SharesTerminal`). Adding a `log.Error` beside a
user-facing message prints the same problem twice.

**Scanning is single-goroutine on purpose.** It preserves `cp`'s ordering,
creates directories before their contents, and keeps `-i` prompts serial. The
overwrite decisions (`-n`, `-i`, `-u`, `-b`) live there, not in the workers.
They are guarded by `needsDestCheck()`, which skips the destination stat
entirely when no option depends on it — that is the difference between a fast
and a slow upload of many small files.

**There are two retry layers and they must not multiply.** The SDK pipeline
retries each HTTP request `--retries` times; `Store.shouldRetry` is where that
decision is made and logged. `retryx` sits above it for whole-file restarts and
is capped at 3 attempts. Raising either without thinking about the other turns
a 6× budget into 36 requests.

**Progress byte accounting.** `Task.Set` takes an absolute total, because the
Azure SDK reports cumulative bytes and can report *less* after retrying a
block. `Task.Add` takes a delta and is for local copies. Mixing them
double-counts.

**Blob storage has no directories.** `store/azure` synthesises them: a prefix
with children behaves as a directory, `WalkAll` emits ancestor prefixes so `**`
sees a tree, and an empty directory is the zero-byte `name/` marker blob.

**`store.Store` is the naming half only.** Bulk data is dispatched concretely
by scheme pair in `engine/copy.go`, because each pairing has its own fast
route. Do not push transfers behind the interface; that would cost the parallel
block upload, the parallel ranged download and the server-side copy.

**Measuring styled text.** Terminal layout arithmetic must use
`progress.stripANSI`. A CSI sequence is `ESC [` … final byte, and `[` is itself
inside the final-byte range — a naive scanner terminates early and mis-measures
every coloured string.

## Testing

`internal/glob` is differential-tested against real `bash` run with `globstar`
and `extglob` (`TestMatchesBash`); it skips where bash is absent. Any change to
matching semantics must keep it passing — bash is the specification.

`internal/engine` drives whole invocations against temporary directories, which
is the closest thing to an end-to-end test that needs no credentials. Add cases
there when changing `cp` semantics.

Run `make race` before proposing changes to the progress display, the worker
pool or anything sharing state between them.

## Conventions

- Comments explain why, not what. If a line needs a comment to say what it
  does, rewrite the line.
- Error messages the user sees follow `cp`'s phrasing and quoting
  (`azcp: cannot stat 'x': No such file or directory`). The location is named
  by the engine, so store-level errors return the cause unadorned.
- New options: extensions get long names; short forms are reserved for `cp`'s
  own, with `-j` the sole exception since `cp` does not define it.
- Anything accepted but not implemented must say so at warn level. Silently
  doing less than asked is worse than refusing.

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

## CI and releases

`ci.yml` runs the tests natively on all six platforms a release covers, because
the copy semantics are full of things that differ per operating system —
symbolic links, file modes, device identity, path separators — and
cross-compiling proves none of it. Formatting, vet, the race detector, the
cross-compile check and the emulator-backed end-to-end suite run once on Linux.

`release.yml` fires on a `v*` tag. It re-runs the tests rather than trusting
that main was green, then builds each platform on its own runner. That last
part is not incidental: macOS reaches its keychain through cgo, so a macOS
binary cross-compiled from Linux silently loses the ability to remember a
sign-in. Linux and Windows are built with `CGO_ENABLED=0` for static binaries.

Version comes from the tag via `-X internal/cli.Version`. A binary built
without that stamp falls back to the module version in its build info, so a
`go install module@v1.2.3` still reports the right thing.

### Container image

`packages.yml` publishes `ghcr.io/johanlindvall/azcp` for amd64 and arm64 on
every tag, and `:edge` from main. The Dockerfile cross-compiles from
`$BUILDPLATFORM` rather than emulating the target, so the arm64 image costs no
more to build than the amd64 one.

The final stage is `FROM scratch` with exactly two things in it: the static
binary and the CA roots it needs to verify a TLS connection to storage. There
is no shell, no libc and no package manager, so there is nothing in the image to
keep patched. If you add a runtime dependency, that property is what you are
spending.

### Dependencies

Six direct ones, and each earns its place. `klauspost/compress` replaces the
standard library's gzip, flate and zlib decoders — measured at 3.1 GB/s against
2.2 GB/s on the benchmark in `decompress_test.go`, over a whole file in one pass
— and brings zstd, which the standard library has no answer for. It pulls in
nothing else.

Dependabot watches the Go modules weekly. The Azure SDK and `golang.org/x`
families are grouped, because those modules move together and separate pull
requests would each conflict with the others in `go.sum`. GitHub Actions and the
Docker base image are not watched; add them to `.github/dependabot.yml` if that
changes.

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

## Where this sits against AzCopy

AzCopy is the tool people already have. `azcp` earns its place by being `cp`,
by having a real pattern language on both sides, and by issuing fewer requests:
no HEAD before every upload, and one flat listing instead of one per directory.
It deliberately does not have resumable job plans, `sync`, or back ends other
than blob storage — see the README for why. Do not add those without a reason
that outweighs the command line staying `cp`'s.

The numbers in the README were measured, not estimated: `scripts/e2e.sh` covers
correctness, and request counts came from the emulator's access log. Re-measure
rather than reason about it if you change a transfer path.

## Invariants worth knowing before editing

**The terminal has one owner.** While the progress display is running it owns
stderr. Every write goes through `logx.WithTerminal`, which the display hooks
via `logx.SetGuard` so it can erase its live region first. Writing straight to
`os.Stdout`/`os.Stderr` from anywhere else tears the bars. The one deliberate
exception is the device-code sign-in prompt in `store/azure/auth.go`, which
must appear whatever the log level.

**The display has two locks, and the distinction matters.** `Reporter.mu`
guards the mutable state and is held only long enough to read or update it;
`Reporter.paint` guards the screen and is held across the write. Workers take
only `mu`, via `Begin` and `Done`, so a slow terminal can never stall a
transfer. Holding one lock across both — as an earlier version did — makes
every file start and finish wait on terminal I/O.

**The live region is erased, never abandoned.** `render` returns to the top of
the region and erases to the end of the screen with `ESC[J`, which removes it
however many rows a resize has since made of it. Lines inside the region are
separated by real newlines, so a terminal that re-wraps on resize can only make
it taller — it splits a line that no longer fits and never rejoins one broken
deliberately — which is why moving up `drawn-1` rows always lands inside the
region rather than above it, where the scrollback is. Forgetting `drawn` on a
resize instead, which is the obvious alternative, leaves a copy of the whole
display on screen every time the window changes size.

**Frames are written in runs, not per character.** Colouring each cell
individually costs an escape sequence per character: about 1.7 kB for one bar,
repainted every frame. `gradientBar` quantises into `gradientBands` colour
steps so a band shares one escape. Keep any new bar or meter to the same rule.

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

**Discovery cannot validate a credential; only the service can.** The chain in
`store/azure/auth.go` finds *a* credential, which the account may still refuse —
an `az login` for the wrong tenant is the common case. `store/azure/signin.go`
turns that rejection into an interactive sign-in and one retry, at most once per
run and only when stderr is a terminal. A 403 with an identity in hand is a
missing role and is *not* escalated, because signing in as the same person again
changes nothing.

**Ask for a token the way the SDK asks for one.** The storage pipeline sets
`EnableCAE` on every token request it makes, and azidentity answers CAE and
non-CAE requests from *separate* MSAL clients with separate caches. A sign-in
that primes the other one leaves the SDK's first request with nothing cached,
and MSAL deals with that by opening a second browser window — one sign-in
completed, another demanded immediately. `tokenRequest()` in `store/azure/auth.go`
is the single place that says how to ask; `authenticate`, `probeWithin` and
`Token` all go through it, and they have to keep agreeing or a resumed sign-in
validates against one cache and the transfer draws on the other.

**A sign-in must survive the process.** Tokens go into the platform's secure
store via `azidentity/cache`, and a non-secret `AuthenticationRecord` under the
user's config directory names the account to look for. Without both, every
command opens a browser again — which is the whole failure this was built to
avoid. `resume` reconstructs the session and is wired to *never* prompt
(`DisableAutomaticAuthentication` plus a prompt that refuses), so it is safe to
call anywhere.

**The account names its own tenant, and that beats guessing.** A 401 from blob
storage carries `WWW-Authenticate: Bearer authorization_uri=.../TENANT/...`,
which is the one thing discovery cannot work out for itself. `tenant.go` reads
it and `refreshAuth` follows it *before* anyone is asked to sign in: the
identity already found is re-asked for a token in that tenant, and any sign-in
that does follow is directed there too. The SDK parses the same header and then
discards the tenant — a standing TODO in azblob's challenge policy — so do not
assume it is handled underneath. This happens once per run, and never when
`--tenant` has already named one. A refusal to *issue* that token arrives as an
unexported azidentity type, which is why `tenantCredential` records it as it
happens instead of the error being recognised by matching on its type.

**One prompt per run, and the tests say so.** `Credentials.escalated`,
`Store.signIn.done` and the counter behind `Credentials.Prompts` all exist to
pin that. `signin_test.go` fires twenty concurrent rejections and asserts a
single sign-in; keep it passing. Resuming a sign-in saved by an earlier run is
not a prompt and must not spend it — an account that refuses that identity too
still has somebody to ask, which is what `Store.signIn.resumed` keeps track of.
Which callers retry is decided by `Store.authGen`: an operation that failed
under an older credential is worth repeating, one that already ran with the
current one is not.

**Prompts are not log records.** The device code, the "opening a browser" notice
and the "account rejected the credential" line go through `logx.Errf` so they
appear whatever `--log-level` says. Anything the user must act on belongs there,
not in the logger.

**A recursive remote copy takes one flat listing, not one per prefix.**
`planRemoteTree` exists because descending prefix by prefix costs a round trip
per directory and delays the first transfer until the last directory has been
listed. It also has to synthesise what the walk gives it: destination
directories created once each, and marker blobs for the directories that turn
out to be empty.

**The SDK single-shots anything up to 256 MiB.** `blockblob.UploadFile` ignores
the block size it is given and sends a file of 256 MiB or less in one request,
which is one unparallelised stream that restarts from nothing if it fails.
`uploadBlocks` in `store/azure/transfer.go` exists for that reason: anything
filling more than one block is staged and committed here instead. Do not
"simplify" it back to UploadFile.

**Connection pool size is a performance feature.** See `store/azure/transport.go`.
The SDK's default of ten idle connections per host means a run with sixty-four
jobs re-establishes a connection for most requests. It costs nothing against a
local emulator, which is exactly why it went unnoticed.

**Attributes ride in blob metadata.** `store/posixmeta.go` defines the keys and
the encoding; `engine/attrs.go` writes them on upload and restores them on
download. The order in `restoreAttrs` is not arbitrary: a successful chown
clears the setuid and setgid bits, so ownership has to be applied before the
mode or those bits are silently lost. The local path in `local.ApplyAttrs`
already had this right; the blob path did not, and a test caught it.

**Transparent decompression must stay off.** `transport.go` sets
`DisableCompression`. Left on, Go advertises gzip and silently expands any
encoded response — so a blob stored with `Content-Encoding: gzip` would arrive
expanded, under its `.gz` name, and shorter than the length the service
reported. For a copier that is data corruption, not a convenience.

**Ranges are fetched by hand, not by `DownloadFile`.** Two reasons, in
`store/azure/download.go`: the SDK dereferences a `Content-Length` the service
does not send for an encoded blob and panics, and resuming needs to know which
ranges landed, which only the code issuing them can know.

**Cancelling is not failing.** Ctrl-C ends every transfer in flight at the same
moment, and the error each one comes back with cannot be trusted to say so: Go
cancels a signal context with a cause of its own ("interrupt signal received"),
and what returns through the SDK is whatever it was given. `engine.interrupted`
asks the context instead. A transfer stopped that way goes to
`progress.Task.Interrupted`, which counts it as unfinished rather than failed,
and the `OnFailedRead` callbacks in `store/azure` fall silent once the context
is done. Getting this wrong turns one keystroke into a screenful of warnings and
a summary claiming dozens of failures — which is what it did.

**`--delete` is the one thing here that destroys data.** `engine/prune.go`
refuses if anything failed to copy, protects whatever `--exclude` ruled out, and
honours `--dry-run`. Do not relax any of those three without a very good
reason: a half-read source looks exactly like a source with fewer files in it.

**Resume is asymmetric on purpose.** An upload asks the service for its
uncommitted block list, so it needs no local state and survives a reboot or a
change of machine. A download cannot: ranges arrive out of order, so a partial
file is indistinguishable from a whole one with holes, and a record beside the
file is the only honest answer. A record describing a different blob is
discarded rather than continued into. Note that `--resume` must not open the
destination with `O_TRUNC`, which would destroy the very bytes the record
vouches for.

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

`scripts/e2e.sh` covers the blob paths against the emulator — 33 checks; it is the only
test that exercises upload, download, blob-to-blob copy and remote wildcards
together. Add to it when changing anything in `store/azure`.

Run `make check` before proposing changes to the progress display, the worker
pool or anything sharing state between them — it includes the race detector.
Run `make cross` after touching anything platform-specific. Windows has no
`syscall.Stat_t` and no `SIGWINCH`, which is why the local store goes through
the accessors in `plat_*.go` and the display polls its width; and the macOS
token cache needs cgo, which is why the `azidentity/cache` import sits behind a
build tag in `tokencache_persist.go` with an in-memory stand-in beside it.

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

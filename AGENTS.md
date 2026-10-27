# AGENTS.md

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
make lint      # check gofmt formatting, go vet
make check     # lint, tests and race detector
```

For manual end-to-end work against blob storage, the Azurite emulator is
enough and needs no account:

```
docker run -d --rm --name azurite -p 10000:10000 \
  mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck

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

`ci.yml` runs the tests natively on Linux and Windows, both architectures each,
because the copy semantics are full of things that differ per operating system
— symbolic links, file modes, device identity, path separators — and
cross-compiling proves none of it. macOS is deliberately not in that matrix: its
runners cost a multiple of the others, and its copy path shares `plat_unix.go`
with Linux, so what goes untested is the spelling of the stat timestamp fields
in `plat_darwin.go`. macOS uses buffered copies: its pathname-based `clonefile`
cannot clone into an open destination without changing file identity. Touch
platform-specific behavior and run the tests on that platform.
Formatting, vet, the race detector, the cross-compile check (which still
covers darwin) and the emulator-backed end-to-end suite run once on Linux.

Every successful push to main calls `release.yml` after all CI jobs pass;
main runs are never cancelled by a later push. Manual `v*` tags and workflow
dispatch still release too. `scripts/release_version.py` adds the first-parent
distance from the highest stable ancestor tag to its patch number. This makes
versions follow commit order even when CI finishes out of order, and reruns
reuse the same tag. Publish jobs queue rather than discard pending releases,
and an older release cannot become `latest` after a newer one. The workflows
call one another explicitly: a tag pushed with `GITHUB_TOKEN` does not trigger
another workflow. The version allocator's tests run in CI.

The release re-runs the tests, then builds each platform on its own runner.
That last part is not incidental: macOS reaches its keychain through cgo, so a macOS
binary cross-compiled from Linux silently loses the ability to remember a
sign-in. Linux and Windows are built with `CGO_ENABLED=0` for static binaries.
The release builds also run the local-store and TUI tests natively, so disk
allocation and file identity are checked on macOS as well as Linux and Windows.

Version comes from the tag via `-X internal/cli.Version`. A binary built
without that stamp falls back to the module version in its build info, so a
`go install module@v1.2.3` still reports the right thing.

### Container image

`release.yml` calls `packages.yml` to publish `ghcr.io/johanlindvall/azcp` for
amd64 and arm64 for each release; main also publishes `:edge` directly.
Only the latest release advances the moving image tags. The Dockerfile
cross-compiles from `$BUILDPLATFORM` rather than emulating the target, so the arm64 image costs no
more to build than the amd64 one.

The final stage is `FROM scratch` with exactly two things in it: the static
binary and the CA roots it needs to verify a TLS connection to storage. There
is no shell, no libc and no package manager, so there is nothing in the image to
keep patched. If you add a runtime dependency, that property is what you are
spending.

### Dependencies

Nine direct ones, and each earns its place. `klauspost/compress` replaces the
standard library's gzip, flate and zlib coders in both directions — measured at 3.1 GB/s against
2.2 GB/s on the benchmark in `decompress_test.go`, over a whole file in one pass
— and brings zstd, which the standard library has no answer for. It pulls in
nothing else.

`rivo/uniseg` measures terminal cells and keeps grapheme clusters intact when
filenames are shortened. Counting runes instead lets wide characters wrap the
live region and splits combining accents and joined emoji. It has no runtime
dependencies.

`gdamore/tcell/v2` handles keyboard events, resizing and terminal restoration
for the opt-in commander interface, including native Windows consoles. It is
pure Go and adds no runtime dependency to the scratch container image.

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
internal/codec       compression formats: encodings, extensions, coders
internal/engine      planning, the worker pool, cp's per-file semantics
internal/parallel    bounded workers shared by transfers and benchmarks
internal/glob        brace expansion and the pattern matcher
internal/store       namespace interface and the pattern-driven walker
internal/store/local filesystem, reflink, sparse copies, attributes
internal/store/azure blob storage, credentials, transfers
internal/progress    the live terminal display
internal/tui         opt-in two-pane file manager for local and Azure trees
internal/logx        logging, terminal arbitration, secret redaction
internal/retryx      transient-failure classification and backoff
internal/selfupdate daily release checks and verified executable replacement
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

**Copies check for updates; only `--self-update` installs one.** `selfupdate.Run`
is called without `Install` after a successful copy or TUI session. Containers,
development versions and disabled checks do no update I/O; dry runs and informational invocations do not
call it. The official image is also stamped with `selfupdate.ContainerBuild`.
The release's existing `SHA256SUMS` names and verifies the compressed binary;
never install an unverified download or reuse cp's `--update` for this feature.
The explicit command bypasses the check cache and automatic-check opt-outs;
it reports unsupported builds, lock contention and installation errors with
a failing exit status, rather than pretending they mean "up to date".
The check cache and OS lock limit concurrent invocations, and replacing a file
that changed during the download is refused. Unix renames the staged binary
over the executable; Windows moves the running image aside and rolls back if
the second rename fails. Check errors cannot fail a successful copy,
and automatic notices use `logx`, only on a terminal. Native replacement tests run on
each platform in the release workflow.

**A reflink attempt keeps the open destination attached to its name.** The
fallback writes to that same descriptor, and existing hard links must keep
seeing its contents. macOS's `clonefile` cannot meet this contract; unlinking
the destination before cloning loses fallback writes and breaks hard links.
It uses the buffered route under `--reflink=auto` and refuses `always`.

**Collision tracking follows name transformations.** Even one source tree can
collide with itself under `--compress` or `--decompress`. Only an untransformed
tree without backups can omit the scheduled-destination map. A backup creates
a destination name too: a later operand must wait for its rename before weighing
that name, and a backup must wait for earlier readers and writers of its name.

**A copied directory must not follow a destination symlink.** GNU cp permits a
symlink as the containing target directory named on the command line, but rejects
one in place of a directory being copied, including with `-T`. The flat remote
walk must remember failed directories and skip their descendants too; otherwise
a download can overwrite files outside the destination tree.
The intermediate path recreated by `--parents` is another containing path:
existing directory symlinks there are permitted too.

**Block workers are reused.** `internal/parallel.Do` handles bounded execution,
cancellation, the first error, and joining workers for all block transfer routes
and benchmarks. It must not launch a goroutine for every completed resume range.

**The terminal has one owner.** While the progress display is running it owns
stderr. Every write goes through `logx.WithTerminal`, which the display hooks
via `logx.SetGuard` so it can erase its live region first. Writing straight to
`os.Stdout`/`os.Stderr` from anywhere else tears the bars. The full-screen TUI
installs `logx.SetTerminalSink` to receive terminal output as messages instead;
file logs remain independent. The device-code sign-in prompt goes through
`logx.Errf` so it reaches either display whatever the log level.

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

**The scan must not be paced by the transfers.** The task queue is deep — far
deeper than the worker count — because a queue of four per worker fills within
seconds against large files, and a blocked scanner examines nothing at all after
that, not even the files it would have skipped. The total, the count of what has
been seen and any estimate of when the run ends all come from the scan, so
starving it means none of them exist until it is over. The queue holds pointers
to nodes the walk has already built, so depth is cheap.

**Scanning decides on one goroutine on purpose.** It preserves `cp`'s ordering,
creates directories before their contents, and keeps `-i` prompts serial. The
overwrite decisions (`-n`, `-i`, `-u`, `-b`) live there, not in the workers.
What is *not* serial is the fetching: `walkContainers` runs several container
listings at once and hands them to the caller in the order they were listed, so
the decisions still see one container after another on the goroutine that asked.
Do not move the decisions off it.
They are guarded by `needsDestCheck()`, which skips the destination stat
entirely when no option depends on it — that is the difference between a fast
and a slow upload of many small files.

**A rerun asks the directory, not the file.** `-n` and `--update=none` decide on
whether something is there, and nothing else. `engine/destindex.go` answers that
from one listing of the destination directory rather than a stat per name, which
matters because the question is asked on the scanner's goroutine, the one the
workers wait behind, and because `--resume` doubles it: a second stat for the
`.azcp-part` record beside the file, which the same listing already reveals.
Sixty thousand blobs already downloaded cost 120,458 stats before and 458 after.
The rules that weigh the two sides — `-u` against a timestamp, `-i`, `--backup`
— still stat, and `existenceDecides()` is what says so; `TestIndexAgreesWithStat`
holds the two routes to the same answer for every combination, so a decision
must never be made on a fact a directory listing cannot supply. The index also
records what the scanner has just queued, since two source arguments naming the
same relative path would otherwise both be written — and under `--resume`, which
deliberately does not open the destination exclusively, both at once.
Symlink entries fall back to stat: GNU cp checks dangling targets even under
`-n`, and `-u` compares the target's timestamp when copying through a link.
An entry ending in `.azcp-part` is only a candidate resume record; its magic and
regular-file identity must be checked before it can override no-clobber.

**Aliases impose dependencies; unrelated files stay parallel.**
`engine/dependencies.go` keeps scanner-owned completion channels for local
writes and outstanding reads. A later source waits for an earlier writer to
its inode, and a later writer waits for all earlier readers. Serialising only
destination writes is insufficient: truncating a file that another task is
still reading silently produces a short copy. Completed readers are swept so
copying a new tree does not retain one entry per file.
Hard-link preservation also follows scanner order. Under `-au`, a newer first
destination supplies the links for later aliases and keeps its own attributes.
An earlier copy must succeed before its link claim can override a later `-u`
decision; a failed copy must leave that newer destination alone.

**There are two retry layers and they must not multiply.** The SDK pipeline
allows `--retries` total attempts per HTTP request, including the first;
`Store.shouldRetry` is where the retry decision is made and logged.
`retryx` sits above it for whole-file restarts and
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

**The Azure CLI holds one account per tenant, and `--tenant` cannot choose
between them.** `az account get-access-token --tenant` answers as the CLI's
*current* account, which a tenant that account is not a member of refuses with
AADSTS50020, however many other accounts the CLI holds there. `--subscription`
answers as the account the subscription belongs to, and the CLI refuses the two
flags together. So when the identity in hand is refused a token in the tenant a
challenge named, `tenantCredential` asks the CLI's other accounts in that tenant
(`cliaccount.go`). A tenant named with `--tenant` gets the same: `resolve` wraps
the ambient identity in `forTenant` with the CLI's accounts there before probing
it, because the challenge is never followed once `--tenant` has named a tenant,
and without the wrapper naming one did worse than leaving it out — the probe was
refused, and a scripted run went on anonymously to "not signed in".
`TestTheCLIsAccountInATenantNamedWithTheFlagAnswersForIt` pins it, with the
ambient chain stood in for by `Credentials.ambientFn`. Each is named by one of its subscriptions from
`azureProfile.json`, and the request's tenant is cleared. The one that answers
is asked first from then on, because asking the refused identity again costs a
process. This is skipped under `--auth=device` and `--auth=browser`, which ask
to sign in as somebody rather than to be found.
`TestTheCLIIsAskedBySubscriptionAndNotByTenantAsWell` runs the real azidentity
credential against a stand-in `az`, which is what pins the command line.
`newTestStore` points `AZURE_CONFIG_DIR` at an empty directory, so no test reads
a real profile.

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

**A credential may be allowed everything in a container and nothing about it.**
A service SAS scoped to one container (`sr=c`) — the least-privilege token
people are normally handed — reads, writes and lists every blob, and is still
refused Get Container Properties, Create Container and anything at account
level, whatever permissions it carries. So `stat` of a container root falls back
to a listing of one name when the properties are refused, and hands back the
*first* refusal untouched when the listing is refused too, because that is the
error `refreshAuth` and `explainAuth` were written against. `mkdirAll` takes a
403 to mean "may not ask" — not "missing", and not "stop" — and leaves a missing
container to the write, which says `ContainerNotFound`. Both go by the status
rather than the code; the emulator alone has two. A new request about a
container rather than its contents breaks these tokens and nothing else:
`TestNamingStaysInsideTheContainer` and the container-SAS section of
`scripts/e2e.sh` are what notice, the emulator enforcing the same rule the
service does.

**A recursive remote copy takes one flat listing, not one per prefix.**
`planRemoteTree` exists because descending prefix by prefix costs a round trip
per directory and delays the first transfer until the last directory has been
listed. A listing large enough to be divided is still flat: it becomes a
handful of them over disjoint key ranges, not one per directory. It also has to synthesise what the walk gives it: destination
directories created once each, and marker blobs for the directories that turn
out to be empty.

**A listing that is more than a few pages is divided.** A flat listing's pages
are strictly sequential — the marker for the next one arrives at the end of the
last — so a container of a quarter of a million blobs is fifty round trips in
single file with the whole run waiting behind them. `store/azure/split.go`
asks, once a listing has been going for `splitAfterTime` (or `splitAfterPages`,
the backstop for an endpoint too fast to trip the clock), what lies immediately
beneath the prefix; cuts those names into key ranges at the byte where they
diverge, so the ranges cover everything and overlap nothing; and lists them at
once. Measured against `cmblagompoc`, whose 258,000 blobs are almost all in one
container: 129s to 44s, and the whole account 154s to 45s, which is where the
link runs out — a listing page is four megabytes of XML, the service will not
gzip it, and an older `x-ms-version` saves 4%.

The trigger is the whole design. Dividing costs a request to find the ranges
and one per range that ends mid-page, so it has to be paid for out of round
trips it actually saves: a container that fits in a page never asks, which is
what keeps the account of ten thousand small containers costing what it did.
Measured, a rerun of 258,000 blobs spends 72 listings where an undivided one
spends about 55. Two refinements that look free are not, and `split.go` says so
with the numbers: waiting for a second page before asking, and asking while the
listing carries on. Do not re-derive them.
`splitUnits` is where the correctness lives — the units come from one level of a
hierarchical listing, so they are disjoint and none is a prefix of another,
which is what lets a group be cut at its first differing byte — and
`TestSplitUnitsCoversEverythingOnce` holds it to reaching every unit exactly
once. What a divided listing gives up is the container's own names in sorted
order: ordering them would mean only the range being consumed may run ahead,
which is the sequential listing again with more goroutines. `Store.WalkAll`
never promised an order. What it does promise, and what `emptyDirs` and the
destination directories are built on, is every blob's directories before the
blob, and that is a property of each blob's own emission rather than of the
sequence.

**Containers are listed several at a time.** One listing per container is
unavoidable — the service cannot list across them — but an account of ten
thousand small containers, listed one after another, is ten thousand round trips
of doing nothing while every transfer waits. `walkContainers` keeps
`Store.listAhead()` of them in flight, which is a quarter of the run's request
budget and never more than 64, and delivers them in order. Measured against an
endpoint 50 ms away, 400 containers took 18.5s one at a time and 0.79s with the
look-ahead; `--jobs=1 --part-concurrency=1` still lists one at a time. The
memory this costs is bounded by `listAhead × listBuffer` nodes and nothing else,
which is why the per-listing channel has a size at all.

**A compressed upload is a stream, and a stream cannot resume.** `--compress`
produces a length nobody knows in advance, so `UploadEncoded` in
`store/azure/encoded.go` is the one place the SDK's own stream upload is used:
a single request for what came from one block of source, staged blocks as they
arrive otherwise, with `--put-md5` set in a request of its own afterwards
because the digest exists only at the end. Progress is measured on the source.
The extension is added in the planner's `fileDestination`, beside the one
`--decompress` removes, so `-n`, `-u`, `--dry-run` and `--delete` all see the
`.gz`; `internal/codec` is the single table of formats, encodings and
extensions the two directions share, which is what keeps them mirrors.

**A pipe is read once and written once.** A fifo named without `-r` is read to
its end, as `cp` reads one (`plan`'s `IsPipe` case); under `-r` special files
are still skipped. Its task is marked `stream`, which gives it a single attempt,
because a whole-file retry would lose what the first attempt read or send twice
what it wrote; `readOnce` makes a sign-in retry inside `UploadEncoded` say so
rather than upload whatever is left. An upload from one goes through
`UploadEncoded` with a length of -1: gathered as far as one block, staged only
if it goes on. A download into anything the destination's open does not find
regular — a pipe, a fifo, a terminal — takes `DownloadTo` in
`store/azure/stream.go`, whose `orderedWriter` puts the parallel ranges back in
order. `admit` keeps a range from starting until it is within
`concurrency × blockSize` of the front, which is what bounds the memory, and it
cannot deadlock because `parallel.Do` hands ranges out in order, so the one at
the front is always already being fetched. Both directions hash on the way past
instead of re-reading. A destination that is a pipe or a device (`streamDest`)
keeps its name, so `--compress` does not aim at `/dev/stdout.gz`.

**The SDK single-shots anything up to 256 MiB.** `blockblob.UploadFile` ignores
the block size it is given and sends a file of 256 MiB or less in one request,
which is one unparallelised stream that restarts from nothing if it fails.
`uploadBlocks` in `store/azure/transfer.go` exists for that reason: anything
filling more than one block is staged and committed here instead. Do not
"simplify" it back to UploadFile.

**A connection that stops delivering is not a connection that breaks.** TCP
keeps a silent socket alive indefinitely — keepalives answer, queues stay empty,
nothing errors — so a response that stops mid-body leaves the reader waiting on
bytes that will never come. Go's dialer sets a 30-second keepalive, after which
the kernel's nine probes at 75 seconds take about eleven minutes to give up: one
wedged listing freezes a whole scan for that long, because the walk is ordered,
and one wedged range read leaves a file at 95% and a run that appears hung.
There is no per-attempt timeout to fall back on, deliberately, since a large
block may honestly take minutes. `stallTransport` in `store/azure/transport.go`
bounds the one thing that is never honest — delivering nothing at all — and the
failure it raises is marked `retryx.ErrRetryable` and carries a cause of its
own, so the pipeline reissues the request and `engine.interrupted` does not
mistake it for Ctrl-C.

**Connection pool size is a performance feature.** See `store/azure/transport.go`.
The SDK's default of ten idle connections per host means a run with sixty-four
jobs re-establishes a connection for most requests. It costs nothing against a
local emulator, which is exactly why it went unnoticed.

**The final dot of an absolute host is for the resolver alone.** `uri.Parse`
takes it off `Host` into `URL.FQDN`, `Store.client` notes the host, and
`dialAbsolute` puts the dot back on the dialled address and nowhere else. The
service answers a `Host` header ending in a dot with an HTML 400 from its front
end, a server-side copy hands the source URL to the service to fetch, and
`SameAccount` has to see both spellings as one account. `String()` keeps the
dot, so messages and the TUI's engine operands round-trip it.

**Attributes ride in blob metadata.** `store/posixmeta.go` defines the keys and
the encoding; `engine/attrs.go` writes them on upload and restores them on
download. Reading metadata back during a scan costs a larger listing response,
so it is opt-in: `--preserve` and `--decompress` imply it, `--copy-metadata`
asks for it by name, and without any of them a recorded symlink downloads as
the empty blob that records it and a staged blob-to-blob copy carries no
metadata. The order in `restoreAttrs` is not arbitrary: a successful chown
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
The streamed blob-to-blob fallback also checks the scanned length before
handing EOF to the uploader: a clean HTTP EOF must not commit an incomplete
blob, even when no MD5 is available.

**Cancelling is not failing.** Ctrl-C ends every transfer in flight at the same
moment, and the error each one comes back with cannot be trusted to say so: Go
cancels a signal context with a cause of its own ("interrupt signal received"),
and what returns through the SDK is whatever it was given. `engine.interrupted`
asks the context instead. A transfer stopped that way goes to
`progress.Task.Interrupted`, which counts it as unfinished rather than failed,
and the `OnFailedRead` callbacks in `store/azure` fall silent once the context
is done. Getting this wrong turns one keystroke into a screenful of warnings and
a summary claiming dozens of failures — which is what it did.

**A bandwidth limit is for the bulk, and charges for what arrives.** Two things
in `store/azure/throttle.go` are easy to get wrong and impossible to see against
the emulator, which fills every buffer it is handed. A read must be charged for
the bytes it returns, not for the buffer it was offered — a socket hands over a
segment at a time, so charging for a 32 KiB buffer per 1.5 KiB read throttles a
stream to a twentieth of the rate asked for. And a response to a control
operation — anything addressed with a `comp` parameter: listings, properties,
block lists — is not paced at all, because a scan of a large container is
megabytes of XML that every transfer in the run is waiting behind. Request
bodies stay paced whatever they are: staging a block is `PUT ...&comp=block`.

**A resume record outranks -n and -u.** A download that stopped part-way is
already the size of the whole blob — ranges arrive out of order — and carries a
timestamp from a moment ago, so nothing on disk distinguishes it from a finished
copy. `azure.IncompleteDownload` is what says otherwise, and `decideOverwrite`
asks before anything else: skipping that file is the one case where -n leaves
broken data behind and reports success. For the same reason `--resume` wins the
open-flag choice in `download`, since -n would otherwise refuse to open it.

**`--delete` prunes only after a successful copy.** `engine/prune.go`
refuses if anything failed to copy, protects whatever `--exclude` ruled out, and
honours `--dry-run`. Do not relax any of those three without a very good
reason: a half-read source looks exactly like a source with fewer files in it.

**The TUI is explicit, and moves must not delete skipped sources.** Only
`--tui` selects it; a missing operand must keep cp's error behavior. Its copy
engine uses literal selected paths, including properly escaped blob names and
the original SAS. Copy policy comes from the dialogs, so incompatible CLI
flags are rejected. Moves remove sources only after a successful, unskipped,
uncancelled copy and matching source inventories. Deletion first lists every
selection completely, never follows local symlinks, and uses anchored local
roots. Azure files are removed by exact name and ETag (`RemoveBlob`); prefixes
use `RemoveMarker`, never the file-first fallback in `Remove`. Containers may
be deleted but never moved, and go whole through `RemoveContainer` without a
blob listing first; accounts are never removed. The `/` entry a container root
lists for blob names beginning with `/` has the container's own URL, since no
URL can name that prefix, so `sharesContainerURL` refuses every operation on
it; taken for the container, it let `--select type:dir` delete a whole one. The Azure
hierarchy is the storage account, its containers, and nested blob prefixes;
it is exposed directly as directories, without a separate collection or
mount configuration.

**A batch removal judges what it removes.** `--rm --select` picks on one
measuring walk and then lists each selection in full, and `rejudge` weighs the
filter again on that inventory — exactly what `remove` takes, under its inode,
parent and ETag checks — so nothing written after the measurement goes
unjudged. On Windows a stat by path decides which file it describes only when
`os.SameFile` first compares it, by the path as it then stands, so an inventory
settles each local node's identity as it is listed (`fixIdentity`) and takes
the parent's from an open handle (`dirIdentity`); otherwise a lookalike swapped
in before the removal passes for what was listed. A container picked by name
or type goes whole through `RemoveContainer`; one picked by its contents is
listed (`inventoried.listed`), judged on that listing, emptied blob by blob
under ETags, and removed only when `removeEmptied` finds nothing new in it —
deleting it whole would take blobs written after the judgment, which only
`--whole-containers` asks for. Operands that another one contains are taken
with it (`covered`) rather than failing on what it already removed, and an
operand ending in `.` or `..` is refused, as rm refuses it.

**TUI directory totals are optional display state.** In the two-pane view,
`z` and `Z` request them;
browsing, sorting and toggling the size/file-count column must not launch
recursive scans. Sum regular-file lengths and counts in the same walk
without following symlinks, discard partial totals on error or cancellation,
and keep them separate from `Node.Size`, which move inventories compare.
Calculating the whole pane uses one `WalkAll`, preserving Azure's flat listing
and significant empty or dot key components. That walk keeps a `sizeTree` of
one total per directory it passed, carried on `location`, so entering a sized
directory shows its children's totals without walking again. An explicit
refresh advances `app.sizeGen`, which retires every older tree. A mutation
goes through `forgetSizes` instead: it drops the subtree of each changed
name and marks every directory above it `stale`, so untouched siblings keep
their totals and a size-sorted pane keeps its order. Sorts must move marks and the
cursor with the original entries, including a blob and prefix of the same name.

**The usage view is a live snapshot.** `--tui=usage` or `u` requests its scan.
Local scanning streams directory entries in bounded batches; Azure keeps one
flat walk per container. Cached rows hold compact usage nodes, not a full URL,
credential, metadata map and platform stat object per file. Navigation reads the
cache. Disk allocation and logical length are distinct; hard-link data counts
once per ancestor directory, while file counts count names. Symlinks are not
followed. An unreadable subtree marks its ancestors incomplete and cannot be
deleted or moved; readable siblings remain usable. A successful deletion builds
a replacement cache in the worker, recounting shared inodes without rescanning.
The displayed snapshot stays immutable until the result arrives. Empty virtual
Azure prefixes disappear after cleanup, while explicit marker blobs remain.
Other mutations invalidate or rescan the cache; never present stale partial
totals as a complete inventory for deletion.

**Resume is asymmetric on purpose.** An upload asks the service for its
uncommitted block list, so it needs no local state and survives a reboot or a
change of machine. A download cannot: ranges arrive out of order, so a partial
file is indistinguishable from a whole one with holes, and a record beside the
file is the only honest answer. A record describing a different blob is
discarded rather than continued into. Note that `--resume` must not open the
destination with `O_TRUNC`, which would destroy the very bytes the record
vouches for. The record is written per range but never fsynced: it guards
against the process ending, not the power, and the data it vouches for is not
flushed either, so an fsync would cost a disk round trip per range for no added
promise.
Only a regular sidecar beginning with `azcp-resume ` belongs to this tool.
Never truncate or delete an unrelated suffix name or follow a sidecar symlink.
Header resets replace the sidecar atomically to avoid modifying a hard-link
alias. A resumed decompression invalidates compressed ranges before rewriting
the destination, and keeps the destination's existing inode and link identity.

**Blob storage has no directories.** `store/azure` synthesises them: a prefix
with children behaves as a directory, `WalkAll` emits ancestor prefixes so `**`
sees a tree, and an empty directory is the zero-byte `name/` marker blob.
Keys retain significant repeated slashes. Pruning a directory uses
`RemoveMarker`, because trying its unsuffixed name first can delete a distinct
blob and cannot identify markers whose directory key already ends in a slash.

**A local operand's dots are directions, not names.** `uri.URL.Join` cleans a
local path but keeps a blob key's `.` and `..`, since those can be real names.
So `targetFor` sends an operand ending in `.` or `..` to the target itself, as
GNU cp does, before any name is made: joining once gave `azcp -r .` blobs named
`./a.txt`, and sent `SRC/..` into the target's parent on a local disk. Under
`--parents` a blob destination drops `.` and refuses `..`.
`TestDotsInALocalOperandNeverBecomeBlobNames` pins both.

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

`internal/store/expand_test.go` has an in-memory `Store` that counts the
requests made of it. The README's claims about request economy — one stat for
a plain path, one listing per wildcard element, one walk for `**` — are pinned
there, so a change to the walker that costs a round trip shows up as a failing
test rather than in someone's bill.

`internal/engine/destindex_test.go` pins the destination side of the same
economy: one listing per directory rather than a stat per file, and the two
routes to an overwrite decision agreeing with each other.

`internal/store/azure/split_test.go` serves a container with real paging and a
working delimiter, so a divided listing can be held to what an undivided one
would have produced. The emulator cannot stand in for this: its round trips are
free, which is the one condition under which dividing is never worth it.

`scripts/e2e.sh` covers the blob paths against the emulator; it is the only
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
  own, with `-j` the sole exception since `cp` does not define it. A long name
  must not begin one of `cp`'s, because getopt takes an unambiguous prefix as
  that option: `--remove` is `cp`'s `--remove-destination`.
  `TestNoExtensionBeginsACpOption` holds it.
- An option that takes one of a fixed set of words goes through `choose` in
  `cli/options.go`, with its values listed once in the order `--help` gives
  them; the rejection message is derived from that list, not written by hand.
- A default lives in `cli.Defaults()` and nowhere else, and it is what `--help`
  and the README say it is. `--check-md5` once claimed `fail` while defaulting
  to `off`; `TestCheckMD5DefaultsToFail` keeps it honest.
- Anything accepted but not implemented must say so at warn level. Silently
  doing less than asked is worse than refusing.
- Every source file starts with `// SPDX-License-Identifier: MIT` (`#` in a
  script), followed by a blank line so it stays out of the package
  documentation and ahead of any `//go:build` constraint. The full text is in
  `LICENSE`; the one line is what lets a dependency review approve the module
  file by file without a meeting.

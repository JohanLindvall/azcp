# azcp

A drop-in `cp` that also copies to and from Azure Blob Storage.

`azcp` takes the same command line as GNU `cp` — the same short and long
options, the same operand shapes, the same exit statuses, the same messages on
stderr — and lets either side of the copy be an `azure://` URL. It adds the
things a network copier needs and a local one does not: parallel transfers, a
live progress display, retries that ride out a blip, and a log of anything that
went wrong.

```
azcp -r ./build azure://myaccount/releases/v2.1/
azcp 'azure://myaccount/logs/**/*.gz' ./archive/
azcp -r 'azure://a/data/2024/**' 'azure://b/backup/2024/'
```


## Why not AzCopy

**Roughly twice the download throughput**, measured against a real storage
account. **Half the requests** to upload a directory of small files. **Fiftyfold
fewer listing requests** on a deep tree. And it is `cp`, so you already know how
to drive it.

| | azcp | azcopy |
| --- | --- | --- |
| Download throughput, real account | **~2×** | baseline |
| Requests to upload 500 small files | **503** | 1000 |
| Requests to fetch 300 files in 341 directories | **93** (2 listings) | 195 (103 listings) |
| Copy local → local | **yes** | not supported |
| `**`, `!(…)`, `{a,b}` over remote paths | **yes** | filename-only patterns |
| `cp` command line | **identical** | its own verb grammar |
| Sign-in remembered between runs | **yes** | `azcopy login` |

Where the speed comes from — none of it clever, all of it measured:

- **The connection pool matches the concurrency.** The Azure SDK keeps ten idle
  connections per host, so a busy transfer re-establishes a connection, and
  re-negotiates TLS, for most requests. Sizing the pool to the work is worth
  more than anything else here on a link with real latency.
- **Concurrency scales with the machine** — and drops to four when both sides
  are local, where the disk is the bottleneck and more parallelism is slower.
- **One flat listing per subtree**, not one request per directory. AzCopy issues
  103 listings for a tree where `azcp` issues 2; at 20 ms each that is seven
  seconds before the last file can start.
- **No HEAD before every upload.** AzCopy checks each destination first, which
  doubles the request count for small files. `azcp` looks only when an option
  actually depends on what is there.
- **Blocks in parallel above one part**, with the block size you asked for.

Honest about the other side: AzCopy has resumable job plans, `azcopy sync`, and
back ends beyond blob storage. If you need those, use it. [The detailed comparison](#compared-with-azcopy)
below says more.

## Installing

Download a binary from [the latest release][releases] — Linux, macOS and
Windows, on x86-64 and arm64 — or:

```
go install github.com/JohanLindvall/azcp/cmd/azcp@latest
```

[releases]: https://github.com/JohanLindvall/azcp/releases/latest

Linux and Windows binaries are static and need no runtime. macOS binaries are
built on macOS so they can reach the keychain and remember a sign-in between
runs.

Or as a container — the image is a static binary on `scratch`, about 4 MB
compressed, and runs as a non-root user:

```
docker run --rm -v "$PWD:/data" \
  ghcr.io/johanlindvall/azcp:latest -r /data/build azure://acct/releases/
```

Published for `linux/amd64` and `linux/arm64`. Credentials reach it the usual
way — a SAS in the URL, the `AZURE_STORAGE_*` variables, or a managed identity
where the container has one. Interactive sign-in is not useful in a container
and the token cache has nowhere to live, so `azcp` says so and carries on.

Or from a clone — `make` on its own lists every target:

```
make build     # ./bin/azcp, with the version stamped from git describe
make check     # formatting, vet, tests, race detector
make e2e       # the blob paths, against the Azurite emulator in Docker
make release   # stripped binaries for every platform into ./dist
```

`make check` needs no credentials and no network: the pattern matcher is
differential-tested against real `bash` run with `globstar` and `extglob`, and
the copy semantics are tested against temporary directories. `make e2e` needs
Docker, and starts and stops the emulator around itself.

Tested on Linux, macOS and Windows, on x86-64 and arm64 — natively, not by
cross-compiling, because copy semantics are exactly the thing that differs per
operating system.

Windows has no POSIX layer, so `--preserve=ownership` and `--preserve=xattr`
have nothing to act on and `--one-file-system` does not apply; file identity
does exist there, so `--preserve=links` and the loop guard for `-L` both work.
A macOS binary reaches the keychain through cgo, so one cross-compiled from
another platform by `make release` signs in once per run instead of
remembering it; the released macOS binaries are built on macOS and do not have
that limitation.

## Locations

A location is a local path, or a blob URL in any of these forms:

```
azure://ACCOUNT.blob.core.windows.net/CONTAINER/PATH
azure://ACCOUNT/CONTAINER/PATH            # the suffix is filled in
azure://ACCOUNT/CONTAINER/PATH?SAS        # a SAS token in the query
https://ACCOUNT.blob.core.windows.net/…   # accepted for pasted URLs
http://127.0.0.1:10000/devstoreaccount1/… # Azurite and other emulators
```

Anything not recognised as a URL is a local path, taken verbatim — a file whose
name contains a colon or a percent sign is never mangled.

Blob storage has no directories, only names containing slashes. `azcp` presents
that flat namespace as a tree, so `cp`'s rules keep working: a name with things
filed under it behaves as a directory, and an empty directory round-trips
through the zero-byte marker blob every Azure tool uses.

## Signing in

Nothing has to be configured. Credentials are looked for in this order:

1. a SAS token in the URL,
2. `AZURE_STORAGE_CONNECTION_STRING`,
3. `AZURE_STORAGE_SAS_TOKEN`,
4. `AZURE_STORAGE_KEY` (with `AZURE_STORAGE_ACCOUNT`),
5. the ambient Azure identity — environment variables, workload identity,
   managed identity, `az login`, `azd auth login`,
6. an interactive device-code sign-in, if a terminal is attached,
7. anonymous, for containers that allow public read.

An interactive sign-in is remembered. The tokens go into the platform's secure
store — a keyring on Linux, the keychain on macOS, DPAPI on Windows — so later
runs pick the session up silently and never open a browser again until it
expires. Where no such store exists, `azcp` says so before asking, since the
sign-in will have to be repeated.

If the storage account then *rejects* whatever was found — an `az login`
session for the wrong tenant produces a perfectly good token that the account
still refuses — `azcp` says so and signs you in: a browser window where there
is a desktop to show one on, otherwise a device code you can complete
elsewhere. It asks once per run, and only when stderr is a terminal, so a
scripted run fails with a message instead of waiting for an answer nobody can
see.

It asks at most once per run, however many transfers are rejected at the same
moment.

`--auth` pins the choice to `identity`, `browser`, `device` or `anonymous` when
the automatic one is not what you want, and `--tenant` selects the directory to
authenticate against; each tenant is remembered separately. Credential material is never written to the terminal or
to a log: SAS signatures, account keys and bearer tokens are redacted on the
way out.

## Choosing what to copy

Beyond selecting sources with a pattern, a recursive copy can be filtered as it
goes:

```
azcp -r --exclude '*.tmp' ./tree azure://acct/backup/
azcp -r --exclude 'node_modules/**' --exclude '*.log' ./app azure://acct/rel/
azcp -r --include '**/*.parquet' azure://acct/lake/ ./local/
```

A pattern with no slash matches the name at any depth, which is what `*.tmp`
means to most people. One with a slash matches the path relative to what is
being copied — not to how you spelled it, so `--exclude 'build/**'` prunes
`src/build` whether the source was written `./src`, `/abs/path/src` or
`azure://acct/c/src`. An excluded directory is pruned rather than walked and
discarded. `--exclude` beats `--include` where both match, and both take the
full pattern language, extended patterns and braces included.

## Verifying a copy

`--put-md5` records a checksum of the whole file on each uploaded blob, and
`--check-md5` verifies a download against the checksum the blob carries:

| `--check-md5` | |
| --- | --- |
| `off` | do not check |
| `warn` | report a mismatch and keep the file |
| `fail` | a mismatch fails the transfer (default) |
| `require` | additionally fail when the blob records no checksum |

The default is safe for blobs that carry no checksum, which is most of them.
A mismatch is retried before it is reported, since bytes that arrived wrong
often arrive right the second time. Both directions cost an extra read of the
file, because blocks and ranges move out of order and cannot be hashed on the
way past — which is why neither is on by default.

## Limiting bandwidth

`--bwlimit=10M` caps throughput at ten mebibytes a second across the whole run,
counted in the HTTP transport so it covers uploads, downloads and listings
alike. It cannot apply to a server-side blob-to-blob copy, where the bytes move
between storage servers and never reach this host; `azcp` says so rather than
appearing to work.

## Wildcards

The shell cannot see inside a container, so `azcp` expands patterns itself — on
both sides, for symmetry. Quote them so the shell passes them through intact.

| Pattern | Matches |
| --- | --- |
| `*` `?` `[a-z]` `[[:digit:]]` | as usual, within one path element |
| `**` | zero or more whole path elements |
| `{a,b}` `{1..9}` | brace expansion, applied first |
| `?(p)` `*(p)` `+(p)` `@(p)` `!(p)` | extended patterns |

```
azcp 'azure://acct/logs/**/*.gz' ./archive/          # recurse into every prefix
azcp 'azure://acct/data/!(*.tmp)' ./out/             # everything but the scratch files
azcp './build/{linux,darwin}-*' azure://acct/rel/    # two branches at once
```

Semantics follow bash with `globstar` and `extglob` enabled, and the test suite
checks that by running bash and comparing its expansions with ours. By default a local path that exists exactly as
written is never treated as a pattern, so a file genuinely called
`report[final].pdf` still copies. `--glob=always|never` overrides that.

Deep patterns are cheap against blob storage: a `**` turns into one prefixed
listing rather than a request per directory.

## Transfers

Files move `--jobs` at a time and each large file is split into `--part-size`
blocks moved `--part-concurrency` at a time. Uploads stage blocks in parallel
and downloads fetch ranges in parallel; a file that does not fill more than one
block goes up in a single request instead.

`--jobs` defaults to the machine — four times the core count, between 16 and 64
— when either side is a URL, because a network transfer spends nearly all its
time waiting and it is the number in flight that fills the link. A copy that
never leaves the filesystem defaults to four instead: there the disk is the
bottleneck, and seeking between many files makes it slower rather than faster.
The HTTP connection pool is sized to match, so a busy run is not re-establishing
a connection for every request.

A recursive copy *from* blob storage enumerates the whole subtree with one flat
listing rather than a request per prefix. A tree of a few hundred directories
would otherwise spend several seconds issuing listings before the last transfer
could start; this way transfers begin at once.

A copy between two blob URLs is done by the storage service, so the bytes never
reach this host. Three server-side routes are tried in turn, because which one
works depends on the endpoint and on how the source can be authorised:

1. **Put Blob From URL** — one request, for blobs up to 256 MiB.
2. **Put Block From URL** — the same, block by block, for larger blobs. Both of
   these can present an OAuth token for the source, so they work across
   accounts.
3. **Copy Blob** — the asynchronous form. It carries no source credential, so
   the source must be readable by the destination account (same account, or
   carrying a SAS), but it is universally implemented and has no size limit.

Streaming through this host happens only when none of those is available. An
endpoint that answers "not implemented" is remembered, so the run does not keep
asking. `--log-level=debug` reports the route each copy took.

Every network request is retried `--retries` times with jittered backoff,
honouring `Retry-After`. Retries are decided from the failure: a timeout, a
dropped connection, a 429 or a 5xx is worth another attempt; a 404, a 403 or a
full disk is not. Each one is logged, so a slow transfer never looks like a
silent hang.

## Progress

On a terminal, `azcp` draws an aggregate bar with throughput and an estimate, a
row per transfer in flight, and a note when something is being retried. Log
records and `-v` output are interleaved without tearing the display. It stands
down entirely when the output is not a terminal, so in a script `azcp` is as
quiet as `cp`. `--progress=always|never` overrides that.

It repaints once a second, and never holds a transfer up to do it: the display
is drawn from a snapshot, so a slow or blocked terminal cannot stall a worker.
Frames are written as runs of like-coloured cells rather than an escape
sequence per character, which keeps a bar to a few hundred bytes instead of a
few thousand — worth having when the terminal is at the far end of an SSH
session. `--progress-interval` changes the rate.

## Logging

Problems go to stderr as one `cp`-style line each. `--log-level=info` or `debug`
adds structured records — every retry, every skipped file, every attribute that
could not be preserved — and `--log-file` sends them to a file instead,
`--log-format=json` makes them machine-readable.

## Options

`azcp --help` lists everything, and `cp`'s options mean what they mean in `cp`:
`-a -b -d -f -i -H -l -L -n -P -p -R -r -s -S -t -T -u -v -x -Z`,
`--attributes-only --backup --copy-contents --parents --preserve --no-preserve
--reflink --remove-destination --sparse --strip-trailing-slashes --update`.

Added by `azcp`:

| Option | |
| --- | --- |
| `-j, --jobs=N` | files at once (default: scaled to the machine, 4 for local) |
| `--part-size=SIZE` | block size for multi-part transfers (default 8MiB) |
| `--part-concurrency=N` | blocks of one file at once (default 4) |
| `--retries=N` | attempts per request (default 6) |
| `--retry-delay`, `--retry-max-delay` | backoff bounds |
| `--timeout=DUR` | bound on a single request |
| `--max-errors=N` | stop after N failures (default: never) |
| `--progress=WHEN` | `auto`, `always`, `never` |
| `--progress-interval=DUR` | how often the display repaints (default 1s) |
| `--exclude=PATTERN` | skip matching entries; repeatable |
| `--include=PATTERN` | copy only matching entries; repeatable |
| `--put-md5` | record a checksum on each uploaded blob |
| `--check-md5=WHEN` | `off`, `warn`, `fail` (default), `require` |
| `--bwlimit=RATE` | cap throughput, in bytes per second |
| `--log-level`, `--log-format`, `--log-file` | |
| `--dry-run` | report what would be copied |
| `--glob=WHEN` | `auto`, `always`, `never` |
| `--auth=MODE` | `auto`, `identity`, `browser`, `device`, `anonymous` |
| `--tenant=ID` | Entra tenant to authenticate against |
| `--endpoint-suffix=SUFFIX` | for sovereign clouds |
| `--create-container` | create a missing destination container |
| `--content-type`, `--access-tier` | blob properties on write |

Exit status is 0 on success, 1 if a file could not be copied, 2 if the command
line was wrong — the same as `cp`.

## Compared with AzCopy

`azcp` is not trying to replace AzCopy's job management. It is trying to be the
tool you reach for when you want `cp`.

The request counts below were measured against `azcopy` 10.32.7 on the same
workloads and the same endpoint. Requests are what a real account charges in
latency, so they predict wall-clock at any distance in a way that a timing
against a local emulator does not.

| workload | azcp | azcopy |
| --- | --- | --- |
| upload 500 × 8 KiB | **503** | 1000 |
| download 500 × 8 KiB | 503 | 504 |
| upload one 200 MiB file | 29 | **27** |
| download one 200 MiB file | **26** | 27 |
| download 300 files in 341 directories | **93** (2 listings) | 195 (103 listings) |

Throughput against a real storage account, rather than the emulator, has
measured about twice AzCopy's on a sample download.

Where `azcp` is ahead:

- **It is `cp`.** Same options, same operands, same exit statuses, same
  messages. AzCopy has its own verb-based grammar and cannot copy local to
  local at all.
- **Real patterns.** `**`, extended patterns and braces, matched against paths,
  on both sides of the transfer. AzCopy has four overlapping filter flags whose
  patterns match the file name only and whose path forms take no wildcards.
- **`cp` semantics that survive the round trip**: `-a`, `-p`, `--preserve`,
  backups, hard and symbolic links, reflink cloning and sparse files.
- **Sign-in that stays signed in**, with a browser or a device code, remembered
  in the platform's credential store.

Where AzCopy is ahead, and deliberately not copied here:

- **Resumable jobs.** AzCopy records a plan file per job and can resume an
  interrupted transfer. `azcp` re-runs with `-n` or `-u`, which is cheaper to
  reason about and needs no state on disk, but it is not the same thing for a
  transfer measured in days.
- **`azcopy sync`**, with deletion at the destination. `-u` covers the common
  case; a real sync does not fit `cp`'s command line and has not been forced
  into it.
- **Other back ends**: Files, ADLS Gen2, S3 and GCS. `azcp` does blobs.
- **`azcopy bench`**, a throughput benchmark that needs no data of your own.

## Differences from cp

- Symbolic links cannot be stored in blob storage. Copying one to a container
  skips it with a warning; `-L` copies what it points at instead.
- `--backup`, `--link` and `--symbolic-link` apply to local destinations only,
  and are refused before anything is transferred rather than partway through.
- `-Z`, `--context` and `--preserve=context` are accepted but do nothing: this
  tool does not set SELinux contexts. Using one logs a warning saying so.
- `--copy-contents` is accepted and has no effect, since special files are
  never recursed into.
- A missing destination container is an error rather than being created
  silently; `--create-container` opts in. Containers behave more like a mount
  point than a directory, so creating one is not something to do by accident.

## Layout

```
cmd/azcp             entry point, signals, exit status
scripts/e2e.sh       the emulator-backed end-to-end check
internal/cli         option table, help, resolved configuration
internal/cpflags     getopt_long-compatible parser
internal/engine      planning, the worker pool, cp's file semantics
internal/glob        brace expansion and the pattern matcher
internal/store       the namespace interface and the pattern-driven walker
internal/store/local filesystem, reflink, sparse copies, attributes
internal/store/azure  blob storage, credentials, parallel transfers
internal/progress    the live display
internal/logx        logging, terminal arbitration, secret redaction
internal/retryx      transient-failure classification and backoff
internal/humanize    sizes, rates, durations
internal/uri         location parsing
```

## License

MIT — see [LICENSE](LICENSE).

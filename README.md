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
  seconds before the last file can start. Where a listing per container really
  is unavoidable — an account full of small containers — they are fetched
  several at a time and handed on in order: 400 containers 50 ms away took
  18.5s one at a time and 0.79s this way.
- **No HEAD before every upload.** AzCopy checks each destination first, which
  doubles the request count for small files. `azcp` looks only when an option
  actually depends on what is there.
- **Blocks in parallel above one part**, with the block size you asked for.

Honest about the other side: AzCopy has resumable job plans, `azcopy sync`, and
back ends beyond blob storage. If you need those, use it. [The detailed comparison](#compared-with-azcopy)
below says more.

## How to migrate

AzCopy is verb-first (`azcopy copy SRC DST --recursive`); `azcp` is `cp`
(`azcp -r SRC DST`). Everything else is a rename.

### Commands

| AzCopy | azcp |
| --- | --- |
| `azcopy copy SRC DST` | `azcp SRC DST` |
| `azcopy copy SRC DST --recursive` | `azcp -r SRC DST` |
| `azcopy sync SRC DST` | `azcp -r -u SRC DST` |
| `azcopy sync SRC DST --delete-destination=true` | `azcp -r -u --delete SRC DST` |
| `azcopy make URL` | `azcp --create-container …` (made when first written to) |
| `azcopy bench URL` | `azcp --benchmark URL` |
| `azcopy jobs resume ID` | `azcp --resume …` — re-run the same command |
| `azcopy login` | nothing: credentials are found, and a sign-in is remembered |
| `azcopy list`, `azcopy remove` | no equivalent; `azcp` copies |

### Flags

| AzCopy | azcp | |
| --- | --- | --- |
| `--recursive` | `-r` | |
| `--overwrite=true` | *(default)* | |
| `--overwrite=false` | `-n` | |
| `--overwrite=prompt` | `-i` | |
| `--overwrite=ifSourceNewer` | `-u` | |
| `--as-subdir=false` | `-T` | |
| `--dry-run` | `--dry-run` | |
| `--include-pattern '*.jpg;*.pdf'` | `--include '*.{jpg,pdf}'` | or repeat `--include` |
| `--exclude-pattern '*.tmp'` | `--exclude '*.tmp'` | |
| `--include-path 'logs;etc/hosts'` | `--include 'logs/**' --include 'etc/hosts'` | patterns here match paths, so this is one mechanism rather than two |
| `--exclude-path 'cache'` | `--exclude 'cache/**'` | |
| `--include-regex`, `--exclude-regex` | *(no regex)* | `**`, `!(…)`, `@(a\|b)` and `[…]` cover most of it |
| `--include-after=2024-01-01` | `--newer-than 2024-01-01` | also accepts `7d` |
| `--include-before=2024-02-01` | `--older-than 2024-02-01` | |
| `--block-size-mb=16` | `--part-size=16MiB` | |
| `--cap-mbps=100` | `--bwlimit=12.5M` | **bytes** per second, not bits |
| `AZCOPY_CONCURRENCY_VALUE=64` | `--jobs=64` | files at once; `--part-concurrency` for blocks within one file |
| `--check-md5=FailIfDifferent` | `--check-md5=fail` | *(default in both)* |
| `--check-md5=NoCheck` | `--check-md5=off` | |
| `--check-md5=LogOnly` | `--check-md5=warn` | |
| `--check-md5=FailIfDifferentOrMissing` | `--check-md5=require` | |
| `--put-md5` | `--put-md5` | |
| `--check-length` | *(always)* | a download is sized to the blob |
| `--metadata 'a=b;c=d'` | `--metadata a=b,c=d` | |
| `--content-type`, `--content-encoding`, `--content-language`, `--content-disposition`, `--cache-control` | same names | |
| `--block-blob-tier=Cool` | `--access-tier=Cool` | |
| `--decompress` | `--decompress` | also handles zstd |
| `--preserve-posix-properties`, `--preserve-permissions`, `--preserve-owner` | `--preserve=mode,ownership,timestamps`, or `-a` | |
| `--preserve-symlinks` | `-a` | included in what `-a` keeps |
| `--follow-symlinks` | `-L` | |
| `--from-to=LocalBlob` | *(inferred)* | from the arguments |
| `--output-type=json` | `--output=json` | |
| `--log-level=WARNING` | `--log-level=warn` | `error`, `warn`, `info`, `debug` |
| `AZCOPY_LOG_LOCATION` | `--log-file` | |
| `--list-of-files list.txt` | `--files-from list.txt` | or `--files-from -` to read it from a pipe |
| `AZCOPY_BUFFER_GB` | *(not needed)* | data is streamed, not buffered whole |
| `AZCOPY_JOB_PLAN_LOCATION` | *(not needed)* | `--resume` asks the service what arrived |

### URLs and credentials

The `https://ACCOUNT.blob.core.windows.net/container/path?SAS` form AzCopy uses
works unchanged. `azure://ACCOUNT/container/path` is shorter and fills in the
endpoint for you.

`azcopy login` has no counterpart because there is nothing to do: a SAS in the
URL, the `AZURE_STORAGE_*` variables, a managed identity or an existing
`az login` are all found automatically, and if the account rejects what was
found, `azcp` offers a browser or device-code sign-in and remembers it. Where
AzCopy takes `AZCOPY_AUTO_LOGIN_TYPE`, `azcp` takes `--auth`.

### Worked examples

```
# azcopy copy 'C:\data' 'https://acct.blob.core.windows.net/backup?SAS' --recursive
azcp -r C:\data 'https://acct.blob.core.windows.net/backup?SAS'

# azcopy sync ./site 'https://acct.blob.core.windows.net/www' \
#   --delete-destination=true --exclude-pattern '*.map'
azcp -r -u --delete --exclude '*.map' ./site azure://acct/www/

# azcopy copy 'https://acct.blob.core.windows.net/logs/*' ./logs --recursive \
#   --include-pattern '*.gz' --include-after 2024-01-01
azcp -r --include '*.gz' --newer-than 2024-01-01 'azure://acct/logs/**' ./logs/

# azcopy copy ./big.iso 'https://acct.blob.core.windows.net/c/big.iso' \
#   --block-size-mb 32 --put-md5 --cap-mbps 400
azcp --part-size=32MiB --put-md5 --bwlimit=50M ./big.iso azure://acct/c/big.iso
```

### What has no counterpart

Azure Files, ADLS Gen2, S3 and GCS: `azcp` does blobs and local files. Page and
append blobs: block blobs only. And `azcopy jobs`, which is a durable record of
every transfer ever run — `--resume` finishes an interrupted copy, but it does
not give you an audit trail afterwards.

## Installing

Download from [the latest release][releases] — Linux, macOS and Windows, on
x86-64 and arm64. Each platform has an archive with the documentation, and the
binary on its own, compressed, for scripts:

```
curl -fsSL https://github.com/JohanLindvall/azcp/releases/latest/download/azcp_v0.1.0_linux_amd64.bin.gz \
  | gunzip > azcp && chmod +x azcp
```

Or:

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

Tagged `latest`, the exact version (`0.1.0`), the moving minor (`0.1`), and
`edge` from the main branch. Published for `linux/amd64` and `linux/arm64`,
with a build-provenance attestation and an SBOM. Credentials reach it the usual
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
still refuses — the account itself says which directory it does trust, in the
challenge it sends back. `azcp` follows that: the identity already in hand is
asked for a token in that tenant, which is all a guest account usually needs,
and nobody is troubled at all.

Where even that is refused, `azcp` says so and signs you in, naming the tenant
rather than leaving you to guess it: a browser window where there is a desktop
to show one on, otherwise a device code you can complete elsewhere, and either
way directed at the tenant the account named. It asks only when stderr is a
terminal, so a scripted run fails with a message instead of waiting for an
answer nobody can see.

It asks at most once per run, however many transfers are rejected at the same
moment.

`--auth` pins the choice to `identity`, `browser`, `device` or `anonymous` when
the automatic one is not what you want, and `--tenant` selects the directory to
authenticate against — pinning it against the account's own answer, too. Each
tenant is remembered separately. Credential material is never written to the
terminal or to a log: SAS signatures, account keys and bearer tokens are
redacted on the way out.

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

A list of sources too long for a command line, or produced by another tool,
can be read from a file — one name per line, `-` for standard input:

```
azcp --files-from=list.txt azure://acct/backup/
find . -name '*.parquet' -newer stamp | azcp --files-from=- -t azure://acct/lake/
```

Listed names are sources like any other, URLs and patterns included, and the
destination is still the last operand or `-t`.

## Archiving a tree

Blob storage has no file mode, no owner and no symbolic links, so a tree copied
into it normally arrives stripped to its bytes and its names. `--preserve`
carries the rest in blob metadata, which means `-a` round-trips:

```
azcp -a ./tree azure://acct/backup/     # modes, owners, timestamps, symlinks
azcp -a azure://acct/backup/tree ./restored
```

Ownership is restored only where you have the privilege to set it, and is
reported when you do not. Directory modes are not kept: a directory that is not
empty has no object of its own to hang metadata on, and inventing one for every
directory would litter the container.

Reading each blob's metadata back makes every listing response larger, so it is
opt-in: `--preserve` and `--decompress` imply it, and `--copy-metadata` asks
for it by name. That flag is what makes a plain `-r` download turn a recorded
symbolic link back into one (without it, the link arrives as the empty blob
that records it), and what lets a blob-to-blob copy carry metadata on every
route — without it, metadata survives only where the service copies it itself,
which covers whole-blob copies but not blobs large enough to be staged in
blocks.

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
counted in the HTTP transport so it covers uploads and downloads alike, and
charged by the bytes that arrive rather than the buffers they were asked for in.

Scanning is neither capped nor slowed. Listing a large container is megabytes of
XML with every transfer in the run waiting behind it, and pacing that would
spend the whole budget on working out what to copy; the bulk is what the limit
is for. Nor can
it apply to a server-side blob-to-blob copy, where the bytes move between
storage servers and never reach this host — `azcp` says so rather than appearing
to work.

## Keeping a destination in step

`-u` copies only what is newer, and `--delete` removes what the source no longer
has, which together make a copy into a replica:

```
azcp -r -u --delete ./site azure://acct/www/
```

`--delete` is deliberately timid, because it is the only thing here that
destroys data. It refuses if any file failed to copy — a listing that stopped
half way looks exactly like a source with fewer files in it. It never removes
anything `--exclude` ruled out, since an exclusion says "not my business", not
"remove it". And `--dry-run` reports every deletion without making one.

`--newer-than` and `--older-than` bound by modification time, for pipelines that
track their own watermark:

```
azcp -r --newer-than 7d azure://acct/logs/ ./recent/
azcp -r --newer-than 2024-01-01 --older-than 2024-02-01 ./archive azure://acct/jan/
```

A connection that goes quiet is abandoned after a minute and tried again. The
bound is on delivering *nothing*, not on taking a long time, so a large block
arriving slowly is left alone — but a socket that has silently stopped no longer
costs the eleven minutes it takes TCP to notice.

## Interrupted transfers

`--resume` continues a transfer that stopped part-way rather than starting it
again:

```
azcp --resume -r ./huge azure://acct/data/
```

It says nothing about the files that already arrived whole, and keeps no list of
them — that would be a job plan by another name, which this tool does not have.
To carry on where a whole interrupted run left off, pair it with `-n`, which
leaves a destination that is already there alone:

```
azcp --resume -n -r azure://acct/data/ ./huge
```

A file that stopped part-way is not one of those: it is already the size of the
whole blob and was touched a moment ago, so nothing about it says it is
unfinished except the record beside it. That record is believed, and `-n` and
`-u` are kept off it — otherwise the one file that needed finishing would be the
one skipped, and the run would report success over it.

Uploading, it needs nothing on this machine. Blocks staged by the earlier
attempt are still held against the blob, so `azcp` asks the service what
arrived and sends only the rest — which means a transfer can be resumed after a
reboot, or from a different machine entirely.

Downloading, only this process knows which ranges landed, because they arrive
out of order and a half-written file is indistinguishable from a whole one with
holes. A small record is kept beside the file and removed when it is complete. A
record that describes a different blob is discarded rather than spliced in.

Stopping a run with Ctrl-C says how much was left unfinished and whether it can
be picked up, which differs by direction for the reason above — an upload can be
continued whatever the interrupted run was given, a download only if it was
already keeping a record. Nothing else is said: everything still in flight ends
at the same moment, and that is not a list of failures.

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

`--jobs` defaults to the machine — twice the core count, between 8 and 32 — when
either side is a URL, because a network transfer spends nearly all its time
waiting and it is the number in flight that fills the link. A copy that never
leaves the filesystem defaults to four instead: there the disk is the
bottleneck, and seeking between many files makes it slower rather than faster.
The HTTP connection pool is sized to match, so a busy run is not re-establishing
a connection for every request.

That default also decides how many sockets a run opens: HTTP/1.1 carries one
request per connection, and each job may have `--part-concurrency` of them
outstanding. Thirty-two jobs of four parts is a hundred and twenty-eight
requests in flight, which is more than a fast link needs — at 70 MB/s over a
15 ms round trip there is about a megabyte in flight altogether — and few enough
that the middleboxes on the way, which count flows rather than bytes, are not
being asked to hold hundreds open. Raise `--jobs` where the path can take it.

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

## Machine-readable output

`--output=json` writes one object per line and ends with a summary, so a
pipeline can read results rather than parse prose:

```
$ azcp --output=json -v -r ./build azure://acct/rel/ | tail -1
{"bytes":41283,"copied":12,"deleted":0,"elapsed_seconds":1.83,"event":"summary",...}
```

Failures appear as they happen and again in the summary's `failures` array.

## Measuring the link

`--benchmark` answers the question you actually have when a copy is slow —
whether the tool is misconfigured or the link is simply that fast. It moves
generated data, so the local disk plays no part, and removes it afterwards:

```
$ azcp --benchmark=10x64MiB azure://acct/scratch/

  10 files of 64.0 MiB — 640 MiB in each direction
  upload       6.1s   105 MiB/s
  download     3.4s   188 MiB/s

  measured with --jobs=64 --part-size=8.00 MiB
```

## Logging

Problems go to stderr as one `cp`-style line each. `--log-level=info` or `debug`
adds structured records — every retry, every skipped file, every attribute that
could not be preserved — and `--log-file` sends them to a file instead,
`--log-format=json` makes them machine-readable.

## Options

`azcp --help` lists everything, and `cp`'s options mean what they mean in `cp`:
`-a -b -d -f -i -H -l -L -n -P -p -R -r -s -S -t -T -u -v -x -Z`,
`--attributes-only --backup --copy-contents --debug --parents --preserve
--no-preserve --reflink --remove-destination --sparse --strip-trailing-slashes
--update`.

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
| `--no-progress` | same as `--progress=never` |
| `--progress-interval=DUR` | how often the display repaints (default 1s) |
| `--exclude=PATTERN` | skip matching entries; repeatable |
| `--include=PATTERN` | copy only matching entries; repeatable |
| `--files-from=FILE` | read further sources from FILE, one per line; `-` is standard input |
| `--put-md5` | record a checksum on each uploaded blob |
| `--check-md5=WHEN` | `off`, `warn`, `fail` (default), `require` |
| `--bwlimit=RATE` | cap throughput, in bytes per second |
| `--delete` | remove destination entries the source does not have |
| `--resume` | continue an interrupted transfer |
| `--newer-than`, `--older-than` | bound by modification time |
| `--decompress` | expand gzip, deflate or zstd blobs on download |
| `--metadata=K=V` | store metadata on uploaded blobs |
| `--copy-metadata` | read blob metadata while scanning (see [Archiving a tree](#archiving-a-tree)) |
| `--content-encoding`, `--content-disposition`, `--content-language`, `--cache-control` | blob headers |
| `--output=FORMAT` | `text` or `json` |
| `--benchmark[=NxSIZE]` | measure throughput and clean up |
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

Things AzCopy once had that `azcp` did not, and now does: resuming an
interrupted transfer (`--resume`), making a destination match a source
(`-u --delete`), checksums (`--put-md5`, `--check-md5`), throttling
(`--bwlimit`), filtering (`--include`, `--exclude`, `--newer-than`,
`--older-than`), blob properties and metadata, `--decompress`, machine-readable
output (`--output=json`) and a throughput benchmark (`--benchmark`).

Resuming is the one where the difference is worth spelling out. AzCopy keeps a
job plan on the machine that started the transfer, and resumes by reading it
back. `azcp` asks the service which blocks arrived, so an upload resumes with
no local state at all — after a reboot, or from a different machine.

What remains AzCopy's, by choice:

- **Other back ends**: Azure Files, ADLS Gen2, S3 and GCS. `azcp` does blobs and
  local files.
- **`azcopy jobs`**, a durable record of every transfer ever run, with a job id
  to look up afterwards. `--resume` covers finishing an interrupted copy;
  it does not give you an audit trail.

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

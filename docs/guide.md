# User guide

[Documentation](README.md) · [Install](installation.md) · [Quick start](quickstart.md) · [Examples](../README.md#everyday-examples)

This is the detailed reference for `azcp`. Start with the
[quick start](quickstart.md) for a first copy, or run `azcp --help` for the
complete command-line interface. The compatibility reference is GNU `cp` 9.4;
[platform and storage differences](#differences-from-cp) are listed below.

- [Locations](#locations)
- [Pipes](#pipes)
- [Signing in](#signing-in)
- [Choosing what to copy](#choosing-what-to-copy)
- [Selecting and removing in batches](#selecting-and-removing-in-batches)
- [Archiving a tree](#archiving-a-tree)
- [Verifying a copy](#verifying-a-copy)
- [Compressing on the way](#compressing-on-the-way)
- [Limiting bandwidth](#limiting-bandwidth)
- [Keeping a destination in step](#keeping-a-destination-in-step)
- [Interrupted transfers](#interrupted-transfers)
- [Wildcards](#wildcards)
- [Transfers](#transfers)
- [Progress](#progress)
- [Machine-readable output](#machine-readable-output)
- [Measuring the link](#measuring-the-link)
- [Logging](#logging)
- [Options](#options)
- [Differences from cp](#differences-from-cp)

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

A host may be written as an absolute DNS name, with a final dot —
`azure://ACCOUNT.blob.core.windows.net./CONTAINER/PATH`, `azure://ACCOUNT./…`, or
`--endpoint-suffix=blob.core.windows.net.` for every bare account name. The dot
is used to look the name up and nowhere else, so the resolver goes straight to
the real name instead of trying each search domain first, as it does inside a
Kubernetes pod. The service itself never sees it: a request naming the host with
the dot is refused with HTTP 400 before it reaches storage.

Blob storage has no directories, only names containing slashes. `azcp` presents
that flat namespace as a tree, so `cp`'s rules keep working: a name with things
filed under it behaves as a directory, and an empty directory round-trips
through the zero-byte marker blob every Azure tool uses.

Blob keys retain repeated slashes and dot components when copied between remote
locations. A local path's dots are directions rather than names, as they are to
`cp`: `azcp -r . azure://acct/c/` uploads the directory's contents, and
`SRC/..` lands in the target too, never above it. Under `--parents` a blob
destination drops `.` components and refuses a path through `..`, which no blob
name can reproduce. A literal final slash asserts a directory; `%2F` names a slash that
belongs to the key. Downloading a tree rejects names that the local filesystem
would collapse or reinterpret, rather than writing two blobs onto one file or
escaping the destination tree. An explicit local filename can still receive
an individually named blob.

## Pipes

A fifo named on the command line is read to its end, as `cp` reads one, so a
pipe — `/dev/stdin`, or `<(…)` — can be the source, and a pipe can be the
destination too:

```
tar -c ./site | azcp /dev/stdin azure://acct/backup/site.tar
azcp azure://acct/backup/site.tar /dev/stdout | tar -x
pg_dump shop | azcp --compress=zstd /dev/stdin azure://acct/dumps/shop.sql
```

A pipe has no length until it ends, so it goes up the way a compressed file
does: in one request if it ends within a block, staged block by block as it
arrives if it does not. With nothing to size them by, the blocks are
`--part-size` each, and a blob holds at most 50,000 of them — about 390 GiB at
the default 8 MiB, so raise `--part-size` for a longer stream. A download into a
pipe still fetches `--part-concurrency` ranges at once and puts them back in
order before writing them, holding about one range per worker in memory.

What a pipe carries exists only as it passes, so nothing about it can be done
twice. A transfer that fails is not started again, `--resume` has nothing to
continue, and the checksum is taken on the way past — on a download, a mismatch
can be reported only once the bytes have gone. A reader that goes away ends the
copy with `error writing '/dev/stdout': Broken pipe`. `-v` and `--output=json`
write to standard output as well, so do not combine them with `/dev/stdout` as
the destination.

A destination that is a pipe or a device keeps its name: `--compress` adds no
extension to `/dev/stdout`. `-r` reads no fifos; see
[Differences from cp](#differences-from-cp).

## Signing in

Nothing has to be configured. Credentials are looked for in this order:

1. a SAS token in the URL,
2. `AZURE_STORAGE_CONNECTION_STRING`,
3. `AZURE_STORAGE_SAS_TOKEN`,
4. `AZURE_STORAGE_KEY` (with `AZURE_STORAGE_ACCOUNT`),
5. the ambient Azure identity — environment variables, workload identity,
   managed identity, `az login`, `azd auth login`,
6. a saved interactive sign-in,
7. an interactive browser or device-code sign-in, if a terminal is attached,
8. anonymous, for containers that allow public read.

A connection string applies only to the account it names. When
`AZURE_STORAGE_ACCOUNT` is set, the environment SAS and account key are also
restricted to that account; a copy involving another account discovers that
account's credentials separately. A SAS on a URL always applies to that URL.

A SAS need not cover the whole account. One scoped to a single container
(`sr=c`) works: `rl` to copy out of it, `rwl` to copy into it, and `d` as well
for `--delete`. The service does not let such a token ask about the container
itself, only about what is in it, so `azcp` cannot check that the container
exists before it starts. One that does not is reported by the first listing of
it, or by the first write into it (`ContainerNotFound`), and
`--create-container` cannot make it: creating a container takes an account SAS,
a key or an identity.

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

An `az login` often holds more than one account: your own directory's, and a
customer's or another employer's. The CLI answers a request for a tenant as its
*current* account, which a tenant that account is not a member of refuses. So
`azcp` goes on to ask the CLI's other accounts in that tenant, naming each by
one of its subscriptions, because the subscription, not the tenant, is how the
CLI is told whose token to hand over. They are found in the CLI's profile, and
asking them troubles nobody either. A tenant named with `--tenant` is treated
the same way, so naming the tenant finds the account that is a member there
just as leaving it to the storage account does.

Where even that is refused, `azcp` says so and signs you in, naming the tenant
rather than leaving you to guess it: a browser window where there is a desktop
to show one on, otherwise a device code you can complete elsewhere, and either
way directed at the tenant the account named. It asks only when stderr is a
terminal, so a scripted run fails with a message instead of waiting for an
answer nobody can see.

It asks at most once per run, however many transfers are rejected at the same
moment.
A browser sign-in that was started and then failed or was cancelled does not
open a second device-code prompt in the same run.

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
destination must still be supplied as the last command-line operand or with
`-t`. An omitted destination is an error; a name in the list is never used as
the destination.

## Selecting and removing in batches

`--select=FILTER` picks entries out of a directory with the
[file manager's filter](tui.md#filtering-a-pane), and does on the command line
what marking them in a pane and pressing a key would. Each source operand must
be a directory; it is replaced by the entries of it that the filter selects,
and the copy then runs as it would with those named one by one, every other
option included:

```
azcp -r --select 'type:dir has:*.mp4' azure://acct/media/ ./clips/
azcp -r --select 'older:90d -*.keep' ./scratch azure://acct/archive/
```

`--rm` deletes instead of copying, and by default only lists what it would
delete, with each entry's size and file count and a total on stderr. Add `--yes`
to delete. Without `--select` it removes the operands themselves. As with `rm`,
a directory needs `-r`, `-v` names each entry as it goes, and an operand ending
in `.` or `..` is refused. It is not called `--remove` because `cp` reads that
as its own `--remove-destination`, abbreviated.

```
azcp --rm -r --select 'older:2024-01-01' azure://acct/logs/       # lists
azcp --rm -r --select 'older:2024-01-01' --yes azure://acct/logs/ # deletes
azcp --rm -r --select 'tmp-* scratch-*' azure://acct/              # whole containers
azcp --rm -r --select 'has:*.mp4 older:60d' azure://acct/          # by what they hold
```

A filter is weighed exactly as a pane weighs it, with one difference: a
directory the filter cannot judge is left out rather than kept. Directories are
always measured, in one walk, when the filter asks about their sizes, ages or
contents, so in practice this means a directory with no files, which has no
age, is never `older:` anything. A plain word matches any name that contains
it, ignoring case, while a pattern must match the whole name: `log` picks
`catalog` too, and `log*` does not.

Removal keeps the file manager's guarantees: every selection is listed in full
before anything is deleted, local symbolic links are removed and never
followed, and a blob is deleted only if it is still the version that was
listed. The filter is weighed again on that listing, so whatever was written
or changed after the selection was made is judged as it now stands, and an
entry that no longer matches is kept. A container chosen by name or type goes
whole, in one request, with anything written to it meanwhile. One chosen for
what it holds is listed in full and judged on that listing, its blobs are
deleted by name, and the container itself only if nothing has arrived in it
since; otherwise it is kept and reported. Blob names that
begin with `/` appear at a container root as a directory called `/`, which no
URL names apart from the container, so that entry is refused rather than
taken for the container; name such blobs one at a time, as
`azure://acct/c/%2Fname`. An operand inside another is removed with it.
Operands are taken literally, without wildcard expansion: the filter is the
pattern. `--rm` accepts only options that make sense without a copy, and
`--output=json` writes one `would-remove` or `removed` event per entry, an
`error` event for each failure, and a closing `summary`.

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

Recorded symbolic-link targets can contain Unicode, whitespace and control
bytes. Ordinary ASCII targets retain the `azcp_symlink` metadata field;
targets that cannot safely fit in an HTTP header use `azcp_symlink_base64`.
Older versions that do not recognize the latter field cannot restore those
links. Invalid mode and owner metadata is ignored rather than passed to the
operating system after integer truncation.

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
way past. Upload checksums are opt-in; downloads check a recorded MD5 by default.

Downloads and server-side copies are conditional on the blob version seen during
the scan. A blob changed during the copy fails the transfer instead of silently
combining ranges from different versions. Run the command again to copy its new
version.

## Compressing on the way

`--compress` stores each file compressed, with `Content-Encoding` saying so and
the matching extension appended — which is how a web pipeline expects a blob it
will serve compressed to look:

```
azcp -r --compress ./site azure://acct/www/           # gzip at its default level
azcp -r --compress=zstd:19 ./logs azure://acct/cold/  # zstd, as small as it goes
azcp -r --compress=9 ./site azure://acct/www/         # gzip, level 9
```

`report.csv` lands as `report.csv.gz` with `Content-Type: text/csv` and
`Content-Encoding: gzip`: the type describes the file inside, the encoding the
wrapper. The formats are `gzip` (levels 1–9), `deflate` (1–9) and `zstd`
(1–19). A file that already carries one of those extensions is copied as it is
rather than wrapped twice, and a copy that never leaves the filesystem gets the
same treatment without the headers. `-n`, `-u`, `--dry-run` and `--delete` all
see the name that lands.

If two sources map to the same name — for example, a directory containing both
`report.csv` and `report.csv.gz` — the second copy is refused. `-n` skips it;
`--backup=numbered` with a local destination retains both in source order.
Dry runs report the same conflicts.

The size shown while copying is the source's, since the compressed size is not
known until the end — which is also why a compressed upload that is interrupted
starts over rather than resuming. Each file is compressed on one core; the
parallelism is `--jobs`.

`--decompress` is the mirror: on download, a blob whose `Content-Encoding` names
one of those formats is expanded on arrival and loses the extension — `.tgz`
and `.tzst` become `.tar` — so a tree that went up with `--compress` comes back
down as it left.

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
Excluding a directory protects all of its descendants. Destination symlinks
are never followed when looking for entries to delete, even with `-L`.
An incomplete or failed scan also prevents deletion.

`--dry-run` creates no destination files, directories or containers, including
with `--parents`. It cannot be combined with `--benchmark`, which requires real
transfers to measure throughput.

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
Each staged block is identified by a hash of its contents, so changing the
source or block size reuses only blocks whose bytes still match. Computing these
identities costs an extra local read of each staged block; it adds no storage
requests. Blocks staged by older versions with index-only identities are uploaded
again rather than trusted.

Downloading, only this process knows which ranges landed, because they arrive
out of order and a half-written file is indistinguishable from a whole one with
holes. A small record is kept beside the file and removed when it is complete. A
record that describes a different blob is discarded rather than spliced in.
The record also identifies the account and blob path, and is rejected if the
local file is missing, has the wrong size, or the record is malformed. Older
record formats restart safely. A checksum failure leaves an incomplete record
and invalidates the saved ranges so `--resume -n` fetches them again.
Without `--resume`, an incomplete download is restarted in full, even with
`-n`, and its stale record is removed after a successful copy.

The record's name is `FILE.azcp-part`. Only a regular file beginning with
`azcp-resume ` is recognized as a record. Unrelated files, empty files and
symlinks at that name cannot override `-n` and are never overwritten as resume
state. If a download needs the name and it is occupied by one of these, the
download fails; move the conflicting file before retrying. A tree that also
contains a blob named `FILE.azcp-part` cannot download both names with resume
enabled, and reports the collision. Rewriting a recognized record replaces
the record itself, leaving any other hard links to its previous contents alone.

With `--decompress`, overwrite checks, dry-run output and deletion tracking all
use the expanded filename. For example, downloading `page.gz` produces `page`,
and `-n` protects an existing `page`. A resumed download keeps its record until
decompression finishes, invalidating its compressed ranges before rewriting
the destination. Existing destination hard links and symlinks keep their
identity. Without resume, decoding streams directly into that destination;
with resume, expansion uses a temporary file before copying the expanded bytes
back. `--attributes-only` leaves the existing contents and
filename alone, even when `--decompress` is also given.

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
Brace expansion is limited to 8,192 results per argument. Exceeding the limit
is an error, not a silently shortened list of sources.

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

Local tasks whose source or destination names alias a file used by an earlier
task wait for that task, preserving operand order. Copies of unrelated files
remain parallel.

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

Content-header overrides also apply to the asynchronous route. When an override
differs from the source, it costs one properties update after the copy finishes;
unchanged properties and the source checksum are retained. A plain asynchronous
copy needs no additional update. `--attributes-only` updates an existing blob's
properties and metadata without copying its contents, including blob-to-blob
invocations.

All copy routes retain a source MD5 when present. A copy streamed through this
host checks the source checksum before committing the upload, according to
`--check-md5`. Checksum and content-header updates after an upload or copy are
conditional on the destination version observed by that operation. Metadata
and content-header changes made by `--attributes-only` use the same safeguard.

Each network request gets up to `--retries` attempts in total, including the
first attempt, with jittered backoff and support for `Retry-After`. Whole-file
restarts have a separate budget of at most three attempts, reduced to one by
`--retries=1`; pipes are never restarted. Retries are decided from the failure:
a timeout, a dropped connection, a 429 or a 5xx is worth another attempt; a 404, a 403 or a
full disk is not. Each one is logged, so a slow transfer never looks like a
silent hang.
Transfer durations and relative ages must be nonnegative and within the supported
duration range. Backoff and server-supplied retry delays saturate instead of
overflowing.

## Progress

On a terminal, `azcp` shows the elapsed time, file and byte totals, and an
aggregate bar with throughput and an estimate. While the scan is finding work,
the bar stays indeterminate; percentages and the estimate appear once the total
is known. Aligned transfer rows keep filenames prominent, and failures, retries
and skipped files get a separate status line. Narrow windows drop the per-file
bars and rates to leave room for names. Filenames are measured in terminal
cells, keeping wide characters, combining accents and joined emoji intact when
shortened. Colours follow the terminal's capabilities and respect `NO_COLOR`.

When the run ends, a final frame stays on screen with the settled file count,
bytes, elapsed time and a completed bar. It appears even if the whole copy
finishes before the first refresh. Empty and all-skipped runs finish at 100%
too. Failed and interrupted runs show their outcome and the amount completed;
active-file rows and the ETA disappear. A dry run says `Would copy` and reports
no transfer rate.

Log records and `-v` output are interleaved without tearing the display. It
stands down entirely when stderr is not a terminal or `TERM=dumb`, so in a
script `azcp` is as quiet as `cp`, and when the copy is itself being written to
that terminal, as `/dev/stdout` is. `--progress=always|never` overrides that.

It draws at startup and completion and refreshes once a second in between.
Log output clears the live frame until the next refresh, so verbose copies do
not repaint the entire display for every file. The display is drawn from a
snapshot, so painting never holds the lock workers use to update progress.
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

Each file is one object: `copy` with `-v`, or `would-copy` under `--dry-run`.
It carries `source`, `destination` and `bytes`, and where the source has them,
`modified` and `content_encoding`. `modified` is when the source was last
written, as `-u` compares it: RFC 3339 in UTC, to the nanosecond, and the
preserved mtime of a blob that carries one. `content_encoding` is the encoding
`--decompress` would undo. So a dry run is enough to decide whether a copy
already made is current, with no second pass to ask:

```
$ azcp --dry-run --output=json -r azure://acct/tree ./copy | head -1
{"bytes":69,"content_encoding":"zstd","destination":"copy/_manifest.json.zst","event":"would-copy","modified":"2026-09-23T13:29:40Z","source":"azure://acct.blob.core.windows.net/tree/_manifest.json.zst"}
```

Failures appear as they happen and again in the summary's `failures` array.
Deletion events use `remove` or `would-remove`, with the destination in the
`destination` field; they are JSON objects too.
Fatal planning errors also produce an error object and a summary with a nonzero
failure count. Errors during initial argument or configuration validation are
reported on stderr before the copy begins.
The summary's `interrupted` field distinguishes cancellation from completion,
even when no files failed. Elapsed time stops when the work ends. When stderr
is a terminal, the progress display and its final frame still appear there;
stdout contains only JSON.

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

Each benchmark uses unique blob names, leaving existing blobs and concurrent
benchmark runs untouched.

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
| `--select=FILTER` | act on the entries of each source directory the filter picks (see [batches](#selecting-and-removing-in-batches)) |
| `--rm` | delete the operands or the selection; lists only, unless `--yes` |
| `--yes` | carry out `--rm` |
| `--put-md5` | record a checksum on each uploaded blob |
| `--check-md5=WHEN` | `off`, `warn`, `fail` (default), `require` |
| `--bwlimit=RATE` | cap throughput, in bytes per second |
| `--delete` | remove destination entries the source does not have |
| `--resume` | continue an interrupted transfer |
| `--newer-than`, `--older-than` | bound by modification time |
| `--compress[=FORMAT[:LEVEL]]` | store files compressed — gzip (default), deflate or zstd — with the encoding set and the extension appended |
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

Exit status is 0 on success and 1 for copy failures or an invalid command line,
matching GNU `cp`.

Local copies reject source/destination aliases before truncating or backing up
anything. When multiple source files map to one destination, the first wins and
later conflicts are reported; `-n` skips them, while `--backup` writes them in
order and retains the earlier versions.

`-f` removes a destination only when opening that destination fails; an
unreadable source leaves the existing destination intact. Compression checks
the open source and destination before truncating. If a copy fails before
creating its destination, a requested backup is restored to the original name.
Local copies read virtual files such as `/proc/version` even when their reported
size is zero, and sparse copies preserve the length actually read.

## Differences from cp

- Blob storage has no native symbolic links. Copying one to a container skips
  it with a warning unless `-a` records it in metadata; `-L` copies what it
  points at instead.
- `--backup`, `--link` and `--symbolic-link` apply to local destinations only,
  and are refused before anything is transferred rather than partway through.
- `-Z`, `--context` and `--preserve=context` are accepted but do nothing: this
  tool does not set SELinux contexts. Using one logs a warning saying so.
- Special files are partly supported. A fifo named as a source is read to its
  end, as `cp` reads one, so a pipe such as `/dev/stdin` copies (see
  [Pipes](#pipes)), and a fifo or device that already exists as the destination
  is written into. A device or socket named as a source is skipped with a
  message, and under `-r` every special file is, where `cp` would recreate it;
  a skip exits 0 if nothing else went wrong. `--copy-contents`, which makes
  `cp -r` read special files, is accepted with a warning and has no effect.
- A missing destination container is an error rather than being created
  silently; `--create-container` opts in. Containers behave more like a mount
  point than a directory, so creating one is not something to do by accident.

Local recursive copies keep the source directory's permissions, subject to the
umask, and leave existing directory permissions alone unless preservation was
requested. Destination parents must exist. Like GNU `cp`, writing through a
dangling destination symlink is refused by default; `--remove-destination`
replaces the link, and `POSIXLY_CORRECT` enables writing through it.

### Platform notes

Windows has no POSIX layer, so `--preserve=ownership` and `--preserve=xattr`
have nothing to act on and `--one-file-system` does not apply; file identity
does exist there, so `--preserve=links` and the loop guard for `-L` both work.
A macOS binary reaches the keychain through cgo, so one cross-compiled from
another platform by `make release` signs in once per run instead of
remembering it; the released macOS binaries are built on macOS and do not have
that limitation.

Local reflink cloning is supported on Linux where the filesystem allows it.
macOS and Windows use ordinary copies with `--reflink=auto`; asking for
`--reflink=always` there fails. macOS's pathname-based `clonefile` cannot safely
replace the open destination while preserving its hard links and permissions.

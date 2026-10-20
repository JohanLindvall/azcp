# azcp and AzCopy

[Documentation](README.md) · [Migration guide](azcopy.md) · [User guide](guide.md)

`azcp` is for people who want familiar `cp` commands to work with local files
and Azure Blob Storage. Start there when choosing a tool: command-line fit,
storage support and operational features matter as much as speed.

## Choose for your workflow

| If you need… | Consider… |
| --- | --- |
| GNU `cp` syntax for local files and blobs | azcp |
| Local-to-local copies using the same tool | azcp |
| Recursive path patterns, braces and extended globs | azcp |
| Compression on upload and decompression on download | azcp |
| A persistent job plan with an ID to inspect or resume | AzCopy |
| A dedicated sync command, including hash comparison | AzCopy |
| Azure Files or other supported storage services | AzCopy |

azcp supports block blobs and local files. Its `--resume` continues partial
transfers without providing durable job history. Its `-u` and `--delete`
options apply timestamp and pruning rules to a copy; they are not a complete
replacement for `azcopy sync`.

See Microsoft's [AzCopy overview](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-v10)
and [sync documentation](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-blobs-synchronize)
for its current storage support and comparison rules. Use the
[migration guide](azcopy.md) for command and flag mappings.

## Recorded request counts

These are the project's existing measurements against AzCopy **10.32.7**,
using the same workloads and endpoint for each comparison:

| Workload | azcp requests | AzCopy requests |
| --- | ---: | ---: |
| Upload 500 × 8 KiB files | **503** | 1,000 |
| Download 500 × 8 KiB files | **503** | 504 |
| Upload one 200 MiB file | 29 | **27** |
| Download one 200 MiB file | **26** | 27 |
| Download 300 files in 341 directories | **93** (2 listings) | 195 (103 listings) |

The first four azcp counts were rechecked against Azurite on **2026-09-16**.
The small-file upload uses an existing container; creating it with
`--create-container` adds one request, for 504 total.

These are historical observations, not new measurements or a benchmark of
the latest AzCopy release. The repository does not include the original
command lines and raw access logs for the table. The
[end-to-end suite](https://github.com/JohanLindvall/azcp/blob/main/scripts/e2e.sh)
checks correctness; it does not regenerate this comparison.

Fewer requests can reduce time spent waiting for the service, especially on
many small files and deep trees. They do not translate directly into the same
multiple of throughput: network latency, file sizes, concurrency, the local
disk and storage limits all affect the result.

## Other recorded observations

The earlier README also reported the following results. They describe
specific workloads; the repository does not retain a complete environment
and reproduction recipe for each one.

| Observation | Reported result |
| --- | --- |
| A sample download from a real Azure account | Roughly twice AzCopy's throughput |
| Listing 400 containers at a simulated 50 ms endpoint latency | 18.5 s sequentially; 0.79 s with look-ahead |
| Scanning an account with 258,000 blobs, mostly in one container | 154 s before splitting large listings; 45 s after |
| Checking 60,000 already-downloaded blobs with `-n --resume` | 120,458 filesystem stat calls before indexing; 458 after |

The cloud download observation is not a general speed guarantee. The other
rows compare azcp implementation changes, not azcp against AzCopy.
The dated [service audit](audit-2026-09-21.md#performance-and-implementation)
and [repository audit](audit-2026-10-03.md#performance-and-implementation)
contain separate microbenchmarks with their methods and validation limits.

## Where request efficiency comes from

- **No destination lookup for a plain upload.** azcp checks the destination
  when an overwrite or preservation rule needs it.
- **Flat recursive listings.** A subtree is enumerated without a listing for
  every directory. Large listings can be split into disjoint key ranges.
- **Container listings overlap.** An account with many containers can list
  several at once, while copy decisions still follow source order.
- **The connection pool follows concurrency.** Connections can be reused
  across the requested number of transfers.
- **Blocks move in parallel.** Files larger than one part use parallel staged
  uploads or ranged downloads.
- **No-clobber reruns use a directory index.** Existing local files can be
  identified with one directory listing instead of a stat per filename.

The [transfer guide](guide.md#transfers) explains the defaults and knobs.
Local copies default to four workers; network copies scale with the machine.

## Measure your own workload

Start with azcp's built-in benchmark to measure the link without reading
source data from your disk:

```sh
azcp --benchmark=10x64MiB azure://myaccount/scratch/
```

Use an existing container you can write to and delete from. This uploads
640 MiB, downloads it, and removes its generated blobs. It performs real
storage operations and may incur storage and transfer charges. This measures
azcp's connection to that endpoint; it does not compare tools.

For a useful comparison:

1. Record both tool versions, OS, architecture, endpoint region/type and all
   transfer flags.
2. Use identical source files, credentials, destination state and overwrite
   rules. Record the number and size distribution of files.
3. Separate first copies, no-clobber reruns and resumed transfers. Record any
   checksums, metadata or compression options.
4. Repeat runs, alternate tool order and report the timings and their range,
   not only the fastest run.
5. For request counts, retain access logs and count data and control requests
   over the same interval. Keep emulator counts separate from cloud timings.

Share a reproduction and measurements in an
[issue](https://github.com/JohanLindvall/azcp/issues/new/choose). A workload
where azcp is slower is useful too.

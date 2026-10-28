# Moving from AzCopy

[Documentation](README.md) · [Comparison](comparison.md) · [Functionality gaps](azcopy-gaps.md) · [User guide](guide.md)

AzCopy is verb-first (`azcopy copy SRC DST --recursive`); `azcp` is `cp`
(`azcp -r SRC DST`). The tables below are a starting point for translating
common copies. Destination layout, timestamp checks and resume state can
differ. Preview the result with `--dry-run` before replacing a command in a
script.

In particular, `azcp -r ./site DEST/` puts `site` inside an existing destination
directory. Use `azcp -rT ./site DEST` to copy its contents directly into DEST.
A trailing slash on the source does not select rsync-style contents-only
behavior. See [destination paths](quickstart.md#understand-the-destination).

`azcp` has no `sync` command. `-u` compares modification times and `--delete`
prunes the copy's destination tree; these are useful building blocks, but do
not reproduce every AzCopy sync rule, including its optional hash comparison.
See Microsoft's [sync documentation](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-blobs-synchronize).

## Commands

| AzCopy | azcp |
| --- | --- |
| `azcopy copy SRC DST` | `azcp SRC DST` |
| `azcopy copy SRC DST --recursive` | `azcp -r SRC DST` |
| `azcopy make URL` | `azcp --create-container …` (made when first written to) |
| `azcopy bench URL` | `azcp --benchmark URL` |
| `azcopy jobs resume ID` | `azcp --resume …` — re-run and rescan; per-file recovery, no job ID |
| `azcopy login` | nothing: credentials are found, and a sign-in is remembered |
| `azcopy list URL` | `azcp --ls URL` |
| `azcopy remove URL` | `azcp --rm URL` previews; add `--yes` to delete, `-r` for directories |

## Flags

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
| `--cap-mbps=100` | `--bwlimit=12500000` | bytes per second; `M` in azcp means MiB |
| `AZCOPY_CONCURRENCY_VALUE=64` | `--jobs=64` | files at once; `--part-concurrency` for blocks within one file |
| `--check-md5=FailIfDifferent` | `--check-md5=fail` | *(default in both)* |
| `--check-md5=NoCheck` | `--check-md5=off` | |
| `--check-md5=LogOnly` | `--check-md5=warn` | |
| `--check-md5=FailIfDifferentOrMissing` | `--check-md5=require` | |
| `--put-md5` | `--put-md5` | |
| `--check-length` | *(always)* | a download is sized to the blob |
| `--metadata 'a=b;c=d'` | `--metadata a=b,c=d` | |
| `--blob-tags 'a=b&c=d'` | `--blob-tags a=b,c=d` | different separators; keys and values are case-sensitive |
| `--s2s-preserve-blob-tags` | `--copy-tags` | opt-in on both sides |
| `--content-type`, `--content-encoding`, `--content-language`, `--content-disposition`, `--cache-control` | same names | |
| `--block-blob-tier=Cool` | `--access-tier=Cool` | |
| `--decompress` | `--decompress` | also handles zstd |
| `--preserve-posix-properties` | `--preserve=mode,ownership,timestamps`, or `-a` | AzCopy-compatible file attributes; see [archive limits](guide.md#archiving-a-tree) |
| `--preserve-permissions`, `--preserve-owner` | *(partial)* | local POSIX ownership/mode preservation does not replace Azure Files or ADLS ACL support |
| `--preserve-symlinks` | `-a` | `is_symlink=true`, target in blob body; same representation |
| `--follow-symlinks` | `-L` | |
| `--from-to=LocalBlob` | *(inferred)* | from the arguments |
| `--output-type=json` | `--output=json` | |
| `--log-level=WARNING` | `--log-level=warn` | `error`, `warn`, `info`, `debug` |
| `AZCOPY_LOG_LOCATION` | `--log-file` | |
| `--list-of-files list.txt` | `--files-from list.txt` | or `--files-from -` to read it from a pipe |
| `AZCOPY_BUFFER_GB` | *(no explicit buffer budget)* | tune jobs, part size and part concurrency |
| `AZCOPY_JOB_PLAN_LOCATION` | *(no job plans)* | uploads use staged blocks; downloads use local sidecars |

## URLs and credentials

The `https://ACCOUNT.blob.core.windows.net/container/path?SAS` form AzCopy uses
works unchanged. `azure://ACCOUNT/container/path` is shorter and fills in the
endpoint for you.

azcp discovers a SAS in the URL, the `AZURE_STORAGE_*` variables, a managed
identity or an existing `az login` session automatically. If interactive
sign-in is needed and a terminal is attached, it can offer a browser or
device-code prompt. Sign-ins are remembered when a platform credential store
is available. See [credential discovery](guide.md#signing-in) for precedence
and permissions.
AzCopy's `AZCOPY_AUTO_LOGIN_TYPE` and azcp's `--auth` select authentication in
different ways; their values are not interchangeable.

## Worked examples

The Windows path in the first example is for PowerShell. The directory update
example previews the affected files; inspect that output before removing
`--dry-run`. It applies azcp's timestamp and pruning rules, not AzCopy's sync
comparison rules.

```
# azcopy copy 'C:\data' 'https://acct.blob.core.windows.net/backup?SAS' --recursive
azcp -r 'C:\data' 'https://acct.blob.core.windows.net/backup?SAS'

# azcopy sync ./site 'https://acct.blob.core.windows.net/www' \
#   --delete-destination=true --exclude-pattern '*.map'
azcp --dry-run -rT -u --delete --exclude '*.map' ./site azure://acct/www/

# azcopy copy 'https://acct.blob.core.windows.net/logs/*' ./logs --recursive \
#   --include-pattern '*.gz' --include-after 2024-01-01
azcp -r --include '*.gz' --newer-than 2024-01-01 'azure://acct/logs/**' ./logs/

# azcopy copy ./big.iso 'https://acct.blob.core.windows.net/c/big.iso' \
#   --block-size-mb 32 --put-md5 --cap-mbps 400
azcp --part-size=32MiB --put-md5 --bwlimit=50000000 ./big.iso azure://acct/c/big.iso
```

## What has no counterpart

Native Azure Files and ADLS Gen2 APIs, S3/GCS ingestion, page/append blob
creation, durable job plans and hash-based sync remain outside azcp's current
feature set. Blob API access to an HNS account is not equivalent to native
ADLS support. See the [functionality gap analysis](azcopy-gaps.md) for the
complete comparison and the limits of partial equivalents.

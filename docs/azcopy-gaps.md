# Functionality gaps compared with AzCopy

[Documentation](README.md) · [Migration guide](azcopy.md) · [Measurements](comparison.md)

Reviewed on **2026-10-28**, against **AzCopy 10.34.1** and azcp's working tree
based on `80afdbc`, including the new index-tag and AzCopy archive-format support.
This is a functional review of source code and official documentation, not a
new performance benchmark. The historical measurements in the comparison page
use a different AzCopy version. [AzCopy release](https://github.com/Azure/azure-storage-azcopy/releases/tag/v10.34.1).

The largest gaps are additional storage services, durable job management,
hash-based synchronization, and specialized Azure blob operations. Several are
deliberate scope choices: azcp keeps GNU cp's command structure and specializes
in local files and Blob Storage. Closing every gap would change that scope.

## Storage services and object types

| Capability | What is missing in azcp |
| --- | --- |
| Azure Files, SMB and NFS | No native share endpoints, share creation/quota management, SMB security descriptors, Windows file attributes, or NFS service permissions/hard-link handling. Copying through a mounted share is a local filesystem operation, not equivalent service support. [Azure Files guide](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-files). |
| ADLS Gen2 / BlobFS | No native `dfs.core.windows.net` endpoint, directory API or ADLS ACL preservation. Some HNS data is accessible through the Blob API, so this does not mean all HNS accounts are unusable. Mixed API access has restrictions. [AzCopy copy reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_copy), [HNS limitations](https://learn.microsoft.com/en-us/azure/storage/blobs/data-lake-storage-known-issues). |
| Amazon S3 and Google Cloud Storage ingestion | No native S3/GCS source enumeration, authentication or migration to Azure blobs. AzCopy supports these source-to-Azure directions; it is not a general copier between every cloud pair. [Copy reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_copy). |
| Page and append blob destinations | azcp writes block blobs. It lacks destination blob-type selection, type preservation and premium page-blob tier selection. Reading another blob type's bytes is not equivalent to reproducing that type. [Copy reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_copy). |

Implementation boundary: [URL parser](../internal/uri/uri.go),
[Azure transfers](../internal/store/azure/transfer.go).

## Jobs, recovery and synchronization

| Capability | azcp today | Remaining gap |
| --- | --- | --- |
| Persistent job plans | `--resume` uses staged blocks for uploads and `.azcp-part` records for downloads. Re-running a command rescans the sources. | No durable manifest identifying the original transfer set, job IDs, job catalog, per-job failure inspection, selective job resume, or plan cleanup commands. AzCopy's plan lets it resume the original unfinished transfers; history lasts while its records are retained. [Jobs and logs](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-configure), [job resume](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_jobs_resume). |
| Dedicated `sync` | `-u` compares timestamps; `--delete` prunes after a successful copy. | No complete equivalent to AzCopy sync's comparison rules, optional MD5-based change detection and destination-deletion prompt. `--check-md5` verifies copied data; it does not select files by comparing hashes. [Sync guide](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-blobs-synchronize), [sync reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_sync). |

These differences matter for long migrations and unattended mirror jobs. A
repeatable copy command with per-file resume is useful recovery, but it cannot
reconstruct an earlier source inventory after that source changes. See
[resume implementation](../internal/store/azure/resume.go) and
[pruning](../internal/engine/prune.go).

## Specialized blob operations

| Capability | What remains missing |
| --- | --- |
| Version and snapshot workflows | No version listing/selection interface, batch download or deletion from a version-ID file, or snapshot deletion policy equivalent to `--delete-snapshots`. This is an interface gap; it does not assert that every manually constructed version/snapshot URL is unusable. [Version downloads](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-blobs-download), [remove reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_remove). |
| Permanent deletion of retained versions/snapshots | No equivalent to AzCopy's preview `--permanent-delete` option for soft-deleted versions and snapshots. Ordinary `--rm` uses normal blob deletion. [Remove reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_remove). |
| Customer-provided encryption settings | No per-request customer-provided key or named encryption-scope options corresponding to AzCopy's `--cpk-by-value` and `--cpk-by-name`. Normal service-side encryption still applies. [Versioned transfer options](https://github.com/Azure/azure-storage-azcopy/blob/v10.34.1/azcopy/transferOptions.go). |
| Standalone property management | `--attributes-only SRC DST` can update headers, metadata, tags and tier without copying content, but still uses cp operands. There is no dedicated recursive `set-properties URL`, clear-all metadata switch or rehydration-priority control. Tags can be cleared with `--blob-tags=`. [Set-properties reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_set-properties). |
| Source access-tier preservation | `--access-tier` selects one destination tier. There is no option guaranteeing each source blob's own tier across all copy routes. Some service routes may preserve a tier themselves. [Tier preservation](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azcopy-blobs-copy). |

Relevant implementation: [CLI options](../internal/cli/spec.go),
[copy planning and options](../internal/engine/copy.go),
[blob deletion](../internal/store/azure/remove.go).

## Partial equivalents and smaller gaps

| Area | Difference |
| --- | --- |
| POSIX archives | azcp now writes AzCopy's standard file-mode, owner/group and timestamp format, reads standard and AMLFS formats, and uses AzCopy's symlink representation. It does not reproduce all Linux stat fields, special files, directory attributes or hard-link identities from blob metadata. Native service ACL support is separate. [AzCopy POSIX schema](https://github.com/Azure/azure-storage-azcopy/blob/v10.34.1/common/unixStatAdapter.go), [azcp archive behavior](guide.md#archiving-a-tree). |
| Listing details | `--ls`, `--totals`, JSON output and property/tag filters exist. There is no selectable output-property list exposing fields such as blob type, version ID, MD5, lease state or archive status. Filtering a property does not add a display column for it. [AzCopy list](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_list), [azcp listing output](../internal/tui/ls.go). |
| Additional filters | azcp has glob, timestamp, size, content, metadata and tag filters. It lacks AzCopy's regex, Windows attribute and blob-type filters. [Copy reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_copy). |
| Transfer resource controls | azcp exposes jobs, block size/concurrency, bandwidth limits and a benchmark. It has no direct equivalent to `AZCOPY_BUFFER_GB`, separate scan concurrency controls or AzCopy's CPU-based tuning controls. Streaming still consumes buffers; these are resource-control differences, not evidence that one tool is faster. [AzCopy settings](https://learn.microsoft.com/en-us/azure/storage/common/storage-ref-azcopy-configuration-settings). |
| Authentication commands | azcp discovers credentials and remembers interactive sign-ins, but has no standalone login/status/logout command family. This is a session-management gap, not a blanket absence of service-principal, workload, managed-identity, CLI or PowerShell authentication. [AzCopy login](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_login), [azcp authentication](../internal/store/azure/auth.go). |
| Empty-container creation | `--create-container` creates a destination on write and the TUI can create a container. There is no standalone CLI `make` operation; Azure file-share creation is also absent. [AzCopy make](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_make). |
| Shell completions | No built-in completion-script generator for Bash, Zsh, Fish or PowerShell. [AzCopy completion](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_completion). |
| MIME detection | azcp guesses content type from the filename extension or accepts `--content-type`; it does not sniff file contents when the extension provides no answer. [Copy reference](https://github.com/Azure/azure-storage-azcopy/wiki/azcopy_copy), [azcp type detection](../internal/store/azure/transfer.go). |

## Features that are already present

Do not treat these as missing merely because the command spelling differs:

- Local-to-blob, blob-to-local, blob-to-blob and local-to-local copies, with
  server-side blob copying and a streamed fallback.
- CLI listing and removal, plus the two-pane TUI and usage view.
- Per-file resume, MD5 recording/verification, dry runs, bandwidth limits,
  parallel transfers, JSON output and file logs.
- Custom metadata and HTTP property filtering in CLI `--select` and TUI `/`.
- Index-tag filtering with `tags.Project=alpha`, setting with `--blob-tags`,
  preservation with `--copy-tags`, and attributes-only tag updates/clearing.
- Compression/decompression and AzCopy-compatible file attributes and symlinks.

Index tags are Azure-indexed labels, separate from ordinary custom metadata.
azcp's filters currently examine tags returned by listings; they do not submit
account-wide Find Blobs by Tags queries. That distinction describes azcp's
implementation, not a claim that AzCopy has a dedicated search command.
[Tag documentation](guide.md#blob-index-tags).

The old `azcp_*` archive attribute keys are no longer interpreted. Existing
archives using them must be restored with an older binary and re-uploaded to
adopt the AzCopy format. This does not remove support for ordinary user metadata.

## Priorities if the scope expands

The largest operational improvements would be durable job plans and hash-based
sync. The more contained additions would be richer listing output, version and
snapshot controls, and explicit source-tier preservation. Additional storage
services and their permission models are larger architectural commitments.
These are candidates for discussion, not promises to implement every AzCopy
feature. The [migration guide](azcopy.md) covers commands that already translate.

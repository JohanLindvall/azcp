# Make your first copy

[Documentation](README.md) · [Install](installation.md) · [User guide](guide.md)

`azcp` uses the same basic shape as `cp`:

```text
azcp [options] SOURCE DESTINATION
```

Either location can be a local path or an Azure Blob Storage URL. The
walkthroughs below use a POSIX shell, such as Bash or Zsh, after
[installation](installation.md).

## Try it locally

This creates a temporary directory containing one small file, copies it and
checks the result. No Azure account or sign-in is needed.

```sh
demo_dir=$(mktemp -d)
cd "$demo_dir"
mkdir source
printf 'Hello from azcp!\n' > source/hello.txt

azcp -rv source copied
diff -r source copied
```

`-r` copies a directory recursively; `-v` prints each copied file. `diff`
prints nothing when the two trees match. Your files are in the directory
printed by `pwd`.

On Windows, the same local copy can be tried in PowerShell:

```powershell
$demoDir = Join-Path ([System.IO.Path]::GetTempPath()) ("azcp-demo-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path "$demoDir\source" | Out-Null
Set-Location $demoDir
Set-Content source\hello.txt 'Hello from azcp!'
azcp -rv source copied
Get-Content copied\hello.txt
```

## Copy to Azure

You need a storage account, a container, and credentials with permission to
write blobs. Replace `myaccount` and `data` below with your account and
container. You can reuse an existing `az login`, supply a SAS, or let azcp
offer interactive sign-in when a terminal is attached. See
[authentication](guide.md#signing-in) for other choices.

Run these commands from the temporary directory created above:

```sh
# Upload one file.
azcp source/hello.txt azure://myaccount/data/hello.txt

# Download it under a different local name.
azcp azure://myaccount/data/hello.txt downloaded.txt

# Check that its contents survived the round trip.
diff source/hello.txt downloaded.txt
```

The short URL expands to
`https://myaccount.blob.core.windows.net/data/hello.txt`. Pasted HTTPS blob
URLs work too. Quote URLs containing a SAS token or wildcards.

A missing container is an error. Add `--create-container` if you want azcp
to create it and your credential permits container creation. A
container-scoped SAS cannot create containers.

## Understand the destination

`azcp` follows `cp`'s directory rules. Suppose `source` contains `hello.txt`:

| Command | Before the copy | Result |
| --- | --- | --- |
| `azcp -r source copied` | `copied` does not exist | `copied/hello.txt` |
| `azcp -r source existing/` | `existing` is a directory | `existing/source/hello.txt` |
| `azcp -rT source existing` | `existing` is a directory | `existing/hello.txt` |

`-T` treats the destination as the tree to fill, rather than a containing
directory. A trailing slash on the source does not change this rule.
Destination parent directories must already exist.

The same rules apply to blob prefixes:

```sh
# An existing container receives a source/ prefix.
azcp -r source azure://myaccount/data/

# Use a specific prefix as the destination tree, even on repeat runs.
azcp -rT source azure://myaccount/data/example
```

For a wildcard that can select multiple files, create the local destination
directory first:

```sh
mkdir -p downloads
azcp 'azure://myaccount/data/**/*.txt' downloads/
```

Keep the quotes: azcp expands remote patterns itself.

## Preview or skip existing files

```sh
azcp --dry-run -rT source azure://myaccount/data/example
azcp -n -rT source azure://myaccount/data/example
```

`--dry-run` reports the planned copy without creating files or containers.
`-n` leaves existing files alone. For an interrupted transfer, use `--resume`
from the first run; combine it with `-n` to skip completed files while finishing
recorded partial downloads. See [resume details](guide.md#interrupted-transfers).

## Try blob storage without an Azure account

[Azurite](https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azurite)
is Microsoft's local storage emulator. You need Docker and an installed
`azcp`. This walkthrough runs in a subshell, so its development credentials
do not replace credentials in your current shell.

It uses a temporary directory, an automatically assigned loopback port, and
an automatically named container. The container is stopped on exit; the
sample files remain in the printed directory.

```sh
(
  set -eu
  demo_dir=$(mktemp -d)
  cd "$demo_dir"
  mkdir source
  printf 'Hello from blob storage!\n' > source/hello.txt

  emulator=$(docker run -d --rm -p 127.0.0.1::10000 \
    mcr.microsoft.com/azure-storage/azurite \
    azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck)
  trap 'docker stop "$emulator" >/dev/null' EXIT
  port=$(docker port "$emulator" 10000/tcp | cut -d: -f2)
  endpoint="http://127.0.0.1:$port/devstoreaccount1"

  # This is Azurite's public development key, not an Azure account secret.
  unset AZURE_STORAGE_CONNECTION_STRING AZURE_STORAGE_SAS_TOKEN
  export AZURE_STORAGE_ACCOUNT=devstoreaccount1
  export AZURE_STORAGE_KEY='Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=='

  # An HTTP response (including an authentication error) means it is ready.
  attempts=0
  until curl -s -o /dev/null "$endpoint?comp=list"; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 30 ]; then
      docker logs "$emulator"
      exit 1
    fi
    sleep 1
  done

  azcp --create-container -r source "$endpoint/demo/"
  azcp -r "$endpoint/demo/source" restored
  diff -r source restored
  printf 'Round trip verified. Sample files: %s\n' "$demo_dir"
)
```

Azurite does not implement the from-URL copy operations, so blob-to-blob
copies use the asynchronous fallback there. Emulator tests cover the main
copy paths, but cannot establish behavior for every Azure configuration.

## If something goes wrong

| Symptom | What to check |
| --- | --- |
| `azcp` is not found | Add the installation directory to `PATH`. On Windows, try `.\azcp.exe` from the extracted directory. |
| The account refuses access | Check that the identity or token has access to this account and container. Signing in alone does not grant permission. See [authentication](guide.md#signing-in). |
| `ContainerNotFound` | Check the container name, or add `--create-container` with a credential that can create it. |
| An extra directory appears at the destination | Review [destination paths](#understand-the-destination). Use `-T` to fill the destination tree directly. |
| A pattern finds no files | Quote it so your shell leaves it intact, then check the account, container and prefix. See [wildcards](guide.md#wildcards). |
| A script shows no progress | Automatic progress is shown on terminals. Use `-v` for per-file output or `--output=json` for structured results. |

If you are still stuck, [open an issue](https://github.com/JohanLindvall/azcp/issues/new/choose)
with your version, command and the result you expected. Remove credentials and
private data before posting.

Next: [everyday examples](../README.md#everyday-examples) or the
[full user guide](guide.md).

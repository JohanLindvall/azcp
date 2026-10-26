# Install azcp

[Documentation](README.md) · [Quick start](quickstart.md) · [User guide](guide.md)

Prebuilt binaries are available for Linux, macOS and Windows, on x86-64 and
ARM64. You do not need Go or the Azure CLI to use them.

## Linux and macOS

```sh
curl -sSfL https://raw.githubusercontent.com/JohanLindvall/azcp/main/scripts/install.sh | sh
```

The [installer](https://github.com/JohanLindvall/azcp/blob/main/scripts/install.sh)
downloads the latest release for your machine, verifies its published SHA-256
checksum and installs it into `~/.local/bin`. When run as root, it uses
`/usr/local/bin`. It prints instructions if the directory needs adding to
your `PATH`.

Verify the installation:

```sh
azcp --version
azcp --help
```

To choose a directory or pin a release, pass the installer's options after
`sh -s --`. For example, to install v0.6.6 into `~/bin`:

```sh
curl -sSfL https://raw.githubusercontent.com/JohanLindvall/azcp/main/scripts/install.sh \
  | sh -s -- --dir "$HOME/bin" --version v0.6.6
```

A pinned release stays installed until you request an update. You can also run
the installer again to update manually. To uninstall, remove the `azcp`
executable from the installation directory.

## Windows and manual downloads

Open the [latest release](https://github.com/JohanLindvall/azcp/releases/latest)
and choose the archive matching your system. `VERSION` below is the release
tag, including its leading `v`.

| System | Archive |
| --- | --- |
| Linux x86-64 | `azcp_VERSION_linux_amd64.tar.gz` |
| Linux ARM64 | `azcp_VERSION_linux_arm64.tar.gz` |
| macOS Apple silicon | `azcp_VERSION_darwin_arm64.tar.gz` |
| macOS Intel | `azcp_VERSION_darwin_amd64.tar.gz` |
| Windows x86-64 | `azcp_VERSION_windows_amd64.zip` |
| Windows ARM64 | `azcp_VERSION_windows_arm64.zip` |

Extract the archive. On Windows, open PowerShell in the extracted directory:

```powershell
.\azcp.exe --version
.\azcp.exe --help
```

Add that directory to your user `Path` to run `azcp` from other directories.
On Linux and macOS, place the extracted `azcp` executable in a directory on
your `PATH`.

Each release also includes a compressed binary without documentation
(`.bin.gz` on Linux/macOS, `.exe.zip` on Windows) and a `SHA256SUMS` file.
Compare the archive's SHA-256 with its entry in that file before extracting:

```sh
# Linux; substitute the name of the archive you downloaded.
sha256sum azcp_VERSION_linux_amd64.tar.gz

# macOS
shasum -a 256 azcp_VERSION_darwin_arm64.tar.gz
```

```powershell
Get-FileHash .\azcp_VERSION_windows_amd64.zip -Algorithm SHA256
```

Linux and Windows binaries are static and need no runtime. Release binaries
for macOS are built on macOS so they can use the keychain to remember sign-ins.

## With Go

Use Go 1.26.0 or newer, as specified in [go.mod](https://github.com/JohanLindvall/azcp/blob/main/go.mod):

```sh
go install github.com/JohanLindvall/azcp/cmd/azcp@latest
```

The executable goes into `GOBIN`, or `$(go env GOPATH)/bin` when `GOBIN` is
unset. Add that directory to your `PATH`. To try the help without installing:

```sh
go run github.com/JohanLindvall/azcp/cmd/azcp@latest --help
```

## Updates

Stable releases check GitHub for a newer stable release after a successful copy
or after leaving the file manager. Checking is enabled by default outside
containers and never installs anything. The check is cached for 24 hours per
executable and user, including failed attempts; changing the installed version
allows a fresh check. Each successful push to `main` produces a stable release
after CI passes, with binaries for all supported platforms and container images.

| Setting | Effect |
| --- | --- |
| `azcp --self-update` | Check now and install the latest stable release. Takes no file operands. |
| `--no-version-check` or `AZCP_NO_VERSION_CHECK=1` | Disable automatic checks, with no update requests or cache writes during copies. |

Install a newer release explicitly:

```sh
azcp --self-update
```

Or disable automatic checks in PowerShell:

```powershell
$env:AZCP_NO_VERSION_CHECK = '1'
```

Unset the variable to restore the default; `0`, `false` and `no` also leave
checks enabled. `--self-update` bypasses the daily cache and automatic-check
opt-outs, so a check or failed download never prevents an explicit retry.
It exits with status 0 when updated or already current, and 1 on failure.
`--no-auto-update` remains accepted for compatibility, and the old
`AZCP_NO_AUTO_UPDATE` variable is no longer needed: copies never install updates.
`--update` and `-u` still control which destination files are copied, as in `cp`.

An update downloads the compressed binary for the current OS and architecture
over HTTPS, verifies its SHA-256 against the release's `SHA256SUMS`, checks its
Go executable identity and platform, and stages it alongside the installed
executable. Installation requires write permission in that directory and never
asks for elevated privileges. Symlinked launchers keep their link; the resolved
executable is updated. Linux and macOS replace the name atomically. Windows
moves the running executable aside, restores it if replacement fails, and
removes the old image on a subsequent run after it is no longer in use.

The current copy finishes before checking. An explicit update takes effect on
the next invocation. A check has a five-second timeout and downloading/installing
has a one-minute deadline. An offline automatic check never changes the copy's
exit status. Automatic notices appear only when stderr is a terminal and name
the `azcp --self-update` command; diagnostic details are available with
`--log-level=debug`. The explicit command reports success or failure even in
scripts. Copy JSON output is unchanged. State is stored under `azcp/updates` in
the OS user cache directory.

Containers, development or prerelease builds, `--dry-run`, `--help`,
`--version`, benchmarks and failed copies skip automatic checks. Explicit
self-updates refuse containers and development or prerelease builds. The official
container image disables updates at build time; Docker, Podman, Kubernetes, systemd and LXC
runtime hints also disable it for binaries installed in other images. Custom
container runtimes that expose none of these hints should set
`AZCP_NO_VERSION_CHECK=1` explicitly. Update container installations by pulling
a newer image.

## Docker

The image is available for `linux/amd64` and `linux/arm64`:

```sh
docker run --rm ghcr.io/johanlindvall/azcp:latest --help
```

Mount the files you want to copy and pass credentials into the container.
For example, after setting `AZURE_STORAGE_ACCOUNT` and `AZURE_STORAGE_KEY`
in your shell:

```sh
docker run --rm \
  -v "$PWD:/data:ro" \
  -e AZURE_STORAGE_ACCOUNT -e AZURE_STORAGE_KEY \
  ghcr.io/johanlindvall/azcp:latest \
  -r /data/build azure://myaccount/releases/
```

Replace `myaccount` and `releases` with your storage account and an existing
container. This copies `./build` into the container's `build/` prefix.
For downloads, use a writable mount without `:ro` and give the container user
permission to write there.

The image contains the static binary and CA certificates, runs as user
`65532:65532`, and has no shell. It is published with a build-provenance
attestation and an SBOM. Tags are `latest`, exact versions such as `0.6.6`,
minor versions such as `0.6`, and `edge` for builds from `main`.

An Azure CLI session on the host is not automatically available inside the
container. Use a SAS, environment credentials or a managed identity available
to the container. See [authentication](guide.md#signing-in).

## Build from source

```sh
git clone https://github.com/JohanLindvall/azcp.git
cd azcp
make build
./bin/azcp --help
```

`make` lists the available targets. See [Contributing](../CONTRIBUTING.md)
for tests, platform requirements and the project layout.

Next: [make your first copy](quickstart.md).

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

Run the installer again to update. To uninstall, remove the `azcp` executable
from the installation directory.

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

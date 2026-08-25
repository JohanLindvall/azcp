# syntax=docker/dockerfile:1

# Go cross-compiles natively, so the build always runs on the machine's own
# architecture and only the output is retargeted. That makes the arm64 image
# build as fast as the amd64 one, with no emulator involved.
FROM --platform=$BUILDPLATFORM golang:1-alpine AS build

ARG TARGETARCH
ARG VERSION=dev

RUN apk add --no-cache ca-certificates
WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags "-s -w -X 'github.com/JohanLindvall/azcp/internal/cli.Version=${VERSION}'" \
      -o /out/azcp ./cmd/azcp

# Nothing but the binary and the roots it needs to verify a TLS connection to
# storage. A static binary needs no libc, so there is no base image to keep
# patched and nothing in the image to have a vulnerability.
FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/azcp /azcp

# There is no /etc/passwd here, so the user is numeric. 65532 is the
# conventional "nonroot" id.
USER 65532:65532

ENTRYPOINT ["/azcp"]

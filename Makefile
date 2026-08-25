BIN := bin/azcp
GO  ?= go

.PHONY: all build test race vet fmt lint clean install

all: build

build:
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/azcp

install:
	$(GO) install ./cmd/azcp

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

# Everything CI runs, in one target.
lint: fmt vet test

clean:
	rm -rf bin

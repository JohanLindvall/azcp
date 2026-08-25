# Build, test and release helpers for azcp.
#
# `make` on its own lists the targets.

BIN       := bin/azcp
PKG       := github.com/JohanLindvall/azcp
GO        ?= go

# Stamped into the binary so `azcp --version` reports the commit it came from.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X '$(PKG)/internal/cli.Version=$(VERSION)'
BUILDOPTS := -trimpath -ldflags "$(LDFLAGS)"

# Platforms `make release` builds for. Windows is included and compiles, but
# the cp semantics it can honour there are limited: ownership, extended
# attributes and hard links have no counterpart.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# Settings for the Azurite emulator used by `make e2e`. The key is the
# well-known development one published by Microsoft, not a secret.
AZURITE_IMAGE := mcr.microsoft.com/azure-storage/azurite
AZURITE_NAME  := azcp-azurite
AZURITE_PORT  := 10000

.DEFAULT_GOAL := help
.PHONY: help build install test race cover bench vet fmt fmt-check tidy lint \
        check cross release e2e azurite azurite-stop clean

help: ## List the available targets
	@echo "azcp $(VERSION)"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build ./bin/azcp
	$(GO) build $(BUILDOPTS) -o $(BIN) ./cmd/azcp

install: ## Install azcp into GOBIN
	$(GO) install $(BUILDOPTS) ./cmd/azcp

test: ## Run the tests
	$(GO) test ./...

race: ## Run the tests under the race detector
	$(GO) test -race ./...

cover: ## Report test coverage per package
	$(GO) test -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -1
	@echo "detail: go tool cover -html=coverage.out"

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Rewrite sources with gofmt
	gofmt -l -w .

fmt-check: ## Fail if anything is unformatted
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

lint: fmt-check vet ## Check formatting and run go vet

check: lint test race ## Everything CI runs

cross: ## Check that every release platform compiles
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  printf '  %-16s ' "$$p"; \
	  if GOOS=$$os GOARCH=$$arch $(GO) build -o /dev/null ./... 2>/dev/null; \
	    then echo ok; else echo FAILED; exit 1; fi; \
	done

release: ## Build stripped binaries for every platform into ./dist
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  ext=''; [ "$$os" = windows ] && ext='.exe'; \
	  out="dist/azcp-$(VERSION)-$$os-$$arch$$ext"; \
	  echo "  $$out"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	    $(GO) build $(BUILDOPTS) -o "$$out" ./cmd/azcp || exit 1; \
	done
	@cd dist && sha256sum * > SHA256SUMS && echo "  dist/SHA256SUMS"

azurite: ## Start the Azure storage emulator in Docker
	@docker rm -f $(AZURITE_NAME) >/dev/null 2>&1 || true
	docker run -d --rm --name $(AZURITE_NAME) -p $(AZURITE_PORT):10000 \
	  $(AZURITE_IMAGE) azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck
	@echo "waiting for the emulator..."
	@for i in $$(seq 1 20); do \
	  code=$$(curl -s -o /dev/null -w '%{http_code}' \
	    http://127.0.0.1:$(AZURITE_PORT)/devstoreaccount1?comp=list 2>/dev/null); \
	  [ "$$code" != "000" ] && echo "ready" && exit 0; sleep 1; \
	done; echo "emulator did not come up"; exit 1

azurite-stop: ## Stop the emulator
	-docker rm -f $(AZURITE_NAME)

e2e: build ## Exercise the blob paths against the emulator (needs Docker)
	@$(MAKE) --no-print-directory azurite
	AZCP=$(CURDIR)/$(BIN) AZURITE_PORT=$(AZURITE_PORT) ./scripts/e2e.sh; \
	  status=$$?; $(MAKE) --no-print-directory azurite-stop >/dev/null 2>&1; \
	  exit $$status

clean: ## Remove build output
	rm -rf bin dist coverage.out

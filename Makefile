# Forebay build and check targets. CI runs the same ones, so a green `make
# check` locally means a green pipeline.

GO      ?= go
# CHROME renders the social card. Override if it lives elsewhere.
CHROME  ?= /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
BIN     := bin
PKGS    := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MODULE  := github.com/mayur-tolexo/forebay
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# Minimum statement coverage. Raising this is easy, lowering it needs a reason.
COVER_MIN ?= 80

.PHONY: all
all: check build

.PHONY: build
build: ## Build both binaries into bin/
	@mkdir -p $(BIN)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

.PHONY: check
check: fmt-check vet test cover-check csrc ## Everything CI enforces

.PHONY: csrc
csrc: ## Compile the C that builds without Ganesha, which is the client and not the hook
	$(MAKE) -C fsal all

.PHONY: test
test: ## Run tests with the race detector
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out $(PKGS)

.PHONY: cover
cover: test ## Show per-function coverage
	$(GO) tool cover -func=coverage.out

.PHONY: cover-check
cover-check: test ## Fail if total coverage falls below COVER_MIN
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "total coverage: $$total% (minimum $(COVER_MIN)%)"; \
	awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { exit !(t+0 >= m+0) }' || \
		{ echo "coverage $$total% is below the $(COVER_MIN)% minimum"; exit 1; }

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt: ## Rewrite files with gofmt
	gofmt -w -s $$(find . -name '*.go' -not -path './.git/*')

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@out=$$(gofmt -l -s $$(find . -name '*.go' -not -path './.git/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: ## Run golangci-lint if it is installed
	@command -v golangci-lint >/dev/null 2>&1 || \
		{ echo "golangci-lint not installed, skipping"; exit 0; }
	golangci-lint run

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build and coverage artefacts
	rm -rf $(BIN) coverage.out

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

# social-card renders the link preview GitHub asks for, since its uploader
# takes a raster and the source is an SVG.
.PHONY: social-card
social-card:
	@printf '<!doctype html><style>html,body{margin:0;width:1280px;height:640px}img{width:1280px;height:640px;display:block}</style><img src="social-card.svg">' > docs/brand/_card.html
	@"$(CHROME)" --headless --disable-gpu --hide-scrollbars --window-size=1280,640 \
		--screenshot=docs/brand/social-card.png "file://$(CURDIR)/docs/brand/_card.html" 2>/dev/null
	@rm -f docs/brand/_card.html
	@echo "docs/brand/social-card.png regenerated"

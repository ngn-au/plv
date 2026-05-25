# PLV — developer tasks. Run `make` or `make help` for the list.
#
# `make check` is the full local gate (gofmt + vet + race tests + govulncheck) —
# the same checks CI's `go` and `govulncheck` jobs run, but directly on the host.
# `make ci-local` runs those same jobs inside the GitHub Actions runner image via
# act, for a faithful reproduction of CI before pushing.

GO       ?= go
ACT      ?= act
VERSION  ?= dev
LDFLAGS  := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: fmt
fmt: ## Format all Go files (gofmt -w).
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is not gofmt-clean.
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt-clean:"; echo "$$unformatted"; echo "Run: make fmt"; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: test
test: ## Run the race-enabled test suite.
	$(GO) test -race ./...

.PHONY: vuln
vuln: ## Run the official Go vulnerability scanner.
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: build
build: ## Build the plv binary (inject VERSION=... to stamp it).
	$(GO) build -ldflags="$(LDFLAGS)" -o plv .

.PHONY: run
run: ## Run PLV locally (LOGDIR=... ADDR=... override defaults).
	$(GO) run . -addr "$${ADDR:-:8080}" -logdir "$${LOGDIR:-/var/log}"

.PHONY: check
check: fmt-check vet test vuln ## The full local gate (matches CI, no Docker).
	@echo "All local checks passed."

.PHONY: ci-local
ci-local: ## Run the CI go + govulncheck jobs in the runner image via act.
	$(ACT) -j go
	$(ACT) -j govulncheck

.PHONY: docker-build
docker-build: ## Build the production container image locally.
	docker build --build-arg VERSION=$(VERSION) -t plv:$(VERSION) .

.PHONY: clean
clean: ## Remove build output.
	rm -f plv plv.exe
	rm -rf dist

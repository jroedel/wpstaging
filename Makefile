# wpstaging — WordPress state capture, staging and promotion.
#
# Targets mirror the sibling project's workflow (build / vet / fmt / lint /
# test) so the same habits apply here. Dependencies are pure Go -- the SQLite
# driver for the state index and the MySQL driver for dumps -- so builds need no
# C toolchain and the result is one static binary.
#
# Offline: everything here runs from the module cache after one `make deps`,
# with one exception -- `vuln-check` queries the Go vulnerability database over
# the network on every run, and `make test` includes it. On a machine with no
# network, run `make test-unit lint` instead.

GO        ?= go
BINARY    ?= wpstaging
CMD       ?= ./cmd/wpstaging
ADDR      ?= 127.0.0.1:8099

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  %-18s %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Download module dependencies into the module cache
	$(GO) mod download

.PHONY: build
build: ## Build the binary
	$(GO) build -o $(BINARY) $(CMD)

.PHONY: run
run: ## Run the web UI (make run ADDR=127.0.0.1:9099)
	$(GO) run $(CMD) serve -addr $(ADDR)

.PHONY: vet
vet: ## go vet the whole module
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go sources in place
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not gofmt-clean
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: lint
lint: vet fmt-check ## vet + gofmt check

.PHONY: vuln-check
vuln-check: ## Check dependencies against the Go vulnerability database (needs network)
	$(GO) tool govulncheck ./...

.PHONY: test-unit
test-unit: ## Run unit tests
	$(GO) test ./...

# Integration tests need a real MySQL/MariaDB, because the properties under
# test -- byte-identical dumps, exact round trips through every column type --
# are properties of a server's behaviour and a fake would only test the fake.
TEST_DB_IMAGE ?= mariadb:10.11
TEST_DB_PORT  ?= 13306
TEST_DSN      ?= root:test@tcp(127.0.0.1:$(TEST_DB_PORT))/wptest

.PHONY: test-db
test-db: ## Start a throwaway MariaDB for the integration tests
	docker rm -f wpstg-test 2>/dev/null || true
	docker run -d --name wpstg-test \
		-e MARIADB_ROOT_PASSWORD=test -e MARIADB_DATABASE=wptest \
		-p $(TEST_DB_PORT):3306 $(TEST_DB_IMAGE)
	@echo "waiting for the server to accept connections..."
	@for i in $$(seq 1 60); do \
		docker exec wpstg-test mariadb -uroot -ptest -e 'SELECT 1' >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	@echo "ready: WPSTAGING_TEST_DSN='$(TEST_DSN)'"

.PHONY: test-db-stop
test-db-stop: ## Remove the throwaway database server
	docker rm -f wpstg-test 2>/dev/null || true

.PHONY: test-integration
test-integration: ## Run integration tests (needs `make test-db` first)
	WPSTAGING_TEST_DSN='$(TEST_DSN)' $(GO) test -tags=integration ./...

.PHONY: test
test: test-unit lint vuln-check ## Full check: unit tests + lint + vulnerability scan

.PHONY: cover
cover: ## Unit tests with a coverage summary
	$(GO) test -cover ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -f $(BINARY)
